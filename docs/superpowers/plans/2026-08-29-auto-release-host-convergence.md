# Automatic Release Host Convergence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent duplicate Dedicated Host release mutations after AWS accepts a release, observe `pending` convergence for up to 24 hours, warn once when stalled, and complete the existing success notification and cleanup flow when AWS becomes clean.

**Architecture:** Persist accepted Host IDs in structured destroy-job outcomes and persist convergence timestamps in each release reminder. The automatic-release coordinator recognizes current and legacy accepted-release evidence, remains in `running`, performs only read-only status reconciliation, and uses the existing notification delivery pipeline for one 24-hour warning and the final success notification.

**Tech Stack:** Go, AWS SDK v2 abstractions, `database/sql`, MySQL/file member stores, embedded HTML/JavaScript, existing JSONL and operation-event logging.

---

## File Map

- `internal/connectmac/job.go`: structured accepted-release evidence on Job outcomes and durable Job records.
- `internal/connectmac/app_aws.go`: write accepted Host IDs after successful or deferred destroy execution.
- `internal/connectmac/job_quality_test.go`: Job outcome ingestion and persistence contract tests.
- `internal/connectmac/member_store.go`: convergence timestamp fields and file-store lifecycle resets.
- `internal/connectmac/member_store_mysql.go`: MySQL schema migration, queries, scan, insert, and update bindings.
- `internal/connectmac/member_store_auto_release_test.go`: file/MySQL persistence and migration contract tests.
- `internal/connectmac/member_store_mysql_test.go`: MySQL query and argument-order regression tests.
- `internal/connectmac/auto_release.go`: convergence classification, durable transitions, 24-hour warning, and 15-minute post-timeout polling gate.
- `internal/connectmac/auto_release_test.go`: deterministic coordinator state-machine tests.
- `internal/connectmac/app_web.go`: Enterprise WeChat stalled notification and structured event severity.
- `internal/connectmac/app_web_auto_release_test.go`: notification wiring, rendering, and action-lock tests.
- `web/index.html`: convergence and stalled user-facing state text only.

### Task 1: Persist Accepted Host Evidence On Destroy Jobs

**Files:**
- Modify: `internal/connectmac/job.go`
- Modify: `internal/connectmac/app_aws.go`
- Test: `internal/connectmac/job_quality_test.go`

- [ ] **Step 1: Write failing Job outcome tests**

Add tests proving that `writeCurrentJobOutcome` round-trips accepted Host IDs and that `finishRunJob` copies them into the final Job before deleting `OutcomePath`:

```go
func TestDestroyJobPersistsAcceptedReleaseHosts(t *testing.T) {
    outcome := JobOutcome{ReleasedHosts: []string{"h-accepted"}}
    // Run the existing test child-command fixture, load the completed Job,
    // and assert completed.ReleasedHosts == []string{"h-accepted"}.
}
```

Also assert defensive copying so callers cannot mutate persisted slices.

- [ ] **Step 2: Run the focused tests and verify failure**

Run:

```bash
go test ./internal/connectmac -run 'JobOutcome|DestroyJobPersistsAcceptedReleaseHosts'
```

Expected: compile failure because `JobOutcome.ReleasedHosts` and `Job.ReleasedHosts` do not exist.

- [ ] **Step 3: Add structured fields and ingestion**

Add JSON-compatible fields:

```go
type JobOutcome struct {
    ErrorCategory JobErrorCategory `json:"error_category,omitempty"`
    ErrorCode     string           `json:"error_code,omitempty"`
    Reason        string           `json:"reason,omitempty"`
    Deferred      bool             `json:"deferred,omitempty"`
    ReleasedHosts []string         `json:"released_hosts,omitempty"`
}

type Job struct {
    // existing fields...
    ReleasedHosts []string `json:"released_hosts,omitempty"`
}
```

Copy with `append([]string(nil), outcome.ReleasedHosts...)` in both `finishRunJob` and `applyPersistedOutcome`.

In `runAWSDestroy`, write the evidence for both complete and deferred results:

```go
outcome := JobOutcome{ReleasedHosts: append([]string(nil), result.ReleasedHosts...)}
if len(result.DeferredHosts) > 0 {
    outcome.ErrorCategory = JobErrorCategoryRecoverable
    outcome.ErrorCode = "host_transition"
    outcome.Reason = fmt.Sprintf("AWS destroy deferred for %d dedicated host transition(s)", len(result.DeferredHosts))
    outcome.Deferred = true
}
_ = writeCurrentJobOutcome(outcome)
```

- [ ] **Step 4: Run focused and package tests**

Run:

```bash
go test ./internal/connectmac -run 'JobOutcome|DestroyJobPersistsAcceptedReleaseHosts|AWSServiceDestroy'
go test ./internal/connectmac
```

Expected: PASS.

- [ ] **Step 5: Commit Task 1**

```bash
git add internal/connectmac/job.go internal/connectmac/app_aws.go internal/connectmac/job_quality_test.go
git commit -m "feat: persist accepted host release evidence"
```

### Task 2: Persist Release Convergence Metadata

**Files:**
- Modify: `internal/connectmac/member_store.go`
- Modify: `internal/connectmac/member_store_mysql.go`
- Test: `internal/connectmac/member_store_auto_release_test.go`
- Test: `internal/connectmac/member_store_mysql_test.go`

- [ ] **Step 1: Write failing file and MySQL persistence tests**

Extend round-trip tests with:

```go
AutoReleaseAcceptedAt:        "2026-08-29T06:57:41Z",
AutoReleaseStalledNotifiedAt: "2026-08-30T06:57:41Z",
```

Add migration assertions for:

```sql
ALTER TABLE cm_release_reminders ADD COLUMN auto_release_accepted_at VARCHAR(64) NULL
ALTER TABLE cm_release_reminders ADD COLUMN auto_release_stalled_notified_at VARCHAR(64) NULL
```

Update expected select, insert, update, and scan argument order in both MySQL test files.

- [ ] **Step 2: Run focused tests and verify failure**

Run:

```bash
go test ./internal/connectmac -run 'ReleaseReminder|MySQL.*Reminder|AutoRelease.*Store'
```

Expected: compile or SQL expectation failures for the missing fields and columns.

- [ ] **Step 3: Add fields and MySQL bindings**

Add to `ReleaseReminder`:

```go
AutoReleaseAcceptedAt        string `json:"auto_release_accepted_at,omitempty"`
AutoReleaseStalledNotifiedAt string `json:"auto_release_stalled_notified_at,omitempty"`
```

Add both columns after `auto_release_last_attempt_at` in table creation and migration lists. Update `mysqlReleaseReminderSelectColumns`, insert/update SQL, `scanMySQLReleaseReminder`, and insert/update argument builders in the same field order.

When scheduling or explicitly re-enabling a new cycle, clear both fields together with existing attempt/error fields:

```go
current.AutoReleaseAcceptedAt = ""
current.AutoReleaseStalledNotifiedAt = ""
```

- [ ] **Step 4: Run storage tests**

Run:

```bash
go test ./internal/connectmac -run 'MemberStore|ReleaseReminder|MySQL'
```

Expected: PASS for file and SQL mock stores.

- [ ] **Step 5: Commit Task 2**

```bash
git add internal/connectmac/member_store.go internal/connectmac/member_store_mysql.go internal/connectmac/member_store_auto_release_test.go internal/connectmac/member_store_mysql_test.go
git commit -m "feat: persist auto-release convergence state"
```

### Task 3: Classify Accepted Release Convergence

**Files:**
- Modify: `internal/connectmac/auto_release.go`
- Test: `internal/connectmac/auto_release_test.go`

- [ ] **Step 1: Write failing classification tests**

Add table-driven tests for a helper with these accepted cases:

```go
job := Job{Status: JobStatusSuccess, ReleasedHosts: []string{"h-1"}}
status := AWSStatus{
    Hosts: []DedicatedHostStatus{{HostID: "h-1", State: "pending", Tags: managedTestTags()}},
    ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"},
}
```

Add the legacy accepted case: matching successful Job, no `ReleasedHosts`, matching reminder Host, zero instances, unassociated EIP, and one `pending` Host.

Reject `available`, blank/unknown states, mismatched Host IDs, multiple Hosts, remaining instances, associated EIP, failed jobs, and tag/ownership mismatch.

- [ ] **Step 2: Run classification tests and verify failure**

Run:

```bash
go test ./internal/connectmac -run 'AcceptedReleaseConvergence'
```

Expected: failure because the classifier does not exist.

- [ ] **Step 3: Implement the pure classifier**

Add constants and a side-effect-free predicate:

```go
const (
    AutoReleaseConvergenceWindow       = 24 * time.Hour
    AutoReleaseStalledStatusInterval   = 15 * time.Minute
)

func acceptedReleaseConverging(reminder ReleaseReminder, job Job, status AWSStatus) bool {
    if len(status.Instances) != 0 || len(status.Hosts) != 1 ||
        strings.TrimSpace(status.ElasticIP.AssociationID) != "" ||
        strings.TrimSpace(status.ElasticIP.InstanceID) != "" {
        return false
    }
    host := status.Hosts[0]
    if host.State != "pending" || host.HostID != reminder.HostID ||
        (job.Status != JobStatusSuccess && job.Status != JobStatusDeferred) {
        return false
    }
    if len(job.ReleasedHosts) > 0 {
        return slices.Contains(job.ReleasedHosts, host.HostID)
    }
    return true
}
```

