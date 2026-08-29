package connectmac

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type AWSClient interface {
	CallerIdentity(ctx context.Context) (CallerIdentity, error)
	DescribeStatus(ctx context.Context, plan MacPlan) (AWSStatus, error)
	DescribeAdoptionCandidates(ctx context.Context, plan MacPlan) (AWSStatus, error)
	DescribeHostByID(ctx context.Context, hostID string) (DedicatedHostStatus, error)
	DescribeAllHosts(ctx context.Context) ([]DedicatedHostStatus, error)
	DedicatedHostQuotas(ctx context.Context, instanceTypes []string) (map[string]float64, error)
	TagResources(ctx context.Context, resourceIDs []string, tags []AWSTagConfig) error
	InstanceTypeOfferings(ctx context.Context, instanceType string) ([]string, error)
	SubnetAvailabilityZoneID(ctx context.Context, subnetID string) (string, error)
	AllocateHost(ctx context.Context, plan MacPlan, availabilityZoneID, instanceType string) (string, error)
	RunInstance(ctx context.Context, plan MacPlan, hostID, instanceType, amiID string) (string, error)
	VerifyElasticIPOwner(ctx context.Context, plan MacPlan) (ElasticIP, error)
	AssociateElasticIP(ctx context.Context, allocationID, instanceID string) (string, error)
	DisassociateElasticIP(ctx context.Context, associationID string) error
	TerminateInstance(ctx context.Context, instanceID string) error
	ReleaseHost(ctx context.Context, hostID string) error
}

type CallerIdentity struct {
	Account string
	ARN     string
	UserID  string
}

type AWSStatus struct {
	CallerIdentity CallerIdentity
	Hosts          []DedicatedHostStatus
	Instances      []InstanceStatus
	ElasticIP      ElasticIP
}

type DedicatedHostStatus struct {
	HostID       string
	State        string
	InstanceType string
	ZoneID       string
	CreatedAt    string
	InstanceIDs  []string
	Tags         []AWSTagConfig
}

type InstanceStatus struct {
	InstanceID          string
	State               string
	InstanceType        string
	HostID              string
	PublicIP            string
	SystemStatus        string
	InstanceStatusCheck string
	EBSStatus           string
	Tags                []AWSTagConfig
}

type ElasticIP struct {
	AllocationID  string
	AssociationID string
	InstanceID    string
	PublicIP      string
	Tags          []AWSTagConfig
}

type RealAWSClient struct {
	ec2           *ec2.Client
	servicequotas *servicequotas.Client
	sts           *sts.Client
}

func NewRealAWSClient(ctx context.Context, plan MacPlan) (RealAWSClient, error) {
	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(plan.Region),
	}
	if plan.AWSProfile != "" {
		options = append(options, awsconfig.WithSharedConfigProfile(plan.AWSProfile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return RealAWSClient{}, fmt.Errorf("load aws config: %w", err)
	}
	return RealAWSClient{
		ec2:           ec2.NewFromConfig(cfg),
		servicequotas: servicequotas.NewFromConfig(cfg),
		sts:           sts.NewFromConfig(cfg),
	}, nil
}

func (c RealAWSClient) CallerIdentity(ctx context.Context) (CallerIdentity, error) {
	out, err := c.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return CallerIdentity{}, fmt.Errorf("sts get caller identity: %w", err)
	}
	return CallerIdentity{
		Account: aws.ToString(out.Account),
		ARN:     aws.ToString(out.Arn),
		UserID:  aws.ToString(out.UserId),
	}, nil
}

