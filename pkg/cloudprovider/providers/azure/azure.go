/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure

import (
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"time"

	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/version"

	"github.com/Azure/azure-sdk-for-go/arm/compute"
	"github.com/Azure/azure-sdk-for-go/arm/disk"
	"github.com/Azure/azure-sdk-for-go/arm/network"
	"github.com/Azure/azure-sdk-for-go/arm/storage"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/ghodss/yaml"
	"github.com/golang/glog"
	"golang.org/x/crypto/pkcs12"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// CloudProviderName is the value used for the --cloud-provider flag
	CloudProviderName            = "azure"
	rateLimitQPSDefault          = 1.0
	rateLimitBucketDefault       = 5
	backoffRetriesDefault        = 6
	backoffExponentDefault       = 1.5
	backoffDurationDefault       = 5 // in seconds
	backoffJitterDefault         = 1.0
	maximumLoadBalancerRuleCount = 148 // According to Azure LB rule default limit

	vmTypeVMSS     = "vmss"
	vmTypeStandard = "standard"
)

// Config holds the configuration parsed from the --cloud-config flag
// All fields are required unless otherwise specified
type Config struct {
	// The cloud environment identifier. Takes values from https://github.com/Azure/go-autorest/blob/ec5f4903f77ed9927ac95b19ab8e44ada64c1356/autorest/azure/environments.go#L13
	Cloud string `json:"cloud" yaml:"cloud"`
	// The AAD Tenant ID for the Subscription that the cluster is deployed in
	TenantID string `json:"tenantId" yaml:"tenantId"`
	// The ID of the Azure Subscription that the cluster is deployed in
	SubscriptionID string `json:"subscriptionId" yaml:"subscriptionId"`
	// The name of the resource group that the cluster is deployed in
	ResourceGroup string `json:"resourceGroup" yaml:"resourceGroup"`
	// The location of the resource group that the cluster is deployed in
	Location string `json:"location" yaml:"location"`
	// The name of the VNet that the cluster is deployed in
	VnetName string `json:"vnetName" yaml:"vnetName"`
	// The name of the resource group that the Vnet is deployed in
	VnetResourceGroup string `json:"vnetResourceGroup" yaml:"vnetResourceGroup"`
	// The name of the subnet that the cluster is deployed in
	SubnetName string `json:"subnetName" yaml:"subnetName"`
	// The name of the security group attached to the cluster's subnet
	SecurityGroupName string `json:"securityGroupName" yaml:"securityGroupName"`
	// (Optional in 1.6) The name of the route table attached to the subnet that the cluster is deployed in
	RouteTableName string `json:"routeTableName" yaml:"routeTableName"`
	// (Optional) The name of the availability set that should be used as the load balancer backend
	// If this is set, the Azure cloudprovider will only add nodes from that availability set to the load
	// balancer backend pool. If this is not set, and multiple agent pools (availability sets) are used, then
	// the cloudprovider will try to add all nodes to a single backend pool which is forbidden.
	// In other words, if you use multiple agent pools (availability sets), you MUST set this field.
	PrimaryAvailabilitySetName string `json:"primaryAvailabilitySetName" yaml:"primaryAvailabilitySetName"`
	// The type of azure nodes. Candidate valudes are: vmss and standard.
	// If not set, it will be default to standard.
	VMType string `json:"vmType" yaml:"vmType"`
	// The name of the scale set that should be used as the load balancer backend.
	// If this is set, the Azure cloudprovider will only add nodes from that scale set to the load
	// balancer backend pool. If this is not set, and multiple agent pools (scale sets) are used, then
	// the cloudprovider will try to add all nodes to a single backend pool which is forbidden.
	// In other words, if you use multiple agent pools (scale sets), you MUST set this field.
	PrimaryScaleSetName string `json:"primaryScaleSetName" yaml:"primaryScaleSetName"`

	// The ClientID for an AAD application with RBAC access to talk to Azure RM APIs
	AADClientID string `json:"aadClientId" yaml:"aadClientId"`
	// The ClientSecret for an AAD application with RBAC access to talk to Azure RM APIs
	AADClientSecret string `json:"aadClientSecret" yaml:"aadClientSecret"`
	// The path of a client certificate for an AAD application with RBAC access to talk to Azure RM APIs
	AADClientCertPath string `json:"aadClientCertPath" yaml:"aadClientCertPath"`
	// The password of the client certificate for an AAD application with RBAC access to talk to Azure RM APIs
	AADClientCertPassword string `json:"aadClientCertPassword" yaml:"aadClientCertPassword"`
	// Enable exponential backoff to manage resource request retries
	CloudProviderBackoff bool `json:"cloudProviderBackoff" yaml:"cloudProviderBackoff"`
	// Backoff retry limit
	CloudProviderBackoffRetries int `json:"cloudProviderBackoffRetries" yaml:"cloudProviderBackoffRetries"`
	// Backoff exponent
	CloudProviderBackoffExponent float64 `json:"cloudProviderBackoffExponent" yaml:"cloudProviderBackoffExponent"`
	// Backoff duration
	CloudProviderBackoffDuration int `json:"cloudProviderBackoffDuration" yaml:"cloudProviderBackoffDuration"`
	// Backoff jitter
	CloudProviderBackoffJitter float64 `json:"cloudProviderBackoffJitter" yaml:"cloudProviderBackoffJitter"`
	// Enable rate limiting
	CloudProviderRateLimit bool `json:"cloudProviderRateLimit" yaml:"cloudProviderRateLimit"`
	// Rate limit QPS
	CloudProviderRateLimitQPS float32 `json:"cloudProviderRateLimitQPS" yaml:"cloudProviderRateLimitQPS"`
	// Rate limit Bucket Size
	CloudProviderRateLimitBucket int `json:"cloudProviderRateLimitBucket" yaml:"cloudProviderRateLimitBucket"`

	// Use managed service identity for the virtual machine to access Azure ARM APIs
	UseManagedIdentityExtension bool `json:"useManagedIdentityExtension"`

	// Maximum allowed LoadBalancer Rule Count is the limit enforced by Azure Load balancer
	MaximumLoadBalancerRuleCount int `json:"maximumLoadBalancerRuleCount"`
}

