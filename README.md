# ConnectMac

![ConnectMac link mark](web/assets/connectmac-mark.svg)

ConnectMac is an internal CLI for managing SSH local port-forwarding profiles. It is built for commands like VNC tunnels, where a small typo in the host, key, or port can connect to the wrong place or silently fail.

The binary command is `cm`.

The shared ConnectMac link mark is `web/assets/connectmac-mark.svg`; packaged Web
managers include it automatically, and the macOS Local Agent exposes the same
mark at `http://127.0.0.1:18765/icon.svg`.

## Build

```bash
go build -o bin/cm ./cmd/cm
```

For internal installation, copy `bin/cm` to a shared tool path such as `/usr/local/bin/cm`.

Build a Debian package for Linux hosts:

```bash
make deb VERSION=0.1.76
make deb-all VERSION=0.1.76
```

Install the generated package on Ubuntu/Debian:

```bash
sudo apt install ./dist/cm_0.1.76_arm64.deb
cm version
```

Deploy a built ARM64 package to staging with background-job protection:

```bash
scripts/build-deb.sh --version 0.1.120 --arch arm64
scripts/deploy-staging.sh --version 0.1.120
```

The deploy script verifies the package checksum, then uses the incoming binary
to block new background jobs and wait up to two hours for active jobs to finish.
If verification or waiting fails, deployment stops before APT installation and
before the `connectmac` service is restarted. Override the target or timeout with
`--host <ssh-alias>` and `--timeout <duration>`.

The Debian package installs the command as `cm`, stores web assets under `/usr/share/connectmac/web`, exposes them through `/var/lib/connectmac/web`, and installs a `connectmac.service` unit. It does not start the service automatically. To run the web manager on a server:

```bash
sudo systemctl enable --now connectmac.service
```

The packaged service uses `/var/lib/connectmac` as its working directory and `HOME` so default `~/.connectmac` paths resolve even when systemd does not provide a user home environment. Optional server settings can be placed in `/etc/connectmac/.env`.

Check the installed version:

```bash
cm version
cm --version
```

## Quick Start

Create the default config:

```bash
cm init
```

Then edit the config manually and point `identity_file` at a PEM file inside your own `~/.ssh/` directory.

The default config path is:

```text
~/.connectmac/config.yaml
```

For many profiles, keep shared or important entries in `config.yaml` and put additional files under:

```text
~/.connectmac/profiles/*.yaml
```

Each file uses the same `profiles:` structure. `cm` loads `config.yaml` first, then all `.yaml` and `.yml` files in `profiles/` by filename. Duplicate profile names are rejected.
Shared `user`, `identity_file`, and region-specific AMI values can be placed in top-level `defaults:`. Profile values override defaults. `aws.creator` is intentionally not inherited from defaults; it must be supplied explicitly in the profile or by the user during confirmed AWS create/open/adopt/launch operations. For a shared team user service, set top-level `server.user_api`.

Example profile:

