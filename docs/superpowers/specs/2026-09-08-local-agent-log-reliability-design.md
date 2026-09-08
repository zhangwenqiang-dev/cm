# Local Agent Log Reliability Design

## Goal

Make local ConnectMac activity diagnosable without exporting large, sensitive
rsync transcripts. Every transfer and interactive session must have a clear
terminal outcome, actionable error classification, and bounded local storage.

## Scope

This change covers local Agent transfer, terminal, VNC, Host Key handling,
structured log retention, and `cm logs export`. It does not change AWS resource
lifecycle behavior, member permissions, or existing page layout.

## Logging layers

Structured JSONL is the primary diagnostic record. It contains lifecycle,
correlation, progress, duration, outcome, and sanitized error fields. Raw
command output is optional diagnostic material: it is isolated per operation,
size bounded, excluded from normal exports, and never required to determine an
operation's final state.

The launchd stdout and stderr files remain useful for Agent startup failures,
but command execution must not stream complete rsync file lists into them.
These files are bounded independently from structured logs.

## Transfer lifecycle

Each accepted transfer has exactly one start event and one terminal event:

- `transfer.local.succeeded`
- `transfer.local.failed`
- `transfer.local.canceled`
- `transfer.local.interrupted`

The in-memory job registry persists enough state to identify unfinished jobs.
When the Agent starts, previously running jobs are changed to interrupted with
an explicit restart reason. All worker exits use one finalization path so a
panic, canceled context, command error, or normal completion cannot omit the
terminal event.

When rsync supports `--info=progress2`, byte progress is authoritative. The
reported transfer phase progresses from preparing to transferring to
finalizing and then a terminal phase. Estimated milestones remain only as the
fallback for older rsync versions and are identified by the existing progress
mode.

## Error classification

Terminal sessions distinguish user exit, normal WebSocket closure, browser
departure, local Agent disconnection, SSH exit 255, SSH timeout, Host Key
change, and unknown command failure. Normal closure is informational.

VNC events include failure stage, local port, tunnel PID when known, command
exit code, and a bounded sanitized stderr summary. Generic `command_failed` is
used only when no narrower category can be derived.

A changed Host Key produces `host_key_changed`. Connection, terminal, VNC, and
transfer operations for that Profile remain blocked until the existing
fingerprint confirmation/fix flow succeeds. Repeated attempts return the same
actionable result without repeatedly running the underlying command.

## Retention and export

Structured logs retain the existing 30-day policy and additionally rotate by
size. Rotation must preserve complete JSON lines and use deterministic suffixes.
Agent stdout/stderr and per-operation raw logs use smaller independent caps.

`cm logs export` exports sanitized structured logs by default and excludes raw
Agent and command logs. `cm logs export --include-raw` includes those files after
streaming redaction. The output archive includes a small manifest describing
the export time, CM version when available, included file categories, and
whether raw diagnostics were included.

Redaction covers credentials, cookies, webhook URLs, PEM data and paths, local
home-directory usernames, public host/IP values, and SSH fingerprints. Profile
names, action names, timestamps, request IDs, transfer IDs, phases, percentages,
durations, and stable error codes remain available for correlation.

## Compatibility

Existing JSON fields and action names remain compatible. New fields and error
codes are additive. The explicit destination syntax remains
`cm logs export --output output.zip`. Existing callers that use
`cm logs export` receive a smaller, safer archive.

## Testing

- Every transfer exit path emits exactly one terminal event.
- Agent restart converts persisted running transfers to interrupted.
- Real progress2 input produces monotonic byte-derived progress and a finalizing
  phase; fallback progress remains identified as estimated.
- Terminal and VNC errors map to stable specific codes and retain sanitized
  diagnostic summaries.
- Host Key changes block all affected local operations until repaired.
- Size rotation preserves valid JSONL and retention removes expired generations.
- Default export omits raw logs and redacts local identity/network data.
- `--include-raw` includes bounded, redacted raw diagnostics.
- Existing local Agent, transfer, terminal, VNC, logging, and CLI tests pass.
