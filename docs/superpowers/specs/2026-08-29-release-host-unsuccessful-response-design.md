# ReleaseHosts Unsuccessful Response Handling

## Problem

AWS EC2 `ReleaseHosts` is a batch API. A request can return without a transport
error while reporting a per-host failure in `ReleaseHostsOutput.Unsuccessful`.
ConnectMac currently checks only the SDK error and therefore treats failures
such as `host is occupied` as accepted releases.

This false acceptance moves automatic release into convergence mode, suppresses
the required retry, and can leave a Dedicated Host allocated indefinitely.

## Design

### SDK response validation

`RealAWSClient.ReleaseHost` must validate the complete response:

- Return success only when the requested Host ID appears in `Successful` and no
  matching `Unsuccessful` item exists.
- Convert a matching `Unsuccessful` item into an error containing its AWS code,
  message, and Host ID.
- Treat an empty or contradictory response as an error rather than acceptance.
- Do not expose credentials or unrelated response data in the error.

The existing destroy service classifies a temporary occupied/pending failure as
a deferred Host release. It must not add that Host to `ReleasedHosts`.

### Acceptance evidence

Only jobs with explicit structured evidence (`release_evidence_recorded=true`)
may establish accepted Host convergence. Text output and legacy successful job
status are not sufficient because older clients ignored `Unsuccessful`.

If an existing reminder is in convergence mode but its source job lacks
structured evidence, the coordinator clears the inferred acceptance state and
returns the cycle to the existing retry path. The retry performs only the
remaining Host release because the EC2 instance is already terminated and the
Elastic IP is already disassociated and retained.

### State and notifications

- An occupied response remains `retrying`, not `convergence-waiting`.
- No release-success notification is sent until the Host disappears from
  managed status.
- Existing retry limits and scheduling remain unchanged.
- No workflow releases an Elastic IP allocation.

## Tests

Add coverage for:

- `Successful` containing the requested Host.
- Matching `Unsuccessful` occupied response with a nil SDK error.
- Empty and contradictory SDK responses.
- A legacy inferred convergence reminder returning to retry.
- Structured accepted evidence continuing read-only convergence.
- No duplicate EC2 termination and no Elastic IP release during recovery.

