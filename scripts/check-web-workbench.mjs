import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const workbenchURL = new URL("../web/assets/connectmac-workbench.js", import.meta.url);
const source = fs.readFileSync(workbenchURL, "utf8");
const workbenchCSSURL = new URL("../web/assets/connectmac-workbench.css", import.meta.url);
const workbenchCSS = fs.readFileSync(workbenchCSSURL, "utf8");
const indexURL = new URL("../web/index.html", import.meta.url);
const html = fs.readFileSync(indexURL, "utf8");
const context = { window: {} };

vm.createContext(context);
new vm.Script(source, { filename: "web/assets/connectmac-workbench.js" }).runInContext(context);

const {
  activeLifecycleTask,
  effectiveState,
  buildActionModel,
  stateCopyMap,
  stateCopy,
  shouldApplyProfileRefresh,
} =
  context.window.ConnectMacWorkbench;

const activeOpenTask = activeLifecycleTask([
  {
    id: "job-other",
    type: "aws-open",
    profile: "other-mac",
    status: "running",
  },
  {
    id: "job-open",
    type: "aws-open",
    profile: "build-mac",
    status: "success",
    lifecycle_state: "waiting",
    request_id: "req-open",
    actor_name: "Build Owner",
    actor_email: "owner@example.com",
    started_at: "2026-07-31T01:02:03Z",
    log: "must not leak",
    command: ["cm", "aws", "open"],
  },
], "build-mac");
assert.deepEqual(
  JSON.parse(JSON.stringify(activeOpenTask)),
  {
    id: "job-open",
    type: "aws-open",
    label: "等待 Mac 状态检查",
    request_id: "req-open",
    actor: "Build Owner",
    started_at: "2026-07-31T01:02:03Z",
    terminal: false,
  },
);
assert.equal(activeLifecycleTask([
  {
    id: "job-destroy",
    type: "aws-destroy",
    profile: "build-mac",
    status: "deferred",
    lifecycle_state: "waiting",
    request_id: "req-destroy",
    actor_email: "release@example.com",
    started_at: "2026-07-31T02:03:04Z",
  },
], "build-mac").label, "等待 Dedicated Host 可释放");
for (const [status, lifecycle_state] of [
  ["failed", "pending"],
  ["interrupted", "waiting"],
  ["success", "finalized"],
  ["running", "failed"],
]) {
  assert.equal(
    activeLifecycleTask([{
      id: "terminal-job",
      type: "aws-open",
      profile: "build-mac",
      status,
      lifecycle_state,
    }], "build-mac"),
    null,
    `${status}/${lifecycle_state} must not be active`,
  );
}
assert.equal(activeLifecycleTask([], "build-mac"), null);
assert.equal(activeLifecycleTask([{ profile: "build-mac", type: "sync", status: "running" }], "build-mac"), null);

const runAWSStart = html.indexOf("async function runAWS(");
const runAWSEnd = html.indexOf("\n    async function previewAWS(", runAWSStart);
assert.ok(runAWSStart >= 0 && runAWSEnd > runAWSStart, "runAWS source must be available");
const runAWSSource = html.slice(runAWSStart, runAWSEnd);
const submitFailureIndex = runAWSSource.indexOf("任务提交失败");
const closeConfirmationIndex = runAWSSource.indexOf("closeAWSConfirm();");
const refreshIndex = runAWSSource.indexOf("await Promise.all([loadJobs({ refreshReminders: true })");
const refreshFailureIndex = runAWSSource.indexOf("任务已提交，状态刷新失败，页面将继续自动更新");
assert.ok(submitFailureIndex >= 0, "confirmed POST failures must be identified as submission failures");
assert.ok(closeConfirmationIndex > submitFailureIndex, "confirmation must close only after the POST succeeds");
assert.ok(refreshIndex > closeConfirmationIndex, "post-submit refresh must start after submitted state is visible");
assert.ok(refreshFailureIndex > refreshIndex, "post-submit refresh must have its own warning");
assert.match(
  runAWSSource.slice(refreshFailureIndex),
  /scheduleJobRefresh\(\);\s+return true;/,
  "refresh failure must retry status loading and retain submission success",
);
assert.doesNotMatch(runAWSSource, /activeTask\?\.type/, "any active lifecycle task must block runAWS");
const previewAWSStart = html.indexOf("async function previewAWS(");
const previewAWSEnd = html.indexOf("\n    function showAWSConfirm(", previewAWSStart);
const previewAWSSource = html.slice(previewAWSStart, previewAWSEnd);
assert.match(previewAWSSource, /if \(activeTask\)/, "any active lifecycle task must block previewAWS");
assert.doesNotMatch(previewAWSSource, /activeTask\?\.type/, "previewAWS must not allow opposite lifecycle operations");

