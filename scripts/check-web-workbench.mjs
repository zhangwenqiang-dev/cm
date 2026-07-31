import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const workbenchURL = new URL("../web/assets/connectmac-workbench.js", import.meta.url);
const source = fs.readFileSync(workbenchURL, "utf8");
const context = { window: {} };

vm.createContext(context);
new vm.Script(source, { filename: "web/assets/connectmac-workbench.js" }).runInContext(context);

const { effectiveState, buildActionModel } = context.window.ConnectMacWorkbench;

assert.equal(effectiveState({ status: { decision: "create" } }), "stopped");
assert.equal(effectiveState({ status: { decision: "wait-ready" } }), "creating");
assert.equal(effectiveState({ status: { decision: "create", ready: true } }), "ready");
assert.equal(
  effectiveState({
    profileName: "build-mac",
    status: { decision: "create" },
    jobs: [{ profile: "build-mac", type: "aws-destroy", status: "running" }],
  }),
  "releasing",
);
assert.equal(
  effectiveState({
    profileName: "build-mac",
    status: { decision: "create" },
    jobs: [{ profile: "build-mac", type: "aws-destroy", status: "deferred" }],
  }),
  "releasing",
);
assert.equal(
  effectiveState({
    profileName: "build-mac",
    status: { decision: "create" },
    jobs: [{ profile: "build-mac", type: "aws-destroy", lifecycle_state: "waiting" }],
  }),
  "releasing",
);
assert.equal(
  effectiveState({
    status: { decision: "create" },
    reminder: { auto_release_state: "retrying" },
  }),
  "releasing",
);
assert.equal(effectiveState({ status: { decision: "blocked" } }), "blocked");
assert.equal(effectiveState({ status: { decision: "error" } }), "blocked");
assert.equal(effectiveState({ status: { decision: "launch-on-host" } }), "creating");
assert.equal(effectiveState({ status: { decision: "ready", error: "status failed" } }), "unknown");
assert.equal(effectiveState({ status: null }), "unknown");

const desktopReady = buildActionModel({
  state: "ready",
  isMobile: false,
  localAgentOnline: true,
});
assert.equal(desktopReady.primary, "connect");
for (const action of ["connect", "vnc", "transfer"]) {
  assert.deepEqual(
    JSON.parse(JSON.stringify(desktopReady[action])),
    { visible: true, enabled: true, reason: "" },
  );
}
assert.equal(desktopReady.open.enabled, false);

const mobileReady = buildActionModel({
  state: "ready",
  isMobile: true,
  localAgentOnline: true,
});
assert.equal(mobileReady.primary, "refresh");
for (const action of ["connect", "vnc", "transfer"]) {
  assert.equal(mobileReady[action].visible, false);
}

const releasing = buildActionModel({
  state: "releasing",
  isMobile: false,
  localAgentOnline: true,
});
for (const action of ["open", "release", "connect", "vnc", "transfer", "cleanup"]) {
  assert.equal(releasing[action].enabled, false, `${action} must be disabled while releasing`);
}

const offline = buildActionModel({
  state: "ready",
  isMobile: false,
  localAgentOnline: false,
});
assert.equal(offline.connect.reason, "本机代理未连接");

const notReady = buildActionModel({
  state: "creating",
  isMobile: false,
  localAgentOnline: true,
});
assert.equal(notReady.connect.reason, "Mac 尚未就绪");

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
    Object.keys(desktopReady[action]).sort(),
    ["enabled", "reason", "visible"],
    `${action} must use the action contract`,
  );
}

console.log("web workbench state model OK");
