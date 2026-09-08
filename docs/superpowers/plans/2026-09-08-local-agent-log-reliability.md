# Local Agent Log Reliability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce bounded, privacy-safe local diagnostics in which every transfer, terminal, and VNC operation has an actionable, correlated final result.

**Architecture:** Keep JSONL as the authoritative log and retain the existing in-memory transfer manager. Stop mirroring rsync transcripts to launchd stdout, reconcile orphaned transfer starts from JSONL when the Agent boots, centralize local error classification, rotate structured logs by size, and make raw-log export explicit. Existing APIs and action names remain compatible; new fields and CLI flags are additive.

**Tech Stack:** Go standard library, Gorilla WebSocket, existing `Runner`, `LogManager`, `LocalTransferJobManager`, and table-driven Go tests.

---

### Task 1: Stop rsync transcript amplification

**Files:**
- Modify: `internal/connectmac/runner.go`
- Create: `internal/connectmac/runner_test.go`

- [ ] **Step 1: Add a failing writer-routing test**

Add a small command-runner seam that accepts stdout/stderr destinations and verify progress callbacks receive both streams while the Agent mode destinations remain `io.Discard`.

```go
func TestRunRsyncCommandProgressToDoesNotRequireProcessStdout(t *testing.T) {
	var chunks []string
	err := runRsyncCommandProgressTo(
		context.Background(), "sh", []string{"-c", "printf out; printf err >&2"},
		io.Discard, io.Discard, func(chunk string) { chunks = append(chunks, chunk) },
	)
	if err != nil || !strings.Contains(strings.Join(chunks, ""), "out") || !strings.Contains(strings.Join(chunks, ""), "err") {
		t.Fatalf("err=%v chunks=%q", err, chunks)
	}
}
```

- [ ] **Step 2: Run the focused test**

Run: `go test ./internal/connectmac -run TestRunRsyncCommandProgressToDoesNotRequireProcessStdout`

Expected: FAIL because `runRsyncCommandProgressTo` does not exist.

- [ ] **Step 3: Route command output only through the callback**

Implement:

```go
func runRsyncCommandProgressTo(ctx context.Context, path string, args []string, stdout, stderr io.Writer, onOutput func(string)) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = nil
	cmd.Stdout = io.MultiWriter(stdout, outputCallbackWriter{onOutput: onOutput})
	cmd.Stderr = io.MultiWriter(stderr, outputCallbackWriter{onOutput: onOutput})
	return cmd.Run()
}
```

`ExecRunner.RunRsyncCommandProgress` must pass `io.Discard` for stdout and stderr. Interactive `RunRsync` remains unchanged.

- [ ] **Step 4: Run focused and runner tests**

Run: `go test ./internal/connectmac -run 'TestRunRsync|Test.*Rsync.*Progress'`

Expected: PASS, and no test command transcript is written to process stdout.

- [ ] **Step 5: Commit**

```bash
git add internal/connectmac/runner.go internal/connectmac/runner_test.go
git commit -m "fix: isolate rsync progress output"
```

### Task 2: Reconcile orphaned transfer lifecycle events

**Files:**
- Modify: `internal/connectmac/logs.go`
- Modify: `internal/connectmac/app_local_agent.go`
- Test: `internal/connectmac/logs_test.go`
- Test: `internal/connectmac/app_local_observability_test.go`

- [ ] **Step 1: Add failing JSONL lifecycle tests**

Write a started transfer, a completed transfer, and a second unmatched start. Verify reconciliation emits one interrupted event only for the unmatched transfer and remains idempotent on a second call.

```go
func TestReconcileInterruptedTransfersIsIdempotent(t *testing.T) {
	manager := NewLogManager(t.TempDir())
	writeTransferFixture(t, manager, "complete", "transfer.local.started")
	writeTransferFixture(t, manager, "complete", "transfer.local.succeeded")
	writeTransferFixture(t, manager, "orphan", "transfer.local.started")

	if err := manager.ReconcileInterruptedTransfers("local agent restarted"); err != nil { t.Fatal(err) }
	if err := manager.ReconcileInterruptedTransfers("local agent restarted"); err != nil { t.Fatal(err) }
	entries := readTestLogEntries(t, manager)
	assertActionCount(t, entries, "orphan", "transfer.local.interrupted", 1)
}
```

- [ ] **Step 2: Run the focused test**

Run: `go test ./internal/connectmac -run TestReconcileInterruptedTransfersIsIdempotent`