// VirtualMachinesClient defines needed functions for azure compute.VirtualMachinesClient
type VirtualMachinesClient interface {
	CreateOrUpdate(resourceGroupName string, VMName string, parameters compute.VirtualMachine, cancel <-chan struct{}) (<-chan compute.VirtualMachine, <-chan error)
	Get(resourceGroupName string, VMName string, expand compute.InstanceViewTypes) (result compute.VirtualMachine, err error)
	List(resourceGroupName string) (result compute.VirtualMachineListResult, err error)
	ListNextResults(lastResults compute.VirtualMachineListResult) (result compute.VirtualMachineListResult, err error)
}

// InterfacesClient defines needed functions for azure network.InterfacesClient
type InterfacesClient interface {
	CreateOrUpdate(resourceGroupName string, networkInterfaceName string, parameters network.Interface, cancel <-chan struct{}) (<-chan network.Interface, <-chan error)
	Get(resourceGroupName string, networkInterfaceName string, expand string) (result network.Interface, err error)
	GetVirtualMachineScaleSetNetworkInterface(resourceGroupName string, virtualMachineScaleSetName string, virtualmachineIndex string, networkInterfaceName string, expand string) (result network.Interface, err error)
}

// LoadBalancersClient defines needed functions for azure network.LoadBalancersClient
type LoadBalancersClient interface {
	CreateOrUpdate(resourceGroupName string, loadBalancerName string, parameters network.LoadBalancer, cancel <-chan struct{}) (<-chan network.LoadBalancer, <-chan error)
	Delete(resourceGroupName string, loadBalancerName string, cancel <-chan struct{}) (<-chan autorest.Response, <-chan error)
	Get(resourceGroupName string, loadBalancerName string, expand string) (result network.LoadBalancer, err error)
	List(resourceGroupName string) (result network.LoadBalancerListResult, err error)
	ListNextResults(lastResult network.LoadBalancerListResult) (result network.LoadBalancerListResult, err error)
}

