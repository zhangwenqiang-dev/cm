package connectmac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJobLifecycleMetadataIsBackwardCompatible(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	legacy := Job{
		ID:        "legacy-lifecycle",
		Type:      "aws-open",
		Profile:   "mac",
		Status:    JobStatusRunning,
		StartedAt: time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC),
	}
	path, err := manager.JobPath(legacy.ID)
	if err != nil {
		t.Fatalf("job path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create job dir: %v", err)
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy job: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy job: %v", err)
	}

	loaded, err := manager.loadRaw(legacy.ID)
	if err != nil {
		t.Fatalf("load legacy job: %v", err)
	}
	if loaded.RequestID != "" || loaded.Source != "" || loaded.ActorMemberID != "" || loaded.ActorEmail != "" || loaded.ActorName != "" {
		t.Fatalf("legacy correlation metadata = %#v", loaded)
	}
	if loaded.LifecycleState != "" || loaded.LifecycleOwnerEmail != "" || !loaded.LifecycleFinalizedAt.IsZero() ||
		!loaded.LifecycleNotifyClaimedAt.IsZero() || !loaded.LifecycleNotifiedAt.IsZero() ||
		loaded.LifecycleNotifyAttempts != 0 || !loaded.LifecycleNotifyNextAttemptAt.IsZero() ||
		loaded.LifecycleNotifyConfigFingerprint != "" || !loaded.LifecycleNotifyExhaustedAt.IsZero() ||
		!loaded.LifecycleNotifyFailureRecordedAt.IsZero() || loaded.LifecycleError != "" {
		t.Fatalf("legacy lifecycle metadata = %#v", loaded)
	}

	updated, err := manager.Update(legacy.ID, func(current Job) (Job, error) {
		current.RequestID = "req-legacy"
		current.Source = "web"
		current.ActorMemberID = "member-1"
		current.ActorEmail = "owner@example.com"
		current.ActorName = "Owner"
		current.LifecycleOwnerEmail = "owner@example.com"
		current.LifecycleState = JobLifecyclePending
		return current, nil
	})
	if err != nil {
		t.Fatalf("update lifecycle metadata: %v", err)
	}
	if updated.LifecycleOwnerEmail != "owner@example.com" || updated.LifecycleState != JobLifecyclePending {
		t.Fatalf("updated job = %#v", updated)
	}
	persisted, err := manager.loadRaw(legacy.ID)
	if err != nil {
		t.Fatalf("load updated job: %v", err)
	}
	if persisted.LifecycleOwnerEmail != updated.LifecycleOwnerEmail || persisted.LifecycleState != updated.LifecycleState {
		t.Fatalf("persisted job = %#v, updated = %#v", persisted, updated)
	}
	if persisted.RequestID != updated.RequestID || persisted.Source != updated.Source ||
		persisted.ActorMemberID != updated.ActorMemberID || persisted.ActorEmail != updated.ActorEmail ||
		persisted.ActorName != updated.ActorName {
		t.Fatalf("persisted correlation metadata = %#v, updated = %#v", persisted, updated)
	}
}

