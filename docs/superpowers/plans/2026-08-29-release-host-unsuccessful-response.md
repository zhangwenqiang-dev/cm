# ReleaseHosts Unsuccessful Response Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent AWS `ReleaseHosts` per-resource failures from being recorded as accepted releases and automatically recover legacy false convergence state.

**Architecture:** Validate the AWS SDK batch output at the client boundary, so the existing destroy service receives a normal error for unsuccessful Host releases. Tighten automatic-release acceptance to require structured job evidence and add an atomic transition that returns legacy inferred convergence to the normal retry path.

**Tech Stack:** Go 1.26, AWS SDK for Go v2, `database/sql`, file-backed and MySQL member stores, Go testing.

---

### Task 1: Validate `ReleaseHosts` batch output

**Files:**
- Modify: `internal/connectmac/aws_client.go`
- Test: `internal/connectmac/aws_client_test.go`

- [ ] **Step 1: Write failing table-driven response tests**

Add tests for a pure response validator using SDK output values:

```go
func TestValidateReleaseHostOutput(t *testing.T) {
	tests := []struct {
		name string
		out  *ec2.ReleaseHostsOutput
		want string
	}{
		{name: "successful", out: &ec2.ReleaseHostsOutput{Successful: []string{"h-1"}}},
		{name: "occupied", out: &ec2.ReleaseHostsOutput{Unsuccessful: []ec2types.UnsuccessfulItem{{ResourceId: aws.String("h-1"), Error: &ec2types.UnsuccessfulItemError{Code: aws.String("Client.HostNotReleasable"), Message: aws.String("host is occupied")}}}}, want: "host is occupied"},
		{name: "empty", out: &ec2.ReleaseHostsOutput{}, want: "did not confirm"},
		{name: "contradictory", out: &ec2.ReleaseHostsOutput{Successful: []string{"h-1"}, Unsuccessful: []ec2types.UnsuccessfulItem{{ResourceId: aws.String("h-1")}}}, want: "contradictory"},
	}
}
```

- [ ] **Step 2: Run the focused test and verify failure**

Run: `go test ./internal/connectmac -run TestValidateReleaseHostOutput -count=1`

Expected: FAIL because `validateReleaseHostOutput` does not exist.

- [ ] **Step 3: Implement strict output validation**

Capture the SDK output and validate the requested Host ID:

```go
out, err := c.ec2.ReleaseHosts(ctx, &ec2.ReleaseHostsInput{HostIds: []string{hostID}})
if err != nil {
	return fmt.Errorf("release host %s: %w", hostID, err)
}
if err := validateReleaseHostOutput(hostID, out); err != nil {
	return fmt.Errorf("release host %s: %w", hostID, err)
}
return nil
```

The validator must reject matching `Unsuccessful`, empty output, nil output, and a Host present in both lists. It must accept only an unambiguous matching `Successful` entry.

- [ ] **Step 4: Run focused AWS client and destroy tests**

Run: `go test ./internal/connectmac -run 'TestValidateReleaseHostOutput|TestAWSServiceDestroy' -count=1`

Expected: PASS. Existing pending-host retry behavior remains intact.

- [ ] **Step 5: Commit the client boundary fix**

```bash
git add internal/connectmac/aws_client.go internal/connectmac/aws_client_test.go
git commit -m "fix: validate release host batch results"
```

### Task 2: Require structured acceptance evidence

**Files:**
- Modify: `internal/connectmac/auto_release.go`
- Test: `internal/connectmac/auto_release_test.go`

- [ ] **Step 1: Write failing acceptance tests**

Add table coverage proving that only explicit structured evidence is accepted:

```go
tests := []struct {
	name string
	job  Job
	want bool
}{
	{name: "structured match", job: Job{Status: JobStatusSuccess, ReleaseEvidenceRecorded: true, ReleasedHosts: []string{"h-1"}}, want: true},
	{name: "legacy text-era success", job: Job{Status: JobStatusSuccess, ReleasedHosts: []string{"h-1"}}, want: false},
	{name: "structured mismatch", job: Job{Status: JobStatusSuccess, ReleaseEvidenceRecorded: true, ReleasedHosts: []string{"h-other"}}, want: false},
}
```

- [ ] **Step 2: Run the test and verify the legacy case fails**

Run: `go test ./internal/connectmac -run TestAcceptedReleaseConverging -count=1`

Expected: FAIL because legacy evidence is currently accepted.

- [ ] **Step 3: Tighten `acceptedReleaseConverging`**

Require all of the following:

```go
if !job.ReleaseEvidenceRecorded || !autoReleaseJobSupportsCompletionChecks(job) {
	return false
}
```