Expected: FAIL because reconciliation is absent.

- [ ] **Step 3: Add bounded JSONL reading and reconciliation**

Add `LogManager.ReadSince(cutoff time.Time) ([]LogEntry, error)` using `bufio.Scanner` with a bounded buffer. Add `ReconcileInterruptedTransfers(reason string)` that groups by `transfer_id`, considers succeeded/failed/canceled/interrupted terminal, and writes:

```go
LogEntry{
	Level: "warn", Action: "transfer.local.interrupted", Outcome: "failure",
	TransferID: started.TransferID, LocalJobID: started.LocalJobID,
	Profile: started.Profile, Direction: started.Direction,
	Status: LocalTransferInterrupted, Phase: TransferPhaseInterrupted,
	RequestID: started.RequestID, Source: "local-agent-recovery",
	ErrorCode: "agent_restarted", Message: reason,
}
```

- [ ] **Step 4: Invoke reconciliation before serving requests**

At local Agent startup, call reconciliation after `LogManager` is configured and before the HTTP listener starts. Log a `local-agent.recovery.failed` event if reading or writing fails; do not prevent Agent startup.

- [ ] **Step 5: Distinguish cancellation**

Add `LocalTransferCanceled = "canceled"`. Map explicit `context.Canceled` to `transfer.local.canceled`; retain deadline/process disappearance as interrupted. Keep the existing terminal callback exactly-once path.

- [ ] **Step 6: Run transfer lifecycle tests**

Run: `go test ./internal/connectmac -run 'Test(ReconcileInterruptedTransfers|LocalTransfer|.*Transfer.*Interrupted|.*Transfer.*Canceled)'`

Expected: PASS, with one terminal action for each accepted transfer.

- [ ] **Step 7: Commit**

```bash
git add internal/connectmac/logs.go internal/connectmac/app_local_agent.go internal/connectmac/local_transfer_jobs.go internal/connectmac/logs_test.go internal/connectmac/app_local_observability_test.go
git commit -m "fix: reconcile interrupted local transfers"
```

### Task 3: Centralize terminal and VNC error classification

**Files:**
- Create: `internal/connectmac/local_error.go`
- Create: `internal/connectmac/local_error_test.go`
- Modify: `internal/connectmac/logs.go`
- Modify: `internal/connectmac/app_local_agent.go`
- Modify: `internal/connectmac/app_web_terminal.go`
- Test: `internal/connectmac/app_local_observability_test.go`

- [ ] **Step 1: Add table-driven classifier tests**

```go
func TestClassifyLocalOperationError(t *testing.T) {
	tests := []struct{ text, code string }{
		{"REMOTE HOST IDENTIFICATION HAS CHANGED", "host_key_changed"},
		{"Host key verification failed", "host_key_changed"},
		{"exit status 255", "ssh_exit_255"},
		{"Operation timed out", "ssh_timeout"},
		{"local port 5900 is already in use", "local_port_in_use"},
		{"connection refused", "connection_refused"},
	}
	for _, test := range tests {
		if got := classifyLocalOperationError(errors.New(test.text)); got.Code != test.code {
			t.Fatalf("%q => %q, want %q", test.text, got.Code, test.code)
		}
	}
}
```

- [ ] **Step 2: Run classifier tests**

Run: `go test ./internal/connectmac -run TestClassifyLocalOperationError`

Expected: FAIL because the classifier is absent.

- [ ] **Step 3: Implement stable local classifications**

Create `LocalOperationError{Code, Level, Detail string; ExitCode int}` and `LocalCodedError{Code string; Cause error}` with `Error`, `Unwrap`, and `ErrorCode` methods. Use `errors.As` for `*exec.ExitError`, `net.Error`, context errors, and Gorilla close errors before bounded text matching. Sanitize `Detail` with `sanitizeLogText`.

- [ ] **Step 4: Add structured diagnostic fields**

Extend `LogEntry` additively:

```go
ExitCode     int    `json:"exit_code,omitempty"`
FailureStage string `json:"failure_stage,omitempty"`
```

Use the classifier in local and server terminal closure. Normal/user/browser closure remains `info` with `Outcome: "success"`; failures include the stable code and sanitized detail in `Message`.

For VNC, classify `errOut.String()`, set `FailureStage` to `profile`, `tunnel`, or `screen-sharing`, and retain `LocalPorts`, `PID`, and `TunnelAction`.

