# Enterprise WeChat Host Architecture Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Simplify all Enterprise WeChat messages and include the actual Mac host architecture in successful open and automatic-release messages.

**Architecture:** Capture architecture from the actual Dedicated Host instance type while finalizing open, persist it on the release reminder, and render it only for the two lifecycle success events that need it. Existing records remain valid with an empty architecture.

**Tech Stack:** Go, `net/http`, JSON member store, MySQL via `database/sql`, AWS SDK v2 status models.

---

### Task 1: Lock the notification format contract

**Files:**
- Modify: `internal/connectmac/wechat_test.go`
- Modify: `internal/connectmac/app_web_auto_release_test.go`

- [ ] **Step 1: Write failing renderer tests**

Update the common notification test to reject `ConnectMac` and `Profile`, require `Host 架构类型：arm64` for an open notification, and add a table test proving unrelated events omit architecture. Update the automatic-release success test to require Apple email and `Host 架构类型：x86` while rejecting the Profile name.

- [ ] **Step 2: Run the focused tests and verify failure**

Run:

```bash
go test ./internal/connectmac -run 'TestWechatNotifier|TestAppAutoReleaseSuccessNotificationUsesWechatWebhook'
```

Expected: failure because the current title contains `ConnectMac`, the body contains `Profile`, and no architecture is rendered.

- [ ] **Step 3: Implement the notification model and renderer**

In `internal/connectmac/wechat.go`, add:

```go
HostArchitecture string
```

Render the title as:

```go
fmt.Fprintf(&b, "## %s\n", title)
```

Remove the Profile field and conditionally render architecture only for `open` and `auto-release-success`:

```go
if notification.Event == "open" || notification.Event == "auto-release-success" {
    writeWechatField(&b, "Host 架构类型", notification.HostArchitecture)
}
```

- [ ] **Step 4: Run the focused tests**

Run the command from Step 2. Expected: renderer tests pass after reminder propagation is completed in later tasks; any remaining failure must specifically identify the missing persisted architecture.

### Task 2: Persist the actual host architecture

**Files:**
- Modify: `internal/connectmac/member_store.go`
- Modify: `internal/connectmac/member_store_mysql.go`
- Modify: `internal/connectmac/member_store_auto_release_test.go`

- [ ] **Step 1: Write failing JSON and MySQL persistence tests**

Add `HostArchitecture: "arm64"` to JSON round-trip fixtures and MySQL insert/update/scan fixtures. Require the loaded reminder to retain the value. Add a legacy-row assertion that the field remains empty.

- [ ] **Step 2: Run persistence tests and verify failure**

```bash
go test ./internal/connectmac -run 'ReleaseReminder|MySQLAutoReleaseNotificationMarker'
```

Expected: compile or assertion failure because `ReleaseReminder` and the MySQL schema do not contain the field.

- [ ] **Step 3: Add the optional field and MySQL migration**

Add to `ReleaseReminder`:

```go
HostArchitecture string `json:"host_architecture,omitempty"`
```

Add nullable `host_architecture` storage to the create/migration SQL, select scans, insert SQL, and update SQL. Keep legacy JSON decoding unchanged and map SQL `NULL`/empty values to an empty string.

- [ ] **Step 4: Run persistence tests**

Run the command from Step 2. Expected: PASS.

### Task 3: Capture architecture from the observed Dedicated Host

**Files:**
- Modify: `internal/connectmac/aws_types.go`
- Modify: `internal/connectmac/web_aws_lifecycle.go`
- Modify: `internal/connectmac/app_web.go`
- Modify: `internal/connectmac/web_aws_lifecycle_test.go`
- Modify: `internal/connectmac/app_web_auto_release_test.go`

- [ ] **Step 1: Write failing architecture mapping and lifecycle tests**

Add table cases requiring `mac1.metal -> x86`, every supported Apple Silicon instance type -> `arm64`, and unknown/empty types -> empty. Require open finalization to save architecture from `status.Hosts[].InstanceType`, including refreshing a reused reminder for the same Host ID.

- [ ] **Step 2: Run focused lifecycle tests and verify failure**

```bash
go test ./internal/connectmac -run 'HostArchitecture|FinalizeWebAWSOpen|AutoReleaseSuccessNotification'
```

Expected: failure because architecture is not mapped or stored.

- [ ] **Step 3: Implement strict instance-type mapping and propagation**

Add a helper in `aws_types.go`:

```go
func MacHostArchitecture(instanceType string) string {
    instanceType = strings.TrimSpace(instanceType)
    if !SupportedMacInstanceTypes[instanceType] {
        return ""
    }
    if IsIntelMacInstanceType(instanceType) {
        return "x86"
    }
    return "arm64"
}
```

While selecting the actual active Host in both open-finalization paths, compute and store the architecture. Pass `reminder.HostArchitecture` into both normal and observed `WechatNotification` values.

- [ ] **Step 4: Run focused and full verification**

```bash
go test ./internal/connectmac -run 'HostArchitecture|WechatNotifier|FinalizeWebAWSOpen|AutoReleaseSuccessNotification'
go test ./...
go test -race ./...
go vet ./...
```

Expected: all commands pass.

- [ ] **Step 5: Commit implementation**

```bash
git add internal/connectmac
git commit -m "feat: include host architecture in wechat lifecycle notifications"
```