const apiSectionStart = html.indexOf("function sanitizedOperationMessage(");
const apiSectionEnd = html.indexOf("\n    function newLocalRequestID()", apiSectionStart);
assert.ok(apiSectionStart >= 0 && apiSectionEnd > apiSectionStart, "API helper source must be available");
const apiSection = html.slice(apiSectionStart, apiSectionEnd);
const apiContext = {
  responseQueue: [],
  state: { clientConfig: { user_api: "" }, auth: null },
  authShown: 0,
  fetch: async () => apiContext.responseQueue.shift(),
};
vm.createContext(apiContext);
new vm.Script(`
  const state = globalThis.state;
  function showAuth() { globalThis.authShown += 1; }
  ${apiSection}
  globalThis.testAPI = api;
`, { filename: "web/index.html:api-contract" }).runInContext(apiContext);

function fakeAPIResponse(text, options = {}) {
  const calls = [];
  const status = options.status ?? 200;
  return {
    calls,
    status,
    ok: status >= 200 && status < 300,
    headers: {
      get(name) {
        calls.push("header:" + name);
        return options.requestID || "";
      },
    },
    async text() {
      calls.push("text");
      return text;
    },
    async json() {
      calls.push("json");
      return JSON.parse(text);
    },
  };
}

const successResponse = fakeAPIResponse('{"ok":true,"data":{}}', { requestID: "req-success" });
apiContext.responseQueue.push(successResponse);
const successBody = await apiContext.testAPI("/api/test");
assert.equal(successBody.request_id, "req-success");
assert.deepEqual(successResponse.calls, ["header:X-Request-ID", "text"]);

for (const testCase of [
  { name: "empty", text: "", status: 503, requestID: "req-empty" },
  { name: "null", text: "null", status: 502, requestID: "req-null" },
  { name: "malformed", text: "<html>bad gateway</html>", status: 502, requestID: "req-malformed" },
]) {
  const response = fakeAPIResponse(testCase.text, testCase);
  apiContext.responseQueue.push(response);
  await assert.rejects(
    apiContext.testAPI("/api/test"),
    (error) => {
      assert.equal(error.status, testCase.status, testCase.name);
      assert.equal(error.requestID, testCase.requestID, testCase.name);
      assert.equal(error.errorCode, "", testCase.name);
      assert.ok(error.message.length > 0, testCase.name);
      return true;
    },
  );
  assert.deepEqual(response.calls, ["header:X-Request-ID", "text"], testCase.name);
}

const errorResponse = fakeAPIResponse(
  '{"ok":false,"error":"token=server-secret","error_code":"upstream_failed"}',
  { status: 500, requestID: "req-error" },
);
apiContext.responseQueue.push(errorResponse);
await assert.rejects(
  apiContext.testAPI("/api/test"),
  (error) => {
    assert.equal(error.status, 500);
    assert.equal(error.requestID, "req-error");
    assert.equal(error.errorCode, "upstream_failed");
    assert.doesNotMatch(error.message, /server-secret/);
    assert.match(error.message, /\[REDACTED\]/);
    return true;
  },
);

