package connectmac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestLocalAgentRequestIDValidationAndCorrelation(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(`{}`))
	req.Header.Set("X-Request-ID", "local-browser-123")
	got, err := validatedLocalAgentRequestID(req, "local-browser-123")
	if err != nil || got != "local-browser-123" {
		t.Fatalf("validated request ID = %q, %v", got, err)
	}
	if _, err := validatedLocalAgentRequestID(req, "different"); err == nil {
		t.Fatal("expected mismatched request ID rejection")
	}
	if _, err := validateLocalAgentRequestID("bad request id"); err == nil {
		t.Fatal("expected invalid character rejection")
	}

	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	ctx := withOperationContext(context.Background(), OperationContext{
		RequestID: "local-browser-123",
		Source:    "web-local",
	})
	app.logLocalCommand(ctx, "ssh.succeeded", Profile{Name: "shared"}, 0, time.Now(), LogEntry{})
	entries := readTestLogEntries(t, app.LogManager)
	if len(entries) != 1 || entries[0].RequestID != "local-browser-123" || entries[0].Source != "web-local" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestLocalAgentStartupReconcilesInterruptedTransfers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	if err := app.LogManager.Write(LogEntry{
		Action: "transfer.local.started", TransferID: "startup-orphan",
		LocalJobID: "local-job-1", Profile: "mac-one", Direction: "pull",
		Status: LocalTransferRunning, Phase: TransferPhasePreparing,
		RequestID: "request-1", Source: "web-local", Message: "started",
	}); err != nil {
		t.Fatal(err)
	}

	opts := localAgentUnusedOptions(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan int, 1)
	go func() {
		result <- app.runLocalAgent(ctx, filepath.Join(home, "missing-config.yaml"), []string{
			"--host", opts.Host, "--port", fmt.Sprint(opts.Port),
		})
	}()
	waitForLocalAgentEndpoint(t, localAgentEndpoint(opts, false, "/health"), localAgentTLSMaterial{})
	cancel()
	if code := <-result; code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}

	entries := readTestLogEntries(t, app.LogManager)
	count := 0
	for _, entry := range entries {
		if entry.TransferID == "startup-orphan" && entry.Action == "transfer.local.interrupted" {
			count++
			if entry.Source != "local-agent-recovery" || entry.ErrorCode != "agent_restarted" {
				t.Fatalf("recovery entry = %+v", entry)
			}
		}
	}
	if count != 1 {
		t.Fatalf("recovery count=%d entries=%+v", count, entries)
	}
}

