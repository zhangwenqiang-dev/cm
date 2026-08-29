# Automatic Release Host Convergence Design

## Problem

An automatic destroy job can finish successfully after EC2 termination, Elastic IP
disassociation, and an accepted Dedicated Host release request while AWS still
returns that Host in `pending`. The coordinator currently treats every returned
Host as an active managed resource, changes the reminder to `retrying`, starts a
new destroy job every five minutes, and sends a misleading failure notification.

The observed `aaronjasonall-use1` cycle submitted eleven successful release
requests during one hour even though the first request had already been accepted.
After the retry window expired, the same completion-check failure was recorded on
every scan.

## Goals

1. Distinguish a failed release mutation from an accepted release that is waiting
   for AWS status convergence.
2. Submit at most one release mutation after AWS reports that the Host release was
   accepted.
3. Keep the UI and operation lock in `releasing` while convergence is pending.
4. Wait up to 24 hours before sending one stalled-release administrator warning.
5. Continue low-frequency read-only reconciliation after that warning and finish
   the normal success notification and cleanup when AWS converges.
6. Preserve Elastic IP allocations and all existing ownership safety checks.

## Selected Architecture

### Structured Destroy Evidence

Extend the persisted destroy-job outcome with the Host IDs for which AWS accepted
`ReleaseHosts`. Ingest that evidence into the durable Job record before deleting
the temporary outcome file. The coordinator must not parse human-readable
`run.log` output.

For jobs created by older versions, a successful or deferred destroy job is
compatible convergence evidence when all of the following hold:

- its Profile and Apple account match the current automatic-release cycle;
- no managed EC2 instance remains;
- the retained Elastic IP has no association and no instance ID;
- exactly one managed Host remains, it matches the reminder Host ID, and its
  state is `pending`.

This compatibility rule recovers the current staging2 cycle without another
mutation. It does not treat an `available` Host or an unknown Host as accepted.

### Durable Convergence Metadata

Keep the existing automatic-release states. `running` continues to mean that a
destroy job is active or that AWS is converging an accepted release. Add two
fields to the release reminder:

- `auto_release_accepted_at`: first time accepted-release evidence was observed;
- `auto_release_stalled_notified_at`: time the 24-hour warning was successfully
  delivered.

These fields must be stored by both file and MySQL member stores. Starting a new
automatic-release cycle clears both fields. No new public state or migration-only
state is introduced.

### Coordinator Flow

When a destroy job finishes:

1. Resolve the exact Profile and run the existing read-only AWS status check.
2. If all managed compute resources are gone, enter the existing durable
   completion-notification flow.
3. If the Job and AWS status prove an accepted Host release is converging,
   atomically persist `auto_release_accepted_at`, keep state `running`, clear the
   transient failure text, emit `auto-release.convergence-waiting`, and do not
   call `StartDestroy` again.
4. On later scans, a cycle with `auto_release_accepted_at` performs only status
   checks. It never starts another destroy job.
5. If AWS becomes clean, continue to `notifying`, send the existing success
   notification, clean owner/reminder data, and finish as `released`.
6. If 24 hours elapse, send one stalled-release warning. Persist its success in
   `auto_release_stalled_notified_at`; a failed webhook attempt remains eligible
   for retry.
7. After 24 hours, the worker keeps its existing cadence, but this Profile's AWS
   status check is gated to at most once every 15 minutes using the persisted
   `auto_release_last_attempt_at`. This avoids another scheduler while reducing
   AWS calls. Duplicate warning logs and database events are suppressed.

The existing one-hour mutation retry window remains in force for genuine
recoverable mutation failures where AWS has not accepted the Host release.

## User Interface

The existing effective `releasing` presentation remains authoritative while
`auto_release_accepted_at` is set and the reminder is not terminal:

- status text: `正在释放`;
- opening, connecting, VNC, and transfer actions remain disabled;
- do not display `自动释放失败，将按计划重试`;
- after 24 hours, show a concise warning that AWS Host release is taking longer
  than expected while status reconciliation continues.

No layout or unrelated styling changes are included.

## Notifications

Do not send the existing first-failure notification for an accepted release that
is merely converging. Add one notification kind for the 24-hour boundary with the
description:

`Host 释放状态超过 24 小时仍未完成，系统将继续检查，不会重复提交释放。`

The notification follows the existing Enterprise WeChat delivery, redaction,
retry, audit, and idempotency patterns. The normal automatic-release success
notification is still sent exactly once after AWS reports no managed Host or EC2
and no EIP association.

## Observability

Add structured actions:

- `auto-release.convergence-waiting` once when accepted evidence is persisted;
- `auto-release.convergence-observed` only when the observed Host state changes;
- `auto-release.convergence-stalled` once when the 24-hour boundary is reached;
- existing `wechat.pending`, `wechat.retrying`, `wechat.sent`, and
  `auto-release.released` actions for warning and success delivery.

Each event includes Profile, Apple account, Host ID, Job ID, request ID, cycle ID,
accepted timestamp, observed state, and elapsed duration where available. It must
not contain AWS credentials, webhook keys, tokens, sessions, or PEM paths.

## Failure And Safety Handling

- An active destroy job remains authoritative; do not run a parallel mutation.
- `available`, unknown, mismatched, or multiple Hosts are not convergence and
  continue through existing safety/failure handling.
- Ownership or tag mismatches remain terminal failures.
- AWS status and network errors remain recoverable and must not erase accepted
  evidence.
- Database persistence failure prevents the transition into convergence mode;
  it must be logged and retried without pretending the state was saved.
- A failed 24-hour webhook delivery is retried, but the warning is never marked
  sent until delivery succeeds.
- Elastic IP allocation is never released.

## Tests

Add deterministic tests for:

- structured Job outcome persists accepted Host IDs;
- successful destroy plus matching `pending` Host enters convergence mode;
- compatibility recovery for a legacy successful Job without accepted-host
  metadata;
- repeated scans in convergence mode never call `StartDestroy`;
- `available`, mismatched, multiple, and unknown Hosts do not enter convergence;
- convergence survives service restart through file and MySQL stores;
- no failure notification is sent during the first 24 hours;
- exactly one stalled warning is delivered after 24 hours;
- AWS status checks are limited to once every 15 minutes after 24 hours;
- failed stalled-warning delivery retries without duplicate successful delivery;
- eventual clean status sends the existing success notification and cleans the
  owner/reminder exactly once;
- UI continues to display `正在释放` and disables unsafe actions;
- genuine mutation failures retain the one-hour retry policy;
- existing automatic release, notification, AWS destroy, storage, and web tests
  remain green.

Verification commands:

```bash
go test ./internal/connectmac -run 'AutoRelease|Destroy|JobOutcome|ReleaseReminder'
go test ./...
go test -race ./...
go vet ./...
```

No test may perform a real AWS mutation or send a real Enterprise WeChat message.

## Rollout

Deploy the new version to staging2 without manually invoking release. On startup,
the legacy compatibility rule should place `aaronjasonall-use1` into convergence
observation without creating a twelfth destroy job. Verify the structured event,
the unchanged Elastic IP allocation, the disabled UI actions, and eventual
success cleanup when AWS no longer returns the Host.

## Non-Goals

- No change to the ten-minute automatic-release grace period.
- No change to manual release confirmation or ownership rules.
- No new AWS mutation or force-release mechanism.
- No Elastic IP release.
- No unrelated UI redesign.