func (c RealAWSClient) DescribeStatus(ctx context.Context, plan MacPlan) (AWSStatus, error) {
	identity, err := c.CallerIdentity(ctx)
	if err != nil {
		return AWSStatus{}, err
	}
	eip, err := c.describeElasticIP(ctx, plan.ElasticIPAllocationID)
	if err != nil {
		return AWSStatus{}, err
	}
	hosts, err := c.describeManagedHosts(ctx, plan)
	if err != nil {
		return AWSStatus{}, err
	}
	instances, err := c.describeManagedInstances(ctx, plan)
	if err != nil {
		return AWSStatus{}, err
	}
	if len(instances) == 0 && eip.InstanceID != "" {
		instance, err := c.describeInstanceByID(ctx, eip.InstanceID)
		if err != nil {
			return AWSStatus{}, err
		}
		instances = append(instances, instance)
	}
	if len(hosts) == 0 {
		hostIDs := hostIDsFromInstances(instances)
		if len(hostIDs) > 0 {
			hosts, err = c.describeHostsByID(ctx, hostIDs)
			if err != nil {
				return AWSStatus{}, err
			}
		}
	}
	if err := c.populateInstanceChecks(ctx, instances); err != nil {
		return AWSStatus{}, err
	}
	return AWSStatus{
		CallerIdentity: identity,
		Hosts:          hosts,
		Instances:      instances,
		ElasticIP:      eip,
	}, nil
}

func (c RealAWSClient) DescribeAdoptionCandidates(ctx context.Context, plan MacPlan) (AWSStatus, error) {
	identity, err := c.CallerIdentity(ctx)
	if err != nil {
		return AWSStatus{}, err
	}
	hosts, err := c.describeHostsByName(ctx, plan.ResourceName)
	if err != nil {
		return AWSStatus{}, err
	}
	instances, err := c.describeInstancesByName(ctx, plan.ResourceName)
	if err != nil {
		return AWSStatus{}, err
	}
	eip, err := c.describeElasticIP(ctx, plan.ElasticIPAllocationID)
	if err != nil {
		return AWSStatus{}, err
	}
	if len(instances) == 0 && eip.InstanceID != "" {
		instance, err := c.describeInstanceByID(ctx, eip.InstanceID)
		if err != nil {
			return AWSStatus{}, err
		}
		instances = append(instances, instance)
	}
	if err := c.populateInstanceChecks(ctx, instances); err != nil {
		return AWSStatus{}, err
	}
	return AWSStatus{
		CallerIdentity: identity,
		Hosts:          hosts,
		Instances:      instances,
		ElasticIP:      eip,
	}, nil
}

func (c RealAWSClient) DescribeHostByID(ctx context.Context, hostID string) (DedicatedHostStatus, error) {
	hosts, err := c.describeHostsByID(ctx, []string{hostID})
	if err != nil {
		return DedicatedHostStatus{}, err
	}
	if len(hosts) == 0 {
		return DedicatedHostStatus{}, fmt.Errorf("dedicated host %s not found", hostID)
	}
	return hosts[0], nil
}

func (c RealAWSClient) DescribeAllHosts(ctx context.Context) ([]DedicatedHostStatus, error) {
	var hosts []DedicatedHostStatus
	input := &ec2.DescribeHostsInput{}
	for {
		out, err := c.ec2.DescribeHosts(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("describe all hosts: %w", err)
		}
		for _, host := range out.Hosts {
			hosts = append(hosts, dedicatedHostStatusFromEC2(host))
		}
		if out.NextToken == nil || aws.ToString(out.NextToken) == "" {
			break
		}
		input.NextToken = out.NextToken
	}
	return hosts, nil
}

func (c RealAWSClient) DedicatedHostQuotas(ctx context.Context, instanceTypes []string) (map[string]float64, error) {
	quotaNames := make(map[string]string, len(instanceTypes))
	for _, instanceType := range instanceTypes {
		quotaNames[dedicatedHostQuotaName(instanceType)] = instanceType
	}
	quotas := make(map[string]float64, len(instanceTypes))
	input := &servicequotas.ListServiceQuotasInput{
		ServiceCode: aws.String("ec2"),
	}
	for {
		out, err := c.servicequotas.ListServiceQuotas(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("list EC2 service quotas: %w", err)
		}
		for _, quota := range out.Quotas {
			instanceType, ok := quotaNames[aws.ToString(quota.QuotaName)]
			if !ok {
				continue
			}
			if quota.Value != nil {
				quotas[instanceType] = *quota.Value
			}
		}
		if out.NextToken == nil || aws.ToString(out.NextToken) == "" {
			break
		}
		input.NextToken = out.NextToken
	}
	return quotas, nil
}

