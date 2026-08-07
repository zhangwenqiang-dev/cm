package connectmac

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultRulesPath = "~/.connectmac/rules.md"
	SkillName        = "connectmac"
	rulesStart       = "<!-- BEGIN CONNECTMAC RULES -->"
	rulesEnd         = "<!-- END CONNECTMAC RULES -->"
	oldRulesStart    = "<!-- BEGIN CONNECTMAC AWS RULES -->"
	oldRulesEnd      = "<!-- END CONNECTMAC AWS RULES -->"
)

type SkillSetupOptions struct {
	Agent      string
	ProjectDir string
	SkillsDir  string
	DryRun     bool
}

type RulesInstall struct {
	Agent      string
	ProjectDir string
	SourcePath string
	AgentPath  string
	SkillPath  string
}

type RulesInstallResult struct {
	Agent      string
	SourcePath string
	AgentPath  string
	SkillPath  string
	Validated  bool
}

func BuildRulesInstall(agent, projectDir string) (RulesInstall, error) {
	return BuildRulesInstallWithOptions(SkillSetupOptions{Agent: agent, ProjectDir: projectDir})
}

func BuildRulesInstallWithOptions(options SkillSetupOptions) (RulesInstall, error) {
	agent := normalizeAgentName(options.Agent)
	if agent == "" {
		return RulesInstall{}, fmt.Errorf("agent is required; choose Codex, Claude, Trae, or Cursor")
	}
	projectDir := options.ProjectDir
	if projectDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return RulesInstall{}, fmt.Errorf("find current directory: %w", err)
		}
		projectDir = wd
	}
	projectDir, err := ExpandPath(projectDir)
	if err != nil {
		return RulesInstall{}, err
	}
	sourcePath, err := ExpandPath(DefaultRulesPath)
	if err != nil {
		return RulesInstall{}, err
	}
	agentPath, err := agentRulesPath(agent, projectDir)
	if err != nil {
		return RulesInstall{}, err
	}
	skillPath, err := connectMacSkillPath(options.SkillsDir)
	if err != nil {
		return RulesInstall{}, err
	}
	return RulesInstall{
		Agent:      agent,
		ProjectDir: projectDir,
		SourcePath: sourcePath,
		AgentPath:  agentPath,
		SkillPath:  skillPath,
	}, nil
}

func InstallRules(install RulesInstall) (RulesInstallResult, error) {
	return InstallRulesWithVersion(install, "dev")
}

func InstallRulesWithVersion(install RulesInstall, version string) (RulesInstallResult, error) {
	manager, err := skillManagerForPath(install.SkillPath, version)
	if err != nil {
		return RulesInstallResult{}, err
	}
	status := manager.Status()
	if status.Name == SkillStatusOutdated {
		if _, err := manager.Update(false, false); err != nil {
			return RulesInstallResult{}, err
		}
	} else if _, err := manager.Install(false); err != nil {
		return RulesInstallResult{}, err
	}
	rules := DefaultRulesTemplate()
	if err := os.MkdirAll(filepath.Dir(install.SourcePath), 0o700); err != nil {
		return RulesInstallResult{}, err
	}
	if err := os.WriteFile(install.SourcePath, []byte(rules), 0o600); err != nil {
		return RulesInstallResult{}, fmt.Errorf("write rules source: %w", err)
	}
	block := markedRulesBlock(rules)
	if err := os.MkdirAll(filepath.Dir(install.AgentPath), 0o755); err != nil {
		return RulesInstallResult{}, err
	}
	content := ""
	if data, err := os.ReadFile(install.AgentPath); err == nil {
		content = string(data)
	} else if !os.IsNotExist(err) {
		return RulesInstallResult{}, fmt.Errorf("read agent rules: %w", err)
	}
	content = upsertMarkedBlock(content, block)
	if err := os.WriteFile(install.AgentPath, []byte(content), 0o644); err != nil {
		return RulesInstallResult{}, fmt.Errorf("write agent rules: %w", err)
	}
	result := RulesInstallResult{Agent: install.Agent, SourcePath: install.SourcePath, AgentPath: install.AgentPath, SkillPath: install.SkillPath}
	if err := ValidateRulesInstall(result); err != nil {
		return RulesInstallResult{}, err
	}
	result.Validated = true
	return result, nil
}

