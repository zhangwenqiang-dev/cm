package connectmac

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultSkillStatePath = "~/.connectmac/skill-state.json"
	skillStateSchema      = 1
)

type SkillStatusName string

const (
	SkillStatusMissing  SkillStatusName = "missing"
	SkillStatusCurrent  SkillStatusName = "current"
	SkillStatusOutdated SkillStatusName = "outdated"
	SkillStatusModified SkillStatusName = "modified"
	SkillStatusInvalid  SkillStatusName = "invalid"
)

type SkillState struct {
	SchemaVersion    int       `json:"schema_version"`
	SkillName        string    `json:"skill_name"`
	SkillPath        string    `json:"skill_path"`
	CMVersion        string    `json:"cm_version"`
	SkillSHA256      string    `json:"skill_sha256"`
	OpenAIYAMLSHA256 string    `json:"openai_yaml_sha256"`
	InstalledAt      time.Time `json:"installed_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type SkillStatus struct {
	Name             SkillStatusName
	SkillPath        string
	InstalledVersion string
	CurrentVersion   string
	Detail           string
	state            *SkillState
	stateMissing     bool
}

type SkillActionResult struct {
	Status     SkillStatus
	BackupPath string
	Changed    bool
}

type SkillManager struct {
	SkillPath string
	StatePath string
	Version   string
	Now       func() time.Time
}

func NewSkillManager(skillsDir, version string) (SkillManager, error) {
	skillPath, err := connectMacSkillPath(skillsDir)
	if err != nil {
		return SkillManager{}, err
	}
	statePath, err := ExpandPath(defaultSkillStatePath)
	if err != nil {
		return SkillManager{}, err
	}
	return SkillManager{
		SkillPath: skillPath,
		StatePath: statePath,
		Version:   version,
		Now:       time.Now,
	}, nil
}

func (m SkillManager) Status() SkillStatus {
	status := SkillStatus{SkillPath: m.SkillPath, CurrentVersion: normalizedCMVersion(m.Version)}
	skillData, skillErr := os.ReadFile(filepath.Join(m.SkillPath, "SKILL.md"))
	metadataData, metadataErr := os.ReadFile(filepath.Join(m.SkillPath, "agents", "openai.yaml"))
	if os.IsNotExist(skillErr) && os.IsNotExist(metadataErr) {
		status.Name = SkillStatusMissing
		status.Detail = "connectmac skill is not installed"
		return status
	}
	if skillErr != nil || metadataErr != nil {
		status.Name = SkillStatusInvalid
		status.Detail = firstSkillReadError(skillErr, metadataErr)
		return status
	}

	actualSkillHash := hashBytes(skillData)
	actualMetadataHash := hashBytes(metadataData)
	currentSkillHash := hashBytes([]byte(DefaultSkillTemplate()))
	currentMetadataHash := hashBytes([]byte(DefaultSkillOpenAIYAML()))

	state, stateMissing, err := m.readState()
	if err != nil {
		status.Name = SkillStatusInvalid
		status.Detail = err.Error()
		return status
	}
	status.stateMissing = stateMissing
	if stateMissing {
		if actualSkillHash == currentSkillHash && actualMetadataHash == currentMetadataHash {
			status.Name = SkillStatusCurrent
			status.Detail = "matching legacy installation; state can be adopted"
		} else {
			status.Name = SkillStatusModified
			status.Detail = "unmanaged skill files differ from the current built-in skill"
		}
		return status
	}
	status.state = &state
	status.InstalledVersion = state.CMVersion
	if err := m.validateState(state); err != nil {
		status.Name = SkillStatusInvalid
		status.Detail = err.Error()
		return status
	}

	if actualSkillHash == currentSkillHash && actualMetadataHash == currentMetadataHash {
		if state.SkillSHA256 != actualSkillHash || state.OpenAIYAMLSHA256 != actualMetadataHash {
			status.Name = SkillStatusInvalid
			status.Detail = "managed state hashes do not match installed files"
			return status
		}
		status.Name = SkillStatusCurrent
		status.Detail = "installed skill matches this cm version"
		return status
	}
	if actualSkillHash == state.SkillSHA256 && actualMetadataHash == state.OpenAIYAMLSHA256 {
		status.Name = SkillStatusOutdated
		status.Detail = "installed skill is managed but differs from this cm version"
		return status
	}
	status.Name = SkillStatusModified
	status.Detail = "installed skill was modified after installation"
	return status
}

func (m SkillManager) Install(dryRun bool) (SkillActionResult, error) {
	status := m.Status()
	switch status.Name {
	case SkillStatusMissing:
		if dryRun {
			return SkillActionResult{Status: status, Changed: true}, nil
		}
		if err := m.writeCurrentState(time.Time{}); err != nil {
			return SkillActionResult{}, err
		}
	case SkillStatusCurrent:
		if !status.stateMissing {
			return SkillActionResult{Status: status}, nil
		}
		if dryRun {
			return SkillActionResult{Status: status, Changed: true}, nil
		}
		if err := m.writeStateForInstalledFiles(time.Time{}); err != nil {
			return SkillActionResult{}, err
		}
	case SkillStatusOutdated:
		return SkillActionResult{}, fmt.Errorf("connectmac skill is outdated; run cm skill update")
	case SkillStatusModified:
		return SkillActionResult{}, fmt.Errorf("connectmac skill was modified; run cm skill update --force to back it up and replace it")
	default:
		return SkillActionResult{}, fmt.Errorf("connectmac skill is invalid: %s", status.Detail)
	}
	return SkillActionResult{Status: m.Status(), Changed: true}, nil
}

func (m SkillManager) Update(force, dryRun bool) (SkillActionResult, error) {
	status := m.Status()
	if status.Name == SkillStatusMissing {
		return SkillActionResult{}, fmt.Errorf("connectmac skill is not installed; run cm skill install")
	}
	if status.Name == SkillStatusCurrent && !status.stateMissing {
		return SkillActionResult{Status: status}, nil
	}
	if (status.Name == SkillStatusModified || status.Name == SkillStatusInvalid) && !force {
		return SkillActionResult{}, fmt.Errorf("connectmac skill is %s; run cm skill update --force to back it up and replace it", status.Name)
	}
	if dryRun {
		return SkillActionResult{Status: status, Changed: true}, nil
	}

	backupPath := ""
	if force && (status.Name == SkillStatusModified || status.Name == SkillStatusInvalid) {
		var err error
		backupPath, err = m.backup()
		if err != nil {
			return SkillActionResult{}, fmt.Errorf("backup modified skill: %w", err)
		}
	}
	installedAt := time.Time{}
	if status.state != nil {
		installedAt = status.state.InstalledAt
	}
	if err := m.writeCurrentState(installedAt); err != nil {
		return SkillActionResult{}, err
	}
	return SkillActionResult{Status: m.Status(), BackupPath: backupPath, Changed: true}, nil
}

func (m SkillManager) Validate() error {
	status := m.Status()
	if status.Name != SkillStatusCurrent && status.Name != SkillStatusOutdated {
		return fmt.Errorf("connectmac skill validation failed: %s: %s", status.Name, status.Detail)
	}
	skill, err := os.ReadFile(filepath.Join(m.SkillPath, "SKILL.md"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(skill), "name: connectmac") {
		return fmt.Errorf("validate skill: missing connectmac name")
	}
	return nil
}

func (m SkillManager) Uninstall(dryRun bool) (SkillActionResult, error) {
	status := m.Status()
	if status.Name == SkillStatusMissing {
		return SkillActionResult{Status: status}, nil
	}
	if status.Name == SkillStatusInvalid {
		return SkillActionResult{}, fmt.Errorf("refusing to remove an invalid connectmac skill: %s", status.Detail)
	}
	if status.stateMissing && status.Name != SkillStatusCurrent {
		return SkillActionResult{}, fmt.Errorf("refusing to remove an unmanaged connectmac skill at %s", m.SkillPath)
	}
	if dryRun {
		return SkillActionResult{Status: status, Changed: true}, nil
	}
	if err := os.RemoveAll(m.SkillPath); err != nil {
		return SkillActionResult{}, fmt.Errorf("remove skill %s: %w", m.SkillPath, err)
	}
	if err := os.Remove(m.StatePath); err != nil && !os.IsNotExist(err) {
		return SkillActionResult{}, fmt.Errorf("remove skill state %s: %w", m.StatePath, err)
	}
	return SkillActionResult{Status: m.Status(), Changed: true}, nil
}

func (m SkillManager) readState() (SkillState, bool, error) {
	data, err := os.ReadFile(m.StatePath)
	if os.IsNotExist(err) {
		return SkillState{}, true, nil
	}
	if err != nil {
		return SkillState{}, false, fmt.Errorf("read skill state %s: %w", m.StatePath, err)
	}
	var state SkillState
	if err := json.Unmarshal(data, &state); err != nil {
		return SkillState{}, false, fmt.Errorf("parse skill state %s: %w", m.StatePath, err)
	}
	return state, false, nil
}

func (m SkillManager) validateState(state SkillState) error {
	if state.SchemaVersion != skillStateSchema {
		return fmt.Errorf("unsupported skill state schema %d", state.SchemaVersion)
	}
	if state.SkillName != SkillName {
		return fmt.Errorf("skill state name is %q", state.SkillName)
	}
	if filepath.Clean(state.SkillPath) != filepath.Clean(m.SkillPath) {
		return fmt.Errorf("skill state path %q does not match %q", state.SkillPath, m.SkillPath)
	}
	if state.SkillSHA256 == "" || state.OpenAIYAMLSHA256 == "" {
		return fmt.Errorf("skill state hashes are missing")
	}
	return nil
}

func (m SkillManager) writeCurrentState(installedAt time.Time) error {
	if err := os.MkdirAll(filepath.Join(m.SkillPath, "agents"), 0o755); err != nil {
		return fmt.Errorf("create skill directory: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(m.SkillPath, "SKILL.md"), []byte(DefaultSkillTemplate()), 0o644); err != nil {
		return fmt.Errorf("write skill: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(m.SkillPath, "agents", "openai.yaml"), []byte(DefaultSkillOpenAIYAML()), 0o644); err != nil {
		return fmt.Errorf("write skill metadata: %w", err)
	}
	return m.writeStateForInstalledFiles(installedAt)
}

func (m SkillManager) writeStateForInstalledFiles(installedAt time.Time) error {
	skill, err := os.ReadFile(filepath.Join(m.SkillPath, "SKILL.md"))
	if err != nil {
		return err
	}
	metadata, err := os.ReadFile(filepath.Join(m.SkillPath, "agents", "openai.yaml"))
	if err != nil {
		return err
	}
	now := m.now().UTC()
	if installedAt.IsZero() {
		installedAt = now
	}
	state := SkillState{
		SchemaVersion:    skillStateSchema,
		SkillName:        SkillName,
		SkillPath:        m.SkillPath,
		CMVersion:        normalizedCMVersion(m.Version),
		SkillSHA256:      hashBytes(skill),
		OpenAIYAMLSHA256: hashBytes(metadata),
		InstalledAt:      installedAt,
		UpdatedAt:        now,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(m.StatePath), 0o700); err != nil {
		return fmt.Errorf("create skill state directory: %w", err)
	}
	if err := atomicWriteFile(m.StatePath, data, 0o600); err != nil {
		return fmt.Errorf("write skill state: %w", err)
	}
	return nil
}

func (m SkillManager) backup() (string, error) {
	backupRoot := filepath.Join(filepath.Dir(m.StatePath), "backups", "skills")
	backupPath := filepath.Join(backupRoot, "connectmac-"+m.now().Format("20060102-150405"))
	if _, err := os.Stat(backupPath); err == nil {
		return "", fmt.Errorf("backup already exists: %s", backupPath)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(backupPath, "connectmac", "agents"), 0o700); err != nil {
		return "", err
	}
	for _, relative := range []string{"SKILL.md", filepath.Join("agents", "openai.yaml")} {
		source := filepath.Join(m.SkillPath, relative)
		if err := copyAndVerifyFile(source, filepath.Join(backupPath, "connectmac", relative)); err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	if err := copyAndVerifyFile(m.StatePath, filepath.Join(backupPath, "skill-state.json")); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return backupPath, nil
}

func (m SkillManager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".connectmac-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func copyAndVerifyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	sourceData, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	destinationData, err := os.ReadFile(destination)
	if err != nil {
		return err
	}
	if hashBytes(sourceData) != hashBytes(destinationData) {
		return errors.New("backup verification failed")
	}
	return nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizedCMVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func firstSkillReadError(skillErr, metadataErr error) string {
	if skillErr != nil {
		return fmt.Sprintf("read SKILL.md: %v", skillErr)
	}
	return fmt.Sprintf("read agents/openai.yaml: %v", metadataErr)
}
