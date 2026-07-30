# ConnectMac Observability And Audit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make ConnectMac runtime logs and audit events complete, attributable, correlated, retained for 90 days, and useful for direct production analysis.

**Architecture:** Introduce a small observability layer that carries request, actor, job, source, phase, and error context into JSONL and audit records. Keep MySQL audit events append-only and query them directly with cursor pagination, while preserving a bounded JSON fallback. Correlate Web requests, AWS jobs, lifecycle completion, Enterprise WeChat delivery, local-agent activity, and transfer phases without logging secrets.

**Tech Stack:** Go, `net/http`, MySQL, JSONL, existing ConnectMac repositories and job manager, embedded HTML/JavaScript UI, Go tests with `httptest` and `sqlmock`.

---

## File Structure

- Create `internal/connectmac/observability.go`: request context, actor context, stable error classification, JSONL fallback behavior, and shared audit construction.
- Create `internal/connectmac/app_web_observability.go`: Web middleware and helpers for request IDs, actor attribution, authorization rejection, and audited mutations.
- Create `internal/connectmac/member_store_events.go`: file-store event retention, pagination, cursor handling, and event filtering.
- Create `internal/connectmac/member_store_mysql_events.go`: direct MySQL event insert, query, pagination, and pruning.
- Modify `internal/connectmac/logs.go`: additive structured fields and stronger redaction.
- Modify `internal/connectmac/member_store.go`: additive audit fields and repository methods.
- Modify `internal/connectmac/member_store_mysql.go`: additive migrations and removal of whole-store event rewriting.
- Modify `internal/connectmac/app_web.go`: middleware installation, audited handlers, AWS request/job correlation, noise suppression, and daily pruning.
- Modify `internal/connectmac/app_web_auth.go`: authentication and security audit coverage.
- Modify `internal/connectmac/app_web_sync.go`: correlated sync events.
- Modify `internal/connectmac/app_web_terminal.go`: terminal open/close lifecycle.
- Modify `internal/connectmac/app_web_transfer_records.go`: transfer phase persistence and deletion audit.
- Modify `internal/connectmac/job.go`: request and actor metadata persisted with jobs.
- Modify `internal/connectmac/web_aws_lifecycle.go`: terminal AWS lifecycle audit events.
- Modify `internal/connectmac/wechat.go`: delivery result metadata without secrets.
- Modify `internal/connectmac/local_transfer_jobs.go`: mapped progress phases.
- Modify `internal/connectmac/app_local_agent.go`: request correlation and local intent/result logging.
- Modify `web/index.html`: finalizing transfer label and correlated local-agent requests.
- Add focused tests in new and existing `*_test.go` files listed by each task.

### Task 1: Core Observability Context And Safe Runtime Logging

**Files:**
- Create: `internal/connectmac/observability.go`
- Modify: `internal/connectmac/logs.go`
- Test: `internal/connectmac/observability_test.go`
- Test: `internal/connectmac/logs_test.go`

- [ ] **Step 1: Write failing tests for structured context and error classification**

Add table-driven tests that expect:

```go
func TestClassifyOperationalError(t *testing.T) {
	tests := []struct {
		err   error
		level string
		code  string
		skip  bool
	}{
		{context.Canceled, "debug", "request_canceled", true},
		{context.DeadlineExceeded, "warn", "request_timeout", false},
		{errors.New("failed to get shared config profile, cm-user"), "error", "aws_profile_missing", false},
		{errors.New("AccessDenied: not authorized"), "error", "aws_permission_denied", false},
		{errors.New("InsufficientHostCapacity"), "error", "aws_capacity_unavailable", false},
	}
	// Assert level, code, and skip for every row.
}
```

Add a log serialization test that verifies request/job/actor/source/phase/error
fields are emitted and Bearer tokens, cookies, webhook keys, PEM blocks, and AWS
credential assignments are absent from the raw file.

- [ ] **Step 2: Run focused tests and verify failure**

