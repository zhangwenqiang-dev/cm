package connectmac

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultLogDir = "~/.connectmac/logs"

const maxStructuredLogLineBytes = 1024 * 1024

const (
	defaultMaxStructuredBytes int64 = 10 * 1024 * 1024
	defaultMaxGenerations           = 3
	maxRawExportBytes         int64 = 5 * 1024 * 1024
	maxRawExportLineBytes           = 1024 * 1024
	maxPEMLookbehindBytes     int64 = 64 * 1024
	maxExportJobEntries             = 1000
	logRedactionPolicyVersion       = "1"
)

var logWriteMu sync.Mutex

type stagedLogFile struct {
	Original string `json:"original"`
	Staged   string `json:"staged"`
	Target   string `json:"target,omitempty"`
}

type logRotationTransaction struct {
	Phase string          `json:"phase"`
	Files []stagedLogFile `json:"files"`
}

type LogManager struct {
	Dir                        string
	Now                        func() time.Time
	MaxStructuredBytes         int64
	MaxGenerations             int
	Rename                     func(string, string) error
	Remove                     func(string) error
	BeforeStructuredExportRead func()
	BeforeRawExportRead        func()
}

type LogExportOptions struct {
	Destination string
	Retention   time.Duration
	IncludeRaw  bool
	CMVersion   string
	JobRoots    []string
}

type logExportManifest struct {
	ExportedAt             string   `json:"exported_at"`
	IncludeRaw             bool     `json:"include_raw"`
	Categories             []string `json:"categories"`
	PayloadFileCount       int      `json:"payload_file_count"`
	RedactionPolicyVersion string   `json:"redaction_policy_version"`
	CMVersion              string   `json:"cm_version,omitempty"`
}

type LogEntry struct {
	Time             string `json:"time"`
	Level            string `json:"level"`
	Action           string `json:"action"`
	Profile          string `json:"profile,omitempty"`
	TunnelAction     string `json:"tunnel_action,omitempty"`
	PID              int    `json:"pid,omitempty"`
	LocalPorts       []int  `json:"local_ports,omitempty"`
	LaunchResult     string `json:"launch_result,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	AppleEmail       string `json:"apple_email,omitempty"`
	MemberEmail      string `json:"member_email,omitempty"`
	ActorMemberID    string `json:"actor_member_id,omitempty"`
	ActorMemberEmail string `json:"actor_member_email,omitempty"`
	ActorMemberName  string `json:"actor_member_name,omitempty"`
	TransferID       string `json:"transfer_id,omitempty"`
	LocalJobID       string `json:"local_job_id,omitempty"`
	Direction        string `json:"direction,omitempty"`
	Status           string `json:"status,omitempty"`
	Percent          int    `json:"percent,omitempty"`
	ElapsedMS        int64  `json:"elapsed_ms,omitempty"`
	DurationMS       int64  `json:"duration_ms,omitempty"`
	Region           string `json:"region,omitempty"`
	AWSProfile       string `json:"aws_profile,omitempty"`
	RequestID        string `json:"request_id,omitempty"`
	JobID            string `json:"job_id,omitempty"`
	CycleID          string `json:"cycle_id,omitempty"`
	SessionIDHash    string `json:"session_id_hash,omitempty"`
	Operation        string `json:"operation,omitempty"`
	Source           string `json:"source,omitempty"`
	Phase            string `json:"phase,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
	ExitCode         int    `json:"exit_code,omitempty"`
	FailureStage     string `json:"failure_stage,omitempty"`
	Attempt          int    `json:"attempt,omitempty"`
	HTTPStatus       int    `json:"http_status,omitempty"`
	Message          string `json:"message"`
}

type LogFile struct {
	Path    string
	Name    string
	ModTime time.Time
	Size    int64
}

func NewLogManager(dir string) LogManager {
	return LogManager{Dir: dir, Now: time.Now, MaxStructuredBytes: defaultMaxStructuredBytes, MaxGenerations: defaultMaxGenerations, Rename: os.Rename, Remove: os.Remove}
}

func (m LogManager) normalize() LogManager {
	if m.Dir == "" {
		m.Dir = DefaultLogDir
	}
	if m.Now == nil {
		m.Now = time.Now
	}
	if m.MaxStructuredBytes <= 0 {
		m.MaxStructuredBytes = defaultMaxStructuredBytes
	}
	if m.MaxGenerations <= 0 {
		m.MaxGenerations = defaultMaxGenerations
	}
	if m.Rename == nil {
		m.Rename = os.Rename
	}
	if m.Remove == nil {
		m.Remove = os.Remove
	}
	return m
}

