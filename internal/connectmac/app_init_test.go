package connectmac

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

type initErrorReader struct {
	err error
}

func (r initErrorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestAppInitPrintsCustomServerBeforeReadingMissingToken(t *testing.T) {
	serverURL := "https://custom.example/managed"
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	writeFile(t, configPath, "server:\n  user_api: "+serverURL+"\ndefaults:\n  user: deploy\n  identity_file: ~/.ssh/deploy.pem\n")
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("n\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) {
		if !strings.Contains(out.String(), "Token validation server: "+serverURL) {
			t.Errorf("output before ReadSecret = %q, want custom validation server", out.String())
		}
		return "", nil
	}

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
}

func TestAppInitPEMDiscoveryWarningDoesNotAbort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("n\n")
	app.DiscoverInitPEMFiles = func(string) ([]string, error) {
		return nil, errors.New("injected scan failure")
	}
	tokenPrompted := false
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) {
		tokenPrompted = true
		return "", nil
	}

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
	if !tokenPrompted {
		t.Fatal("token step was skipped after PEM discovery error")
	}
	if !strings.Contains(errOut.String(), "warning:") || !strings.Contains(errOut.String(), "injected scan failure") {
		t.Fatalf("error output = %q, want PEM scan warning", errOut.String())
	}
	if !strings.Contains(errOut.String(), "Initialize AI Skill now?") {
		t.Fatalf("Skill confirmation was not reached: %q", errOut.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("minimal config was not created: %v", err)
	}
	if string(data) != DefaultConfigTemplate() {
		t.Fatalf("config = %q, want minimal config", data)
	}
}

func TestReadInitSecretImmediateEOFSkips(t *testing.T) {
	var out bytes.Buffer
	got, err := readInitSecret("Token: ", strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("readInitSecret returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("readInitSecret = %q, want skipped", got)
	}
}

func TestReadInitSecretPreservesNonEOFReadErrors(t *testing.T) {
	readErr := errors.New("input failed")
	_, err := readInitSecret("Token: ", initErrorReader{err: readErr}, io.Discard)
	if !errors.Is(err, readErr) {
		t.Fatalf("readInitSecret error = %v, want %v", err, readErr)
	}
}

func TestAppInitDevNullCreatesMinimalConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = devNull

	result := make(chan int, 1)
	go func() {
		result <- app.Run(context.Background(), []string{"init", "--config", configPath})
	}()
	select {
	case code := <-result:
		if code != 0 {
			t.Fatalf("init code = %d, err = %s", code, errOut.String())
		}
	case <-time.After(time.Second):
		t.Fatal("init with /dev/null hung")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != DefaultConfigTemplate() {
		t.Fatalf("config = %q, want minimal config", data)
	}
	for _, want := range []string{
		"Token: skipped",
		"generate a token in the management page",
		DefaultConnectMacServer,
		"rerun cm init",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary missing %q: %q", want, out.String())
		}
	}
}

func TestAppInitPipedInputUsesOnePersistentReader(t *testing.T) {
	const token = "cm_api_piped_secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profiles": []webManagedProfile{}}})
	}))
	defer server.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "pipe.pem"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.yaml")
	writeFile(t, configPath, "server:\n  user_api: "+server.URL+"\n")

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if _, err := writer.WriteString("1\n" + token + "\nn\n"); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = reader
	result := make(chan int, 1)
	go func() {
		result <- app.Run(context.Background(), []string{"init", "--config", configPath})
	}()

	select {
	case code := <-result:
		if code != 0 {
			t.Fatalf("init code = %d, err = %s", code, errOut.String())
		}
	case <-time.After(time.Second):
		_ = writer.Close()
		t.Fatal("init hung while piped writer remained open")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"identity_file: ~/.ssh/pipe.pem", "token: " + token} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config missing %q:\n%s", want, data)
		}
	}
}

