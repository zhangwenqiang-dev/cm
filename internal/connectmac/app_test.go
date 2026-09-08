package connectmac

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type fakeRunner struct {
	foreground     []string
	foregroundErr  error
	background     []string
	startErr       error
	rsync          []string
	rsyncPath      string
	rsyncOutput    []string
	rsyncErr       error
	rsyncWait      <-chan struct{}
	rsyncProbe     map[string]error
	forgotHost     string
	knownHost      string
	missingHostKey bool
	scannedKey     string
	scanKeys       []string
	scanCalls      int
	scanErr        error
	openedURL      string
	openedVNC      string
	openVNCErr     error
	stopPID        int
	stopErr        error
}

type synchronizedRunner struct {
	*fakeRunner
	mu     sync.Mutex
	starts int
}

type blockingStartRunner struct {
	*fakeRunner
	entered chan struct{}
	release chan struct{}
}

func (r *blockingStartRunner) StartBackground(ctx context.Context, args []string) (int, error) {
	close(r.entered)
	select {
	case <-r.release:
		return r.fakeRunner.StartBackground(ctx, args)
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func managedTestState(profile Profile, pid int) State {
	return NewState(profile, pid, testProcessIdentity("ssh fake-managed-tunnel", fmt.Sprintf("start-%d", pid)))
}

func (r *synchronizedRunner) StartBackground(ctx context.Context, args []string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	return r.fakeRunner.StartBackground(ctx, args)
}

func (r *fakeRunner) RunForeground(ctx context.Context, args []string) error {
	r.foreground = args
	return r.foregroundErr
}

func (r *fakeRunner) StartBackground(ctx context.Context, args []string) (int, error) {
	r.background = args
	if r.startErr != nil {
		return 0, r.startErr
	}
	return 55, nil
}

func (r *fakeRunner) Stop(pid int) error {
	r.stopPID = pid
	return r.stopErr
}

func (r *fakeRunner) RunRsync(ctx context.Context, args []string) error {
	r.rsync = args
	return nil
}

func (r *fakeRunner) RunRsyncProgress(ctx context.Context, args []string, onOutput func(string)) error {
	return r.RunRsyncCommandProgress(ctx, "rsync", args, onOutput)
}

func (r *fakeRunner) RunRsyncCommandProgress(ctx context.Context, path string, args []string, onOutput func(string)) error {
	r.rsyncPath = path
	r.rsync = args
	for _, output := range r.rsyncOutput {
		onOutput(output)
	}
	if r.rsyncWait != nil {
		select {
		case <-r.rsyncWait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.rsyncErr
}

func (r *fakeRunner) RsyncCommandOutput(ctx context.Context, path string, args []string) ([]byte, error) {
	if r.rsyncProbe != nil {
		if err, ok := r.rsyncProbe[path]; ok {
			return nil, err
		}
	}
	return []byte("rsync: unrecognized option --info=progress2"), errors.New("exit status 1")
}

func (r *fakeRunner) KnownHostKey(ctx context.Context, host string) (string, error) {
	if r.missingHostKey {
		return "", nil
	}
	if r.knownHost == "" {
		return "mac-host.example.com ssh-ed25519 AAAACURRENT\n", nil
	}
	return r.knownHost, nil
}

func (r *fakeRunner) ScanHostKey(ctx context.Context, host string) (string, error) {
	if r.scanErr != nil {
		return r.scannedKey, r.scanErr
	}
	if len(r.scanKeys) > 0 {
		index := r.scanCalls
		if index >= len(r.scanKeys) {
			index = len(r.scanKeys) - 1
		}
		r.scanCalls++
		return r.scanKeys[index], nil
	}
	if r.scannedKey == "" {
		return "mac-host.example.com ssh-ed25519 AAAACURRENT\n", nil
	}
	return r.scannedKey, nil
}

func (r *fakeRunner) ForgetHost(ctx context.Context, host string) error {
	r.forgotHost = host
	return nil
}

func (r *fakeRunner) OpenURL(ctx context.Context, target string) error {
	r.openedURL = target
	return nil
}

func (r *fakeRunner) OpenVNC(ctx context.Context, target string) error {
	r.openedVNC = target
	return r.openVNCErr
}

func TestLocalAgentVNCLogLifecycleAndLaunch(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		setup       func(*App, *fakeRunner, Profile)
		wantAction  string
		wantPID     int
		wantOutcome string
		wantLaunch  string
	}{
		{name: "started", command: "start", wantAction: "started", wantPID: 55, wantOutcome: "success"},
		{name: "reused", command: "start", wantAction: "reused", wantPID: 77, wantOutcome: "success", setup: func(app *App, _ *fakeRunner, profile Profile) {
			app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
			if err := app.StateManager.Save(managedTestState(profile, 77)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "restarted", command: "start", wantAction: "restarted", wantPID: 55, wantOutcome: "success", setup: func(app *App, _ *fakeRunner, profile Profile) {
			app.StateManager.IsRunning = func(pid int) bool { return pid == 77 || pid == 55 }
			old := profile
			old.Host = "old-host.example.com"
			if err := app.StateManager.Save(managedTestState(old, 77)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "conflict", command: "start", wantAction: "conflict", wantOutcome: "failure", setup: func(app *App, _ *fakeRunner, _ Profile) {
			app.Validator.CheckPort = func(port int) error { return fmt.Errorf("local port %d is already in use", port) }
		}},
		{name: "existing live conflict", command: "start", wantAction: "conflict", wantPID: 77, wantOutcome: "failure", setup: func(app *App, _ *fakeRunner, profile Profile) {
			app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
			state := managedTestState(profile, 77)
			state.ProcessStartMarker = "wrong-start"
			if err := app.StateManager.Save(state); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "launch success", command: "open-vnc", wantPID: 77, wantOutcome: "success", wantLaunch: "success", setup: func(app *App, _ *fakeRunner, profile Profile) {
			app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
			if err := app.StateManager.Save(managedTestState(profile, 77)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "launch failure", command: "open-vnc", wantOutcome: "failure", wantLaunch: "failure", setup: func(_ *App, runner *fakeRunner, _ Profile) {
			runner.openVNCErr = errors.New(`open failed password=hunter2 token=session-token cookie=browser-cookie vnc://user:secret@localhost:5900 /Users/test/.ssh/private.pem`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			key := writeSSHKey(t, 0o600)
			profile := validProfile(key)
			profile.VNC.Username = "mac-user"
			var out, errOut bytes.Buffer
			runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
			app := testApp(&out, &errOut, dir)
			app.Runner = runner
			if tt.setup != nil {
				tt.setup(&app, runner, profile)
			}
			body := strings.NewReader(`{"profile":"xcode-vnc","profile_yaml":` + strconv.Quote(FormatProfileFile(profile)) + `}`)
			req := httptest.NewRequest(http.MethodPost, "/"+tt.command, body)
			req.Header.Set("Origin", "https://cm.hsgitlab.xyz")
			rec := httptest.NewRecorder()
			app.newLocalAgentHandler().ServeHTTP(rec, req)
			var resp webAPIResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v: %s", err, rec.Body.String())
			}
			if resp.OK != (tt.wantOutcome == "success") {
				t.Fatalf("response = %+v", resp)
			}
			entries := readTestLogEntries(t, app.LogManager)
			if len(entries) != 1 {
				t.Fatalf("log entries = %#v", entries)
			}
			entry := entries[0]
			if entry.Action != "local-agent.vnc" || entry.Profile != "xcode-vnc" ||
				!reflect.DeepEqual(entry.LocalPorts, []int{5900}) || entry.TunnelAction != tt.wantAction ||
				entry.PID != tt.wantPID || entry.Outcome != tt.wantOutcome || entry.LaunchResult != tt.wantLaunch ||
				entry.RequestID == "" || entry.Source != "web-local" || entry.DurationMS < 1 {
				t.Fatalf("entry = %+v", entry)
			}
			raw := readTestLogsRaw(t, app.LogManager)
			for _, secret := range []string{key, "hunter2", "session-token", "browser-cookie", "user:secret", "profile_yaml", "PRIVATE KEY"} {
				if strings.Contains(raw, secret) {
					t.Fatalf("log contains secret %q: %s", secret, raw)
				}
			}
		})
	}
}

func TestLocalAgentVNCStartSyntaxFailureReportsExistingLiveStateConflict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	oldProfile := validProfile(key)
	newProfile := validProfile(key)
	newProfile.Host = "replacement-host.example.com"
	newProfile.Tunnels[0].RemotePort = 0
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	original := managedTestState(oldProfile, 77)
	if err := app.StateManager.Save(original); err != nil {
		t.Fatal(err)
	}
	original, ok, err := app.StateManager.Load(oldProfile.Name)
	if err != nil || !ok {
		t.Fatalf("load original state: ok=%t err=%v", ok, err)
	}

	code, result := app.runStartLockedResult(context.Background(), newProfile, newProfile.Name)

	if code != 1 || result.Action != "conflict" || result.PID != 77 {
		t.Fatalf("code=%d result=%+v err=%q", code, result, errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("existing pid was stopped: %d", runner.stopPID)
	}
	got, ok, err := app.StateManager.Load(oldProfile.Name)
	if err != nil || !ok || !reflect.DeepEqual(got, original) {
		t.Fatalf("existing state changed: got=%+v ok=%t err=%v want=%+v", got, ok, err, original)
	}
}

func TestLocalAgentVNCStartAccessFailureReportsExistingLiveStateConflict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	oldProfile := validProfile(key)
	newProfile := validProfile(key)
	newProfile.Host = "replacement-host.example.com"
	newProfile.User = ""
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	original := managedTestState(oldProfile, 77)
	if err := app.StateManager.Save(original); err != nil {
		t.Fatal(err)
	}
	original, ok, err := app.StateManager.Load(oldProfile.Name)
	if err != nil || !ok {
		t.Fatalf("load original state: ok=%t err=%v", ok, err)
	}

	code, result := app.runStartLockedResult(context.Background(), newProfile, newProfile.Name)

	if code != 1 || result.Action != "conflict" || result.PID != 77 {
		t.Fatalf("code=%d result=%+v err=%q", code, result, errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("existing pid was stopped: %d", runner.stopPID)
	}
	got, ok, err := app.StateManager.Load(oldProfile.Name)
	if err != nil || !ok || !reflect.DeepEqual(got, original) {
		t.Fatalf("existing state changed: got=%+v ok=%t err=%v want=%+v", got, ok, err, original)
	}
}

func TestLocalAgentVNCEarlyFailuresLogGenericSecretSafeEntry(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		body        string
		wantStatus  int
		wantProfile string
		wantStage   string
	}{
		{
			name: "start decode", command: "start",
			body:       `{"profile":"secret-profile","profile_yaml":"password=hunter2`,
			wantStatus: http.StatusBadRequest,
			wantStage:  "request",
		},
		{
			name: "open-vnc config", command: "open-vnc",
			body:        `{"profile":"malicious-profile-token","profile_yaml":"password=hunter2 token=session-token"}`,
			wantStatus:  http.StatusOK,
			wantProfile: "malicious-profile-token",
			wantStage:   "request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, dir)
			req := httptest.NewRequest(http.MethodPost, "/"+tt.command, strings.NewReader(tt.body))
			req.Header.Set("Origin", "https://cm.hsgitlab.xyz")
			rec := httptest.NewRecorder()
			app.newLocalAgentHandler().ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			entries := readTestLogEntries(t, app.LogManager)
			if len(entries) != 1 {
				t.Fatalf("entries = %#v", entries)
			}
			entry := entries[0]
			if entry.Action != "local-agent.vnc" || entry.Outcome != "failure" ||
				entry.Message == "" || entry.ErrorCode == "" || entry.FailureStage != tt.wantStage ||
				entry.Profile != tt.wantProfile || entry.TunnelAction != "" || entry.LaunchResult != "" {
				t.Fatalf("entry = %+v", entry)
			}
			raw := readTestLogsRaw(t, app.LogManager)
			for _, secret := range []string{"hunter2", "session-token", "secret-profile", "profile_yaml"} {
				if strings.Contains(raw, secret) {
					t.Fatalf("log contains secret %q: %s", secret, raw)
				}
			}
		})
	}
}

func TestLocalAgentOpenVNCWaitsForConcurrentRestartAndLogsVerifiedPID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	profile := validProfile(key)
	profile.VNC.Username = "mac-user"
	var startOut, startErr, openOut, openErr bytes.Buffer
	runner := &blockingStartRunner{
		fakeRunner: &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"},
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	startApp := testApp(&startOut, &startErr, dir)
	startApp.Runner = runner
	openApp := testApp(&openOut, &openErr, dir)
	openApp.Runner = runner
	startDone := make(chan int, 1)
	go func() {
		cfg := Config{Profiles: map[string]Profile{profile.Name: profile}}
		startDone <- startApp.runStart(context.Background(), cfg, []string{profile.Name})
	}()
	<-runner.entered

	openDone := make(chan webAPIResponse, 1)
	beforeLock := make(chan struct{})
	go func() {
		_, configPath, err := writeLocalAgentProfileConfig(localAgentRequest{
			Profile: profile.Name, ProfileYAML: FormatProfileFile(profile),
		})
		if err != nil {
			t.Errorf("write profile config: %v", err)
			return
		}
		openDone <- openApp.localAgentRunVNCWithBeforeLock(context.Background(), "open-vnc", profile, configPath, func() {
			close(beforeLock)
		})
	}()
	<-beforeLock
	select {
	case resp := <-openDone:
		t.Fatalf("open-vnc completed before restart released lock: %+v", resp)
	default:
	}
	close(runner.release)
	if code := <-startDone; code != 0 {
		t.Fatalf("start code=%d err=%q", code, startErr.String())
	}
	resp := <-openDone
	if !resp.OK || resp.Code != 0 {
		t.Fatalf("open response=%+v", resp)
	}
	entries := readTestLogEntries(t, openApp.LogManager)
	var launch LogEntry
	for _, entry := range entries {
		if entry.Action == "local-agent.vnc" {
			launch = entry
		}
	}
	if launch.PID != 55 || launch.LaunchResult != "success" {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestLocalAgentOpenVNCLockFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	profile := validProfile(key)
	profile.VNC.Username = "mac-user"
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	blockedStateDir := filepath.Join(dir, "blocked-state")
	if err := os.WriteFile(blockedStateDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	app.StateManager.Dir = blockedStateDir
	_, configPath, err := writeLocalAgentProfileConfig(localAgentRequest{
		Profile: profile.Name, ProfileYAML: FormatProfileFile(profile),
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := app.localAgentRunVNC(context.Background(), "open-vnc", profile, configPath)
	if resp.OK || resp.Code != 1 || !strings.Contains(resp.Error, "lock open-vnc lifecycle") {
		t.Fatalf("response=%+v", resp)
	}
	if runner.openedVNC != "" {
		t.Fatalf("open-vnc ran despite lock failure: %q", runner.openedVNC)
	}
	entries := readTestLogEntries(t, app.LogManager)
	if len(entries) != 1 || entries[0].LaunchResult != "failure" || entries[0].PID != 0 {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestLocalAgentVNCLogFailureDoesNotChangeResponse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	profile := validProfile(key)
	profile.VNC.Username = "mac-user"
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.Runner = &fakeRunner{}
	blockedLogPath := filepath.Join(dir, "blocked-log-path")
	if err := os.WriteFile(blockedLogPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	app.LogManager.Dir = blockedLogPath

	body := strings.NewReader(`{"profile":"xcode-vnc","profile_yaml":` + strconv.Quote(FormatProfileFile(profile)) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/open-vnc", body)
	req.Header.Set("Origin", "https://cm.hsgitlab.xyz")
	rec := httptest.NewRecorder()
	app.newLocalAgentHandler().ServeHTTP(rec, req)
	var resp webAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.Code != 0 {
		t.Fatalf("logging failure changed response: %+v", resp)
	}
}

func readTestLogEntries(t *testing.T, manager LogManager) []LogEntry {
	t.Helper()
	raw := readTestLogsRaw(t, manager)
	var entries []LogEntry
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var entry LogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log entry: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func readTestLogsRaw(t *testing.T, manager LogManager) string {
	t.Helper()
	files, err := manager.List()
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("log files = %#v", files)
	}
	data, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(data)
}

func testScannedHostKey(t *testing.T, host string) (string, string) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n", ssh.FingerprintSHA256(key)
}

func TestAppInitCreatesConfigWithoutExampleProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	config := filepath.Join(dir, "config.yaml")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.In = strings.NewReader("n\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) { return "", nil }
	if code := app.Run(context.Background(), []string{"init", "--config", config}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatalf("read created config: %v", err)
	}
	if strings.Contains(string(data), "profiles:") || strings.Contains(string(data), "xcode-vnc") {
		t.Fatalf("init created example profile:\n%s", data)
	}
}

func TestAppListFormatsProfilesAsTable(t *testing.T) {
	cfg := Config{Profiles: map[string]Profile{
		"short": {
			Name:        "short",
			Description: "Apple account: user@example.com",
		},
		"long-profile-name": {
			Name: "long-profile-name",
		},
	}}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.runList(context.Background(), DefaultConfigPath, cfg); code != 0 {
		t.Fatalf("list code = %d", code)
	}
	text := out.String()
	for _, want := range []string{"PROFILE", "DESCRIPTION", "long-profile-name  -", "short"} {
		if !strings.Contains(text, want) {
			t.Fatalf("list output missing %q: %q", want, text)
		}
	}
}

func TestAppListFetchesRemoteProfiles(t *testing.T) {
	var seenAuth string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/managed-profiles" || r.URL.Query().Get("include_yaml") != "1" {
			t.Fatalf("unexpected request path = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if seenAuth != "Bearer cm_api_remote" {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profiles": []webManagedProfile{{
			Name: "remote-usw2",
			ProfileYAML: `profiles:
  remote-usw2:
    description: Apple account: remote@example.com
    user: ec2-user
    aws:
      account_email: remote@example.com
      region: us-west-2
`,
		}}}})
	}))
	defer remote.Close()
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, `server:
  user_api: `+remote.URL+`
  token: cm_api_remote
defaults:
  identity_file: ~/.ssh/local.pem
`)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"list", "--config", config}); code != 0 {
		t.Fatalf("list code = %d, err = %s", code, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "remote-usw2") || !strings.Contains(text, "Apple account: remote@example.com") {
		t.Fatalf("list output = %q", text)
	}
}

func TestAppCompletionProfilesFetchesRemoteProfiles(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/managed-profiles" || r.URL.Query().Get("include_yaml") != "1" {
			t.Fatalf("unexpected request path = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer cm_api_remote" {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profiles": []webManagedProfile{{
			Name: "remote-usw2",
			ProfileYAML: `profiles:
  remote-usw2:
    description: Apple account: remote@example.com
    user: ec2-user
    aws:
      account_email: remote@example.com
      region: us-west-2
`,
		}}}})
	}))
	defer remote.Close()
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, `server:
  user_api: `+remote.URL+`
  token: cm_api_remote
defaults:
  identity_file: ~/.ssh/local.pem
profiles:
`)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"completion", "profiles", "--config", config}); code != 0 {
		t.Fatalf("completion profiles code = %d, err = %s", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "remote-usw2" {
		t.Fatalf("completion profiles = %q", got)
	}
}

func TestAppCheckUsesRemoteProfilesWhenLocalProfilesAreEmpty(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/managed-profiles" || r.URL.Query().Get("include_yaml") != "1" {
			t.Fatalf("unexpected request path = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer cm_api_remote" {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profiles": []webManagedProfile{{
			Name: "remote-usw2",
			ProfileYAML: `profiles:
  remote-usw2:
    description: Apple account: remote@example.com
    user: ec2-user
    host: ec2-1-2-3-4.us-west-2.compute.amazonaws.com
    aws:
      account_email: remote@example.com
      region: us-west-2
    tunnels:
      - local_port: 5900
        remote_host: localhost
        remote_port: 5900
`,
		}}}})
	}))
	defer remote.Close()
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, `server:
  user_api: `+remote.URL+`
  token: cm_api_remote
defaults:
  identity_file: `+key+`
profiles:
`)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"check", "remote-usw2", "--config", config}); code != 0 {
		t.Fatalf("check code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Profile: remote-usw2") || !strings.Contains(out.String(), "check passed") {
		t.Fatalf("check output = %q", out.String())
	}
}

func TestAppCheckUsesRemoteProfilesWhenLocalPrivateProfilesExist(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/managed-profiles" || r.URL.Query().Get("include_yaml") != "1" {
			t.Fatalf("unexpected request path = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer cm_api_remote" {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profiles": []webManagedProfile{{
			Name: "remote-usw2",
			ProfileYAML: `profiles:
  remote-usw2:
    description: Apple account: remote@example.com
    user: ec2-user
    host: ec2-1-2-3-4.us-west-2.compute.amazonaws.com
    aws:
      account_email: remote@example.com
      region: us-west-2
    tunnels:
      - local_port: 5900
        remote_host: localhost
        remote_port: 5900
`,
		}}}})
	}))
	defer remote.Close()
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, `server:
  user_api: `+remote.URL+`
  token: cm_api_remote
defaults:
  identity_file: `+key+`
profiles:
  local-private-only:
    identity_file: ~/.ssh/local-private.pem
`)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"check", "remote-usw2", "--config", config}); code != 0 {
		t.Fatalf("check code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Profile: remote-usw2") || !strings.Contains(out.String(), "check passed") {
		t.Fatalf("check output = %q", out.String())
	}
}

func TestAppCheckMergesLocalProfileIdentityFileIntoRemoteProfile(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/managed-profiles" || r.URL.Query().Get("include_yaml") != "1" {
			t.Fatalf("unexpected request path = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer cm_api_remote" {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profiles": []webManagedProfile{{
			Name: "remote-usw2",
			ProfileYAML: `profiles:
  remote-usw2:
    description: Apple account: remote@example.com
    user: ec2-user
    host: ec2-1-2-3-4.us-west-2.compute.amazonaws.com
    aws:
      account_email: remote@example.com
      region: us-west-2
    tunnels:
      - local_port: 5900
        remote_host: localhost
        remote_port: 5900
`,
		}}}})
	}))
	defer remote.Close()
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, `server:
  user_api: `+remote.URL+`
  token: cm_api_remote
profiles:
  remote-usw2:
    identity_file: `+key+`
`)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"check", "remote-usw2", "--config", config}); code != 0 {
		t.Fatalf("check code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Identity: "+key) {
		t.Fatalf("check output = %q", out.String())
	}
}

func TestWriteLocalAgentProfileConfigUsesLocalDefaultIdentityFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".connectmac")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(configDir, "config.yaml"), `defaults:
  identity_file: ~/.ssh/local-default.pem
profiles:
`)
	profileName, configPath, err := writeLocalAgentProfileConfig(localAgentRequest{ProfileYAML: `profiles:
  remote-usw2:
    description: Apple account: remote@example.com
    user: ec2-user
    host: ec2-1-2-3-4.us-west-2.compute.amazonaws.com
    tunnels:
      - local_port: 5900
        remote_host: localhost
        remote_port: 5900
`})
	if err != nil {
		t.Fatalf("write local agent profile: %v", err)
	}
	if profileName != "remote-usw2" {
		t.Fatalf("profile name = %q", profileName)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	profile, ok := cfg.Profile("remote-usw2")
	if !ok {
		t.Fatal("generated profile missing")
	}
	if profile.IdentityFile != "~/.ssh/local-default.pem" {
		t.Fatalf("identity_file = %q", profile.IdentityFile)
	}
}

func TestWriteLocalAgentProfileConfigUsesLocalProfileIdentityFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".connectmac")
	profilesDir := filepath.Join(configDir, "profiles")
	if err := os.MkdirAll(profilesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(configDir, "config.yaml"), `defaults:
  identity_file: ~/.ssh/local-default.pem
profiles:
`)
	writeFile(t, filepath.Join(profilesDir, "remote-usw2.yaml"), `profiles:
  remote-usw2:
    identity_file: ~/.ssh/special-profile.pem
`)
	_, configPath, err := writeLocalAgentProfileConfig(localAgentRequest{ProfileYAML: `profiles:
  remote-usw2:
    description: Apple account: remote@example.com
    user: ec2-user
    host: ec2-1-2-3-4.us-west-2.compute.amazonaws.com
    tunnels:
      - local_port: 5900
        remote_host: localhost
        remote_port: 5900
`})
	if err != nil {
		t.Fatalf("write local agent profile: %v", err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	profile, ok := cfg.Profile("remote-usw2")
	if !ok {
		t.Fatal("generated profile missing")
	}
	if profile.IdentityFile != "~/.ssh/special-profile.pem" {
		t.Fatalf("identity_file = %q", profile.IdentityFile)
	}
}

func TestLocalAgentTerminalCheckMergesIdentityAndRequiresCurrentHostKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := filepath.Join(home, ".ssh", "key.pem")
	writeFile(t, key, "secret")
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(home, ".connectmac")
	writeFile(t, filepath.Join(configDir, "config.yaml"), `defaults:
  identity_file: `+key+`
profiles:
`)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, home)
	app.Runner = runner
	body := strings.NewReader(`{"profile":"remote-usw2","profile_yaml":"profiles:\n  remote-usw2:\n    description: Apple account: remote@example.com\n    user: ec2-user\n    host: mac-host.example.com\n    tunnels:\n      - local_port: 5900\n        remote_host: localhost\n        remote_port: 5900\n"}`)
	req := httptest.NewRequest(http.MethodPost, "/terminal/check", body)
	req.Header.Set("Origin", "https://cm.hsgitlab.xyz")
	rec := httptest.NewRecorder()
	app.newLocalAgentHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp webAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("response error = %s", resp.Error)
	}
	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("response data = %#v", resp.Data)
	}
	if data["target"] != "ec2-user@mac-host.example.com" {
		t.Fatalf("target = %#v", data["target"])
	}
	if data["host_key_status"] != string(HostKeyCurrent) {
		t.Fatalf("host_key_status = %#v", data["host_key_status"])
	}
	if strings.TrimSpace(fmt.Sprint(data["terminal_session_token"])) == "" {
		t.Fatalf("terminal_session_token = %#v", data["terminal_session_token"])
	}
	token := strings.TrimSpace(fmt.Sprint(data["terminal_session_token"]))
	grant, err := app.TerminalSessions.Consume(token, "remote-usw2")
	if err != nil {
		t.Fatalf("consume terminal session token: %v", err)
	}
	if grant.Profile != "remote-usw2" || grant.RequestID == "" {
		t.Fatalf("terminal session grant = %+v", grant)
	}
	generated, err := LoadConfig(filepath.Join(home, ".connectmac", "local-agent", "profiles", "remote-usw2.yaml"))
	if err != nil {
		t.Fatalf("load generated profile: %v", err)
	}
	profile, ok := generated.Profile("remote-usw2")
	if !ok {
		t.Fatal("generated profile missing")
	}
	if profile.IdentityFile != key {
		t.Fatalf("identity_file = %q", profile.IdentityFile)
	}
}

func TestLocalAgentTerminalWSRequiresCurrentHostKeyBeforeUpgrade(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := filepath.Join(home, ".ssh", "key.pem")
	writeFile(t, key, "secret")
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	profileDir := filepath.Join(home, ".connectmac", "local-agent", "profiles")
	writeFile(t, filepath.Join(profileDir, "remote-usw2.yaml"), `profiles:
  remote-usw2:
    user: ec2-user
    identity_file: `+key+`
    host: mac-host.example.com
`)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, home)
	app.Runner = runner
	app.TerminalSessions = newTerminalSessionRegistry(16, time.Minute)
	token, err := app.TerminalSessions.Issue("remote-usw2", "terminal-ws-request")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/terminal/ws?profile=remote-usw2&terminal_session_token="+url.QueryEscape(token), nil)
	req.Header.Set("Origin", "https://cm.hsgitlab.xyz")
	rec := httptest.NewRecorder()
	app.newLocalAgentHandler().ServeHTTP(rec, req)
	if _, err := app.TerminalSessions.Reserve(token, "remote-usw2"); err != nil {
		t.Fatalf("failed websocket upgrade must release token for retry: %v", err)
	}
	if err := app.TerminalSessions.Release(token); err != nil {
		t.Fatal(err)
	}
}

func TestLocalAgentHostKeyConfirmationRepairsOnlyAfterExplicitMatchingConfirmation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := writeSSHKey(t, 0o600)
	scannedFirst, fingerprint := testScannedHostKey(t, "mac-host.example.com")
	scannedSecond, secondFingerprint := testScannedHostKey(t, "mac-host.example.com")
	scanned := scannedFirst + scannedSecond
	runner := &fakeRunner{
		knownHost:  "mac-host.example.com ssh-ed25519 AAAAOLD\n",
		scannedKey: scanned,
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.Runner = runner
	profile := validProfile(key)
	body := FormatProfileFile(profile)
	handler := app.newLocalAgentHandler()

	checkReq := localAgentTestRequest(http.MethodPost, "/host-key/check", strings.NewReader(`{"profile":"xcode-vnc","profile_yaml":`+strconv.Quote(body)+`}`))
	checkRec := httptest.NewRecorder()
	handler.ServeHTTP(checkRec, checkReq)
	if !strings.Contains(checkRec.Body.String(), `"status":"stale"`) || !strings.Contains(checkRec.Body.String(), fingerprint) || !strings.Contains(checkRec.Body.String(), secondFingerprint) {
		t.Fatalf("check response=%s", checkRec.Body.String())
	}
	if runner.forgotHost != "" {
		t.Fatalf("check mutated known_hosts for %q", runner.forgotHost)
	}
	if files, err := app.LogManager.List(); err != nil || len(files) != 0 {
		t.Fatalf("fingerprint check should not log pending fingerprint: files=%v err=%v", files, err)
	}
	var checked struct {
		Data struct {
			Challenge string `json:"challenge"`
		} `json:"data"`
	}
	if err := json.Unmarshal(checkRec.Body.Bytes(), &checked); err != nil || checked.Data.Challenge == "" {
		t.Fatalf("decode check challenge: %+v err=%v", checked, err)
	}

	wrongReq := localAgentTestRequest(http.MethodPost, "/host-key/fix", strings.NewReader(`{"profile":"xcode-vnc","profile_yaml":`+strconv.Quote(body)+`,"confirm":true,"host_key_fingerprints":["SHA256:wrong"],"host_key_challenge":`+strconv.Quote(checked.Data.Challenge)+`}`))
	wrongRec := httptest.NewRecorder()
	handler.ServeHTTP(wrongRec, wrongReq)
	if !strings.Contains(wrongRec.Body.String(), "does not match") || runner.forgotHost != "" {
		t.Fatalf("mismatched confirmation response=%s forgot=%q", wrongRec.Body.String(), runner.forgotHost)
	}

	checkRec = httptest.NewRecorder()
	handler.ServeHTTP(checkRec, localAgentTestRequest(http.MethodPost, "/host-key/check", strings.NewReader(`{"profile":"xcode-vnc","profile_yaml":`+strconv.Quote(body)+`}`)))
	if err := json.Unmarshal(checkRec.Body.Bytes(), &checked); err != nil || checked.Data.Challenge == "" {
		t.Fatalf("refresh challenge: %v", err)
	}
	fixReq := localAgentTestRequest(http.MethodPost, "/host-key/fix", strings.NewReader(`{"profile":"xcode-vnc","profile_yaml":`+strconv.Quote(body)+`,"confirm":true,"host_key_fingerprints":[`+strconv.Quote(secondFingerprint)+`,`+strconv.Quote(fingerprint)+`],"host_key_challenge":`+strconv.Quote(checked.Data.Challenge)+`}`))
	fixRec := httptest.NewRecorder()
	handler.ServeHTTP(fixRec, fixReq)
	if !strings.Contains(fixRec.Body.String(), `"ok":true`) {
		t.Fatalf("fix response=%s forgot=%q", fixRec.Body.String(), runner.forgotHost)
	}
	replayBody := `{"profile":"xcode-vnc","profile_yaml":` + strconv.Quote(body) + `,"confirm":true,"host_key_fingerprints":[` + strconv.Quote(secondFingerprint) + `,` + strconv.Quote(fingerprint) + `],"host_key_challenge":` + strconv.Quote(checked.Data.Challenge) + `}`
	replayRec := httptest.NewRecorder()
	handler.ServeHTTP(replayRec, localAgentTestRequest(http.MethodPost, "/host-key/fix", strings.NewReader(replayBody)))
	if !strings.Contains(replayRec.Body.String(), "already used") {
		t.Fatalf("challenge replay response=%s", replayRec.Body.String())
	}
	knownHosts, err := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts"))
	if err != nil || !strings.Contains(string(knownHosts), strings.Fields(scannedFirst)[2]) || !strings.Contains(string(knownHosts), strings.Fields(scannedSecond)[2]) {
		t.Fatalf("known_hosts=%q err=%v", knownHosts, err)
	}
}

func TestLocalAgentHostKeyConfirmationRejectsChangedKeyBetweenCheckAndFix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := writeSSHKey(t, 0o600)
	scannedA, fingerprintA := testScannedHostKey(t, "mac-host.example.com")
	scannedB, fingerprintB := testScannedHostKey(t, "mac-host.example.com")
	if fingerprintA == fingerprintB {
		t.Fatal("test keys unexpectedly have the same fingerprint")
	}
	runner := &fakeRunner{
		knownHost: "mac-host.example.com ssh-ed25519 AAAAOLD\n",
		scanKeys:  []string{scannedA, scannedB},
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.Runner = runner
	profileYAML := FormatProfileFile(validProfile(key))
	handler := app.newLocalAgentHandler()

	checkReq := localAgentTestRequest(http.MethodPost, "/host-key/check", strings.NewReader(`{"profile":"xcode-vnc","profile_yaml":`+strconv.Quote(profileYAML)+`}`))
	checkRec := httptest.NewRecorder()
	handler.ServeHTTP(checkRec, checkReq)
	if !strings.Contains(checkRec.Body.String(), fingerprintA) || strings.Contains(checkRec.Body.String(), fingerprintB) {
		t.Fatalf("check response=%s", checkRec.Body.String())
	}
	var checked struct {
		Data struct {
			Challenge string `json:"challenge"`
		} `json:"data"`
	}
	if err := json.Unmarshal(checkRec.Body.Bytes(), &checked); err != nil || checked.Data.Challenge == "" {
		t.Fatalf("decode challenge: %v", err)
	}

	fixReq := localAgentTestRequest(http.MethodPost, "/host-key/fix", strings.NewReader(`{"profile":"xcode-vnc","profile_yaml":`+strconv.Quote(profileYAML)+`,"confirm":true,"host_key_fingerprints":[`+strconv.Quote(fingerprintA)+`],"host_key_challenge":`+strconv.Quote(checked.Data.Challenge)+`}`))
	fixRec := httptest.NewRecorder()
	handler.ServeHTTP(fixRec, fixReq)
	if !strings.Contains(fixRec.Body.String(), "fingerprint changed") {
		t.Fatalf("fix response=%s", fixRec.Body.String())
	}
	if runner.forgotHost != "" {
		t.Fatalf("changed key removed known_hosts entry for %q", runner.forgotHost)
	}
	knownHosts := filepath.Join(home, ".ssh", "known_hosts")
	if data, err := os.ReadFile(knownHosts); !os.IsNotExist(err) {
		t.Fatalf("changed key wrote known_hosts: data=%q err=%v", data, err)
	}
	if files, err := app.LogManager.List(); err != nil || len(files) != 0 {
		t.Fatalf("confirmation flow logged key or fingerprint: files=%v err=%v", files, err)
	}
}