func TestLocalAgentRecoveryFailureDoesNotBlockStartup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, home)
	if err := os.MkdirAll(app.LogManager.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	brokenPath := filepath.Join(app.LogManager.Dir, "cm-2026-06-30.log")
	if err := os.Symlink(filepath.Join(home, "missing-log-target"), brokenPath); err != nil {
		t.Fatal(err)
	}

	opts := localAgentUnusedOptions(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan int, 1)
	go func() {
		result <- app.runLocalAgent(ctx, filepath.Join(home, "missing-config.yaml"), []string{
			"--host", opts.Host, "--port", fmt.Sprint(opts.Port),
		})
	}()
	waitForLocalAgentEndpoint(t, localAgentEndpoint(opts, false, "/health"), localAgentTLSMaterial{})
	cancel()
	if code := <-result; code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	raw, err := os.ReadFile(filepath.Join(app.LogManager.Dir, "cm-2026-07-01.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"action":"local-agent.recovery.failed"`)) {
		t.Fatalf("recovery failure event missing from log tail")
	}
}

func TestLocalTransferCanceledLogIsDistinctFromInterrupted(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	app.writeLocalTransferEventWithRequest(LocalTransferEvent{
		TransferID: "canceled-transfer", LocalJobID: "job-1", Profile: "mac-one",
		Direction: "push", Status: LocalTransferCanceled, Phase: TransferPhaseInterrupted,
		Percent: 48, Error: context.Canceled.Error(),
	}, "request-1")

	entries := readTestLogEntries(t, app.LogManager)
	if len(entries) != 1 || entries[0].Action != "transfer.local.canceled" ||
		entries[0].Status != LocalTransferCanceled || entries[0].Level != "warn" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestLocalAgentBrowserBoundaryRequiresExactConfiguredOrigin(t *testing.T) {
	app := testApp(&bytes.Buffer{}, &bytes.Buffer{}, t.TempDir())
	app.LocalAgentBrowserOrigins = map[string]struct{}{
		"https://cm.hsgitlab.xyz": {},
		"http://127.0.0.1:4173":   {},
	}
	handler := app.newLocalAgentHandler()
	protected := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/terminal/check"},
		{http.MethodGet, "/terminal/ws"},
		{http.MethodPost, "/start"},
		{http.MethodPost, "/open-vnc"},
		{http.MethodPost, "/sync/push"},
		{http.MethodPost, "/sync/pull"},
		{http.MethodPost, "/ssh"},
	}
	for _, endpoint := range protected {
		t.Run(endpoint.path+" missing origin", func(t *testing.T) {
			req := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
		t.Run(endpoint.path+" arbitrary localhost", func(t *testing.T) {
			req := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(`{}`))
			req.Header.Set("Origin", "http://localhost:9999")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	for _, origin := range []string{"https://cm.hsgitlab.xyz", "http://127.0.0.1:4173"} {
		req := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(`{}`))
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("configured origin %q rejected: %s", origin, rec.Body.String())
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Fatalf("allow origin=%q", rec.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}

func TestLocalAgentOriginConfigurationUsesExactOrigins(t *testing.T) {
	cfg, err := ParseConfig(`server:
  user_api: https://cm.hsgitlab.xyz/api
  local_agent_origin: http://127.0.0.1:4173
profiles:
`)
	if err != nil {
		t.Fatal(err)
	}
	origins, err := localAgentBrowserOrigins(cfg.Server)
	if err != nil {
		t.Fatal(err)
	}
	if len(origins) != 2 {
		t.Fatalf("origins=%v", origins)
	}
	for _, origin := range []string{"https://cm.hsgitlab.xyz", "http://127.0.0.1:4173"} {
		if _, ok := origins[origin]; !ok {
			t.Fatalf("missing exact origin %q in %v", origin, origins)
		}
	}
	if _, ok := origins["http://localhost:4173"]; ok {
		t.Fatalf("localhost alias must not be allowed: %v", origins)
	}
}

func TestExecLogsLifecycleWithoutCommandContent(t *testing.T) {
	for _, test := range []struct {
		name      string
		runnerErr error
		wantLast  string
	}{
		{name: "success", wantLast: "ssh.exec.succeeded"},
		{name: "failure", runnerErr: errors.New("exit status 255"), wantLast: "ssh.exec.failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, dir)
			runner := &fakeRunner{foregroundErr: test.runnerErr}
			app.Runner = runner
			profile := validProfile(writeSSHKey(t, 0o600))
			profile.Name = "exec-profile"
			ctx := withOperationContext(context.Background(), OperationContext{
				RequestID: "exec-request-123",
				Source:    "cli",
			})

			code := app.runExec(ctx, Config{Profiles: map[string]Profile{profile.Name: profile}},
				[]string{profile.Name, "--", "echo", "super-secret-command"})
			if test.runnerErr == nil && code != 0 {
				t.Fatalf("code=%d err=%s", code, errOut.String())
			}
			if test.runnerErr != nil && code == 0 {
				t.Fatal("expected failure")
			}
			entries := readTestLogEntries(t, app.LogManager)
			if len(entries) != 2 {
				t.Fatalf("entries=%+v", entries)
			}
			if entries[0].Action != "ssh.exec.attempted" || entries[1].Action != test.wantLast {
				t.Fatalf("entries=%+v", entries)
			}
			for _, entry := range entries {
				if entry.Profile != profile.Name || entry.RequestID != "exec-request-123" ||
					entry.Source != "cli" || entry.DurationMS < 0 {
					t.Fatalf("entry=%+v", entry)
				}
				if strings.Contains(entry.Message, "super-secret-command") ||
					strings.Contains(entry.Message, "echo") {
					t.Fatalf("command leaked in log: %+v", entry)
				}
			}
			raw := readTestLogsRaw(t, app.LogManager)
			for _, forbidden := range []string{"super-secret-command", `"echo"`, strings.Join(runner.foreground, " ")} {
				if forbidden != "" && strings.Contains(raw, forbidden) {
					t.Fatalf("command or arguments leaked in log: %s", raw)
				}
			}
		})
	}
}

func TestLocalCommandLifecycleLogContract(t *testing.T) {
	t.Run("ssh", func(t *testing.T) {
		dir := t.TempDir()
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, dir)
		profile := validProfile(writeSSHKey(t, 0o600))
		profile.Name = "ssh-profile"
		ctx := localLifecycleTestContext("ssh-request")
		if code := app.runSSH(ctx, Config{Profiles: map[string]Profile{profile.Name: profile}}, []string{profile.Name}); code != 0 {
			t.Fatalf("code=%d err=%s", code, errOut.String())
		}
		assertLocalLifecycleEntries(t, app.LogManager, "ssh-profile", "ssh-request",
			[]string{"ssh.attempted", "ssh.succeeded"})
	})

	t.Run("tunnel", func(t *testing.T) {
		dir := t.TempDir()
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, dir)
		runner := &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
		app.Runner = runner
		profile := validProfile(writeSSHKey(t, 0o600))
		profile.Name = "tunnel-profile"
		ctx := localLifecycleTestContext("tunnel-request")
		if code := app.runStart(ctx, Config{Profiles: map[string]Profile{profile.Name: profile}}, []string{profile.Name}); code != 0 {
			t.Fatalf("code=%d err=%s", code, errOut.String())
		}
		entries := assertLocalLifecycleEntries(t, app.LogManager, profile.Name, "tunnel-request",
			[]string{"tunnel.started"})
		if entries[0].PID != 55 || len(entries[0].LocalPorts) != 1 ||
			entries[0].LocalPorts[0] != profile.Tunnels[0].LocalPort ||
			entries[0].TunnelAction != "started" {
			t.Fatalf("tunnel entry=%+v", entries[0])
		}
	})

	t.Run("host key", func(t *testing.T) {
		dir := t.TempDir()
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, dir)
		app.Runner = &fakeRunner{knownHost: "mac-host.example.com ssh-ed25519 AAAACURRENT\n"}
		profile := validProfile(writeSSHKey(t, 0o600))
		profile.Name = "host-key-profile"
		ctx := localLifecycleTestContext("host-key-request")
		if code := app.runHostKey(ctx, Config{Profiles: map[string]Profile{profile.Name: profile}},
			[]string{"check", profile.Name}); code != 0 {
			t.Fatalf("code=%d err=%s", code, errOut.String())
		}
		assertLocalLifecycleEntries(t, app.LogManager, profile.Name, "host-key-request",
			[]string{"known-host.checked"})
	})

	t.Run("sync", func(t *testing.T) {
		dir := t.TempDir()
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, dir)
		profile := validProfile(writeSSHKey(t, 0o600))
		profile.Name = "sync-profile"
		localPath := filepath.Join(dir, "payload.txt")
		if err := os.WriteFile(localPath, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := localLifecycleTestContext("sync-request")
		cfg := Config{Profiles: map[string]Profile{profile.Name: profile}}
		if code := app.runPush(ctx, cfg, []string{profile.Name, localPath, "~/Documents/"}); code != 0 {
			t.Fatalf("push code=%d err=%s", code, errOut.String())
		}
		if code := app.runPull(ctx, cfg, []string{profile.Name, "~/Documents/payload.txt"}); code != 0 {
			t.Fatalf("pull code=%d err=%s", code, errOut.String())
		}
		assertLocalLifecycleEntries(t, app.LogManager, profile.Name, "sync-request", []string{
			"sync.push.started", "sync.push.succeeded", "sync.pull.started", "sync.pull.succeeded",
		})
	})
}

func localLifecycleTestContext(requestID string) context.Context {
	return withOperationContext(context.Background(), OperationContext{
		RequestID: requestID,
		Source:    "cli",
	})
}

func assertLocalLifecycleEntries(
	t *testing.T,
	manager LogManager,
	profile string,
	requestID string,
	wantActions []string,
) []LogEntry {
	t.Helper()
	entries := readTestLogEntries(t, manager)
	if len(entries) != len(wantActions) {
		t.Fatalf("entries=%+v want actions=%v", entries, wantActions)
	}
	for index, action := range wantActions {
		entry := entries[index]
		if entry.Action != action || entry.Profile != profile || entry.RequestID != requestID ||
			entry.Source != "cli" || entry.Outcome == "" || entry.DurationMS < 1 {
			t.Fatalf("entry[%d]=%+v", index, entry)
		}
	}
	return entries
}

func TestLocalAgentVNCEarlyFailureKeepsCorrelationAndDuration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	req := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(
		`{"profile":"broken","profile_yaml":"profiles:\n  broken:\n    tunnels:\n      - local_port: 5907\n        remote_port: 5900\n","request_id":"vnc-early-123"}`,
	))
	req.Header.Set("X-Request-ID", "vnc-early-123")
	req.Header.Set("Origin", "https://cm.hsgitlab.xyz")
	rec := httptest.NewRecorder()
	app.newLocalAgentHandler().ServeHTTP(rec, req)
	var response webAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK {
		t.Fatalf("expected failure, body=%s", rec.Body.String())
	}
	entries := readTestLogEntries(t, app.LogManager)
	if len(entries) != 1 {
		t.Fatalf("entries=%+v", entries)
	}
	entry := entries[0]
	if entry.Action != "local-agent.vnc" || entry.Outcome != "failure" ||
		entry.RequestID != "vnc-early-123" || entry.Source != "web-local" ||
		entry.Profile != "broken" || entry.DurationMS < 0 ||
		len(entry.LocalPorts) != 1 || entry.LocalPorts[0] != 5907 ||
		entry.FailureStage != "tunnel" {
		t.Fatalf("entry=%+v", entry)
	}
}

