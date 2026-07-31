package connectmac

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const DefaultJobDir = "~/.connectmac/jobs"

type JobStatus string

const (
	JobStatusStarting    JobStatus = "starting"
	JobStatusRunning     JobStatus = "running"
	JobStatusSuccess     JobStatus = "success"
	JobStatusFailed      JobStatus = "failed"
	JobStatusDeferred    JobStatus = "deferred"
	JobStatusInterrupted JobStatus = "interrupted"
	JobStatusUnknown     JobStatus = "unknown"
)

type JobLifecycleState string

const (
	JobLifecyclePending   JobLifecycleState = "pending"
	JobLifecycleWaiting   JobLifecycleState = "waiting"
	JobLifecycleFinalized JobLifecycleState = "finalized"
	JobLifecycleFailed    JobLifecycleState = "failed"
)

const interruptedJobError = "background process exited before recording completion"

const jobRunnerTokenEnv = "CM_JOB_RUNNER_TOKEN"
const jobOutcomePathEnv = "CM_JOB_OUTCOME_PATH"
const jobRequestIDEnv = "CM_OPERATION_REQUEST_ID"
const jobIDEnv = "CM_OPERATION_JOB_ID"
const jobSourceEnv = "CM_OPERATION_SOURCE"
const jobActorMemberIDEnv = "CM_OPERATION_ACTOR_MEMBER_ID"
const jobActorEmailEnv = "CM_OPERATION_ACTOR_EMAIL"
const jobActorNameEnv = "CM_OPERATION_ACTOR_NAME"

type JobErrorCategory string

const (
	JobErrorCategoryRecoverable JobErrorCategory = "recoverable"
	JobErrorCategoryTerminal    JobErrorCategory = "terminal"
	JobErrorCategoryUnknown     JobErrorCategory = "unknown"
)

type JobOutcome struct {
	ErrorCategory JobErrorCategory `json:"error_category,omitempty"`
	ErrorCode     string           `json:"error_code,omitempty"`
	Reason        string           `json:"reason,omitempty"`
	Deferred      bool             `json:"deferred,omitempty"`
}

type Job struct {
	ID                               string            `json:"id"`
	Type                             string            `json:"type"`
	Profile                          string            `json:"profile"`
	AppleEmail                       string            `json:"apple_email,omitempty"`
	RequestID                        string            `json:"request_id,omitempty"`
	Source                           string            `json:"source,omitempty"`
	ActorMemberID                    string            `json:"actor_member_id,omitempty"`
	ActorEmail                       string            `json:"actor_email,omitempty"`
	ActorName                        string            `json:"actor_name,omitempty"`
	Status                           JobStatus         `json:"status"`
	PID                              int               `json:"pid,omitempty"`
	CreatorPID                       int               `json:"creator_pid,omitempty"`
	StartedAt                        time.Time         `json:"started_at"`
	FinishedAt                       time.Time         `json:"finished_at,omitempty"`
	Log                              string            `json:"log"`
	Command                          []string          `json:"command"`
	Notify                           bool              `json:"notify"`
	ExitCode                         *int              `json:"exit_code,omitempty"`
	LastError                        string            `json:"last_error,omitempty"`
	ErrorCategory                    JobErrorCategory  `json:"error_category,omitempty"`
	ErrorCode                        string            `json:"error_code,omitempty"`
	OutcomePath                      string            `json:"outcome_path,omitempty"`
	CleanupPaths                     []string          `json:"cleanup_paths,omitempty"`
	CompletedBy                      int               `json:"completed_by,omitempty"`
	RunnerToken                      string            `json:"runner_token,omitempty"`
	LifecycleOwnerEmail              string            `json:"lifecycle_owner_email,omitempty"`
	LifecycleState                   JobLifecycleState `json:"lifecycle_state,omitempty"`
	LifecycleFinalizedAt             time.Time         `json:"lifecycle_finalized_at,omitempty"`
	LifecycleEventRecordedAt         time.Time         `json:"lifecycle_event_recorded_at,omitempty"`
	LifecycleNotifyClaimedAt         time.Time         `json:"lifecycle_notify_claimed_at,omitempty"`
	LifecycleNotifyAttempts          int               `json:"lifecycle_notify_attempts,omitempty"`
	LifecycleNotifyNextAttemptAt     time.Time         `json:"lifecycle_notify_next_attempt_at,omitempty"`
	LifecycleNotifyConfigFingerprint string            `json:"lifecycle_notify_config_fingerprint,omitempty"`
	LifecycleNotifyExhaustedAt       time.Time         `json:"lifecycle_notify_exhausted_at,omitempty"`
	LifecycleNotifyFailureRecordedAt time.Time         `json:"lifecycle_notify_failure_recorded_at,omitempty"`
	LifecycleNotifiedAt              time.Time         `json:"lifecycle_notified_at,omitempty"`
	LifecycleError                   string            `json:"lifecycle_error,omitempty"`
}

