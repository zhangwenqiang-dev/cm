# System Events Switch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Render `包含系统事件` as a compact Bootstrap switch without changing its behavior or other existing styles.

**Architecture:** Preserve the checkbox IDs and JavaScript handlers. Apply Bootstrap switch classes and narrowly scoped CSS that overrides the global input and mobile toolbar sizing only for this control.

**Tech Stack:** HTML, CSS, Bootstrap 5.3, Go source-contract tests.

---

### Task 1: Add the compact switch contract

**Files:**
- Modify: `web/index.html`
- Modify: `internal/connectmac/app_test.go`

- [ ] **Step 1: Add a failing source-contract test**

Add a test that reads `web/index.html` and requires:

```html
<div id="includeSystemEventsWrap" class="admin-only form-check form-switch">
<input id="includeSystemEvents" class="form-check-input" type="checkbox">
<label class="form-check-label" for="includeSystemEvents">包含系统事件</label>
```

The test must also require narrowly scoped CSS selectors for the wrapper and
checkbox.

- [ ] **Step 2: Run the focused test and verify failure**

Run:

```bash
go test ./internal/connectmac -run TestWebSystemEventsUsesCompactSwitch -count=1
```

Expected: fail because the compact switch markup is absent.

- [ ] **Step 3: Apply the minimal HTML and CSS change**

Replace only the existing wrapper markup with the Bootstrap switch structure.
Add scoped rules that set inline-flex alignment, intrinsic width, zero global
padding, normal checkbox minimum height, and prevent the mobile toolbar input
rule from stretching this checkbox.

- [ ] **Step 4: Run checks**

Run:

```bash
node scripts/check-web-js.mjs
go test ./internal/connectmac -run TestWebSystemEventsUsesCompactSwitch -count=1
go test ./... -count=1
```

Expected: all checks pass.

### Task 2: Browser verification

**Files:**
- No additional source changes expected.

- [ ] **Step 1: Verify desktop layout**

Open the operations page and confirm the switch is compact, aligned with the
toolbar, and does not alter adjacent buttons.

- [ ] **Step 2: Verify mobile layout**

Use a mobile viewport and confirm the switch remains compact and its label fits
without overlap.

- [ ] **Step 3: Verify behavior**

As an administrator, toggle the switch and confirm the existing event reload
handler still runs without console errors.
