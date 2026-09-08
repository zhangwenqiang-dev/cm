package connectmac

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strings"

	"github.com/gorilla/websocket"
)

type LocalOperationError struct {
	Code     string
	Level    string
	Detail   string
	ExitCode int
}

type LocalCodedError struct {
	Code  string
	Cause error
}

func (e LocalCodedError) Error() string {
	if e.Cause == nil {
		return e.Code
	}
	return e.Cause.Error()
}

func (e LocalCodedError) Unwrap() error { return e.Cause }

func (e LocalCodedError) ErrorCode() string { return e.Code }

type localErrorCoder interface {
	ErrorCode() string
}

func classifyLocalOperationError(err error) LocalOperationError {
	if err == nil {
		return LocalOperationError{Code: "closed", Level: "info"}
	}

	result := LocalOperationError{Code: "local_operation_failed", Level: "error", Detail: sanitizeOperationalErrorText(err.Error())}
	var coded localErrorCoder
	codedCode := ""
	if errors.As(err, &coded) && strings.TrimSpace(coded.ErrorCode()) != "" {
		codedCode = strings.TrimSpace(coded.ErrorCode())
		if codedCode != "command_failed" && codedCode != "local_operation_failed" {
			result.Code = codedCode
			return result
		}
	}
	message := strings.ToLower(err.Error())
	if containsAny(message, "remote host identification has changed", "host key verification failed") {
		result.Code = "host_key_changed"
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
		return result
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		if result.ExitCode == 255 {
			result.Code = "ssh_exit_255"
		} else {
			result.Code = "ssh_exit"
		}
		return result
	}

	if errors.Is(err, context.Canceled) {
		result.Code, result.Level = "user_canceled", "info"
		return result
	}
	if errors.Is(err, context.DeadlineExceeded) {
		result.Code, result.Level = "ssh_timeout", "warn"
		return result
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		result.Code, result.Level = "ssh_timeout", "warn"
		return result
	}
	if closeErr, ok := err.(*websocket.CloseError); ok {
		switch closeErr.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway:
			result.Code, result.Level = "browser_closed", "info"
			return result
		case websocket.CloseNoStatusReceived:
			result.Code, result.Level = "browser_disconnected", "warn"
			return result
		default:
			result.Code = "websocket_closed"
			return result
		}
	}
	var wrappedClose *websocket.CloseError
	if errors.As(err, &wrappedClose) {
		result.Code = "websocket_closed"
		return result
	}

	switch {
	case containsAny(message, "operation timed out", "i/o timeout", "connection timed out"):
		result.Code, result.Level = "ssh_timeout", "warn"
	case strings.Contains(message, "local port 5900 is already in use") ||
		(strings.Contains(message, "local port") && strings.Contains(message, "already in use")):
		result.Code = "local_port_in_use"
	case strings.Contains(message, "connection refused"):
		result.Code = "connection_refused"
	case strings.Contains(message, "exit status 255"):
		result.Code, result.ExitCode = "ssh_exit_255", 255
	case codedCode != "":
		result.Code = codedCode
	}
	return result
}

func applyLocalOperationFailure(entry LogEntry, err error, fallbackExitCode int, stage string) LogEntry {
	classified := classifyLocalOperationError(err)
	entry.Level = classified.Level
	entry.ErrorCode = classified.Code
	entry.ExitCode = classified.ExitCode
	if entry.ExitCode == 0 {
		entry.ExitCode = fallbackExitCode
	}
	entry.FailureStage = stage
	entry.Message = classified.Detail
	entry.Outcome = "failure"
	return entry
}

func finalizeTerminalClosedEntry(entry LogEntry, err error) LogEntry {
	if normalTerminalClose(err) {
		entry.Level = "info"
		entry.Outcome = "success"
		if entry.Message == "" {
			entry.Message = "terminal.closed reason=normal"
		}
		return entry
	}
	return applyLocalOperationFailure(entry, err, 1, "session")
}