func (c RealAWSClient) TagResources(ctx context.Context, resourceIDs []string, tags []AWSTagConfig) error {
	if len(resourceIDs) == 0 {
		return nil
	}
	_, err := c.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: resourceIDs,
		Tags:      toEC2Tags(tags),
	})
	if err != nil {
		return fmt.Errorf("tag resources %v: %w", resourceIDs, err)
	}
	return nil
}

func (c RealAWSClient) SubnetAvailabilityZoneID(ctx context.Context, subnetID string) (string, error) {
	out, err := c.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: []string{subnetID},
	})
	if err != nil {
		return "", fmt.Errorf("describe subnet %s: %w", subnetID, err)
	}
	if len(out.Subnets) == 0 {
		return "", fmt.Errorf("subnet %s not found", subnetID)
	}
	return aws.ToString(out.Subnets[0].AvailabilityZoneId), nil
}

func (c RealAWSClient) InstanceTypeOfferings(ctx context.Context, instanceType string) ([]string, error) {
	out, err := c.ec2.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: ec2types.LocationTypeAvailabilityZoneId,
		Filters: []ec2types.Filter{{
			Name:   aws.String("instance-type"),
			Values: []string{instanceType},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("describe instance type offerings for %s: %w", instanceType, err)
	}
	locations := make([]string, 0, len(out.InstanceTypeOfferings))
	for _, offering := range out.InstanceTypeOfferings {
		if location := aws.ToString(offering.Location); location != "" {
			locations = append(locations, location)
		}
	}
	return locations, nil
}

func (c RealAWSClient) AllocateHost(ctx context.Context, plan MacPlan, availabilityZoneID, instanceType string) (string, error) {
	out, err := c.ec2.AllocateHosts(ctx, &ec2.AllocateHostsInput{
		AutoPlacement:      ec2types.AutoPlacementOff,
		AvailabilityZoneId: aws.String(availabilityZoneID),
		HostMaintenance:    ec2types.HostMaintenanceOff,
		InstanceType:       aws.String(instanceType),
		Quantity:           aws.Int32(1),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeDedicatedHost,
			Tags:         toEC2Tags(plan.Tags),
		}},
	})
	if err != nil {
		return "", fmt.Errorf("allocate host %s in %s: %w", instanceType, availabilityZoneID, err)
	}
	if len(out.HostIds) == 0 {
		return "", fmt.Errorf("allocate host returned no host id")
	}
	return out.HostIds[0], nil
}

func (c RealAWSClient) RunInstance(ctx context.Context, plan MacPlan, hostID, instanceType, amiID string) (string, error) {
	out, err := c.ec2.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: ec2types.InstanceType(instanceType),
		KeyName:      aws.String(plan.KeyName),
		MaxCount:     aws.Int32(1),
		MinCount:     aws.Int32(1),
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{{
			AssociatePublicIpAddress: aws.Bool(false),
			DeviceIndex:              aws.Int32(0),
			Groups:                   []string{plan.SecurityGroupID},
			SubnetId:                 aws.String(plan.SubnetID),
		}},
		Placement: &ec2types.Placement{
			Affinity: aws.String("host"),
			HostId:   aws.String(hostID),
			Tenancy:  ec2types.TenancyHost,
		},
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags:         toEC2Tags(plan.Tags),
		}},
	})
	if err != nil {
		return "", fmt.Errorf("run instance on host %s: %w", hostID, err)
	}
	if len(out.Instances) == 0 {
		return "", fmt.Errorf("run instances returned no instance")
	}
	return aws.ToString(out.Instances[0].InstanceId), nil
}

