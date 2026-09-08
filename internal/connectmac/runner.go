package connectmac

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

func (ExecRunner) RunForeground(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
func (ExecRunner) StartBackground(ctx context.Context, args []string) (int, error) {
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-waitDone:
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("ssh tunnel exited before it became healthy")
	case <-timer.C:
		return pid, cmd.Process.Release()
	}
}
func (ExecRunner) Stop(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
func (ExecRunner) RunRsync(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, defaultRsyncCommandPath(), args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (ExecRunner) RunRsyncProgress(ctx context.Context, args []string, onOutput func(string)) error {
	return ExecRunner{}.RunRsyncCommandProgress(ctx, defaultRsyncCommandPath(), args, onOutput)
}

func (ExecRunner) RunRsyncCommandProgress(ctx context.Context, path string, args []string, onOutput func(string)) error {
	if path == "" {
		path = "rsync"
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = outputCallbackWriter{onOutput: onOutput}
	cmd.Stderr = outputCallbackWriter{onOutput: onOutput}
	return cmd.Run()
}

func (ExecRunner) RsyncCommandOutput(ctx context.Context, path string, args []string) ([]byte, error) {
	if path == "" {
		path = "rsync"
	}
	return exec.CommandContext(ctx, path, args...).CombinedOutput()
}

type outputCallbackWriter struct {
	onOutput func(string)
}

func (w outputCallbackWriter) Write(data []byte) (int, error) {
	if w.onOutput != nil && len(data) > 0 {
		w.onOutput(string(data))
	}
	return len(data), nil
}
func (ExecRunner) KnownHostKey(ctx context.Context, host string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-F", host)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return string(out), nil
		}
		return string(out), err
	}
	return string(out), nil
}
func (ExecRunner) ScanHostKey(ctx context.Context, host string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh-keyscan", "-T", "5", host)
	out, err := cmd.Output()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}
func (ExecRunner) ForgetHost(ctx context.Context, host string) error {
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-R", host)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
func (ExecRunner) OpenURL(ctx context.Context, target string) error {
	cmd := exec.CommandContext(ctx, "open", target)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (r ExecRunner) OpenVNC(ctx context.Context, target string) error {
	cmd := r.openVNCCommand(ctx, target)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (ExecRunner) openVNCCommand(ctx context.Context, target string) *exec.Cmd {
	return exec.CommandContext(ctx, "open", "-n", "-a", "Screen Sharing", target)
}