func ValidateRulesInstall(result RulesInstallResult) error {
	source, err := os.ReadFile(result.SourcePath)
	if err != nil {
		return fmt.Errorf("validate rules source: %w", err)
	}
	if !strings.Contains(string(source), "ConnectMac AI Rules") {
		return fmt.Errorf("validate rules source: missing ConnectMac AI Rules")
	}
	agent, err := os.ReadFile(result.AgentPath)
	if err != nil {
		return fmt.Errorf("validate agent rules: %w", err)
	}
	agentText := string(agent)
	if strings.Count(agentText, rulesStart) != 1 || strings.Count(agentText, rulesEnd) != 1 {
		return fmt.Errorf("validate agent rules: expected exactly one ConnectMac marker block")
	}
	skill, err := os.ReadFile(filepath.Join(result.SkillPath, "SKILL.md"))
	if err != nil {
		return fmt.Errorf("validate skill: %w", err)
	}
	if !strings.Contains(string(skill), "name: connectmac") {
		return fmt.Errorf("validate skill: missing connectmac name")
	}
	if _, err := os.Stat(filepath.Join(result.SkillPath, "agents", "openai.yaml")); err != nil {
		return fmt.Errorf("validate skill metadata: %w", err)
	}
	return nil
}

func normalizeAgentName(agent string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "codex", "claude", "trae", "cursor":
		return strings.ToLower(strings.TrimSpace(agent))
	default:
		return ""
	}
}

func connectMacSkillPath(skillsDir string) (string, error) {
	if skillsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		skillsDir = filepath.Join(home, ".agents", "skills")
	}
	skillsDir, err := ExpandPath(skillsDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(skillsDir, SkillName), nil
}

func InstallSkill(skillPath string) error {
	manager, err := skillManagerForPath(skillPath, "dev")
	if err != nil {
		return err
	}
	_, err = manager.Install(false)
	return err
}

func skillManagerForPath(skillPath, version string) (SkillManager, error) {
	statePath, err := ExpandPath(defaultSkillStatePath)
	if err != nil {
		return SkillManager{}, err
	}
	return SkillManager{SkillPath: skillPath, StatePath: statePath, Version: version, Now: time.Now}, nil
}

func agentRulesPath(agent, projectDir string) (string, error) {
	switch agent {
	case "codex":
		return filepath.Join(projectDir, "AGENTS.md"), nil
	case "claude":
		return filepath.Join(projectDir, "CLAUDE.md"), nil
	case "trae":
		return filepath.Join(projectDir, "AGENTS.md"), nil
	case "cursor":
		return filepath.Join(projectDir, ".cursor", "rules", "connectmac.mdc"), nil
	default:
		return "", fmt.Errorf("unsupported agent %q; choose Codex, Claude, Trae, or Cursor", agent)
	}
}

func markedRulesBlock(rules string) string {
	return rulesStart + "\n" + strings.TrimSpace(rules) + "\n" + rulesEnd + "\n"
}

func upsertMarkedBlock(content, block string) string {
	content = strings.TrimRight(content, "\n")
	start := strings.Index(content, rulesStart)
	end := strings.Index(content, rulesEnd)
	if start < 0 || end < start {
		start = strings.Index(content, oldRulesStart)
		end = strings.Index(content, oldRulesEnd)
	}
	if start >= 0 && end >= start {
		if strings.HasPrefix(content[start:], oldRulesStart) {
			end += len(oldRulesEnd)
		} else {
			end += len(rulesEnd)
		}
		next := strings.TrimLeft(content[end:], "\n")
		prefix := strings.TrimRight(content[:start], "\n")
		if prefix != "" && next != "" {
			return prefix + "\n\n" + strings.TrimRight(block, "\n") + "\n\n" + next + "\n"
		}
		if prefix != "" {
			return prefix + "\n\n" + block
		}
		if next != "" {
			return strings.TrimRight(block, "\n") + "\n\n" + next + "\n"
		}
		return block
	}
	if content == "" {
		return block
	}
	return content + "\n\n" + block
}

