package connectmac

import (
	"context"
	"fmt"
	"io"
	"strings"
)

type doctorCheck struct {
	Name   string
	OK     bool
	Detail string
}

func (a App) runGuide(args []string) int {
	topic := "overview"
	if len(args) > 1 {
		fmt.Fprintln(a.Err, "usage: cm guide [first-use|profile|open|close|sync|vnc|mcp]")
		return 2
	}
	if len(args) == 1 {
		topic = strings.ToLower(args[0])
	}
	text, ok := guideText(topic)
	if !ok {
		fmt.Fprintf(a.Err, "unknown guide topic %q\n", topic)
		fmt.Fprintln(a.Err, "usage: cm guide [first-use|profile|open|close|sync|vnc|mcp]")
		return 2
	}
	fmt.Fprint(a.Out, text)
	return 0
}

func guideText(topic string) (string, bool) {
	switch topic {
	case "overview", "":
		return `ConnectMac guide

Common paths:
1. First use:       cm guide first-use
2. Add profile:     cm profile wizard
3. Check next step: cm next <profile-or-apple-email>
4. Open Mac:        cm aws open <apple-email>
5. Release Mac:     cm aws destroy <apple-email>
6. Upload files:    cm push <profile-or-apple-email> <local-path> <remote-dir>
7. Pull files:      cm pull <profile-or-apple-email> <remote-path>
8. VNC setup:       cm setup-vnc <profile>
9. AI/MCP:          cm guide mcp

Tip: AWS mutations always preview first. Add --confirm only after reviewing the preview.
`, true
	case "first-use", "first", "init":
		return `ConnectMac first-use guide

1. Run the guided, rerunnable setup:
   cm init
   Obtain a member token from the ConnectMac server if needed and configure it when prompted.
   If you skip the token or PEM, rerun cm init to complete setup later.

2. List shared server Profiles:
   cm list
   Shared Profiles come from the ConnectMac server.

3. Configure local access:
   Use local config only for member PEM overrides.
   Rerun cm init to select a PEM under ~/.ssh/.

4. Optionally set up the ConnectMac AI Skill when cm init offers it.

5. Check local setup:
   cm doctor --fix

6. Ask cm what to do next:
   cm next <profile-or-apple-email>
`, true
	case "profile", "add-profile", "wizard":
		return `ConnectMac profile guide

Interactive creation:
  cm profile wizard

Non-interactive creation:
  cm profile add --name <profile> --apple-email <email> --aws-profile <aws-profile> --region <region> --key-name <key> --security-group-id <sg-id> --eip-allocation-id <eipalloc-id> --eip <public-ip> --az <az-id>

Review an existing profile:
  cm profile show <profile-or-apple-email>

Find by Apple account:
  cm profile find <apple-email>
`, true
	case "open", "start", "create":
		return `ConnectMac open-Mac guide

1. Preview the operation:
   cm aws open <apple-email>

2. Review Decision and Next.
   - ready: no AWS resource creation is needed.
   - wait-ready: a managed instance exists; wait for AWS checks.
   - launch-on-host: EC2 can be launched on an existing available Dedicated Host.
   - create: a new Dedicated Host and EC2 would be created.
   - blocked: stop and fix the reported reason.

3. Confirm only after review:
   cm aws open <apple-email> --confirm

4. After ready, connect:
   cm ssh <profile>
   cm start <profile>
   cm open-vnc <profile>
`, true
	case "close", "destroy", "release":
		return `ConnectMac release-Mac guide

1. Preview release:
   cm aws destroy <apple-email>

2. Confirm only after review:
   cm aws destroy <apple-email> --confirm

Safety:
- Elastic IP allocations are retained.
- Managed EC2 may be terminated.
- Dedicated Host release may be deferred until AWS allows it.
- If release is deferred, run the same destroy command again later.
`, true
	case "sync", "push", "pull":
		return `ConnectMac sync guide

Upload to remote:
  cm push <profile-or-apple-email> <local-path> <remote-dir>

Pull from remote:
  cm pull <profile-or-apple-email> <remote-path> [local-dir]

Filters:
  cm push <profile> ./build ~/Downloads/ --include "*.zip" --exclude "*.tmp"
  cm pull <profile> ~/Downloads/app.zip . --exclude ".git"

Default excludes include xcuserdata, .svn, .git, and .DS_Store.
`, true
	case "vnc", "screen-sharing":
		return `ConnectMac VNC guide

1. Configure remote macOS Screen Sharing manually:
   cm setup-vnc <profile>

2. Start local SSH tunnel:
   cm start <profile>

3. Open macOS Screen Sharing:
   cm open-vnc <profile>

Stop the local tunnel:
   cm stop <profile>
`, true
	case "mcp", "ai":
		return `ConnectMac MCP guide

Show tools:
  cm mcp tools
  cm mcp tools --json

Use in AI:
- Prefer MCP tools when available.
- Use cm_guide for step-by-step help and cm_next for the next safe action.
- Always provide an explicit Apple account email for open/create/destroy.
- For create/open/adopt/launch, creator must come from explicit user input or the profile; never infer it from old context or defaults.
- Preview first; confirm only after user approval.
- If MCP tools are hidden, use CLI fallback:
  cm profile accounts
  cm next <profile-or-apple-email>
  cm aws open <apple-email>
  cm aws destroy <apple-email>
`, true
	default:
		return "", false
	}
}

