# Auto Release Completion Notification Reliability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ensure every successfully completed automatic Mac release eventually sends one Enterprise WeChat completion notification and safely clears its records after scan stalls, webhook failures, cleanup failures, or service restarts.

**Architecture:** Isolate Web lifecycle and automatic-release scans into independent bounded workers. Extend the persisted release-reminder state with a notification-success timestamp so delivery and cleanup recover independently and idempotently.

**Tech Stack:** Go, `context`, existing JSON/MySQL repositories, background Job manager, Enterprise WeChat notifier, Go tests and `sqlmock`.

---

## File Map

- Modify `internal/connectmac/app_web.go`: independent bounded worker loops and scan logs.
- Modify `internal/connectmac/app_web_auto_release_test.go`: worker isolation and timeout contracts.
- Modify `internal/connectmac/member_store.go`: durable notification marker and JSON semantics.
- Modify `internal/connectmac/member_store_mysql.go`: schema migration and persistence.
- Modify `internal/connectmac/member_store_auto_release_test.go`: JSON/MySQL persistence tests.
- Modify `internal/connectmac/auto_release.go`: restart-safe notification and cleanup flow.
- Modify `internal/connectmac/auto_release_test.go`: coordinator recovery tests.

### Task 1: Reproduce The Production Failure

**Files:**
- Modify: `internal/connectmac/auto_release_test.go`

- [ ] **Step 1: Add a regression test matching `aaronjasonall-use1`**

Create a `running` reminder and a successful `aws-destroy` Job with empty Web lifecycle fields. Return AWS status with no host, no instance, and a retained but unassociated EIP.

```go
func TestAutoReleaseRecoversSuccessfulLegacyDestroyJobAndNotifies(t *testing.T) {
	// The matching Job is successful and intentionally has no LifecycleState.
	// One scan must notify, clean the exact reminder cycle, and emit released.
}
```

Assert that `Notify` receives exactly one `AutoReleaseNotificationSuccess`, `CompleteAutoRelease` is called once, `released` is emitted once, and `StartDestroy` is never called.

- [ ] **Step 2: Run the regression test**

```bash
go test ./internal/connectmac -run TestAutoReleaseRecoversSuccessfulLegacyDestroyJobAndNotifies -count=1 -v
```

Expected before the fix: FAIL because the completion chain is not guaranteed.

- [ ] **Step 3: Commit the regression test**

```bash
git add internal/connectmac/auto_release_test.go
git commit -m "test: reproduce lost auto-release completion notification"
```

### Task 2: Isolate And Bound Background Scans

**Files:**
- Modify: `internal/connectmac/app_web.go`
- Modify: `internal/connectmac/app_web_auto_release_test.go`

- [ ] **Step 1: Add failing isolation tests**

Add a channel-driven test where `WebAWSLifecycleScan` blocks until cancellation while an automatic-release tick still completes.

```go
func TestWebBackgroundWorkersDoNotLetLifecycleScanBlockAutoRelease(t *testing.T) {
	// Block lifecycle reconciliation, trigger reminder reconciliation independently,
	// and assert reminder completion before releasing the lifecycle blocker.
}
```

- [ ] **Step 2: Add timeout recovery tests**

Inject a short timeout, force one automatic-release scan to block, then trigger a second scan. Assert the first emits `auto-release.scan.timeout` and the second emits `auto-release.scan.completed`.

- [ ] **Step 3: Verify the tests fail**

```bash
go test ./internal/connectmac -run 'TestWebBackgroundWorkers|TestAutoReleaseScan' -count=1 -v
```

Expected: FAIL because both scans currently share one loop.

- [ ] **Step 4: Split the workers**

Keep `runReleaseReminderWorker` as owner and launch two child loops:

```go
func (a App) runReleaseReminderWorker(ctx context.Context, configPath string) {
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); a.runWebAWSLifecycleWorker(ctx, configPath) }()
	go func() { defer workers.Done(); a.runAutoReleaseWorker(ctx, configPath) }()
	<-ctx.Done()
	workers.Wait()
}
```

Keep daily audit pruning in the automatic-release worker.

- [ ] **Step 5: Add bounded scan execution**

