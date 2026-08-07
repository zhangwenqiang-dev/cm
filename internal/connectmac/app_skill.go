package connectmac

import (
	"fmt"
	"io"
	"strings"
)

type SkillPathOptions struct {
	SkillsDir string
	DryRun    bool
}

type SkillUpdateOptions struct {
	SkillPathOptions
	Force bool
}

type SkillValidateOptions struct {
	SkillPathOptions
	Agent      string
	ProjectDir string
}

type SkillUninstallOptions struct {
	SkillPathOptions
	Rules      bool
	Agent      string
	ProjectDir string
}

func (a App) runSkill(args []string) int {
	if len(args) == 0 {
		printSkillUsage(a.Err)
		return 2
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printSkillUsage(a.Out)
		return 0
	}
	switch args[0] {
	case "setup":
		return a.runSkillSetup(args[1:])
	case "install":
		options, err := parseSkillPathOptions(args[1:], true)
		if err != nil {
			return a.skillUsageError(err)
		}
		manager, code := a.skillManager(options.SkillsDir)
		if code != 0 {
			return code
		}
		result, err := manager.Install(options.DryRun)
		if err != nil {
			return a.skillOperationError(err)
		}
		if options.DryRun {
			fmt.Fprintf(a.Out, "Would install connectmac skill: %s\nNo files were written.\n", manager.SkillPath)
			return 0
		}
		if result.Changed {
			fmt.Fprintf(a.Out, "installed connectmac skill: %s\n", manager.SkillPath)
		} else {
			fmt.Fprintf(a.Out, "connectmac skill is already current: %s\n", manager.SkillPath)
		}
		return 0
	case "status":
		options, err := parseSkillPathOptions(args[1:], false)
		if err != nil {
			return a.skillUsageError(err)
		}
		manager, code := a.skillManager(options.SkillsDir)
		if code != 0 {
			return code
		}
		status := manager.Status()
		printSkillStatus(a.Out, status)
		if status.Name == SkillStatusModified || status.Name == SkillStatusInvalid {
			return 1
		}
		return 0
	case "update":
		options, err := parseSkillUpdateOptions(args[1:])
		if err != nil {
			return a.skillUsageError(err)
		}
		manager, code := a.skillManager(options.SkillsDir)
		if code != 0 {
			return code
		}
		result, err := manager.Update(options.Force, options.DryRun)
		if err != nil {
			return a.skillOperationError(err)
		}
		if options.DryRun {
			fmt.Fprintf(a.Out, "Would update connectmac skill: %s\nNo files were written.\n", manager.SkillPath)
			return 0
		}
		if result.BackupPath != "" {
			fmt.Fprintf(a.Out, "backed up previous skill: %s\n", result.BackupPath)
		}
		if result.Changed {
			fmt.Fprintf(a.Out, "updated connectmac skill: %s\n", manager.SkillPath)
		} else {
			fmt.Fprintf(a.Out, "connectmac skill is already current: %s\n", manager.SkillPath)
		}
		return 0
	case "validate":
		options, err := parseSkillValidateOptions(args[1:])
		if err != nil {
			return a.skillUsageError(err)
		}
		manager, code := a.skillManager(options.SkillsDir)
		if code != 0 {
			return code
		}
		if err := manager.Validate(); err != nil {
			return a.skillOperationError(err)
		}
		if options.Agent != "" {
			install, err := BuildRulesInstallWithOptions(SkillSetupOptions{Agent: options.Agent, ProjectDir: options.ProjectDir, SkillsDir: options.SkillsDir})
			if err != nil {
				return a.skillOperationError(err)
			}
			result := RulesInstallResult{Agent: install.Agent, SourcePath: install.SourcePath, AgentPath: install.AgentPath, SkillPath: install.SkillPath}
			if err := ValidateRulesInstall(result); err != nil {
				return a.skillOperationError(err)
			}
		}
		fmt.Fprintln(a.Out, "validation passed")
		return 0
	case "path":
		options, err := parseSkillPathOptions(args[1:], false)
		if err != nil {
			return a.skillUsageError(err)
		}
		manager, code := a.skillManager(options.SkillsDir)
		if code != 0 {
			return code
		}
		fmt.Fprintln(a.Out, manager.SkillPath)
		return 0
	case "print":
		if len(args) != 1 {
			return a.skillUsageError(fmt.Errorf("skill print does not accept options"))
		}
		fmt.Fprint(a.Out, DefaultSkillTemplate())
		return 0
	case "uninstall":
		return a.runSkillUninstall(args[1:])
	default:
		return a.skillUsageError(fmt.Errorf("unknown skill command %q", args[0]))
	}
}