func TestValidateInitTokenHonorsParentDeadlineAndCancelsRequest(t *testing.T) {
	if initTokenValidationTimeout != 15*time.Second {
		t.Fatalf("initTokenValidationTimeout = %s, want 15s", initTokenValidationTimeout)
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	app := NewApp(io.Discard, io.Discard)
	err := app.validateInitToken(ctx, server.URL, "cm_api_timeout")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("validateInitToken error = %v, want deadline exceeded", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("validation request did not reach handler")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("validation request context was not canceled")
	}
}

func TestAppInitCancellationDuringTokenValidationDoesNotRetryOrWrite(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	original := []byte("server:\n  user_api: " + server.URL + "\ndefaults:\n  identity_file: ~/.ssh/existing.pem\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("y\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) {
		return "cm_api_cancel", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if code := app.Run(ctx, []string{"init", "--config", configPath}); code != 1 {
		t.Fatalf("init code = %d, want 1", code)
	}
	select {
	case <-started:
	default:
		t.Fatal("validation request did not reach handler")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("validation request context was not canceled")
	}
	if strings.Contains(errOut.String(), "Retry token entry?") {
		t.Fatalf("canceled init offered retry: %q", errOut.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("canceled init changed config:\ngot  %q\nwant %q", data, original)
	}
}

func TestAppInitAlreadyCanceledDoesNotPromptOrWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("1\ntoken\ny\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) {
		t.Fatal("ReadSecret called")
		return "", nil
	}

	if code := app.Run(ctx, []string{"init", "--config", configPath}); code != 1 {
		t.Fatalf("init code = %d, want 1", code)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("canceled init created config: %v", err)
	}
	if strings.Contains(errOut.String(), "Select default PEM") || strings.Contains(errOut.String(), "Initialize AI Skill") {
		t.Fatalf("canceled init prompted: %q", errOut.String())
	}
}

func TestAppInitFirstRunCreatesMinimalConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "available.pem"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, ".connectmac", "config.yaml")
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("0\nn\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) { return "", nil }

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "server:\n  user_api: https://cm.hsgitlab.xyz\ndefaults:\n  user: ec2-user\n"
	if string(data) != want {
		t.Fatalf("config = %q, want %q", data, want)
	}
	for _, unwanted := range []string{"xcode-vnc", "example.pem", "amis_by_region", "elastic_ip", "security_group"} {
		if strings.Contains(string(data), unwanted) {
			t.Errorf("minimal config contains placeholder %q:\n%s", unwanted, data)
		}
	}
	if got := out.String(); !strings.Contains(got, "created") || !strings.Contains(got, "Token: skipped") || !strings.Contains(got, "PEM: skipped") || !strings.Contains(got, "cm init") {
		t.Fatalf("summary = %q, want created/skipped status and retry action", got)
	}
}

func TestAppInitSummaryReportsMissingPEM(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("n\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) { return "", nil }

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "PEM: missing") {
		t.Fatalf("summary = %q, want missing PEM status", out.String())
	}
}

func TestAppInitSelectsDiscoveredPEM(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "selected.pem"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.yaml")
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("1\nn\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) { return "", nil }

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "identity_file: ~/.ssh/selected.pem") {
		t.Fatalf("config missing selected PEM:\n%s", data)
	}
	if !strings.Contains(out.String(), "PEM: configured (~/.ssh/selected.pem, readable)") {
		t.Fatalf("summary = %q, want readable configured PEM", out.String())
	}
}

func TestAppInitStoresOnlyValidatedToken(t *testing.T) {
	const token = "cm_api_valid_secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("Authorization = %q", got)
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profiles": []webManagedProfile{}}})
	}))
	defer server.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	writeFile(t, configPath, "server:\n  user_api: "+server.URL+"\ndefaults:\n  user: admin\n  identity_file: ~/.ssh/existing.pem\n")
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("n\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) { return token, nil }

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "token: "+token) {
		t.Fatalf("validated token was not stored:\n%s", data)
	}
	if strings.Contains(out.String()+errOut.String(), token) {
		t.Fatal("full token was printed")
	}
}

func TestAppInitRejectedTokenCanBeSkippedWithoutWritingIt(t *testing.T) {
	const token = "cm_api_rejected_secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeWebError(w, http.StatusUnauthorized, "rejected "+token)
	}))
	defer server.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	writeFile(t, configPath, "server:\n  user_api: "+server.URL+"\ndefaults:\n  user: admin\n  identity_file: ~/.ssh/existing.pem\n")
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("n\nn\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) { return token, nil }

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) || strings.Contains(out.String()+errOut.String(), token) {
		t.Fatal("rejected token was stored or printed")
	}
	if !strings.Contains(errOut.String(), "Token validation failed") || !strings.Contains(out.String(), "Token: skipped") {
		t.Fatalf("out = %q, err = %q", out.String(), errOut.String())
	}
}