func TestJobManagerUpdatePreservesExistingFields(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	job, err := manager.Create(Job{
		ID:        "atomic-lifecycle-update",
		Type:      "aws-open",
		Profile:   "mac",
		Status:    JobStatusRunning,
		Notify:    true,
		LastError: "existing error",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	updated, err := manager.Update(job.ID, func(current Job) (Job, error) {
		current.LifecycleState = JobLifecyclePending
		current.LifecycleOwnerEmail = "owner@example.com"
		return current, nil
	})
	if err != nil {
		t.Fatalf("update job: %v", err)
	}
	if updated.Type != job.Type || updated.Profile != job.Profile || updated.Status != job.Status ||
		updated.Notify != job.Notify || updated.LastError != job.LastError {
		t.Fatalf("update lost existing fields: updated=%#v original=%#v", updated, job)
	}
	if updated.LifecycleState != JobLifecyclePending || updated.LifecycleOwnerEmail != "owner@example.com" {
		t.Fatalf("updated job = %#v", updated)
	}

	callbackErr := errors.New("reject update")
	if _, err := manager.Update(job.ID, func(current Job) (Job, error) {
		current.LifecycleState = JobLifecycleFailed
		return current, callbackErr
	}); !errors.Is(err, callbackErr) {
		t.Fatalf("callback error = %v", err)
	}
	persisted, err := manager.loadRaw(job.ID)
	if err != nil {
		t.Fatalf("load persisted job: %v", err)
	}
	if persisted.LifecycleState != JobLifecyclePending {
		t.Fatalf("failed callback changed persisted job: %#v", persisted)
	}

	if _, err := manager.Update(job.ID, func(current Job) (Job, error) {
		current.ID = "different-job"
		return current, nil
	}); err == nil {
		t.Fatal("changing job ID must fail")
	}
}

func TestAWSCLIOperationWritesBestEffortStartAndResultLogs(t *testing.T) {
	for _, test := range []struct {
		command string
		code    int
	}{
		{command: "open", code: 0},
		{command: "destroy", code: 1},
	} {
		t.Run(test.command, func(t *testing.T) {
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, t.TempDir())
			profile := validAWSProfile()
			got := app.runObservedAWSCLI(context.Background(), test.command, profile.Name, true, false, func() int {
				return test.code
			})
			if got != test.code {
				t.Fatalf("exit code = %d, want %d", got, test.code)
			}
			entries := readTestLogEntries(t, app.LogManager)
			if len(entries) != 2 {
				t.Fatalf("entries = %+v", entries)
			}
			if entries[0].Source != "cli" || entries[0].Phase != "started" ||
				entries[0].RequestID == "" || entries[1].RequestID != entries[0].RequestID {
				t.Fatalf("uncorrelated CLI entries = %+v", entries)
			}
			wantPhase := "succeeded"
			if test.code != 0 {
				wantPhase = "failed"
			}
			if entries[1].Phase != wantPhase {
				t.Fatalf("result phase = %q, want %q", entries[1].Phase, wantPhase)
			}
		})
	}
}

func TestRunAWSOpenAndDestroyLogEarlyFailuresFromRealEntry(t *testing.T) {
	tests := []struct {
		name    string
		command string
		cfg     Config
		ref     string
	}{
		{
			name:    "open unknown profile",
			command: "open",
			cfg:     Config{Profiles: map[string]Profile{}},
			ref:     "missing-profile",
		},
		{
			name:    "destroy validation failure",
			command: "destroy",
			cfg: func() Config {
				profile := validAWSProfile()
				profile.AWS.SecurityGroupID = ""
				return Config{Profiles: map[string]Profile{profile.Name: profile}}
			}(),
			ref: "xcode-vnc",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, t.TempDir())
			code := app.runAWS(context.Background(), test.cfg, []string{test.command, test.ref}, "unused.yaml")
			if code == 0 {
				t.Fatalf("runAWS code = 0, stderr = %s", errOut.String())
			}
			entries := readTestLogEntries(t, app.LogManager)
			if len(entries) != 2 || entries[0].Phase != "started" || entries[1].Phase != "failed" {
				t.Fatalf("early failure logs = %+v", entries)
			}
			if entries[0].Profile != test.ref || entries[1].Profile != test.ref ||
				entries[0].RequestID == "" || entries[1].RequestID != entries[0].RequestID {
				t.Fatalf("uncorrelated early failure logs = %+v", entries)
			}
		})
	}
}

func TestRunAWSPlanDoesNotCreateOpenOrDestroyLogs(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	profile := validAWSProfile()
	cfg := Config{Profiles: map[string]Profile{profile.Name: profile}}
	if code := app.runAWS(context.Background(), cfg, []string{"plan", profile.Name}, "unused.yaml"); code != 0 {
		t.Fatalf("plan code = %d, stderr = %s", code, errOut.String())
	}
	files, err := app.LogManager.List()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("list logs: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("plan unexpectedly wrote runtime logs: %+v", files)
	}
}

func TestAWSCLIBackgroundDestroySuccessIsQueuedNotCompleted(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	code := app.runObservedAWSCLI(context.Background(), "destroy", "xcode-vnc", true, true, func() int {
		return 0
	})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	entries := readTestLogEntries(t, app.LogManager)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	result := entries[1]
	if result.Action != "aws.release.queued" || result.Phase != "queued" || result.Status != "queued" ||
		result.Action == "aws.release.succeeded" || result.Status == "success" {
		t.Fatalf("background destroy result = %+v", result)
	}
}