func TestLocalAgentVNCProfileLoadFailureStage(t *testing.T) {
	dir := t.TempDir()
	app := testApp(&bytes.Buffer{}, &bytes.Buffer{}, dir)
	ctx := withOperationContext(context.Background(), OperationContext{RequestID: "vnc-profile-load", Source: "web-local"})
	response := app.localAgentRunVNC(ctx, "start", Profile{Name: "missing"}, filepath.Join(dir, "missing.yaml"))
	if response.OK {
		t.Fatalf("response=%+v", response)
	}
	entries := readTestLogEntries(t, app.LogManager)
	if len(entries) != 1 || entries[0].FailureStage != "profile" || entries[0].ErrorCode == "" {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestLocalAgentVNCConfigWriteFailureStage(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".connectmac"), []byte("blocks directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := testApp(&bytes.Buffer{}, &bytes.Buffer{}, dir)
	body, err := json.Marshal(localAgentRequest{Profile: "config-write", ProfileYAML: `profiles:
  config-write:
    user: ec2-user
    host: mac.example.com
    tunnels:
      - local_port: 5900
        remote_host: localhost
        remote_port: 5900
`})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/start", bytes.NewReader(body))
	req.Header.Set("Origin", "https://cm.hsgitlab.xyz")
	rec := httptest.NewRecorder()
	app.newLocalAgentHandler().ServeHTTP(rec, req)
	entries := readTestLogEntries(t, app.LogManager)
	if len(entries) != 1 || entries[0].FailureStage != "config-write" || entries[0].ErrorCode == "" {
		t.Fatalf("entries=%+v response=%s", entries, rec.Body.String())
	}
}

func TestTerminalSessionTokenOneTimeBoundedAndExpiring(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	registry := newTerminalSessionRegistry(2, time.Minute)
	registry.Now = func() time.Time { return now }

	first, err := registry.Issue("one", "request-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Issue("two", "request-two")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Consume(first, "wrong"); err == nil {
		t.Fatal("expected profile mismatch")
	}
	if grant, err := registry.Consume(first, "one"); err != nil || grant.RequestID != "request-one" {
		t.Fatalf("profile mismatch must not consume valid token: grant=%+v err=%v", grant, err)
	}
	grant, err := registry.Consume(second, "two")
	if err != nil || grant.RequestID != "request-two" {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
	if _, err := registry.Consume(second, "two"); err == nil {
		t.Fatal("expected replay rejection")
	}

	expired, err := registry.Issue("expired", "request-expired")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := registry.Consume(expired, "expired"); err == nil {
		t.Fatal("expected expired token rejection")
	}

}

func TestTerminalSessionRegistryReserveReleaseConsumeLifecycle(t *testing.T) {
	registry := newTerminalSessionRegistry(2, time.Minute)
	token, err := registry.Issue("shared", "request-one")
	if err != nil {
		t.Fatal(err)
	}
	grant, err := registry.Reserve(token, "shared")
	if err != nil || grant.RequestID != "request-one" {
		t.Fatalf("reserve grant=%+v err=%v", grant, err)
	}
	if _, err := registry.Reserve(token, "shared"); err == nil {
		t.Fatal("concurrent reserve must fail")
	}
	if err := registry.Release(token); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Reserve(token, "shared"); err != nil {
		t.Fatalf("retry reserve after release: %v", err)
	}
	consumed, err := registry.ConsumeReserved(token)
	if err != nil || consumed.Profile != "shared" {
		t.Fatalf("consume grant=%+v err=%v", consumed, err)
	}
	if _, err := registry.Reserve(token, "shared"); err == nil {
		t.Fatal("successful consume must prevent replay")
	}
}

func TestTerminalSessionRegistryCapacityNeverEvictsValidTokens(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	registry := newTerminalSessionRegistry(2, time.Minute)
	registry.Now = func() time.Time { return now }
	first, _ := registry.Issue("one", "request-one")
	second, _ := registry.Issue("two", "request-two")
	if _, err := registry.Issue("three", "request-three"); !errors.Is(err, errTerminalSessionCapacity) {
		t.Fatalf("capacity error=%v", err)
	}
	if _, err := registry.Reserve(first, "one"); err != nil {
		t.Fatalf("first valid token was evicted: %v", err)
	}
	_ = registry.Release(first)
	if _, err := registry.Reserve(second, "two"); err != nil {
		t.Fatalf("second valid token was evicted: %v", err)
	}
	_ = registry.Release(second)
	now = now.Add(2 * time.Minute)
	if _, err := registry.Issue("three", "request-three"); err != nil {
		t.Fatalf("expired cleanup did not restore capacity: %v", err)
	}
}

func TestTerminalSessionRegistryConcurrentReserveAndIssue(t *testing.T) {
	registry := newTerminalSessionRegistry(32, time.Minute)
	token, err := registry.Issue("shared", "request-shared")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 32)
	for index := 0; index < 32; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, reserveErr := registry.Reserve(token, "shared")
			results <- reserveErr
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for reserveErr := range results {
		if reserveErr == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful reserves=%d", successes)
	}

	registry = newTerminalSessionRegistry(16, time.Minute)
	results = make(chan error, 64)
	for index := 0; index < 64; index++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, issueErr := registry.Issue(fmt.Sprintf("profile-%d", i), fmt.Sprintf("request-%d", i))
			results <- issueErr
		}(index)
	}
	wg.Wait()
	close(results)
	issued := 0
	capacity := 0
	for issueErr := range results {
		switch {
		case issueErr == nil:
			issued++
		case errors.Is(issueErr, errTerminalSessionCapacity):
			capacity++
		default:
			t.Fatalf("unexpected issue error: %v", issueErr)
		}
	}
	if issued != 16 || capacity != 48 {
		t.Fatalf("issued=%d capacity=%d", issued, capacity)
	}
}