// PublicIPAddressesClient defines needed functions for azure network.PublicIPAddressesClient
type PublicIPAddressesClient interface {
	CreateOrUpdate(resourceGroupName string, publicIPAddressName string, parameters network.PublicIPAddress, cancel <-chan struct{}) (<-chan network.PublicIPAddress, <-chan error)
	Delete(resourceGroupName string, publicIPAddressName string, cancel <-chan struct{}) (<-chan autorest.Response, <-chan error)
	Get(resourceGroupName string, publicIPAddressName string, expand string) (result network.PublicIPAddress, err error)
	List(resourceGroupName string) (result network.PublicIPAddressListResult, err error)
	ListNextResults(lastResults network.PublicIPAddressListResult) (result network.PublicIPAddressListResult, err error)
}

// SubnetsClient defines needed functions for azure network.SubnetsClient
type SubnetsClient interface {
	CreateOrUpdate(resourceGroupName string, virtualNetworkName string, subnetName string, subnetParameters network.Subnet, cancel <-chan struct{}) (<-chan network.Subnet, <-chan error)
	Delete(resourceGroupName string, virtualNetworkName string, subnetName string, cancel <-chan struct{}) (<-chan autorest.Response, <-chan error)
	Get(resourceGroupName string, virtualNetworkName string, subnetName string, expand string) (result network.Subnet, err error)
	List(resourceGroupName string, virtualNetworkName string) (result network.SubnetListResult, err error)
}

// SecurityGroupsClient defines needed functions for azure network.SecurityGroupsClient
type SecurityGroupsClient interface {
	CreateOrUpdate(resourceGroupName string, networkSecurityGroupName string, parameters network.SecurityGroup, cancel <-chan struct{}) (<-chan network.SecurityGroup, <-chan error)
	Delete(resourceGroupName string, networkSecurityGroupName string, cancel <-chan struct{}) (<-chan autorest.Response, <-chan error)
	Get(resourceGroupName string, networkSecurityGroupName string, expand string) (result network.SecurityGroup, err error)
	List(resourceGroupName string) (result network.SecurityGroupListResult, err error)
}

// VirtualMachineScaleSetsClient defines needed functions for azure compute.VirtualMachineScaleSetsClient
type VirtualMachineScaleSetsClient interface {
	CreateOrUpdate(resourceGroupName string, VMScaleSetName string, parameters compute.VirtualMachineScaleSet, cancel <-chan struct{}) (<-chan compute.VirtualMachineScaleSet, <-chan error)
	Get(resourceGroupName string, VMScaleSetName string) (result compute.VirtualMachineScaleSet, err error)
	List(resourceGroupName string) (result compute.VirtualMachineScaleSetListResult, err error)
	ListNextResults(lastResults compute.VirtualMachineScaleSetListResult) (result compute.VirtualMachineScaleSetListResult, err error)
	UpdateInstances(resourceGroupName string, VMScaleSetName string, VMInstanceIDs compute.VirtualMachineScaleSetVMInstanceRequiredIDs, cancel <-chan struct{}) (<-chan compute.OperationStatusResponse, <-chan error)
}

// VirtualMachineScaleSetVMsClient defines needed functions for azure compute.VirtualMachineScaleSetVMsClient
type VirtualMachineScaleSetVMsClient interface {
	Get(resourceGroupName string, VMScaleSetName string, instanceID string) (result compute.VirtualMachineScaleSetVM, err error)
	GetInstanceView(resourceGroupName string, VMScaleSetName string, instanceID string) (result compute.VirtualMachineScaleSetVMInstanceView, err error)
	List(resourceGroupName string, virtualMachineScaleSetName string, filter string, selectParameter string, expand string) (result compute.VirtualMachineScaleSetVMListResult, err error)
	ListNextResults(lastResults compute.VirtualMachineScaleSetVMListResult) (result compute.VirtualMachineScaleSetVMListResult, err error)
}

