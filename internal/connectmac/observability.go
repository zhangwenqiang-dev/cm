package connectmac

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type AuditActor struct {
	MemberID    string
	MemberEmail string
	MemberName  string
}

type OperationContext struct {
	RequestID string
	JobID     string
	Source    string
	Route     string
	Method    string
	Actor     AuditActor
}

type ClassifiedError struct {
	Level string
	Code  string
	Skip  bool
}

type operationContextKey struct{}

func classifyOperationalError(err error) ClassifiedError {
	if err == nil {
		return ClassifiedError{}
	}
	if errors.Is(err, context.Canceled) {
		return ClassifiedError{Level: "debug", Code: "request_canceled", Skip: true}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassifiedError{Level: "warn", Code: "request_timeout"}
	}

	message := strings.ToLower(err.Error())
	switch {
	case containsAny(message,
		"failed to get shared config profile",
		"failed to load shared config profile",
		"shared config profile does not exist",
		"config profile could not be found",
		"profile was not found in the configuration",
	):
		return ClassifiedError{Level: "error", Code: "aws_profile_missing"}
	case containsAny(message,
		"accessdenied",
		"access denied",
		"not authorized",
		"unauthorizedoperation",
		"authfailure",
		"unrecognizedclientexception",
		"invalidclienttokenid",
		"expiredtoken",
		"signaturedoesnotmatch",
	):
		return ClassifiedError{Level: "error", Code: "aws_permission_denied"}
	case containsAny(message,
		"insufficienthostcapacity",
		"insufficientinstancecapacity",
		"capacity is not available",
		"capacity unavailable",
		"unsupportedavailabilityzone",
	):
		return ClassifiedError{Level: "error", Code: "aws_capacity_unavailable"}
	case containsAny(message,
		"database",
		"mysql",
		"sql:",
		"storage",
		"persistence",
		"persist event",
		"disk full",
		"no space left on device",
		"read-only file system",
	):
		return ClassifiedError{Level: "error", Code: "storage_error"}
	case containsAny(message,
		"wechat",
		"we chat",
		"webhook",
		"notification",
		"notify",
	):
		return ClassifiedError{Level: "error", Code: "notification_error"}
	case containsAny(message,
		"rsync",
		"transfer",
		"scp ",
		"sftp",
	):
		return ClassifiedError{Level: "error", Code: "transfer_error"}
	case containsAny(message, "config.yaml", "config.yml", "config file", "configuration file") &&
		containsAny(message, "no such file", "does not exist", "not found", "missing"):
		return ClassifiedError{Level: "error", Code: "config_missing"}
	case os.IsNotExist(err) || containsAny(message, "no such file or directory", "file does not exist"):
		return ClassifiedError{Level: "error", Code: "storage_error"}
	case containsAny(message, "parse config", "invalid configuration", "invalid config"):
		return ClassifiedError{Level: "error", Code: "config_invalid"}
	case containsAny(message, "permission denied", "forbidden", "authorization denied"):
		return ClassifiedError{Level: "error", Code: "authorization_denied"}
	case containsAny(message, "validation failed", "is required", "invalid value"):
		return ClassifiedError{Level: "error", Code: "validation_error"}
	default:
		return ClassifiedError{Level: "error", Code: "aws_api_error"}
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func withOperationContext(ctx context.Context, value OperationContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, operationContextKey{}, value)
}

func operationContextFrom(ctx context.Context) OperationContext {
	if ctx == nil {
		return OperationContext{}
	}
	value, _ := ctx.Value(operationContextKey{}).(OperationContext)
	return value
}

func newRequestID(now time.Time, random io.Reader) (string, error) {
	if random == nil {
		return "", errors.New("request ID random source is required")
	}
	data := make([]byte, 16)
	if _, err := io.ReadFull(random, data); err != nil {
		return "", fmt.Errorf("generate request ID: %w", err)
	}
	return "req-" + now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(data), nil
}

func (a App) writeRuntimeLog(entry LogEntry) {
	if err := a.LogManager.Write(entry); err != nil {
		writer := a.Err
		if writer == nil {
			writer = os.Stderr
		}
		message := fmt.Sprintf(
			"runtime log write failed action=%s request_id=%s job_id=%s error=%s",
			entry.Action,
			entry.RequestID,
			entry.JobID,
			err,
		)
		message = strings.NewReplacer("\r", " ", "\n", " ").Replace(sanitizeLogText(message))
		_, _ = fmt.Fprintln(writer, message)
	}
}

func elapsedDurationMS(startedAt time.Time) int64 {
	return positiveDurationMS(time.Since(startedAt))
}

func positiveDurationMS(duration time.Duration) int64 {
	elapsed := duration.Milliseconds()
	if elapsed < 1 {
		return 1
	}
	return elapsed
}
