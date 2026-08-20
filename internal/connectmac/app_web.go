package connectmac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type webOptions struct {
	Host string
	Port int
	Open bool
	Dir  string
}

type webAPIResponse struct {
	OK     bool        `json:"ok"`
	Code   int         `json:"code,omitempty"`
	Output string      `json:"output,omitempty"`
	Error  string      `json:"error,omitempty"`
	Data   interface{} `json:"data,omitempty"`
}

type webClientConfig struct {
	UserAPI string `json:"user_api"`
}

type webProfile struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	AppleEmail  string         `json:"apple_email"`
	Region      string         `json:"region"`
	AWSProfile  string         `json:"aws_profile"`
	Host        string         `json:"host"`
	Owners      []PublicMember `json:"owners"`
	ProfileYAML string         `json:"profile_yaml,omitempty"`
}

type webAWSStatus struct {
	Profile    string             `json:"profile"`
	AppleEmail string             `json:"apple_email"`
	Region     string             `json:"region"`
	Decision   string             `json:"decision"`
	Detail     string             `json:"detail"`
	Next       string             `json:"next"`
	Ready      bool               `json:"ready"`
	Hosts      []webDedicatedHost `json:"hosts"`
	Instances  []webInstance      `json:"instances"`
	ElasticIP  webElasticIP       `json:"elastic_ip"`
}

type webDedicatedHost struct {
	HostID       string `json:"host_id"`
	State        string `json:"state"`
	InstanceType string `json:"instance_type"`
	ZoneID       string `json:"zone_id"`
	CreatedAt    string `json:"created_at,omitempty"`
}

type webInstance struct {
	InstanceID     string `json:"instance_id"`
	State          string `json:"state"`
	InstanceType   string `json:"instance_type"`
	HostID         string `json:"host_id"`
	PublicIP       string `json:"public_ip"`
	SystemStatus   string `json:"system_status"`
	InstanceStatus string `json:"instance_status"`
	EBSStatus      string `json:"ebs_status"`
	Ready          bool   `json:"ready"`
}

type webElasticIP struct {
	AllocationID  string `json:"allocation_id"`
	AssociationID string `json:"association_id"`
	InstanceID    string `json:"instance_id"`
	PublicIP      string `json:"public_ip"`
}

const (
	webAWSLifecycleScanTimeout = 45 * time.Second
	autoReleaseScanTimeout     = 45 * time.Second
)

type webBackgroundWorkerSchedule struct {
	lifecycleTicks       <-chan time.Time
	reminderTicks        <-chan time.Time
	lifecycleScanTimeout time.Duration
	autoReleaseTimeout   time.Duration
	lifecycleScan        func(context.Context, string) error
	autoReleaseScan      func(context.Context, string, time.Time) error
}

func (a App) runWeb(ctx context.Context, configPath string, args []string) int {
	opts, err := parseWebArgs(args)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 2
	}
	if opts.Dir != "" {
		a.WebDir = opts.Dir
	}
	changedJobs, err := a.JobManager.Reconcile()
	if err != nil {
		fmt.Fprintf(a.Err, "job reconciliation failed: %v\n", err)
		return 1
	}
	for _, job := range changedJobs {
		fmt.Fprintf(a.Out, "Reconciled background job %s: %s\n", job.ID, job.Status)
	}
	if cfg, err := LoadConfig(configPath); err == nil && cfg.Server.UserAPI != "" {
		a.RemoteUserAPI = true
		fmt.Fprintf(a.Out, "ConnectMac user API: %s\n", cfg.Server.UserAPI)
	}
	if store, ok, err := NewMySQLMemberStoreFromEnv(); err != nil {
		fmt.Fprintf(a.Err, "mysql member store failed: %v\n", err)
		return 1
	} else if ok {
		if err := store.EnsureSchema(); err != nil {
			fmt.Fprintf(a.Err, "mysql member schema failed: %v\n", err)
			return 1
		}
		a.MemberStore = store
		fmt.Fprintln(a.Out, "ConnectMac member store: mysql")
	}
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))
	listen := a.Listen
	if listen == nil {
		listen = net.Listen
	}
	listener, err := listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(a.Err, "web server failed: %v\n", err)
		return 1
	}
	if err := a.JobManager.EndDrain(); err != nil {
		_ = listener.Close()
		fmt.Fprintf(a.Err, "job drain cleanup failed: %v\n", err)
		return 1
	}
	handler := a.WebHandler
	if handler == nil {
		handler = a.newWebHandler(configPath)
	}
	actualAddr := listener.Addr().String()
	server := &http.Server{Addr: actualAddr, Handler: handler}
	worker := a.WebReminderWorker
	if worker == nil {
		worker = func(ctx context.Context) { a.runReleaseReminderWorker(ctx, configPath) }
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker(workerCtx)
	}()
	if opts.Open {
		url := "http://" + actualAddr
		if err := a.Runner.OpenURL(ctx, url); err != nil {
			fmt.Fprintf(a.Err, "open browser failed: %v\n", err)
		}
	}
	fmt.Fprintf(a.Out, "ConnectMac web manager: http://%s\n", actualAddr)
	fmt.Fprintln(a.Out, "Press Ctrl+C to stop.")
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(listener)
	}()
	var serveErr error
	serveFinished := false
	select {
	case serveErr = <-serveResult:
		serveFinished = true
	case <-ctx.Done():
	}
	shutdownWebServer(server, webShutdownTimeout(a.WebShutdownTimeout))
	if !serveFinished {
		serveErr = <-serveResult
	}
	cancelWorker()
	workerTimeout := webShutdownTimeout(a.WebWorkerShutdownTimeout)
	if !waitForWebWorker(workerDone, workerTimeout) {
		fmt.Fprintf(a.Err, "warning: reminder worker did not stop within %s\n", workerTimeout)
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		fmt.Fprintf(a.Err, "web server failed: %v\n", serveErr)
		return 1
	}
	return 0
}

func webShutdownTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 5 * time.Second
	}
	return timeout
}

func shutdownWebServer(server *http.Server, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	err := server.Shutdown(ctx)
	cancel()
	if err != nil {
		_ = server.Close()
	}
}

func waitForWebWorker(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (a App) runReleaseReminderWorker(ctx context.Context, configPath string) {
	lifecycleTicker := time.NewTicker(10 * time.Second)
	reminderTicker := time.NewTicker(time.Minute)
	defer lifecycleTicker.Stop()
	defer reminderTicker.Stop()
	a.runWebBackgroundWorker(ctx, configPath, lifecycleTicker.C, reminderTicker.C)
}

func (a App) runWebBackgroundWorker(ctx context.Context, configPath string, lifecycleTicks, reminderTicks <-chan time.Time) {
	a.runWebBackgroundWorkers(ctx, configPath, webBackgroundWorkerSchedule{
		lifecycleTicks: lifecycleTicks,
		reminderTicks:  reminderTicks,
	})
}

func (a App) runWebBackgroundWorkers(ctx context.Context, configPath string, schedule webBackgroundWorkerSchedule) {
	if schedule.lifecycleScanTimeout <= 0 {
		schedule.lifecycleScanTimeout = webAWSLifecycleScanTimeout
	}
	if schedule.autoReleaseTimeout <= 0 {
		schedule.autoReleaseTimeout = autoReleaseScanTimeout
	}
	if schedule.lifecycleScan == nil {
		schedule.lifecycleScan = a.WebAWSLifecycleScan
		if schedule.lifecycleScan == nil {
			schedule.lifecycleScan = func(ctx context.Context, configPath string) error {
				return a.reconcileWebAWSLifecycles(ctx, configPath)
			}
		}
	}
	if schedule.autoReleaseScan == nil {
		schedule.autoReleaseScan = a.scanAutoReleaseReminders
	}

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		a.runWebLifecycleWorker(ctx, configPath, schedule)
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		a.runWebReminderWorker(ctx, configPath, schedule)
	}()
	<-done
	<-done
}

func (a App) runWebLifecycleWorker(ctx context.Context, configPath string, schedule webBackgroundWorkerSchedule) {
	for {
		scanCtx, cancel := context.WithTimeout(ctx, schedule.lifecycleScanTimeout)
		_ = schedule.lifecycleScan(scanCtx, configPath)
		cancel()
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-schedule.lifecycleTicks:
		}
	}
}

func (a App) runWebReminderWorker(ctx context.Context, configPath string, schedule webBackgroundWorkerSchedule) {
	now := time.Now()
	a.pruneWebAuditEvents(now)
	nextAuditPrune := now.Add(24 * time.Hour)
	a.runAutoReleaseScan(ctx, configPath, now, schedule.autoReleaseTimeout, schedule.autoReleaseScan)
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case now := <-schedule.reminderTicks:
			if !now.Before(nextAuditPrune) {
				a.pruneWebAuditEvents(now)
				nextAuditPrune = now.Add(24 * time.Hour)
			}
			a.runAutoReleaseScan(ctx, configPath, now, schedule.autoReleaseTimeout, schedule.autoReleaseScan)
		}
	}
}

func (a App) runAutoReleaseScan(
	ctx context.Context,
	configPath string,
	now time.Time,
	timeout time.Duration,
	scan func(context.Context, string, time.Time) error,
) {
	startedAt := time.Now()
	a.writeRuntimeLog(LogEntry{
		Level:     "info",
		Action:    "auto-release.scan.started",
		Operation: "auto-release",
		Source:    "background-worker",
		Phase:     "started",
		Message:   "automatic release reconciliation scan started",
	})
	scanCtx, cancel := context.WithTimeout(ctx, timeout)
	err := scan(scanCtx, configPath, now)
	scanContextErr := scanCtx.Err()
	cancel()
	durationMS := elapsedDurationMS(startedAt)
	if ctx.Err() != nil {
		return
	}
	if errors.Is(scanContextErr, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		a.writeRuntimeLog(LogEntry{
			Level:      "warn",
			Action:     "auto-release.scan.timeout",
			Operation:  "auto-release",
			Source:     "background-worker",
			Phase:      "timeout",
			DurationMS: durationMS,
			ErrorCode:  classifyOperationalError(context.DeadlineExceeded).Code,
			Message:    context.DeadlineExceeded.Error(),
		})
		return
	}
	entry := LogEntry{
		Level:      "info",
		Action:     "auto-release.scan.completed",
		Operation:  "auto-release",
		Source:     "background-worker",
		Phase:      "completed",
		DurationMS: durationMS,
		Message:    "automatic release reconciliation scan completed",
	}
	if err != nil {
		classified := classifyOperationalError(err)
		entry.Level = classified.Level
		entry.ErrorCode = classified.Code
		entry.Message = err.Error()
	}
	a.writeRuntimeLog(entry)
}

