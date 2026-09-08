package connectmac

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppWebLocalAgentSecureFallbackContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`secureURL: "https://127.0.0.1:18765"`,
		`legacyURL: "http://127.0.0.1:18765"`,
		`async function probeLocalAgent(url, timeoutMs = 1200)`,
		`if (!res.ok || !body?.ok)`,
		`[state.localAgent.secureURL, state.localAgent.legacyURL]`,
		`state.localAgent.errorReason = connectedURL ? "" : localAgentOfflineMessage;`,
		`state.localAgent.url.replace(/^http:/, "ws:").replace(/^https:/, "wss:")`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("secure local-agent contract missing %q", want)
		}
	}
	if strings.Contains(html, `body.local-agent-off .local-action { display: none !important; }`) {
		t.Fatal("desktop local actions must remain visible while the agent is offline")
	}
	if !strings.Contains(html, `class="local-action mobile-local-hidden"`) {
		t.Fatal("local actions must opt into mobile hiding")
	}
}

func TestAppWebLocalAgentCapabilityUI(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`id="localAgentRepairBtn"`,
		`id="localAgentRepairLayer"`,
		`id="copyLocalAgentCommandsBtn"`,
		`id="recheckLocalAgentBtn"`,
		`本机代理未连接。请运行 cm local-agent install && cm local-agent start`,
		`window.matchMedia("(max-width: 640px)")`,
		`mobile-local-hidden`,
		`recordLocalIntent(profile, "vnc")`,
		`localAgentAPI("/open-vnc"`,
		`if (!state.localAgent.online)`,
		`if (shouldReturn && state.view === "terminalView")`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("local capability UI missing %q", want)
		}
	}
	if strings.Contains(html, `state.terminalConnectedProfiles.has(profile)`) {
		t.Fatal("VNC must not require a prior terminal connection")
	}

	cssData, err := os.ReadFile(filepath.Join("..", "..", "web", "assets", "connectmac-workbench.css"))
	if err != nil {
		t.Fatalf("read workbench css: %v", err)
	}
	css := string(cssData)
	for _, want := range []string{
		`@media (max-width: 640px)`,
		`.mobile-local-hidden`,
		`.workbench-mobile-actions`,
		`padding-bottom: 84px`,
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("mobile capability CSS missing %q", want)
		}
	}
}

func TestAppWebLocalAgentDoesNotSwitchEndpointDuringActiveWork(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`function localAgentEndpointLocked()`,
		`state.terminal.socket && state.terminal.socket.readyState !== WebSocket.CLOSED`,
		`Object.values(state.syncJobs).some((job) => syncJobActive(job))`,
		`...(locked ? [] : [state.localAgent.secureURL, state.localAgent.legacyURL]`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("active endpoint lock missing %q", want)
		}
	}
}

func TestAppWebTerminalUsesSharedCapabilityGuard(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	guard := extractWebSource(t, html, "function terminalConnectionCurrent(", "\n    async function openTerminal(profile)")
	openTerminal := extractWebSource(t, html, "async function openTerminal(profile)", "\n    async function connectLocalTerminal(")
	connectTerminal := extractWebSource(t, html, "async function connectLocalTerminal(profile", "\n    function closeTerminal(")

	for _, want := range []string{
		`if (!localTerminalAvailable(profile)) return;`,
		`return connectLocalTerminal(profile);`,
		`if (!profileReady(profile)) {`,
		`setStatus("Mac 尚未 ready，不能打开终端。");`,
		`if (!state.localAgent.online) {`,
		`setStatus(localAgentOfflineMessage);`,
		`openLocalAgentRepair();`,
		`connectionGeneration`,
		`terminalConnectionCurrent(generation, profile)`,
	} {
		if !strings.Contains(guard+openTerminal+connectTerminal, want) {
			t.Fatalf("terminal shared capability guard missing %q", want)
		}
	}
	if !strings.Contains(openTerminal, `if (!localTerminalAvailable(profile)) return;`) ||
		!strings.Contains(connectTerminal, `if (!localTerminalAvailable(profile)) {`) {
		t.Fatalf("initial and reconnect paths must both use the shared terminal guard")
	}
	readyGuard := strings.Index(guard, `if (!profileReady(profile)) {`)
	agentGuard := strings.Index(guard, `if (!state.localAgent.online) {`)
	intentCall := strings.Index(connectTerminal, `recordLocalIntent(profile, "connect")`)
	if readyGuard < 0 || agentGuard < 0 || intentCall < 0 || readyGuard > agentGuard {
		t.Fatalf("terminal guards must validate readiness before local Agent availability: %s", guard)
	}
}

func TestAppWebRepairCommandCopyFallbackBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for repair copy behavior test: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	copySource := extractWebSource(t, html, "function selectLocalAgentCommandsForCopy()", "\n    async function recheckLocalAgent()")
	harness := `
import assert from "node:assert/strict";

const commandsNode = { textContent: "cm local-agent install\ncm local-agent start" };
const statusNode = { textContent: "" };
const selected = [];
const selection = { removeAllRanges() { selected.length = 0; }, addRange(range) { selected.push(range.node); } };
const document = { createRange() { return { node: null, selectNodeContents(node) { this.node = node; } }; } };
const window = { getSelection() { return selection; } };
function $(id) { return id === "localAgentRepairCommands" ? commandsNode : statusNode; }

` + copySource + `

let writes = [];
Object.defineProperty(globalThis, "navigator", { configurable: true, value: {
  platform: "MacIntel",
  clipboard: { async writeText(value) { writes.push(value); } }
} });
await copyLocalAgentCommands();
assert.deepEqual(writes, [commandsNode.textContent]);
assert.equal(statusNode.textContent, "命令已复制。");
assert.deepEqual(selected, []);

Object.defineProperty(globalThis, "navigator", { configurable: true, value: { platform: "MacIntel" } });
await copyLocalAgentCommands();
assert.deepEqual(selected, [commandsNode]);
assert.equal(statusNode.textContent, "已选中，请按 Cmd+C");

selected.length = 0;
Object.defineProperty(globalThis, "navigator", { configurable: true, value: {
  platform: "MacIntel",
  clipboard: { async writeText() { throw new Error("denied"); } }
} });
await copyLocalAgentCommands();
assert.deepEqual(selected, [commandsNode]);
assert.equal(statusNode.textContent, "已选中，请按 Cmd+C");
`
	scriptPath := filepath.Join(t.TempDir(), "repair-copy-fallback.mjs")
	if err := os.WriteFile(scriptPath, []byte(harness), 0o600); err != nil {
		t.Fatalf("write repair copy harness: %v", err)
	}
	if output, err := exec.Command(node, scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("repair copy behavior test failed: %v\n%s", err, output)
	}
}

func TestAppWebTerminalConnectionGenerationBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for terminal generation behavior test: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	terminalHelpers := extractWebSource(t, html, "function terminalConnectionCurrent(", "\n    async function openTerminal(profile)")
	connectTerminal := extractWebSource(t, html, "async function connectLocalTerminal(profile", "\n    function closeTerminal(")
	closeTerminal := extractWebSource(t, html, "function closeTerminal(", "\n    function sendTerminalInput(")
	harness := `
import assert from "node:assert/strict";

const state = {
  localAgent: { online: true, url: "https://127.0.0.1:18765" },
  terminalConnectedProfiles: new Set(),
  terminal: { profile: "", socket: null, manualClose: false, returnOnClose: false, connectionGeneration: 0, xterm: null },
  view: "terminalView"
};
const localAgentOfflineMessage = "offline";
const statuses = [];
const appended = [];
const views = [];
const intentDeferred = [];
const agentDeferred = [];
const sockets = [];
function deferred() { let resolve; const promise = new Promise((done) => { resolve = done; }); return { promise, resolve }; }
function flush() { return new Promise((resolve) => setImmediate(resolve)); }
function profileReady() { return true; }
function setStatus(value) { statuses.push(value); }
function terminalSetStatus(value) { statuses.push(value); }
function terminalAppend(value) { appended.push(value); }
function terminalClear() {}
function renderProfiles() {}
function renderSelected() {}
function openLocalAgentRepair() {}
function showOperationError() {}
function showView(value) { views.push(value); state.view = value; }
function localAgentPayload(profile) { return { profile }; }
async function ensureLocalHostKey() {}
function sanitizedOperationMessage(error) { return String(error?.message || error); }
function $(id) { return { focus() {} }; }
async function recordLocalIntent() { const item = deferred(); intentDeferred.push(item); return item.promise; }
async function localAgentAPI() { const item = deferred(); agentDeferred.push(item); return item.promise; }
class FakeWebSocket {
  static CLOSED = 3;
  constructor(url) { this.url = url; this.readyState = 0; this.closeCount = 0; sockets.push(this); }
  close() { this.closeCount++; this.readyState = FakeWebSocket.CLOSED; }
}
globalThis.WebSocket = FakeWebSocket;

` + terminalHelpers + "\n" + connectTerminal + "\n" + closeTerminal + `

const first = connectLocalTerminal("shared");
intentDeferred[0].resolve({ data: { request_id: "first" } });
await flush();
agentDeferred[0].resolve({ data: { terminal_session_token: "first-token" } });
await first;
assert.equal(sockets.length, 1);
const staleSocket = sockets[0];

const second = connectLocalTerminal("shared");
assert.equal(staleSocket.closeCount, 1);
intentDeferred[1].resolve({ data: { request_id: "second" } });
await flush();
agentDeferred[1].resolve({ data: { terminal_session_token: "second-token" } });
await second;
assert.equal(sockets.length, 2);
const activeSocket = sockets[1];
const statusCount = statuses.length;
const appendCount = appended.length;
staleSocket.onopen();
staleSocket.onmessage({ data: "stale output" });
staleSocket.onerror();
staleSocket.onclose();
assert.equal(state.terminal.socket, activeSocket);
assert.equal(statuses.length, statusCount);
assert.equal(appended.length, appendCount);
activeSocket.onopen();
assert.equal(state.terminal.socket, activeSocket);
assert.equal(state.terminalConnectedProfiles.has("shared"), true);

const duringFetch = connectLocalTerminal("shared");
intentDeferred[2].resolve({ data: { request_id: "third" } });
await flush();
assert.equal(agentDeferred.length, 3);
const socketCountBeforeClose = sockets.length;
closeTerminal(true, true);
agentDeferred[2].resolve({ data: { terminal_session_token: "third-token" } });
await duringFetch;
assert.equal(sockets.length, socketCountBeforeClose);
assert.equal(state.terminal.socket, null);
assert.equal(views.at(-1), "operationsView");
`
	scriptPath := filepath.Join(t.TempDir(), "terminal-generation.mjs")
	if err := os.WriteFile(scriptPath, []byte(harness), 0o600); err != nil {
		t.Fatalf("write terminal generation harness: %v", err)
	}
	if output, err := exec.Command(node, scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("terminal generation behavior test failed: %v\n%s", err, output)
	}
}

func TestAppWebTerminalCapabilityGuardBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for terminal capability behavior test: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	guard := extractWebSource(t, html, "function terminalConnectionCurrent(", "\n    async function openTerminal(profile)")
	openTerminal := extractWebSource(t, html, "async function openTerminal(profile)", "\n    async function connectLocalTerminal(")
	connectTerminal := extractWebSource(t, html, "async function connectLocalTerminal(profile", "\n    function closeTerminal(")
	harness := `
import assert from "node:assert/strict";

const state = {
  localAgent: { online: true },
  selected: "",
  terminal: { profile: "", manualClose: false, returnOnClose: false, connectionGeneration: 0, socket: null, xterm: null }
};
const localAgentOfflineMessage = "本机代理未连接。请运行 cm local-agent install && cm local-agent start";
let ready = false;
let intentCalls = 0;
let agentCalls = 0;
let repairCalls = 0;
const statuses = [];
const terminalStatuses = [];
const views = [];

function profileReady() { return ready; }
function setStatus(message) { statuses.push(message); }
function terminalSetStatus(message) { terminalStatuses.push(message); }
function openLocalAgentRepair() { repairCalls++; }
function showView(view) { views.push(view); }
function ensureTerminalSurface() {}
function renderProfiles() {}
function renderSelected() {}
function closeTerminal() {}
function terminalClear() {}
function terminalAppend() {}
function localAgentPayload(profile) { return { profile }; }
function showOperationError() {}
async function recordLocalIntent() { intentCalls++; return { data: { request_id: "request" } }; }
async function localAgentAPI() { agentCalls++; return { data: { terminal_session_token: "token" } }; }

` + guard + "\n" + openTerminal + "\n" + connectTerminal + `

await openTerminal("not-ready-initial");
await connectLocalTerminal("not-ready-reconnect");
assert.equal(intentCalls, 0);
assert.equal(agentCalls, 0);
assert.equal(views.length, 0);
assert.equal(statuses.at(-1), "Mac 尚未 ready，不能打开终端。");
assert.equal(terminalStatuses.at(-1), "Mac 尚未 ready，不能打开终端。");

ready = true;
state.localAgent.online = false;
await openTerminal("offline-initial");
await connectLocalTerminal("offline-reconnect");
assert.equal(intentCalls, 0);
assert.equal(agentCalls, 0);
assert.equal(views.length, 0);
assert.equal(repairCalls, 2);
assert.equal(statuses.at(-1), localAgentOfflineMessage);
assert.equal(terminalStatuses.at(-1), localAgentOfflineMessage);
`

	scriptPath := filepath.Join(t.TempDir(), "terminal-capability-guard.mjs")
	if err := os.WriteFile(scriptPath, []byte(harness), 0o600); err != nil {
		t.Fatalf("write terminal capability harness: %v", err)
	}
	if output, err := exec.Command(node, scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("terminal capability behavior test failed: %v\n%s", err, output)
	}
}
