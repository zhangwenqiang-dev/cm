package connectmac

import (
	"strings"
	"testing"
)

func TestFirstUseGuideUsesGuidedInitAndSharedProfiles(t *testing.T) {
	text, ok := guideText("first-use")
	if !ok {
		t.Fatal("first-use guide not found")
	}
	for _, want := range []string{
		"cm init",
		"member token",
		"rerun cm init",
		"cm list",
		"Shared Profiles come from the ConnectMac server.",
		"Use local config only for member PEM overrides.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("first-use guide missing %q: %q", want, text)
		}
	}
	for _, unwanted := range []string{"cm init-rules", "cm profile wizard", "cm profile import"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("first-use guide contains obsolete command %q: %q", unwanted, text)
		}
	}
}