- [ ] **Step 5: Run terminal and VNC observability tests**

Run: `go test ./internal/connectmac -run 'Test(ClassifyLocalOperationError|.*Terminal.*Log|.*VNC.*Log)'`

Expected: PASS; `terminal.closed` must no longer use `aws_api_error` for SSH/WebSocket failures.

- [ ] **Step 6: Commit**

```bash
git add internal/connectmac/local_error.go internal/connectmac/local_error_test.go internal/connectmac/logs.go internal/connectmac/app_local_agent.go internal/connectmac/app_web_terminal.go internal/connectmac/app_local_observability_test.go
git commit -m "fix: classify local terminal and vnc failures"
```

### Task 4: Block stale Host Keys until explicit repair

**Files:**
- Modify: `internal/connectmac/host_key.go`
- Modify: `internal/connectmac/app_connect.go`
- Modify: `internal/connectmac/app_local_agent.go`
- Modify: `internal/connectmac/app_web_terminal.go`
- Test: `internal/connectmac/app_test.go`
- Test: `internal/connectmac/app_local_observability_test.go`

- [ ] **Step 1: Add failing stale-key guard tests**

Verify terminal, VNC, push, and pull return `host_key_changed` without invoking SSH/rsync when `checkHostKey` returns stale. Verify `cm host-key fix` still replaces the key and unblocks subsequent access.

- [ ] **Step 2: Run focused Host Key tests**

Run: `go test ./internal/connectmac -run 'Test.*HostKey.*(Block|Stale|Fix)'`

Expected: FAIL because access paths currently call `fixHostKey` and replace stale keys automatically.

- [ ] **Step 3: Add a non-mutating access guard**

Implement:

```go
func (a App) requireCurrentHostKey(ctx context.Context, profile Profile) (HostKeyCheck, error) {
	check, err := a.checkHostKey(ctx, profile)
	if err != nil { return check, err }
	switch check.Status {
	case HostKeyCurrent:
		return check, nil
	case HostKeyStale:
		return check, LocalCodedError{Code: "host_key_changed", Cause: errors.New("host key changed; confirm the new fingerprint with cm host-key fix")}
	case HostKeyMissing:
		return check, LocalCodedError{Code: "host_key_missing", Cause: errors.New("host key is not trusted; confirm it with cm host-key fix")}
	default:
		return check, LocalCodedError{Code: "host_key_scan_failed", Cause: errors.New(check.Message)}
	}
}
```

Use it in connection, terminal, VNC, and transfer execution paths. Keep mutation exclusively in `cm host-key fix` and the existing web fingerprint confirmation endpoint.

- [ ] **Step 4: Add deduplicated structured events**

Log one `host-key.blocked` event per request with Profile, request ID, status, and error code. Do not write scanned key material or fingerprints to logs.

- [ ] **Step 5: Run Host Key and local workflow tests**

Run: `go test ./internal/connectmac -run 'Test.*(HostKey|Terminal|VNC|Transfer)'`

Expected: PASS, and stale keys cannot reach command execution.

- [ ] **Step 6: Commit**

```bash
git add internal/connectmac/host_key.go internal/connectmac/app_connect.go internal/connectmac/app_local_agent.go internal/connectmac/app_web_terminal.go internal/connectmac/app_test.go internal/connectmac/app_local_observability_test.go
git commit -m "fix: require explicit host key repair"
```

### Task 5: Rotate logs and export safe diagnostics by default

**Files:**
- Modify: `internal/connectmac/logs.go`
- Modify: `internal/connectmac/app_logs.go`
- Modify: `internal/connectmac/app_usage.go`
- Modify: `internal/connectmac/app_completion.go`
- Test: `internal/connectmac/logs_test.go`
- Test: `internal/connectmac/app_test.go`

- [ ] **Step 1: Add failing rotation and export tests**

Test that a small configured size threshold creates valid `cm-YYYY-MM-DD.1.log` generations, old generations expire, default export omits `local-agent.out.log`, and raw export includes a redacted version rather than original secrets, home usernames, IPs, or fingerprints.

- [ ] **Step 2: Run focused logging tests**

Run: `go test ./internal/connectmac -run 'TestLogManager.*(Rotate|Export|Redact)'`

Expected: FAIL for rotation and raw export options.

- [ ] **Step 3: Add log policy and safe rotation**

Extend `LogManager`:

```go
type LogManager struct {
	Dir string
	Now func() time.Time
	MaxStructuredBytes int64
	MaxGenerations int
}
```

