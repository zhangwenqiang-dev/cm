package connectmac

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClassifyOperationalError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ClassifiedError
	}{
		{
			name: "canceled request",
			err:  context.Canceled,
			want: ClassifiedError{Level: "debug", Code: "request_canceled", Skip: true},
		},
		{
			name: "request timeout",
			err:  context.DeadlineExceeded,
			want: ClassifiedError{Level: "warn", Code: "request_timeout"},
		},
		{
			name: "wrapped canceled request",
			err:  errors.Join(errors.New("poll failed"), context.Canceled),
			want: ClassifiedError{Level: "debug", Code: "request_canceled", Skip: true},
		},
		{
			name: "AWS profile missing",
			err:  errors.New("failed to get shared config profile, cm-user"),
			want: ClassifiedError{Level: "error", Code: "aws_profile_missing"},
		},
		{
			name: "AWS permission denied",
			err:  errors.New("AccessDenied: not authorized to perform ec2:DescribeInstances"),
			want: ClassifiedError{Level: "error", Code: "aws_permission_denied"},
		},
		{
			name: "AWS capacity unavailable",
			err:  errors.New("InsufficientHostCapacity: no capacity in requested availability zone"),
			want: ClassifiedError{Level: "error", Code: "aws_capacity_unavailable"},
		},
		{
			name: "storage failure",
			err:  errors.New("mysql persistence failed: database unavailable"),
			want: ClassifiedError{Level: "error", Code: "storage_error"},
		},
		{
			name: "notification failure",
			err:  errors.New("Enterprise WeChat webhook returned status 500"),
			want: ClassifiedError{Level: "error", Code: "notification_error"},
		},
		{
			name: "transfer failure",
			err:  errors.New("rsync transfer failed with exit status 23"),
			want: ClassifiedError{Level: "error", Code: "transfer_error"},
		},
		{
			name: "missing storage file is not config missing",
			err:  errors.New("open member storage: no such file or directory"),
			want: ClassifiedError{Level: "error", Code: "storage_error"},
		},
		{
			name: "generic missing file is storage error",
			err:  errors.New("open /tmp/connectmac-state: no such file or directory"),
			want: ClassifiedError{Level: "error", Code: "storage_error"},
		},
		{
			name: "missing transfer source is not config missing",
			err:  errors.New("rsync source: no such file or directory"),
			want: ClassifiedError{Level: "error", Code: "transfer_error"},
		},
		{
			name: "explicit missing config",
			err:  errors.New("load config.yaml: no such file or directory"),
			want: ClassifiedError{Level: "error", Code: "config_missing"},
		},
		{
			name: "unknown operational error",
			err:  errors.New("EC2 request failed"),
			want: ClassifiedError{Level: "error", Code: "aws_api_error"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyOperationalError(test.err); got != test.want {
				t.Fatalf("classifyOperationalError() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestOperationContextRoundTrip(t *testing.T) {
	value := OperationContext{
		RequestID:     "request-1",
		JobID:         "job-1",
		SessionIDHash: "sha256:session-1",
		Source:        "web",
		Route:         "/api/aws/open",
		Method:        "POST",
		Actor: AuditActor{
			MemberID:    "member-1",
			MemberEmail: "member@example.com",
			MemberName:  "Member",
		},
	}

	ctx := withOperationContext(context.Background(), value)
	if got := operationContextFrom(ctx); got != value {
		t.Fatalf("operationContextFrom() = %+v, want %+v", got, value)
	}
	if got := operationContextFrom(nil); got != (OperationContext{}) {
		t.Fatalf("operationContextFrom(nil) = %+v", got)
	}
}

func TestHashSessionIdentifierIsStableAndDoesNotExposeInput(t *testing.T) {
	got := hashSessionIdentifier("session-secret")
	if got == "" || got == "session-secret" || !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("hashSessionIdentifier() = %q", got)
	}
	if got != hashSessionIdentifier("session-secret") {
		t.Fatal("session identifier hash is not stable")
	}
	if hashSessionIdentifier(" ") != "" {
		t.Fatal("blank session identifier must not produce a hash")
	}
}

func TestNewRequestID(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 34, 56, 0, time.UTC)
	random := bytes.NewReader([]byte{
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	})

	got, err := newRequestID(now, random)
	if err != nil {
		t.Fatalf("newRequestID: %v", err)
	}
	want := "req-20260730T123456Z-000102030405060708090a0b0c0d0e0f"
	if got != want {
		t.Fatalf("newRequestID() = %q, want %q", got, want)
	}

	if _, err := newRequestID(now, bytes.NewReader(nil)); err == nil {
		t.Fatal("newRequestID should fail when random data cannot be read")
	}
}

func TestAppWriteRuntimeLogFallsBackToSanitizedStderr(t *testing.T) {
	dir := t.TempDir()
	blocked := dir + "/not-a-directory"
	if err := os.WriteFile(blocked, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	app := App{
		Err:        &stderr,
		LogManager: NewLogManager(blocked),
	}

	app.writeRuntimeLog(LogEntry{
		Level:     "error",
		Action:    "web.aws.open\nforged-line",
		RequestID: "request-1",
		Message:   "failed Authorization: Bearer fallback-secret",
	})

	got := stderr.String()
	if !strings.Contains(got, "runtime log write failed") ||
		!strings.Contains(got, "web.aws.open") ||
		!strings.Contains(got, "request-1") {
		t.Fatalf("fallback stderr = %q", got)
	}
	if strings.Contains(got, "fallback-secret") {
		t.Fatalf("fallback stderr contains secret: %q", got)
	}
	if strings.Count(strings.TrimSpace(got), "\n") != 0 {
		t.Fatalf("fallback stderr should be one line: %q", got)
	}
}
