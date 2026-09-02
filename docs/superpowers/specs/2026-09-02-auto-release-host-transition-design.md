# Auto-release Host transition handling

## Goal

Make automatic release treat the AWS Mac Dedicated Host cleanup interval as a
normal asynchronous phase instead of an immediate release failure. The UI must
remain in `releasing`, and WeCom must receive one completion notification only
after compute resources are clean.

## State model

- A host in `released` is terminal and does not count as a remaining managed
  resource, even when AWS still returns it from `DescribeHosts`.
- `Client.InvalidHost.Occupied` after every managed EC2 instance is terminated
  is a recoverable Host cleanup transition.
- During that transition, the current release cycle remains running/releasing.
  It does not emit the first-failure notification.
- The existing one-hour mutation retry window remains the terminal boundary.
  A recoverable transition that outlives that window becomes a real failure.
- Elastic IP allocations remain retained and are never released.

## Execution flow

1. Terminate and wait for managed EC2 instances.
2. Attempt Dedicated Host release only after instances are terminal.
3. If AWS reports the Host as pending or occupied, retry within the same destroy
   job using bounded backoff.
4. Record structured release evidence when AWS accepts release or reports the
   Host as terminal.
5. The coordinator verifies that no active Host or instance remains, sends one
   success notification using the existing release cycle identity, and marks
   the reminder released.

## Observability

Use explicit transition events rather than a failure event for the AWS cleanup
window. Logs must distinguish Host cleanup waiting, retry, accepted release,
terminal failure, and notification completion. Existing job ID, cycle ID,
profile, attempt, and actor/source fields remain the correlation contract.

## Compatibility

Manual destroy behavior, preview/confirmation rules, Elastic IP retention,
database schema, and public CLI output remain compatible. The change is limited
to release state classification, retry scheduling, notification timing, and
their regression tests.

## Tests

- A returned `released` Host is excluded from active managed resources.
- `InvalidHost.Occupied` with no live managed EC2 remains releasing and sends no
  first-failure notification.
- The same condition beyond the one-hour boundary becomes a final failure.
- A later successful Host release sends exactly one success notification.
- Non-transition AWS errors retain their existing failure behavior.
