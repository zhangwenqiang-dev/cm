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

	renderSelected := webInlineFunctionSource(t, html, `function renderSelected()`)
	if !strings.Contains(renderSelected, `renderWorkbench(p, status, reminder);`) {
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

func TestWebHomeUsesWorkbenchEntryAndRefreshTimestamp(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)

	for _, want := range []string{
		`id="profileLastUpdated"`,
		`aria-live="polite"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("profile home is missing %q", want)
		}
	}
	if strings.Contains(html, `title="选择当前 Profile"`) {
		t.Error("profile home must not retain the legacy select-profile title")
	}

	renderProfiles := webInlineFunctionSource(t, html, `function renderProfiles()`)
	for _, want := range []string{
		`data-workbench`,
		`进入工作台`,
		`管理`,
		`event.key !== "Enter"`,
		`event.key !== " "`,
		`event.target.closest("button, a")`,
	} {
		if !strings.Contains(renderProfiles, want) {
			t.Errorf("renderProfiles is missing %q", want)
		}
	}
	for _, unwanted := range []string{
		`data-terminal`,
		`data-start`,
		`data-sync`,
		`title="选择当前 Profile"`,
	} {
		if strings.Contains(renderProfiles, unwanted) {
			t.Errorf("renderProfiles must not contain %q", unwanted)
		}
	}
	if got := strings.Count(renderProfiles, "p.apple_email"); got != 1 {
		t.Errorf("renderProfiles must render the Apple email once, found %d references", got)
	}

	scheduleProfileRefresh := webInlineFunctionSource(t, html, `function scheduleProfileRefresh()`)
	for _, want := range []string{
		`refreshVisibleStatuses`,
		`document.visibilityState`,
		`state.auth?.authenticated`,
		`profileRefreshTimer`,
		`profileRefreshPromise`,
		`new AbortController()`,
		`profileRefreshGeneration`,
		`background: true`,
		`startedGeneration: generation`,
		`状态更新失败，正在重试`,
		`最后更新（北京时间）`,
	} {
		if !strings.Contains(scheduleProfileRefresh, want) {
			t.Errorf("scheduleProfileRefresh is missing %q", want)
		}
	}

	refreshVisibleStatuses := extractWebSource(t, html, "async function refreshVisibleStatuses(", "\n    function clearProfileRefreshTimer()")
	for _, want := range []string{
		`!state.auth?.authenticated`,
		`document.visibilityState !== "visible"`,
		`return false;`,
		`profileRefreshPromise`,
		`await activeRefresh`,
		`ConnectMacWorkbench.shouldApplyProfileRefresh({`,
		`background: background`,
		`startedGeneration: startedGeneration`,
		`renderProfiles();`,
		`renderSelected();`,
	} {
		if !strings.Contains(refreshVisibleStatuses, want) {
			t.Errorf("refreshVisibleStatuses is missing %q", want)
		}
	}

	stopProfileRefresh := webInlineFunctionSource(t, html, `function stopProfileRefresh()`)
	for _, want := range []string{
		`clearProfileRefreshTimer();`,
		`profileRefreshController.abort();`,
		`profileRefreshController = null;`,
		`profileRefreshGeneration += 1;`,
	} {
		if !strings.Contains(stopProfileRefresh, want) {
			t.Errorf("stopProfileRefresh is missing %q", want)
		}
	}

	clearProfileRefreshTimer := webInlineFunctionSource(t, html, `function clearProfileRefreshTimer()`)
	if !strings.Contains(clearProfileRefreshTimer, `window.clearTimeout(profileRefreshTimer)`) {
		t.Error("clearProfileRefreshTimer must clear the active timer")
	}

	refreshStatus := extractWebSource(t, html, "async function refreshStatus(profile, showOutput, options", "\n    async function runAWS(")
	for _, want := range []string{
		`state.statusRefreshRequests.get(profile)`,
		`await existingRequest`,
		`return finish(result);`,
		`setOutput(result.body?.output || "");`,
		`return refreshStatus(profile, showOutput, { background: false });`,
		`requestPromise.finally(`,
		`state.statusRefreshRequests.get(profile) === trackedPromise`,
		`state.statusRefreshRequests.delete(profile)`,
		`api("/api/aws/status?profile=" + encodeURIComponent(profile), { signal: signal })`,
		`ConnectMacWorkbench.shouldApplyProfileRefresh({`,
		`startedGeneration`,
		`err.name === "AbortError"`,
	} {
		if !strings.Contains(refreshStatus, want) {
			t.Errorf("refreshStatus signal handling is missing %q", want)
		}
	}
	const helperMarker = `ConnectMacWorkbench.shouldApplyProfileRefresh({`
	const assignmentMarker = `state.statuses[profile] =`
	previousAssignment := -1
	assignmentCount := 0
	for searchFrom := 0; ; {
		next := strings.Index(refreshStatus[searchFrom:], assignmentMarker)
		if next < 0 {
			break
		}
		assignment := searchFrom + next
		helperCall := strings.LastIndex(refreshStatus[:assignment], helperMarker)
		if helperCall <= previousAssignment {
			t.Errorf("refreshStatus assignment %d must have a fresh shouldApplyProfileRefresh check", assignmentCount+1)
		}
		previousAssignment = assignment
		assignmentCount++
		searchFrom = assignment + len(assignmentMarker)
	}
	if assignmentCount != 2 {
		t.Errorf("refreshStatus status assignment count = %d, want 2", assignmentCount)
	}
	waitForExisting := strings.Index(refreshStatus, `await existingRequest`)
	retryForeground := strings.Index(refreshStatus, `return refreshStatus(profile, showOutput, { background: false });`)
	if waitForExisting < 0 || retryForeground < waitForExisting {
		t.Error("manual refresh must await an existing request before retrying an aborted or stale background refresh")
	}

	loadProfiles := extractWebSource(t, html, "async function loadProfiles()", "\n    async function loadReleaseReminders(")
	for _, want := range []string{
		`const options = arguments[0] || {};`,
		`const startedGeneration = profileRefreshGeneration;`,
		`options.refreshStatuses !== false`,
		`refreshVisibleStatuses({ background: true, startedGeneration: startedGeneration });`,
	} {
		if !strings.Contains(loadProfiles, want) {
			t.Errorf("loadProfiles background refresh is missing %q", want)
		}
	}
	if strings.Contains(loadProfiles, `await refreshVisibleStatuses({ background: true`) {
		t.Error("loadProfiles initial background refresh must not delay login")
	}

	refreshAllData := extractWebSource(t, html, "async function refreshAllData()", "\n\n    $(\"refresh\").addEventListener")
	for _, want := range []string{
		`stopProfileRefresh();`,
		`await loadProfiles({ refreshStatuses: false });`,
		`await refreshVisibleStatuses({`,
		`background: false`,
		`if (refreshed === false) {`,
		`setStatus("状态刷新失败，其他数据已更新");`,
		`if (refreshed !== true) return;`,
		`setStatus("刷新完成");`,
		`scheduleProfileRefresh();`,
	} {
		if !strings.Contains(refreshAllData, want) {
			t.Errorf("manual top refresh is missing %q", want)
		}
	}
	awaitStatuses := strings.Index(refreshAllData, `await refreshVisibleStatuses({`)
	partialFailure := strings.Index(refreshAllData, `setStatus("状态刷新失败，其他数据已更新");`)
	refreshComplete := strings.Index(refreshAllData, `setStatus("刷新完成");`)
	if awaitStatuses < 0 || partialFailure < awaitStatuses || refreshComplete < partialFailure {
		t.Error("manual top refresh must await visible statuses before reporting completion")
	}
}