Then require the reminder Host ID in `job.ReleasedHosts` and the existing exact pending-Host topology.

- [ ] **Step 4: Run acceptance tests**

Run: `go test ./internal/connectmac -run 'TestAcceptedReleaseConverging|TestAutoRelease.*Convergence' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the evidence rule**

```bash
git add internal/connectmac/auto_release.go internal/connectmac/auto_release_test.go
git commit -m "fix: require structured host release evidence"
```

### Task 3: Recover legacy false convergence atomically

**Files:**
- Modify: `internal/connectmac/auto_release.go`
- Modify: `internal/connectmac/member_store.go`
- Modify: `internal/connectmac/member_store_mysql.go`
- Test: `internal/connectmac/auto_release_test.go`
- Test: `internal/connectmac/member_store_auto_release_test.go`

- [ ] **Step 1: Add failing store transition tests**

Define and test an atomic store operation:

```go
ResetLegacyAutoReleaseConvergence(cycle ReleaseReminderCycle, retryAt, reason string) (ReleaseReminder, bool, error)
```

The matching transition must clear `AutoReleaseAcceptedAt`, stalled-warning claim/notification fields, set state to `retrying`, set `AutoReleaseAt=retryAt`, and retain Host, owner, attempts, EIP-safe workflow data, and cycle identity. A stale cycle must not update.

- [ ] **Step 2: Run store tests and verify failure**

Run: `go test ./internal/connectmac -run 'Test.*ResetLegacyAutoReleaseConvergence' -count=1`

Expected: FAIL because the interface and implementations do not exist.

- [ ] **Step 3: Implement file and MySQL atomic transitions**

For the file store, use the existing locked `UpdateReleaseReminder` pattern. For MySQL, use a conditional `UPDATE` guarded by profile, cycle timestamps, running state, and current accepted timestamp; reload the row after a successful update. Both implementations return `transitioned=false` for stale or already-reset state.

- [ ] **Step 4: Add coordinator migration tests**

Cover an accepted reminder whose latest source job is legacy:

```go
Job{Type: "aws-destroy", Profile: "mac", Status: JobStatusSuccess, ReleaseEvidenceRecorded: false}
```

After one scan, assert state `retrying`, accepted fields cleared, no notification, no destroy started in that same scan, and event `convergence-evidence-invalidated`. Also assert structured evidence remains in read-only convergence.

- [ ] **Step 5: Implement migration before convergence observation**

When `AutoReleaseAcceptedAt` is set, list completion-check jobs and locate the latest job for the cycle. If it lacks structured matching evidence, call `ResetLegacyAutoReleaseConvergence` with `now` as the retry time and emit `convergence-evidence-invalidated`. Return without starting a mutation; the next scan follows the normal retry path.

- [ ] **Step 6: Run migration and concurrency tests**

Run: `go test ./internal/connectmac -run 'Test.*Legacy.*Convergence|Test.*ResetLegacyAutoReleaseConvergence|TestAutoRelease.*Convergence' -count=1`

Run: `go test -race ./internal/connectmac -run 'Test.*Legacy.*Convergence|Test.*ResetLegacyAutoReleaseConvergence' -count=10`

Expected: PASS with no races or duplicate transition.

- [ ] **Step 7: Commit legacy recovery**

```bash
git add internal/connectmac/auto_release.go internal/connectmac/member_store.go internal/connectmac/member_store_mysql.go internal/connectmac/auto_release_test.go internal/connectmac/member_store_auto_release_test.go
git commit -m "fix: recover false host release convergence"
```

### Task 4: Complete regression verification

**Files:**
- Modify only if a test exposes a scoped defect in the files above.

- [ ] **Step 1: Run formatting and focused tests**

Run: `gofmt -w internal/connectmac/aws_client.go internal/connectmac/aws_client_test.go internal/connectmac/auto_release.go internal/connectmac/auto_release_test.go internal/connectmac/member_store.go internal/connectmac/member_store_mysql.go internal/connectmac/member_store_auto_release_test.go`

Run: `go test ./internal/connectmac -count=1`

Expected: PASS.

- [ ] **Step 2: Run the full quality gate**

Run: `go test ./...`

Run: `go test -race ./...`

Run: `go vet ./...`

Run: `git diff --check`

Expected: all commands pass.

- [ ] **Step 3: Verify safety invariants**

Confirm tests show that recovery starts no EC2 termination when no active instance exists, never calls an Elastic IP release operation, and sends no success notification while the Host remains.

- [ ] **Step 4: Commit any final test-only adjustments**

```bash
git add internal/connectmac
git commit -m "test: cover occupied host release recovery"
```