func (a App) pruneWebAuditEvents(now time.Time) {
	removed, err := a.MemberStore.PruneEvents(now.AddDate(0, 0, -eventRetentionDays))
	if err != nil {
		a.writeRuntimeLog(LogEntry{
			Level:     "error",
			Action:    "audit.prune",
			Operation: "audit.retention",
			Source:    "system",
			Phase:     "failed",
			ErrorCode: classifyOperationalError(err).Code,
			Message:   err.Error(),
		})
		return
	}
	if removed == 0 {
		return
	}
	a.writeRuntimeLog(LogEntry{
		Level:     "info",
		Action:    "audit.prune",
		Operation: "audit.retention",
		Source:    "system",
		Phase:     "completed",
		Message:   fmt.Sprintf("deleted %d audit events older than %d days", removed, eventRetentionDays),
	})
}

func (a App) sendDueReleaseReminders(now time.Time) {
	a.advanceAutoReleaseReminders(context.Background(), DefaultConfigPath, now)
}

func (a App) advanceAutoReleaseReminders(ctx context.Context, configPath string, now time.Time) {
	if err := a.scanAutoReleaseReminders(ctx, configPath, now); err != nil {
		a.writeRuntimeLog(LogEntry{Level: "error", Action: "release-reminder.worker", Operation: "auto-release", Source: "background-worker", Phase: "scan", ErrorCode: classifyOperationalError(err).Code, Message: err.Error()})
	}
}

func (a App) scanAutoReleaseReminders(ctx context.Context, configPath string, now time.Time) error {
	coordinator := a.newAutoReleaseCoordinator(configPath)
	coordinator.Now = func() time.Time { return now }
	return coordinator.Scan(ctx)
}

func (a App) newAutoReleaseCoordinator(configPath string) *AutoReleaseCoordinator {
	return &AutoReleaseCoordinator{
		Now:   time.Now,
		Store: a.MemberStore,
		Jobs:  a.JobManager,
		ResolveProfile: func(ctx context.Context, reminder ReleaseReminder) (Profile, error) {
			return a.resolveAutoReleaseProfile(ctx, configPath, reminder)
		},
		Status: func(ctx context.Context, profile Profile) (AWSStatus, error) {
			_, status, err := a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: false})
			return status, classifyAWSAutoReleaseError(err)
		},
		StartDestroy: func(ctx context.Context, profile Profile) (Job, error) {
			runConfigPath, err := writeAutoReleaseProfileConfig(profile)
			if err != nil {
				return Job{}, TerminalAutoReleaseError(err)
			}
			job, _, err := a.startAWSJobForResolvedProfile(ctx, runConfigPath, "destroy", profile, true, runConfigPath)
			return job, err
		},
		Notify: func(notification AutoReleaseNotification) error {
			description := "Mac 释放提醒已到期"
			event := "due"
			switch notification.Kind {
			case AutoReleaseNotificationFirstFailure:
				event, description = "auto-release-failure", "自动释放失败，将按计划重试："+notification.Error
			case AutoReleaseNotificationFinalFailure:
				event, description = "auto-release-failed", "自动释放已停止："+notification.Error
			case AutoReleaseNotificationSuccess:
				event, description = "auto-release-success", "Mac 自动释放成功，Elastic IP 分配已保留"
			}
			return a.notifyReleaseReminder(event, notification.Reminder, "", description)
		},
		Emit: func(event AutoReleaseEvent) {
			level := "info"
			if event.Action == "retrying" || event.Action == "failed" {
				level = "error"
			}
			action := "auto-release." + event.Action
			a.writeRuntimeLog(LogEntry{
				Level: level, Action: action, Operation: "auto-release",
				Profile: event.Reminder.ProfileName, AppleEmail: event.Reminder.AppleEmail,
				Source: "background-worker", Phase: event.Action, Status: event.Action,
				Message: event.Message,
			})
			_ = a.recordEventWithFallback(OperationEvent{
				Action: action, Profile: event.Reminder.ProfileName,
				AppleEmail: event.Reminder.AppleEmail, Source: "background-worker",
				Phase: event.Action, Confirmed: true, Status: event.Action,
				Message: event.Message,
			})
		},
	}
}

func (a App) resolveAutoReleaseProfile(_ context.Context, configPath string, reminder ReleaseReminder) (Profile, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) || !os.IsNotExist(pathErr.Err) {
			return Profile{}, TerminalAutoReleaseError(err)
		}
		cfg = Config{Profiles: map[string]Profile{}}
	}
	records, err := a.MemberStore.ListManagedProfiles("")
	if err != nil {
		return Profile{}, TerminalAutoReleaseError(fmt.Errorf("load managed profiles: %w", err))
	}
	if len(records) > 0 {
		cfg, err = a.mergeManagedProfileRecords(cfg, records)
		if err != nil {
			return Profile{}, TerminalAutoReleaseError(err)
		}
	}
	profile, err := resolveProfileRef(cfg, reminder.ProfileName)
	if err != nil {
		return Profile{}, TerminalAutoReleaseError(err)
	}
	if errs := a.Validator.ValidateAWSProfile(profile); len(errs) > 0 {
		return Profile{}, TerminalAutoReleaseError(errors.New(strings.Join(validationMessages(errs), "\n")))
	}
	return profile, nil
}

func writeAutoReleaseProfileConfig(profile Profile) (string, error) {
	file, err := os.CreateTemp("", "cm-auto-release-config-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create automatic release config: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("secure automatic release config: %w", err)
	}
	_, writeErr := file.WriteString(FormatConfigProfiles(Config{Profiles: map[string]Profile{profile.Name: profile}}))
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(path)
		return "", errors.Join(writeErr, closeErr)
	}
	return path, nil
}

func parseWebArgs(args []string) (webOptions, error) {
	opts := webOptions{Host: "127.0.0.1", Port: 8765}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			i++
			if i >= len(args) || args[i] == "" {
				return opts, fmt.Errorf("--host requires a value")
			}
			opts.Host = args[i]
		case "--port":
			i++
			if i >= len(args) || args[i] == "" {
				return opts, fmt.Errorf("--port requires a value")
			}
			port, err := strconv.Atoi(args[i])
			if err != nil || port < 1 || port > 65535 {
				return opts, fmt.Errorf("--port must be between 1 and 65535")
			}
			opts.Port = port
		case "--open":
			opts.Open = true
		case "--web-dir":
			i++
			if i >= len(args) || args[i] == "" {
				return opts, fmt.Errorf("--web-dir requires a value")
			}
			opts.Dir = args[i]
		case "--help", "-h":
			return opts, fmt.Errorf("usage: cm web [--host 127.0.0.1] [--port 8765] [--open] [--web-dir <path>]")
		default:
			return opts, fmt.Errorf("unknown web option %q", args[i])
		}
	}
	return opts, nil
}

