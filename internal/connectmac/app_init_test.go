package connectmac

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

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
