# Member Auto-Release Access Design

## Goal

Allow every authenticated member to enable or disable automatic release for
profiles assigned to that member. Administrators retain access to every
profile.

## Authorization

- Administrators may change automatic release for any profile.
- Non-admin members may change automatic release only for profiles present in
  their profile-access assignments.
- The API must enforce this rule. Hiding or enabling a browser button is not an
  authorization boundary.
- A member must receive `403 Forbidden` when attempting to change an
  unassigned profile.

## User Interface

- Show the automatic-release toggle to administrators and members who can view
  the selected profile.
- Preserve existing readiness and lifecycle guards. The toggle remains
  disabled when the Mac is not ready, required reminder data is missing, a
  request is busy, or an automatic release/destroy operation is active.
- Keep the existing confirmation dialog for both enabling and disabling.

## Audit And Notifications

- Record the authenticated member as the actor for every successful change.
- Preserve the existing event log and WeCom notification behavior.
- Do not change the profile owner when a member toggles automatic release.

## Tests

Cover these cases:

- An administrator can change any profile.
- A non-admin member can change an assigned profile.
- A non-admin member cannot change an unassigned profile.
- The browser does not hide the toggle solely because the user is not an
  administrator.
- Existing readiness and active-release button guards remain intact.

## Scope

This change does not alter reminder deadlines, automatic release execution,
AWS destroy behavior, Elastic IP retention, or profile assignment management.
