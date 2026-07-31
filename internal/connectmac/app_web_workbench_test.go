package connectmac

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func webInlineFunctionSource(t *testing.T, html, declaration string) string {
	t.Helper()
	start := strings.Index(html, declaration)
	if start < 0 {
		t.Fatalf("web inline function is missing %q", declaration)
	}
	end := strings.Index(html[start+1:], "\n    function ")
	if end < 0 {
		t.Fatalf("web inline function %q has no following function boundary", declaration)
	}
	return html[start : start+end+1]
}

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
		`id="technicalOutput" class="output hidden"`,
		`id="workbenchEmptyTask"`,
		`function renderWorkbench(p, status, reminder)`,
		`function handleWorkbenchStatusAction()`,
		`$("statusBtn").dataset.workbenchAction === "details"`,
		`$("technicalOutput").classList.toggle("hidden", !value);`,
		`$("technicalDetails").open = true;`,
		`const workbenchMobileMedia = window.matchMedia("(max-width: 820px)");`,
		`workbenchMobileMedia.addEventListener("change", () => renderSelected());`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("workbench structure is missing %q", want)
		}
	}

	renderSelected := strings.Index(html, `function renderSelected()`)
	renderWorkbenchCall := -1
	if renderSelected >= 0 {
		renderWorkbenchCall = strings.Index(html[renderSelected:], `renderWorkbench(p, status, reminder);`)
	}
	if renderWorkbenchCall < 0 {
		t.Error("renderSelected must delegate workbench state and action rendering to renderWorkbench")
	}

	renderWorkbench := webInlineFunctionSource(t, html, `function renderWorkbench(p, status, reminder)`)
	for _, want := range []string{
		`ConnectMacWorkbench.effectiveState({`,
		`ConnectMacWorkbench.buildActionModel({`,
		`ConnectMacWorkbench.stateCopy(effectiveState)`,
		`hasProfile: !!p,`,
		`mobile: workbenchMobileMedia.matches,`,
		`const statusAction = model.primary === "details" ? model.actions.details : model.actions.refresh;`,
		`applyWorkbenchAction("statusBtn", statusAction`,
		`effectiveState === "releasing" ? "查看释放进度" : "查看任务详情"`,
		`$("statusBtn").dataset.workbenchAction = model.primary === "details" ? "details" : "refresh";`,
		`applyWorkbenchAction("openMacBtn", model.actions.open`,
		`applyWorkbenchAction("releaseMacBtn", model.actions.release`,
		`applyWorkbenchAction("extendReminderBtn", model.actions.extend`,
		`applyWorkbenchAction("cleanupRecordsBtn", model.actions.cleanup`,
		`applyWorkbenchAction("terminalBtn", model.actions.connect`,
		`applyWorkbenchAction("vncBtn", model.actions.vnc`,
		`applyWorkbenchAction("syncBtn", model.actions.transfer`,
		`applyWorkbenchAction("eventsBtn", model.actions.events`,
	} {
		if !strings.Contains(renderWorkbench, want) {
			t.Errorf("renderWorkbench is missing %q", want)
		}
	}

	handler := webInlineFunctionSource(t, html, `function handleWorkbenchStatusAction()`)
	for _, want := range []string{
		`$("technicalDetails").open = true;`,
		`loadStatus();`,
	} {
		if !strings.Contains(handler, want) {
			t.Errorf("workbench details handler is missing %q", want)
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
