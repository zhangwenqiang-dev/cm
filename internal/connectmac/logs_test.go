package connectmac

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogManagerWriteCleanAndExport(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.Now = func() time.Time {
		return time.Date(2026, 7, 2, 10, 30, 0, 0, time.UTC)
	}
	if err := manager.Write(LogEntry{
		Level: "error", Action: "web.aws.status", Profile: "iossupport-usw2",
		MemberEmail: "member@example.com", TransferID: "transfer-1", LocalJobID: "job-1",
		Direction: "push", Status: "running", Percent: 50, ElapsedMS: 1234,
		Message: "aws status failed with password=secret-token",
	}); err != nil {
		t.Fatalf("write log: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "cm-2026-07-02.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "iossupport-usw2") || strings.Contains(string(data), "password=secret-token") {
		t.Fatalf("unexpected log data: %s", data)
	}
	for _, field := range []string{`"member_email":"member@example.com"`, `"transfer_id":"transfer-1"`, `"local_job_id":"job-1"`, `"direction":"push"`, `"status":"running"`, `"percent":50`, `"elapsed_ms":1234`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("missing structured field %s in %s", field, data)
		}
	}
	oldPath := filepath.Join(dir, "cm-2026-05-01.log")
	if err := os.WriteFile(oldPath, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("write old log: %v", err)
	}
	oldTime := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old log: %v", err)
	}
	exportPath := filepath.Join(dir, "export.zip")
	got, err := manager.Export(exportPath, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("export logs: %v", err)
	}
	if got != exportPath {
		t.Fatalf("export path = %s, want %s", got, exportPath)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old log should be cleaned, err=%v", err)
	}
	zr, err := zip.OpenReader(exportPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	if len(zr.File) != 2 || zr.File[0].Name != "cm-2026-07-02.log" || zr.File[1].Name != "manifest.json" {
		t.Fatalf("zip files = %+v", zr.File)
	}
}

func TestLogManagerRotatesStructuredLogsWithBoundedGenerations(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.MaxStructuredBytes = 220
	manager.MaxGenerations = 2
	manager.Now = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	for i := 0; i < 8; i++ {
		if err := manager.Write(LogEntry{Action: "rotation.test", Message: strings.Repeat("x", 80)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"cm-2026-09-08.log", "cm-2026-09-08.log.1", "cm-2026-09-08.log.2"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var entry LogEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("invalid JSONL in %s: %v", name, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "cm-2026-09-08.log.3")); !os.IsNotExist(err) {
		t.Fatalf("unexpected extra generation: %v", err)
	}
}

func TestLogManagerConcurrentWritesRemainValidJSONL(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.MaxStructuredBytes = 1024 * 1024
	manager.Now = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := manager.Write(LogEntry{Action: "concurrent.test", Message: fmt.Sprintf("entry-%d", i)}); err != nil {
				t.Errorf("write: %v", err)
			}
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(dir, "cm-2026-09-08.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 20 {
		t.Fatalf("line count = %d", len(lines))
	}
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("invalid JSON line: %q", line)
		}
	}
}

