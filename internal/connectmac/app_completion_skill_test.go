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
	if !strings.Contains(out.String(), "cm skill setup") {
		t.Fatalf("usage missing cm skill: %q", out.String())
	}
	if strings.Contains(out.String(), "cm init-rules") {
		t.Fatalf("usage contains removed command: %q", out.String())
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
	want := []string{"setup", "install", "status", "update", "validate", "path", "print", "uninstall"}
	if got := completionSkillCommands(); !reflect.DeepEqual(got, want) {
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