type JobsDrainingError struct{}

func (*JobsDrainingError) Error() string { return "background jobs are draining" }

var ErrJobsDraining = &JobsDrainingError{}

type DuplicateActiveJobError struct {
	Type     string
	Profile  string
	Existing Job
}

func (e *DuplicateActiveJobError) Error() string {
	return fmt.Sprintf("active %s job already exists for profile %s: %s", e.Type, e.Profile, e.Existing.ID)
}

func IsDuplicateActiveJob(err error, jobType, profile string) bool {
	var duplicate *DuplicateActiveJobError
	return errors.As(err, &duplicate) && duplicate.Type == jobType && duplicate.Profile == profile
}

type JobManager struct {
	Dir        string
	Executable string
	Now        func() time.Time
	IsRunning  func(pid int) bool
	Sleep      func(ctx context.Context, duration time.Duration) error
	Rename     func(oldPath, newPath string) error
	Notify     func(title, message string) error
}

type WaitAllTimeoutError struct {
	Timeout time.Duration
	Active  []Job
}

func (e *WaitAllTimeoutError) Error() string {
	return fmt.Sprintf("timed out after %s waiting for %d active job(s)", e.Timeout, len(e.Active))
}

func NewJobManager(dir string) JobManager {
	return JobManager{
		Dir:       dir,
		Now:       time.Now,
		IsRunning: ProcessRunning,
		Sleep:     sleepContext,
		Rename:    os.Rename,
		Notify:    SendMacNotification,
	}
}

func (m JobManager) normalize() JobManager {
	if m.Dir == "" {
		m.Dir = DefaultJobDir
	}
	if m.Now == nil {
		m.Now = time.Now
	}
	if m.IsRunning == nil {
		m.IsRunning = ProcessRunning
	}
	if m.Sleep == nil {
		m.Sleep = sleepContext
	}
	if m.Rename == nil {
		m.Rename = os.Rename
	}
	if m.Notify == nil {
		m.Notify = SendMacNotification
	}
	return m
}

func (m JobManager) Create(job Job) (Job, error) {
	if job.Type == "aws-open" || job.Type == "aws-destroy" {
		return m.CreateUnique(job)
	}
	return m.create(job, false)
}

func (m JobManager) CreateUnique(job Job) (created Job, err error) {
	if strings.TrimSpace(job.Profile) == "" {
		return Job{}, errors.New("job profile is required for unique creation")
	}
	guardEntered := false
	err = m.WithProfileOperation(job.Profile, func() error {
		guardEntered = true
		created, err = m.createUniqueLocked(job)
		return err
	})
	if err != nil && !guardEntered {
		_ = removeJobPaths(job.CleanupPaths)
	}
	return created, err
}

func (m JobManager) createUniqueLocked(job Job) (Job, error) {
	return m.create(job, true)
}

func jobBlocksUniqueCreation(job Job) bool {
	return job.Status == JobStatusStarting ||
		job.Status == JobStatusRunning ||
		job.LifecycleState == JobLifecyclePending ||
		job.LifecycleState == JobLifecycleWaiting
}

