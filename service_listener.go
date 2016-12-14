package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/restclient"
	"k8s.io/kubernetes/pkg/client/transport"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"

	"github.com/cloudflare/cloudflare-go"
	"golang.org/x/net/publicsuffix"
)

// Don't actually commit the changes to route53 records, just print out what we would have done.
var dryRun bool
var cfAPIKey, cfAPIEmail string
var cfEnable bool
var cfAPI *cloudflare.API

func init() {
	dryRunStr := os.Getenv("DRY_RUN")
	if dryRunStr != "" {
		dryRun = true
	}
}

func main() {
	flag.Parse()
	glog.Info("DNS Update Service")

	config, err := restclient.InClusterConfig()
	if err != nil {
		kubernetesService := os.Getenv("KUBERNETES_SERVICE_HOST")
		kubernetesServicePort := os.Getenv("KUBERNETES_SERVICE_PORT")
		if kubernetesService == "" {
			glog.Fatal("Please specify the Kubernetes server via KUBERNETES_SERVICE_HOST")
		}
		if kubernetesServicePort == "" {
			kubernetesServicePort = "443"
		}
		apiServer := fmt.Sprintf("https://%s:%s", kubernetesService, kubernetesServicePort)

		caFilePath := os.Getenv("CA_FILE_PATH")
		certFilePath := os.Getenv("CERT_FILE_PATH")
		keyFilePath := os.Getenv("KEY_FILE_PATH")
		if caFilePath == "" || certFilePath == "" || keyFilePath == "" {
			glog.Fatal("You must provide paths for CA, Cert, and Key files")
		}

		tls := transport.TLSConfig{
			CAFile:   caFilePath,
			CertFile: certFilePath,
			KeyFile:  keyFilePath,
		}
		// tlsTransport := transport.New(transport.Config{TLS: tls})
		tlsTransport, err := transport.New(&transport.Config{TLS: tls})
		if err != nil {
			glog.Fatalf("Couldn't set up tls transport: %s", err)
		}

		config = &restclient.Config{
			Host:      apiServer,
			Transport: tlsTransport,
		}
	}

	c, err := client.New(config)
	if err != nil {
		glog.Fatalf("Failed to make client: %v", err)
	}
	glog.Infof("Connected to kubernetes @ %s", config.Host)

	metadata := ec2metadata.New(session.New())

	creds := credentials.NewChainCredentials(
		[]credentials.Provider{
			&credentials.EnvProvider{},
			&credentials.SharedCredentialsProvider{},
			&ec2rolecreds.EC2RoleProvider{Client: metadata},
		})

	region, err := metadata.Region()
	if err != nil {
		glog.Fatalf("Unable to retrieve the region from the EC2 instance %v\n", err)
	}

	awsConfig := aws.NewConfig()
	awsConfig.WithCredentials(creds)
	awsConfig.WithRegion(region)
	sess := session.New(awsConfig)

	r53Api := route53.New(sess)
	elbAPI := elb.New(sess)
	if r53Api == nil || elbAPI == nil {
		glog.Fatal("Failed to make AWS connection")
	}

	cfAPIKey := os.Getenv("CF_API_KEY")
	cfAPIEmail := os.Getenv("CF_API_EMAIL")
	if cfAPIKey != "" && cfAPIEmail != "" {
		cfEnable = true
	}

	if cfEnable {
		var err error
		cfAPI, err = cloudflare.New(cfAPIKey, cfAPIEmail)
		if err != nil {
			glog.Fatal(err)
		}
	}

	r53Selector := "dns=route53"
	l, err := labels.Parse(r53Selector)
	if err != nil {
		glog.Fatalf("Failed to parse selector %q: %v", r53Selector, err)
	}
	r53ListOptions := api.ListOptions{
		LabelSelector: l,
	}

	cfSelector := "dns=cloudflare"
	l, err = labels.Parse(cfSelector)
	if err != nil {
		glog.Fatalf("Failed to parse selector %q: %v", cfSelector, err)
	}
	cfListOptions := api.ListOptions{
		LabelSelector: l,
	}

	glog.Infof("Starting Service Polling every 30s")
	for {
		// Update route53 records
		services, err := c.Services(api.NamespaceAll).List(r53ListOptions)
		if err != nil {
			glog.Fatalf("Failed to list pods: %v", err)
		}

		glog.Infof("Found %d DNS services in all namespaces with selector %q", len(services.Items), r53Selector)
		for i := range services.Items {
			s := &services.Items[i]
			hn, err := serviceHostname(s)
			if err != nil {
				glog.Warningf("Couldn't find hostname for %s: %s", s.Name, err)
				continue
			}

			annotation, ok := s.ObjectMeta.Annotations["domainName"]
			if !ok {
				glog.Warningf("Domain name not set for %s", s.Name)
				continue
			}

			domains := strings.Split(annotation, ",")
			for j := range domains {
				domain := domains[j]

				glog.Infof("Creating DNS for %s service: %s -> %s", s.Name, hn, domain)
				elbZoneID, err := r53HostedZoneId(elbAPI, hn)
				if err != nil {
					glog.Warningf("Couldn't get zone ID: %s", err)
					continue
				}

				zone, err := r53GetDestinationZone(domain, r53Api)
				if err != nil {
					glog.Warningf("Couldn't find destination zone: %s", err)
					continue
				}

				zoneID := *zone.Id
				zoneParts := strings.Split(zoneID, "/")
				zoneID = zoneParts[len(zoneParts)-1]

				if err = r53UpdateDNS(r53Api, hn, elbZoneID, strings.TrimLeft(domain, "."), zoneID); err != nil {
					glog.Warning(err)
					continue
				}
				glog.Infof("Created dns record set: domain=%s, zoneID=%s", domain, zoneID)
			}
		}

		if cfEnable {
			// Update Cloudflare records
			services, err = c.Services(api.NamespaceAll).List(cfListOptions)
			if err != nil {
				glog.Fatalf("Failed to list pods: %v", err)
			}

			glog.Infof("Found %d DNS services in all namespaces with selector %q", len(services.Items), cfSelector)
			for i := range services.Items {
				s := &services.Items[i]
				hn, err := serviceHostname(s)
				if err != nil {
					glog.Warningf("Couldn't find hostname for %s: %s", s.Name, err)
					continue
				}

				annotation, ok := s.ObjectMeta.Annotations["domainName"]
				if !ok {
					glog.Warningf("Domain name not set for %s", s.Name)
					continue
				}

				domains := strings.Split(annotation, ",")
				for j := range domains {
					domain := domains[j]

					glog.Infof("Creating DNS for %s service: %s -> %s", s.Name, hn, domain)

					// Fetch the zone ID
					domainTLD, _ := publicsuffix.EffectiveTLDPlusOne(domain)
					zoneID, err := cfAPI.ZoneIDByName(domainTLD)
					if err != nil {
						glog.Warning(err)
						continue
					}

					// Prepare DNS record
					var rr cloudflare.DNSRecord
					rr.Name = domain
					rr.Type = "CNAME"

					rrs, err := cfAPI.DNSRecords(zoneID, rr)
					if err != nil {
						glog.Warning(err)
						continue
					}

					// Update/Create DNS record
					rr.Content = hn
					if len(rrs) > 0 {

						err := cfAPI.UpdateDNSRecord(zoneID, rrs[0].ID, rr)
						if err != nil {
							glog.Warning(err)
							continue
						}

						glog.Infof("Updated dns record set: domain=%s, content=%s", domain, hn)
					} else {
						res, err := cfAPI.CreateDNSRecord(zoneID, rr)
						if err != nil {
							glog.Warning(err)
							continue
						}
						glog.Infof("Created dns record set: domain=%s, content=%s", domain, hn)
						glog.Info(res)
					}
				}
			}
		}
		time.Sleep(30 * time.Second)
	}
}