// Cloud holds the config and clients
type Cloud struct {
	Config
	Environment              azure.Environment
	RoutesClient             network.RoutesClient
	SubnetsClient            SubnetsClient
	InterfacesClient         InterfacesClient
	RouteTablesClient        network.RouteTablesClient
	LoadBalancerClient       LoadBalancersClient
	PublicIPAddressesClient  PublicIPAddressesClient
	SecurityGroupsClient     SecurityGroupsClient
	VirtualMachinesClient    VirtualMachinesClient
	StorageAccountClient     storage.AccountsClient
	DisksClient              disk.DisksClient
	operationPollRateLimiter flowcontrol.RateLimiter
	resourceRequestBackoff   wait.Backoff
	vmSet                    VMSet

	// Clients for vmss.
	VirtualMachineScaleSetsClient   VirtualMachineScaleSetsClient
	VirtualMachineScaleSetVMsClient VirtualMachineScaleSetVMsClient

	*BlobDiskController
	*ManagedDiskController
	*controllerCommon
}

func init() {
	cloudprovider.RegisterCloudProvider(CloudProviderName, NewCloud)
}

// decodePkcs12 decodes a PKCS#12 client certificate by extracting the public certificate and
// the private RSA key
func decodePkcs12(pkcs []byte, password string) (*x509.Certificate, *rsa.PrivateKey, error) {
	privateKey, certificate, err := pkcs12.Decode(pkcs, password)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding the PKCS#12 client certificate: %v", err)
	}
	rsaPrivateKey, isRsaKey := privateKey.(*rsa.PrivateKey)
	if !isRsaKey {
		return nil, nil, fmt.Errorf("PKCS#12 certificate must contain a RSA private key")
	}

	return certificate, rsaPrivateKey, nil
}

// GetServicePrincipalToken creates a new service principal token based on the configuration
func GetServicePrincipalToken(config *Config, env *azure.Environment) (*adal.ServicePrincipalToken, error) {
	oauthConfig, err := adal.NewOAuthConfig(env.ActiveDirectoryEndpoint, config.TenantID)
	if err != nil {
		return nil, fmt.Errorf("creating the OAuth config: %v", err)
	}

	if config.UseManagedIdentityExtension {
		glog.V(2).Infoln("azure: using managed identity extension to retrieve access token")
		msiEndpoint, err := adal.GetMSIVMEndpoint()
		if err != nil {
			return nil, fmt.Errorf("Getting the managed service identity endpoint: %v", err)
		}
		return adal.NewServicePrincipalTokenFromMSI(
			msiEndpoint,
			env.ServiceManagementEndpoint)
	}

	if len(config.AADClientSecret) > 0 {
		glog.V(2).Infoln("azure: using client_id+client_secret to retrieve access token")
		return adal.NewServicePrincipalToken(
			*oauthConfig,
			config.AADClientID,
			config.AADClientSecret,
			env.ServiceManagementEndpoint)
	}

	if len(config.AADClientCertPath) > 0 && len(config.AADClientCertPassword) > 0 {
		glog.V(2).Infoln("azure: using jwt client_assertion (client_cert+client_private_key) to retrieve access token")
		certData, err := ioutil.ReadFile(config.AADClientCertPath)
		if err != nil {
			return nil, fmt.Errorf("reading the client certificate from file %s: %v", config.AADClientCertPath, err)
		}
		certificate, privateKey, err := decodePkcs12(certData, config.AADClientCertPassword)
		if err != nil {
			return nil, fmt.Errorf("decoding the client certificate: %v", err)
		}
		return adal.NewServicePrincipalTokenFromCertificate(
			*oauthConfig,
			config.AADClientID,
			certificate,
			privateKey,
			env.ServiceManagementEndpoint)
	}

	return nil, fmt.Errorf("No credentials provided for AAD application %s", config.AADClientID)
}