func (a App) newWebHandler(configPath string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		dir, err := a.resolveWebDir()
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
	mux.HandleFunc("/vendor/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		dir, err := a.resolveWebDir()
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.StripPrefix("/vendor/", http.FileServer(http.Dir(filepath.Join(dir, "vendor")))).ServeHTTP(w, r)
	})
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		dir, err := a.resolveWebDir()
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.StripPrefix("/assets/", http.FileServer(http.Dir(filepath.Join(dir, "assets")))).ServeHTTP(w, r)
	})
	mux.HandleFunc("/api/auth/me", a.webAuthMeHandler())
	mux.HandleFunc("/api/auth/challenge", a.webAuthChallengeHandler())
	mux.HandleFunc("/api/auth/setup", a.webAuthSetupHandler(configPath))
	mux.HandleFunc("/api/auth/login", a.webAuthLoginHandler(configPath))
	mux.HandleFunc("/api/auth/logout", a.webAuthLogoutHandler())
	mux.HandleFunc("/api/auth/change-password", a.requireWebRole(a.webAuthChangePasswordHandler(), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/auth/token", a.requireWebRole(a.webAuthTokenHandler(), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/config", a.webConfigHandler(configPath))
	mux.HandleFunc("/api/user-proxy/", a.webUserProxyHandler(configPath))
	mux.HandleFunc("/api/auth/update-email", a.requireWebRole(a.webAuthUpdateEmailHandler(), "admin"))
	mux.HandleFunc("/api/settings", a.requireWebRole(a.webSettingsHandler(), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/profiles", a.requireWebRole(a.webProfilesHandler(configPath), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/members", a.requireWebRole(a.webMembersHandler(), "admin"))
	mux.HandleFunc("/api/member/add", a.requireWebRole(a.webMemberAddHandler(), "admin"))
	mux.HandleFunc("/api/member/update", a.requireWebRole(a.webMemberUpdateHandler(), "admin"))
	mux.HandleFunc("/api/member/password", a.requireWebRole(a.webMemberPasswordHandler(), "admin"))
	mux.HandleFunc("/api/member/token", a.requireWebRole(a.webMemberTokenHandler(), "admin"))
	mux.HandleFunc("/api/member/enable", a.requireWebRole(a.webMemberEnabledHandler(true), "admin"))
	mux.HandleFunc("/api/member/disable", a.requireWebRole(a.webMemberEnabledHandler(false), "admin"))
	mux.HandleFunc("/api/member/assign", a.requireWebRole(a.webMemberAssignHandler(false), "admin"))
	mux.HandleFunc("/api/member/unassign", a.requireWebRole(a.webMemberAssignHandler(true), "admin"))
	mux.HandleFunc("/api/member/profiles", a.requireWebRole(a.webMemberProfilesHandler(), "admin"))
	mux.HandleFunc("/api/profile-owners", a.requireWebRole(a.webProfileOwnersHandler(), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/profile-owner/set", a.requireWebRole(a.webProfileOwnerSetHandler(), "admin"))
	mux.HandleFunc("/api/release-reminders", a.requireWebRole(a.webReleaseRemindersHandler(), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/release-reminder/extend", a.requireWebRole(a.webReleaseReminderExtendHandler(), "operator", "admin"))
	mux.HandleFunc("/api/release-reminder/auto-release", a.requireWebRole(a.webReleaseReminderAutoReleaseHandler(configPath), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/release-reminder/cleanup", a.requireWebRole(a.webReleaseReminderCleanupHandler(), "admin"))
	mux.HandleFunc("/api/managed-profiles", a.requireWebRole(a.webManagedProfilesHandler(), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/managed-profile/save", a.requireWebRole(a.webManagedProfileSaveHandler(), "admin"))
	mux.HandleFunc("/api/managed-profile/status", a.requireWebRole(a.webManagedProfileStatusHandler(), "admin"))
	mux.HandleFunc("/api/managed-profile/delete", a.requireWebRole(a.webManagedProfileDeleteHandler(), "admin"))
	mux.HandleFunc("/api/managed-profile/access", a.requireWebRole(a.webManagedProfileAccessHandler(), "admin"))
	mux.HandleFunc("/api/events", a.requireWebRole(a.webEventsHandler(), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/jobs", a.requireWebRole(a.webJobsHandler(configPath), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/job/log", a.requireWebRole(a.webJobLogHandler(configPath), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/debug/status-config", a.requireWebRole(a.webDebugStatusConfigHandler(configPath), "admin"))
	mux.HandleFunc("/api/aws/status", a.requireWebRole(a.webAWSStatusHandler(configPath), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/aws/open", a.requireWebRole(a.webAWSActionHandler(configPath, "open"), "operator", "admin"))
	mux.HandleFunc("/api/aws/destroy", a.requireWebRole(a.webAWSActionHandler(configPath, "destroy"), "operator", "admin"))
	mux.HandleFunc("/api/tunnel/start", a.requireWebRole(a.webTunnelStartHandler(configPath), "operator", "admin"))
	mux.HandleFunc("/api/terminal/check", a.requireWebRole(a.webTerminalCheckHandler(configPath), "operator", "admin"))
	mux.HandleFunc("/api/terminal/ws", a.requireWebRole(a.webTerminalWSHandler(configPath), "operator", "admin"))
	mux.HandleFunc("/api/sync/history", a.requireWebRole(a.webSyncHistoryHandler(), "admin"))
	mux.HandleFunc("/api/sync/history/delete", a.requireWebRole(a.webSyncHistoryDeleteHandler(), "admin"))
	mux.HandleFunc("/api/sync/push", a.requireWebRole(a.webSyncPushHandler(configPath), "operator", "admin"))
	mux.HandleFunc("/api/sync/pull", a.requireWebRole(a.webSyncPullHandler(configPath), "operator", "admin"))
	mux.HandleFunc("/api/transfer-records", a.requireWebRole(a.webTransferRecordsHandler(), "viewer", "operator", "admin"))
	mux.HandleFunc("/api/transfer-record/start", a.requireWebRole(a.webTransferRecordStartHandler(configPath), "operator", "admin"))
	mux.HandleFunc("/api/transfer-record/update", a.requireWebRole(a.webTransferRecordUpdateHandler(), "operator", "admin"))
	mux.HandleFunc("/api/transfer-record/delete", a.requireWebRole(a.webTransferRecordDeleteHandler(), "operator", "admin"))
	mux.HandleFunc("/api/local-intent", a.requireWebRole(a.webLocalIntentHandler(configPath), "operator", "admin"))
	mux.HandleFunc("/api/local/list", a.requireWebRole(a.webLocalListHandler(), "operator", "admin"))
	return a.withWebObservability(mux)
}

func (a App) webUserProxyHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg, err := LoadConfig(configPath)
		if err != nil || cfg.Server.UserAPI == "" {
			writeWebError(w, http.StatusBadGateway, "remote user api is not configured")
			return
		}
		remotePath := strings.TrimPrefix(r.URL.Path, "/api/user-proxy")
		if !isRemoteUserAPIPath(remotePath) {
			writeWebError(w, http.StatusForbidden, "path is not allowed")
			return
		}
		target := strings.TrimRight(cfg.Server.UserAPI, "/") + remotePath
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
		if err != nil {
			writeWebError(w, http.StatusBadGateway, err.Error())
			return
		}
		for _, name := range []string{"Accept", "Content-Type", "Cookie"} {
			if value := r.Header.Get(name); value != "" {
				req.Header.Set(name, value)
			}
		}
		req.Header.Set("X-Forwarded-Proto", "https")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			writeWebError(w, http.StatusBadGateway, err.Error())
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			if strings.EqualFold(key, "Set-Cookie") {
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		copyUserProxySessionCookies(w, resp)
		if isUserProxyLoginSuccess(remotePath, resp.StatusCode) {
			a.cleanupLocalConfigAfterLogin(configPath)
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			a.logProfileError("user-proxy", Profile{}, err.Error())
		}
	}
}

func isUserProxyLoginSuccess(path string, status int) bool {
	return status >= 200 && status < 300 && (path == "/api/auth/login" || path == "/api/auth/setup")
}

func copyUserProxySessionCookies(w http.ResponseWriter, resp *http.Response) {
	for _, cookie := range resp.Cookies() {
		if cookie.Name != webSessionCookie {
			continue
		}
		http.SetCookie(w, &http.Cookie{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Path:     "/",
			Expires:  cookie.Expires,
			MaxAge:   cookie.MaxAge,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

func isRemoteUserAPIPath(path string) bool {
	return strings.HasPrefix(path, "/api/auth/") ||
		path == "/api/members" ||
		strings.HasPrefix(path, "/api/member/") ||
		path == "/api/profile-owners" ||
		strings.HasPrefix(path, "/api/profile-owner/") ||
		path == "/api/managed-profiles" ||
		strings.HasPrefix(path, "/api/managed-profile/") ||
		path == "/api/release-reminders" ||
		path == "/api/release-reminder/auto-release" ||
		path == "/api/settings" ||
		strings.HasPrefix(path, "/api/events")
}

func (a App) webConfigHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		cfg, err := LoadConfig(configPath)
		if err != nil {
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) || !os.IsNotExist(pathErr.Err) {
				writeWebError(w, http.StatusInternalServerError, err.Error())
				return
			}
			cfg = Config{}
		}
		writeWebJSON(w, webAPIResponse{
			OK: true,
			Data: map[string]interface{}{
				"config": webClientConfig{UserAPI: cfg.Server.UserAPI},
			},
		})
	}
}

func (a App) resolveWebDir() (string, error) {
	candidates := []string{}
	if env := os.Getenv("CM_WEB_DIR"); env != "" {
		candidates = append(candidates, env)
	}
	if a.WebDir != "" {
		candidates = append(candidates, a.WebDir)
	}
	if executable, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		binDir := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(binDir, "..", "share", "cm", "web"),
			filepath.Join(binDir, "..", "web"),
		)
	}
	candidates = append(candidates, "web")
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		expanded, err := ExpandPath(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(filepath.Join(expanded, "index.html"))
		if err == nil && !info.IsDir() {
			return expanded, nil
		}
	}
	return "", errors.New("web assets not found; set CM_WEB_DIR or install cm through Homebrew")
}

func (a App) webProfilesHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		cfg, err := a.loadWebConfig(r, configPath)
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		profiles := make([]webProfile, 0, len(cfg.Profiles))
		for _, name := range sortedProfileNames(cfg) {
			profile, _ := cfg.Profile(name)
			owners := []PublicMember{}
			if owner, ok, _ := a.MemberStore.ProfileOwner(profile.Name); ok {
				owners = []PublicMember{owner.Owner}
			}
			profiles = append(profiles, webProfile{
				Name:        profile.Name,
				Description: profile.Description,
				AppleEmail:  profile.AWS.AccountEmail,
				Region:      profile.AWS.Region,
				AWSProfile:  profile.AWS.Profile,
				Host:        profile.Host,
				Owners:      owners,
				ProfileYAML: FormatProfileFile(profile),
			})
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profiles": profiles}})
	}
}

func (a App) loadWebConfig(r *http.Request, configPath string) (Config, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) || !os.IsNotExist(pathErr.Err) {
			return Config{}, err
		}
		cfg = Config{Profiles: map[string]Profile{}}
	}
	if strings.TrimSpace(cfg.Server.UserAPI) == "" {
		member, ok := a.currentWebMember(r)
		memberEmail := ""
		if ok {
			memberEmail = member.Email
		}
		records, err := a.MemberStore.ListManagedProfiles(memberEmail)
		if err != nil {
			return cfg, nil
		}
		if len(records) == 0 {
			return cfg, nil
		}
		return a.mergeManagedProfileRecords(cfg, records)
	}
	remoteProfiles, err := a.fetchRemoteManagedProfiles(r, cfg.Server.UserAPI)
	if err != nil {
		return cfg, nil
	}
	if len(remoteProfiles) == 0 {
		return Config{Profiles: map[string]Profile{}}, nil
	}
	records := make([]ManagedProfile, 0, len(remoteProfiles))
	for _, remote := range remoteProfiles {
		records = append(records, ManagedProfile{Name: remote.Name, ProfileYAML: remote.ProfileYAML})
	}
	return a.mergeManagedProfileRecords(cfg, records)
}

func (a App) mergeManagedProfileRecords(cfg Config, records []ManagedProfile) (Config, error) {
	merged := Config{Profiles: map[string]Profile{}}
	merged.Defaults = cfg.Defaults
	for _, record := range records {
		profile, err := ParseSingleProfileYAML(record.ProfileYAML)
		if err != nil {
			return Config{}, fmt.Errorf("parse managed profile %s: %w", record.Name, err)
		}
		if local, ok := cfg.Profile(profile.Name); ok {
			applyLocalPrivateProfileFields(&profile, local)
		}
		if profile.IdentityFile == "" {
			profile.IdentityFile = cfg.Defaults.IdentityFile
		}
		merged.Profiles[profile.Name] = profile
	}
	merged.ApplyDefaults()
	return merged, nil
}

func (a App) fetchRemoteManagedProfiles(r *http.Request, userAPI string) ([]webManagedProfile, error) {
	target := strings.TrimRight(userAPI, "/") + "/api/managed-profiles?include_yaml=1"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if cookie := r.Header.Get("Cookie"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Data  struct {
			Profiles []webManagedProfile `json:"profiles"`
		} `json:"data"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if decodeErr == nil && strings.TrimSpace(body.Error) != "" {
			return nil, fmt.Errorf("remote profile request failed: %s: %s", resp.Status, body.Error)
		}
		return nil, fmt.Errorf("remote profile request failed: %s", resp.Status)
	}
	if decodeErr != nil {
		return nil, decodeErr
	}
	if !body.OK {
		if body.Error == "" {
			body.Error = resp.Status
		}
		return nil, errors.New(body.Error)
	}
	return body.Data.Profiles, nil
}

func applyLocalPrivateProfileFields(remote *Profile, local Profile) {
	if local.IdentityFile != "" {
		remote.IdentityFile = local.IdentityFile
	}
	if !syncConfigEmpty(local.Sync) {
		remote.Sync = local.Sync
	}
	if local.VNC.Username != "" {
		remote.VNC = local.VNC
	}
}

func syncConfigEmpty(sync SyncConfig) bool {
	return len(sync.Push.Includes) == 0 &&
		len(sync.Push.Excludes) == 0 &&
		len(sync.Pull.Includes) == 0 &&
		len(sync.Pull.Excludes) == 0
}

func (a App) webMembersHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		members, err := a.MemberStore.ListMembers()
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"members": members}})
	}
}

func (a App) webMemberAddHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Name     string `json:"name"`
			Email    string `json:"email"`
			Role     string `json:"role"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		var member Member
		var err error
		if strings.TrimSpace(req.Password) != "" {
			member, err = a.MemberStore.AddMemberWithPassword(req.Name, req.Email, req.Role, req.Password)
		} else {
			member, err = a.MemberStore.AddMember(req.Name, req.Email, req.Role)
		}
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"member": member}})
	}
}

func (a App) webMemberUpdateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			OriginalEmail string `json:"original_email"`
			Name          string `json:"name"`
			Email         string `json:"email"`
			Role          string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		member, err := a.MemberStore.UpdateMember(req.OriginalEmail, req.Name, req.Email, req.Role)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"member": member}})
	}
}

func (a App) webMemberPasswordHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if strings.TrimSpace(req.Email) == "" {
			writeWebError(w, http.StatusBadRequest, "member email is required")
			return
		}
		if err := a.MemberStore.SetMemberPassword(req.Email, req.Password); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true})
	}
}