func TestAppInitPreservesExistingServerAndTokenWithoutReadingSecret(t *testing.T) {
	const original = "# exact formatting\nserver: {user_api: 'https://custom.example/v1', token: 'cm_api_existing_secret'}\ndefaults: {user: deploy, identity_file: '~/.ssh/deploy.pem'}\n"
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	writeFile(t, configPath, original)
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("n\n")
	secretCalls := 0
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) {
		secretCalls++
		return "", nil
	}

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("rerun changed config bytes:\ngot  %q\nwant %q", data, original)
	}
	if secretCalls != 0 {
		t.Fatalf("ReadSecret calls = %d, want 0", secretCalls)
	}
	if strings.Contains(out.String()+errOut.String(), "cm_api_existing_secret") {
		t.Fatal("existing token was printed")
	}
	if !strings.Contains(out.String(), "unchanged") || !strings.Contains(out.String(), "Token: configured") || !strings.Contains(out.String(), "cm list") {
		t.Fatalf("summary = %q", out.String())
	}
}

func TestAppInitAddsMissingServerAndPreservesDocumentContent(t *testing.T) {
	const original = "# keep this comment\ncustom_top_level: retained\ndefaults:\n  user: custom-user\n  identity_file: ~/.ssh/default.pem\nserver:\n  token: existing-token\nprofiles:\n  operations:\n    identity_file: ~/.ssh/profile.pem\n"
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	writeFile(t, configPath, original)
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("n\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) { t.Fatal("ReadSecret called"); return "", nil }

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# keep this comment", "custom_top_level: retained", "user_api: " + DefaultConnectMacServer, "user: custom-user", "operations:", "identity_file: ~/.ssh/profile.pem"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("updated config missing %q:\n%s", want, data)
		}
	}
}

func TestAppInitMalformedYAMLLeavesFileUntouchedAndDoesNotPrompt(t *testing.T) {
	const original = "server: [unterminated\n"
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	writeFile(t, configPath, original)
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = nil
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) { t.Fatal("ReadSecret called"); return "", nil }

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 1 {
		t.Fatalf("init code = %d, want 1", code)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("malformed config changed: %q", data)
	}
	if !strings.Contains(errOut.String(), "parse config") {
		t.Fatalf("error = %q, want parse config", errOut.String())
	}
}

func TestAppInitAndWizardUseSameGuidedFlow(t *testing.T) {
	for _, args := range [][]string{{"init"}, {"init", "wizard"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			configPath := filepath.Join(home, "config.yaml")
			var out, errOut bytes.Buffer
			app := NewApp(&out, &errOut)
			app.In = strings.NewReader("n\n")
			app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) { return "", nil }
			command := append(append([]string(nil), args...), "--config", configPath)
			if code := app.Run(context.Background(), command); code != 0 {
				t.Fatalf("init code = %d, err = %s", code, errOut.String())
			}
			data, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != DefaultConfigTemplate() {
				t.Fatalf("config = %q, want minimal template", data)
			}
		})
	}
}

func TestAppInitRejectsUnknownOptions(t *testing.T) {
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	if code := app.Run(context.Background(), []string{"init", "--unknown"}); code != 2 {
		t.Fatalf("init code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "unknown init option") {
		t.Fatalf("error = %q", errOut.String())
	}
}

func TestAppInitAtomicUpdateLeavesModePrivate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	writeFile(t, configPath, "defaults:\n  user: admin\n  identity_file: ~/.ssh/admin.pem\nserver:\n  token: existing\n")
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("n\n")

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code != 0 {
		t.Fatalf("init code = %d, err = %s", code, errOut.String())
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %04o, want 0600", got)
	}
}

func TestAppInitSkillFailureKeepsValidConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	app.In = strings.NewReader("y\nunsupported-agent\n")
	app.ReadSecret = func(string, io.Reader, io.Writer) (string, error) { return "", nil }

	if code := app.Run(context.Background(), []string{"init", "--config", configPath}); code == 0 {
		t.Fatal("init code = 0, want skill setup failure")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("valid config was rolled back: %v", err)
	}
	if string(data) != DefaultConfigTemplate() {
		t.Fatalf("config after skill failure = %q", data)
	}
}