func TestJobManagerRunJobPropagatesOperationCorrelationEnvironment(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	job, err := manager.Create(Job{
		ID:            "correlated-child",
		Status:        JobStatusRunning,
		RunnerToken:   "correlated-runner-token",
		RequestID:     "req-child",
		Source:        "web",
		ActorMemberID: "member-1",
		ActorEmail:    "actor@example.com",
		ActorName:     "Actor Name",
		Command: []string{"/bin/sh", "-c", strings.Join([]string{
			`test "$` + jobRequestIDEnv + `" = "req-child"`,
			`test "$` + jobIDEnv + `" = "correlated-child"`,
			`test "$` + jobSourceEnv + `" = "web"`,
			`test "$` + jobActorMemberIDEnv + `" = "member-1"`,
			`test "$` + jobActorEmailEnv + `" = "actor@example.com"`,
			`test "$` + jobActorNameEnv + `" = "Actor Name"`,
		}, " && ")},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	t.Setenv(jobRunnerTokenEnv, job.RunnerToken)
	completed, err := manager.RunJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("run correlated child: %v", err)
	}
	if completed.Status != JobStatusSuccess {
		t.Fatalf("completed job = %+v", completed)
	}
}

func TestAWSCLIChildLogsReuseJobCorrelationAndWebDirectCallStaysSuppressed(t *testing.T) {
	t.Run("child correlation", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, t.TempDir())
		t.Setenv(jobRequestIDEnv, "req-child")
		t.Setenv(jobIDEnv, "job-child")
		t.Setenv(jobSourceEnv, "web")
		t.Setenv(jobActorMemberIDEnv, "member-1")
		t.Setenv(jobActorEmailEnv, "actor@example.com")
		t.Setenv(jobActorNameEnv, "Actor Name")
		if code := app.runObservedAWSCLI(context.Background(), "open", "xcode-vnc", true, false, func() int { return 0 }); code != 0 {
			t.Fatalf("exit code = %d", code)
		}
		entries := readTestLogEntries(t, app.LogManager)
		if len(entries) != 2 {
			t.Fatalf("entries = %+v", entries)
		}
		for _, entry := range entries {
			if entry.RequestID != "req-child" || entry.JobID != "job-child" || entry.Source != "web" ||
				entry.ActorMemberID != "member-1" || entry.ActorMemberEmail != "actor@example.com" ||
				entry.ActorMemberName != "Actor Name" {
				t.Fatalf("uncorrelated child log = %+v", entry)
			}
		}
	})

	t.Run("direct web call suppressed", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := testApp(&out, &errOut, t.TempDir())
		ctx := withOperationContext(context.Background(), OperationContext{
			RequestID: "req-web-direct",
			Source:    "web",
		})
		if code := app.runObservedAWSCLI(ctx, "open", "xcode-vnc", true, false, func() int { return 0 }); code != 0 {
			t.Fatalf("exit code = %d", code)
		}
		files, err := app.LogManager.List()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("list logs: %v", err)
		}
		if len(files) != 0 {
			t.Fatalf("direct Web command wrote duplicate CLI logs: %+v", files)
		}
	})
}

func TestManualAndAutomaticDestroyPathsShareAtomicUniqueness(t *testing.T) {
	var out, errOut bytes.Buffer
	app := NewApp(&out, &errOut)
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	manager.Executable = "/usr/bin/true"
	manager.IsRunning = func(int) bool { return true }
	app.JobManager = manager
	profile := validAWSProfile()
	plan, err := app.AWSService.Plan(profile)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if code := app.startAWSDestroyJob(context.Background(), profile, plan, filepath.Join(t.TempDir(), "config.yaml"), false); code != 0 {
		t.Fatalf("manual start code=%d err=%s", code, errOut.String())
	}
	manualJobs, err := manager.listRaw()
	if err != nil {
		t.Fatalf("list manual jobs: %v", err)
	}
	if len(manualJobs) != 1 || manualJobs[0].LifecycleState != "" || manualJobs[0].LifecycleOwnerEmail != "" {
		t.Fatalf("manual CLI job lifecycle metadata = %+v", manualJobs)
	}
	autoConfig := filepath.Join(t.TempDir(), "auto-config.yaml")
	if err := os.WriteFile(autoConfig, []byte("profiles: {}\n"), 0o600); err != nil {
		t.Fatalf("write automatic config: %v", err)
	}
	if _, _, err := app.startAWSJobForResolvedProfile(context.Background(), autoConfig, "destroy", profile, true, autoConfig); !IsDuplicateActiveJob(err, "aws-destroy", profile.Name) {
		t.Fatalf("automatic start error = %v", err)
	}
	if _, err := os.Stat(autoConfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate automatic config was not cleaned: %v", err)
	}
}