func (a App) webMemberTokenHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Email  string `json:"email"`
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		data, err := a.applyWebAPITokenAction(req.Email, req.Action)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: data})
	}
}

func (a App) webMemberEnabledHandler(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		member, err := a.MemberStore.SetMemberEnabled(req.Email, enabled)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"member": member}})
	}
}

func (a App) webMemberAssignHandler(unassign bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			AppleEmail  string `json:"apple_email"`
			MemberEmail string `json:"member_email"`
			Relation    string `json:"relation"`
			Profile     string `json:"profile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if unassign {
			if err := a.MemberStore.UnassignMember(req.AppleEmail, req.MemberEmail); err != nil {
				writeWebError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeWebJSON(w, webAPIResponse{OK: true})
			return
		}
		assignment, err := a.MemberStore.AssignMember(req.AppleEmail, req.MemberEmail, req.Relation)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		data := map[string]interface{}{"assignment": assignment}
		if strings.TrimSpace(req.Profile) != "" {
			owner, err := a.MemberStore.SetProfileOwner(req.Profile, req.MemberEmail)
			if err != nil {
				writeWebError(w, http.StatusBadRequest, err.Error())
				return
			}
			data["profile_owner"] = owner
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: data})
	}
}

func (a App) webMemberProfilesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			MemberEmail string   `json:"member_email"`
			Profiles    []string `json:"profiles"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		access, err := a.MemberStore.SetMemberProfileAccess(req.MemberEmail, req.Profiles)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profile_access": access}})
	}
}

func (a App) webProfileOwnersHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		owners, err := a.MemberStore.ProfileOwners()
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if member.Role != "admin" {
			profiles, err := a.MemberStore.ListManagedProfiles(member.Email)
			if err != nil {
				writeWebError(w, http.StatusInternalServerError, err.Error())
				return
			}
			allowed := make(map[string]struct{}, len(profiles))
			for _, profile := range profiles {
				allowed[profile.Name] = struct{}{}
			}
			filtered := make([]PublicProfileOwner, 0, len(owners))
			for _, owner := range owners {
				if _, ok := allowed[owner.ProfileName]; ok {
					filtered = append(filtered, owner)
				}
			}
			owners = filtered
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"owners": owners}})
	}
}

func (a App) webProfileOwnerSetHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		var req struct {
			Profile     string `json:"profile"`
			MemberEmail string `json:"member_email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		profileName := strings.TrimSpace(req.Profile)
		profiles, err := a.MemberStore.ListManagedProfiles(member.Email)
		if err != nil {
			a.logProfileError("profile-owner.set", Profile{Name: profileName}, err.Error())
			writeWebError(w, http.StatusInternalServerError, "failed to validate profile owner request")
			return
		}
		found := false
		for _, profile := range profiles {
			if profile.Name == profileName {
				found = true
				break
			}
		}
		if !found {
			writeWebError(w, http.StatusBadRequest, fmt.Sprintf("profile %s not found", profileName))
			return
		}
		event := OperationEvent{
			Action:      "profile-owner.set",
			MemberID:    member.ID,
			MemberEmail: member.Email,
			MemberName:  member.Name,
			Confirmed:   true,
			Status:      "success",
			Message:     "profile owner set to " + normalizeEmail(req.MemberEmail),
		}
		owner, err := a.MemberStore.SetProfileOwnerAndRecordEvent(profileName, req.MemberEmail, event)
		if err != nil {
			if errors.Is(err, ErrProfileOwnerValidation) {
				writeWebError(w, http.StatusBadRequest, err.Error())
				return
			}
			a.logProfileError("profile-owner.set", Profile{Name: profileName}, err.Error())
			writeWebError(w, http.StatusInternalServerError, "failed to update profile owner")
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"owner": owner}})
	}
}

func (a App) webReleaseRemindersHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		reminders, err := a.MemberStore.ListReleaseReminders(member.Email)
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"reminders": reminders}})
	}
}

var (
	errAutomaticReleaseRunning = errors.New("automatic release is already running; wait for the release to finish")
	errActiveDestroyJob        = errors.New("automatic release cannot be changed while an active aws-destroy job exists")
	errAutoReleaseMacNotReady  = errors.New("automatic release requires a ready Mac")
	errAutoReleaseOwnerMissing = errors.New("automatic release requires a profile owner")
)

func (a App) webReleaseReminderAutoReleaseHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		var req struct {
			Profile string `json:"profile"`
			Enabled *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		profileName := strings.TrimSpace(req.Profile)
		if profileName == "" {
			writeWebError(w, http.StatusBadRequest, "profile is required")
			return
		}
		if req.Enabled == nil {
			writeWebError(w, http.StatusBadRequest, "enabled is required")
			return
		}
		if err := a.ensureWebMemberProfileAccess(member, profileName); err != nil {
			writeWebError(w, http.StatusForbidden, err.Error())
			return
		}
		enabled := *req.Enabled
		nowTime := a.JobManager.normalize().Now().UTC()
		reactivateReleased := false
		var reactivationOwner PublicMember
		if enabled {
			current, ok, err := a.MemberStore.ReleaseReminder(profileName)
			if err != nil {
				writeReleaseReminderUpdateError(w, err)
				return
			}
			if ok && (current.Status == ReleaseReminderStatusReleased || current.AutoReleaseState == ReleaseReminderAutoReleaseStateReleased) {
				ready, err := a.webAWSProfileReady(r.Context(), r, configPath, profileName)
				if err != nil || !ready {
					writeReleaseReminderUpdateError(w, errAutoReleaseMacNotReady)
					return
				}
				if owner, found, err := a.MemberStore.ProfileOwner(profileName); err != nil {
					writeReleaseReminderUpdateError(w, err)
					return
				} else if found {
					reactivationOwner = owner.Owner
				} else {
					writeReleaseReminderUpdateError(w, errAutoReleaseOwnerMissing)
					return
				}
				reactivateReleased = true
			}
		}
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		event := OperationEvent{
			Action:      "release-reminder.auto-release." + state,
			MemberID:    member.ID,
			MemberEmail: member.Email,
			MemberName:  member.Name,
			Confirmed:   true,
			Status:      "success",
			Message:     "automatic release " + state + " by " + displayNameEmail(member.Name, member.Email),
		}

		var reminder ReleaseReminder
		update := func() error {
			if !enabled {
				active, err := a.JobManager.Active()
				if err != nil {
					return fmt.Errorf("check active jobs: %w", err)
				}
				if hasActiveDestroyJob(active, profileName) {
					return errActiveDestroyJob
				}
			}
			var err error
			reminder, err = a.MemberStore.UpdateReleaseReminderAndRecordEvent(profileName, func(current ReleaseReminder) (ReleaseReminder, error) {
				if enabled {
					if reactivateReleased && (current.Status == ReleaseReminderStatusReleased || current.AutoReleaseState == ReleaseReminderAutoReleaseStateReleased) {
						current.Status = ReleaseReminderStatusActive
						current.ReleasedAt = ""
						current.ReleaseDueAt = nowTime.Add(24 * time.Hour).Format(time.RFC3339)
						if reactivationOwner.Email != "" {
							current.OwnerEmail = reactivationOwner.Email
							current.OwnerName = reactivationOwner.Name
						}
						current = resetAutoReleaseForNewCycle(current)
					}
					current.AutoReleaseEnabled = true
					if current.Status == ReleaseReminderStatusDueNotified && current.AutoReleaseState == "" {
						current.AutoReleaseAt = nowTime.Add(AutoReleaseGracePeriod).Format(time.RFC3339)
						current.AutoReleaseState = ReleaseReminderAutoReleaseStateScheduled
					}
					return current, nil
				}
				if current.AutoReleaseState == ReleaseReminderAutoReleaseStateRunning {
					return current, errAutomaticReleaseRunning
				}
				current.AutoReleaseEnabled = false
				return clearReleaseReminderAutoCycle(current), nil
			}, event)
			return err
		}
		var err error
		if enabled {
			err = update()
		} else {
			err = a.JobManager.WithProfileOperation(profileName, update)
		}
		if err != nil {
			writeReleaseReminderUpdateError(w, err)
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"reminder": reminder}})
	}
}

func (a App) webReleaseReminderExtendHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		var req struct {
			Profile      string `json:"profile"`
			ReleaseDueAt string `json:"release_due_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		profileName := strings.TrimSpace(req.Profile)
		if profileName == "" {
			writeWebError(w, http.StatusBadRequest, "profile is required")
			return
		}
		dueAt, err := time.Parse(time.RFC3339, strings.TrimSpace(req.ReleaseDueAt))
		if err != nil {
			writeWebError(w, http.StatusBadRequest, "release_due_at must be RFC3339")
			return
		}
		nowTime := a.JobManager.normalize().Now().UTC()
		if dueAt.Before(nowTime.Add(AutoReleaseGracePeriod)) {
			writeWebError(w, http.StatusBadRequest, "release_due_at must be at least 10 minutes in the future")
			return
		}
		if err := a.ensureWebMemberProfileAccess(member, profileName); err != nil {
			writeWebError(w, http.StatusForbidden, err.Error())
			return
		}
		oldDueAt := ""
		var reminder ReleaseReminder
		err = a.JobManager.WithProfileOperation(profileName, func() error {
			active, err := a.JobManager.Active()
			if err != nil {
				return fmt.Errorf("check active jobs: %w", err)
			}
			if hasActiveDestroyJob(active, profileName) {
				return errActiveDestroyJob
			}
			var updateErr error
			reminder, updateErr = a.MemberStore.UpdateReleaseReminder(profileName, func(current ReleaseReminder) (ReleaseReminder, error) {
				if current.AutoReleaseState == ReleaseReminderAutoReleaseStateRunning {
					return current, errAutomaticReleaseRunning
				}
				oldDueAt = current.ReleaseDueAt
				return applyReleaseReminderExtension(current, dueAt, nowTime, member.Email, member.Name)
			})
			return updateErr
		})
		if err != nil {
			writeReleaseReminderUpdateError(w, err)
			return
		}
		a.notifyReleaseReminder("extend", reminder, member.Name, "释放提醒已延长（原时间："+formatBeijingDisplayTime(oldDueAt)+"）")
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"reminder": reminder}})
	}
}

func clearReleaseReminderAutoCycle(reminder ReleaseReminder) ReleaseReminder {
	reminder.AutoReleaseAt = ""
	reminder.AutoReleaseStartedAt = ""
	reminder.AutoReleaseLastAttemptAt = ""
	reminder.AutoReleaseAttempts = 0
	reminder.AutoReleaseLastError = ""
	reminder.AutoReleaseState = ""
	return reminder
}

func writeReleaseReminderUpdateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrReleaseReminderNotFound):
		writeWebError(w, http.StatusNotFound, "release reminder not found")
	case errors.Is(err, errAutomaticReleaseRunning), errors.Is(err, errActiveDestroyJob):
		writeWebError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errAutoReleaseMacNotReady), errors.Is(err, errAutoReleaseOwnerMissing):
		writeWebError(w, http.StatusBadRequest, err.Error())
	default:
		writeWebError(w, http.StatusInternalServerError, err.Error())
	}
}

func (a App) webReleaseReminderCleanupHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Profile string `json:"profile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		profileName := strings.TrimSpace(req.Profile)
		if profileName == "" {
			writeWebError(w, http.StatusBadRequest, "profile is required")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		op := a.operationContextForRequest(r)
		reminder, _, err := a.cleanupProfileLocalRecordsWithEvent(profileName, "manual", OperationEvent{
			Action:      "profile.cleanup.completed",
			Profile:     profileName,
			MemberID:    member.ID,
			MemberEmail: member.Email,
			MemberName:  member.Name,
			RequestID:   op.RequestID,
			Source:      "web",
			Phase:       "completed",
			Confirmed:   true,
			Status:      "success",
			Message:     "cleared profile owner and converged release reminder",
		})
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"reminder": reminder}})
	}
}

func (a App) ensureWebMemberProfileAccess(member Member, profileName string) error {
	if member.Role == "admin" {
		return nil
	}
	profiles, err := a.MemberStore.ListManagedProfiles(member.Email)
	if err != nil {
		return err
	}
	for _, profile := range profiles {
		if profile.Name == profileName {
			return nil
		}
	}
	return fmt.Errorf("profile %s is not assigned to %s", profileName, member.Email)
}

type webManagedProfile struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	AppleEmail  string         `json:"apple_email"`
	Region      string         `json:"region"`
	AWSProfile  string         `json:"aws_profile"`
	Host        string         `json:"host"`
	Enabled     bool           `json:"enabled"`
	ProfileYAML string         `json:"profile_yaml,omitempty"`
	Members     []PublicMember `json:"members,omitempty"`
	UpdatedAt   string         `json:"updated_at"`
}

func (a App) webManagedProfilesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		records, err := a.MemberStore.ListManagedProfiles(member.Email)
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		includeYAML := r.URL.Query().Get("include_yaml") == "1"
		items := make([]webManagedProfile, 0, len(records))
		for _, record := range records {
			profile, err := ParseSingleProfileYAML(record.ProfileYAML)
			if err != nil {
				continue
			}
			item := webManagedProfile{
				Name:        profile.Name,
				Description: profile.Description,
				AppleEmail:  profile.AWS.AccountEmail,
				Region:      profile.AWS.Region,
				AWSProfile:  profile.AWS.Profile,
				Host:        profile.Host,
				Enabled:     record.Enabled,
				UpdatedAt:   record.UpdatedAt,
			}
			if includeYAML || member.Role == "admin" {
				item.ProfileYAML = record.ProfileYAML
			}
			if member.Role == "admin" {
				item.Members, _ = a.MemberStore.MembersForProfile(profile.Name)
			}
			items = append(items, item)
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profiles": items}})
	}
}

func (a App) webManagedProfileSaveHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			ProfileYAML string `json:"profile_yaml"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		profile, err := ParseSingleProfileYAML(req.ProfileYAML)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		record, err := a.MemberStore.UpsertManagedProfile(profile)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profile": record}})
	}
}

func (a App) webManagedProfileStatusHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Profile string `json:"profile"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		record, err := a.MemberStore.SetManagedProfileEnabled(req.Profile, req.Enabled)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"profile": record}})
	}
}

func (a App) webManagedProfileDeleteHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Profile string `json:"profile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := a.MemberStore.DeleteManagedProfile(req.Profile); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true})
	}
}