func r53GetDestinationZone(domain string, r53Api *route53.Route53) (*route53.HostedZone, error) {
	tld, err := getTLD(domain)
	if err != nil {
		return nil, err
	}

	listHostedZoneInput := route53.ListHostedZonesByNameInput{
		DNSName: &tld,
	}
	hzOut, err := r53Api.ListHostedZonesByName(&listHostedZoneInput)
	if err != nil {
		return nil, fmt.Errorf("No zone found for %s: %v", tld, err)
	}
	// TODO: The AWS API may return multiple pages, we should parse them all

	return findMostSpecificZoneForDomain(domain, hzOut.HostedZones)
}

func findMostSpecificZoneForDomain(domain string, zones []*route53.HostedZone) (*route53.HostedZone, error) {
	domain = domainWithTrailingDot(domain)
	if len(zones) < 1 {
		return nil, fmt.Errorf("No zone found for %s", domain)
	}
	var mostSpecific *route53.HostedZone
	curLen := 0

	for i := range zones {
		zone := zones[i]
		zoneName := *zone.Name

		if strings.HasSuffix(domain, zoneName) && curLen < len(zoneName) {
			curLen = len(zoneName)
			mostSpecific = zone
		}
	}

	if mostSpecific == nil {
		return nil, fmt.Errorf("Zone found %s does not match domain given %s", *zones[0].Name, domain)
	}

	return mostSpecific, nil
}