```yaml
server:
  user_api: https://cm.hsgitlab.xyz
  token: cm_api_xxx

defaults:
  user: ec2-user
  identity_file: ~/.ssh/example.pem
  aws:
    amis_by_region:
      us-east-1:
        mac_x86: "<us-east-1-x86-mac-ami>"
        mac_arm: "<us-east-1-arm-mac-ami>"
      us-east-2:
        mac_x86: "<us-east-2-x86-mac-ami>"
        mac_arm: "<us-east-2-arm-mac-ami>"
      us-west-2:
        mac_x86: ami-0538568e5d3653bea
        mac_arm: ami-063755aadeb97329a

profiles:
  xcode-vnc:
    description: Apple account: user@example.com
    host: mac-host.example.com
    sync:
      push:
        includes: []
        excludes:
          - xcuserdata
          - .svn
          - .git
          - .DS_Store
      pull:
        includes: []
        excludes: []
    vnc:
      username: mac-user
    aws:
      profile: cm-xcode
      region: us-west-2
      creator: ""
      resource_name: ""
      account_email: user@example.com
      key_name: example-key
      subnet_id: "<subnet-id>"
      subnets_by_az:
        usw2-az1: "<subnet-id-az1>"
        usw2-az2: "<subnet-id-az2>"
        usw2-az3: "<subnet-id-az3>"
        usw2-az4: "<subnet-id-az4>"
      security_group_id: "<security-group-id>"
      elastic_ip_allocation_id: "<elastic-ip-allocation-id>"
      elastic_ip_public_ip: "<elastic-ip-public-ip>"
      elastic_ip_owner_tag:
        key: Apple
        value: user@example.com
      availability_zone_ids:
        - usw2-az1
        - usw2-az2
        - usw2-az3
        - usw2-az4
      instance_type_priority:
        - mac2.metal
        - mac2-m2.metal
        - mac-m4.metal
        - mac2-m2pro.metal
        - mac-m4pro.metal
        - mac2-m1ultra.metal
        - mac-m4max.metal
        - mac-m3ultra.metal
      allow_intel_fallback: false
    tunnels:
      - local_port: 5900
        remote_host: localhost
        remote_port: 5900
```

`server.token` is optional for local-only use. When `server.user_api` points to a shared ConnectMac server, generate a member token in the web UI and put it here so `cm list`, `cm mcp`, and other CLI flows can read remote profiles without a browser session.

AMI defaults are resolved in this order: `profile.aws.ami`, then `defaults.aws.amis_by_region[profile.aws.region]`, then legacy `defaults.aws.ami`.

For a deployed user API, `cm web` automatically uses MySQL for members when these environment variables are present:

```env
CONNECTMAC_DB_HOST=127.0.0.1
CONNECTMAC_DB_PORT=3306
CONNECTMAC_DB_DATABASE=connectmac
CONNECTMAC_DB_USERNAME=connectmac
CONNECTMAC_DB_PASSWORD=...
CONNECTMAC_WECHAT_WEBHOOK_URL=https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...
CONNECTMAC_WEB_BASE_URL=https://cm.hsgitlab.xyz
```

Without those variables, `cm web` keeps using the local JSON member store at `~/.connectmac/members.json`.

`CONNECTMAC_WECHAT_WEBHOOK_URL` enables Enterprise WeChat notifications for Mac open confirmation, release-reminder extension, due reminders, automatic-release failures, and confirmed release. Automatic release is configured per Profile and is disabled by default. When enabled, a due reminder starts a 10-minute grace period; a member can cancel that cycle only by extending the release time to at least 10 minutes after the current time. The automatic release worker retries recoverable failures every 5 minutes for at most 1 hour. It stops immediately for non-recoverable errors and reports the reason. It never releases the Elastic IP allocation; successful events include `eip_retained=true`. Reminder and automatic-release state is persisted, so the service can recover the cycle after a restart. Staging deployments use the job drain guard and wait for these background jobs before installing or restarting the service.

## Commands

Show step-by-step guidance:

```bash
cm guide
cm guide first-use
cm guide profile
cm guide open
cm guide close
cm guide sync
cm guide vnc
cm guide mcp
```

List configured profiles:

```bash
cm list
```

Manage profile files under `~/.connectmac/profiles/`:

```bash
cm profile show xcode-vnc
cm profile wizard
cm profile add --wizard
cm profile add --name user-usw2 --apple-email user@example.com --aws-profile cm-xcode --region us-west-2 --eip 54.1.2.3 --eip-allocation-id eipalloc-example --key-name example-key --security-group-id sg-example --az usw2-az1 --subnet usw2-az1=subnet-example
cm profile rename user-usw2 user-renamed-usw2
cm profile edit user-renamed-usw2
cm profile export user@example.com
cm profile import ./profile.yaml
cm profile import ./profile.yaml --overwrite
cm profile import-dir ./profiles --overwrite
cm profile remove user-renamed-usw2
cm profile remove user-renamed-usw2 --force-local
```

