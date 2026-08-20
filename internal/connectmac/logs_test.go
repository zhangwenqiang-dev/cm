package connectmac

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	if len(zr.File) != 1 || zr.File[0].Name != "cm-2026-07-02.log" {
		t.Fatalf("zip files = %+v", zr.File)
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