func (c RealAWSClient) VerifyElasticIPOwner(ctx context.Context, plan MacPlan) (ElasticIP, error) {
	eip, err := c.describeElasticIP(ctx, plan.ElasticIPAllocationID)
	if err != nil {
		return ElasticIP{}, err
	}
	if !hasTag(eip.Tags, plan.ElasticIPOwnerTag.Key, plan.ElasticIPOwnerTag.Value) {
		return ElasticIP{}, fmt.Errorf("elastic ip %s is missing required owner tag %s=%s", plan.ElasticIPAllocationID, plan.ElasticIPOwnerTag.Key, plan.ElasticIPOwnerTag.Value)
	}
	return eip, nil
}

func (c RealAWSClient) AssociateElasticIP(ctx context.Context, allocationID, instanceID string) (string, error) {
	out, err := c.ec2.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId: aws.String(allocationID),
		InstanceId:   aws.String(instanceID),
	})
	if err != nil {
		return "", fmt.Errorf("associate elastic ip %s to %s: %w", allocationID, instanceID, err)
	}
	return aws.ToString(out.AssociationId), nil
}

func (c RealAWSClient) DisassociateElasticIP(ctx context.Context, associationID string) error {
	_, err := c.ec2.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{
		AssociationId: aws.String(associationID),
	})
	if err != nil {
		return fmt.Errorf("disassociate elastic ip association %s: %w", associationID, err)
	}
	return nil
}

func (c RealAWSClient) TerminateInstance(ctx context.Context, instanceID string) error {
	input := &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	}
	_, err := c.ec2.TerminateInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("terminate instance %s: %w", instanceID, err)
	}
	return nil
}

func (c RealAWSClient) ReleaseHost(ctx context.Context, hostID string) error {
	out, err := c.ec2.ReleaseHosts(ctx, &ec2.ReleaseHostsInput{
		HostIds: []string{hostID},
	})
	if err != nil {
		return fmt.Errorf("release host %s: %w", hostID, err)
	}
	if err := validateReleaseHostOutput(hostID, out); err != nil {
		return fmt.Errorf("release host %s: %w", hostID, err)
	}
	return nil
}

func validateReleaseHostOutput(hostID string, out *ec2.ReleaseHostsOutput) error {
	if out == nil {
		return errors.New("AWS did not confirm host release")
	}
	successful := false
	for _, releasedHostID := range out.Successful {
		if releasedHostID == hostID {
			successful = true
			break
		}
	}
	var unsuccessful *ec2types.UnsuccessfulItem
	for i := range out.Unsuccessful {
		if aws.ToString(out.Unsuccessful[i].ResourceId) == hostID {
			unsuccessful = &out.Unsuccessful[i]
			break
		}
	}
	if successful && unsuccessful != nil {
		return errors.New("AWS returned contradictory host release results")
	}
	if unsuccessful != nil {
		code, message := "unknown error", "host release was unsuccessful"
		if unsuccessful.Error != nil {
			if value := strings.TrimSpace(aws.ToString(unsuccessful.Error.Code)); value != "" {
				code = value
			}
			if value := strings.TrimSpace(aws.ToString(unsuccessful.Error.Message)); value != "" {
				message = value
			}
		}
		return fmt.Errorf("%s: %s", code, message)
	}
	if !successful {
		return errors.New("AWS did not confirm host release")
	}
	return nil
}

func (c RealAWSClient) describeManagedHosts(ctx context.Context, plan MacPlan) ([]DedicatedHostStatus, error) {
	out, err := c.ec2.DescribeHosts(ctx, &ec2.DescribeHostsInput{
		Filter: managedFilters(plan),
	})
	if err != nil {
		return nil, fmt.Errorf("describe hosts: %w", err)
	}
	hosts := make([]DedicatedHostStatus, 0, len(out.Hosts))
	for _, host := range out.Hosts {
		hosts = append(hosts, dedicatedHostStatusFromEC2(host))
	}
	return hosts, nil
}

