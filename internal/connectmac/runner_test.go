package connectmac

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

func TestRunRsyncCommandProgressDeliversOutputWithoutMirroringProcessStreams(t *testing.T) {
	stdoutOutput, stderrOutput, callbackOutput, err := captureProgressCommandOutput(t,
		"printf 'stdout chunk'; printf 'stderr chunk' >&2",
	)
	if err != nil {
		t.Fatalf("RunRsyncCommandProgress() error = %v", err)
	}
	if stdoutOutput != "" {
		t.Fatalf("process stdout = %q, want empty", stdoutOutput)
	}
	if stderrOutput != "" {
		t.Fatalf("process stderr = %q, want empty", stderrOutput)
	}
	if !strings.Contains(callbackOutput, "stdout chunk") {
		t.Fatalf("callback output %q does not contain stdout", callbackOutput)
	}
	if !strings.Contains(callbackOutput, "stderr chunk") {
		t.Fatalf("callback output %q does not contain stderr", callbackOutput)
	}
}

func TestRunRsyncCommandProgressPreservesCommandFailure(t *testing.T) {
	stdoutOutput, stderrOutput, callbackOutput, err := captureProgressCommandOutput(t,
		"printf 'before failure'; printf 'failure detail' >&2; exit 23",
	)
	if stdoutOutput != "" || stderrOutput != "" {
		t.Fatalf("process output = stdout %q, stderr %q; want both empty", stdoutOutput, stderrOutput)
	}
	if !strings.Contains(callbackOutput, "before failure") || !strings.Contains(callbackOutput, "failure detail") {
		t.Fatalf("callback output = %q, want stdout and stderr content", callbackOutput)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("RunRsyncCommandProgress() error = %v, want *exec.ExitError", err)
	}
	if exitErr.ExitCode() != 23 {
		t.Fatalf("exit code = %d, want 23", exitErr.ExitCode())
	}
}

func captureProgressCommandOutput(t *testing.T, script string) (string, string, string, error) {
	t.Helper()

	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		stdoutReader.Close()
		stdoutWriter.Close()
		t.Fatalf("create stderr pipe: %v", err)
	}

	originalStdout := os.Stdout
	originalStderr := os.Stderr
	os.Stdout = stdoutWriter
	os.Stderr = stderrWriter
	t.Cleanup(func() {
		os.Stdout = originalStdout
		os.Stderr = originalStderr
		stdoutReader.Close()
		stderrReader.Close()
	})

	var callbackMu sync.Mutex
	var callback strings.Builder
	runErr := ExecRunner{}.RunRsyncCommandProgress(context.Background(), "/bin/sh", []string{"-c", script}, func(chunk string) {
		callbackMu.Lock()
		defer callbackMu.Unlock()
		callback.WriteString(chunk)
	})

	os.Stdout = originalStdout
	os.Stderr = originalStderr
	if err := stdoutWriter.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	if err := stderrWriter.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	stdoutOutput, err := readPipe(stdoutReader)
	if err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	stderrOutput, err := readPipe(stderrReader)
	if err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}

	callbackMu.Lock()
	callbackOutput := callback.String()
	callbackMu.Unlock()
	return stdoutOutput, stderrOutput, callbackOutput, runErr
}

func readPipe(reader *os.File) (string, error) {
	output, err := io.ReadAll(reader)
	return string(output), err
}