`cm profile remove` only removes the local profile file and local tunnel state. It does not release AWS resources or Elastic IPs. By default it checks AWS first and blocks removal when active Mac hosts or instances still exist. Use `--force-local` only when you intentionally want to remove a local profile without checking or closing AWS resources.

`cm profile wizard` and `cm profile add --wizard` collect the profile interactively, derive `host` from Elastic IP and region when possible, warn when `identity_file` and AWS `key_name` look mismatched, print a YAML preview, and only write after confirmation.

Check local installation, config, profile basics, MCP tools, and completion visibility:

```bash
cm doctor
cm doctor --fix
cm dashboard
cm dashboard --aws
cm next user@example.com
```

`cm doctor` prints a `NEXT` column with suggested repair commands. `cm dashboard --aws` adds read-only AWS columns including readiness, the next open decision, and a suggested next command. `cm next <profile-or-apple-email>` turns the current config and AWS state into a single recommended next step. Decisions include `ready`, `wait-ready`, `launch-on-host`, `create`, `blocked`, `fix-config`, `config`, or `error`.

Check a profile before connecting:

```bash
cm check xcode-vnc
```

Start a foreground tunnel:

```bash
cm connect xcode-vnc
```

Open an interactive SSH shell:

```bash
cm ssh xcode-vnc
```

Run a non-interactive SSH command through the profile configuration:

```bash
cm exec xcode-vnc -- 'ls -ld ~/Downloads/Vitora && du -sh ~/Downloads/Vitora'
```

Start a managed background tunnel:

```bash
cm start xcode-vnc
```

Show managed background tunnels:

```bash
cm status
```

Stop a managed background tunnel:

```bash
cm stop xcode-vnc
```

Remove old SSH host fingerprints for a profile after the remote machine has been rebuilt:

```bash
cm forget-host xcode-vnc
```

Open macOS Screen Sharing for a VNC tunnel:

```bash
cm open-vnc xcode-vnc
cm setup-vnc xcode-vnc
```

Pull a remote file or directory into the current directory:

```bash
cm pull xcode-vnc ~/Desktop/App.ipa
cm pull user@example.com ~/Desktop/App.ipa
cm pull xcode-vnc ~/Desktop/App.ipa --include "*.ipa" --exclude "*.tmp"
```

Upload a local file or directory to a remote directory:

```bash
cm push xcode-vnc ./MyProject ~/Downloads/
cm push user@example.com ./MyProject ~/Downloads/
cm push xcode-vnc ./MyProject ~/Downloads/ --include "Sources/***" --exclude "DerivedData"
```

When pushing a directory, `cm` uploads it directly with rsync and applies the profile's push include/exclude rules plus any command-line filters.

After a push, use `cm exec` to verify the remote path with the same SSH host, user, and key configured in the profile:

```bash
cm exec xcode-vnc -- 'ls -ld ~/Downloads/MyProject && du -sh ~/Downloads/MyProject && find ~/Downloads/MyProject -maxdepth 1 -name .git -type d -print'
```

Use a custom config path:

```bash
cm list --config ./examples/config.yaml
```

Install shell tab completion:

```bash
cm completion zsh > ~/.zsh/completions/_cm
cm completion bash > ~/.bash_completion.d/cm
cm completion fish > ~/.config/fish/completions/cm.fish
```

Homebrew installs completion scripts automatically during `brew install cm` or `brew upgrade cm`. For manual zsh installation, ensure your `~/.zshrc` loads the completion directory:

```bash
fpath=(~/.zsh/completions $fpath)
autoload -Uz compinit
compinit
```

Completion dynamically reads configured profiles and Apple account emails, so commands like `cm ssh <Tab>` and `cm aws open <Tab>` can suggest current config values.

