package connectmac

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppSkillInstallStatusPathValidateAndUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skillsDir := filepath.Join(home, "shared-skills")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	app.Version = "0.1.200"

	if code := app.Run(context.Background(), []string{"skill", "install", "--skills-dir", skillsDir}); code != 0 {
		t.Fatalf("install code=%d err=%s", code, errOut.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"skill", "status", "--skills-dir", skillsDir}); code != 0 {
		t.Fatalf("status code=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Status: current") || !strings.Contains(out.String(), "Installed CM: 0.1.200") {
		t.Fatalf("status output = %q", out.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"skill", "path", "--skills-dir", skillsDir}); code != 0 {
		t.Fatalf("path code=%d err=%s", code, errOut.String())
	}
	wantPath := filepath.Join(skillsDir, SkillName) + "\n"
	if out.String() != wantPath {
		t.Fatalf("path output = %q, want %q", out.String(), wantPath)
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"skill", "validate", "--skills-dir", skillsDir}); code != 0 {
		t.Fatalf("validate code=%d err=%s", code, errOut.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"skill", "uninstall", "--skills-dir", skillsDir}); code != 0 {
		t.Fatalf("uninstall code=%d err=%s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(skillsDir, SkillName)); !os.IsNotExist(err) {
		t.Fatalf("skill still exists: %v", err)
	}
}

func TestAppSkillDiffMatchingInstallation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skillsDir := filepath.Join(home, "skills")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())

	if code := app.Run(context.Background(), []string{"skill", "install", "--skills-dir", skillsDir}); code != 0 {
		t.Fatalf("install code=%d err=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"skill", "diff", "--skills-dir", skillsDir}); code != 0 {
		t.Fatalf("diff code=%d err=%s", code, errOut.String())
	}
	if got, want := out.String(), "connectmac skill matches the built-in version\n"; got != want {
		t.Fatalf("diff output = %q, want %q", got, want)
	}
	if errOut.Len() != 0 {
		t.Fatalf("diff stderr = %q", errOut.String())
	}
}

func TestAppSkillDiffModifiedInstallation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skillsDir := filepath.Join(home, "skills")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())

	if code := app.Run(context.Background(), []string{"skill", "install", "--skills-dir", skillsDir}); code != 0 {
		t.Fatalf("install code=%d err=%s", code, errOut.String())
	}
	installedSkill := filepath.Join(skillsDir, SkillName, "SKILL.md")
	if err := os.WriteFile(installedSkill, []byte(DefaultSkillTemplate()+"manual note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"skill", "diff", "--skills-dir", skillsDir}); code != 0 {
		t.Fatalf("diff code=%d err=%s", code, errOut.String())
	}
	want, changed := renderUnifiedTextDiff("installed/SKILL.md", "built-in/SKILL.md", DefaultSkillTemplate()+"manual note\n", DefaultSkillTemplate())
	if !changed {
		t.Fatal("test diff fixture did not change")
	}
	if got := out.String(); got != want {
		t.Fatalf("diff output = %q, want %q", got, want)
	}
}

func TestAppSkillDiffMissingInstallation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skillsDir := filepath.Join(home, "skills")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())

	if code := app.Run(context.Background(), []string{"skill", "diff", "--skills-dir", skillsDir}); code != 1 {
		t.Fatalf("diff code=%d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "connectmac skill is not installed") || !strings.Contains(errOut.String(), "cm skill install") {
		t.Fatalf("diff error = %q", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("diff stdout = %q", out.String())
	}
}

func TestAppSkillDiffCustomSkillsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	customDir := filepath.Join(t.TempDir(), "custom-skills")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())

	if code := app.Run(context.Background(), []string{"skill", "install", "--skills-dir", customDir}); code != 0 {
		t.Fatalf("install code=%d err=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"skill", "diff", "--skills-dir", customDir}); code != 0 {
		t.Fatalf("diff code=%d err=%s", code, errOut.String())
	}
	if got, want := out.String(), "connectmac skill matches the built-in version\n"; got != want {
		t.Fatalf("diff output = %q, want %q", got, want)
	}
}

func TestAppSkillDiffRejectsUnknownOption(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())

	if code := app.Run(context.Background(), []string{"skill", "diff", "--force"}); code != 2 {
		t.Fatalf("diff code=%d, want 2", code)
	}
	if !strings.Contains(errOut.String(), `unknown skill option "--force"`) {
		t.Fatalf("usage error = %q", errOut.String())
	}
}

func TestAppSkillUpdateModifiedRequiresForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skillsDir := filepath.Join(home, "skills")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"skill", "install", "--skills-dir", skillsDir}); code != 0 {
		t.Fatalf("install code=%d err=%s", code, errOut.String())
	}
	skillPath := filepath.Join(skillsDir, SkillName, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(DefaultSkillTemplate()+"\nmanual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"skill", "update", "--skills-dir", skillsDir}); code != 1 {
		t.Fatalf("ordinary update code=%d err=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"skill", "update", "--skills-dir", skillsDir, "--force"}); code != 0 {
		t.Fatalf("force update code=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "backed up previous skill") {
		t.Fatalf("force output = %q", out.String())
	}
}

func TestAppSkillUninstallRulesRequiresExplicitTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"skill", "uninstall", "--rules"}); code != 2 {
		t.Fatalf("uninstall --rules code=%d", code)
	}
	if !strings.Contains(errOut.String(), "requires both --agent and --project") {
		t.Fatalf("error = %q", errOut.String())
	}
}

func TestAppSkillDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skillsDir := filepath.Join(home, "skills")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"skill", "install", "--skills-dir", skillsDir, "--dry-run"}); code != 0 {
		t.Fatalf("dry-run code=%d err=%s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(skillsDir, SkillName)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote skill: %v", err)
	}
}

func TestAppSkillPrintDoesNotWriteFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"skill", "print"}); code != 0 {
		t.Fatalf("print code=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "name: connectmac") {
		t.Fatalf("print output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".connectmac")); !os.IsNotExist(err) {
		t.Fatalf("print wrote files: %v", err)
	}
}

func TestAppInitRulesIsUnknown(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"init-rules"}); code != 2 {
		t.Fatalf("init-rules code=%d", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "init-rules"`) {
		t.Fatalf("error = %q", errOut.String())
	}
}