const unauthorizedResponse = fakeAPIResponse(
  '{"ok":false,"error":"unauthorized"}',
  { status: 401, requestID: "req-unauthorized" },
);
apiContext.responseQueue.push(unauthorizedResponse);
await assert.rejects(apiContext.testAPI("/api/test"), (error) => {
  assert.equal(error.status, 401);
  assert.equal(error.requestID, "req-unauthorized");
  return true;
});
assert.equal(apiContext.authShown, 1);
assert.equal(apiContext.state.auth.authenticated, false);

const profileRefreshCases = [
  {
    name: "current visible authenticated refresh",
    input: {
      startedGeneration: 7,
      currentGeneration: 7,
      authenticated: true,
      visible: true,
      aborted: false,
    },
    expected: true,
  },
  {
    name: "logout rejects stale refresh",
    input: {
      startedGeneration: 7,
      currentGeneration: 7,
      authenticated: false,
      visible: true,
      aborted: false,
    },
    expected: false,
  },
  {
    name: "hidden page rejects stale refresh",
    input: {
      startedGeneration: 7,
      currentGeneration: 7,
      authenticated: true,
      visible: false,
      aborted: false,
    },
    expected: false,
  },
  {
    name: "generation change rejects stale refresh",
    input: {
      startedGeneration: 7,
      currentGeneration: 8,
      authenticated: true,
      visible: true,
      aborted: false,
    },
    expected: false,
  },
  {
    name: "abort rejects stale refresh",
    input: {
      startedGeneration: 7,
      currentGeneration: 7,
      authenticated: true,
      visible: true,
      aborted: true,
    },
    expected: false,
  },
];
for (const testCase of profileRefreshCases) {
  assert.equal(
    shouldApplyProfileRefresh(testCase.input),
    testCase.expected,
    testCase.name,
  );
}

const expectedStateCopy = {
  stopped: {
    badge: "已停止",
    heading: "这台 Mac 尚未运行",
  },
  creating: {
    badge: "正在打开",
    heading: "正在打开这台 Mac",
  },
  ready: {
    badge: "已就绪",
    heading: "这台 Mac 已可使用",
  },
  releasing: {
    badge: "正在释放",
    heading: "正在释放这台 Mac",
  },
  blocked: {
    badge: "受阻",
    heading: "当前流程已停止",
  },
  unknown: {
    badge: "状态未知",
    heading: "暂时无法确认 Mac 状态",
  },
};
for (const [state, expected] of Object.entries(expectedStateCopy)) {
  assert.equal(stateCopyMap[state].badge, expected.badge);
  assert.equal(stateCopy(state).heading, expected.heading);
  assert.equal(stateCopy(state).badge, expected.badge);
  assert.ok(stateCopy(state).detail.length > 0, `${state} must explain the next step`);
}
assert.equal(stateCopy("not-a-state").heading, expectedStateCopy.unknown.heading);