func TestJobManagerCreatePreventsConcurrentAWSDestroyAcrossManagers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jobs")
	managers := []JobManager{NewJobManager(dir), NewJobManager(dir)}
	managers[0].Now = func() time.Time { return time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC) }
	managers[1].Now = func() time.Time { return time.Date(2026, 7, 13, 8, 0, 1, 0, time.UTC) }
	start := make(chan struct{})
	results := make(chan error, len(managers))
	var wg sync.WaitGroup
	for i := range managers {
		wg.Add(1)
		go func(manager JobManager) {
			defer wg.Done()
			<-start
			_, err := manager.Create(Job{Type: "aws-destroy", Profile: "mac"})
			results <- err
		}(managers[i])
	}
	close(start)
	wg.Wait()
	close(results)

	successes, duplicates := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case IsDuplicateActiveJob(err, "aws-destroy", "mac"):
			duplicates++
		default:
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("successes=%d duplicates=%d", successes, duplicates)
	}
	jobs, err := managers[0].listRaw()
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Type != "aws-destroy" || jobs[0].Profile != "mac" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestJobManagerProfileOperationGuardSerializesAcrossManagers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jobs")
	first := NewJobManager(dir)
	second := NewJobManager(dir)
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.WithProfileOperation("../mac/unsafe", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- second.WithProfileOperation("../mac/unsafe", func() error { return nil })
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second profile operation bypassed guard: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first profile operation: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second profile operation: %v", err)
	}
	lockPath, err := first.profileOperationLockPath("../mac/unsafe")
	if err != nil {
		t.Fatalf("profile lock path: %v", err)
	}
	if filepath.Dir(lockPath) != filepath.Join(dir, ".profile-locks") || strings.Contains(filepath.Base(lockPath), "mac") || filepath.Ext(lockPath) != ".lock" {
		t.Fatalf("unsafe profile lock path = %q", lockPath)
	}
}

func TestJobManagerAWSDestroyCreateUsesProfileOperationGuard(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jobs")
	guard := NewJobManager(dir)
	creator := NewJobManager(dir)
	createDone := make(chan error, 1)
	err := guard.WithProfileOperation("mac", func() error {
		go func() {
			_, err := creator.Create(Job{Type: "aws-destroy", Profile: "mac"})
			createDone <- err
		}()
		select {
		case err := <-createDone:
			t.Fatalf("destroy create bypassed profile guard: %v", err)
		case <-time.After(100 * time.Millisecond):
			return nil
		}
		return nil
	})
	if err != nil {
		t.Fatalf("hold profile guard: %v", err)
	}
	if err := <-createDone; err != nil {
		t.Fatalf("create destroy after guard: %v", err)
	}
}

func TestJobManagerAWSDestroyUniquenessAllowsOtherProfilesAndTerminalReplacement(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	first, err := manager.Create(Job{ID: "destroy-mac-1", Type: "aws-destroy", Profile: "mac"})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := manager.Create(Job{ID: "destroy-other", Type: "aws-destroy", Profile: "other"}); err != nil {
		t.Fatalf("create other profile: %v", err)
	}
	first.Status = JobStatusSuccess
	first.LifecycleState = JobLifecycleWaiting
	first.FinishedAt = time.Now()
	if err := manager.Save(first); err != nil {
		t.Fatalf("move first destroy to lifecycle waiting: %v", err)
	}
	artifact := filepath.Join(t.TempDir(), "duplicate-destroy-config.yaml")
	if err := os.WriteFile(artifact, []byte("secret config"), 0o600); err != nil {
		t.Fatalf("write duplicate destroy artifact: %v", err)
	}
	if _, err := manager.Create(Job{
		ID:           "destroy-mac-waiting-duplicate",
		Type:         "aws-destroy",
		Profile:      "mac",
		CleanupPaths: []string{artifact},
	}); !IsDuplicateActiveJob(err, "aws-destroy", "mac") {
		t.Fatalf("lifecycle waiting destroy duplicate error = %v", err)
	}
	if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lifecycle waiting destroy artifact was not cleaned: %v", err)
	}
	first.LifecycleState = JobLifecycleFinalized
	if err := manager.Save(first); err != nil {
		t.Fatalf("finalize first destroy: %v", err)
	}
	if _, err := manager.Create(Job{ID: "destroy-mac-2", Type: "aws-destroy", Profile: "mac"}); err != nil {
		t.Fatalf("create replacement: %v", err)
	}
}

