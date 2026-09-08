package connectmac

import (
	"context"
	"fmt"
	"os"
	"time"
)

func (a App) runPull(ctx context.Context, cfg Config, args []string) int {
	startedAt := time.Now()
	if len(args) < 2 {
		fmt.Fprintln(a.Err, "usage: cm pull <profile-or-apple-email> <remote-path> [--include <pattern>] [--exclude <pattern>]")
		return 2
	}
	extraFilters, err := parseSyncFilterFlags(args[2:])
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 2
	}
	profile, err := resolveProfileRef(cfg, args[0])
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 2
	}
	profile = a.promptMissingIdentityFile(profile)
	if !a.validateRsyncAccess(profile) {
		a.logLocalCommand(ctx, "sync.pull.failed", profile, 1, startedAt, LogEntry{Direction: "pull", ErrorCode: "validation_error"})
		return 1
	}
	if _, err := a.requireCurrentHostKey(ctx, profile); err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	rsyncArgs, err := RsyncPullArgs(profile, args[1], ".", mergeSyncFilters(profile.Sync.Pull, extraFilters))
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	fmt.Fprintf(a.Out, "Pull: %s -> .\n", RemoteTarget(profile, args[1]))
	a.logLocalCommand(ctx, "sync.pull.started", profile, 0, startedAt, LogEntry{Direction: "pull", Phase: "started"})
	if err := a.Runner.RunRsync(ctx, rsyncArgs); err != nil {
		fmt.Fprintf(a.Err, "rsync failed: %v\n", err)
		a.logLocalCommand(ctx, "sync.pull.failed", profile, 1, startedAt, LogEntry{Direction: "pull", ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	a.logLocalCommand(ctx, "sync.pull.succeeded", profile, 0, startedAt, LogEntry{Direction: "pull"})
	return 0
}
func (a App) runPush(ctx context.Context, cfg Config, args []string) int {
	startedAt := time.Now()
	if len(args) < 3 {
		fmt.Fprintln(a.Err, "usage: cm push <profile-or-apple-email> <local-path> <remote-dir> [--include <pattern>] [--exclude <pattern>]")
		return 2
	}
	extraFilters, err := parseSyncFilterFlags(args[3:])
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 2
	}
	profile, err := resolveProfileRef(cfg, args[0])
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 2
	}
	profile = a.promptMissingIdentityFile(profile)
	if !a.validateRsyncAccess(profile) {
		a.logLocalCommand(ctx, "sync.push.failed", profile, 1, startedAt, LogEntry{Direction: "push", ErrorCode: "validation_error"})
		return 1
	}
	if _, err := a.requireCurrentHostKey(ctx, profile); err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	localPath := args[1]
	if _, err := os.Stat(localPath); err != nil {
		fmt.Fprintf(a.Err, "read local path %s: %v\n", localPath, err)
		return 1
	}
	remoteDir := NormalizeRemotePath(args[2])
	rsyncArgs, err := RsyncPushArgs(profile, localPath, remoteDir, mergeSyncFilters(profile.Sync.Push, extraFilters))
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	fmt.Fprintf(a.Out, "Push: %s -> %s\n", localPath, RemoteTarget(profile, remoteDir))
	a.logLocalCommand(ctx, "sync.push.started", profile, 0, startedAt, LogEntry{Direction: "push", Phase: "started"})
	if err := a.Runner.RunRsync(ctx, rsyncArgs); err != nil {
		fmt.Fprintf(a.Err, "rsync failed: %v\n", err)
		a.logLocalCommand(ctx, "sync.push.failed", profile, 1, startedAt, LogEntry{Direction: "push", ErrorCode: classifyOperationalError(err).Code})
		return 1
	}
	a.logLocalCommand(ctx, "sync.push.succeeded", profile, 0, startedAt, LogEntry{Direction: "push"})
	return 0
}
func (a App) validateRsyncAccess(profile Profile) bool {
	errs := a.Validator.ValidateAccess(profile)
	if a.Validator.CheckRsync != nil {
		if err := a.Validator.CheckRsync(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		printErrors(a.Err, errs)
		return false
	}
	return true
}
func parseSyncFilterFlags(args []string) (SyncFilters, error) {
	var filters SyncFilters
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--include":
			i++
			if i >= len(args) || args[i] == "" {
				return filters, fmt.Errorf("--include requires a value")
			}
			filters.Includes = append(filters.Includes, args[i])
		case "--exclude":
			i++
			if i >= len(args) || args[i] == "" {
				return filters, fmt.Errorf("--exclude requires a value")
			}
			filters.Excludes = append(filters.Excludes, args[i])
		default:
			return filters, fmt.Errorf("unknown sync option %q", args[i])
		}
	}
	return filters, nil
}
func mergeSyncFilters(direction SyncDirection, extra SyncFilters) SyncFilters {
	return SyncFilters{
		Includes: append(append([]string{}, direction.Includes...), extra.Includes...),
		Excludes: append(append([]string{}, direction.Excludes...), extra.Excludes...),
	}
}
