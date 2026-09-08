package connectmac

import (
	"context"
	"fmt"
	"strings"
)

func (a App) runCompletion(ctx context.Context, configPath string, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(a.Err, "usage: cm completion <zsh|bash|fish|commands|profiles|apple-emails|aws-commands|mcp-commands|job-commands|profile-commands|member-commands|logs-commands|host-key-commands|local-agent-commands|skill-commands>")
		return 2
	}
	switch args[0] {
	case "zsh":
		fmt.Fprint(a.Out, zshCompletionScript())
		return 0
	case "bash":
		fmt.Fprint(a.Out, bashCompletionScript())
		return 0
	case "fish":
		fmt.Fprint(a.Out, fishCompletionScript())
		return 0
	case "commands":
		printLines(a.Out, completionCommands())
		return 0
	case "aws-commands":
		printLines(a.Out, completionAWSCommands())
		return 0
	case "mcp-commands":
		printLines(a.Out, completionMCPCommands())
		return 0
	case "job-commands":
		printLines(a.Out, completionJobCommands())
		return 0
	case "profile-commands":
		printLines(a.Out, completionProfileCommands())
		return 0
	case "member-commands":
		printLines(a.Out, completionMemberCommands())
		return 0
	case "logs-commands":
		printLines(a.Out, completionLogsCommands())
		return 0
	case "host-key-commands":
		printLines(a.Out, completionHostKeyCommands())
		return 0
	case "local-agent-commands":
		printLines(a.Out, completionLocalAgentCommands())
		return 0
	case "skill-commands":
		printLines(a.Out, completionSkillCommands())
		return 0
	case "profiles":
		cfg, code := a.loadCompletionConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		printLines(a.Out, sortedProfileNames(cfg))
		return 0
	case "apple-emails":
		cfg, code := a.loadCompletionConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		printLines(a.Out, sortedAppleEmails(cfg))
		return 0
	default:
		fmt.Fprintf(a.Err, "unknown completion target %q\n", args[0])
		return 2
	}
}

func (a App) loadCompletionConfig(ctx context.Context, configPath string) (Config, int) {
	cfg, code := a.loadConfig(configPath)
	if code != 0 {
		return Config{}, code
	}
	if strings.TrimSpace(cfg.Server.UserAPI) == "" || !hasCLIRemoteAuth(configPath, cfg) {
		return cfg, 0
	}
	remoteCfg, err := a.loadRemoteListConfig(ctx, configPath, cfg)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return Config{}, 1
	}
	return remoteCfg, 0
}