func TestTerminalWebSocketRejectsMissingMismatchReplayAndExpiredTokens(t *testing.T) {
	app := testApp(&bytes.Buffer{}, &bytes.Buffer{}, t.TempDir())
	app.TerminalSessions = newTerminalSessionRegistry(8, time.Minute)
	handler := app.newLocalAgentHandler()

	request := func(profile, token string) *httptest.ResponseRecorder {
		path := "/terminal/ws?profile=" + url.QueryEscape(profile)
		if token != "" {
			path += "&terminal_session_token=" + url.QueryEscape(token)
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", "https://cm.hsgitlab.xyz")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := request("shared", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status=%d body=%s", rec.Code, rec.Body.String())
	}
	mismatch, err := app.TerminalSessions.Issue("shared", "request-mismatch")
	if err != nil {
		t.Fatal(err)
	}
	if rec := request("other", mismatch); rec.Code != http.StatusUnauthorized {
		t.Fatalf("mismatch status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := app.TerminalSessions.Validate(mismatch, "shared"); err != nil {
		t.Fatalf("profile mismatch must leave token available: %v", err)
	}

	now := time.Now()
	app.TerminalSessions.Now = func() time.Time { return now }
	expired, err := app.TerminalSessions.Issue("shared", "request-expired")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if rec := request("shared", expired); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTerminalCloseLoggedExactlyOnceForNormalExitAndBrowserDisconnect(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "normal exit"},
		{name: "browser disconnect", err: &websocket.CloseError{Code: websocket.CloseNormalClosure}},
		{name: "user disconnect", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, dir)
			ctx := withOperationContext(context.Background(), OperationContext{
				RequestID: "terminal-request-123",
				Source:    "web-local",
			})
			profile := Profile{Name: "terminal-profile"}
			got := app.observeLocalTerminalSession(ctx, profile, func() error { return test.err })
			if !errors.Is(got, test.err) {
				t.Fatalf("error=%v want=%v", got, test.err)
			}
			entries := readTestLogEntries(t, app.LogManager)
			closed := 0
			for _, entry := range entries {
				if entry.Action != "terminal.closed" {
					continue
				}
				closed++
				if entry.Outcome != "success" || entry.Phase != "closed" ||
					entry.RequestID != "terminal-request-123" || entry.DurationMS < 1 ||
					entry.ActorMemberID != "" || entry.ActorMemberEmail != "" || entry.ActorMemberName != "" {
					t.Fatalf("entry=%+v", entry)
				}
			}
			if closed != 1 {
				t.Fatalf("closed=%d entries=%+v", closed, entries)
			}
		})
	}
}

