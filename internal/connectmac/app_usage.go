package connectmac

import "fmt"

func (a App) printUsage() {
	fmt.Fprint(a.Out, `Usage:
  cm version
  cm init [--config <path>]
  cm init wizard [--config <path>]
    cm init is a guided, rerunnable setup for server, local PEM, member token, and AI Skill.
  cm skill setup [--agent <codex|claude|trae|cursor>] [--project <path>] [--skills-dir <path>] [--dry-run]
  cm skill <install|status|diff|update|validate|path|print|uninstall> [options]
  cm guide [first-use|profile|open|close|sync|vnc|mcp]
  cm completion <zsh|bash|fish>
  cm list [--config <path>]
  cm next <profile-or-apple-email> [--config <path>]
  cm check <profile> [--config <path>]
  cm connect <profile> [--config <path>]
  cm open <profile-or-apple-email> [--confirm] [--config <path>]
  cm close <profile-or-apple-email> [--confirm] [--background] [--notify] [--config <path>]
  cm ssh <profile> [--config <path>]
  cm exec <profile> [--config <path>] -- <command>
  cm start <profile> [--config <path>]
  cm pull <profile-or-apple-email> <remote-path> [--include <pattern>] [--exclude <pattern>] [--config <path>]
  cm push <profile-or-apple-email> <local-path> <remote-dir> [--include <pattern>] [--exclude <pattern>] [--config <path>]
  cm forget-host <profile> [--config <path>]
  cm host-key check <profile> [--config <path>]
  cm host-key fix <profile> [--confirm --fingerprint <SHA256:...> ...] [--config <path>]
  cm open-vnc <profile> [--config <path>]
  cm setup-vnc <profile> [--config <path>]
  cm profile accounts [--config <path>]
  cm profile find <apple-email> [--config <path>]
  cm profile show <profile-or-apple-email> [--config <path>]
  cm profile wizard [--config <path>]
  cm profile add --wizard [--config <path>]
  cm profile add --name <profile> [options] [--config <path>]
  cm profile remove <profile> [--force-local] [--config <path>]
  cm profile rename <old> <new> [--config <path>]
  cm profile edit <profile> [--config <path>]
  cm profile export <profile-or-apple-email> [--config <path>]
  cm profile import <profile-file.yaml> [--overwrite] [--config <path>]
  cm profile import-dir <profiles-dir> [--overwrite] [--config <path>]
  cm member list
  cm member add --name <name> --email <email> [--role <admin|operator|viewer>]
  cm member enable <email>
  cm member disable <email>
  cm member assign <apple-email> --member <member-email> [--relation owner]
  cm member unassign <apple-email> --member <member-email>
  cm logs list
  cm logs export [--output <zip>]
  cm logs clean
  cm aws plan <profile-or-apple-email> [--config <path>]
  cm aws capacity <profile-or-apple-email> [--config <path>]
  cm aws open <profile-or-apple-email> [--confirm] [--config <path>]
  cm aws create <profile-or-apple-email> [--confirm] [--config <path>]
  cm aws status <profile-or-apple-email> [--config <path>]
  cm aws wait-ready <profile-or-apple-email> [--config <path>]
  cm aws adopt <profile-or-apple-email> [--confirm] [--config <path>]
  cm aws adopt-host <profile-or-apple-email> --host-id <id> [--confirm] [--config <path>]
  cm aws launch-on-host <profile-or-apple-email> --host-id <id> [--confirm] [--config <path>]
  cm aws destroy <profile-or-apple-email> [--confirm] [--background] [--notify] [--config <path>]
  cm aws destroy-many <profile-or-apple-email>... [--confirm] [--config <path>]
  cm aws destroy-all [--except <profile-or-apple-email>] [--confirm] [--config <path>]
  cm aws running [--config <path>]
  cm job list
  cm job status <job-id>
  cm job log <job-id>
  cm job wait <job-id>
  cm job active
  cm job active --json
  cm job wait-all [--timeout 2h] [--interval 10s] [--drain]
  cm web [--host 127.0.0.1] [--port 8765] [--open] [--web-dir <path>] [--config <path>]
  cm local-agent [--host 127.0.0.1] [--port 18765]
  cm local-agent install [--host 127.0.0.1] [--port 18765]
  cm local-agent start
  cm local-agent stop [--host 127.0.0.1] [--port 18765] [--force]
  cm local-agent restart [--host 127.0.0.1] [--port 18765] [--force]
  cm local-agent status [--host 127.0.0.1] [--port 18765]
  cm local-agent uninstall [--host 127.0.0.1] [--port 18765] [--force]
  cm mcp [--config <path>]
  cm mcp tools [--json]
  cm doctor [--fix] [--config <path>]
  cm dashboard [--aws] [--config <path>]
  cm stop <profile>
  cm status
`)
}
func (a App) printVersion() {
	version := a.Version
	if version == "" {
		version = "dev"
	}
	fmt.Fprintf(a.Out, "cm %s\n", version)
}