Call `validateAutoReleaseOwnership` before the helper; do not weaken existing tag safety. Use a local loop instead of adding a dependency if the repository Go version does not support `slices`.

- [ ] **Step 4: Run classification and coordinator regression tests**

```bash
go test ./internal/connectmac -run 'AcceptedReleaseConvergence|AutoRelease'
```

Expected: PASS.

- [ ] **Step 5: Commit Task 3**

```bash
git add internal/connectmac/auto_release.go internal/connectmac/auto_release_test.go
git commit -m "feat: classify accepted host release convergence"
```

### Task 4: Make Convergence Read-Only And Durable

**Files:**
- Modify: `internal/connectmac/auto_release.go`
- Test: `internal/connectmac/auto_release_test.go`

- [ ] **Step 1: Write failing coordinator transition tests**

Cover this sequence with a fake store, jobs, status reader, and `StartDestroy` counter:

1. A successful Job plus matching `pending` Host persists `AutoReleaseAcceptedAt`, remains `running`, clears `AutoReleaseLastError`, and emits `convergence-waiting`.
2. Repeated scans before 24 hours call `Status` but never `StartDestroy` and send no failure notification.
3. Reconstructing the coordinator from persisted reminder data still never calls `StartDestroy`.
4. A clean later status enters existing `notifying`/success cleanup exactly once.
5. A genuine recoverable mutation failure still uses five-minute retries for one hour.

- [ ] **Step 2: Run transition tests and verify failure**

```bash
go test ./internal/connectmac -run 'AutoRelease.*Convergence|AutoRelease.*MutationRetry'
```

Expected: current coordinator marks `retrying` and creates another Job.

- [ ] **Step 3: Implement convergence observation**

In `observeRunning`, check clean completion first, then accepted convergence before `recordAttemptFailure`. Persist the first acceptance atomically:

```go
func (c *AutoReleaseCoordinator) markConvergenceWaiting(reminder ReleaseReminder, job Job, now time.Time) error {
    updated, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
        if !sameAutoReleaseClaim(current, reminder) || current.AutoReleaseState != ReleaseReminderAutoReleaseStateRunning {
            return current, errAutoReleaseCycleChanged
        }
        if current.AutoReleaseAcceptedAt == "" {
            current.AutoReleaseAcceptedAt = now.Format(time.RFC3339)
        }
        current.AutoReleaseLastError = ""
        return current, nil
    })
    if err == nil && reminder.AutoReleaseAcceptedAt == "" {
        c.emitWithJob("convergence-waiting", updated, job, "host release accepted; waiting for AWS convergence")
    }
    return err
}
```

At the beginning of `observeRunning`, when `AutoReleaseAcceptedAt` is set, bypass active/latest Job mutation logic and call a dedicated read-only `observeConvergence` method. AWS status errors remain recoverable but must not clear acceptance or switch to a mutation-retry path.

- [ ] **Step 4: Run coordinator tests**

```bash
go test ./internal/connectmac -run 'AutoRelease'
```

Expected: PASS, including the old one-hour retry tests.

- [ ] **Step 5: Commit Task 4**

```bash
git add internal/connectmac/auto_release.go internal/connectmac/auto_release_test.go
git commit -m "fix: observe accepted host releases without remutation"
```

### Task 5: Add One 24-Hour Stalled Warning And Polling Gate

**Files:**
- Modify: `internal/connectmac/auto_release.go`
- Modify: `internal/connectmac/app_web.go`
- Test: `internal/connectmac/auto_release_test.go`
- Test: `internal/connectmac/app_web_auto_release_test.go`

- [ ] **Step 1: Write failing timing and notification tests**

Use fixed times to assert:

- at accepted time plus 23h59m, no warning;
- at plus 24h, one `AutoReleaseNotificationStalled` attempt;
- successful delivery persists `AutoReleaseStalledNotifiedAt`;
- later scans do not resend;
- failed delivery leaves the timestamp blank and retries;
- after 24 hours, scans before `AutoReleaseLastAttemptAt + 15m` do not call AWS Status;
- clean status after the warning still sends normal success and cleans records.

- [ ] **Step 2: Run tests and verify failure**

```bash
go test ./internal/connectmac -run 'AutoRelease.*Stalled|Wechat.*Stalled'
```

Expected: missing notification kind and repeated per-scan checks.

- [ ] **Step 3: Implement timing and notification state**