Initialize AI safety rules for supported agents:

```bash
cm init-rules --agent codex --project .
cm init-rules --agent claude --project .
cm init-rules --agent cursor --project .
cm init-rules --agent trae --project .
cm init-rules --agent codex --project . --skills-dir ~/.agents/skills
cm init-rules --agent cursor --project . --dry-run
cm init-rules --print-rules
```

`cm init-rules` writes the source rule file to `~/.connectmac/rules.md`, syncs the same rule block into the selected agent location, installs the `connectmac` skill, and validates the installation. Codex/Trae rules go to `AGENTS.md`, Claude rules go to `CLAUDE.md`, and Cursor rules go to `.cursor/rules/connectmac.mdc`. The skill is installed to `~/.agents/skills/connectmac` by default so supported AI tools can share it; pass `--skills-dir` to choose another skills directory. Use `--dry-run` to preview file paths without writing, and `--print-rules` to print the long-term rule content. When `cm init` creates a new config, it also asks whether to initialize AI rules. After installation, tell your AI agent to remember the content of `~/.connectmac/rules.md` exactly as long-term memory.

Start the MCP server for AI clients:

```bash
cm mcp
cm mcp tools
cm mcp tools --json
```

`cm mcp` is a stdio MCP server and waits for JSON-RPC messages on stdin. It does not print tools when run directly. Use `cm mcp tools` for a human-readable list with required and key parameters, `cm mcp tools --json` for the MCP `tools/list` result JSON, or `scripts/cm-mcp-tools` as a small local probe.

For AI clients, call `cm_mcp_guide` first when the workflow is unclear. It explains stable flows, main parameters, and preview/confirm rules without requiring a valid local config. Common tools return both readable text and `structuredContent`: `cm_dashboard`, `cm_find_profile_by_apple`, `cm_check_profile`, `cm_push`, `cm_pull`, and AWS status/open/destroy tools expose fields such as `profile`, `apple_email`, `decision`, `next`, `confirmed`, `ready`, `errors`, `includes`, `excludes`, and `eip_retained`.

Preview AWS Mac Dedicated Host automation:

```bash
cm profile accounts
cm profile find user@example.com
cm open user@example.com
cm close user@example.com
cm aws plan xcode-vnc
cm aws capacity user@example.com
cm aws running
cm aws open user@example.com
cm aws status xcode-vnc
cm aws status xcode-vnc --all
cm aws wait-ready xcode-vnc
cm aws adopt xcode-vnc
cm aws adopt-host xcode-vnc --host-id h-example
cm aws launch-on-host xcode-vnc --host-id h-example
cm aws create xcode-vnc
cm aws destroy user@example.com
cm aws destroy-many user1@example.com user2@example.com
cm aws destroy-all --except operations@example.com
```

`cm profile accounts` lists configured Apple accounts, and `cm profile find <apple-email>` shows which profile owns an Apple account. AWS commands accept either a profile name or an Apple account email. Email lookup uses `aws.account_email` and the Elastic IP `Apple` owner tag, so Apple account remains the unique operator-facing identity for AWS Mac work.

`cm aws plan` is local-only and does not call AWS APIs. `cm aws capacity` is read-only and uses the configured AWS profile and region to show Mac Dedicated Host service quotas, active host usage, remaining capacity, and instance type offering AZs for the profile's `instance_type_priority`. `cm aws running` checks configured profiles and prints the currently running AWS Mac instances as a table. `cm aws status` uses the configured AWS profile and region to describe managed Dedicated Hosts, EC2 instances, Elastic IP association, and EC2 system, instance, and optional EBS status checks. Terminal resources such as terminated instances and released hosts are hidden by default; pass `--all` to include them for troubleshooting. `cm aws open` inspects current AWS state and then chooses the safe next action: report ready, wait for readiness, launch EC2 on an available managed host, or create a new host and instance. `cm aws wait-ready` waits until the managed EC2 instance is running, the Elastic IP is bound to that instance, and system/instance status checks are `ok`; EBS status must be `ok` only when AWS reports it for that instance type. `cm aws adopt-host` tags an existing empty Dedicated Host as managed, and `cm aws launch-on-host` launches EC2 on a usable existing host. `cm aws open`, `cm aws create`, `cm aws adopt-host`, `cm aws launch-on-host`, `cm aws destroy`, `cm aws destroy-many`, and `cm aws destroy-all` preview by default; pass `--confirm` to execute AWS mutations. After a confirmed open/create/launch-on-host, `cm` waits for AWS readiness checks before reporting the Mac ready; it does not run SSH probes during this wait.