assert.equal(effectiveState({ status: { decision: "create" } }), "stopped");
assert.equal(effectiveState({ status: { decision: "wait-ready" } }), "creating");
assert.equal(effectiveState({ status: { decision: "create", ready: true } }), "ready");
for (const status of ["starting", "running", "deferred"]) {
  assert.equal(
    effectiveState({
      profileName: "build-mac",
      status: { decision: "create" },
      jobs: [{ profile: "build-mac", type: "aws-destroy", status }],
    }),
    "releasing",
  );
  assert.equal(
    effectiveState({
      profileName: "build-mac",
      status: { decision: "create" },
      jobs: [{ profile: "build-mac", type: "aws-open", status }],
    }),
    "creating",
  );
}
for (const lifecycle_state of ["pending", "waiting"]) {
  assert.equal(
    effectiveState({
      profileName: "build-mac",
      status: { decision: "create" },
      jobs: [{ profile: "build-mac", type: "aws-destroy", lifecycle_state }],
    }),
    "releasing",
  );
  assert.equal(
    effectiveState({
      profileName: "build-mac",
      status: { decision: "create" },
      jobs: [{ profile: "build-mac", type: "aws-open", lifecycle_state }],
    }),
    "creating",
  );
}
for (const type of ["aws-open", "aws-destroy"]) {
  for (const [status, lifecycle_state] of [
    ["failed", "pending"],
    ["interrupted", "waiting"],
  ]) {
    assert.equal(
      effectiveState({
        profileName: "build-mac",
        status: { decision: "create" },
        jobs: [{ profile: "build-mac", type, status, lifecycle_state }],
      }),
      "stopped",
      `${type} ${status}+${lifecycle_state} must be inactive`,
    );
  }

  for (const [status, lifecycle_state] of [
    ["success", "pending"],
    ["deferred", "waiting"],
  ]) {
    assert.equal(
      effectiveState({
        profileName: "build-mac",
        status: { decision: "create" },
        jobs: [{ profile: "build-mac", type, status, lifecycle_state }],
      }),
      type === "aws-open" ? "creating" : "releasing",
      `${type} ${status}+${lifecycle_state} must follow lifecycle state`,
    );
  }
}
assert.equal(
  effectiveState({
    profileName: "build-mac",
    status: { decision: "create" },
    jobs: [{
      profile: "build-mac",
      type: "aws-destroy",
      status: "deferred",
      lifecycle_state: "finalized",
    }],
  }),
  "stopped",
);
assert.equal(
  effectiveState({
    profileName: "build-mac",
    status: { decision: "create" },
    jobs: [{
      profile: "build-mac",
      type: "aws-open",
      status: "running",
      lifecycle_state: "failed",
    }],
  }),
  "stopped",
);
assert.equal(
  effectiveState({
    profileName: "build-mac",
    status: { decision: "wait-ready" },
    jobs: [
      { profile: "build-mac", type: "aws-open", status: "running" },
      { profile: "build-mac", type: "aws-destroy", status: "starting" },
    ],
  }),
  "releasing",
);
assert.equal(
  effectiveState({
    status: { profile: "mac-a", decision: "create" },
    jobs: [{ profile: "mac-b", type: "aws-destroy", status: "running" }],
  }),
  "stopped",
);
assert.equal(
  effectiveState({
    status: { profile: "mac-a", decision: "create" },
    jobs: [{ profile: "mac-a", type: "aws-open", status: "running" }],
  }),
  "creating",
);
assert.equal(
  effectiveState({
    status: { decision: "create" },
    jobs: [{ profile: "mac-b", type: "aws-destroy", status: "running" }],
  }),
  "stopped",
);
for (const auto_release_state of ["running", "retrying", "notifying"]) {
  assert.equal(
    effectiveState({
      status: { decision: "create" },
      reminder: { auto_release_state },
    }),
    "releasing",
  );
}
assert.equal(effectiveState({ status: { decision: "blocked" } }), "blocked");
assert.equal(effectiveState({ status: { decision: "error" } }), "blocked");
assert.equal(effectiveState({ status: { decision: "launch-on-host" } }), "creating");
assert.equal(effectiveState({ status: { decision: "ready", error: "status failed" } }), "unknown");
assert.equal(effectiveState({ status: null }), "unknown");

function actionModel(overrides = {}) {
  return buildActionModel({
    hasProfile: true,
    effectiveState: "ready",
    mobile: false,
    localAgentOnline: true,
    busy: false,
    canOperate: true,
    canAdmin: true,
    ...overrides,
  });
}