func TestDiscoverInitPEMFiles(t *testing.T) {
	sshDir := t.TempDir()
	for _, name := range []string{"z-last.PEM", "a-first.pem", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(sshDir, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(sshDir, "ignored.pem"), 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(sshDir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "nested.pem"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := discoverInitPEMFiles(sshDir)
	if err != nil {
		t.Fatalf("discoverInitPEMFiles returned error: %v", err)
	}
	want := []string{"~/.ssh/a-first.pem", "~/.ssh/z-last.PEM"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverInitPEMFiles = %v, want %v", got, want)
	}
}

func TestDiscoverInitPEMFilesMissingDirectory(t *testing.T) {
	got, err := discoverInitPEMFiles(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("discoverInitPEMFiles returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("discoverInitPEMFiles = %v, want empty list", got)
	}
}

func TestReadInitSecretNonTTY(t *testing.T) {
	var out bytes.Buffer
	got, err := readInitSecret("Token: ", strings.NewReader("  cm_api_secret  \n"), &out)
	if err != nil {
		t.Fatalf("readInitSecret returned error: %v", err)
	}
	if got != "cm_api_secret" {
		t.Fatalf("readInitSecret = %q, want trimmed secret", got)
	}
	if text := out.String(); !strings.Contains(text, "Token: ") || !strings.Contains(text, "cannot be disabled") {
		t.Fatalf("readInitSecret output = %q, want prompt and non-TTY warning", text)
	}
}

func TestReadInitSecretNonTTYDoesNotConsumeNextPipeLine(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := writer.WriteString("cm_api_secret\nnext response\n"); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := readInitSecret("Token: ", reader, io.Discard)
	if err != nil {
		t.Fatalf("readInitSecret returned error: %v", err)
	}
	if got != "cm_api_secret" {
		t.Fatalf("readInitSecret = %q, want %q", got, "cm_api_secret")
	}
	next, err := readInputLine(reader)
	if err != nil && len(next) == 0 {
		t.Fatalf("next prompt read returned error: %v", err)
	}
	if got := strings.TrimSpace(next); got != "next response" {
		t.Fatalf("next prompt read = %q, want %q", got, "next response")
	}
}

func TestReadInitSecretNilInput(t *testing.T) {
	if _, err := readInitSecret("Token: ", nil, io.Discard); err == nil {
		t.Fatal("readInitSecret returned nil error for nil input")
	}
}

func TestNewAppProvidesReadSecret(t *testing.T) {
	app := NewApp(io.Discard, io.Discard)
	if app.ReadSecret == nil {
		t.Fatal("NewApp ReadSecret = nil")
	}
}

func TestValidateInitToken(t *testing.T) {
	const token = "cm_api_do_not_leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/managed-profiles" || r.URL.Query().Get("include_yaml") != "1" {
			t.Errorf("request target = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profiles": []webManagedProfile{}}})
	}))
	defer server.Close()

	app := NewApp(io.Discard, io.Discard)
	if err := app.validateInitToken(context.Background(), server.URL, token); err != nil {
		t.Fatalf("validateInitToken returned error: %v", err)
	}
}

func TestValidateInitTokenRejectsUnauthorizedWithoutLeakingToken(t *testing.T) {
	const token = "cm_api_do_not_leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeWebError(w, http.StatusUnauthorized, "login required for "+token)
	}))
	defer server.Close()

	app := NewApp(io.Discard, io.Discard)
	err := app.validateInitToken(context.Background(), server.URL, token)
	if err == nil {
		t.Fatal("validateInitToken returned nil error for unauthorized response")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("validateInitToken error leaked token: %v", err)
	}
}

func TestValidateInitTokenRejectsHTTPErrorResponses(t *testing.T) {
	const token = "cm_api_do_not_leak"
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{name: "unauthorized success JSON", status: http.StatusUnauthorized, contentType: "application/json", body: `{"ok":true,"data":{"profiles":[]}}`},
		{name: "server error success JSON", status: http.StatusInternalServerError, contentType: "application/json", body: `{"ok":true,"data":{"profiles":[]}}`},
		{name: "non JSON error", status: http.StatusBadGateway, contentType: "text/plain", body: "upstream unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			app := NewApp(io.Discard, io.Discard)
			err := app.validateInitToken(context.Background(), server.URL, token)
			if err == nil {
				t.Fatalf("validateInitToken returned nil error for HTTP %d", tt.status)
			}
			if !strings.Contains(err.Error(), http.StatusText(tt.status)) {
				t.Fatalf("validateInitToken error = %q, want HTTP status", err)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("validateInitToken error leaked token: %v", err)
			}
		})
	}
}

