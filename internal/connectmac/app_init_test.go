package connectmac

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

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