`cm aws destroy` disassociates the configured Elastic IP from the managed instance but keeps the Elastic IP allocation. Destroy previews show the matching instance, host, and retained EIP before any mutation. During confirmed destroy, `cm` prints EC2 termination progress, retries pending Dedicated Host release while AWS finishes the Mac host transition, and prints a final status check after the release attempt. `cm aws destroy-many` releases specific Apple accounts/profiles in order. `cm aws destroy-all --except <profile-or-apple-email>` previews or releases all active configured Mac compute except the excluded account/profile.

Use `aws.creator` to tag who originally created the Mac. AWS already records resource creation and launch times, so `cm` does not write a separate creator-date tag. Use `aws.account_email` for the Apple account email. Leave `aws.resource_name` empty for new resources so `cm` generates `xcode-<account-email>`. Set `aws.resource_name` only when adopting resources that were created before `cm` managed them.

Confirmed AWS create/open/adopt/launch commands require `aws.creator`; if it is missing, the CLI prompts and stops when no value is entered. MCP tools return a user-input-required result instead; the AI must ask the user for creator and must not infer it from old conversation context or defaults. `cm` does not create missing key pairs or change security group ingress automatically; those actions require explicit user confirmation in separate AWS setup steps.

AWS credentials are read through the normal AWS SDK credential chain. Keep access keys in `~/.aws/credentials`, AWS SSO, environment variables, or IAM roles. Do not put AWS secret keys in `~/.connectmac/config.yaml`.

## Safety Checks

Before starting SSH, `cm` checks:

- The named profile exists.
- `user`, `host`, and `identity_file` are configured directly or through `defaults:`.
- The private key path is under `~/.ssh/`.
- The private key file exists.
- The private key file is not group/world-readable on Unix-like systems.
- The local tunnel port is available.
- The system `ssh` executable is available.
- The system `rsync` executable is available for `pull` and `push`.

The generated SSH command includes:

```bash
-o ExitOnForwardFailure=yes
-o ServerAliveInterval=30
-o ServerAliveCountMax=3
```

`ExitOnForwardFailure=yes` is especially important: if local forwarding fails, SSH exits instead of pretending the tunnel is usable.

## Rebuilt Remote Hosts

If a remote machine is destroyed and recreated, SSH may reject the connection because the old host fingerprint in `~/.ssh/known_hosts` no longer matches.

Use:

```bash
cm forget-host xcode-vnc
```

This runs:

```bash
ssh-keygen -R <profile-host>
```

Then connect again and confirm the new fingerprint when SSH prompts you. This command is explicit on purpose; normal `connect`, `start`, `pull`, and `push` commands do not silently remove host identity checks.

## Opening VNC

`cm open-vnc <profile>` opens macOS Screen Sharing using the first configured tunnel's local port.

```bash
cm open-vnc xcode-vnc
```

With this config:

```yaml
vnc:
  username: mac-user
tunnels:
  - local_port: 5900
    remote_host: localhost
    remote_port: 5900
```

It runs:

```bash
open "vnc://mac-user@localhost:5900"
```

Do not put VNC passwords in the config. Let macOS Screen Sharing and Keychain remember the password after the first successful login.

## PEM File Rule