func (m LogManager) Write(entry LogEntry) error {
	m = m.normalize()
	if entry.Level == "" {
		entry.Level = "info"
	}
	if entry.Time == "" {
		entry.Time = m.Now().Format(time.RFC3339)
	}
	entry = sanitizeLogEntry(entry)
	dir, err := ExpandPath(m.Dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logWriteMu.Lock()
	defer logWriteMu.Unlock()
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(realDir, ".structured-log.lock")
	return withFileLock(lockPath, func() error {
		if err := recoverRotationsInDir(realDir, m.Rename, m.Remove, structuredLogExportName); err != nil {
			return err
		}
		if err := m.cleanUnlocked(realDir, 30*24*time.Hour); err != nil {
			return err
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		line := append(data, '\n')
		path := filepath.Join(realDir, "cm-"+m.Now().Format("2006-01-02")+".log")
		if err := rotateLogIfNeeded(path, int64(len(line)), m.MaxStructuredBytes, m.MaxGenerations, m.Rename, m.Remove); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(line); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
}

func rotateLogIfNeeded(path string, incoming, maxBytes int64, generations int, rename func(string, string) error, remove func(string) error) error {
	if err := recoverLogRotation(path, rename, remove); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && info.Size()+incoming <= maxBytes) {
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
			return fmt.Errorf("refuse non-regular structured log %s", filepath.Base(path))
		}
		return nil
	}
	if err != nil {
		return err
	}
	if generations < 1 {
		generations = 1
	}
	staged := make([]stagedLogFile, 0, generations+1)
	for generation := 0; generation <= generations; generation++ {
		original := path
		if generation > 0 {
			original = fmt.Sprintf("%s.%d", path, generation)
		}
		generationInfo, err := os.Lstat(original)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			rollbackStagedRotation(staged, rename)
			return err
		}
		if generationInfo.Mode()&os.ModeSymlink != 0 || !generationInfo.Mode().IsRegular() {
			rollbackStagedRotation(staged, rename)
			return fmt.Errorf("refuse non-regular structured log generation %s", filepath.Base(original))
		}
		target := ""
		if generation < generations {
			target = fmt.Sprintf("%s.%d", path, generation+1)
		}
		item := stagedLogFile{Original: original, Staged: fmt.Sprintf("%s.rotate-stage.%d", path, generation), Target: target}
		staged = append(staged, item)
	}
	transaction := logRotationTransaction{Phase: "staging", Files: staged}
	if err := writeRotationTransaction(path, transaction); err != nil {
		return err
	}
	stagedCount := 0
	for _, item := range staged {
		if err := rename(item.Original, item.Staged); err != nil {
			rollbackStagedRotation(staged[:stagedCount], rename)
			_ = remove(rotationMetadataPath(path))
			return fmt.Errorf("stage log rotation: %w", err)
		}
		stagedCount++
	}
	transaction.Phase = "publishing"
	if err := writeRotationTransaction(path, transaction); err != nil {
		rollbackStagedRotation(staged, rename)
		_ = remove(rotationMetadataPath(path))
		return err
	}
	published := make([]stagedLogFile, 0, len(staged))
	for i := len(staged) - 1; i >= 0; i-- {
		item := staged[i]
		if item.Target == "" {
			continue
		}
		if err := rename(item.Staged, item.Target); err != nil {
			for j := len(published) - 1; j >= 0; j-- {
				_ = rename(published[j].Target, published[j].Staged)
			}
			rollbackStagedRotation(staged, rename)
			_ = remove(rotationMetadataPath(path))
			return fmt.Errorf("publish log rotation: %w", err)
		}
		published = append(published, item)
	}
	for _, item := range staged {
		if item.Target == "" {
			if err := remove(item.Staged); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove expired log generation: %w", err)
			}
		}
	}
	return removeIfExists(rotationMetadataPath(path), remove)
}

func rollbackStagedRotation(staged []stagedLogFile, rename func(string, string) error) {
	for i := len(staged) - 1; i >= 0; i-- {
		if _, err := os.Lstat(staged[i].Staged); err == nil {
			_ = rename(staged[i].Staged, staged[i].Original)
		}
	}
}

func rotationMetadataPath(path string) string { return path + ".rotate-meta.json" }

func writeRotationTransaction(path string, transaction logRotationTransaction) error {
	data, err := json.Marshal(transaction)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".rotate-meta-*.tmp")
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
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, rotationMetadataPath(path))
}

