# ConnectMac State-Driven Web UX Design

## Goal

Reorganize the ConnectMac web management interface around the current Mac
state and the user's next valid action. The design must reduce ambiguous or
unsafe actions, preserve existing AWS safety rules, remain usable on desktop
and mobile browsers, and make background work understandable after navigation,
refresh, or sign-in.

This design covers the web management interface first. CLI and MCP behavior
changes are limited to terminology or state consistency required by the web
experience.

## Chosen Approach

Use a state-driven workbench.

- Keep the existing Go server, server-rendered web shell, Bootstrap 5.3 assets,
  database, local Agent, AWS jobs, and lifecycle coordinator.
- Reorganize page information and action rendering without changing AWS
  allocation, launch, readiness, release, EIP, or confirmation safety rules.
- Show one clear recommendation for the current state.
- Keep actions in stable locations, but disable or hide them according to an
  explicit state model.
- Keep AWS diagnostic fields available in a collapsed technical-details area.
- Persist and restore background task feedback instead of relying on transient
  modal messages.
- Do not introduce a frontend framework or a second state store.

## Non-Goals

- Replacing Bootstrap or rewriting the page as React, Vue, or another SPA.
- Changing AWS Mac creation, readiness, release, or retry semantics.
- Moving local PEM files, SSH, VNC, or transfer execution to the server.
- Adding new member roles or changing existing authorization policy.
- Redesigning CLI commands or MCP tools beyond shared labels and state names.
- Changing profile, member, job, reminder, or audit database ownership.

## Information Architecture

### Global Navigation

The left navigation has three possible entries:

- `首页`: daily Mac status and operation entry.
- `Profiles`: Profile and AWS resource configuration. Admin only.
- `用户管理`: members, roles, tokens, passwords, and Profile authorization.
  Admin only.

Account settings remain behind the user control in the upper-right corner.
There is no separate operation-page entry in the navigation. A Profile
workbench is entered from the home page and retains the left navigation.

Members only see navigation entries they can use. Direct access to a forbidden
page returns a visible permission explanation and records an authorization
audit event.

### Home Page

The home page lists Profiles available to the signed-in member. Admins see all
active Profiles.

Each desktop row or mobile card shows:

- Profile name.
- Apple account once.
- Region.
- Responsible member summary.
- Human-readable state.
- A single `进入工作台` or `管理` action.

The whole row or card may also open the workbench. The old generic `选择`
wording is removed. The home page does not edit Profiles, member authorization,
or local PEM configuration.

The page shows the last successful status refresh time and performs a partial
refresh every 10 seconds while visible.

### Profiles Page

The Profiles page owns resource configuration:

- Add and edit Profile metadata.
- Enable or disable a Profile.
- Associate shared AWS configuration.
- Show the configuration health of all Profiles to admins.

Local `identity_file` overrides remain on each member's computer and are not
edited on this server page. Profile configuration is not repeated in user
management.

### User Management

User management owns people and authorization:

- Add, edit, enable, disable, and remove members.
- Change passwords under the existing administrator/member rules.
- Generate, rotate, view the generated state of, and delete API tokens.
- Assign Profiles through the existing `配置` dialog.

The Profile assignment dialog shows all assignable Profiles, current selections,
and a `全选` control. A successful save closes the dialog and immediately
updates the member row without requiring a full page refresh. The page does not
render a second Profile management section.

## Profile Workbench

### Header

The workbench header stacks the Profile name and Apple account on the left.
Region and a human-readable state badge remain visible without opening
technical details. The redundant `返回首页` button remains removed because the
left navigation provides that route.

### Main Task Area

The primary panel answers three questions:

1. What is happening now?
2. What should the user do next?
3. Which actions are currently valid?

The recommendation uses Chinese product language. Raw `Decision`, `Next`,
`Ready`, Host state, Instance state, EIP, request ID, and job ID appear in a
collapsed `技术详情` section.

### Task and Reminder Panel

A secondary panel shows:

- Current background task and elapsed time.
- Current lifecycle step.
- Next retry or status-check time.
- Release reminder and auto-release state.
- Recent event entry point.

An empty panel displays `暂无进行中的任务`; it does not render an empty dark
output area.

## State and Action Model

All action availability comes from one frontend view model built from server
state, task state, role, device capability, local Agent state, and responsible
member requirements. Individual click handlers do not recalculate business
state.