func TestLocalTerminalFailureUsesLocalClassifier(t *testing.T) {
	dir := t.TempDir()
	app := testApp(&bytes.Buffer{}, &bytes.Buffer{}, dir)
	ctx := withOperationContext(context.Background(), OperationContext{
		RequestID: "terminal-failure-123",
		Source:    "web-local",
	})
	err := errors.New("ssh session failed: exit status 255")
	if got := app.observeLocalTerminalSession(ctx, Profile{Name: "terminal-profile"}, func() error { return err }); !errors.Is(got, err) {
		t.Fatalf("error=%v", got)
	}
	entries := readTestLogEntries(t, app.LogManager)
	for _, entry := range entries {
		if entry.Action != "terminal.closed" {
			continue
		}
		if entry.Outcome != "failure" || entry.ErrorCode != "ssh_exit_255" ||
			entry.ExitCode != 255 || entry.FailureStage != "session" || entry.Level != "error" {
			t.Fatalf("entry=%+v", entry)
		}
		if entry.ErrorCode == "aws_api_error" {
			t.Fatalf("terminal error used AWS classifier: %+v", entry)
		}
		if !strings.Contains(entry.Message, "exit status 255") || entry.Message == "terminal.closed" {
			t.Fatalf("message=%q", entry.Message)
		}
		return
	}
	t.Fatalf("terminal.closed not found: %+v", entries)
}

