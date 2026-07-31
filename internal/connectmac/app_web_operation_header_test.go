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
	cssData, err := os.ReadFile(filepath.Join("..", "..", "web", "assets", "connectmac-workbench.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssData)

	for _, want := range []string{
		`class="workbench-head`,
		`class="selected-title"`,
		`id="selectedProfileName" class="selected-profile-name"`,
		`id="selectedAppleEmail" class="selected-apple-email"`,
		`id="selectedProfileRegion" class="selected-profile-region"`,
		`id="workbenchStateBadge"`,
		`.selected-title {`,
		`.selected-profile-name,`,
		`.selected-apple-email,`,
		`.selected-profile-region {`,
		`$("selectedProfileName").textContent = p ? p.name : "未选择";`,
		`$("selectedAppleEmail").textContent = p ? (p.apple_email || "无 Apple 邮箱") : "-";`,
		`$("selectedProfileRegion").textContent = "Region：" + (p ? (p.region || "-") : "-");`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("operation header is missing %q", want)
		}
	}
	for _, want := range []string{
		`.workbench-actions button {`,
		`flex: 0 0 auto;`,
		`white-space: nowrap;`,
		`.profile-card-value`,
		`overflow-wrap: anywhere;`,
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("operation header CSS is missing %q", want)
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
