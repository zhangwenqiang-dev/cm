package connectmac

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	LocalTransferQueued      = "queued"
	LocalTransferRunning     = "running"
	LocalTransferSucceeded   = "succeeded"
	LocalTransferFailed      = "failed"
	LocalTransferCanceled    = "canceled"
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
	ID                 string     `json:"id"`
	TransferID         string     `json:"transfer_id,omitempty"`
	Profile            string     `json:"profile"`
	Direction          string     `json:"direction"`
	Status             string     `json:"status"`
	Phase              string     `json:"phase"`
	Percent            int        `json:"percent"`
	BytesTransferred   int64      `json:"bytes_transferred,omitempty"`
	BytesTotal         int64      `json:"bytes_total,omitempty"`
	BytesPerSecond     int64      `json:"bytes_per_second,omitempty"`
	ETASeconds         int64      `json:"eta_seconds,omitempty"`
	ProgressMode       string     `json:"progress_mode,omitempty"`
	Output             string     `json:"output"`
	Error              string     `json:"error"`
	CallbackWarning    string     `json:"callback_warning,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	StartedAt          *time.Time `json:"started_at"`
	FinishedAt         *time.Time `json:"finished_at"`
	callbackEvents     chan localTransferCallbackDispatch
	progressBuffer     string
	bytesTotalReliable bool
}

func (j LocalTransferJob) Active() bool {
	return j.Status == LocalTransferQueued || j.Status == LocalTransferRunning
}

type LocalTransferEvent struct {
	TransferID       string
	LocalJobID       string
	Profile          string
	Direction        string
	Status           string
	Phase            string
	Percent          int
	BytesTransferred int64 `json:"bytes_transferred,omitempty"`
	BytesTotal       int64 `json:"bytes_total,omitempty"`
	BytesPerSecond   int64 `json:"bytes_per_second,omitempty"`
	ETASeconds       int64 `json:"eta_seconds,omitempty"`
	ProgressMode     string
	Elapsed          time.Duration
	Error            string
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
		ID:           fmt.Sprintf("transfer-%d-%d", created.UnixNano(), m.sequence),
		TransferID:   transferID,
		Profile:      profile,
		Direction:    direction,
		Status:       LocalTransferQueued,
		Phase:        TransferPhasePreparing,
		ProgressMode: normalizeLocalTransferProgressMode(progressMode),
		CreatedAt:    created,
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
		normalizeSuccessfulTransferMetrics(&terminal)
	case errors.Is(err, context.Canceled):
		terminal.Status = LocalTransferCanceled
		terminal.Phase = TransferPhaseInterrupted
		terminal.Error = localTransferFailureError(terminal.Output, err)
	case errors.Is(err, context.DeadlineExceeded):
		terminal.Status = LocalTransferInterrupted
		terminal.Phase = TransferPhaseInterrupted
		terminal.Error = localTransferFailureError(terminal.Output, err)
	case isSignalTerminatedTransfer(err):
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
		job.BytesTransferred = terminal.BytesTransferred
		job.BytesTotal = terminal.BytesTotal
		job.BytesPerSecond = terminal.BytesPerSecond
		job.ETASeconds = terminal.ETASeconds
		job.bytesTotalReliable = terminal.bytesTotalReliable
		job.Error = terminal.Error
		job.FinishedAt = terminal.FinishedAt
	}
	m.mu.Unlock()
}

func isSignalTerminatedTransfer(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ProcessState == nil {
		return false
	}
	status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return false
	}
	signal := status.Signal()
	return signal == os.Kill || signal == syscall.SIGTERM
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
	job.progressBuffer += output
	if len(job.progressBuffer) > localTransferOutputLimit {
		job.progressBuffer = job.progressBuffer[len(job.progressBuffer)-localTransferOutputLimit:]
	}
	previousPhase, previousPercent := job.Phase, job.Percent
	if progress, ok := parseRsyncProgressRecord(job.progressBuffer); ok && job.ProgressMode == LocalTransferProgressTotal {
		if progress.BytesTransferred >= job.BytesTransferred {
			job.BytesTransferred = progress.BytesTransferred
		}
		job.BytesTotal = progress.BytesTotal
		job.bytesTotalReliable = progress.BytesTotalReliable
		job.BytesPerSecond = progress.BytesPerSecond
		job.ETASeconds = progress.ETASeconds
		phase, displayed := mapRsyncProgress(progress.Percent, false, job.ProgressMode)
		if displayed > job.Percent {
			job.Percent = displayed
		}
		if phase == TransferPhaseFinalizing || job.Phase == TransferPhasePreparing && phase == TransferPhaseTransferring {
			job.Phase = phase
		}
	} else if raw, ok := parseRsyncProgress(job.progressBuffer); ok {
		phase, displayed := mapRsyncProgress(raw, false, job.ProgressMode)
		if displayed > job.Percent {
			job.Percent = displayed
		}
		if phase == TransferPhaseFinalizing || job.Phase == TransferPhasePreparing && phase == TransferPhaseTransferring {
			job.Phase = phase
		}
	}
	var event *LocalTransferEvent
	if job.Percent > previousPercent || job.Phase != previousPhase {
		value := localTransferEvent(*job, m.now())
		event = &value
	}
	m.mu.Unlock()
	if event != nil {
		m.dispatchLocalTransferEvent(id, *event, false)
	}
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
		TransferID:       job.TransferID,
		LocalJobID:       job.ID,
		Profile:          job.Profile,
		Direction:        job.Direction,
		Status:           job.Status,
		Phase:            job.Phase,
		Percent:          job.Percent,
		BytesTransferred: job.BytesTransferred,
		BytesTotal:       job.BytesTotal,
		BytesPerSecond:   job.BytesPerSecond,
		ETASeconds:       job.ETASeconds,
		ProgressMode:     job.ProgressMode,
		Elapsed:          elapsed,
		Error:            job.Error,
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
	if progress, ok := parseRsyncProgressRecord(output); ok {
		return progress.Percent, true
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

type rsyncProgressRecord struct {
	Percent            int
	BytesTransferred   int64
	BytesTotal         int64
	BytesPerSecond     int64
	ETASeconds         int64
	BytesTotalReliable bool
}

var rsyncProgress2Pattern = regexp.MustCompile(`([0-9][0-9 ,.]*?)\s+(\d{1,3})%\s+([0-9]+(?:[.,][0-9]+)?)\s*([kKMGT]?B)/s\s+(\d+):(\d{2}):(\d{2})`)

func parseRsyncProgressRecord(output string) (rsyncProgressRecord, bool) {
	output = strings.ReplaceAll(output, "\u00a0", " ")
	matches := rsyncProgress2Pattern.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return rsyncProgressRecord{}, false
	}
	match := matches[len(matches)-1]
	bytesTransferred, err := parseGroupedInt64(match[1])
	if err != nil {
		return rsyncProgressRecord{}, false
	}
	percent, err := strconv.Atoi(match[2])
	if err != nil {
		return rsyncProgressRecord{}, false
	}
	rate, err := parseRsyncRate(match[3], match[4])
	if err != nil {
		return rsyncProgressRecord{}, false
	}
	hours, errHours := strconv.ParseInt(match[5], 10, 64)
	minutes, errMinutes := strconv.ParseInt(match[6], 10, 64)
	seconds, errSeconds := strconv.ParseInt(match[7], 10, 64)
	if errHours != nil || errMinutes != nil || errSeconds != nil || minutes > 59 || seconds > 59 {
		return rsyncProgressRecord{}, false
	}
	percent = clampRsyncProgress(percent)
	total, totalReliable := int64(0), false
	if percent == 100 {
		total = bytesTransferred
		totalReliable = true
	} else if percent > 0 {
		total, _ = estimateRsyncTotal(bytesTransferred, percent)
	}
	eta, ok := safeETASeconds(hours, minutes, seconds)
	if !ok {
		return rsyncProgressRecord{}, false
	}
	return rsyncProgressRecord{
		Percent: percent, BytesTransferred: bytesTransferred, BytesTotal: total,
		BytesPerSecond: rate, ETASeconds: eta, BytesTotalReliable: totalReliable,
	}, true
}

func estimateRsyncTotal(transferred int64, percent int) (int64, bool) {
	if transferred < 0 || percent <= 0 || percent >= 100 {
		return 0, false
	}
	divisor := int64(percent)
	quotient, remainder := transferred/divisor, transferred%divisor
	if quotient > math.MaxInt64/100 {
		return 0, false
	}
	base := quotient * 100
	extra := (remainder*100 + divisor/2) / divisor
	if base > math.MaxInt64-extra {
		return 0, false
	}
	return base + extra, true
}

func safeETASeconds(hours, minutes, seconds int64) (int64, bool) {
	if hours < 0 || minutes < 0 || minutes > 59 || seconds < 0 || seconds > 59 {
		return 0, false
	}
	tail := minutes*60 + seconds
	if hours > (math.MaxInt64-tail)/3600 {
		return 0, false
	}
	return hours*3600 + tail, true
}

func normalizeSuccessfulTransferMetrics(job *LocalTransferJob) {
	job.ETASeconds = 0
	if job.BytesTransferred <= 0 {
		if !job.bytesTotalReliable {
			job.BytesTotal = 0
		}
		return
	}
	if job.bytesTotalReliable && job.BytesTotal > 0 {
		job.BytesTransferred = job.BytesTotal
		return
	}
	// A rounded percentage only provides an estimate. Successful process exit makes
	// the latest transferred byte count the consistent terminal total.
	job.BytesTotal = job.BytesTransferred
	job.bytesTotalReliable = true
}

func parseGroupedInt64(value string) (int64, error) {
	value = strings.NewReplacer(" ", "", ",", "", ".", "").Replace(strings.TrimSpace(value))
	return strconv.ParseInt(value, 10, 64)
}

func parseRsyncRate(value, unit string) (int64, error) {
	number, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", "."), 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return 0, fmt.Errorf("invalid rsync rate %q", value)
	}
	multiplier := float64(1)
	switch strings.ToUpper(unit) {
	case "KB":
		multiplier = 1000
	case "MB":
		multiplier = 1000 * 1000
	case "GB":
		multiplier = 1000 * 1000 * 1000
	case "TB":
		multiplier = 1000 * 1000 * 1000 * 1000
	}
	scaled := number * multiplier
	// float64(math.MaxInt64) rounds up to 2^63, which is exactly the first
	// value that cannot be represented by int64.
	const int64UpperBound = float64(1 << 63)
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled < 0 || scaled >= int64UpperBound {
		return 0, fmt.Errorf("rsync rate %q%s/s overflows int64", value, unit)
	}
	return int64(scaled), nil
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
