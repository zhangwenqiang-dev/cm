package connectmac

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillManagerDiffMissingInstallation(t *testing.T) {
	manager := newTestSkillManager(t)

	_, err := manager.Diff()
	if err == nil || !strings.Contains(err.Error(), "connectmac skill is not installed") || !strings.Contains(err.Error(), "cm skill install") {
		t.Fatalf("diff error = %v", err)
	}
}

func TestSkillManagerDiffCurrentInstallationHasNoDifferences(t *testing.T) {
	manager := newTestSkillManager(t)
	writeTestSkillFiles(t, manager, DefaultSkillTemplate(), DefaultSkillOpenAIYAML())

	got, err := manager.Diff()
	if err != nil {
		t.Fatal(err)
	}
	if got != (SkillDiffResult{}) {
		t.Fatalf("diff result = %#v, want no differences", got)
	}
}

func TestSkillManagerDiffModifiedSkill(t *testing.T) {
	manager := newTestSkillManager(t)
	writeTestSkillFiles(t, manager, DefaultSkillTemplate()+"manual note\n", DefaultSkillOpenAIYAML())

	got, err := manager.Diff()
	if err != nil {
		t.Fatal(err)
	}
	want, changed := renderUnifiedTextDiff("installed/SKILL.md", "built-in/SKILL.md", DefaultSkillTemplate()+"manual note\n", DefaultSkillTemplate())
	if !changed || got.Text != want || !got.Changed {
		t.Fatalf("diff result = %#v, want text %q", got, want)
	}
}

func TestSkillManagerDiffModifiedOpenAIYAML(t *testing.T) {
	manager := newTestSkillManager(t)
	writeTestSkillFiles(t, manager, DefaultSkillTemplate(), DefaultSkillOpenAIYAML()+"manual: true\n")

	got, err := manager.Diff()
	if err != nil {
		t.Fatal(err)
	}
	want, changed := renderUnifiedTextDiff("installed/agents/openai.yaml", "built-in/agents/openai.yaml", DefaultSkillOpenAIYAML()+"manual: true\n", DefaultSkillOpenAIYAML())
	if !changed || got.Text != want || !got.Changed {
		t.Fatalf("diff result = %#v, want text %q", got, want)
	}
}

func TestSkillManagerDiffBothFilesDifferInStableOrder(t *testing.T) {
	manager := newTestSkillManager(t)
	installedSkill := DefaultSkillTemplate() + "manual skill note\n"
	installedMetadata := DefaultSkillOpenAIYAML() + "manual: true\n"
	writeTestSkillFiles(t, manager, installedSkill, installedMetadata)

	got, err := manager.Diff()
	if err != nil {
		t.Fatal(err)
	}
	skillDiff, _ := renderUnifiedTextDiff("installed/SKILL.md", "built-in/SKILL.md", installedSkill, DefaultSkillTemplate())
	metadataDiff, _ := renderUnifiedTextDiff("installed/agents/openai.yaml", "built-in/agents/openai.yaml", installedMetadata, DefaultSkillOpenAIYAML())
	want := skillDiff + metadataDiff
	if !got.Changed || got.Text != want {
		t.Fatalf("diff result = %#v, want text %q", got, want)
	}
	if strings.Index(got.Text, "--- installed/SKILL.md\n") > strings.Index(got.Text, "--- installed/agents/openai.yaml\n") {
		t.Fatalf("diffs are in the wrong order: %q", got.Text)
	}
}

func TestSkillManagerDiffPreservesInstalledFilesAndState(t *testing.T) {
	manager := newTestSkillManager(t)
	installedSkill := DefaultSkillTemplate() + "manual skill note\n"
	installedMetadata := DefaultSkillOpenAIYAML() + "manual: true\n"
	writeTestSkillFiles(t, manager, installedSkill, installedMetadata)
	state := []byte("state bytes that Diff must preserve\n")
	if err := os.MkdirAll(filepath.Dir(manager.StatePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.StatePath, state, 0o600); err != nil {
		t.Fatal(err)
	}

	beforeSkill, err := os.ReadFile(filepath.Join(manager.SkillPath, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	beforeMetadata, err := os.ReadFile(filepath.Join(manager.SkillPath, "agents", "openai.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Diff(); err != nil {
		t.Fatal(err)
	}
	afterSkill, err := os.ReadFile(filepath.Join(manager.SkillPath, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	afterMetadata, err := os.ReadFile(filepath.Join(manager.SkillPath, "agents", "openai.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	afterState, err := os.ReadFile(manager.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeSkill, afterSkill) || !bytes.Equal(beforeMetadata, afterMetadata) || !bytes.Equal(state, afterState) {
		t.Fatal("Diff changed installed files or StatePath")
	}
}

func TestRenderUnifiedTextDiff(t *testing.T) {
	tests := []struct {
		name    string
		oldText string
		newText string
		want    string
		changed bool
	}{
		{
			name:    "identical text",
			oldText: "one\ntwo\n",
			newText: "one\ntwo\n",
			want:    "",
		},
		{
			name:    "one changed line",
			oldText: "one\ntwo\nthree\n",
			newText: "one\nchanged\nthree\n",
			want:    "--- old.txt\n+++ new.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+changed\n three\n",
			changed: true,
		},
		{
			name:    "insertion and deletion preserve context",
			oldText: "one\ntwo\nthree\nfour\n",
			newText: "one\nthree\nfour\nfive\n",
			want:    "--- old.txt\n+++ new.txt\n@@ -1,4 +1,4 @@\n one\n-two\n three\n four\n+five\n",
			changed: true,
		},
		{
			name:    "multiple separated hunks",
			oldText: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n",
			newText: "1\ntwo\n3\n4\n5\n6\n7\n8\n9\nten\n11\n12\n",
			want:    "--- old.txt\n+++ new.txt\n@@ -1,5 +1,5 @@\n 1\n-2\n+two\n 3\n 4\n 5\n@@ -7,6 +7,6 @@\n 7\n 8\n 9\n-10\n+ten\n 11\n 12\n",
			changed: true,
		},
		{
			name:    "no trailing newline",
			oldText: "one\ntwo",
			newText: "one\nchanged",
			want:    "--- old.txt\n+++ new.txt\n@@ -1,2 +1,2 @@\n one\n-two\n\\ No newline at end of file\n+changed\n\\ No newline at end of file\n",
			changed: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, changed := renderUnifiedTextDiff("old.txt", "new.txt", test.oldText, test.newText)
			if changed != test.changed {
				t.Fatalf("changed = %v, want %v", changed, test.changed)
			}
			if got != test.want {
				t.Fatalf("diff = %q, want %q", got, test.want)
			}
			if test.want != "" && !strings.HasPrefix(got, "--- old.txt\n+++ new.txt\n") {
				t.Fatalf("diff is missing unified headers: %q", got)
			}
		})
	}
}
