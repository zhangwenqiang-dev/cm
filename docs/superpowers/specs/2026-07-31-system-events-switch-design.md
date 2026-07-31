# System Events Switch Design

## Problem

The operations toolbar renders the `包含系统事件` checkbox as a large text
input because the global `input` rules apply a fixed width, minimum height,
padding, and border to every input type.

## Design

Replace only this control's presentation with a compact Bootstrap 5.3 switch:

- Keep the existing element IDs and change handler.
- Add Bootstrap `form-check form-switch` classes to the label wrapper.
- Add `form-check-input` to the checkbox and use an explicit associated label.
- Add narrowly scoped CSS under `#includeSystemEventsWrap` and
  `#includeSystemEvents` so global input and mobile toolbar rules cannot stretch
  the switch.
- Keep it in the existing toolbar position after the events button.
- Preserve admin-only visibility and all event-loading behavior.

No other toolbar buttons, page layout, responsive table behavior, or previously
styled components will change.

## Verification

- Desktop Chrome, Safari, and Firefox-compatible layout: compact switch and
  label remain aligned on one line when space permits.
- Mobile layout: the control remains usable without stretching into a text
  field.
- Toggling the switch still reloads events with `include_system=1` for admins.
- Existing JavaScript syntax and Go tests pass.