Run:

```bash
go test ./internal/connectmac -run 'TestClassifyOperationalError|TestLogManagerStructuredRedaction' -count=1
```

Expected: FAIL because the classifier and new fields do not exist.

- [ ] **Step 3: Add request, actor, and error context types**

Implement these core shapes in `observability.go`:

```go
type AuditActor struct {
	MemberID    string
	MemberEmail string
	MemberName  string
}

type OperationContext struct {
	RequestID string
	JobID    string
	Source   string
	Route    string
	Method   string
	Actor    AuditActor
}

type ClassifiedError struct {
	Level string
	Code  string
	Skip  bool
}

func classifyOperationalError(err error) ClassifiedError
func withOperationContext(ctx context.Context, value OperationContext) context.Context
func operationContextFrom(ctx context.Context) OperationContext
func newRequestID(now time.Time, random io.Reader) (string, error)
```

Use stable lower-case error codes from the approved design. Do not inspect or
serialize secret values.

- [ ] **Step 4: Extend and harden `LogEntry`**

Add:

```go
RequestID  string `json:"request_id,omitempty"`
JobID      string `json:"job_id,omitempty"`
Operation  string `json:"operation,omitempty"`
Source     string `json:"source,omitempty"`
Phase      string `json:"phase,omitempty"`
ErrorCode  string `json:"error_code,omitempty"`
Attempt    int    `json:"attempt,omitempty"`
HTTPStatus int    `json:"http_status,omitempty"`
```

Replace keyword substitution with structured regex redaction for:

- `Authorization: Bearer ...`;
- Cookie and Set-Cookie values;
- `password=`, `token=`, `secret=`, and `session=`;
- `key=` URL query values;
- AWS access key and secret key assignments;
- PEM blocks.

Add `App.writeRuntimeLog(entry LogEntry)` that writes to `LogManager` and emits
one concise sanitized line to `a.Err` when JSONL writing fails.

- [ ] **Step 5: Run focused and package tests**

Run:

```bash
gofmt -w internal/connectmac/observability.go internal/connectmac/observability_test.go internal/connectmac/logs.go internal/connectmac/logs_test.go
go test ./internal/connectmac -run 'TestClassifyOperationalError|TestLogManager' -count=1
go test ./internal/connectmac -count=1
```

Expected: PASS.

### Task 2: Append-Only Audit Storage, Pagination, And Retention

**Files:**
- Create: `internal/connectmac/member_store_events.go`
- Create: `internal/connectmac/member_store_mysql_events.go`
- Modify: `internal/connectmac/member_store.go`
- Modify: `internal/connectmac/member_store_mysql.go`
- Test: `internal/connectmac/member_store_events_test.go`
- Test: `internal/connectmac/member_store_mysql_test.go`

- [ ] **Step 1: Write failing file-store retention and pagination tests**

Create tests that seed events before and after a fixed 90-day cutoff and more
than 10,000 recent events. Assert:

```go
page, err := store.QueryEvents(EventQuery{Limit: 50})
// Newest event first, NextCursor populated, expired rows absent.

next, err := store.QueryEvents(EventQuery{Limit: 50, Cursor: page.NextCursor})
// No duplicate IDs across pages.
```

Assert the file store retains the newest 10,000 non-expired events.

- [ ] **Step 2: Write failing MySQL tests**

Use `sqlmock` to require:

```sql
SELECT ... FROM cm_events
WHERE created_at < ? OR (created_at = ? AND id < ?)
ORDER BY created_at DESC, id DESC
LIMIT ?
```

Also require:

```sql
DELETE FROM cm_events WHERE created_at < ?
```

Verify ordinary member/profile saves do not execute `DELETE FROM cm_events` and
do not reinsert the loaded event list.

- [ ] **Step 3: Extend the repository contract**

Add:

```go
type EventQuery struct {
	AppleEmail string
	Profile    string
	ActorEmail string
	Limit      int
	Cursor     string
	IncludeSystem bool
}

type EventPage struct {
	Events     []OperationEvent `json:"events"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

QueryEvents(EventQuery) (EventPage, error)
PruneEvents(before time.Time) (int64, error)
```

Keep `RecentEvents` as a compatibility wrapper around `QueryEvents`.

- [ ] **Step 4: Add event fields and additive migrations**

Extend `OperationEvent` with:

```go
RequestID         string `json:"request_id,omitempty"`
JobID             string `json:"job_id,omitempty"`
Source            string `json:"source,omitempty"`
Phase             string `json:"phase,omitempty"`
TargetMemberEmail string `json:"target_member_email,omitempty"`
ErrorCode         string `json:"error_code,omitempty"`
DurationMS        int64  `json:"duration_ms,omitempty"`
```

Add idempotent `ALTER TABLE` migrations and indexes for these columns. Update
the insert statement and scan list in one place so field order cannot diverge.

- [ ] **Step 5: Remove MySQL whole-store event truncation**

Remove event selection from `MySQLMemberStore.Load` and event deletion/reinsert
from `saveUnlocked`. Implement direct insert, direct paginated query, and direct
prune in `member_store_mysql_events.go`.

For the JSON store, prune by age and cap at 10,000 during append/save.

- [ ] **Step 6: Run storage tests**

Run:

```bash
gofmt -w internal/connectmac/member_store_events.go internal/connectmac/member_store_mysql_events.go internal/connectmac/member_store.go internal/connectmac/member_store_mysql.go internal/connectmac/member_store_events_test.go internal/connectmac/member_store_mysql_test.go
go test ./internal/connectmac -run 'Test.*Event|TestMySQL.*Event|TestMemberStore' -count=1
```

Expected: PASS and no test expectation for `DELETE FROM cm_events` during an
unrelated save.

### Task 3: Web Middleware And Security/Admin Audit Coverage

**Files:**
- Create: `internal/connectmac/app_web_observability.go`
- Modify: `internal/connectmac/app_web.go`
- Modify: `internal/connectmac/app_web_auth.go`
- Test: `internal/connectmac/app_web_observability_test.go`
- Test: `internal/connectmac/app_test.go`

- [ ] **Step 1: Write failing middleware and audit contract tests**

Add a table covering:

- admin setup;
- login success and failure;
- logout;
- own password change;
- administrator password reset;
- own/admin token generate, regenerate, and delete;
- administrator email change;
- settings change;
- member add/update/enable/disable;
- assignment and Profile access change;
- managed Profile save/status/delete/access.

For each mutation assert exactly one terminal audit event with:

```go
event.RequestID != ""
event.Source == "web"
event.MemberEmail == expectedActor
event.TargetMemberEmail == expectedTarget
event.Status == "success" || event.Status == "failed"
```

Assert raw event JSON contains none of the submitted password, token, auth
secret, PEM path, or full Profile YAML.

- [ ] **Step 2: Add request middleware**

Implement:

```go
func (a App) withWebObservability(next http.Handler) http.Handler
func (a App) operationContextForRequest(r *http.Request) OperationContext
func (a App) recordAudit(ctx context.Context, event OperationEvent) error
func (a App) recordAuthorizationDenied(r *http.Request, reason string)
```

Generate a server request ID, return it in `X-Request-ID`, attach actor data
after authentication, recover panics into a sanitized 500 response, and record
authorization rejection for protected mutations.

- [ ] **Step 3: Install middleware without changing route compatibility**

Wrap the completed mux once in `runWeb`. Keep static assets and health behavior
unchanged. Do not emit success audit events for ordinary GET polling.

- [ ] **Step 4: Audit authentication and security handlers**

Record stable actions:

```text
auth.setup.succeeded|failed
auth.login.succeeded|failed
auth.logout.succeeded
auth.email.changed
auth.password.changed
member.password.changed
auth.token.generated|regenerated|deleted
```

Login failures include normalized submitted identity, sanitized client IP and
user agent, but never password or challenge answer.

- [ ] **Step 5: Audit member, settings, and Profile mutations**

Use actions:

```text
settings.updated
member.created
member.updated
member.enabled
member.disabled
member.assignment.granted|removed
profile.access.replaced|granted|removed
profile.created|updated|enabled|disabled|deleted
```

Messages contain changed field names and target identities only. Keep existing
response bodies and status codes.

- [ ] **Step 6: Make audit failures visible**

For security-critical repository transactions, return a 500 if the mutation and
event cannot both commit. For existing non-atomic handlers, write a runtime
`audit.persistence.failed` entry to stderr/JSONL and return a clear 500 instead
of silently ignoring `RecordEvent`.

- [ ] **Step 7: Run Web audit tests**

Run:

```bash
gofmt -w internal/connectmac/app_web_observability.go internal/connectmac/app_web.go internal/connectmac/app_web_auth.go internal/connectmac/app_web_observability_test.go internal/connectmac/app_test.go
go test ./internal/connectmac -run 'TestWeb.*Audit|TestWebAuth|TestWebMember|TestWebManagedProfile' -count=1
```

Expected: PASS.

### Task 4: AWS Request, Job, And Completion Correlation

**Files:**
- Modify: `internal/connectmac/job.go`
- Modify: `internal/connectmac/app_web.go`
- Modify: `internal/connectmac/web_aws_lifecycle.go`
- Modify: `internal/connectmac/app_aws.go`
- Test: `internal/connectmac/job_quality_test.go`
- Test: `internal/connectmac/app_web_job_cleanup_test.go`
- Test: `internal/connectmac/web_aws_lifecycle_test.go`

- [ ] **Step 1: Write failing job compatibility and lifecycle audit tests**

Expect additive job JSON fields:

```go
RequestID       string `json:"request_id,omitempty"`
Source          string `json:"source,omitempty"`
ActorMemberID   string `json:"actor_member_id,omitempty"`
ActorEmail      string `json:"actor_email,omitempty"`
ActorName       string `json:"actor_name,omitempty"`
```

Verify old job files still load. Verify one confirmed Web open shares request ID,
job ID, and actor across `requested`, `started`, and `ready`. Verify destroy
shares them across `requested`, `started`, and `stopped`. Verify failed or
interrupted jobs produce one `failed` event.

- [ ] **Step 2: Persist correlation data on job creation**

Populate job request/source/actor fields in `startWebAWSJob` and
`startAWSJobForResolvedProfile`. Preserve them through runner updates and
restarts.

- [ ] **Step 3: Replace ambiguous Web events**

Record preview as:

```text
aws.open.previewed
aws.release.previewed
```

Record confirmation and start as:

```text
aws.open.requested
aws.open.started
aws.release.requested
aws.release.started
```

Do not describe a successfully queued job as a completed AWS operation.

- [ ] **Step 4: Record terminal lifecycle events**

In `reconcileWebAWSLifecycleJobLocked`, record exactly one:

```text
aws.open.ready
aws.release.stopped
aws.open.failed
aws.release.failed
```

Include duration, request ID, job ID, actor, Profile, Apple email, error code,
and `eip_retained=true` for successful release.

- [ ] **Step 5: Suppress cancellation noise and classify AWS failures**

In the status handler:

```go
classified := classifyOperationalError(err)
if !classified.Skip {
	a.writeRuntimeLog(LogEntry{Level: classified.Level, ErrorCode: classified.Code, ...})
}
```

Missing server-local config returns an empty local config and merges managed
Profiles. Existing invalid config remains an error.

- [ ] **Step 6: Add best-effort local CLI runtime logs**

Record local JSONL start/result events for `cm aws open` and `cm aws destroy`.
Do not add remote reporting or change confirmation rules in this task.

- [ ] **Step 7: Run AWS lifecycle tests**

Run:

```bash
gofmt -w internal/connectmac/job.go internal/connectmac/app_web.go internal/connectmac/web_aws_lifecycle.go internal/connectmac/app_aws.go internal/connectmac/job_quality_test.go internal/connectmac/app_web_job_cleanup_test.go internal/connectmac/web_aws_lifecycle_test.go
go test ./internal/connectmac -run 'TestJob|TestWebAWS|TestAWS' -count=1
```

Expected: PASS with idempotent terminal event creation.

### Task 5: Cleanup Noise, Event Pruning, And WeChat Delivery Observability

**Files:**
- Modify: `internal/connectmac/app_web_login_cleanup.go`
- Modify: `internal/connectmac/app_web.go`
- Modify: `internal/connectmac/member_store.go`
- Modify: `internal/connectmac/member_store_mysql.go`
- Modify: `internal/connectmac/wechat.go`
- Modify: `internal/connectmac/web_aws_lifecycle.go`
- Test: `internal/connectmac/app_web_auto_release_test.go`
- Test: `internal/connectmac/wechat_test.go`
- Test: `internal/connectmac/web_aws_lifecycle_test.go`

- [ ] **Step 1: Write failing noise and cleanup idempotency tests**

Assert:

- missing default local `config.yaml` returns no warning;
- unreadable or invalid existing config logs an error;
- repeated stopped-status polling after convergence inserts no event;
- automatic cleanup uses `system.cleanup.completed`;
- manual cleanup includes administrator actor and uses
  `profile.cleanup.completed`.

- [ ] **Step 2: Fix missing-config behavior**

In `cleanupDefaultLocalConfigProfiles`, treat `os.IsNotExist` as a clean no-op.
Keep permission and parse failures observable.

- [ ] **Step 3: Make cleanup event policy explicit**

Keep repository-level changed detection. Pass actor context into manual cleanup,
use a system source for auto status cleanup, and exclude system cleanup events
from default event queries unless `IncludeSystem` is true.

- [ ] **Step 4: Schedule daily event pruning**

Add a daily maintenance ticker to the existing Web background worker. Call:

```go
deleted, err := a.MemberStore.PruneEvents(now.UTC().Add(-90 * 24 * time.Hour))
```

Log one maintenance summary only when rows were deleted or pruning failed.

- [ ] **Step 5: Extend WeChat result metadata**

Add:

```go
type WechatNotifyResult struct {
	Skipped   bool
	Message   string
	HTTPStatus int
	ErrCode   int
}
```

Record pending before send, sent after success, retrying when a persisted
lifecycle claim is cleared, and failed when retry policy is exhausted. Include
attempt, request ID, job ID, Profile, HTTP status, and error code. Never include
the webhook URL or raw response body.

- [ ] **Step 6: Verify notification restart idempotency**

Extend lifecycle tests to stop after a failed send, reload the job from disk,
retry successfully, and assert one `sent` event and no duplicate webhook call
after `LifecycleNotifiedAt` is persisted.

- [ ] **Step 7: Run cleanup and notification tests**

Run:

```bash
gofmt -w internal/connectmac/app_web_login_cleanup.go internal/connectmac/app_web.go internal/connectmac/member_store.go internal/connectmac/member_store_mysql.go internal/connectmac/wechat.go internal/connectmac/web_aws_lifecycle.go internal/connectmac/app_web_auto_release_test.go internal/connectmac/wechat_test.go internal/connectmac/web_aws_lifecycle_test.go
go test ./internal/connectmac -run 'Test.*Cleanup|TestWechat|TestWebAWSLifecycleNotification|Test.*Prune' -count=1
```

Expected: PASS.

### Task 6: Transfer Phases And Finalization UX

**Files:**
- Modify: `internal/connectmac/member_store.go`
- Modify: `internal/connectmac/member_store_mysql.go`
- Modify: `internal/connectmac/local_transfer_jobs.go`
- Modify: `internal/connectmac/app_local_agent.go`
- Modify: `internal/connectmac/app_web_transfer_records.go`
- Modify: `web/index.html`
- Test: `internal/connectmac/local_transfer_jobs_test.go`
- Test: `internal/connectmac/member_store_transfer_test.go`
- Test: `internal/connectmac/app_web_transfer_records_test.go`

- [ ] **Step 1: Write failing transfer mapping tests**

Test:

```go
raw 0   -> preparing, 0
raw 1   -> transferring, 1
raw 50  -> transferring, 48
raw 99  -> transferring, 95
raw 100 while process active -> finalizing, 99
successful process exit -> succeeded, 100
failed process exit -> failed, previous percentage
```

Assert milestone events do not jump directly from 0 to application 99 when the
first rsync output contains a raw completion line.

- [ ] **Step 2: Add transfer phase**

Add `Phase` to `TransferRecord`, `LocalTransferJob`, and `LocalTransferEvent`.
Add an additive MySQL migration for `cm_transfer_records.phase`. Validate
allowed phases independently from terminal status.

- [ ] **Step 3: Map rsync progress into application progress**

Replace direct percentage reuse with:

```go
func mapRsyncProgress(raw int, processDone bool) (phase string, displayed int)
```

Map active data transfer to 1-95, raw completion to 99/finalizing, and set 100
only after a successful process exit.

- [ ] **Step 4: Persist and log phase transitions**

Include phase in local JSONL and server transfer records. Avoid duplicate
milestone writes for the same phase and percentage. Preserve the last progress
on failure or interruption.

- [ ] **Step 5: Update the embedded Web UI**

Render:

```text
preparing    -> 上传准备中 / 下载准备中
transferring -> 上传中 / 下载中
finalizing   -> 正在完成校验
succeeded    -> 上传完成 / 下载完成
```

Keep the progress bar fixed at 99% while finalizing. Do not close the result
panel until the terminal transfer record has been persisted.

- [ ] **Step 6: Run transfer and Web tests**

Run:

```bash
gofmt -w internal/connectmac/member_store.go internal/connectmac/member_store_mysql.go internal/connectmac/local_transfer_jobs.go internal/connectmac/app_local_agent.go internal/connectmac/app_web_transfer_records.go internal/connectmac/local_transfer_jobs_test.go internal/connectmac/member_store_transfer_test.go internal/connectmac/app_web_transfer_records_test.go
go test ./internal/connectmac -run 'Test.*Transfer|Test.*RsyncProgress' -count=1
```

Expected: PASS.

### Task 7: Connection, VNC, Terminal, Sync, And CLI Coverage

**Files:**
- Modify: `internal/connectmac/app_connect.go`
- Modify: `internal/connectmac/app_sync.go`
- Modify: `internal/connectmac/app_local_agent.go`
- Modify: `internal/connectmac/app_web_terminal.go`
- Modify: `internal/connectmac/app_web_sync.go`
- Modify: `web/index.html`
- Test: `internal/connectmac/app_test.go`
- Test: `internal/connectmac/app_web_terminal_test.go`

- [ ] **Step 1: Write failing lifecycle coverage tests**

Assert local JSONL events for:

- SSH attempted/succeeded/failed without command or terminal output;
- tunnel started/reused/replaced/stopped/failed;
- known-host checked/fixed/forgotten;
- VNC requested/launched/failed;
- sync push/pull started/succeeded/failed;
- terminal opened/closed with duration and close reason.

Assert request IDs from browser local-agent requests appear in local logs.

- [ ] **Step 2: Propagate request IDs to the local agent**

The Web UI generates a request ID for local-only actions and sends it in the
local-agent request body/header. The local agent validates length and character
set, then attaches it to runtime logs. It never trusts browser-provided actor
identity; server-side intent events remain authoritative for actor attribution.

- [ ] **Step 3: Add local CLI runtime events**

Wrap the existing command outcomes without changing behavior. Log command name,
Profile, source `cli`, result, duration, tunnel action, PID, and local ports.
Do not log SSH command arguments, terminal content, local file contents, or
credentials.

- [ ] **Step 4: Complete terminal close logging**

After WebSocket proxy completion, record one close event with:

```text
phase=closed
duration_ms=<elapsed>
outcome=success|failure
error_code=<classified code>
```

Treat normal browser disconnect and `exit` as successful closure.

- [ ] **Step 5: Add server intent events for local-only operations**

Record member intent before Connect, VNC, and Transfer local-agent calls. Keep
execution results local except for existing transfer status callbacks. Do not
pretend the server executed local SSH or VNC.

- [ ] **Step 6: Run connection and sync tests**

Run:

```bash
gofmt -w internal/connectmac/app_connect.go internal/connectmac/app_sync.go internal/connectmac/app_local_agent.go internal/connectmac/app_web_terminal.go internal/connectmac/app_web_sync.go internal/connectmac/app_test.go internal/connectmac/app_web_terminal_test.go
go test ./internal/connectmac -run 'Test.*Tunnel|Test.*VNC|Test.*Terminal|Test.*Sync|Test.*HostKey' -count=1
```

Expected: PASS.

### Task 8: Event API, UI Compatibility, Documentation, And Full Verification

**Files:**
- Modify: `internal/connectmac/app_web.go`
- Modify: `web/index.html`
- Modify: `README.md`
- Test: `internal/connectmac/app_test.go`
- Test: `internal/connectmac/member_store_mysql_test.go`

- [ ] **Step 1: Write failing event API pagination tests**

Verify `/api/events`:

- defaults to 50 newest non-system events;
- accepts `limit`, `cursor`, `profile`, and `apple_email`;
- rejects invalid cursor and limits above 200;
- returns `next_cursor`;
- filters non-admin members to assigned Profiles;
- preserves the existing `data.events` field.

- [ ] **Step 2: Implement API query parsing and UI pagination**

Parse the bounded query into `EventQuery`. Keep the current first-page display,
add a load-more action only when `next_cursor` is non-empty, and label system
events separately when an administrator elects to include them.

- [ ] **Step 3: Document log locations, fields, retention, and analysis**

Update README with:

```text
Local JSONL: ~/.connectmac/logs/cm-YYYY-MM-DD.log, 30 days
Server JSONL: /var/lib/connectmac/.connectmac/logs, 30 days
MySQL audit: cm_events, 90 days
Job metadata/logs: /var/lib/connectmac/.connectmac/jobs
Service log: journalctl -u connectmac
```

Document event pagination and state that secrets and terminal contents are not
logged.

- [ ] **Step 4: Run formatting, static checks, and all tests**

Run:

```bash
gofmt -w internal/connectmac
git diff --check
go test ./...
```

Expected: PASS with no formatting errors.

- [ ] **Step 5: Run local smoke verification**

Start the Web server against a temporary file store, then verify:

- login success and failure create sanitized events;
- member and Profile mutations include actor and request ID;
- missing local config creates no warning;
- event pagination has no duplicates;
- mocked AWS completion records terminal state;
- transfer finalizing displays 99% and succeeds at 100%.

- [ ] **Step 6: Review migration and rollback safety**

Inspect the final diff and verify:

- all schema changes are additive;
- no migration deletes `cm_events`;
- existing event/job JSON remains readable;
- no EIP release behavior changed;
- no secret is introduced into logs or events.

- [ ] **Step 7: Commit implementation**

Stage only tracked implementation, tests, and documentation:

```bash
git add internal/connectmac web/index.html README.md docs/superpowers/plans/2026-07-30-observability-audit-implementation.md
git commit -m "feat: complete observability and audit coverage"
```

Do not add `.mcp.json`, `CLAUDE.md`, or `web-ui/`.