func getTLD(domain string) (string, error) {
	domainParts := strings.Split(domain, ".")
	segments := len(domainParts)
	if segments < 3 {
		return "", fmt.Errorf("Domain %s is invalid - it should be a fully qualified domain name and subdomain (i.e. test.example.com)", domain)
	}
	return strings.Join(domainParts[segments-2:], "."), nil
}

func domainWithTrailingDot(withoutDot string) string {
	if withoutDot[len(withoutDot)-1:] == "." {
		return withoutDot
	}
	return fmt.Sprint(withoutDot, ".")
}

func serviceHostname(service *api.Service) (string, error) {
	ingress := service.Status.LoadBalancer.Ingress
	if len(ingress) < 1 {
		return "", errors.New("No ingress defined for ELB")
	}
	if len(ingress) > 1 {
		return "", errors.New("Multiple ingress points found for ELB, not supported")
	}
	return ingress[0].Hostname, nil
}

func loadBalancerNameFromHostname(hostname string) (string, error) {
	var name string
	hostnameSegments := strings.Split(hostname, "-")
	if len(hostnameSegments) < 2 {
		return name, fmt.Errorf("%s is not a valid ELB hostname", hostname)
	}
	name = hostnameSegments[0]

	// handle internal load balancer naming
	if name == "internal" {
		name = hostnameSegments[1]
	}

	return name, nil
}

func r53HostedZoneId(elbAPI *elb.ELB, hostname string) (string, error) {
	elbName, err := loadBalancerNameFromHostname(hostname)
	if err != nil {
		return "", fmt.Errorf("Couldn't parse ELB hostname: %v", err)
	}
	lbInput := &elb.DescribeLoadBalancersInput{
		LoadBalancerNames: []*string{
			&elbName,
		},
	}
	resp, err := elbAPI.DescribeLoadBalancers(lbInput)
	if err != nil {
		return "", fmt.Errorf("Could not describe load balancer: %v", err)
	}
	descs := resp.LoadBalancerDescriptions
	if len(descs) < 1 {
		return "", fmt.Errorf("No lb found: %v", err)
	}
	if len(descs) > 1 {
		return "", fmt.Errorf("Multiple lbs found: %v", err)
	}
	return *descs[0].CanonicalHostedZoneNameID, nil
}

func r53UpdateDNS(r53Api *route53.Route53, hn, hzID, domain, zoneID string) error {
	at := route53.AliasTarget{
		DNSName:              &hn,
		EvaluateTargetHealth: aws.Bool(false),
		HostedZoneId:         &hzID,
	}
	rrs := route53.ResourceRecordSet{
		AliasTarget: &at,
		Name:        &domain,
		Type:        aws.String("A"),
	}
	change := route53.Change{
		Action:            aws.String("UPSERT"),
		ResourceRecordSet: &rrs,
	}
	batch := route53.ChangeBatch{
		Changes: []*route53.Change{&change},
		Comment: aws.String("Kubernetes Update to Service"),
	}
	crrsInput := route53.ChangeResourceRecordSetsInput{
		ChangeBatch:  &batch,
		HostedZoneId: &zoneID,
	}
	if dryRun {
		glog.Infof("DRY RUN: We normally would have updated %s to point to %s (%s)", zoneID, hzID, hn)
		return nil
	}

	_, err := r53Api.ChangeResourceRecordSets(&crrsInput)
	if err != nil {
		return fmt.Errorf("Failed to update record set: %v", err)
	}
	return nil
}
