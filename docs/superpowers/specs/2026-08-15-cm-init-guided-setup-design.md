# `cm init` Guided Setup Design

## Goal

Replace the outdated sample-file generator with a small, rerunnable guided setup. A member should be able to configure ConnectMac without manually understanding `config.yaml`, while existing server, profile, PEM, and other settings remain intact.

## Current Problems

- A first run writes placeholder AMIs, EIPs, security groups, AWS resources, and an `xcode-vnc` profile that do not represent the server-managed architecture.
- The template writes `~/.ssh/example.pem`, which looks valid but usually does not exist.
- A second run exits with `config already exists`, so skipped settings cannot be completed later.
- The output still tells members to add or edit local Profile files even though shared Profiles now come from the ConnectMac server.
- Plain `cm init` gives little validation or useful next-step guidance.

## Selected Approach

Use one guided initializer for both `cm init` and the compatibility command `cm init wizard`.

The initializer owns only member-local setup:

- ConnectMac server URL
- member API token
- default SSH user
- default local PEM path
- optional AI Skill installation

It does not create shared Profiles, AWS resources, AMI mappings, EIP settings, or security-group settings.

## First Run

When the config file does not exist, `cm init` will:

1. Display the server URL `https://cm.hsgitlab.xyz`.
2. Use `ec2-user` as the default SSH user.
3. Scan regular `*.pem` files directly under `~/.ssh` and show a numbered selection list.
4. Allow `0` or an empty selection to skip the default PEM.
5. Prompt for a member token and allow it to be skipped.
6. Offer optional ConnectMac AI Skill installation.
7. Write only selected values to a minimal `~/.connectmac/config.yaml` with mode `0600`.
8. Print a status summary and concrete next steps.

The minimal file may contain:

```yaml
server:
  user_api: https://cm.hsgitlab.xyz
  token: <member-token-if-entered>

defaults:
  user: ec2-user
  identity_file: ~/.ssh/selected.pem
```

Skipped optional fields are omitted rather than written as empty placeholders.

## Repeated Runs

When a config already exists, `cm init` will not fail and will not recreate the file.

It will:

1. Parse and report the current setup status.
2. Preserve the existing `server.user_api`; only add the standard URL when the field is missing.
3. Preserve an existing `server.token` and never print its full value; initialization only prompts when the token is missing.
4. Preserve `defaults`, Profile-specific PEM mappings, local Profiles, comments, and unrelated settings unless the user explicitly chooses to change a supported initializer field.
5. Offer to fill missing token, default PEM, and AI Skill setup.
6. Allow every optional step to be skipped again.

If the user makes no changes, the existing config file remains byte-for-byte unchanged.

For safe targeted updates, the initializer will use a small YAML-aware configuration update boundary rather than rebuilding an existing file from the parsed `Config` model. This prevents comments, unknown fields, and Profile-specific settings from being dropped.

## Server Safety

The initializer must not overwrite an existing server configuration:

- Existing `server.user_api`: preserve exactly.
- Missing `server.user_api`: add `https://cm.hsgitlab.xyz`.
- Existing `server.token`: preserve without offering replacement in the initialization flow.
- Missing token: prompt, but allow skip.
- Token input and summaries: never display the complete token.

When a missing token is entered, the initializer will validate it by requesting the member's remote Profile list. A failed validation is not written. The error is shown and the user can retry or skip.

## PEM Handling

- Only regular `.pem` files directly inside `~/.ssh` are offered.
- The saved value uses a stable `~/.ssh/<name>.pem` path.
- Missing or unreadable PEM files are reported in the final status.
- There is no fallback PEM.
- Existing Profile-specific mappings such as the following remain valid and take precedence over the default:

```yaml
profiles:
  operations-use2:
    identity_file: ~/.ssh/eonebill-xcode.pem
```

Skipping the default PEM is valid; SSH-dependent commands continue to report that `identity_file` is required for Profiles without a local override.

## Token Input

Token entry should use hidden terminal input when stdin is an interactive terminal. If hidden input is unavailable, the initializer warns before accepting visible input. The implementation should reuse an existing terminal helper if present; otherwise a small maintained terminal package may be introduced only after confirming no standard-library solution fits.

## Output

The final output is concise and actionable:

```text
ConnectMac initialization

Server: https://cm.hsgitlab.xyz
SSH user: ec2-user

Configuration: ~/.connectmac/config.yaml

Checks:
OK  Server configured
OK  SSH user configured
--  Member token skipped
--  Default PEM skipped

Next:
- Generate a token in the management page, then rerun cm init
- Run cm list to view available Profiles
```

Messages may follow the CLI's existing language style, but status meanings and next steps must remain explicit.

## Compatibility

- `cm init wizard` remains accepted and invokes the same flow as `cm init`.
- Existing configuration files remain supported without migration.
- Existing local and server-managed Profile loading behavior does not change.
- `DefaultConfigTemplate()` becomes the minimal first-run representation or is replaced by an equivalent focused formatter.
- No existing Profile is deleted or rewritten by initialization.

## Error Handling

- Invalid existing YAML: report the parse error and do not modify the file.
- Config write failure: preserve the original file and report the failure.
- Token validation network failure: distinguish network failure from rejected authentication and do not overwrite an existing token.
- PEM scan failure: report it and allow the rest of initialization to continue.
- Skill installation failure: report it without rolling back valid config changes.
- Updates use an atomic temporary-file-and-rename sequence with mode `0600`.

## Testing

Add focused tests for:

- Minimal first-run config with all optional values skipped.
- First-run PEM selection and token entry.
- Existing server URL and token preservation.
- Adding the standard server URL only when missing.
- Rerun with no changes leaves the file byte-for-byte unchanged.
- Existing Profiles, Profile-specific PEM mappings, comments, and unknown fields survive targeted updates.
- Token validation success, authentication rejection, and network failure.
- PEM discovery only includes regular `~/.ssh/*.pem` files.
- Invalid existing YAML is never modified.
- `cm init wizard` remains compatible.
- AI Skill setup remains optional.

## Documentation

Update command help and README initialization examples to describe guided setup, optional token/PEM entry, rerun behavior, server-managed Profiles, and the lack of a default PEM fallback.
