package connectmac

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebOperationHeaderUsesVerticalProfileIdentityWithoutHomeButton(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)

	for _, want := range []string{
		`class="workbench-head`,
		`class="selected-title"`,
		`id="selectedProfileName" class="selected-profile-name"`,
		`id="selectedAppleEmail" class="selected-apple-email"`,
		`id="workbenchStateBadge"`,
		`.selected-title {`,
		`.selected-profile-name,`,
		`.selected-apple-email {`,
		`$("selectedProfileName").textContent = p ? p.name : "未选择";`,
		`$("selectedAppleEmail").textContent = p ? (p.apple_email || "无 Apple 邮箱") : "-";`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("operation header is missing %q", want)
		}
	}

	for _, unwanted := range []string{
		`id="selectedTitle"`,
		`id="backHomeBtn"`,
		`"backHomeBtn"`,
		`$("backHomeBtn")`,
		`返回首页`,
	} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("operation header still contains %q", unwanted)
		}
	}
}
