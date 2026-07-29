# Profile Owner And Auto-Release Access Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve server-backed profile owners in the web UI and allow members to manage automatic release only for profiles assigned to them.

**Architecture:** The browser seeds its owner map from `/api/profiles`, then reconciles it with `/api/profile-owners` without destructive fallback. The automatic-release endpoint uses the existing `ensureWebMemberProfileAccess` authorization boundary, while the browser exposes the existing guarded toggle to every authenticated member.

**Tech Stack:** Go HTTP handlers and tests, vanilla JavaScript, HTML, MySQL-backed member store.

---

### Task 1: Preserve Profile Owners

**Files:**
- Modify: `web/index.html`
- Create: `internal/connectmac/app_web_profile_owner_ui_test.go`

- [ ] **Step 1: Write the failing owner-loading contract test**

Add a test that reads `web/index.html` and requires owner seeding before
`loadProfileOwners()`, rejects the `clientConfig.user_api` early return, and
requires the catch path to preserve the current owner map.

- [ ] **Step 2: Run the focused test and verify failure**

Run:

```bash
go test ./internal/connectmac -run TestWebProfileOwnerLoadingContract -count=1
```

Expected: FAIL because the current implementation clears owners and skips the
owner endpoint when `user_api` is empty.

- [ ] **Step 3: Implement owner seeding and non-destructive reconciliation**

In `loadProfiles()`, build `state.profileOwners` from each profile's first
embedded owner before calling `loadProfileOwners()`. In
`loadProfileOwners()`, always call `/api/profile-owners`, replace the map only
after success, and return without clearing the seeded map on failure.

- [ ] **Step 4: Run the focused owner test**

Run:

```bash
go test ./internal/connectmac -run TestWebProfileOwnerLoadingContract -count=1
```

Expected: PASS.

### Task 2: Authorize Assigned Members

**Files:**
- Modify: `internal/connectmac/app_web.go`
- Modify: `internal/connectmac/app_web_auto_release_test.go`

- [ ] **Step 1: Replace the role-only test with assignment tests**

Add cases proving an operator and viewer can toggle an assigned managed
profile, while the same roles receive HTTP 403 for an unassigned profile.
Keep the existing administrator success coverage.

- [ ] **Step 2: Run the focused handler tests and verify failure**

Run:

```bash
go test ./internal/connectmac -run 'TestAppWebAutoReleaseToggle(RoleAndValidation|AssignedMemberAccess)' -count=1
```

Expected: assigned-member cases FAIL with `admin role required`.

- [ ] **Step 3: Apply the existing profile-access boundary**

Remove the handler's administrator-only rejection. After parsing and
validating `profile`, call:

```go
if err := a.ensureWebMemberProfileAccess(member, profileName); err != nil {
    writeWebError(w, http.StatusForbidden, err.Error())
    return
}
```

This preserves administrator access because `ensureWebMemberProfileAccess`
already allows administrators.

- [ ] **Step 4: Run focused handler tests**

Run:

```bash
go test ./internal/connectmac -run 'TestAppWebAutoReleaseToggle(RoleAndValidation|AssignedMemberAccess)' -count=1
```

Expected: PASS.

### Task 3: Expose The Guarded Toggle

**Files:**
- Modify: `web/index.html`
- Modify: `internal/connectmac/app_web_auto_release_test.go`

- [ ] **Step 1: Update the failing UI contract**

Require the automatic-release button to omit `admin-only`, require
`renderAutoRelease()` not to hide it based on `isAdmin()`, and require dialog
open/submit guards not to reject non-admin members.

- [ ] **Step 2: Run the UI contract and verify failure**

Run:

```bash
go test ./internal/connectmac -run TestWebAutoReleaseUIContract -count=1
```

Expected: FAIL on the current administrator-only markup or guards.

- [ ] **Step 3: Remove administrator-only browser guards**

Remove `admin-only` from `autoReleaseToggleBtn`, remove its `!isAdmin()` hidden
toggle, and remove `!isAdmin()` from the dialog open and submit guards. Keep
all readiness, busy, reminder, and active-release checks unchanged.

- [ ] **Step 4: Run focused UI tests**

Run:

```bash
go test ./internal/connectmac -run 'TestWebAutoRelease(UIContract|ReleasingStateLocksConflictingActions)' -count=1
```

Expected: PASS.

### Task 4: Regression And Release Verification

**Files:**
- Modify only if tests expose a defect in files already listed above.

- [ ] **Step 1: Format and run the complete test suite**

Run:

```bash
gofmt -w internal/connectmac/app_web.go internal/connectmac/app_web_auto_release_test.go internal/connectmac/app_web_profile_owner_ui_test.go
go test ./...
```

Expected: PASS.

- [ ] **Step 2: Verify the working tree**

Run:

```bash
git diff --check
git status --short
```

Expected: only the planned files and pre-existing unrelated untracked files
are present.

- [ ] **Step 3: Commit the implementation**

```bash
git add web/index.html internal/connectmac/app_web.go internal/connectmac/app_web_auto_release_test.go internal/connectmac/app_web_profile_owner_ui_test.go docs/superpowers/plans/2026-07-29-profile-owner-and-auto-release-access.md
git commit -m "fix: preserve owners and allow member auto release"
```

- [ ] **Step 4: Publish and deploy**

Use the repository's established release scripts to publish Homebrew and APT
artifacts, upgrade the local installation, deploy the same version to
`staging2`, and restart `connectmac.service`.

- [ ] **Step 5: Verify production behavior**

Confirm:

- `iossupport-usw2` displays owner `张会林`.
- An assigned non-admin member sees and can open the automatic-release dialog.
- An unassigned profile is absent from that member's profile list and a direct
  API request is rejected with HTTP 403.
- Administrators retain access to every profile.