func TestLocalTerminalPreservesWarnLevelAndSanitizedMessage(t *testing.T) {
	dir := t.TempDir()
	app := testApp(&bytes.Buffer{}, &bytes.Buffer{}, dir)
	ctx := withOperationContext(context.Background(), OperationContext{RequestID: "terminal-timeout", Source: "web-local"})
	err := fmt.Errorf("ssh operation timed out token=%s", "terminal-secret")
	_ = app.observeLocalTerminalSession(ctx, Profile{Name: "terminal-profile"}, func() error { return err })
	entries := readTestLogEntries(t, app.LogManager)
	for _, entry := range entries {
		if entry.Action != "terminal.closed" {
			continue
		}
		if entry.Level != "warn" || entry.ErrorCode != "ssh_timeout" || entry.Outcome != "failure" {
			t.Fatalf("entry=%+v", entry)
		}
		if entry.Message == "" || strings.Contains(entry.Message, "terminal-secret") {
			t.Fatalf("message=%q", entry.Message)
		}
		return
	}
	t.Fatalf("terminal.closed not found: %+v", entries)
}

type terminalTestSocket struct {
	readErr   error
	writeErr  error
	readDelay time.Duration
}

func (s *terminalTestSocket) ReadMessage() (int, []byte, error) {
	if s.readDelay > 0 {
		time.Sleep(s.readDelay)
	}
	return websocket.TextMessage, nil, s.readErr
}

func (s *terminalTestSocket) WriteMessage(int, []byte) error {
	return s.writeErr
}

type terminalErrorReader struct {
	data []byte
	err  error
	read bool
}

func (r *terminalErrorReader) Read(buffer []byte) (int, error) {
	if !r.read && len(r.data) > 0 {
		r.read = true
		return copy(buffer, r.data), nil
	}
	return 0, r.err
}