func TestLocalAgentEntryPointsBlockNonCurrentHostKeys(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		extra     string
		configure func(*fakeRunner)
		wantCode  string
	}{
		{name: "ssh stale", path: "/ssh", configure: func(r *fakeRunner) {
			r.knownHost = "mac-host.example.com ssh-ed25519 AAAAOLD\n"
		}, wantCode: "host_key_changed"},
		{name: "terminal missing", path: "/terminal/check", configure: func(r *fakeRunner) {
			r.missingHostKey = true
		}, wantCode: "host_key_missing"},
		{name: "transfer stale", path: "/sync/push", extra: `,"local_path":"/does/not/matter","remote_path":"~/Documents/"`, configure: func(r *fakeRunner) {
			r.knownHost = "mac-host.example.com ssh-ed25519 AAAAOLD\n"
		}, wantCode: "host_key_changed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			key := writeSSHKey(t, 0o600)
			profile := validProfile(key)
			runner := &fakeRunner{}
			tc.configure(runner)
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, home)
			app.Runner = runner
			payload := `{"profile":"xcode-vnc","profile_yaml":` + strconv.Quote(FormatProfileFile(profile)) + tc.extra + `}`
			req := localAgentTestRequest(http.MethodPost, tc.path, strings.NewReader(payload))
			rec := httptest.NewRecorder()
			app.newLocalAgentHandler().ServeHTTP(rec, req)
			if !strings.Contains(rec.Body.String(), "host key") {
				t.Fatalf("response=%s", rec.Body.String())
			}
			if len(runner.foreground) != 0 || len(runner.background) != 0 || len(runner.rsync) != 0 || runner.openedVNC != "" || runner.forgotHost != "" {
				t.Fatalf("blocked local Agent request invoked command or mutation: runner=%+v", runner)
			}
			entries := readTestLogEntries(t, app.LogManager)
			if len(entries) != 1 || entries[0].Action != "host-key.blocked" || entries[0].ErrorCode != tc.wantCode || entries[0].RequestID == "" {
				t.Fatalf("blocked entries=%+v", entries)
			}
		})
	}
}

func TestLocalAgentLaunchAgentPlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath, err := expandedAbsoluteConfigPath("~/Connect Mac/config file.yaml")
	if err != nil {
		t.Fatal(err)
	}
	plist := localAgentLaunchAgentPlist(
		localAgentLaunchLabel,
		"/usr/local/bin/cm",
		configPath,
		localAgentOptions{Host: "127.0.0.1", Port: 18765},
		"/tmp/connectmac.out.log",
		"/tmp/connectmac.err.log",
	)
	args, err := decodePlistStringArray(xml.NewDecoder(strings.NewReader(plist)))
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"/usr/local/bin/cm",
		"--config", configPath,
		"local-agent",
		"--host", "127.0.0.1",
		"--port", "18765",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("ProgramArguments = %#v, want %#v", args, wantArgs)
	}
	for _, want := range []string{
		"<string>com.connectmac.local-agent</string>",
		"<string>/usr/local/bin/cm</string>",
		"<string>local-agent</string>",
		"<string>--config</string>",
		"<string>" + configPath + "</string>",
		"<string>--host</string>",
		"<string>127.0.0.1</string>",
		"<string>--port</string>",
		"<string>18765</string>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
		"<key>StandardOutPath</key>",
		"<string>/tmp/connectmac.out.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestLocalAgentEndpoint(t *testing.T) {
	for _, tt := range []struct {
		host string
		want string
	}{
		{host: "localhost", want: "localhost:29876"},
		{host: "127.0.0.1", want: "127.0.0.1:29876"},
		{host: "::1", want: "[::1]:29876"},
	} {
		t.Run(tt.host, func(t *testing.T) {
			opts := localAgentOptions{Host: tt.host, Port: 29876}
			if got := localAgentEndpoint(opts, true, "/health"); got != "https://"+tt.want+"/health" {
				t.Fatalf("HTTPS endpoint = %q", got)
			}
			if got := localAgentEndpoint(opts, false, "activity"); got != "http://"+tt.want+"/activity" {
				t.Fatalf("HTTP endpoint = %q", got)
			}
		})
	}
}

func TestLocalAgentHTTPClientsHaveDeadlines(t *testing.T) {
	home := t.TempDir()
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []*http.Client{localAgentHTTPClient(), localAgentHTTPSClient(material)} {
		if client.Timeout != localAgentClientTimeout {
			t.Fatalf("client timeout = %s", client.Timeout)
		}
		transport, ok := client.Transport.(*http.Transport)
		if !ok || transport.ResponseHeaderTimeout != localAgentProbeTimeout {
			t.Fatalf("client transport = %#v", client.Transport)
		}
	}
}

func TestLocalAgentHTTPSClientVerifiesGeneratedCA(t *testing.T) {
	home := t.TempDir()
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	listener, stop := startLocalAgentTLSServer(t, "127.0.0.1:0", material, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true})
	}))
	defer stop()

	opts := localAgentOptionsForAddr(t, listener.Addr().String())
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, localAgentEndpoint(opts, true, "/health"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := localAgentHTTPSClient(material).Do(req)
	if err != nil {
		t.Fatalf("verified HTTPS request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}
}

func TestLocalAgentStatusAndDrainUseInstalledTLS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	listener, stop := startLocalAgentTLSServer(t, "127.0.0.1:0", material, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			writeWebJSON(w, webAPIResponse{OK: true})
		case "/activity/drain":
			writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"draining": true, "active": []LocalTransferJob{}}})
		case "/activity/resume":
			writeWebJSON(w, webAPIResponse{OK: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer stop()

	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	opts := localAgentOptionsForAddr(t, listener.Addr().String())
	if code := app.statusLocalAgent(context.Background(), opts); code != 0 {
		t.Fatalf("status code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "https://") {
		t.Fatalf("status did not report HTTPS: %q", out.String())
	}
	if drained, allowed := app.prepareLocalAgentShutdown(context.Background(), opts); !drained || !allowed {
		t.Fatalf("prepare TLS shutdown = (%v, %v), err = %q", drained, allowed, errOut.String())
	}
	app.resumeLocalAgentActivity(opts)
}

func TestLocalAgentServiceFallsBackToLegacyHTTPDuringMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, _, err := ensureLocalAgentTLS(home, time.Now()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true})
	}))
	defer server.Close()

	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	if code := app.statusLocalAgent(context.Background(), localAgentOptionsForURL(t, server.URL)); code != 0 {
		t.Fatalf("status code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), server.URL) {
		t.Fatalf("status did not fall back to HTTP: %q", out.String())
	}
}

func TestLocalAgentManualStartupUsesTLSOrLegacyHTTP(t *testing.T) {
	for _, tt := range []struct {
		name       string
		prepare    func(t *testing.T, home string) localAgentTLSMaterial
		wantScheme string
		wantCode   int
	}{
		{
			name:       "legacy HTTP without TLS directory",
			wantScheme: "http://",
		},
		{
			name: "HTTPS with valid TLS material",
			prepare: func(t *testing.T, home string) localAgentTLSMaterial {
				t.Helper()
				material, _, err := ensureLocalAgentTLS(home, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				return material
			},
			wantScheme: "https://",
		},
		{
			name: "invalid TLS material is refused",
			prepare: func(t *testing.T, home string) localAgentTLSMaterial {
				t.Helper()
				material := localAgentTLSPaths(home)
				writeFile(t, material.CACertPath, "not a certificate")
				return material
			},
			wantCode: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			material := localAgentTLSMaterial{}
			if tt.prepare != nil {
				material = tt.prepare(t, home)
			}
			opts := localAgentUnusedOptions(t)
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, home)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan int, 1)
			go func() {
				result <- app.runLocalAgent(ctx, DefaultConfigPath, []string{"--host", opts.Host, "--port", strconv.Itoa(opts.Port)})
			}()
			if tt.wantCode != 0 {
				if code := <-result; code != tt.wantCode {
					t.Fatalf("code = %d, err = %q", code, errOut.String())
				}
				if !strings.Contains(errOut.String(), "cm local-agent install") {
					t.Fatalf("invalid TLS error = %q", errOut.String())
				}
				return
			}
			endpoint := localAgentEndpoint(opts, tt.wantScheme == "https://", "/health")
			waitForLocalAgentEndpoint(t, endpoint, material)
			if !strings.Contains(out.String(), tt.wantScheme) {
				t.Fatalf("startup output = %q", out.String())
			}
			cancel()
			if code := <-result; code != 0 {
				t.Fatalf("code = %d, err = %q", code, errOut.String())
			}
		})
	}
}

func TestLocalAgentInstallTLSLifecycleAndHealthFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "Connect Mac", "custom config.yaml")
	opts := localAgentUnusedOptions(t)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	var serverStop func()
	defer func() {
		if serverStop != nil {
			serverStop()
		}
	}()
	var launchCalls [][]string
	app.LocalAgentServiceCommand = func(_ context.Context, args ...string) ([]byte, error) {
		launchCalls = append(launchCalls, append([]string(nil), args...))
		switch args[0] {
		case "bootout":
			if serverStop != nil {
				serverStop()
				serverStop = nil
			}
		case "kickstart":
			material := localAgentTLSPaths(home)
			listener, stop := startLocalAgentTLSServer(t, net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)), material, app.newLocalAgentHandler())
			if listener.Addr().String() != net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)) {
				stop()
				t.Fatalf("listener = %s, want %s", listener.Addr(), net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)))
			}
			serverStop = stop
		}
		return nil, nil
	}
	securityCalls := 0
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		securityCalls++
		if args[0] == "find-certificate" && securityCalls > 1 {
			fingerprint, err := localAgentCAFingerprint(localAgentTLSPaths(home).CACertPath)
			if err != nil {
				t.Fatal(err)
			}
			return []byte("SHA-1 hash: " + fingerprint), nil
		}
		return nil, nil
	}

	if code := app.runLocalAgent(context.Background(), configPath, []string{"install", "--host", opts.Host, "--port", strconv.Itoa(opts.Port)}); code != 0 {
		t.Fatalf("first install code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "ready at https://") {
		t.Fatalf("install output = %q", out.String())
	}
	plistPath, err := ExpandPath(localAgentPlistPath)
	if err != nil {
		t.Fatal(err)
	}
	installedPlist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installedPlist), "<string>--config</string>\n    <string>"+configPath+"</string>") {
		t.Fatalf("installed plist does not preserve config path:\n%s", installedPlist)
	}
	if code := app.runLocalAgent(context.Background(), configPath, []string{"restart", "--force"}); code != 0 {
		t.Fatalf("restart code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	restartedPlist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restartedPlist, installedPlist) {
		t.Fatalf("restart changed installed plist:\n%s", restartedPlist)
	}
	if len(launchCalls) != 6 {
		t.Fatalf("launchctl calls = %#v", launchCalls)
	}
	if serverStop != nil {
		serverStop()
		serverStop = nil
	}

	failing := testApp(&out, &errOut, home)
	failing.LocalAgentServiceCommand = func(_ context.Context, _ ...string) ([]byte, error) { return nil, nil }
	failing.LocalAgentSecurityCommand = app.LocalAgentSecurityCommand
	out.Reset()
	errOut.Reset()
	if code := failing.installLocalAgentLaunchAgent(context.Background(), configPath, opts); code != 1 {
		t.Fatalf("health failure code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "https://") || !strings.Contains(errOut.String(), "local-agent.err.log") {
		t.Fatalf("health failure error = %q", errOut.String())
	}
}

func TestLocalAgentInstallKeychainCancellationKeepsExistingServiceRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := ExpandPath(localAgentPlistPath)
	if err != nil {
		t.Fatal(err)
	}
	original := localAgentLaunchAgentPlist(localAgentLaunchLabel, "/usr/local/bin/cm", DefaultConfigPath, localAgentOptions{Host: "127.0.0.9", Port: 29876}, "/tmp/out", "/tmp/err")
	writeFile(t, path, original)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.LocalAgentSecurityCommand = func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, context.Canceled
	}
	app.LocalAgentServiceCommand = func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("launchctl must not run after keychain cancellation: %#v", args)
		return nil, nil
	}

	if code := app.installLocalAgentLaunchAgent(context.Background(), DefaultConfigPath, localAgentOptions{Host: "127.0.0.1", Port: 18765}); code != 1 {
		t.Fatalf("install code = %d, err = %q", code, errOut.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("plist changed after cancellation:\n%s", got)
	}
}

func TestLocalAgentInstallLaunchctlFailuresAreAuthoritative(t *testing.T) {
	for _, tt := range []struct {
		name         string
		bootout      []byte
		bootErr      error
		bootstrap    []byte
		bootstrapErr error
		kickstart    []byte
		kickstartErr error
		want         string
	}{
		{
			name:         "known missing service permits bootstrap",
			bootout:      []byte(`Could not find service "com.connectmac.local-agent" in domain for user gui: 501`),
			bootErr:      errors.New("exit status 3"),
			bootstrap:    []byte("operation not permitted"),
			bootstrapErr: errors.New("exit status 1"),
			want:         "bootstrap launch agent",
		},
		{
			name:         "kickstart failure is not ignored",
			bootstrap:    []byte(""),
			kickstart:    []byte("Could not kickstart service: Operation not permitted"),
			kickstartErr: errors.New("exit status 1"),
			want:         "start launch agent",
		},
		{
			name:         "bootout io error with missing printed service permits bootstrap",
			bootout:      []byte("Boot-out failed: 5: Input/output error"),
			bootErr:      errors.New("exit status 5"),
			bootstrap:    []byte("operation not permitted"),
			bootstrapErr: errors.New("exit status 1"),
			want:         "bootstrap launch agent",
		},
		{
			name:    "bootout io error with authoritative print failure is not ignored",
			bootout: []byte("Boot-out failed: 5: Input/output error"),
			bootErr: errors.New("exit status 5"),
			want:    "stop launch agent",
		},
		{
			name:    "bootout permission failure is not ignored",
			bootout: []byte("Boot-out failed: Operation not permitted"),
			bootErr: errors.New("exit status 1"),
			want:    "stop launch agent",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, home)
			app.LocalAgentSecurityCommand = func(_ context.Context, _ ...string) ([]byte, error) { return nil, nil }
			var commands []string
			app.LocalAgentServiceCommand = func(_ context.Context, args ...string) ([]byte, error) {
				commands = append(commands, args[0])
				switch args[0] {
				case "bootout":
					return tt.bootout, tt.bootErr
				case "print":
					if tt.name == "bootout io error with missing printed service permits bootstrap" {
						return []byte(`Could not find service "com.connectmac.local-agent" in domain for user gui: 501`), errors.New("exit status 113")
					}
					return []byte("Could not find service configuration: Operation not permitted"), errors.New("exit status 1")
				case "bootstrap":
					return tt.bootstrap, tt.bootstrapErr
				case "kickstart":
					return tt.kickstart, tt.kickstartErr
				default:
					t.Fatalf("unexpected launchctl command: %#v", args)
					return nil, nil
				}
			}

			if code := app.installLocalAgentLaunchAgent(context.Background(), DefaultConfigPath, localAgentUnusedOptions(t)); code != 1 {
				t.Fatalf("install code = %d, out = %q, err = %q", code, out.String(), errOut.String())
			}
			if !strings.Contains(errOut.String(), tt.want) {
				t.Fatalf("install error = %q", errOut.String())
			}
			if tt.name == "bootout permission failure is not ignored" && len(commands) != 1 {
				t.Fatalf("launchctl commands = %#v", commands)
			}
			if tt.name == "bootout io error with authoritative print failure is not ignored" && len(commands) != 2 {
				t.Fatalf("launchctl commands = %#v", commands)
			}
		})
	}
}

func TestLocalAgentLaunchServiceNotLoadedClassification(t *testing.T) {
	for _, tt := range []struct {
		output string
		want   bool
	}{
		{output: `Could not find service "com.connectmac.local-agent" in domain for user gui: 501`, want: true},
		{output: "Boot-out failed: 3: No such process", want: true},
		{output: "Service is not loaded.", want: true},
		{output: "Could not find service configuration: Operation not permitted", want: false},
		{output: "Service is not loaded because access was denied", want: false},
		{output: "Boot-out failed: 5: Input/output error", want: false},
	} {
		t.Run(tt.output, func(t *testing.T) {
			if got := localAgentLaunchServiceNotLoaded([]byte(tt.output)); got != tt.want {
				t.Fatalf("classified %q as %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestLocalAgentInstallRejectsOldHealthyVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	opts := localAgentUnusedOptions(t)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.Version = "new-version"
	app.LocalAgentSecurityCommand = func(_ context.Context, _ ...string) ([]byte, error) { return nil, nil }
	var stop func()
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	app.LocalAgentServiceCommand = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "bootout", "bootstrap":
			return nil, nil
		case "kickstart":
			old := app
			old.Version = "old-version"
			_, stop = startLocalAgentTLSServer(t, net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)), localAgentTLSPaths(home), old.newLocalAgentHandler())
			return nil, nil
		default:
			t.Fatalf("unexpected launchctl command: %#v", args)
			return nil, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	if code := app.installLocalAgentLaunchAgent(ctx, DefaultConfigPath, opts); code != 1 {
		t.Fatalf("install code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "ready at") || !strings.Contains(errOut.String(), "health check failed") {
		t.Fatalf("install output = %q, error = %q", out.String(), errOut.String())
	}
}

func TestLocalAgentInstallHostMigrationAndRejection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := ExpandPath(localAgentPlistPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, localAgentLaunchAgentPlist(localAgentLaunchLabel, "/usr/local/bin/cm", DefaultConfigPath, localAgentOptions{Host: "127.0.0.9", Port: 29876}, "/tmp/out", "/tmp/err"))
	opts, err := resolveInstalledLocalAgentOptions(localAgentOptions{Host: "127.0.0.1", Port: 18765})
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	migrated, err := app.normalizeLocalAgentInstallOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Host != "127.0.0.1" || migrated.Port != 29876 || !strings.Contains(out.String(), "migrating local-agent host 127.0.0.9") {
		t.Fatalf("migration = %#v, output = %q", migrated, out.String())
	}

	for _, host := range []string{"127.0.0.9", "192.0.2.10"} {
		t.Run(host, func(t *testing.T) {
			var rejectedOut, rejectedErr bytes.Buffer
			rejected := testApp(&rejectedOut, &rejectedErr, home)
			tlsDir := localAgentTLSPaths(home).Dir
			plistPath, err := ExpandPath(localAgentPlistPath)
			if err != nil {
				t.Fatal(err)
			}
			beforePlist, err := os.ReadFile(plistPath)
			if err != nil {
				t.Fatal(err)
			}
			rejected.LocalAgentSecurityCommand = func(_ context.Context, _ ...string) ([]byte, error) {
				t.Fatal("security must not run for rejected host")
				return nil, nil
			}
			rejected.LocalAgentServiceCommand = func(_ context.Context, _ ...string) ([]byte, error) {
				t.Fatal("launchctl must not run for rejected host")
				return nil, nil
			}
			if code := rejected.installLocalAgentLaunchAgent(context.Background(), DefaultConfigPath, localAgentOptions{Host: host, Port: 18765, HostExplicit: true}); code != 2 {
				t.Fatalf("install code = %d, error = %q", code, rejectedErr.String())
			}
			if !strings.Contains(rejectedErr.String(), "localhost, 127.0.0.1, or ::1") {
				t.Fatalf("rejection error = %q", rejectedErr.String())
			}
			if _, err := os.Stat(tlsDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("TLS directory changed for rejected host: %v", err)
			}
			afterPlist, err := os.ReadFile(plistPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterPlist, beforePlist) {
				t.Fatal("plist changed for rejected host")
			}
		})
	}
}

func TestLocalAgentProbeTimeoutForHangingServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	started := time.Now()
	if code := app.statusLocalAgent(context.Background(), localAgentOptionsForURL(t, server.URL)); code != 1 {
		t.Fatalf("status code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	if elapsed := time.Since(started); elapsed > 2*localAgentProbeTimeout {
		t.Fatalf("hanging probe took %s", elapsed)
	}
}

func TestLocalAgentKeychainTrustInstallCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	opts := localAgentUnusedOptions(t)
	stop := configureLocalAgentInstallService(t, &app, home, opts)
	defer stop()
	var calls [][]string
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}

	if code := app.installLocalAgentLaunchAgent(context.Background(), DefaultConfigPath, opts); code != 0 {
		t.Fatalf("install code = %d, err = %q", code, errOut.String())
	}
	material := localAgentTLSPaths(home)
	want := [][]string{
		{"find-certificate", "-a", "-Z", filepath.Join(home, "Library", "Keychains", "login.keychain-db")},
		{"add-trusted-cert", "-r", "trustRoot", "-p", "ssl", "-k", filepath.Join(home, "Library", "Keychains", "login.keychain-db"), material.CACertPath},
		{"verify-cert", "-c", material.ServerCertPath, "-p", "ssl", "-n", "127.0.0.1", "-k", filepath.Join(home, "Library", "Keychains", "login.keychain-db"), "-L"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("security commands = %#v, want %#v", calls, want)
	}
}

func TestLocalAgentKeychainTrustSkipsAlreadyTrustedCA(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sha1Fingerprint, sha256Fingerprint, err := localAgentCAFingerprints(material.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		output string
	}{
		{name: "SHA-1", output: "SHA-1 hash: " + strings.ToUpper(sha1Fingerprint[:2]) + ":" + strings.ToUpper(sha1Fingerprint[2:])},
		{name: "SHA-256", output: "SHA-256 hash: " + strings.ToUpper(sha256Fingerprint[:2]) + ":" + strings.ToUpper(sha256Fingerprint[2:])},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, home)
			opts := localAgentUnusedOptions(t)
			stop := configureLocalAgentInstallService(t, &app, home, opts)
			defer stop()
			var calls [][]string
			app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				return []byte(tt.output + "\n"), nil
			}

			if code := app.installLocalAgentLaunchAgent(context.Background(), DefaultConfigPath, opts); code != 0 {
				t.Fatalf("install code = %d, err = %q", code, errOut.String())
			}
			want := [][]string{
				{"find-certificate", "-a", "-Z", filepath.Join(home, "Library", "Keychains", "login.keychain-db")},
				{"verify-cert", "-c", material.ServerCertPath, "-p", "ssl", "-n", "127.0.0.1", "-k", filepath.Join(home, "Library", "Keychains", "login.keychain-db"), "-L"},
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("security commands = %#v", calls)
			}
		})
	}
}

func TestLocalAgentKeychainTrustVerifiesPresentCertificateAndRepairsTrust(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := localAgentCAFingerprint(material.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	var calls [][]string
	verifyCalls := 0
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "find-certificate":
			return []byte("SHA-1 hash: " + fingerprint), nil
		case "verify-cert":
			verifyCalls++
			if verifyCalls == 1 {
				return []byte("CSSMERR_TP_NOT_TRUSTED"), errors.New("exit status 1")
			}
			return nil, nil
		case "add-trusted-cert":
			return nil, nil
		default:
			t.Fatalf("unexpected security command: %#v", args)
			return nil, nil
		}
	}

	if err := app.ensureLocalAgentCATrust(context.Background(), home, material); err != nil {
		t.Fatal(err)
	}
	keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	want := [][]string{
		{"find-certificate", "-a", "-Z", keychain},
		{"verify-cert", "-c", material.ServerCertPath, "-p", "ssl", "-n", "127.0.0.1", "-k", keychain, "-L"},
		{"add-trusted-cert", "-r", "trustRoot", "-p", "ssl", "-k", keychain, material.CACertPath},
		{"verify-cert", "-c", material.ServerCertPath, "-p", "ssl", "-n", "127.0.0.1", "-k", keychain, "-L"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("security commands = %#v, want %#v", calls, want)
	}
	ledger, err := os.ReadFile(localAgentTrustedCAFingerprintLedgerPath(home))
	if err != nil || strings.TrimSpace(string(ledger)) != fingerprint {
		t.Fatalf("ledger = %q, err = %v", ledger, err)
	}
	info, err := os.Stat(localAgentTrustedCAFingerprintLedgerPath(home))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %v, err = %v", info.Mode(), err)
	}
}

func TestLocalAgentKeychainTrustFailsWhenVerificationStillFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := localAgentCAFingerprint(material.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "find-certificate":
			return []byte("SHA-1 hash: " + fingerprint), nil
		case "verify-cert":
			return []byte("CSSMERR_TP_NOT_TRUSTED"), errors.New("exit status 1")
		case "add-trusted-cert":
			return nil, nil
		default:
			t.Fatalf("unexpected security command: %#v", args)
			return nil, nil
		}
	}

	if code := app.installLocalAgentLaunchAgent(context.Background(), DefaultConfigPath, localAgentOptions{Host: "127.0.0.1", Port: 18765}); code != 1 {
		t.Fatalf("install code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	ledger, err := os.ReadFile(localAgentTrustedCAFingerprintLedgerPath(home))
	if err != nil || strings.TrimSpace(string(ledger)) != fingerprint {
		t.Fatalf("ledger after failed verification = %q, err = %v", ledger, err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "com.connectmac.local-agent.plist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist exists after failed verification: %v", err)
	}
}

func TestLocalAgentKeychainTrustRotatesAndRemovesStaleLedgerFingerprint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldMaterial, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	oldFingerprint, err := localAgentCAFingerprint(oldMaterial.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, localAgentTrustedCAFingerprintLedgerPath(home), oldFingerprint+"\n")
	if err := os.Remove(oldMaterial.CAKeyPath); err != nil {
		t.Fatal(err)
	}
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	newFingerprint, err := localAgentCAFingerprint(material.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	if newFingerprint == oldFingerprint {
		t.Fatal("CA was not rotated")
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	var calls [][]string
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "find-certificate":
			return nil, nil
		case "add-trusted-cert", "verify-cert":
			return nil, nil
		case "delete-certificate":
			if args[2] != oldFingerprint {
				t.Fatalf("delete fingerprint = %q, want %q", args[2], oldFingerprint)
			}
			return nil, nil
		default:
			t.Fatalf("unexpected security command: %#v", args)
			return nil, nil
		}
	}

	if err := app.ensureLocalAgentCATrust(context.Background(), home, material); err != nil {
		t.Fatal(err)
	}
	verifyIndex, deleteIndex := -1, -1
	for i, call := range calls {
		if call[0] == "verify-cert" {
			verifyIndex = i
		}
		if call[0] == "delete-certificate" {
			deleteIndex = i
		}
	}
	if verifyIndex < 0 || deleteIndex <= verifyIndex {
		t.Fatalf("commands must verify before stale deletion: %#v", calls)
	}
	ledger, err := os.ReadFile(localAgentTrustedCAFingerprintLedgerPath(home))
	if err != nil || strings.TrimSpace(string(ledger)) != newFingerprint {
		t.Fatalf("ledger = %q, err = %v", ledger, err)
	}
}

func TestLocalAgentKeychainTrustKeepsLedgerWhenStaleCleanupFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	oldFingerprint := strings.Repeat("a", 40)
	writeFile(t, localAgentTrustedCAFingerprintLedgerPath(home), oldFingerprint+"\n")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	opts := localAgentUnusedOptions(t)
	stop := configureLocalAgentInstallService(t, &app, home, opts)
	defer stop()
	attempt := 0
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "find-certificate", "verify-cert", "add-trusted-cert":
			return nil, nil
		case "delete-certificate":
			attempt++
			if attempt == 1 {
				return []byte("access denied"), errors.New("exit status 1")
			}
			return nil, nil
		default:
			t.Fatalf("unexpected security command: %#v", args)
			return nil, nil
		}
	}

	if code := app.installLocalAgentLaunchAgent(context.Background(), DefaultConfigPath, opts); code != 1 {
		t.Fatalf("install code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	ledger, err := readLocalAgentTrustedCAFingerprints(home)
	currentFingerprint, err := localAgentCAFingerprint(material.CACertPath)
	if err != nil || len(ledger) != 2 || (ledger[0] != oldFingerprint && ledger[1] != oldFingerprint) ||
		(ledger[0] != currentFingerprint && ledger[1] != currentFingerprint) {
		t.Fatalf("ledger after failed cleanup = %#v, err = %v", ledger, err)
	}
	if code := app.installLocalAgentLaunchAgent(context.Background(), DefaultConfigPath, opts); code != 0 {
		t.Fatalf("retry install code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	currentFingerprint, err = localAgentCAFingerprint(material.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err = readLocalAgentTrustedCAFingerprints(home)
	if err != nil || !reflect.DeepEqual(ledger, []string{currentFingerprint}) {
		t.Fatalf("ledger after retry = %#v, err = %v", ledger, err)
	}
}

func TestLocalAgentKeychainTrustWritesAheadBeforeCanceledMutationAndRecoversWithoutTLS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := localAgentCAFingerprint(material.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	ledgerRecordedBeforeMutation := false
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "find-certificate":
			return nil, nil
		case "add-trusted-cert":
			ledger, readErr := os.ReadFile(localAgentTrustedCAFingerprintLedgerPath(home))
			ledgerRecordedBeforeMutation = readErr == nil && strings.TrimSpace(string(ledger)) == fingerprint
			cancel()
			return []byte("signal: killed"), errors.New("signal: killed")
		default:
			t.Fatalf("unexpected security command: %#v", args)
			return nil, nil
		}
	}

	if code := app.installLocalAgentLaunchAgent(ctx, DefaultConfigPath, localAgentOptions{Host: "127.0.0.1", Port: 18765}); code != 1 {
		t.Fatalf("install code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	if !ledgerRecordedBeforeMutation {
		t.Fatal("current fingerprint was not recorded before trust mutation")
	}
	if err := os.RemoveAll(material.Dir); err != nil {
		t.Fatal(err)
	}
	installTestLaunchctl(t)
	var deleted []string
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] != "delete-certificate" {
			t.Fatalf("unexpected security command: %#v", args)
		}
		deleted = append(deleted, args[2])
		return nil, nil
	}
	if code := app.uninstallLocalAgentLaunchAgent(context.Background(), localAgentOptions{Force: true}); code != 0 {
		t.Fatalf("uninstall code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	if !reflect.DeepEqual(deleted, []string{fingerprint}) {
		t.Fatalf("deleted fingerprints = %#v", deleted)
	}
}

func TestLocalAgentKeychainTrustInstallFailureAndCancellation(t *testing.T) {
	tests := []struct {
		name    string
		command func([]string) ([]byte, error)
		want    string
	}{
		{
			name: "failure includes safe command output",
			command: func(args []string) ([]byte, error) {
				if args[0] == "add-trusted-cert" {
					return []byte("User interaction is not allowed\n-----BEGIN PRIVATE KEY-----\nnot-a-real-key"), errors.New("exit status 1")
				}
				return nil, nil
			},
			want: "User interaction is not allowed",
		},
		{
			name: "cancellation does not report install success",
			command: func(args []string) ([]byte, error) {
				if args[0] == "add-trusted-cert" {
					return nil, context.Canceled
				}
				return nil, nil
			},
			want: "canceled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, home)
			app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
				return tt.command(args)
			}

			if code := app.installLocalAgentLaunchAgent(context.Background(), DefaultConfigPath, localAgentOptions{Host: "127.0.0.1", Port: 18765}); code != 1 {
				t.Fatalf("install code = %d, out = %q, err = %q", code, out.String(), errOut.String())
			}
			if !strings.Contains(strings.ToLower(errOut.String()), strings.ToLower(tt.want)) || strings.Contains(out.String(), "installed") {
				t.Fatalf("out = %q, err = %q", out.String(), errOut.String())
			}
			if strings.Contains(errOut.String(), "not-a-real-key") {
				t.Fatalf("error leaked key material: %q", errOut.String())
			}
			if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "com.connectmac.local-agent.plist")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("plist exists after failed trust: %v", err)
			}
			fingerprint, err := localAgentCAFingerprint(localAgentTLSPaths(home).CACertPath)
			if err != nil {
				t.Fatal(err)
			}
			ledger, err := readLocalAgentTrustedCAFingerprints(home)
			if err != nil || !reflect.DeepEqual(ledger, []string{fingerprint}) {
				t.Fatalf("ledger after failed trust = %#v, err = %v", ledger, err)
			}
		})
	}
}

func TestLocalAgentUninstallTLSDeletesExactTrustedCA(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := localAgentCAFingerprint(material.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	installTestLaunchctl(t)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	var calls [][]string
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}

	if code := app.uninstallLocalAgentLaunchAgent(context.Background(), localAgentOptions{Force: true}); code != 0 {
		t.Fatalf("uninstall code = %d, err = %q", code, errOut.String())
	}
	want := [][]string{{"delete-certificate", "-Z", fingerprint, "-t", filepath.Join(home, "Library", "Keychains", "login.keychain-db")}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("security commands = %#v, want %#v", calls, want)
	}
	if _, err := os.Stat(material.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TLS directory remains: %v", err)
	}
}

func TestLocalAgentUninstallTLSExplicitItemNotFoundIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	installTestLaunchctl(t)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.LocalAgentSecurityCommand = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain."), errors.New("exit status 1")
	}

	if code := app.uninstallLocalAgentLaunchAgent(context.Background(), localAgentOptions{Force: true}); code != 0 {
		t.Fatalf("uninstall code = %d, err = %q", code, errOut.String())
	}
	if _, err := os.Stat(material.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TLS directory remains: %v", err)
	}
}

func TestLocalAgentUninstallTLSMissingDeletesRecordedLedgerFingerprints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fingerprints := []string{strings.Repeat("a", 40), strings.Repeat("b", 40)}
	writeFile(t, localAgentTrustedCAFingerprintLedgerPath(home), strings.Join(fingerprints, "\n")+"\n")
	installTestLaunchctl(t)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	var calls [][]string
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}

	if code := app.uninstallLocalAgentLaunchAgent(context.Background(), localAgentOptions{Force: true}); code != 0 {
		t.Fatalf("uninstall code = %d, err = %q", code, errOut.String())
	}
	keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	want := [][]string{
		{"delete-certificate", "-Z", fingerprints[0], "-t", keychain},
		{"delete-certificate", "-Z", fingerprints[1], "-t", keychain},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("security commands = %#v, want %#v", calls, want)
	}
	if _, err := os.Stat(localAgentTrustedCAFingerprintLedgerPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ledger remains: %v", err)
	}
}