func (m JobManager) create(job Job, unique bool) (Job, error) {
	m = m.normalize()
	if job.StartedAt.IsZero() {
		job.StartedAt = m.Now()
	}
	if job.Status == "" {
		job.Status = JobStatusStarting
		job.CreatorPID = os.Getpid()
	}
	if job.ID == "" {
		job.ID = newJobID(job.Type, job.Profile, job.StartedAt)
	}
	var created Job
	err := m.withGlobalLock(func(baseDir string) error {
		if _, err := os.Stat(filepath.Join(baseDir, ".draining")); err == nil {
			return ErrJobsDraining
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check jobs drain marker: %w", err)
		}
		if unique {
			jobs, err := m.listRaw()
			if err != nil {
				return err
			}
			for _, existing := range jobs {
				if existing.Type == job.Type && existing.Profile == job.Profile && jobBlocksUniqueCreation(existing) {
					return &DuplicateActiveJobError{Type: job.Type, Profile: job.Profile, Existing: existing}
				}
			}
		}
		path, err := m.JobPath(job.ID)
		if err != nil {
			return err
		}
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create job dir %s: %w", dir, err)
		}
		if job.Log == "" {
			job.Log = filepath.Join(dir, "run.log")
		}
		return m.withJobLock(job.ID, func() error {
			if err := m.Save(job); err != nil {
				return err
			}
			created = job
			return nil
		})
	})
	if err != nil {
		_ = removeJobPaths(job.CleanupPaths)
		return Job{}, err
	}
	return created, nil
}

func (m JobManager) WithProfileOperation(profileName string, fn func() error) error {
	if fn == nil {
		return errors.New("profile operation is required")
	}
	path, err := m.profileOperationLockPath(profileName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create profile operation lock directory: %w", err)
	}
	return withFileLock(path, fn)
}

func (m JobManager) profileOperationLockPath(profileName string) (string, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return "", errors.New("profile is required")
	}
	m = m.normalize()
	baseDir, err := ExpandPath(m.Dir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(profileName))
	return filepath.Join(baseDir, ".profile-locks", hex.EncodeToString(sum[:])+".lock"), nil
}

func (m JobManager) Save(job Job) error {
	m = m.normalize()
	path, err := m.JobPath(job.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create job dir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".job-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary job file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("chmod temporary job file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary job file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary job file: %w", err)
	}
	if err := m.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace job file: %w", err)
	}
	return nil
}

func (m JobManager) Load(id string) (Job, error) {
	if _, err := m.loadRaw(id); err != nil {
		return Job{}, err
	}
	if _, err := m.reconcileID(id); err != nil {
		return Job{}, err
	}
	return m.loadRaw(id)
}

func (m JobManager) Update(id string, update func(Job) (Job, error)) (Job, error) {
	if update == nil {
		return Job{}, errors.New("job update callback is required")
	}
	var updated Job
	err := m.withJobLock(id, func() error {
		current, err := m.loadRaw(id)
		if err != nil {
			return err
		}
		next, err := update(current)
		if err != nil {
			return err
		}
		if next.ID != current.ID {
			return fmt.Errorf("job update cannot change ID from %q to %q", current.ID, next.ID)
		}
		if err := m.Save(next); err != nil {
			return err
		}
		updated = next
		return nil
	})
	return updated, err
}

func (m JobManager) loadRaw(id string) (Job, error) {
	path, err := m.JobPath(id)
	if err != nil {
		return Job{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Job{}, err
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return Job{}, err
	}
	if job.ID != id {
		return Job{}, fmt.Errorf("job ID mismatch: requested %q, file contains %q", id, job.ID)
	}
	return job, nil
}

func (m JobManager) List() ([]Job, error) {
	if _, err := m.Reconcile(); err != nil {
		return nil, err
	}
	return m.listRaw()
}

func (m JobManager) listRaw() ([]Job, error) {
	m = m.normalize()
	dir, err := ExpandPath(m.Dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	jobs := []Job{}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		job, err := m.loadRaw(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("load job %s: %w", entry.Name(), err)
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].StartedAt.After(jobs[j].StartedAt)
	})
	return jobs, nil
}