func (a App) webManagedProfileAccessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Profile     string `json:"profile"`
			MemberEmail string `json:"member_email"`
			Grant       bool   `json:"grant"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		var err error
		if req.Grant {
			_, err = a.MemberStore.AssignProfileAccess(req.Profile, req.MemberEmail)
		} else {
			err = a.MemberStore.UnassignProfileAccess(req.Profile, req.MemberEmail)
		}
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true})
	}
}

func (a App) webEventsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		limit := 50
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 200 {
				writeWebError(w, http.StatusBadRequest, "limit must be between 1 and 200")
				return
			}
			limit = value
		}
		query := EventQuery{
			AppleEmail: strings.TrimSpace(r.URL.Query().Get("apple_email")),
			Profile:    strings.TrimSpace(r.URL.Query().Get("profile")),
			Cursor:     strings.TrimSpace(r.URL.Query().Get("cursor")),
			Limit:      limit,
		}
		if member.Role == "admin" {
			query.IncludeSystem = r.URL.Query().Get("include_system") == "1"
		}
		page, err := a.queryWebEventsForMember(member, query)
		if err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "invalid event cursor") {
				status = http.StatusBadRequest
			}
			if errors.Is(err, errWebProfileAccessDenied) {
				status = http.StatusForbidden
			}
			writeWebError(w, status, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{
			"events": page.Events, "next_cursor": page.NextCursor,
		}})
	}
}

var errWebProfileAccessDenied = errors.New("profile access denied")

func (a App) queryWebEventsForMember(member Member, query EventQuery) (EventPage, error) {
	if member.Role == "admin" {
		return a.MemberStore.QueryEvents(query)
	}
	profiles, err := a.MemberStore.ListManagedProfiles(member.Email)
	if err != nil {
		return EventPage{}, err
	}
	allowed := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		allowed[profile.Name] = struct{}{}
	}
	if query.Profile != "" {
		if _, ok := allowed[query.Profile]; !ok {
			return EventPage{}, errWebProfileAccessDenied
		}
	}
	targetCount := query.Limit + 1
	result := EventPage{Events: make([]OperationEvent, 0, targetCount)}
	scan := query
	scan.Limit = 200
	for len(result.Events) < targetCount {
		page, err := a.MemberStore.QueryEvents(scan)
		if err != nil {
			return EventPage{}, err
		}
		for _, event := range page.Events {
			if _, ok := allowed[event.Profile]; !ok {
				continue
			}
			result.Events = append(result.Events, event)
			if len(result.Events) == targetCount {
				break
			}
		}
		if len(result.Events) == targetCount || page.NextCursor == "" {
			break
		}
		scan.Cursor = page.NextCursor
	}
	if len(result.Events) > query.Limit {
		result.Events = result.Events[:query.Limit]
		result.NextCursor = encodeEventCursor(result.Events[len(result.Events)-1])
	}
	return result, nil
}

func (a App) webJobsHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		allowed, all, err := a.webJobProfileAccess(r, configPath)
		if err != nil {
			writeWebError(w, http.StatusForbidden, err.Error())
			return
		}
		jobs, err := a.JobManager.List()
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !all {
			filtered := jobs[:0]
			for _, job := range jobs {
				if _, ok := allowed[job.Profile]; ok {
					filtered = append(filtered, job)
				}
			}
			jobs = filtered
		}
		if jobs == nil {
			jobs = []Job{}
		}
		sort.Slice(jobs, func(i, j int) bool {
			return jobs[i].StartedAt.After(jobs[j].StartedAt)
		})
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"jobs": jobs}})
	}
}

func (a App) webJobProfileAccess(r *http.Request, configPath string) (map[string]struct{}, bool, error) {
	if member, ok := a.currentWebMember(r); ok {
		if member.Role == "admin" {
			return nil, true, nil
		}
		profiles, err := a.MemberStore.ListManagedProfiles(member.Email)
		if err != nil {
			return nil, false, err
		}
		allowed := make(map[string]struct{}, len(profiles))
		for _, profile := range profiles {
			allowed[profile.Name] = struct{}{}
		}
		return allowed, false, nil
	}
	if !a.RemoteUserAPI {
		return nil, false, errors.New("authentication required")
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, false, err
	}
	profiles, err := a.fetchRemoteManagedProfiles(r, cfg.Server.UserAPI)
	if err != nil {
		return nil, false, err
	}
	allowed := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		allowed[profile.Name] = struct{}{}
	}
	return allowed, false, nil
}

func (a App) webDebugStatusConfigHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ref := strings.TrimSpace(r.URL.Query().Get("profile"))
		rawCfg, rawErr := LoadConfig(configPath)
		member, memberOK := a.currentWebMember(r)
		memberEmail := ""
		memberRole := ""
		if memberOK {
			memberEmail = member.Email
			memberRole = member.Role
		}
		managedRecords, managedErr := a.MemberStore.ListManagedProfiles(memberEmail)
		if managedErr != nil && memberEmail != "" {
			managedRecords, managedErr = a.MemberStore.ListManagedProfiles("")
		}
		effectiveCfg, effectiveErr := a.loadWebConfig(r, configPath)
		resolveOK := false
		resolveErr := ""
		if effectiveErr == nil && ref != "" {
			if _, err := resolveProfileRef(effectiveCfg, ref); err != nil {
				resolveErr = err.Error()
			} else {
				resolveOK = true
			}
		}
		data := map[string]interface{}{
			"profile":                     ref,
			"config_path":                 configPath,
			"remote_user_api_mode":        a.RemoteUserAPI,
			"authenticated_member":        memberOK,
			"member_email":                memberEmail,
			"member_role":                 memberRole,
			"managed_profiles_count":      len(managedRecords),
			"managed_profiles":            managedProfileNames(managedRecords),
			"effective_profiles_count":    len(effectiveCfg.Profiles),
			"effective_profiles":          sortedProfileNames(effectiveCfg),
			"resolve_ok":                  resolveOK,
			"resolve_error":               resolveErr,
			"load_config_error":           errorString(rawErr),
			"load_effective_config_error": errorString(effectiveErr),
			"managed_profiles_error":      errorString(managedErr),
		}
		if rawErr == nil {
			data["server_user_api_configured"] = strings.TrimSpace(rawCfg.Server.UserAPI) != ""
			data["local_profiles_count"] = len(rawCfg.Profiles)
			data["local_profiles"] = sortedProfileNames(rawCfg)
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: data})
	}
}

func managedProfileNames(records []ManagedProfile) []string {
	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Name)
	}
	sort.Strings(names)
	return names
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (a App) webAWSStatusHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ref := strings.TrimSpace(r.URL.Query().Get("profile"))
		if ref == "" {
			writeWebError(w, http.StatusBadRequest, "profile is required")
			return
		}
		cfg, err := a.loadWebConfig(r, configPath)
		if err != nil {
			a.logWebAWSStatusError(r, ref, err)
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		profile, err := resolveProfileRef(cfg, ref)
		if err != nil {
			a.logWebAWSStatusError(r, ref, err)
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errs := a.Validator.ValidateAWSProfile(profile); len(errs) > 0 {
			message := fmt.Sprintf("profile %s config error:\n%s", profile.Name, strings.Join(validationMessages(errs), "\n"))
			a.logProfileError("web.aws.status", profile, message)
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: message})
			return
		}
		plan, status, err := a.webAWSStatusWithCleanup(r.Context(), profile)
		if err != nil {
			a.logWebAWSStatusError(r, profile.Name, err)
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: fmt.Sprintf("aws status failed: %v", err)})
			return
		}
		writeWebJSON(w, webAPIResponse{
			OK:     true,
			Code:   0,
			Output: FormatAWSStatus(plan, status),
			Data:   webAWSStatusData(profile, plan, status),
		})
	}
}

func (a App) logWebAWSStatusError(r *http.Request, profile string, err error) {
	classified := classifyOperationalError(err)
	if classified.Skip {
		return
	}
	op := a.operationContextForRequest(r)
	a.writeRuntimeLog(LogEntry{
		Level:            classified.Level,
		Action:           "web.aws.status",
		Operation:        "aws.status",
		Profile:          profile,
		RequestID:        op.RequestID,
		Source:           op.Source,
		Phase:            "failed",
		ErrorCode:        classified.Code,
		MemberEmail:      op.Actor.MemberEmail,
		ActorMemberID:    op.Actor.MemberID,
		ActorMemberEmail: op.Actor.MemberEmail,
		ActorMemberName:  op.Actor.MemberName,
		Message:          err.Error(),
	})
}

func (a App) webAWSActionHandler(configPath, command string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Profile    string `json:"profile"`
			Confirm    bool   `json:"confirm"`
			Background bool   `json:"background"`
			Notify     bool   `json:"notify"`
			OwnerEmail string `json:"owner_email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			a.writeRuntimeLog(LogEntry{Level: "error", Action: "web.aws." + command, Operation: "aws." + command, Source: "web", Phase: "validate", ErrorCode: "validation_error", Message: "invalid json body"})
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		req.Profile = strings.TrimSpace(req.Profile)
		if req.Profile == "" {
			a.writeRuntimeLog(LogEntry{Level: "error", Action: "web.aws." + command, Operation: "aws." + command, Source: "web", Phase: "validate", ErrorCode: "validation_error", Message: "profile is required"})
			writeWebError(w, http.StatusBadRequest, "profile is required")
			return
		}
		if command == "open" {
			releasing, err := a.profileReleaseInProgress(req.Profile)
			if err != nil {
				writeWebError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if releasing {
				writeWebError(w, http.StatusConflict, "profile is currently releasing; wait for automatic release to finish")
				return
			}
		}
		if req.Confirm {
			if err := a.validateWebAWSOwner(r, configPath, command, req.Profile, req.OwnerEmail); err != nil {
				a.writeRuntimeLog(LogEntry{Level: "error", Action: "web.aws.open", Operation: "aws.open", Profile: req.Profile, Source: "web", Phase: "validate", ErrorCode: classifyOperationalError(err).Code, Message: err.Error()})
				writeWebError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		args := []string{"aws", command, req.Profile}
		if req.Confirm {
			args = append(args, "--confirm")
		}
		if req.Confirm && req.Background {
			resp := a.startWebAWSJob(r, configPath, command, req.Profile, req.OwnerEmail, req.Notify)
			a.logWebResponse("web.aws."+command, req.Profile, resp)
			writeWebJSON(w, resp)
			return
		}
		if command == "destroy" && req.Confirm {
			args = append(args, "--background")
			if req.Notify {
				args = append(args, "--notify")
			}
		}
		resp := a.webRunCommand(r, configPath, args)
		if resp.OK && req.Confirm {
			if err := a.afterConfirmedWebAWSAction(r, configPath, command, req.Profile, req.OwnerEmail); err != nil {
				resp.Output = strings.TrimSpace(resp.Output+"\n"+resp.Error) + "\n负责人记录更新失败：" + err.Error()
				resp.Error = ""
			}
		}
		a.logWebResponse("web.aws."+command, req.Profile, resp)
		a.recordWebEventForRequest(r, configPath, req.Profile, command, req.Confirm, resp)
		writeWebJSON(w, resp)
	}
}

func (a App) profileReleaseInProgress(profileName string) (bool, error) {
	reminder, ok, err := a.MemberStore.ReleaseReminder(profileName)
	if err != nil {
		return false, err
	}
	if ok {
		switch reminder.AutoReleaseState {
		case ReleaseReminderAutoReleaseStateRunning, ReleaseReminderAutoReleaseStateRetrying, ReleaseReminderAutoReleaseStateNotifying:
			return true, nil
		}
	}
	jobs, err := a.JobManager.Active()
	if err != nil {
		return false, err
	}
	return hasActiveDestroyJob(jobs, profileName), nil
}

func (a App) webTunnelStartHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Profile string `json:"profile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			a.writeRuntimeLog(LogEntry{Level: "error", Action: "web.tunnel.start", Operation: "tunnel.start", Source: "web", Phase: "validate", ErrorCode: "validation_error", Message: "invalid json body"})
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		req.Profile = strings.TrimSpace(req.Profile)
		if req.Profile == "" {
			a.writeRuntimeLog(LogEntry{Level: "error", Action: "web.tunnel.start", Operation: "tunnel.start", Source: "web", Phase: "validate", ErrorCode: "validation_error", Message: "profile is required"})
			writeWebError(w, http.StatusBadRequest, "profile is required")
			return
		}
		resp := a.webRunCommand(r, configPath, []string{"start", req.Profile})
		a.logWebResponse("web.tunnel.start", req.Profile, resp)
		a.recordWebEventForRequest(r, configPath, req.Profile, "start", true, resp)
		writeWebJSON(w, resp)
	}
}

func (a App) validateWebAWSOwner(r *http.Request, configPath, command, profileRef, ownerEmail string) error {
	member, ok := a.currentWebMember(r)
	if !ok {
		return errors.New("login required")
	}
	if command == "open" && member.Role == "admin" {
		ownerEmail = normalizeEmail(ownerEmail)
		if ownerEmail == "" {
			ready, err := a.webAWSProfileReady(r.Context(), r, configPath, profileRef)
			if err == nil && ready {
				return nil
			}
			return errors.New("owner_email is required when admin confirms open")
		}
	}
	return nil
}

func (a App) webAWSProfileReady(ctx context.Context, r *http.Request, configPath, profileRef string) (bool, error) {
	cfg, err := a.loadWebConfig(r, configPath)
	if err != nil {
		return false, err
	}
	profile, err := resolveProfileRef(cfg, profileRef)
	if err != nil {
		return false, err
	}
	_, status, err := a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: false})
	if err != nil {
		return false, err
	}
	return AWSStatusReady(status), nil
}

func (a App) afterConfirmedWebAWSAction(r *http.Request, configPath, command, profileRef, ownerEmail string) error {
	cfg, err := a.loadWebConfig(r, configPath)
	if err != nil {
		return err
	}
	profile, err := resolveProfileRef(cfg, profileRef)
	if err != nil {
		return err
	}
	switch command {
	case "open":
		member, ok := a.currentWebMember(r)
		if !ok {
			return errors.New("login required")
		}
		if member.Role != "admin" {
			ownerEmail = member.Email
		}
		ownerEmail = normalizeEmail(ownerEmail)
		if ownerEmail == "" {
			return nil
		}
		if _, err = a.MemberStore.AssignMember(profile.AWS.AccountEmail, ownerEmail, "owner"); err != nil {
			return err
		}
		owner, err := a.MemberStore.SetProfileOwner(profile.Name, ownerEmail)
		if err != nil {
			return err
		}
		return a.upsertReleaseReminderAfterOpen(r.Context(), profile, owner.Owner)
	case "destroy":
		if err := a.MemberStore.ClearProfileOwner(profile.Name); err != nil {
			return err
		}
		return a.markReleaseReminderAfterDestroy(profile)
	default:
		return nil
	}
}

func (a App) upsertReleaseReminderAfterOpen(ctx context.Context, profile Profile, owner PublicMember) error {
	now := time.Now()
	hostID := ""
	hostCreatedAt := now.Format(time.RFC3339)
	if _, status, err := a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: false}); err == nil {
		for _, host := range status.Hosts {
			if host.HostID == "" || strings.EqualFold(host.State, "released") {
				continue
			}
			hostID = host.HostID
			if strings.TrimSpace(host.CreatedAt) != "" {
				hostCreatedAt = host.CreatedAt
			}
			break
		}
	}
	existing, ok, err := a.MemberStore.ReleaseReminder(profile.Name)
	if err != nil {
		return err
	}
	if ok && existing.HostID == hostID && existing.Status != ReleaseReminderStatusReleased {
		existing.OwnerEmail = owner.Email
		existing.OwnerName = owner.Name
		existing, err = a.MemberStore.UpsertReleaseReminder(existing)
		if err != nil {
			return err
		}
		a.notifyReleaseReminder("open", existing, owner.Name, "Mac 打开确认成功")
		return nil
	}
	createdAt, err := time.Parse(time.RFC3339, hostCreatedAt)
	if err != nil {
		createdAt = now
		hostCreatedAt = now.Format(time.RFC3339)
	}
	reminder := ReleaseReminder{
		ProfileName:   profile.Name,
		AppleEmail:    profile.AWS.AccountEmail,
		HostID:        hostID,
		HostCreatedAt: hostCreatedAt,
		ReleaseDueAt:  createdAt.Add(24 * time.Hour).Format(time.RFC3339),
		OwnerEmail:    owner.Email,
		OwnerName:     owner.Name,
		Status:        ReleaseReminderStatusActive,
	}
	reminder, err = a.MemberStore.UpsertReleaseReminder(reminder)
	if err != nil {
		return err
	}
	a.notifyReleaseReminder("open", reminder, owner.Name, "Mac 打开确认成功")
	return nil
}

func (a App) markReleaseReminderAfterDestroy(profile Profile) error {
	reminder, ok, err := a.MemberStore.ReleaseReminder(profile.Name)
	if err != nil || !ok {
		return err
	}
	reminder, err = a.MemberStore.MarkReleaseReminderReleased(profile.Name, time.Now().Format(time.RFC3339))
	if err != nil {
		return err
	}
	a.notifyReleaseReminder("release", reminder, "", "Mac 释放成功")
	return nil
}

func shouldAutoCleanupProfileRecords(status AWSStatus) bool {
	return len(status.Hosts) == 0 && len(status.Instances) == 0 && strings.TrimSpace(status.ElasticIP.InstanceID) == ""
}

func (a App) webAWSStatusWithCleanup(ctx context.Context, profile Profile) (MacPlan, AWSStatus, error) {
	var plan MacPlan
	var status AWSStatus
	err := a.JobManager.WithProfileOperation(profile.Name, func() error {
		var err error
		plan, status, err = a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: false})
		if err != nil {
			return err
		}
		if shouldAutoCleanupProfileRecords(status) {
			if _, err := a.cleanupProfileLocalRecordsLocked(profile.Name, "auto-status"); err != nil {
				a.writeRuntimeLog(LogEntry{Level: "error", Action: "release-reminder.cleanup.auto", Operation: "release-reminder.cleanup", Profile: profile.Name, AppleEmail: profile.AWS.AccountEmail, Source: "background-worker", Phase: "failed", ErrorCode: classifyOperationalError(err).Code, Message: err.Error()})
			}
		}
		return nil
	})
	return plan, status, err
}

func (a App) cleanupProfileLocalRecords(profileName, reason string) (ReleaseReminder, error) {
	reminder, _, err := a.cleanupProfileLocalRecordsChanged(profileName, reason)
	return reminder, err
}

func (a App) cleanupProfileLocalRecordsChanged(profileName, reason string) (ReleaseReminder, bool, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return ReleaseReminder{}, false, errors.New("profile is required")
	}
	var reminder ReleaseReminder
	var changed bool
	err := a.JobManager.WithProfileOperation(profileName, func() error {
		var err error
		reminder, changed, err = a.cleanupProfileLocalRecordsLockedChanged(profileName, reason)
		return err
	})
	return reminder, changed, err
}

func (a App) cleanupProfileLocalRecordsWithEvent(profileName, reason string, event OperationEvent) (ReleaseReminder, bool, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return ReleaseReminder{}, false, errors.New("profile is required")
	}
	var reminder ReleaseReminder
	var changed bool
	err := a.JobManager.WithProfileOperation(profileName, func() error {
		var err error
		reminder, changed, err = a.MemberStore.CleanupProfileRecordsAndRecordEvent(
			profileName,
			time.Now().Format(time.RFC3339),
			reason,
			event,
		)
		return err
	})
	return reminder, changed, err
}

func (a App) cleanupProfileLocalRecordsLocked(profileName, reason string) (ReleaseReminder, error) {
	reminder, _, err := a.cleanupProfileLocalRecordsLockedChanged(profileName, reason)
	return reminder, err
}

func (a App) cleanupProfileLocalRecordsLockedChanged(profileName, reason string) (ReleaseReminder, bool, error) {
	return a.MemberStore.CleanupProfileRecords(
		profileName,
		time.Now().Format(time.RFC3339),
		reason,
	)
}

func (a App) notifyReleaseReminder(event string, reminder ReleaseReminder, operator, description string) error {
	requestToken, _ := randomToken(12)
	context := wechatDeliveryContext{
		RequestID:  "req-wechat-" + requestToken,
		Profile:    reminder.ProfileName,
		AppleEmail: reminder.AppleEmail,
		Source:     "system",
		Event:      event,
		Attempt:    1,
	}
	notification := WechatNotification{
		Event:         event,
		Profile:       reminder.ProfileName,
		AppleEmail:    reminder.AppleEmail,
		Owner:         displayNameEmail(reminder.OwnerName, reminder.OwnerEmail),
		Operator:      operator,
		HostID:        reminder.HostID,
		HostCreatedAt: reminder.HostCreatedAt,
		DueAt:         reminder.ReleaseDueAt,
		Management:    true,
		Description:   description,
	}
	return a.deliverWechatNotification(context, func() (WechatNotifyResult, error) {
		return NewWechatNotifierFromEnv().Send(notification)
	})
}

type wechatDeliveryContext struct {
	RequestID   string
	JobID       string
	Profile     string
	AppleEmail  string
	Source      string
	Actor       AuditActor
	Event       string
	DeliveryKey string
	Attempt     int
}

func (a App) deliverWechatNotification(context wechatDeliveryContext, send func() (WechatNotifyResult, error)) error {
	result, sendErr := a.attemptWechatNotification(context, send)
	if sendErr == nil && !result.Skipped {
		return a.recordWechatDeliveryState(context, "sent", result, nil)
	}
	if sendErr == nil {
		sendErr = errWechatWebhookNotConfigured
	}
	redacted := errors.New(redactWechatWebhookURL(sendErr.Error()))
	failedErr := a.recordWechatDeliveryState(context, "failed", result, redacted)
	return errors.Join(redacted, failedErr)
}

func (a App) attemptWechatNotification(context wechatDeliveryContext, send func() (WechatNotifyResult, error)) (WechatNotifyResult, error) {
	if context.Attempt <= 0 {
		context.Attempt = 1
	}
	if context.Source == "" {
		context.Source = "system"
	}
	if err := a.recordWechatDeliveryState(context, "pending", WechatNotifyResult{}, nil); err != nil {
		return WechatNotifyResult{}, err
	}
	return send()
}

func (a App) recordWechatDeliveryState(context wechatDeliveryContext, phase string, result WechatNotifyResult, cause error) error {
	level := "info"
	status := phase
	if phase == "sent" {
		status = "success"
	}
	errorCode := ""
	message := result.Message
	if cause != nil {
		if phase == "retrying" {
			level = "warn"
		} else {
			level = "error"
			status = "failed"
		}
		errorCode = classifyOperationalError(cause).Code
		message = cause.Error()
	}
	if result.Skipped {
		message = result.Message
	}
	operation := "wechat.notify"
	if event := strings.TrimSpace(context.Event); event != "" {
		operation += "." + event
	}
	entry := LogEntry{
		Level:            level,
		Action:           "wechat." + phase,
		Profile:          context.Profile,
		AppleEmail:       context.AppleEmail,
		ActorMemberID:    context.Actor.MemberID,
		ActorMemberEmail: context.Actor.MemberEmail,
		ActorMemberName:  context.Actor.MemberName,
		RequestID:        context.RequestID,
		JobID:            context.JobID,
		Operation:        operation,
		Source:           context.Source,
		Phase:            phase,
		Status:           status,
		ErrorCode:        errorCode,
		Attempt:          context.Attempt,
		HTTPStatus:       result.HTTPStatus,
		Message:          message,
	}
	a.writeRuntimeLog(entry)
	eventID := ""
	if context.JobID != "" {
		deliveryKey := strings.TrimSpace(context.DeliveryKey)
		if len(deliveryKey) > 16 {
			deliveryKey = deliveryKey[:16]
		}
		eventID = fmt.Sprintf("event-%s-wechat-%s-%d-%s", context.JobID, phase, context.Attempt, deliveryKey)
		if phase == "sent" {
			eventID = "event-" + context.JobID + "-wechat-sent"
		}
	}
	return a.MemberStore.RecordEvent(OperationEvent{
		ID:          eventID,
		Action:      "wechat." + phase,
		Profile:     context.Profile,
		AppleEmail:  context.AppleEmail,
		MemberID:    context.Actor.MemberID,
		MemberEmail: context.Actor.MemberEmail,
		MemberName:  context.Actor.MemberName,
		RequestID:   context.RequestID,
		JobID:       context.JobID,
		Source:      "system",
		Phase:       phase,
		ErrorCode:   errorCode,
		Status:      status,
		Message:     wechatAuditMessageForEvent(result, cause, context.Attempt, context.Event),
	})
}

func wechatAuditMessageForEvent(result WechatNotifyResult, cause error, attempt int, event string) string {
	parts := []string{fmt.Sprintf("attempt=%d", attempt)}
	if strings.TrimSpace(event) != "" {
		parts = append(parts, "notification_event="+strings.TrimSpace(event))
	}
	if result.HTTPStatus != 0 {
		parts = append(parts, fmt.Sprintf("http_status=%d", result.HTTPStatus))
	}
	if result.ErrorCode != 0 {
		parts = append(parts, fmt.Sprintf("wechat_error_code=%d", result.ErrorCode))
	}
	if result.Skipped {
		parts = append(parts, "skipped=true")
	}
	if cause != nil {
		parts = append(parts, "delivery_failed=true")
	}
	return strings.Join(parts, " ")
}

func displayNameEmail(name, email string) string {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	if name == "" {
		return email
	}
	if email == "" || strings.EqualFold(name, email) {
		return name
	}
	return name + " <" + email + ">"
}

func (a App) webJobLogHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		allowed, all, err := a.webJobProfileAccess(r, configPath)
		if err != nil {
			writeWebError(w, http.StatusForbidden, err.Error())
			return
		}
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeWebError(w, http.StatusBadRequest, "id is required")
			return
		}
		job, err := a.JobManager.Load(id)
		if err != nil {
			writeWebError(w, http.StatusNotFound, err.Error())
			return
		}
		if _, ok := allowed[job.Profile]; !all && !ok {
			writeWebError(w, http.StatusForbidden, "job profile is not assigned to the current member")
			return
		}
		var out bytes.Buffer
		if err := copyTail(&out, job.Log, 128*1024); err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"job": job}, Output: out.String()})
	}
}

func (a App) webRunCommand(r *http.Request, configPath string, args []string) webAPIResponse {
	runConfigPath, cleanup, err := a.webCommandConfigPath(r, configPath)
	if err != nil {
		return webAPIResponse{OK: false, Code: 1, Error: err.Error()}
	}
	defer cleanup()
	var out, errOut bytes.Buffer
	sub := a
	sub.In = nil
	sub.Out = &out
	sub.Err = &errOut
	code := sub.Run(r.Context(), append(args, "--config", runConfigPath))
	resp := webAPIResponse{OK: code == 0, Code: code, Output: out.String(), Error: errOut.String()}
	return resp
}

func (a App) startWebAWSJob(r *http.Request, configPath, command, profileRef, ownerEmail string, notify bool) webAPIResponse {
	cfg, err := a.loadWebConfig(r, configPath)
	if err != nil {
		return webAPIResponse{OK: false, Code: 1, Error: err.Error()}
	}
	profile, err := resolveProfileRef(cfg, profileRef)
	if err != nil {
		return webAPIResponse{OK: false, Code: 2, Error: err.Error()}
	}
	if errs := a.Validator.ValidateAWSProfile(profile); len(errs) > 0 {
		return webAPIResponse{OK: false, Code: 1, Error: strings.Join(validationMessages(errs), "\n")}
	}
	runConfigPath, _, err := a.writeWebTempConfig(r, configPath)
	if err != nil {
		return webAPIResponse{OK: false, Code: 1, Error: err.Error()}
	}
	lifecycleOwnerEmail := ""
	if command == "open" {
		member, ok := a.currentWebMember(r)
		if !ok {
			_ = removeJobPaths([]string{runConfigPath})
			return webAPIResponse{OK: false, Code: 1, Error: "login required"}
		}
		lifecycleOwnerEmail = normalizeEmail(ownerEmail)
		if member.Role != "admin" {
			lifecycleOwnerEmail = member.Email
		}
	}
	op := a.operationContextForRequest(r)
	startedAt := a.JobManager.normalize().Now()
	seed := Job{
		ID:                  newJobID("aws-"+command, profile.Name, startedAt),
		Type:                "aws-" + command,
		Profile:             profile.Name,
		AppleEmail:          profile.AWS.AccountEmail,
		RequestID:           op.RequestID,
		Source:              "web",
		ActorMemberID:       op.Actor.MemberID,
		ActorEmail:          op.Actor.MemberEmail,
		ActorName:           op.Actor.MemberName,
		StartedAt:           startedAt,
		LifecycleOwnerEmail: lifecycleOwnerEmail,
		LifecycleState:      JobLifecyclePending,
	}
	job, plan, err := a.prepareAWSJobForResolvedProfile(r.Context(), runConfigPath, command, profile, notify, seed, runConfigPath)
	if err != nil {
		return webAPIResponse{OK: false, Code: 1, Error: err.Error()}
	}
	if err := a.recordWebAWSJobPhase(job, "requested", "success", ""); err != nil {
		_, _ = a.JobManager.failRunnerStartup(job.ID, fmt.Errorf("record requested event: %w", err))
		return webAPIResponse{OK: false, Code: 1, Error: err.Error()}
	}
	detached := context.WithoutCancel(r.Context())
	job, err = a.JobManager.StartRunner(detached, job)
	if err != nil {
		if lifecycleErr := a.reconcileWebAWSLifecycleJob(detached, configPath, job); lifecycleErr != nil {
			classified := classifyOperationalError(lifecycleErr)
			a.writeRuntimeLog(LogEntry{
				Level:     classified.Level,
				Action:    "web.aws." + command + ".startup_failure_reconcile",
				Profile:   job.Profile,
				RequestID: job.RequestID,
				JobID:     job.ID,
				Source:    job.Source,
				Phase:     "failed",
				ErrorCode: classified.Code,
				Message:   lifecycleErr.Error(),
			})
		}
		return webAPIResponse{OK: false, Code: 1, Error: err.Error()}
	}
	_ = a.recordWebAWSJobPhase(job, "started", "success", "")
	var out strings.Builder
	fmt.Fprintf(&out, "Started background AWS %s job: %s\n", command, job.ID)
	fmt.Fprintf(&out, "Profile: %s\n", profile.Name)
	if plan.AccountEmail != "" {
		fmt.Fprintf(&out, "Apple account: %s\n", plan.AccountEmail)
	}
	fmt.Fprintf(&out, "PID: %d\n", job.PID)
	fmt.Fprintf(&out, "Log: %s\n", job.Log)
	if command == "destroy" {
		fmt.Fprintln(&out, "Elastic IP allocation will be retained.")
	}
	return webAPIResponse{OK: true, Code: 0, Output: out.String(), Data: map[string]interface{}{"job": job}}
}

func (a App) startAWSJobForResolvedProfile(ctx context.Context, configPath, command string, profile Profile, notify bool, cleanupPaths ...string) (job Job, plan MacPlan, err error) {
	return a.startAWSJobForResolvedProfileJob(ctx, configPath, command, profile, notify, Job{}, cleanupPaths...)
}

func (a App) startAWSJobForResolvedProfileJob(ctx context.Context, configPath, command string, profile Profile, notify bool, job Job, cleanupPaths ...string) (created Job, plan MacPlan, err error) {
	created, plan, err = a.prepareAWSJobForResolvedProfile(ctx, configPath, command, profile, notify, job, cleanupPaths...)
	if err != nil {
		return created, plan, err
	}
	created, err = a.JobManager.StartRunner(ctx, created)
	return created, plan, err
}

func (a App) prepareAWSJobForResolvedProfile(ctx context.Context, configPath, command string, profile Profile, notify bool, job Job, cleanupPaths ...string) (created Job, plan MacPlan, err error) {
	defer func() {
		if err != nil {
			_ = removeJobPaths(cleanupPaths)
		}
	}()
	plan, err = a.AWSService.Plan(profile)
	if err != nil {
		return Job{}, MacPlan{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return Job{}, MacPlan{}, err
	}
	job.Type = "aws-" + command
	job.Profile = profile.Name
	job.AppleEmail = plan.AccountEmail
	op := operationContextFrom(ctx)
	if job.RequestID == "" {
		job.RequestID = op.RequestID
	}
	if job.Source == "" {
		job.Source = op.Source
	}
	if job.ActorMemberID == "" {
		job.ActorMemberID = op.Actor.MemberID
	}
	if job.ActorEmail == "" {
		job.ActorEmail = op.Actor.MemberEmail
	}
	if job.ActorName == "" {
		job.ActorName = op.Actor.MemberName
	}
	job.Command = []string{executable, "aws", command, profile.Name, "--confirm", "--config", configPath}
	job.Notify = notify
	job.CleanupPaths = append([]string(nil), cleanupPaths...)
	created, err = a.JobManager.Create(job)
	if err != nil {
		return Job{}, MacPlan{}, err
	}
	return created, plan, err
}

func (a App) recordWebAWSJobPhase(job Job, phase, status, errorCode string) error {
	actionPrefix := "aws.open"
	if job.Type == "aws-destroy" {
		actionPrefix = "aws.release"
	}
	err := a.MemberStore.RecordEvent(OperationEvent{
		Action:      actionPrefix + "." + phase,
		Profile:     job.Profile,
		AppleEmail:  job.AppleEmail,
		MemberID:    job.ActorMemberID,
		MemberEmail: job.ActorEmail,
		MemberName:  job.ActorName,
		RequestID:   job.RequestID,
		JobID:       job.ID,
		Source:      job.Source,
		Phase:       phase,
		ErrorCode:   errorCode,
		Confirmed:   true,
		Status:      status,
	})
	if err != nil {
		classified := classifyOperationalError(err)
		a.writeRuntimeLog(LogEntry{
			Level:       classified.Level,
			Action:      actionPrefix + "." + phase + ".audit_failed",
			Operation:   actionPrefix,
			Profile:     job.Profile,
			AppleEmail:  job.AppleEmail,
			MemberEmail: job.ActorEmail,
			RequestID:   job.RequestID,
			JobID:       job.ID,
			Source:      job.Source,
			Phase:       phase,
			ErrorCode:   classified.Code,
			Message:     err.Error(),
		})
	}
	return err
}

func (a App) recordWebEvent(configPath, profileRef, action string, confirmed bool, resp webAPIResponse) {
	a.recordWebEventForRequest(nil, configPath, profileRef, action, confirmed, resp)
}

func (a App) recordWebEventForRequest(r *http.Request, configPath, profileRef, action string, confirmed bool, resp webAPIResponse) {
	status := "success"
	message := strings.TrimSpace(resp.Output)
	if !resp.OK {
		status = "failed"
		message = strings.TrimSpace(resp.Error)
	}
	if len(message) > 400 {
		message = message[:400]
	}
	eventAction := action
	eventPhase := ""
	if !confirmed {
		switch action {
		case "open":
			eventAction = "aws.open.previewed"
			eventPhase = "previewed"
		case "destroy":
			eventAction = "aws.release.previewed"
			eventPhase = "previewed"
		}
	} else {
		switch action {
		case "open":
			eventAction = "aws.open.requested"
			eventPhase = "requested"
		case "destroy":
			eventAction = "aws.release.requested"
			eventPhase = "requested"
		}
	}
	op := OperationContext{Source: "web"}
	if r != nil {
		op = a.operationContextForRequest(r)
	}
	event := OperationEvent{
		Action:        eventAction,
		Profile:       profileRef,
		MemberID:      op.Actor.MemberID,
		MemberEmail:   op.Actor.MemberEmail,
		MemberName:    op.Actor.MemberName,
		RequestID:     op.RequestID,
		SessionIDHash: op.SessionIDHash,
		Source:        op.Source,
		Phase:         eventPhase,
		Confirmed:     confirmed,
		Status:        status,
		Message:       message,
	}
	if cfg, err := LoadConfig(configPath); err == nil {
		if profile, err := resolveProfileRef(cfg, profileRef); err == nil {
			event.Profile = profile.Name
			event.AppleEmail = profile.AWS.AccountEmail
		}
	}
	_ = a.recordEventWithFallback(event)
}

func (a App) webCommandConfigPath(r *http.Request, configPath string) (string, func(), error) {
	cfg, err := LoadConfig(configPath)
	if err == nil && strings.TrimSpace(cfg.Server.UserAPI) == "" {
		return configPath, func() {}, nil
	}
	path, cleanup, err := a.writeWebTempConfig(r, configPath)
	return path, cleanup, err
}

func (a App) writeWebTempConfig(r *http.Request, configPath string) (string, func(), error) {
	cfg, err := a.loadWebConfig(r, configPath)
	if err != nil {
		return "", func() {}, err
	}
	file, err := os.CreateTemp("", "cm-web-config-*.yaml")
	if err != nil {
		return "", func() {}, err
	}
	path := file.Name()
	if _, err := file.WriteString(FormatConfigProfiles(cfg)); err != nil {
		file.Close()
		os.Remove(path)
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", func() {}, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

func (a App) logProfileError(action string, profile Profile, message string) {
	a.writeRuntimeLog(LogEntry{
		Level:      "error",
		Action:     action,
		Profile:    profile.Name,
		AppleEmail: profile.AWS.AccountEmail,
		Region:     profile.AWS.Region,
		AWSProfile: profile.AWS.Profile,
		Message:    message,
	})
}

func (a App) logWebResponse(action, profileRef string, resp webAPIResponse) {
	level := "info"
	message := strings.TrimSpace(resp.Output)
	if !resp.OK {
		level = "error"
		message = strings.TrimSpace(resp.Error)
	}
	if message == "" {
		message = fmt.Sprintf("code=%d ok=%t", resp.Code, resp.OK)
	}
	a.writeRuntimeLog(LogEntry{Level: level, Action: action, Profile: profileRef, Message: message})
}

func webAWSStatusData(profile Profile, plan MacPlan, status AWSStatus) webAWSStatus {
	action := AWSOpenAction(status)
	data := webAWSStatus{
		Profile:    profile.Name,
		AppleEmail: profile.AWS.AccountEmail,
		Region:     plan.Region,
		Decision:   action.Kind,
		Detail:     action.Detail,
		Next:       AWSOpenDecisionNextStep(profile.Name, action),
		Ready:      AWSStatusReady(status),
		ElasticIP: webElasticIP{
			AllocationID:  status.ElasticIP.AllocationID,
			AssociationID: status.ElasticIP.AssociationID,
			InstanceID:    status.ElasticIP.InstanceID,
			PublicIP:      status.ElasticIP.PublicIP,
		},
	}
	for _, host := range status.Hosts {
		data.Hosts = append(data.Hosts, webDedicatedHost{
			HostID:       host.HostID,
			State:        host.State,
			InstanceType: host.InstanceType,
			ZoneID:       host.ZoneID,
			CreatedAt:    host.CreatedAt,
		})
	}
	for _, instance := range status.Instances {
		data.Instances = append(data.Instances, webInstance{
			InstanceID:     instance.InstanceID,
			State:          instance.State,
			InstanceType:   instance.InstanceType,
			HostID:         instance.HostID,
			PublicIP:       instance.PublicIP,
			SystemStatus:   emptyStatus(instance.SystemStatus),
			InstanceStatus: emptyStatus(instance.InstanceStatusCheck),
			EBSStatus:      emptyStatus(instance.EBSStatus),
			Ready:          InstanceReady(instance, status.ElasticIP),
		})
	}
	if data.Hosts == nil {
		data.Hosts = []webDedicatedHost{}
	}
	if data.Instances == nil {
		data.Instances = []webInstance{}
	}
	return data
}

func validationMessages(errs []error) []string {
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		messages = append(messages, err.Error())
	}
	return messages
}

func writeWebJSON(w http.ResponseWriter, resp webAPIResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !resp.OK && resp.Code == 0 {
		resp.Code = 1
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeWebError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	writeWebJSON(w, webAPIResponse{OK: false, Code: status, Error: message})
}
