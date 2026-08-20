# Auto Release Completion Notification Reliability Design

## Goal

Guarantee that a successful automatic Mac release eventually produces exactly one Enterprise WeChat completion notification and then clears its owner and reminder records. Temporary worker stalls, AWS status errors, webhook failures, and service restarts must not lose the completion notification.

This design addresses the observed `aaronjasonall-use1` incident: the destroy job completed successfully, EC2 and the Dedicated Host were removed, and the Elastic IP allocation was retained, but no `auto-release.released` or completion-notification event followed.

## Required Invariant

For an automatic-release cycle, once AWS reports all managed compute resources stopped:

1. Persist that completion notification is pending.
2. Retry the Enterprise WeChat notification until it succeeds or reaches an explicitly visible terminal failure policy.
3. Record exactly one successful completion notification.
4. Clear the owner and release-reminder records only after notification success.
5. Never release the Elastic IP allocation.

The system must recover this sequence after a process or server restart without requiring a user to repeat an operation.

## Selected Architecture

### Independent Background Loops

Run Web AWS lifecycle reconciliation and automatic-release reconciliation in separate background loops. A slow or blocked Web lifecycle scan must not prevent the one-minute automatic-release scan from observing a completed destroy job.

Each scan receives its own bounded context. A timeout or error ends only the current scan, emits a structured diagnostic event, and allows the next scheduled scan to run. Worker startup and shutdown remain tied to the `cm web` service context.

### Persisted Auto-Release Completion State

Continue using the release reminder as the durable state machine:

- `running`: a destroy job is active or awaiting observation.
- `notifying`: AWS resources are confirmed clean and completion notification is pending.
- `released`: notification succeeded and cleanup completed.
- `retrying`: a recoverable release or notification check failed and will be retried.
- `failed`: the bounded release retry policy reached a terminal failure.

Transition from `running` to `notifying` must be persisted before calling the webhook. This makes notification recovery independent of the lifetime of the scan that observed AWS completion.

The state identity is the immutable automatic-release cycle tuple: Profile, Apple account, Host ID, automatic-release start time, and scheduled automatic-release time. Updates must reject a stale cycle.

### Notification Idempotency

Use the automatic-release cycle identity plus notification kind `auto-release-success` as the idempotency key. Persist notification attempt and success state before cleanup. Repeated scans may retry a pending notification but must not emit a second successful completion event after success is recorded.

The webhook request itself cannot provide transactional exactly-once delivery. ConnectMac therefore guarantees durable at-least-once attempts and application-level single completion state. A crash after WeChat accepts the request but before the local success write may produce a duplicate message; logs must expose this rare ambiguity instead of silently losing the notification.

## Reconciliation Flow

For every reminder in `running`:

1. Check whether a matching destroy job is still active.
2. If active, leave the state unchanged.
3. If terminal, resolve the exact stored Profile and Apple account.
4. Run a read-only AWS status check.
5. Confirm there are no managed EC2 instances, no managed Dedicated Hosts, and no EIP association to the managed instance.
6. Atomically change the reminder to `notifying`.
7. Attempt the completion notification.
8. After notification success, atomically complete cleanup and emit `auto-release.released`.

For every reminder already in `notifying`, skip job and mutation logic. Recheck the clean AWS state, retry the notification when due, and finish cleanup after success.

A successful destroy process is supporting evidence, not sufficient completion by itself. AWS read state remains authoritative.

## Compatibility Recovery

Existing reminder cycles may be left in `running` with a successful destroy job and no lifecycle metadata, as happened with `aaronjasonall-use1`. The automatic-release coordinator must discover these jobs from their Profile, Apple account, start time, and type; it must not require Web lifecycle fields.

On service startup, the automatic-release loop performs an immediate scan before waiting for its first timer tick. A compatible stale `running` cycle with clean AWS resources advances to `notifying` and resumes notification delivery.

## Error Handling

- AWS status timeout or transient API failure: keep durable state and retry on the next scan.
- WeChat timeout, non-success response, or temporary network failure: keep `notifying`, record the attempt, and retry.
- Profile or Apple-account mismatch: stop the cycle as a visible safety failure; never infer another account.
- Managed resources reappear while `notifying`: do not notify or clean records; emit a safety error for investigation.
- Database persistence failure: do not send the notification until the pending state is durably stored.
- Cleanup failure after notification success: retain a durable notification-success marker and retry cleanup without resending.

The existing one-hour limit applies to AWS release attempts. It must not silently discard a completion notification for a release that AWS has already completed. Notification and post-notification cleanup retry independently until resolved or explicitly marked for administrator intervention.

## Observability

Every scan and state transition includes `profile`, `apple_email`, `job_id` when available, cycle ID, attempt, duration, source, phase, and error code.

Required actions:

- `auto-release.scan.started`
- `auto-release.scan.completed`
- `auto-release.scan.timeout`
- `auto-release.job.observed`
- `auto-release.notification-pending`
- `wechat.pending`
- `wechat.sent`
- `wechat.retrying`
- `wechat.failed`
- `auto-release.cleanup.retrying`
- `auto-release.released`

Logs and events must never contain the webhook URL, webhook key, AWS secrets, session token, or PEM path.

## Testing

Add deterministic tests covering:

- A successful automatic destroy job with clean AWS state enters `notifying`, sends success notification, cleans records, and emits `released`.
- The `aaronjasonall-use1` compatibility shape: successful job, zero Web lifecycle fields, reminder still `running`.
- A blocked Web lifecycle scan does not stop automatic-release scans.
- Scan timeout is logged and the next scheduled scan still executes.
- Service restart resumes `running` and `notifying` cycles.
- WeChat failure persists pending state and succeeds on a later retry.
- Repeated scans after notification success do not resend.
- Cleanup failure retries cleanup without resending notification.
- AWS resources reappearing prevents notification and cleanup.
- EIP allocation remains retained in every completion path.

Run the focused coordinator and worker tests, followed by:

```bash
go test ./...
go test -race ./...
go vet ./...
```

No test may perform a real AWS mutation or send a real Enterprise WeChat message.

## Rollout And Verification

1. Implement independent worker loops and bounded scans.
2. Strengthen durable notification and cleanup transitions.
3. Add compatibility and failure-recovery tests.
4. Deploy to staging2 without triggering an AWS mutation.
5. Verify startup reconciliation, structured logs, and current reminder state.
6. During the next real automatic release, verify the complete chain: destroy success, clean AWS status, notification pending, WeChat sent, cleanup, and released event.

## Non-Goals

- No change to the ten-minute grace period.
- No change to AWS resource ownership rules.
- No automatic opening or creation of Mac resources.
- No Elastic IP release.
- No completion notification before AWS reports the stopped state.