Store PEM files only under `~/.ssh/`:

```bash
mkdir -p ~/.ssh
mv example.pem ~/.ssh/
chmod 600 ~/.ssh/example.pem
```

Do not keep PEM files in project folders, Desktop, Downloads, shared folders, or repositories. `cm check`, `cm connect`, `cm start`, `cm pull`, and `cm push` reject `identity_file` paths outside `~/.ssh/`.

## Rsync Transfers

`cm pull` reads the SSH target from the selected profile or Apple email and downloads into the current directory:

```bash
cm pull xcode-vnc ~/Desktop/file.zip
cm pull user@example.com ~/Desktop/file.zip
```

This runs rsync against:

```text
user@<profile-host>:~/Desktop/file.zip -> .
```

`cm push` uploads a file directly:

```bash
cm push xcode-vnc ./build.zip ~/Downloads/
cm push user@example.com ./build.zip ~/Downloads/
```

For directories, `cm push` uploads the directory directly with rsync. By default, these paths are excluded:

```text
xcuserdata
.svn
.git
.DS_Store
```

You can configure push and pull includes/excludes separately per profile. When `includes` is non-empty, `cm` appends a final `--exclude "*"` so only matching include patterns are transferred; `excludes` are still applied before that final catch-all.

```yaml
profiles:
  xcode-vnc:
    sync:
      push:
        includes:
          - "Sources/***"
          - "*.xcodeproj/***"
        excludes:
          - xcuserdata
          - .svn
          - .git
          - .DS_Store
          - docs
          - "*.md"
      pull:
        includes:
          - "*.ipa"
          - "*.log"
        excludes:
          - .DS_Store
```

## MCP Server

`cm mcp` starts a stdio MCP server for AI clients. It waits for MCP JSON-RPC on stdin and may appear to print nothing when run directly. To discover tools without an MCP client, run:

```bash
cm mcp tools
cm mcp tools --json
scripts/cm-mcp-tools
```

Available tools:

```text
cm_list_profiles
cm_find_profile_by_apple
cm_check_profile
cm_push
cm_pull
cm_forget_host
cm_aws_plan
cm_aws_capacity
cm_aws_status
cm_aws_wait_ready
cm_aws_create_mac
cm_aws_open_mac_by_email
cm_aws_adopt_mac
cm_aws_adopt_host
cm_aws_launch_on_host
cm_aws_destroy_mac
cm_aws_destroy_mac_by_email
```

Tools with side effects require `confirm: true`. Without confirmation, they return a preview only.

Example tool arguments:

```json
{
  "profile": "xcode-vnc",
  "local_path": "/Users/example/project",
  "remote_dir": "~/Documents/",
  "confirm": true
}
```

`cm_ssh`, `cm_start`, `cm_stop`, and `cm_open_vnc` are intentionally not exposed through MCP.

## Logs And Audit Events

ConnectMac keeps operational logs in these locations:

```text
Local JSONL: ~/.connectmac/logs/cm-YYYY-MM-DD.log, retained for 30 days
Server JSONL: /var/lib/connectmac/.connectmac/logs, retained for 30 days
MySQL audit: cm_events, retained for 90 days
Job metadata/logs: /var/lib/connectmac/.connectmac/jobs
Service log: journalctl -u connectmac
```

JSONL and audit records include correlation fields such as `request_id`,
`job_id`, `profile`, `source`, `phase`, `duration_ms`, and `error_code`.
Passwords, tokens, webhook URLs, PEM contents, SSH commands, terminal contents,
and transferred file contents are not logged.

The authenticated event API returns newest events first and preserves the
legacy `data.events` field:

```text
GET /api/events?limit=50&profile=<profile>&apple_email=<email>&cursor=<cursor>
```

Use `data.next_cursor` to request the next page. The maximum page size is 200.
Administrators may add `include_system=1`; other members only receive events
for Profiles assigned to them.