func recoverLogRotation(path string, rename func(string, string) error, remove func(string) error) error {
	data, err := os.ReadFile(rotationMetadataPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return recoverLegacyRotationStages(path, rename)
	}
	if err != nil {
		return err
	}
	var transaction logRotationTransaction
	if err := json.Unmarshal(data, &transaction); err != nil {
		return fmt.Errorf("parse log rotation transaction: %w", err)
	}
	switch transaction.Phase {
	case "staging":
		rollbackStagedRotation(transaction.Files, rename)
	case "publishing":
		for i := len(transaction.Files) - 1; i >= 0; i-- {
			item := transaction.Files[i]
			if _, err := os.Lstat(item.Staged); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if item.Target == "" {
				if err := removeIfExists(item.Staged, remove); err != nil {
					return err
				}
			} else if err := rename(item.Staged, item.Target); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown log rotation phase %q", transaction.Phase)
	}
	return removeIfExists(rotationMetadataPath(path), remove)
}

func recoverLegacyRotationStages(path string, rename func(string, string) error) error {
	matches, err := filepath.Glob(path + ".rotate-*")
	if err != nil {
		return err
	}
	for _, staged := range matches {
		if strings.Contains(staged, ".rotate-stage.") || strings.HasSuffix(staged, ".rotate-meta.json") {
			continue
		}
		lastDot := strings.LastIndexByte(staged, '.')
		if lastDot < 0 {
			continue
		}
		generation, err := strconv.Atoi(staged[lastDot+1:])
		if err != nil {
			continue
		}
		original := path
		if generation > 0 {
			original = fmt.Sprintf("%s.%d", path, generation)
		}
		if _, err := os.Lstat(original); errors.Is(err, os.ErrNotExist) {
			if err := rename(staged, original); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeIfExists(path string, remove func(string) error) error {
	if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m LogManager) List() ([]LogFile, error) {
	m = m.normalize()
	dir, err := ExpandPath(m.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	logWriteMu.Lock()
	defer logWriteMu.Unlock()
	var files []LogFile
	err = withFileLock(filepath.Join(realDir, ".structured-log.lock"), func() error {
		if err := recoverRotationsInDir(realDir, m.Rename, m.Remove, structuredLogExportName); err != nil {
			return err
		}
		files, err = listLogFilesUnlocked(realDir)
		return err
	})
	return files, err
}

func (m LogManager) withStructuredLogLock(fn func(realDir string) error) error {
	dir, err := ExpandPath(m.Dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	logWriteMu.Lock()
	defer logWriteMu.Unlock()
	return withFileLock(filepath.Join(realDir, ".structured-log.lock"), func() error {
		if err := recoverRotationsInDir(realDir, m.Rename, m.Remove, structuredLogExportName); err != nil {
			return err
		}
		return fn(realDir)
	})
}

func listLogFilesUnlocked(dir string) ([]LogFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []LogFile{}, nil
		}
		return nil, err
	}
	files := []LogFile{}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".rotate-") {
			continue
		}
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".log") && !strings.Contains(entry.Name(), ".log.")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		files = append(files, LogFile{
			Path:    filepath.Join(dir, entry.Name()),
			Name:    entry.Name(),
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})
	return files, nil
}

func recoverRotationsInDir(dir string, rename func(string, string) error, remove func(string) error, allowed func(string) bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	bases := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		base := ""
		if strings.HasSuffix(name, ".rotate-meta.json") {
			base = strings.TrimSuffix(name, ".rotate-meta.json")
		} else if index := strings.Index(name, ".rotate-"); index > 0 {
			base = name[:index]
		}
		if base == "" {
			continue
		}
		if !allowed(base) {
			continue
		}
		bases[base] = struct{}{}
	}
	for base := range bases {
		if err := recoverLogRotation(filepath.Join(dir, base), rename, remove); err != nil {
			return err
		}
	}
	return nil
}

// ReadSince reads recent structured JSONL entries. Non-JSON and legacy entries
// without a usable timestamp are ignored so raw or partial logs cannot prevent
// local-agent recovery.
func (m LogManager) ReadSince(cutoff time.Time) ([]LogEntry, error) {
	m = m.normalize()
	files, err := m.List()
	if err != nil {
		return nil, err
	}
	entries := make([]LogEntry, 0)
	for _, file := range files {
		if !strings.HasPrefix(file.Name, "cm-") {
			continue
		}
		f, _, err := openRegularFileNoSymlink(file.Path)
		if err != nil {
			return nil, err
		}
		err = readBoundedLogLines(f, maxStructuredLogLineBytes, func(line []byte) {
			var entry LogEntry
			if err := json.Unmarshal(line, &entry); err != nil {
				return
			}
			createdAt, err := time.Parse(time.RFC3339Nano, entry.Time)
			if err != nil || createdAt.Before(cutoff) {
				return
			}
			entries = append(entries, entry)
		})
		closeErr := f.Close()
		if err != nil {
			return nil, fmt.Errorf("read structured log %s: %w", file.Name, err)
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return entries, nil
}

func readBoundedLogLines(r io.Reader, maxLineBytes int, visit func([]byte)) error {
	if maxLineBytes <= 0 {
		maxLineBytes = maxStructuredLogLineBytes
	}
	reader := bufio.NewReaderSize(r, 64*1024)
	line := make([]byte, 0, 64*1024)
	oversized := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if !oversized {
			if len(line)+len(fragment) > maxLineBytes {
				line = line[:0]
				oversized = true
			} else {
				line = append(line, fragment...)
			}
		}
		switch err {
		case nil:
			if !oversized {
				visit(line)
			}
			line = line[:0]
			oversized = false
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if !oversized && len(line) > 0 {
				visit(line)
			}
			return nil
		default:
			return err
		}
	}
}

func (m LogManager) ReconcileInterruptedTransfers(reason string) error {
	m = m.normalize()
	entries, err := m.ReadSince(m.Now().Add(-localTransferRetention))
	if err != nil {
		return err
	}
	open := make(map[string]LogEntry)
	for _, entry := range entries {
		if entry.TransferID == "" {
			continue
		}
		switch {
		case entry.Action == "transfer.local.started":
			open[entry.TransferID] = entry
		case isLocalTransferTerminalEntry(entry):
			delete(open, entry.TransferID)
		}
	}
	if strings.TrimSpace(reason) == "" {
		reason = "local agent restarted"
	}
	for _, started := range open {
		if err := m.Write(LogEntry{
			Level: "warn", Action: "transfer.local.interrupted", Outcome: "failure",
			TransferID: started.TransferID, LocalJobID: started.LocalJobID,
			Profile: started.Profile, Direction: started.Direction,
			Status: LocalTransferInterrupted, Phase: TransferPhaseInterrupted,
			RequestID: started.RequestID, Source: "local-agent-recovery",
			ErrorCode: "agent_restarted", Message: reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

func isLocalTransferTerminalEntry(entry LogEntry) bool {
	switch entry.Action {
	case "transfer.local.succeeded", "transfer.local.failed", "transfer.local.canceled", "transfer.local.interrupted":
		return true
	}
	switch entry.Status {
	case LocalTransferSucceeded, LocalTransferFailed, LocalTransferCanceled, LocalTransferInterrupted:
		return true
	default:
		return false
	}
}

func (m LogManager) Clean(retention time.Duration) error {
	m = m.normalize()
	if retention <= 0 {
		retention = 30 * 24 * time.Hour
	}
	dir, err := ExpandPath(m.Dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	logWriteMu.Lock()
	defer logWriteMu.Unlock()
	return withFileLock(filepath.Join(realDir, ".structured-log.lock"), func() error {
		if err := recoverRotationsInDir(realDir, m.Rename, m.Remove, structuredLogExportName); err != nil {
			return err
		}
		return m.cleanUnlocked(realDir, retention)
	})
}

func (m LogManager) cleanUnlocked(dir string, retention time.Duration) error {
	files, err := listLogFilesUnlocked(dir)
	if err != nil {
		return err
	}
	cutoff := m.Now().Add(-retention)
	for _, file := range files {
		if file.ModTime.Before(cutoff) {
			if err := m.Remove(file.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (m LogManager) Export(dest string, retention time.Duration) (string, error) {
	return m.ExportWithOptions(LogExportOptions{Destination: dest, Retention: retention})
}

func (m LogManager) ExportWithOptions(options LogExportOptions) (string, error) {
	m = m.normalize()
	if options.Retention <= 0 {
		options.Retention = 30 * 24 * time.Hour
	}
	cutoff := m.Now().Add(-options.Retention)
	dest := options.Destination
	if dest == "" {
		dest = fmt.Sprintf("connectmac-logs-%s.zip", m.Now().Format("20060102-150405"))
	}
	dest, err := filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(filepath.Dir(dest), ".connectmac-logs-*.zip.tmp")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return "", err
	}
	zw := zip.NewWriter(temp)
	fileCount := 0
	err = m.withStructuredLogLock(func(realDir string) error {
		if err := m.cleanUnlocked(realDir, options.Retention); err != nil {
			return err
		}
		files, err := listLogFilesUnlocked(realDir)
		if err != nil {
			return err
		}
		if m.BeforeStructuredExportRead != nil {
			m.BeforeStructuredExportRead()
		}
		for _, file := range files {
			if file.ModTime.Before(cutoff) || !structuredLogExportName(file.Name) {
				continue
			}
			if err := addStructuredLogToZip(zw, file); err != nil {
				return err
			}
			fileCount++
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	categories := []string{"structured"}
	if options.IncludeRaw {
		categories = append(categories, "raw")
		err := withRawLogLock(mustExpandLogDir(m.Dir), func(realDir string) error {
			if m.BeforeRawExportRead != nil {
				m.BeforeRawExportRead()
			}
			for _, name := range []string{"local-agent.out.log", "local-agent.err.log", "local-agent.out.log.1", "local-agent.err.log.1"} {
				path := filepath.Join(realDir, name)
				if added, err := addSanitizedRawLogToZip(zw, path, filepath.Join("raw", name)); err != nil {
					return err
				} else if added {
					fileCount++
				}
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		for _, root := range options.JobRoots {
			added, err := addJobRunLogsToZip(zw, root, cutoff)
			if err != nil {
				return "", err
			}
			fileCount += added
		}
	}
	manifest := logExportManifest{ExportedAt: m.Now().Format(time.RFC3339), IncludeRaw: options.IncludeRaw, Categories: categories, PayloadFileCount: fileCount, RedactionPolicyVersion: logRedactionPolicyVersion, CMVersion: strings.TrimSpace(options.CMVersion)}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	w, err := zw.Create("manifest.json")
	if err != nil {
		return "", err
	}
	if _, err := w.Write(append(manifestData, '\n')); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := temp.Sync(); err != nil {
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := m.Rename(tempPath, dest); err != nil {
		return "", err
	}
	committed = true
	return dest, nil
}

var structuredLogExportPattern = regexp.MustCompile(`^cm-\d{4}-\d{2}-\d{2}\.log(?:\.\d+)?$`)

func structuredLogExportName(name string) bool { return structuredLogExportPattern.MatchString(name) }

func mustExpandLogDir(dir string) string {
	expanded, err := ExpandPath(dir)
	if err != nil {
		return dir
	}
	return expanded
}

func addSanitizedRawLogToZip(zw *zip.Writer, path, archiveName string) (bool, error) {
	f, info, err := openRegularFileNoSymlink(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	start := int64(0)
	initialPEM := false
	if info.Size() > maxRawExportBytes {
		start = info.Size() - maxRawExportBytes
		var boundaryFound bool
		initialPEM, boundaryFound, err = pemStateBeforeOffsetStatus(f, start)
		if err != nil {
			return false, err
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return false, err
		}
		consumed, found, err := discardThroughNextNewline(f)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		start += consumed
		if !boundaryFound && !initialPEM {
			initialPEM, err = startsWithPEMContinuation(f, start, info.Size()-start)
			if err != nil {
				return false, err
			}
		}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return false, err
	}
	w, err := zw.Create(filepath.ToSlash(archiveName))
	if err != nil {
		return false, err
	}
	remaining := info.Size() - start
	if remaining > maxRawExportBytes {
		remaining = maxRawExportBytes
	}
	return true, streamSanitizedRaw(io.LimitReader(f, remaining), w, initialPEM)
}

func openRegularFileNoSymlink(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("refuse non-regular log source %s", filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("refuse non-regular opened log source %s", filepath.Base(path))
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) || !os.SameFile(after, opened) {
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("log source changed while opening %s", filepath.Base(path))
	}
	return f, opened, nil
}

func pemStateBeforeOffset(r io.ReaderAt, offset int64) (bool, error) {
	inPEM, _, err := pemStateBeforeOffsetStatus(r, offset)
	return inPEM, err
}

func pemStateBeforeOffsetStatus(r io.ReaderAt, offset int64) (bool, bool, error) {
	if offset <= 0 {
		return false, false, nil
	}
	start := offset - maxPEMLookbehindBytes
	if start < 0 {
		start = 0
	}
	window := make([]byte, offset-start)
	if _, err := r.ReadAt(window, start); err != nil && !errors.Is(err, io.EOF) {
		return false, false, err
	}
	begin := bytes.LastIndex(window, []byte("-----BEGIN "))
	end := bytes.LastIndex(window, []byte("-----END "))
	if begin < 0 && end < 0 {
		return false, false, nil
	}
	return begin > end, true, nil
}

func startsWithPEMContinuation(f *os.File, start, available int64) (bool, error) {
	if available <= 0 {
		return false, nil
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return false, err
	}
	limit := available
	if limit > maxRawExportLineBytes+1 {
		limit = maxRawExportLineBytes + 1
	}
	reader := bufio.NewReaderSize(io.LimitReader(f, limit), 64*1024)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	line = strings.TrimSpace(line)
	return exportBase64LinePattern.MatchString(line), nil
}

func discardThroughNextNewline(r io.Reader) (int64, bool, error) {
	reader := bufio.NewReaderSize(r, 64*1024)
	var consumed int64
	for {
		fragment, err := reader.ReadSlice('\n')
		consumed += int64(len(fragment))
		switch err {
		case nil:
			return consumed, true, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			return consumed, false, nil
		default:
			return consumed, false, err
		}
	}
}

type rawExportSanitizer struct{ inPEM bool }

func (s *rawExportSanitizer) sanitize(line string) (string, bool) {
	begin := strings.Contains(line, "-----BEGIN ")
	end := strings.Contains(line, "-----END ")
	if end && !s.inPEM && !begin {
		return "", false
	}
	if s.inPEM || begin {
		wasInPEM := s.inPEM
		s.inPEM = !end
		return "[REDACTED PEM BLOCK]", !wasInPEM
	}
	return sanitizeExportLine(line), true
}

func streamSanitizedRaw(r io.Reader, w io.Writer, initialPEM bool) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	sanitizer := &rawExportSanitizer{inPEM: initialPEM}
	line := make([]byte, 0, 64*1024)
	oversized := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if !oversized && len(line)+len(fragment) <= maxRawExportLineBytes {
			line = append(line, fragment...)
		} else {
			oversized = true
		}
		if oversized {
			if bytes.Contains(fragment, []byte("-----BEGIN ")) {
				sanitizer.inPEM = true
			}
			if bytes.Contains(fragment, []byte("-----END ")) {
				sanitizer.inPEM = false
			}
		}
		switch err {
		case nil:
			if oversized {
				if _, writeErr := fmt.Fprintln(w, "[REDACTED OVERSIZED LINE]"); writeErr != nil {
					return writeErr
				}
			} else if sanitized, emit := sanitizer.sanitize(strings.TrimSuffix(string(line), "\n")); emit {
				if _, writeErr := fmt.Fprintln(w, sanitized); writeErr != nil {
					return writeErr
				}
			}
			line = line[:0]
			oversized = false
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			// A final unterminated raw line may still be in flight. Drop it rather than
			// exporting an unsafe partial fragment.
			return nil
		default:
			return err
		}
	}
}

func addJobRunLogsToZip(zw *zip.Writer, root string, cutoff time.Time) (int, error) {
	expanded, err := ExpandPath(root)
	if err != nil {
		return 0, err
	}
	rootInfo, err := os.Lstat(expanded)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(expanded)
	if err != nil {
		return 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if len(entries) > maxExportJobEntries {
		entries = entries[len(entries)-maxExportJobEntries:]
	}
	added := 0
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !safeArchiveComponent(entry.Name()) {
			continue
		}
		jobDir := filepath.Join(expanded, entry.Name())
		jobInfo, err := os.Lstat(jobDir)
		if err != nil || !jobInfo.IsDir() || jobInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(jobDir, "run.log")
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return added, err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.ModTime().Before(cutoff) {
			continue
		}
		ok, err := addSanitizedRawLogToZip(zw, path, filepath.Join("raw", "jobs", entry.Name(), "run.log"))
		if err != nil {
			return added, err
		}
		if ok {
			added++
		}
	}
	return added, nil
}

func safeArchiveComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !strings.ContainsAny(value, `/\\`)
}

var (
	exportHomePathPattern    = regexp.MustCompile(`(?:/Users|/home)/[^/\s]+`)
	exportIPv4Pattern        = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	exportHostnamePattern    = regexp.MustCompile(`\b(?:ec2-[a-zA-Z0-9-]+\.(?:compute|amazonaws)\S*|[a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)+)\b`)
	exportFingerprintPattern = regexp.MustCompile(`\b(?:SHA256:[A-Za-z0-9+/=]+|(?:[0-9a-fA-F]{2}:){15,}[0-9a-fA-F]{2})\b`)
	exportGitHubTokenPattern = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
	exportJWTPattern         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	exportSSHKeyPattern      = regexp.MustCompile(`\b(?:ssh-rsa|ssh-ed25519|ecdsa-sha2-[A-Za-z0-9-]+)[ \t]+[A-Za-z0-9+/=]{20,}(?:[ \t]+[^\r\n]+)?`)
	exportLongAPIKeyPattern  = regexp.MustCompile(`\b[A-Za-z0-9_-]{40,}\b`)
	exportBase64LinePattern  = regexp.MustCompile(`^[A-Za-z0-9+/=]{40,}$`)
)

func sanitizeExportLine(line string) string {
	line = sanitizeOperationalErrorText(line)
	if exportBase64LinePattern.MatchString(strings.TrimSpace(line)) {
		return "[REDACTED HIGH-ENTROPY LINE]"
	}
	line = exportSSHKeyPattern.ReplaceAllString(line, "[REDACTED SSH PUBLIC KEY]")
	line = exportGitHubTokenPattern.ReplaceAllString(line, "[REDACTED TOKEN]")
	line = exportJWTPattern.ReplaceAllString(line, "[REDACTED JWT]")
	line = exportLongAPIKeyPattern.ReplaceAllString(line, "[REDACTED API KEY]")
	line = exportHomePathPattern.ReplaceAllString(line, "[REDACTED HOME]")
	line = exportFingerprintPattern.ReplaceAllString(line, "[REDACTED FINGERPRINT]")
	line = exportIPv4Pattern.ReplaceAllString(line, "[REDACTED IP]")
	line = exportHostnamePattern.ReplaceAllString(line, "[REDACTED HOST]")
	return line
}

func rotateLocalAgentRawLogs(logDir string) error {
	return withRawLogLock(logDir, func(realDir string) error {
		for _, name := range []string{"local-agent.out.log", "local-agent.err.log"} {
			path := filepath.Join(realDir, name)
			if err := rotateLogIfNeeded(path, 0, 1024*1024, 1, os.Rename, os.Remove); err != nil {
				return err
			}
		}
		return nil
	})
}

func recoverRawLogRotations(logDir string) error {
	return withRawLogLock(logDir, func(string) error { return nil })
}

func withRawLogLock(logDir string, fn func(realDir string) error) error {
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	realDir, err := filepath.EvalSymlinks(logDir)
	if err != nil {
		return err
	}
	logWriteMu.Lock()
	defer logWriteMu.Unlock()
	return withFileLock(filepath.Join(realDir, ".raw-log.lock"), func() error {
		if err := recoverRotationsInDir(realDir, os.Rename, os.Remove, func(base string) bool {
			return base == "local-agent.out.log" || base == "local-agent.err.log"
		}); err != nil {
			return err
		}
		return fn(realDir)
	})
}

func addStructuredLogToZip(zw *zip.Writer, file LogFile) error {
	in, _, err := openRegularFileNoSymlink(file.Path)
	if err != nil {
		return err
	}
	defer in.Close()
	header := &zip.FileHeader{Name: file.Name, Method: zip.Deflate}
	header.SetModTime(file.ModTime)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	var writeErr error
	readErr := readBoundedLogLines(in, maxStructuredLogLineBytes, func(line []byte) {
		if writeErr != nil {
			return
		}
		clean := sanitizeStructuredExportLine(line)
		_, writeErr = w.Write(append(clean, '\n'))
	})
	if readErr != nil {
		return readErr
	}
	return writeErr
}

func sanitizeStructuredExportLine(line []byte) []byte {
	var value any
	if err := json.Unmarshal(line, &value); err != nil {
		fallback, _ := json.Marshal(LogEntry{Level: "warn", Action: "log.export.malformed", Message: sanitizeExportLine(string(line))})
		return fallback
	}
	value = sanitizeStructuredValue("", value)
	clean, err := json.Marshal(value)
	if err != nil {
		fallback, _ := json.Marshal(LogEntry{Level: "warn", Action: "log.export.malformed", Message: "structured log entry could not be exported"})
		return fallback
	}
	return clean
}

func sanitizeStructuredValue(key string, value any) any {
	if sensitiveExportKey(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case string:
		return sanitizeExportLine(typed)
	case []any:
		for i := range typed {
			typed[i] = sanitizeStructuredValue("", typed[i])
		}
		return typed
	case map[string]any:
		for childKey, child := range typed {
			typed[childKey] = sanitizeStructuredValue(childKey, child)
		}
		return typed
	default:
		return value
	}
}

func sensitiveExportKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return sensitiveExportKeyPattern.MatchString(key)
}

var sensitiveExportKeyPattern = regexp.MustCompile(`(?:password|token|secret|session|cookie|authorization|identity_file|private_key|webhook)`)

func addLogFileToZip(zw *zip.Writer, file LogFile) error {
	in, err := os.Open(file.Path)
	if err != nil {
		return err
	}
	defer in.Close()
	header := &zip.FileHeader{
		Name:   file.Name,
		Method: zip.Deflate,
	}
	header.SetModTime(file.ModTime)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, in)
	return err
}

func sanitizeLogText(text string) string {
	text = strings.TrimSpace(text)
	text = logPEMBlockPattern.ReplaceAllString(text, "[REDACTED PEM]")
	text = logAuthorizationPattern.ReplaceAllString(text, "${1}[REDACTED]")
	text = logCookieHeaderPattern.ReplaceAllString(text, "${1}[REDACTED]")
	text = logURLCredentialPattern.ReplaceAllString(text, "${1}[REDACTED]@")
	text = logJSONSensitivePattern.ReplaceAllString(text, `${1}"[REDACTED]"`)
	text = operationalQuotedSensitiveAssignmentPattern.ReplaceAllString(text, `${1}"[REDACTED]"`)
	text = logSensitiveQueryPattern.ReplaceAllString(text, "${1}[REDACTED]")
	text = logAWSAssignmentPattern.ReplaceAllString(text, "${1}${2}[REDACTED]")
	text = logSensitiveAssignmentPattern.ReplaceAllString(text, "${1}${2}[REDACTED]")
	text = operationalSensitiveAssignmentPattern.ReplaceAllString(text, "${1}${2}[REDACTED]")
	text = logWebhookURLPattern.ReplaceAllString(text, "[REDACTED WEBHOOK URL]")
	text = logAWSAccessKeyPattern.ReplaceAllString(text, "[REDACTED AWS ACCESS KEY]")
	text = logPEMPathPattern.ReplaceAllString(text, "[REDACTED PEM PATH]")
	if len(text) > 4000 {
		text = text[:4000]
	}
	return text
}

func sanitizeOperationalError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(sanitizeOperationalErrorText(err.Error()))
}

const logSensitiveKeyPattern = `access_token|client_secret|aws_access_key_id|aws_secret_access_key|aws_session_token|awsaccesskeyid|secretaccesskey|session_token|sessiontoken|pem_path|pem_file|identity_file|private_key_path|password|token|secret|session|cookie`

const operationalSensitiveKeyPattern = logSensitiveKeyPattern + `|access_key|secret_access_key|webhook_key|webhook_url|wechat_webhook_url|key`

var (
	logPEMBlockPattern = regexp.MustCompile(
		`(?s)-----BEGIN [^-\r\n]+-----.*?(?:-----END [^-\r\n]+-----|$)`,
	)
	logPEMPathPattern = regexp.MustCompile(
		`(?i)(?:"[^"\r\n]*\.pem"|'[^'\r\n]*\.pem'|(?:~|/)[^\s,;\)\]\}]+\.pem)`,
	)
	logWebhookURLPattern = regexp.MustCompile(
		`(?i)https?://[^\s"'<>]*(?:[?&]key=)[^\s"'<>]*`,
	)
	logAuthorizationPattern = regexp.MustCompile(
		`(?i)(authorization[ \t]*:[ \t]*)[^\r\n]*`,
	)
	operationalBearerTokenPattern = regexp.MustCompile(
		`(?i)\b(bearer[ \t]+)[^\s,;]+`,
	)
	logCookieHeaderPattern = regexp.MustCompile(
		`(?i)((?:set-cookie|cookie)[ \t]*:[ \t]*)[^\r\n]*`,
	)
	logURLCredentialPattern = regexp.MustCompile(
		`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]+@`,
	)
	logJSONSensitivePattern = regexp.MustCompile(
		`(?i)("(?:` + logSensitiveKeyPattern + `|x-amz-credential|x-amz-security-token|x-amz-signature)"[ \t]*:[ \t]*)"(?:\\.|[^"\\])*"`,
	)
	logSensitiveQueryPattern = regexp.MustCompile(
		`(?i)([?&](?:key|` + logSensitiveKeyPattern + `|x-amz-credential|x-amz-security-token|x-amz-signature)=)[^&#\s]+`,
	)
	logAWSAssignmentPattern = regexp.MustCompile(
		`(?i)\b(aws_access_key_id|aws_secret_access_key|aws_session_token)([ \t]*[:=][ \t]*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&]+)`,
	)
	logSensitiveAssignmentPattern = regexp.MustCompile(
		`(?i)(^|[?&;,\s])((?:` + logSensitiveKeyPattern + `)(?:[ \t]*[:=][ \t]*|[ \t]+))(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&]+)`,
	)
	operationalSensitiveAssignmentPattern = regexp.MustCompile(
		`(?i)(^|[?&;,\s"'])((?:` + operationalSensitiveKeyPattern + `)[ \t]*[:=][ \t]*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&]+)`,
	)
	operationalQuotedSensitiveAssignmentPattern = regexp.MustCompile(
		`(?i)(["'](?:` + operationalSensitiveKeyPattern + `)["'][ \t]*[:=][ \t]*)(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;\}\]]+)`,
	)
	logAWSAccessKeyPattern = regexp.MustCompile(
		`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`,
	)
)

func sanitizeOperationalErrorText(text string) string {
	text = strings.TrimSpace(text)
	text = redactWechatWebhookURL(text)
	text = logPEMBlockPattern.ReplaceAllString(text, "[REDACTED PEM]")
	text = logAuthorizationPattern.ReplaceAllString(text, "${1}[REDACTED]")
	text = logCookieHeaderPattern.ReplaceAllString(text, "${1}[REDACTED]")
	text = logURLCredentialPattern.ReplaceAllString(text, "${1}[REDACTED]@")
	text = operationalBearerTokenPattern.ReplaceAllString(text, "${1}[REDACTED]")
	text = logJSONSensitivePattern.ReplaceAllString(text, `${1}"[REDACTED]"`)
	text = operationalQuotedSensitiveAssignmentPattern.ReplaceAllString(text, `${1}"[REDACTED]"`)
	text = logSensitiveQueryPattern.ReplaceAllString(text, "${1}[REDACTED]")
	text = logAWSAssignmentPattern.ReplaceAllString(text, "${1}${2}[REDACTED]")
	text = operationalSensitiveAssignmentPattern.ReplaceAllString(text, "${1}${2}[REDACTED]")
	text = logWebhookURLPattern.ReplaceAllString(text, "[REDACTED WEBHOOK URL]")
	text = logAWSAccessKeyPattern.ReplaceAllString(text, "[REDACTED AWS ACCESS KEY]")
	text = logPEMPathPattern.ReplaceAllString(text, "[REDACTED PEM PATH]")
	if len(text) > 4000 {
		text = text[:4000]
	}
	return text
}

func sanitizeLogEntry(entry LogEntry) LogEntry {
	entry.Time = sanitizeLogText(entry.Time)
	entry.Level = sanitizeLogText(entry.Level)
	entry.Action = sanitizeLogText(entry.Action)
	entry.Profile = sanitizeLogText(entry.Profile)
	entry.TunnelAction = sanitizeLogText(entry.TunnelAction)
	entry.LaunchResult = sanitizeLogText(entry.LaunchResult)
	entry.Outcome = sanitizeLogText(entry.Outcome)
	entry.AppleEmail = sanitizeLogText(entry.AppleEmail)
	entry.MemberEmail = sanitizeLogText(entry.MemberEmail)
	entry.ActorMemberID = sanitizeLogText(entry.ActorMemberID)
	entry.ActorMemberEmail = sanitizeLogText(entry.ActorMemberEmail)
	entry.ActorMemberName = sanitizeLogText(entry.ActorMemberName)
	entry.TransferID = sanitizeLogText(entry.TransferID)
	entry.LocalJobID = sanitizeLogText(entry.LocalJobID)
	entry.Direction = sanitizeLogText(entry.Direction)
	entry.Status = sanitizeLogText(entry.Status)
	entry.Region = sanitizeLogText(entry.Region)
	entry.AWSProfile = sanitizeLogText(entry.AWSProfile)
	entry.RequestID = sanitizeLogText(entry.RequestID)
	entry.JobID = sanitizeLogText(entry.JobID)
	entry.CycleID = sanitizeLogText(entry.CycleID)
	entry.SessionIDHash = sanitizeLogText(entry.SessionIDHash)
	entry.Operation = sanitizeLogText(entry.Operation)
	entry.Source = sanitizeLogText(entry.Source)
	entry.Phase = sanitizeLogText(entry.Phase)
	entry.ErrorCode = sanitizeLogText(entry.ErrorCode)
	entry.FailureStage = sanitizeLogText(entry.FailureStage)
	entry.Message = sanitizeLogText(entry.Message)
	return entry
}