func (c RealAWSClient) describeHostsByName(ctx context.Context, name string) ([]DedicatedHostStatus, error) {
	out, err := c.ec2.DescribeHosts(ctx, &ec2.DescribeHostsInput{
		Filter: []ec2types.Filter{{Name: aws.String("tag:Name"), Values: []string{name}}},
	})
	if err != nil {
		return nil, fmt.Errorf("describe hosts by name: %w", err)
	}
	hosts := make([]DedicatedHostStatus, 0, len(out.Hosts))
	for _, host := range out.Hosts {
		hosts = append(hosts, dedicatedHostStatusFromEC2(host))
	}
	return hosts, nil
}

func (c RealAWSClient) describeHostsByID(ctx context.Context, hostIDs []string) ([]DedicatedHostStatus, error) {
	out, err := c.ec2.DescribeHosts(ctx, &ec2.DescribeHostsInput{
		HostIds: hostIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("describe hosts by id: %w", err)
	}
	hosts := make([]DedicatedHostStatus, 0, len(out.Hosts))
	for _, host := range out.Hosts {
		hosts = append(hosts, dedicatedHostStatusFromEC2(host))
	}
	return hosts, nil
}

func dedicatedHostStatusFromEC2(host ec2types.Host) DedicatedHostStatus {
	instanceIDs := make([]string, 0, len(host.Instances))
	for _, instance := range host.Instances {
		if id := aws.ToString(instance.InstanceId); id != "" {
			instanceIDs = append(instanceIDs, id)
		}
	}
	createdAt := ""
	if host.AllocationTime != nil {
		createdAt = host.AllocationTime.Format(time.RFC3339)
	}
	return DedicatedHostStatus{
		HostID:       aws.ToString(host.HostId),
		State:        string(host.State),
		InstanceType: aws.ToString(host.HostProperties.InstanceType),
		ZoneID:       aws.ToString(host.AvailabilityZoneId),
		CreatedAt:    createdAt,
		InstanceIDs:  instanceIDs,
		Tags:         fromEC2Tags(host.Tags),
	}
}

func (c RealAWSClient) describeManagedInstances(ctx context.Context, plan MacPlan) ([]InstanceStatus, error) {
	out, err := c.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: managedFilters(plan),
	})
	if err != nil {
		return nil, fmt.Errorf("describe instances: %w", err)
	}
	var instances []InstanceStatus
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			instances = append(instances, InstanceStatus{
				InstanceID:   aws.ToString(instance.InstanceId),
				State:        string(instance.State.Name),
				InstanceType: string(instance.InstanceType),
				HostID:       aws.ToString(instance.Placement.HostId),
				PublicIP:     aws.ToString(instance.PublicIpAddress),
				Tags:         fromEC2Tags(instance.Tags),
			})
		}
	}
	return instances, nil
}

func (c RealAWSClient) describeInstanceByID(ctx context.Context, instanceID string) (InstanceStatus, error) {
	out, err := c.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return InstanceStatus{}, fmt.Errorf("describe instance %s: %w", instanceID, err)
	}
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			return InstanceStatus{
				InstanceID:   aws.ToString(instance.InstanceId),
				State:        string(instance.State.Name),
				InstanceType: string(instance.InstanceType),
				HostID:       aws.ToString(instance.Placement.HostId),
				PublicIP:     aws.ToString(instance.PublicIpAddress),
				Tags:         fromEC2Tags(instance.Tags),
			}, nil
		}
	}
	return InstanceStatus{}, fmt.Errorf("instance %s not found", instanceID)
}

func (c RealAWSClient) describeInstancesByName(ctx context.Context, name string) ([]InstanceStatus, error) {
	out, err := c.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{{Name: aws.String("tag:Name"), Values: []string{name}}},
	})
	if err != nil {
		return nil, fmt.Errorf("describe instances by name: %w", err)
	}
	var instances []InstanceStatus
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			instances = append(instances, InstanceStatus{
				InstanceID:   aws.ToString(instance.InstanceId),
				State:        string(instance.State.Name),
				InstanceType: string(instance.InstanceType),
				HostID:       aws.ToString(instance.Placement.HostId),
				PublicIP:     aws.ToString(instance.PublicIpAddress),
				Tags:         fromEC2Tags(instance.Tags),
			})
		}
	}
	return instances, nil
}