const desktopReady = actionModel();
assert.deepEqual(Object.keys(desktopReady).sort(), ["actions", "primary", "state"]);
assert.equal(desktopReady.state, "ready");
assert.equal(desktopReady.primary, "connect");
for (const action of ["connect", "vnc", "transfer"]) {
  assert.deepEqual(
    JSON.parse(JSON.stringify(desktopReady.actions[action])),
    { visible: true, enabled: true, reason: "" },
  );
}
assert.equal(desktopReady.actions.open.enabled, false);

const mobileReady = actionModel({ mobile: true });
assert.equal(mobileReady.primary, "refresh");
for (const action of ["connect", "vnc", "transfer"]) {
  assert.equal(mobileReady.actions[action].visible, false);
}

const releasing = actionModel({ effectiveState: "releasing" });
for (const action of ["open", "release", "connect", "vnc", "transfer", "cleanup"]) {
  assert.equal(releasing.actions[action].enabled, false, `${action} must be disabled while releasing`);
}

const offline = actionModel({ localAgentOnline: false });
assert.equal(offline.primary, "connect");
assert.equal(offline.actions.connect.enabled, false);
assert.equal(offline.actions.connect.reason, "本机代理未连接");

const notReady = actionModel({ effectiveState: "creating" });
assert.equal(notReady.actions.connect.reason, "Mac 尚未就绪");

const expectedActions = [
  "refresh",
  "open",
  "release",
  "connect",
  "vnc",
  "transfer",
  "extend",
  "cleanup",
  "events",
  "details",
];
for (const action of expectedActions) {
  assert.deepEqual(
    Object.keys(desktopReady.actions[action]).sort(),
    ["enabled", "reason", "visible"],
    `${action} must use the action contract`,
  );
}

const busyReady = actionModel({ effectiveState: "ready", busy: true });
for (const action of ["refresh", "release", "connect", "vnc", "transfer", "extend", "events"]) {
  assert.equal(busyReady.actions[action].enabled, false, `${action} must be locked while busy`);
}
const busyStopped = actionModel({ effectiveState: "stopped", busy: true });
assert.equal(busyStopped.actions.open.enabled, false);
assert.equal(busyStopped.actions.cleanup.enabled, false);

const cannotOperate = actionModel({ effectiveState: "ready", canOperate: false });
for (const action of ["release", "connect", "vnc", "transfer", "extend"]) {
  assert.equal(cannotOperate.actions[action].enabled, false, `${action} requires canOperate`);
}
assert.equal(actionModel({ effectiveState: "stopped", canOperate: false }).actions.open.enabled, false);

const cannotAdmin = actionModel({ effectiveState: "stopped", canAdmin: false });
assert.equal(cannotAdmin.actions.cleanup.visible, false);
assert.equal(cannotAdmin.actions.cleanup.enabled, false);
const canAdmin = actionModel({ effectiveState: "stopped", canAdmin: true });
assert.equal(canAdmin.actions.cleanup.visible, true);
assert.equal(canAdmin.actions.cleanup.enabled, true);

for (const missingPermission of [undefined, null]) {
  const denied = buildActionModel({
    hasProfile: true,
    effectiveState: "stopped",
    mobile: false,
    localAgentOnline: true,
    busy: false,
    canOperate: missingPermission,
    canAdmin: missingPermission,
  });
  assert.equal(denied.actions.open.enabled, false);
  assert.equal(denied.actions.cleanup.visible, false);
  assert.equal(denied.actions.cleanup.enabled, false);
}
const omittedPermissions = buildActionModel({
  hasProfile: true,
  effectiveState: "stopped",
  mobile: false,
  localAgentOnline: true,
  busy: false,
});
assert.equal(omittedPermissions.actions.open.enabled, false);
assert.equal(omittedPermissions.actions.cleanup.visible, false);
assert.equal(omittedPermissions.actions.cleanup.enabled, false);