Add:

```go
const AutoReleaseNotificationStalled AutoReleaseNotificationKind = "stalled"
```

Before a post-24-hour status call, parse `AutoReleaseLastAttemptAt` and return early until 15 minutes pass. Persist the new attempt timestamp before the read. At the 24-hour boundary, emit `convergence-stalled`, deliver the warning, then atomically set `AutoReleaseStalledNotifiedAt` only after successful delivery.

Map the notification in `newAutoReleaseCoordinator`:

```go
case AutoReleaseNotificationStalled:
    event = "auto-release-stalled"
    description = "Host 释放状态超过 24 小时仍未完成，系统将继续检查，不会重复提交释放。"
```

Use the existing WeChat delivery context with cycle ID and attempt metadata.

- [ ] **Step 4: Run coordinator and web notification tests**

```bash
go test ./internal/connectmac -run 'AutoRelease|Wechat'
```

Expected: PASS with one successful stalled notification and no duplicate events.

- [ ] **Step 5: Commit Task 5**

```bash
git add internal/connectmac/auto_release.go internal/connectmac/app_web.go internal/connectmac/auto_release_test.go internal/connectmac/app_web_auto_release_test.go
git commit -m "feat: warn once for stalled host convergence"
```

### Task 6: Refine Web Status Without Layout Changes

**Files:**
- Modify: `web/index.html`
- Test: `internal/connectmac/app_web_auto_release_test.go`

- [ ] **Step 1: Write failing rendered-contract tests**

Assert that `autoReleaseStateText` checks `auto_release_accepted_at` before generic running/retrying text and contains both messages:

```js
if (reminder.auto_release_accepted_at) {
  if (reminder.auto_release_stalled_notified_at) return "AWS Host 释放时间较长，系统仍在检查";
  return "正在释放，等待 AWS 完成";
}
```

Assert accepted convergence does not expose `auto_release_last_error`, remains active in `autoReleaseActive`, and therefore keeps open/connect/VNC/transfer actions disabled.

- [ ] **Step 2: Run web contract tests and verify failure**

```bash
go test ./internal/connectmac -run 'Web.*AutoRelease|AutoRelease.*HTML|Workbench'
```

Expected: missing convergence-specific text.

- [ ] **Step 3: Implement the minimal text branch**

Update only `autoReleaseStateText` and `showError`. Keep the existing Bootstrap layout, controls, breakpoints, and workbench code unchanged. Suppress the old error while `auto_release_accepted_at` is non-empty.

- [ ] **Step 4: Run web and full verification**

```bash
gofmt -w internal/connectmac/job.go internal/connectmac/app_aws.go internal/connectmac/member_store.go internal/connectmac/member_store_mysql.go internal/connectmac/auto_release.go internal/connectmac/app_web.go internal/connectmac/*_test.go
go test ./internal/connectmac -run 'AutoRelease|Destroy|JobOutcome|ReleaseReminder|Wechat|Workbench'
go test ./...
go test -race ./...
go vet ./...
```

Expected: all commands PASS.

- [ ] **Step 5: Commit Task 6**

```bash
git add web/index.html internal/connectmac/app_web_auto_release_test.go
git commit -m "fix: clarify accepted host release status"
```

### Task 7: Staging2 Compatibility Verification

**Files:**
- No source changes expected.
- Inspect: staging2 service, Job records, release reminder, JSON logs, and operation events.

- [ ] **Step 1: Verify repository state before rollout**

```bash
git status --short
git log --oneline -8
```

Expected: only known user-owned untracked files remain; all implementation changes are committed.

- [ ] **Step 2: Deploy through the repository's existing release workflow**

Use the established version, Homebrew, apt, local, and staging2 scripts from the repository. Do not invoke `cm aws destroy`, `cm aws open`, or any other AWS mutation during deployment verification.

- [ ] **Step 3: Verify legacy recovery read-only**

On staging2, inspect `aaronjasonall-use1` and assert:

- no new destroy Job appears after deployment;
- reminder has `auto_release_accepted_at`;
- state/UI remains `releasing` while Host is `pending`;
- EC2 is absent and EIP allocation remains unassociated and retained;
- logs contain one `auto-release.convergence-waiting`, not repeated failure events.

- [ ] **Step 4: Verify eventual completion or continued observation**

If AWS is clean, verify one success WeChat event followed by owner/reminder cleanup. If the Host remains `pending`, report the accepted timestamp and next 24-hour warning boundary without forcing a release.

- [ ] **Step 5: Record rollout result**

Summarize deployed version, tests, staging2 state, Job count before/after, EIP retention, convergence event, and whether final AWS cleanup has occurred.