func (a App) runNext(ctx context.Context, cfg Config, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(a.Err, "usage: cm next <profile-or-apple-email>")
		return 2
	}
	profile, err := resolveProfileRef(cfg, args[0])
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	fmt.Fprintf(a.Out, "Next step for profile %s\n", profile.Name)
	fmt.Fprintf(a.Out, "Apple account: %s\n", emptyTableValue(profile.AWS.AccountEmail))
	fmt.Fprintf(a.Out, "Region: %s\n", emptyTableValue(profile.AWS.Region))

	accessErrs := a.Validator.ValidateAccess(profile)
	awsErrs := a.Validator.ValidateAWSProfile(profile)
	if len(accessErrs) > 0 || len(awsErrs) > 0 {
		fmt.Fprintln(a.Out, "Decision: fix-config")
		if len(accessErrs) > 0 {
			fmt.Fprintln(a.Out, "Local access issues:")
			writeErrorBullets(a.Out, accessErrs)
		}
		if len(awsErrs) > 0 {
			fmt.Fprintln(a.Out, "AWS config issues:")
			writeErrorBullets(a.Out, awsErrs)
		}
		fmt.Fprintf(a.Out, "Next: cm profile edit %s\n", profile.Name)
		fmt.Fprintf(a.Out, "Then: cm check %s\n", profile.Name)
		return 0
	}

	_, status, err := a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: false})
	if err != nil {
		fmt.Fprintln(a.Out, "Decision: inspect-aws")
		fmt.Fprintf(a.Out, "AWS status error: %v\n", err)
		fmt.Fprintf(a.Out, "Next: cm aws status %s\n", profile.Name)
		return 1
	}
	action := AWSOpenAction(status)
	fmt.Fprintf(a.Out, "Decision: %s\n", action.Kind)
	if action.Detail != "" {
		fmt.Fprintf(a.Out, "Detail: %s\n", action.Detail)
	}
	fmt.Fprintf(a.Out, "Ready: %t\n", AWSStatusReady(status))
	fmt.Fprintf(a.Out, "Next: %s\n", AWSOpenDecisionNextStep(profile.Name, action))
	switch action.Kind {
	case "ready":
		fmt.Fprintf(a.Out, "After tunnel: cm open-vnc %s\n", profile.Name)
	case "wait-ready":
		fmt.Fprintf(a.Out, "After ready: cm start %s\n", profile.Name)
	case "launch-on-host", "create":
		fmt.Fprintln(a.Out, "Preview first. Add --confirm only after reviewing the AWS mutation.")
	case "blocked":
		fmt.Fprintln(a.Out, "Stop here and fix the blocking reason before continuing.")
	}
	return 0
}

func writeErrorBullets(out io.Writer, errs []error) {
	for _, err := range errs {
		fmt.Fprintf(out, "- %s\n", err)
	}
}

func doctorAction(item doctorCheck) string {
	if item.OK {
		return "-"
	}
	switch item.Name {
	case "config file":
		return "cm init"
	case "profiles dir":
		return "cm doctor --fix"
	case "ssh executable":
		return "install OpenSSH"
	case "rsync executable":
		return "install rsync"
	case "config parse":
		return "cm profile edit <profile>"
	case "duplicate Apple email":
		return "cm profile accounts"
	default:
		if strings.HasPrefix(item.Name, "profile ") {
			name := strings.TrimPrefix(item.Name, "profile ")
			return "cm profile edit " + name
		}
		return "inspect manually"
	}
}