```go
const (
	webAWSLifecycleScanTimeout = 45 * time.Second
	autoReleaseScanTimeout     = 45 * time.Second
)

func runBoundedBackgroundScan(parent context.Context, timeout time.Duration, scan func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return scan(ctx)
}
```

Log deadline expiry as a warning; treat service cancellation as normal shutdown; always allow the next scheduled scan.

- [ ] **Step 6: Run worker tests**

```bash
go test ./internal/connectmac -run 'TestWebBackgroundWorkers|TestAutoReleaseScan|TestWebBackgroundWorkerPrunesEvents' -count=1 -v
```

Expected: PASS, including existing audit-prune behavior.

- [ ] **Step 7: Commit**

```bash
git add internal/connectmac/app_web.go internal/connectmac/app_web_auto_release_test.go
git commit -m "fix: isolate automatic release reconciliation worker"
```

### Task 3: Persist Notification Success

**Files:**
- Modify: `internal/connectmac/member_store.go`
- Modify: `internal/connectmac/member_store_mysql.go`
- Modify: `internal/connectmac/member_store_auto_release_test.go`

- [ ] **Step 1: Add compatibility and persistence tests**

Verify legacy JSON/MySQL rows load an empty marker, while a marked reminder survives reload and remains scoped to its exact release cycle.

```go
type ReleaseReminder struct {
	// Existing fields remain unchanged.
	AutoReleaseNotifiedAt string `json:"auto_release_notified_at,omitempty"`
}
```

- [ ] **Step 2: Add cleanup-retry tests**

Test that a `notifying` reminder with a marker can complete cleanup, and a stale cycle cannot clear a newer owner or reminder.

- [ ] **Step 3: Verify store tests fail**

```bash
go test ./internal/connectmac -run 'TestMemberStoreCompleteAutoRelease|TestMySQLCompleteAutoRelease|TestAutoReleaseNotificationMarker' -count=1 -v
```

- [ ] **Step 4: Add JSON and MySQL persistence**

Add `auto_release_notified_at` to `ReleaseReminder`, MySQL schema migration, `mysqlReleaseReminderSelectColumns`, row scanners, and insert/update arguments. Existing rows must load an empty value without changing their state or timestamps.

- [ ] **Step 5: Add a cycle-checked marker operation**

Extend `AutoReleaseStore` and both repositories:

```go
MarkAutoReleaseNotified(cycle ReleaseReminderCycle, notifiedAt string) (ReleaseReminder, error)
```

Require an exact cycle, `due_notified`, automatic release enabled, and state `notifying`. Repeating the operation for the same marked cycle is idempotent.

- [ ] **Step 6: Preserve the marker across cleanup failure**

If `CompleteAutoRelease` fails, retain `notifying` and `AutoReleaseNotifiedAt`, allowing cleanup retry without another webhook call.

- [ ] **Step 7: Run store tests**

```bash
go test ./internal/connectmac -run 'TestMemberStoreCompleteAutoRelease|TestMySQLCompleteAutoRelease|TestAutoReleaseNotificationMarker' -count=1 -v
```

Expected: PASS for JSON and MySQL stores.

- [ ] **Step 8: Commit**

```bash
git add internal/connectmac/member_store.go internal/connectmac/member_store_mysql.go internal/connectmac/member_store_auto_release_test.go
git commit -m "feat: persist automatic release completion notification"
```

### Task 4: Make Completion Restart-Safe

**Files:**
- Modify: `internal/connectmac/auto_release.go`
- Modify: `internal/connectmac/auto_release_test.go`

- [ ] **Step 1: Add state-transition tests**

Cover:

```text
running -> notifying -> webhook failure -> notifying
notifying -> webhook success -> marker persisted -> cleanup
notifying + marker persisted -> cleanup without webhook
service restart + notifying -> resume
released -> no notification and no cleanup
```

- [ ] **Step 2: Add cleanup failure test**

Make `CompleteAutoRelease` fail after the marker succeeds. On the next scan, assert `Notify` is not called again and only cleanup retries.

- [ ] **Step 3: Verify focused tests fail**

```bash
go test ./internal/connectmac -run 'TestAutoRelease.*(Notify|Notification|Cleanup|Legacy|Restart)' -count=1 -v
```