| State | Recommendation | Primary action | Other available actions | Unavailable behavior |
| --- | --- | --- | --- | --- |
| `stopped` | Mac is not running | `打开` | Status, events | Connection, VNC, transfer, reminder extension, and release are disabled with `Mac 尚未就绪` |
| `creating` | Background job is opening the Mac | `查看任务详情` | Status, events | Resource mutations and local operations are disabled; the existing job is reused |
| `ready` | Mac is usable | Desktop: `连接`; mobile: `刷新状态` | VNC, transfer, reminder extension, release, status, events | `打开` is unavailable because the Mac is already ready |
| `releasing` | Instance/Host release is in progress | `查看释放进度` | Status, events | Open, release, connection, VNC, transfer, cleanup, and reminder mutation are disabled |
| `blocked` | Automation stopped for a reported reason | `重新检查` | Error details, status, events | No automatic alternate Host/type creation or EC2 termination |
| `unknown` or load failure | State must be recovered first | `刷新状态` | Configuration diagnosis, events | All resource and local operations are disabled |

The existing `ready` result remains authoritative: it means the Mac is already
usable. `releasing` must never be displayed as `creating`.

Disabled controls keep a stable layout where that aids scanning. Their reason
must be visible next to the action group or accessible through focus; it must
not depend solely on hover or a `title` attribute.

Role and ownership rules remain server-authoritative. The UI reflects them but
does not replace endpoint authorization.

## Confirmation Flows

`打开` and `释放` open purpose-built confirmation dialogs rather than exposing
separate preview and confirm buttons.

The server preview is loaded into the dialog before confirmation. The dialog
shows:

- Profile and Apple account.
- Current state and decision.
- Responsible member selection when required.
- Instance and Host identity when present.
- For release, an explicit statement that the EIP is retained.
- The exact operation to be submitted.

The confirm button remains disabled until preview succeeds and all required
fields are present. On success, the dialog closes and the workbench enters the
task state. On validation failure, the dialog stays open and shows a focused,
human-readable error.

## Background Task Feedback

### Lifecycle

The page renders the existing server-side job as:

- Requested.
- Started.
- Current step with elapsed time and retry information.
- Completed.
- Failed or blocked.

A repeated click that targets the same active Profile and operation returns the
existing task instead of creating a duplicate.

The task is restored after page refresh, sign-in, navigation, or another
browser session. The page polls task and Profile status every 10 seconds while
visible and immediately updates the action model when the effective state
changes.

### Completion

`打开成功` is shown and the WeCom notification is sent only after the effective
state reaches `ready`.

`释放完成` is shown and the WeCom notification is sent only after the effective
state reaches `stopped` and managed release completion has been recorded.

Submission success is described as `任务已提交`, never as lifecycle completion.

### Errors

Errors use two layers:

- A human-readable summary and recommended next step.
- Expandable, copyable technical details with `request_id`, `job_id`,
  `error_code`, and the sanitized underlying message.

`context.Canceled` caused by navigation or partial refresh is not displayed as
an AWS failure. A timeout is a warning with a retry action. Permission,
configuration, AWS API, and lifecycle errors are shown as failures.

Loading longer than eight seconds changes from a spinner to a recoverable state
with retry and request ID. This rule applies to login challenges, Profile
status, dialogs, member data, and task restoration.

## Local Agent Integration

Desktop connection, VNC, and transfer continue to execute through the local
Agent and local `cm` installation.

- Desktop shows these actions only when the Mac state permits them.
- A disconnected Agent produces a repair action and clear status.
- VNC checks and replaces stale tunnel state before opening Screen Sharing.
- Terminal exit or disconnect closes the terminal page.
- Transfer uses persisted per-member records and phase-aware progress.

Mobile browsers do not display connection, VNC, or transfer controls because
those actions cannot operate the user's desktop environment.

## Responsive Design

### Desktop

- Support Chrome, Safari, and Firefox at 1280, 1440, and 1920 pixel widths.
- Keep the Profile identity, state, and primary action visible without
  horizontal scrolling.
- Allow four routine operation buttons to fit without disappearing in Safari.
- Use a two-column workbench where space permits: main task and task/reminder
  context.
- Avoid large unused regions on the Profiles page by sizing the content to its
  actual configuration task.

### Mobile