func (m JobManager) Reconcile() ([]Job, error) {
	m = m.normalize()
	jobs, err := m.listRaw()
	if err != nil {
		return nil, err
	}
	changed := make([]Job, 0)
	for _, job := range jobs {
		updated, err := m.reconcileID(job.ID)
		if err != nil {
			return nil, err
		}
		if updated != nil {
			changed = append(changed, *updated)
		}
	}
	return changed, nil
}

func (m JobManager) reconcileID(id string) (*Job, error) {
	m = m.normalize()
	var changed *Job
	err := m.withJobLock(id, func() error {
		job, err := m.loadRaw(id)
		if err != nil {
			return err
		}
		interrupted := false
		switch job.Status {
		case JobStatusStarting:
			interrupted = (job.CreatorPID > 0 && !m.IsRunning(job.CreatorPID)) || m.Now().Sub(job.StartedAt) >= time.Minute
		case JobStatusRunning:
			interrupted = job.PID > 0 && !m.IsRunning(job.PID)
		}
		if !interrupted {
			return nil
		}
		job.Status = JobStatusInterrupted
		job.FinishedAt = m.Now()
		job.LastError = interruptedJobError
		m.applyPersistedOutcome(&job)
		m.cleanupFinishedJob(&job)
		if err := m.Save(job); err != nil {
			return fmt.Errorf("save reconciled job %s: %w", id, err)
		}
		changed = &job
		return nil
	})
	return changed, err
}

func (m JobManager) Active() ([]Job, error) {
	m = m.normalize()
	if _, err := m.Reconcile(); err != nil {
		return nil, err
	}
	jobs, err := m.listRaw()
	if err != nil {
		return nil, err
	}
	active := make([]Job, 0)
	for _, job := range jobs {
		if job.Status == JobStatusStarting || job.Status == JobStatusRunning {
			active = append(active, job)
		}
	}
	return active, nil
}

func (m JobManager) WaitAll(ctx context.Context, timeout, interval time.Duration, progress func(time.Duration, []Job)) error {
	m = m.normalize()
	if timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	if interval <= 0 {
		return fmt.Errorf("interval must be positive")
	}
	started := m.Now()
	slept := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		active, err := m.Active()
		if err != nil {
			return err
		}
		if len(active) == 0 {
			return nil
		}
		elapsed := m.Now().Sub(started)
		if slept && progress != nil {
			progress(elapsed, active)
		}
		if elapsed >= timeout {
			return &WaitAllTimeoutError{Timeout: timeout, Active: active}
		}
		wait := interval
		if remaining := timeout - elapsed; wait > remaining {
			wait = remaining
		}
		if err := m.Sleep(ctx, wait); err != nil {
			return err
		}
		slept = true
	}
}

func (m JobManager) BeginDrain() error {
	m = m.normalize()
	return m.withGlobalLock(func(baseDir string) error {
		path := filepath.Join(baseDir, ".draining")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("create jobs drain marker: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			return fmt.Errorf("chmod jobs drain marker: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close jobs drain marker: %w", err)
		}
		return nil
	})
}

func (m JobManager) EndDrain() error {
	m = m.normalize()
	return m.withGlobalLock(func(baseDir string) error {
		if err := os.Remove(filepath.Join(baseDir, ".draining")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove jobs drain marker: %w", err)
		}
		return nil
	})
}

func (m JobManager) withGlobalLock(fn func(baseDir string) error) error {
	m = m.normalize()
	baseDir, err := ExpandPath(m.Dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return fmt.Errorf("create jobs dir %s: %w", baseDir, err)
	}
	return withFileLock(filepath.Join(baseDir, ".lock"), func() error { return fn(baseDir) })
}

