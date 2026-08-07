package connectmac

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSkillManager(t *testing.T) SkillManager {
	t.Helper()
	root := t.TempDir()
	return SkillManager{
		SkillPath: filepath.Join(root, "skills", SkillName),
		StatePath: filepath.Join(root, ".connectmac", "skill-state.json"),
		Version:   "0.1.200",
		Now: func() time.Time {
			return time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC)
		},
	}
}

func TestSkillManagerStatusMissingAndCurrent(t *testing.T) {
	manager := newTestSkillManager(t)
	if got := manager.Status().Name; got != SkillStatusMissing {
		t.Fatalf("missing status = %q", got)
	}
	if _, err := manager.Install(false); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.Name != SkillStatusCurrent || status.InstalledVersion != "0.1.200" {
		t.Fatalf("installed status = %#v", status)
	}
}

func TestSkillManagerStatusLegacyCurrentAndModified(t *testing.T) {
	manager := newTestSkillManager(t)
	writeTestSkillFiles(t, manager, DefaultSkillTemplate(), DefaultSkillOpenAIYAML())
	status := manager.Status()
	if status.Name != SkillStatusCurrent || !status.stateMissing {
		t.Fatalf("legacy current status = %#v", status)
	}
	if err := os.WriteFile(filepath.Join(manager.SkillPath, "SKILL.md"), []byte("manual"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := manager.Status().Name; got != SkillStatusModified {
		t.Fatalf("legacy modified status = %q", got)
	}
}

func TestSkillManagerStatusOutdatedModifiedAndInvalid(t *testing.T) {
	manager := newTestSkillManager(t)
	if _, err := manager.Install(false); err != nil {
		t.Fatal(err)
	}
	state := readTestSkillState(t, manager.StatePath)
	oldSkill := "---\nname: connectmac\n---\nold\n"
	oldMetadata := "interface:\n  display_name: old\n"
	writeTestSkillFiles(t, manager, oldSkill, oldMetadata)
	state.SkillSHA256 = hashBytes([]byte(oldSkill))
	state.OpenAIYAMLSHA256 = hashBytes([]byte(oldMetadata))
	writeTestSkillState(t, manager.StatePath, state)
	if got := manager.Status().Name; got != SkillStatusOutdated {
		t.Fatalf("outdated status = %q", got)
	}
	if err := os.WriteFile(filepath.Join(manager.SkillPath, "SKILL.md"), []byte(oldSkill+"manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := manager.Status().Name; got != SkillStatusModified {
		t.Fatalf("modified status = %q", got)
	}
	if err := os.WriteFile(manager.StatePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := manager.Status().Name; got != SkillStatusInvalid {
		t.Fatalf("invalid status = %q", got)
	}
}

func TestSkillManagerInstallAdoptsMatchingLegacyFiles(t *testing.T) {
	manager := newTestSkillManager(t)
	writeTestSkillFiles(t, manager, DefaultSkillTemplate(), DefaultSkillOpenAIYAML())
	result, err := manager.Install(false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || manager.Status().Name != SkillStatusCurrent {
		t.Fatalf("result = %#v, status = %#v", result, manager.Status())
	}
	if _, err := os.Stat(manager.StatePath); err != nil {
		t.Fatalf("state was not adopted: %v", err)
	}
}

func TestSkillManagerInstallRefusesModifiedLegacyFiles(t *testing.T) {
	manager := newTestSkillManager(t)
	writeTestSkillFiles(t, manager, "manual", DefaultSkillOpenAIYAML())
	if _, err := manager.Install(false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("install error = %v", err)
	}
}

func TestSkillManagerUpdateAndForceBackup(t *testing.T) {
	manager := newTestSkillManager(t)
	if _, err := manager.Install(false); err != nil {
		t.Fatal(err)
	}
	manual := []byte(DefaultSkillTemplate() + "\nmanual note\n")
	if err := os.WriteFile(filepath.Join(manager.SkillPath, "SKILL.md"), manual, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update(false, false); err == nil {
		t.Fatal("ordinary update should refuse modified files")
	}
	result, err := manager.Update(true, false)
	if err != nil {
		t.Fatal(err)
	}
	wantBackup := filepath.Join(filepath.Dir(manager.StatePath), "backups", "skills", "connectmac-20260807-123000")
	if result.BackupPath != wantBackup {
		t.Fatalf("backup path = %q, want %q", result.BackupPath, wantBackup)
	}
	backup, err := os.ReadFile(filepath.Join(wantBackup, "connectmac", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(manual) {
		t.Fatalf("backup = %q", backup)
	}
	if manager.Status().Name != SkillStatusCurrent {
		t.Fatalf("status = %#v", manager.Status())
	}
}

func TestSkillManagerForceUpdateBackupFailurePreservesOriginal(t *testing.T) {
	manager := newTestSkillManager(t)
	if _, err := manager.Install(false); err != nil {
		t.Fatal(err)
	}
	manual := []byte(DefaultSkillTemplate() + "\nmanual note\n")
	skillPath := filepath.Join(manager.SkillPath, "SKILL.md")
	if err := os.WriteFile(skillPath, manual, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(manager.StatePath), "backups"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update(true, false); err == nil {
		t.Fatal("force update should fail when backup cannot be created")
	}
	after, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(manual) {
		t.Fatalf("original was changed after backup failure")
	}
}

func TestSkillManagerUninstallIsIdempotent(t *testing.T) {
	manager := newTestSkillManager(t)
	if _, err := manager.Install(false); err != nil {
		t.Fatal(err)
	}
	if result, err := manager.Uninstall(false); err != nil || !result.Changed {
		t.Fatalf("uninstall result=%#v err=%v", result, err)
	}
	if result, err := manager.Uninstall(false); err != nil || result.Changed {
		t.Fatalf("second uninstall result=%#v err=%v", result, err)
	}
}

func TestSkillManagerUninstallRefusesMismatchedStatePath(t *testing.T) {
	manager := newTestSkillManager(t)
	if _, err := manager.Install(false); err != nil {
		t.Fatal(err)
	}
	state := readTestSkillState(t, manager.StatePath)
	state.SkillPath = filepath.Join(t.TempDir(), "different-skill")
	writeTestSkillState(t, manager.StatePath, state)
	if _, err := manager.Uninstall(false); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("uninstall error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.SkillPath, "SKILL.md")); err != nil {
		t.Fatalf("skill was removed despite mismatched state: %v", err)
	}
}

func TestRemoveMarkedRulesBlockPreservesOtherContent(t *testing.T) {
	input := "before\n\n" + rulesStart + "\nmanaged\n" + rulesEnd + "\n\nafter\n"
	updated, changed := removeMarkedRulesBlock(input)
	if !changed {
		t.Fatal("expected marker block removal")
	}
	if updated != "before\n\nafter\n" {
		t.Fatalf("updated rules = %q", updated)
	}
	if unchanged, changed := removeMarkedRulesBlock("unrelated\n"); changed || unchanged != "unrelated\n" {
		t.Fatalf("unrelated rules changed: %q, %v", unchanged, changed)
	}
}

func TestUninstallRulesBlockPreservesAgentFile(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, "AGENTS.md")
	input := "project instructions\n\n" + markedRulesBlock(DefaultRulesTemplate())
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}
	gotPath, changed, err := UninstallRulesBlock("codex", project, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path || !changed {
		t.Fatalf("path=%q changed=%v", gotPath, changed)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "project instructions\n" {
		t.Fatalf("agent file = %q", data)
	}
}

func writeTestSkillFiles(t *testing.T, manager SkillManager, skill, metadata string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(manager.SkillPath, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manager.SkillPath, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manager.SkillPath, "agents", "openai.yaml"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestSkillState(t *testing.T, path string) SkillState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state SkillState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func writeTestSkillState(t *testing.T, path string, state SkillState) {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