- Convert Profile rows to compact cards.
- Show the Apple account only once per card.
- Keep Profile, state, Region, and management entry in the first viewport.
- Stack workbench information into one column.
- Keep the current primary lifecycle action in a stable bottom action area.
- Hide desktop-local operations instead of rendering disabled placeholders.
- Preserve confirmation dialogs, reminder controls, events, and technical
  details.

Text must wrap without overlapping adjacent content. Fixed controls use stable
dimensions so changing status labels cannot move the page structure.

## Accessibility and Interaction

- Every action is keyboard reachable.
- Focus indicators remain visible in all supported browsers.
- Dialog focus is trapped while open and returns to the triggering control when
  closed.
- State is conveyed by Chinese text as well as color.
- Icon-only controls have accessible names and visible tooltips when unfamiliar.
- Buttons use icons plus concise command labels where appropriate.
- Destructive actions have a distinct visual treatment and explicit
  confirmation.
- Success closes add/edit/configuration/password dialogs and updates the
  relevant view.
- Validation errors focus the first invalid field and preserve entered values.

## Frontend Structure

Keep `web/index.html` as the deployed entry point, but separate behavior into
clear in-file modules or existing asset files during implementation:

- `navigation`: active page, role-based visibility, history, and account entry.
- `profile-list-view`: filtering, desktop rows, mobile cards, and partial
  refresh.
- `workbench-view-model`: normalized state, recommendation, action availability,
  disabled reasons, and responsive capability flags.
- `workbench-renderer`: header, main task, reminder/task panel, and technical
  details.
- `task-controller`: job restoration, polling, progress, completion, and error
  feedback.
- `dialog-controller`: preview, validation, submission, focus, and close-on-
  success behavior.
- `local-agent-controller`: connection health and local operation entry.

These units consume existing API responses. If the frontend currently derives a
state from multiple responses, the implementation may add a read-only
presentation field to an existing response, but it must not move lifecycle
authority into JavaScript or change mutation contracts.

## Observability

Every web mutation and background transition keeps the existing structured
audit contract:

- `request_id`
- `job_id`
- `profile`
- `apple_email`
- `actor`
- `source`
- `operation`
- `status`
- `duration_ms`
- `error_code`

UI errors display the request ID but never expose session tokens, API tokens,
PEM paths, AWS credentials, webhook keys, or cookies. Audit write failures are
sent to the service log rather than silently ignored.

## Verification

### Unit and Contract Tests

- Table-driven tests cover every state/action matrix row.
- Role, ownership, local Agent, mobile capability, and responsible-member
  requirements affect actions as specified.
- `ready`, `creating`, `releasing`, `blocked`, and `unknown` labels cannot be
  interchanged.
- Duplicate submissions restore the existing job.
- Completion text and WeCom notification occur only at `ready` or `stopped`.
- Forbidden endpoints remain forbidden regardless of UI visibility.

### Browser Tests

Automated browser tests cover:

- Login challenge success, timeout recovery, retry, and failure.
- Home filtering, partial refresh, and last-updated text.
- Profile selection and browser history.
- Open and release preview/confirm dialogs.
- Persistent creating and releasing progress after reload.
- Ready-state connection, VNC, and transfer visibility on desktop.
- Local Agent disconnected repair state.
- Member add, edit, password, token, and Profile assignment dialogs.
- Save-success dialog closure and immediate row refresh.
- Keyboard navigation and dialog focus behavior.

### Visual Verification

Capture and inspect screenshots for:

- Chrome, Safari, and Firefox desktop layouts.
- 1280, 1440, and 1920 pixel widths.
- Mobile Safari and Chrome widths.
- Home, Profile workbench, Profiles, user management, dialogs, loading,
  empty, blocked, creating, ready, releasing, and error states.

Verification checks button visibility, text wrapping, overflow, overlapping,
stable dimensions, console errors, failed asset loads, and failed network
requests. Safari must show the same valid operation controls as Chrome.

### Regression Suite

- JavaScript syntax validation passes.
- The complete Go test suite passes.
- Existing AWS safety and lifecycle tests remain unchanged or are expanded.
- Local terminal, VNC tunnel recovery, and transfer workflows remain usable.
- No local PEM location or sensitive credential enters the server database or
  rendered page.

## Delivery Boundary

The implementation is complete when all confirmed page structures, state rules,
feedback behavior, responsive layouts, and verification requirements above are
present and tested. Publishing Homebrew, APT, local upgrades, and staging2
deployment are separate release steps after implementation approval and
verification.