func (m JobManager) withJobLock(id string, fn func() error) error {
	path, err := m.JobPath(id)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create job dir %s: %w", dir, err)
	}
	return withFileLock(filepath.Join(dir, ".lock"), fn)
}

func withFileLock(path string, fn func() error) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file %s: %w", path, err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod lock file %s: %w", path, err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer func() {
		if unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN); err == nil && unlockErr != nil {
			err = fmt.Errorf("unlock %s: %w", path, unlockErr)
		}
	}()
	return fn()
}

func (m JobManager) StartRunner(ctx context.Context, job Job) (Job, error) {
	m = m.normalize()
	executable := m.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return m.failRunnerStartup(job.ID, err)
		}
	}
	logFile, err := os.OpenFile(job.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return m.failRunnerStartup(job.ID, err)
	}
	defer logFile.Close()
	token, err := newRunnerToken()
	if err != nil {
		return m.failRunnerStartup(job.ID, err)
	}
	cmd := exec.CommandContext(ctx, executable, "job", "run", job.ID)
	cmd.Env = append(environmentWithout(jobRunnerTokenEnv), jobRunnerTokenEnv+"="+token)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	var started Job
	err = m.withJobLock(job.ID, func() error {
		current, err := m.loadRaw(job.ID)
		if err != nil {
			return err
		}
		if (current.Status != JobStatusStarting && current.Status != JobStatusRunning) || current.PID != 0 || current.CompletedBy != 0 {
			return fmt.Errorf("job %s is not eligible to start", job.ID)
		}
		current.RunnerToken = token
		if err := m.Save(current); err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			current.Status = JobStatusFailed
			current.FinishedAt = m.Now()
			current.LastError = err.Error()
			current.RunnerToken = ""
			current.CreatorPID = 0
			m.cleanupFinishedJob(&current)
			if saveErr := m.Save(current); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			started = current
			return err
		}
		current.PID = cmd.Process.Pid
		current.Status = JobStatusRunning
		current.CreatorPID = 0
		if err := m.Save(current); err != nil {
			current.Status = JobStatusFailed
			current.FinishedAt = m.Now()
			current.LastError = err.Error()
			current.RunnerToken = ""
			m.cleanupFinishedJob(&current)
			_ = m.Save(current)
			return err
		}
		if err := cmd.Process.Release(); err != nil {
			current.Status = JobStatusFailed
			current.FinishedAt = m.Now()
			current.LastError = err.Error()
			current.RunnerToken = ""
			m.cleanupFinishedJob(&current)
			if saveErr := m.Save(current); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return err
		}
		started = current
		return nil
	})
	if err != nil {
		if started.ID != "" {
			return started, err
		}
		return m.failRunnerStartup(job.ID, err)
	}
	return started, nil
}