func completionCommands() []string {
	return []string{
		"init",
		"skill",
		"guide",
		"version",
		"completion",
		"list",
		"profile",
		"member",
		"logs",
		"next",
		"check",
		"connect",
		"open",
		"close",
		"ssh",
		"exec",
		"start",
		"pull",
		"push",
		"forget-host",
		"host-key",
		"open-vnc",
		"setup-vnc",
		"stop",
		"status",
		"doctor",
		"dashboard",
		"job",
		"web",
		"local-agent",
		"mcp",
		"aws",
	}
}
func completionJobCommands() []string {
	return []string{"list", "status", "log", "wait", "active", "wait-all"}
}
func completionProfileCommands() []string {
	return []string{"accounts", "find", "show", "add", "wizard", "remove", "rename", "edit", "export", "import", "import-dir"}
}
func completionMemberCommands() []string {
	return []string{"list", "add", "enable", "disable", "assign", "unassign"}
}
func completionLogsCommands() []string {
	return []string{"list", "export", "clean"}
}
func completionHostKeyCommands() []string {
	return []string{"check", "fix"}
}
func completionAWSCommands() []string {
	return []string{
		"plan",
		"capacity",
		"open",
		"create",
		"status",
		"wait-ready",
		"adopt",
		"adopt-host",
		"launch-on-host",
		"destroy",
		"destroy-many",
		"destroy-all",
		"running",
	}
}
func completionMCPCommands() []string {
	return []string{"tools"}
}
func completionLocalAgentCommands() []string {
	return []string{"install", "start", "stop", "restart", "status", "uninstall"}
}
func completionSkillCommands() []string {
	return []string{"setup", "install", "status", "diff", "update", "validate", "path", "print", "uninstall"}
}
func zshCompletionScript() string {
	return `#compdef cm

_cm_config_args() {
  local -a args
  args=()
  local i
  for (( i = 1; i < ${#words}; i++ )); do
    if [[ "${words[$i]}" == "--config" && -n "${words[$((i + 1))]}" ]]; then
      args=(--config "${words[$((i + 1))]}")
      break
    fi
  done
  echo "${args[@]}"
}

_cm_profiles() {
  local -a config_args values
  config_args=(${(z)$(_cm_config_args)})
  values=("${(@f)$(command cm completion profiles "${config_args[@]}" 2>/dev/null)}")
  compadd -- "${values[@]}"
}

_cm_profile_or_apple() {
  local -a config_args values
  config_args=(${(z)$(_cm_config_args)})
  values=("${(@f)$(command cm completion profiles "${config_args[@]}" 2>/dev/null)}")
  values+=("${(@f)$(command cm completion apple-emails "${config_args[@]}" 2>/dev/null)}")
  compadd -- "${values[@]}"
}

_cm() {
  local -a commands aws_commands mcp_commands job_commands profile_commands member_commands logs_commands host_key_commands local_agent_commands skill_commands
  commands=("${(@f)$(command cm completion commands 2>/dev/null)}")
  aws_commands=("${(@f)$(command cm completion aws-commands 2>/dev/null)}")
  mcp_commands=("${(@f)$(command cm completion mcp-commands 2>/dev/null)}")
	job_commands=("${(@f)$(command cm completion job-commands 2>/dev/null)}")
  profile_commands=("${(@f)$(command cm completion profile-commands 2>/dev/null)}")
  member_commands=("${(@f)$(command cm completion member-commands 2>/dev/null)}")
  logs_commands=("${(@f)$(command cm completion logs-commands 2>/dev/null)}")
  host_key_commands=("${(@f)$(command cm completion host-key-commands 2>/dev/null)}")
  local_agent_commands=("${(@f)$(command cm completion local-agent-commands 2>/dev/null)}")
  skill_commands=("${(@f)$(command cm completion skill-commands 2>/dev/null)}")

  if [[ "${words[$((CURRENT - 1))]}" == "--config" ]]; then
    _files
    return
  fi

  if (( CURRENT == 2 )); then
    compadd -- "${commands[@]}"
    return
  fi

  case "${words[2]}" in
    skill)
      if (( CURRENT == 3 )); then
        compadd -- "${skill_commands[@]}"
      else
        _values 'skill option' --agent --project --skills-dir --dry-run --force --rules
      fi
      ;;
    check|connect|ssh|start|forget-host|open-vnc|setup-vnc|stop)
      (( CURRENT == 3 )) && _cm_profiles
      ;;
    open|close|next)
      (( CURRENT == 3 )) && _cm_profile_or_apple
      ;;
    guide)
      (( CURRENT == 3 )) && _values 'guide topic' first-use profile open close sync vnc mcp
      ;;
    pull)
      if (( CURRENT == 3 )); then
        _cm_profile_or_apple
      elif [[ "${words[$((CURRENT - 1))]}" == "--include" || "${words[$((CURRENT - 1))]}" == "--exclude" ]]; then
        _files
      else
        _values 'pull option' --include --exclude --config
      fi
      ;;
    push)
      if (( CURRENT == 3 )); then
        _cm_profile_or_apple
      elif (( CURRENT == 4 )); then
        _files
      elif [[ "${words[$((CURRENT - 1))]}" == "--include" || "${words[$((CURRENT - 1))]}" == "--exclude" ]]; then
        _files
      else
        _values 'push option' --include --exclude --config
      fi
      ;;
    exec)
      (( CURRENT == 3 )) && _cm_profiles
      ;;
    host-key)
      if (( CURRENT == 3 )); then
        compadd -- "${host_key_commands[@]}"
      elif (( CURRENT == 4 )); then
        _cm_profiles
      fi
      ;;
    profile)
      if (( CURRENT == 3 )); then
        compadd -- "${profile_commands[@]}"
      elif [[ "${words[3]}" == "find" || "${words[3]}" == "show" || "${words[3]}" == "export" ]]; then
        local -a config_args apple_emails
        config_args=(${(z)$(_cm_config_args)})
        apple_emails=("${(@f)$(command cm completion apple-emails "${config_args[@]}" 2>/dev/null)}")
        values=("${(@f)$(command cm completion profiles "${config_args[@]}" 2>/dev/null)}")
        values+=("${apple_emails[@]}")
        compadd -- "${values[@]}"
      elif [[ "${words[3]}" == "remove" || "${words[3]}" == "rename" || "${words[3]}" == "edit" ]]; then
        _cm_profiles
      elif [[ "${words[3]}" == "import" || "${words[3]}" == "import-dir" ]]; then
        _files
      fi
      ;;
    member)
      if (( CURRENT == 3 )); then
        compadd -- "${member_commands[@]}"
      else
        _values 'member option' --name --email --role --member --relation
      fi
      ;;
    aws)
      if (( CURRENT == 3 )); then
        compadd -- "${aws_commands[@]}"
      elif (( CURRENT == 4 )); then
        _cm_profile_or_apple
      else
        case "${words[$((CURRENT - 1))]}" in
          --host-id) ;;
          --except) _cm_profile_or_apple ;;
          *) _values 'aws option' --confirm --background --notify --all --host-id --except --config ;;
        esac
      fi
      ;;
    job)
      if (( CURRENT == 3 )); then
        compadd -- "${job_commands[@]}"
	  elif [[ "${words[3]}" == "active" ]]; then
		_values 'job active option' --json
	  elif [[ "${words[3]}" == "wait-all" ]]; then
		_values 'job wait-all option' --timeout --interval --drain
      fi
      ;;
    logs)
      if (( CURRENT == 3 )); then
        compadd -- "${logs_commands[@]}"
      elif [[ "${words[3]}" == "export" && "${words[$((CURRENT - 1))]}" == "--output" ]]; then
        _files
      elif [[ "${words[3]}" == "export" ]]; then
		_values 'logs option' --output --include-raw --config
      fi
      ;;
    web)
      _values 'web option' --host --port --open --web-dir --config
      ;;
    local-agent)
      if (( CURRENT == 3 )); then
        compadd -- "${local_agent_commands[@]}"
        _values 'local-agent option' --host --port
      elif [[ "${words[3]}" == "stop" || "${words[3]}" == "restart" || "${words[3]}" == "uninstall" ]]; then
        _values 'local-agent option' --host --port --force
      else
        _values 'local-agent option' --host --port
      fi
      ;;
    mcp)
      if (( CURRENT == 3 )); then
        compadd -- "${mcp_commands[@]}"
      elif [[ "${words[3]}" == "tools" ]]; then
        _values 'mcp tools option' --json --config
      fi
      ;;
    completion)
      _values 'shell' zsh bash fish
      ;;
  esac
}

_cm "$@"
`
}
func bashCompletionScript() string {
	return `_cm_completion()
{
  local cur prev cmd sub config_args
  COMPREPLY=()
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  cmd="${COMP_WORDS[1]}"
  config_args=()
  local i
  for (( i=1; i<COMP_CWORD; i++ )); do
    if [[ "${COMP_WORDS[i]}" == "--config" && -n "${COMP_WORDS[i+1]}" ]]; then
      config_args=(--config "${COMP_WORDS[i+1]}")
      break
    fi
  done

  if [[ "$prev" == "--config" ]]; then
    COMPREPLY=( $(compgen -f -- "$cur") )
    return 0
  fi

  if [[ $COMP_CWORD -eq 1 ]]; then
    COMPREPLY=( $(compgen -W "$(cm completion commands 2>/dev/null)" -- "$cur") )
    return 0
  fi

  case "$cmd" in
    skill)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion skill-commands 2>/dev/null)" -- "$cur") )
      else
        COMPREPLY=( $(compgen -W "--agent --project --skills-dir --dry-run --force --rules" -- "$cur") )
      fi
      ;;
    check|connect|ssh|start|forget-host|open-vnc|setup-vnc|stop|exec)
      [[ $COMP_CWORD -eq 2 ]] && COMPREPLY=( $(compgen -W "$(cm completion profiles "${config_args[@]}" 2>/dev/null)" -- "$cur") )
      ;;
    open|close|next)
      [[ $COMP_CWORD -eq 2 ]] && COMPREPLY=( $(compgen -W "$(cm completion profiles "${config_args[@]}" 2>/dev/null; cm completion apple-emails "${config_args[@]}" 2>/dev/null)" -- "$cur") )
      ;;
    guide)
      [[ $COMP_CWORD -eq 2 ]] && COMPREPLY=( $(compgen -W "first-use profile open close sync vnc mcp" -- "$cur") )
      ;;
    pull)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion profiles "${config_args[@]}" 2>/dev/null; cm completion apple-emails "${config_args[@]}" 2>/dev/null)" -- "$cur") )
      elif [[ "$prev" == "--include" || "$prev" == "--exclude" ]]; then
        COMPREPLY=( $(compgen -f -- "$cur") )
      else
        COMPREPLY=( $(compgen -W "--include --exclude --config" -- "$cur") )
      fi
      ;;
    push)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion profiles "${config_args[@]}" 2>/dev/null; cm completion apple-emails "${config_args[@]}" 2>/dev/null)" -- "$cur") )
      elif [[ "$prev" == "--include" || "$prev" == "--exclude" ]]; then
        COMPREPLY=( $(compgen -f -- "$cur") )
      else
        COMPREPLY=( $(compgen -W "--include --exclude --config" -- "$cur") )
      fi
      ;;
    host-key)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion host-key-commands 2>/dev/null)" -- "$cur") )
      elif [[ $COMP_CWORD -eq 3 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion profiles "${config_args[@]}" 2>/dev/null)" -- "$cur") )
      fi
      ;;
    profile)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion profile-commands 2>/dev/null)" -- "$cur") )
      elif [[ "${COMP_WORDS[2]}" == "find" || "${COMP_WORDS[2]}" == "show" || "${COMP_WORDS[2]}" == "export" ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion profiles "${config_args[@]}" 2>/dev/null; cm completion apple-emails "${config_args[@]}" 2>/dev/null)" -- "$cur") )
      elif [[ "${COMP_WORDS[2]}" == "remove" || "${COMP_WORDS[2]}" == "rename" || "${COMP_WORDS[2]}" == "edit" ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion profiles "${config_args[@]}" 2>/dev/null)" -- "$cur") )
      elif [[ "${COMP_WORDS[2]}" == "import" || "${COMP_WORDS[2]}" == "import-dir" ]]; then
        COMPREPLY=( $(compgen -f -- "$cur") )
      fi
      ;;
    member)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion member-commands 2>/dev/null)" -- "$cur") )
      else
        COMPREPLY=( $(compgen -W "--name --email --role --member --relation" -- "$cur") )
      fi
      ;;
    aws)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion aws-commands 2>/dev/null)" -- "$cur") )
      elif [[ $COMP_CWORD -eq 3 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion profiles "${config_args[@]}" 2>/dev/null; cm completion apple-emails "${config_args[@]}" 2>/dev/null)" -- "$cur") )
      else
        if [[ "$prev" == "--except" ]]; then
          COMPREPLY=( $(compgen -W "$(cm completion profiles "${config_args[@]}" 2>/dev/null; cm completion apple-emails "${config_args[@]}" 2>/dev/null)" -- "$cur") )
        else
          COMPREPLY=( $(compgen -W "--confirm --background --notify --all --host-id --except --config" -- "$cur") )
        fi
      fi
      ;;
    job)
      if [[ $COMP_CWORD -eq 2 ]]; then
		COMPREPLY=( $(compgen -W "$(cm completion job-commands 2>/dev/null)" -- "$cur") )
	  elif [[ "${COMP_WORDS[2]}" == "active" ]]; then
		COMPREPLY=( $(compgen -W "--json" -- "$cur") )
	  elif [[ "${COMP_WORDS[2]}" == "wait-all" ]]; then
		COMPREPLY=( $(compgen -W "--timeout --interval --drain" -- "$cur") )
      fi
      ;;
    logs)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion logs-commands 2>/dev/null)" -- "$cur") )
      elif [[ "${COMP_WORDS[2]}" == "export" && "$prev" == "--output" ]]; then
        COMPREPLY=( $(compgen -f -- "$cur") )
      elif [[ "${COMP_WORDS[2]}" == "export" ]]; then
		COMPREPLY=( $(compgen -W "--output --include-raw --config" -- "$cur") )
      fi
      ;;
    web)
      COMPREPLY=( $(compgen -W "--host --port --open --web-dir --config" -- "$cur") )
      ;;
    local-agent)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion local-agent-commands 2>/dev/null) --host --port" -- "$cur") )
      elif [[ "${COMP_WORDS[2]}" == "stop" || "${COMP_WORDS[2]}" == "restart" || "${COMP_WORDS[2]}" == "uninstall" ]]; then
        COMPREPLY=( $(compgen -W "--host --port --force" -- "$cur") )
      else
        COMPREPLY=( $(compgen -W "--host --port" -- "$cur") )
      fi
      ;;
    mcp)
      if [[ $COMP_CWORD -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "$(cm completion mcp-commands 2>/dev/null)" -- "$cur") )
      elif [[ "${COMP_WORDS[2]}" == "tools" ]]; then
        COMPREPLY=( $(compgen -W "--json --config" -- "$cur") )
      fi
      ;;
    completion)
      COMPREPLY=( $(compgen -W "zsh bash fish" -- "$cur") )
      ;;
  esac
  return 0
}
complete -F _cm_completion cm
`
}
func fishCompletionScript() string {
	return `complete -c cm -f
complete -c cm -n "not __fish_seen_subcommand_from (cm completion commands)" -a "(cm completion commands)"
complete -c cm -n "__fish_seen_subcommand_from skill; and not __fish_seen_subcommand_from (cm completion skill-commands)" -a "(cm completion skill-commands)"
complete -c cm -n "__fish_seen_subcommand_from skill" -a "--agent --project --skills-dir --dry-run --force --rules"
complete -c cm -n "__fish_seen_subcommand_from check connect ssh start forget-host open-vnc setup-vnc stop exec" -a "(cm completion profiles)"
complete -c cm -n "__fish_seen_subcommand_from open close next" -a "(cm completion profiles)"
complete -c cm -n "__fish_seen_subcommand_from open close next" -a "(cm completion apple-emails)"
complete -c cm -n "__fish_seen_subcommand_from guide" -a "first-use profile open close sync vnc mcp"
complete -c cm -n "__fish_seen_subcommand_from pull push" -a "(cm completion profiles)"
complete -c cm -n "__fish_seen_subcommand_from pull push" -a "(cm completion apple-emails)"
complete -c cm -n "__fish_seen_subcommand_from pull push" -a "--include --exclude"
complete -c cm -n "__fish_seen_subcommand_from host-key; and not __fish_seen_subcommand_from (cm completion host-key-commands)" -a "(cm completion host-key-commands)"
complete -c cm -n "__fish_seen_subcommand_from host-key; and __fish_seen_subcommand_from (cm completion host-key-commands)" -a "(cm completion profiles)"
complete -c cm -n "__fish_seen_subcommand_from profile; and not __fish_seen_subcommand_from (cm completion profile-commands)" -a "(cm completion profile-commands)"
complete -c cm -n "__fish_seen_subcommand_from member; and not __fish_seen_subcommand_from (cm completion member-commands)" -a "(cm completion member-commands)"
complete -c cm -n "__fish_seen_subcommand_from member" -a "--name --email --role --member --relation"
complete -c cm -n "__fish_seen_subcommand_from profile; and __fish_seen_subcommand_from find" -a "(cm completion apple-emails)"
complete -c cm -n "__fish_seen_subcommand_from aws; and not __fish_seen_subcommand_from (cm completion aws-commands)" -a "(cm completion aws-commands)"
complete -c cm -n "__fish_seen_subcommand_from aws; and __fish_seen_subcommand_from (cm completion aws-commands)" -a "(cm completion profiles)"
complete -c cm -n "__fish_seen_subcommand_from aws; and __fish_seen_subcommand_from (cm completion aws-commands)" -a "(cm completion apple-emails)"
complete -c cm -n "__fish_seen_subcommand_from aws; and __fish_seen_subcommand_from destroy" -a "--background --notify"
complete -c cm -n "__fish_seen_subcommand_from job; and not __fish_seen_subcommand_from (cm completion job-commands)" -a "(cm completion job-commands)"
complete -c cm -n "__fish_seen_subcommand_from job; and __fish_seen_subcommand_from active" -a "--json"
complete -c cm -n "__fish_seen_subcommand_from job; and __fish_seen_subcommand_from wait-all" -a "--timeout --interval --drain"
complete -c cm -n "__fish_seen_subcommand_from logs; and not __fish_seen_subcommand_from (cm completion logs-commands)" -a "(cm completion logs-commands)"
complete -c cm -n "__fish_seen_subcommand_from logs; and __fish_seen_subcommand_from export" -a "--output --include-raw"
complete -c cm -n "__fish_seen_subcommand_from web" -a "--host --port --open --web-dir --config"
complete -c cm -n "__fish_seen_subcommand_from local-agent; and not __fish_seen_subcommand_from (cm completion local-agent-commands)" -a "(cm completion local-agent-commands)"
complete -c cm -n "__fish_seen_subcommand_from local-agent" -a "--host --port"
complete -c cm -n "__fish_seen_subcommand_from local-agent; and __fish_seen_subcommand_from stop restart uninstall" -a "--force"
complete -c cm -n "__fish_seen_subcommand_from mcp; and not __fish_seen_subcommand_from (cm completion mcp-commands)" -a "(cm completion mcp-commands)"
complete -c cm -n "__fish_seen_subcommand_from mcp; and __fish_seen_subcommand_from tools" -a "--json"
complete -c cm -n "__fish_seen_subcommand_from completion" -a "zsh bash fish"
`
}
