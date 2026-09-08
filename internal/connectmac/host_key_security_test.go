package connectmac

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReplaceKnownHostKeyAtomicSerializesConcurrentUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte("other.example.com ssh-ed25519 OTHER\nmac-host.example.com ssh-ed25519 OLD\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	keyA, _ := testScannedHostKey(t, "mac-host.example.com")
	keyB, _ := testScannedHostKey(t, "mac-host.example.com")
	app := NewApp(&bytes.Buffer{}, &bytes.Buffer{})
	app.KnownHosts = path
	checks := []HostKeyCheck{{Host: "mac-host.example.com", Status: HostKeyStale, Scanned: keyA}, {Host: "MAC-HOST.EXAMPLE.COM", Status: HostKeyStale, Scanned: keyB}}
	var wg sync.WaitGroup
	errs := make(chan error, len(checks))
	for _, check := range checks {
		wg.Add(1)
		go func(c HostKeyCheck) { defer wg.Done(); errs <- app.replaceKnownHostKeyAtomic(c) }(check)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	hasA := strings.Contains(text, strings.Fields(keyA)[2])
	hasB := strings.Contains(text, strings.Fields(keyB)[2])
	if !strings.Contains(text, "other.example.com") || hasA == hasB {
		t.Fatalf("atomic result=%q", text)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestReplaceKnownHostKeyAtomicSerializesDifferentHostsByFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte("other.example.com ssh-ed25519 OTHER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyA, _ := testScannedHostKey(t, "host-a.example.com")
	keyB, _ := testScannedHostKey(t, "host-b.example.com")
	app := NewApp(&bytes.Buffer{}, &bytes.Buffer{})
	app.KnownHosts = path

	firstWriteEntered := make(chan struct{})
	releaseFirstWrite := make(chan struct{})
	secondWriteEntered := make(chan struct{})
	var writes int
	var writesMu sync.Mutex
	app.WriteKnownHostsAtomic = func(path string, data []byte, mode os.FileMode) error {
		writesMu.Lock()
		writes++
		call := writes
		writesMu.Unlock()
		if call == 1 {
			close(firstWriteEntered)
			<-releaseFirstWrite
		} else if call == 2 {
			close(secondWriteEntered)
		}
		return writeFileAtomically(path, data, mode)
	}

	errA := make(chan error, 1)
	go func() {
		errA <- app.replaceKnownHostKeyAtomic(HostKeyCheck{Host: "host-a.example.com", Status: HostKeyMissing, Scanned: keyA})
	}()
	<-firstWriteEntered
	errB := make(chan error, 1)
	go func() {
		errB <- app.replaceKnownHostKeyAtomic(HostKeyCheck{Host: "host-b.example.com", Status: HostKeyMissing, Scanned: keyB})
	}()

	select {
	case <-secondWriteEntered:
		close(releaseFirstWrite)
		t.Fatal("different hosts wrote the same known_hosts file concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirstWrite)
	if err := <-errA; err != nil {
		t.Fatal(err)
	}
	if err := <-errB; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondWriteEntered:
	default:
		t.Fatal("second update did not run after the first update completed")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"other.example.com", strings.Fields(keyA)[2], strings.Fields(keyB)[2]} {
		if !strings.Contains(text, want) {
			t.Fatalf("known_hosts missing %q: %q", want, text)
		}
	}
}

func TestReplaceKnownHostKeyAtomicSerializesSymlinkAndRealPath(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "known_hosts.real")
	symlinkPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(realPath, []byte("other.example.com ssh-ed25519 OTHER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	keyA, _ := testScannedHostKey(t, "host-a.example.com")
	keyB, _ := testScannedHostKey(t, "host-b.example.com")

	firstWriteEntered := make(chan struct{})
	releaseFirstWrite := make(chan struct{})
	secondWriteEntered := make(chan struct{})
	var writes int
	var writesMu sync.Mutex
	write := func(path string, data []byte, mode os.FileMode) error {
		writesMu.Lock()
		writes++
		call := writes
		writesMu.Unlock()
		if call == 1 {
			close(firstWriteEntered)
			<-releaseFirstWrite
		} else if call == 2 {
			close(secondWriteEntered)
		}
		return writeFileAtomically(path, data, mode)
	}
	appViaLink := NewApp(&bytes.Buffer{}, &bytes.Buffer{})
	appViaLink.KnownHosts = symlinkPath
	appViaLink.WriteKnownHostsAtomic = write
	appViaRealPath := NewApp(&bytes.Buffer{}, &bytes.Buffer{})
	appViaRealPath.KnownHosts = realPath
	appViaRealPath.WriteKnownHostsAtomic = write

	errA := make(chan error, 1)
	go func() {
		errA <- appViaLink.replaceKnownHostKeyAtomic(HostKeyCheck{Host: "host-a.example.com", Status: HostKeyMissing, Scanned: keyA})
	}()
	<-firstWriteEntered
	errB := make(chan error, 1)
	go func() {
		errB <- appViaRealPath.replaceKnownHostKeyAtomic(HostKeyCheck{Host: "host-b.example.com", Status: HostKeyMissing, Scanned: keyB})
	}()
	select {
	case <-secondWriteEntered:
		close(releaseFirstWrite)
		t.Fatal("symlink and real paths wrote the same known_hosts file concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirstWrite)
	if err := <-errA; err != nil {
		t.Fatal(err)
	}
	if err := <-errB; err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"other.example.com", strings.Fields(keyA)[2], strings.Fields(keyB)[2]} {
		if !strings.Contains(text, want) {
			t.Fatalf("known_hosts missing %q: %q", want, text)
		}
	}
	info, err := os.Lstat(symlinkPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("known_hosts symlink was replaced")
	}
}

func TestNormalizedKnownHostsPathRejectsBrokenSymlinkAndNonRegularTarget(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken")
	if err := os.Symlink(filepath.Join(dir, "missing"), broken); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizedKnownHostsPath(broken); err == nil {
		t.Fatal("broken symlink accepted")
	}
	targetDir := filepath.Join(dir, "target-dir")
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(dir, "directory-link")
	if err := os.Symlink(targetDir, dirLink); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizedKnownHostsPath(dirLink); err == nil {
		t.Fatal("non-regular symlink target accepted")
	}
}

func TestReplaceKnownHostKeyAtomicWriteFailurePreservesOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	old := []byte("mac-host.example.com ssh-ed25519 OLD\n")
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	scanned, _ := testScannedHostKey(t, "mac-host.example.com")
	app := NewApp(&bytes.Buffer{}, &bytes.Buffer{})
	app.KnownHosts = path
	app.WriteKnownHostsAtomic = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
	if err := app.replaceKnownHostKeyAtomic(HostKeyCheck{Host: "mac-host.example.com", Status: HostKeyStale, Scanned: scanned}); err == nil {
		t.Fatal("expected write failure")
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, old) {
		t.Fatalf("old file changed: %q", data)
	}
}

func TestHostKeyChallengeOneTimeExpiryAndBinding(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := newHostKeyChallengeRegistry(4, time.Minute)
	r.now = func() time.Time { return now }
	set := []string{"SHA256:b", "SHA256:a"}
	token, err := r.Issue("p", "Host", 22, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Consume(token, "p", "host", 22, []string{"SHA256:a", "SHA256:b"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Consume(token, "p", "host", 22, set); err == nil {
		t.Fatal("replay accepted")
	}
	token, _ = r.Issue("p", "host", 22, set)
	if err := r.Consume(token, "other", "host", 22, set); err == nil {
		t.Fatal("mismatch accepted")
	}
	token, _ = r.Issue("p", "host", 22, set)
	now = now.Add(2 * time.Minute)
	if err := r.Consume(token, "p", "host", 22, set); err == nil {
		t.Fatal("expired challenge accepted")
	}
}

func TestHostKeyBlockedDeduplicatesOnlySameRequest(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.Runner = &fakeRunner{missingHostKey: true}
	app.HostKeyBlockedEvents = newHostKeyBlockedEventCache(8, time.Minute)
	profile := validProfile(writeSSHKey(t, 0o600))
	ctx := localLifecycleTestContext("same-request")
	_, _ = app.requireCurrentHostKey(ctx, profile)
	_, _ = app.requireCurrentHostKey(ctx, profile)
	_ = app.hostKeyBlocked(ctx, profile, HostKeyCheck{Status: HostKeyStale}, "host_key_changed", "host key changed")
	_, _ = app.requireCurrentHostKey(localLifecycleTestContext("distinct-request"), profile)
	count := 0
	for _, entry := range readTestLogEntries(t, app.LogManager) {
		if entry.Action == "host-key.blocked" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("blocked count=%d", count)
	}
}

func TestCLIHostKeyFixRequiresCompleteExplicitFingerprintSet(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	first, firstFingerprint := testScannedHostKey(t, "mac-host.example.com")
	second, secondFingerprint := testScannedHostKey(t, "mac-host.example.com")
	scanned := first + second
	profile := validProfile(key)
	cfg := Config{Profiles: map[string]Profile{profile.Name: profile}}
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.Runner = &fakeRunner{missingHostKey: true, scannedKey: scanned}

	partial := []string{"fix", profile.Name, "--confirm", "--fingerprint", firstFingerprint}
	if code := app.runHostKey(context.Background(), cfg, partial); code != 1 {
		t.Fatalf("partial fingerprint set code=%d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, ".ssh", "known_hosts")); !os.IsNotExist(err) {
		t.Fatalf("partial fingerprint set modified known_hosts: %v", err)
	}

	complete := []string{"fix", profile.Name, "--confirm", "--fingerprint", secondFingerprint, "--fingerprint", firstFingerprint}
	if code := app.runHostKey(context.Background(), cfg, complete); code != 0 {
		t.Fatalf("complete fingerprint set code=%d err=%s", code, errOut.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, ".ssh", "known_hosts"))
	if err != nil || !strings.Contains(string(data), strings.Fields(first)[2]) || !strings.Contains(string(data), strings.Fields(second)[2]) {
		t.Fatalf("known_hosts=%q err=%v", data, err)
	}
}
