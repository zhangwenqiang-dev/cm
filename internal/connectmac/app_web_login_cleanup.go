package connectmac

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (a App) cleanupLocalConfigAfterLogin(configPath string) {
	if !a.LoginConfigCleanup {
		return
	}
	backup, err := cleanupDefaultLocalConfigProfiles(configPath, time.Now())
	if err != nil {
		a.writeRuntimeLog(LogEntry{
			Level:     "error",
			Action:    "web.auth.cleanup",
			Operation: "config.cleanup",
			Source:    "system",
			Phase:     "failed",
			ErrorCode: classifyOperationalError(err).Code,
			Message:   err.Error(),
		})
		return
	}
	if backup != "" {
		a.writeRuntimeLog(LogEntry{
			Level: "info", Action: "web.auth.cleanup", Operation: "config.cleanup",
			Source: "system", Phase: "completed",
			Message: "backed up old local profiles to " + backup,
		})
	}
}

func cleanupDefaultLocalConfigProfiles(configPath string, now time.Time) (string, error) {
	expanded, err := ExpandPath(configPath)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	defaultPath := filepath.Join(home, ".connectmac", "config.yaml")
	if filepath.Clean(expanded) != filepath.Clean(defaultPath) {
		return "", nil
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	configDir := filepath.Dir(expanded)
	profilesDir := filepath.Join(configDir, "profiles")
	hasProfilesDir := false
	if entries, err := os.ReadDir(profilesDir); err == nil && len(entries) > 0 {
		hasProfilesDir = true
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if len(cfg.Profiles) == 0 && !hasProfilesDir {
		return "", nil
	}
	backupDir := filepath.Join(configDir, "backups", "login-cleanup-"+now.Format("20060102150405"))
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	if err := copyFile(expanded, filepath.Join(backupDir, "config.yaml")); err != nil {
		return "", err
	}
	if hasProfilesDir {
		if err := os.Rename(profilesDir, filepath.Join(backupDir, "profiles")); err != nil {
			if err := copyDir(profilesDir, filepath.Join(backupDir, "profiles")); err != nil {
				return "", err
			}
			if err := os.RemoveAll(profilesDir); err != nil {
				return "", err
			}
		}
	}
	if err := os.MkdirAll(profilesDir, 0o700); err != nil {
		return "", err
	}
	clean := formatLocalConfigWithoutProfiles(cfg)
	if err := os.WriteFile(expanded, []byte(clean), 0o600); err != nil {
		return "", err
	}
	return backupDir, nil
}

func formatLocalConfigWithoutProfiles(cfg Config) string {
	var b strings.Builder
	if cfg.Server.UserAPI != "" || cfg.Server.LocalAgentOrigin != "" {
		fmt.Fprintln(&b, "server:")
		writeStringField(&b, "  ", "user_api", cfg.Server.UserAPI)
		writeStringField(&b, "  ", "local_agent_origin", cfg.Server.LocalAgentOrigin)
		fmt.Fprintln(&b)
	}
	if !profileDefaultsEmpty(cfg.Defaults) {
		fmt.Fprintln(&b, "defaults:")
		writeStringField(&b, "  ", "user", cfg.Defaults.User)
		writeStringField(&b, "  ", "identity_file", cfg.Defaults.IdentityFile)
		writeAWSDefaultsYAML(&b, cfg.Defaults.AWS, "  ")
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, "profiles:")
	return b.String()
}

func profileDefaultsEmpty(defaults ProfileDefaults) bool {
	return defaults.User == "" &&
		defaults.IdentityFile == "" &&
		awsDefaultsEmpty(defaults.AWS)
}

func awsDefaultsEmpty(defaults AWSDefaults) bool {
	return defaults.Creator == "" &&
		defaults.AMI.MacX86 == "" &&
		defaults.AMI.MacARM == "" &&
		len(defaults.AMIsByRegion) == 0
}

func writeAWSDefaultsYAML(b *strings.Builder, defaults AWSDefaults, indent string) {
	if awsDefaultsEmpty(defaults) {
		return
	}
	fmt.Fprintf(b, "%saws:\n", indent)
	writeStringField(b, indent+"  ", "creator", defaults.Creator)
	if defaults.AMI.MacX86 != "" || defaults.AMI.MacARM != "" {
		fmt.Fprintf(b, "%s  ami:\n", indent)
		writeStringField(b, indent+"    ", "mac_x86", defaults.AMI.MacX86)
		writeStringField(b, indent+"    ", "mac_arm", defaults.AMI.MacARM)
	}
	if len(defaults.AMIsByRegion) > 0 {
		fmt.Fprintf(b, "%s  amis_by_region:\n", indent)
		regions := make([]string, 0, len(defaults.AMIsByRegion))
		for region := range defaults.AMIsByRegion {
			regions = append(regions, region)
		}
		sort.Strings(regions)
		for _, region := range regions {
			ami := defaults.AMIsByRegion[region]
			fmt.Fprintf(b, "%s    %s:\n", indent, region)
			writeStringField(b, indent+"      ", "mac_x86", ami.MacX86)
			writeStringField(b, indent+"      ", "mac_arm", ami.MacARM)
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}
