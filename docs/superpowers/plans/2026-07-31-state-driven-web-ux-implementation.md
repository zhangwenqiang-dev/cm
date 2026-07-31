# ConnectMac State-Driven Web UX Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reorganize the ConnectMac web interface into a state-driven workbench with persistent task feedback, clear page responsibilities, and reliable desktop/mobile behavior.

**Architecture:** Keep the existing Go web server, Bootstrap 5.3 shell, APIs, AWS lifecycle coordinator, and local Agent. Add a small pure JavaScript state/action module, use it as the single source for action availability, and progressively replace the operation page and home rendering without changing mutation contracts or AWS safety semantics.

**Tech Stack:** Go 1.25, standard `net/http`, vanilla JavaScript, Bootstrap 5.3, existing CSS, Node.js `vm`/`assert` for pure frontend tests, Go tests for HTTP and source contracts.

---

## Implementation Boundaries

- Execute this plan in a clean worktree created from commit `6e749e2`.
- Do not modify `.mcp.json`, `CLAUDE.md`, `web-ui/`, or `.superpowers/`.
- Do not introduce React, Vue, a bundler, npm dependencies, or a second
  frontend state store.
- Do not change AWS create/open/destroy/retry behavior, EIP retention, Host
  reuse, EC2 termination rules, or preview-before-confirm behavior.
- Keep local PEM paths, SSH, VNC, and transfer execution on the member's
  computer.
- Publishing Homebrew/APT and deploying local/staging2 builds happen only after
  this implementation plan passes and the user separately approves release.

## File Map

**Create**

- `web/assets/connectmac-workbench.js`: pure effective-state and action-model
  functions exposed as `window.ConnectMacWorkbench`.
- `web/assets/connectmac-workbench.css`: workbench, home-card, task panel,
  technical details, disabled-reason, and responsive styles.
- `scripts/check-web-workbench.mjs`: dependency-free state/action unit tests.
- `internal/connectmac/app_web_workbench_test.go`: Go entry that runs the
  JavaScript model test and checks required web structure.

**Modify**

- `web/index.html`: link the new assets; update home, operation, Profile, and
  member markup; add renderers, polling, dialog, focus, and error behavior.
- `scripts/check-web-js.mjs`: syntax-check the new external JavaScript asset as
  well as inline scripts.
- `internal/connectmac/app_web_smoke_test.go`: add end-to-end mocked state and
  lifecycle assertions where backend presentation data changes.
- `internal/connectmac/app_web_auto_release_test.go`: preserve and extend
  releasing/ready action regressions.
- `internal/connectmac/app_web_local_agent_https_test.go`: verify local actions
  and Agent-repair markup.
- `internal/connectmac/app_web_operation_header_test.go`: replace narrow header
  assertions with workbench header and navigation assertions.
- `internal/connectmac/web_script_syntax_test.go`: continue to make JS syntax a
  required part of `go test`.

## Task 1: Add the Pure State and Action Model

**Files:**
- Create: `web/assets/connectmac-workbench.js`
- Create: `scripts/check-web-workbench.mjs`
- Create: `internal/connectmac/app_web_workbench_test.go`
- Modify: `scripts/check-web-js.mjs`

- [ ] **Step 1: Write the failing state-model test script**

Create `scripts/check-web-workbench.mjs` with a browser-like VM context and
table-driven assertions:

```js
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(
  new URL("../web/assets/connectmac-workbench.js", import.meta.url),
  "utf8",
);
const context = { window: {} };
vm.createContext(context);
vm.runInContext(source, context, { filename: "connectmac-workbench.js" });

const { effectiveState, buildActionModel } = context.window.ConnectMacWorkbench;

const cases = [
  [{ status: { decision: "create", ready: false } }, "stopped"],
  [{ status: { decision: "wait-ready", ready: false } }, "creating"],
  [{ status: { decision: "ready", ready: true } }, "ready"],
  [{
    status: { decision: "blocked", ready: false },
    jobs: [{ type: "aws-destroy", status: "running" }],
  }, "releasing"],
  [{ status: { decision: "blocked", ready: false } }, "blocked"],
  [{ status: null }, "unknown"],
];

for (const [input, expected] of cases) {
  assert.equal(effectiveState(input), expected);
}

const readyDesktop = buildActionModel({
  effectiveState: "ready",
  mobile: false,
  localAgentOnline: true,
  busy: false,
  canOperate: true,
  canAdmin: false,
});
assert.equal(readyDesktop.primary, "connect");
assert.equal(readyDesktop.actions.connect.enabled, true);
assert.equal(readyDesktop.actions.vnc.enabled, true);
assert.equal(readyDesktop.actions.transfer.enabled, true);
assert.equal(readyDesktop.actions.open.enabled, false);

const readyMobile = buildActionModel({
  effectiveState: "ready",
  mobile: true,
  localAgentOnline: false,
  busy: false,
  canOperate: true,
  canAdmin: false,
});
assert.equal(readyMobile.primary, "refresh");
assert.equal(readyMobile.actions.connect.visible, false);
assert.equal(readyMobile.actions.vnc.visible, false);
assert.equal(readyMobile.actions.transfer.visible, false);

const releasing = buildActionModel({
  effectiveState: "releasing",
  mobile: false,
  localAgentOnline: true,
  busy: false,
  canOperate: true,
  canAdmin: true,
});
for (const name of ["open", "release", "connect", "vnc", "transfer", "cleanup"]) {
  assert.equal(releasing.actions[name].enabled, false, name);
}

console.log("ConnectMac workbench state model OK");
```

- [ ] **Step 2: Add a Go test entry and verify the test fails**

Create `internal/connectmac/app_web_workbench_test.go`:

```go
package connectmac

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWebWorkbenchStateModel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to validate the web workbench state model")
	}
	script := filepath.Join("..", "..", "scripts", "check-web-workbench.mjs")
	output, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("web workbench state model failed: %v\n%s", err, output)
	}
}
```

Run:

```bash
go test ./internal/connectmac -run TestWebWorkbenchStateModel -count=1
```

Expected: FAIL because `web/assets/connectmac-workbench.js` does not exist.

- [ ] **Step 3: Implement the minimal pure workbench model**

Create `web/assets/connectmac-workbench.js`:

```js
(function attachConnectMacWorkbench(root) {
  "use strict";

  const activeJobStatuses = new Set(["starting", "running", "deferred"]);
  const activeLifecycleStates = new Set(["pending", "waiting"]);

  function activeJob(jobs, type) {
    return (jobs || []).find((job) =>
      job && job.type === type &&
      (activeJobStatuses.has(job.status) ||
        activeLifecycleStates.has(job.lifecycle_state)));
  }

  function effectiveState({ status, reminder, jobs = [] }) {
    if (activeJob(jobs, "aws-destroy") ||
        ["running", "retrying", "notifying"].includes(reminder?.auto_release_state)) {
      return "releasing";
    }
    if (activeJob(jobs, "aws-open")) return "creating";
    if (!status) return "unknown";
    if (status.error) return "unknown";
    if (status.ready) return "ready";
    if (status.decision === "create") return "stopped";
    if (status.decision === "wait-ready" ||
        status.decision === "launch-on-host") return "creating";
    if (status.decision === "blocked" || status.decision === "error") {
      return "blocked";
    }
    return "unknown";
  }

  function action(visible, enabled, reason = "") {
    return { visible, enabled, reason };
  }

  function buildActionModel({
    effectiveState: current,
    mobile,
    localAgentOnline,
    busy,
    canOperate,
    canAdmin,
  }) {
    const locked = busy || current === "creating" || current === "releasing";
    const ready = current === "ready";
    const stopped = current === "stopped";
    const localReason = localAgentOnline ? "" : "本机代理未连接";
    const stateReason = ready ? "" : "Mac 尚未就绪";
    const actions = {
      refresh: action(true, !busy),
      open: action(true, canOperate && stopped && !locked,
        stopped ? "" : "当前状态不能打开"),
      release: action(true, canOperate && ready && !locked,
        ready ? "" : "当前状态不能释放"),
      connect: action(!mobile, canOperate && ready && localAgentOnline && !locked,
        stateReason || localReason),
      vnc: action(!mobile, canOperate && ready && localAgentOnline && !locked,
        stateReason || localReason),
      transfer: action(!mobile, canOperate && ready && localAgentOnline && !locked,
        stateReason || localReason),
      extend: action(true, canOperate && ready && !locked,
        ready ? "" : "Mac 尚未就绪"),
      cleanup: action(canAdmin, canAdmin && stopped && !locked,
        stopped ? "" : "仅已停止的 Mac 可清理记录"),
      events: action(true, true),
      details: action(true, true),
    };
    const primary = current === "ready" ? (mobile ? "refresh" : "connect") :
      current === "stopped" ? "open" :
      current === "creating" || current === "releasing" ? "details" :
      "refresh";
    return { state: current, primary, actions };
  }

  root.ConnectMacWorkbench = {
    effectiveState,
    buildActionModel,
  };
})(window);
```

- [ ] **Step 4: Extend syntax validation to external ConnectMac assets**

Add this block to `scripts/check-web-js.mjs` after the inline-script loop:

```js
for (const asset of ["../web/assets/connectmac-workbench.js"]) {
  const assetURL = new URL(asset, import.meta.url);
  const source = fs.readFileSync(assetURL, "utf8");
  try {
    new vm.Script(source, { filename: assetURL.pathname });
  } catch (error) {
    console.error(`web JavaScript syntax error in ${asset}: ${error.message}`);
    process.exitCode = 1;
  }
}
```

- [ ] **Step 5: Run the focused tests**

Run:

```bash
node scripts/check-web-workbench.mjs
node scripts/check-web-js.mjs
go test ./internal/connectmac -run 'TestWebWorkbenchStateModel|TestWebInlineScriptsParse' -count=1
```

Expected: all commands PASS and print both syntax and state-model success.

- [ ] **Step 6: Commit the state model**

```bash
git add web/assets/connectmac-workbench.js scripts/check-web-workbench.mjs \
  scripts/check-web-js.mjs internal/connectmac/app_web_workbench_test.go
git commit -m "feat: add web workbench state model"
```

## Task 2: Build the Workbench Structure and Styles

**Files:**
- Create: `web/assets/connectmac-workbench.css`
- Modify: `web/index.html:8-12`
- Modify: `web/index.html:806-892`
- Modify: `internal/connectmac/app_web_workbench_test.go`
- Modify: `internal/connectmac/app_web_operation_header_test.go`

- [ ] **Step 1: Write failing structural assertions**

Add to `internal/connectmac/app_web_workbench_test.go`:

```go
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
			t.Fatalf("workbench markup missing %q", want)
		}
	}
}
```

Update imports to include `os` and `strings`.

Run:

```bash
go test ./internal/connectmac -run TestWebWorkbenchStructure -count=1
```

Expected: FAIL because the workbench elements and assets are absent.

- [ ] **Step 2: Link the new assets**

Add the stylesheet after Bootstrap and the script before the existing inline
application script:

```html
<link rel="stylesheet" href="vendor/bootstrap/bootstrap.min.css">
<link rel="stylesheet" href="assets/connectmac-workbench.css">
```

At the end of the body, preserve this exact dependency order:

```html
<script src="vendor/bootstrap/bootstrap.bundle.min.js"></script>
<script src="assets/connectmac-workbench.js"></script>
<script>
```

- [ ] **Step 3: Replace the operation-page body with the approved hierarchy**

Keep the existing Profile identity elements and button IDs, but group them:

```html
<section id="operationsView" class="view hidden card shadow-sm">
  <div class="workbench-head card-header">
    <h2 class="selected-title">
      <span id="selectedProfileName" class="selected-profile-name">未选择</span>
      <span id="selectedAppleEmail" class="selected-apple-email">-</span>
    </h2>
    <span id="workbenchStateBadge" class="badge loading">状态未知</span>
  </div>

  <div class="workbench-grid">
    <section class="workbench-primary" aria-labelledby="workbenchRecommendation">
      <span class="eyebrow">当前建议</span>
      <h3 id="workbenchRecommendation">请先选择 Profile</h3>
      <p id="workbenchRecommendationDetail" class="muted"></p>
      <div id="workbenchActions" class="workbench-actions"
           aria-describedby="workbenchActionReason">
        <button id="statusBtn">刷新状态</button>
        <button id="openMacBtn" class="primary">打开</button>
        <button id="releaseMacBtn" class="danger">释放</button>
        <button id="extendReminderBtn">延长提醒</button>
        <button id="cleanupRecordsBtn" class="admin-only">清理记录</button>
        <button id="terminalBtn" class="local-action mobile-local-hidden">连接</button>
        <button id="vncBtn" class="local-action mobile-local-hidden">VNC</button>
        <button id="syncBtn" class="local-action mobile-local-hidden">传输</button>
      </div>
      <p id="workbenchActionReason" class="action-reason" aria-live="polite"></p>
    </section>

    <aside id="workbenchTaskPanel" class="workbench-task-panel">
      <span class="eyebrow">任务与提醒</span>
      <div id="workbenchEmptyTask">暂无进行中的任务</div>
      <div id="workbenchActiveTask" class="hidden"></div>
      <div class="auto-release-strip">
        <div class="auto-release-copy">
          <span>自动释放</span>
          <strong id="autoReleaseSummary">-</strong>
          <small id="autoReleaseTime" class="hidden"></small>
          <small id="autoReleaseError" class="error hidden"></small>
        </div>
        <button id="autoReleaseToggleBtn" aria-pressed="false">开启自动释放</button>
      </div>
      <button id="eventsBtn">查看最近事件</button>
    </aside>
  </div>

  <details id="technicalDetails" class="technical-details">
    <summary>技术详情</summary>
    <div class="details">
      <div class="metric"><span>Decision</span><strong id="decisionValue">-</strong></div>
      <div class="metric"><span>Ready</span><strong id="readyValue">-</strong></div>
      <div class="metric"><span>Public IP</span><strong id="ipValue">-</strong></div>
      <div class="metric"><span>Next</span><strong id="nextValue">-</strong></div>
      <div class="metric"><span>负责人</span><strong id="ownerValue">-</strong></div>
      <div class="metric"><span>Host 创建</span><strong id="hostCreatedValue">-</strong></div>
      <div class="metric"><span>提醒时间</span><strong id="reminderDueValue">-</strong></div>
      <div class="metric"><span>提醒状态</span><strong id="reminderStatusValue">-</strong></div>
      <div class="metric admin-only">
        <span>负责人</span>
        <select id="assignMemberSelect" class="inline-select"></select>
      </div>
    </div>
    <div id="technicalOutput" class="output hidden">
      <div id="statusLine" class="status-line"></div>
      <pre id="output"></pre>
    </div>
  </details>
</section>
```

Move the existing auto-release controls into `workbenchTaskPanel`. Move the
global jobs table out of the workbench; it remains reachable through task/event
details instead of occupying the default operation page.

Wire `vncBtn` to the existing `startTunnel(state.selected)` function. This moves
the current home-row VNC behavior into the workbench without changing local
Agent request semantics.

- [ ] **Step 4: Add focused workbench CSS**

Create `web/assets/connectmac-workbench.css`:

```css
.workbench-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  padding: 16px 18px;
}

.workbench-grid {
  display: grid;
  grid-template-columns: minmax(0, 2fr) minmax(280px, 1fr);
  gap: 16px;
  padding: 18px;
}

.workbench-primary,
.workbench-task-panel {
  min-width: 0;
  border: 1px solid var(--line);
  border-radius: 10px;
  padding: 18px;
  background: var(--panel);
}

.workbench-actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 18px;
}

.workbench-actions button {
  min-width: 92px;
}

.action-reason {
  min-height: 20px;
  margin: 10px 0 0;
  color: var(--muted);
  font-size: 13px;
}

.technical-details {
  margin: 0 18px 18px;
  border-top: 1px solid var(--line);
  padding-top: 14px;
}

.technical-details summary {
  cursor: pointer;
  font-weight: 700;
}

@media (max-width: 820px) {
  .workbench-grid { grid-template-columns: 1fr; padding: 12px; }
  .workbench-head { align-items: center; }
}
```

- [ ] **Step 5: Update the old header regression test**

Keep assertions for the vertically stacked identity and removed home button.
Add assertions for `workbench-head` and `workbenchStateBadge`; remove assertions
that require old toolbar placement.

- [ ] **Step 6: Run structural and syntax tests**

Run:

```bash
node scripts/check-web-js.mjs
go test ./internal/connectmac -run \
  'TestWebWorkbenchStructure|TestWebOperationHeaderUsesVerticalProfileIdentityWithoutHomeButton|TestWebInlineScriptsParse' \
  -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the workbench shell**

```bash
git add web/index.html web/assets/connectmac-workbench.css \
  internal/connectmac/app_web_workbench_test.go \
  internal/connectmac/app_web_operation_header_test.go
git commit -m "feat: add state-driven workbench layout"
```

## Task 3: Render Human State, Recommendations, and Actions

**Files:**
- Modify: `web/assets/connectmac-workbench.js`
- Modify: `scripts/check-web-workbench.mjs`
- Modify: `web/index.html:1841-2003`
- Modify: `internal/connectmac/app_web_auto_release_test.go`

- [ ] **Step 1: Add failing recommendation and reason tests**

Extend `scripts/check-web-workbench.mjs`:

```js
const copyCases = [
  ["stopped", "这台 Mac 尚未运行"],
  ["creating", "正在打开这台 Mac"],
  ["ready", "这台 Mac 已可使用"],
  ["releasing", "正在释放这台 Mac"],
  ["blocked", "当前流程已停止"],
  ["unknown", "暂时无法确认 Mac 状态"],
];
for (const [state, heading] of copyCases) {
  assert.equal(context.window.ConnectMacWorkbench.stateCopy(state).heading, heading);
}

const agentOffline = buildActionModel({
  effectiveState: "ready",
  mobile: false,
  localAgentOnline: false,
  busy: false,
  canOperate: true,
  canAdmin: false,
});
assert.equal(agentOffline.actions.connect.reason, "本机代理未连接");
```

Run:

```bash
node scripts/check-web-workbench.mjs
```

Expected: FAIL because `stateCopy` is undefined.

- [ ] **Step 2: Add human-readable state copy**

Add to `web/assets/connectmac-workbench.js`:

```js
const stateCopyMap = {
  stopped: {
    badge: "已停止",
    heading: "这台 Mac 尚未运行",
    detail: "打开前会先展示 AWS 预览并要求确认。",
  },
  creating: {
    badge: "正在打开",
    heading: "正在打开这台 Mac",
    detail: "系统正在创建资源并等待 AWS 状态检查。",
  },
  ready: {
    badge: "已就绪",
    heading: "这台 Mac 已可使用",
    detail: "可以连接终端、打开 VNC 或传输文件。",
  },
  releasing: {
    badge: "正在释放",
    heading: "正在释放这台 Mac",
    detail: "系统正在终止实例并等待 Dedicated Host 可释放。",
  },
  blocked: {
    badge: "受阻",
    heading: "当前流程已停止",
    detail: "请查看阻塞原因；系统不会自动创建其他 Host 或终止 EC2。",
  },
  unknown: {
    badge: "状态未知",
    heading: "暂时无法确认 Mac 状态",
    detail: "请先刷新状态或诊断配置。",
  },
};