func (a App) runSkillSetup(args []string) int {
	options, err := parseSkillSetupOptions(args)
	if err != nil {
		return a.skillUsageError(err)
	}
	if options.Agent == "" {
		if a.In == nil {
			return a.skillOperationError(fmt.Errorf("agent is required; choose Codex, Claude, Trae, or Cursor"))
		}
		options.Agent = strings.ToLower(a.promptLine("Agent for AI rules (Codex/Claude/Trae/Cursor): "))
	}
	install, err := BuildRulesInstallWithOptions(options)
	if err != nil {
		return a.skillOperationError(err)
	}
	if options.DryRun {
		fmt.Fprintln(a.Out, "ConnectMac skill setup dry run")
		fmt.Fprintf(a.Out, "Rules source: %s\n", install.SourcePath)
		fmt.Fprintf(a.Out, "%s rules: %s\n", install.Agent, install.AgentPath)
		fmt.Fprintf(a.Out, "connectmac skill: %s\n", install.SkillPath)
		fmt.Fprintln(a.Out, "No files were written.")
		return 0
	}
	result, err := InstallRulesWithVersion(install, a.Version)
	if err != nil {
		return a.skillOperationError(fmt.Errorf("skill setup failed: %w", err))
	}
	fmt.Fprintf(a.Out, "created rules source: %s\n", result.SourcePath)
	fmt.Fprintf(a.Out, "installed %s rules: %s\n", result.Agent, result.AgentPath)
	fmt.Fprintf(a.Out, "installed connectmac skill: %s\n", result.SkillPath)
	if result.Validated {
		fmt.Fprintln(a.Out, "validation passed")
	}
	fmt.Fprintln(a.Out, "Rule content source: ~/.connectmac/rules.md")
	fmt.Fprintln(a.Out, "Ask your AI agent to remember the rule content exactly and keep it as long-term memory.")
	return 0
}

func (a App) runSkillUninstall(args []string) int {
	options, err := parseSkillUninstallOptions(args)
	if err != nil {
		return a.skillUsageError(err)
	}
	manager, code := a.skillManager(options.SkillsDir)
	if code != 0 {
		return code
	}
	result, err := manager.Uninstall(options.DryRun)
	if err != nil {
		return a.skillOperationError(err)
	}
	if options.Rules {
		path, changed, err := UninstallRulesBlock(options.Agent, options.ProjectDir, options.DryRun)
		if err != nil {
			return a.skillOperationError(fmt.Errorf("remove project rules: %w", err))
		}
		if changed {
			verb := "removed"
			if options.DryRun {
				verb = "would remove"
			}
			fmt.Fprintf(a.Out, "%s ConnectMac rules: %s\n", verb, path)
		}
	}
	if options.DryRun {
		fmt.Fprintf(a.Out, "Would uninstall connectmac skill: %s\nNo files were written.\n", manager.SkillPath)
	} else if result.Changed {
		fmt.Fprintf(a.Out, "uninstalled connectmac skill: %s\n", manager.SkillPath)
	} else {
		fmt.Fprintf(a.Out, "connectmac skill is not installed: %s\n", manager.SkillPath)
	}
	return 0
}

func (a App) skillManager(skillsDir string) (SkillManager, int) {
	manager, err := NewSkillManager(skillsDir, a.Version)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return SkillManager{}, 1
	}
	return manager, 0
}

func (a App) skillUsageError(err error) int {
	fmt.Fprintln(a.Err, err)
	printSkillUsage(a.Err)
	return 2
}

func (a App) skillOperationError(err error) int {
	fmt.Fprintln(a.Err, err)
	return 1
}

func printSkillStatus(out io.Writer, status SkillStatus) {
	fmt.Fprintf(out, "Status: %s\nPath: %s\n", status.Name, status.SkillPath)
	if status.InstalledVersion != "" {
		fmt.Fprintf(out, "Installed CM: %s\n", status.InstalledVersion)
	}
	fmt.Fprintf(out, "Current CM: %s\nDetail: %s\n", status.CurrentVersion, status.Detail)
	switch status.Name {
	case SkillStatusMissing:
		fmt.Fprintln(out, "Next: cm skill install")
	case SkillStatusOutdated:
		fmt.Fprintln(out, "Next: cm skill update")
	case SkillStatusModified, SkillStatusInvalid:
		fmt.Fprintln(out, "Next: review the files, then run cm skill update --force")
	}
}

func printSkillUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  cm skill setup [--agent <codex|claude|trae|cursor>] [--project <path>] [--skills-dir <path>] [--dry-run]
  cm skill install [--skills-dir <path>] [--dry-run]
  cm skill status [--skills-dir <path>]
  cm skill update [--skills-dir <path>] [--force] [--dry-run]
  cm skill validate [--skills-dir <path>] [--agent <agent> --project <path>]
  cm skill path [--skills-dir <path>]
  cm skill print
  cm skill uninstall [--skills-dir <path>] [--rules --agent <agent> --project <path>] [--dry-run]
`)
}

func parseSkillPathOptions(args []string, allowDryRun bool) (SkillPathOptions, error) {
	var options SkillPathOptions
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--skills-dir":
			value, next, err := skillOptionValue(args, i)
			if err != nil {
				return options, err
			}
			options.SkillsDir, i = value, next
		case "--dry-run":
			if !allowDryRun {
				return options, fmt.Errorf("--dry-run is not supported here")
			}
			options.DryRun = true
		default:
			return options, fmt.Errorf("unknown skill option %q", args[i])
		}
	}
	return options, nil
}

func parseSkillSetupOptions(args []string) (SkillSetupOptions, error) {
	var options SkillSetupOptions
	for i := 0; i < len(args); i++ {
		value, next, err := skillNamedOption(args, i, map[string]*string{"--agent": &options.Agent, "--project": &options.ProjectDir, "--skills-dir": &options.SkillsDir})
		if err != nil {
			return options, err
		}
		if value {
			i = next
			continue
		}
		if args[i] == "--dry-run" {
			options.DryRun = true
			continue
		}
		return options, fmt.Errorf("unknown skill setup option %q", args[i])
	}
	return options, nil
}

func parseSkillUpdateOptions(args []string) (SkillUpdateOptions, error) {
	var options SkillUpdateOptions
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--skills-dir":
			value, next, err := skillOptionValue(args, i)
			if err != nil {
				return options, err
			}
			options.SkillsDir, i = value, next
		case "--force":
			options.Force = true
		case "--dry-run":
			options.DryRun = true
		default:
			return options, fmt.Errorf("unknown skill update option %q", args[i])
		}
	}
	return options, nil
}

func parseSkillValidateOptions(args []string) (SkillValidateOptions, error) {
	var options SkillValidateOptions
	for i := 0; i < len(args); i++ {
		matched, next, err := skillNamedOption(args, i, map[string]*string{"--skills-dir": &options.SkillsDir, "--agent": &options.Agent, "--project": &options.ProjectDir})
		if err != nil {
			return options, err
		}
		if !matched {
			return options, fmt.Errorf("unknown skill validate option %q", args[i])
		}
		i = next
	}
	if (options.Agent == "") != (options.ProjectDir == "") {
		return options, fmt.Errorf("--agent and --project must be used together")
	}
	return options, nil
}

func parseSkillUninstallOptions(args []string) (SkillUninstallOptions, error) {
	var options SkillUninstallOptions
	for i := 0; i < len(args); i++ {
		matched, next, err := skillNamedOption(args, i, map[string]*string{"--skills-dir": &options.SkillsDir, "--agent": &options.Agent, "--project": &options.ProjectDir})
		if err != nil {
			return options, err
		}
		if matched {
			i = next
			continue
		}
		switch args[i] {
		case "--rules":
			options.Rules = true
		case "--dry-run":
			options.DryRun = true
		default:
			return options, fmt.Errorf("unknown skill uninstall option %q", args[i])
		}
	}
	if options.Rules && (options.Agent == "" || options.ProjectDir == "") {
		return options, fmt.Errorf("--rules requires both --agent and --project")
	}
	if !options.Rules && (options.Agent != "" || options.ProjectDir != "") {
		return options, fmt.Errorf("--agent and --project require --rules")
	}
	return options, nil
}

func skillNamedOption(args []string, index int, targets map[string]*string) (bool, int, error) {
	target, ok := targets[args[index]]
	if !ok {
		return false, index, nil
	}
	value, next, err := skillOptionValue(args, index)
	if err != nil {
		return false, index, err
	}
	*target = value
	return true, next, nil
}

func skillOptionValue(args []string, index int) (string, int, error) {
	next := index + 1
	if next >= len(args) || strings.TrimSpace(args[next]) == "" {
		return "", index, fmt.Errorf("%s requires a value", args[index])
	}
	return args[next], next, nil
}
