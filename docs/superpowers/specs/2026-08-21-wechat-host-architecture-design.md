# Enterprise WeChat Host Architecture Notification Design

## Goal

Simplify every Enterprise WeChat notification by removing the `ConnectMac` title prefix and the `Profile` field. Add the actual AWS Mac host architecture to successful open and automatic-release notifications.

## Notification Format

- Every notification title contains only its event description, without `ConnectMac`.
- Every notification omits the `Profile` field.
- `Mac 打开确认成功` and `Mac 自动释放成功，Elastic IP 分配已保留` include `Host 架构类型：arm64` or `Host 架构类型：x86`.
- Other notification types do not include the architecture field.
- Existing Apple account, operator, Host ID, Beijing timestamps, and management URL fields keep their current behavior.

## Architecture Source

The architecture is derived only from the instance type of the actual non-released Dedicated Host observed when open completion is finalized:

- `mac1.metal` maps to `x86`.
- Every other supported Mac instance type maps to `arm64`.

The implementation must not infer architecture from AMI configuration or `instance_type_priority`, because those values do not prove which host was allocated.

## Persistence

Add an optional host architecture field to `ReleaseReminder`. Persist it in both JSON and MySQL stores when open completion records the actual host. Reused reminders for the same host must refresh the architecture from the current AWS observation.

MySQL startup migration adds the nullable column without disrupting existing rows. Legacy JSON and MySQL reminders remain valid with an empty architecture. Notifications omit the field when it is empty rather than guessing.

Persisting the value allows the automatic-release success notification to include the architecture after AWS has released the Dedicated Host.

## Error Handling

An absent host or absent instance type does not fail lifecycle completion or notification delivery. The architecture field is simply omitted. Unsupported non-empty instance types are also omitted rather than mislabeled.

## Tests

- Verify all notification titles omit `ConnectMac` and all bodies omit `Profile`.
- Verify successful open and automatic-release notifications include the persisted architecture.
- Verify unrelated notifications omit architecture.
- Verify `mac1.metal` maps to `x86` and supported Apple Silicon types map to `arm64`.
- Verify JSON and MySQL round trips preserve the field.
- Verify legacy records without the field remain readable and do not emit an architecture line.