func TestProxyTerminalIOPreservesRealErrorsAndNormalClose(t *testing.T) {
	readFailure := errors.New("stdout read failed")
	writeFailure := errors.New("websocket write failed")
	waitFailure := errors.New("ssh exited 23")
	abnormalClose := &websocket.CloseError{Code: websocket.CloseInternalServerErr, Text: "broken"}
	normalClose := &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "closed"}

	tests := []struct {
		name       string
		socket     *terminalTestSocket
		stdout     io.Reader
		stderr     io.Reader
		wait       func() error
		want       error
		wantResult string
	}{
		{
			name: "stdout read error", socket: &terminalTestSocket{readErr: abnormalClose, readDelay: 100 * time.Millisecond},
			stdout: &terminalErrorReader{err: readFailure}, stderr: strings.NewReader(""),
			wait: func() error { time.Sleep(time.Second); return nil }, want: readFailure, wantResult: "failure",
		},
		{
			name: "stderr read error", socket: &terminalTestSocket{readErr: abnormalClose, readDelay: 100 * time.Millisecond},
			stdout: strings.NewReader(""), stderr: &terminalErrorReader{err: readFailure},
			wait: func() error { time.Sleep(time.Second); return nil }, want: readFailure, wantResult: "failure",
		},
		{
			name: "websocket write error", socket: &terminalTestSocket{readErr: abnormalClose, writeErr: writeFailure, readDelay: 100 * time.Millisecond},
			stdout: &terminalErrorReader{data: []byte("output"), err: io.EOF}, stderr: strings.NewReader(""),
			wait: func() error { time.Sleep(time.Second); return nil }, want: writeFailure, wantResult: "failure",
		},
		{
			name: "ssh nonzero exit", socket: &terminalTestSocket{readErr: abnormalClose, readDelay: 100 * time.Millisecond},
			stdout: strings.NewReader(""), stderr: strings.NewReader(""),
			wait: func() error { return waitFailure }, want: waitFailure, wantResult: "failure",
		},
		{
			name: "normal browser close", socket: &terminalTestSocket{readErr: normalClose},
			stdout: strings.NewReader(""), stderr: strings.NewReader(""),
			wait: func() error { time.Sleep(time.Second); return nil }, want: normalClose, wantResult: "success",
		},
		{
			name: "clean ssh exit", socket: &terminalTestSocket{readErr: abnormalClose, readDelay: 100 * time.Millisecond},
			stdout: strings.NewReader(""), stderr: strings.NewReader(""),
			wait: func() error { return nil }, wantResult: "success",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			app := testApp(&bytes.Buffer{}, &bytes.Buffer{}, dir)
			ctx := localLifecycleTestContext("terminal-proxy-request")
			profile := Profile{Name: "terminal-profile"}
			got := app.observeLocalTerminalSession(ctx, profile, func() error {
				return proxyTerminalIO(ctx, test.socket, io.Discard, test.stdout, test.stderr, test.wait)
			})
			if test.want == nil {
				if got != nil {
					t.Fatalf("error=%v", got)
				}
			} else if !errors.Is(got, test.want) && got != test.want {
				t.Fatalf("error=%v want=%v", got, test.want)
			}
			entries := readTestLogEntries(t, app.LogManager)
			closed := 0
			for _, entry := range entries {
				if entry.Action == "terminal.closed" {
					closed++
					if entry.Outcome != test.wantResult {
						t.Fatalf("closed entry=%+v", entry)
					}
				}
			}
			if closed != 1 {
				t.Fatalf("closed=%d entries=%+v", closed, entries)
			}
		})
	}
}

func TestWebLocalIntentRecordsActorWithoutLocalExecutionClaim(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	clientRequestID := "duplicate-client-request"
	var responseRequestIDs []string
	for _, operation := range []string{"connect", "connect", "vnc", "transfer"} {
		rec := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/local-intent",
			`{"profile":"shared","operation":"`+operation+`","request_id":"`+clientRequestID+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", operation, rec.Code, rec.Body.String())
		}
		var response webAPIResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		data, ok := response.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("response data=%#v", response.Data)
		}
		requestID := strings.TrimSpace(fmt.Sprint(data["request_id"]))
		if requestID == "" || requestID == clientRequestID {
			t.Fatalf("authoritative request ID=%q client=%q", requestID, clientRequestID)
		}
		responseRequestIDs = append(responseRequestIDs, requestID)
	}
	events, err := app.MemberStore.RecentEvents("shared@example.com", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events=%+v", events)
	}
	seen := map[string]bool{}
	seenRequestIDs := map[string]bool{}
	for _, event := range events {
		seen[event.Action] = true
		if seenRequestIDs[event.RequestID] {
			t.Fatalf("duplicate server request ID: %+v", events)
		}
		seenRequestIDs[event.RequestID] = true
		if event.MemberEmail != operator.Email || event.Status != "requested" ||
			event.Phase != "requested" || event.Source != "web" {
			t.Fatalf("event=%+v", event)
		}
		if strings.Contains(event.Action, "succeeded") || strings.Contains(event.Action, "failed") {
			t.Fatalf("server claimed local execution result: %+v", event)
		}
	}
	for _, operation := range []string{"connect", "vnc", "transfer"} {
		if !seen["local."+operation+".requested"] {
			t.Fatalf("missing %s intent: %+v", operation, events)
		}
	}
	for _, requestID := range responseRequestIDs {
		if !seenRequestIDs[requestID] {
			t.Fatalf("response request ID %q missing from events %+v", requestID, events)
		}
	}
}

