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
		`id="workbenchTaskProgress" aria-label="生命周期任务进行中"></progress>`,
		`id="technicalDetails"`,
		`id="technicalOutput" class="output hidden"`,
		`id="workbenchEmptyTask"`,
		`function renderWorkbench(p, status, reminder)`,
		`function handleWorkbenchStatusAction()`,
		`$("statusBtn").dataset.workbenchAction === "details"`,
		`$("technicalOutput").classList.toggle("hidden", !value);`,
		`$("technicalDetails").open = true;`,
		`const workbenchMobileMedia = window.matchMedia("(max-width: 640px)");`,
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
	for _, want := range []string{
		`ConnectMacWorkbench.activeLifecycleTask(state.jobs, p?.name || "")`,
		`$("workbenchEmptyTask").classList.toggle("hidden", !!task);`,
		`$("workbenchActiveTask").classList.toggle("hidden", !task);`,
		`task.label`,
		`task.id`,
		`task.request_id`,
		`task.actor`,
		`task.started_at`,
	} {
		if !strings.Contains(renderSelected, want) {
			t.Errorf("renderSelected active task rendering is missing %q", want)
		}
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

	api := webInlineFunctionSource(t, html, `async function api(path, options = {})`)
	for _, want := range []string{
		`res.headers.get("X-Request-ID")`,
		`await res.text()`,
		`JSON.parse(`,
		`body && typeof body === "object"`,
		`body.request_id`,
		`createAPIError(`,
		`body.error_code || body.code`,
		`res.status === 401`,
	} {
		if !strings.Contains(api, want) {
			t.Errorf("api response metadata handling is missing %q", want)
		}
	}
	headerRead := strings.Index(api, `res.headers.get("X-Request-ID")`)
	bodyRead := strings.Index(api, `await res.text()`)
	bodyParse := strings.Index(api, `JSON.parse(`)
	if headerRead < 0 || bodyRead < headerRead || bodyParse < bodyRead {
		t.Error("api must capture X-Request-ID before reading and parsing the response body")
	}
	apiError := webInlineFunctionSource(t, html, `function createAPIError(message, status, requestID, errorCode)`)
	for _, want := range []string{
		`sanitizedOperationMessage(message)`,
		`error.status = status;`,
		`error.requestID = requestID || "";`,
		`error.errorCode = errorCode || "";`,
	} {
		if !strings.Contains(apiError, want) {
			t.Errorf("createAPIError is missing %q", want)
		}
	}

	showOperationError := webInlineFunctionSource(t, html, `function showOperationError(summary, error)`)
	for _, want := range []string{
		`error?.name === "AbortError"`,
		`context canceled`,
		`请求超时，任务状态可能仍会更新`,
		`error?.errorCode`,
		`error?.requestID`,
		`$("technicalDetails").open = true;`,
	} {
		if !strings.Contains(showOperationError, want) {
			t.Errorf("showOperationError is missing %q", want)
		}
	}

	runAWS := extractWebSource(t, html, "async function runAWS(", "\n    async function previewAWS(")
	for _, want := range []string{
		`ConnectMacWorkbench.activeLifecycleTask(state.jobs, p.name)`,
		`if (activeTask)`,
		`任务已提交，页面会自动更新进度`,
		`任务已提交，状态刷新失败，页面将继续自动更新`,
		`closeAWSConfirm();`,
		`state.pendingAWS = null;`,
		`await Promise.all([loadJobs({ refreshReminders: true })`,
		`showOperationError(`,
		`return true;`,
	} {
		if !strings.Contains(runAWS, want) {
			t.Errorf("runAWS lifecycle feedback is missing %q", want)
		}
	}
	if strings.Count(runAWS, `catch (err)`) < 2 {
		t.Error("runAWS must separate confirmed submission failure from post-submit refresh failure")
	}
	if strings.Count(runAWS, `任务提交失败`) != 1 {
		t.Error("runAWS must report submission failure only for the confirmed POST")
	}
	for _, unwanted := range []string{
		`后台任务已启动`,
		`打开成功`,
		`释放完成`,
	} {
		if strings.Contains(runAWS, unwanted) {
			t.Errorf("runAWS must not report submission as completion with %q", unwanted)
		}
	}

	previewAWS := extractWebSource(t, html, "async function previewAWS(", "\n    function showAWSConfirm(")
	for _, want := range []string{`if (activeTask)`, `setStatus(activeTask.label);`, `showOperationError(`} {
		if !strings.Contains(previewAWS, want) {
			t.Errorf("previewAWS is missing %q", want)
		}
	}

	refreshStatus := extractWebSource(t, html, "async function refreshStatus(profile, showOutput, options", "\n    async function runAWS(")
	if !strings.Contains(refreshStatus, `showOperationError(`) {
		t.Error("foreground AWS status failures must use showOperationError")
	}

	errorPaths := []struct {
		start string
		end   string
	}{
		{"async function submitAutoRelease()", "\n    async function saveReleaseReminder()"},
		{"async function saveReleaseReminder()", "\n    const beijingTimeFormatter"},
		{"async function cleanupLocalRecords()", "\n    async function confirmPendingAWS()"},
		{"async function startTunnel(profile)", "\n    async function openSync(profile)"},
		{"async function runSync(direction)", "\n    function terminalSetStatus"},
		{"async function connectLocalTerminal(profile)", "\n    function closeTerminal("},
		{"async function addMember()", "\n    async function toggleMember("},
		{"async function toggleMember(", "\n    async function resetMemberPassword("},
		{"async function saveMemberPassword()", "\n    function openOwnPasswordEditor()"},
		{"async function changeOwnPassword()", "\n    async function generateOwnToken()"},
		{"async function runAPITokenAction(", "\n    function showAPIToken("},
		{"async function saveManagedProfile()", "\n    async function deleteManagedProfile("},
		{"async function deleteManagedProfile(", "\n    async function setManagedProfileStatus("},
		{"async function setManagedProfileStatus(", "\n    async function saveMemberProfiles()"},
		{"async function saveMemberProfiles()", "\n    async function setProfileAccess("},
		{"async function setProfileAccess(", "\n    async function saveSettings()"},
		{"async function saveSettings()", "\n    async function loadEvents("},
	}
	for _, path := range errorPaths {
		source := extractWebSource(t, html, path.start, path.end)
		if !strings.Contains(source, `showOperationError(`) {
			t.Errorf("%s must use showOperationError", path.start)
		}
		if strings.Contains(source, `setOutput(String(err.message || err))`) {
			t.Errorf("%s must not emit raw operation errors", path.start)
		}
	}
	autoRelease := extractWebSource(t, html, "async function submitAutoRelease()", "\n    async function saveReleaseReminder()")
	if !strings.Contains(autoRelease, `err.status === 409`) || !strings.Contains(autoRelease, `await loadReleaseReminders().catch`) {
		t.Error("auto release must preserve conflict refresh behavior")
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

func TestWebWorkbenchDialogContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`id="awsConfirmProfile"`,
		`id="awsConfirmApple"`,
		`id="awsConfirmOwner"`,
		`id="awsConfirmResourceSummary"`,
		`id="awsConfirmEIPNotice"`,
		`data-dialog-initial-focus`,
		`function openDialog(`,
		`function closeDialog(`,
		`function trapDialogFocus(`,
		`function validatePendingAWS(`,
		`previewSucceeded: true`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("AWS confirmation contract missing %q", want)
		}
	}

	confirm := webInlineFunctionSource(t, html, `async function confirmPendingAWS()`)
	for _, want := range []string{
		`validatePendingAWS()`,
		`runAWS(pending.command, true)`,
	} {
		if !strings.Contains(confirm, want) {
			t.Errorf("AWS confirmation validation missing %q", want)
		}
	}
	validate := webInlineFunctionSource(t, html, `function validatePendingAWS()`)
	for _, want := range []string{`profile.name !== state.pendingAWS.profile`, `model.actions.open`, `model.actions.release`} {
		if !strings.Contains(validate, want) {
			t.Errorf("AWS pending preview validation missing %q", want)
		}
	}

	for _, contract := range []struct {
		function string
		want     string
	}{
		{`async function addMember()`, `focusInvalidField(`},
		{`async function saveMemberPassword()`, `focusInvalidField(`},
		{`async function changeOwnPassword()`, `focusInvalidField(`},
		{`async function saveReleaseReminder()`, `focusInvalidField(`},
		{`async function saveManagedProfile()`, `focusInvalidField(`},
		{`async function addMember()`, `closeMemberForm();`},
		{`async function saveMemberPassword()`, `closeMemberPasswordEditor();`},
		{`async function changeOwnPassword()`, `closeOwnPasswordEditor();`},
		{`async function saveReleaseReminder()`, `closeReleaseReminderEditor();`},
		{`async function saveManagedProfile()`, `closeProfileForm();`},
		{`async function saveMemberProfiles()`, `closeMemberProfileEditor();`},
		{`async function runAPITokenAction(action, self, email)`, `if (action === "delete") closeAPIToken();`},
	} {
		source := webInlineFunctionSource(t, html, contract.function)
		if !strings.Contains(source, contract.want) {
			t.Errorf("%s dialog behavior missing %q", contract.function, contract.want)
		}
	}
}