for (const mobile of [false, true]) {
  const noProfile = buildActionModel({
    hasProfile: false,
    effectiveState: "ready",
    mobile,
    localAgentOnline: true,
    busy: false,
    canOperate: true,
    canAdmin: true,
  });
  assert.equal(noProfile.primary, "refresh");
  for (const actionName of expectedActions) {
    const action = noProfile.actions[actionName];
    assert.equal(action.enabled, false, `${actionName} must require a selected Profile`);
    assert.equal(action.reason, "请先选择 Profile");
  }
  for (const actionName of ["connect", "vnc", "transfer"]) {
    assert.equal(noProfile.actions[actionName].visible, !mobile);
  }
}

const omittedProfile = buildActionModel({
  effectiveState: "ready",
  mobile: false,
  localAgentOnline: true,
  busy: false,
  canOperate: true,
  canAdmin: true,
});
assert.equal(omittedProfile.primary, "refresh");
for (const actionName of expectedActions) {
  assert.equal(omittedProfile.actions[actionName].enabled, false);
  assert.equal(omittedProfile.actions[actionName].reason, "请先选择 Profile");
}

for (const effectiveState of ["creating", "releasing", "ready", "blocked", "unknown"]) {
  assert.equal(
    actionModel({ effectiveState }).actions.cleanup.enabled,
    false,
    `${effectiveState} must not allow cleanup`,
  );
}
const invalidState = actionModel({ effectiveState: "typo-state" });
assert.equal(invalidState.state, "unknown");
assert.equal(invalidState.primary, "refresh");
assert.equal(invalidState.actions.cleanup.enabled, false);

const primaryCases = [
  ["stopped", false, "open"],
  ["ready", false, "connect"],
  ["ready", true, "refresh"],
  ["creating", false, "details"],
  ["releasing", false, "details"],
  ["blocked", false, "refresh"],
  ["unknown", false, "refresh"],
];
for (const [effectiveState, mobile, primary] of primaryCases) {
  assert.equal(
    actionModel({ effectiveState, mobile }).primary,
    primary,
    `${effectiveState}/${mobile ? "mobile" : "desktop"} primary`,
  );
}

function inlineFunctions(startDeclaration, endDeclaration) {
  const start = html.indexOf(startDeclaration);
  const end = html.indexOf(endDeclaration, start);
  assert.ok(start >= 0 && end > start, `${startDeclaration} source must be available`);
  return html.slice(start, end);
}

function fakeElement(initialText = "") {
  const attributes = new Map();
  let text = initialText;
  let writes = 0;
  return {
    disabled: false,
    classList: { toggle() {} },
    get textContent() { return text; },
    set textContent(value) { text = value; writes += 1; },
    get writes() { return writes; },
    setAttribute(name, value) { attributes.set(name, String(value)); },
    getAttribute(name) { return attributes.get(name) ?? null; },
    removeAttribute(name) { attributes.delete(name); },
  };
}

const actionElements = {
  openMacBtn: fakeElement(),
  openMacBtnReason: fakeElement(),
};
const actionContext = {
  elements: actionElements,
  document: { getElementById(id) { return actionElements[id] || null; } },
};
vm.createContext(actionContext);
new vm.Script(`
  const $ = (id) => document.getElementById(id);
  ${inlineFunctions("function applyWorkbenchAction(", "function renderWorkbench(")}
  globalThis.applyWorkbenchAction = applyWorkbenchAction;
  globalThis.guardWorkbenchAction = guardWorkbenchAction;
`, { filename: "web/index.html:workbench-action-a11y" }).runInContext(actionContext);

actionContext.applyWorkbenchAction("openMacBtn", {
  visible: true,
  enabled: false,
  reason: "当前状态不能打开",
});
assert.equal(actionElements.openMacBtn.disabled, false, "aria-disabled actions must remain keyboard focusable");
assert.equal(actionElements.openMacBtn.getAttribute("aria-disabled"), "true");
assert.equal(actionElements.openMacBtn.getAttribute("aria-describedby"), "openMacBtnReason");
assert.equal(actionElements.openMacBtnReason.textContent, "当前状态不能打开");
let guardedCalls = 0;
let prevented = 0;
let stopped = 0;
const guardedOpen = actionContext.guardWorkbenchAction("openMacBtn", () => { guardedCalls += 1; });
assert.equal(guardedOpen({
  preventDefault() { prevented += 1; },
  stopImmediatePropagation() { stopped += 1; },
}), false);
assert.equal(guardedCalls, 0, "aria-disabled action click must be a no-op");
assert.equal(prevented, 1);
assert.equal(stopped, 1);