func TestLogExportDefaultsExcludeRawAndIncludePrivateManifest(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.Now = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	if err := manager.Write(LogEntry{Action: "export.test", Message: "safe"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "local-agent.out.log"), "secret local output\n")
	dest := filepath.Join(dir, "default.zip")
	if _, err := manager.ExportWithOptions(LogExportOptions{Destination: dest, CMVersion: "0.1.149"}); err != nil {
		t.Fatal(err)
	}
	contents := readZipContents(t, dest)
	if _, ok := contents["local-agent.out.log"]; ok {
		t.Fatal("default export included raw log")
	}
	if _, ok := contents["raw/local-agent.out.log"]; ok {
		t.Fatal("default export included raw log directory")
	}
	manifest := contents["manifest.json"]
	if strings.Contains(manifest, dir) || !strings.Contains(manifest, `"include_raw": false`) || !strings.Contains(manifest, `"redaction_policy_version": "1"`) || !strings.Contains(manifest, `"cm_version": "0.1.149"`) {
		t.Fatalf("manifest = %s", manifest)
	}
	var decoded logExportManifest
	if err := json.Unmarshal([]byte(manifest), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.PayloadFileCount != len(contents)-1 {
		t.Fatalf("payload_file_count=%d archive payloads=%d", decoded.PayloadFileCount, len(contents)-1)
	}
}

func TestStructuredExportResanitizesHistoricalEntriesAsValidJSONL(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.Now = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	legacy := strings.Join([]string{
		`{"time":"2026-09-08T12:00:00Z","action":"legacy","message":"password=old-secret","legacy_token":"plain-token"}`,
		`malformed token=also-secret /Users/wenqiang/private`,
	}, "\n") + "\n"
	writeFile(t, filepath.Join(dir, "cm-2026-09-08.log"), legacy)
	dest := filepath.Join(dir, "structured.zip")
	if _, err := manager.ExportWithOptions(LogExportOptions{Destination: dest}); err != nil {
		t.Fatal(err)
	}
	exported := readZipContents(t, dest)["cm-2026-09-08.log"]
	for _, secret := range []string{"old-secret", "plain-token", "also-secret", "wenqiang"} {
		if strings.Contains(exported, secret) {
			t.Fatalf("structured export leaked %q: %s", secret, exported)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(exported), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("structured export is not valid JSONL: %q", line)
		}
	}
}

func TestLogExportIncludeRawIsBoundedAndRedacted(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.Now = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	prefix := strings.Repeat("unsafe-fragment", 400000)
	raw := prefix + "\n/Users/wenqiang/project token=secret 54.190.87.133 ec2-54-190-87-133.us-west-2.compute.amazonaws.com SHA256:abcdefghijklmnop\n"
	writeFile(t, filepath.Join(dir, "local-agent.out.log"), raw)
	dest := filepath.Join(dir, "raw.zip")
	if _, err := manager.ExportWithOptions(LogExportOptions{Destination: dest, IncludeRaw: true}); err != nil {
		t.Fatal(err)
	}
	contents := readZipContents(t, dest)
	exported := contents["raw/local-agent.out.log"]
	for _, marker := range []string{"[REDACTED HOME]", "[REDACTED]", "[REDACTED IP]", "[REDACTED HOST]", "[REDACTED FINGERPRINT]"} {
		if !strings.Contains(exported, marker) {
			t.Fatalf("raw export missing redaction marker %q: %s", marker, exported)
		}
	}
	if len(exported) > int(maxRawExportBytes)+1024 {
		t.Fatalf("raw export size = %d", len(exported))
	}
	for _, secret := range []string{"wenqiang", "secret", "54.190.87.133", "ec2-54-190-87-133", "SHA256:abcdefghijklmnop", "unsafe-fragment"} {
		if strings.Contains(exported, secret) {
			t.Fatalf("raw export leaked %q: %s", secret, exported)
		}
	}
}

func TestRawExportRedactsMultilinePEMBlock(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	raw := "before\n-----BEGIN PRIVATE KEY-----\nBASE64SECRETLINE\nanother-secret-line\n-----END PRIVATE KEY-----\nafter\n"
	writeFile(t, filepath.Join(dir, "local-agent.err.log"), raw)
	dest := filepath.Join(dir, "pem.zip")
	if _, err := manager.ExportWithOptions(LogExportOptions{Destination: dest, IncludeRaw: true}); err != nil {
		t.Fatal(err)
	}
	exported := readZipContents(t, dest)["raw/local-agent.err.log"]
	if strings.Contains(exported, "BASE64SECRETLINE") || strings.Contains(exported, "another-secret-line") || strings.Contains(exported, "BEGIN PRIVATE KEY") || strings.Contains(exported, "END PRIVATE KEY") {
		t.Fatalf("PEM block leaked: %s", exported)
	}
	if strings.Count(exported, "[REDACTED PEM BLOCK]") != 1 || !strings.Contains(exported, "before") || !strings.Contains(exported, "after") {
		t.Fatalf("PEM block redaction = %s", exported)
	}
}

func TestRawExportTailStartingInsidePEMDoesNotLeakKeyBody(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	var raw strings.Builder
	raw.WriteString("-----BEGIN PRIVATE KEY-----\n")
	for raw.Len() < int(maxRawExportBytes)+512*1024 {
		raw.WriteString("U0VDUkVUS0VZQk9EWUxJTkVTRUNSRVRLRVlCT0RZTElORQ==\n")
	}
	raw.WriteString("-----END PRIVATE KEY-----\nafter-tail\n")
	writeFile(t, filepath.Join(dir, "local-agent.err.log"), raw.String())
	dest := filepath.Join(dir, "tail-pem.zip")
	if _, err := manager.ExportWithOptions(LogExportOptions{Destination: dest, IncludeRaw: true}); err != nil {
		t.Fatal(err)
	}
	exported := readZipContents(t, dest)["raw/local-agent.err.log"]
	if strings.Contains(exported, "U0VDUkVU") || strings.Contains(exported, "PRIVATE KEY") || !strings.Contains(exported, "after-tail") {
		t.Fatalf("tail PEM export = %s", exported)
	}
}

func TestRawExportTailPEMBeginBeyondLookbehindSuppressesShortFinalBodyLine(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	var body strings.Builder
	for body.Len() < int(maxRawExportBytes)+128*1024 {
		body.WriteString("U0VDUkVUS0VZQk9EWUxJTkVTRUNSRVRLRVlCT0RZTElORQ==\n")
	}
	raw := "prefix\n-----BEGIN PRIVATE KEY-----\n" + body.String() + "QUJDRA==\n-----END PRIVATE KEY-----\nafter-exact-tail\n"
	writeFile(t, filepath.Join(dir, "local-agent.err.log"), raw)
	dest := filepath.Join(dir, "exact-tail-pem.zip")
	if _, err := manager.ExportWithOptions(LogExportOptions{Destination: dest, IncludeRaw: true}); err != nil {
		t.Fatal(err)
	}
	exported := readZipContents(t, dest)["raw/local-agent.err.log"]
	for _, leaked := range []string{"U0VDUkVU", "QUJDRA==", "PRIVATE KEY"} {
		if strings.Contains(exported, leaked) {
			t.Fatalf("tail PEM leaked %q", leaked)
		}
	}
	if !strings.Contains(exported, "after-exact-tail") {
		t.Fatalf("post-PEM line missing: %s", exported)
	}
}

func TestRawExportDiscardsOversizedLineAndContinues(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	raw := "first\n" + strings.Repeat("x", maxRawExportLineBytes+200000) + "\nafter 10.0.0.1\n"
	writeFile(t, filepath.Join(dir, "local-agent.out.log"), raw)
	dest := filepath.Join(dir, "oversized.zip")
	if _, err := manager.ExportWithOptions(LogExportOptions{Destination: dest, IncludeRaw: true}); err != nil {
		t.Fatal(err)
	}
	exported := readZipContents(t, dest)["raw/local-agent.out.log"]
	if !strings.Contains(exported, "[REDACTED OVERSIZED LINE]") || !strings.Contains(exported, "after [REDACTED IP]") || strings.Contains(exported, strings.Repeat("x", 100)) {
		t.Fatalf("oversized raw export = %s", exported)
	}
}

func TestRawExportRedactsStandaloneTokenAndPublicKeyPatterns(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	secrets := []string{
		"ghp_abcdefghijklmnopqrstuvwxyz123456",
		"github_pat_11AAabcdefghijklmnopqrstuvwxyz_1234567890",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature1234567890",
		"AKIAIOSFODNN7EXAMPLE",
		"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJ",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKlongpublickeybodyexample user@example.com",
	}
	writeFile(t, filepath.Join(dir, "local-agent.out.log"), strings.Join(secrets, "\n")+"\nordinary transfer completed\n")
	dest := filepath.Join(dir, "tokens.zip")
	if _, err := manager.ExportWithOptions(LogExportOptions{Destination: dest, IncludeRaw: true}); err != nil {
		t.Fatal(err)
	}
	exported := readZipContents(t, dest)["raw/local-agent.out.log"]
	for _, secret := range secrets {
		if strings.Contains(exported, secret) {
			t.Fatalf("token leaked: %q in %s", secret, exported)
		}
	}
	if !strings.Contains(exported, "ordinary transfer completed") {
		t.Fatalf("ordinary text was lost: %s", exported)
	}
}

func TestLogExportRefusesSymlinkSources(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	target := filepath.Join(dir, "target.log")
	writeFile(t, target, "token=secret\n")
	if err := os.Symlink(target, filepath.Join(dir, "local-agent.out.log")); err != nil {
		t.Fatal(err)
	}
	_, err := manager.ExportWithOptions(LogExportOptions{Destination: filepath.Join(dir, "raw-symlink.zip"), IncludeRaw: true})
	if err == nil || !strings.Contains(err.Error(), "refuse non-regular") {
		t.Fatalf("raw symlink export error = %v", err)
	}

	structuredDir := t.TempDir()
	structuredTarget := filepath.Join(structuredDir, "outside.log")
	writeFile(t, structuredTarget, `{"action":"outside","message":"secret"}`+"\n")
	if err := os.Symlink(structuredTarget, filepath.Join(structuredDir, "cm-2026-09-08.log")); err != nil {
		t.Fatal(err)
	}
	structuredManager := NewLogManager(structuredDir)
	dest := filepath.Join(structuredDir, "structured-symlink.zip")
	if _, err := structuredManager.ExportWithOptions(LogExportOptions{Destination: dest}); err == nil || !strings.Contains(err.Error(), "refuse non-regular") {
		t.Fatalf("structured symlink export error = %v", err)
	}
}

func TestStructuredRotationRollsBackOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm-2026-09-08.log")
	original := map[string]string{path: "current\n", path + ".1": "one\n", path + ".2": "two\n"}
	for name, content := range original {
		writeFile(t, name, content)
	}
	calls := 0
	rename := func(from, to string) error {
		calls++
		if calls == 2 {
			return errors.New("injected rename failure")
		}
		return os.Rename(from, to)
	}
	if err := rotateLogIfNeeded(path, 100, 1, 2, rename, os.Remove); err == nil {
		t.Fatal("expected rotation failure")
	}
	for name, want := range original {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != want {
			t.Fatalf("rollback %s = %q, err=%v", name, data, err)
		}
	}
	matches, err := filepath.Glob(path + ".rotate-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("staging files remain: %v, err=%v", matches, err)
	}
}