func TestWebWorkbenchManagement(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`Profiles 通过弹框表单新增或编辑`,
		`id="profileEmptyState"`,
		`id="memberEmptyState"`,
		`id="managedProfileEmptyState"`,
		`校验题加载失败，点击换一题重试`,
		`const componentLoadTimeoutMS = 8000`,
		`暂无事件`,
		`暂无后台任务`,
		`id="managementSuccessStatus"`,
		`aria-live="polite"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("management/loading contract missing %q", want)
		}
	}

	userManagement := extractWebSource(t, html, `<section id="userManagementView"`, "\n        <section id=\"profilesAdminView\"")
	for _, unwanted := range []string{
		`id="managedProfiles"`,
		`id="systemSettingsPanel"`,
		`id="accountSettingsPanel"`,
		`<h2>Profiles</h2>`,
	} {
		if strings.Contains(userManagement, unwanted) {
			t.Errorf("user management must not contain %q", unwanted)
		}
	}
	userView := extractWebSource(t, html, `<section id="userView"`, "\n        <section id=\"userManagementView\"")
	for _, want := range []string{
		`id="systemSettingsPanel"`,
		`id="accountSettingsPanel"`,
		`<h2>系统设置</h2>`,
		`<h2>管理员账号</h2>`,
	} {
		if !strings.Contains(userView, want) {
			t.Errorf("user view must contain %q", want)
		}
	}
	roleVisibility := webInlineFunctionSource(t, html, `function applyRoleVisibility()`)
	for _, want := range []string{
		`el.id === "systemSettingsPanel"`,
		`el.id === "accountSettingsPanel"`,
		`applyUserViewMode();`,
	} {
		if !strings.Contains(roleVisibility, want) {
			t.Errorf("role visibility must preserve explicit user-view mode with %q", want)
		}
	}

	api := webInlineFunctionSource(t, html, `async function api(path, options = {})`)
	for _, want := range []string{
		`const { timeoutMs = 0, ...fetchOptions } = options;`,
		`const controller = timeoutMs > 0 ? new AbortController() : null;`,
		`timedOut = true;`,
		`controller.abort()`,
		`"component_timeout"`,
		`window.clearTimeout(timer)`,
	} {
		if !strings.Contains(api, want) {
			t.Errorf("timeout-aware api contract missing %q", want)
		}
	}

	loadChallenge := webInlineFunctionSource(t, html, `async function loadChallenge()`)
	for _, want := range []string{
		`timeoutMs: componentLoadTimeoutMS`,
		`state.challenge = null;`,
		`校验题加载失败，点击换一题重试`,
		`$("refreshChallengeBtn").disabled = false;`,
		`updateAuthSubmitState();`,
	} {
		if !strings.Contains(loadChallenge, want) {
			t.Errorf("recoverable challenge contract missing %q", want)
		}
	}

	for _, contract := range []struct {
		function string
		path     string
	}{
		{`async function loadMembers()`, `/api/members`},
		{`async function loadManagedProfiles()`, `/api/managed-profiles`},
		{`async function loadSettings()`, `/api/settings`},
	} {
		source := webInlineFunctionSource(t, html, contract.function)
		if !strings.Contains(source, contract.path) || !strings.Contains(source, `timeoutMs: componentLoadTimeoutMS`) {
			t.Errorf("%s must use the component read timeout", contract.function)
		}
	}
	loadProfiles := webInlineFunctionSource(t, html, `async function loadProfiles(options = {})`)
	for _, want := range []string{
		`const timeoutMs = Number(options.timeoutMs) || 0;`,
		`timeoutMs > 0 ? { timeoutMs } : {}`,
		`api("/api/profiles", requestOptions)`,
	} {
		if !strings.Contains(loadProfiles, want) {
			t.Errorf("optional Profile load timeout contract missing %q", want)
		}
	}
	loadAppData := webInlineFunctionSource(t, html, `async function loadAppData(options = {})`)
	if strings.Count(loadAppData, `loadProfiles({ timeoutMs: componentLoadTimeoutMS })`) != 2 {
		t.Error("initial admin and member Profile loads must explicitly use the component timeout")
	}
	refreshLifecycleProfiles := webInlineFunctionSource(t, html, `async function refreshLifecycleProfiles(jobs)`)
	if !strings.Contains(refreshLifecycleProfiles, `loadProfiles({ timeoutMs: 0 })`) ||
		strings.Contains(refreshLifecycleProfiles, `componentLoadTimeoutMS`) {
		t.Error("lifecycle Profile refresh must explicitly avoid the component timeout")
	}
	for _, mutation := range []string{
		`async function runAWS(`,
		`async function refreshStatus(`,
		`async function runSync(`,
		`async function connectLocalTerminal(`,
	} {
		source := webInlineFunctionSource(t, html, mutation)
		if strings.Contains(source, `timeoutMs: componentLoadTimeoutMS`) {
			t.Errorf("%s must not use the component read timeout", mutation)
		}
	}

	for _, want := range []string{
		`userViewMode: "personal"`,
		`function applyUserViewMode()`,
		`state.userViewMode = "personal";`,
		`state.userViewMode = "settings";`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("user-view entry mode contract missing %q", want)
		}
	}
	currentUserView := webInlineFunctionSource(t, html, `function openCurrentUserView()`)
	if !strings.Contains(currentUserView, `state.userViewMode = "personal";`) ||
		!strings.Contains(currentUserView, `showView("userView");`) {
		t.Error("avatar entry must select the personal user-view mode")
	}
	accountSettings := webInlineFunctionSource(t, html, `async function toggleAccountSettings()`)
	if !strings.Contains(accountSettings, `state.userViewMode = "settings";`) ||
		!strings.Contains(accountSettings, `showView("userView");`) {
		t.Error("account settings entry must select the settings user-view mode")
	}
	applyUserViewMode := webInlineFunctionSource(t, html, `function applyUserViewMode()`)
	for _, want := range []string{
		`state.userViewMode === "settings"`,
		`$("systemSettingsPanel").classList.toggle("hidden", !showSettings);`,
		`$("accountSettingsPanel").classList.toggle("hidden", !showSettings);`,
	} {
		if !strings.Contains(applyUserViewMode, want) {
			t.Errorf("user-view mode rendering missing %q", want)
		}
	}

	for _, contract := range []struct {
		function string
		reload   string
		close    string
		success  string
	}{
		{`async function addMember()`, `await loadMembers();`, `closeMemberForm();`, `announceManagementSuccess(`},
		{`async function saveMemberPassword()`, `await loadMembers();`, `closeMemberPasswordEditor();`, `announceManagementSuccess(`},
		{`async function changeOwnPassword()`, `renderUserPage();`, `closeOwnPasswordEditor();`, `announceManagementSuccess(`},
		{`async function saveManagedProfile()`, `await Promise.all([loadManagedProfiles(), loadProfiles({ timeoutMs: componentLoadTimeoutMS })]);`, `closeProfileForm();`, `announceManagementSuccess(`},
		{`async function saveMemberProfiles()`, `await Promise.all([loadMembers(), loadManagedProfiles(), loadProfiles({ timeoutMs: componentLoadTimeoutMS })]);`, `closeMemberProfileEditor();`, `announceManagementSuccess(`},
	} {
		source := webInlineFunctionSource(t, html, contract.function)
		reloadAt := strings.Index(source, contract.reload)
		closeAt := strings.Index(source, contract.close)
		successAt := strings.Index(source, contract.success)
		if reloadAt < 0 || closeAt < reloadAt || successAt < closeAt {
			t.Errorf("%s must reload, render/close, then announce success", contract.function)
		}
		if !strings.Contains(source, `showOperationError(`) {
			t.Errorf("%s must keep correlated failure feedback", contract.function)
		}
	}
}

func TestWebDialogFocusLifecycleContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`const dialogTriggers = new WeakMap();`,
		`const pendingDialogInitialFocus = new Set();`,
		`const pendingDialogRestoreFocus = new Set();`,
		`const dialogLayerStack = [];`,
		`function flushDialogFocus()`,
		`function scheduleDialogFocusFlush()`,
		`pendingDialogInitialFocus.add(layer);`,
		`pendingDialogRestoreFocus.add(layer);`,
		`dialogTriggers.get(layer)`,
		`dialogTriggers.delete(layer)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dialog focus lifecycle contract missing %q", want)
		}
	}
	setBusy := webInlineFunctionSource(t, html, `function setBusy(busy, message = "处理中...")`)
	if !strings.Contains(setBusy, `if (!busy) {`) || !strings.Contains(setBusy, `scheduleDialogFocusFlush();`) {
		t.Error("setBusy(false) must flush deferred dialog focus after controls render")
	}
	revalidate := strings.Index(setBusy, `renderAWSConfirmValidation();`)
	schedule := strings.Index(setBusy, `scheduleDialogFocusFlush();`)
	if revalidate < 0 || schedule < 0 || revalidate > schedule {
		t.Error("setBusy(false) must revalidate the visible AWS dialog before scheduling focus")
	}
	focusManager := extractWebSource(t, html, "const dialogTriggers = new WeakMap();", "\n    function cancelDialog(")
	if strings.Contains(focusManager, "setInterval(") || strings.Contains(focusManager, "setTimeout(") {
		t.Error("dialog focus lifecycle must not use polling")
	}
}

func TestWebDialogFocusLifecycleBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for dialog focus behavior test: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	manager := extractWebSource(t, html, "const dialogTriggers = new WeakMap();", "\n    function cancelDialog(")
	setBusy := webInlineFunctionSource(t, html, `function setBusy(busy, message = "处理中...")`)
	harness := `
import assert from "node:assert/strict";

class FakeClassList {
  constructor(values = []) { this.values = new Set(values); }
  contains(value) { return this.values.has(value); }
  add(value) { this.values.add(value); }
  remove(value) { this.values.delete(value); }
  toggle(value, force) {
    if (force === undefined ? !this.values.has(value) : force) this.values.add(value);
    else this.values.delete(value);
  }
}

class FakeElement {
  constructor(id, classes = []) {
    this.id = id;
    this.classList = new FakeClassList(classes);
    this.disabled = false;
    this.hidden = false;
    this.attributes = new Map();
    this.controls = [];
    this.parent = null;
    this.style = { display: "block", visibility: "visible" };
    this.focusCount = 0;
  }
  focus() { document.activeElement = this; this.focusCount += 1; }
  getAttribute(name) { return this.attributes.get(name) || null; }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  addEventListener() {}
  querySelectorAll() { return this.controls.slice(); }
  querySelector(selector) {
    if (selector.includes("data-dialog-initial-focus")) {
      return this.controls.find((item) => item.attributes.has("data-dialog-initial-focus")) || null;
    }
    return this.controls[0] || null;
  }
  contains(element) {
    for (let current = element; current; current = current.parent) {
      if (current === this) return true;
    }
    return false;
  }
  closest(selector) {
    for (let current = this; current; current = current.parent) {
      if (selector === ".hidden" && current.classList.contains("hidden")) return current;
      if (selector === ".picker-layer" && current.classList.contains("picker-layer")) return current;
      if (selector === "section, main, .card" && current.classList.contains("surface")) return current;
    }
    return null;
  }
}

const elements = new Map();
const attached = new Set();
function register(element) {
  attached.add(element);
  if (element.id) elements.set(element.id, element);
  return element;
}
function $(id) {
  if (!elements.has(id)) register(new FakeElement(id));
  return elements.get(id);
}
function layer(id, controls) {
  const result = register(new FakeElement(id, ["picker-layer", "hidden"]));
  result.controls = controls;
  controls.forEach((control) => {
    register(control);
    control.parent = result;
  });
  return result;
}

const document = {
  activeElement: null,
  contains: (element) => attached.has(element),
  querySelectorAll: (selector) => selector.startsWith("[data-")
    ? []
    : [...attached].filter((element) => !element.classList.contains("picker-layer"))
};
const window = { getComputedStyle: (element) => element.style };
const HTMLElement = FakeElement;
const state = { busy: false };

` + manager + `

let awsValidationCalls = 0;
function renderAWSConfirmValidation() {
  awsValidationCalls += 1;
  $("runAWSConfirmBtn").disabled = true;
}

` + setBusy + `

// Opening during busy defers focus. Validation may keep the requested control
// disabled, so the flush must choose the first enabled visible fallback.
const openTrigger = $("openMacBtn");
const awsClose = $("awsConfirmCloseBtn");
const awsConfirm = $("runAWSConfirmBtn");
awsConfirm.setAttribute("data-dialog-initial-focus", "");
const awsLayer = layer("awsConfirmLayer", [awsClose, awsConfirm]);
openTrigger.focus();
setBusy(true);
openDialog(awsLayer, awsConfirm);
assert.equal(document.activeElement, openTrigger);
setBusy(false);
await Promise.resolve();
assert.equal(awsValidationCalls, 1);
assert.equal(awsConfirm.disabled, true);
assert.equal(document.activeElement, awsClose);

// Closing during busy defers restoration until setBusy(false) has re-enabled
// and rendered the trigger.
closeDialog(awsLayer);
const passwordTrigger = $("openOwnPasswordBtn");
const passwordInput = $("userCurrentPassword");
const passwordClose = $("ownPasswordCloseBtn");
const passwordLayer = layer("ownPasswordLayer", [passwordClose, passwordInput]);
passwordTrigger.focus();
openDialog(passwordLayer, passwordInput);
assert.equal(document.activeElement, passwordInput);
setBusy(true);
closeDialog(passwordLayer);
assert.equal(document.activeElement, passwordInput);
setBusy(false);
await Promise.resolve();
assert.equal(document.activeElement, passwordTrigger);

// Per-layer triggers preserve nesting. Closing a hidden unrelated layer must
// not clear the active child or parent restoration chain.
const original = $("openMemberFormBtn");
const parentControl = new FakeElement("parentControl");
const parentClose = new FakeElement("parentClose");
const parentLayer = layer("parentLayer", [parentClose, parentControl]);
const childControl = new FakeElement("childControl");
const childClose = new FakeElement("childClose");
const childLayer = layer("childLayer", [childClose, childControl]);
const unrelated = layer("unrelatedLayer", [new FakeElement("unrelatedClose")]);
original.focus();
openDialog(parentLayer, parentControl);
openDialog(childLayer, childControl);
closeDialog(unrelated);
closeDialog(childLayer);
assert.equal(document.activeElement, parentControl);
closeDialog(parentLayer);
assert.equal(document.activeElement, original);

// Reopening the same layer before a deferred restore keeps its original
// trigger instead of replacing it with focus stranded inside the hidden layer.
const continuousTrigger = $("openProfileFormBtn");
const continuousInput = $("profileFormName");
const continuousClose = $("profileFormCloseBtn");
const continuousLayer = layer("profileFormLayer", [continuousClose, continuousInput]);
continuousTrigger.focus();
openDialog(continuousLayer, continuousInput);
setBusy(true);
closeDialog(continuousLayer);
openDialog(continuousLayer, continuousInput);
setBusy(false);
await Promise.resolve();
assert.equal(document.activeElement, continuousInput);
closeDialog(continuousLayer);
assert.equal(document.activeElement, continuousTrigger);
`
	script := filepath.Join(t.TempDir(), "dialog_focus_behavior.mjs")
	if err := os.WriteFile(script, []byte(harness), 0o600); err != nil {
		t.Fatalf("write dialog focus node test: %v", err)
	}
	output, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("dialog focus node behavior test failed: %v\n%s", err, output)
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

	loadProfiles := extractWebSource(t, html, "async function loadProfiles(options = {})", "\n    async function loadReleaseReminders(")
	for _, want := range []string{
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
		`await loadProfiles({ refreshStatuses: false, timeoutMs: componentLoadTimeoutMS });`,
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