func TestJobManagerAWSOpenUniquenessByProfileAndActiveState(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	first, err := manager.Create(Job{ID: "open-mac-1", Type: "aws-open", Profile: "mac"})
	if err != nil {
		t.Fatalf("create first open: %v", err)
	}

	artifact := filepath.Join(t.TempDir(), "duplicate-open-config.yaml")
	if err := os.WriteFile(artifact, []byte("secret config"), 0o600); err != nil {
		t.Fatalf("write duplicate artifact: %v", err)
	}
	if _, err := manager.Create(Job{
		ID:           "open-mac-duplicate",
		Type:         "aws-open",
		Profile:      "mac",
		CleanupPaths: []string{artifact},
	}); !IsDuplicateActiveJob(err, "aws-open", "mac") {
		t.Fatalf("duplicate open error = %v", err)
	}
	if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate open artifact was not cleaned: %v", err)
	}

	if _, err := manager.Create(Job{ID: "open-other", Type: "aws-open", Profile: "other"}); err != nil {
		t.Fatalf("create open for other profile: %v", err)
	}

	first.Status = JobStatusSuccess
	first.LifecycleState = JobLifecycleWaiting
	first.FinishedAt = time.Now()
	if err := manager.Save(first); err != nil {
		t.Fatalf("move first open to lifecycle waiting: %v", err)
	}
	waitingArtifact := filepath.Join(t.TempDir(), "waiting-open-config.yaml")
	if err := os.WriteFile(waitingArtifact, []byte("secret config"), 0o600); err != nil {
		t.Fatalf("write waiting open artifact: %v", err)
	}
	if _, err := manager.Create(Job{
		ID:           "open-mac-waiting-duplicate",
		Type:         "aws-open",
		Profile:      "mac",
		CleanupPaths: []string{waitingArtifact},
	}); !IsDuplicateActiveJob(err, "aws-open", "mac") {
		t.Fatalf("lifecycle waiting open duplicate error = %v", err)
	}
	if _, err := os.Stat(waitingArtifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lifecycle waiting open artifact was not cleaned: %v", err)
	}
	first.LifecycleState = JobLifecycleFinalized
	if err := manager.Save(first); err != nil {
		t.Fatalf("finalize first open: %v", err)
	}
	if _, err := manager.Create(Job{ID: "open-mac-2", Type: "aws-open", Profile: "mac"}); err != nil {
		t.Fatalf("create open after terminal job: %v", err)
	}
}

func TestJobBlocksUniqueCreationForProcessOrLifecycleActivity(t *testing.T) {
	tests := []struct {
		name string
		job  Job
		want bool
	}{
		{name: "process starting", job: Job{Status: JobStatusStarting}, want: true},
		{name: "process running", job: Job{Status: JobStatusRunning}, want: true},
		{name: "process deferred without lifecycle", job: Job{Status: JobStatusDeferred}, want: true},
		{name: "lifecycle pending", job: Job{Status: JobStatusSuccess, LifecycleState: JobLifecyclePending}, want: true},
		{name: "success lifecycle waiting", job: Job{Status: JobStatusSuccess, LifecycleState: JobLifecycleWaiting}, want: true},
		{name: "failed overrides stale pending", job: Job{Status: JobStatusFailed, LifecycleState: JobLifecyclePending}, want: false},
		{name: "interrupted overrides stale waiting", job: Job{Status: JobStatusInterrupted, LifecycleState: JobLifecycleWaiting}, want: false},
		{name: "deferred lifecycle finalized", job: Job{Status: JobStatusDeferred, LifecycleState: JobLifecycleFinalized}, want: false},
		{name: "success lifecycle finalized", job: Job{Status: JobStatusSuccess, LifecycleState: JobLifecycleFinalized}, want: false},
		{name: "lifecycle failed", job: Job{Status: JobStatusFailed, LifecycleState: JobLifecycleFailed}, want: false},
		{name: "success without lifecycle", job: Job{Status: JobStatusSuccess}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := jobBlocksUniqueCreation(test.job); got != test.want {
				t.Fatalf("jobBlocksUniqueCreation(%+v) = %t, want %t", test.job, got, test.want)
			}
		})
	}
}

func TestJobManagerDeferredWithoutLifecycleBlocksDuplicate(t *testing.T) {
	for _, jobType := range []string{"aws-open", "aws-destroy"} {
		t.Run(jobType, func(t *testing.T) {
			manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
			if _, err := manager.Create(Job{
				ID:      jobType + "-deferred",
				Type:    jobType,
				Profile: "mac",
				Status:  JobStatusDeferred,
			}); err != nil {
				t.Fatalf("create deferred job: %v", err)
			}
			if _, err := manager.Create(Job{
				ID:      jobType + "-other",
				Type:    jobType,
				Profile: "other",
				Status:  JobStatusDeferred,
			}); err != nil {
				t.Fatalf("create deferred job for other profile: %v", err)
			}
			artifact := filepath.Join(t.TempDir(), "duplicate-deferred-config.yaml")
			if err := os.WriteFile(artifact, []byte("secret config"), 0o600); err != nil {
				t.Fatalf("write deferred duplicate artifact: %v", err)
			}
			if _, err := manager.Create(Job{
				ID:           jobType + "-deferred-duplicate",
				Type:         jobType,
				Profile:      "mac",
				CleanupPaths: []string{artifact},
			}); !IsDuplicateActiveJob(err, jobType, "mac") {
				t.Fatalf("deferred duplicate error = %v", err)
			}
			if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("deferred duplicate artifact was not cleaned: %v", err)
			}
		})
	}
}