func TestLocalAgentTrustLedgerSymlinkIsRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := localAgentTrustedCAFingerprintLedgerPath(home)
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), ledgerPath); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("security command ran with unsafe ledger: %#v", args)
		return nil, nil
	}

	if err := app.ensureLocalAgentCATrust(context.Background(), home, material); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("trust error = %v", err)
	}
}

func TestLocalAgentUninstallTLSRetainsFilesOnUnrelatedKeychainErrors(t *testing.T) {
	tests := []struct {
		name   string
		output string
		err    error
	}{
		{
			name:   "unrelated not found output",
			output: "security: authorization helper not found",
			err:    errors.New("exit status 1"),
		},
		{
			name:   "user interaction error",
			output: "User interaction is not allowed",
			err:    errors.New("exit status 1"),
		},
		{
			name: "cancellation",
			err:  context.Canceled,
		},
		{
			name:   "cancellation with item not found output",
			output: "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.",
			err:    context.Canceled,
		},
		{
			name:   "deadline with item not found output",
			output: "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.",
			err:    context.DeadlineExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			material, _, err := ensureLocalAgentTLS(home, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			installTestLaunchctl(t)
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, home)
			app.LocalAgentSecurityCommand = func(_ context.Context, _ ...string) ([]byte, error) {
				return []byte(tt.output), tt.err
			}

			if code := app.uninstallLocalAgentLaunchAgent(context.Background(), localAgentOptions{Force: true}); code != 1 {
				t.Fatalf("uninstall code = %d, out = %q, err = %q", code, out.String(), errOut.String())
			}
			if _, err := os.Stat(material.Dir); err != nil {
				t.Fatalf("TLS directory was removed: %v", err)
			}
			if strings.Contains(out.String(), "uninstalled") {
				t.Fatalf("uninstall reported success: %q", out.String())
			}
		})
	}
}

func TestLocalAgentUninstallTLSChecksCanceledContextAfterSecurityCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	installTestLaunchctl(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.LocalAgentSecurityCommand = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] != "delete-certificate" {
			t.Fatalf("unexpected security command: %#v", args)
		}
		cancel()
		return []byte("security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain."), errors.New("signal: killed")
	}

	if code := app.uninstallLocalAgentLaunchAgent(ctx, localAgentOptions{Force: true}); code != 1 {
		t.Fatalf("uninstall code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(material.Dir); err != nil {
		t.Fatalf("TLS directory was removed: %v", err)
	}
	if strings.Contains(out.String(), "uninstalled") {
		t.Fatalf("uninstall reported success: %q", out.String())
	}
}

func TestLocalAgentUninstallTLSRetainsFilesWhenTrustDeletionFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	material, _, err := ensureLocalAgentTLS(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	installTestLaunchctl(t)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.LocalAgentSecurityCommand = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("access denied"), errors.New("exit status 1")
	}

	if code := app.uninstallLocalAgentLaunchAgent(context.Background(), localAgentOptions{Force: true}); code != 1 {
		t.Fatalf("uninstall code = %d, err = %q", code, errOut.String())
	}
	if _, err := os.Stat(material.Dir); err != nil {
		t.Fatalf("TLS directory was removed: %v", err)
	}
	if !strings.Contains(errOut.String(), "access denied") {
		t.Fatalf("error = %q", errOut.String())
	}
}

func installTestLaunchctl(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	launchctl := filepath.Join(binDir, "launchctl")
	writeFile(t, launchctl, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(launchctl, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
}

func TestLocalAgentRecoversInstalledAddressAndHonorsExplicitOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := ExpandPath(localAgentPlistPath)
	if err != nil {
		t.Fatal(err)
	}
	plist := localAgentLaunchAgentPlist(
		localAgentLaunchLabel,
		"/usr/local/bin/cm",
		DefaultConfigPath,
		localAgentOptions{Host: "127.0.0.9", Port: 29876},
		"/tmp/out.log",
		"/tmp/err.log",
	)
	writeFile(t, path, plist)

	opts, err := parseLocalAgentArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveInstalledLocalAgentOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Host != "127.0.0.9" || resolved.Port != 29876 {
		t.Fatalf("resolved = %#v", resolved)
	}

	explicit, err := parseLocalAgentArgs([]string{"--host", "127.0.0.7"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = resolveInstalledLocalAgentOptions(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Host != "127.0.0.7" || resolved.Port != 29876 {
		t.Fatalf("explicit resolved = %#v", resolved)
	}
}

func TestLocalAgentStatusUsesInstalledAddressWithoutFlags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true})
	}))
	defer server.Close()
	opts := localAgentOptionsForURL(t, server.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := ExpandPath(localAgentPlistPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, localAgentLaunchAgentPlist(localAgentLaunchLabel, "/usr/local/bin/cm", DefaultConfigPath, opts, "/tmp/out", "/tmp/err"))
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	if code := app.runLocalAgentService(context.Background(), DefaultConfigPath, []string{"status"}); code != 0 {
		t.Fatalf("status code = %d, out = %q, err = %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))) {
		t.Fatalf("status output = %q", out.String())
	}
}

func TestLocalAgentForceOptionSkipsDrainGuard(t *testing.T) {
	opts, err := parseLocalAgentArgs([]string{"--host", "invalid.invalid", "--port", "1", "--force"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Force || !opts.HostExplicit || !opts.PortExplicit {
		t.Fatalf("opts = %#v", opts)
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	drained, allowed := app.prepareLocalAgentShutdown(context.Background(), opts)
	if drained || !allowed || errOut.Len() != 0 {
		t.Fatalf("prepare force = (%v, %v), err = %q", drained, allowed, errOut.String())
	}
}

func TestLocalAgentLegacyDrainFallbackStillDetectsActivity(t *testing.T) {
	active := LocalTransferJob{Profile: "remote-usw2", Direction: "push", Status: LocalTransferRunning}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/activity/drain":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/activity":
			writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"active": []LocalTransferJob{active}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	opts := localAgentOptionsForURL(t, server.URL)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if drained, allowed := app.prepareLocalAgentShutdown(context.Background(), opts); drained || allowed {
		t.Fatalf("prepare = (%v, %v)", drained, allowed)
	}
	if !strings.Contains(errOut.String(), "profile=remote-usw2") || !strings.Contains(errOut.String(), "direction=push") {
		t.Fatalf("error = %q", errOut.String())
	}
}

func TestLocalAgentBootoutFailureResumesDraining(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plistPath, err := ExpandPath(localAgentPlistPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, plistPath, localAgentLaunchAgentPlist(localAgentLaunchLabel, "/usr/local/bin/cm", DefaultConfigPath, localAgentOptions{Host: "127.0.0.1", Port: 18765}, "/tmp/out", "/tmp/err"))
	binDir := t.TempDir()
	launchctl := filepath.Join(binDir, "launchctl")
	writeFile(t, launchctl, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(launchctl, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	resumed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/drain":
			writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"draining": true, "active": []LocalTransferJob{}}})
		case "/activity/resume":
			resumed <- struct{}{}
			writeWebJSON(w, webAPIResponse{OK: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	opts := localAgentOptionsForURL(t, server.URL)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	if code := app.stopLocalAgentLaunchAgent(context.Background(), opts, false); code != 1 {
		t.Fatalf("stop code = %d", code)
	}
	select {
	case <-resumed:
	case <-time.After(time.Second):
		t.Fatal("resume endpoint was not called")
	}
}

func localAgentOptionsForURL(t *testing.T, rawURL string) localAgentOptions {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return localAgentOptions{Host: host, Port: port, HostExplicit: true, PortExplicit: true}
}

func localAgentOptionsForAddr(t *testing.T, address string) localAgentOptions {
	t.Helper()
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return localAgentOptions{Host: host, Port: port, HostExplicit: true, PortExplicit: true}
}

func localAgentUnusedOptions(t *testing.T) localAgentOptions {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opts := localAgentOptionsForAddr(t, listener.Addr().String())
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return opts
}

func startLocalAgentTLSServer(t *testing.T, address string, material localAgentTLSMaterial, handler http.Handler) (net.Listener, func()) {
	t.Helper()
	certificate, err := tls.LoadX509KeyPair(material.ServerCertPath, material.ServerKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}}}
	done := make(chan struct{})
	go func() {
		_ = server.ServeTLS(listener, "", "")
		close(done)
	}()
	return listener, func() {
		_ = server.Close()
		<-done
	}
}

func configureLocalAgentInstallService(t *testing.T, app *App, home string, opts localAgentOptions) func() {
	t.Helper()
	var stop func()
	app.LocalAgentServiceCommand = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "bootout":
			if stop != nil {
				stop()
				stop = nil
			}
		case "kickstart":
			_, stop = startLocalAgentTLSServer(t, net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)), localAgentTLSPaths(home), app.newLocalAgentHandler())
		}
		return nil, nil
	}
	return func() {
		if stop != nil {
			stop()
		}
	}
}

func waitForLocalAgentEndpoint(t *testing.T, endpoint string, material localAgentTLSMaterial) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		client := localAgentHTTPClient()
		if strings.HasPrefix(endpoint, "https://") {
			client = localAgentHTTPSClient(material)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("local agent did not respond at %s", endpoint)
}

func TestLocalAgentDrainEndpointsAreAtomic(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	release := make(chan struct{})
	job, err := app.LocalTransfers.Start("remote-usw2", "push", func(onOutput func(string)) error {
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := app.newLocalAgentHandler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, localAgentTestRequest(http.MethodPost, "/activity/drain", nil))
	var activeResp struct {
		OK   bool `json:"ok"`
		Data struct {
			Draining bool               `json:"draining"`
			Active   []LocalTransferJob `json:"active"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &activeResp); err != nil {
		t.Fatal(err)
	}
	if !activeResp.OK || activeResp.Data.Draining || len(activeResp.Data.Active) != 1 || activeResp.Data.Active[0].ID != job.ID {
		t.Fatalf("active drain response = %s", rec.Body.String())
	}
	close(release)
	waitForLocalTransferJob(t, app.LocalTransfers, job.ID)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, localAgentTestRequest(http.MethodPost, "/activity/drain", nil))
	var drainedResp struct {
		OK   bool `json:"ok"`
		Data struct {
			Draining bool `json:"draining"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &drainedResp); err != nil {
		t.Fatal(err)
	}
	if !drainedResp.OK || !drainedResp.Data.Draining {
		t.Fatalf("drained response = %s", rec.Body.String())
	}
	if _, err := app.LocalTransfers.Start("remote-usw2", "pull", func(onOutput func(string)) error { return nil }); !errors.Is(err, ErrLocalTransferDraining) {
		t.Fatalf("start while draining error = %v", err)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, localAgentTestRequest(http.MethodPost, "/activity/resume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resumed, err := app.LocalTransfers.Start("remote-usw2", "pull", func(onOutput func(string)) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	waitForLocalTransferJob(t, app.LocalTransfers, resumed.ID)
}

func TestLocalAgentTransferJobAPI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := filepath.Join(home, ".ssh", "private.pem")
	writeFile(t, key, "private key")
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".connectmac", "config.yaml"), `defaults:
  identity_file: `+key+`
profiles:
`)
	localPath := filepath.Join(home, "payload.txt")
	writeFile(t, localPath, "payload")

	release := make(chan struct{})
	runner := &fakeRunner{
		rsyncOutput: []string{"  1,024  63%  1.00MB/s  0:00:01 (xfr#1, to-chk=1/3)\n"},
		rsyncWait:   release,
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.Runner = runner
	handler := app.newLocalAgentHandler()
	body := fmt.Sprintf(`{"profile":"remote-usw2","local_path":%q,"profile_yaml":"profiles:\n  remote-usw2:\n    user: ec2-user\n    host: mac-host.example.com\n"}`, localPath)

	job := postLocalTransferForTest(t, handler, "/sync/push", body)
	duplicate := postLocalTransferForTest(t, handler, "/sync/push", body)
	if duplicate.ID != job.ID {
		t.Fatalf("duplicate job id = %q, want %q", duplicate.ID, job.ID)
	}
	if job.Profile != "remote-usw2" || job.Direction != "push" || !job.Active() {
		t.Fatalf("job = %#v", job)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, localAgentTestRequest(http.MethodGet, "/sync/job?id="+url.QueryEscape(job.ID), nil))
	var jobResp struct {
		OK   bool `json:"ok"`
		Data struct {
			Job LocalTransferJob `json:"job"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &jobResp); err != nil || !jobResp.OK || jobResp.Data.Job.ID != job.ID {
		t.Fatalf("job response = %s, decode error = %v", rec.Body.String(), err)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, localAgentTestRequest(http.MethodGet, "/sync/jobs?profile=remote-usw2", nil))
	var jobsResp struct {
		OK   bool `json:"ok"`
		Data struct {
			Jobs []LocalTransferJob `json:"jobs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &jobsResp); err != nil || !jobsResp.OK || len(jobsResp.Data.Jobs) != 1 {
		t.Fatalf("jobs response = %s, decode error = %v", rec.Body.String(), err)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, localAgentTestRequest(http.MethodGet, "/activity", nil))
	activity, activityErr := decodeLocalAgentActivityResponse(rec.Result())
	if activityErr != nil || len(activity) != 1 || activity[0].ID != job.ID {
		t.Fatalf("activity = %#v, error = %v, body = %s", activity, activityErr, rec.Body.String())
	}

	close(release)
	finished := waitForLocalTransferJob(t, app.LocalTransfers, job.ID)
	if finished.Status != LocalTransferSucceeded || finished.Percent != 100 {
		t.Fatalf("finished job = %#v", finished)
	}
	joinedArgs := strings.Join(runner.rsync, " ")
	if !strings.Contains(joinedArgs, key) || !strings.Contains(joinedArgs, "ec2-user@mac-host.example.com:~/Downloads/") {
		t.Fatalf("rsync args = %#v", runner.rsync)
	}

	runner.rsyncOutput = []string{"  2,048  41%  1.00MB/s  0:00:01\n"}
	runner.rsyncWait = nil
	runner.rsyncErr = errors.New("exit status 23")
	pullBody := `{"profile":"remote-usw2","profile_yaml":"profiles:\n  remote-usw2:\n    user: ec2-user\n    host: mac-host.example.com\n"}`
	pull := postLocalTransferForTest(t, handler, "/sync/pull", pullBody)
	failed := waitForLocalTransferJob(t, app.LocalTransfers, pull.ID)
	if failed.Status != LocalTransferFailed || failed.Phase != TransferPhaseFailed || failed.Percent != 39 || !strings.Contains(failed.Error, "exit status 23") {
		t.Fatalf("failed pull = %#v", failed)
	}
	if got := runner.rsync[len(runner.rsync)-1]; got != "." {
		t.Fatalf("pull local path = %q, want .; args = %#v", got, runner.rsync)
	}
	if !strings.Contains(runner.rsync[len(runner.rsync)-2], "ec2-user@mac-host.example.com:~/Downloads/") {
		t.Fatalf("pull remote path args = %#v", runner.rsync)
	}
}

func TestLocalAgentTransferUsesProgress2WhenSupported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(connectMacRsyncEnv, "/usr/local/bin/rsync")
	key := filepath.Join(home, ".ssh", "private.pem")
	writeFile(t, key, "private key")
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(home, "payload.txt")
	writeFile(t, localPath, "payload")

	release := make(chan struct{})
	runner := &fakeRunner{
		rsyncProbe:  map[string]error{"/usr/local/bin/rsync": nil},
		rsyncOutput: []string{"     7,654,321  73%   44.35MB/s    0:00:01 (xfr#12, ir-chk=8/20)\n"},
		rsyncWait:   release,
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.Runner = runner
	handler := app.newLocalAgentHandler()
	profileYAML := "profiles:\n  remote-usw2:\n    user: ec2-user\n    host: mac-host.example.com\n    identity_file: " + key + "\n"
	body := fmt.Sprintf(`{"profile":"remote-usw2","local_path":%q,"profile_yaml":%q}`, localPath, profileYAML)
	job := postLocalTransferForTest(t, handler, "/sync/push", body)

	deadline := time.Now().Add(time.Second)
	var active LocalTransferJob
	for time.Now().Before(deadline) {
		var ok bool
		active, ok = app.LocalTransfers.Get(job.ID)
		if ok && active.Percent == 73 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if active.ProgressMode != LocalTransferProgressTotal || active.Percent != 73 {
		t.Fatalf("active job = %+v", active)
	}
	if runner.rsyncPath != "/usr/local/bin/rsync" || !containsString(runner.rsync, "--info=progress2") || containsString(runner.rsync, "-avzP") {
		t.Fatalf("rsync path=%q args=%#v", runner.rsyncPath, runner.rsync)
	}
	close(release)
	finished := waitForLocalTransferJob(t, app.LocalTransfers, job.ID)
	if finished.Status != LocalTransferSucceeded || finished.Percent != 100 {
		t.Fatalf("finished job = %+v", finished)
	}
}

func TestLocalTransferCorrelationAndLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := filepath.Join(home, ".ssh", "private.pem")
	writeFile(t, key, "-----BEGIN PRIVATE KEY-----\nprivate-key-material\n-----END PRIVATE KEY-----\n")
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(home, "payload.txt")
	writeFile(t, localPath, "payload")

	runner := &fakeRunner{
		rsyncOutput: []string{
			"file-one (xfr#1, to-chk=9/10)\n",
			"file-two (xfr#2, to-chk=5/10)\n",
			"file-three (xfr#3, to-chk=0/10)\n",
		},
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.Runner = runner
	handler := app.newLocalAgentHandler()
	profileYAML := "profiles:\n  remote-usw2:\n    user: ec2-user\n    host: mac-host.example.com\n    identity_file: " + key + "\n"
	body := fmt.Sprintf(`{"transfer_id":"member-transfer-1","profile":"remote-usw2","local_path":%q,"profile_yaml":%q}`, localPath, profileYAML)
	job := postLocalTransferForTest(t, handler, "/sync/push", body)
	finished := waitForLocalTransferJob(t, app.LocalTransfers, job.ID)
	if finished.TransferID != "member-transfer-1" {
		t.Fatalf("job transfer id = %q", finished.TransferID)
	}

	entries := waitForLocalTransferLogs(t, app.LogManager, 8)
	actions := make(map[string]bool)
	for _, entry := range entries {
		actions[entry.Action] = true
		if entry.TransferID != "member-transfer-1" || entry.LocalJobID != job.ID ||
			entry.Profile != "remote-usw2" || entry.Direction != "push" ||
			entry.Status == "" || entry.ElapsedMS < 0 {
			t.Fatalf("log entry = %+v", entry)
		}
	}
	for _, action := range []string{"transfer.local.started", "transfer.progress", "transfer.local.succeeded"} {
		if !actions[action] {
			t.Fatalf("missing action %q in %+v", action, entries)
		}
	}
	raw := readTestLogsRaw(t, app.LogManager)
	for _, secret := range []string{profileYAML, key, "private-key-material", "profile_yaml", "cookie=", "token="} {
		if strings.Contains(raw, secret) {
			t.Fatalf("local transfer log contains secret %q: %s", secret, raw)
		}
	}
}

func TestLocalTransferFailedLogSanitizesError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := filepath.Join(home, ".ssh", "private.pem")
	writeFile(t, key, "private key")
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(home, "payload.txt")
	writeFile(t, localPath, "payload")

	runner := &fakeRunner{
		rsyncOutput: []string{
			"  1,024  25% password=hunter2 token session-token cookie browser-cookie\n",
			"-----BEGIN CERTIFICATE-----\ncertificate-material\n-----END CERTIFICATE-----\n",
		},
		rsyncErr: errors.New("rsync failed with token terminal-token"),
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.Runner = runner
	body := fmt.Sprintf(`{"transfer_id":"member-transfer-failed","profile":"remote-usw2","local_path":%q,"profile_yaml":"profiles:\n  remote-usw2:\n    user: ec2-user\n    host: mac-host.example.com\n    identity_file: %s\n"}`, localPath, key)
	job := postLocalTransferForTest(t, app.newLocalAgentHandler(), "/sync/push", body)
	waitForLocalTransferJob(t, app.LocalTransfers, job.ID)

	entries := waitForLocalTransferLogs(t, app.LogManager, 3)
	var failed LogEntry
	for _, entry := range entries {
		if entry.Action == "transfer.local.failed" {
			failed = entry
			break
		}
	}
	if failed.Action == "" || failed.Status != LocalTransferFailed || failed.Phase != TransferPhaseFailed || failed.Percent != 24 {
		t.Fatalf("failed log = %+v, entries = %+v", failed, entries)
	}
	raw := readTestLogsRaw(t, app.LogManager)
	for _, secret := range []string{"hunter2", "session-token", "browser-cookie", "terminal-token", "certificate-material", "BEGIN CERTIFICATE", key, "profile_yaml"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("failed log contains secret %q: %s", secret, raw)
		}
	}
}

func TestLocalTransferInterruptedLifecycleLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := filepath.Join(home, ".ssh", "private.pem")
	writeFile(t, key, "private key")
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(home, "payload.txt")
	writeFile(t, localPath, "payload")

	runner := &fakeRunner{
		rsyncOutput: []string{"  1,024  50% cookie interrupted-cookie\n"},
		rsyncErr:    context.DeadlineExceeded,
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	app.Runner = runner
	body := fmt.Sprintf(`{"transfer_id":"member-transfer-interrupted","profile":"remote-usw2","local_path":%q,"profile_yaml":"profiles:\n  remote-usw2:\n    user: ec2-user\n    host: mac-host.example.com\n    identity_file: %s\n"}`, localPath, key)
	job := postLocalTransferForTest(t, app.newLocalAgentHandler(), "/sync/push", body)
	finished := waitForLocalTransferJob(t, app.LocalTransfers, job.ID)
	if finished.Status != LocalTransferInterrupted || finished.Phase != TransferPhaseInterrupted || finished.Percent != 48 {
		t.Fatalf("interrupted job = %+v", finished)
	}

	entries := waitForLocalTransferLogs(t, app.LogManager, 4)
	var interrupted LogEntry
	for _, entry := range entries {
		if entry.Action == "transfer.local.interrupted" {
			interrupted = entry
			break
		}
	}
	if interrupted.Action == "" || interrupted.Level != "warn" ||
		interrupted.TransferID != "member-transfer-interrupted" ||
		interrupted.LocalJobID != job.ID || interrupted.Status != LocalTransferInterrupted ||
		interrupted.Phase != TransferPhaseInterrupted || interrupted.Percent != 48 {
		t.Fatalf("interrupted log = %+v, entries = %+v", interrupted, entries)
	}
	raw := readTestLogsRaw(t, app.LogManager)
	if strings.Contains(raw, "interrupted-cookie") {
		t.Fatalf("interrupted log leaked cookie: %s", raw)
	}
}

func waitForLocalTransferLogs(t *testing.T, manager LogManager, minimum int) []LogEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		files, err := manager.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 1 {
			entries := readTestLogEntries(t, manager)
			if len(entries) >= minimum {
				return entries
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d local transfer logs", minimum)
	return nil
}

func TestLocalAgentActivityDecodeAndGuard(t *testing.T) {
	activeJob := LocalTransferJob{
		ID:        "transfer-1",
		Profile:   "remote-usw2",
		Direction: "pull",
		Status:    LocalTransferRunning,
		CreatedAt: time.Now(),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activity" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"active": []LocalTransferJob{activeJob}}})
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(serverURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if app.guardLocalAgentActivity(context.Background(), localAgentOptions{Host: host, Port: port}) {
		t.Fatal("guard allowed an active transfer")
	}
	if !strings.Contains(errOut.String(), "profile=remote-usw2") || !strings.Contains(errOut.String(), "direction=pull") {
		t.Fatalf("guard error = %q", errOut.String())
	}

	server.Close()
	oldServer := httptest.NewServer(http.NotFoundHandler())
	oldURL, err := url.Parse(oldServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	oldHost, oldPortText, err := net.SplitHostPort(oldURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	oldPort, err := strconv.Atoi(oldPortText)
	if err != nil {
		t.Fatal(err)
	}
	oldOptions := localAgentOptions{Host: oldHost, Port: oldPort}
	if !app.guardLocalAgentActivity(context.Background(), oldOptions) {
		t.Fatal("guard blocked an old agent returning 404")
	}
	oldServer.Close()
	if !app.guardLocalAgentActivity(context.Background(), oldOptions) {
		t.Fatal("guard blocked when the agent was unreachable")
	}

	tests := []struct {
		name       string
		handler    http.Handler
		wantReason string
	}{
		{
			name: "http 500",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "internal failure", http.StatusInternalServerError)
			}),
			wantReason: "500",
		},
		{
			name: "malformed json",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"ok":`))
			}),
			wantReason: "decode",
		},
		{
			name: "ok false",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeWebJSON(w, webAPIResponse{OK: false, Error: "activity unavailable"})
			}),
			wantReason: "ok=false",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failureServer := httptest.NewServer(tt.handler)
			defer failureServer.Close()
			failureURL, err := url.Parse(failureServer.URL)
			if err != nil {
				t.Fatal(err)
			}
			failureHost, failurePortText, err := net.SplitHostPort(failureURL.Host)
			if err != nil {
				t.Fatal(err)
			}
			failurePort, err := strconv.Atoi(failurePortText)
			if err != nil {
				t.Fatal(err)
			}
			errOut.Reset()
			if app.guardLocalAgentActivity(context.Background(), localAgentOptions{Host: failureHost, Port: failurePort}) {
				t.Fatal("guard allowed unverifiable activity")
			}
			if !strings.Contains(errOut.String(), "unable to verify local-agent activity") || !strings.Contains(errOut.String(), tt.wantReason) {
				t.Fatalf("guard error = %q", errOut.String())
			}
		})
	}
}

func postLocalTransferForTest(t *testing.T, handler http.Handler, path, body string) LocalTransferJob {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, localAgentTestRequest(http.MethodPost, path, strings.NewReader(body)))
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Data  struct {
			Job LocalTransferJob `json:"job"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v, body = %s", err, rec.Body.String())
	}
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("status = %d, error = %q, body = %s", rec.Code, resp.Error, rec.Body.String())
	}
	return resp.Data.Job
}

func localAgentTestRequest(method, path string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Origin", "https://cm.hsgitlab.xyz")
	return req
}

func TestAppListRemoteProfilesRequiresSession(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, `server:
  user_api: https://cm.example.com
`)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"list", "--config", config}); code == 0 {
		t.Fatalf("list code = 0, want failure")
	}
	if !strings.Contains(errOut.String(), "remote profile list requires server.token") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	app.Version = "0.1.test"
	if code := app.Run(context.Background(), []string{"version"}); code != 0 {
		t.Fatalf("version code = %d, err = %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "cm 0.1.test" {
		t.Fatalf("version output = %q", out.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"--version"}); code != 0 {
		t.Fatalf("--version code = %d, err = %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "cm 0.1.test" {
		t.Fatalf("--version output = %q", out.String())
	}
}

func TestAppMCPHelpExplainsStdioServer(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"mcp", "--help"}); code != 0 {
		t.Fatalf("mcp help code = %d, err = %s", code, errOut.String())
	}
	for _, want := range []string{"cm mcp tools", "stdio MCP server", "does not print a tool list"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("mcp help missing %q:\n%s", want, out.String())
		}
	}
}

func TestAppMCPToolsHumanReadable(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"mcp", "tools"}); code != 0 {
		t.Fatalf("mcp tools code = %d, err = %s", code, errOut.String())
	}
	for _, want := range []string{"TOOL", "DESCRIPTION", "REQUIRED", "KEY PARAMS", "cm_mcp_guide", "cm_list_profiles", "cm_aws_destroy_mac_by_email", "apple_email"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("mcp tools missing %q:\n%s", want, out.String())
		}
	}
}

func TestAppMCPToolsJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"mcp", "tools", "--json"}); code != 0 {
		t.Fatalf("mcp tools --json code = %d, err = %s", code, errOut.String())
	}
	var payload struct {
		Tools []struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			InputSchema map[string]interface{} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out.String())
	}
	if len(payload.Tools) != len(mcpTools()) {
		t.Fatalf("tool count = %d, want %d", len(payload.Tools), len(mcpTools()))
	}
	found := false
	for _, tool := range payload.Tools {
		if tool.Name == "cm_push" {
			found = true
			required, _ := tool.InputSchema["required"].([]interface{})
			if len(required) != 3 {
				t.Fatalf("cm_push required = %#v", required)
			}
		}
	}
	if !found {
		t.Fatalf("cm_push not found in tools: %#v", payload.Tools)
	}
}

func TestAppGuideShowsStepByStepTopics(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"guide"}); code != 0 {
		t.Fatalf("guide code = %d, err = %s", code, errOut.String())
	}
	for _, want := range []string{"ConnectMac guide", "cm profile wizard", "cm next <profile-or-apple-email>", "AWS mutations always preview first"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("guide output missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"guide", "open"}); code != 0 {
		t.Fatalf("guide open code = %d, err = %s", code, errOut.String())
	}
	for _, want := range []string{"ConnectMac open-Mac guide", "cm aws open <apple-email>", "blocked: stop"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("guide open missing %q:\n%s", want, out.String())
		}
	}
}

func TestAppCompletionProfiles(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"completion", "profiles", "--config", config}); code != 0 {
		t.Fatalf("completion profiles code = %d, err = %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "xcode-vnc" {
		t.Fatalf("profiles output = %q", out.String())
	}
}

func TestAppMemberCommands(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"member", "add", "--name", "王恒辉", "--email", "whh@example.com"}); code != 0 {
		t.Fatalf("member add code = %d, err = %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"member", "assign", "apple@example.com", "--member", "whh@example.com"}); code != 0 {
		t.Fatalf("member assign code = %d, err = %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"member", "list"}); code != 0 {
		t.Fatalf("member list code = %d, err = %s", code, errOut.String())
	}
	for _, want := range []string{"EMAIL", "whh@example.com", "王恒辉", "apple@example.com(owner)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("member list missing %q:\n%s", want, out.String())
		}
	}
}

func TestAppWebProfilesAPI(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if _, err := app.MemberStore.AddMember("王恒辉", "whh@example.com", "operator"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if _, err := app.MemberStore.AssignMember("user@example.com", "whh@example.com", "owner"); err != nil {
		t.Fatalf("assign member: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/profiles", nil)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp webAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("response not ok: %#v", resp)
	}
	if !strings.Contains(rec.Body.String(), "xcode-vnc") || !strings.Contains(rec.Body.String(), "user@example.com") || strings.Contains(rec.Body.String(), "whh@example.com") {
		t.Fatalf("profiles body = %s", rec.Body.String())
	}
}

func TestAppWebProfileOwnerAPI(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if _, err := app.MemberStore.AddMember("旧负责人", "old@example.com", "operator"); err != nil {
		t.Fatalf("add old member: %v", err)
	}
	if _, err := app.MemberStore.AddMember("王恒辉", "whh@example.com", "operator"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if _, err := app.MemberStore.UpsertManagedProfile(Profile{Name: "xcode-vnc"}); err != nil {
		t.Fatalf("upsert managed profile: %v", err)
	}
	if _, err := app.MemberStore.AssignMember("user@example.com", "old@example.com", "owner"); err != nil {
		t.Fatalf("assign old member: %v", err)
	}

	body := strings.NewReader(`{"profile":"xcode-vnc","member_email":"whh@example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/profile-owner/set", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"profile_name":"xcode-vnc"`) || !strings.Contains(rec.Body.String(), "whh@example.com") {
		t.Fatalf("profile owner set body = %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/profiles", nil)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "whh@example.com") || strings.Contains(rec.Body.String(), "old@example.com") {
		t.Fatalf("profiles body = %s", rec.Body.String())
	}
}

func TestAppWebManagedProfilesAccessAPI(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)

	profileYAML := `profiles:
  managed-usw2:
    description: Apple account: managed@example.com
    host: ec2-1-2-3-4.us-west-2.compute.amazonaws.com
    aws:
      profile: cm-xcode
      region: us-west-2
      account_email: managed@example.com
`
	body := strings.NewReader(`{"profile_yaml":` + strconv.Quote(profileYAML) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/managed-profile/save", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}

	if _, err := app.MemberStore.AddMember("User", "user@example.com", "operator"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/managed-profiles?include_yaml=1", nil)
	addWebAuth(t, &app, req, "operator")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("member list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "managed-usw2") {
		t.Fatalf("ungranted member should not see profile: %s", rec.Body.String())
	}

	body = strings.NewReader(`{"profile":"managed-usw2","member_email":"admin@example.com","grant":true}`)
	req = httptest.NewRequest(http.MethodPost, "/api/managed-profile/access", body)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/managed-profiles?include_yaml=1", nil)
	addWebAuth(t, &app, req, "operator")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "managed-usw2") {
		t.Fatalf("granted member list body = %s status=%d", rec.Body.String(), rec.Code)
	}

	body = strings.NewReader(`{"profile":"managed-usw2","enabled":false}`)
	req = httptest.NewRequest(http.MethodPost, "/api/managed-profile/status", body)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/managed-profiles?include_yaml=1", nil)
	addWebAuth(t, &app, req, "operator")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled member list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "managed-usw2") {
		t.Fatalf("disabled profile should be hidden from member: %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/managed-profiles?include_yaml=1", nil)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "managed-usw2") || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("admin should see disabled profile body = %s status=%d", rec.Body.String(), rec.Code)
	}

	body = strings.NewReader(`{"profile":"managed-usw2","enabled":true}`)
	req = httptest.NewRequest(http.MethodPost, "/api/managed-profile/status", body)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/managed-profiles?include_yaml=1", nil)
	addWebAuth(t, &app, req, "operator")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "managed-usw2") {
		t.Fatalf("re-enabled member list body = %s status=%d", rec.Body.String(), rec.Code)
	}
}