func removeMarkedRulesBlock(content string) (string, bool) {
	start := strings.Index(content, rulesStart)
	endMarker := rulesEnd
	if start < 0 {
		start = strings.Index(content, oldRulesStart)
		endMarker = oldRulesEnd
	}
	if start < 0 {
		return content, false
	}
	endRelative := strings.Index(content[start:], endMarker)
	if endRelative < 0 {
		return content, false
	}
	end := start + endRelative + len(endMarker)
	prefix := strings.TrimRight(content[:start], "\n")
	suffix := strings.TrimLeft(content[end:], "\n")
	switch {
	case prefix != "" && suffix != "":
		return prefix + "\n\n" + suffix, true
	case prefix != "":
		return prefix + "\n", true
	case suffix != "":
		return suffix, true
	default:
		return "", true
	}
}

func UninstallRulesBlock(agent, projectDir string, dryRun bool) (string, bool, error) {
	agent = normalizeAgentName(agent)
	if agent == "" {
		return "", false, fmt.Errorf("agent is required; choose Codex, Claude, Trae, or Cursor")
	}
	projectDir, err := ExpandPath(projectDir)
	if err != nil {
		return "", false, err
	}
	path, err := agentRulesPath(agent, projectDir)
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return path, false, nil
	}
	if err != nil {
		return path, false, err
	}
	updated, changed := removeMarkedRulesBlock(string(data))
	if !changed || dryRun {
		return path, changed, nil
	}
	if agent == "cursor" && strings.TrimSpace(updated) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return path, false, err
		}
		return path, true, nil
	}
	if err := atomicWriteFile(path, []byte(updated), 0o644); err != nil {
		return path, false, err
	}
	return path, true, nil
}

