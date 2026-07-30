package connectmac

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const DefaultLogDir = "~/.connectmac/logs"

type LogManager struct {
	Dir string
	Now func() time.Time
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
	Operation        string `json:"operation,omitempty"`
	Source           string `json:"source,omitempty"`
	Phase            string `json:"phase,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
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
	return LogManager{Dir: dir, Now: time.Now}
}

func (m LogManager) normalize() LogManager {
	if m.Dir == "" {
		m.Dir = DefaultLogDir
	}
	if m.Now == nil {
		m.Now = time.Now
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
	if err := m.Clean(30 * 24 * time.Hour); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "cm-"+m.Now().Format("2006-01-02")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []LogFile{}, nil
		}
		return nil, err
	}
	files := []LogFile{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
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

func (m LogManager) Clean(retention time.Duration) error {
	if retention <= 0 {
		retention = 30 * 24 * time.Hour
	}
	files, err := m.List()
	if err != nil {
		return err
	}
	cutoff := m.normalize().Now().Add(-retention)
	for _, file := range files {
		if file.ModTime.Before(cutoff) {
			if err := os.Remove(file.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (m LogManager) Export(dest string, retention time.Duration) (string, error) {
	m = m.normalize()
	if retention <= 0 {
		retention = 30 * 24 * time.Hour
	}
	if err := m.Clean(retention); err != nil {
		return "", err
	}
	files, err := m.List()
	if err != nil {
		return "", err
	}
	cutoff := m.Now().Add(-retention)
	if dest == "" {
		dest = fmt.Sprintf("connectmac-logs-%s.zip", m.Now().Format("20060102-150405"))
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()
	for _, file := range files {
		if file.ModTime.Before(cutoff) {
			continue
		}
		if err := addLogFileToZip(zw, file); err != nil {
			return "", err
		}
	}
	return dest, nil
}

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
	text = logURLCredentialPattern.ReplaceAllString(text, "${1}[REDACTED]${2}")
	text = logJSONSensitivePattern.ReplaceAllString(text, `${1}"[REDACTED]"`)
	text = logSensitiveQueryPattern.ReplaceAllString(text, "${1}[REDACTED]")
	text = logAWSAssignmentPattern.ReplaceAllString(text, "${1}${2}[REDACTED]")
	text = logSensitiveAssignmentPattern.ReplaceAllString(text, "${1}${2}[REDACTED]")
	text = logAWSAccessKeyPattern.ReplaceAllString(text, "[REDACTED AWS ACCESS KEY]")
	if len(text) > 4000 {
		text = text[:4000]
	}
	return text
}

const logSensitiveKeyPattern = `access_token|client_secret|aws_access_key_id|aws_secret_access_key|aws_session_token|awsaccesskeyid|secretaccesskey|sessiontoken|password|token|secret|session|cookie`

var (
	logPEMBlockPattern = regexp.MustCompile(
		`(?s)-----BEGIN [^-\r\n]+-----.*?(?:-----END [^-\r\n]+-----|$)`,
	)
	logAuthorizationPattern = regexp.MustCompile(
		`(?i)(authorization[ \t]*:[ \t]*)[^\r\n]*`,
	)
	logCookieHeaderPattern = regexp.MustCompile(
		`(?i)((?:set-cookie|cookie)[ \t]*:[ \t]*)[^\r\n]*`,
	)
	logURLCredentialPattern = regexp.MustCompile(
		`(?i)([a-z][a-z0-9+.-]*://[^\s/:@]+:)[^\s/@]+(@)`,
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
	logAWSAccessKeyPattern = regexp.MustCompile(
		`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`,
	)
)

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
	entry.Operation = sanitizeLogText(entry.Operation)
	entry.Source = sanitizeLogText(entry.Source)
	entry.Phase = sanitizeLogText(entry.Phase)
	entry.ErrorCode = sanitizeLogText(entry.ErrorCode)
	entry.Message = sanitizeLogText(entry.Message)
	return entry
}