func (c RealAWSClient) populateInstanceChecks(ctx context.Context, instances []InstanceStatus) error {
	if len(instances) == 0 {
		return nil
	}
	ids := make([]string, 0, len(instances))
	indexByID := make(map[string]int, len(instances))
	for i, instance := range instances {
		if instance.InstanceID == "" {
			continue
		}
		ids = append(ids, instance.InstanceID)
		indexByID[instance.InstanceID] = i
	}
	if len(ids) == 0 {
		return nil
	}
	out, err := c.ec2.DescribeInstanceStatus(ctx, &ec2.DescribeInstanceStatusInput{
		IncludeAllInstances: aws.Bool(true),
		InstanceIds:         ids,
	})
	if err != nil {
		return fmt.Errorf("describe instance status checks: %w", err)
	}
	for _, status := range out.InstanceStatuses {
		id := aws.ToString(status.InstanceId)
		i, ok := indexByID[id]
		if !ok {
			continue
		}
		instances[i].SystemStatus = summaryStatus(status.SystemStatus)
		instances[i].InstanceStatusCheck = summaryStatus(status.InstanceStatus)
		instances[i].EBSStatus = ebsStatus(status.AttachedEbsStatus)
	}
	return nil
}

func summaryStatus(status *ec2types.InstanceStatusSummary) string {
	if status == nil {
		return ""
	}
	return string(status.Status)
}

func ebsStatus(status *ec2types.EbsStatusSummary) string {
	if status == nil {
		return ""
	}
	return string(status.Status)
}

func hostIDsFromInstances(instances []InstanceStatus) []string {
	seen := map[string]bool{}
	var hostIDs []string
	for _, instance := range instances {
		if instance.HostID == "" || seen[instance.HostID] {
			continue
		}
		seen[instance.HostID] = true
		hostIDs = append(hostIDs, instance.HostID)
	}
	return hostIDs
}

func (c RealAWSClient) describeElasticIP(ctx context.Context, allocationID string) (ElasticIP, error) {
	out, err := c.ec2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		AllocationIds: []string{allocationID},
	})
	if err != nil {
		return ElasticIP{}, fmt.Errorf("describe elastic ip %s: %w", allocationID, err)
	}
	if len(out.Addresses) == 0 {
		return ElasticIP{}, fmt.Errorf("elastic ip %s not found", allocationID)
	}
	address := out.Addresses[0]
	return ElasticIP{
		AllocationID:  aws.ToString(address.AllocationId),
		AssociationID: aws.ToString(address.AssociationId),
		InstanceID:    aws.ToString(address.InstanceId),
		PublicIP:      aws.ToString(address.PublicIp),
		Tags:          fromEC2Tags(address.Tags),
	}, nil
}

func managedFilters(plan MacPlan) []ec2types.Filter {
	return []ec2types.Filter{
		{Name: aws.String("tag:cm-managed"), Values: []string{"true"}},
		{Name: aws.String("tag:cm-profile"), Values: []string{plan.ProfileName}},
		{Name: aws.String("tag:cm-account-email"), Values: []string{plan.AccountEmail}},
	}
}

func toEC2Tags(tags []AWSTagConfig) []ec2types.Tag {
	out := make([]ec2types.Tag, 0, len(tags))
	for _, tag := range tags {
		out = append(out, ec2types.Tag{Key: aws.String(tag.Key), Value: aws.String(tag.Value)})
	}
	return out
}

func fromEC2Tags(tags []ec2types.Tag) []AWSTagConfig {
	out := make([]AWSTagConfig, 0, len(tags))
	for _, tag := range tags {
		out = append(out, AWSTagConfig{Key: aws.ToString(tag.Key), Value: aws.ToString(tag.Value)})
	}
	return out
}