func TestInitConfigDocumentMinimalFirstRun(t *testing.T) {
	doc, err := newInitConfigDocument(nil)
	if err != nil {
		t.Fatalf("newInitConfigDocument returned error: %v", err)
	}

	doc.SetServerUserAPI(DefaultConnectMacServer)
	doc.SetDefaultUser(DefaultAWSUser)

	got, changed, err := doc.Bytes()
	if err != nil {
		t.Fatalf("Bytes returned error: %v", err)
	}
	if !changed {
		t.Fatal("Bytes changed = false, want true")
	}
	want := "server:\n  user_api: https://cm.hsgitlab.xyz\ndefaults:\n  user: ec2-user\n"
	if string(got) != want {
		t.Fatalf("Bytes = %q, want %q", got, want)
	}
}

func TestInitConfigDocumentPreservesProfilesCommentsAndUnknownFields(t *testing.T) {
	original := []byte(`# keep this comment
custom_top_level: retained
profiles:
  operations-use2:
    identity_file: ~/.ssh/operations.pem
`)
	doc, err := newInitConfigDocument(original)
	if err != nil {
		t.Fatalf("newInitConfigDocument returned error: %v", err)
	}

	doc.SetServerToken("cm_api_new")

	got, changed, err := doc.Bytes()
	if err != nil {
		t.Fatalf("Bytes returned error: %v", err)
	}
	if !changed {
		t.Fatal("Bytes changed = false, want true")
	}
	for _, want := range []string{
		"# keep this comment",
		"custom_top_level: retained",
		"profiles:",
		"operations-use2:",
		"identity_file: ~/.ssh/operations.pem",
		"server:\n  token: cm_api_new",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("Bytes missing %q:\n%s", want, got)
		}
	}
}

func TestInitConfigDocumentNoChangeReturnsOriginalBytes(t *testing.T) {
	original := []byte("# spacing is intentional\nserver: {user_api: https://example.com}\n")
	doc, err := newInitConfigDocument(original)
	if err != nil {
		t.Fatalf("newInitConfigDocument returned error: %v", err)
	}

	got, changed, err := doc.Bytes()
	if err != nil {
		t.Fatalf("Bytes returned error: %v", err)
	}
	if changed {
		t.Fatal("Bytes changed = true, want false")
	}
	if string(got) != string(original) {
		t.Fatalf("Bytes = %q, want original %q", got, original)
	}
}

func TestInitConfigDocumentGettersAndNoOpSetters(t *testing.T) {
	original := []byte(`server:
  user_api: https://example.com
  token: existing-token
defaults:
  user: admin
  identity_file: ~/.ssh/admin.pem
`)
	doc, err := newInitConfigDocument(original)
	if err != nil {
		t.Fatalf("newInitConfigDocument returned error: %v", err)
	}

	if got := doc.ServerUserAPI(); got != "https://example.com" {
		t.Errorf("ServerUserAPI = %q, want %q", got, "https://example.com")
	}
	if got := doc.ServerToken(); got != "existing-token" {
		t.Errorf("ServerToken = %q, want %q", got, "existing-token")
	}
	if got := doc.DefaultUser(); got != "admin" {
		t.Errorf("DefaultUser = %q, want %q", got, "admin")
	}
	if got := doc.DefaultIdentityFile(); got != "~/.ssh/admin.pem" {
		t.Errorf("DefaultIdentityFile = %q, want %q", got, "~/.ssh/admin.pem")
	}

	doc.SetServerUserAPI("https://example.com")
	doc.SetServerToken("")
	doc.SetDefaultUser("admin")
	doc.SetDefaultIdentityFile("")
	got, changed, err := doc.Bytes()
	if err != nil {
		t.Fatalf("Bytes returned error: %v", err)
	}
	if changed {
		t.Fatal("Bytes changed = true, want false")
	}
	if string(got) != string(original) {
		t.Fatalf("Bytes = %q, want original %q", got, original)
	}
}

func TestInitConfigDocumentTreatsNullScalarsAsAbsent(t *testing.T) {
	doc, err := newInitConfigDocument([]byte(`server:
  user_api: null
  token: ~
defaults:
  user: !!null null
  identity_file:
`))
	if err != nil {
		t.Fatalf("newInitConfigDocument returned error: %v", err)
	}
	for name, value := range map[string]string{
		"server.user_api":        doc.ServerUserAPI(),
		"server.token":           doc.ServerToken(),
		"defaults.user":          doc.DefaultUser(),
		"defaults.identity_file": doc.DefaultIdentityFile(),
	} {
		if value != "" {
			t.Errorf("%s = %q, want absent", name, value)
		}
	}
}