func TestBrowserLocalActionsRecordServerIntentBeforeLocalCalls(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is required for browser local-action test: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	startTunnel := extractWebSource(t, html, "async function startTunnel(profile)", "\n    async function openSync(profile)")
	runSync := extractWebSource(t, html, "async function runSync(direction)", "\n    function terminalSetStatus")
	terminalGuard := extractWebSource(t, html, "function terminalConnectionCurrent(", "\n    async function openTerminal(profile)")
	connectTerminal := extractWebSource(t, html, "async function connectLocalTerminal(profile)", "\n    function closeTerminal")
	script := `
import assert from "node:assert/strict";
const state = {
  busy: false,
  localAgent: {online:true, url:"https://127.0.0.1:18765"},
  selected: "shared",
  profiles: [{name:"shared", profile_yaml:"profiles:\n  shared: {}\n"}],
  terminalConnectedProfiles: new Set(),
  terminal: {profile:"", manualClose:false, returnOnClose:false, connectionGeneration:0, socket:null, xterm:null},
  syncJobsLoading: {},
  syncHistory: [],
  syncPollUnavailable: {}
};
let sequence = [];
function newLocalRequestID() { throw new Error("client request ID must not be authoritative"); }
async function recordLocalIntent(profile, operation) {
  const requestID = "server-" + operation + "-123";
  sequence.push("intent:" + operation + ":" + requestID);
  return {data:{request_id:requestID}};
}
function localAgentPayload(profile, extra = {}) {
  return {profile, profile_yaml:"profiles:\n  " + profile + ": {}\n", ...extra};
}
async function localAgentAPI(path, options) {
  sequence.push("local:" + path + ":" + options.requestID);
  if (path === "/start") return {output:"started"};
  if (path === "/open-vnc") return {output:"opened"};
  if (path === "/terminal/check") return {data:{
    terminal_session_token:"terminal-token-123",
    target:"ec2-user@host",
    host_key_status:"current"
  }};
  if (path.startsWith("/sync/")) return {data:{job:{
    id:"job-1", profile:"shared", direction:"push", status:"running", phase:"transferring", percent:1
  }}};
  throw new Error("unexpected local path " + path);
}
function setBusy(value) { state.busy = value; }
function setStatus() {}
function setOutput() {}
function showView() {}
function renderProfiles() {}
function renderSelected() {}
async function loadEvents() {}
function closeTerminal() { state.terminal.socket = null; }
function terminalClear() {}
function terminalSetStatus() {}
function terminalAppend() {}
class FakeWebSocket {
  static CLOSED = 3;
  constructor(target) {
    this.url = target;
    this.readyState = 0;
    sequence.push("ws:" + target);
  }
  close() {}
}
globalThis.WebSocket = FakeWebSocket;
function selectedProfile() { return {name:"shared"}; }
function profileReady() { return true; }
function syncJob() { return null; }
function syncJobBusy() { return false; }
function $(id) { return {value:id.includes("Remote") ? "~/Documents/" : "/tmp/local"}; }
function syncDirectionLabel() { return "上传"; }
async function api(path) {
  sequence.push("server:" + path);
  return {data:{record:{id:"transfer-1"}}};
}
function renderSyncHistory() {}
function syncJobKey(profile, direction) { return profile + "\n" + direction; }
function storeSyncJob(key, job) { return job; }
function scheduleSyncPoll() {}
function transferTerminal() { return false; }
async function persistTerminalTransfer() { return true; }
async function updateTransferRecord() {}
function loadSyncJobs() {}
function presentSyncJob() {}
async function failTransferRecord() {}
	` + startTunnel + "\n" + runSync + "\n" + terminalGuard + "\n" + connectTerminal + `

sequence = [];
await startTunnel("shared");
assert.deepEqual(sequence.slice(0, 3), [
  "intent:vnc:server-vnc-123",
  "local:/start:server-vnc-123",
  "local:/open-vnc:server-vnc-123"
]);

sequence = [];
await connectLocalTerminal("shared");
assert.equal(sequence[0], "intent:connect:server-connect-123");
assert.equal(sequence[1], "local:/terminal/check:server-connect-123");
assert.match(sequence[2], /^ws:.*terminal_session_token=terminal-token-123$/);
assert.doesNotMatch(sequence[2], /request_id=/);

sequence = [];
await runSync("push");
const transferIntent = sequence.indexOf("intent:transfer:server-transfer-123");
const transferLocal = sequence.indexOf("local:/sync/push:server-transfer-123");
assert.ok(transferIntent >= 0 && transferLocal > transferIntent, sequence.join(","));
`
	scriptPath := filepath.Join(t.TempDir(), "local-action-order.mjs")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("browser local-action order test failed: %v\n%s", err, output)
	}
}

func TestBrowserLocalRequestIDUsesCryptographicRandomness(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	source := extractWebSource(t, string(data), "function newLocalRequestID()", "\n    async function recordLocalIntent")
	if !strings.Contains(source, "crypto.randomUUID") || !strings.Contains(source, "crypto.getRandomValues") ||
		strings.Contains(source, "Math.random") {
		t.Fatalf("local request ID generator is not cryptographically safe: %s", source)
	}
}
