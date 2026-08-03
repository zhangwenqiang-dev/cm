package connectmac

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	LocalTransferQueued      = "queued"
	LocalTransferRunning     = "running"
	LocalTransferSucceeded   = "succeeded"
	LocalTransferFailed      = "failed"
	LocalTransferInterrupted = "interrupted"

	localTransferOutputLimit       = 64 * 1024
	localTransferRetention         = 24 * time.Hour
	terminalCallbackTimeout        = 5 * time.Second
	localTransferCallbackQueueSize = 16

	localTransferCallbackPanicWarning   = "transfer event callback panicked"
	localTransferCallbackTimeoutWarning = "transfer event callback timed out"
	localTransferCallbackQueueWarning   = "transfer event callback queue is full"
)

var (
	ErrLocalTransferDraining = errors.New("local transfer manager is draining")
	ErrLocalTransferConflict = errors.New("active local transfer has a different transfer ID")
	rsyncPercentPattern      = regexp.MustCompile(`(?:^|\s)(\d{1,3})%`)
	rsyncToCheckPattern      = regexp.MustCompile(`to-(?:chk|check)=(\d+)/(\d+)`)
)

const (
	LocalTransferProgressEstimated = "estimated"
	LocalTransferProgressTotal     = "total"
)

type LocalTransferJob struct {
	ID                string     `json:"id"`
	TransferID        string     `json:"transfer_id,omitempty"`
	Profile           string     `json:"profile"`
	Direction         string     `json:"direction"`
	Status            string     `json:"status"`
	Phase             string     `json:"phase"`
	Percent           int        `json:"percent"`
	ProgressMode      string     `json:"progress_mode,omitempty"`
	Output            string     `json:"output"`
	Error             string     `json:"error"`
	CallbackWarning   string     `json:"callback_warning,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	StartedAt         *time.Time `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	callbackEvents    chan localTransferCallbackDispatch
	emittedMilestones map[string]bool
}

func (j LocalTransferJob) Active() bool {
	return j.Status == LocalTransferQueued || j.Status == LocalTransferRunning
}

type LocalTransferEvent struct {
	TransferID   string
	LocalJobID   string
	Profile      string
	Direction    string
	Status       string
	Phase        string
	Percent      int
	ProgressMode string
	Elapsed      time.Duration
	Error        string
}

type localTransferCallbackDispatch struct {
	event    LocalTransferEvent
	terminal bool
	done     chan struct{}
}

type LocalTransferJobManager struct {
	mu                      sync.Mutex
	jobs                    map[string]*LocalTransferJob
	now                     func() time.Time
	retention               time.Duration
	sequence                uint64
	draining                bool
	terminalCallbackTimeout time.Duration
}

func NewLocalTransferJobManager() *LocalTransferJobManager {
	return &LocalTransferJobManager{
		jobs:                    make(map[string]*LocalTransferJob),
		now:                     time.Now,
		retention:               localTransferRetention,
		terminalCallbackTimeout: terminalCallbackTimeout,
	}
}

func (m *LocalTransferJobManager) Start(profile, direction string, run func(func(string)) error) (LocalTransferJob, error) {
	return m.StartWithEvents("", profile, direction, nil, run)
}

func (m *LocalTransferJobManager) StartWithEvents(transferID, profile, direction string, onEvent func(LocalTransferEvent), run func(func(string)) error) (LocalTransferJob, error) {
	return m.StartWithOptions(transferID, profile, direction, LocalTransferProgressEstimated, onEvent, run)
}

func (m *LocalTransferJobManager) StartWithOptions(transferID, profile, direction, progressMode string, onEvent func(LocalTransferEvent), run func(func(string)) error) (LocalTransferJob, error) {
	m.mu.Lock()
	m.cleanupLocked()
	if m.draining {
		m.mu.Unlock()
		return LocalTransferJob{}, ErrLocalTransferDraining
	}
	for _, job := range m.jobs {
		if job.Profile == profile && job.Direction == direction && job.Active() {
			if transferID != "" && job.TransferID != "" && transferID != job.TransferID {
				m.mu.Unlock()
				return LocalTransferJob{}, fmt.Errorf("%w: active=%s requested=%s", ErrLocalTransferConflict, job.TransferID, transferID)
			}
			result := *job
			m.mu.Unlock()
			return result, nil
		}
	}
	m.sequence++
	created := m.now()
	job := &LocalTransferJob{
		ID:                fmt.Sprintf("transfer-%d-%d", created.UnixNano(), m.sequence),
		TransferID:        transferID,
		Profile:           profile,
		Direction:         direction,
		Status:            LocalTransferQueued,
		Phase:             TransferPhasePreparing,
		ProgressMode:      normalizeLocalTransferProgressMode(progressMode),
		CreatedAt:         created,
		emittedMilestones: make(map[string]bool),
	}
	if onEvent != nil {
		job.callbackEvents = make(chan localTransferCallbackDispatch, localTransferCallbackQueueSize)
	}
	m.jobs[job.ID] = job
	result := *job
	m.mu.Unlock()

	if onEvent != nil {
		// Production callbacks are internal bounded JSONL writes. The dispatcher still
		// isolates callback failures so transfer execution never depends on that contract.
		go m.runCallbackDispatcher(job.ID, onEvent, job.callbackEvents)
	}
	go m.run(job.ID, run)
	return result, nil
}