func (m JobManager) RunJob(ctx context.Context, id string) (Job, error) {
	m = m.normalize()
	token := os.Getenv(jobRunnerTokenEnv)
	if token == "" {
		return Job{}, errors.New("job run is restricted to the internal background runner")
	}
	var job Job
	err := m.withJobLock(id, func() error {
		current, err := m.loadRaw(id)
		if err != nil {
			return err
		}
		if current.Status != JobStatusRunning {
			return fmt.Errorf("job %s cannot run with status %s", id, current.Status)
		}
		if current.RunnerToken == "" || current.RunnerToken != token {
			return errors.New("invalid or already consumed background runner token")
		}
		if current.PID != 0 && current.CompletedBy != 0 {
			return fmt.Errorf("job %s has already been claimed", id)
		}
		current.RunnerToken = ""
		current.CompletedBy = os.Getpid()
		current.OutcomePath = filepath.Join(filepath.Dir(current.Log), "outcome.json")
		if err := os.Remove(current.OutcomePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := m.Save(current); err != nil {
			return err
		}
		job = current
		return nil
	})
	if err != nil {
		return Job{}, err
	}
	if len(job.Command) == 0 {
		return m.finishRunJob(id, JobStatusFailed, nil, errors.New("job command is empty"), nil)
	}
	logFile, err := os.OpenFile(job.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return m.finishRunJob(id, JobStatusFailed, nil, err, nil)
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "cm job %s started at %s\n", job.ID, m.Now().Format(time.RFC3339))
	cmd := exec.CommandContext(ctx, job.Command[0], job.Command[1:]...)
	cmd.Env = append(environmentWithout(
		jobRunnerTokenEnv,
		jobOutcomePathEnv,
		jobRequestIDEnv,
		jobIDEnv,
		jobSourceEnv,
		jobActorMemberIDEnv,
		jobActorEmailEnv,
		jobActorNameEnv,
	), jobOutcomePathEnv+"="+job.OutcomePath)
	cmd.Env = append(cmd.Env, jobCorrelationEnvironment(job)...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	err = cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		job.LastError = err.Error()
	}
	outcome, outcomeErr := readJobOutcome(job.OutcomePath)
	if outcomeErr != nil && !errors.Is(outcomeErr, os.ErrNotExist) {
		if err == nil {
			err = outcomeErr
		}
	}
	status := JobStatusSuccess
	if exitCode != 0 {
		status = JobStatusFailed
	} else if outcome != nil && outcome.Deferred {
		status = JobStatusDeferred
	}
	job, finishErr := m.finishRunJob(id, status, &exitCode, err, outcome)
	if finishErr != nil {
		err = finishErr
	}
	fmt.Fprintf(logFile, "cm job %s finished with status %s at %s\n", job.ID, job.Status, job.FinishedAt.Format(time.RFC3339))
	if job.Notify {
		message := fmt.Sprintf("%s %s", job.Type, job.Status)
		if job.Profile != "" {
			message = fmt.Sprintf("%s: %s", job.Profile, message)
		}
		_ = m.Notify("ConnectMac", message)
	}
	return job, err
}

func jobCorrelationEnvironment(job Job) []string {
	values := []struct {
		key   string
		value string
	}{
		{jobRequestIDEnv, job.RequestID},
		{jobIDEnv, job.ID},
		{jobSourceEnv, job.Source},
		{jobActorMemberIDEnv, job.ActorMemberID},
		{jobActorEmailEnv, job.ActorEmail},
		{jobActorNameEnv, job.ActorName},
	}
	env := make([]string, 0, len(values))
	for _, item := range values {
		value := strings.Map(func(r rune) rune {
			if r == 0 || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, strings.TrimSpace(item.value))
		if value != "" {
			env = append(env, item.key+"="+value)
		}
	}
	return env
}

func (m JobManager) finishRunJob(id string, status JobStatus, exitCode *int, runErr error, outcome *JobOutcome) (Job, error) {
	var finished Job
	err := m.withJobLock(id, func() error {
		job, err := m.loadRaw(id)
		if err != nil {
			return err
		}
		if job.Status != JobStatusRunning {
			finished = job
			return fmt.Errorf("job %s changed to terminal status %s before runner completion", id, job.Status)
		}
		job.Status = status
		job.ExitCode = exitCode
		job.FinishedAt = m.Now()
		if runErr != nil {
			job.LastError = runErr.Error()
		}
		if outcome != nil {
			job.ErrorCategory = outcome.ErrorCategory
			job.ErrorCode = outcome.ErrorCode
			if outcome.Reason != "" {
				job.LastError = outcome.Reason
			}
		}
		m.cleanupFinishedJob(&job)
		if err := m.Save(job); err != nil {
			return err
		}
		finished = job
		return nil
	})
	if err != nil {
		return finished, errors.Join(runErr, err)
	}
	return finished, runErr
}

func (m JobManager) failRunnerStartup(id string, cause error) (Job, error) {
	var failed Job
	saveErr := m.withJobLock(id, func() error {
		job, err := m.loadRaw(id)
		if err != nil {
			return err
		}
		if (job.Status == JobStatusStarting || job.Status == JobStatusRunning) && job.PID == 0 {
			job.Status = JobStatusFailed
			job.FinishedAt = m.Now()
			job.LastError = cause.Error()
			job.RunnerToken = ""
			job.CreatorPID = 0
			m.cleanupFinishedJob(&job)
			if err := m.Save(job); err != nil {
				return err
			}
		}
		failed = job
		return nil
	})
	return failed, errors.Join(cause, saveErr)
}

func newRunnerToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate background runner token: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func readJobOutcome(path string) (*JobOutcome, error) {
	if strings.TrimSpace(path) == "" {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var outcome JobOutcome
	if err := json.Unmarshal(data, &outcome); err != nil {
		return nil, fmt.Errorf("read structured job outcome: %w", err)
	}
	return &outcome, nil
}

func writeCurrentJobOutcome(outcome JobOutcome) error {
	path := strings.TrimSpace(os.Getenv(jobOutcomePathEnv))
	if path == "" {
		return nil
	}
	data, err := json.Marshal(outcome)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".outcome-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func (m JobManager) applyPersistedOutcome(job *Job) {
	outcome, err := readJobOutcome(job.OutcomePath)
	if err != nil || outcome == nil {
		return
	}
	job.ErrorCategory = outcome.ErrorCategory
	job.ErrorCode = outcome.ErrorCode
	if outcome.Reason != "" {
		job.LastError = outcome.Reason
	}
	if outcome.Deferred {
		job.Status = JobStatusDeferred
	}
}

func (m JobManager) cleanupFinishedJob(job *Job) {
	if job.OutcomePath != "" {
		if err := os.Remove(job.OutcomePath); err == nil || errors.Is(err, os.ErrNotExist) {
			job.OutcomePath = ""
		}
	}
	remaining := job.CleanupPaths[:0]
	for _, path := range job.CleanupPaths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			remaining = append(remaining, path)
		}
	}
	job.CleanupPaths = remaining
}

func removeJobPaths(paths []string) error {
	var result error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func environmentWithout(keys ...string) []string {
	prefixes := make([]string, len(keys))
	for i, key := range keys {
		prefixes[i] = key + "="
	}
	environment := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		keep := true
		for _, prefix := range prefixes {
			if strings.HasPrefix(value, prefix) {
				keep = false
				break
			}
		}
		if keep {
			environment = append(environment, value)
		}
	}
	return environment
}

func (m JobManager) JobPath(id string) (string, error) {
	m = m.normalize()
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) || strings.Contains(id, string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid job id %q", id)
	}
	dir, err := ExpandPath(m.Dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id, "job.json"), nil
}