func TestJobManagerRejectsOppositeAWSLifecycleOperationForProfile(t *testing.T) {
	tests := []struct {
		existingType string
		incomingType string
	}{
		{existingType: "aws-open", incomingType: "aws-destroy"},
		{existingType: "aws-destroy", incomingType: "aws-open"},
	}
	for _, test := range tests {
		t.Run(test.existingType+"_blocks_"+test.incomingType, func(t *testing.T) {
			manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
			existing, err := manager.Create(Job{
				ID:      test.existingType + "-active",
				Type:    test.existingType,
				Profile: "mac",
			})
			if err != nil {
				t.Fatalf("create existing lifecycle job: %v", err)
			}
			if _, err := manager.Create(Job{
				ID:      test.incomingType + "-other",
				Type:    test.incomingType,
				Profile: "other",
			}); err != nil {
				t.Fatalf("create opposite operation for other profile: %v", err)
			}
			artifact := filepath.Join(t.TempDir(), "opposite-operation-config.yaml")
			if err := os.WriteFile(artifact, []byte("secret config"), 0o600); err != nil {
				t.Fatalf("write opposite operation artifact: %v", err)
			}
			_, err = manager.Create(Job{
				ID:           test.incomingType + "-blocked",
				Type:         test.incomingType,
				Profile:      "mac",
				CleanupPaths: []string{artifact},
			})
			if !IsDuplicateActiveJob(err, test.incomingType, "mac") {
				t.Fatalf("opposite operation error = %v", err)
			}
			if !strings.Contains(err.Error(), "active "+test.existingType+" job") ||
				!strings.Contains(err.Error(), "cannot create "+test.incomingType) {
				t.Fatalf("cross-operation error is inaccurate: %v", err)
			}
			var duplicate *DuplicateActiveJobError
			if !errors.As(err, &duplicate) || duplicate.Existing.ID != existing.ID {
				t.Fatalf("duplicate existing job = %+v", duplicate)
			}
			if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("opposite operation artifact was not cleaned: %v", err)
			}
		})
	}
}