function stateCopy(state) {
  return stateCopyMap[state] || stateCopyMap.unknown;
}
```

Export `stateCopy`.

- [ ] **Step 3: Make `renderSelected()` consume one view model**

At the start of `renderSelected()` compute:

```js
const status = p ? state.statuses[p.name] : null;
const reminder = p ? state.reminders[p.name] : null;
const jobs = p ? state.jobs.filter((job) => job.profile === p.name) : [];
const effective = window.ConnectMacWorkbench.effectiveState({
  status,
  reminder,
  jobs,
});
const model = window.ConnectMacWorkbench.buildActionModel({
  effectiveState: effective,
  mobile: window.matchMedia("(max-width: 640px)").matches,
  localAgentOnline: state.localAgent.online,
  busy: state.busy,
  canOperate: ["operator", "admin"].includes(state.auth?.member?.role),
  canAdmin: isAdmin(),
});
const copy = window.ConnectMacWorkbench.stateCopy(effective);
```

Then render the badge, recommendation, buttons, and visible disabled reason
from `model`. Apply `hidden` from `action.visible`; apply `disabled` from
`action.enabled`; do not recalculate those rules in button-specific branches.

- [ ] **Step 4: Keep diagnostics but hide empty output**

Replace `setOutput()` with:

```js
function setOutput(text) {
  const value = text || "";
  $("output").textContent = value;
  $("technicalOutput").classList.toggle("hidden", !value && !$("statusLine").textContent);
}
```

`loadStatus()` and error flows open `technicalDetails` only when the user
explicitly asks for status or an error needs technical context.

- [ ] **Step 5: Update releasing and ready regression assertions**

In `internal/connectmac/app_web_auto_release_test.go`, assert the page calls
`ConnectMacWorkbench.effectiveState` and no longer independently labels active
destroy jobs as `creating`. Preserve existing backend auto-release tests.

- [ ] **Step 6: Run focused tests**

Run:

```bash
node scripts/check-web-workbench.mjs
node scripts/check-web-js.mjs
go test ./internal/connectmac -run \
  'TestWebWorkbench|TestWebAutoRelease|TestWebInlineScriptsParse' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the renderer**

```bash
git add web/assets/connectmac-workbench.js scripts/check-web-workbench.mjs \
  web/index.html internal/connectmac/app_web_auto_release_test.go
git commit -m "feat: drive web actions from Mac state"
```

## Task 4: Simplify the Home Page and Add Stable Partial Refresh

**Files:**
- Modify: `web/index.html:772-804`
- Modify: `web/index.html:1520-1706`
- Modify: `web/assets/connectmac-workbench.css`
- Modify: `internal/connectmac/app_web_workbench_test.go`

- [ ] **Step 1: Write failing home-page contract assertions**

Add:

```go
func TestWebHomeUsesWorkbenchEntryAndRefreshTimestamp(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for _, want := range []string{
		`id="profileLastUpdated"`,
		`data-workbench=`,
		`进入工作台`,
		`scheduleProfileRefresh`,
		`document.visibilityState`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("home page contract missing %q", want)
		}
	}
	if strings.Contains(html, `title="选择当前 Profile"`) {
		t.Fatal("home page still uses generic Profile selection wording")
	}
}
```

Run:

```bash
go test ./internal/connectmac -run TestWebHomeUsesWorkbenchEntryAndRefreshTimestamp -count=1
```

Expected: FAIL.

- [ ] **Step 2: Add the last-updated display**

Place this after the Profile table/card container:

```html
<div id="profileLastUpdated" class="profile-last-updated" aria-live="polite">
  尚未更新
</div>
```

- [ ] **Step 3: Replace row action wording and duplicate mobile content**

In `renderProfiles()`, render one Apple email, a human state badge, and:

```html
<button data-workbench="PROFILE_NAME">
  <span class="action-icon" aria-hidden="true">→</span>进入工作台
</button>
```

Attach the same workbench navigation to row/card activation without placing
nested click handlers on local-action buttons. Stop rendering direct
connection, VNC, and transfer actions on the home page; those belong to the
workbench.

- [ ] **Step 4: Add a visibility-aware 10-second refresh**

Add:

```js
let profileRefreshTimer = null;

function scheduleProfileRefresh() {
  if (profileRefreshTimer || !state.auth?.authenticated) return;
  profileRefreshTimer = window.setTimeout(async () => {
    profileRefreshTimer = null;
    if (document.visibilityState === "visible") {
      try {
        await refreshVisibleStatuses();
        $("profileLastUpdated").textContent =
          "最后更新：" + new Intl.DateTimeFormat("zh-CN", {
            hour: "2-digit",
            minute: "2-digit",
            second: "2-digit",
          }).format(new Date());
      } catch (error) {
        $("profileLastUpdated").textContent = "状态更新失败，正在重试";
      }
    }
    scheduleProfileRefresh();
  }, 10000);
}
```

Call it after authenticated app data loads. Clear the timer on logout.

- [ ] **Step 5: Add compact desktop/mobile styles**

Add styles for `.profile-last-updated` and make the existing small-screen table
cards show each field once. At `max-width: 640px`, hide desktop-local action
containers rather than reserving empty space.

- [ ] **Step 6: Run focused tests**

Run:

```bash
node scripts/check-web-js.mjs
go test ./internal/connectmac -run \
  'TestWebHomeUsesWorkbenchEntryAndRefreshTimestamp|TestWebInlineScriptsParse' \
  -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the home page**

```bash
git add web/index.html web/assets/connectmac-workbench.css \
  internal/connectmac/app_web_workbench_test.go
