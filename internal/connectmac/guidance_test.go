package connectmac

import (
	"strings"
	"testing"
)

func TestFirstUseGuideUsesSkillSetup(t *testing.T) {
	text, ok := guideText("first-use")
	if !ok {
		t.Fatal("first-use guide not found")
	}
	if !strings.Contains(text, "cm skill setup --agent codex --project .") {
		t.Fatalf("first-use guide missing cm skill setup: %q", text)
	}
	if strings.Contains(text, "cm init-rules") {
		t.Fatalf("first-use guide contains removed command: %q", text)
	}
}