func TestStructuredListRecoversCrashedStagingTransaction(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "cm-2026-09-08.log")
	files := []stagedLogFile{
		{Original: base, Staged: base + ".rotate-stage.0", Target: base + ".1"},
		{Original: base + ".1", Staged: base + ".rotate-stage.1", Target: base + ".2"},
	}
	writeFile(t, files[0].Staged, "current\n")
	writeFile(t, files[1].Staged, "previous\n")
	writeRotationFixture(t, base, logRotationTransaction{Phase: "staging", Files: files})
	manager := NewLogManager(dir)
	if _, err := manager.List(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, base, "current\n")
	assertFileContent(t, base+".1", "previous\n")
	assertPathMissing(t, rotationMetadataPath(base))
	assertPathMissing(t, files[0].Staged)
}

func TestStructuredListCompletesCrashedPublishingTransaction(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "cm-2026-09-08.log")
	files := []stagedLogFile{
		{Original: base, Staged: base + ".rotate-stage.0", Target: base + ".1"},
		{Original: base + ".1", Staged: base + ".rotate-stage.1", Target: base + ".2"},
		{Original: base + ".2", Staged: base + ".rotate-stage.2"},
	}
	// The oldest generation and current file remain staged; generation 1 was
	// already published to generation 2 before the simulated crash.
	writeFile(t, files[0].Staged, "current\n")
	writeFile(t, base+".2", "previous\n")
	writeFile(t, files[2].Staged, "oldest\n")
	writeRotationFixture(t, base, logRotationTransaction{Phase: "publishing", Files: files})
	manager := NewLogManager(dir)
	if _, err := manager.List(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, base+".1", "current\n")
	assertFileContent(t, base+".2", "previous\n")
	assertPathMissing(t, files[2].Staged)
	assertPathMissing(t, rotationMetadataPath(base))
}

