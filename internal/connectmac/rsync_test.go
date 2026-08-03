package connectmac

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRsyncPullArgs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profile := validProfile("~/.ssh/example.pem")
	got, err := RsyncPullArgs(profile, "~/Desktop/App.ipa", ".", SyncFilters{Excludes: []string{".DS_Store"}})
	if err != nil {
		t.Fatalf("RsyncPullArgs returned error: %v", err)
	}
	key := filepath.Join(home, ".ssh", "example.pem")
	want := []string{
		"-avzP",
		"-e", "ssh -i " + key,
		"--exclude", ".DS_Store",
		"user@mac-host.example.com:~/Desktop/App.ipa",
		".",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestRsyncPullArgsWithProgress2(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profile := validProfile("~/.ssh/example.pem")
	got, err := RsyncPullArgsWithOptions(profile, "~/Desktop/App.ipa", ".", SyncFilters{}, RsyncOptions{Progress2: true})
	if err != nil {
		t.Fatalf("RsyncPullArgsWithOptions returned error: %v", err)
	}
	key := filepath.Join(home, ".ssh", "example.pem")
	want := []string{
		"-avz",
		"--partial",
		"--info=progress2",
		"-e", "ssh -i " + key,
		"user@mac-host.example.com:~/Desktop/App.ipa",
		".",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestRsyncPushArgs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profile := validProfile("~/.ssh/example.pem")
	got, err := RsyncPushArgs(profile, "/tmp/project", "~/Downloads/", SyncFilters{Excludes: []string{"xcuserdata", ".git"}})
	if err != nil {
		t.Fatalf("RsyncPushArgs returned error: %v", err)
	}
	key := filepath.Join(home, ".ssh", "example.pem")
	want := []string{
		"-avzP",
		"-e", "ssh -i " + key,
		"--exclude", "xcuserdata",
		"--exclude", ".git",
		"/tmp/project",
		"user@mac-host.example.com:~/Downloads/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestRsyncPushArgsWithProgress2(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profile := validProfile("~/.ssh/example.pem")
	got, err := RsyncPushArgsWithOptions(profile, "/tmp/project", "~/Downloads/", SyncFilters{}, RsyncOptions{Progress2: true})
	if err != nil {
		t.Fatalf("RsyncPushArgsWithOptions returned error: %v", err)
	}
	if !containsString(got, "--info=progress2") || containsString(got, "-avzP") {
		t.Fatalf("progress2 args = %#v", got)
	}
}

func TestDetectRsyncOptionsPrefersProgress2Candidate(t *testing.T) {
	t.Setenv(connectMacRsyncEnv, "/usr/local/bin/rsync")
	runner := &fakeRunner{rsyncProbe: map[string]error{"/usr/local/bin/rsync": nil}}

	options := DetectRsyncOptions(context.Background(), runner)

	if options.Path != "/usr/local/bin/rsync" || !options.Progress2 {
		t.Fatalf("options = %+v", options)
	}
}

func TestDetectRsyncOptionsFallsBackWhenProgress2Unsupported(t *testing.T) {
	t.Setenv(connectMacRsyncEnv, "/usr/bin/rsync")
	runner := &fakeRunner{rsyncProbe: map[string]error{"/usr/bin/rsync": errors.New("unsupported")}}

	options := DetectRsyncOptions(context.Background(), runner)

	if options.Path != "/usr/bin/rsync" || options.Progress2 {
		t.Fatalf("options = %+v", options)
	}
}

func TestRsyncPathCandidatesUsesConfiguredPathOnly(t *testing.T) {
	t.Setenv(connectMacRsyncEnv, " /custom/rsync ")
	candidates := rsyncPathCandidates()
	if len(candidates) != 1 || strings.TrimSpace(candidates[0]) != "/custom/rsync" {
		t.Fatalf("candidates = %#v", candidates)
	}
}

func TestRsyncPushArgsNormalizesShellExpandedHomeRemoteDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profile := validProfile("~/.ssh/example.pem")
	got, err := RsyncPushArgs(profile, "/tmp/project", filepath.Join(home, "Documents")+"/", SyncFilters{})
	if err != nil {
		t.Fatalf("RsyncPushArgs returned error: %v", err)
	}
	key := filepath.Join(home, ".ssh", "example.pem")
	want := []string{
		"-avzP",
		"-e", "ssh -i " + key,
		"/tmp/project",
		"user@mac-host.example.com:~/Documents/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestRsyncArgsIncludeOnlyAddsFinalExcludeAll(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profile := validProfile("~/.ssh/example.pem")
	got, err := RsyncPushArgs(profile, "/tmp/project", "~/Downloads/", SyncFilters{
		Includes: []string{"Sources/***", "*.xcodeproj/***"},
		Excludes: []string{"DerivedData", ".git"},
	})
	if err != nil {
		t.Fatalf("RsyncPushArgs returned error: %v", err)
	}
	key := filepath.Join(home, ".ssh", "example.pem")
	want := []string{
		"-avzP",
		"-e", "ssh -i " + key,
		"--include", "Sources/***",
		"--include", "*.xcodeproj/***",
		"--exclude", "DerivedData",
		"--exclude", ".git",
		"--exclude", "*",
		"/tmp/project",
		"user@mac-host.example.com:~/Downloads/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestNormalizeRemotePathKeepsRemoteAbsolutePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := NormalizeRemotePath("/var/tmp/uploads/")
	if got != "/var/tmp/uploads/" {
		t.Fatalf("path = %q", got)
	}
}

func TestRsyncPullArgsEscapesRemotePathSpaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profile := validProfile("~/.ssh/example.pem")
	got, err := RsyncPullArgs(profile, "~/Documents/Telegram Bot 头像", "/Users/wenqiang/Downloads/", SyncFilters{})
	if err != nil {
		t.Fatalf("RsyncPullArgs returned error: %v", err)
	}
	key := filepath.Join(home, ".ssh", "example.pem")
	want := []string{
		"-avzP",
		"-e", "ssh -i " + key,
		"user@mac-host.example.com:~/Documents/Telegram\\ Bot\\ 头像",
		"/Users/wenqiang/Downloads/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}