func TestAppWebAPITokenCanListManagedProfiles(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	profileYAML := `profiles:
  token-usw2:
    description: Apple account: token@example.com
    aws:
      region: us-west-2
      account_email: token@example.com
`
	body := strings.NewReader(`{"profile_yaml":` + strconv.Quote(profileYAML) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/managed-profile/save", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/auth/token", strings.NewReader(`{}`))
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var bodyResp struct {
		OK   bool `json:"ok"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bodyResp); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if !strings.HasPrefix(bodyResp.Data.Token, webAPITokenPrefix) {
		t.Fatalf("token = %q", bodyResp.Data.Token)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/managed-profiles?include_yaml=1", nil)
	req.Header.Set("Authorization", "Bearer "+bodyResp.Data.Token)
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "token-usw2") {
		t.Fatalf("token list body = %s status=%d", rec.Body.String(), rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/auth/token", strings.NewReader(`{"action":"delete"}`))
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete token status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/managed-profiles?include_yaml=1", nil)
	req.Header.Set("Authorization", "Bearer "+bodyResp.Data.Token)
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted token status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppWebMemberProfilesAPIReplacesMemberAccess(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)

	for _, profileYAML := range []string{
		`profiles:
  first-usw2:
    description: Apple account: first@example.com
    host: ec2-1-2-3-4.us-west-2.compute.amazonaws.com
    aws:
      profile: cm-xcode
      region: us-west-2
      account_email: first@example.com
`,
		`profiles:
  second-usw2:
    description: Apple account: second@example.com
    host: ec2-5-6-7-8.us-west-2.compute.amazonaws.com
    aws:
      profile: cm-xcode
      region: us-west-2
      account_email: second@example.com
`,
	} {
		body := strings.NewReader(`{"profile_yaml":` + strconv.Quote(profileYAML) + `}`)
		req := httptest.NewRequest(http.MethodPost, "/api/managed-profile/save", body)
		addWebAuth(t, &app, req, "admin")
		rec := httptest.NewRecorder()
		app.newWebHandler(config).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("save profile status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	body := strings.NewReader(`{"member_email":"admin@example.com","profiles":["first-usw2","second-usw2"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/member/profiles", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("member profiles status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/managed-profiles?include_yaml=1", nil)
	addWebAuth(t, &app, req, "operator")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "first-usw2") || !strings.Contains(rec.Body.String(), "second-usw2") {
		t.Fatalf("member should see both profiles body = %s status=%d", rec.Body.String(), rec.Code)
	}

	body = strings.NewReader(`{"member_email":"admin@example.com","profiles":["second-usw2"]}`)
	req = httptest.NewRequest(http.MethodPost, "/api/member/profiles", body)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace member profiles status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/managed-profiles?include_yaml=1", nil)
	addWebAuth(t, &app, req, "operator")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "first-usw2") || !strings.Contains(rec.Body.String(), "second-usw2") {
		t.Fatalf("member should see only replacement profile body = %s status=%d", rec.Body.String(), rec.Code)
	}
}

func TestCleanupDefaultLocalConfigProfilesBacksUpAndClearsProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".connectmac")
	profilesDir := filepath.Join(configDir, "profiles")
	configPath := filepath.Join(configDir, "config.yaml")
	writeFile(t, configPath, `server:
  user_api: https://cm.example.com
defaults:
  user: ec2-user
  identity_file: ~/.ssh/member.pem
  aws:
    amis_by_region:
      us-west-2:
        mac_x86: ami-x86
        mac_arm: ami-arm
profiles:
  old-usw2:
    description: old local profile
    host: old.example.com
`)
	writeFile(t, filepath.Join(profilesDir, "extra.yaml"), `profiles:
  extra-usw2:
    description: extra local profile
    host: extra.example.com
`)

	backup, err := cleanupDefaultLocalConfigProfiles(DefaultConfigPath, time.Date(2026, 7, 7, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if backup == "" {
		t.Fatalf("expected backup path")
	}
	if _, err := os.Stat(filepath.Join(backup, "config.yaml")); err != nil {
		t.Fatalf("backup config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backup, "profiles", "extra.yaml")); err != nil {
		t.Fatalf("backup profiles: %v", err)
	}
	if entries, err := os.ReadDir(profilesDir); err != nil {
		t.Fatalf("profiles dir after cleanup: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("profiles dir should be empty, got %d entries", len(entries))
	}
	cfg, err := LoadConfig(DefaultConfigPath)
	if err != nil {
		t.Fatalf("load cleaned config: %v", err)
	}
	if cfg.Server.UserAPI != "https://cm.example.com" {
		t.Fatalf("server user api = %q", cfg.Server.UserAPI)
	}
	if cfg.Defaults.IdentityFile != "~/.ssh/member.pem" {
		t.Fatalf("identity file = %q", cfg.Defaults.IdentityFile)
	}
	if len(cfg.Profiles) != 0 {
		t.Fatalf("profiles after cleanup = %+v", cfg.Profiles)
	}
}

func TestAppWebAWSStatusUsesManagedProfilesWhenLocalProfilesAreCleared(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, `defaults:
  user: ec2-user
  identity_file: `+key+`
profiles:
`)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{}}, nil
	}
	profile, err := ParseSingleProfileYAML(`profiles:
  managed-usw2:
    description: Apple account: managed@example.com
    host: ec2-1-2-3-4.us-west-2.compute.amazonaws.com
    aws:
      profile: cm-xcode
      region: us-west-2
      account_email: managed@example.com
      key_name: maiqi-xcode
      security_group_id: sg-example
      elastic_ip_allocation_id: eipalloc-example
      elastic_ip_owner_tag:
        key: Apple
        value: managed@example.com
      availability_zone_ids:
        - usw2-az1
      instance_type_priority:
        - mac2-m2.metal
`)
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	if _, err := app.MemberStore.UpsertManagedProfile(profile); err != nil {
		t.Fatalf("upsert managed profile: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/aws/status?profile=managed-usw2", nil)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "no profiles are configured") || strings.Contains(rec.Body.String(), "unknown profile") {
		t.Fatalf("status used cleared local profiles: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "managed-usw2") {
		t.Fatalf("status body = %s", rec.Body.String())
	}
}

func TestAppWebAWSStatusFallsBackToManagedProfilesWithoutLocalAuth(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, `defaults:
  user: ec2-user
  identity_file: `+key+`
profiles:
`)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.RemoteUserAPI = true
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{}}, nil
	}
	profile, err := ParseSingleProfileYAML(`profiles:
  managed-no-auth-usw2:
    description: Apple account: managed-no-auth@example.com
    host: ec2-1-2-3-5.us-west-2.compute.amazonaws.com
    aws:
      profile: cm-xcode
      region: us-west-2
      account_email: managed-no-auth@example.com
      key_name: maiqi-xcode
      security_group_id: sg-example
      elastic_ip_allocation_id: eipalloc-example
      elastic_ip_owner_tag:
        key: Apple
        value: managed-no-auth@example.com
      availability_zone_ids:
        - usw2-az1
      instance_type_priority:
        - mac2-m2.metal
`)
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	if _, err := app.MemberStore.UpsertManagedProfile(profile); err != nil {
		t.Fatalf("upsert managed profile: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/aws/status?profile=managed-no-auth-usw2", nil)
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "no profiles are configured") || strings.Contains(rec.Body.String(), "unknown profile") {
		t.Fatalf("status used cleared local profiles: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "managed-no-auth-usw2") {
		t.Fatalf("status body = %s", rec.Body.String())
	}
}

func TestAppWebProfilesUsesManagedProfilesWhenConfigMissing(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "missing", "config.yaml")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	profile, err := ParseSingleProfileYAML(`profiles:
  managed-missing-config-usw2:
    description: Apple account: managed-missing-config@example.com
    host: ec2-1-2-3-6.us-west-2.compute.amazonaws.com
    aws:
      profile: cm-xcode
      region: us-west-2
      account_email: managed-missing-config@example.com
      key_name: maiqi-xcode
      security_group_id: sg-example
      elastic_ip_allocation_id: eipalloc-example
      elastic_ip_owner_tag:
        key: Apple
        value: managed-missing-config@example.com
      availability_zone_ids:
        - usw2-az1
      instance_type_priority:
        - mac2-m2.metal
`)
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	if _, err := app.MemberStore.UpsertManagedProfile(profile); err != nil {
		t.Fatalf("upsert managed profile: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/profiles", nil)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "managed-missing-config-usw2") {
		t.Fatalf("profiles body = %s", rec.Body.String())
	}
}

func TestAppWebProfilesAPISkipsLocalAuthWithRemoteUserAPI(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.RemoteUserAPI = true
	req := httptest.NewRequest(http.MethodGet, "/api/profiles", nil)
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "xcode-vnc") {
		t.Fatalf("profiles body = %s", rec.Body.String())
	}
}

func TestAppWebConfigAPI(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, `
server:
  user_api: https://cm.hsgitlab.xyz/
profiles:
  xcode-vnc:
    description: Example
`)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"user_api":"https://cm.hsgitlab.xyz"`) {
		t.Fatalf("config body = %s", rec.Body.String())
	}
}

func TestAppWebUserProxySessionCookieBecomesSameOrigin(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "cm_session=signed-session; Path=/; HttpOnly; Secure; SameSite=None")
	rec := httptest.NewRecorder()
	copyUserProxySessionCookies(rec, resp)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %+v, want one session cookie", cookies)
	}
	if cookies[0].Secure {
		t.Fatalf("proxied local cookie should not require Secure on local http")
	}
	if cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("proxied local cookie SameSite = %v, want Lax", cookies[0].SameSite)
	}
}

func TestAppWebMemberAPIs(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)

	body := strings.NewReader(`{"name":"王恒辉","email":"whh@example.com","role":"operator"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/member/add", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("add status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = strings.NewReader(`{"email":"whh@example.com","password":"newpassword123"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/member/password", body)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set member password status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok, err := app.MemberStore.VerifyMemberPassword("whh@example.com", "newpassword123"); err != nil || !ok {
		t.Fatalf("member password not updated ok=%t err=%v", ok, err)
	}

	body = strings.NewReader(`{"original_email":"whh@example.com","name":"王恒辉2","email":"whh2@example.com","role":"viewer"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/member/update", body)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update member status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = strings.NewReader(`{"apple_email":"user@example.com","member_email":"whh@example.com","relation":"owner"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/member/assign", body)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("assign should not accept old member email after update")
	}

	body = strings.NewReader(`{"apple_email":"user@example.com","member_email":"whh2@example.com","relation":"owner"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/member/assign", body)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign updated member status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/members", nil)
	addWebAuth(t, &app, req, "admin")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"whh2@example.com", "王恒辉2", "viewer", "user@example.com", "owner"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("members body missing %q:\n%s", want, rec.Body.String())
		}
	}

	body = strings.NewReader(`{"name":"Nope","email":"nope@example.com","role":"operator"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/member/add", body)
	addWebAuth(t, &app, req, "operator")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("operator add member status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/members", nil)
	addWebAuth(t, &app, req, "operator")
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("operator list members status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppWebAuthSetupLoginAndSettings(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)

	challenge := webChallengeForTest(t, app)
	body := strings.NewReader(`{"name":"管理员","email":"admin@example.com","password":"password123","challenge_token":"` + challenge["token"] + `","challenge_answer":"` + challenge["answer"] + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup", body)
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatalf("expected setup to set session cookie")
	}

	challenge = webChallengeForTest(t, app)
	body = strings.NewReader(`{"username":"admin@example.com","password":"password123","challenge_token":"` + challenge["token"] + `","challenge_answer":"` + challenge["answer"] + `"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	rec = httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected login to set session cookie")
	}
	if cookies[0].Secure {
		t.Fatalf("local login cookie should not be secure")
	}
	if cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("local login cookie SameSite = %v, want Lax", cookies[0].SameSite)
	}

	body = strings.NewReader(`{"default_owner_email":"admin@example.com","default_status_filter":"ready","background_confirm":true,"show_released":true}`)
	req = httptest.NewRequest(http.MethodPost, "/api/settings", body)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "auth_secret") {
		t.Fatalf("settings leaked auth secret: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "admin@example.com") || !strings.Contains(rec.Body.String(), "ready") {
		t.Fatalf("settings body = %s", rec.Body.String())
	}

	challenge = webChallengeForTest(t, app)
	body = strings.NewReader(`{"email":"new-admin@example.com","password":"password123","challenge_token":"` + challenge["token"] + `","challenge_answer":"` + challenge["answer"] + `"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/auth/update-email", body)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update email status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "new-admin@example.com") || strings.Contains(rec.Body.String(), "password_hash") {
		t.Fatalf("update email body = %s", rec.Body.String())
	}
	updatedCookies := rec.Result().Cookies()
	if len(updatedCookies) == 0 {
		t.Fatalf("expected update email to refresh session cookie")
	}
	_, ok, err := app.MemberStore.VerifyMemberPassword("new-admin@example.com", "password123")
	if err != nil || !ok {
		t.Fatalf("updated email login failed ok=%t err=%v", ok, err)
	}

	body = strings.NewReader(`{"current_password":"password123","new_password":"newpassword123","confirm_password":"newpassword123"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/auth/change-password", body)
	req.AddCookie(updatedCookies[0])
	rec = httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password status = %d, body = %s", rec.Code, rec.Body.String())
	}
	_, ok, err = app.MemberStore.VerifyMemberPassword("new-admin@example.com", "newpassword123")
	if err != nil || !ok {
		t.Fatalf("changed password login failed ok=%t err=%v", ok, err)
	}
}

func TestAppWebAuthHTTPSProxyCookie(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if _, err := app.MemberStore.SetupAdmin("管理员", "admin@example.com", "password123"); err != nil {
		t.Fatalf("setup admin: %v", err)
	}
	challenge := webChallengeForTest(t, app)
	body := strings.NewReader(`{"username":"admin@example.com","password":"password123","challenge_token":"` + challenge["token"] + `","challenge_answer":"` + challenge["answer"] + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected login to set session cookie")
	}
	if !cookies[0].Secure {
		t.Fatalf("https proxy cookie should be secure")
	}
	if cookies[0].SameSite != http.SameSiteNoneMode {
		t.Fatalf("https proxy cookie SameSite = %v, want None", cookies[0].SameSite)
	}
}

func TestAppWebServesExternalIndex(t *testing.T) {
	dir := t.TempDir()
	webDir := filepath.Join(dir, "web")
	writeFile(t, filepath.Join(webDir, "index.html"), "<!doctype html><title>External ConnectMac</title>")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.WebDir = webDir
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "External ConnectMac") {
		t.Fatalf("index body = %s", rec.Body.String())
	}
}

func TestAppWebServesVendorAssets(t *testing.T) {
	dir := t.TempDir()
	webDir := filepath.Join(dir, "web")
	writeFile(t, filepath.Join(webDir, "index.html"), "<!doctype html><title>External ConnectMac</title>")
	writeFile(t, filepath.Join(webDir, "vendor", "xterm", "xterm.js"), "window.Terminal = function() {};")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.WebDir = webDir
	req := httptest.NewRequest(http.MethodGet, "/vendor/xterm/xterm.js", nil)
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "window.Terminal") {
		t.Fatalf("asset body = %s", rec.Body.String())
	}
}

func TestAppWebServesAssets(t *testing.T) {
	dir := t.TempDir()
	webDir := filepath.Join(dir, "web")
	writeFile(t, filepath.Join(webDir, "index.html"), "<!doctype html><title>External ConnectMac</title>")
	writeFile(t, filepath.Join(webDir, "assets", "connectmac-mark.svg"), "<svg>ConnectMac</svg>")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.WebDir = webDir

	req := httptest.NewRequest(http.MethodGet, "/assets/connectmac-mark.svg", nil)
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ConnectMac") {
		t.Fatalf("asset body = %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/assets/connectmac-mark.svg", nil)
	rec = httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppWebAWSStatusAPI(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "running", InstanceType: "mac2.metal", HostID: "h-1", PublicIP: "203.0.113.10", SystemStatus: "ok", InstanceStatusCheck: "ok", EBSStatus: "ok", Tags: managedTestTags()}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", AssociationID: "eipassoc-1", InstanceID: "i-1", PublicIP: "203.0.113.10"},
		}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/api/aws/status?profile=xcode-vnc", nil)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "AWS Mac status for profile xcode-vnc") || !strings.Contains(rec.Body.String(), "203.0.113.10") {
		t.Fatalf("status body = %s", rec.Body.String())
	}
	var resp webAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("status data = %#v", resp.Data)
	}
	if data["decision"] != "ready" || data["ready"] != true || data["next"] != "cm start xcode-vnc" {
		t.Fatalf("structured status data = %#v", data)
	}
}

func TestAppWebAWSStatusAutoCleansReleasedRecords(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if _, err := app.MemberStore.AddMember("Owner", "admin@example.com", "admin"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if _, err := app.MemberStore.SetProfileOwner("xcode-vnc", "admin@example.com"); err != nil {
		t.Fatalf("set profile owner: %v", err)
	}
	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:   "xcode-vnc",
		AppleEmail:    "user@example.com",
		HostID:        "h-1",
		HostCreatedAt: "2026-07-01T08:00:00Z",
		ReleaseDueAt:  "2026-07-02T08:00:00Z",
		OwnerEmail:    "admin@example.com",
		OwnerName:     "Owner",
		Status:        ReleaseReminderStatusActive,
	}); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/api/aws/status?profile=xcode-vnc", nil)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok, err := app.MemberStore.ProfileOwner("xcode-vnc"); err != nil {
		t.Fatalf("profile owner: %v", err)
	} else if ok {
		t.Fatalf("profile owner should be auto-cleared")
	}
	reminder, ok, err := app.MemberStore.ReleaseReminder("xcode-vnc")
	if err != nil || !ok {
		t.Fatalf("release reminder ok=%t err=%v", ok, err)
	}
	if reminder.Status != ReleaseReminderStatusReleased {
		t.Fatalf("reminder should be released: %+v", reminder)
	}
}

func TestAppWebCleanupRecordsAPI(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if _, err := app.MemberStore.AddMember("Owner", "admin@example.com", "admin"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if _, err := app.MemberStore.SetProfileOwner("xcode-vnc", "admin@example.com"); err != nil {
		t.Fatalf("set profile owner: %v", err)
	}
	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:   "xcode-vnc",
		AppleEmail:    "user@example.com",
		HostID:        "h-1",
		HostCreatedAt: "2026-07-01T08:00:00Z",
		ReleaseDueAt:  "2026-07-02T08:00:00Z",
		OwnerEmail:    "admin@example.com",
		OwnerName:     "Owner",
		Status:        ReleaseReminderStatusDueNotified,
	}); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	body := strings.NewReader(`{"profile":"xcode-vnc"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/release-reminder/cleanup", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok, err := app.MemberStore.ProfileOwner("xcode-vnc"); err != nil {
		t.Fatalf("profile owner: %v", err)
	} else if ok {
		t.Fatalf("profile owner should be cleared")
	}
	reminder, ok, err := app.MemberStore.ReleaseReminder("xcode-vnc")
	if err != nil || !ok {
		t.Fatalf("release reminder ok=%t err=%v", ok, err)
	}
	if reminder.Status != ReleaseReminderStatusReleased {
		t.Fatalf("reminder should be released: %+v", reminder)
	}
}

func TestAppWebAWSStatusWritesErrorLog(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return nil, errors.New("missing aws profile website")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/aws/status?profile=xcode-vnc", nil)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing aws profile website") {
		t.Fatalf("status body = %s", rec.Body.String())
	}
	files, err := app.LogManager.List()
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("log files = %+v", files)
	}
	data, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, want := range []string{"web.aws.status", "xcode-vnc", "missing aws profile website"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("log missing %q: %s", want, data)
		}
	}
}

func TestAppWebBackgroundDestroyDefersLifecycleMutation(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if _, err := app.MemberStore.AddMember("Owner", "admin@example.com", "admin"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if _, err := app.MemberStore.SetProfileOwner("xcode-vnc", "admin@example.com"); err != nil {
		t.Fatalf("set profile owner: %v", err)
	}
	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:   "xcode-vnc",
		AppleEmail:    "user@example.com",
		HostID:        "h-1",
		HostCreatedAt: "2026-07-01T08:00:00Z",
		ReleaseDueAt:  "2026-07-02T08:00:00Z",
		OwnerEmail:    "admin@example.com",
		OwnerName:     "Owner",
		Status:        ReleaseReminderStatusActive,
	}); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	body := strings.NewReader(`{"profile":"xcode-vnc","confirm":true,"background":true,"notify":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/aws/destroy", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	text := rec.Body.String()
	for _, want := range []string{"Started background AWS destroy job", "aws-destroy-xcode-vnc-20260701123045", "Elastic IP allocation will be retained"} {
		if !strings.Contains(text, want) {
			t.Fatalf("destroy response missing %q:\n%s", want, text)
		}
	}
	events, err := app.MemberStore.RecentEvents("user@example.com", 10)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	if len(events) != 2 ||
		events[0].Action != "aws.release.started" ||
		events[1].Action != "aws.release.requested" ||
		!events[0].Confirmed ||
		!events[1].Confirmed ||
		events[0].Profile != "xcode-vnc" ||
		events[1].Profile != "xcode-vnc" ||
		events[0].JobID == "" ||
		events[0].JobID != events[1].JobID ||
		events[0].RequestID == "" ||
		events[0].RequestID != events[1].RequestID {
		t.Fatalf("events = %+v", events)
	}
	if owner, ok, err := app.MemberStore.ProfileOwner("xcode-vnc"); err != nil {
		t.Fatalf("profile owner: %v", err)
	} else if !ok || owner.Owner.Email != "admin@example.com" {
		t.Fatalf("profile owner must remain until stopped: %+v ok=%t", owner, ok)
	}
	if reminder, ok, err := app.MemberStore.ReleaseReminder("xcode-vnc"); err != nil {
		t.Fatalf("release reminder: %v", err)
	} else if !ok || reminder.Status != ReleaseReminderStatusActive || reminder.OwnerEmail != "admin@example.com" {
		t.Fatalf("release reminder must remain until stopped: %+v ok=%t", reminder, ok)
	}
}

func TestAppWebForegroundDestroyKeepsImmediateLifecycleMutation(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if _, err := app.MemberStore.AddMember("Owner", "admin@example.com", "admin"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if _, err := app.MemberStore.SetProfileOwner("xcode-vnc", "admin@example.com"); err != nil {
		t.Fatalf("set profile owner: %v", err)
	}
	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:  "xcode-vnc",
		AppleEmail:   "user@example.com",
		ReleaseDueAt: time.Now().Add(12 * time.Hour).Format(time.RFC3339),
		OwnerEmail:   "admin@example.com",
		OwnerName:    "Owner",
		Status:       ReleaseReminderStatusActive,
	}); err != nil {
		t.Fatalf("seed release reminder: %v", err)
	}

	body := strings.NewReader(`{"profile":"xcode-vnc","confirm":true,"notify":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/aws/destroy", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok, err := app.MemberStore.ProfileOwner("xcode-vnc"); err != nil {
		t.Fatalf("profile owner: %v", err)
	} else if ok {
		t.Fatal("foreground confirmed destroy must clear owner immediately")
	}
	if reminder, ok, err := app.MemberStore.ReleaseReminder("xcode-vnc"); err != nil {
		t.Fatalf("release reminder: %v", err)
	} else if !ok || reminder.Status != ReleaseReminderStatusReleased {
		t.Fatalf("foreground confirmed destroy must release reminder immediately: %+v ok=%t", reminder, ok)
	}
}

func TestAppWebBackgroundOpenDefersLifecycleMutation(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	body := strings.NewReader(`{"profile":"xcode-vnc","confirm":true,"background":true,"notify":true,"owner_email":"admin@example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/aws/open", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	text := rec.Body.String()
	for _, want := range []string{"Started background AWS open job", "aws-open-xcode-vnc-20260701123045", "user@example.com"} {
		if !strings.Contains(text, want) {
			t.Fatalf("open response missing %q:\n%s", want, text)
		}
	}
	if _, ok, err := app.MemberStore.ProfileOwner("xcode-vnc"); err != nil {
		t.Fatalf("profile owner: %v", err)
	} else if ok {
		t.Fatalf("profile owner must remain unset until ready")
	}
	if _, ok, err := app.MemberStore.ReleaseReminder("xcode-vnc"); err != nil {
		t.Fatalf("release reminder: %v", err)
	} else if ok {
		t.Fatalf("release reminder must remain unset until ready")
	}
}

func TestAppWebBackgroundOpenDefersOperatorAssignment(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)

	body := strings.NewReader(`{"profile":"xcode-vnc","confirm":true,"background":true,"notify":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/aws/open", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("admin open without owner status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "owner_email is required") {
		t.Fatalf("admin open without owner body = %s", rec.Body.String())
	}

	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:  "xcode-vnc",
		AppleEmail:   "user@example.com",
		ReleaseDueAt: time.Now().Add(12 * time.Hour).Format(time.RFC3339),
		OwnerEmail:   "old-owner@example.com",
		OwnerName:    "Old Owner",
		Status:       ReleaseReminderStatusActive,
	}); err != nil {
		t.Fatalf("seed active reminder: %v", err)
	}

	operator, err := app.MemberStore.AddMemberWithPassword("Operator", "operator@example.com", "operator", "password123")
	if err != nil {
		t.Fatalf("add operator: %v", err)
	}
	body = strings.NewReader(`{"profile":"xcode-vnc","confirm":true,"background":true,"notify":true}`)
	req = httptest.NewRequest(http.MethodPost, "/api/aws/open", body)
	sessionRec := httptest.NewRecorder()
	if err := app.setWebSession(sessionRec, operator); err != nil {
		t.Fatalf("set operator session: %v", err)
	}
	cookies := sessionRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected operator auth cookie")
	}
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator open status = %d, body = %s", rec.Code, rec.Body.String())
	}
	owners, err := app.MemberStore.MembersForApple("user@example.com")
	if err != nil {
		t.Fatalf("members for apple: %v", err)
	}
	for _, owner := range owners {
		if owner.Email == "operator@example.com" {
			t.Fatalf("operator assignment must wait until ready: %+v", owners)
		}
	}
	if _, ok, err := app.MemberStore.ProfileOwner("xcode-vnc"); err != nil {
		t.Fatalf("profile owner: %v", err)
	} else if ok {
		t.Fatalf("profile owner must remain unset until ready")
	}
	if reminder, ok, err := app.MemberStore.ReleaseReminder("xcode-vnc"); err != nil {
		t.Fatalf("release reminder: %v", err)
	} else if !ok || reminder.OwnerEmail != "old-owner@example.com" || reminder.OwnerName != "Old Owner" || reminder.Status != ReleaseReminderStatusActive {
		t.Fatalf("release reminder must remain unchanged until ready: %+v ok=%v", reminder, ok)
	}
	jobs, err := app.JobManager.listRaw()
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].LifecycleState != JobLifecyclePending || jobs[0].LifecycleOwnerEmail != "operator@example.com" {
		t.Fatalf("operator lifecycle intent = %+v", jobs)
	}
}

func TestAppWebOpenReadyAllowsAdminWithoutOwner(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Instances: []InstanceStatus{{
				InstanceID:          "i-ready",
				State:               "running",
				SystemStatus:        "ok",
				InstanceStatusCheck: "ok",
				EBSStatus:           "ok",
				Tags:                managedTestTags(),
			}},
			ElasticIP: ElasticIP{InstanceID: "i-ready", PublicIP: "54.1.2.3", AllocationID: "eipalloc-1"},
		}}, nil
	}
	body := strings.NewReader(`{"profile":"xcode-vnc","confirm":true,"background":true,"notify":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/aws/open", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "owner_email is required") {
		t.Fatalf("ready open should not require owner: %s", rec.Body.String())
	}
	if _, ok, err := app.MemberStore.ProfileOwner("xcode-vnc"); err != nil {
		t.Fatalf("profile owner: %v", err)
	} else if ok {
		t.Fatalf("ready open without owner should not set profile owner")
	}
}

func TestAppWebReleaseReminderExtendAPI(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:   "xcode-vnc",
		AppleEmail:    "user@example.com",
		HostID:        "h-1",
		HostCreatedAt: "2026-07-01T08:00:00Z",
		ReleaseDueAt:  time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		OwnerEmail:    "admin@example.com",
		OwnerName:     "Test Admin",
		Status:        ReleaseReminderStatusActive,
	}); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	nextDue := time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339)
	body := strings.NewReader(`{"profile":"xcode-vnc","release_due_at":"` + nextDue + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/release-reminder/extend", body)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("extend status = %d, body = %s", rec.Code, rec.Body.String())
	}
	reminder, ok, err := app.MemberStore.ReleaseReminder("xcode-vnc")
	if err != nil || !ok {
		t.Fatalf("lookup reminder ok=%t err=%v", ok, err)
	}
	if reminder.ReleaseDueAt != nextDue || reminder.LastExtendedByEmail != "admin@example.com" || reminder.Status != ReleaseReminderStatusActive {
		t.Fatalf("reminder after extend = %+v", reminder)
	}
}

func TestAppReleaseReminderWorkerSendsDueNotificationOnce(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()
	t.Setenv(envWechatWebhookURL, server.URL)
	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:   "xcode-vnc",
		AppleEmail:    "user@example.com",
		HostID:        "h-1",
		HostCreatedAt: "2026-07-01T08:00:00Z",
		ReleaseDueAt:  "2026-07-01T09:00:00Z",
		OwnerEmail:    "admin@example.com",
		OwnerName:     "Test Admin",
		Status:        ReleaseReminderStatusActive,
	}); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	app.sendDueReleaseReminders(now)
	app.sendDueReleaseReminders(now.Add(time.Minute))
	if calls != 1 {
		t.Fatalf("webhook calls = %d, want 1", calls)
	}
	reminder, ok, err := app.MemberStore.ReleaseReminder("xcode-vnc")
	if err != nil || !ok {
		t.Fatalf("lookup reminder ok=%t err=%v", ok, err)
	}
	if reminder.Status != ReleaseReminderStatusDueNotified || reminder.LastNotifiedAt != now.Format(time.RFC3339) {
		t.Fatalf("reminder after worker = %+v", reminder)
	}
}

func TestAppWebTerminalCheckRequiresReadyAWSMac(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/check?profile=xcode-vnc", nil)
	addWebAuth(t, &app, req, "operator")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "aws mac is not ready") || !strings.Contains(rec.Body.String(), "no managed instance found") {
		t.Fatalf("terminal check body = %s", rec.Body.String())
	}
}

func TestAppWebTerminalCheckReady(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Instances: []InstanceStatus{{
				InstanceID:          "i-ready",
				State:               "running",
				SystemStatus:        "ok",
				InstanceStatusCheck: "ok",
				EBSStatus:           "ok",
				Tags:                managedTestTags(),
			}},
			ElasticIP: ElasticIP{InstanceID: "i-ready", PublicIP: "54.1.2.3", AllocationID: "eipalloc-1"},
		}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/check?profile=xcode-vnc", nil)
	addWebAuth(t, &app, req, "operator")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"ok":true`, `"profile":"xcode-vnc"`, `"target":"user@mac-host.example.com"`, `"ready":true`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("terminal check missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestAppWebTerminalAndSyncBlockNonCurrentHostKeys(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      string
		body      string
		configure func(*fakeRunner)
		wantCode  string
	}{
		{name: "terminal stale", method: http.MethodGet, path: "/api/terminal/check?profile=xcode-vnc",
			configure: func(r *fakeRunner) { r.knownHost = "mac-host.example.com ssh-ed25519 AAAAOLD\n" }, wantCode: "host_key_changed"},
		{name: "sync missing", method: http.MethodPost, path: "/api/sync/pull",
			body:      `{"profile":"xcode-vnc","remote_path":"~/Downloads/","local_path":"."}`,
			configure: func(r *fakeRunner) { r.missingHostKey = true }, wantCode: "host_key_missing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			key := writeSSHKey(t, 0o600)
			config := writeConfig(t, dir, key)
			var out, errOut bytes.Buffer
			runner := &fakeRunner{}
			tc.configure(runner)
			app := testApp(&out, &errOut, dir)
			app.Runner = runner
			app.AWSService.NewClient = func(context.Context, MacPlan) (AWSClient, error) {
				return &fakeAWSClient{status: AWSStatus{
					Instances: []InstanceStatus{{InstanceID: "i-ready", State: "running", SystemStatus: "ok", InstanceStatusCheck: "ok", EBSStatus: "ok", Tags: managedTestTags()}},
					ElasticIP: ElasticIP{InstanceID: "i-ready", PublicIP: "54.1.2.3", AllocationID: "eipalloc-1"},
				}}, nil
			}
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			addWebAuth(t, &app, req, "operator")
			rec := httptest.NewRecorder()
			app.newWebHandler(config).ServeHTTP(rec, req)
			if !strings.Contains(rec.Body.String(), "host key") {
				t.Fatalf("response=%s", rec.Body.String())
			}
			if len(runner.rsync) != 0 || len(runner.foreground) != 0 || runner.forgotHost != "" {
				t.Fatalf("blocked web request invoked command or mutation: runner=%+v", runner)
			}
			entries := readTestLogEntries(t, app.LogManager)
			var blocked []LogEntry
			for _, entry := range entries {
				if entry.Action == "host-key.blocked" {
					blocked = append(blocked, entry)
				}
			}
			if len(blocked) != 1 || blocked[0].ErrorCode != tc.wantCode || blocked[0].RequestID == "" {
				t.Fatalf("blocked entries=%+v all=%+v", blocked, entries)
			}
		})
	}
}

func TestAppWebSyncPushRequiresReadyAndSavesHistory(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	localFile := filepath.Join(dir, "build.zip")
	writeFile(t, localFile, "zip")
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Instances: []InstanceStatus{{
				InstanceID:          "i-ready",
				State:               "running",
				SystemStatus:        "ok",
				InstanceStatusCheck: "ok",
				EBSStatus:           "ok",
				Tags:                managedTestTags(),
			}},
			ElasticIP: ElasticIP{InstanceID: "i-ready", PublicIP: "54.1.2.3", AllocationID: "eipalloc-1"},
		}}, nil
	}
	body := strings.NewReader(fmt.Sprintf(`{"profile":"xcode-vnc","local_path":%q,"remote_path":"~/Downloads/"}`, localFile))
	req := httptest.NewRequest(http.MethodPost, "/api/sync/push", body)
	addWebAuth(t, &app, req, "operator")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Push:") || !strings.Contains(rec.Body.String(), localFile) {
		t.Fatalf("push body = %s", rec.Body.String())
	}
	if !containsString(runner.rsync, localFile) || !containsString(runner.rsync, "user@mac-host.example.com:~/Downloads/") {
		t.Fatalf("rsync args = %#v", runner.rsync)
	}
	items, err := app.SyncHistory.List("xcode-vnc", 10)
	if err != nil {
		t.Fatalf("history list: %v", err)
	}
	if len(items) != 1 || items[0].Direction != "push" || items[0].LocalPath != localFile || items[0].RemotePath != "~/Downloads/" {
		t.Fatalf("history items = %+v", items)
	}
}

func TestAppWebSyncPullBlocksWhenNotReady(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{}}, nil
	}
	body := strings.NewReader(`{"profile":"xcode-vnc","remote_path":"~/Downloads/","local_path":"."}`)
	req := httptest.NewRequest(http.MethodPost, "/api/sync/pull", body)
	addWebAuth(t, &app, req, "operator")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "aws mac is not ready") {
		t.Fatalf("pull body = %s", rec.Body.String())
	}
	if len(runner.rsync) != 0 {
		t.Fatalf("rsync should not run: %#v", runner.rsync)
	}
}

func TestAppWebLocalListRoots(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	req := httptest.NewRequest(http.MethodGet, "/api/local/list", nil)
	addWebAuth(t, &app, req, "operator")
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"entries"`) || !strings.Contains(rec.Body.String(), `"path":""`) {
		t.Fatalf("local roots body = %s", rec.Body.String())
	}
}

