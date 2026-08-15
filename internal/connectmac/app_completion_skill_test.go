package connectmac

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestUsageListsSkillWithoutInitRules(t *testing.T) {
	var out bytes.Buffer
	app := App{Out: &out}
	app.printUsage()
	wantSkillSummary := "cm skill <install|status|diff|update|validate|path|print|uninstall> [options]"
	if !strings.Contains(out.String(), wantSkillSummary) {
		t.Fatalf("usage missing skill summary %q: %q", wantSkillSummary, out.String())
	}
	if strings.Contains(out.String(), "cm init-rules") {
		t.Fatalf("usage contains removed command: %q", out.String())
	}
}

func TestUsageDescribesGuidedRerunnableInit(t *testing.T) {
	var out bytes.Buffer
	app := App{Out: &out}
	app.printUsage()
	usage := out.String()

	for _, want := range []string{
		"cm init [--config <path>]",
		"cm init wizard [--config <path>]",
		"cm init is a guided, rerunnable setup",
	} {
		if !strings.Contains(usage, want) {
			t.Fatalf("usage missing %q: %q", want, usage)
		}
	}
}

func TestCompletionCommandsUseSkillWithoutInitRules(t *testing.T) {
	commands := completionCommands()
	if !containsCompletion(commands, "skill") {
		t.Fatalf("commands missing skill: %#v", commands)
	}
	if containsCompletion(commands, "init-rules") {
		t.Fatalf("commands contain init-rules: %#v", commands)
	}
}

func TestCompletionSkillCommands(t *testing.T) {
	want := []string{"setup", "install", "status", "diff", "update", "validate", "path", "print", "uninstall"}
	got := completionSkillCommands()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skill commands = %#v, want %#v", got, want)
	}
}

func TestShellCompletionsRouteSkillCommands(t *testing.T) {
	for name, script := range map[string]string{
		"zsh":  zshCompletionScript(),
		"bash": bashCompletionScript(),
		"fish": fishCompletionScript(),
	} {
		if !strings.Contains(script, "cm completion skill-commands") {
			t.Fatalf("%s completion does not route skill commands", name)
		}
	}
}

func containsCompletion(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