func TestPEMStateLookbehindIsBoundedForHugeSparseOffset(t *testing.T) {
	reader := &countingReaderAt{fill: 'x'}
	state, err := pemStateBeforeOffset(reader, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if state {
		t.Fatal("unexpected PEM state")
	}
	if reader.read > maxPEMLookbehindBytes {
		t.Fatalf("lookbehind read %d bytes, max %d", reader.read, maxPEMLookbehindBytes)
	}
}

func TestRawRotationRecoversCrashAndConcurrentCalls(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "local-agent.out.log")
	files := []stagedLogFile{
		{Original: base, Staged: base + ".rotate-stage.0", Target: base + ".1"},
		{Original: base + ".1", Staged: base + ".rotate-stage.1"},
	}
	writeFile(t, files[0].Staged, "new-active\n")
	writeFile(t, files[1].Staged, "old-generation\n")
	writeRotationFixture(t, base, logRotationTransaction{Phase: "publishing", Files: files})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rotateLocalAgentRawLogs(dir); err != nil {
				t.Errorf("rotate raw logs: %v", err)
			}
		}()
	}
	wg.Wait()
	assertFileContent(t, base+".1", "new-active\n")
	assertPathMissing(t, files[1].Staged)
	assertPathMissing(t, rotationMetadataPath(base))
}

func TestStructuredExportBlocksRotationUntilSecureReadCompletes(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.MaxStructuredBytes = 300
	manager.Now = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	if err := manager.Write(LogEntry{Action: "before-export", Message: strings.Repeat("a", 80)}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	manager.BeforeStructuredExportRead = func() {
		close(entered)
		<-release
	}
	exportDone := make(chan error, 1)
	go func() {
		_, err := manager.ExportWithOptions(LogExportOptions{Destination: filepath.Join(dir, "concurrent-structured.zip")})
		exportDone <- err
	}()
	<-entered
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- manager.Write(LogEntry{Action: "during-export", Message: strings.Repeat("b", 180)})
	}()
	assertStillBlocked(t, writeDone)
	close(release)
	if err := <-exportDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	exported := readZipContents(t, filepath.Join(dir, "concurrent-structured.zip"))["cm-2026-09-08.log"]
	if !strings.Contains(exported, "before-export") || strings.Contains(exported, "during-export") {
		t.Fatalf("structured export snapshot = %s", exported)
	}
}

func TestRawExportBlocksRotationUntilSecureReadCompletes(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	writeFile(t, filepath.Join(dir, "local-agent.out.log"), strings.Repeat("raw-before-export\n", 70000))
	entered := make(chan struct{})
	release := make(chan struct{})
	manager.BeforeRawExportRead = func() {
		close(entered)
		<-release
	}
	exportDone := make(chan error, 1)
	dest := filepath.Join(dir, "concurrent-raw.zip")
	go func() {
		_, err := manager.ExportWithOptions(LogExportOptions{Destination: dest, IncludeRaw: true})
		exportDone <- err
	}()
	<-entered
	rotateDone := make(chan error, 1)
	go func() { rotateDone <- rotateLocalAgentRawLogs(dir) }()
	assertStillBlocked(t, rotateDone)
	close(release)
	if err := <-exportDone; err != nil {
		t.Fatal(err)
	}
	if err := <-rotateDone; err != nil {
		t.Fatal(err)
	}
	exported := readZipContents(t, dest)["raw/local-agent.out.log"]
	if !strings.Contains(exported, "raw-before-export") {
		t.Fatalf("raw export lost source during concurrent rotation")
	}
	assertFileContent(t, filepath.Join(dir, "local-agent.out.log.1"), strings.Repeat("raw-before-export\n", 70000))
}

func assertStillBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("operation completed before export lock released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

type countingReaderAt struct {
	read int64
	fill byte
}

func (r *countingReaderAt) ReadAt(p []byte, _ int64) (int, error) {
	for i := range p {
		p[i] = r.fill
	}
	r.read += int64(len(p))
	return len(p), nil
}

func writeRotationFixture(t *testing.T, base string, transaction logRotationTransaction) {
	t.Helper()
	data, err := json.Marshal(transaction)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, rotationMetadataPath(base), string(data))
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("%s = %q, err=%v, want %q", path, data, err, want)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent, err=%v", path, err)
	}
}

func TestLogExportAtomicFailurePreservesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	if err := manager.Write(LogEntry{Action: "export.atomic", Message: "safe"}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "existing.zip")
	writeFile(t, dest, "existing-content")
	manager.Rename = func(string, string) error { return errors.New("injected publish failure") }
	if _, err := manager.ExportWithOptions(LogExportOptions{Destination: dest}); err == nil {
		t.Fatal("expected export failure")
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "existing-content" {
		t.Fatalf("existing destination changed: %q err=%v", data, err)
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".connectmac-logs-*.zip.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary exports remain: %v err=%v", temps, err)
	}
}

func TestRawExportIncludesBoundedJobRunLogsWithoutSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	jobDir := filepath.Join(jobs, "aws-open-profile-123")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(jobDir, "run.log"), "job token=secret 192.0.2.10\n")
	outside := filepath.Join(dir, "outside.log")
	writeFile(t, outside, "must-not-export\n")
	linkedDir := filepath.Join(jobs, "linked-job")
	if err := os.Symlink(filepath.Dir(outside), linkedDir); err != nil {
		t.Fatal(err)
	}
	manager := NewLogManager(filepath.Join(dir, "logs"))
	dest := filepath.Join(dir, "jobs.zip")
	if _, err := manager.ExportWithOptions(LogExportOptions{Destination: dest, IncludeRaw: true, JobRoots: []string{jobs}}); err != nil {
		t.Fatal(err)
	}
	contents := readZipContents(t, dest)
	exported, ok := contents["raw/jobs/aws-open-profile-123/run.log"]
	if !ok || strings.Contains(exported, "secret") || strings.Contains(exported, "192.0.2.10") {
		t.Fatalf("job run log = %q, present=%t", exported, ok)
	}
	for name, content := range contents {
		if strings.Contains(name, dir) || strings.Contains(content, "must-not-export") {
			t.Fatalf("symlink/path escaped through %q", name)
		}
	}
}

func TestRotateLocalAgentRawLogs(t *testing.T) {
	dir := t.TempDir()
	large := strings.Repeat("x", 1024*1024+1)
	writeFile(t, filepath.Join(dir, "local-agent.out.log"), large)
	writeFile(t, filepath.Join(dir, "local-agent.out.log.1"), "old")
	writeFile(t, filepath.Join(dir, "local-agent.err.log"), "small")
	if err := rotateLocalAgentRawLogs(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "local-agent.out.log.1"))
	if err != nil || string(data) != large {
		t.Fatalf("rotated output: len=%d err=%v", len(data), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "local-agent.out.log")); !os.IsNotExist(err) {
		t.Fatalf("active output should be recreated by launchd: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "local-agent.err.log")); err != nil || string(data) != "small" {
		t.Fatalf("small stderr changed: %q %v", data, err)
	}
}

func readZipContents(t *testing.T, path string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	result := make(map[string]string, len(zr.File))
	for _, file := range zr.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		result[file.Name] = string(data)
	}
	return result
}

func TestReconcileInterruptedTransfersIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	manager.Now = func() time.Time { return now }
	writeTransferLogFixture(t, manager, "complete", "transfer.local.started", LocalTransferRunning)
	writeTransferLogFixture(t, manager, "complete", "transfer.local.succeeded", LocalTransferSucceeded)
	writeTransferLogFixture(t, manager, "orphan", "transfer.local.started", LocalTransferRunning)

	if err := manager.ReconcileInterruptedTransfers("local agent restarted"); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileInterruptedTransfers("local agent restarted"); err != nil {
		t.Fatal(err)
	}

	entries, err := manager.ReadSince(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	interrupted := 0
	for _, entry := range entries {
		if entry.TransferID != "orphan" || entry.Action != "transfer.local.interrupted" {
			continue
		}
		interrupted++
		if entry.LocalJobID != "job-orphan" || entry.Profile != "profile-orphan" ||
			entry.Direction != "push" || entry.RequestID != "request-orphan" ||
			entry.Source != "local-agent-recovery" || entry.ErrorCode != "agent_restarted" ||
			entry.Outcome != "failure" || entry.Level != "warn" ||
			entry.Status != LocalTransferInterrupted || entry.Phase != TransferPhaseInterrupted {
			t.Fatalf("interrupted entry = %+v", entry)
		}
	}
	if interrupted != 1 {
		t.Fatalf("interrupted count = %d, entries = %+v", interrupted, entries)
	}
}

