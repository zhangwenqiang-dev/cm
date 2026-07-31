# Operation Header Layout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the redundant operation-page home button and render Profile and Apple email as a vertical, left-aligned title.

**Architecture:** Preserve operation navigation and state. Replace one combined text node with two dedicated text nodes, style only their title container, and remove all references to the deleted button.

**Tech Stack:** HTML, CSS, browser JavaScript, Go source-contract tests.

---

### Task 1: Define the operation-header contract

**Files:**
- Modify: `internal/connectmac/app_test.go`

- [ ] Add a source-contract test that requires `selectedProfileName`,
  `selectedAppleEmail`, their scoped CSS classes, and separate `textContent`
  assignments in `renderSelected()`.
- [ ] Require that `backHomeBtn` and `返回首页` are absent.
- [ ] Run the focused test and verify that it fails before implementation.

### Task 2: Apply the minimal header change

**Files:**
- Modify: `web/index.html`

- [ ] Replace `selectedTitle` with a left-aligned two-line heading containing
  `selectedProfileName` and `selectedAppleEmail`.
- [ ] Add scoped title styles using grid layout, zero margin, and responsive
  text wrapping.
- [ ] Remove `backHomeBtn` from markup, the busy-state ID list, and event
  binding.
- [ ] Update `renderSelected()` to assign Profile and Apple email separately
  through `textContent`.

### Task 3: Verify

- [ ] Run `node scripts/check-web-js.mjs`.
- [ ] Run the focused contract test.
- [ ] Run `go test ./... -count=1`.
- [ ] Use a local browser preview to inspect desktop and mobile computed
  alignment, wrapping, and console errors.
- [ ] Confirm no unrelated source files changed.