Defaults are 10 MiB and three generations. Rotate before append when the next complete JSON line would exceed the limit. Rename generations from highest to lowest and keep each file valid JSONL.

Before installing or restarting the launchd Agent, rotate `local-agent.out.log` and `local-agent.err.log` when either exceeds 1 MiB, retaining one previous generation. Combined with Task 1 removing rsync transcripts, these files then contain only bounded bootstrap diagnostics.

- [ ] **Step 4: Add export options and manifest**

```go
type LogExportOptions struct {
	Destination string
	Retention time.Duration
	IncludeRaw bool
}
```

Default export includes `cm-*.log` and `manifest.json`. `--include-raw` additionally streams raw files through `sanitizeExportLine`, truncates each included raw file to its most recent 5 MiB, and writes names under `raw/`.

The manifest records export time, `include_raw`, categories, file count, and redaction policy version. It must not contain local paths.

- [ ] **Step 5: Parse and document `--include-raw`**

Support both orders:

```bash
cm logs export --include-raw --output diagnostics.zip
cm logs export --output diagnostics.zip --include-raw
```

Update usage text and shell completion without changing existing `--output` behavior.

- [ ] **Step 6: Run logging and CLI tests**

Run: `go test ./internal/connectmac -run 'Test(LogManager|AppLogs|.*LogsExport)'`

Expected: PASS; default archives contain no raw Agent logs.

- [ ] **Step 7: Commit**

```bash
git add internal/connectmac/logs.go internal/connectmac/app_logs.go internal/connectmac/app_usage.go internal/connectmac/app_completion.go internal/connectmac/logs_test.go internal/connectmac/app_test.go
git commit -m "feat: export bounded privacy-safe logs"
```

### Task 6: Correct progress semantics and complete verification

**Files:**
- Modify: `internal/connectmac/local_transfer_jobs.go`
- Modify: `internal/connectmac/logs.go`
- Test: `internal/connectmac/local_transfer_jobs_test.go`
- Test: `internal/connectmac/app_test.go`

- [ ] **Step 1: Add real progress parsing tests**

Use representative `--info=progress2` carriage-return lines and verify monotonic percent, transferred bytes, total bytes, rate, and finalizing state. Verify a chunk that crosses several thresholds emits one current progress event, not several synthetic events with the same timestamp.

- [ ] **Step 2: Run progress tests**

Run: `go test ./internal/connectmac -run 'Test.*RsyncProgress|Test.*Transfer.*Milestone'`

Expected: FAIL for byte fields and batch milestone emission.

- [ ] **Step 3: Add additive progress fields**

Extend transfer job/event/log types:

```go
BytesTransferred int64  `json:"bytes_transferred,omitempty"`
BytesTotal       int64  `json:"bytes_total,omitempty"`
BytesPerSecond   int64  `json:"bytes_per_second,omitempty"`
ETASeconds       int64  `json:"eta_seconds,omitempty"`
```

Parse progress2 data from the latest carriage-return record. Emit at most one progress event per received chunk and only when percent advances or phase changes. Clamp live progress to 99; emit 100 only after successful process exit.

- [ ] **Step 4: Run all transfer tests**

Run: `go test ./internal/connectmac -run 'Test.*(Rsync|Transfer)'`

Expected: PASS with monotonic byte-derived progress.

- [ ] **Step 5: Run complete verification**

```bash
gofmt -w internal/connectmac/runner.go internal/connectmac/runner_test.go internal/connectmac/logs.go internal/connectmac/logs_test.go internal/connectmac/app_logs.go internal/connectmac/app_usage.go internal/connectmac/app_completion.go internal/connectmac/local_transfer_jobs.go internal/connectmac/local_transfer_jobs_test.go internal/connectmac/local_error.go internal/connectmac/local_error_test.go internal/connectmac/host_key.go internal/connectmac/app_connect.go internal/connectmac/app_local_agent.go internal/connectmac/app_web_terminal.go internal/connectmac/app_local_observability_test.go internal/connectmac/app_test.go
go test ./internal/connectmac
go test ./...
git diff --check
```

Expected: all tests pass and no whitespace errors remain.

- [ ] **Step 6: Commit**

```bash
git add internal/connectmac/local_transfer_jobs.go internal/connectmac/local_transfer_jobs_test.go internal/connectmac/logs.go internal/connectmac/app_test.go
git commit -m "fix: report byte-derived transfer progress"
```