// NewCloud returns a Cloud with initialized clients
func NewCloud(configReader io.Reader) (cloudprovider.Interface, error) {
	config, env, err := ParseConfig(configReader)
	if err != nil {
		return nil, err
	}
	az := Cloud{
		Config:      *config,
		Environment: *env,
	}

	servicePrincipalToken, err := GetServicePrincipalToken(config, env)
	if err != nil {
		return nil, err
	}

	subnetsClient := network.NewSubnetsClient(az.SubscriptionID)
	subnetsClient.BaseURI = az.Environment.ResourceManagerEndpoint
	subnetsClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	subnetsClient.PollingDelay = 5 * time.Second
	configureUserAgent(&subnetsClient.Client)
	az.SubnetsClient = subnetsClient

	az.RouteTablesClient = network.NewRouteTablesClient(az.SubscriptionID)
	az.RouteTablesClient.BaseURI = az.Environment.ResourceManagerEndpoint
	az.RouteTablesClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	az.RouteTablesClient.PollingDelay = 5 * time.Second
	configureUserAgent(&az.RouteTablesClient.Client)

	az.RoutesClient = network.NewRoutesClient(az.SubscriptionID)
	az.RoutesClient.BaseURI = az.Environment.ResourceManagerEndpoint
	az.RoutesClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	az.RoutesClient.PollingDelay = 5 * time.Second
	configureUserAgent(&az.RoutesClient.Client)

	interfacesClient := network.NewInterfacesClient(az.SubscriptionID)
	interfacesClient.BaseURI = az.Environment.ResourceManagerEndpoint
	interfacesClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	interfacesClient.PollingDelay = 5 * time.Second
	configureUserAgent(&interfacesClient.Client)
	az.InterfacesClient = interfacesClient

	loadBalancerClient := network.NewLoadBalancersClient(az.SubscriptionID)
	loadBalancerClient.BaseURI = az.Environment.ResourceManagerEndpoint
	loadBalancerClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	loadBalancerClient.PollingDelay = 5 * time.Second
	configureUserAgent(&loadBalancerClient.Client)
	az.LoadBalancerClient = loadBalancerClient

	virtualMachinesClient := compute.NewVirtualMachinesClient(az.SubscriptionID)
	virtualMachinesClient.BaseURI = az.Environment.ResourceManagerEndpoint
	virtualMachinesClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	virtualMachinesClient.PollingDelay = 5 * time.Second
	configureUserAgent(&virtualMachinesClient.Client)
	az.VirtualMachinesClient = virtualMachinesClient

	publicIPAddressClient := network.NewPublicIPAddressesClient(az.SubscriptionID)
	publicIPAddressClient.BaseURI = az.Environment.ResourceManagerEndpoint
	publicIPAddressClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	publicIPAddressClient.PollingDelay = 5 * time.Second
	configureUserAgent(&publicIPAddressClient.Client)
	az.PublicIPAddressesClient = publicIPAddressClient

	securityGroupsClient := network.NewSecurityGroupsClient(az.SubscriptionID)
	securityGroupsClient.BaseURI = az.Environment.ResourceManagerEndpoint
	securityGroupsClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	securityGroupsClient.PollingDelay = 5 * time.Second
	configureUserAgent(&securityGroupsClient.Client)
	az.SecurityGroupsClient = securityGroupsClient

	virtualMachineScaleSetVMsClient := compute.NewVirtualMachineScaleSetVMsClient(az.SubscriptionID)
	virtualMachineScaleSetVMsClient.BaseURI = az.Environment.ResourceManagerEndpoint
	virtualMachineScaleSetVMsClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	virtualMachineScaleSetVMsClient.PollingDelay = 5 * time.Second
	configureUserAgent(&virtualMachineScaleSetVMsClient.Client)
	az.VirtualMachineScaleSetVMsClient = virtualMachineScaleSetVMsClient

	virtualMachineScaleSetsClient := compute.NewVirtualMachineScaleSetsClient(az.SubscriptionID)
	virtualMachineScaleSetsClient.BaseURI = az.Environment.ResourceManagerEndpoint
	virtualMachineScaleSetsClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	virtualMachineScaleSetsClient.PollingDelay = 5 * time.Second
	configureUserAgent(&virtualMachineScaleSetsClient.Client)
	az.VirtualMachineScaleSetsClient = virtualMachineScaleSetsClient

	az.StorageAccountClient = storage.NewAccountsClientWithBaseURI(az.Environment.ResourceManagerEndpoint, az.SubscriptionID)
	az.StorageAccountClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	configureUserAgent(&az.StorageAccountClient.Client)

	az.DisksClient = disk.NewDisksClientWithBaseURI(az.Environment.ResourceManagerEndpoint, az.SubscriptionID)
	az.DisksClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	configureUserAgent(&az.DisksClient.Client)

	// Conditionally configure rate limits
	if az.CloudProviderRateLimit {
		// Assign rate limit defaults if no configuration was passed in
		if az.CloudProviderRateLimitQPS == 0 {
			az.CloudProviderRateLimitQPS = rateLimitQPSDefault
		}
		if az.CloudProviderRateLimitBucket == 0 {
			az.CloudProviderRateLimitBucket = rateLimitBucketDefault
		}
		az.operationPollRateLimiter = flowcontrol.NewTokenBucketRateLimiter(
			az.CloudProviderRateLimitQPS,
			az.CloudProviderRateLimitBucket)
		glog.V(2).Infof("Azure cloudprovider using rate limit config: QPS=%g, bucket=%d",
			az.CloudProviderRateLimitQPS,
			az.CloudProviderRateLimitBucket)
	} else {
		// if rate limits are configured off, az.operationPollRateLimiter.Accept() is a no-op
		az.operationPollRateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()
	}

	// Conditionally configure resource request backoff
	if az.CloudProviderBackoff {
		// Assign backoff defaults if no configuration was passed in
		if az.CloudProviderBackoffRetries == 0 {
			az.CloudProviderBackoffRetries = backoffRetriesDefault
		}
		if az.CloudProviderBackoffExponent == 0 {
			az.CloudProviderBackoffExponent = backoffExponentDefault
		}
		if az.CloudProviderBackoffDuration == 0 {
			az.CloudProviderBackoffDuration = backoffDurationDefault
		}
		if az.CloudProviderBackoffJitter == 0 {
			az.CloudProviderBackoffJitter = backoffJitterDefault
		}
		az.resourceRequestBackoff = wait.Backoff{
			Steps:    az.CloudProviderBackoffRetries,
			Factor:   az.CloudProviderBackoffExponent,
			Duration: time.Duration(az.CloudProviderBackoffDuration) * time.Second,
			Jitter:   az.CloudProviderBackoffJitter,
		}
		glog.V(2).Infof("Azure cloudprovider using retry backoff: retries=%d, exponent=%f, duration=%d, jitter=%f",
			az.CloudProviderBackoffRetries,
			az.CloudProviderBackoffExponent,
			az.CloudProviderBackoffDuration,
			az.CloudProviderBackoffJitter)
	}

	if az.MaximumLoadBalancerRuleCount == 0 {
		az.MaximumLoadBalancerRuleCount = maximumLoadBalancerRuleCount
	}

	if az.Config.VMType == vmTypeVMSS {
		az.vmSet = newScaleSet(&az)
	} else {
		az.vmSet = newAvailabilitySet(&az)
	}

	if err := initDiskControllers(&az); err != nil {
		return nil, err
	}
	return &az, nil
}