func (m *LocalTransferJobManager) TryDrain() ([]LocalTransferJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	active := make([]LocalTransferJob, 0)
	for _, job := range m.jobs {
		if job.Active() {
			active = append(active, *job)
		}
	}
	if len(active) > 0 {
		sort.Slice(active, func(i, j int) bool { return active[i].CreatedAt.Before(active[j].CreatedAt) })
		return active, false
	}
	m.draining = true
	return active, true
}

func (m *LocalTransferJobManager) Resume() {
	m.mu.Lock()
	m.draining = false
	m.mu.Unlock()
}

func (m *LocalTransferJobManager) Get(id string) (LocalTransferJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	job, ok := m.jobs[id]
	if !ok {
		return LocalTransferJob{}, false
	}
	return *job, true
}

func (m *LocalTransferJobManager) List(profile string) []LocalTransferJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	jobs := make([]LocalTransferJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		if profile == "" || job.Profile == profile {
			jobs = append(jobs, *job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].CreatedAt.After(jobs[j].CreatedAt)
	})
	return jobs
}

func (m *LocalTransferJobManager) Active() []LocalTransferJob {
	jobs := m.List("")
	active := jobs[:0]
	for _, job := range jobs {
		if job.Active() {
			active = append(active, job)
		}
	}
	return active
}

func (m *LocalTransferJobManager) run(id string, run func(func(string)) error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	started := m.now()
	job.Status = LocalTransferRunning
	job.Phase = TransferPhasePreparing
	job.StartedAt = &started
	job.emittedMilestones[transferMilestoneKey(job.Phase, 0)] = true
	event := localTransferEvent(*job, started)
	m.mu.Unlock()
	m.dispatchLocalTransferEvent(id, event, false)

	err := run(func(output string) {
		m.appendOutput(id, output)
	})

	m.mu.Lock()
	job, ok = m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	finished := m.now()
	terminal := *job
	terminal.FinishedAt = &finished
	switch {
	case err == nil:
		terminal.Status = LocalTransferSucceeded
		terminal.Phase, terminal.Percent = mapRsyncProgress(100, true, terminal.ProgressMode)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		terminal.Status = LocalTransferInterrupted
		terminal.Phase = TransferPhaseInterrupted
		terminal.Error = localTransferFailureError(terminal.Output, err)
	default:
		terminal.Status = LocalTransferFailed
		terminal.Phase = TransferPhaseFailed
		terminal.Error = localTransferFailureError(terminal.Output, err)
	}
	event = localTransferEvent(terminal, finished)
	m.mu.Unlock()
	m.dispatchLocalTransferEvent(id, event, true)

	m.mu.Lock()
	job, ok = m.jobs[id]
	if ok {
		job.Status = terminal.Status
		job.Phase = terminal.Phase
		job.Percent = terminal.Percent
		job.Error = terminal.Error
		job.FinishedAt = terminal.FinishedAt
	}
	m.mu.Unlock()
}

func localTransferFailureError(output string, err error) string {
	detail := strings.TrimSpace(output)
	cause := ""
	if err != nil {
		cause = strings.TrimSpace(err.Error())
	}
	switch {
	case detail == "":
		return cause
	case cause == "":
		return detail
	case detail == cause, strings.Contains(detail, cause):
		return detail
	case strings.Contains(cause, detail):
		return cause
	default:
		return detail + "\n" + cause
	}
}

func (m *LocalTransferJobManager) appendOutput(id, output string) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	job.Output += output
	if len(job.Output) > localTransferOutputLimit {
		job.Output = job.Output[len(job.Output)-localTransferOutputLimit:]
	}
	if raw, ok := parseRsyncProgress(job.Output); ok {
		phase, progress := mapRsyncProgress(raw, false, job.ProgressMode)
		if progress > job.Percent || phase == TransferPhaseFinalizing && job.Phase != TransferPhaseFinalizing {
			job.Phase = phase
			job.Percent = progress
		}
	}
	events := job.milestoneEvents(m.now())
	m.mu.Unlock()
	for _, event := range events {
		m.dispatchLocalTransferEvent(id, event, false)
	}
}

func (j *LocalTransferJob) milestoneEvents(now time.Time) []LocalTransferEvent {
	var events []LocalTransferEvent
	for _, milestone := range []int{10, 25, 50, 75, 90, 99} {
		if j.Percent < milestone {
			continue
		}
		phase := TransferPhaseTransferring
		if milestone == 99 {
			phase = TransferPhaseFinalizing
		}
		key := transferMilestoneKey(phase, milestone)
		if j.emittedMilestones[key] {
			continue
		}
		j.emittedMilestones[key] = true
		event := localTransferEvent(*j, now)
		event.Phase = phase
		event.Percent = milestone
		events = append(events, event)
	}
	return events
}