func DefaultRulesTemplate() string {
	return strings.Join([]string{
		"# ConnectMac AI Rules",
		"",
		"Use the connectmac skill for any request involving cm aws, AWS Mac Dedicated Hosts, Mac virtual machines, 提包机, Apple-account-based Mac access, or opening/creating/releasing/destroying AWS Mac resources.",
		"",
		"Follow these rules:",
		"",
		"1. Require an explicit Apple account email for AI-driven open/create/destroy requests. Never infer the email from conversation context.",
		"2. If the user does not provide an Apple email, list configured accounts first and ask the user to choose.",
		"3. Preview before every AWS mutation. Do not pass --confirm or confirm=true until the user explicitly approves that exact operation.",
		"4. Destroy/release workflows must never release Elastic IP allocations. They may only disassociate EIP from the managed instance, terminate managed EC2, and release managed Dedicated Hosts.",
		"5. After a Dedicated Host is allocated, any EC2 launch/start failure must stop the create loop. Do not try another instance type or allocate another Dedicated Host.",
		"6. Never terminate EC2 during open/create/launch-on-host recovery. Investigate or recover the EC2 launch/start failure on the existing Dedicated Host. Terminate EC2 only in an explicit destroy/release workflow after user confirmation.",
		"7. If a Dedicated Host exists, reuse that host and launch/recover EC2 on it instead of creating another Dedicated Host.",
		"8. When AWS create/open/launch-on-host fails, report the exact error reason, stop the current action, and wait for explicit user instructions before continuing.",
		"9. Launch EC2 on a Dedicated Host only when the host state is available. If the host is pending, stop and wait instead of attempting instance creation.",
		"10. Destroy workflows that terminate EC2 must defer Dedicated Host release until a later repeated destroy run after AWS finishes the Mac host transition.",
		"11. Do not create AWS key pairs or change security group ingress unless the user explicitly asks for that setup step.",
		"12. Do not SSH-probe a newly launched Mac until AWS readiness checks pass.",
		"13. Before opening or creating a Mac, use cm_aws_capacity or cm aws capacity when the user asks about quota, instance type order, capacity, or why a type was selected.",
		"14. If SSH fails with Permission denied (publickey), first compare the profile's AWS key_name with local identity_file; report any mismatch before changing config or retrying.",
		"15. Treat ready as \"the managed Mac is already usable.\" Do not describe ready as needing to wait, create, or open a new AWS resource.",
		"16. For blocked decisions, stop and explain the blocking reason instead of continuing automatically.",
		"17. For AWS create/open/adopt/launch, creator must come from explicit user input or an explicit profile value. Never infer creator from conversation context or defaults; if it is missing, ask the user.",
		"",
		"Preferred MCP tools:",
		"",
		"- cm_mcp_guide",
		"- cm_guide",
		"- cm_list_profiles",
		"- cm_find_profile_by_apple",
		"- cm_check_profile",
		"- cm_profile_show",
		"- cm_profile_add",
		"- cm_profile_remove",
		"- cm_profile_rename",
		"- cm_profile_export",
		"- cm_doctor",
		"- cm_dashboard",
		"- cm_next",
		"- cm_push",
		"- cm_pull",
		"- cm_forget_host",
		"- cm_aws_capacity",
		"- cm_aws_plan",
		"- cm_aws_status",
		"- cm_aws_wait_ready",
		"- cm_aws_create_mac",
		"- cm_aws_open_mac_by_email",
		"- cm_aws_adopt_mac",
		"- cm_aws_adopt_host",
		"- cm_aws_launch_on_host",
		"- cm_aws_destroy_mac",
		"- cm_aws_destroy_mac_by_email",
		"",
		"Tool discovery:",
		"",
		"- There is no plain `cm tools` command.",
		"- `cm mcp` starts the stdio MCP server and may print nothing when run directly.",
		"- Use `cm mcp tools` for a human-readable tool list.",
		"- Use `cm mcp tools --json` or MCP `initialize` + `tools/list` for the raw MCP tool list.",
		"- Use `cm_mcp_guide` first when the workflow is unclear; it explains main parameters and preview/confirm rules.",
		"- Dashboard, find-profile, check, push, pull, and AWS status/open/destroy tools return text plus structuredContent fields such as profile, apple_email, decision, next, confirmed, ready, errors, includes, excludes, and eip_retained.",
		"- If MCP tools are not visible to the agent, use the CLI fallback below.",
		"",
		"CLI fallback:",
		"",
		"```bash",
		"cm guide",
		"cm guide first-use",
		"cm next <profile-or-apple-email>",
		"cm profile accounts",
		"cm profile find <apple-email>",
		"cm profile show <profile-or-apple-email>",
		"cm profile wizard",
		"cm profile add --name <profile> [options]",
		"cm profile add --wizard",
		"cm profile remove <profile>",
		"cm profile remove <profile> --force-local",
		"cm profile rename <old> <new>",
		"cm profile edit <profile>",
		"cm profile export <profile-or-apple-email>",
		"cm profile import <profile-file.yaml>",
		"cm profile import <profile-file.yaml> --overwrite",
		"cm profile import-dir <profiles-dir>",
		"cm profile import-dir <profiles-dir> --overwrite",
		"cm doctor",
		"cm doctor --fix",
		"cm dashboard",
		"cm dashboard --aws",
		"cm setup-vnc <profile>",
		"cm open <profile-or-apple-email>",
		"cm close <profile-or-apple-email>",
		"cm aws capacity <profile-or-apple-email>",
		"cm aws plan <profile-or-apple-email>",
		"cm aws running",
		"cm aws status <profile-or-apple-email>",
		"cm aws open <profile-or-apple-email>",
		"cm aws destroy <profile-or-apple-email>",
		"cm aws destroy-many <profile-or-apple-email>...",
		"cm aws destroy-all --except <profile-or-apple-email>",
		"```",
		"",
		"Only after explicit approval:",
		"",
		"```bash",
		"cm aws open <apple-email> --confirm",
		"cm aws destroy <apple-email> --confirm",
		"```",
		"",
	}, "\n")
}

