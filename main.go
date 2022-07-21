package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"time"

	nutanixclient "github.com/nutanix-cloud-native/prism-go-client/pkg/nutanix"
	nutanixclientv3 "github.com/nutanix-cloud-native/prism-go-client/pkg/nutanix/v3"
	"k8s.io/klog"
)

// More info about the API is here https://www.nutanix.dev/api_references/prism-central-v3/

var secretDirectory = ".secret/nutanix"

/*
{"type":"basic_auth","data":{"prismCentral":{"username":"<user>","password":"<password>"},"prismElements":null}}
*/
var secretFile = "secret.conf"

/*
{ "prismCentral": { "address": "<address>", "port": <port> }}
*/
var endpointFile = "endpoint.conf"

type PrismCentral struct {
	Address  string
	Port     int
	Username string
	Password string
}

type PrismData struct {
	PrismCentral PrismCentral
	// PrismElements PrismElements // not needed
}

type PrismEndpoint struct {
	Type string
	Data PrismData
}

// This method is a shameless copy of https://github.com/openshift/installer/blob/master/pkg/types/nutanix/client.go
// so that I don't have to reinvent the wheel. All credits to the authors of that code and obviously
// the original license applies here, too.
// Some modifications applied after that.
// CreateNutanixClient creates a Nutanix V3 Client
func CreateNutanixClient(ctx context.Context, endpoint PrismEndpoint) (*nutanixclientv3.Client, error) {
	_, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cred := nutanixclient.Credentials{
		URL:      fmt.Sprintf("%s:%d", endpoint.Data.PrismCentral.Address, endpoint.Data.PrismCentral.Port),
		Username: endpoint.Data.PrismCentral.Username,
		Password: endpoint.Data.PrismCentral.Password,
		Port:     fmt.Sprintf("%d", endpoint.Data.PrismCentral.Port),
		Endpoint: endpoint.Data.PrismCentral.Address,
	}

	return nutanixclientv3.NewV3Client(cred)
}

var nodeNameFlag = flag.String("node-name", "", "name of the node to update")
var ipAddressAddFlag = flag.String("add-address", "", "add this ip address")
var ipAddressRemoveFlag = flag.String("remove-address", "", "remove this ip address")

func main() {
	flag.Parse()

	if *nodeNameFlag == "" {
		klog.Fatal("Provide a node name")
	}
	if *ipAddressAddFlag != "" && *ipAddressRemoveFlag != "" {
		klog.Fatal("Must provide exactly one, add-address or remove-address")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		klog.Fatal(err)
	}
	secretText, err := ioutil.ReadFile(path.Join(home, secretDirectory, secretFile))
	if err != nil {
		klog.Fatal(err)
	}
	var secret PrismEndpoint
	err = json.Unmarshal(secretText, &secret)
	if err != nil {
		klog.Fatal(err)
	}
	endpointText, err := ioutil.ReadFile(path.Join(home, secretDirectory, endpointFile))
	if err != nil {
		klog.Fatal(err)
	}
	var endpoint PrismData
	err = json.Unmarshal(endpointText, &endpoint)
	if err != nil {
		klog.Fatal(err)
	}

	secret.Data.PrismCentral.Address = endpoint.PrismCentral.Address
	secret.Data.PrismCentral.Port = endpoint.PrismCentral.Port

	client, err := CreateNutanixClient(context.TODO(), secret)
	if err != nil {
		klog.Fatal(err)
	}

	// https://www.nutanix.dev/api_references/prism-central-v3/#/1249f40c4f9db-get-a-list-of-existing-vms
	// Question: Do we have to read the status or the spec?
	vmList, err := client.V3.ListAllVM("")
	if err != nil {
		klog.Fatal(err)
	}
	for _, vmFromList := range vmList.Entities {
		// All of that info is already part of the list request, but for our real implementation
		// we must run the get request, so let's try that as well.
		name := vmFromList.Spec.Name
		if *name != *nodeNameFlag {
			continue
		}
		resB, err := json.MarshalIndent(vmFromList, "", "    ")
		if err != nil {
			klog.Fatal(err)
		}
		klog.Infof("VM res: %s", resB)

		uuid := vmFromList.Metadata.UUID
		klog.Infof("Getting further info for VM %s with UUID %s", *name, *uuid)
		vm, err := client.V3.GetVM(*uuid)
		if err != nil {
			klog.Fatal(err)
		}
		for k, nic := range vm.Spec.Resources.NicList {
			var ips []string
			for _, ip := range nic.IPEndpointList {
				ips = append(ips, *ip.IP)
			}
			klog.Infof("VM %s (uuid %s) interface %d has the following MAC %s and IP addresses %v", *name, *uuid, k, *nic.MacAddress, ips)
			subnetRef := nic.SubnetReference
			subnet, err := client.V3.GetSubnet(*subnetRef.UUID)
			if err != nil {
				klog.Fatal(err)
			}
			subnetName := subnet.Spec.Name
			ipConfig := subnet.Spec.Resources.IPConfig
			_, ipNet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", *ipConfig.SubnetIP, *ipConfig.PrefixLength))
			if err != nil {
				klog.Fatal(err)
			}
			klog.Infof("Attached subnet %s with CIDR %s", *subnetName, ipNet)
			if *ipAddressAddFlag != "" && ipNet.Contains(net.ParseIP(*ipAddressAddFlag)) {
				klog.Infof("Adding IP %s to node %s", *ipAddressAddFlag, *name)
				assigned := "ASSIGNED"
				nic.IPEndpointList = append(nic.IPEndpointList, &nutanixclientv3.IPAddress{
					IP:   ipAddressAddFlag,
					Type: &assigned,
				})
			}
			if *ipAddressRemoveFlag != "" && ipNet.Contains(net.ParseIP(*ipAddressRemoveFlag)) {
				klog.Infof("Removing IP %s to node %s", *ipAddressRemoveFlag, *name)
				var newIPEndpointList []*nutanixclientv3.IPAddress
				for _, ip := range nic.IPEndpointList {
					if net.ParseIP(*ip.IP).Equal(net.ParseIP(*ipAddressRemoveFlag)) {
						continue
					}
					newIPEndpointList = append(newIPEndpointList, ip)
				}
				nic.IPEndpointList = newIPEndpointList
			}
		}

		if *ipAddressRemoveFlag != "" || *ipAddressAddFlag != "" {
			var intent nutanixclientv3.VMIntentInput
			intent.Metadata = vm.Metadata
			intent.Spec = vm.Spec
			/* intent.Spec = &nutanixclientv3.VM{}
			intent.Spec.Name = name
			intent.Spec.Resources = &nutanixclientv3.VMResources{}
			intent.Spec.Resources.NicList = nicList*/
			// Now, I run into: https://next.nutanix.com/how-it-works-22/updating-vm-via-prismcentral-v3-api-37435
			res, err := client.V3.UpdateVM(*uuid, &intent)
			if err != nil {
				klog.Fatal(err)
			}
			resB, err := json.MarshalIndent(res, "", "    ")
			if err != nil {
				klog.Fatal(err)
			}
			klog.Infof("Update res: %s", resB)
		}
	}
}
