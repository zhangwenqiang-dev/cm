# Operation Header Layout Design

## Goal

Simplify the operation-page header by removing the redundant `返回首页`
button and displaying the selected Profile and Apple email as two vertically
stacked, left-aligned lines.

## Design

- Remove `backHomeBtn` from the operation toolbar.
- Remove its busy-state registration and click handler.
- Keep the left navigation `首页` entry as the only direct route back home.
- Replace the combined `selectedTitle` text with two child elements:
  `selectedProfileName` and `selectedAppleEmail`.
- Update `renderSelected()` through `textContent` assignments only.
- Add narrowly scoped `.selected-title`, `.selected-profile-name`, and
  `.selected-apple-email` styles for vertical alignment and hierarchy.
- Preserve every other operation button, toolbar behavior, responsive rule,
  history behavior, and selected Profile state.

## Verification

- No `backHomeBtn` or `返回首页` remains in the web source.
- Profile appears on the first line and Apple email on the second line.
- Both lines are left-aligned on desktop and mobile.
- Selecting a Profile still opens the operation page and updates both values.
- JavaScript syntax and the complete Go test suite pass.
