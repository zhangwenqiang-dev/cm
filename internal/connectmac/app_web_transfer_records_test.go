package connectmac

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebTransferRecordAuthorizationAndLifecycle(t *testing.T) {
	app, handler, admin, operator := newWebTransferTestApp(t)

	adminRecord := startWebTransferRecord(t, &app, handler, admin, `{"profile":"shared","direction":"push","local_path":"/admin","remote_path":"~/admin"}`)
	operatorRecord := startWebTransferRecord(t, &app, handler, operator, `{"profile":"shared","direction":"pull","local_path":"/operator","remote_path":"~/operator"}`)
	if adminRecord.Phase != TransferPhasePreparing || operatorRecord.Phase != TransferPhasePreparing {
		t.Fatalf("created phases = %q, %q", adminRecord.Phase, operatorRecord.Phase)
	}

	rec := serveWebTransfer(t, &app, handler, operator, http.MethodGet, "/api/transfer-records", "")
	operatorRecords := decodeWebTransferRecords(t, rec)
	if len(operatorRecords) != 1 || operatorRecords[0].ID != operatorRecord.ID {
		t.Fatalf("operator records = %+v, want only %s", operatorRecords, operatorRecord.ID)
	}
	rec = serveWebTransfer(t, &app, handler, admin, http.MethodGet, "/api/transfer-records", "")
	adminRecords := decodeWebTransferRecords(t, rec)
	if len(adminRecords) != 1 || adminRecords[0].ID != adminRecord.ID {
		t.Fatalf("admin records = %+v, want only %s", adminRecords, adminRecord.ID)
	}

	update := `{"id":"` + operatorRecord.ID + `","local_job_id":"job-1","status":"running","phase":"transferring","percent":25}`
	rec = serveWebTransfer(t, &app, handler, admin, http.MethodPost, "/api/transfer-record/update", update)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin cross-member update status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = serveWebTransfer(t, &app, handler, admin, http.MethodPost, "/api/transfer-record/delete", `{"id":"`+operatorRecord.ID+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin cross-member delete status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update", update)
	if rec.Code != http.StatusOK {
		t.Fatalf("milestone update status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update",
		`{"id":"`+operatorRecord.ID+`","local_job_id":"job-1","status":"succeeded","phase":"succeeded","percent":100,"elapsed_ms":1250}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("terminal update status=%d body=%s", rec.Code, rec.Body.String())
	}

	injected := `{"profile":"shared","direction":"push","local_path":"/tmp","remote_path":"~/tmp","member_id":"` + admin.ID + `"}`
	rec = serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/start", injected)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("member_id injection status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/start",
		`{"profile":"shared","direction":"push","local_path":"/tmp","remote_path":"~/tmp"} {"member_id":"`+admin.ID+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing member_id injection status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/start",
		`{"profile":"private","direction":"push","local_path":"/tmp","remote_path":"~/tmp"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("profile authorization status=%d body=%s", rec.Code, rec.Body.String())
	}

	entries := readTestLogEntries(t, app.LogManager)
	createdLog := findTransferLogMessage(t, entries, "created")
	if createdLog.MemberEmail != admin.Email || createdLog.TransferID != adminRecord.ID ||
		createdLog.Profile != "shared" || createdLog.Direction != TransferDirectionPush ||
		createdLog.Status != TransferStatusCreated || createdLog.Phase != TransferPhasePreparing || createdLog.Percent != 0 {
		t.Fatalf("creation log fields = %+v", createdLog)
	}
	milestoneLog := findTransferLogMessage(t, entries, "milestone")
	if milestoneLog.MemberEmail != operator.Email || milestoneLog.TransferID != operatorRecord.ID ||
		milestoneLog.LocalJobID != "job-1" || milestoneLog.Profile != "shared" ||
		milestoneLog.Direction != TransferDirectionPull || milestoneLog.Status != TransferStatusRunning || milestoneLog.Phase != TransferPhaseTransferring ||
		milestoneLog.Percent != 25 {
		t.Fatalf("milestone log fields = %+v", milestoneLog)
	}
	terminalLog := findTransferLogMessage(t, entries, "terminal")
	if terminalLog.MemberEmail != operator.Email || terminalLog.TransferID != operatorRecord.ID ||
		terminalLog.LocalJobID != "job-1" || terminalLog.Profile != "shared" ||
		terminalLog.Direction != TransferDirectionPull || terminalLog.Status != TransferStatusSucceeded || terminalLog.Phase != TransferPhaseSucceeded ||
		terminalLog.Percent != 100 || terminalLog.ElapsedMS != 1250 {
		t.Fatalf("terminal log fields = %+v", terminalLog)
	}
	authLog := findTransferLogMessage(t, entries, "authorization rejected: transfer profile access denied")
	if authLog.MemberEmail != operator.Email || authLog.Profile != "private" ||
		authLog.Direction != TransferDirectionPush {
		t.Fatalf("authorization rejection log fields = %+v", authLog)
	}
}

func TestWebTransferRecordFinalizingPhasePersistsAndLogs(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	record := startWebTransferRecord(t, &app, handler, operator, `{"profile":"shared","direction":"push","local_path":"/tmp","remote_path":"~/tmp"}`)
	rec := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update",
		`{"id":"`+record.ID+`","local_job_id":"job-1","status":"running","phase":"finalizing","percent":99}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	records := decodeWebTransferRecords(t, serveWebTransfer(t, &app, handler, operator, http.MethodGet, "/api/transfer-records", ""))
	if len(records) != 1 || records[0].Phase != TransferPhaseFinalizing || records[0].Percent != 99 {
		t.Fatalf("records = %+v", records)
	}
	entry := findTransferLogMessage(t, readTestLogEntries(t, app.LogManager), "milestone")
	if entry.Phase != TransferPhaseFinalizing || entry.Percent != 99 {
		t.Fatalf("finalizing log = %+v", entry)
	}
}

func TestWebTransferRecordPersistsExactFailureAndInterruptedProgress(t *testing.T) {
	for _, test := range []struct {
		name   string
		status string
		phase  string
	}{
		{name: "failed", status: TransferStatusFailed, phase: TransferPhaseFailed},
		{name: "interrupted", status: TransferStatusInterrupted, phase: TransferPhaseInterrupted},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, handler, _, operator := newWebTransferTestApp(t)
			record := startWebTransferRecord(t, &app, handler, operator, `{"profile":"shared","direction":"pull","local_path":"/tmp","remote_path":"~/tmp"}`)
			body := `{"id":"` + record.ID + `","local_job_id":"job-1","status":"` + test.status +
				`","phase":"` + test.phase + `","percent":48,"error_summary":"exit status 23"}`
			rec := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			records := decodeWebTransferRecords(t, serveWebTransfer(t, &app, handler, operator, http.MethodGet, "/api/transfer-records", ""))
			if len(records) != 1 || records[0].Status != test.status || records[0].Phase != test.phase || records[0].Percent != 48 {
				t.Fatalf("records = %+v", records)
			}
		})
	}
}

func TestWebTransferRecordTerminalRetryIsIdempotent(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	record := startWebTransferRecord(t, &app, handler, operator, `{"profile":"shared","direction":"pull","local_path":"/tmp","remote_path":"~/tmp"}`)
	body := `{"id":"` + record.ID + `","local_job_id":"job-1","status":"failed","phase":"failed","percent":48,"error_summary":"exit status 23"}`
	first := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first terminal update status=%d body=%s", first.Code, first.Body.String())
	}
	second := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update", body)
	if second.Code != http.StatusOK {
		t.Fatalf("idempotent terminal retry status=%d body=%s", second.Code, second.Body.String())
	}

	conflict := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update",
		`{"id":"`+record.ID+`","local_job_id":"job-1","status":"failed","phase":"failed","percent":49,"error_summary":"exit status 23"}`)
	if conflict.Code != http.StatusBadRequest {
		t.Fatalf("conflicting terminal retry status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestWebTransferRecordInfersPhaseForOlderClients(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	record := startWebTransferRecord(t, &app, handler, operator, `{"profile":"shared","direction":"push","local_path":"/tmp","remote_path":"~/tmp"}`)
	rec := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update",
		`{"id":"`+record.ID+`","local_job_id":"job-1","status":"running","percent":99}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	records := decodeWebTransferRecords(t, serveWebTransfer(t, &app, handler, operator, http.MethodGet, "/api/transfer-records", ""))
	if len(records) != 1 || records[0].Phase != TransferPhaseFinalizing {
		t.Fatalf("records = %+v", records)
	}
}

func TestWebTransferPhaseLabelsAndTerminalPersistenceOrdering(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`if (phase === "preparing") return label + "准备中"`,
		`if (job.phase === "transferring") return label + "中" + estimated`,
		`if (phase === "finalizing") return "正在完成校验"`,
		`if (phase === "succeeded") return label + "完成"`,
		`function transferPersistedPercent(job, exactPercent, terminal)`,
		`const value = terminalSyncPending(job) || job.phase === "finalizing"`,
		`? Math.min(99, Math.max(0, Math.round(percent)))`,
		`phase,`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web index missing %q", want)
		}
	}
	pollStart := strings.Index(html, "async function pollSyncJob(")
	if pollStart < 0 {
		t.Fatal("pollSyncJob source not found")
	}
	pollEnd := strings.Index(html[pollStart:], "\n    function cancelSyncJobsRequest")
	if pollEnd < 0 {
		t.Fatal("pollSyncJob source not found")
	}
	poll := html[pollStart : pollStart+pollEnd]
	persist := strings.Index(poll, "await updateTransferRecord(record.id, savedJob)")
	present := strings.Index(poll, "presentSyncJob(savedJob)")
	if persist < 0 || present < 0 || persist >= present {
		t.Fatalf("terminal job must be persisted before it is presented; function = %s", poll)
	}
	runStart := strings.Index(html, "async function runSync(direction)")
	if runStart < 0 {
		t.Fatal("runSync source not found")
	}
	runEnd := strings.Index(html[runStart:], "\n    function terminalSetStatus")
	if runEnd < 0 {
		t.Fatal("runSync source end not found")
	}
	runSync := html[runStart : runStart+runEnd]
	for _, want := range []string{
		`let terminalPersisted = true;`,
		`terminalPersisted = await persistTerminalTransfer(record, savedJob);`,
		`if (!transferTerminal(savedJob.status) || terminalPersisted)`,
	} {
		if !strings.Contains(runSync, want) {
			t.Fatalf("fast runSync terminal presentation guard missing %q; function = %s", want, runSync)
		}
	}
}

func TestWebTerminalTransferRetriesPersistenceBeforePresentation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for transfer retry behavior test: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	start := strings.Index(html, "function transferTerminal(status)")
	if start < 0 {
		t.Fatal("transfer persistence source not found")
	}
	end := strings.Index(html[start:], "\n    function clearTransferMissingConfirmation")
	if end < 0 {
		t.Fatal("transfer persistence source not found")
	}
	source := html[start : start+end]
	source = strings.Replace(source,
		"const terminalSyncBaseDelayMS = 1000;",
		"const terminalSyncBaseDelayMS = 1;",
		1,
	)
	script := `
import assert from "node:assert/strict";
const calls = [];
const presentations = [];
let failures = 1;
const window = globalThis;
const terminalSyncBaseDelayMS = 1;
const terminalSyncMaxDelayMS = 10;
const terminalSyncMaxDurationMS = 1000;
const terminalSyncMaxAttempts = 10;
const terminalSyncRequestTimeoutMS = 100;
const state = {
  transferMilestones: {},
  pendingTerminalSyncs: {},
  terminalSyncGeneration: 0,
  terminalSyncRequests: new Set(),
  syncHistory: [{id:"transfer-1", profile_name:"shared", status:"running", phase:"transferring", percent:25}],
  view: "syncView",
  selected: "shared"
};
function syncJobKey(profile, direction) { return profile && direction ? profile + "\n" + direction : ""; }
async function api(path, options) {
  const body = JSON.parse(options.body);
  calls.push(body);
  if (failures-- > 0) {
    globalThis.committedBody = structuredClone(body);
    throw new Error("response lost after commit");
  }
  assert.deepEqual(body, globalThis.committedBody);
  return {data:{record:{id:body.id, profile_name:"shared", ...body}}};
}
function renderSyncHistory() {}
function presentSyncJob(job) { presentations.push({status:job.status, phase:job.phase, percent:job.percent}); }
function renderSelected() {}
function loadSyncHistory() { return Promise.resolve(); }
function loadEvents() { return Promise.resolve(); }
function setOutput() {}
function setStatus() {}
` + source + `
const job = {
  id:"job-1", transfer_id:"transfer-1", profile:"shared", direction:"pull",
  status:"failed", phase:"failed", percent:48, error:"exit status 23",
  created_at:new Date().toISOString(), finished_at:new Date().toISOString()
};
const persisted = await persistTerminalTransfer({id:"transfer-1"}, job);
assert.equal(persisted, false);
assert.equal(calls.length, 1);
assert.equal(calls[0].percent, 48);
assert.equal(calls[0].phase, "failed");
assert.equal(presentations.length, 0);
await new Promise((resolve) => setTimeout(resolve, 30));
assert.equal(calls.length, 2);
assert.equal(calls[1].percent, 48);
assert.equal(calls[1].phase, "failed");
assert.equal(state.pendingTerminalSyncs["shared\npull"], undefined);
assert.deepEqual(presentations, [{status:"failed", phase:"failed", percent:48}]);
`
	scriptPath := filepath.Join(t.TempDir(), "transfer-retry.mjs")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("terminal transfer retry node test failed: %v\n%s", err, output)
	}
}

func TestWebTerminalTransferSyncLifecycleIsBoundedAndSessionSafe(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for terminal sync lifecycle test: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	start := strings.Index(html, "function transferTerminal(status)")
	if start < 0 {
		t.Fatal("terminal sync source not found")
	}
	end := strings.Index(html[start:], "\n    function clearTransferMissingConfirmation")
	if end < 0 {
		t.Fatal("terminal sync source end not found")
	}
	source := html[start : start+end]
	script := `
import assert from "node:assert/strict";
const window = globalThis;
const terminalSyncBaseDelayMS = 1;
const terminalSyncMaxDelayMS = 2;
const terminalSyncMaxDurationMS = 100;
const terminalSyncMaxAttempts = 2;
const terminalSyncRequestTimeoutMS = 5;
const state = {
  transferMilestones: {},
  pendingTerminalSyncs: {},
  terminalSyncGeneration: 0,
  terminalSyncRequests: new Set(),
  syncHistory: [{id:"transfer-1", profile_name:"shared", status:"running", phase:"transferring", percent:25}],
  syncJobs: {},
  view: "syncView",
  selected: "shared"
};
let mode = "deferred";
let calls = 0;
let resolveDeferred;
let presentations = 0;
let deferredAborted = false;
let timeoutAborted = false;
function abortError() {
  const err = new Error("aborted");
  err.name = "AbortError";
  return err;
}
async function api(path, options) {
  calls += 1;
  const body = JSON.parse(options.body);
  if (mode === "deferred") {
    return await new Promise((resolve, reject) => {
      resolveDeferred = () => resolve({data:{record:{profile_name:"shared", ...body}}});
      options.signal.addEventListener("abort", () => {
        deferredAborted = true;
        reject(abortError());
      }, {once:true});
    });
  }
  if (mode === "never") {
    return await new Promise((resolve, reject) => {
      options.signal.addEventListener("abort", () => {
        timeoutAborted = true;
        reject(abortError());
      }, {once:true});
    });
  }
  if (mode === "server-error") {
    const err = new Error("temporary");
    err.status = 503;
    throw err;
  }
  if (mode === "client-error") {
    const err = new Error("invalid");
    err.status = 400;
    throw err;
  }
  return {data:{record:{profile_name:"shared", ...body}}};
}
function syncJobKey(profile, direction) { return profile && direction ? profile + "\n" + direction : ""; }
function renderSyncHistory() {}
function renderSelected() {}
function presentSyncJob() { presentations += 1; }
function loadSyncHistory() { return Promise.resolve(); }
function loadEvents() { return Promise.resolve(); }
function setStatus() {}
function setOutput() {}
` + source + `
const job = {
  id:"job-1", transfer_id:"transfer-1", profile:"shared", direction:"pull",
  status:"failed", phase:"failed", percent:48, error:"exit status 23"
};

const stale = persistTerminalTransfer({id:"transfer-1"}, job);
await new Promise((resolve) => setTimeout(resolve, 0));
cancelPendingTerminalSyncs();
resolveDeferred();
assert.equal(await stale, false);
assert.equal(deferredAborted, true);
assert.deepEqual(state.transferMilestones, {});
assert.equal(presentations, 0);

mode = "server-error";
const beforeCanceledTimer = calls;
await persistTerminalTransfer({id:"transfer-1"}, job);
assert.ok(state.pendingTerminalSyncs["shared\npull"]);
cancelPendingTerminalSyncs();
await new Promise((resolve) => setTimeout(resolve, 10));
assert.equal(calls, beforeCanceledTimer + 1);

await persistTerminalTransfer({id:"transfer-1"}, job);
await new Promise((resolve) => setTimeout(resolve, 15));
const failed = state.pendingTerminalSyncs["shared\npull"];
assert.equal(failed.state, "terminal_sync_failed");
assert.equal(failed.timerID, 0);
const boundedCalls = calls;
await new Promise((resolve) => setTimeout(resolve, 10));
assert.equal(calls, boundedCalls);

mode = "success";
retryTerminalSync("pull");
await new Promise((resolve) => setTimeout(resolve, 10));
assert.equal(state.pendingTerminalSyncs["shared\npull"], undefined);
assert.equal(presentations, 1);

mode = "client-error";
const pushJob = {...job, id:"job-2", direction:"push"};
const beforeClientError = calls;
await persistTerminalTransfer({id:"transfer-2"}, pushJob);
assert.equal(state.pendingTerminalSyncs["shared\npush"].state, "terminal_sync_failed");
await new Promise((resolve) => setTimeout(resolve, 10));
assert.equal(calls, beforeClientError + 1);

cancelPendingTerminalSyncs();
mode = "never";
const beforeTimeout = calls;
assert.equal(await persistTerminalTransfer({id:"transfer-1"}, job), false);
assert.equal(timeoutAborted, true);
assert.equal(calls, beforeTimeout + 1);
assert.equal(state.pendingTerminalSyncs["shared\npull"].state, "pending");
assert.equal(state.pendingTerminalSyncs["shared\npull"].attempts, 1);
cancelPendingTerminalSyncs();
`
	scriptPath := filepath.Join(t.TempDir(), "terminal-sync-lifecycle.mjs")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("terminal sync lifecycle node test failed: %v\n%s", err, output)
	}

	for _, want := range []string{
		`async function logout() {
      cancelPendingTerminalSyncs();`,
		`function clearSyncProfileState(profile) {
      if (!profile) return;
      cancelPendingTerminalSyncs();`,
		`"terminal_sync_failed"`,
		`const terminalSyncRequestTimeoutMS = 15000;`,
		`重试保存结果`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web lifecycle source missing %q", want)
		}
	}
}

func TestWebFastRunSyncPresentsTerminalOnlyAfterPersistence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for fast transfer behavior test: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	start := strings.Index(html, "async function runSync(direction)")
	if start < 0 {
		t.Fatal("runSync source not found")
	}
	end := strings.Index(html[start:], "\n    function terminalSetStatus")
	if end < 0 {
		t.Fatal("runSync source end not found")
	}
	source := html[start : start+end]
	script := `
import assert from "node:assert/strict";
const state = {
  syncJobsLoading: {},
  localAgent: {online:true},
  syncHistory: [],
  syncPollUnavailable: {},
  busy: false
};
let persistResult = false;
let persistCalls = 0;
let presentations = 0;
const profile = {name:"shared"};
function selectedProfile() { return profile; }
function profileReady() { return true; }
function syncJob() { return null; }
function syncJobBusy() { return false; }
function $(id) { return {value:id.includes("Remote") ? "~/Documents/" : "/tmp/local"}; }
function syncDirectionLabel(direction) { return direction === "push" ? "上传" : "下载"; }
function setBusy(value) { state.busy = value; }
function setStatus() {}
function setOutput() {}
async function api() { return {data:{record:{id:"transfer-1"}}}; }
function renderSyncHistory() {}
function localAgentPayload(profileName, payload) { return {...payload, profile:profileName}; }
async function recordLocalIntent(profileName, operation) {
  return {data:{request_id:"server-" + operation + "-request"}};
}
async function ensureLocalHostKey() {}
async function localAgentAPI() {
  return {data:{job:{
    id:"job-1", transfer_id:"transfer-1", profile:"shared", direction:"pull",
    status:"failed", phase:"failed", percent:48, error:"exit status 23"
  }}};
}
function syncJobKey(profileName, direction) { return profileName + "\n" + direction; }
function storeSyncJob(key, job) { return job; }
function scheduleSyncPoll() {}
function transferTerminal(status) { return ["succeeded","failed","interrupted","unconfirmed"].includes(status); }
async function persistTerminalTransfer() { persistCalls += 1; return persistResult; }
async function updateTransferRecord() { throw new Error("running update should not be called"); }
function loadSyncJobs() {}
function renderProfiles() {}
function renderSelected() {}
function presentSyncJob() { presentations += 1; }
async function failTransferRecord() {}
` + source + `
await runSync("pull");
assert.equal(persistCalls, 1);
assert.equal(presentations, 0);
persistResult = true;
await runSync("pull");
assert.equal(persistCalls, 2);
assert.equal(presentations, 1);
`
	scriptPath := filepath.Join(t.TempDir(), "fast-transfer.mjs")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("fast runSync node test failed: %v\n%s", err, output)
	}
}

func TestWebTransferRecordUpdateValidationErrorIsBadRequest(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	record := startWebTransferRecord(t, &app, handler, operator, `{"profile":"shared","direction":"push","local_path":"/tmp","remote_path":"~/tmp"}`)
	before := len(readTestLogEntries(t, app.LogManager))
	rec := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update",
		`{"id":"`+record.ID+`","local_job_id":"job-1","status":"running","phase":"transferring","percent":101}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	entries := readTestLogEntries(t, app.LogManager)
	for _, entry := range entries[before:] {
		if strings.Contains(entry.Message, "persistence error") {
			t.Fatalf("validation error logged as persistence error: %+v", entry)
		}
	}
}

func TestWebTransferRecordRejectsContradictoryTerminalState(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	record := startWebTransferRecord(t, &app, handler, operator, `{"profile":"shared","direction":"push","local_path":"/tmp","remote_path":"~/tmp"}`)
	for _, body := range []string{
		`{"id":"` + record.ID + `","local_job_id":"job-1","status":"succeeded","phase":"succeeded","percent":99}`,
		`{"id":"` + record.ID + `","local_job_id":"job-1","status":"failed","phase":"failed","percent":100}`,
		`{"id":"` + record.ID + `","local_job_id":"job-1","status":"interrupted","phase":"failed","percent":48}`,
		`{"id":"` + record.ID + `","local_job_id":"job-1","status":"running","phase":"finalizing","percent":95}`,
	} {
		rec := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s for %s", rec.Code, rec.Body.String(), body)
		}
	}
}

func TestWebTransferRecordStartValidationErrorIsBadRequest(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	rec := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/start",
		`{"profile":"shared","direction":"sideways","local_path":"/tmp","remote_path":"~/tmp"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	files, err := app.LogManager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		return
	}
	for _, entry := range readTestLogEntries(t, app.LogManager) {
		if strings.Contains(entry.Message, "persistence error") {
			t.Fatalf("start validation error logged as persistence error: %+v", entry)
		}
	}
}

func TestWebTransferRecordUpdatePersistenceErrorIsInternalServerError(t *testing.T) {
	app, _, _, operator := newWebTransferTestApp(t)
	store := failingTransferRepository{MemberRepository: app.MemberStore, updateErr: errors.New("database unavailable")}
	app.MemberStore = store
	handler := app.newWebHandler("")
	rec := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update",
		`{"id":"transfer-1","local_job_id":"job-1","status":"running","percent":25}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	entry := findTransferLogMessage(t, readTestLogEntries(t, app.LogManager), "persistence error")
	if entry.MemberEmail != operator.Email || entry.TransferID != "transfer-1" ||
		entry.LocalJobID != "job-1" || entry.Status != TransferStatusRunning || entry.Percent != 25 {
		t.Fatalf("update persistence error log fields = %+v", entry)
	}
}

func TestWebTransferRecordPersistenceErrorIsLogged(t *testing.T) {
	app, _, _, operator := newWebTransferTestApp(t)
	app.MemberStore = failingTransferRepository{MemberRepository: app.MemberStore, createErr: errors.New("database unavailable")}
	handler := app.newWebHandler("")
	rec := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/transfer-record/start",
		`{"profile":"shared","direction":"push","local_path":"/tmp","remote_path":"~/tmp"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	entry := findTransferLogMessage(t, readTestLogEntries(t, app.LogManager), "persistence error")
	if entry.MemberEmail != operator.Email || entry.Profile != "shared" ||
		entry.Direction != TransferDirectionPush || entry.Status != TransferStatusCreated {
		t.Fatalf("persistence error log fields = %+v", entry)
	}
}

func newWebTransferTestApp(t *testing.T) (App, http.Handler, Member, Member) {
	t.Helper()
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	admin, err := app.MemberStore.SetupAdmin("Admin", "admin@example.com", "password123")
	if err != nil {
		t.Fatal(err)
	}
	operator, err := app.MemberStore.AddMember("Operator", "operator@example.com", "operator")
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"shared", "private"} {
		_, err = app.MemberStore.UpsertManagedProfile(Profile{
			Name: profile,
			AWS:  AWSConfig{AccountEmail: profile + "@example.com", Region: "us-west-2"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.MemberStore.AssignProfileAccess("shared", operator.Email); err != nil {
		t.Fatal(err)
	}
	return app, app.newWebHandler(""), admin, operator
}

func startWebTransferRecord(t *testing.T, app *App, handler http.Handler, member Member, body string) TransferRecord {
	t.Helper()
	rec := serveWebTransfer(t, app, handler, member, http.MethodPost, "/api/transfer-record/start", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data struct {
			Record TransferRecord `json:"record"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Data.Record
}

func serveWebTransfer(t *testing.T, app *App, handler http.Handler, member Member, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	session := httptest.NewRecorder()
	if err := app.setWebSession(session, member); err != nil {
		t.Fatal(err)
	}
	req.AddCookie(session.Result().Cookies()[0])
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func findTransferLogMessage(t *testing.T, entries []LogEntry, want string) LogEntry {
	t.Helper()
	for _, entry := range entries {
		if entry.Action == webTransferLogAction && strings.Contains(entry.Message, want) {
			return entry
		}
	}
	t.Fatalf("missing transfer log %q in %+v", want, entries)
	return LogEntry{}
}

func decodeWebTransferRecords(t *testing.T, rec *httptest.ResponseRecorder) []TransferRecord {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data struct {
			Records []TransferRecord `json:"records"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Data.Records
}

type failingTransferRepository struct {
	MemberRepository
	createErr error
	updateErr error
}

func (r failingTransferRepository) CreateTransferRecord(string, TransferRecord) (TransferRecord, error) {
	return TransferRecord{}, r.createErr
}

func (r failingTransferRepository) UpdateTransferRecord(string, string, string, func(TransferRecord) (TransferRecord, error)) (TransferRecord, error) {
	return TransferRecord{}, r.updateErr
}
