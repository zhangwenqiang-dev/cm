package connectmac

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type PortChecker func(port int) error
type SSHChecker func() error

type Validator struct {
	CheckPort  PortChecker
	CheckSSH   SSHChecker
	CheckRsync SSHChecker
}

func NewValidator() Validator {
	return Validator{
		CheckPort:  CheckLocalPortAvailable,
		CheckSSH:   CheckSSHAvailable,
		CheckRsync: CheckRsyncAvailable,
	}
}

func (v Validator) ValidateProfile(profile Profile) []error {
	errs := v.ValidateProfileSyntax(profile)
	errs = append(errs, v.ValidateAccess(profile)...)
	for _, tunnel := range profile.Tunnels {
		if v.CheckPort != nil && tunnel.LocalPort > 0 && tunnel.LocalPort <= 65535 {
			if err := v.CheckPort(tunnel.LocalPort); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

func (v Validator) ValidateProfileSyntax(profile Profile) []error {
	var errs []error
	if len(profile.Tunnels) == 0 {
		errs = append(errs, errors.New("at least one tunnel is required"))
	}
	for i, tunnel := range profile.Tunnels {
		if tunnel.LocalPort < 1 || tunnel.LocalPort > 65535 {
			errs = append(errs, fmt.Errorf("tunnel %d local_port must be between 1 and 65535", i+1))
		}
		if tunnel.RemoteHost == "" {
			errs = append(errs, fmt.Errorf("tunnel %d remote_host is required", i+1))
		}
		if tunnel.RemotePort < 1 || tunnel.RemotePort > 65535 {
			errs = append(errs, fmt.Errorf("tunnel %d remote_port must be between 1 and 65535", i+1))
		}
	}
	return errs
}

func (v Validator) ValidateNewLocalPorts(profile Profile, existing State) []error {
	owned := make(map[int]struct{}, len(existing.Tunnels))
	for _, tunnel := range existing.Tunnels {
		owned[tunnel.LocalPort] = struct{}{}
	}
	var errs []error
	for _, tunnel := range profile.Tunnels {
		if _, ok := owned[tunnel.LocalPort]; ok {
			continue
		}
		if v.CheckPort != nil && tunnel.LocalPort > 0 && tunnel.LocalPort <= 65535 {
			if err := v.CheckPort(tunnel.LocalPort); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

func (v Validator) ValidateAccess(profile Profile) []error {
	var errs []error
	if profile.Name == "" {
		errs = append(errs, errors.New("profile name is empty"))
	}
	if profile.User == "" {
		errs = append(errs, errors.New("user is required"))
	}
	if profile.Host == "" {
		errs = append(errs, errors.New("host is required"))
	}
	keyPath, err := ExpandPath(profile.IdentityFile)
	if profile.IdentityFile == "" {
		errs = append(errs, errors.New("identity_file is required; set profile.identity_file or defaults.identity_file"))
	} else if err != nil {
		errs = append(errs, err)
	} else if pathErr := validateIdentityFileLocation(keyPath); pathErr != nil {
		errs = append(errs, pathErr)
	} else if statErr := validateIdentityFile(keyPath); statErr != nil {
		errs = append(errs, statErr)
	}
	if v.CheckSSH != nil {
		if err := v.CheckSSH(); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (v Validator) ValidateAWSProfile(profile Profile) []error {
	var errs []error
	if profile.Name == "" {
		errs = append(errs, errors.New("profile name is empty"))
	}
	requiredAWS := []struct {
		name  string
		value string
	}{
		{"aws.profile", profile.AWS.Profile},
		{"aws.region", profile.AWS.Region},
		{"aws.account_email", profile.AWS.AccountEmail},
		{"aws.key_name", profile.AWS.KeyName},
		{"aws.security_group_id", profile.AWS.SecurityGroupID},
		{"aws.elastic_ip_allocation_id", profile.AWS.ElasticIPAllocationID},
		{"aws.elastic_ip_owner_tag.key", profile.AWS.ElasticIPOwnerTag.Key},
		{"aws.elastic_ip_owner_tag.value", profile.AWS.ElasticIPOwnerTag.Value},
	}
	for _, field := range requiredAWS {
		if field.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", field.name))
		}
	}
	if len(profile.AWS.AvailabilityZoneIDs) == 0 {
		errs = append(errs, errors.New("aws.availability_zone_ids is required"))
	}
	if profile.AWS.SubnetID == "" && len(profile.AWS.SubnetsByAZ) == 0 {
		errs = append(errs, errors.New("aws.subnet_id or aws.subnets_by_az is required"))
	}
	for _, zoneID := range profile.AWS.AvailabilityZoneIDs {
		if !validAvailabilityZoneID(zoneID) {
			errs = append(errs, fmt.Errorf("aws availability zone id %q must end with -az1 through -az99", zoneID))
		}
	}
	for zoneID, subnetID := range profile.AWS.SubnetsByAZ {
		if !validAvailabilityZoneID(zoneID) {
			errs = append(errs, fmt.Errorf("aws subnets_by_az key %q must end with -az1 through -az99", zoneID))
		}
		if subnetID == "" {
			errs = append(errs, fmt.Errorf("aws subnets_by_az %q subnet id is required", zoneID))
		}
	}
	instanceTypes := profile.AWS.InstanceTypePriority
	explicitInstanceTypes := len(instanceTypes) > 0
	if len(instanceTypes) == 0 {
		instanceTypes = DefaultMacInstanceTypePriority
	}
	if !profile.AWS.AllowIntelFallback && !explicitInstanceTypes {
		instanceTypes = withoutIntel(instanceTypes)
	}
	hasARM := false
	hasIntel := false
	for _, instanceType := range instanceTypes {
		if !SupportedMacInstanceTypes[instanceType] {
			errs = append(errs, fmt.Errorf("unsupported aws instance type %q", instanceType))
			continue
		}
		if IsIntelMacInstanceType(instanceType) {
			hasIntel = true
			if explicitInstanceTypes && !profile.AWS.AllowIntelFallback {
				errs = append(errs, errors.New("mac1.metal requires aws.allow_intel_fallback: true"))
			}
			continue
		}
		hasARM = true
	}
	if hasARM && profile.AWS.AMI.MacARM == "" {
		errs = append(errs, errors.New("aws.ami.mac_arm is required for Apple Silicon Mac instance types"))
	}
	if hasIntel && profile.AWS.AMI.MacX86 == "" {
		errs = append(errs, errors.New("aws.ami.mac_x86 is required for mac1.metal"))
	}
	if !hasARM && !hasIntel {
		errs = append(errs, errors.New("aws.instance_type_priority has no usable instance types"))
	}
	return errs
}

func ValidateNamedProfile(cfg Config, name string, validator Validator) (Profile, []error) {
	profile, ok := cfg.Profile(name)
	if !ok {
		return Profile{}, []error{unknownProfileError(cfg, name)}
	}
	return profile, validator.ValidateProfile(profile)
}

func CheckLocalPortAvailable(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("local port %d is already in use", port)
	}
	return listener.Close()
}

func CheckSSHAvailable() error {
	if _, err := exec.LookPath("ssh"); err != nil {
		return errors.New("ssh executable not found on PATH")
	}
	return nil
}

func CheckRsyncAvailable() error {
	if configured := strings.TrimSpace(os.Getenv(connectMacRsyncEnv)); configured != "" {
		info, err := os.Stat(configured)
		if err != nil {
			return fmt.Errorf("rsync executable %s not found: %w", configured, err)
		}
		if info.IsDir() {
			return fmt.Errorf("rsync executable %s is a directory", configured)
		}
		return nil
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		return errors.New("rsync executable not found on PATH")
	}
	return nil
}

func validateIdentityFileLocation(path string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve identity file path %s: %w", path, err)
	}
	cleanSSHDir, err := filepath.Abs(sshDir)
	if err != nil {
		return fmt.Errorf("resolve ssh directory %s: %w", sshDir, err)
	}
	if cleanPath == cleanSSHDir || !strings.HasPrefix(cleanPath, cleanSSHDir+string(os.PathSeparator)) {
		return fmt.Errorf("identity file must be stored under %s; move the PEM into ~/.ssh and update identity_file", sshDir)
	}
	return nil
}

func validateIdentityFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("identity file does not exist: %s", path)
		}
		return fmt.Errorf("read identity file %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("identity file is a directory: %s", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("identity file permissions are too open: %s has mode %o; run chmod 600 %s", path, info.Mode().Perm(), path)
	}
	return nil
}

func validAvailabilityZoneID(zoneID string) bool {
	index := strings.LastIndex(zoneID, "-az")
	if index < 0 || index+3 >= len(zoneID) {
		return false
	}
	number := zoneID[index+3:]
	if len(number) > 2 {
		return false
	}
	value, err := strconv.Atoi(number)
	return err == nil && value >= 1 && value <= 99
}

func unknownProfileError(cfg Config, name string) error {
	if len(cfg.Profiles) == 0 {
		return fmt.Errorf("unknown profile %q; no profiles are configured", name)
	}
	names := make([]string, 0, len(cfg.Profiles))
	for profileName := range cfg.Profiles {
		names = append(names, profileName)
	}
	return fmt.Errorf("unknown profile %q; available profiles: %v", name, names)
}
