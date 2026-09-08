package connectmac

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalTransferJobManagerSuccessAndFailure(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		job, err := manager.Start("mac-one", "push", func(onOutput func(string)) error {
			onOutput("  1,024  73%  1.00MB/s  0:00:01 (xfr#1, to-chk=1/4)\n")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		finished := waitForLocalTransferJob(t, manager, job.ID)
		if finished.Status != LocalTransferSucceeded || finished.Percent != 100 {
			t.Fatalf("job = %#v", finished)
		}
		if finished.Error != "" {
			t.Fatalf("success error = %q", finished.Error)
		}
		if finished.StartedAt == nil || finished.FinishedAt == nil || !strings.Contains(finished.Output, "73%") {
			t.Fatalf("job timestamps/output = %#v", finished)
		}
	})

	t.Run("failure keeps real progress", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		job, err := manager.Start("mac-one", "pull", func(onOutput func(string)) error {
			onOutput("  1,024  41%  1.00MB/s  0:00:01\nrsync: connection unexpectedly closed (code 12) at io.c\nssh: connect to host mac.example.com port 22: Connection refused\n")
			return errors.New("rsync exit status 23")
		})
		if err != nil {
			t.Fatal(err)
		}

		finished := waitForLocalTransferJob(t, manager, job.ID)
		if finished.Status != LocalTransferFailed || finished.Percent != 39 {
			t.Fatalf("job = %#v", finished)
		}
		if !strings.Contains(finished.Error, "connection unexpectedly closed") ||
			!strings.Contains(finished.Error, "Connection refused") ||
			!strings.Contains(finished.Error, "exit status 23") {
			t.Fatalf("error = %q", finished.Error)
		}
	})

	t.Run("context cancellation is canceled", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		job, err := manager.Start("mac-one", "pull", func(onOutput func(string)) error {
			onOutput("rsync: received SIGINT\n")
			return context.Canceled
		})
		if err != nil {
			t.Fatal(err)
		}
		finished := waitForLocalTransferJob(t, manager, job.ID)
		if finished.Status != LocalTransferCanceled {
			t.Fatalf("status = %q", finished.Status)
		}
		if !strings.Contains(finished.Error, "received SIGINT") || !strings.Contains(finished.Error, context.Canceled.Error()) {
			t.Fatalf("error = %q", finished.Error)
		}
	})

	t.Run("deadline is interrupted", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		job, err := manager.Start("mac-one", "pull", func(onOutput func(string)) error {
			onOutput("rsync: process disappeared\n")
			return context.DeadlineExceeded
		})
		if err != nil {
			t.Fatal(err)
		}
		finished := waitForLocalTransferJob(t, manager, job.ID)
		if finished.Status != LocalTransferInterrupted || finished.Phase != TransferPhaseInterrupted {
			t.Fatalf("job = %#v", finished)
		}
	})

	t.Run("signal killed child is interrupted", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		job, err := manager.Start("mac-one", "pull", func(onOutput func(string)) error {
			return exec.Command("sh", "-c", "kill -KILL $$").Run()
		})
		if err != nil {
			t.Fatal(err)
		}
		finished := waitForLocalTransferJob(t, manager, job.ID)
		if finished.Status != LocalTransferInterrupted || finished.Phase != TransferPhaseInterrupted {
			t.Fatalf("job = %#v", finished)
		}
	})

	t.Run("sigpipe child remains failed", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		job, err := manager.Start("mac-one", "pull", func(onOutput func(string)) error {
			return exec.Command("sh", "-c", "kill -PIPE $$").Run()
		})
		if err != nil {
			t.Fatal(err)
		}
		finished := waitForLocalTransferJob(t, manager, job.ID)
		if finished.Status != LocalTransferFailed || finished.Phase != TransferPhaseFailed {
			t.Fatalf("job = %#v", finished)
		}
	})

	t.Run("ordinary child exit remains failed", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		job, err := manager.Start("mac-one", "push", func(onOutput func(string)) error {
			return exec.Command("sh", "-c", "exit 23").Run()
		})
		if err != nil {
			t.Fatal(err)
		}
		finished := waitForLocalTransferJob(t, manager, job.ID)
		if finished.Status != LocalTransferFailed || finished.Phase != TransferPhaseFailed {
			t.Fatalf("job = %#v", finished)
		}
	})

	t.Run("does not duplicate exit error already in output", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		job, err := manager.Start("mac-one", "push", func(onOutput func(string)) error {
			onOutput("rsync: write failed\nexit status 23\n")
			return errors.New("exit status 23")
		})
		if err != nil {
			t.Fatal(err)
		}
		finished := waitForLocalTransferJob(t, manager, job.ID)
		if strings.Count(finished.Error, "exit status 23") != 1 {
			t.Fatalf("error = %q", finished.Error)
		}
	})
}