func TestJobManagerPreventsConcurrentOppositeAWSLifecycleOperations(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jobs")
	attempts := []struct {
		jobType string
		manager JobManager
	}{
		{jobType: "aws-open", manager: NewJobManager(dir)},
		{jobType: "aws-destroy", manager: NewJobManager(dir)},
	}
	start := make(chan struct{})
	results := make(chan struct {
		jobType string
		err     error
	}, len(attempts))
	var wg sync.WaitGroup
	for _, attempt := range attempts {
		wg.Add(1)
		go func(attempt struct {
			jobType string
			manager JobManager
		}) {
			defer wg.Done()
			<-start
			_, err := attempt.manager.Create(Job{Type: attempt.jobType, Profile: "mac"})
			results <- struct {
				jobType string
				err     error
			}{jobType: attempt.jobType, err: err}
		}(attempt)
	}
	close(start)
	wg.Wait()
	close(results)

	successes, duplicates := 0, 0
	for result := range results {
		switch {
		case result.err == nil:
			successes++
		case IsDuplicateActiveJob(result.err, result.jobType, "mac"):
			duplicates++
		default:
			t.Fatalf("%s create error = %v", result.jobType, result.err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("successes=%d duplicates=%d, want 1/1", successes, duplicates)
	}
	jobs, err := attempts[0].manager.listRaw()
	if err != nil {
		t.Fatalf("list concurrent lifecycle jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Profile != "mac" {
		t.Fatalf("concurrent lifecycle jobs = %+v", jobs)
	}
}

func TestJobManagerCreateUniquePreservesNonAWSJobTypeScope(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	if _, err := manager.CreateUnique(Job{ID: "sync-active", Type: "sync", Profile: "mac"}); err != nil {
		t.Fatalf("create first non-AWS unique job: %v", err)
	}
	if _, err := manager.CreateUnique(Job{ID: "other-active", Type: "other", Profile: "mac"}); err != nil {
		t.Fatalf("different non-AWS type should not conflict: %v", err)
	}
	if _, err := manager.CreateUnique(Job{ID: "sync-duplicate", Type: "sync", Profile: "mac"}); !IsDuplicateActiveJob(err, "sync", "mac") {
		t.Fatalf("same non-AWS type duplicate error = %v", err)
	}
}

func TestJobManagerArtifactLifecycle(t *testing.T) {
	newArtifact := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "auto-config.yaml")
		if err := os.WriteFile(path, []byte("secret config"), 0o600); err != nil {
			t.Fatalf("write artifact: %v", err)
		}
		return path
	}
	assertRemoved := func(t *testing.T, path string) {
		t.Helper()
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact %s still exists: %v", path, err)
		}
	}

	t.Run("create failure", func(t *testing.T) {
		manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
		if err := manager.BeginDrain(); err != nil {
			t.Fatalf("begin drain: %v", err)
		}
		artifact := newArtifact(t)
		if _, err := manager.Create(Job{ID: "create-failure", CleanupPaths: []string{artifact}}); !errors.Is(err, ErrJobsDraining) {
			t.Fatalf("create error = %v", err)
		}
		assertRemoved(t, artifact)
	})

	t.Run("runner startup failure", func(t *testing.T) {
		manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
		manager.Executable = filepath.Join(t.TempDir(), "missing-cm")
		artifact := newArtifact(t)
		job, err := manager.Create(Job{ID: "startup-failure-artifact", CleanupPaths: []string{artifact}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := manager.StartRunner(context.Background(), job); err == nil {
			t.Fatal("StartRunner error = nil")
		}
		assertRemoved(t, artifact)
	})

	for _, test := range []struct {
		name    string
		command []string
	}{
		{name: "success", command: []string{"/bin/sh", "-c", `test -f "$1"`, "sh"}},
		{name: "failed job", command: []string{"/bin/false"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
			artifact := newArtifact(t)
			command := append([]string(nil), test.command...)
			if test.name == "success" {
				command = append(command, artifact)
			}
			job, err := manager.Create(Job{ID: "run-artifact", Status: JobStatusRunning, RunnerToken: "artifact-token", Command: command, CleanupPaths: []string{artifact}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			t.Setenv(jobRunnerTokenEnv, "artifact-token")
			_, _ = manager.RunJob(context.Background(), job.ID)
			assertRemoved(t, artifact)
		})
	}

	t.Run("stale reconciliation", func(t *testing.T) {
		manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
		manager.IsRunning = func(int) bool { return false }
		artifact := newArtifact(t)
		if _, err := manager.Create(Job{ID: "stale-artifact", Status: JobStatusRunning, PID: 123, CleanupPaths: []string{artifact}}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := manager.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertRemoved(t, artifact)
	})
}

func TestJobManagerPersistsStructuredChildOutcome(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	job, err := manager.Create(Job{
		ID:          "structured-outcome",
		Type:        "aws-destroy",
		Profile:     "mac",
		Status:      JobStatusRunning,
		RunnerToken: "outcome-token",
		Command: []string{"/bin/sh", "-c",
			`printf '%s' '{"error_category":"terminal","error_code":"AccessDenied","reason":"exact redacted reason"}' > "$CM_JOB_OUTCOME_PATH"; exit 1`},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Setenv(jobRunnerTokenEnv, "outcome-token")
	completed, err := manager.RunJob(context.Background(), job.ID)
	if err == nil {
		t.Fatal("RunJob error = nil")
	}
	if completed.Status != JobStatusFailed || completed.ErrorCategory != JobErrorCategoryTerminal || completed.ErrorCode != "AccessDenied" || completed.LastError != "exact redacted reason" {
		t.Fatalf("completed job = %+v", completed)
	}
	if completed.OutcomePath != "" {
		t.Fatalf("outcome path was persisted after ingestion: %q", completed.OutcomePath)
	}
}

func TestJobOutcomeRoundTripsReleasedHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.json")
	t.Setenv(jobOutcomePathEnv, path)
	want := JobOutcome{ReleasedHosts: []string{"h-accepted-1", "h-accepted-2"}}
	if err := writeCurrentJobOutcome(want); err != nil {
		t.Fatalf("write outcome: %v", err)
	}
	got, err := readJobOutcome(path)
	if err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if strings.Join(got.ReleasedHosts, ",") != strings.Join(want.ReleasedHosts, ",") {
		t.Fatalf("released hosts = %v, want %v", got.ReleasedHosts, want.ReleasedHosts)
	}
}

func TestJobManagerFinishRunJobPersistsReleasedHostsWithoutAliasing(t *testing.T) {
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	outcomePath := filepath.Join(t.TempDir(), "outcome.json")
	if err := os.WriteFile(outcomePath, []byte(`{"released_hosts":["h-accepted"]}`), 0o600); err != nil {
		t.Fatalf("write outcome file: %v", err)
	}
	job, err := manager.Create(Job{
		ID:          "accepted-host-outcome",
		Type:        "aws-destroy",
		Profile:     "mac",
		Status:      JobStatusRunning,
		OutcomePath: outcomePath,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	outcome := &JobOutcome{ReleasedHosts: []string{"h-accepted"}}
	completed, err := manager.finishRunJob(job.ID, JobStatusSuccess, nil, nil, outcome)
	if err != nil {
		t.Fatalf("finish job: %v", err)
	}
	outcome.ReleasedHosts[0] = "h-mutated"
	if strings.Join(completed.ReleasedHosts, ",") != "h-accepted" {
		t.Fatalf("completed released hosts aliased outcome: %v", completed.ReleasedHosts)
	}
	persisted, err := manager.loadRaw(job.ID)
	if err != nil {
		t.Fatalf("load completed job: %v", err)
	}
	if strings.Join(persisted.ReleasedHosts, ",") != "h-accepted" || persisted.OutcomePath != "" {
		t.Fatalf("persisted job = %+v", persisted)
	}
	if _, err := os.Stat(outcomePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outcome file still exists: %v", err)
	}
}

func TestDestroyJobPersistsAcceptedReleaseHosts(t *testing.T) {
	for _, test := range []struct {
		name         string
		fake         *fakeAWSClient
		wantDeferred bool
	}{
		{
			name: "complete",
			fake: &fakeAWSClient{status: AWSStatus{
				Hosts: []DedicatedHostStatus{{HostID: "h-complete", State: "available", Tags: managedTestTags()}},
			}},
		},
		{
			name: "deferred after accepted release",
			fake: &fakeAWSClient{status: AWSStatus{
				Hosts: []DedicatedHostStatus{
					{HostID: "h-accepted", State: "available", Tags: managedTestTags()},
					{HostID: "h-deferred", State: "pending", Tags: managedTestTags()},
				},
			}, releaseErrs: []error{nil}, releaseErr: errors.New("host transition is still in progress")},
			wantDeferred: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			app := testApp(&out, &errOut, t.TempDir())
			app.AWSService = testAWSService(test.fake)
			app.AWSService.DestroyPollInterval = time.Millisecond
			app.AWSService.DestroyTimeout = 3 * time.Millisecond
			outcomePath := filepath.Join(t.TempDir(), "outcome.json")
			t.Setenv(jobOutcomePathEnv, outcomePath)
			profile := validAWSProfile()
			plan, err := app.AWSService.Plan(profile)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if code := app.runAWSDestroy(context.Background(), profile, plan, true); code != 0 {
				t.Fatalf("destroy code = %d, stderr = %s", code, errOut.String())
			}
			outcome, err := readJobOutcome(outcomePath)
			if err != nil {
				t.Fatalf("read outcome: %v", err)
			}
			if strings.Join(outcome.ReleasedHosts, ",") != map[bool]string{false: "h-complete", true: "h-accepted"}[test.wantDeferred] || outcome.Deferred != test.wantDeferred {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}

func TestTerminalDestroyChildOutcomeStopsAutoReleaseWithExactReason(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	manager := NewJobManager(filepath.Join(t.TempDir(), "jobs"))
	manager.Now = func() time.Time { return now }
	job, err := manager.Create(Job{
		ID:          "terminal-destroy",
		Type:        "aws-destroy",
		Profile:     "mac",
		AppleEmail:  "apple@example.com",
		Status:      JobStatusRunning,
		RunnerToken: "terminal-token",
		StartedAt:   now,
		Command: []string{"/bin/sh", "-c",
			`printf '%s' '{"error_category":"terminal","error_code":"AccessDenied","reason":"authorization denied for expected account"}' > "$CM_JOB_OUTCOME_PATH"; exit 1`},
	})
	if err != nil {
		t.Fatalf("create destroy job: %v", err)
	}
	t.Setenv(jobRunnerTokenEnv, "terminal-token")
	completed, err := manager.RunJob(context.Background(), job.ID)
	if err == nil || completed.ErrorCategory != JobErrorCategoryTerminal {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	reminder := scheduledAutoRelease(now)
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
	reminder.AutoReleaseStartedAt = now.Format(time.RFC3339)
	reminder.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
	reminder.AutoReleaseAttempts = 1
	store := newAutoReleaseTestStore(reminder)
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(now.Add(time.Minute), store)
	coordinator.Jobs = manager
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		return AWSStatus{Hosts: []DedicatedHostStatus{autoReleaseTestHost("available")}}, nil
	}
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("coordinator Scan: %v", err)
	}
	got := store.get("mac")
	if got.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || got.AutoReleaseLastError != "authorization denied for expected account" || len(*starts) != 0 {
		t.Fatalf("terminal outcome retried or lost reason: reminder=%+v starts=%d", got, len(*starts))
	}
	if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationFinalFailure {
		t.Fatalf("notifications = %+v", *notifications)
	}
}