func transferMilestoneKey(phase string, percent int) string {
	return fmt.Sprintf("%s:%d", phase, percent)
}

func localTransferEvent(job LocalTransferJob, now time.Time) LocalTransferEvent {
	elapsed := time.Duration(0)
	if job.StartedAt != nil {
		elapsed = now.Sub(*job.StartedAt)
		if elapsed < 0 {
			elapsed = 0
		}
	}
	return LocalTransferEvent{
		TransferID:   job.TransferID,
		LocalJobID:   job.ID,
		Profile:      job.Profile,
		Direction:    job.Direction,
		Status:       job.Status,
		Phase:        job.Phase,
		Percent:      job.Percent,
		ProgressMode: job.ProgressMode,
		Elapsed:      elapsed,
		Error:        job.Error,
	}
}

func normalizeLocalTransferProgressMode(mode string) string {
	if mode == LocalTransferProgressTotal {
		return LocalTransferProgressTotal
	}
	return LocalTransferProgressEstimated
}

func (m *LocalTransferJobManager) runCallbackDispatcher(id string, callback func(LocalTransferEvent), events <-chan localTransferCallbackDispatch) {
	for dispatch := range events {
		panicked := func() (panicked bool) {
			defer func() {
				if recover() != nil {
					panicked = true
				}
			}()
			callback(dispatch.event)
			return false
		}()
		if panicked {
			m.setLocalTransferCallbackWarning(id, localTransferCallbackPanicWarning)
		}
		if dispatch.done != nil {
			close(dispatch.done)
		}
		if dispatch.terminal {
			return
		}
	}
}

func (m *LocalTransferJobManager) dispatchLocalTransferEvent(id string, event LocalTransferEvent, terminal bool) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	var events chan localTransferCallbackDispatch
	if ok {
		events = job.callbackEvents
	}
	m.mu.Unlock()
	if events == nil {
		return
	}
	dispatch := localTransferCallbackDispatch{event: event, terminal: terminal}
	if !terminal {
		select {
		case events <- dispatch:
		default:
			m.setLocalTransferCallbackWarning(id, localTransferCallbackQueueWarning)
		}
		return
	}
	dispatch.done = make(chan struct{})
	timeout := m.terminalCallbackTimeout
	if timeout <= 0 {
		timeout = terminalCallbackTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case events <- dispatch:
	case <-timer.C:
		m.setLocalTransferCallbackWarning(id, localTransferCallbackTimeoutWarning)
		return
	}
	select {
	case <-dispatch.done:
	case <-timer.C:
		m.setLocalTransferCallbackWarning(id, localTransferCallbackTimeoutWarning)
	}
}

func (m *LocalTransferJobManager) setLocalTransferCallbackWarning(id, warning string) {
	m.mu.Lock()
	if job := m.jobs[id]; job != nil {
		job.CallbackWarning = warning
	}
	m.mu.Unlock()
}

func (m *LocalTransferJobManager) cleanupLocked() {
	cutoff := m.now().Add(-m.retention)
	for id, job := range m.jobs {
		if job.Active() || job.FinishedAt == nil {
			continue
		}
		if job.FinishedAt.Before(cutoff) {
			delete(m.jobs, id)
		}
	}
}

func parseRsyncProgress(output string) (int, bool) {
	toCheckMatches := rsyncToCheckPattern.FindAllStringSubmatch(output, -1)
	if len(toCheckMatches) > 0 {
		match := toCheckMatches[len(toCheckMatches)-1]
		remaining, errRemaining := strconv.Atoi(match[1])
		total, errTotal := strconv.Atoi(match[2])
		if errRemaining == nil && errTotal == nil && total > 0 {
			return clampRsyncProgress((total - remaining) * 100 / total), true
		}
		return 0, false
	}
	percentMatches := rsyncPercentPattern.FindAllStringSubmatch(output, -1)
	if len(percentMatches) == 0 {
		return 0, false
	}
	progress, err := strconv.Atoi(percentMatches[len(percentMatches)-1][1])
	if err != nil {
		return 0, false
	}
	return clampRsyncProgress(progress), true
}

func clampRsyncProgress(progress int) int {
	if progress > 100 {
		progress = 100
	}
	if progress < 0 {
		progress = 0
	}
	return progress
}

func mapRsyncProgress(raw int, processDone bool, mode ...string) (phase string, displayed int) {
	raw = clampRsyncProgress(raw)
	if processDone {
		return TransferPhaseSucceeded, 100
	}
	if len(mode) > 0 && mode[0] == LocalTransferProgressTotal {
		switch {
		case raw == 0:
			return TransferPhasePreparing, 0
		case raw >= 99:
			return TransferPhaseFinalizing, 99
		default:
			return TransferPhaseTransferring, raw
		}
	}
	switch {
	case raw == 0:
		return TransferPhasePreparing, 0
	case raw >= 100:
		return TransferPhaseFinalizing, 99
	default:
		displayed = (raw*95 + 49) / 99
		if displayed < 1 {
			displayed = 1
		}
		if displayed > 95 {
			displayed = 95
		}
		return TransferPhaseTransferring, displayed
	}
}
