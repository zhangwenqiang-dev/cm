package connectmac

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type TunnelStartResult struct {
	Action     string
	PID        int
	Profile    string
	LocalPorts []int
}

func (r TunnelStartResult) existingLiveStateConflict(pid int) TunnelStartResult {
	r.Action = "conflict"
	r.PID = pid
	return r
}

func (a App) runConnect(ctx context.Context, cfg Config, args []string) int {
	startedAt := time.Now()
	profile, ok := requireProfileArg(a.Err, cfg, args)
	if !ok {
		return 2
	}
	profile = a.promptMissingIdentityFile(profile)
	if !a.validateAndSummarize(profile) {
		a.logLocalCommand(ctx, "ssh.failed", profile, 1, startedAt, LogEntry{ErrorCode: "validation_error"})
		return 1
	}
	sshArgs, err := SSHArgs(profile)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		a.logLocalCommand(ctx, "ssh.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	a.logLocalCommand(ctx, "ssh.attempted", profile, 0, startedAt, LogEntry{Phase: "attempted"})
	if err := a.Runner.RunForeground(ctx, sshArgs); err != nil {
		fmt.Fprintf(a.Err, "ssh failed: %v\n", err)
		a.logLocalCommand(ctx, "ssh.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	a.logLocalCommand(ctx, "ssh.succeeded", profile, 0, startedAt, LogEntry{Phase: "closed"})
	return 0
}
func (a App) runStart(ctx context.Context, cfg Config, args []string) int {
	startedAt := time.Now()
	code, result := a.runStartResult(ctx, cfg, args)
	profile := Profile{}
	if len(args) == 1 {
		profile, _ = cfg.Profile(args[0])
	}
	action := "tunnel.failed"
	if code == 0 {
		switch result.Action {
		case "reused":
			action = "tunnel.reused"
		case "restarted":
			action = "tunnel.replaced"
		default:
			action = "tunnel.started"
		}
	}
	a.logLocalCommand(ctx, action, profile, code, startedAt, LogEntry{
		TunnelAction: result.Action,
		PID:          result.PID,
		LocalPorts:   result.LocalPorts,
	})
	return code
}

func (a App) runStartResult(ctx context.Context, cfg Config, args []string) (int, TunnelStartResult) {
	profileRef := ""
	if len(args) == 1 {
		profileRef = args[0]
	}
	profile, ok := requireProfileArg(a.Err, cfg, args)
	if !ok {
		return 2, TunnelStartResult{}
	}
	profile = a.promptMissingIdentityFile(profile)
	result := TunnelStartResult{Profile: profile.Name, LocalPorts: profileLocalPorts(profile)}
	stateKey := profile.Name
	if stateKey == "" {
		stateKey = profileRef
	}
	code := 0
	err := a.StateManager.WithProfileLock(stateKey, func() error {
		code, result = a.runStartLockedResult(ctx, profile, stateKey)
		return nil
	})
	if err != nil {
		fmt.Fprintf(a.Err, "lock start lifecycle: %v\n", err)
		return 1, result
	}
	return code, result
}

func (a App) runStartLocked(ctx context.Context, profile Profile, stateKey string) int {
	code, _ := a.runStartLockedResult(ctx, profile, stateKey)
	return code
}

func (a App) runStartLockedResult(ctx context.Context, profile Profile, stateKey string) (int, TunnelStartResult) {
	result := TunnelStartResult{Profile: profile.Name, LocalPorts: profileLocalPorts(profile)}
	replacing := false
	if err := a.StateManager.PreflightTunnelLifecycle(); err != nil {
		fmt.Fprintf(a.Err, "cannot manage SSH tunnel on this platform: %v\n", err)
		return 1, result
	}
	sshArgs, err := SSHArgs(profile)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1, result
	}
	if state, ok, err := a.StateManager.Load(stateKey); err != nil {
		fmt.Fprintf(a.Err, "load state: %v\n", err)
		return 1, result
	} else if ok {
		if a.StateManager.IsRunning != nil && !a.StateManager.IsRunning(state.PID) {
			if err := a.StateManager.Remove(stateKey); err != nil {
				fmt.Fprintf(a.Err, "remove stale tunnel state: %v\n", err)
				return 1, result
			}
		} else if state.SSHCommandFingerprint == "" || state.ProcessStartMarker == "" || state.IdentityFile == "" {
			if !state.matchesLegacyProfile(profile) {
				fmt.Fprintf(a.Err, "refusing to replace legacy live tunnel pid %d: %v\n", state.PID, legacyStateError("complete process identity"))
				return 1, result.existingLiveStateConflict(state.PID)
			}
			identity, err := a.StateManager.InspectExpectedProcess(state.PID, sshArgs)
			if err != nil {
				fmt.Fprintf(a.Err, "refusing to adopt legacy tunnel pid %d: %v; it cannot be safely killed\n", state.PID, err)
				return 1, result.existingLiveStateConflict(state.PID)
			}
			adopted := NewState(profile, state.PID, identity)
			adopted.StartedAt = state.StartedAt
			if err := a.StateManager.Save(adopted); err != nil {
				fmt.Fprintf(a.Err, "save adopted legacy tunnel state: %v\n", err)
				return 1, result.existingLiveStateConflict(state.PID)
			}
			fmt.Fprintf(a.Out, "already started %s with pid %d (adopted legacy state)\n", stateKey, state.PID)
			result.Action, result.PID = "reused", state.PID
			return 0, result
		} else if state.Matches(profile) {
			if err := a.StateManager.VerifyExpectedManagedProcess(state, sshArgs); err != nil {
				fmt.Fprintf(a.Err, "refusing to reuse tunnel pid %d: %v\n", state.PID, err)
				return 1, result.existingLiveStateConflict(state.PID)
			}
			fmt.Fprintf(a.Out, "already started %s with pid %d\n", stateKey, state.PID)
			result.Action, result.PID = "reused", state.PID
			return 0, result
		} else if errs := a.Validator.ValidateProfileSyntax(profile); len(errs) > 0 {
			printErrors(a.Err, errs)
			return 1, result.existingLiveStateConflict(state.PID)
		} else if errs := a.Validator.ValidateAccess(profile); len(errs) > 0 {
			printErrors(a.Err, errs)
			return 1, result.existingLiveStateConflict(state.PID)
		} else if errs := a.Validator.ValidateNewLocalPorts(profile, state); len(errs) > 0 {
			printErrors(a.Err, errs)
			return 1, result.existingLiveStateConflict(state.PID)
		} else if err := a.StateManager.TerminateManagedProcess(state, a.Runner.Stop); err != nil {
			fmt.Fprintf(a.Err, "stop mismatched managed tunnel pid %d: %v\n", state.PID, err)
			return 1, result.existingLiveStateConflict(state.PID)
		} else if err := a.StateManager.Remove(stateKey); err != nil {
			fmt.Fprintf(a.Err, "remove mismatched managed tunnel state: %v\n", err)
			return 1, result.existingLiveStateConflict(state.PID)
		}
		replacing = true
	}
	syntaxErrs := a.Validator.ValidateProfileSyntax(profile)
	accessErrs := a.Validator.ValidateAccess(profile)
	portErrs := a.Validator.ValidateNewLocalPorts(profile, State{})
	errs := append(append(syntaxErrs, accessErrs...), portErrs...)
	if len(errs) > 0 {
		printErrors(a.Err, errs)
		if len(portErrs) > 0 {
			result.Action = "conflict"
		}
		return 1, result
	}
	printSummary(a.Out, profile)
	check, err := a.fixHostKey(ctx, profile)
	if err != nil {
		fmt.Fprintf(a.Err, "host key fix failed: %v\n", err)
		return 1, result
	}
	fmt.Fprintf(a.Out, "Host key: %s (%s)\n", check.Status, check.Message)
	if check.Status == HostKeyScanFailed {
		fmt.Fprintf(a.Err, "host key scan failed for %s: %s\n", profile.Host, check.Message)
		return 1, result
	}
	pid, err := a.Runner.StartBackground(ctx, sshArgs)
	if err != nil {
		fmt.Fprintf(a.Err, "start ssh tunnel failed: %v\n", err)
		return 1, result
	}
	identity, err := a.StateManager.InspectExpectedProcess(pid, sshArgs)
	if err != nil {
		fmt.Fprintf(a.Err, "inspect started tunnel pid %d: %v; cannot safely terminate the unverified process, manually verify and terminate it\n", pid, err)
		return 1, result
	}
	state := NewState(profile, pid, identity)
	if err := a.StateManager.Save(state); err != nil {
		cleanupErr := a.StateManager.TerminateManagedProcess(state, a.Runner.Stop)
		if cleanupErr != nil {
			fmt.Fprintf(a.Err, "save state for started tunnel pid %d: %v; cleanup failed: %v\n", pid, err, cleanupErr)
		} else {
			fmt.Fprintf(a.Err, "save state for started tunnel pid %d: %v; stopped unrecorded tunnel\n", pid, err)
		}
		return 1, result
	}
	fmt.Fprintf(a.Out, "started %s with pid %d\n", profile.Name, pid)
	result.Action, result.PID = "started", pid
	if replacing {
		result.Action = "restarted"
	}
	return 0, result
}

func profileLocalPorts(profile Profile) []int {
	ports := make([]int, 0, len(profile.Tunnels))
	for _, tunnel := range profile.Tunnels {
		ports = append(ports, tunnel.LocalPort)
	}
	return ports
}
func (a App) runSSH(ctx context.Context, cfg Config, args []string) int {
	startedAt := time.Now()
	if len(args) != 1 {
		fmt.Fprintln(a.Err, "usage: cm ssh <profile>")
		return 2
	}
	profile, ok := cfg.Profile(args[0])
	if !ok {
		fmt.Fprintln(a.Err, unknownProfileError(cfg, args[0]))
		return 2
	}
	profile = a.promptMissingIdentityFile(profile)
	errs := a.Validator.ValidateAccess(profile)
	if len(errs) > 0 {
		printErrors(a.Err, errs)
		a.logLocalCommand(ctx, "ssh.failed", profile, 1, startedAt, LogEntry{ErrorCode: "validation_error"})
		return 1
	}
	sshArgs, err := InteractiveSSHArgs(profile)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		a.logLocalCommand(ctx, "ssh.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	fmt.Fprintf(a.Out, "SSH: %s@%s\n", profile.User, profile.Host)
	a.logLocalCommand(ctx, "ssh.attempted", profile, 0, startedAt, LogEntry{Phase: "attempted"})
	if err := a.Runner.RunForeground(ctx, sshArgs); err != nil {
		fmt.Fprintf(a.Err, "ssh failed: %v\n", err)
		a.logLocalCommand(ctx, "ssh.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	a.logLocalCommand(ctx, "ssh.succeeded", profile, 0, startedAt, LogEntry{Phase: "closed"})
	return 0
}
func (a App) runExec(ctx context.Context, cfg Config, args []string) int {
	startedAt := time.Now()
	if len(args) >= 2 && args[1] == "--" {
		args = append(args[:1], args[2:]...)
	}
	if len(args) < 2 {
		fmt.Fprintln(a.Err, "usage: cm exec <profile> -- <command>")
		return 2
	}
	profile, ok := cfg.Profile(args[0])
	if !ok {
		fmt.Fprintln(a.Err, unknownProfileError(cfg, args[0]))
		return 2
	}
	profile = a.promptMissingIdentityFile(profile)
	a.logLocalCommand(ctx, "ssh.exec.attempted", profile, 0, startedAt, LogEntry{Phase: "attempted"})
	errs := a.Validator.ValidateAccess(profile)
	if len(errs) > 0 {
		printErrors(a.Err, errs)
		a.logLocalCommand(ctx, "ssh.exec.failed", profile, 1, startedAt, LogEntry{ErrorCode: "validation_error"})
		return 1
	}
	command := args[1:]
	sshArgs, err := ExecSSHArgs(profile, command)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		a.logLocalCommand(ctx, "ssh.exec.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	fmt.Fprintf(a.Out, "Exec: %s@%s %s\n", profile.User, profile.Host, strings.Join(command, " "))
	if err := a.Runner.RunForeground(ctx, sshArgs); err != nil {
		fmt.Fprintf(a.Err, "ssh exec failed: %v\n", err)
		a.logLocalCommand(ctx, "ssh.exec.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	a.logLocalCommand(ctx, "ssh.exec.succeeded", profile, 0, startedAt, LogEntry{Phase: "closed"})
	return 0
}
func (a App) runOpenVNC(ctx context.Context, cfg Config, args []string) int {
	startedAt := time.Now()
	if len(args) != 1 {
		fmt.Fprintln(a.Err, "usage: cm open-vnc <profile>")
		return 2
	}
	profile, ok := cfg.Profile(args[0])
	if !ok {
		fmt.Fprintln(a.Err, unknownProfileError(cfg, args[0]))
		return 2
	}
	target, err := VNCURL(profile)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		a.logLocalCommand(ctx, "vnc.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	fmt.Fprintf(a.Out, "Opening %s\n", target)
	a.logLocalCommand(ctx, "vnc.requested", profile, 0, startedAt, LogEntry{Phase: "requested"})
	if err := a.Runner.OpenVNC(ctx, target); err != nil {
		fmt.Fprintf(a.Err, "open failed: %v\n", err)
		a.logLocalCommand(ctx, "vnc.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	a.logLocalCommand(ctx, "vnc.launched", profile, 0, startedAt, LogEntry{Phase: "launched"})
	return 0
}
func (a App) runForgetHost(ctx context.Context, cfg Config, args []string) int {
	startedAt := time.Now()
	if len(args) != 1 {
		fmt.Fprintln(a.Err, "usage: cm forget-host <profile>")
		return 2
	}
	profile, ok := cfg.Profile(args[0])
	if !ok {
		fmt.Fprintln(a.Err, unknownProfileError(cfg, args[0]))
		return 2
	}
	if profile.Host == "" {
		fmt.Fprintln(a.Err, "host is required")
		return 1
	}
	fmt.Fprintf(a.Out, "Removing known_hosts entries for %s\n", profile.Host)
	if err := a.Runner.ForgetHost(ctx, profile.Host); err != nil {
		fmt.Fprintf(a.Err, "ssh-keygen failed: %v\n", err)
		a.logLocalCommand(ctx, "known-host.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	a.logLocalCommand(ctx, "known-host.forgotten", profile, 0, startedAt, LogEntry{})
	return 0
}
func (a App) runHostKey(ctx context.Context, cfg Config, args []string) int {
	startedAt := time.Now()
	if len(args) != 2 {
		fmt.Fprintln(a.Err, "usage: cm host-key <check|fix> <profile>")
		return 2
	}
	action := args[0]
	profile, ok := cfg.Profile(args[1])
	if !ok {
		fmt.Fprintln(a.Err, unknownProfileError(cfg, args[1]))
		return 2
	}
	switch action {
	case "check":
		check, err := a.checkHostKey(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "host key check failed: %v\n", err)
			a.logLocalCommand(ctx, "known-host.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
			return 1
		}
		fmt.Fprintf(a.Out, "Host key: %s (%s)\n", check.Status, check.Message)
		if check.Status == HostKeyScanFailed {
			a.logLocalCommand(ctx, "known-host.failed", profile, 1, startedAt, LogEntry{ErrorCode: "host_key_scan_failed"})
			return 1
		}
		a.logLocalCommand(ctx, "known-host.checked", profile, 0, startedAt, LogEntry{})
		return 0
	case "fix":
		check, err := a.fixHostKey(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "host key fix failed: %v\n", err)
			a.logLocalCommand(ctx, "known-host.failed", profile, 1, startedAt, LogEntry{ErrorCode: classifyOperationalError(err).Code})
			return 1
		}
		fmt.Fprintf(a.Out, "Host key: %s (%s)\n", check.Status, check.Message)
		if check.Status == HostKeyScanFailed {
			a.logLocalCommand(ctx, "known-host.failed", profile, 1, startedAt, LogEntry{ErrorCode: "host_key_scan_failed"})
			return 1
		}
		a.logLocalCommand(ctx, "known-host.fixed", profile, 0, startedAt, LogEntry{})
		return 0
	default:
		fmt.Fprintf(a.Err, "unknown host-key command %q\n", action)
		return 2
	}
}
func (a App) runStop(args []string) int {
	startedAt := time.Now()
	if len(args) != 1 {
		fmt.Fprintln(a.Err, "usage: cm stop <profile>")
		return 2
	}
	extra := LogEntry{}
	if state, ok, err := a.StateManager.Load(args[0]); err == nil && ok {
		extra.PID = state.PID
		for _, tunnel := range state.Tunnels {
			extra.LocalPorts = append(extra.LocalPorts, tunnel.LocalPort)
		}
	}
	code := 0
	err := a.StateManager.WithProfileLock(args[0], func() error {
		code = a.runStopLocked(args[0])
		return nil
	})
	if err != nil {
		fmt.Fprintf(a.Err, "lock stop lifecycle: %v\n", err)
		extra.ErrorCode = classifyOperationalError(err).Code
		a.logLocalCommand(context.Background(), "tunnel.failed", Profile{Name: args[0]}, 1, startedAt, extra)
		return 1
	}
	action := "tunnel.stopped"
	if code != 0 {
		action = "tunnel.failed"
	}
	a.logLocalCommand(context.Background(), action, Profile{Name: args[0]}, code, startedAt, extra)
	return code
}

func (a App) logLocalCommand(ctx context.Context, action string, profile Profile, code int, startedAt time.Time, extra LogEntry) {
	op := operationContextFrom(ctx)
	if op.Source == "local-agent-internal" {
		return
	}
	if op.Source == "" {
		op.Source = "cli"
	}
	extra.Action = action
	extra.Profile = profile.Name
	extra.AppleEmail = profile.AWS.AccountEmail
	extra.RequestID = op.RequestID
	extra.JobID = op.JobID
	extra.Source = op.Source
	extra.DurationMS = elapsedDurationMS(startedAt)
	extra.Outcome = outcomeForCode(code)
	if extra.Message == "" {
		extra.Message = action
	}
	if code != 0 {
		if extra.Level == "" {
			extra.Level = "error"
		}
		if extra.ErrorCode == "" {
			extra.ErrorCode = "command_failed"
		}
	}
	a.writeRuntimeLog(extra)
}

func (a App) runStopLocked(profile string) int {
	state, ok, err := a.StateManager.Load(profile)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	if !ok {
		fmt.Fprintf(a.Err, "no running managed tunnel for %s\n", profile)
		return 1
	}
	if a.StateManager.IsRunning != nil && !a.StateManager.IsRunning(state.PID) {
		if err := a.StateManager.Remove(profile); err != nil {
			fmt.Fprintf(a.Err, "remove stale tunnel state: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.Err, "no running managed tunnel for %s (removed stale state)\n", profile)
		return 1
	}
	if err := a.StateManager.TerminateManagedProcess(state, a.Runner.Stop); err != nil {
		fmt.Fprintf(a.Err, "stop pid %d: %v\n", state.PID, err)
		return 1
	}
	if err := a.StateManager.Remove(profile); err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	fmt.Fprintf(a.Out, "stopped %s\n", profile)
	return 0
}
func (a App) runStatus() int {
	states, err := a.StateManager.List()
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	if len(states) == 0 {
		fmt.Fprintln(a.Out, "no managed tunnels running")
		return 0
	}
	for _, state := range states {
		fmt.Fprintf(a.Out, "%s\tpid=%d\ttarget=%s", state.Profile, state.PID, state.Target)
		for _, tunnel := range state.Tunnels {
			fmt.Fprintf(a.Out, "\t%s", TunnelSummary(tunnel))
		}
		fmt.Fprintln(a.Out)
	}
	return 0
}