git commit -m "feat: simplify profile home experience"
```

## Task 5: Persist Task Feedback and Correlated Errors

**Files:**
- Modify: `web/index.html:1086-1150`
- Modify: `web/index.html:2004-2113`
- Modify: `web/index.html:4179-4325`
- Modify: `web/assets/connectmac-workbench.js`
- Modify: `scripts/check-web-workbench.mjs`
- Modify: `internal/connectmac/app_web_smoke_test.go`

- [ ] **Step 1: Add failing active-task model tests**

Add to `scripts/check-web-workbench.mjs`:

```js
const task = context.window.ConnectMacWorkbench.activeLifecycleTask([
  {
    id: "job-1",
    type: "aws-destroy",
    profile: "iossupport-usw2",
    status: "success",
    lifecycle_state: "waiting",
    started_at: "2026-07-31T08:00:00Z",
    request_id: "req-1",
  },
], "iossupport-usw2");
assert.equal(task.id, "job-1");
assert.equal(task.label, "等待 Dedicated Host 可释放");
assert.equal(task.terminal, false);
```

Run:

```bash
node scripts/check-web-workbench.mjs
```

Expected: FAIL because `activeLifecycleTask` is undefined.

- [ ] **Step 2: Add the pure task selector**

Add:

```js
function activeLifecycleTask(jobs, profile) {
  const job = (jobs || []).find((candidate) =>
    candidate?.profile === profile &&
    (activeJobStatuses.has(candidate.status) ||
      activeLifecycleStates.has(candidate.lifecycle_state)));
  if (!job) return null;
  const destroy = job.type === "aws-destroy";
  const label = job.lifecycle_state === "waiting"
    ? (destroy ? "等待 Dedicated Host 可释放" : "等待 Mac 状态检查")
    : (destroy ? "正在释放 AWS 资源" : "正在打开 Mac");
  return {
    ...job,
    label,
    terminal: false,
  };
}
```

Export it.

- [ ] **Step 3: Capture `X-Request-ID` in `api()`**

Update `api()`:

```js
const requestID = res.headers.get("X-Request-ID") || "";
const body = await res.json();
if (body && typeof body === "object" && !body.request_id) {
  body.request_id = requestID;
}
if (!body.ok) {
  if (res.status === 401) {
    state.auth = { authenticated: false, setup_required: false };
    showAuth();
  }
  const err = new Error((body.error || "") + (body.output ? "\n" + body.output : ""));
  err.status = res.status;
  err.requestID = requestID;
  err.errorCode = body.error_code || "";
  throw err;
}
return body;
```

Keep the existing 401 sign-out behavior.

- [ ] **Step 4: Render the active task from `/api/jobs`**

In `renderSelected()`, call `activeLifecycleTask(state.jobs, p.name)`. Render
job ID, request ID, actor, elapsed time, lifecycle step, and retry text into
`workbenchActiveTask`. Show `workbenchEmptyTask` only when no active task
exists.

Do not estimate a percent for AWS lifecycle tasks. Use an indeterminate progress
bar and explicit step text because AWS Host release duration is not linear.

- [ ] **Step 5: Make task submission wording accurate**

After confirmed `runAWS()` success:

```js
setStatus("任务已提交，页面会自动更新进度");
closeAWSConfirm({ restoreFocus: false });
await loadJobs({ refreshReminders: true });
renderSelected();
```

Never show `打开成功` or `释放完成` at submission. Those labels come only from
the finalized status/job combination already reconciled by the backend.

- [ ] **Step 6: Add a reusable correlated error renderer**

Add:

```js
function showOperationError(summary, error) {
  $("statusLine").textContent = summary;
  const requestID = error?.requestID ? `\nRequest ID: ${error.requestID}` : "";
  const errorCode = error?.errorCode ? `\nError code: ${error.errorCode}` : "";
  setOutput(`${error?.message || error}${errorCode}${requestID}`);
  $("technicalDetails").open = true;
}
```

Use it for AWS preview/confirm, status, member/Profile saves, reminder actions,
and local-intent failures. Do not treat `AbortError` or `context canceled` as an
AWS failure; leave the current data visible and retry on the next poll.

- [ ] **Step 7: Extend the mocked lifecycle smoke test**

In `internal/connectmac/app_web_smoke_test.go`, retain the existing assertion
that `aws.open.ready` is recorded only after lifecycle finalization. Add an
equivalent mocked destroy case asserting `aws.destroy.stopped` and the job's
`LifecycleState == JobLifecycleFinalized`.

- [ ] **Step 8: Run task and lifecycle tests**

Run:

```bash
node scripts/check-web-workbench.mjs
go test ./internal/connectmac -run \
  'TestWebWorkbenchStateModel|TestWebObservabilitySmoke|TestWebAWSLifecycle' \
  -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit persistent feedback**

```bash
git add web/index.html web/assets/connectmac-workbench.js \
  scripts/check-web-workbench.mjs internal/connectmac/app_web_smoke_test.go
git commit -m "feat: persist web lifecycle feedback"
```

## Task 6: Harden Preview/Confirm Dialogs and Focus Behavior

**Files:**
- Modify: `web/index.html:520-670`
- Modify: `web/index.html:2040-2166`
- Modify: `internal/connectmac/app_web_workbench_test.go`
- Modify: `internal/connectmac/app_web_profile_owner_ui_test.go`

- [ ] **Step 1: Write failing dialog-contract assertions**

Assert the AWS confirmation dialog contains:

```go
for _, want := range []string{
	`id="awsConfirmProfile"`,
	`id="awsConfirmApple"`,
	`id="awsConfirmOwner"`,
	`id="awsConfirmResourceSummary"`,
	`id="awsConfirmEIPNotice"`,
	`data-dialog-initial-focus`,
	`function openDialog(`,
	`function closeDialog(`,
} {
	if !strings.Contains(html, want) {
		t.Fatalf("AWS confirmation contract missing %q", want)
	}
}
```

Run:

```bash
go test ./internal/connectmac -run TestWebWorkbenchDialog -count=1
```

Expected: FAIL.

- [ ] **Step 2: Add structured preview fields**

Keep the sanitized raw preview in expandable technical content, but add visible
fields for Profile, Apple email, selected owner, Instance, Host, and EIP.
Release dialogs include:

```html
<dl class="confirm-summary">
  <div><dt>Profile</dt><dd id="awsConfirmProfile">-</dd></div>
  <div><dt>Apple 账号</dt><dd id="awsConfirmApple">-</dd></div>
  <div><dt>负责人</dt><dd id="awsConfirmOwner">-</dd></div>
</dl>
<div id="awsConfirmResourceSummary" class="confirm-resource-summary"></div>
<div id="awsConfirmEIPNotice" class="alert alert-info hidden">
  弹性 IP 只会解除关联，不会释放。
</div>
<button id="runAWSConfirmBtn" data-dialog-initial-focus disabled>确认</button>
```

- [ ] **Step 3: Add reusable dialog open/close helpers**

Add:

```js
let dialogTrigger = null;

function openDialog(layer, initialFocus) {
  dialogTrigger = document.activeElement;
  layer.classList.remove("hidden");
  window.requestAnimationFrame(() => initialFocus?.focus());
}

function closeDialog(layer, { restoreFocus = true } = {}) {
  layer.classList.add("hidden");
  if (restoreFocus && dialogTrigger instanceof HTMLElement) {
    dialogTrigger.focus();
  }
  dialogTrigger = null;
}
```

Add a `keydown` handler per open layer that traps `Tab` within focusable
controls and closes non-destructive dialogs on `Escape`:

```js
function trapDialogFocus(event, layer) {
  if (event.key === "Escape") {
    closeDialog(layer);
    return;
  }
  if (event.key !== "Tab") return;
  const focusable = [...layer.querySelectorAll(
    'button:not([disabled]), input:not([disabled]), select:not([disabled]), ' +
    'textarea:not([disabled]), summary, [tabindex]:not([tabindex="-1"])',
  )].filter((element) => !element.classList.contains("hidden"));
  if (!focusable.length) return;
  const first = focusable[0];
  const last = focusable[focusable.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

document.querySelectorAll(".picker-layer").forEach((layer) => {
  layer.addEventListener("keydown", (event) => trapDialogFocus(event, layer));
});
```

- [ ] **Step 4: Enforce preview and owner requirements in the dialog**

Disable `runAWSConfirmBtn` until:

- Preview succeeded.
- The selected Profile still matches `state.pendingAWS.profile`.
- Admin open has a responsible member.
- The current effective state still allows the requested action.

For non-admin open, display the signed-in member as owner. Do not infer an owner
from unrelated conversation or old page state.

- [ ] **Step 5: Close successful dialogs and focus invalid fields**

Apply `closeDialog()` to member add/edit, Profile add/edit, Profile assignment,
member password, own password, API token deletion, settings, reminder, and AWS
confirm flows. On validation error, focus the first invalid field and preserve
all other values.

- [ ] **Step 6: Run dialog and owner tests**

Run:

```bash
node scripts/check-web-js.mjs
go test ./internal/connectmac -run \
  'TestWebWorkbenchDialog|TestWebProfileOwner|TestWebInlineScriptsParse' \
  -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit dialog behavior**

```bash
git add web/index.html internal/connectmac/app_web_workbench_test.go \
  internal/connectmac/app_web_profile_owner_ui_test.go
git commit -m "feat: clarify web operation confirmations"
```

## Task 7: Make Local Agent and Mobile Capabilities Explicit

**Files:**
- Modify: `web/index.html:1140-1253`
- Modify: `web/index.html:2320-2410`
- Modify: `web/index.html:3495-3585`
- Modify: `web/assets/connectmac-workbench.css`
- Modify: `internal/connectmac/app_web_local_agent_https_test.go`

- [ ] **Step 1: Write failing local-capability assertions**

Add assertions for:

```go
for _, want := range []string{
	`id="localAgentRepairBtn"`,
	`本机代理未连接`,
	`window.matchMedia("(max-width: 640px)")`,
	`mobile-local-hidden`,
} {
	if !strings.Contains(html, want) {
		t.Fatalf("local capability UI missing %q", want)
	}
}
```

Run:

```bash
go test ./internal/connectmac -run TestWebLocalAgent -count=1
```

Expected: FAIL on the new repair and mobile behavior.

- [ ] **Step 2: Make the Agent status actionable**

Change the header status to a button or adjacent repair button:

```html
<button id="localAgentRepairBtn" class="local-agent-repair hidden">
  修复本机代理
</button>
```

When offline, show:

```text
本机代理未连接。请运行 cm local-agent install && cm local-agent start
```

The repair action opens a small dialog with copyable commands and a `重新检测`
button. It does not attempt privileged installation from the browser.

- [ ] **Step 3: Keep local actions independent but state-driven**

Continue to use `recordLocalIntent()` and `localAgentAPI()`. The workbench model
decides whether connection/VNC/transfer are visible and enabled; the handler
still validates readiness and Agent state before executing.

- [ ] **Step 4: Hide local operations on mobile**

Apply `mobile-local-hidden` to connection, VNC, transfer, terminal controls, and
desktop-only local status detail. Add:

```css
@media (max-width: 640px) {
  .mobile-local-hidden { display: none !important; }
  .workbench-primary { padding-bottom: 84px; }
  .workbench-mobile-actions {
    position: sticky;
    bottom: 0;
    z-index: 10;
    background: var(--panel);
    border-top: 1px solid var(--line);
    padding: 10px 12px;
  }
}
```

- [ ] **Step 5: Preserve VNC and terminal recovery behavior**

Verify the local Agent still replaces stale VNC tunnel state and that terminal
`exit`/disconnect returns to the workbench. Do not reintroduce the rule that VNC
requires a prior terminal click.

- [ ] **Step 6: Run local-Agent and terminal tests**

Run:

```bash
go test ./internal/connectmac -run \
  'TestWebLocalAgent|TestLocalAgent|TestWebTerminal|TestWebInlineScriptsParse' \
  -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit device capability behavior**

```bash
git add web/index.html web/assets/connectmac-workbench.css \
  internal/connectmac/app_web_local_agent_https_test.go
git commit -m "feat: expose local agent capability state"
```

## Task 8: Finish Profiles, Members, Loading, and Empty States

**Files:**
- Modify: `web/index.html:930-1042`
- Modify: `web/index.html:1370-1840`
- Modify: `web/index.html:3599-4178`
- Modify: `web/assets/connectmac-workbench.css`
- Modify: `internal/connectmac/app_web_workbench_test.go`
- Modify: `internal/connectmac/app_web_smoke_test.go`

- [ ] **Step 1: Write failing responsibility and loading assertions**

Assert:

```go
for _, want := range []string{
	`Profiles 通过弹框表单新增或编辑`,
	`id="memberEmptyState"`,
	`id="managedProfileEmptyState"`,
	`校验题加载失败，点击换一题重试`,
	`const componentLoadTimeoutMS = 8000`,
} {
	if !strings.Contains(html, want) {
		t.Fatalf("management/loading contract missing %q", want)
	}
}
```

Also assert user management does not contain a `Profiles` management section.

- [ ] **Step 2: Add a timeout-aware API option**

Extend `api()` without applying an eight-second timeout to AWS mutations:

```js
const componentLoadTimeoutMS = 8000;

async function api(path, options = {}) {
  const { timeoutMs = 0, ...fetchOptions } = options;
  const controller = timeoutMs > 0 ? new AbortController() : null;
  const timer = controller
    ? window.setTimeout(() => controller.abort(), timeoutMs)
    : null;
  try {
    const res = await fetch(apiURL(path), {
      headers: { "Content-Type": "application/json" },
      credentials: "same-origin",
      ...fetchOptions,
      ...(controller ? { signal: controller.signal } : {}),
    });
    const requestID = res.headers.get("X-Request-ID") || "";
    const body = await res.json();
    if (body && typeof body === "object" && !body.request_id) {
      body.request_id = requestID;
    }
    if (!body.ok) {
      if (res.status === 401) {
        state.auth = { authenticated: false, setup_required: false };
        showAuth();
      }
      const error = new Error(
        (body.error || "") + (body.output ? "\n" + body.output : ""),
      );
      error.status = res.status;
      error.requestID = requestID;
      error.errorCode = String(body.error_code || body.code || "");
      throw error;
    }
    return body;
  } finally {
    if (timer) window.clearTimeout(timer);
  }
}
```

Use `timeoutMs: componentLoadTimeoutMS` for challenge, members, managed
Profiles, settings, and initial Profile list reads. Do not use it for open,
destroy, status, transfer, terminal, or lifecycle polling requests.

- [ ] **Step 3: Make challenge failure recoverable**

On challenge timeout or failure:

```js
state.challenge = null;
$("challengeQuestion").textContent = "校验题加载失败，点击换一题重试";
$("authStatus").textContent =
  "无法加载校验题" + (error.requestID ? ` · Request ID: ${error.requestID}` : "");
$("refreshChallengeBtn").disabled = false;
```

Keep the username and password fields intact. The login button remains disabled
until a challenge token exists.

- [ ] **Step 4: Add explicit empty states**

Render:

- Home:

  ```html
  <div id="profileEmptyState" class="empty-state hidden">
    当前账号没有可用 Profile
  </div>
  ```

- Profiles:

  ```html
  <div id="managedProfileEmptyState" class="empty-state hidden">
    <strong>尚未添加 Profile</strong>
    <button id="emptyAddProfileBtn" class="primary">添加 Profile</button>
  </div>
  ```

- User management:

  ```html
  <div id="memberEmptyState" class="empty-state hidden">
    <strong>尚未添加成员</strong>
    <button id="emptyAddMemberBtn" class="primary">添加成员</button>
  </div>
  ```

- Event and task panels use the exact text `暂无事件` and `暂无后台任务`.

Each admin empty state includes its existing add action. Member empty states do
not expose admin actions.

- [ ] **Step 5: Keep page responsibilities separate**

Confirm:

- Home has status and workbench entry only.
- Profiles has configuration cards and add/edit/status/delete controls.
- User management has members and Profile assignment dialog only.
- `currentUserBtn` opens `userView` for the member's own password and token.
- `accountSettingsBtn` opens `userView` with the admin-only system settings and
  administrator-email panels visible.

Do not duplicate the Profile catalog below the member table.

Move the existing `Settings` and `Account` blocks out of
`userManagementView` and into the end of `userView`:

```html
<section id="systemSettingsPanel" class="admin-only account-section">
  <div class="section-head">
    <h2>系统设置</h2>
    <button id="saveSettingsBtn">保存设置</button>
  </div>
  <div class="settings-form">
    <select id="settingsDefaultOwner"></select>
    <select id="settingsStatusFilter"></select>
    <label>
      <input id="settingsBackgroundConfirm" type="checkbox">
      确认操作默认后台执行
    </label>
    <label>
      <input id="settingsShowReleased" type="checkbox">
      显示释放/终止资源
    </label>
  </div>
</section>
<section id="accountSettingsPanel" class="hidden admin-only account-section">
  <div class="section-head">
    <h2>管理员账号</h2>
    <button id="refreshAccountChallengeBtn">换一题</button>
  </div>
  <div class="account-form">
    <input id="accountEmail" placeholder="新的管理员邮箱">
    <input id="accountPassword" type="password" placeholder="当前密码">
    <strong id="accountChallengeQuestion">校验题加载中...</strong>
    <input id="accountChallengeAnswer" placeholder="校验答案">
    <button id="saveAccountEmailBtn" class="primary">保存管理员邮箱</button>
  </div>
</section>
```

- [ ] **Step 6: Fix modal save convergence**

For Profile assignment, member edit, member password, own password, and Profile
edit:

1. Await the mutation.
2. Reload only the affected data.
3. Render the affected row/list.
4. Close the dialog.
5. Announce success through an `aria-live` status line.

If any step fails, keep the dialog open and show the correlated error.

- [ ] **Step 7: Run management and smoke tests**

Run:

```bash
node scripts/check-web-js.mjs
go test ./internal/connectmac -run \
  'TestWebWorkbenchManagement|TestWebObservabilitySmoke|TestWebInlineScriptsParse' \
  -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit management and loading UX**

```bash
git add web/index.html web/assets/connectmac-workbench.css \
  internal/connectmac/app_web_workbench_test.go \
  internal/connectmac/app_web_smoke_test.go
git commit -m "feat: improve web management guidance"
```

## Task 9: Accessibility and Responsive Regression Coverage

**Files:**
- Modify: `web/index.html`
- Modify: `web/assets/connectmac-workbench.css`
- Modify: `internal/connectmac/app_web_workbench_test.go`
- Modify: `internal/connectmac/app_web_operation_header_test.go`

- [ ] **Step 1: Add failing accessibility assertions**

Require:

```go
for _, want := range []string{
	`aria-live="polite"`,
	`aria-describedby="workbenchActionReason"`,
	`aria-label="Profile 状态筛选"`,
	`aria-label="搜索 Profile、Apple 邮箱或 Region"`,
	`focus-visible`,
} {
	if !strings.Contains(html, want) {
		t.Fatalf("accessibility contract missing %q", want)
	}
}
```

Run:

```bash
go test ./internal/connectmac -run TestWebWorkbenchAccessibility -count=1
```

Expected: FAIL.

- [ ] **Step 2: Add labels and live regions**

- Label search, filters, owner selection, local paths, and icon buttons.
- Add `aria-live="polite"` to task state, last update, modal status, and local
  Agent state.
- Associate action reasons with disabled action groups.
- Keep visible text in addition to badge color.

- [ ] **Step 3: Add browser-stable focus and sizing**

Add:

```css
button:focus-visible,
input:focus-visible,
select:focus-visible,
summary:focus-visible,
[tabindex]:focus-visible {
  outline: 3px solid rgba(36, 107, 254, .35);
  outline-offset: 2px;
}

.workbench-actions button {
  flex: 0 0 auto;
  min-height: 38px;
  white-space: nowrap;
}

.selected-profile-name,
.selected-apple-email,
.profile-card-value {
  overflow-wrap: anywhere;
}
```

Avoid CSS features unsupported by the minimum Safari version already used by
the project. Use flex/grid fallback-friendly dimensions and no viewport-scaled
font sizes.

- [ ] **Step 4: Add reduced-motion behavior**

```css
@media (prefers-reduced-motion: reduce) {
  *, *::before, *::after {
    scroll-behavior: auto !important;
    transition-duration: .01ms !important;
    animation-duration: .01ms !important;
    animation-iteration-count: 1 !important;
  }
}
```

- [ ] **Step 5: Run source and syntax checks**

Run:

```bash
node scripts/check-web-js.mjs
node scripts/check-web-workbench.mjs
go test ./internal/connectmac -run \
  'TestWebWorkbenchAccessibility|TestWebOperationHeader|TestWebInlineScriptsParse' \
  -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit accessibility and compatibility**

```bash
git add web/index.html web/assets/connectmac-workbench.css \
  internal/connectmac/app_web_workbench_test.go \
  internal/connectmac/app_web_operation_header_test.go
git commit -m "fix: harden web accessibility and browser layout"
```

## Task 10: Full Verification and Visual QA

**Files:**
- Modify only files required by failures found in this task.
- Test: all `internal/connectmac` tests and scripts.

- [ ] **Step 1: Run formatting and JavaScript checks**

Run:

```bash
gofmt -w internal/connectmac/app_web_workbench_test.go \
  internal/connectmac/app_web_operation_header_test.go \
  internal/connectmac/app_web_auto_release_test.go \
  internal/connectmac/app_web_local_agent_https_test.go \
  internal/connectmac/app_web_smoke_test.go
node scripts/check-web-js.mjs
node scripts/check-web-workbench.mjs
```

Expected: both Node checks PASS; `gofmt` produces no subsequent diff after a
second run.

- [ ] **Step 2: Run the complete test suite**

Run:

```bash
go test ./...
```

Expected: PASS with no failing package.

- [ ] **Step 3: Start a local test server**

Run:

```bash
go run ./cmd/cm web --host 127.0.0.1 --port 18766
```

Expected: the server listens on `http://127.0.0.1:18766` without changing the
installed `cm` or the production `18765` local Agent.

- [ ] **Step 4: Verify desktop layouts**

Using browser automation or the connected browsers, capture home, workbench,
Profiles, user management, confirmation dialog, creating, ready, releasing,
blocked, unknown, empty, and error states at:

```text
Chrome: 1280x800, 1440x900, 1920x1080
Safari: 1280x800, 1440x900, 1920x1080
Firefox: 1280x800, 1440x900, 1920x1080
```

For every screenshot verify:

- All permitted action buttons are visible.
- Safari shows the same permitted actions as Chrome.
- No text overlaps or escapes its container.
- The Apple email appears once per mobile card.
- Empty task/output areas do not render dark blank panels.
- Browser console has no JavaScript errors.
- Network panel has no failed CSS, JavaScript, SVG, Bootstrap, or xterm assets.

- [ ] **Step 5: Verify mobile layouts**

Capture authenticated home and workbench at `390x844` in mobile Safari and
Chrome emulation.

Expected:

- Connection, VNC, transfer, and terminal controls are absent.
- Profile, Apple email, Region, state, and management entry fit in the first
  viewport.
- Lifecycle action remains reachable.
- Dialogs fit the viewport and preserve focus/scroll.

- [ ] **Step 6: Verify workflow behavior with mocked or non-mutating data**

Perform:

1. Load and retry the login challenge.
2. Filter Profiles and wait for a partial status refresh.
3. Enter a Profile workbench and use browser back/forward.
4. Preview open and cancel without confirming.
5. Preview release and verify the EIP-retention notice.
6. Reload while a test/mocked lifecycle job is visible.
7. Disconnect the local Agent and verify the repair state.
8. Save Profile assignment and member edits, confirming dialogs close.

Expected: no AWS mutation occurs during preview-only checks.

- [ ] **Step 7: Inspect the final diff**

Run:

```bash
git diff --check
git status --short
git diff --stat
```

Expected:

- No whitespace errors.
- Only planned files are modified.
- `.mcp.json`, `CLAUDE.md`, `web-ui/`, and `.superpowers/` remain untouched and
  untracked.

- [ ] **Step 8: Commit verification fixes**

If visual or full-suite verification required code changes:

```bash
git add web/index.html web/assets/connectmac-workbench.css \
  web/assets/connectmac-workbench.js scripts/check-web-js.mjs \
  scripts/check-web-workbench.mjs \
  internal/connectmac/app_web_workbench_test.go \
  internal/connectmac/app_web_operation_header_test.go \
  internal/connectmac/app_web_auto_release_test.go \
  internal/connectmac/app_web_local_agent_https_test.go \
  internal/connectmac/app_web_smoke_test.go
git commit -m "test: complete state-driven web UX verification"
```

If there were no changes, do not create an empty commit.

## Task 11: Prepare Release Handoff

**Files:**
- Modify: none.

- [ ] **Step 1: Record the verified commit range**

Run:

```bash
git log --oneline 6e749e2..HEAD
```

Expected: one focused commit per completed task, with no unrelated personal
files.

- [ ] **Step 2: Produce the implementation report**

Report:

- State/action model implemented.
- Home/workbench/Profiles/user-management responsibility changes.
- Task persistence and correlated error behavior.
- Desktop/mobile/browser verification results.
- Full test-suite result.
- Any remaining operational risk.

- [ ] **Step 3: Stop before publishing**

Do not push, tag, publish Homebrew/APT, upgrade the local installation, or
deploy staging2 until the user reviews the implementation report and explicitly
requests release.