// ParseConfig returns a parsed configuration and azure.Environment for an Azure cloudprovider config file
func ParseConfig(configReader io.Reader) (*Config, *azure.Environment, error) {
	var config Config
	var env azure.Environment

	if configReader == nil {
		return &config, &env, nil
	}

	configContents, err := ioutil.ReadAll(configReader)
	if err != nil {
		return nil, nil, err
	}
	err = yaml.Unmarshal(configContents, &config)
	if err != nil {
		return nil, nil, err
	}

	if config.Cloud == "" {
		env = azure.PublicCloud
	} else {
		env, err = azure.EnvironmentFromName(config.Cloud)
		if err != nil {
			return nil, nil, err
		}
	}

	if config.VMType != "" {
		config.VMType = strings.ToLower(config.VMType)
	}

	return &config, &env, nil
}

// Initialize passes a Kubernetes clientBuilder interface to the cloud provider
func (az *Cloud) Initialize(clientBuilder controller.ControllerClientBuilder) {}

// LoadBalancer returns a balancer interface. Also returns true if the interface is supported, false otherwise.
func (az *Cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return az, true
}

// Instances returns an instances interface. Also returns true if the interface is supported, false otherwise.
func (az *Cloud) Instances() (cloudprovider.Instances, bool) {
	return az, true
}