func TestLocalTransferJobManagerDeduplicatesActiveDirection(t *testing.T) {
	manager := NewLocalTransferJobManager()
	release := make(chan struct{})
	started := make(chan struct{})
	first, err := manager.Start("mac-one", "push", func(onOutput func(string)) error {
		close(started)
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	duplicate, err := manager.Start("mac-one", "push", func(onOutput func(string)) error {
		t.Fatal("duplicate job must not run")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	otherDirection, err := manager.Start("mac-one", "pull", func(onOutput func(string)) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	if duplicate.ID != first.ID {
		t.Fatalf("duplicate id = %q, want %q", duplicate.ID, first.ID)
	}
	if otherDirection.ID == first.ID {
		t.Fatal("different direction was deduplicated")
	}
	close(release)
	waitForLocalTransferJob(t, manager, first.ID)
	waitForLocalTransferJob(t, manager, otherDirection.ID)
}

func TestLocalTransferJobManagerRejectsConflictingActiveCorrelation(t *testing.T) {
	manager := NewLocalTransferJobManager()
	release := make(chan struct{})
	started := make(chan struct{})
	var (
		mu          sync.Mutex
		firstEvents []LocalTransferEvent
		otherEvents []LocalTransferEvent
	)
	first, err := manager.StartWithEvents("member-transfer-1", "mac-one", "push", func(event LocalTransferEvent) {
		mu.Lock()
		firstEvents = append(firstEvents, event)
		mu.Unlock()
	}, func(onOutput func(string)) error {
		close(started)
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	retry, err := manager.StartWithEvents("member-transfer-1", "mac-one", "push", func(event LocalTransferEvent) {
		t.Fatal("same-transfer retry callback must not replace the original callback")
	}, func(onOutput func(string)) error {
		t.Fatal("same-transfer retry must not run")
		return nil
	})
	if err != nil || retry.ID != first.ID {
		t.Fatalf("same-transfer retry = (%+v, %v), want job %q", retry, err, first.ID)
	}

	_, err = manager.StartWithEvents("member-transfer-2", "mac-one", "push", func(event LocalTransferEvent) {
		mu.Lock()
		otherEvents = append(otherEvents, event)
		mu.Unlock()
	}, func(onOutput func(string)) error {
		t.Fatal("conflicting transfer must not run")
		return nil
	})
	if !errors.Is(err, ErrLocalTransferConflict) {
		t.Fatalf("conflicting transfer error = %v", err)
	}

	close(release)
	waitForLocalTransferJob(t, manager, first.ID)
	mu.Lock()
	defer mu.Unlock()
	if len(firstEvents) == 0 {
		t.Fatal("original callback received no events")
	}
	if len(otherEvents) != 0 {
		t.Fatalf("conflicting callback events = %+v", otherEvents)
	}
}

func TestLocalTransferJobManagerTryDrainIsAtomicWithStart(t *testing.T) {
	for i := 0; i < 100; i++ {
		manager := NewLocalTransferJobManager()
		release := make(chan struct{})
		startGate := make(chan struct{})
		type startResult struct {
			job LocalTransferJob
			err error
		}
		startResultCh := make(chan startResult, 1)
		drainResultCh := make(chan struct {
			active  []LocalTransferJob
			drained bool
		}, 1)
		go func() {
			<-startGate
			job, err := manager.Start("mac-one", "push", func(onOutput func(string)) error {
				<-release
				return nil
			})
			startResultCh <- startResult{job: job, err: err}
		}()
		go func() {
			<-startGate
			active, drained := manager.TryDrain()
			drainResultCh <- struct {
				active  []LocalTransferJob
				drained bool
			}{active: active, drained: drained}
		}()
		close(startGate)
		started := <-startResultCh
		drain := <-drainResultCh
		switch {
		case drain.drained:
			if !errors.Is(started.err, ErrLocalTransferDraining) {
				t.Fatalf("iteration %d: drained with start error %v", i, started.err)
			}
		case started.err == nil:
			if len(drain.active) != 1 || drain.active[0].ID != started.job.ID {
				t.Fatalf("iteration %d: active = %#v, job = %#v", i, drain.active, started.job)
			}
		default:
			t.Fatalf("iteration %d: start error = %v, drain = %#v", i, started.err, drain)
		}
		close(release)
		manager.Resume()
	}
}

func TestLocalTransferJobManagerRejectsStartsWhileDraining(t *testing.T) {
	manager := NewLocalTransferJobManager()
	active, drained := manager.TryDrain()
	if !drained || len(active) != 0 {
		t.Fatalf("TryDrain() = (%#v, %v)", active, drained)
	}
	if _, err := manager.Start("mac-one", "pull", func(onOutput func(string)) error { return nil }); !errors.Is(err, ErrLocalTransferDraining) {
		t.Fatalf("Start() error = %v", err)
	}
	manager.Resume()
	job, err := manager.Start("mac-one", "pull", func(onOutput func(string)) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	waitForLocalTransferJob(t, manager, job.ID)
}

func TestLocalTransferJobManagerCapsOutputAndCleansOldJobs(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	manager := NewLocalTransferJobManager()
	manager.now = func() time.Time { return now }
	job, err := manager.Start("mac-one", "push", func(onOutput func(string)) error {
		onOutput(strings.Repeat("a", localTransferOutputLimit+1024))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := waitForLocalTransferJob(t, manager, job.ID)
	if len(finished.Output) != localTransferOutputLimit {
		t.Fatalf("output length = %d", len(finished.Output))
	}

	now = now.Add(localTransferRetention + time.Second)
	if _, ok := manager.Get(job.ID); ok {
		t.Fatal("expired job was not cleaned")
	}
}

func TestParseRsyncProgress(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   int
		ok     bool
	}{
		{name: "percent", output: "  5,242,880  67%  12.34MB/s  0:00:01", want: 67, ok: true},
		{name: "to-chk", output: "file (xfr#3, to-chk=2/10)", want: 80, ok: true},
		{name: "to-check", output: "file (xfr#3, to-check=1/4)", want: 75, ok: true},
		{name: "invalid to-check does not use file percent", output: "  1,024 100% (xfr#1, to-chk=0/0)", ok: false},
		{name: "raw completion", output: "  5,242,880 100%  12.34MB/s  0:00:01", want: 100, ok: true},
		{
			name: "multi-file uses final overall to-check",
			output: "file-one\n  1,024 100%  1.00MB/s  0:00:01 (xfr#1, to-chk=3/4)\n" +
				"file-two\n  256  25%  1.00MB/s  0:00:01 (xfr#2, to-chk=2/4)\n",
			want: 50,
			ok:   true,
		},
		{name: "none", output: "sending incremental file list", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRsyncProgress(tt.output)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("parseRsyncProgress() = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestMapRsyncProgress(t *testing.T) {
	tests := []struct {
		name        string
		raw         int
		processDone bool
		wantPhase   string
		wantPercent int
	}{
		{name: "preparing", raw: 0, wantPhase: TransferPhasePreparing, wantPercent: 0},
		{name: "started", raw: 1, wantPhase: TransferPhaseTransferring, wantPercent: 1},
		{name: "middle", raw: 50, wantPhase: TransferPhaseTransferring, wantPercent: 48},
		{name: "data complete", raw: 99, wantPhase: TransferPhaseTransferring, wantPercent: 95},
		{name: "process active finalizing", raw: 100, wantPhase: TransferPhaseFinalizing, wantPercent: 99},
		{name: "process complete", raw: 100, processDone: true, wantPhase: TransferPhaseSucceeded, wantPercent: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase, percent := mapRsyncProgress(tt.raw, tt.processDone)
			if phase != tt.wantPhase || percent != tt.wantPercent {
				t.Fatalf("mapRsyncProgress(%d, %v) = (%q, %d), want (%q, %d)",
					tt.raw, tt.processDone, phase, percent, tt.wantPhase, tt.wantPercent)
			}
		})
	}
}

func TestMapRsyncProgressTotalMode(t *testing.T) {
	tests := []struct {
		name        string
		raw         int
		processDone bool
		wantPhase   string
		wantPercent int
	}{
		{name: "preparing", raw: 0, wantPhase: TransferPhasePreparing, wantPercent: 0},
		{name: "exact middle", raw: 73, wantPhase: TransferPhaseTransferring, wantPercent: 73},
		{name: "exact late", raw: 98, wantPhase: TransferPhaseTransferring, wantPercent: 98},
		{name: "active finalizing", raw: 100, wantPhase: TransferPhaseFinalizing, wantPercent: 99},
		{name: "process complete", raw: 100, processDone: true, wantPhase: TransferPhaseSucceeded, wantPercent: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase, percent := mapRsyncProgress(tt.raw, tt.processDone, LocalTransferProgressTotal)
			if phase != tt.wantPhase || percent != tt.wantPercent {
				t.Fatalf("mapRsyncProgress(%d, %v, total) = (%q, %d), want (%q, %d)",
					tt.raw, tt.processDone, phase, percent, tt.wantPhase, tt.wantPercent)
			}
		})
	}
}

func TestLocalTransferJobManagerUsesTotalProgressMode(t *testing.T) {
	manager := NewLocalTransferJobManager()
	release := make(chan struct{})
	outputWritten := make(chan struct{})
	job, err := manager.StartWithOptions("member-transfer-1", "mac-one", "push", LocalTransferProgressTotal, nil, func(onOutput func(string)) error {
		onOutput("     7,654,321  73%   44.35MB/s    0:00:01 (xfr#12, ir-chk=8/20)\n")
		close(outputWritten)
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-outputWritten
	active, ok := manager.Get(job.ID)
	if !ok {
		t.Fatalf("job %q not found", job.ID)
	}
	if active.ProgressMode != LocalTransferProgressTotal || active.Phase != TransferPhaseTransferring || active.Percent != 73 {
		t.Fatalf("active job = %+v", active)
	}
	close(release)
	finished := waitForLocalTransferJob(t, manager, job.ID)
	if finished.Phase != TransferPhaseSucceeded || finished.Percent != 100 {
		t.Fatalf("finished job = %+v", finished)
	}
}

func TestLocalTransferJobManagerParsesProgressAcrossOutputChunks(t *testing.T) {
	manager := NewLocalTransferJobManager()
	job, err := manager.Start("mac-one", "push", func(onOutput func(string)) error {
		onOutput("  1,024  5")
		onOutput("8%  1.00MB/s")
		return errors.New("exit status 23")
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := waitForLocalTransferJob(t, manager, job.ID)
	if finished.Percent != 56 {
		t.Fatalf("percent = %d, output = %q", finished.Percent, finished.Output)
	}
}

func TestLocalTransferJobManagerFinalizingAndFailurePhases(t *testing.T) {
	t.Run("active raw completion waits for process exit", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		release := make(chan struct{})
		outputWritten := make(chan struct{})
		job, err := manager.Start("mac-one", "push", func(onOutput func(string)) error {
			onOutput("  1,024 100%  1.00MB/s  0:00:01\n")
			close(outputWritten)
			<-release
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		<-outputWritten
		active, ok := manager.Get(job.ID)
		if !ok {
			t.Fatalf("job %q not found", job.ID)
		}
		if active.Phase != TransferPhaseFinalizing || active.Percent != 99 {
			t.Fatalf("active job = %+v", active)
		}
		close(release)
		finished := waitForLocalTransferJob(t, manager, job.ID)
		if finished.Phase != TransferPhaseSucceeded || finished.Percent != 100 {
			t.Fatalf("finished job = %+v", finished)
		}
	})

	t.Run("failure preserves mapped progress", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		job, err := manager.Start("mac-one", "pull", func(onOutput func(string)) error {
			onOutput("  1,024 50%  1.00MB/s  0:00:01\n")
			return errors.New("exit status 23")
		})
		if err != nil {
			t.Fatal(err)
		}
		finished := waitForLocalTransferJob(t, manager, job.ID)
		if finished.Phase != TransferPhaseFailed || finished.Percent != 48 {
			t.Fatalf("failed job = %+v", finished)
		}
	})
}

func TestLocalTransferJobManagerPublishesTerminalStateAfterCallback(t *testing.T) {
	manager := NewLocalTransferJobManager()
	callbackEntered := make(chan LocalTransferEvent, 1)
	releaseCallback := make(chan struct{})
	job, err := manager.StartWithEvents("member-transfer-1", "mac-one", "pull", func(event LocalTransferEvent) {
		if event.Status == LocalTransferFailed {
			callbackEntered <- event
			<-releaseCallback
		}
	}, func(onOutput func(string)) error {
		onOutput("  1,024 50%  1.00MB/s  0:00:01\n")
		return errors.New("exit status 23")
	})
	if err != nil {
		t.Fatal(err)
	}

	var terminalEvent LocalTransferEvent
	select {
	case terminalEvent = <-callbackEntered:
	case <-time.After(time.Second):
		t.Fatal("terminal callback was not entered")
	}
	if terminalEvent.Status != LocalTransferFailed || terminalEvent.Phase != TransferPhaseFailed || terminalEvent.Percent != 48 {
		t.Fatalf("terminal callback event = %+v", terminalEvent)
	}

	visible, ok := manager.Get(job.ID)
	if !ok {
		t.Fatalf("job %q not found", job.ID)
	}
	if !visible.Active() || visible.Status != LocalTransferRunning || visible.FinishedAt != nil {
		t.Fatalf("terminal state became visible before callback completed: %+v", visible)
	}
	listed := manager.List("mac-one")
	if len(listed) != 1 || !listed[0].Active() || listed[0].Status != LocalTransferRunning {
		t.Fatalf("listed jobs before callback completion = %+v", listed)
	}

	close(releaseCallback)
	finished := waitForLocalTransferJob(t, manager, job.ID)
	if finished.Status != LocalTransferFailed || finished.Phase != TransferPhaseFailed || finished.Percent != 48 {
		t.Fatalf("published terminal job = %+v", finished)
	}
}

func TestLocalTransferJobManagerTerminalCallbackPanicDoesNotBlockPublication(t *testing.T) {
	manager := NewLocalTransferJobManager()
	job, err := manager.StartWithEvents("member-transfer-1", "mac-one", "pull", func(event LocalTransferEvent) {
		if event.Status == LocalTransferFailed {
			panic("terminal callback panic")
		}
	}, func(onOutput func(string)) error {
		onOutput("  1,024 50%  1.00MB/s  0:00:01\n")
		return errors.New("exit status 23")
	})
	if err != nil {
		t.Fatal(err)
	}

	finished := waitForLocalTransferJob(t, manager, job.ID)
	if finished.Status != LocalTransferFailed || finished.Phase != TransferPhaseFailed || finished.Percent != 48 {
		t.Fatalf("published terminal job = %+v", finished)
	}
}

func TestLocalTransferJobManagerTerminalCallbackIsBounded(t *testing.T) {
	manager := NewLocalTransferJobManager()
	manager.terminalCallbackTimeout = 20 * time.Millisecond
	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	job, err := manager.StartWithEvents("member-transfer-1", "mac-one", "pull", func(event LocalTransferEvent) {
		if event.Status != LocalTransferFailed {
			return
		}
		close(callbackEntered)
		<-releaseCallback
	}, func(onOutput func(string)) error {
		return errors.New("exit status 23")
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-callbackEntered:
	case <-time.After(time.Second):
		t.Fatal("terminal callback was not entered")
	}

	beforeTimeout, ok := manager.Get(job.ID)
	if !ok || !beforeTimeout.Active() {
		t.Fatalf("job before callback timeout = %+v, %v", beforeTimeout, ok)
	}
	finished := waitForLocalTransferJob(t, manager, job.ID)
	close(releaseCallback)
	if finished.Status != LocalTransferFailed || finished.FinishedAt == nil {
		t.Fatalf("job after callback timeout = %+v", finished)
	}
}

func TestLocalTransferJobManagerStartedCallbackPanicIsContained(t *testing.T) {
	manager := NewLocalTransferJobManager()
	job, err := manager.StartWithEvents("member-transfer-1", "mac-one", "push", func(event LocalTransferEvent) {
		if event.Status == LocalTransferRunning && event.Percent == 0 {
			panic("started callback panic with secret output")
		}
	}, func(onOutput func(string)) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := waitForLocalTransferJob(t, manager, job.ID)
	if finished.Status != LocalTransferSucceeded {
		t.Fatalf("job = %+v", finished)
	}
	if finished.CallbackWarning != localTransferCallbackPanicWarning {
		t.Fatalf("callback warning = %q", finished.CallbackWarning)
	}
	if strings.Contains(finished.CallbackWarning, "secret") {
		t.Fatalf("callback warning leaked callback content: %q", finished.CallbackWarning)
	}
}

func TestLocalTransferJobManagerStartedCallbackBlockIsBoundedPerJob(t *testing.T) {
	manager := NewLocalTransferJobManager()
	manager.terminalCallbackTimeout = 20 * time.Millisecond
	startedEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	var once sync.Once
	job, err := manager.StartWithEvents("member-transfer-1", "mac-one", "push", func(event LocalTransferEvent) {
		if event.Status == LocalTransferRunning && event.Percent == 0 {
			once.Do(func() { close(startedEntered) })
			<-releaseCallback
		}
	}, func(onOutput func(string)) error {
		onOutput("  1,024 100%  1.00MB/s  0:00:01\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-startedEntered:
	case <-time.After(time.Second):
		t.Fatal("started callback was not entered")
	}
	finished := waitForLocalTransferJob(t, manager, job.ID)
	close(releaseCallback)
	if finished.Status != LocalTransferSucceeded || finished.CallbackWarning != localTransferCallbackTimeoutWarning {
		t.Fatalf("job = %+v", finished)
	}
}

func TestLocalTransferJobManagerProgressCallbackPanicAndBlockAreContained(t *testing.T) {
	t.Run("panic", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		job, err := manager.StartWithEvents("member-transfer-1", "mac-one", "push", func(event LocalTransferEvent) {
			if event.Percent == 10 {
				panic("progress callback panic")
			}
		}, func(onOutput func(string)) error {
			onOutput("file (xfr#1, to-chk=8/10)\n")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		finished := waitForLocalTransferJob(t, manager, job.ID)
		if finished.Status != LocalTransferSucceeded || finished.CallbackWarning != localTransferCallbackPanicWarning {
			t.Fatalf("job = %+v", finished)
		}
	})

	t.Run("block", func(t *testing.T) {
		manager := NewLocalTransferJobManager()
		manager.terminalCallbackTimeout = 20 * time.Millisecond
		progressEntered := make(chan struct{})
		releaseCallback := make(chan struct{})
		job, err := manager.StartWithEvents("member-transfer-1", "mac-one", "push", func(event LocalTransferEvent) {
			if event.Percent == 10 {
				close(progressEntered)
				<-releaseCallback
			}
		}, func(onOutput func(string)) error {
			onOutput("file (xfr#1, to-chk=8/10)\n")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-progressEntered:
		case <-time.After(time.Second):
			t.Fatal("progress callback was not entered")
		}
		finished := waitForLocalTransferJob(t, manager, job.ID)
		close(releaseCallback)
		if finished.Status != LocalTransferSucceeded || finished.CallbackWarning != localTransferCallbackTimeoutWarning {
			t.Fatalf("job = %+v", finished)
		}
	})
}

func TestLocalTransferJobManagerCorrelationAndMilestoneEvents(t *testing.T) {
	manager := NewLocalTransferJobManager()
	var (
		mu     sync.Mutex
		events []LocalTransferEvent
	)
	job, err := manager.StartWithEvents("member-transfer-1", "mac-one", "push", func(event LocalTransferEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}, func(onOutput func(string)) error {
		onOutput("file-one (xfr#1, to-chk=8/10)\n")
		onOutput("file-two (xfr#2, to-chk=0/10)\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := waitForLocalTransferJob(t, manager, job.ID)
	if finished.TransferID != "member-transfer-1" {
		t.Fatalf("transfer id = %q", finished.TransferID)
	}

	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		count := len(events)
		mu.Unlock()
		if count == 8 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, event := range events {
		if event.TransferID != "member-transfer-1" || event.LocalJobID != job.ID ||
			event.Profile != "mac-one" || event.Direction != "push" || event.Elapsed < 0 {
			t.Fatalf("event = %+v", event)
		}
		got = append(got, fmt.Sprintf("%s:%s:%d", event.Status, event.Phase, event.Percent))
	}
	want := []string{
		"running:preparing:0",
		"running:transferring:10",
		"running:transferring:25",
		"running:transferring:50",
		"running:transferring:75",
		"running:transferring:90",
		"running:finalizing:99",
		"succeeded:succeeded:100",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func TestLocalTransferJobManagerDoesNotJumpFromZeroToFinalizing(t *testing.T) {
	manager := NewLocalTransferJobManager()
	var (
		mu     sync.Mutex
		events []LocalTransferEvent
	)
	job, err := manager.StartWithEvents("member-transfer-1", "mac-one", "push", func(event LocalTransferEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}, func(onOutput func(string)) error {
		onOutput("file-one 100%\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForLocalTransferJob(t, manager, job.ID)
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 8 {
		t.Fatalf("events = %+v", events)
	}
	var got []int
	for _, event := range events {
		got = append(got, event.Percent)
	}
	want := []int{0, 10, 25, 50, 75, 90, 99, 100}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event percentages = %v, want %v", got, want)
	}
}

func TestOutputCallbackWriter(t *testing.T) {
	var output string
	writer := outputCallbackWriter{onOutput: func(chunk string) { output += chunk }}
	written, err := writer.Write([]byte("rsync output"))
	if err != nil || written != len("rsync output") || output != "rsync output" {
		t.Fatalf("Write() = (%d, %v), output = %q", written, err, output)
	}
}

func waitForLocalTransferJob(t *testing.T, manager *LocalTransferJobManager, id string) LocalTransferJob {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := manager.Get(id)
		if !ok {
			t.Fatalf("job %q not found", id)
		}
		if !job.Active() {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for transfer job")
	return LocalTransferJob{}
}