- [ ] **Step 4: Split notification from cleanup**

Implement this control flow:

```go
func (c *AutoReleaseCoordinator) notifyAndFinalizeRelease(reminder ReleaseReminder, now time.Time) error {
	current := reminder
	if current.AutoReleaseNotifiedAt == "" {
		if c.Notify != nil {
			if err := c.Notify(AutoReleaseNotification{Kind: AutoReleaseNotificationSuccess, Reminder: current}); err != nil {
				return c.recordNotificationFailure(current, now, err)
			}
		}
		var err error
		current, err = c.Store.MarkAutoReleaseNotified(releaseReminderCycleFromReminder(current), now.Format(time.RFC3339))
		if err != nil { return err }
	}
	return c.finalizeNotifiedRelease(current, now)
}
```

`finalizeNotifiedRelease` calls `CompleteAutoRelease`, emits `auto-release.cleanup.retrying` on failure, and emits `auto-release.released` only after cleanup succeeds.

- [ ] **Step 5: Keep notification failures in `notifying`**

Update attempt/error timestamps without returning to AWS release retries. The one-hour AWS mutation retry window must not discard a completion notification after resources are clean.

- [ ] **Step 6: Log legacy Job observation**

When `observeRunning` finds a terminal matching Job, emit `auto-release.job.observed` with Job ID and status before read-only AWS status. Matching must not depend on Web lifecycle fields.

- [ ] **Step 7: Run automatic-release tests**

```bash
go test ./internal/connectmac -run 'AutoRelease|ReleaseReminder' -count=1 -v
```

Expected: PASS without real AWS or webhook calls.

- [ ] **Step 8: Commit**

```bash
git add internal/connectmac/auto_release.go internal/connectmac/auto_release_test.go
git commit -m "fix: guarantee automatic release completion notification"
```

### Task 5: Complete Observability And Verification

**Files:**
- Modify: `internal/connectmac/app_web.go`
- Modify: `internal/connectmac/app_web_auto_release_test.go`
- Modify: `internal/connectmac/auto_release.go`
- Modify: `internal/connectmac/auto_release_test.go`

- [ ] **Step 1: Add the event contract test**

Assert this successful sequence:

```text
auto-release.job.observed
auto-release.notification-pending
wechat.pending
wechat.sent
auto-release.released
```

Retry paths must contain `wechat.retrying` or `auto-release.cleanup.retrying` with Profile, Apple account, cycle ID, attempt, phase, and error code.

- [ ] **Step 2: Verify redaction**

Inject a fake webhook key, token, PEM path, and AWS secret into errors. Assert none appears in JSON logs or operation events.

- [ ] **Step 3: Run focused tests**

```bash
go test ./internal/connectmac -run 'AutoRelease|WebBackgroundWorker|ReleaseReminder' -count=1
```

- [ ] **Step 4: Run full verification**

```bash
gofmt -w internal/connectmac/app_web.go internal/connectmac/app_web_auto_release_test.go internal/connectmac/member_store.go internal/connectmac/member_store_mysql.go internal/connectmac/member_store_auto_release_test.go internal/connectmac/auto_release.go internal/connectmac/auto_release_test.go
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 5: Commit final test adjustments**

```bash
git add internal/connectmac/app_web.go internal/connectmac/app_web_auto_release_test.go internal/connectmac/auto_release.go internal/connectmac/auto_release_test.go
git commit -m "test: verify automatic release completion recovery"
```

### Task 6: Release And Staging Verification

**Files:**
- No source changes expected.

- [ ] **Step 1: Build locally**

```bash
go build ./cmd/cm
```

Expected: exit 0.

- [ ] **Step 2: Release only after explicit authorization**

Use the existing Homebrew, apt, local, and staging2 release process. Do not run `cm aws open`, `cm aws destroy`, or any confirmed AWS mutation for deployment verification.

- [ ] **Step 3: Verify staging startup reconciliation**

Check service health and scan logs. Confirm stale `running` or `notifying` reminders reconcile through read-only AWS status.

- [ ] **Step 4: Verify the next real automatic release**

Confirm Job success, clean AWS status, notification pending, WeChat sent, cleanup, `auto-release.released`, and retained EIP.
