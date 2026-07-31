import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const workbenchURL = new URL("../web/assets/connectmac-workbench.js", import.meta.url);
const source = fs.readFileSync(workbenchURL, "utf8");
const context = { window: {} };

vm.createContext(context);
new vm.Script(source, { filename: "web/assets/connectmac-workbench.js" }).runInContext(context);

const { effectiveState, buildActionModel, stateCopyMap, stateCopy } =
  context.window.ConnectMacWorkbench;

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
  effectiveState: "stopped",
  mobile: false,
  localAgentOnline: true,
  busy: false,
});
assert.equal(omittedPermissions.actions.open.enabled, false);
assert.equal(omittedPermissions.actions.cleanup.visible, false);
assert.equal(omittedPermissions.actions.cleanup.enabled, false);

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

console.log("web workbench state model OK");
