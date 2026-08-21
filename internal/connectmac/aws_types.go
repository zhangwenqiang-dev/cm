package connectmac

import (
	"fmt"
	"strings"
	"time"
)

var DefaultMacInstanceTypePriority = []string{
	"mac2.metal",
	"mac2-m2.metal",
	"mac-m4.metal",
	"mac2-m2pro.metal",
	"mac-m4pro.metal",
	"mac2-m1ultra.metal",
	"mac-m4max.metal",
	"mac-m3ultra.metal",
	"mac1.metal",
}

var SupportedMacInstanceTypes = map[string]bool{
	"mac1.metal":         true,
	"mac2.metal":         true,
	"mac2-m1ultra.metal": true,
	"mac2-m2.metal":      true,
	"mac2-m2pro.metal":   true,
	"mac-m3ultra.metal":  true,
	"mac-m4.metal":       true,
	"mac-m4max.metal":    true,
	"mac-m4pro.metal":    true,
}

type MacPlan struct {
	ProfileName           string
	ResourceName          string
	Region                string
	AWSProfile            string
	AccountEmail          string
	AvailabilityZoneIDs   []string
	InstanceTypePriority  []string
	SelectedInstanceType  string
	SelectedAMI           string
	KeyName               string
	SubnetID              string
	SubnetsByAZ           map[string]string
	SecurityGroupID       string
	ElasticIPAllocationID string
	ElasticIPOwnerTag     AWSTagConfig
	Tags                  []AWSTagConfig
}

func BuildMacPlan(profile Profile, _ time.Time) (MacPlan, error) {
	if len(profile.AWS.InstanceTypePriority) == 0 {
		profile.AWS.InstanceTypePriority = append([]string(nil), DefaultMacInstanceTypePriority...)
	}
	if !profile.AWS.AllowIntelFallback {
		profile.AWS.InstanceTypePriority = withoutIntel(profile.AWS.InstanceTypePriority)
	}
	selectedInstanceType := ""
	selectedAMI := ""
	for _, instanceType := range profile.AWS.InstanceTypePriority {
		if !SupportedMacInstanceTypes[instanceType] {
			return MacPlan{}, fmt.Errorf("unsupported aws instance type %q", instanceType)
		}
		ami := amiForInstanceType(profile.AWS, instanceType)
		if ami == "" {
			continue
		}
		selectedInstanceType = instanceType
		selectedAMI = ami
		break
	}
	if selectedInstanceType == "" {
		return MacPlan{}, fmt.Errorf("no architecture-compatible aws instance type and AMI configured")
	}
	resourceName := profile.AWS.ResourceName
	if resourceName == "" {
		resourceName = MacResourceName(profile.AWS.AccountEmail)
	}
	tags := []AWSTagConfig{
		{Key: "Name", Value: resourceName},
		{Key: "cm-managed", Value: "true"},
		{Key: "cm-profile", Value: profile.Name},
		{Key: "cm-account-email", Value: profile.AWS.AccountEmail},
	}
	if profile.AWS.Creator != "" {
		tags = append(tags, AWSTagConfig{Key: "cm-creator", Value: profile.AWS.Creator})
	}
	return MacPlan{
		ProfileName:           profile.Name,
		ResourceName:          resourceName,
		Region:                profile.AWS.Region,
		AWSProfile:            profile.AWS.Profile,
		AccountEmail:          profile.AWS.AccountEmail,
		AvailabilityZoneIDs:   append([]string(nil), profile.AWS.AvailabilityZoneIDs...),
		InstanceTypePriority:  append([]string(nil), profile.AWS.InstanceTypePriority...),
		SelectedInstanceType:  selectedInstanceType,
		SelectedAMI:           selectedAMI,
		KeyName:               profile.AWS.KeyName,
		SubnetID:              profile.AWS.SubnetID,
		SubnetsByAZ:           copyStringMap(profile.AWS.SubnetsByAZ),
		SecurityGroupID:       profile.AWS.SecurityGroupID,
		ElasticIPAllocationID: profile.AWS.ElasticIPAllocationID,
		ElasticIPOwnerTag:     profile.AWS.ElasticIPOwnerTag,
		Tags:                  tags,
	}, nil
}

func (p MacPlan) SubnetForAZ(availabilityZoneID string) string {
	if len(p.SubnetsByAZ) > 0 {
		return p.SubnetsByAZ[availabilityZoneID]
	}
	return p.SubnetID
}

func MacResourceName(accountEmail string) string {
	return fmt.Sprintf("xcode-%s", strings.ToLower(accountEmail))
}

func IsIntelMacInstanceType(instanceType string) bool {
	return instanceType == "mac1.metal"
}

func MacHostArchitecture(instanceType string) string {
	instanceType = strings.TrimSpace(instanceType)
	if !SupportedMacInstanceTypes[instanceType] {
		return ""
	}
	if IsIntelMacInstanceType(instanceType) {
		return "x86"
	}
	return "arm64"
}

func amiForInstanceType(cfg AWSConfig, instanceType string) string {
	if IsIntelMacInstanceType(instanceType) {
		return cfg.AMI.MacX86
	}
	return cfg.AMI.MacARM
}

func withoutIntel(instanceTypes []string) []string {
	out := make([]string, 0, len(instanceTypes))
	for _, instanceType := range instanceTypes {
		if IsIntelMacInstanceType(instanceType) {
			continue
		}
		out = append(out, instanceType)
	}
	return out
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
