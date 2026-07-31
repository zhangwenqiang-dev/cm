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
