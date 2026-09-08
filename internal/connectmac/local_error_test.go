package connectmac

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

type timeoutTestError struct{}

func (timeoutTestError) Error() string   { return "network timeout" }
func (timeoutTestError) Timeout() bool   { return true }
func (timeoutTestError) Temporary() bool { return true }

var _ net.Error = timeoutTestError{}

func TestClassifyLocalOperationError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
		wantLvl  string
		wantExit int
	}{
		{name: "host identification changed", err: errors.New("WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!"), wantCode: "host_key_changed", wantLvl: "error"},
		{name: "host key verification", err: errors.New("Host key verification failed"), wantCode: "host_key_changed", wantLvl: "error"},
		{name: "exit 255", err: errors.New("exit status 255"), wantCode: "ssh_exit_255", wantLvl: "error", wantExit: 255},
		{name: "operation timeout", err: errors.New("Operation timed out"), wantCode: "ssh_timeout", wantLvl: "warn"},
		{name: "deadline", err: context.DeadlineExceeded, wantCode: "ssh_timeout", wantLvl: "warn"},
		{name: "network timeout", err: timeoutTestError{}, wantCode: "ssh_timeout", wantLvl: "warn"},
		{name: "port occupied", err: errors.New("error: local port 5900 is already in use"), wantCode: "local_port_in_use", wantLvl: "error"},
		{name: "refused", err: errors.New("dial tcp: connection refused"), wantCode: "connection_refused", wantLvl: "error"},
		{name: "normal", wantCode: "closed", wantLvl: "info"},
		{name: "user close", err: context.Canceled, wantCode: "user_canceled", wantLvl: "info"},
		{name: "browser close", err: &websocket.CloseError{Code: websocket.CloseNormalClosure}, wantCode: "browser_closed", wantLvl: "info"},
		{name: "browser going away", err: &websocket.CloseError{Code: websocket.CloseGoingAway}, wantCode: "browser_closed", wantLvl: "info"},
		{name: "browser no status", err: &websocket.CloseError{Code: websocket.CloseNoStatusReceived}, wantCode: "browser_disconnected", wantLvl: "warn"},
		{name: "coded", err: LocalCodedError{Code: "profile_invalid", Cause: errors.New("bad profile")}, wantCode: "profile_invalid", wantLvl: "error"},
		{name: "generic coded host key", err: LocalCodedError{Code: "command_failed", Cause: errors.New("Host key verification failed")}, wantCode: "host_key_changed", wantLvl: "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyLocalOperationError(test.err)
			if got.Code != test.wantCode || got.Level != test.wantLvl || got.ExitCode != test.wantExit {
				t.Fatalf("classifyLocalOperationError()=%+v", got)
			}
		})
	}
}

func TestClassifyLocalOperationErrorSanitizesDetail(t *testing.T) {
	got := classifyLocalOperationError(errors.New("connection refused Authorization: Bearer secret-value"))
	if got.Detail == "" || got.Detail == "connection refused Authorization: Bearer secret-value" {
		t.Fatalf("detail was not sanitized: %q", got.Detail)
	}
}

func TestClassifyLocalOperationErrorHostKeyOutranksWrappedExit255(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestHelperProcessExit255")
	command.Env = append(os.Environ(), "GO_WANT_HELPER_EXIT_255=1")
	exitErr := command.Run()
	got := classifyLocalOperationError(fmt.Errorf("REMOTE HOST IDENTIFICATION HAS CHANGED: %w", exitErr))
	if got.Code != "host_key_changed" || got.ExitCode != 255 {
		t.Fatalf("classification=%+v", got)
	}
}

func TestHelperProcessExit255(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_EXIT_255") != "1" {
		return
	}
	os.Exit(255)
}

func TestFinalizeTerminalClosedEntryContract(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantOutcome string
		wantLevel   string
		wantCode    string
	}{
		{name: "normal", wantOutcome: "success", wantLevel: "info"},
		{name: "browser normal", err: &websocket.CloseError{Code: websocket.CloseNormalClosure}, wantOutcome: "success", wantLevel: "info"},
		{name: "browser no status", err: &websocket.CloseError{Code: websocket.CloseNoStatusReceived}, wantOutcome: "failure", wantLevel: "warn", wantCode: "browser_disconnected"},
		{name: "wrapped browser normal is failure", err: fmt.Errorf("terminal websocket write: %w", &websocket.CloseError{Code: websocket.CloseNormalClosure}), wantOutcome: "failure", wantLevel: "error", wantCode: "websocket_closed"},
		{name: "browser abnormal", err: &websocket.CloseError{Code: websocket.CloseInternalServerErr, Text: "token=session-secret"}, wantOutcome: "failure", wantLevel: "error", wantCode: "websocket_closed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := finalizeTerminalClosedEntry(LogEntry{Action: "terminal.closed", Source: "web-server"}, test.err)
			if got.Outcome != test.wantOutcome || got.Level != test.wantLevel || got.ErrorCode != test.wantCode {
				t.Fatalf("entry=%+v", got)
			}
			if strings.Contains(got.Message, "session-secret") {
				t.Fatalf("unsanitized detail=%q", got.Message)
			}
		})
	}
}

func TestVNCFailureContract(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		stage     string
		wantCode  string
		wantLevel string
	}{
		{name: "profile", err: LocalCodedError{Code: "profile_invalid", Cause: errors.New("invalid profile")}, stage: "profile", wantCode: "profile_invalid", wantLevel: "error"},
		{name: "port occupied", err: errors.New("local port 5900 is already in use"), stage: "tunnel", wantCode: "local_port_in_use", wantLevel: "error"},
		{name: "connection refused", err: errors.New("dial tcp: connection refused"), stage: "tunnel", wantCode: "connection_refused", wantLevel: "error"},
		{name: "screen sharing", err: LocalCodedError{Code: "screen_sharing_failed", Cause: errors.New("open failed")}, stage: "screen-sharing", wantCode: "screen_sharing_failed", wantLevel: "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := LogEntry{Action: "local-agent.vnc", PID: 42, LocalPorts: []int{5900}, TunnelAction: "reused"}
			got := applyLocalOperationFailure(base, test.err, 1, test.stage)
			if got.ErrorCode != test.wantCode || got.Level != test.wantLevel || got.FailureStage != test.stage ||
				got.ExitCode != 1 || got.PID != 42 || len(got.LocalPorts) != 1 || got.LocalPorts[0] != 5900 || got.TunnelAction != "reused" {
				t.Fatalf("entry=%+v", got)
			}
		})
	}
}