func TestLogManagerReadSinceSkipsMalformedAndLegacyLines(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	manager.Now = func() time.Time { return now }
	path := filepath.Join(dir, "cm-2026-09-08.log")
	data := strings.Join([]string{
		`not json`,
		`{"action":"legacy.without-time"}`,
		`{"time":"2026-09-08T10:00:00Z","action":"old"}`,
		`{"time":"2026-09-08T11:30:00Z","action":"recent"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := manager.ReadSince(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Action != "recent" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestReconcileInterruptedTransfersSkipsOversizedLineAndContinues(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	manager.Now = func() time.Time { return now }
	writeTransferLogFixture(t, manager, "complete", "transfer.local.started", LocalTransferRunning)

	path := filepath.Join(dir, "cm-2026-09-08.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("x", maxStructuredLogLineBytes+1) + "\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	writeTransferLogFixture(t, manager, "complete", "transfer.local.succeeded", LocalTransferSucceeded)
	writeTransferLogFixture(t, manager, "orphan", "transfer.local.started", LocalTransferRunning)
	if err := manager.ReconcileInterruptedTransfers("local agent restarted"); err != nil {
		t.Fatal(err)
	}
	entries, err := manager.ReadSince(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, entry := range entries {
		if entry.Action == "transfer.local.interrupted" {
			counts[entry.TransferID]++
		}
	}
	if counts["complete"] != 0 || counts["orphan"] != 1 {
		t.Fatalf("interrupted counts = %#v", counts)
	}
}

func writeTransferLogFixture(t *testing.T, manager LogManager, transferID, action, status string) {
	t.Helper()
	if err := manager.Write(LogEntry{
		Action: action, TransferID: transferID, LocalJobID: "job-" + transferID,
		Profile: "profile-" + transferID, Direction: "push", Status: status,
		RequestID: "request-" + transferID, Source: "web-local", Message: action,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLogManagerStructuredRedaction(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.Now = func() time.Time {
		return time.Date(2026, 7, 30, 10, 30, 0, 0, time.UTC)
	}
	entry := LogEntry{
		RequestID:        "request-1",
		JobID:            "job-1",
		CycleID:          "arc-cycle-1",
		SessionIDHash:    "sha256:session-1",
		Operation:        "aws.open",
		Source:           "web",
		Phase:            "requested",
		ErrorCode:        "aws_permission_denied",
		Attempt:          2,
		HTTPStatus:       403,
		MemberEmail:      "password=structured-field-secret",
		ActorMemberID:    "member-1",
		ActorMemberEmail: "actor@example.com",
		ActorMemberName:  "Actor Name",
		Message: strings.Join([]string{
			"Authorization: Bearer bearer-secret",
			"Authorization: Basic basic-secret",
			"Authorization: AWS4-HMAC-SHA256 Credential=auth-credential-secret, SignedHeaders=host, Signature=auth-signature-secret",
			"Cookie: cm_session=cookie-secret; preference=dark",
			"Set-Cookie: cm_session=set-cookie-secret; HttpOnly",
			"vnc://member:uri-password-secret@localhost:5900",
			"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=webhook-secret&safe=1",
			`{"token":"json-token-secret","access_token":"json-access-token-secret","secret":"json-secret","session":"json-session-secret","password":"json-password-secret","cookie":"json-cookie-secret"}`,
			`{"AWS_ACCESS_KEY_ID":"json-access-key-secret","aws_secret_access_key":"json-aws-secret","AWS_SESSION_TOKEN":"json-session-token-secret"}`,
			`{"client_secret":"json-client-secret","SecretAccessKey":"json-secret-access-key","SessionToken":"json-session-token"}`,
			"https://ec2.amazonaws.com/?X-Amz-Credential=presigned-credential-secret%2F20260730&X-Amz-Security-Token=presigned-session-secret&X-Amz-Signature=presigned-signature-secret",
			"https://example.com/?access_token=query-access-token-secret&AWSAccessKeyId=query-access-key-secret",
			"password=password-secret token=token-secret secret=field-secret session=session-secret",
			"session_token=generic-session-token-secret",
			"pem_path=/Users/test/.ssh/automation-private.pem",
			"load key /Users/test/.ssh/fallback-private.pem failed",
			"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
			"AWS_SECRET_ACCESS_KEY=aws-secret-value",
			"aws_access_key_id: ASIAIOSFODNN7EXAMPLE",
			"aws_secret_access_key: another-aws-secret",
			"-----BEGIN PRIVATE KEY-----\npem-secret\n-----END PRIVATE KEY-----",
		}, "\n"),
	}
	if err := manager.Write(entry); err != nil {
		t.Fatalf("write log: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "cm-2026-07-30.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, secret := range []string{
		"bearer-secret",
		"basic-secret",
		"auth-credential-secret",
		"auth-signature-secret",
		"cookie-secret",
		"set-cookie-secret",
		"uri-password-secret",
		"webhook-secret",
		"password-secret",
		"token-secret",
		"field-secret",
		"session-secret",
		"generic-session-token-secret",
		"/Users/test/.ssh/automation-private.pem",
		"/Users/test/.ssh/fallback-private.pem",
		"AKIAIOSFODNN7EXAMPLE",
		"ASIAIOSFODNN7EXAMPLE",
		"aws-secret-value",
		"another-aws-secret",
		"pem-secret",
		"structured-field-secret",
		"json-token-secret",
		"json-access-token-secret",
		"json-secret",
		"json-session-secret",
		"json-password-secret",
		"json-cookie-secret",
		"json-access-key-secret",
		"json-aws-secret",
		"json-session-token-secret",
		"json-client-secret",
		"json-secret-access-key",
		"json-session-token",
		"presigned-credential-secret",
		"presigned-session-secret",
		"presigned-signature-secret",
		"query-access-token-secret",
		"query-access-key-secret",
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("raw log contains secret %q: %s", secret, raw)
		}
	}

	var got LogEntry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if got.RequestID != entry.RequestID ||
		got.JobID != entry.JobID ||
		got.CycleID != entry.CycleID ||
		got.SessionIDHash != entry.SessionIDHash ||
		got.Operation != entry.Operation ||
		got.Source != entry.Source ||
		got.Phase != entry.Phase ||
		got.ErrorCode != entry.ErrorCode ||
		got.Attempt != entry.Attempt ||
		got.HTTPStatus != entry.HTTPStatus ||
		got.ActorMemberID != entry.ActorMemberID ||
		got.ActorMemberEmail != entry.ActorMemberEmail ||
		got.ActorMemberName != entry.ActorMemberName {
		t.Fatalf("structured log = %+v", got)
	}
	rawText := string(raw)
	for _, field := range []string{
		`"cycle_id":"arc-cycle-1"`,
		`"actor_member_id":"member-1"`,
		`"actor_member_email":"actor@example.com"`,
		`"actor_member_name":"Actor Name"`,
		`\"token\":\"[REDACTED]\"`,
		`\"access_token\":\"[REDACTED]\"`,
		`\"AWS_ACCESS_KEY_ID\":\"[REDACTED]\"`,
		`\"client_secret\":\"[REDACTED]\"`,
		`\"SecretAccessKey\":\"[REDACTED]\"`,
		`\"SessionToken\":\"[REDACTED]\"`,
		`X-Amz-Credential=[REDACTED]`,
		`X-Amz-Security-Token=[REDACTED]`,
	} {
		if !strings.Contains(rawText, field) {
			t.Fatalf("raw log missing actor field %s: %s", field, raw)
		}
	}
	if strings.Contains(rawText, "qyapi.weixin.qq.com") {
		t.Fatalf("raw log retained full webhook URL: %s", raw)
	}
}

func TestLogManagerRedactsLongPEMBeforeTruncating(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.Now = func() time.Time {
		return time.Date(2026, 7, 30, 10, 30, 0, 0, time.UTC)
	}
	secret := strings.Repeat("private-material-", 400)
	message := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + secret +
		"\n-----END OPENSSH PRIVATE KEY-----\nsafe suffix"
	if err := manager.Write(LogEntry{Action: "test", Message: message}); err != nil {
		t.Fatalf("write log: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "cm-2026-07-30.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(raw), "private-material") ||
		strings.Contains(string(raw), "OPENSSH PRIVATE KEY") {
		t.Fatalf("raw log contains long PEM material: %s", raw)
	}
}

func TestLogManagerRedactsUnterminatedPEMBlock(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.Now = func() time.Time {
		return time.Date(2026, 7, 30, 10, 30, 0, 0, time.UTC)
	}
	message := "command failed\n-----BEGIN PRIVATE KEY-----\nunterminated-pem-secret"
	if err := manager.Write(LogEntry{Action: "test", Message: message}); err != nil {
		t.Fatalf("write log: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "cm-2026-07-30.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(raw), "unterminated-pem-secret") ||
		strings.Contains(string(raw), "BEGIN PRIVATE KEY") {
		t.Fatalf("raw log contains unterminated PEM material: %s", raw)
	}
}

func TestLogManagerRedactsPlainSensitiveAssignments(t *testing.T) {
	dir := t.TempDir()
	manager := NewLogManager(dir)
	manager.Now = func() time.Time {
		return time.Date(2026, 7, 30, 10, 30, 0, 0, time.UTC)
	}
	message := strings.Join([]string{
		"access_token=form-access-secret",
		"client_secret=form-client-secret",
		"AWSAccessKeyId=form-access-key",
		"SecretAccessKey=form-secret-key",
		"SessionToken=form-session-token",
		"password=existing-password",
		"token=existing-token",
		"secret=existing-secret",
		"session=existing-session",
		"cookie=existing-cookie",
	}, "&")
	if err := manager.Write(LogEntry{Action: "test", Message: message}); err != nil {
		t.Fatalf("write log: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "cm-2026-07-30.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, secret := range []string{
		"form-access-secret",
		"form-client-secret",
		"form-access-key",
		"form-secret-key",
		"form-session-token",
		"existing-password",
		"existing-token",
		"existing-secret",
		"existing-session",
		"existing-cookie",
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("raw log contains plain assignment secret %q: %s", secret, raw)
		}
	}

	var got LogEntry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	want := strings.Join([]string{
		"access_token=[REDACTED]",
		"client_secret=[REDACTED]",
		"AWSAccessKeyId=[REDACTED]",
		"SecretAccessKey=[REDACTED]",
		"SessionToken=[REDACTED]",
		"password=[REDACTED]",
		"token=[REDACTED]",
		"secret=[REDACTED]",
		"session=[REDACTED]",
		"cookie=[REDACTED]",
	}, "&")
	if got.Message != want {
		t.Fatalf("redacted message = %q, want %q", got.Message, want)
	}
}

func TestSanitizeOperationalErrorTextRedactsSensitiveValues(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		secrets []string
	}{
		{
			name: "webhook URLs and keys",
			input: strings.Join([]string{
				"post https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=webhook-url-secret failed",
				"wechat webhook key=standalone-webhook-key-secret",
				`{"key":"json-webhook-key-secret"}`,
				"webhook_url=https://hooks.example.invalid/send?key=assigned-webhook-url-secret",
			}, "\n"),
			secrets: []string{"webhook-url-secret", "standalone-webhook-key-secret", "json-webhook-key-secret", "assigned-webhook-url-secret", "qyapi.weixin.qq.com", "hooks.example.invalid"},
		},
		{
			name: "cookie headers",
			input: strings.Join([]string{
				"Cookie: cm_session=cookie-session-secret; preference=cookie-preference-secret",
				"Set-Cookie: cm_session=set-cookie-session-secret; Path=/; HttpOnly",
				"Set-Cookie: csrf=set-cookie-csrf-secret; Path=/; Secure",
			}, "\n"),
			secrets: []string{"cookie-session-secret", "cookie-preference-secret", "set-cookie-session-secret", "set-cookie-csrf-secret"},
		},
		{
			name:    "URL basic auth credentials",
			input:   "request https://basic-user-secret:basic-password-secret@example.invalid/path failed",
			secrets: []string{"basic-user-secret", "basic-password-secret"},
		},
		{
			name: "strict assignments",
			input: strings.Join([]string{
				"token=assigned-token-secret",
				"session: 'assigned-session-secret'",
				`secret = "assigned-secret-secret"`,
				"password: assigned-password-secret",
				"access_key=assigned-access-key-secret",
				`secret_access_key: "assigned-secret-access-key-secret"`,
			}, "\n"),
			secrets: []string{"assigned-token-secret", "assigned-session-secret", "assigned-secret-secret", "assigned-password-secret", "assigned-access-key-secret", "assigned-secret-access-key-secret"},
		},
		{
			name:  "quoted JSON assignments",
			input: `{"token":"json-token-secret","session":'json-session-secret','secret':"json-secret-secret","password"='json-password-secret',"access_key":"json-access-key-secret",'secret_access_key':'json-secret-access-key-secret'}`,
			secrets: []string{
				"json-token-secret", "json-session-secret", "json-secret-secret",
				"json-password-secret", "json-access-key-secret", "json-secret-access-key-secret",
			},
		},
		{
			name: "authorization and bearer values",
			input: strings.Join([]string{
				"Authorization: Bearer authorization-bearer-secret",
				"Bearer standalone-bearer-secret",
			}, "\n"),
			secrets: []string{"authorization-bearer-secret", "standalone-bearer-secret"},
		},
		{
			name: "AWS assignments and access key IDs",
			input: strings.Join([]string{
				"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
				"AWS_SECRET_ACCESS_KEY=aws-secret-value",
			}, "\n"),
			secrets: []string{"AKIAIOSFODNN7EXAMPLE", "aws-secret-value"},
		},
		{
			name: "PEM paths",
			input: strings.Join([]string{
				"load /Users/test/.ssh/absolute-private.pem failed",
				"load ~/.ssh/home-private.pem failed",
			}, "\n"),
			secrets: []string{"/Users/test/.ssh/absolute-private.pem", "~/.ssh/home-private.pem"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := sanitizeOperationalErrorText(test.input)
			for _, secret := range test.secrets {
				if strings.Contains(got, secret) {
					t.Fatalf("operational error retained %q: %s", secret, got)
				}
			}
			if !strings.Contains(got, "[REDACTED") {
				t.Fatalf("operational error did not record redaction: %s", got)
			}
		})
	}
}

func TestSanitizeOperationalErrorTextPreservesNoValueDiagnosticsExactly(t *testing.T) {
	for _, diagnostic := range []string{"token expired", "session unavailable", "secret rotation failed"} {
		t.Run(diagnostic, func(t *testing.T) {
			if got := sanitizeOperationalErrorText(diagnostic); got != diagnostic {
				t.Fatalf("sanitizeOperationalErrorText(%q) = %q", diagnostic, got)
			}
		})
	}
}
