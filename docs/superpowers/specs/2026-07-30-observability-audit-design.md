# ConnectMac Observability And Audit Design

## Goal

Make ConnectMac logs reliable enough to answer who performed an operation, what
resource it affected, how the operation progressed, why it failed, and whether
the final notification was delivered.

The implementation will improve the existing JSONL logs, MySQL audit events,
background job metadata, and Web UI transfer status. It will not introduce an
external logging platform.

## Scope

This design covers:

- audit-event retention and querying;
- actor attribution for Web operations;
- request and job correlation;
- AWS lifecycle completion events;
- harmless-error suppression and error classification;
- Enterprise WeChat notification observability;
- transfer progress phases;
- logging reliability and contract tests.

It does not cover:

- deploying Loki, Elasticsearch, or another external log service;
- recording SSH terminal commands or terminal contents;
- recording passwords, API tokens, cookies, PEM contents, webhook URLs, or AWS
  credentials;
- changing AWS resource lifecycle safety rules.

## Architecture

### Request Context

Every Web request will receive a generated request ID. A request-scoped context
will carry:

- `request_id`;
- authenticated member ID, email, and display name when available;
- source (`web`, `cli`, `local-agent`, `background-worker`);
- HTTP route and method;
- sanitized client IP and user agent for security events.

The server will return the request ID in `X-Request-ID`. Incoming request IDs
will not be trusted as authoritative; the server will generate its own ID.

### Structured Runtime Log

`LogEntry` will gain:

- `request_id`;
- `job_id`;
- `operation`;
- `source`;
- `phase`;
- `error_code`;
- `attempt`;
- `http_status`;
- `duration_ms` when not already present.

Existing fields remain compatible. JSONL files remain one object per line and
continue to be retained for 30 days.

Runtime logging will use a shared helper that:

1. adds request and actor context;
2. classifies errors;
3. sanitizes structured fields and message text;
4. writes JSONL;
5. writes a concise fallback message to stderr when JSONL writing fails.

Ordinary application behavior will not fail solely because diagnostic JSONL
cannot be written. Security and audit persistence failures will still be
returned to the caller when the audited mutation and audit event are required
to be atomic.

### Audit Events

`OperationEvent` will gain:

- `request_id`;
- `job_id`;
- `source`;
- `phase`;
- `target_member_email`;
- `error_code`;
- `duration_ms`.

Audit events describe state-changing or security-significant actions. Read-only
status polling is not audited unless it fails for a real operational reason.

Event names follow `domain.operation.phase`, for example:

- `auth.login.succeeded`;
- `auth.login.failed`;
- `member.password.changed`;
- `profile.access.granted`;
- `aws.open.requested`;
- `aws.open.started`;
- `aws.open.ready`;
- `aws.release.requested`;
- `aws.release.started`;
- `aws.release.stopped`;
- `wechat.notification.sent`;
- `transfer.push.finalizing`.

Passwords, token values, complete configuration YAML, PEM paths, terminal
contents, and webhook URLs are forbidden in audit messages.

## Storage And Retention

### MySQL

MySQL is the authoritative audit store on the server.

- Remove the 500-event truncation from whole-store save paths.
- Stop loading audit rows as part of general `MemberData` reads and writes.
- Query recent events directly with `ORDER BY created_at DESC, id DESC`.
- Support bounded pagination with `limit` and an opaque cursor composed from
  creation time and event ID.
- Default API limit is 50; maximum is 200.
- Delete events older than 90 days in a daily background maintenance pass.
- Add indexes required by global, Apple-email, Profile, actor, and creation-time
  queries.

Existing rows are preserved during migration. No table recreation or bulk
rewrite is allowed.

### JSON Fallback

The file-backed member store remains supported for local-only use.

- Retain events for 90 days.
- Retain at most the newest 10,000 events.
- Apply filtering during event append and save.
- Preserve the existing API response shape while adding pagination metadata
  where available.

## Audit Coverage

### Authentication And Security

Record:

- initial administrator setup;
- login success and failure;
- logout;
- administrator email change;
- self password change;
- administrator password reset for a member;
- API token generation, regeneration, and deletion;
- role and authorization rejection for protected mutations.

Login-failure events contain the submitted normalized account identifier,
request ID, sanitized client IP, and user agent. They never contain passwords or
challenge answers.

### Member And Profile Administration

Record:

- member creation and update;
- member enable and disable;
- Apple-account assignment and removal;
- Profile-access replacement, grant, and removal;
- managed Profile creation, update, enable, disable, and deletion;
- Profile owner changes;
- Web settings changes.

Messages describe changed field names and target identities. They do not contain
password hashes, API token hashes, authentication secrets, PEM paths, or full
Profile YAML.

Where practical, the mutation and audit insert share one repository transaction.
Operations that cannot be made atomic must record a runtime error if audit
persistence fails.

### AWS Lifecycle

A confirmed Web request records `requested` before starting a background job.
The created job persists:

- request ID;
- initiating member identity;
- source;
- lifecycle owner;
- notification state.

Job startup records `started`. The lifecycle reconciler records exactly one
terminal event:

- `ready` for a successful open;
- `stopped` for a successful release;
- `failed` for terminal job or lifecycle failure.

Preview operations are recorded separately with `confirmed=false` and do not
claim that AWS resources changed.

Automatic release keeps its existing state events and adopts the same request,
job, source, error-code, and notification fields.

Elastic IP allocation identifiers may be recorded, but successful release
events must state `eip_retained=true`.

### SSH, VNC, Terminal, And Transfer

