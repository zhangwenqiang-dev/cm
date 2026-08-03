package connectmac

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

const connectMacRsyncEnv = "CONNECTMAC_RSYNC"

type RsyncOptions struct {
	Path      string
	Progress2 bool
}

func RemoteTarget(profile Profile, path string) string {
	return fmt.Sprintf("%s@%s:%s", profile.User, profile.Host, path)
}

func NormalizeRemotePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	cleanHome := filepath.Clean(home)
	cleanPath := filepath.Clean(path)
	if cleanPath == cleanHome {
		return "~"
	}
	if strings.HasPrefix(cleanPath, cleanHome+string(os.PathSeparator)) {
		rel, err := filepath.Rel(cleanHome, cleanPath)
		if err != nil || rel == "." {
			return path
		}
		normalized := "~/" + filepath.ToSlash(rel)
		if strings.HasSuffix(path, string(os.PathSeparator)) || strings.HasSuffix(path, "/") {
			normalized += "/"
		}
		return normalized
	}
	return path
}

type SyncFilters struct {
	Includes []string
	Excludes []string
}

func RsyncPullArgs(profile Profile, remotePath, localDir string, filters SyncFilters) ([]string, error) {
	return RsyncPullArgsWithOptions(profile, remotePath, localDir, filters, RsyncOptions{})
}

func RsyncPullArgsWithOptions(profile Profile, remotePath, localDir string, filters SyncFilters, options RsyncOptions) ([]string, error) {
	keyPath, err := ExpandPath(profile.IdentityFile)
	if err != nil {
		return nil, err
	}
	args := rsyncBaseArgs(options)
	args = append(args, "-e", "ssh -i "+keyPath)
	args = appendRsyncFilters(args, filters)
	args = append(args, RemoteTarget(profile, EscapeRemotePath(remotePath)), localDir)
	return args, nil
}

func RsyncPushArgs(profile Profile, localPath, remoteDir string, filters SyncFilters) ([]string, error) {
	return RsyncPushArgsWithOptions(profile, localPath, remoteDir, filters, RsyncOptions{})
}

func RsyncPushArgsWithOptions(profile Profile, localPath, remoteDir string, filters SyncFilters, options RsyncOptions) ([]string, error) {
	keyPath, err := ExpandPath(profile.IdentityFile)
	if err != nil {
		return nil, err
	}
	remoteDir = NormalizeRemotePath(remoteDir)
	args := rsyncBaseArgs(options)
	args = append(args, "-e", "ssh -i "+keyPath)
	args = appendRsyncFilters(args, filters)
	args = append(args, localPath, RemoteTarget(profile, EscapeRemotePath(remoteDir)))
	return args, nil
}

func rsyncBaseArgs(options RsyncOptions) []string {
	if options.Progress2 {
		return []string{"-avz", "--partial", "--info=progress2"}
	}
	return []string{"-avzP"}
}

func defaultRsyncCommandPath() string {
	candidates := rsyncPathCandidates()
	if len(candidates) == 0 {
		return "rsync"
	}
	return candidates[0]
}

func EscapeRemotePath(path string) string {
	var b strings.Builder
	for _, r := range path {
		if shouldEscapeRemotePathRune(r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func shouldEscapeRemotePathRune(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	return strings.ContainsRune("\\'\"$`&;()|<>*?[]{}!", r)
}

func appendRsyncFilters(args []string, filters SyncFilters) []string {
	for _, include := range filters.Includes {
		args = append(args, "--include", include)
	}
	for _, exclude := range filters.Excludes {
		args = append(args, "--exclude", exclude)
	}
	if len(filters.Includes) > 0 {
		args = append(args, "--exclude", "*")
	}
	return args
}

func DetectRsyncOptions(ctx context.Context, runner Runner) RsyncOptions {
	candidates := rsyncPathCandidates()
	if len(candidates) == 0 {
		return RsyncOptions{}
	}
	for _, path := range candidates {
		if rsyncSupportsProgress2(ctx, runner, path) {
			return RsyncOptions{Path: path, Progress2: true}
		}
	}
	return RsyncOptions{Path: candidates[0]}
}

func rsyncPathCandidates() []string {
	var candidates []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		for _, existing := range candidates {
			if existing == path {
				return
			}
		}
		candidates = append(candidates, path)
	}
	if configured := os.Getenv(connectMacRsyncEnv); configured != "" {
		add(configured)
		return candidates
	}
	if path, err := exec.LookPath("rsync"); err == nil {
		add(path)
	}
	for _, path := range []string{"/usr/local/bin/rsync", "/opt/homebrew/bin/rsync"} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			add(path)
		}
	}
	add("rsync")
	return candidates
}

func rsyncSupportsProgress2(ctx context.Context, runner Runner, path string) bool {
	if runner == nil {
		return false
	}
	_, err := runner.RsyncCommandOutput(ctx, path, []string{"--info=progress2", "--version"})
	return err == nil
}