func DefaultSkillTemplate() string {
	return strings.Join([]string{
		"---",
		"name: connectmac",
		"description: Safely operate the local `cm` ConnectMac tool for AWS Mac Dedicated Host workflows. Use when the user asks to open, create, release, destroy, check, list, or manage Mac virtual machines, AWS Mac hosts, 提包机, Apple-account-based Mac access, `cm aws`, or `cm` MCP workflows. Always require an explicit Apple account email for AI-driven open/create/destroy requests; if missing, list configured accounts and ask the user to choose.",
		"---",
		"",
		"# ConnectMac",
		"",
		"## Core Rules",
		"",
		"- Treat the Apple account email as the operator-facing identity.",
		"- Never infer the Apple email from conversation context. If the user did not explicitly provide one for open/create/destroy, list accounts first.",
		"- Preview before every AWS mutation. Do not pass `--confirm` or `confirm=true` until the user explicitly approves that exact operation.",
		"- Destroy/release workflows must never release Elastic IP allocations. They may only disassociate EIP from the managed instance, terminate managed EC2, and release managed Dedicated Hosts.",
		"- After a Dedicated Host is allocated, any EC2 launch/start failure must stop the create loop. Do not try another instance type or allocate another Dedicated Host.",
		"- Never terminate EC2 during open/create/launch-on-host recovery. Investigate or recover the EC2 launch/start failure on the existing Dedicated Host. Terminate EC2 only in an explicit destroy/release workflow after user confirmation.",
		"- If a Dedicated Host exists, reuse that host and launch/recover EC2 on it instead of creating another Dedicated Host.",
		"- When AWS create/open/launch-on-host fails, report the exact error reason, stop the current action, and wait for explicit user instructions before continuing.",
		"- Launch EC2 on a Dedicated Host only when the host state is available. If the host is pending, stop and wait instead of attempting instance creation.",
		"- Destroy workflows that terminate EC2 must defer Dedicated Host release until a later repeated destroy run after AWS finishes the Mac host transition.",
		"- Do not create AWS key pairs or change security group ingress unless the user explicitly asks for that setup step.",
		"- Do not SSH-probe a newly launched Mac until AWS readiness checks pass.",
		"- Before opening or creating a Mac, use `cm_aws_capacity` or `cm aws capacity` when the user asks about quota, instance type order, capacity, or why a type was selected.",
		"- If SSH fails with `Permission denied (publickey)`, first compare the profile's AWS `key_name` with local `identity_file`; report any mismatch before changing config or retrying.",
		"- For AWS create/open/adopt/launch, creator must come from explicit user input or an explicit profile value. Never infer creator from conversation context or defaults; if it is missing, ask the user.",
		"",
		"## Preferred MCP Flow",
		"",
		"Use `cm mcp` tools when available:",
		"",
		"- `cm_mcp_guide`: read this first when the workflow is unclear; it explains stable flows, main parameters, and preview/confirm rules.",
		"- `cm_guide`: show step-by-step human guidance for common workflows.",
		"- `cm_list_profiles`: list configured profiles.",
		"- `cm_find_profile_by_apple`: resolve an explicit `apple_email` to a profile; returns structuredContent.",
		"- `cm_check_profile`: validate a profile without connecting; returns structuredContent with ok and errors.",
		"- `cm_profile_show`: show a profile file by profile name or Apple account email.",
		"- `cm_profile_add`: preview or create a local profile file; requires `confirm=true` to write.",
		"- `cm_profile_remove`: preview or remove a local profile file; checks AWS resources unless `force_local=true`, and requires `confirm=true` to remove.",
		"- `cm_profile_rename`: preview or rename a local profile file; requires `confirm=true` to write.",
		"- `cm_profile_export`: export a profile file by profile name or Apple account email.",
		"- `cm_doctor`: run local ConnectMac diagnostics.",
		"- `cm_dashboard`: show profile/tunnel dashboard, with optional read-only AWS decision and next-step columns; returns structuredContent profiles array.",
		"- `cm_next`: recommend the next safe action for a profile or Apple account; returns structuredContent with decision, next, and ready.",
		"- `cm_push`: preview or execute rsync upload with optional `includes`/`excludes`; requires `confirm=true` to execute; returns structuredContent.",
		"- `cm_pull`: preview or execute rsync download with optional `includes`/`excludes`; requires `confirm=true` to execute; returns structuredContent.",
		"- `cm_forget_host`: preview or remove `known_hosts` entries for a profile host; requires `confirm=true` to execute.",
		"- `cm_aws_plan`: preview local AWS Mac creation settings without calling AWS APIs.",
		"- `cm_aws_capacity`: read-only Mac Dedicated Host quotas, active host usage, remaining capacity, and offering AZs.",
		"- `cm_aws_status`: inspect a known profile; returns text plus structuredContent fields such as profile, apple_email, decision, next, confirmed, ready, hosts, instances, and EIP.",
		"- `cm_aws_wait_ready`: wait for AWS readiness after confirmed create/open/launch.",
		"- `cm_aws_create_mac`: preview or create AWS Mac compute by profile; creator must be explicit when missing; requires `confirm=true` to execute.",
		"- `cm_aws_open_mac_by_email`: preview or open by explicit `apple_email`; creator must be explicit when missing; requires `confirm=true` to mutate AWS; returns structuredContent.",
		"- `cm_aws_adopt_mac`: preview or tag existing AWS Mac resources as managed; creator must be explicit when missing; requires `confirm=true` to execute.",
		"- `cm_aws_adopt_host`: preview or tag an existing empty Dedicated Host as managed; creator must be explicit when missing.",
		"- `cm_aws_launch_on_host`: preview or launch EC2 on an explicit existing Dedicated Host; creator must be explicit when missing; requires `confirm=true` to execute.",
		"- `cm_aws_destroy_mac`: preview or destroy AWS Mac compute by profile; requires `confirm=true` to execute; never releases Elastic IP allocations; returns structuredContent.",
		"- `cm_aws_destroy_mac_by_email`: preview or release compute by explicit `apple_email`; requires `confirm=true` to execute; never releases Elastic IP allocations; returns structuredContent.",
		"",
		"Tool discovery:",
		"",
		"- Do not use `cm tools`; ordinary CLI has no such command.",
		"- `cm mcp` is a stdio MCP server. Running it directly can appear to hang or print nothing because it waits for JSON-RPC on stdin.",
		"- Use `cm mcp tools` to print tool names, descriptions, and required parameters.",
		"- Use `cm mcp tools --json` to print the MCP `tools/list` result JSON.",
		"- Use `cm_mcp_guide` for stable tool flows, main parameters, and confirm rules.",
		"- MCP clients should call `initialize` followed by `tools/list`.",
		"- If MCP tools are unavailable or hidden, use the CLI fallback commands below.",
		"",
		"For AI requests like \"给 xxx@mail.com 打开 Mac\" or \"释放 xxx@mail.com 的 Mac\":",
		"",
		"1. Confirm the request includes the Apple email.",
		"2. Call the matching MCP tool without `confirm` first.",
		"3. Summarize the preview and decision.",
		"4. Only call again with `confirm=true` after the user explicitly confirms.",
		"",
		"If no email is provided, call `cm_find_profile_by_apple` with no email or `cm_list_profiles`, then ask the user to choose an Apple account.",
		"",
		"## Decision Handling",
		"",
		"Interpret `cm_aws_open_mac_by_email` and `cm aws open` decisions this way:",
		"",
		"- `ready`: say the managed Mac is already usable. Do not say it needs to wait, create, or open a new resource.",
		"- `wait-ready`: say a managed instance exists but AWS readiness checks are still pending; wait-ready may be used after confirmation when appropriate.",
		"- `launch-on-host`: say an available managed Dedicated Host exists and EC2 can be launched on it after confirmation.",
		"- `create`: say no active managed compute was found and a new Dedicated Host plus EC2 would be created after confirmation.",
		"- `blocked`: stop and explain the blocking reason. Do not continue automatically.",
		"",
		"For `ready` previews, there is usually no AWS mutation to perform. If the user asks to \"confirm open\" while already ready, run the confirmed flow only to re-check/report readiness; do not describe it as creating or changing AWS resources.",
		"",
		"## CLI Fallback",
		"",
		"Use the installed `cm` command when MCP is unavailable:",
		"",
		"```bash",
		"cm guide",
		"cm guide first-use",
		"cm next <profile-or-apple-email>",
		"cm profile accounts",
		"cm profile find <apple-email>",
		"cm profile show <profile-or-apple-email>",
		"cm profile wizard",
		"cm profile add --name <profile> [options]",
		"cm profile add --wizard",
		"cm profile remove <profile>",
		"cm profile remove <profile> --force-local",
		"cm profile rename <old> <new>",
		"cm profile edit <profile>",
		"cm profile export <profile-or-apple-email>",
		"cm profile import <profile-file.yaml>",
		"cm profile import <profile-file.yaml> --overwrite",
		"cm profile import-dir <profiles-dir>",
		"cm profile import-dir <profiles-dir> --overwrite",
		"cm doctor",
		"cm doctor --fix",
		"cm dashboard",
		"cm dashboard --aws",
		"cm setup-vnc <profile>",
		"cm open <profile-or-apple-email>",
		"cm close <profile-or-apple-email>",
		"cm aws capacity <profile-or-apple-email>",
		"cm aws plan <profile-or-apple-email>",
		"cm aws running",
		"cm aws status <profile-or-apple-email>",
		"cm aws open <profile-or-apple-email>",
		"cm aws destroy <profile-or-apple-email>",
		"cm aws destroy-many <profile-or-apple-email>...",
		"cm aws destroy-all --except <profile-or-apple-email>",
		"```",
		"",
		"Add `--confirm` only after explicit approval:",
		"",
		"```bash",
		"cm aws open <apple-email> --confirm",
		"cm aws destroy <apple-email> --confirm",
		"```",
		"",
		"## Readiness",
		"",
		"Treat a Mac as connectable only when `cm aws status` or `cm_aws_open_mac_by_email` reports ready, or when `cm aws wait-ready` / `cm_aws_wait_ready` succeeds. Required checks are:",
		"",
		"- EC2 instance state is running.",
		"- Elastic IP is associated with the managed instance.",
		"- System status check is `ok`.",
		"- Instance status check is `ok`.",
		"- EBS status is `ok` only when AWS reports EBS status for that instance type.",
		"",
		"## User-Facing Summary",
		"",
		"When reporting results, include:",
		"",
		"- Resolved profile name.",
		"- Region.",
		"- Current decision, such as `ready`, `wait-ready`, `launch-on-host`, `create`, or `blocked`.",
		"- Whether the command was preview-only or confirmed.",
		"- For destroy previews/results, explicitly say the Elastic IP allocation is retained.",
		"",
	}, "\n")
}

func DefaultSkillOpenAIYAML() string {
	return strings.Join([]string{
		"interface:",
		"  display_name: \"ConnectMac\"",
		"  short_description: \"Safely operate cm AWS Mac profiles by Apple account email.\"",
		"  default_prompt: \"Use ConnectMac to preview or operate AWS Mac workflows safely by explicit Apple account email.\"",
		"",
	}, "\n")
}