// Zones returns a zones interface. Also returns true if the interface is supported, false otherwise.
func (az *Cloud) Zones() (cloudprovider.Zones, bool) {
	return az, true
}

// Clusters returns a clusters interface.  Also returns true if the interface is supported, false otherwise.
func (az *Cloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// Routes returns a routes interface along with whether the interface is supported.
func (az *Cloud) Routes() (cloudprovider.Routes, bool) {
	return az, true
}

// HasClusterID returns true if the cluster has a clusterID
func (az *Cloud) HasClusterID() bool {
	return true
}

// ProviderName returns the cloud provider ID.
func (az *Cloud) ProviderName() string {
	return CloudProviderName
}

// configureUserAgent configures the autorest client with a user agent that
// includes "kubernetes" and the full kubernetes git version string
// example:
// Azure-SDK-for-Go/7.0.1-beta arm-network/2016-09-01; kubernetes-cloudprovider/v1.7.0-alpha.2.711+a2fadef8170bb0-dirty;
func configureUserAgent(client *autorest.Client) {
	k8sVersion := version.Get().GitVersion
	client.UserAgent = fmt.Sprintf("%s; kubernetes-cloudprovider/%s", client.UserAgent, k8sVersion)
}

func initDiskControllers(az *Cloud) error {
	// Common controller contains the function
	// needed by both blob disk and managed disk controllers

	common := &controllerCommon{
		aadResourceEndPoint:   az.Environment.ServiceManagementEndpoint,
		clientID:              az.AADClientID,
		clientSecret:          az.AADClientSecret,
		location:              az.Location,
		storageEndpointSuffix: az.Environment.StorageEndpointSuffix,
		managementEndpoint:    az.Environment.ResourceManagerEndpoint,
		resourceGroup:         az.ResourceGroup,
		tenantID:              az.TenantID,
		tokenEndPoint:         az.Environment.ActiveDirectoryEndpoint,
		subscriptionID:        az.SubscriptionID,
		cloud:                 az,
	}

	// BlobDiskController: contains the function needed to
	// create/attach/detach/delete blob based (unmanaged disks)
	blobController, err := newBlobDiskController(common)
	if err != nil {
		return fmt.Errorf("AzureDisk -  failed to init Blob Disk Controller with error (%s)", err.Error())
	}

	// ManagedDiskController: contains the functions needed to
	// create/attach/detach/delete managed disks
	managedController, err := newManagedDiskController(common)
	if err != nil {
		return fmt.Errorf("AzureDisk -  failed to init Managed  Disk Controller with error (%s)", err.Error())
	}

	az.BlobDiskController = blobController
	az.ManagedDiskController = managedController
	az.controllerCommon = common

	return nil
}