The server records a user intent event when a member requests a local connection,
VNC launch, terminal session, or transfer.

The local agent records execution results with the same request ID:

- tunnel reused, replaced, started, stopped, or failed;
- VNC launched or failed;
- terminal opened and closed, including close reason and duration;
- transfer created, running, finalizing, succeeded, failed, or interrupted.

Terminal commands and terminal output are never logged.

Local execution logs remain on the member computer. Server-side intent and
transfer records provide central attribution without uploading terminal data.

Direct CLI commands continue to work without a server token. When a valid
server token is configured, state-changing CLI commands may submit a best-effort
central audit event. Local JSONL logging is always attempted.

## Error Classification

Errors receive stable codes, including:

- `request_canceled`;
- `request_timeout`;
- `config_missing`;
- `config_invalid`;
- `aws_profile_missing`;
- `aws_permission_denied`;
- `aws_capacity_unavailable`;
- `aws_api_error`;
- `storage_error`;
- `notification_error`;
- `transfer_error`;
- `authorization_denied`;
- `validation_error`.

Rules:

- `context.Canceled` caused by browser navigation is not logged as an error and
  does not create an audit failure.
- `context.DeadlineExceeded` is a warning with `request_timeout`.
- Missing server-local `~/.connectmac/config.yaml` is normal when Profiles are
  database-backed.
- Invalid existing configuration remains an error.
- AWS permission, configuration, service, and capacity errors remain errors with
  stable codes.

## Cleanup Event Policy

Automatic status reconciliation may update stale owner or reminder state, but:

- no event is inserted when no data changed;
- automatic reconciliation uses `system.cleanup.completed`;
- manual administrator cleanup uses `profile.cleanup.completed` with actor data;
- system cleanup events are excluded from the default human operation view;
- repeated status polling cannot generate duplicate cleanup events for an
  already-converged Profile.

## Enterprise WeChat Notifications

Notification observability uses:

- `wechat.notification.pending`;
- `wechat.notification.sent`;
- `wechat.notification.retrying`;
- `wechat.notification.failed`.

Each record includes request ID, job ID, Profile, event type, HTTP status,
attempt number, and stable error code. It excludes webhook URL, webhook key,
request body secrets, and raw response bodies.

The persisted job notification claim remains the idempotency mechanism.
Successful delivery records `sent` before setting `lifecycle_notified_at`.
Failures clear the claim and remain retryable. A restart resumes undelivered
notifications without sending a completed notification twice.

## Transfer Progress

Transfer progress uses explicit phases:

- `preparing`: 0%;
- `transferring`: displayed from 1% through 95%;
- `finalizing`: displayed from 96% through 99%;
- `succeeded`: 100%;
- `failed` or `interrupted`: preserve the last displayed percentage.

Raw rsync progress is mapped into the transfer range. A raw 100% from rsync does
not produce application 100% until the process exits successfully. During that
gap the UI displays `正在完成校验`.

Transfer events retain transfer ID, local job ID, member, Profile, direction,
phase, percentage, elapsed time, and sanitized error summary.

## API And UI Compatibility

- Existing event and job fields remain available.
- New fields are additive.
- `/api/events` continues to return the first page when no cursor is supplied.
- The Web UI may request additional pages without requiring a new login.
- Existing Profile and member configuration is not migrated or rewritten.
- Existing old audit rows with empty actor fields remain readable.

## Testing

### Unit Tests

- JSONL field serialization and secret redaction;
- error classification;
- missing-config and canceled-request suppression;
- transfer phase and percentage mapping;
- cursor encoding and decoding;
- file-store 90-day and 10,000-event retention.

### Repository Tests

- MySQL append does not delete existing events;
- recent-event queries are descending and paginated;
- 90-day pruning removes only expired events;
- mutation plus audit insertion is atomic where required;
- cleanup reconciliation is idempotent.

### Handler Tests

Table-driven tests verify that each sensitive mutation produces one success or
failure event with actor, target, request ID, source, and no secrets.

AWS tests verify requested, started, ready/stopped, and failed events share a
request and job ID.

Notification tests verify pending, retrying, sent, failed, restart recovery, and
no duplicate delivery.

Transfer tests verify phase display, 99% finalization behavior, success at 100%,
failure preservation, and correlation fields.

### Integration Verification

- Run `go test ./...`.
- Start a local Web server with the file store and exercise authentication,
  member/Profile mutation, transfer, and mocked AWS lifecycle paths.
- Deploy to staging2 only after unit and repository tests pass.
- Verify new events with direct read-only MySQL queries.
- Verify harmless browser cancellation does not create error logs.
- Verify notification records correlate with job metadata.

## Rollout

1. Apply additive schema migrations.
2. Deploy code with new fields and direct event queries.
3. Start daily 90-day pruning.
4. Keep existing JSONL and job files untouched.
5. Verify event insertion, pagination, and notification correlation on staging2.

Rollback is safe because schema changes are additive and existing readers ignore
unknown JSON fields. The deployment must not delete existing audit events.

## Acceptance Criteria

- Every sensitive Web mutation records actor, request ID, operation, result, and
  target without secrets.
- AWS open and release produce one terminal ready or stopped event correlated
  with their request and job.
- MySQL retains audit history for 90 days without a 500-row cap.
- Repeated stopped-state polling creates no duplicate cleanup event.
- Missing server-local config and browser cancellation do not appear as errors.
- Enterprise WeChat delivery and retries are observable and idempotent.
- Transfer progress does not show 100% before process success and clearly labels
  finalization.
- A JSONL write failure is visible in service logs.
- Existing configuration and API consumers continue to work.