actionContext.applyWorkbenchAction("openMacBtn", { visible: true, enabled: true, reason: "" });
guardedOpen({});
assert.equal(guardedCalls, 1, "enabled action must call its handler");
assert.equal(actionElements.openMacBtn.getAttribute("aria-describedby"), null);

const localStatus = fakeElement("本机代理未启动");
const repairStatus = fakeElement("等待重新检测。");
const localElements = {
  localAgentStatus: localStatus,
  localAgentRepairBtn: fakeElement(),
  localAgentRepairLayer: {
    classList: { contains() { return true; } },
  },
  localAgentRepairStatus: repairStatus,
};
const localContext = {
  state: { localAgent: { online: false, errorReason: "本机代理未连接" } },
  document: {
    body: { classList: { toggle() {} } },
    getElementById(id) { return localElements[id] || null; },
  },
};
vm.createContext(localContext);
new vm.Script(`
  const state = globalThis.state;
  const localAgentOfflineMessage = "本机代理未连接";
  const $ = (id) => document.getElementById(id);
  ${inlineFunctions("function updateLiveRegion(", "function openLocalAgentRepair(")}
  globalThis.renderLocalAgentStatus = renderLocalAgentStatus;
`, { filename: "web/index.html:local-agent-live-region" }).runInContext(localContext);

localContext.renderLocalAgentStatus();
const firstOfflineWrites = localStatus.writes;
localContext.renderLocalAgentStatus();
assert.equal(localStatus.writes, firstOfflineWrites, "unchanged offline checks must not rewrite live text");
localContext.state.localAgent.online = true;
localContext.state.localAgent.errorReason = "";
localContext.renderLocalAgentStatus();
assert.equal(localStatus.writes, firstOfflineWrites + 1, "online transition must announce once");
localContext.renderLocalAgentStatus();
assert.equal(localStatus.writes, firstOfflineWrites + 1, "unchanged online checks must not rewrite live text");
localContext.state.localAgent.online = false;
localContext.state.localAgent.errorReason = "证书错误";
localContext.renderLocalAgentStatus();
assert.equal(localStatus.writes, firstOfflineWrites + 2, "offline error change must announce once");

function cssRule(selector) {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = workbenchCSS.match(new RegExp(`${escaped}\\s*\\{([^}]+)\\}`));
  assert.ok(match, `${selector} CSS rule must exist`);
  return Object.fromEntries(match[1].split(";")
    .map((declaration) => declaration.trim())
    .filter(Boolean)
    .map((declaration) => {
      const separator = declaration.indexOf(":");
      return [declaration.slice(0, separator).trim(), declaration.slice(separator + 1).trim()];
    }));
}

const actionGroupRule = cssRule(".workbench-actions");
assert.equal(actionGroupRule["min-width"], "0");
assert.equal(actionGroupRule["flex-wrap"], "wrap");
const actionButtonRule = cssRule(".workbench-actions button");
assert.equal(actionButtonRule["max-width"], "100%");
assert.equal(actionButtonRule["flex"], "0 0 auto");
assert.ok(
  !/(?:^|\\s)width\\s*:\\s*[4-9]\\d{2}px/.test(
    workbenchCSS.match(/@media \\(max-width: 640px\\)[\\s\\S]*$/)?.[0] || "",
  ),
  "mobile workbench rules must not introduce a fixed width wider than a 390px viewport",
);

console.log("web workbench state model OK");