func TestInitConfigDocumentResolvesAliasedScalarValues(t *testing.T) {
	doc, err := newInitConfigDocument([]byte(`shared:
  server: &server https://custom.example
  token: &token cm_api_aliased
  user: &user deploy
  identity: &identity ~/.ssh/aliased.pem
server:
  user_api: *server
  token: *token
defaults:
  user: *user
  identity_file: *identity
`))
	if err != nil {
		t.Fatalf("newInitConfigDocument returned error: %v", err)
	}
	got := []string{doc.ServerUserAPI(), doc.ServerToken(), doc.DefaultUser(), doc.DefaultIdentityFile()}
	want := []string{"https://custom.example", "cm_api_aliased", "deploy", "~/.ssh/aliased.pem"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aliased values = %#v, want %#v", got, want)
	}
}

func TestInitConfigDocumentAliasCycleIsAbsent(t *testing.T) {
	doc, err := newInitConfigDocument(nil)
	if err != nil {
		t.Fatal(err)
	}
	first := &yaml.Node{Kind: yaml.AliasNode}
	second := &yaml.Node{Kind: yaml.AliasNode}
	first.Alias = second
	second.Alias = first
	server := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	appendMappingEntry(server, "token", first)
	appendMappingEntry(doc.root.Content[0], "server", server)

	if got := doc.ServerToken(); got != "" {
		t.Fatalf("ServerToken = %q, want absent for alias cycle", got)
	}
}

func TestInitConfigDocumentSerializationFailureReturnsNoBytes(t *testing.T) {
	doc, err := newInitConfigDocument(nil)
	if err != nil {
		t.Fatalf("newInitConfigDocument returned error: %v", err)
	}
	doc.root.Content[0] = &yaml.Node{Kind: yaml.Kind(99)}
	doc.changed = true

	got, changed, err := doc.Bytes()
	if err == nil {
		t.Fatal("Bytes returned nil error, want serialization failure")
	}
	if changed {
		t.Fatal("Bytes changed = true on serialization failure, want false")
	}
	if got != nil {
		t.Fatalf("Bytes returned %q on serialization failure, want nil", got)
	}
}

func TestInitConfigDocumentRejectsNonMappingKnownSections(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		section string
	}{
		{name: "server scalar", data: "server: https://example.com\n", section: "server"},
		{name: "defaults sequence", data: "defaults:\n  - ec2-user\n", section: "defaults"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newInitConfigDocument([]byte(tt.data))
			if err == nil {
				t.Fatal("newInitConfigDocument returned nil error, want section type error")
			}
			if !strings.Contains(err.Error(), tt.section) || !strings.Contains(err.Error(), "mapping") {
				t.Fatalf("error = %q, want section name and mapping requirement", err)
			}
		})
	}
}

func TestWritePrivateFileAtomicCreatesPrivateParentAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "config.yaml")
	if err := writePrivateFileAtomic(path, []byte("secret\n")); err != nil {
		t.Fatalf("writePrivateFileAtomic returned error: %v", err)
	}

	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := parentInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent mode = %04o, want 0700", got)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", got)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret\n" {
		t.Fatalf("file contents = %q, want %q", got, "secret\\n")
	}
}

func TestWritePrivateFileAtomicPreservesExistingParentMode(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o750); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(parent, "config.yaml")
	if err := writePrivateFileAtomic(path, []byte("secret\n")); err != nil {
		t.Fatalf("writePrivateFileAtomic returned error: %v", err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("existing parent mode = %04o, want unchanged 0750", got)
	}
}

func TestWritePrivateFileAtomicFailureDoesNotDamageExistingDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "existing")
	if err := os.WriteFile(marker, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writePrivateFileAtomic(path, []byte("replacement\n")); err == nil {
		t.Fatal("writePrivateFileAtomic returned nil error, want rename failure")
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("existing destination was damaged: %v", err)
	}
	if string(got) != "keep me" {
		t.Fatalf("existing contents = %q, want %q", got, "keep me")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".config.yaml.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files were not cleaned up: %v", matches)
	}
}
