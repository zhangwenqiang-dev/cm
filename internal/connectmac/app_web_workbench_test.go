package connectmac

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebWorkbenchStructure(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)

	for _, want := range []string{
		`href="assets/connectmac-workbench.css"`,
		`src="assets/connectmac-workbench.js"`,
		`id="workbenchRecommendation"`,
		`id="workbenchActionReason"`,
		`id="workbenchTaskPanel"`,
		`id="technicalDetails"`,
		`id="workbenchEmptyTask"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("workbench structure is missing %q", want)
		}
	}

	bootstrapCSS := strings.Index(html, `href="/vendor/bootstrap/bootstrap.min.css"`)
	workbenchCSS := strings.Index(html, `href="assets/connectmac-workbench.css"`)
	bootstrapJS := strings.Index(html, `src="/vendor/bootstrap/bootstrap.bundle.min.js"`)
	workbenchJS := strings.Index(html, `src="assets/connectmac-workbench.js"`)
	inlineAppJS := strings.Index(html, "<script>\n    const state =")
	if bootstrapCSS < 0 || workbenchCSS < bootstrapCSS {
		t.Error("workbench stylesheet must load after Bootstrap")
	}
	if bootstrapJS < 0 || workbenchJS < bootstrapJS || inlineAppJS < workbenchJS {
		t.Error("workbench script must load after Bootstrap JS and before the inline app script")
	}

	cssData, err := os.ReadFile(filepath.Join("..", "..", "web", "assets", "connectmac-workbench.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssData)
	if !strings.Contains(css, ".view.workbench {\n  display: grid;\n}") {
		t.Error("workbench stylesheet must isolate the inline .view display override in .view.workbench")
	}
	if !strings.Contains(css, ".workbench {\n  gap: 16px;\n  padding: 18px;") {
		t.Error("base .workbench rule must own desktop gap and padding")
	}
	if !strings.Contains(css, "@media (max-width: 820px) {\n  .workbench {\n    padding: 12px;\n  }") {
		t.Error("mobile .workbench rule must override padding at 820px")
	}
}

func TestWebWorkbenchStateModel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to validate the web workbench state model")
	}

	script := filepath.Join("..", "..", "scripts", "check-web-workbench.mjs")
	output, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("web workbench state model check failed: %v\n%s", err, output)
	}
}
