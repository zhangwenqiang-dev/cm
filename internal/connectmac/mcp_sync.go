package connectmac

import (
	"context"
	"fmt"
	"os"
)

func (s MCPServer) mcpPush(ctx context.Context, cfg Config, args map[string]interface{}) (interface{}, error) {
	profile, err := requireMCPProfile(cfg, args)
	if err != nil {
		return mcpUserError("profile", err), nil
	}
	if text := missingMCPInputText(profile, false); text != "" {
		return mcpTextData(text, mcpSyncData(profile, "push", false, "", "", SyncFilters{}, text)), nil
	}
	localPath, err := requiredString(args, "local_path")
	if err != nil {
		return mcpUserError("local_path", err), nil
	}
	remoteDir, err := requiredString(args, "remote_dir")
	if err != nil {
		return mcpUserError("remote_dir", err), nil
	}
	remoteDir = NormalizeRemotePath(remoteDir)
	extraFilters, err := mcpSyncFilters(args)
	if err != nil {
		return mcpUserError("sync_filters", err), nil
	}
	filters := mergeSyncFilters(profile.Sync.Push, extraFilters)
	preview := fmt.Sprintf("Push %s -> %s\n", localPath, RemoteTarget(profile, remoteDir))
	if !boolArg(args, "confirm") {
		return mcpTextData(preview+"Preview only. Call again with confirm=true to execute.", mcpSyncData(profile, "push", false, localPath, remoteDir, filters, "")), nil
	}
	if _, err := os.Stat(localPath); err != nil {
		return mcpUserError("local_path", fmt.Errorf("read local path %s: %w", localPath, err)), nil
	}
	if !s.validateMCPRsync(profile) {
		return mcpTextData("profile validation failed", mcpSyncData(profile, "push", true, localPath, remoteDir, filters, "profile validation failed")), nil
	}
	if _, err := s.App.requireCurrentHostKey(ctx, profile); err != nil {
		return nil, err
	}
	rsyncArgs, err := RsyncPushArgs(profile, localPath, remoteDir, filters)
	if err != nil {
		return mcpUserError("rsync_args", err), nil
	}
	if err := s.App.Runner.RunRsync(ctx, rsyncArgs); err != nil {
		return nil, err
	}
	return mcpTextData(preview+"Executed.", mcpSyncData(profile, "push", true, localPath, remoteDir, filters, "")), nil
}
func (s MCPServer) mcpPull(ctx context.Context, cfg Config, args map[string]interface{}) (interface{}, error) {
	profile, err := requireMCPProfile(cfg, args)
	if err != nil {
		return mcpUserError("profile", err), nil
	}
	if text := missingMCPInputText(profile, false); text != "" {
		return mcpTextData(text, mcpSyncData(profile, "pull", false, "", "", SyncFilters{}, text)), nil
	}
	remotePath, err := requiredString(args, "remote_path")
	if err != nil {
		return mcpUserError("remote_path", err), nil
	}
	localDir := stringArg(args, "local_dir", ".")
	extraFilters, err := mcpSyncFilters(args)
	if err != nil {
		return mcpUserError("sync_filters", err), nil
	}
	filters := mergeSyncFilters(profile.Sync.Pull, extraFilters)
	preview := fmt.Sprintf("Pull %s -> %s\n", RemoteTarget(profile, remotePath), localDir)
	if !boolArg(args, "confirm") {
		return mcpTextData(preview+"Preview only. Call again with confirm=true to execute.", mcpSyncData(profile, "pull", false, remotePath, localDir, filters, "")), nil
	}
	if !s.validateMCPRsync(profile) {
		return mcpTextData("profile validation failed", mcpSyncData(profile, "pull", true, remotePath, localDir, filters, "profile validation failed")), nil
	}
	if _, err := s.App.requireCurrentHostKey(ctx, profile); err != nil {
		return nil, err
	}
	rsyncArgs, err := RsyncPullArgs(profile, remotePath, localDir, filters)
	if err != nil {
		return mcpUserError("rsync_args", err), nil
	}
	if err := s.App.Runner.RunRsync(ctx, rsyncArgs); err != nil {
		return nil, err
	}
	return mcpTextData(preview+"Executed.", mcpSyncData(profile, "pull", true, remotePath, localDir, filters, "")), nil
}
func (s MCPServer) mcpForgetHost(ctx context.Context, cfg Config, args map[string]interface{}) (interface{}, error) {
	profile, err := requireMCPProfile(cfg, args)
	if err != nil {
		return mcpUserError("profile", err), nil
	}
	preview := fmt.Sprintf("Remove known_hosts entries for %s\n", profile.Host)
	if !boolArg(args, "confirm") {
		return mcpText(preview + "Preview only. Call again with confirm=true to execute."), nil
	}
	if profile.Host == "" {
		return mcpUserError("host", fmt.Errorf("host is required")), nil
	}
	if err := s.App.Runner.ForgetHost(ctx, profile.Host); err != nil {
		return nil, err
	}
	return mcpText(preview + "Executed."), nil
}
func (s MCPServer) validateMCPRsync(profile Profile) bool {
	errs := s.App.Validator.ValidateAccess(profile)
	if s.App.Validator.CheckRsync != nil {
		if err := s.App.Validator.CheckRsync(); err != nil {
			errs = append(errs, err)
		}
	}
	return len(errs) == 0
}
