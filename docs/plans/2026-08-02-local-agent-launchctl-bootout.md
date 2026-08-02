# Local Agent LaunchAgent Bootout Handling

## Context

`cm local-agent install` and `cm local-agent uninstall` can fail while stopping the LaunchAgent before install or cleanup:

```text
stop launch agent: exit status 5: Boot-out failed: 5: Input/output error
Try re-running the command as root for richer errors.
```

This was observed with `cm 0.1.136` on macOS.

## Root Cause

The local-agent plist can exist at:

```text
~/Library/LaunchAgents/com.connectmac.local-agent.plist
```

while the service is not actually loaded in the current `launchctl` domain. In that state, `cm local-agent install` writes the plist and TLS material, then calls:

```bash
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.connectmac.local-agent.plist
```

macOS may return:

```text
Boot-out failed: 5: Input/output error
```

for this not-loaded service state. Current code does not classify that response as an ignorable missing-service case, so install/uninstall stops before the next step.

Manual bootstrap succeeds after the plist has been generated:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.connectmac.local-agent.plist
```

That confirms the local-agent binary and generated TLS material are valid; the failure is in LaunchAgent lifecycle handling.

## Current Workaround

When `cm local-agent install` fails with the bootout exit 5 message, check whether the service is loaded:

```bash
launchctl print gui/$(id -u)/com.connectmac.local-agent
```

If launchctl reports the service cannot be found, bootstrap the already generated plist:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.connectmac.local-agent.plist
```

Then verify:

```bash
cm local-agent status
```

Expected:

```text
local-agent is running at https://127.0.0.1:18765/health
```

## Proposed Code Change

Update `stopLocalAgentLaunchAgent` / missing-service detection in:

```text
internal/connectmac/app_local_agent.go
```

Recommended behavior:

```text
bootout failed
  if ignoreMissing is false:
    return the original error
  if bootout output already indicates service not loaded:
    return success
  if bootout output contains "Boot-out failed: 5":
    run launchctl print gui/<uid>/com.connectmac.local-agent
    if print output indicates "Could not find service":
      treat as already stopped and return success
    otherwise:
      return the original bootout error
```

Do not ignore every exit status 5. Exit 5 can also represent real LaunchAgent failures, so the fallback check must confirm the service is absent before continuing.

## Tests To Add

Add coverage for `stopLocalAgentLaunchAgent` and install/uninstall flow:

- `bootout` returns exit 5 and `Boot-out failed: 5: Input/output error`
- `ignoreMissing=true`
- fallback `launchctl print gui/<uid>/com.connectmac.local-agent` returns `Could not find service ...`
- expected result: stop returns success and install continues to bootstrap

Add a negative case:

- `bootout` returns exit 5
- fallback `launchctl print` does not indicate missing service
- expected result: preserve the original failure

## Remote Management Origin Note

The remote management UI is:

```text
https://cm.hsgitlab.xyz
```

For that hosted UI to talk to the local agent, local config must allow that exact browser origin:

```yaml
server:
  local_agent_origin: https://cm.hsgitlab.xyz
```

If `local_agent_origin` is missing or points at a different origin such as `http://127.0.0.1:8765`, the browser request to local-agent will fail CORS and the page will show:

```text
本机代理未连接
```

Verification command:

```bash
curl -k -i -H 'Origin: https://cm.hsgitlab.xyz' https://127.0.0.1:18765/health
```

Expected headers include:

```text
HTTP/2 200
access-control-allow-origin: https://cm.hsgitlab.xyz
```