func newJobID(jobType, profile string, at time.Time) string {
	parts := []string{slugPart(jobType), slugPart(profile), at.Format("20060102150405")}
	out := []string{}
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return "job-" + at.Format("20060102150405")
	}
	return strings.Join(out, "-")
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func slugPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func formatJobsTable(jobs []Job) string {
	rows := [][]string{{"ID", "TYPE", "PROFILE", "STATUS", "PID", "STARTED"}}
	for _, job := range jobs {
		pid := ""
		if job.PID > 0 {
			pid = strconv.Itoa(job.PID)
		}
		started := ""
		if !job.StartedAt.IsZero() {
			started = job.StartedAt.Format("2006-01-02 15:04:05")
		}
		rows = append(rows, []string{job.ID, job.Type, job.Profile, string(job.Status), pid, started})
	}
	return formatRows(rows)
}

func SendMacNotification(title, message string) error {
	script := fmt.Sprintf("display notification %s with title %s", appleScriptString(message), appleScriptString(title))
	return exec.Command("osascript", "-e", script).Run()
}

func appleScriptString(value string) string {
	escaped := strings.ReplaceAll(value, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}

func copyTail(out io.Writer, path string, maxBytes int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		if _, err := file.Seek(info.Size()-maxBytes, io.SeekStart); err != nil {
			return err
		}
		fmt.Fprintln(out, "... truncated ...")
	}
	_, err = io.Copy(out, file)
	return err
}