func TestAppWebLocalListRejectsHiddenPath(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	hiddenPath := filepath.Join(mustGetwd(t), ".git")
	req := httptest.NewRequest(http.MethodGet, "/api/local/list?path="+url.QueryEscape(hiddenPath), nil)
	addWebAuth(t, &app, req, "operator")
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside allowed local directories") {
		t.Fatalf("hidden path body = %s", rec.Body.String())
	}
}

func TestAppWebJobsAPI(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if _, err := app.JobManager.Create(Job{Type: "aws-destroy", Profile: "xcode-vnc", Status: JobStatusSuccess}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "aws-destroy") || !strings.Contains(rec.Body.String(), "success") {
		t.Fatalf("jobs body = %s", rec.Body.String())
	}
}

func TestAppWebJobsAPIFiltersMemberProfiles(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	for _, name := range []string{"assigned-usw2", "private-usw2"} {
		if _, err := app.MemberStore.UpsertManagedProfile(Profile{Name: name}); err != nil {
			t.Fatalf("upsert managed profile %s: %v", name, err)
		}
	}
	if _, err := app.JobManager.Create(Job{Type: "aws-open", Profile: "assigned-usw2", Status: JobStatusRunning}); err != nil {
		t.Fatalf("create assigned job: %v", err)
	}
	if _, err := app.JobManager.Create(Job{Type: "aws-open", Profile: "private-usw2", Status: JobStatusRunning}); err != nil {
		t.Fatalf("create private job: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	addWebAuth(t, &app, req, "viewer")
	if _, err := app.MemberStore.AssignProfileAccess("assigned-usw2", "admin@example.com"); err != nil {
		t.Fatalf("assign profile access: %v", err)
	}
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "assigned-usw2") {
		t.Fatalf("assigned job missing: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "private-usw2") {
		t.Fatalf("unassigned job leaked: %s", rec.Body.String())
	}
}

func TestAppWebJobsAPIRemoteUserModeUsesRemoteProfileAccess(t *testing.T) {
	dir := t.TempDir()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer remote-token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		writeWebJSON(w, webAPIResponse{
			OK: true,
			Data: map[string]interface{}{
				"profiles": []webManagedProfile{{Name: "assigned-usw2", Enabled: true}},
			},
		})
	}))
	defer remote.Close()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  user_api: "+remote.URL+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.RemoteUserAPI = true
	for _, name := range []string{"assigned-usw2", "private-usw2"} {
		if _, err := app.JobManager.Create(Job{Type: "aws-open", Profile: name, Status: JobStatusRunning}); err != nil {
			t.Fatalf("create %s job: %v", name, err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer remote-token")
	rec := httptest.NewRecorder()
	app.newWebHandler(configPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "assigned-usw2") {
		t.Fatalf("assigned remote job missing: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "private-usw2") {
		t.Fatalf("unassigned remote job leaked: %s", rec.Body.String())
	}
}

func TestAppWebStartupReconcilesInterruptedJob(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.JobManager.IsRunning = func(int) bool { return false }
	job, err := app.JobManager.Create(Job{ID: "stale-web-job", Status: JobStatusRunning, PID: 424242, Command: []string{"must-not-run"}})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	listeners := injectEphemeralWebListener(&app)
	ctx, cancel := context.WithCancel(context.Background())
	codes := make(chan int, 1)
	go func() { codes <- app.runWeb(ctx, DefaultConfigPath, []string{"--open"}) }()
	listener := <-listeners
	waitForTCP(t, listener.Addr().String())
	cancel()
	if code := <-codes; code != 0 {
		t.Fatalf("runWeb code = %d, err = %s", code, errOut.String())
	}
	got, err := app.JobManager.Load(job.ID)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if got.Status != JobStatusInterrupted {
		t.Fatalf("status = %q, want interrupted", got.Status)
	}
	if !strings.Contains(out.String(), "Reconciled background job stale-web-job: interrupted") {
		t.Fatalf("output = %q", out.String())
	}
	if runner := app.Runner.(*fakeRunner); len(runner.foreground) != 0 || len(runner.background) != 0 {
		t.Fatalf("stored command was executed: %#v %#v", runner.foreground, runner.background)
	}
	if got := app.Runner.(*fakeRunner).openedURL; got != "http://"+listener.Addr().String() {
		t.Fatalf("opened URL = %q, want actual listener address", got)
	}
}

func TestAppWebStartupReconcileFailureStopsBeforeListening(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	invalid := filepath.Join(dir, "not-a-directory")
	writeFile(t, invalid, "invalid")
	app.JobManager.Dir = invalid
	listenCalled := false
	app.Listen = func(string, string) (net.Listener, error) {
		listenCalled = true
		return nil, errors.New("listen should not be called")
	}
	if code := app.runWeb(context.Background(), DefaultConfigPath, nil); code != 1 {
		t.Fatalf("runWeb code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "job reconciliation failed:") {
		t.Fatalf("error = %q", errOut.String())
	}
	if listenCalled {
		t.Fatal("listener initialized after reconciliation failure")
	}
}

func TestAppWebBindFailurePreservesDrain(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if err := app.JobManager.BeginDrain(); err != nil {
		t.Fatalf("begin drain: %v", err)
	}
	app.Listen = func(string, string) (net.Listener, error) { return nil, errors.New("address already in use") }
	if code := app.runWeb(context.Background(), DefaultConfigPath, nil); code != 1 {
		t.Fatalf("runWeb code = %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(app.JobManager.Dir, ".draining")); err != nil {
		t.Fatalf("drain marker should remain: %v", err)
	}
}

func TestAppWebSuccessfulBindEndsDrain(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if err := app.JobManager.BeginDrain(); err != nil {
		t.Fatalf("begin drain: %v", err)
	}
	listeners := injectEphemeralWebListener(&app)
	ctx, cancel := context.WithCancel(context.Background())
	codes := make(chan int, 1)
	go func() { codes <- app.runWeb(ctx, DefaultConfigPath, nil) }()
	listener := <-listeners
	waitForTCP(t, listener.Addr().String())
	if _, err := os.Stat(filepath.Join(app.JobManager.Dir, ".draining")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drain marker still exists: %v", err)
	}
	cancel()
	if code := <-codes; code != 0 {
		t.Fatalf("runWeb code = %d, err = %s", code, errOut.String())
	}
}

func TestAppWebCancelWaitsForServeAndReminderWorker(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	listeners := injectEphemeralWebListener(&app)
	workerStarted := make(chan struct{})
	workerDone := make(chan struct{})
	app.WebReminderWorker = func(ctx context.Context) {
		close(workerStarted)
		<-ctx.Done()
		close(workerDone)
	}
	ctx, cancel := context.WithCancel(context.Background())
	codes := make(chan int, 1)
	go func() { codes <- app.runWeb(ctx, DefaultConfigPath, nil) }()
	listener := <-listeners
	waitForTCP(t, listener.Addr().String())
	<-workerStarted
	cancel()
	if code := <-codes; code != 0 {
		t.Fatalf("runWeb code = %d, err = %s", code, errOut.String())
	}
	select {
	case <-workerDone:
	default:
		t.Fatal("runWeb returned before reminder worker stopped")
	}
	if _, err := net.DialTimeout("tcp", listener.Addr().String(), 25*time.Millisecond); err == nil {
		t.Fatal("listener still accepts connections after runWeb returned")
	}
}

func TestAppWebLifecycleWorkerScansImmediatelyOnTickAndStops(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	lifecycleTicks := make(chan time.Time)
	reminderTicks := make(chan time.Time)
	scans := make(chan string, 3)
	app.WebAWSLifecycleScan = func(_ context.Context, configPath string) error {
		scans <- configPath
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.runWebBackgroundWorker(ctx, "test-config.yaml", lifecycleTicks, reminderTicks)
	}()

	if got := <-scans; got != "test-config.yaml" {
		t.Fatalf("immediate scan config = %q", got)
	}
	lifecycleTicks <- time.Now()
	if got := <-scans; got != "test-config.yaml" {
		t.Fatalf("tick scan config = %q", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lifecycle worker did not stop after cancellation")
	}
}

func TestAppWebShutdownTimeoutForcesClose(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	listeners := injectEphemeralWebListener(&app)
	requestStarted := make(chan struct{})
	unblockRequest := make(chan struct{})
	app.WebHandler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(requestStarted)
		<-unblockRequest
	})
	app.WebShutdownTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	codes := make(chan int, 1)
	go func() { codes <- app.runWeb(ctx, DefaultConfigPath, nil) }()
	listener := <-listeners
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		_, _ = http.Get("http://" + listener.Addr().String())
	}()
	<-requestStarted
	started := time.Now()
	cancel()
	if code := <-codes; code != 0 {
		t.Fatalf("runWeb code = %d, err = %s", code, errOut.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded shutdown took %s", elapsed)
	}
	close(unblockRequest)
	<-requestDone
}

func TestAppWebServeErrorShutsDownAcceptedConnections(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	serverConn, clientConn := net.Pipe()
	serveFailure := errors.New("injected serve failure")
	requestStarted := make(chan struct{})
	unblockRequest := make(chan struct{})
	listener := &singleConnErrorListener{
		conn:       serverConn,
		afterFirst: requestStarted,
		err:        serveFailure,
	}
	app.Listen = func(string, string) (net.Listener, error) { return listener, nil }
	app.WebHandler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(requestStarted)
		<-unblockRequest
	})
	app.WebShutdownTimeout = 20 * time.Millisecond
	clientDone := make(chan error, 1)
	go func() {
		if _, err := io.WriteString(clientConn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
			clientDone <- err
			return
		}
		_, err := io.ReadAll(clientConn)
		clientDone <- err
	}()
	if code := app.runWeb(context.Background(), DefaultConfigPath, nil); code != 1 {
		t.Fatalf("runWeb code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), serveFailure.Error()) {
		t.Fatalf("error = %q, want original Serve error", errOut.String())
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("accepted connection remained open after Serve failure")
	}
	close(unblockRequest)
	clientConn.Close()
}

func TestAppWebWorkerShutdownTimeoutWarnsAndReturns(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	listeners := injectEphemeralWebListener(&app)
	workerStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	workerExited := make(chan struct{})
	app.WebReminderWorker = func(context.Context) {
		close(workerStarted)
		<-releaseWorker
		close(workerExited)
	}
	app.WebWorkerShutdownTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	codes := make(chan int, 1)
	go func() { codes <- app.runWeb(ctx, DefaultConfigPath, nil) }()
	listener := <-listeners
	waitForTCP(t, listener.Addr().String())
	<-workerStarted
	started := time.Now()
	cancel()
	if code := <-codes; code != 0 {
		t.Fatalf("runWeb code = %d, err = %s", code, errOut.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("worker shutdown timeout took %s", elapsed)
	}
	if !strings.Contains(errOut.String(), "warning: reminder worker did not stop") {
		t.Fatalf("error = %q, want worker shutdown warning", errOut.String())
	}
	close(releaseWorker)
	select {
	case <-workerExited:
	case <-time.After(time.Second):
		t.Fatal("released reminder worker did not exit")
	}
}

type singleConnErrorListener struct {
	conn       net.Conn
	afterFirst <-chan struct{}
	err        error
	accepted   bool
}

func (l *singleConnErrorListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.afterFirst
	return nil, l.err
}

func (l *singleConnErrorListener) Close() error   { return nil }
func (l *singleConnErrorListener) Addr() net.Addr { return testNetAddr("injected:0") }

type testNetAddr string

func (a testNetAddr) Network() string { return "tcp" }
func (a testNetAddr) String() string  { return string(a) }

func injectEphemeralWebListener(app *App) <-chan net.Listener {
	listeners := make(chan net.Listener, 1)
	app.Listen = func(string, string) (net.Listener, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			listeners <- listener
		}
		return listener, err
	}
	return listeners
}

func waitForTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 25*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", addr)
}

func TestAppWebJobsAPIReconcilesInterruptedJob(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.JobManager.IsRunning = func(int) bool { return false }
	job, err := app.JobManager.Create(Job{ID: "stale-api-job", Status: JobStatusRunning, PID: 989898})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"`+job.ID+`"`) || !strings.Contains(rec.Body.String(), `"status":"interrupted"`) {
		t.Fatalf("jobs body = %s", rec.Body.String())
	}
}

func TestWebJobBadgesIncludeStartingAndInterruptedLabels(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`starting: { label: "启动中", cls: "wait" }`,
		`interrupted: { label: "已中断", cls: "blocked" }`,
		`function lifecycleJobPollingNeeded(job)`,
		`job?.lifecycle_state === "pending" || job?.lifecycle_state === "waiting"`,
		`async function refreshLifecycleProfiles(jobs)`,
		`await Promise.all(names.map((name) => refreshStatus(name, false)))`,
		`await loadProfiles({ timeoutMs: 0 })`,
		`const lifecycleJobs = state.jobs.filter(lifecycleJobPollingNeeded)`,
		`await refreshLifecycleProfiles(lifecycleJobs)`,
		`"等待释放完成" : "等待 Mac ready"`,
		`后台任务运行中`,
		`已完成`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web index missing %q", want)
		}
	}
	if strings.Contains(extractWebSource(t, html, "async function refreshLifecycleProfiles(jobs)", "async function loadJobs(options = {})"), "window.location.reload") {
		t.Fatal("lifecycle refresh must not reload the page")
	}
}

func extractWebSource(t *testing.T, html, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(html, startMarker)
	if start < 0 {
		t.Fatalf("web source start marker is missing: %q", startMarker)
	}
	end := strings.Index(html[start:], endMarker)
	if end < 0 {
		t.Fatalf("web source end marker %q is missing after %q", endMarker, startMarker)
	}
	return html[start : start+end]
}

func TestAppWebVNCStartTunnelContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	fn := extractWebSource(t, html, "async function startTunnel(profile)", "\n    async function openSync(profile)")

	startCall := `const start = await localAgentAPI("/start", {`
	openCall := `const opened = await localAgentAPI("/open-vnc", {`
	startIndex := strings.Index(fn, startCall)
	openIndex := strings.Index(fn, openCall)
	if startIndex < 0 {
		t.Fatalf("startTunnel must always POST /start; function = %s", fn)
	}
	if openIndex < 0 {
		t.Fatalf("startTunnel must POST /open-vnc after /start succeeds; function = %s", fn)
	}
	if startIndex >= openIndex {
		t.Fatalf("/open-vnc must follow successful /start; function = %s", fn)
	}
	for _, call := range []string{startCall, openCall} {
		callIndex := strings.Index(fn, call)
		callEnd := strings.Index(fn[callIndex:], "\n        });")
		if callEnd < 0 {
			t.Fatalf("%s call boundary is missing; function = %s", call, fn)
		}
		callBlock := fn[callIndex : callIndex+callEnd]
		if !strings.Contains(callBlock, `method: "POST"`) {
			t.Fatalf("%s must use POST; function = %s", call, fn)
		}
	}
}

func TestAppWebTransferStartContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	fn := extractWebSource(t, html, "async function runSync(direction)", "\n    function terminalSetStatus(text)")

	recordStart := `await api("/api/transfer-record/start", {`
	localStart := `await localAgentAPI("/sync/" + direction, {`
	saveJob := `const savedJob = storeSyncJob(key, job);`
	pollJob := `scheduleSyncPoll(savedJob);`
	bind := `await updateTransferRecord(record.id, savedJob, true)`
	reconcileRetry := `loadSyncJobs(p.name);`
	for _, want := range []string{
		recordStart,
		localStart,
		saveJob,
		pollJob,
		bind,
		reconcileRetry,
		`payload.transfer_id = record.id;`,
		`body: JSON.stringify({ profile: p.name, direction, local_path: payload.local_path, remote_path: payload.remote_path })`,
		`if (record?.id && !job) {`,
	} {
		if !strings.Contains(fn, want) {
			t.Fatalf("runSync missing %q; function = %s", want, fn)
		}
	}
	if strings.Index(fn, recordStart) >= strings.Index(fn, localStart) {
		t.Fatalf("server transfer record must be created before local sync starts; function = %s", fn)
	}
	if strings.Index(fn, localStart) >= strings.Index(fn, saveJob) {
		t.Fatalf("valid local job must be saved after local start; function = %s", fn)
	}
	if strings.Index(fn, saveJob) >= strings.Index(fn, bind) || strings.Index(fn, pollJob) >= strings.Index(fn, bind) {
		t.Fatalf("valid local job must be saved and made recoverable before the first bind update; function = %s", fn)
	}
	bindWindow := fn[strings.Index(fn, bind):]
	bindCatch := strings.Index(bindWindow, `.catch((err) => {`)
	failCatch := strings.Index(bindWindow, `} catch (err) {`)
	if bindCatch < 0 || failCatch < 0 || bindCatch >= failCatch {
		t.Fatalf("first bind update must handle failure locally before the outer startup catch; function = %s", fn)
	}
	bindCatchWindow := bindWindow[bindCatch:failCatch]
	if !strings.Contains(bindCatchWindow, reconcileRetry) {
		t.Fatalf("first bind failure must explicitly reload and reconcile jobs even when the local job is already terminal; function = %s", fn)
	}
	for _, forbidden := range []string{"member_id", "member_email", "/api/sync/history"} {
		if strings.Contains(fn, forbidden) {
			t.Fatalf("runSync must not contain %q; function = %s", forbidden, fn)
		}
	}
}

func TestAppWebTransferReconcileContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	fn := extractWebSource(t, html, "async function reconcileTransferRecords(profile, jobs)", "\n    function useSyncHistory(id)")

	localMatch := `byID.get(record.local_job_id)`
	transferMatch := `job.transfer_id === record.id`
	clearMissing := `clearTransferMissingConfirmation(record.id);`
	bind := `await updateTransferRecord(record.id, job, true);`
	poll := `if (syncJobActive(job)) scheduleSyncPoll(job);`
	confirmMissing := `confirmMissingTransferRecord(profile, record)`
	for _, want := range []string{localMatch, transferMatch, clearMissing, bind, poll, confirmMissing} {
		if !strings.Contains(fn, want) {
			t.Fatalf("reconcileTransferRecords missing %q; function = %s", want, fn)
		}
	}
	if strings.Index(fn, transferMatch) >= strings.Index(fn, clearMissing) ||
		strings.Index(fn, clearMissing) >= strings.Index(fn, bind) ||
		strings.Index(fn, bind) >= strings.Index(fn, poll) {
		t.Fatalf("transfer_id recovery must clear missing confirmation and bind local_job_id before polling continues; function = %s", fn)
	}
	if strings.Contains(fn, `status: "unconfirmed"`) {
		t.Fatalf("a single successful jobs list must not immediately mark a transfer unconfirmed; function = %s", fn)
	}
}

func TestAppWebTransferMissingGraceContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	confirm := extractWebSource(t, html, "function confirmMissingTransferRecord(profile, record)", "\n    async function reconcileTransferRecords(profile, jobs)")
	clearMissing := extractWebSource(t, html, "function clearTransferMissingConfirmation(transferID)", "\n    function confirmMissingTransferRecord(profile, record)")
	reload := extractWebSource(t, html, "function scheduleTransferMissingReload(profile, transferID, missing)", "\n    function confirmMissingTransferRecord(profile, record)")

	for _, want := range []string{
		`const transferMissingGraceMS = 10000;`,
		`const transferMissingMaxReloads = 4;`,
		`successfulLoads: 0`,
		`missing.successfulLoads += 1;`,
		`Date.now() - missing.startedAt >= transferMissingGraceMS`,
		`missing.successfulLoads >= 2`,
		`state.localAgent.online`,
		`missing.reloads < transferMissingMaxReloads`,
		`const loaded = await loadSyncJobs(profile);`,
		`if (!loaded && state.transferMissingConfirmations[transferID] === missing)`,
		`scheduleTransferMissingReload(profile, transferID, missing);`,
		`status: "unconfirmed"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing-transfer grace contract missing %q", want)
		}
	}
	if strings.Index(confirm, `missing.successfulLoads += 1;`) >= strings.Index(confirm, `status: "unconfirmed"`) {
		t.Fatalf("successful jobs-list observations must be counted before unconfirmed is allowed; function = %s", confirm)
	}
	if strings.Contains(reload, `successfulLoads += 1`) {
		t.Fatalf("failed jobs-list reloads must not count as successful observations; function = %s", reload)
	}
	if strings.Index(reload, `missing.reloads += 1;`) >= strings.Index(reload, `const loaded = await loadSyncJobs(profile);`) {
		t.Fatalf("each bounded reload attempt must be counted before loading; function = %s", reload)
	}
	if !strings.Contains(clearMissing, `window.clearTimeout(missing.timerID)`) ||
		!strings.Contains(clearMissing, `delete state.transferMissingConfirmations[transferID]`) {
		t.Fatalf("matched transfers must clear missing state and timer; function = %s", clearMissing)
	}
}

func TestAppWebTransferNavigationCleanupContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	cleanup := extractWebSource(t, html, "function clearSyncProfileState(profile)", "\n    async function loadSyncJobs(profile)")
	showView := extractWebSource(t, html, "function showView(viewID, options = {})", "\n    function updateBrowserHistory(viewID, mode)")
	openSync := extractWebSource(t, html, "async function openSync(profile)", "\n    async function restoreSyncProfile(profile)")
	restoreHistory := extractWebSource(t, html, "function restoreBrowserHistory(event)", "\n    async function loadChallenge()")

	for _, want := range []string{
		`state.syncViewGeneration += 1;`,
		`state.syncHistoryGenerations[profile] = (state.syncHistoryGenerations[profile] || 0) + 1;`,
		`state.syncJobsGenerations[profile] = (state.syncJobsGenerations[profile] || 0) + 1;`,
		`cancelSyncJobsRequest(profile);`,
		`clearSyncPollTimer(key);`,
		`cancelSyncPollRequest(key);`,
		`delete state.syncJobs[key];`,
		`delete state.syncLoadedJobs[profile];`,
		`Object.entries(state.transferMissingConfirmations)`,
		`clearTransferMissingConfirmation(transferID);`,
	} {
		if !strings.Contains(cleanup, want) {
			t.Fatalf("sync profile cleanup missing %q; function = %s", want, cleanup)
		}
	}
	if !strings.Contains(showView, `state.view === "syncView" && viewID !== "syncView"`) ||
		!strings.Contains(showView, `clearSyncProfileState(state.selected);`) {
		t.Fatalf("leaving syncView must clear profile transfer state; function = %s", showView)
	}
	for name, source := range map[string]string{"openSync": openSync, "restoreBrowserHistory": restoreHistory} {
		if !strings.Contains(source, `clearSyncProfileState(state.selected);`) {
			t.Fatalf("%s must clear the old profile before switching sync history; function = %s", name, source)
		}
	}
	reconcile := extractWebSource(t, html, "async function reconcileTransferRecords(profile, jobs)", "\n    function useSyncHistory(id)")
	poll := extractWebSource(t, html, "async function pollSyncJob(", "\n    function cancelSyncJobsRequest")
	loadJobs := extractWebSource(t, html, "async function loadSyncJobs(profile)", "\n    async function deleteSyncHistory(id)")
	if !strings.Contains(reconcile, `state.view !== "syncView" || state.selected !== profile`) {
		t.Fatalf("reconcile must stop writing after its sync profile becomes stale; function = %s", reconcile)
	}
	if !strings.Contains(poll, `state.syncPollRequests[key] !== request || state.view !== "syncView" || state.selected !== profile`) {
		t.Fatalf("poll completion must stop writing after cancellation or navigation; function = %s", poll)
	}
	if strings.Count(loadJobs, `state.syncJobsGenerations[profile] !== generation || state.syncJobsRequests[profile] !== request`) < 2 {
		t.Fatalf("jobs loading must recheck cancellation after reconcile; function = %s", loadJobs)
	}
}

func TestAppWebTransferMemberJobFilteringContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	loadJobs := extractWebSource(t, html, "async function loadSyncJobs(profile)", "\n    async function deleteSyncHistory(id)")
	reconcile := extractWebSource(t, html, "async function reconcileTransferRecords(profile, jobs)", "\n    function useSyncHistory(id)")
	loadHistory := extractWebSource(t, html, "async function loadSyncHistory(", "\n    function renderSyncHistoryMessage")

	if !strings.Contains(loadJobs, `state.syncLoadedJobs[profile] = jobs;`) ||
		!strings.Contains(loadJobs, `await reconcileTransferRecords(profile, jobs);`) {
		t.Fatalf("loadSyncJobs must retain the raw global list and defer member filtering to reconcile; function = %s", loadJobs)
	}
	for _, forbidden := range []string{`latestSyncJob(jobs`, `storeSyncJob(`, `scheduleSyncPoll(`} {
		if strings.Contains(loadJobs, forbidden) {
			t.Fatalf("loadSyncJobs must not expose or poll unfiltered global jobs via %q; function = %s", forbidden, loadJobs)
		}
	}
	for _, want := range []string{
		`record.local_job_id === job.id`,
		`Boolean(job.transfer_id) && records.some((record) =>`,
		`record.id === job.transfer_id`,
		`const matchedJobs = (jobs || []).filter`,
		`latestSyncJob(matchedJobs, direction)`,
	} {
		if !strings.Contains(reconcile, want) {
			t.Fatalf("reconcile must filter jobs through current member records using %q; function = %s", want, reconcile)
		}
	}
	if !strings.Contains(loadHistory, `state.syncLoadedJobs[p.name]`) ||
		!strings.Contains(loadHistory, `await reconcileTransferRecords(p.name, state.syncLoadedJobs[p.name]);`) {
		t.Fatalf("history/jobs concurrent loading must reconcile the retained raw list after history arrives; function = %s", loadHistory)
	}
}

func TestAppWebTransferHistoryContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	history := extractWebSource(t, html, "async function loadSyncHistory(", "\n    function renderSyncHistoryMessage")
	polling := extractWebSource(t, html, "async function pollSyncJob(", "\n    function cancelSyncJobsRequest")
	deletion := extractWebSource(t, html, "async function deleteSyncHistory(", "\n    async function runSync(direction)")

	for _, want := range []string{
		`api("/api/transfer-records?profile="`,
		`body.data.records || []`,
		`reconcileTransferRecords`,
		`local_job_id`,
		`"unconfirmed"`,
		`updateTransferRecord`,
		`transferMilestones`,
		`"/api/transfer-record/delete"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web transfer flow missing %q", want)
		}
	}
	if !strings.Contains(polling, "await updateTransferRecord") {
		t.Fatalf("polling must report transfer milestones and terminal states; function = %s", polling)
	}
	if !strings.Contains(deletion, `api("/api/transfer-record/delete", {`) {
		t.Fatalf("history deletion must use member transfer records; function = %s", deletion)
	}
	for _, source := range []string{history, polling, deletion} {
		for _, forbidden := range []string{"/api/sync/history", "member_id", "member_email"} {
			if strings.Contains(source, forbidden) {
				t.Fatalf("active transfer flow must not contain %q; source = %s", forbidden, source)
			}
		}
	}
	for _, label := range []string{"方向", "路径", "状态", "进度", "开始", "结束", "耗时", "错误", "使用", "删除"} {
		if !strings.Contains(html, label) {
			t.Fatalf("transfer history rendering missing label %q", label)
		}
	}
}

func TestAppWebVNCReadinessGating(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	renderWorkbench := extractWebSource(t, html, "function renderWorkbench(p, status, reminder)", "\n    function renderSelected()")
	if !strings.Contains(renderWorkbench, `applyWorkbenchAction("vncBtn", model.actions.vnc);`) {
		t.Fatalf("renderWorkbench must drive VNC availability from model.actions.vnc; function = %s", renderWorkbench)
	}

	startTunnel := extractWebSource(t, html, "async function startTunnel(profile)", "\n    async function openSync(profile)")
	for _, want := range []string{
		`if (!profileReady(profile)) {`,
		`setStatus("Mac 未 ready，不能打开 VNC。");`,
		`return;`,
	} {
		if !strings.Contains(startTunnel, want) {
			t.Fatalf("startTunnel readiness guard is missing %q; function = %s", want, startTunnel)
		}
	}
	readinessGuard := strings.Index(startTunnel, `if (!profileReady(profile)) {`)
	agentGuard := strings.Index(startTunnel, `if (!state.localAgent.online) {`)
	if readinessGuard < 0 || agentGuard < 0 || readinessGuard > agentGuard {
		t.Fatalf("startTunnel must reject non-ready profiles before checking the local agent; function = %s", startTunnel)
	}
}

func TestAppWebVNCPortConflictIsNotSuppressedByReadiness(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	start := strings.Index(html, "async function startTunnel(profile)")
	if start < 0 {
		t.Fatal("startTunnel function is missing")
	}
	end := strings.Index(html[start:], "\n    async function openSync(profile)")
	if end < 0 {
		t.Fatal("startTunnel function boundary is missing")
	}
	fn := html[start : start+end]

	for _, forbidden := range []string{
		"isLocalPortInUseError",
		"profileVNCReady",
		"按已有连接继续打开 VNC",
	} {
		if strings.Contains(fn, forbidden) {
			t.Fatalf("startTunnel must not suppress /start errors using %q; function = %s", forbidden, fn)
		}
	}
	for _, definition := range []string{"function isLocalPortInUseError", "function profileVNCReady"} {
		if strings.Contains(html, definition) {
			t.Fatalf("unused VNC fallback definition must be absent globally: %q", definition)
		}
	}
}

func TestAppWebVNCStartTunnelBehavior(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	payloadFunctions := extractWebSource(t, html, "function selectedProfileYAML(profileName)", "\n    async function loadClientConfig()")
	fn := extractWebSource(t, html, "async function startTunnel(profile)", "\n    async function openSync(profile)")

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for embedded startTunnel behavior test: %v", err)
	}
	harness := `
import assert from "node:assert/strict";

const state = {
  busy: false,
  localAgent: { online: true },
  profiles: [],
  selected: "",
  terminalConnectedProfiles: new Set()
};
let scenario;
let calls;
let busyTransitions;
let statuses;
let outputs;
let views;
let renderProfileCount;
let renderSelectedCount;
let loadEventsCalls;

function reset(next) {
  scenario = next;
  state.profiles = [{
    name: next.profile,
    profile_yaml: next.profileYAML
  }];
  calls = [];
  busyTransitions = [];
  statuses = [];
  outputs = [];
  views = [];
  renderProfileCount = 0;
  renderSelectedCount = 0;
  loadEventsCalls = [];
  state.busy = false;
  state.selected = "";
  state.terminalConnectedProfiles.clear();
}

async function localAgentAPI(path, options) {
  calls.push([path, options.method, JSON.parse(options.body)]);
  if (path === "/host-key/check") return {data:{status:"current"}};
  if (path === "/start") {
    if (scenario.startError) throw scenario.startError;
    return scenario.start;
  }
  if (path === "/open-vnc") {
    if (scenario.openError) throw scenario.openError;
    return scenario.opened;
  }
  throw new Error("unexpected API path: " + path);
}
async function recordLocalIntent(profile, operation) {
  return {data:{request_id:"server-" + operation + "-request"}};
}
function setBusy(value, message) {
  busyTransitions.push([value, message]);
  state.busy = value;
}
function setStatus(value) { statuses.push(value); }
function setOutput(value) { outputs.push(value); }
function showOperationError(summary, error) {
  setStatus(summary);
  setOutput(String(error?.message || error || "未知错误"));
  return true;
}
function showView(value) { views.push(value); }
function renderProfiles() { renderProfileCount++; }
function renderSelected() { renderSelectedCount++; }
function profileReady() { return scenario.ready !== false; }
async function loadEvents(value) {
  loadEventsCalls.push(value);
  if (scenario.loadEventsError) throw scenario.loadEventsError;
}

` + payloadFunctions + "\n" + fn + `

reset({
  profile: "not-ready-profile",
  profileYAML: "name: not-ready-profile\napple_email: not-ready@example.com\n",
  ready: false
});
await startTunnel("not-ready-profile");
assert.deepEqual(calls, []);
assert.deepEqual(busyTransitions, []);
assert.deepEqual(views, []);
assert.equal(statuses.at(-1), "Mac 未 ready，不能打开 VNC。");

reset({
  profile: "reject-profile",
  profileYAML: "name: reject-profile\napple_email: reject@example.com\n",
  startError: new Error("start exploded")
});
await startTunnel("reject-profile");
assert.deepEqual(calls, [
  ["/host-key/check", "POST", {
    profile: "reject-profile",
    profile_yaml: "name: reject-profile\napple_email: reject@example.com\n"
  }],
  ["/start", "POST", {
    profile: "reject-profile",
    profile_yaml: "name: reject-profile\napple_email: reject@example.com\n"
  }]
]);
assert.deepEqual(busyTransitions, [
  [true, "正在打开 VNC..."],
  [false, undefined]
]);
assert.equal(outputs.at(-1), "start exploded");
assert.equal(statuses.at(-1), "VNC 打开失败");
assert.equal(calls.some(([path]) => path === "/open-vnc"), false);

reset({
  profile: "open-reject-profile",
  profileYAML: "name: open-reject-profile\napple_email: open-reject@example.com\n",
  start: { output: "tunnel started\n" },
  openError: new Error("open vnc exploded")
});
await startTunnel("open-reject-profile");
assert.deepEqual(calls, [
  ["/host-key/check", "POST", {
    profile: "open-reject-profile",
    profile_yaml: "name: open-reject-profile\napple_email: open-reject@example.com\n"
  }],
  ["/start", "POST", {
    profile: "open-reject-profile",
    profile_yaml: "name: open-reject-profile\napple_email: open-reject@example.com\n"
  }],
  ["/open-vnc", "POST", {
    profile: "open-reject-profile",
    profile_yaml: "name: open-reject-profile\napple_email: open-reject@example.com\n"
  }]
]);
assert.deepEqual(busyTransitions, [
  [true, "正在打开 VNC..."],
  [false, undefined]
]);
assert.equal(outputs.at(-1), "open vnc exploded");
assert.equal(statuses.at(-1), "VNC 打开失败");
assert.equal(state.terminalConnectedProfiles.has("open-reject-profile"), false);

reset({
  profile: "new-profile",
  profileYAML: "name: new-profile\napple_email: new@example.com\n",
  start: { output: "tunnel started\n" },
  opened: { output: "vnc opened\n" }
});
await startTunnel("new-profile");
assert.deepEqual(calls, [
  ["/host-key/check", "POST", {
    profile: "new-profile",
    profile_yaml: "name: new-profile\napple_email: new@example.com\n"
  }],
  ["/start", "POST", {
    profile: "new-profile",
    profile_yaml: "name: new-profile\napple_email: new@example.com\n"
  }],
  ["/open-vnc", "POST", {
    profile: "new-profile",
    profile_yaml: "name: new-profile\napple_email: new@example.com\n"
  }]
]);
assert.deepEqual(busyTransitions, [
  [true, "正在打开 VNC..."],
  [false, undefined]
]);
assert.equal(statuses.at(-1), "已启动 SSH 隧道并打开 VNC");
assert.deepEqual(loadEventsCalls, [false]);
assert.equal(state.terminalConnectedProfiles.has("new-profile"), true);

reset({
  profile: "reuse-profile",
  profileYAML: "name: reuse-profile\napple_email: reuse@example.com\n",
  start: { output: "tunnel already started; reused\n" },
  opened: { output: "vnc opened\n" }
});
await startTunnel("reuse-profile");
assert.deepEqual(calls, [
  ["/host-key/check", "POST", {
    profile: "reuse-profile",
    profile_yaml: "name: reuse-profile\napple_email: reuse@example.com\n"
  }],
  ["/start", "POST", {
    profile: "reuse-profile",
    profile_yaml: "name: reuse-profile\napple_email: reuse@example.com\n"
  }],
  ["/open-vnc", "POST", {
    profile: "reuse-profile",
    profile_yaml: "name: reuse-profile\napple_email: reuse@example.com\n"
  }]
]);
assert.deepEqual(busyTransitions, [
  [true, "正在打开 VNC..."],
  [false, undefined]
]);
assert.equal(statuses.at(-1), "已复用 SSH 隧道并打开新的 VNC 窗口");

reset({
  profile: "events-profile",
  profileYAML: "name: events-profile\napple_email: events@example.com\n",
  start: { output: "tunnel started\n" },
  opened: { output: "vnc opened\n" },
  loadEventsError: new Error("events unavailable")
});
await startTunnel("events-profile");
assert.deepEqual(calls, [
  ["/host-key/check", "POST", {
    profile: "events-profile",
    profile_yaml: "name: events-profile\napple_email: events@example.com\n"
  }],
  ["/start", "POST", {
    profile: "events-profile",
    profile_yaml: "name: events-profile\napple_email: events@example.com\n"
  }],
  ["/open-vnc", "POST", {
    profile: "events-profile",
    profile_yaml: "name: events-profile\napple_email: events@example.com\n"
  }]
]);
assert.deepEqual(busyTransitions, [
  [true, "正在打开 VNC..."],
  [false, undefined]
]);
assert.equal(statuses.at(-1), "已启动 SSH 隧道并打开 VNC");
assert.equal(statuses.includes("VNC 打开失败"), false);
`
	script := filepath.Join(t.TempDir(), "start_tunnel_test.mjs")
	if err := os.WriteFile(script, []byte(harness), 0o600); err != nil {
		t.Fatalf("write node harness: %v", err)
	}
	cmd := exec.Command(node, script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("startTunnel node behavior test failed: %v\n%s", err, output)
	}
}

func TestAppWebBeijingTimeContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`Intl.DateTimeFormat("zh-CN", {`,
		`timeZone: "Asia/Shanghai"`,
		`hourCycle: "h23"`,
		`formatToParts`,
		`function formatTime(value)`,
		`function toDateTimeLocal(value)`,
		`function beijingDateTimeLocalToISOString(value)`,
		`const dueAt = beijingDateTimeLocalToISOString(value);`,
		`release_due_at: dueAt`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web index missing %q", want)
		}
	}
}

func TestAppWebBeijingDateTimeBehavior(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	helpers := extractWebSource(t, string(data), "const beijingTimeFormatter", "\n    async function cleanupLocalRecords")

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for embedded Beijing datetime behavior test: %v", err)
	}
	harness := `
import assert from "node:assert/strict";

` + helpers + `

assert.equal(formatTime("2026-07-16T08:03:24Z"), "2026-07-16 16:03:24（北京时间）");
assert.equal(formatTime("2026-07-16T18:30:00Z"), "2026-07-17 02:30:00（北京时间）");
assert.equal(formatTime("not-a-date"), "not-a-date");
assert.equal(toDateTimeLocal("2026-07-17T08:00:00Z"), "2026-07-17T16:00");
assert.equal(beijingDateTimeLocalToISOString("2026-07-17T16:00"), "2026-07-17T08:00:00.000Z");
assert.equal(beijingDateTimeLocalToISOString("2026-02-29T12:00"), null);
assert.equal(beijingDateTimeLocalToISOString("2026-07-17 16:00"), null);
assert.equal(beijingDateTimeLocalToISOString("2026-07-17T16:00:00"), null);
`
	script := filepath.Join(t.TempDir(), "beijing_datetime_test.mjs")
	if err := os.WriteFile(script, []byte(harness), 0o600); err != nil {
		t.Fatalf("write node harness: %v", err)
	}
	output, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("Beijing datetime node behavior test failed: %v\n%s", err, output)
	}
}

func TestAppWebBeijingTokenTimestampBehavior(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	helpers := extractWebSource(t, html, "const beijingTimeFormatter", "\n    async function cleanupLocalRecords")
	showToken := extractWebSource(t, html, "function showAPIToken(member, token)", "\n    function closeAPIToken()")

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for embedded token timestamp behavior test: %v", err)
	}
	harness := `
import assert from "node:assert/strict";

const elements = new Map();
function $(id) {
  if (!elements.has(id)) {
    elements.set(id, {
      textContent: "",
      disabled: false,
      classList: { remove() {} }
    });
  }
  return elements.get(id);
}
const state = {
  auth: { member: { email: "member@example.com" } },
  clientConfig: { user_api: "https://manager.example.com/" }
};
const window = { location: { origin: "https://fallback.example.com" } };

` + helpers + "\n" + showToken + `

showAPIToken({
  name: "Member",
  email: "member@example.com",
  has_api_token: true,
  api_token_at: "2026-07-16T08:03:24Z"
}, "");

assert.equal(
  $("apiTokenOutput").textContent,
  "令牌已生成\n生成时间: 2026-07-16 16:03:24（北京时间）\n\n出于安全原因，旧 token 明文不会再次显示。"
);
assert.equal($("apiTokenOutput").textContent.includes("Z"), false);
`
	script := filepath.Join(t.TempDir(), "beijing_token_timestamp_test.mjs")
	if err := os.WriteFile(script, []byte(harness), 0o600); err != nil {
		t.Fatalf("write node harness: %v", err)
	}
	output, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("token timestamp node behavior test failed: %v\n%s", err, output)
	}
}

func TestAppWebJobLogAPI(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	job, err := app.JobManager.Create(Job{Type: "aws-destroy", Profile: "xcode-vnc", Status: JobStatusSuccess})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := os.WriteFile(job.Log, []byte("job log line\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/job/log?id="+job.ID, nil)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "job log line") || !strings.Contains(rec.Body.String(), job.ID) {
		t.Fatalf("job log body = %s", rec.Body.String())
	}
}

func TestAppWebJobLogRequiresProfileAccess(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	for _, name := range []string{"assigned-usw2", "private-usw2"} {
		if _, err := app.MemberStore.UpsertManagedProfile(Profile{Name: name}); err != nil {
			t.Fatalf("upsert managed profile %s: %v", name, err)
		}
	}
	assignedJob, err := app.JobManager.Create(Job{Type: "aws-open", Profile: "assigned-usw2", Status: JobStatusSuccess})
	if err != nil {
		t.Fatalf("create assigned job: %v", err)
	}
	privateJob, err := app.JobManager.Create(Job{Type: "aws-open", Profile: "private-usw2", Status: JobStatusSuccess})
	if err != nil {
		t.Fatalf("create private job: %v", err)
	}
	for _, job := range []Job{assignedJob, privateJob} {
		if err := os.WriteFile(job.Log, []byte(job.Profile+" log\n"), 0o600); err != nil {
			t.Fatalf("write %s log: %v", job.Profile, err)
		}
	}
	baseReq := httptest.NewRequest(http.MethodGet, "/api/job/log?id="+assignedJob.ID, nil)
	addWebAuth(t, &app, baseReq, "viewer")
	if _, err := app.MemberStore.AssignProfileAccess("assigned-usw2", "admin@example.com"); err != nil {
		t.Fatalf("assign profile access: %v", err)
	}
	cookies := baseReq.Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected auth cookie")
	}
	for _, tc := range []struct {
		name string
		job  Job
		code int
	}{
		{name: "assigned", job: assignedJob, code: http.StatusOK},
		{name: "unassigned", job: privateJob, code: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/job/log?id="+tc.job.ID, nil)
			req.AddCookie(cookies[0])
			rec := httptest.NewRecorder()
			app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAppWebLegacyGlobalTransferEndpointsRequireAdmin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
		role   string
		body   string
		code   int
	}{
		{name: "viewer list", method: http.MethodGet, path: "/api/sync/history", role: "viewer", code: http.StatusUnauthorized},
		{name: "operator delete", method: http.MethodPost, path: "/api/sync/history/delete", role: "operator", body: `{"id":"missing"}`, code: http.StatusUnauthorized},
		{name: "admin list", method: http.MethodGet, path: "/api/sync/history", role: "admin", code: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, dir)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			addWebAuth(t, &app, req, tc.role)
			rec := httptest.NewRecorder()
			app.newWebHandler(DefaultConfigPath).ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAppProfileAddShowRenameRemove(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	code := app.Run(context.Background(), []string{
		"profile", "add",
		"--name", "new-user-usw2",
		"--apple-email", "new@example.com",
		"--aws-profile", "cm-xcode",
		"--region", "us-west-2",
		"--eip", "54.1.2.3",
		"--eip-allocation-id", "eipalloc-new",
		"--key-name", "new-key",
		"--security-group-id", "sg-new",
		"--az", "usw2-az1",
		"--subnet", "usw2-az1=subnet-new",
		"--config", config,
	})
	if code != 0 {
		t.Fatalf("profile add code = %d, err = %s", code, errOut.String())
	}
	profilePath := filepath.Join(dir, "profiles", "new-user-usw2.yaml")
	if _, err := os.Stat(profilePath); err != nil {
		t.Fatalf("expected profile file: %v", err)
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"profile", "show", "new@example.com", "--config", config}); code != 0 {
		t.Fatalf("profile show code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "new-user-usw2") || !strings.Contains(out.String(), "new@example.com") {
		t.Fatalf("profile show output = %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"profile", "rename", "new-user-usw2", "renamed-usw2", "--config", config}); code != 0 {
		t.Fatalf("profile rename code = %d, err = %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "renamed-usw2.yaml")); err != nil {
		t.Fatalf("expected renamed profile file: %v", err)
	}
	out.Reset()
	errOut.Reset()
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{}}, nil
	}
	if code := app.Run(context.Background(), []string{"profile", "remove", "renamed-usw2", "--force-local", "--config", config}); code != 0 {
		t.Fatalf("profile remove code = %d, err = %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "renamed-usw2.yaml")); !os.IsNotExist(err) {
		t.Fatalf("expected removed profile file, err=%v", err)
	}
}

func TestAppProfileRemoveBlocksWhenAWSResourcesExist(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "available"}},
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "running"}},
		}}, nil
	}
	if code := app.Run(context.Background(), []string{"profile", "remove", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("profile remove code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"still has AWS Mac resources", "never releases Elastic IP", "cm close xcode-vnc", "--force-local"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("err missing %q: %q", want, errOut.String())
		}
	}
}

func TestAppProfileAddWizardPreviewsAndWrites(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.In = strings.NewReader(strings.Join([]string{
		"wizard-usw2",
		"wizard@example.com",
		"",
		"",
		key,
		"cm-xcode",
		"us-west-2",
		"",
		"example-key",
		"sg-example",
		"eipalloc-example",
		"54.1.2.3",
		"usw2-az1",
		"subnet-example",
		"",
		"y",
	}, "\n") + "\n")
	if code := app.Run(context.Background(), []string{"profile", "add", "--wizard", "--config", config}); code != 0 {
		t.Fatalf("profile add wizard code = %d, err = %s, out = %s", code, errOut.String(), out.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "profiles", "wizard-usw2.yaml"))
	if err != nil {
		t.Fatalf("read wizard profile: %v", err)
	}
	for _, want := range []string{"Profile preview", "Warnings", "ec2-54-1-2-3.us-west-2.compute.amazonaws.com"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("wizard output missing %q: %s", want, out.String())
		}
	}
	if !strings.Contains(string(data), "wizard@example.com") || !strings.Contains(string(data), "subnet-example") {
		t.Fatalf("wizard profile = %s", data)
	}
}

func TestAppProfileWizardAliasPreviewsAndWrites(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.In = strings.NewReader(strings.Join([]string{
		"alias-usw2",
		"alias@example.com",
		"",
		"",
		key,
		"cm-xcode",
		"us-west-2",
		"",
		"example-key",
		"sg-example",
		"eipalloc-example",
		"54.1.2.4",
		"",
		"y",
	}, "\n") + "\n")
	if code := app.Run(context.Background(), []string{"profile", "wizard", "--config", config}); code != 0 {
		t.Fatalf("profile wizard code = %d, err = %s, out = %s", code, errOut.String(), out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "alias-usw2.yaml")); err != nil {
		t.Fatalf("expected profile wizard alias file: %v", err)
	}
	if !strings.Contains(out.String(), "Profile preview") {
		t.Fatalf("wizard alias output = %s", out.String())
	}
}

func TestAppProfileAddWizardRejectsDuplicateAppleEmail(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.In = strings.NewReader("duplicate-usw2\nuser@example.com\n")
	if code := app.Run(context.Background(), []string{"profile", "add", "--wizard", "--config", config}); code != 1 {
		t.Fatalf("profile add wizard code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "already configured") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppProfileExportImport(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"profile", "export", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("profile export code = %d, err = %s", code, errOut.String())
	}
	exported := filepath.Join(t.TempDir(), "export.yaml")
	writeFile(t, exported, strings.Replace(out.String(), "xcode-vnc:", "imported:", 1))
	otherDir := t.TempDir()
	otherConfig := filepath.Join(otherDir, "config.yaml")
	writeFile(t, otherConfig, "profiles:\n")
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"profile", "import", exported, "--config", otherConfig}); code != 0 {
		t.Fatalf("profile import code = %d, err = %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(otherDir, "profiles", "imported.yaml")); err != nil {
		t.Fatalf("expected imported profile: %v", err)
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"profile", "import", exported, "--config", otherConfig}); code != 1 {
		t.Fatalf("duplicate import code = %d, err = %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"profile", "import", exported, "--overwrite", "--config", otherConfig}); code != 0 {
		t.Fatalf("overwrite import code = %d, err = %s", code, errOut.String())
	}
}

func TestAppDoctorDashboardAndSetupVNC(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"doctor", "--fix", "--config", config}); code != 0 {
		t.Fatalf("doctor code = %d, err = %s\nout=%s", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), "mcp tools") || !strings.Contains(out.String(), "config file") || !strings.Contains(out.String(), "NEXT") {
		t.Fatalf("doctor output = %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"dashboard", "--config", config}); code != 0 {
		t.Fatalf("dashboard code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "PROFILE") || !strings.Contains(out.String(), "xcode-vnc") {
		t.Fatalf("dashboard output = %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "running", SystemStatus: "ok", InstanceStatusCheck: "ok"}},
			ElasticIP: ElasticIP{InstanceID: "i-1", PublicIP: "54.1.2.3"},
		}}, nil
	}
	if code := app.Run(context.Background(), []string{"dashboard", "--aws", "--config", config}); code != 0 {
		t.Fatalf("dashboard --aws code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "READY") || !strings.Contains(out.String(), "DECISION") || !strings.Contains(out.String(), "NEXT") || !strings.Contains(out.String(), "cm start xcode-vnc") || !strings.Contains(out.String(), "ready") || !strings.Contains(out.String(), "54.1.2.3") {
		t.Fatalf("dashboard --aws output = %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"setup-vnc", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("setup-vnc code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "sudo passwd ec2-user") || !strings.Contains(out.String(), "cm open-vnc xcode-vnc") {
		t.Fatalf("setup-vnc output = %s", out.String())
	}
}

func TestAppDoctorSuggestsFixCommands(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "missing.yaml")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"doctor", "--config", config}); code != 1 {
		t.Fatalf("doctor missing config code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"config file", "fail", "cm init", "profiles dir", "cm doctor --fix"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("doctor missing config output missing %q:\n%s", want, out.String())
		}
	}
}

func TestAppNextReportsReadyFlow(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "running", SystemStatus: "ok", InstanceStatusCheck: "ok"}},
			ElasticIP: ElasticIP{InstanceID: "i-1", PublicIP: "54.1.2.3"},
		}}, nil
	}
	if code := app.Run(context.Background(), []string{"next", "user@example.com", "--config", config}); code != 0 {
		t.Fatalf("next code = %d, err = %s, out = %s", code, errOut.String(), out.String())
	}
	for _, want := range []string{"Next step for profile xcode-vnc", "Decision: ready", "Next: cm start xcode-vnc", "After tunnel: cm open-vnc xcode-vnc"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("next output missing %q:\n%s", want, out.String())
		}
	}
}

func TestAppNextReportsConfigFixes(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, "profiles:\n  broken:\n    description: broken\n")
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"next", "broken", "--config", config}); code != 0 {
		t.Fatalf("next broken code = %d, err = %s, out = %s", code, errOut.String(), out.String())
	}
	for _, want := range []string{"Decision: fix-config", "Local access issues", "AWS config issues", "Next: cm profile edit broken"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("next broken output missing %q:\n%s", want, out.String())
		}
	}
}

func TestAppCompletionZshScriptUsesDynamicProfiles(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"completion", "zsh"}); code != 0 {
		t.Fatalf("completion zsh code = %d, err = %s", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{"#compdef cm", "cm completion profiles", "cm completion apple-emails", "cm completion aws-commands", "cm completion local-agent-commands", "open|close|next", "guide topic", "--force"} {
		if !strings.Contains(text, want) {
			t.Fatalf("zsh completion missing %q: %q", want, text)
		}
	}
}

func TestAppCompletionLocalAgentCommands(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"completion", "local-agent-commands"}); code != 0 {
		t.Fatalf("completion local-agent-commands code = %d, err = %s", code, errOut.String())
	}
	for _, want := range []string{"install", "start", "stop", "restart", "status", "uninstall"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("local-agent completion missing %q:\n%s", want, out.String())
		}
	}
}

func TestAppCheck(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"check", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("check code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "check passed") {
		t.Fatalf("check output = %q", out.String())
	}
}

func TestAppCheckFailsForMissingIdentityFile(t *testing.T) {
	dir := t.TempDir()
	configData := strings.ReplaceAll(sampleConfig, "  identity_file: ~/.ssh/default.pem\n", "")
	configData = strings.ReplaceAll(configData, "    identity_file: ~/.ssh/example.pem\n", "")
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"check", "xcode-vnc", "--config", config}); code == 0 {
		t.Fatalf("check code = 0, want failure, out = %s", out.String())
	}
	if strings.Contains(errOut.String(), "identity_file for xcode-vnc") {
		t.Fatalf("did not expect identity_file prompt, got %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "set profile.identity_file or defaults.identity_file") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppConnectUsesRunner(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"connect", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("connect code = %d, err = %s", code, errOut.String())
	}
	if len(runner.foreground) == 0 {
		t.Fatal("expected foreground runner to be called")
	}
}

func TestAppSSHUsesRunner(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"ssh", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("ssh code = %d, err = %s", code, errOut.String())
	}
	if len(runner.foreground) == 0 {
		t.Fatal("expected foreground runner to be called")
	}
	for _, arg := range runner.foreground {
		if arg == "-N" {
			t.Fatalf("interactive ssh args must not include -N: %#v", runner.foreground)
		}
	}
	if !strings.Contains(out.String(), "SSH: user@") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestAppStartSavesStateAfterHealthyTunnel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("start code = %d, err = %s", code, errOut.String())
	}
	if len(runner.background) == 0 {
		t.Fatal("expected background runner to be called")
	}
	if !strings.Contains(out.String(), "started xcode-vnc with pid 55") {
		t.Fatalf("out = %q", out.String())
	}
	state, ok, err := app.StateManager.Load("xcode-vnc")
	if err != nil || !ok || state.PID != 55 {
		t.Fatalf("state = %+v ok=%t err=%v", state, ok, err)
	}
}

func TestAppStartReusesExistingRunningTunnel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.Validator.CheckPort = func(port int) error {
		return fmt.Errorf("local port %d is already in use", port)
	}
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	if err := app.StateManager.Save(managedTestState(validProfile(key), 77)); err != nil {
		t.Fatal(err)
	}
	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("start code = %d, err = %s", code, errOut.String())
	}
	if len(runner.background) != 0 {
		t.Fatalf("did not expect background runner, got %#v", runner.background)
	}
	if !strings.Contains(out.String(), "already started xcode-vnc with pid 77") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestAppStartReusesExistingRunningTunnelWithoutPEMOrSSHAvailability(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.Validator.CheckSSH = func() error { return errors.New("ssh executable not found on PATH") }
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	profile := validProfile(key)
	if err := app.StateManager.Save(managedTestState(profile, 77)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 || len(runner.background) != 0 {
		t.Fatalf("exact reuse mutated lifecycle: stop=%d background=%#v", runner.stopPID, runner.background)
	}
}

func TestAppStartRefusesExactMatchWithDifferentCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	if err := app.StateManager.Save(managedTestState(validProfile(key), 77)); err != nil {
		t.Fatal(err)
	}
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		return testProcessIdentity("unrelated-process --serve", "start-77"), nil
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 || len(runner.background) != 0 {
		t.Fatalf("unverified exact match was mutated: stop=%d background=%#v", runner.stopPID, runner.background)
	}
	if !strings.Contains(errOut.String(), "refusing to reuse tunnel pid 77") ||
		!strings.Contains(errOut.String(), "command does not match") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppStartAdoptsMatchingLegacyLiveState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	state := managedTestState(validProfile(key), 77)
	state.IdentityFile = ""
	state.SSHCommandFingerprint = ""
	state.ProcessStartMarker = ""
	if err := app.StateManager.Save(state); err != nil {
		t.Fatal(err)
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 || len(runner.background) != 0 {
		t.Fatalf("legacy exact match was restarted: stop=%d background=%#v", runner.stopPID, runner.background)
	}
	adopted, ok, err := app.StateManager.Load("xcode-vnc")
	if err != nil || !ok {
		t.Fatalf("adopted state ok=%t err=%v", ok, err)
	}
	if adopted.IdentityFile == "" || adopted.SSHCommandFingerprint == "" || adopted.ProcessStartMarker != "start-77" {
		t.Fatalf("adopted state = %+v", adopted)
	}
	if !strings.Contains(out.String(), "adopted legacy state") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestAppStartRefusesMismatchedLegacyLiveCommandWithoutKilling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	state := managedTestState(validProfile(key), 77)
	state.IdentityFile = ""
	state.SSHCommandFingerprint = ""
	state.ProcessStartMarker = ""
	if err := app.StateManager.Save(state); err != nil {
		t.Fatal(err)
	}
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		return testProcessIdentity("unrelated --serve", "start-77"), nil
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 || len(runner.background) != 0 {
		t.Fatalf("mismatched legacy process was mutated: stop=%d background=%#v", runner.stopPID, runner.background)
	}
	if !strings.Contains(errOut.String(), "cannot be safely killed") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppStartRefusesExactMatchWithReusedPIDStartMarker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	if err := app.StateManager.Save(managedTestState(validProfile(key), 77)); err != nil {
		t.Fatal(err)
	}
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		return testProcessIdentity("ssh fake-managed-tunnel", "different-start"), nil
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 || len(runner.background) != 0 {
		t.Fatalf("reused pid was mutated: stop=%d background=%#v", runner.stopPID, runner.background)
	}
	if !strings.Contains(errOut.String(), "start marker does not match") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppStartCaptureFailureStopsUnrecordedTunnel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{}, errors.New("process disappeared")
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("unverified cleanup stopped pid = %d", runner.stopPID)
	}
	if _, ok, err := app.StateManager.Load("xcode-vnc"); err != nil || ok {
		t.Fatalf("unrecorded tunnel left state, ok=%t err=%v", ok, err)
	}
	if !strings.Contains(errOut.String(), "inspect started tunnel pid 55: inspect pid 55: process disappeared") ||
		!strings.Contains(errOut.String(), "cannot safely terminate the unverified process") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppStartRefusesStartedPIDWithUnrelatedCommandWithoutKilling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		return testProcessIdentity("unrelated --serve", "reused-start"), nil
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("unrelated reused pid was killed: %d", runner.stopPID)
	}
	if _, ok, err := app.StateManager.Load("xcode-vnc"); err != nil || ok {
		t.Fatalf("unrelated pid was saved, ok=%t err=%v", ok, err)
	}
}

func TestAppStartRestartsDeadPIDAndReplacesState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 55 }
	if err := app.StateManager.Save(managedTestState(validProfile(key), 77)); err != nil {
		t.Fatal(err)
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 || len(runner.background) == 0 {
		t.Fatalf("dead pid handling stop=%d background=%#v", runner.stopPID, runner.background)
	}
	state, ok, err := app.StateManager.Load("xcode-vnc")
	if err != nil || !ok || state.PID != 55 {
		t.Fatalf("replacement state = %+v ok=%t err=%v", state, ok, err)
	}
}

func TestAppStatusCannotDeleteStateReplacedByConcurrentStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var startOut, startErr, statusOut, statusErr bytes.Buffer
	startApp := testApp(&startOut, &startErr, dir)
	startApp.Runner = &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
	statusApp := testApp(&statusOut, &statusErr, dir)
	statusReadOld := make(chan struct{})
	releaseStatus := make(chan struct{})
	statusApp.StateManager.IsRunning = func(pid int) bool {
		if pid == 77 {
			close(statusReadOld)
			<-releaseStatus
			return false
		}
		return pid == 55
	}
	startApp.StateManager.IsRunning = func(pid int) bool { return pid == 55 }
	if err := startApp.StateManager.Save(managedTestState(validProfile(key), 77)); err != nil {
		t.Fatal(err)
	}
	statusDone := make(chan int, 1)
	go func() {
		statusDone <- statusApp.runStatus()
	}()
	<-statusReadOld
	if code := startApp.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("start code=%d err=%q", code, startErr.String())
	}
	close(releaseStatus)
	if code := <-statusDone; code != 0 {
		t.Fatalf("status code=%d err=%q", code, statusErr.String())
	}
	state, ok, err := startApp.StateManager.Load("xcode-vnc")
	if err != nil || !ok || state.PID != 55 {
		t.Fatalf("concurrent status lost replacement state: %+v ok=%t err=%v", state, ok, err)
	}
}

func TestAppStatusOmitsLivePIDWithCommandMismatch(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	if err := app.StateManager.Save(managedTestState(validProfile("/tmp/key.pem"), 77)); err != nil {
		t.Fatal(err)
	}
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		return testProcessIdentity("unrelated --serve", "start-77"), nil
	}

	if code := app.runStatus(); code != 0 {
		t.Fatalf("status code = %d, err = %s", code, errOut.String())
	}
	if out.String() != "no managed tunnels running\n" {
		t.Fatalf("status output = %q", out.String())
	}
}

func TestAppStatusOmitsLivePIDWithStartMarkerMismatch(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	if err := app.StateManager.Save(managedTestState(validProfile("/tmp/key.pem"), 77)); err != nil {
		t.Fatal(err)
	}
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		return testProcessIdentity("ssh fake-managed-tunnel", "different-start"), nil
	}

	if code := app.runStatus(); code != 0 {
		t.Fatalf("status code = %d, err = %s", code, errOut.String())
	}
	if out.String() != "no managed tunnels running\n" {
		t.Fatalf("status output = %q", out.String())
	}
}

func TestAppStartPreflightFailureDoesNotSpawnOrReplaceTunnel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.Preflight = func() error { return errors.New("unsupported test OS") }

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 || len(runner.background) != 0 {
		t.Fatalf("preflight failure mutated lifecycle: stop=%d background=%#v", runner.stopPID, runner.background)
	}
	if !strings.Contains(errOut.String(), "unsupported test OS") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppStartSaveFailureStopsUnrecordedTunnel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		if err := os.RemoveAll(app.StateManager.Dir); err != nil {
			return ProcessIdentity{}, err
		}
		if err := os.WriteFile(app.StateManager.Dir, []byte("blocks state directory"), 0o600); err != nil {
			return ProcessIdentity{}, err
		}
		return testProcessIdentity("ssh fake-managed-tunnel", "start-55"), nil
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 55 {
		t.Fatalf("cleanup stopped pid = %d, want 55", runner.stopPID)
	}
	if !strings.Contains(errOut.String(), "save state for started tunnel pid 55") ||
		!strings.Contains(errOut.String(), "stopped unrecorded tunnel") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppStartSaveFailureRefusesCleanupAfterIdentityChanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	inspections := 0
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		inspections++
		if inspections == 1 {
			if err := os.RemoveAll(app.StateManager.Dir); err != nil {
				return ProcessIdentity{}, err
			}
			if err := os.WriteFile(app.StateManager.Dir, []byte("blocks state directory"), 0o600); err != nil {
				return ProcessIdentity{}, err
			}
			return testProcessIdentity("ssh fake-managed-tunnel", "start-55"), nil
		}
		return testProcessIdentity("unrelated-process --serve", "start-55"), nil
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("identity-mismatched cleanup stopped pid = %d", runner.stopPID)
	}
	if inspections != 2 || !strings.Contains(errOut.String(), "cleanup failed") ||
		!strings.Contains(errOut.String(), "command does not match") {
		t.Fatalf("inspections=%d err=%q", inspections, errOut.String())
	}
}

func TestAppStopVerifiesManagedProcess(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	if err := app.StateManager.Save(managedTestState(validProfile("/tmp/key.pem"), 77)); err != nil {
		t.Fatal(err)
	}
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		return testProcessIdentity("unrelated-process", "start-77"), nil
	}

	if code := app.runStop([]string{"xcode-vnc"}); code != 1 {
		t.Fatalf("stop code = %d, out=%q err=%q", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("unverified pid was stopped: %d", runner.stopPID)
	}
	if !strings.Contains(errOut.String(), "command does not match") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppStopRefusesLegacyLiveStateActionably(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	state := managedTestState(validProfile("/tmp/key.pem"), 77)
	state.ProcessStartMarker = ""
	if err := app.StateManager.Save(state); err != nil {
		t.Fatal(err)
	}

	if code := app.runStop([]string{"xcode-vnc"}); code != 1 {
		t.Fatalf("stop code = %d, out=%q err=%q", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("legacy pid was stopped: %d", runner.stopPID)
	}
	for _, want := range []string{"cannot be safely managed", "manually verify and terminate", "remove the stale state only after the process is gone"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("err missing %q: %q", want, errOut.String())
		}
	}
}

func TestAppConcurrentStartAndStopUseSameProfileLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var startOut, startErr, stopOut, stopErr bytes.Buffer
	runner := &blockingStartRunner{
		fakeRunner: &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"},
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	startApp := testApp(&startOut, &startErr, dir)
	startApp.Runner = runner
	stopApp := testApp(&stopOut, &stopErr, dir)
	stopApp.Runner = runner
	startDone := make(chan int, 1)
	stopDone := make(chan int, 1)
	go func() {
		startDone <- startApp.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config})
	}()
	<-runner.entered
	go func() {
		stopDone <- stopApp.runStop([]string{"xcode-vnc"})
	}()
	select {
	case code := <-stopDone:
		t.Fatalf("stop completed before start released the profile lock: %d", code)
	case <-time.After(50 * time.Millisecond):
	}
	close(runner.release)
	if code := <-startDone; code != 0 {
		t.Fatalf("start code=%d err=%q", code, startErr.String())
	}
	if code := <-stopDone; code != 0 {
		t.Fatalf("stop code=%d err=%q", code, stopErr.String())
	}
	if runner.stopPID != 55 {
		t.Fatalf("stop pid=%d, want 55", runner.stopPID)
	}
}

func TestAppStartReplacesHealthyMismatchedManagedTunnel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 || pid == 55 }
	oldProfile := validProfile(key)
	oldProfile.Host = "old-host.example.com"
	if err := app.StateManager.Save(managedTestState(oldProfile, 77)); err != nil {
		t.Fatal(err)
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("start code = %d, err = %s", code, errOut.String())
	}
	if runner.stopPID != 77 {
		t.Fatalf("stopped pid = %d, want 77", runner.stopPID)
	}
	if len(runner.background) == 0 {
		t.Fatal("expected replacement tunnel to start")
	}
	state, ok, err := app.StateManager.Load("xcode-vnc")
	if err != nil || !ok {
		t.Fatalf("replacement state ok=%t err=%v", ok, err)
	}
	if state.PID != 55 || !state.Matches(validProfile(key)) {
		t.Fatalf("replacement state = %+v", state)
	}
}

func TestAppStartReturnsClearErrorWhenMismatchedManagedTunnelStopFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{stopErr: errors.New("operation not permitted")}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	oldProfile := validProfile(key)
	oldProfile.Tunnels[0].LocalPort = 5901
	if err := app.StateManager.Save(managedTestState(oldProfile, 77)); err != nil {
		t.Fatal(err)
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "stop mismatched managed tunnel pid 77: operation not permitted") {
		t.Fatalf("err = %q", errOut.String())
	}
	if len(runner.background) != 0 {
		t.Fatalf("replacement tunnel should not start: %#v", runner.background)
	}
	if _, ok, err := app.StateManager.Load("xcode-vnc"); err != nil || !ok {
		t.Fatalf("managed state should remain after stop failure, ok=%t err=%v", ok, err)
	}
}

func TestAppStartPreservesMismatchedManagedTunnelWhenNewPortIsOccupied(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.Validator.CheckPort = func(port int) error {
		if port == 5901 {
			return fmt.Errorf("local port %d is already in use", port)
		}
		return nil
	}
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	oldProfile := validProfile(key)
	oldProfile.Host = "old-host.example.com"
	newProfile := validProfile(key)
	newProfile.Tunnels = append(newProfile.Tunnels, Tunnel{
		LocalPort:  5901,
		RemoteHost: "localhost",
		RemotePort: 5901,
	})
	configData, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	configData = []byte(strings.Replace(string(configData),
		"      - local_port: 5900\n        remote_host: localhost\n        remote_port: 5900\n",
		"      - local_port: 5900\n        remote_host: localhost\n        remote_port: 5900\n"+
			"      - local_port: 5901\n        remote_host: localhost\n        remote_port: 5901\n", 1))
	if err := os.WriteFile(config, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.StateManager.Save(managedTestState(oldProfile, 77)); err != nil {
		t.Fatal(err)
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("old tunnel was stopped: %d", runner.stopPID)
	}
	if !strings.Contains(errOut.String(), "local port 5901 is already in use") {
		t.Fatalf("err = %q", errOut.String())
	}
	if len(runner.background) != 0 {
		t.Fatalf("replacement tunnel should not start: %#v", runner.background)
	}
	if state, ok, err := app.StateManager.Load("xcode-vnc"); err != nil || !ok || state.PID != 77 {
		t.Fatalf("old managed state should remain, state=%+v ok=%t err=%v", state, ok, err)
	}
}

func TestAppStartDefersExistingTunnelPortValidationUntilAfterStop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 || pid == 55 }
	app.Validator.CheckPort = func(port int) error {
		if port == 5900 && runner.stopPID != 77 {
			return fmt.Errorf("local port %d is already in use", port)
		}
		return nil
	}
	oldProfile := validProfile(key)
	oldProfile.Host = "old-host.example.com"
	if err := app.StateManager.Save(managedTestState(oldProfile, 77)); err != nil {
		t.Fatal(err)
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 77 || len(runner.background) == 0 {
		t.Fatalf("replacement lifecycle stop=%d background=%#v", runner.stopPID, runner.background)
	}
}

func TestAppStartRefusesToStopUnverifiedMismatchedPID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	oldProfile := validProfile(key)
	oldProfile.Host = "old-host.example.com"
	if err := app.StateManager.Save(managedTestState(oldProfile, 77)); err != nil {
		t.Fatal(err)
	}
	app.StateManager.InspectProcess = func(pid int) (ProcessIdentity, error) {
		return testProcessIdentity("unrelated-process --serve", fmt.Sprintf("start-%d", pid)), nil
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("unverified pid was stopped: %d", runner.stopPID)
	}
	if !strings.Contains(errOut.String(), "stop mismatched managed tunnel pid 77") {
		t.Fatalf("err = %q", errOut.String())
	}
	if _, ok, err := app.StateManager.Load("xcode-vnc"); err != nil || !ok {
		t.Fatalf("old state should be preserved, ok=%t err=%v", ok, err)
	}
}

func TestAppStartRefusesToStopLegacyMismatchedState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	oldProfile := validProfile(key)
	oldProfile.Host = "old-host.example.com"
	state := managedTestState(oldProfile, 77)
	state.SSHCommandFingerprint = ""
	if err := app.StateManager.Save(state); err != nil {
		t.Fatal(err)
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("legacy state pid was stopped: %d", runner.stopPID)
	}
	if !strings.Contains(errOut.String(), "legacy live tunnel pid 77") ||
		!strings.Contains(errOut.String(), "complete process identity") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppStartInvalidReplacementPreservesOldTunnel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.Validator.CheckSSH = func() error { return errors.New("ssh executable not found on PATH") }
	app.StateManager.IsRunning = func(pid int) bool { return pid == 77 }
	oldProfile := validProfile(key)
	oldProfile.Host = "old-host.example.com"
	if err := app.StateManager.Save(managedTestState(oldProfile, 77)); err != nil {
		t.Fatal(err)
	}

	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if runner.stopPID != 0 {
		t.Fatalf("healthy old tunnel was stopped: %d", runner.stopPID)
	}
	if !strings.Contains(errOut.String(), "ssh executable not found on PATH") {
		t.Fatalf("err = %q", errOut.String())
	}
	if _, ok, err := app.StateManager.Load("xcode-vnc"); err != nil || !ok {
		t.Fatalf("old state should be preserved, ok=%t err=%v", ok, err)
	}
}

func TestAppStartSerializesConcurrentStartsPerProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	runner := &synchronizedRunner{fakeRunner: &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.StateManager.IsRunning = func(pid int) bool { return pid == 55 }

	start := make(chan struct{})
	results := make(chan int, 2)
	for range 2 {
		go func() {
			<-start
			results <- app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config})
		}()
	}
	close(start)
	for range 2 {
		if code := <-results; code != 0 {
			t.Fatalf("start code = %d", code)
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.starts != 1 {
		t.Fatalf("background starts = %d, want 1", runner.starts)
	}
}

func TestAppStartBlocksMissingHostKeyBeforeTunnel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{missingHostKey: true}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, err = %s", code, errOut.String())
	}
	if len(runner.background) != 0 || runner.forgotHost != "" {
		t.Fatalf("missing key mutated state or started tunnel: background=%v forgot=%q", runner.background, runner.forgotHost)
	}
}

func TestAppStartBlocksStaleHostKeyUntilExplicitFix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	scanned, _ := testScannedHostKey(t, "mac-host.example.com")
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAAOLD\n", scannedKey: scanned}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.In = strings.NewReader("yes\n")
	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("blocked start code = %d, err = %s", code, errOut.String())
	}
	if runner.forgotHost != "" || len(runner.background) != 0 {
		t.Fatalf("stale key mutated state or started tunnel: forgot=%q background=%v", runner.forgotHost, runner.background)
	}
	if code := app.Run(context.Background(), []string{"host-key", "fix", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("host-key fix code = %d, err = %s", code, errOut.String())
	}
	runner.knownHost = runner.scannedKey
	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("start after explicit fix code = %d, err = %s", code, errOut.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, ".ssh", "known_hosts"))
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	if !strings.Contains(string(data), strings.Fields(scanned)[2]) {
		t.Fatalf("known_hosts=%q out=%q", data, out.String())
	}
}

func TestAppStartStopsWhenHostKeyScanFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{scanErr: errors.New("scan failed")}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if len(runner.background) != 0 {
		t.Fatalf("tunnel should not start after scan failure: %#v", runner.background)
	}
	if !strings.Contains(errOut.String(), "host key scan failed") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppStartDoesNotSaveStateWhenTunnelFailsHealthCheck(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n", startErr: errors.New("Permission denied (publickey)")}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"start", "xcode-vnc", "--config", config}); code != 1 {
		t.Fatalf("start code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "started") {
		t.Fatalf("should not report started on failed health check: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "Permission denied") {
		t.Fatalf("err = %q", errOut.String())
	}
	if _, ok, err := app.StateManager.Load("xcode-vnc"); err != nil || ok {
		t.Fatalf("state should not be saved, ok=%t err=%v", ok, err)
	}
}

func TestAppPullUsesRsyncRunner(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"pull", "xcode-vnc", "~/Desktop/a.zip", "--config", config}); code != 0 {
		t.Fatalf("pull code = %d, err = %s", code, errOut.String())
	}
	if len(runner.rsync) == 0 {
		t.Fatal("expected rsync runner to be called")
	}
	if !strings.Contains(out.String(), "Pull:") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestAppPullAcceptsAppleEmail(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"pull", "user@example.com", "~/Desktop/a.zip", "--config", config}); code != 0 {
		t.Fatalf("pull code = %d, err = %s", code, errOut.String())
	}
	if !containsString(runner.rsync, "user@mac-host.example.com:~/Desktop/a.zip") {
		t.Fatalf("rsync args = %#v", runner.rsync)
	}
}

func TestAppPullAcceptsSyncFilters(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"pull", "xcode-vnc", "~/Desktop/a.zip", "--include", "*.zip", "--exclude", "*.tmp", "--config", config}); code != 0 {
		t.Fatalf("pull code = %d, err = %s", code, errOut.String())
	}
	for _, want := range []string{"--include", "*.zip", "--exclude", ".DS_Store", "--exclude", "*.tmp", "--exclude", "*"} {
		if !containsString(runner.rsync, want) {
			t.Fatalf("rsync args missing %q = %#v", want, runner.rsync)
		}
	}
}

func TestAppPushUsesRsyncRunner(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	localFile := filepath.Join(dir, "build.zip")
	writeFile(t, localFile, "zip")
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"push", "xcode-vnc", localFile, "~/Downloads/", "--config", config}); code != 0 {
		t.Fatalf("push code = %d, err = %s", code, errOut.String())
	}
	if len(runner.rsync) == 0 {
		t.Fatal("expected rsync runner to be called")
	}
	if !strings.Contains(out.String(), "Push:") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestAppPushAcceptsSyncFilters(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	localFile := filepath.Join(dir, "build.zip")
	writeFile(t, localFile, "zip")
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"push", "xcode-vnc", localFile, "~/Downloads/", "--include", "Sources/***", "--exclude", "DerivedData", "--config", config}); code != 0 {
		t.Fatalf("push code = %d, err = %s", code, errOut.String())
	}
	for _, want := range []string{"--include", "Sources/***", "--exclude", "xcuserdata", "--exclude", "DerivedData", "--exclude", "*"} {
		if !containsString(runner.rsync, want) {
			t.Fatalf("rsync args missing %q = %#v", want, runner.rsync)
		}
	}
}

func TestAppPushAcceptsAppleEmail(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	localFile := filepath.Join(dir, "build.zip")
	writeFile(t, localFile, "zip")
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"push", "user@example.com", localFile, "~/Downloads/", "--config", config}); code != 0 {
		t.Fatalf("push code = %d, err = %s", code, errOut.String())
	}
	if !containsString(runner.rsync, "user@mac-host.example.com:~/Downloads/") {
		t.Fatalf("rsync args = %#v", runner.rsync)
	}
}

func TestAppExecUsesForegroundRunner(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	command := "ls -ld ~/Downloads/Vitora && du -sh ~/Downloads/Vitora"
	if code := app.Run(context.Background(), []string{"exec", "xcode-vnc", "--config", config, "--", command, "--config", "remote.yaml"}); code != 0 {
		t.Fatalf("exec code = %d, err = %s", code, errOut.String())
	}
	if len(runner.foreground) == 0 {
		t.Fatal("expected foreground runner to be called")
	}
	if !containsString(runner.foreground, "IdentitiesOnly=yes") {
		t.Fatalf("foreground args missing IdentitiesOnly=yes: %#v", runner.foreground)
	}
	if got := runner.foreground[len(runner.foreground)-3:]; !reflect.DeepEqual(got, []string{command, "--config", "remote.yaml"}) {
		t.Fatalf("command args = %#v, want command and remote --config preserved", got)
	}
	if !strings.Contains(out.String(), "Exec:") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestAppForgetHostUsesRunner(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"forget-host", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("forget-host code = %d, err = %s", code, errOut.String())
	}
	want := "mac-host.example.com"
	if runner.forgotHost != want {
		t.Fatalf("forgot host = %q, want %q", runner.forgotHost, want)
	}
	if !strings.Contains(out.String(), "Removing known_hosts entries") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestAppHostKeyCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	scanned, _ := testScannedHostKey(t, "mac-host.example.com")
	runner := &fakeRunner{knownHost: scanned, scannedKey: scanned}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	app.In = strings.NewReader("yes\n")
	if code := app.Run(context.Background(), []string{"host-key", "check", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("host-key check code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Host key: current") {
		t.Fatalf("check out = %q", out.String())
	}
	out.Reset()
	errOut.Reset()
	runner.missingHostKey = true
	if code := app.Run(context.Background(), []string{"host-key", "fix", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("host-key fix code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Host key: missing") {
		t.Fatalf("fix out = %q", out.String())
	}
}

func TestHostKeyGateBlocksCommandsWithoutMutationOrKeyMaterialInLogs(t *testing.T) {
	commands := []struct {
		name string
		run  func(App, context.Context, Config, Profile) int
	}{
		{name: "ssh", run: func(app App, ctx context.Context, cfg Config, profile Profile) int {
			return app.runSSH(ctx, cfg, []string{profile.Name})
		}},
		{name: "tunnel", run: func(app App, ctx context.Context, cfg Config, profile Profile) int {
			return app.runStart(ctx, cfg, []string{profile.Name})
		}},
		{name: "vnc", run: func(app App, ctx context.Context, cfg Config, profile Profile) int {
			return app.runOpenVNC(ctx, cfg, []string{profile.Name})
		}},
		{name: "push", run: func(app App, ctx context.Context, cfg Config, profile Profile) int {
			localPath := filepath.Join(app.StateManager.Dir, "payload")
			writeFile(t, localPath, "payload")
			return app.runPush(ctx, cfg, []string{profile.Name, localPath, "~/Documents/"})
		}},
		{name: "pull", run: func(app App, ctx context.Context, cfg Config, profile Profile) int {
			return app.runPull(ctx, cfg, []string{profile.Name, "~/Documents/payload"})
		}},
	}
	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var out, errOut bytes.Buffer
			runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAA-DO-NOT-LOG\n"}
			app := testApp(&out, &errOut, dir)
			app.Runner = runner
			profile := validProfile(writeSSHKey(t, 0o600))
			profile.Name = "blocked-profile"
			cfg := Config{Profiles: map[string]Profile{profile.Name: profile}}
			ctx := localLifecycleTestContext("blocked-" + tc.name)

			if code := tc.run(app, ctx, cfg, profile); code != 1 {
				t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
			if len(runner.foreground) != 0 || len(runner.background) != 0 || len(runner.rsync) != 0 || runner.openedVNC != "" || runner.forgotHost != "" {
				t.Fatalf("blocked command invoked runner or mutation: %+v", runner)
			}
			entries := readTestLogEntries(t, app.LogManager)
			blocked := make([]LogEntry, 0, 1)
			for _, entry := range entries {
				if entry.Action == "host-key.blocked" {
					blocked = append(blocked, entry)
				}
			}
			if len(blocked) != 1 {
				t.Fatalf("host-key.blocked entries=%d, all=%+v", len(blocked), entries)
			}
			if blocked[0].ErrorCode != "host_key_changed" || blocked[0].Status != string(HostKeyStale) || blocked[0].RequestID != "blocked-"+tc.name {
				t.Fatalf("blocked entry=%+v", blocked[0])
			}
			raw := readTestLogsRaw(t, app.LogManager)
			if strings.Contains(raw, "AAAA-DO-NOT-LOG") || strings.Contains(raw, "ssh-ed25519") {
				t.Fatalf("host key material leaked in logs: %s", raw)
			}
		})
	}
}

func TestRequireCurrentHostKeyErrorCodes(t *testing.T) {
	tests := []struct {
		name       string
		runner     *fakeRunner
		wantCode   string
		wantStatus HostKeyStatus
	}{
		{name: "missing", runner: &fakeRunner{missingHostKey: true}, wantCode: "host_key_missing", wantStatus: HostKeyMissing},
		{name: "scan failed", runner: &fakeRunner{scanErr: errors.New("network unavailable")}, wantCode: "host_key_scan_failed", wantStatus: HostKeyScanFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, t.TempDir())
			app.Runner = tc.runner
			profile := validProfile(writeSSHKey(t, 0o600))
			check, err := app.requireCurrentHostKey(localLifecycleTestContext("host-key-code"), profile)
			if err == nil || check.Status != tc.wantStatus {
				t.Fatalf("check=%+v err=%v", check, err)
			}
			classified := classifyLocalOperationError(err)
			if classified.Code != tc.wantCode {
				t.Fatalf("error code=%q want=%q err=%v", classified.Code, tc.wantCode, err)
			}
		})
	}
}

func TestAppOpenVNCUsesRunner(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	runner := &fakeRunner{}
	app := testApp(&out, &errOut, dir)
	app.Runner = runner
	if code := app.Run(context.Background(), []string{"open-vnc", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("open-vnc code = %d, err = %s", code, errOut.String())
	}
	if runner.openedVNC != "vnc://mac-user@localhost:5900" {
		t.Fatalf("opened VNC url = %q", runner.openedVNC)
	}
	if runner.openedURL != "" {
		t.Fatalf("ordinary URL opener called with %q", runner.openedURL)
	}
	if !strings.Contains(out.String(), "Opening vnc://mac-user@localhost:5900") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestExecRunnerOpenVNCCommand(t *testing.T) {
	cmd := (ExecRunner{}).openVNCCommand(context.Background(), "vnc://mac-user@localhost:5900")
	want := []string{"open", "-n", "-a", "Screen Sharing", "vnc://mac-user@localhost:5900"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("command args = %#v, want %#v", cmd.Args, want)
	}
}

func TestAppAWSPlan(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"aws", "plan", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("aws plan code = %d, err = %s", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{"AWS Mac plan", "Selected instance type: mac2.metal", "Selected AMI: ami-063755aadeb97329a"} {
		if !strings.Contains(text, want) {
			t.Fatalf("aws plan output missing %q:\n%s", want, text)
		}
	}
}

func TestAppProfileFindByAppleEmail(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"profile", "find", "user@example.com", "--config", config}); code != 0 {
		t.Fatalf("profile find code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Profile: xcode-vnc") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestAppAWSWaitReady(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			CallerIdentity: CallerIdentity{Account: "123456789012", ARN: "arn:aws:iam::123456789012:user/cm"},
			Instances: []InstanceStatus{{
				InstanceID:          "i-1",
				State:               "running",
				SystemStatus:        "ok",
				InstanceStatusCheck: "ok",
				EBSStatus:           "ok",
			}},
			ElasticIP: ElasticIP{InstanceID: "i-1"},
		}}, nil
	}
	app.AWSService.ReadyPollInterval = time.Millisecond
	app.AWSService.ReadyTimeout = time.Second
	if code := app.Run(context.Background(), []string{"aws", "wait-ready", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("aws wait-ready code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "AWS Mac ready for profile xcode-vnc: true") {
		t.Fatalf("out = %q", out.String())
	}
	if !strings.Contains(out.String(), "Manual GUI setup:") || !strings.Contains(out.String(), "sudo passwd ec2-user") {
		t.Fatalf("ready output missing manual setup guide:\n%s", out.String())
	}
}

func TestAppAWSCreateFailureReportsReasonAndStops(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{
			eip:    ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
			runErr: errString("RunInstances rejected selected host"),
		}, nil
	}
	if code := app.Run(context.Background(), []string{"aws", "create", "xcode-vnc", "--confirm", "--config", config}); code != 1 {
		t.Fatalf("aws create code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"aws create failed", "RunInstances rejected selected host", "Stopped", "wait for explicit instructions"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("err missing %q:\n%s", want, errOut.String())
		}
	}
}

func TestAppAWSDestroyResolvesAppleEmail(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "available", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "running", Tags: managedTestTags()}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", AssociationID: "eipassoc-1", InstanceID: "i-1", PublicIP: "203.0.113.10"},
		}}, nil
	}
	if code := app.Run(context.Background(), []string{"aws", "destroy", "user@example.com", "--config", config}); code != 0 {
		t.Fatalf("aws destroy by email code = %d, err = %s", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{"Resolved Apple account user@example.com -> profile xcode-vnc", "AWS Mac destroy preview", "Matched resources:", "retain the Elastic IP allocation"} {
		if !strings.Contains(text, want) {
			t.Fatalf("destroy by email output missing %q:\n%s", want, text)
		}
	}
}

func TestAppAWSDestroyConfirmPrintsFinalStatus(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	fake := &fakeAWSClient{status: AWSStatus{
		Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1", Tags: managedTestTags()}},
		Instances: []InstanceStatus{{InstanceID: "i-1", State: "running", InstanceType: "mac2.metal", HostID: "h-1", Tags: managedTestTags()}},
		ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", AssociationID: "eipassoc-1", InstanceID: "i-1", PublicIP: "203.0.113.10"},
	}}
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return fake, nil
	}
	if code := app.Run(context.Background(), []string{"aws", "destroy", "user@example.com", "--confirm", "--config", config}); code != 0 {
		t.Fatalf("aws destroy confirm code = %d, err = %s", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{"AWS Mac destroy executed", "Final status", "Dedicated hosts: 0", "Instances: 0", "Elastic IP retained: true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("destroy confirm output missing %q:\n%s", want, text)
		}
	}
}

func TestAppAWSDestroyBackgroundRequiresConfirm(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"aws", "destroy", "user@example.com", "--background", "--config", config}); code != 2 {
		t.Fatalf("aws destroy background code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "--background requires --confirm") {
		t.Fatalf("err missing background confirm guidance: %s", errOut.String())
	}
}

func TestAppAWSDestroyBackgroundCreatesJob(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"aws", "destroy", "user@example.com", "--confirm", "--background", "--notify", "--config", config}); code != 0 {
		t.Fatalf("aws destroy background code = %d, out = %s, err = %s", code, out.String(), errOut.String())
	}
	text := out.String()
	for _, want := range []string{"Started background AWS destroy job", "aws-destroy-xcode-vnc-20260701123045", "cm job status", "Elastic IP allocation will be retained"} {
		if !strings.Contains(text, want) {
			t.Fatalf("background output missing %q:\n%s", want, text)
		}
	}
	job, err := app.JobManager.Load("aws-destroy-xcode-vnc-20260701123045")
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if job.Profile != "xcode-vnc" || job.AppleEmail != "user@example.com" || !job.Notify {
		t.Fatalf("job = %#v", job)
	}
	joined := strings.Join(job.Command, " ")
	if !strings.Contains(joined, "aws destroy xcode-vnc --confirm --config "+config) {
		t.Fatalf("job command = %#v", job.Command)
	}
}

func TestAppJobListStatusAndLog(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	job, err := app.JobManager.Create(Job{
		Type:       "aws-destroy",
		Profile:    "xcode-vnc",
		AppleEmail: "user@example.com",
		Status:     JobStatusSuccess,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := os.WriteFile(job.Log, []byte("hello job\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if code := app.Run(context.Background(), []string{"job", "list"}); code != 0 {
		t.Fatalf("job list code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "ID") || !strings.Contains(out.String(), "xcode-vnc") || !strings.Contains(out.String(), "success") {
		t.Fatalf("job list output = %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"job", "status", job.ID}); code != 0 {
		t.Fatalf("job status code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Apple account: user@example.com") || !strings.Contains(out.String(), "Status: success") {
		t.Fatalf("job status output = %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"job", "log", job.ID}); code != 0 {
		t.Fatalf("job log code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "hello job") {
		t.Fatalf("job log output = %s", out.String())
	}
}

func TestJobManagerRunJobMarksStructuredDeferred(t *testing.T) {
	dir := t.TempDir()
	manager := NewJobManager(filepath.Join(dir, "jobs"))
	manager.Now = func() time.Time {
		return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	}
	manager.Notify = func(title, message string) error { return nil }
	job, err := manager.Create(Job{
		Type:    "aws-destroy",
		Profile: "xcode-vnc",
		Status:  JobStatusRunning,
		Command: []string{"/bin/sh", "-c",
			`printf '%s' '{"error_category":"recoverable","error_code":"host_transition","reason":"host is pending","deferred":true}' > "$CM_JOB_OUTCOME_PATH"`},
		Notify:      true,
		RunnerToken: "test-runner-token",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	t.Setenv(jobRunnerTokenEnv, "test-runner-token")
	job, err = manager.RunJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	if job.Status != JobStatusDeferred {
		t.Fatalf("status = %s", job.Status)
	}
	if job.ErrorCategory != JobErrorCategoryRecoverable || job.ErrorCode != "host_transition" || job.LastError != "host is pending" {
		t.Fatalf("structured outcome = %+v", job)
	}
	if job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatalf("exit code = %#v", job.ExitCode)
	}
}

func TestJobManagerCreateDefaultsToStartingWithCreator(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	job, err := manager.Create(Job{ID: "default-starting"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if job.Status != JobStatusStarting || job.CreatorPID != os.Getpid() || job.PID != 0 {
		t.Fatalf("default job = %#v", job)
	}

	legacy, err := manager.Create(Job{ID: "legacy-running", Status: JobStatusRunning})
	if err != nil {
		t.Fatalf("create legacy job: %v", err)
	}
	if legacy.Status != JobStatusRunning || legacy.CreatorPID != 0 {
		t.Fatalf("legacy job = %#v", legacy)
	}
}

func TestJobManagerReconcileStartingLease(t *testing.T) {
	now := time.Date(2026, 7, 2, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name        string
		creatorPID  int
		startedAt   time.Time
		ownerAlive  bool
		wantStatus  JobStatus
		wantChanged bool
	}{
		{name: "owner dead", creatorPID: 101, startedAt: now.Add(-time.Second), wantStatus: JobStatusInterrupted, wantChanged: true},
		{name: "owner alive within lease", creatorPID: 102, startedAt: now.Add(-59 * time.Second), ownerAlive: true, wantStatus: JobStatusStarting},
		{name: "lease expired", creatorPID: 103, startedAt: now.Add(-time.Minute), ownerAlive: true, wantStatus: JobStatusInterrupted, wantChanged: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
			manager.Now = func() time.Time { return now }
			manager.IsRunning = func(pid int) bool { return tc.ownerAlive && pid == tc.creatorPID }
			job, err := manager.Create(Job{ID: "starting", Status: JobStatusStarting, CreatorPID: tc.creatorPID, StartedAt: tc.startedAt})
			if err != nil {
				t.Fatalf("create job: %v", err)
			}
			changed, err := manager.Reconcile()
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if (len(changed) == 1) != tc.wantChanged {
				t.Fatalf("changed = %#v", changed)
			}
			got, err := manager.loadRaw(job.ID)
			if err != nil {
				t.Fatalf("load job: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", got.Status, tc.wantStatus)
			}
			if tc.wantChanged && got.LastError != interruptedJobError {
				t.Fatalf("last error = %q", got.LastError)
			}
		})
	}
}

func TestJobManagerReconcilePersistsInterruptedDeadRunningJob(t *testing.T) {
	dir := t.TempDir()
	finishedAt := time.Date(2026, 7, 2, 9, 30, 0, 0, time.UTC)
	manager := NewJobManager(filepath.Join(dir, "jobs"))
	manager.Now = func() time.Time { return finishedAt }
	manager.IsRunning = func(pid int) bool { return false }
	job, err := manager.Create(Job{
		ID:        "dead-job",
		Status:    JobStatusRunning,
		PID:       123,
		StartedAt: finishedAt.Add(-time.Hour),
		Command:   []string{"/bin/false"},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	changed, err := manager.Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !reflect.DeepEqual(jobIDs(changed), []string{job.ID}) {
		t.Fatalf("changed jobs = %#v", jobIDs(changed))
	}
	got, err := manager.Load(job.ID)
	if err != nil {
		t.Fatalf("load reconciled job: %v", err)
	}
	if got.Status != JobStatusInterrupted {
		t.Fatalf("status = %q, want interrupted", got.Status)
	}
	if !got.FinishedAt.Equal(finishedAt) {
		t.Fatalf("finished at = %s, want %s", got.FinishedAt, finishedAt)
	}
	if got.LastError != "background process exited before recording completion" {
		t.Fatalf("last error = %q", got.LastError)
	}
	data, err := os.ReadFile(mustJobPath(t, manager, job.ID))
	if err != nil {
		t.Fatalf("read persisted job: %v", err)
	}
	if !bytes.Contains(data, []byte(`"status": "interrupted"`)) {
		t.Fatalf("persisted job was not interrupted: %s", data)
	}
	lockInfo, err := os.Stat(filepath.Join(filepath.Dir(mustJobPath(t, manager, job.ID)), ".lock"))
	if err != nil {
		t.Fatalf("stat job lock: %v", err)
	}
	if lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("job lock mode = %o, want 600", lockInfo.Mode().Perm())
	}
}

func TestJobManagerReconcileLeavesLiveAndTerminalJobsUnchanged(t *testing.T) {
	dir := t.TempDir()
	manager := NewJobManager(filepath.Join(dir, "jobs"))
	manager.Now = func() time.Time { return time.Date(2026, 7, 2, 9, 30, 0, 0, time.UTC) }
	manager.IsRunning = func(pid int) bool { return pid == 101 }
	tests := []Job{
		{ID: "live", Status: JobStatusRunning, PID: 101},
		{ID: "pidless", Status: JobStatusRunning},
		{ID: "success", Status: JobStatusSuccess, PID: 202},
		{ID: "failed", Status: JobStatusFailed, PID: 203},
		{ID: "deferred", Status: JobStatusDeferred, PID: 204},
		{ID: "unknown", Status: JobStatusUnknown, PID: 205},
		{ID: "interrupted", Status: JobStatusInterrupted, PID: 206},
	}
	for _, job := range tests {
		if _, err := manager.Create(job); err != nil {
			t.Fatalf("create %s: %v", job.ID, err)
		}
	}
	changed, err := manager.Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("changed jobs = %#v", changed)
	}
	for _, want := range tests {
		got, err := manager.Load(want.ID)
		if err != nil {
			t.Fatalf("load %s: %v", want.ID, err)
		}
		if got.Status != want.Status || !got.FinishedAt.IsZero() || got.LastError != "" {
			t.Fatalf("job %s changed: %#v", want.ID, got)
		}
	}
}

func TestJobManagerLoadAndListReconcileDeadRunningJobs(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	manager.IsRunning = func(int) bool { return false }
	for _, id := range []string{"load-dead", "list-dead"} {
		if _, err := manager.Create(Job{ID: id, Status: JobStatusRunning, PID: 100}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	loaded, err := manager.Load("load-dead")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Status != JobStatusInterrupted {
		t.Fatalf("loaded status = %s", loaded.Status)
	}
	jobs, err := manager.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, job := range jobs {
		if job.Status == JobStatusRunning && job.PID > 0 {
			t.Fatalf("list returned stale running job: %#v", job)
		}
	}
}

func TestJobManagerReconcileRereadsUnderJobLock(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	manager.IsRunning = func(int) bool { return false }
	if _, err := manager.Create(Job{ID: "race-job", Status: JobStatusRunning, PID: 123}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	terminalSaved := make(chan error, 1)
	go func() {
		terminalSaved <- manager.withJobLock("race-job", func() error {
			close(locked)
			<-release
			job, err := manager.loadRaw("race-job")
			if err != nil {
				return err
			}
			job.Status = JobStatusSuccess
			job.FinishedAt = manager.Now()
			return manager.Save(job)
		})
	}()
	<-locked
	reconciled := make(chan error, 1)
	go func() {
		_, err := manager.Reconcile()
		reconciled <- err
	}()
	close(release)
	if err := <-terminalSaved; err != nil {
		t.Fatalf("save terminal: %v", err)
	}
	if err := <-reconciled; err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	job, err := manager.loadRaw("race-job")
	if err != nil {
		t.Fatalf("load final: %v", err)
	}
	if job.Status != JobStatusSuccess {
		t.Fatalf("terminal status overwritten: %#v", job)
	}
}

func TestJobManagerRunJobRequiresOneTimeRunnerTokenAndRejectsTerminal(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	output := filepath.Join(t.TempDir(), "ran")
	job, err := manager.Create(Job{
		ID:          "token-job",
		Status:      JobStatusRunning,
		Command:     []string{"/usr/bin/touch", output},
		RunnerToken: "one-time-token",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := manager.RunJob(context.Background(), job.ID); err == nil {
		t.Fatal("manual run error = nil")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manual run executed command: %v", err)
	}
	t.Setenv(jobRunnerTokenEnv, "one-time-token")
	completed, err := manager.RunJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("internal run: %v", err)
	}
	if completed.Status != JobStatusSuccess {
		t.Fatalf("completed status = %s", completed.Status)
	}
	if _, err := manager.RunJob(context.Background(), job.ID); err == nil {
		t.Fatal("terminal rerun error = nil")
	}
}

func TestJobManagerStartRunnerFailureMarksJobFailed(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	manager.Executable = filepath.Join(t.TempDir(), "missing-cm")
	finished := time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)
	manager.Now = func() time.Time { return finished }
	job, err := manager.Create(Job{ID: "startup-failure"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := manager.StartRunner(context.Background(), job); err == nil {
		t.Fatal("start runner error = nil")
	}
	got, err := manager.Load(job.ID)
	if err != nil {
		t.Fatalf("load failed job: %v", err)
	}
	if got.Status != JobStatusFailed || got.CreatorPID != 0 || !got.FinishedAt.Equal(finished) || got.LastError == "" {
		t.Fatalf("failed job = %#v", got)
	}
}

func TestJobManagerStartRunnerTransitionsStartingAndLegacyRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  Job
	}{
		{name: "starting", job: Job{ID: "starting-runner"}},
		{name: "legacy running", job: Job{ID: "legacy-runner", Status: JobStatusRunning}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
			executable, err := exec.LookPath("true")
			if err != nil {
				t.Fatalf("find true: %v", err)
			}
			manager.Executable = executable
			job, err := manager.Create(tc.job)
			if err != nil {
				t.Fatalf("create job: %v", err)
			}
			started, err := manager.StartRunner(context.Background(), job)
			if err != nil {
				t.Fatalf("start runner: %v", err)
			}
			if started.Status != JobStatusRunning || started.PID <= 0 || started.CreatorPID != 0 || started.RunnerToken == "" {
				t.Fatalf("started job = %#v", started)
			}
			persisted, err := manager.loadRaw(job.ID)
			if err != nil {
				t.Fatalf("load persisted job: %v", err)
			}
			if persisted.Status != JobStatusRunning || persisted.PID != started.PID || persisted.CreatorPID != 0 {
				t.Fatalf("persisted job = %#v", persisted)
			}
		})
	}
}

func TestJobManagerRejectsUnsafeAndMismatchedJobIDs(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	for _, id := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := manager.JobPath(id); err == nil {
			t.Fatalf("JobPath(%q) error = nil", id)
		}
	}
	if _, err := manager.Create(Job{ID: "requested", Status: JobStatusSuccess}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	path := mustJobPath(t, manager, "requested")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	data = bytes.Replace(data, []byte(`"id": "requested"`), []byte(`"id": "../escaped"`), 1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("corrupt ID: %v", err)
	}
	if _, err := manager.Load("requested"); err == nil {
		t.Fatal("mismatched ID load error = nil")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(path)), "escaped", "job.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected escaped write: %v", err)
	}
}

func TestJobManagerSaveIsAtomicAndReturnsErrors(t *testing.T) {
	dir := t.TempDir()
	manager := NewJobManager(filepath.Join(dir, "jobs"))
	job := Job{ID: "atomic", Status: JobStatusRunning}
	if err := manager.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}
	path := mustJobPath(t, manager, job.ID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat job: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("job mode = %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read job dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "job.json" {
		t.Fatalf("job dir entries = %#v", entries)
	}

	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	manager.Dir = blocked
	if err := manager.Save(Job{ID: "cannot-save"}); err == nil {
		t.Fatal("save error = nil")
	}
}

func TestJobManagerSaveRenameFailurePreservesOldJobAndCleansTempFile(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	original := Job{ID: "atomic-failure", Type: "original", Status: JobStatusRunning}
	if err := manager.Save(original); err != nil {
		t.Fatalf("save original: %v", err)
	}
	path := mustJobPath(t, manager, original.ID)
	oldData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	manager.Rename = func(oldPath, newPath string) error {
		if filepath.Dir(oldPath) != filepath.Dir(newPath) {
			t.Errorf("rename crossed directories: %s -> %s", oldPath, newPath)
		}
		return errors.New("injected rename failure")
	}
	updated := original
	updated.Type = "updated"
	if err := manager.Save(updated); err == nil || !strings.Contains(err.Error(), "injected rename failure") {
		t.Fatalf("save error = %v", err)
	}
	gotData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read preserved job: %v", err)
	}
	if !bytes.Equal(gotData, oldData) {
		t.Fatalf("job changed after rename failure:\nold=%s\nnew=%s", oldData, gotData)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read job dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "job.json" {
		t.Fatalf("temporary file was not cleaned up: %#v", entries)
	}
}

func TestJobManagerActiveReconcilesAndSorts(t *testing.T) {
	dir := t.TempDir()
	manager := NewJobManager(filepath.Join(dir, "jobs"))
	manager.Now = func() time.Time { return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC) }
	manager.IsRunning = func(pid int) bool { return pid != 20 }
	for _, job := range []Job{
		{ID: "older", Status: JobStatusRunning, PID: 10, StartedAt: manager.Now().Add(-time.Hour)},
		{ID: "dead", Status: JobStatusRunning, PID: 20, StartedAt: manager.Now().Add(-30 * time.Minute)},
		{ID: "newer", Status: JobStatusRunning, PID: 30, StartedAt: manager.Now().Add(-time.Minute)},
		{ID: "starting", Status: JobStatusStarting, CreatorPID: 40, StartedAt: manager.Now().Add(-30 * time.Second)},
		{ID: "done", Status: JobStatusSuccess, StartedAt: manager.Now()},
	} {
		if _, err := manager.Create(job); err != nil {
			t.Fatalf("create %s: %v", job.ID, err)
		}
	}
	active, err := manager.Active()
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if got := jobIDs(active); !reflect.DeepEqual(got, []string{"starting", "newer", "older"}) {
		t.Fatalf("active IDs = %#v", got)
	}
	dead, err := manager.Load("dead")
	if err != nil {
		t.Fatalf("load dead: %v", err)
	}
	if dead.Status != JobStatusInterrupted {
		t.Fatalf("dead status = %s", dead.Status)
	}
}

func TestJobManagerWaitAllIncludesStarting(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	manager.Now = func() time.Time { return now }
	manager.IsRunning = func(pid int) bool { return pid == 77 }
	manager.Sleep = func(context.Context, time.Duration) error {
		now = now.Add(time.Second)
		return nil
	}
	if _, err := manager.Create(Job{ID: "starting-wait", Status: JobStatusStarting, CreatorPID: 77, StartedAt: now}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	err := manager.WaitAll(context.Background(), time.Second, time.Second, nil)
	var timeoutErr *WaitAllTimeoutError
	if !errors.As(err, &timeoutErr) || !reflect.DeepEqual(jobIDs(timeoutErr.Active), []string{"starting-wait"}) {
		t.Fatalf("wait error = %T %v", err, err)
	}
}

func TestJobManagerActiveReturnsReadError(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	if _, err := manager.Create(Job{ID: "bad", Status: JobStatusRunning}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	path := mustJobPath(t, manager, "bad")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatalf("corrupt job: %v", err)
	}
	if _, err := manager.Active(); err == nil {
		t.Fatal("active read error = nil")
	}
}

func TestJobManagerActiveReturnsReconcileSaveError(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	manager.IsRunning = func(int) bool { return false }
	if _, err := manager.Create(Job{ID: "bad-save", Status: JobStatusRunning, PID: 77}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	manager.Rename = func(string, string) error {
		return errors.New("injected reconcile rename failure")
	}
	if _, err := manager.Active(); err == nil || !strings.Contains(err.Error(), "save reconciled job") {
		t.Fatalf("active save error = %v", err)
	}
}

func TestJobManagerWaitAllImmediateAndEventual(t *testing.T) {
	t.Run("immediate", func(t *testing.T) {
		manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
		calls := 0
		manager.Sleep = func(context.Context, time.Duration) error {
			calls++
			return nil
		}
		if err := manager.WaitAll(context.Background(), time.Hour, time.Second, nil); err != nil {
			t.Fatalf("wait all: %v", err)
		}
		if calls != 0 {
			t.Fatalf("sleep calls = %d, want 0", calls)
		}
	})

	t.Run("eventual", func(t *testing.T) {
		dir := t.TempDir()
		now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		running := true
		manager := NewJobManager(filepath.Join(dir, "jobs"))
		manager.Now = func() time.Time { return now }
		manager.IsRunning = func(int) bool { return running }
		manager.Sleep = func(ctx context.Context, duration time.Duration) error {
			now = now.Add(duration)
			running = false
			return nil
		}
		if _, err := manager.Create(Job{ID: "eventual", Status: JobStatusRunning, PID: 44}); err != nil {
			t.Fatalf("create job: %v", err)
		}
		progressCalls := 0
		err := manager.WaitAll(context.Background(), time.Minute, 10*time.Second, func(elapsed time.Duration, active []Job) {
			progressCalls++
			if elapsed != 10*time.Second || !reflect.DeepEqual(jobIDs(active), []string{"eventual"}) {
				t.Errorf("progress = %s %#v", elapsed, jobIDs(active))
			}
		})
		if err != nil {
			t.Fatalf("wait all: %v", err)
		}
		if progressCalls != 0 {
			t.Fatalf("progress calls = %d, want 0 after completion", progressCalls)
		}
	})

	t.Run("progress follows sleep", func(t *testing.T) {
		manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
		now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		nowCalls := 0
		manager.Now = func() time.Time {
			nowCalls++
			return now.Add(time.Duration(nowCalls) * time.Millisecond)
		}
		manager.IsRunning = func(int) bool { return true }
		sleepCalls := 0
		manager.Sleep = func(context.Context, time.Duration) error {
			sleepCalls++
			now = now.Add(time.Second)
			return nil
		}
		if _, err := manager.Create(Job{ID: "progress-order", Status: JobStatusRunning, PID: 55}); err != nil {
			t.Fatalf("create job: %v", err)
		}
		err := manager.WaitAll(context.Background(), time.Second, time.Second, func(time.Duration, []Job) {
			if sleepCalls == 0 {
				t.Error("progress called before first sleep")
			}
		})
		var timeoutErr *WaitAllTimeoutError
		if !errors.As(err, &timeoutErr) {
			t.Fatalf("error = %v, want timeout", err)
		}
	})
}

func TestJobManagerWaitAllTimeoutInvalidAndCanceled(t *testing.T) {
	newActiveManager := func(t *testing.T) (JobManager, *time.Time) {
		t.Helper()
		now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
		manager.Now = func() time.Time { return now }
		manager.IsRunning = func(int) bool { return true }
		if _, err := manager.Create(Job{ID: "still-running", Status: JobStatusRunning, PID: 99}); err != nil {
			t.Fatalf("create job: %v", err)
		}
		return manager, &now
	}

	t.Run("timeout", func(t *testing.T) {
		manager, now := newActiveManager(t)
		manager.Sleep = func(ctx context.Context, duration time.Duration) error {
			*now = now.Add(duration)
			return nil
		}
		progressCalls := 0
		err := manager.WaitAll(context.Background(), 20*time.Second, 10*time.Second, func(time.Duration, []Job) {
			progressCalls++
		})
		var timeoutErr *WaitAllTimeoutError
		if !errors.As(err, &timeoutErr) {
			t.Fatalf("error = %T %v, want WaitAllTimeoutError", err, err)
		}
		if !reflect.DeepEqual(jobIDs(timeoutErr.Active), []string{"still-running"}) {
			t.Fatalf("timeout active = %#v", jobIDs(timeoutErr.Active))
		}
		if progressCalls != 2 {
			t.Fatalf("progress calls = %d, want 2", progressCalls)
		}
	})

	for _, tc := range []struct {
		name     string
		timeout  time.Duration
		interval time.Duration
	}{
		{name: "zero timeout", interval: time.Second},
		{name: "negative timeout", timeout: -time.Second, interval: time.Second},
		{name: "zero interval", timeout: time.Second},
		{name: "negative interval", timeout: time.Second, interval: -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
			if err := manager.WaitAll(context.Background(), tc.timeout, tc.interval, nil); err == nil {
				t.Fatal("invalid duration error = nil")
			}
		})
	}

	t.Run("canceled", func(t *testing.T) {
		manager, _ := newActiveManager(t)
		ctx, cancel := context.WithCancel(context.Background())
		manager.Sleep = func(context.Context, time.Duration) error {
			cancel()
			return ctx.Err()
		}
		err := manager.WaitAll(ctx, time.Minute, time.Second, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	})

	t.Run("already canceled", func(t *testing.T) {
		manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := manager.WaitAll(ctx, time.Minute, time.Second, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	})
}

func TestJobManagerBeginDrainBlocksCreateAndEndDrainReopens(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	if err := manager.BeginDrain(); err != nil {
		t.Fatalf("begin drain: %v", err)
	}
	lockInfo, err := os.Stat(filepath.Join(manager.Dir, ".lock"))
	if err != nil {
		t.Fatalf("stat global lock: %v", err)
	}
	if lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("global lock mode = %o, want 600", lockInfo.Mode().Perm())
	}
	if _, err := manager.Create(Job{ID: "blocked", Status: JobStatusRunning}); !errors.Is(err, ErrJobsDraining) {
		t.Fatalf("create error = %v, want ErrJobsDraining", err)
	}
	if _, err := os.Stat(mustJobPath(t, manager, "blocked")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked job was written: %v", err)
	}
	if err := manager.EndDrain(); err != nil {
		t.Fatalf("end drain: %v", err)
	}
	if _, err := manager.Create(Job{ID: "allowed", Status: JobStatusRunning}); err != nil {
		t.Fatalf("create after drain: %v", err)
	}
}

func TestJobManagerCreateAndBeginDrainAreAtomic(t *testing.T) {
	for i := 0; i < 10; i++ {
		manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
		start := make(chan struct{})
		createResult := make(chan error, 1)
		drainResult := make(chan error, 1)
		go func() {
			<-start
			_, err := manager.Create(Job{ID: "racing", Status: JobStatusRunning})
			createResult <- err
		}()
		go func() {
			<-start
			drainResult <- manager.BeginDrain()
		}()
		close(start)
		createErr := <-createResult
		if err := <-drainResult; err != nil {
			t.Fatalf("begin drain: %v", err)
		}
		_, statErr := os.Stat(mustJobPath(t, manager, "racing"))
		switch {
		case createErr == nil && statErr == nil:
		case errors.Is(createErr, ErrJobsDraining) && errors.Is(statErr, os.ErrNotExist):
		default:
			t.Fatalf("inconsistent race result: create=%v stat=%v", createErr, statErr)
		}
	}
}

func mustJobPath(t *testing.T, manager JobManager, id string) string {
	t.Helper()
	path, err := manager.JobPath(id)
	if err != nil {
		t.Fatalf("job path: %v", err)
	}
	return path
}

func jobIDs(jobs []Job) []string {
	ids := make([]string, len(jobs))
	for i, job := range jobs {
		ids[i] = job.ID
	}
	return ids
}

func TestAppJobActiveTextAndJSON(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"job", "active"}); code != 0 {
		t.Fatalf("empty active code = %d, err = %s", code, errOut.String())
	}
	if out.String() != "No active jobs.\n" {
		t.Fatalf("empty active output = %q", out.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"job", "active", "--json"}); code != 0 {
		t.Fatalf("empty active JSON code = %d, err = %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("empty active JSON = %q", out.String())
	}

	for _, job := range []Job{
		{ID: "active-new", Type: "aws-destroy", Profile: "new", Status: JobStatusRunning, PID: 10, StartedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)},
		{ID: "active-old", Type: "aws-destroy", Profile: "old", Status: JobStatusRunning, PID: 11, StartedAt: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)},
		{ID: "completed", Status: JobStatusSuccess, StartedAt: time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)},
	} {
		if _, err := app.JobManager.Create(job); err != nil {
			t.Fatalf("create %s: %v", job.ID, err)
		}
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"job", "active"}); code != 0 {
		t.Fatalf("active code = %d, err = %s", code, errOut.String())
	}
	if text := out.String(); !strings.Contains(text, "ID") || !strings.Contains(text, "active-new") || !strings.Contains(text, "active-old") || strings.Contains(text, "completed") {
		t.Fatalf("active table = %s", text)
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"job", "active", "--json"}); code != 0 {
		t.Fatalf("active JSON code = %d, err = %s", code, errOut.String())
	}
	var jobs []Job
	if err := json.Unmarshal(out.Bytes(), &jobs); err != nil {
		t.Fatalf("decode active JSON: %v\n%s", err, out.String())
	}
	if got := jobIDs(jobs); !reflect.DeepEqual(got, []string{"active-new", "active-old"}) {
		t.Fatalf("active JSON IDs = %#v", got)
	}
}

func TestAppJobWaitAllImmediateProgressAndTimeout(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, t.TempDir())
		now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
		app.JobManager.Now = func() time.Time { return now }
		var sleepDurations []time.Duration
		app.JobManager.Sleep = func(_ context.Context, duration time.Duration) error {
			sleepDurations = append(sleepDurations, duration)
			now = now.Add(defaultJobWaitAllTimeout)
			return nil
		}
		if _, err := app.JobManager.Create(Job{ID: "job-defaults", Status: JobStatusRunning, PID: 42}); err != nil {
			t.Fatalf("create job: %v", err)
		}
		if code := app.Run(context.Background(), []string{"job", "wait-all"}); code != 1 {
			t.Fatalf("wait-all defaults code = %d, out = %s, err = %s", code, out.String(), errOut.String())
		}
		if !reflect.DeepEqual(sleepDurations, []time.Duration{10 * time.Second}) {
			t.Fatalf("sleep durations = %#v, want [10s]", sleepDurations)
		}
		if !strings.Contains(errOut.String(), "timed out after 2h0m0s") {
			t.Fatalf("default timeout error = %q", errOut.String())
		}
	})

	t.Run("immediate", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, t.TempDir())
		app.JobManager.Sleep = func(context.Context, time.Duration) error {
			t.Fatal("unexpected sleep")
			return nil
		}
		if code := app.Run(context.Background(), []string{"job", "wait-all", "--timeout", "1m", "--interval", "5s"}); code != 0 {
			t.Fatalf("wait-all code = %d, err = %s", code, errOut.String())
		}
		if !strings.Contains(out.String(), "All background jobs completed.") {
			t.Fatalf("wait-all output = %q", out.String())
		}
	})

	t.Run("progress then complete", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, t.TempDir())
		now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
		running := true
		sleeps := 0
		app.JobManager.Now = func() time.Time { return now }
		app.JobManager.IsRunning = func(int) bool { return running }
		app.JobManager.Sleep = func(context.Context, time.Duration) error {
			sleeps++
			now = now.Add(10 * time.Second)
			if sleeps == 2 {
				running = false
			}
			return nil
		}
		if _, err := app.JobManager.Create(Job{ID: "job-progress", Status: JobStatusRunning, PID: 42}); err != nil {
			t.Fatalf("create job: %v", err)
		}
		if code := app.Run(context.Background(), []string{"job", "wait-all", "--timeout", "1m", "--interval", "10s"}); code != 0 {
			t.Fatalf("wait-all code = %d, err = %s", code, errOut.String())
		}
		if text := out.String(); !strings.Contains(text, "elapsed=10s") || !strings.Contains(text, "job-progress") || !strings.Contains(text, "All background jobs completed.") {
			t.Fatalf("wait-all progress = %q", text)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, t.TempDir())
		now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
		app.JobManager.Now = func() time.Time { return now }
		app.JobManager.Sleep = func(context.Context, time.Duration) error {
			now = now.Add(10 * time.Second)
			return nil
		}
		if _, err := app.JobManager.Create(Job{ID: "job-timeout", Status: JobStatusRunning, PID: 42}); err != nil {
			t.Fatalf("create job: %v", err)
		}
		if code := app.Run(context.Background(), []string{"job", "wait-all", "--timeout", "20s", "--interval", "10s"}); code != 1 {
			t.Fatalf("wait-all timeout code = %d, out = %s, err = %s", code, out.String(), errOut.String())
		}
		if text := errOut.String(); !strings.Contains(text, "timed out after 20s") || !strings.Contains(text, "job-timeout") {
			t.Fatalf("wait-all timeout error = %q", text)
		}
	})
}

func TestAppJobWaitWaitsForStarting(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	app.JobManager.IsRunning = func(pid int) bool { return pid == os.Getpid() }
	job, err := app.JobManager.Create(Job{ID: "starting-wait"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := app.Run(ctx, []string{"job", "wait", job.ID}); code != 1 {
		t.Fatalf("wait code = %d, out=%s err=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "job wait canceled: context canceled") || strings.Contains(out.String(), "Status:") {
		t.Fatalf("wait output=%q err=%q", out.String(), errOut.String())
	}
}

func TestAppUsageIncludesJobActiveAndWaitAll(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), nil); code != 0 {
		t.Fatalf("help code = %d, err = %s", code, errOut.String())
	}
	for _, want := range []string{
		"cm job active\n",
		"cm job active --json\n",
		"cm job wait-all [--timeout 2h] [--interval 10s] [--drain]\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("usage missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "cm job run") {
		t.Fatalf("usage exposes internal job run:\n%s", out.String())
	}
}

func TestAppJobWaitAllStrictArgumentsAndCancellation(t *testing.T) {
	invalid := [][]string{
		{"job", "wait-all", "--wat"},
		{"job", "wait-all", "--timeout"},
		{"job", "wait-all", "--interval"},
		{"job", "wait-all", "--timeout", "1m", "--timeout", "2m"},
		{"job", "wait-all", "--interval", "1s", "--interval", "2s"},
		{"job", "wait-all", "--timeout", "nope"},
		{"job", "wait-all", "--interval", "0s"},
		{"job", "wait-all", "--timeout", "-1s"},
		{"job", "wait-all", "--drain", "--drain"},
	}
	for _, args := range invalid {
		t.Run(strings.Join(args[2:], "_"), func(t *testing.T) {
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, t.TempDir())
			if code := app.Run(context.Background(), args); code != 2 {
				t.Fatalf("code = %d, want 2; out=%s err=%s", code, out.String(), errOut.String())
			}
		})
	}

	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if _, err := app.JobManager.Create(Job{ID: "job-cancel", Status: JobStatusRunning, PID: 42}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app.JobManager.Sleep = func(context.Context, time.Duration) error {
		cancel()
		return ctx.Err()
	}
	if code := app.Run(ctx, []string{"job", "wait-all", "--timeout", "1m", "--interval", "10s"}); code != 1 {
		t.Fatalf("cancel code = %d, out=%s err=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "context canceled") {
		t.Fatalf("cancel error = %q", errOut.String())
	}
}

func TestAppJobWaitAllDrainCleanupAndSuccessMarker(t *testing.T) {
	t.Run("success retains marker", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, t.TempDir())
		if code := app.Run(context.Background(), []string{"job", "wait-all", "--drain"}); code != 0 {
			t.Fatalf("wait-all drain code = %d, err = %s", code, errOut.String())
		}
		if _, err := app.JobManager.Create(Job{ID: "blocked-after-success"}); !errors.Is(err, ErrJobsDraining) {
			t.Fatalf("create error = %v, want draining", err)
		}
		if err := app.JobManager.EndDrain(); err != nil {
			t.Fatalf("end drain: %v", err)
		}
	})

	t.Run("timeout clears marker", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, t.TempDir())
		now := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
		app.JobManager.Now = func() time.Time { return now }
		app.JobManager.Sleep = func(_ context.Context, duration time.Duration) error {
			now = now.Add(duration)
			return nil
		}
		if _, err := app.JobManager.Create(Job{ID: "active-timeout", Status: JobStatusRunning}); err != nil {
			t.Fatalf("create active job: %v", err)
		}
		if code := app.Run(context.Background(), []string{"job", "wait-all", "--drain", "--timeout", "10s", "--interval", "10s"}); code != 1 {
			t.Fatalf("timeout code = %d, err = %s", code, errOut.String())
		}
		if _, err := app.JobManager.Create(Job{ID: "after-timeout"}); err != nil {
			t.Fatalf("marker retained after timeout: %v", err)
		}
	})

	t.Run("cancel clears marker", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, t.TempDir())
		if _, err := app.JobManager.Create(Job{ID: "active-cancel", Status: JobStatusRunning}); err != nil {
			t.Fatalf("create active job: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		app.JobManager.Sleep = func(context.Context, time.Duration) error {
			cancel()
			return ctx.Err()
		}
		if code := app.Run(ctx, []string{"job", "wait-all", "--drain"}); code != 1 {
			t.Fatalf("cancel code = %d, err = %s", code, errOut.String())
		}
		if _, err := app.JobManager.Create(Job{ID: "after-cancel"}); err != nil {
			t.Fatalf("marker retained after cancel: %v", err)
		}
	})

	t.Run("error clears marker", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, t.TempDir())
		if _, err := app.JobManager.Create(Job{ID: "corrupt", Status: JobStatusRunning}); err != nil {
			t.Fatalf("create corrupt job: %v", err)
		}
		if err := os.WriteFile(mustJobPath(t, app.JobManager, "corrupt"), []byte("{"), 0o600); err != nil {
			t.Fatalf("corrupt job: %v", err)
		}
		if code := app.Run(context.Background(), []string{"job", "wait-all", "--drain"}); code != 1 {
			t.Fatalf("error code = %d, err = %s", code, errOut.String())
		}
		if _, err := app.JobManager.Create(Job{ID: "after-error"}); err != nil {
			t.Fatalf("marker retained after error: %v", err)
		}
	})
}

func TestAppJobEndDrainIsInternalRecoveryCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if err := app.JobManager.BeginDrain(); err != nil {
		t.Fatalf("begin drain: %v", err)
	}
	if code := app.Run(context.Background(), []string{"job", "end-drain"}); code != 0 {
		t.Fatalf("end-drain code = %d, err = %s", code, errOut.String())
	}
	if _, err := app.JobManager.Create(Job{ID: "after-end-drain"}); err != nil {
		t.Fatalf("create after end-drain: %v", err)
	}
	out.Reset()
	app.printUsage()
	if strings.Contains(out.String(), "end-drain") {
		t.Fatal("internal recovery command must not appear in public usage")
	}
}

func TestAppJobRunIsInternalOnly(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	job, err := app.JobManager.Create(Job{ID: "manual-run", Status: JobStatusRunning, RunnerToken: "secret"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if code := app.Run(context.Background(), []string{"job", "run", job.ID}); code != 1 {
		t.Fatalf("manual run code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "restricted to the internal background runner") {
		t.Fatalf("manual run error = %q", errOut.String())
	}
}

func TestAppJobCompletionCommandsAndOptions(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"completion", "job-commands"}); code != 0 {
		t.Fatalf("job commands code = %d, err = %s", code, errOut.String())
	}
	for _, want := range []string{"list", "status", "log", "wait", "active", "wait-all"} {
		if !strings.Contains(out.String(), want+"\n") {
			t.Fatalf("job commands missing %q: %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "run\n") {
		t.Fatalf("job completion exposes internal run: %q", out.String())
	}
	for _, shell := range []string{"zsh", "bash", "fish"} {
		out.Reset()
		errOut.Reset()
		if code := app.Run(context.Background(), []string{"completion", shell}); code != 0 {
			t.Fatalf("%s completion code = %d, err = %s", shell, code, errOut.String())
		}
		for _, want := range []string{"active", "wait-all", "--json", "--timeout", "--interval", "--drain"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%s completion missing %q", shell, want)
			}
		}
	}
}

func TestAppAWSRunningListsRunningInstances(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Hosts: []DedicatedHostStatus{{HostID: "h-1", State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{
				InstanceID:          "i-1",
				State:               "running",
				InstanceType:        "mac2.metal",
				HostID:              "h-1",
				PublicIP:            "203.0.113.10",
				SystemStatus:        "ok",
				InstanceStatusCheck: "ok",
				EBSStatus:           "ok",
				Tags:                managedTestTags(),
			}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", InstanceID: "i-1", PublicIP: "203.0.113.10"},
		}}, nil
	}
	if code := app.Run(context.Background(), []string{"aws", "running", "--config", config}); code != 0 {
		t.Fatalf("aws running code = %d, err = %s", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{"APPLE ACCOUNT", "user@example.com", "xcode-vnc", "usw2-az1", "mac2.metal", "i-1", "true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("aws running output missing %q:\n%s", want, text)
		}
	}
}

func TestAppAWSDestroyManyPreview(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "available", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "running", Tags: managedTestTags()}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", AssociationID: "eipassoc-1", InstanceID: "i-1", PublicIP: "203.0.113.10"},
		}}, nil
	}
	if code := app.Run(context.Background(), []string{"aws", "destroy-many", "user@example.com", "--config", config}); code != 0 {
		t.Fatalf("aws destroy-many code = %d, err = %s", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{"Resolved Apple account user@example.com -> profile xcode-vnc", "AWS Mac destroy preview", "Preview only. Include --confirm"} {
		if !strings.Contains(text, want) {
			t.Fatalf("destroy-many output missing %q:\n%s", want, text)
		}
	}
}

func TestAppAWSStatusAllIncludesTerminalResources(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			CallerIdentity: CallerIdentity{Account: "123456789012", ARN: "arn:aws:iam::123456789012:user/cm"},
			Hosts: []DedicatedHostStatus{
				{HostID: "h-active", State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1", Tags: managedTestTags()},
				{HostID: "h-released", State: "released", InstanceType: "mac2.metal", ZoneID: "usw2-az1", Tags: managedTestTags()},
			},
			Instances: []InstanceStatus{
				{InstanceID: "i-active", State: "running", Tags: managedTestTags()},
				{InstanceID: "i-terminated", State: "terminated", Tags: managedTestTags()},
			},
		}}, nil
	}
	if code := app.Run(context.Background(), []string{"aws", "status", "xcode-vnc", "--config", config}); code != 0 {
		t.Fatalf("aws status code = %d, err = %s", code, errOut.String())
	}
	if strings.Contains(out.String(), "h-released") || strings.Contains(out.String(), "i-terminated") {
		t.Fatalf("default status should hide terminal resources:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Use --all") {
		t.Fatalf("default status should mention --all:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run(context.Background(), []string{"aws", "status", "xcode-vnc", "--all", "--config", config}); code != 0 {
		t.Fatalf("aws status --all code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "h-released") || !strings.Contains(out.String(), "i-terminated") {
		t.Fatalf("status --all should include terminal resources:\n%s", out.String())
	}
}

func TestAppAWSAdoptHostPreview(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{host: DedicatedHostStatus{HostID: "h-empty", State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1"}}, nil
	}
	if code := app.Run(context.Background(), []string{"aws", "adopt-host", "xcode-vnc", "--host-id", "h-empty", "--config", config}); code != 0 {
		t.Fatalf("aws adopt-host code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "AWS Mac adopt-host preview") || !strings.Contains(out.String(), "Preview only") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestAppAWSLaunchOnHostRequiresHostID(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if code := app.Run(context.Background(), []string{"aws", "launch-on-host", "xcode-vnc", "--config", config}); code != 2 {
		t.Fatalf("aws launch-on-host code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "--host-id is required") {
		t.Fatalf("err = %q", errOut.String())
	}
}

func TestAppUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if code := app.Run(context.Background(), []string{"wat"}); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("err output = %q", errOut.String())
	}
}

func TestAppWebDesktopLayoutStabilizationContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	page := string(data)

	for _, want := range []string{
		`<div class="table-scroll profile-table-scroll">`,
		`<tbody id="profiles"></tbody>`,
		`<div class="table-scroll member-table-scroll">`,
		`<tbody id="members"></tbody>`,
		`<div class="jobs">`,
		`.app-layout.container-fluid { width: 100%; max-width: 2400px; margin-inline: auto; }`,
		`.table-scroll { min-width: 0; overflow-x: auto; overscroll-behavior-inline: contain; }`,
		`.profile-table-scroll table { min-width: 960px; }`,
		`.member-table-scroll table { min-width: 900px; }`,
		`.picker-card { min-width: 0; max-width: calc(100vw - 36px); }`,
		`.profile-form-grid { min-height: 0; }`,
		`input[type="datetime-local"] { min-width: 0; }`,
		`.terminal-surface { height: clamp(420px, 62vh, 720px); min-height: 0; max-height: none; }`,
		`main, .view, .section-head > *, .toolbar, .row-actions { min-width: 0; }`,
		`.picker-layer { overflow: auto; }`,
		`.xterm, .xterm-screen, .xterm-viewport { max-width: 100%; }`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("web desktop layout contract missing %q", want)
		}
	}

	mobileStart := strings.Index(page, "@media (max-width: 720px) {")
	if mobileStart < 0 {
		t.Fatal("mobile layout contract block is missing")
	}
	mobileEnd := strings.Index(page[mobileStart:], "    }\n    /* Bootstrap 5.3")
	if mobileEnd < 0 {
		t.Fatal("mobile layout contract block is missing")
	}
	mobile := page[mobileStart : mobileStart+mobileEnd]
	for _, want := range []string{
		`.profile-table-scroll table, .member-table-scroll table { min-width: 0; }`,
		`.table-scroll { overflow-x: visible; }`,
		`table, thead, tbody, tr, th, td { display: block; width: 100%; }`,
	} {
		if !strings.Contains(mobile, want) {
			t.Errorf("web mobile layout contract missing %q", want)
		}
	}
	if !strings.Contains(page, `class="local-action mobile-local-hidden"`) {
		t.Error("web local operations must use the dedicated mobile capability class")
	}
}

func TestAppLogsCommands(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	if err := app.LogManager.Write(LogEntry{Level: "error", Action: "test", Message: "hello"}); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if code := app.Run(context.Background(), []string{"logs", "list"}); code != 0 {
		t.Fatalf("logs list code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "cm-2026-07-01.log") {
		t.Fatalf("logs list output = %s", out.String())
	}
	out.Reset()
	errOut.Reset()
	exportPath := filepath.Join(dir, "logs.zip")
	if code := app.Run(context.Background(), []string{"logs", "export", "--output", exportPath}); code != 0 {
		t.Fatalf("logs export code = %d, err = %s", code, errOut.String())
	}
	if _, err := os.Stat(exportPath); err != nil {
		t.Fatalf("expected export zip: %v", err)
	}
	if !strings.Contains(out.String(), "exported logs") {
		t.Fatalf("logs export output = %s", out.String())
	}
}

func TestParseLogsExportArgsSupportsIncludeRawInEitherOrder(t *testing.T) {
	for _, args := range [][]string{
		{"--include-raw", "--output", "logs.zip"},
		{"--output", "logs.zip", "--include-raw"},
	} {
		options, err := parseLogsExportArgs(args)
		if err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		if !options.IncludeRaw || options.Destination != "logs.zip" {
			t.Fatalf("parse %v = %+v", args, options)
		}
	}
}

func TestParseLogsExportArgsRejectsMalformedAndDuplicateOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--output", "--include-raw"},
		{"--output", "-o"},
		{"--output", "one.zip", "--output", "two.zip"},
		{"--include-raw", "--include-raw"},
	} {
		if _, err := parseLogsExportArgs(args); err == nil {
			t.Fatalf("parse %v unexpectedly succeeded", args)
		}
	}
}

func testApp(out, errOut *bytes.Buffer, stateDir string) App {
	return App{
		Out:       out,
		Err:       errOut,
		Runner:    &fakeRunner{},
		Validator: NewValidatorForTest(nil),
		StateManager: StateManager{
			Dir:       filepath.Join(stateDir, "state"),
			IsRunning: func(pid int) bool { return pid == 55 },
			InspectProcess: func(pid int) (ProcessIdentity, error) {
				return testProcessIdentity("ssh fake-managed-tunnel", fmt.Sprintf("start-%d", pid)), nil
			},
			TerminateProcess: func(state State, verify func(State) error, stop func(int) error) error {
				if err := verify(state); err != nil {
					return err
				}
				return stop(state.PID)
			},
			Preflight:     func() error { return nil },
			SyncDirectory: func(string) error { return nil },
			CommandMatches: func(actual, expected string) bool {
				return actual == "ssh fake-managed-tunnel" || normalizeSSHCommand(actual) == normalizeSSHCommand(expected)
			},
			FingerprintMatches: func(actual, fingerprint string) bool {
				return actual == "ssh fake-managed-tunnel" || commandFingerprint(actual) == fingerprint
			},
		},
		JobManager: JobManager{
			Dir:        filepath.Join(stateDir, "jobs"),
			Executable: "/bin/echo",
			Now: func() time.Time {
				return time.Date(2026, 7, 1, 12, 30, 45, 0, time.UTC)
			},
			IsRunning: func(pid int) bool { return pid > 0 },
			Notify:    func(title, message string) error { return nil },
		},
		AWSService: AWSService{Now: func() time.Time {
			return time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
		}},
		WebDir:      filepath.Join("..", "..", "web"),
		MemberStore: NewMemberStore(filepath.Join(stateDir, "members.json")),
		LogManager: LogManager{
			Dir: filepath.Join(stateDir, "logs"),
			Now: func() time.Time {
				return time.Date(2026, 7, 1, 12, 30, 45, 0, time.UTC)
			},
		},
		SyncHistory:    NewSyncHistoryStore(filepath.Join(stateDir, "sync-history.json")),
		LocalTransfers: NewLocalTransferJobManager(),
		KnownHosts:     filepath.Join(stateDir, ".ssh", "known_hosts"),
		LocalAgentBrowserOrigins: map[string]struct{}{
			"https://cm.hsgitlab.xyz": {},
		},
	}
}

func addWebAuth(t *testing.T, app *App, req *http.Request, role string) {
	t.Helper()
	member, err := app.MemberStore.SetupAdmin("Test Admin", "admin@example.com", "password123")
	if err != nil {
		t.Fatalf("setup admin: %v", err)
	}
	if role != "" && role != "admin" {
		member.Role = role
		db, err := app.MemberStore.Load()
		if err != nil {
			t.Fatalf("load members: %v", err)
		}
		for i := range db.Members {
			if db.Members[i].Email == member.Email {
				db.Members[i].Role = role
			}
		}
		if err := app.MemberStore.Save(db); err != nil {
			t.Fatalf("save members: %v", err)
		}
	}
	rec := httptest.NewRecorder()
	if err := app.setWebSession(rec, member); err != nil {
		t.Fatalf("set web session: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected auth cookie")
	}
	req.AddCookie(cookies[0])
}

func webChallengeForTest(t *testing.T, app App) map[string]string {
	t.Helper()
	challenge, err := app.newWebChallenge()
	if err != nil {
		t.Fatalf("new challenge: %v", err)
	}
	token := challenge["token"]
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 {
		t.Fatalf("challenge parts = %#v", parts)
	}
	return map[string]string{"token": token, "answer": parts[1]}
}

func writeConfig(t *testing.T, dir, key string) string {
	t.Helper()
	config := strings.ReplaceAll(sampleConfig, "~/.ssh/example.pem", key)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
