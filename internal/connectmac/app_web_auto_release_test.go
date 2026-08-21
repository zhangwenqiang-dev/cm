package connectmac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebAutoReleaseUIContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)

	for _, want := range []string{
		`id="autoReleaseSummary"`,
		`id="autoReleaseError"`,
		`id="autoReleaseToggleBtn" aria-pressed="false"`,
		`/api/release-reminder/auto-release`,
		`未开启自动释放`,
		`等待提醒`,
		`将在 ${formatTime(reminder.auto_release_at)} 自动释放`,
		`正在自动释放`,
		`释放重试中（第 ${attempts} 次）`,
		`if (reminder.auto_release_notified_at) return "释放通知已发送，清理重试中";`,
		`return "释放完成，企业微信通知重试中";`,
		`自动释放失败`,
		`const showError = !!reminder?.auto_release_last_error && (reminder?.auto_release_state === "retrying" || reminder?.auto_release_state === "failed");`,
		`已释放`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web auto release UI missing %q", want)
		}
	}
}

func TestAppAutoReleaseCleanupRetryEventLogsError(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	coordinator := app.newAutoReleaseCoordinator("")
	reminder := ReleaseReminder{
		ProfileName:           "xcode-vnc",
		AppleEmail:            "user@example.com",
		OwnerEmail:            "admin@example.com",
		AutoReleaseAt:         "2026-07-13T08:00:00Z",
		AutoReleaseStartedAt:  "2026-07-13T08:01:00Z",
		AutoReleaseState:      ReleaseReminderAutoReleaseStateNotifying,
		AutoReleaseNotifiedAt: "2026-07-13T09:00:00Z",
	}
	cycleID := autoReleaseCycleID(reminder)
	coordinator.Emit(AutoReleaseEvent{
		Action: "cleanup-retrying", Reminder: reminder, Attempt: 2, CycleID: cycleID,
		Message: "cleanup released profile records: database unavailable",
	})

	for _, entry := range readTestLogEntries(t, app.LogManager) {
		if entry.Action != "auto-release.cleanup-retrying" {
			continue
		}
		if entry.Level != "error" || entry.Operation != "auto-release" || entry.Source != "background-worker" || entry.Phase != "cleanup-retrying" || entry.Profile != reminder.ProfileName || entry.AppleEmail != reminder.AppleEmail || entry.CycleID != cycleID || entry.Attempt != 2 || entry.ErrorCode != "storage_error" {
			t.Fatalf("cleanup retry log = %+v", entry)
		}
		events, err := app.MemberStore.QueryEvents(EventQuery{Profile: reminder.ProfileName, IncludeSystem: true, Limit: 10})
		if err != nil {
			t.Fatalf("query cleanup retry event: %v", err)
		}
		if len(events.Events) != 1 || events.Events[0].Action != entry.Action || events.Events[0].CycleID != cycleID || events.Events[0].Attempt != 2 || events.Events[0].ErrorCode != entry.ErrorCode {
			t.Fatalf("cleanup retry events = %+v", events.Events)
		}
		return
	}
	t.Fatal("cleanup retry runtime log not found")
}

func TestAppAutoReleaseCompletionObservabilityChain(t *testing.T) {
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	const webhookKey = "task5-success-webhook-key"
	var webhookCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookCalls.Add(1)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()
	t.Setenv(envWechatWebhookURL, server.URL+"?key="+webhookKey)

	app := newWebAutoReleaseTestApp(t)
	reminder := ReleaseReminder{
		ProfileName:              "xcode-vnc",
		AppleEmail:               "user@example.com",
		HostID:                   "h-observed",
		OwnerEmail:               "owner@example.com",
		ReleaseDueAt:             now.Add(-20 * time.Minute).Format(time.RFC3339),
		Status:                   ReleaseReminderStatusDueNotified,
		AutoReleaseEnabled:       true,
		AutoReleaseAt:            now.Add(-10 * time.Minute).Format(time.RFC3339),
		AutoReleaseStartedAt:     now.Add(-6 * time.Minute).Format(time.RFC3339),
		AutoReleaseLastAttemptAt: now.Add(-5 * time.Minute).Format(time.RFC3339),
		AutoReleaseAttempts:      2,
		AutoReleaseState:         ReleaseReminderAutoReleaseStateRunning,
	}
	if _, err := app.MemberStore.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	job := Job{
		ID:         "job-auto-release-observed",
		Type:       "aws-destroy",
		Profile:    reminder.ProfileName,
		AppleEmail: reminder.AppleEmail,
		RequestID:  "req-auto-release-observed",
		Source:     "background-worker",
		Status:     JobStatusSuccess,
		StartedAt:  now.Add(-4 * time.Minute),
		FinishedAt: now.Add(-time.Minute),
	}
	if _, err := app.JobManager.Create(job); err != nil {
		t.Fatalf("seed destroy job: %v", err)
	}

	coordinator := app.newAutoReleaseCoordinator("")
	coordinator.Now = func() time.Time { return now }
	coordinator.ResolveProfile = func(context.Context, ReleaseReminder) (Profile, error) {
		return Profile{Name: reminder.ProfileName, AWS: AWSConfig{AccountEmail: reminder.AppleEmail}}, nil
	}
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}, nil
	}
	coordinator.StartDestroy = func(context.Context, Profile) (Job, error) {
		return Job{}, errors.New("unexpected destroy start")
	}
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if webhookCalls.Load() != 1 {
		t.Fatalf("webhook calls = %d, want 1", webhookCalls.Load())
	}
	if got := mustReleaseReminder(t, app, reminder.ProfileName); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased {
		t.Fatalf("completed reminder = %+v", got)
	}

	wantActions := []string{
		"auto-release.job.observed",
		"auto-release.notification-pending",
		"wechat.pending",
		"wechat.sent",
		"auto-release.released",
	}
	wantAction := make(map[string]bool, len(wantActions))
	for _, action := range wantActions {
		wantAction[action] = true
	}
	var chain []LogEntry
	for _, entry := range readTestLogEntries(t, app.LogManager) {
		if wantAction[entry.Action] {
			chain = append(chain, entry)
		}
	}
	if len(chain) != len(wantActions) {
		t.Fatalf("observability chain = %+v, want actions %v", chain, wantActions)
	}
	for i, entry := range chain {
		if entry.Action != wantActions[i] || entry.Profile != reminder.ProfileName || entry.AppleEmail != reminder.AppleEmail || entry.Phase == "" {
			t.Fatalf("chain[%d] = %+v, want action %s with profile correlation", i, entry, wantActions[i])
		}
	}
	if observed := chain[0]; observed.JobID != job.ID || observed.RequestID != job.RequestID || observed.Attempt != reminder.AutoReleaseAttempts || observed.Phase != "job.observed" {
		t.Fatalf("observed job log = %+v", observed)
	}
	for _, entry := range chain[1:2] {
		if entry.Attempt != reminder.AutoReleaseAttempts {
			t.Fatalf("automatic release log missing attempt: %+v", entry)
		}
	}
	for _, entry := range chain[2:4] {
		if entry.RequestID == "" || entry.Attempt != reminder.AutoReleaseAttempts {
			t.Fatalf("wechat log missing request correlation: %+v", entry)
		}
	}
	if chain[4].Attempt != reminder.AutoReleaseAttempts {
		t.Fatalf("released log missing attempt: %+v", chain[4])
	}

	events, err := app.MemberStore.QueryEvents(EventQuery{Profile: reminder.ProfileName, IncludeSystem: true, Limit: 20})
	if err != nil {
		t.Fatalf("query operation events: %v", err)
	}
	eventCounts := make(map[string]int, len(wantActions))
	for _, event := range events.Events {
		if wantAction[event.Action] {
			eventCounts[event.Action]++
			if event.Attempt != reminder.AutoReleaseAttempts {
				t.Fatalf("operation event %s attempt = %d, want %d: %+v", event.Action, event.Attempt, reminder.AutoReleaseAttempts, event)
			}
		}
		if event.Action == "auto-release.job.observed" && (event.JobID != job.ID || event.RequestID != job.RequestID) {
			t.Fatalf("observed job event missing correlation: %+v", event)
		}
	}
	for _, action := range wantActions {
		if eventCounts[action] != 1 {
			t.Fatalf("operation events contain %d %s entries, want 1: %+v", eventCounts[action], action, events.Events)
		}
	}
	logs, err := os.ReadFile(filepath.Join(app.LogManager.Dir, "cm-2026-07-01.log"))
	if err != nil {
		t.Fatalf("read runtime logs: %v", err)
	}
	if strings.Contains(string(logs), webhookKey) {
		t.Fatalf("runtime logs leaked webhook key: %s", logs)
	}
}

func TestAppAutoReleaseScheduleDueNormalizesWechatAttempt(t *testing.T) {
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name           string
		response       string
		terminalAction string
		wantError      bool
	}{
		{name: "sent", response: `{"errcode":0,"errmsg":"ok"}`, terminalAction: "wechat.sent"},
		{name: "failed", response: `{"errcode":93000,"errmsg":"temporary failure"}`, terminalAction: "wechat.failed", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()
			t.Setenv(envWechatWebhookURL, server.URL+"?key=schedule-due-secret")

			app := newWebAutoReleaseTestApp(t)
			reminder := ReleaseReminder{
				ProfileName:        "xcode-vnc",
				AppleEmail:         "user@example.com",
				ReleaseDueAt:       now.Add(-time.Minute).Format(time.RFC3339),
				Status:             ReleaseReminderStatusActive,
				AutoReleaseEnabled: true,
			}
			if _, err := app.MemberStore.UpsertReleaseReminder(reminder); err != nil {
				t.Fatalf("seed reminder: %v", err)
			}
			coordinator := app.newAutoReleaseCoordinator("")
			coordinator.Now = func() time.Time { return now }
			err := coordinator.Scan(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("scan error = %v, wantError=%t", err, test.wantError)
			}

			entries := readTestLogEntries(t, app.LogManager)
			var pending, terminal LogEntry
			for _, entry := range entries {
				switch entry.Action {
				case "wechat.pending":
					pending = entry
				case test.terminalAction:
					terminal = entry
				}
			}
			if pending.Attempt != 1 || terminal.Attempt != 1 || pending.RequestID == "" || pending.RequestID != terminal.RequestID {
				t.Fatalf("scheduleDue delivery attempts pending=%+v terminal=%+v", pending, terminal)
			}

			page, queryErr := app.MemberStore.QueryEvents(EventQuery{Profile: reminder.ProfileName, IncludeSystem: true, Limit: 10})
			if queryErr != nil {
				t.Fatalf("query events: %v", queryErr)
			}
			attempts := map[string]int{}
			for _, event := range page.Events {
				attempts[event.Action] = event.Attempt
			}
			if attempts["wechat.pending"] != 1 || attempts[test.terminalAction] != 1 {
				t.Fatalf("scheduleDue operation event attempts = %+v", attempts)
			}
		})
	}
}

func TestAppAutoReleaseNotificationRetryUsesProductionDeliveryAndRedactsSecrets(t *testing.T) {
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	secrets := []string{
		"task5-fake-webhook-key",
		"task5-fake-bearer-token",
		"task5-fake-session-token",
		"/Users/test/.ssh/task5-private.pem",
		"AKIAIOSFODNN7EXAMPLE",
		"task5-fake-aws-secret",
		"task5-cookie-session",
		"task5-cookie-preference",
		"task5-set-cookie-session",
		"task5-basic-user",
		"task5-basic-password",
		"task5-standalone-webhook-key",
		"task5-json-token",
		"task5-json-session",
		"task5-json-secret",
		"task5-json-password",
		"task5-access-key",
		"task5-secret-access-key",
		"~/.ssh/task5-home-private.pem",
		"task5-set-cookie-csrf",
	}
	var injected string
	var webhookCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if webhookCalls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errcode": 93000, "errmsg": injected})
			return
		}
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()
	webhookURL := server.URL + "/cgi-bin/webhook/send?key=" + secrets[0]
	sensitiveValues := append(append([]string(nil), secrets...), server.URL)
	injected = strings.Join([]string{
		webhookURL,
		"Authorization: Bearer " + secrets[1],
		"session_token=" + secrets[2],
		"pem_path=" + secrets[3],
		"AWS_ACCESS_KEY_ID=" + secrets[4],
		"AWS_SECRET_ACCESS_KEY=" + secrets[5],
		"Cookie: cm_session=" + secrets[6] + "; preference=" + secrets[7],
		"Set-Cookie: cm_session=" + secrets[8] + "; Path=/; HttpOnly",
		"Set-Cookie: csrf=" + secrets[19] + "; Path=/; Secure",
		"https://" + secrets[9] + ":" + secrets[10] + "@example.invalid/path",
		"key=" + secrets[11],
		`{"token":"` + secrets[12] + `","session":'` + secrets[13] + `','secret':"` + secrets[14] + `","password"='` + secrets[15] + `'}`,
		"access_key=" + secrets[16],
		"secret_access_key: " + secrets[17],
		"load " + secrets[18] + " failed",
		"token expired",
		"session unavailable",
		"secret rotation failed",
	}, "\n")
	t.Setenv(envWechatWebhookURL, webhookURL)

	app := newWebAutoReleaseTestApp(t)
	reminder := ReleaseReminder{
		ProfileName:              "xcode-vnc",
		AppleEmail:               "user@example.com",
		HostID:                   "h-observed",
		OwnerEmail:               "admin@example.com",
		OwnerName:                "Test Admin",
		ReleaseDueAt:             now.Add(-20 * time.Minute).Format(time.RFC3339),
		Status:                   ReleaseReminderStatusDueNotified,
		AutoReleaseEnabled:       true,
		AutoReleaseAt:            now.Add(-10 * time.Minute).Format(time.RFC3339),
		AutoReleaseStartedAt:     now.Add(-6 * time.Minute).Format(time.RFC3339),
		AutoReleaseLastAttemptAt: now.Add(-5 * time.Minute).Format(time.RFC3339),
		AutoReleaseAttempts:      1,
		AutoReleaseState:         ReleaseReminderAutoReleaseStateRunning,
	}
	if _, err := app.MemberStore.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	apiRequest := httptest.NewRequest(http.MethodGet, "/api/release-reminders", nil)
	addWebAuth(t, &app, apiRequest, "admin")
	if _, err := app.MemberStore.SetProfileOwner(reminder.ProfileName, reminder.OwnerEmail); err != nil {
		t.Fatalf("set profile owner: %v", err)
	}
	job := Job{
		ID: "job-auto-release-retry", Type: "aws-destroy",
		Profile: reminder.ProfileName, AppleEmail: reminder.AppleEmail,
		RequestID: "req-auto-release-retry", Source: "background-worker",
		Status: JobStatusSuccess, StartedAt: now.Add(-4 * time.Minute), FinishedAt: now.Add(-time.Minute),
	}
	if _, err := app.JobManager.Create(job); err != nil {
		t.Fatalf("seed destroy job: %v", err)
	}

	scanNow := now
	coordinator := app.newAutoReleaseCoordinator("")
	coordinator.Now = func() time.Time { return scanNow }
	coordinator.ResolveProfile = func(context.Context, ReleaseReminder) (Profile, error) {
		return Profile{Name: reminder.ProfileName, AWS: AWSConfig{AccountEmail: reminder.AppleEmail}}, nil
	}
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}, nil
	}
	coordinator.StartDestroy = func(context.Context, Profile) (Job, error) {
		return Job{}, errors.New("unexpected destroy start")
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	first := mustReleaseReminder(t, app, reminder.ProfileName)
	if first.Status == ReleaseReminderStatusReleased || first.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying || first.AutoReleaseAttempts != 1 || first.AutoReleaseLastError == "" {
		t.Fatalf("first reminder = %+v", first)
	}
	assertTask5SecretsAbsent(t, "persisted reminder error", first.AutoReleaseLastError, sensitiveValues)
	for _, diagnostic := range []string{"token expired", "session unavailable", "secret rotation failed"} {
		if !strings.Contains(first.AutoReleaseLastError, diagnostic) {
			t.Fatalf("persisted reminder error lost diagnostic %q: %s", diagnostic, first.AutoReleaseLastError)
		}
	}

	apiResponse := httptest.NewRecorder()
	app.newWebHandler("").ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusOK || !strings.Contains(apiResponse.Body.String(), "auto_release_last_error") {
		t.Fatalf("reminder API status=%d body=%s", apiResponse.Code, apiResponse.Body.String())
	}
	assertTask5SecretsAbsent(t, "reminder API response", apiResponse.Body.String(), sensitiveValues)
	for _, diagnostic := range []string{"token expired", "session unavailable", "secret rotation failed"} {
		if !strings.Contains(apiResponse.Body.String(), diagnostic) {
			t.Fatalf("reminder API response lost diagnostic %q: %s", diagnostic, apiResponse.Body.String())
		}
	}

	scanNow = now.Add(AutoReleaseRetryInterval)
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("retry scan: %v", err)
	}
	completed := mustReleaseReminder(t, app, reminder.ProfileName)
	if completed.Status != ReleaseReminderStatusReleased || completed.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || completed.AutoReleaseAttempts != 2 || completed.AutoReleaseStartedAt != reminder.AutoReleaseStartedAt || completed.AutoReleaseLastError != "" {
		t.Fatalf("completed reminder = %+v", completed)
	}
	if webhookCalls.Load() != 2 {
		t.Fatalf("webhook calls = %d, want 2", webhookCalls.Load())
	}

	cycleID := autoReleaseCycleID(reminder)
	logCounts := map[string]int{}
	var pending, failed, retrying, sent LogEntry
	for _, entry := range readTestLogEntries(t, app.LogManager) {
		if entry.Profile != reminder.ProfileName {
			continue
		}
		logCounts[entry.Action]++
		if strings.HasPrefix(entry.Action, "auto-release.") || strings.HasPrefix(entry.Action, "wechat.") {
			if entry.CycleID != cycleID {
				t.Fatalf("log cycle ID = %q, want %q: %+v", entry.CycleID, cycleID, entry)
			}
		}
		switch entry.Action {
		case "wechat.pending":
			pending = entry
		case "wechat.failed":
			failed = entry
		case "wechat.retrying":
			retrying = entry
		case "wechat.sent":
			sent = entry
		}
	}
	for _, action := range []string{"wechat.pending", "wechat.failed", "wechat.retrying", "wechat.sent", "auto-release.notification-retrying", "auto-release.released"} {
		if logCounts[action] != 1 {
			t.Fatalf("runtime log count %s = %d, want 1", action, logCounts[action])
		}
	}
	if pending.Attempt != 1 || failed.Attempt != 1 || pending.RequestID == "" || pending.RequestID != failed.RequestID || pending.Phase != "pending" || failed.Phase != "failed" || failed.ErrorCode != "notification_error" {
		t.Fatalf("first delivery logs pending=%+v failed=%+v", pending, failed)
	}
	if retrying.Attempt != 2 || sent.Attempt != 2 || retrying.RequestID == "" || retrying.RequestID != sent.RequestID || retrying.Phase != "retrying" || retrying.ErrorCode != "notification_error" || sent.Phase != "sent" {
		t.Fatalf("retry delivery logs retrying=%+v sent=%+v", retrying, sent)
	}

	rawLogs, err := os.ReadFile(filepath.Join(app.LogManager.Dir, "cm-2026-07-01.log"))
	if err != nil {
		t.Fatalf("read runtime logs: %v", err)
	}
	events, err := app.MemberStore.QueryEvents(EventQuery{Profile: reminder.ProfileName, IncludeSystem: true, Limit: 20})
	if err != nil {
		t.Fatalf("query operation events: %v", err)
	}
	rawEvents, err := json.Marshal(events.Events)
	if err != nil {
		t.Fatalf("marshal operation events: %v", err)
	}
	assertTask5SecretsAbsent(t, "runtime logs", string(rawLogs), sensitiveValues)
	assertTask5SecretsAbsent(t, "operation events", string(rawEvents), sensitiveValues)

	eventCounts := map[string]int{}
	wantAttempts := map[string]int{
		"wechat.pending":                     1,
		"wechat.failed":                      1,
		"wechat.retrying":                    2,
		"wechat.sent":                        2,
		"auto-release.notification-retrying": 1,
		"auto-release.released":              2,
	}
	for _, event := range events.Events {
		if event.Profile != reminder.ProfileName {
			continue
		}
		eventCounts[event.Action]++
		if strings.HasPrefix(event.Action, "auto-release.") || strings.HasPrefix(event.Action, "wechat.") {
			if event.CycleID != cycleID {
				t.Fatalf("event cycle ID = %q, want %q: %+v", event.CycleID, cycleID, event)
			}
		}
		if wantAttempt, ok := wantAttempts[event.Action]; ok && event.Attempt != wantAttempt {
			t.Fatalf("event %s attempt = %d, want %d: %+v", event.Action, event.Attempt, wantAttempt, event)
		}
		if event.Action == "wechat.retrying" && event.ErrorCode != "notification_error" {
			t.Fatalf("wechat retry event missing error code: %+v", event)
		}
	}
	for _, action := range []string{"wechat.pending", "wechat.failed", "wechat.retrying", "wechat.sent", "auto-release.notification-retrying", "auto-release.released"} {
		if eventCounts[action] != 1 {
			t.Fatalf("operation event count %s = %d, want 1: %+v", action, eventCounts[action], events.Events)
		}
	}
	for _, diagnostic := range []string{"token expired", "session unavailable", "secret rotation failed"} {
		if !strings.Contains(string(rawEvents), diagnostic) {
			t.Fatalf("operation events lost diagnostic %q: %s", diagnostic, rawEvents)
		}
	}
}

func TestDeliverWechatNotificationReturnsComprehensivelySanitizedError(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	secrets := []string{
		"return-webhook-key", "return-bearer", "return-session", "/Users/test/.ssh/return.pem", "return-aws-secret",
		"return-cookie", "return-set-cookie", "return-basic-user", "return-basic-password", "return-standalone-key",
		"return-json-token", "return-access-key", "return-secret-access-key", "~/.ssh/return-home.pem",
	}
	raw := strings.Join([]string{
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + secrets[0],
		"Authorization: Bearer " + secrets[1],
		"session_token=" + secrets[2],
		"pem_path=" + secrets[3],
		"AWS_SECRET_ACCESS_KEY=" + secrets[4],
		"Cookie: cm_session=" + secrets[5] + "; preference=dark",
		"Set-Cookie: csrf=" + secrets[6] + "; Path=/; Secure",
		"https://" + secrets[7] + ":" + secrets[8] + "@example.invalid/path",
		"key=" + secrets[9],
		`{"token":"` + secrets[10] + `"}`,
		"access_key=" + secrets[11],
		"secret_access_key: " + secrets[12],
		"load " + secrets[13] + " failed",
		"token expired",
		"session unavailable",
		"secret rotation failed",
	}, "\n")
	err := app.deliverWechatNotification(wechatDeliveryContext{
		RequestID: "req-return", Profile: "xcode-vnc", AppleEmail: "user@example.com",
		Event: "auto-release-success", Attempt: 1, CycleID: "arc-test",
	}, func() (WechatNotifyResult, error) {
		return WechatNotifyResult{HTTPStatus: http.StatusBadGateway}, errors.New(raw)
	})
	if err == nil {
		t.Fatal("deliverWechatNotification error = nil")
	}
	assertTask5SecretsAbsent(t, "delivery error", err.Error(), secrets)
	if strings.Contains(err.Error(), "qyapi.weixin.qq.com") {
		t.Fatalf("delivery error retained full webhook URL: %v", err)
	}
	for _, diagnostic := range []string{"token expired", "session unavailable", "secret rotation failed"} {
		if !strings.Contains(err.Error(), diagnostic) {
			t.Fatalf("delivery error lost diagnostic %q: %v", diagnostic, err)
		}
	}
}

func assertTask5SecretsAbsent(t *testing.T, label, value string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(value, secret) {
			t.Fatalf("%s leaked %q: %s", label, secret, value)
		}
	}
}

func TestWebAutoReleaseReleasingStateLocksConflictingActions(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	renderWorkbench := extractWebSource(t, html, "function renderWorkbench(p, status, reminder)", "\n    function renderSelected()")
	for _, want := range []string{
		`ConnectMacWorkbench.effectiveState({`,
		`reminder: reminder,`,
		`jobs: jobs,`,
		`ConnectMacWorkbench.buildActionModel({`,
		`applyWorkbenchAction("openMacBtn", model.actions.open`,
		`applyWorkbenchAction("releaseMacBtn", model.actions.release`,
	} {
		if !strings.Contains(renderWorkbench, want) {
			t.Fatalf("renderWorkbench releasing contract missing %q", want)
		}
	}

	profileStatusSummary := extractWebSource(t, html, "function profileStatusSummary(status, profileName)", "\n    async function refreshVisibleStatuses(")
	for _, want := range []string{
		`autoReleaseActive(state.reminders[profileName], profileName)`,
		`return "正在释放";`,
	} {
		if !strings.Contains(profileStatusSummary, want) {
			t.Fatalf("profileStatusSummary releasing source missing %q", want)
		}
	}

	for _, want := range []string{
		`if (reminder?.status === "released" || reminder?.auto_release_state === "released") return false;`,
		`reminder?.auto_release_state === "running" || reminder?.auto_release_state === "retrying" || reminder?.auto_release_state === "notifying"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web releasing state contract missing %q", want)
		}
	}
	for _, obsolete := range []string{
		`const openDisabled =`,
		`const destroyDisabled =`,
		`const releaseActive =`,
	} {
		if strings.Contains(html, obsolete) {
			t.Fatalf("web releasing state still uses obsolete independent guard %q", obsolete)
		}
	}
	activeJob := strings.Index(html, `if (state.jobs.some((job) => job.profile === profileName && job.type === "aws-destroy" && (job.status === "starting" || job.status === "running"))) return true;`)
	terminalReminder := strings.Index(html, `if (reminder?.status === "released" || reminder?.auto_release_state === "released") return false;`)
	if activeJob < 0 || terminalReminder < 0 || activeJob > terminalReminder {
		t.Fatal("active destroy jobs must take precedence over terminal reminder fields")
	}
}

func TestCleanupProfileLocalRecordsIsIdempotent(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:          "xcode-vnc",
		AppleEmail:           "user@example.com",
		Status:               ReleaseReminderStatusReleased,
		ReleasedAt:           "2026-07-01T09:00:00Z",
		AutoReleaseEnabled:   true,
		AutoReleaseState:     ReleaseReminderAutoReleaseStateRunning,
		AutoReleaseLastError: "stale release error",
	}); err != nil {
		t.Fatalf("seed stale released reminder: %v", err)
	}

	first, err := app.cleanupProfileLocalRecords("xcode-vnc", "auto-status")
	if err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	if first.Status != ReleaseReminderStatusReleased ||
		first.ReleasedAt != "2026-07-01T09:00:00Z" ||
		first.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased ||
		first.AutoReleaseLastError != "" {
		t.Fatalf("first cleanup did not converge reminder: %+v", first)
	}

	events, err := app.MemberStore.RecentEvents("user@example.com", 10)
	if err != nil {
		t.Fatalf("events after first cleanup: %v", err)
	}
	if len(events) != 1 || events[0].Action != "system.cleanup.completed" {
		t.Fatalf("events after first cleanup = %+v", events)
	}
	if events[0].Source != "system" {
		t.Fatalf("cleanup event source = %q, want system", events[0].Source)
	}
	if events[0].Message != "marked release reminder released (auto-status)" {
		t.Fatalf("first cleanup event message = %q", events[0].Message)
	}

	second, err := app.cleanupProfileLocalRecords("xcode-vnc", "auto-status")
	if err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if second.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased {
		t.Fatalf("second cleanup reminder = %+v", second)
	}
	events, err = app.MemberStore.RecentEvents("user@example.com", 10)
	if err != nil {
		t.Fatalf("events after second cleanup: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("repeated cleanup recorded duplicate events: %+v", events)
	}
}

func TestWebAWSStatusCleanupPreservesNotifyingCycleUntilCoordinatorCompletes(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	owner, err := app.MemberStore.SetupAdmin("Test Admin", "admin@example.com", "password123")
	if err != nil {
		t.Fatalf("setup owner: %v", err)
	}
	profile := validAWSProfile()
	if _, err := app.MemberStore.SetProfileOwner(profile.Name, owner.Email); err != nil {
		t.Fatalf("set profile owner: %v", err)
	}
	now := app.JobManager.Now().UTC()
	reminder := ReleaseReminder{
		ProfileName: profile.Name, AppleEmail: profile.AWS.AccountEmail, HostID: "h-1",
		ReleaseDueAt: now.Add(-2 * time.Hour).Format(time.RFC3339), OwnerEmail: owner.Email, OwnerName: owner.Name,
		LastNotifiedAt: now.Add(-time.Hour).Format(time.RFC3339), Status: ReleaseReminderStatusDueNotified,
		AutoReleaseEnabled: true, AutoReleaseAt: now.Add(-50 * time.Minute).Format(time.RFC3339),
		AutoReleaseStartedAt:     now.Add(-45 * time.Minute).Format(time.RFC3339),
		AutoReleaseLastAttemptAt: now.Add(-AutoReleaseRetryInterval).Format(time.RFC3339),
		AutoReleaseAttempts:      1, AutoReleaseState: ReleaseReminderAutoReleaseStateNotifying,
	}
	if _, err := app.MemberStore.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert notifying reminder: %v", err)
	}
	beforePoll := mustReleaseReminder(t, app, profile.Name)
	cleanStatus := AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}
	app.AWSService.NewClient = func(context.Context, MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: cleanStatus}, nil
	}

	if _, status, err := app.webAWSStatusWithCleanup(context.Background(), profile); err != nil {
		t.Fatalf("poll AWS status: %v", err)
	} else if !autoReleaseResourcesClean(status) {
		t.Fatalf("polled status is not clean: %+v", status)
	}
	if afterPoll := mustReleaseReminder(t, app, profile.Name); !reflect.DeepEqual(afterPoll, beforePoll) {
		t.Fatalf("status polling changed coordinator-owned reminder:\n got: %+v\nwant: %+v", afterPoll, beforePoll)
	}
	if persistedOwner, ok, err := app.MemberStore.ProfileOwner(profile.Name); err != nil || !ok || persistedOwner.Owner.Email != owner.Email {
		t.Fatalf("status polling changed owner: owner=%+v ok=%t err=%v", persistedOwner, ok, err)
	}

	notifications := 0
	starts := 0
	coordinator := AutoReleaseCoordinator{
		Now:   func() time.Time { return now },
		Store: app.MemberStore,
		Jobs:  app.JobManager,
		ResolveProfile: func(context.Context, ReleaseReminder) (Profile, error) {
			return profile, nil
		},
		Status: func(context.Context, Profile) (AWSStatus, error) { return cleanStatus, nil },
		StartDestroy: func(context.Context, Profile) (Job, error) {
			starts++
			return Job{}, errors.New("unexpected destroy mutation")
		},
		Notify: func(notification AutoReleaseNotification) error {
			if notification.Kind != AutoReleaseNotificationSuccess {
				t.Fatalf("notification = %+v", notification)
			}
			notifications++
			return nil
		},
	}
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("coordinator Scan: %v", err)
	}
	completed := mustReleaseReminder(t, app, profile.Name)
	if completed.Status != ReleaseReminderStatusReleased || completed.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || completed.AutoReleaseNotifiedAt == "" || notifications != 1 || starts != 0 {
		t.Fatalf("coordinator completion reminder=%+v notifications=%d starts=%d", completed, notifications, starts)
	}
	if persistedOwner, ok, err := app.MemberStore.ProfileOwner(profile.Name); err != nil || ok {
		t.Fatalf("owner after exact-cycle completion: owner=%+v ok=%t err=%v", persistedOwner, ok, err)
	}
}

func TestCleanupDefaultLocalConfigProfilesMissingFileIsNoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	backup, err := cleanupDefaultLocalConfigProfiles(
		filepath.Join(home, ".connectmac", "config.yaml"),
		time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("missing default config should be ignored: %v", err)
	}
	if backup != "" {
		t.Fatalf("backup = %q, want empty", backup)
	}
}

func TestCleanupDefaultLocalConfigProfilesInvalidFileRemainsObservable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".connectmac", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("profiles:\n  broken: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanupDefaultLocalConfigProfiles(path, time.Now()); err == nil {
		t.Fatal("invalid existing config must return an observable error")
	}
}

func TestCleanupDefaultLocalConfigProfilesUnreadablePathRemainsObservable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".connectmac", "config.yaml")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanupDefaultLocalConfigProfiles(path, time.Now()); err == nil {
		t.Fatal("existing unreadable config path must return an observable error")
	}
}

func TestCleanupLocalConfigAfterLoginLogsInvalidAsErrorAndMissingSilently(t *testing.T) {
	tests := []struct {
		name      string
		create    func(t *testing.T, path string)
		wantLogs  int
		wantLevel string
	}{
		{name: "missing", create: func(*testing.T, string) {}, wantLogs: 0},
		{
			name: "invalid",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("profiles:\n  broken: ["), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantLogs:  1,
			wantLevel: "error",
		},
		{
			name: "unreadable",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			wantLogs:  1,
			wantLevel: "error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, ".connectmac", "config.yaml")
			tt.create(t, path)
			app := newWebAutoReleaseTestApp(t)
			app.LoginConfigCleanup = true
			app.cleanupLocalConfigAfterLogin(path)
			files, err := app.LogManager.List()
			if err != nil {
				t.Fatalf("list logs: %v", err)
			}
			if tt.wantLogs == 0 {
				if len(files) != 0 {
					t.Fatalf("missing config produced logs: %+v", files)
				}
				return
			}
			entries := readTestLogEntries(t, app.LogManager)
			if len(entries) != tt.wantLogs || entries[0].Action != "web.auth.cleanup" || entries[0].Level != tt.wantLevel {
				t.Fatalf("cleanup logs = %+v", entries)
			}
		})
	}
}

func TestManualCleanupRecordsAttributedAdminEvent(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName: "xcode-vnc",
		AppleEmail:  "user@example.com",
		Status:      ReleaseReminderStatusActive,
	}); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/release-reminder/cleanup", strings.NewReader(`{"profile":"xcode-vnc"}`))
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler("").ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d body=%s", rec.Code, rec.Body.String())
	}
	page, err := app.MemberStore.QueryEvents(EventQuery{Profile: "xcode-vnc", Limit: 20})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("manual cleanup events = %+v", page.Events)
	}
	event := page.Events[0]
	if event.Action != "profile.cleanup.completed" || event.Source != "web" ||
		event.MemberEmail != "admin@example.com" || event.MemberName != "Test Admin" {
		t.Fatalf("manual cleanup attribution = %+v", event)
	}
}

func TestManualCleanupAuditFailureRollsBackFileMutation(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	if _, err := store.UpsertReleaseReminder(ReleaseReminder{
		ProfileName: "xcode-vnc",
		AppleEmail:  "user@example.com",
		Status:      ReleaseReminderStatusActive,
	}); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	if _, _, err := store.CleanupProfileRecordsAndRecordEvent(
		"xcode-vnc",
		"2026-07-30T12:00:00Z",
		"manual",
		OperationEvent{Action: "", Source: "web", Status: "success"},
	); err == nil {
		t.Fatal("invalid audit event should fail atomic cleanup")
	}
	reminder, ok, err := store.ReleaseReminder("xcode-vnc")
	if err != nil || !ok {
		t.Fatalf("load reminder: ok=%t err=%v", ok, err)
	}
	if reminder.Status != ReleaseReminderStatusActive {
		t.Fatalf("cleanup mutation committed despite audit failure: %+v", reminder)
	}
	page, err := store.QueryEvents(EventQuery{Profile: "xcode-vnc", IncludeSystem: true, Limit: 10})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("audit failure persisted events: %+v", page.Events)
	}
}

func TestDefaultEventQueryExcludesOnlySystemSource(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	for _, event := range []OperationEvent{
		{Action: "system.cleanup.completed", Profile: "xcode-vnc", Source: "system", Status: "success"},
		{Action: "legacy.manual", Profile: "xcode-vnc", Source: "web", Status: "success"},
	} {
		if err := app.MemberStore.RecordEvent(event); err != nil {
			t.Fatalf("record event: %v", err)
		}
	}
	page, err := app.MemberStore.QueryEvents(EventQuery{Profile: "xcode-vnc", Limit: 20})
	if err != nil {
		t.Fatalf("query default events: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Action != "legacy.manual" {
		t.Fatalf("default events = %+v", page.Events)
	}
	all, err := app.MemberStore.QueryEvents(EventQuery{Profile: "xcode-vnc", Limit: 20, IncludeSystem: true})
	if err != nil {
		t.Fatalf("query all events: %v", err)
	}
	if len(all.Events) != 2 {
		t.Fatalf("all events = %+v", all.Events)
	}
}

type pruneTrackingRepository struct {
	MemberRepository
	mu      sync.Mutex
	calls   int
	removed int64
	err     error
}

func (r *pruneTrackingRepository) PruneEvents(time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.removed, r.err
}

func (r *pruneTrackingRepository) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestWebBackgroundWorkersBlockedLifecycleDoesNotBlockAutoRelease(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	lifecycleStarted := make(chan struct{})
	releaseLifecycle := make(chan struct{})
	lifecycleReturned := make(chan struct{})
	autoReleaseScanned := make(chan struct{}, 1)
	var releaseLifecycleOnce sync.Once
	t.Cleanup(func() { releaseLifecycleOnce.Do(func() { close(releaseLifecycle) }) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.runWebBackgroundWorkers(ctx, "config.yaml", webBackgroundWorkerSchedule{
			lifecycleTicks:       make(chan time.Time),
			reminderTicks:        make(chan time.Time),
			lifecycleScanTimeout: time.Second,
			autoReleaseTimeout:   time.Second,
			lifecycleScan: func(context.Context, string) error {
				close(lifecycleStarted)
				defer close(lifecycleReturned)
				<-releaseLifecycle
				return nil
			},
			autoReleaseScan: func(context.Context, string, time.Time) error {
				autoReleaseScanned <- struct{}{}
				return nil
			},
		})
	}()

	select {
	case <-lifecycleStarted:
	case <-time.After(time.Second):
		t.Fatal("lifecycle scan did not start")
	}
	select {
	case <-autoReleaseScanned:
	case <-time.After(time.Second):
		t.Fatal("blocked lifecycle scan prevented auto-release scan")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background workers did not stop")
	}
	releaseLifecycleOnce.Do(func() { close(releaseLifecycle) })
	select {
	case <-lifecycleReturned:
	case <-time.After(time.Second):
		t.Fatal("noncooperative lifecycle callback did not return after release")
	}
}

func TestAutoReleaseScanTimeoutRecoversOnNextTick(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	reminderTicks := make(chan time.Time)
	releaseFirst := make(chan struct{})
	firstReturned := make(chan struct{})
	scanStarted := make(chan int32, 3)
	var releaseFirstOnce sync.Once
	t.Cleanup(func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) })
	var calls atomic.Int32
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.runWebBackgroundWorkers(ctx, "config.yaml", webBackgroundWorkerSchedule{
			lifecycleTicks:     make(chan time.Time),
			reminderTicks:      reminderTicks,
			autoReleaseTimeout: 20 * time.Millisecond,
			lifecycleScan:      func(context.Context, string) error { return nil },
			autoReleaseScan: func(context.Context, string, time.Time) error {
				active := concurrent.Add(1)
				defer concurrent.Add(-1)
				for observed := maxConcurrent.Load(); active > observed; observed = maxConcurrent.Load() {
					if maxConcurrent.CompareAndSwap(observed, active) {
						break
					}
				}
				call := calls.Add(1)
				scanStarted <- call
				if call == 1 {
					<-releaseFirst
					close(firstReturned)
					return errors.New("late first scan failure")
				}
				return nil
			},
		})
	}()

	select {
	case call := <-scanStarted:
		if call != 1 {
			t.Fatalf("initial scan call = %d, want 1", call)
		}
	case <-time.After(time.Second):
		t.Fatal("first auto-release scan did not start")
	}
	waitForTestLogAction(t, app.LogManager, "auto-release.scan.timeout")
	for i := 0; i < 2; i++ {
		select {
		case reminderTicks <- time.Now().Add(time.Duration(i+1) * time.Minute):
		case <-time.After(time.Second):
			t.Fatal("worker did not accept a tick while the timed-out scan was blocked")
		}
	}
	select {
	case call := <-scanStarted:
		t.Fatalf("scan %d started while the timed-out callback remained blocked", call)
	case <-time.After(50 * time.Millisecond):
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls while first scan blocked = %d, want 1", got)
	}

	releaseFirstOnce.Do(func() { close(releaseFirst) })
	select {
	case <-firstReturned:
	case <-time.After(time.Second):
		t.Fatal("timed-out callback did not exit after release")
	}
	select {
	case call := <-scanStarted:
		if call != 2 {
			t.Fatalf("catch-up scan call = %d, want 2", call)
		}
	case <-time.After(time.Second):
		t.Fatal("catch-up scan did not start after the timed-out callback returned")
	}
	waitForTestLogAction(t, app.LogManager, "auto-release.scan.completed")
	select {
	case call := <-scanStarted:
		t.Fatalf("unexpected extra coalesced scan %d", call)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background workers did not stop")
	}
	if got := maxConcurrent.Load(); got != 1 {
		t.Fatalf("max concurrent auto-release scans = %d, want 1", got)
	}

	var started, completed, failed, timedOut int
	for _, entry := range readTestLogEntries(t, app.LogManager) {
		switch entry.Action {
		case "auto-release.scan.started":
			started++
		case "auto-release.scan.completed":
			completed++
			if entry.Operation != "auto-release" || entry.Source != "background-worker" ||
				entry.Phase != "completed" || entry.DurationMS <= 0 {
				t.Fatalf("completed scan log = %+v", entry)
			}
		case "auto-release.scan.timeout":
			timedOut++
			if entry.Level != "warn" || entry.Operation != "auto-release" ||
				entry.Source != "background-worker" || entry.Phase != "timeout" ||
				entry.DurationMS <= 0 || entry.ErrorCode != "request_timeout" {
				t.Fatalf("timeout scan log = %+v", entry)
			}
		case "auto-release.scan.failed":
			failed++
		}
	}
	if started != 2 || completed != 1 || timedOut != 1 || failed != 0 {
		t.Fatalf("scan logs: started=%d completed=%d timeout=%d failed=%d", started, completed, timedOut, failed)
	}
}

func TestAutoReleaseScanErrorLogsFailed(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	scanErr := errors.New("database unavailable")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.runWebReminderWorker(ctx, "config.yaml", webBackgroundWorkerSchedule{
			reminderTicks:      make(chan time.Time),
			autoReleaseTimeout: time.Second,
			autoReleaseScan: func(context.Context, string, time.Time) error {
				return scanErr
			},
		})
	}()
	waitForTestLogAction(t, app.LogManager, "auto-release.scan.failed")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reminder worker did not stop")
	}

	entries := readTestLogEntries(t, app.LogManager)
	if len(entries) != 2 || entries[0].Action != "auto-release.scan.started" {
		t.Fatalf("scan logs = %+v", entries)
	}
	failed := entries[1]
	if failed.Level != "error" || failed.Action != "auto-release.scan.failed" ||
		failed.Operation != "auto-release" || failed.Source != "background-worker" ||
		failed.Phase != "failed" || failed.DurationMS <= 0 ||
		failed.ErrorCode != "storage_error" || failed.Message != scanErr.Error() {
		t.Fatalf("failed scan log = %+v", failed)
	}
}

func waitForTestLogAction(t *testing.T, manager LogManager, action string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	needle := `"action":"` + action + `"`
	for time.Now().Before(deadline) {
		files, err := manager.List()
		if err != nil {
			t.Fatalf("list logs: %v", err)
		}
		if len(files) == 1 {
			data, err := os.ReadFile(files[0].Path)
			if err != nil {
				t.Fatalf("read logs: %v", err)
			}
			if strings.Contains(string(data), needle) {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("log action %q was not written", action)
}

func TestWebBackgroundWorkerPrunesEventsDailyAndLogsSummary(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	tracking := &pruneTrackingRepository{MemberRepository: app.MemberStore, removed: 7}
	app.MemberStore = tracking
	app.WebAWSLifecycleScan = func(context.Context, string) error { return nil }
	lifecycleTicks := make(chan time.Time)
	reminderTicks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.runWebBackgroundWorker(ctx, "config.yaml", lifecycleTicks, reminderTicks)
	}()
	deadline := time.Now().Add(time.Second)
	for tracking.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if tracking.callCount() != 1 {
		cancel()
		t.Fatalf("initial prune calls = %d, want 1", tracking.callCount())
	}
	reminderTicks <- time.Now().Add(time.Hour)
	time.Sleep(10 * time.Millisecond)
	if tracking.callCount() != 1 {
		cancel()
		t.Fatalf("same-day prune calls = %d, want 1", tracking.callCount())
	}
	reminderTicks <- time.Now().Add(25 * time.Hour)
	deadline = time.Now().Add(time.Second)
	for tracking.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if tracking.callCount() != 2 {
		t.Fatalf("next-day prune calls = %d, want 2", tracking.callCount())
	}
	var summaries int
	for _, entry := range readTestLogEntries(t, app.LogManager) {
		if entry.Action == "audit.prune" {
			summaries++
		}
	}
	if summaries != 2 {
		t.Fatalf("prune summary logs = %d, want 2", summaries)
	}
}

func TestPruneWebAuditEventsLogsOnlyDeletionOrFailure(t *testing.T) {
	tests := []struct {
		name      string
		removed   int64
		err       error
		wantLogs  int
		wantLevel string
		wantPhase string
	}{
		{name: "nothing deleted", wantLogs: 0},
		{name: "deleted", removed: 3, wantLogs: 1, wantLevel: "info", wantPhase: "completed"},
		{name: "failed", err: errors.New("storage unavailable"), wantLogs: 1, wantLevel: "error", wantPhase: "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newWebAutoReleaseTestApp(t)
			tracking := &pruneTrackingRepository{
				MemberRepository: app.MemberStore,
				removed:          tt.removed,
				err:              tt.err,
			}
			app.MemberStore = tracking
			app.pruneWebAuditEvents(time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC))
			files, err := app.LogManager.List()
			if err != nil {
				t.Fatalf("list logs: %v", err)
			}
			if tt.wantLogs == 0 {
				if len(files) != 0 {
					t.Fatalf("unexpected prune logs: %+v", files)
				}
				return
			}
			entries := readTestLogEntries(t, app.LogManager)
			if len(entries) != 1 || entries[0].Action != "audit.prune" ||
				entries[0].Level != tt.wantLevel || entries[0].Phase != tt.wantPhase {
				t.Fatalf("prune logs = %+v", entries)
			}
		})
	}
}

func TestCleanupProfileLocalRecordsConcurrentCallsRecordOneEvent(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:          "xcode-vnc",
		AppleEmail:           "user@example.com",
		Status:               ReleaseReminderStatusReleased,
		AutoReleaseState:     ReleaseReminderAutoReleaseStateRunning,
		AutoReleaseLastError: "stale release error",
	}); err != nil {
		t.Fatalf("seed stale released reminder: %v", err)
	}

	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := app.cleanupProfileLocalRecords("xcode-vnc", "auto-status")
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent cleanup: %v", err)
		}
	}

	events, err := app.MemberStore.RecentEvents("user@example.com", 10)
	if err != nil {
		t.Fatalf("events after concurrent cleanup: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("concurrent cleanup recorded duplicate events: %+v", events)
	}
}

func TestWebAWSStatusCleanupSharesLifecycleProfileLock(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	profile := validAWSProfile()
	statusStarted := make(chan struct{})
	app.AWSService.NewClient = func(context.Context, MacPlan) (AWSClient, error) {
		close(statusStarted)
		return &fakeAWSClient{}, nil
	}

	lockEntered := make(chan struct{})
	releaseLock := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- app.JobManager.WithProfileOperation(profile.Name, func() error {
			close(lockEntered)
			<-releaseLock
			return nil
		})
	}()
	<-lockEntered

	statusDone := make(chan error, 1)
	go func() {
		_, _, err := app.webAWSStatusWithCleanup(context.Background(), profile)
		statusDone <- err
	}()
	select {
	case <-statusStarted:
		t.Fatal("AWS status started before the lifecycle profile lock was released")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseLock)
	if err := <-lockDone; err != nil {
		t.Fatalf("release lifecycle lock: %v", err)
	}
	select {
	case err := <-statusDone:
		if err != nil {
			t.Fatalf("status with cleanup: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("status cleanup did not continue after lifecycle lock release")
	}
}

func TestWebAWSStatusKeepsValidStatusWhenCleanupFails(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	app.MemberStore = failingCleanupRepository{
		MemberRepository: app.MemberStore,
		err:              errors.New("cleanup storage unavailable"),
	}
	app.AWSService.NewClient = func(context.Context, MacPlan) (AWSClient, error) {
		return &fakeAWSClient{}, nil
	}

	_, status, err := app.webAWSStatusWithCleanup(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("AWS status was replaced by cleanup error: %v", err)
	}
	if len(status.Hosts) != 0 || len(status.Instances) != 0 {
		t.Fatalf("unexpected stopped status: %+v", status)
	}
}

func TestWebAutoReleaseDialogAndSubmissionContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)

	for _, want := range []string{
		`到期提醒后等待10分钟`,
		`失败每5分钟重试`,
		`最多1小时`,
		`永久保留弹性IP`,
		`取消已安排或正在重试的自动释放周期`,
		`自动释放已经开始时无法撤回`,
		`state.pendingAutoRelease = { profile: p.name, enabled };`,
		`const pending = state.pendingAutoRelease;`,
		`if (!pending || pending.profile !== state.selected)`,
		`state.autoReleaseSubmitting = true;`,
		`$("autoReleaseConfirmBtn").disabled = state.autoReleaseSubmitting;`,
		`body: JSON.stringify({ profile: pending.profile, enabled: pending.enabled })`,
		`await loadReleaseReminders({ required: true });`,
		`closeAutoReleaseDialog();`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web auto release dialog missing %q", want)
		}
	}
}

func TestWebAutoReleaseRemoteModeRoutesReminderAPIs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	start := strings.Index(html, "function remoteUserAPIPath(path)")
	if start < 0 {
		t.Fatal("remote user API routing functions are missing")
	}
	end := strings.Index(html[start:], "function apiURL(path)")
	if end < 0 {
		t.Fatal("remote user API routing functions are missing")
	}
	routing := html[start : start+end]
	for _, path := range []string{`path === "/api/release-reminders"`, `path === "/api/release-reminder/auto-release"`} {
		if !strings.Contains(routing, path) {
			t.Fatalf("remote user API routing missing %q", path)
		}
	}
	if !strings.Contains(html, `return "/api/user-proxy" + path;`) {
		t.Fatal("remote user API paths do not use the user proxy")
	}
}

func TestRemoteUserAPIPathAllowsReleaseReminderRoutes(t *testing.T) {
	for _, path := range []string{
		"/api/release-reminders",
		"/api/release-reminder/auto-release",
	} {
		if !isRemoteUserAPIPath(path) {
			t.Errorf("remote user API path %q is not allowed", path)
		}
	}
	for _, path := range []string{
		"/api/release-reminder/cleanup",
		"/api/release-reminder/auto-release/extra",
		"/api/aws/destroy",
	} {
		if isRemoteUserAPIPath(path) {
			t.Errorf("unexpected remote user API path %q is allowed", path)
		}
	}
}

func TestWebAutoReleaseModalRejectsChangedSelection(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	start := strings.Index(html, "async function submitAutoRelease()")
	if start < 0 {
		t.Fatal("auto release submit function is missing")
	}
	end := strings.Index(html[start:], "async function saveReleaseReminder()")
	if end < 0 {
		t.Fatal("auto release submit function is missing")
	}
	submit := html[start : start+end]
	guard := strings.Index(submit, `if (!pending || pending.profile !== state.selected)`)
	mutation := strings.Index(submit, `api("/api/release-reminder/auto-release"`)
	if guard < 0 || mutation < 0 || guard > mutation {
		t.Fatal("changed selection is not rejected before the auto release mutation")
	}
	if strings.Contains(submit, `profile: p.name`) {
		t.Fatal("auto release submit must not read the current selected profile")
	}
	if !strings.Contains(submit, `body: JSON.stringify({ profile: pending.profile, enabled: pending.enabled })`) {
		t.Fatal("auto release submit does not use the captured modal target")
	}
}

func TestWebAutoReleaseRefreshAndPollingContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`async function loadReleaseReminders(options = {})`,
		`if (options.required) throw err;`,
		`setStatus("提醒状态刷新失败，将继续重试");`,
		`return false;`,
		`autoReleasePollingNeeded()`,
		`function jobPollingNeeded()`,
		`await loadJobs({ refreshReminders: true });`,
		`setStatus("任务状态刷新失败，将继续重试");`,
		`} finally {`,
		`if (jobPollingNeeded()) {`,
		`if (jobRefreshTimer) return;`,
		`auto_release_state === "scheduled" || reminder.auto_release_state === "running" || reminder.auto_release_state === "retrying"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web auto release refresh/polling contract missing %q", want)
		}
	}
	reminderLoaderStart := strings.Index(html, "async function loadReleaseReminders(options = {})")
	if reminderLoaderStart < 0 {
		t.Fatal("release reminder loader is missing")
	}
	reminderLoaderEnd := strings.Index(html[reminderLoaderStart:], "async function loadProfileOwners(options = {})")
	if reminderLoaderEnd < 0 {
		t.Fatal("release reminder loader boundary is missing")
	}
	reminderLoader := html[reminderLoaderStart : reminderLoaderStart+reminderLoaderEnd]
	if strings.Contains(reminderLoader, "state.reminders = {};\n        if (options.required)") {
		t.Fatal("optional reminder refresh must preserve existing reminders")
	}

	pollingStart := strings.Index(html, "function scheduleJobRefresh()")
	if pollingStart < 0 {
		t.Fatal("job polling function is missing")
	}
	pollingEnd := strings.Index(html[pollingStart:], "async function loadJobLog(")
	if pollingEnd < 0 {
		t.Fatal("job polling boundary is missing")
	}
	polling := html[pollingStart : pollingStart+pollingEnd]
	if !strings.Contains(polling, "try {") || !strings.Contains(polling, "} catch (err) {") || !strings.Contains(polling, "} finally {") {
		t.Fatal("job polling must catch failures and schedule from finally")
	}
	if strings.Count(polling, "scheduleJobRefresh();") < 1 {
		t.Fatal("job polling must schedule another attempt after a failed refresh")
	}
}

func TestWebAutoReleaseDialogAccessibilityContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`role="dialog" aria-modal="true" aria-labelledby="autoReleaseTitle" aria-describedby="autoReleaseDialogCopy autoReleaseSafetyCopy"`,
		`openDialog($("autoReleaseLayer"), $("autoReleaseConfirmBtn"));`,
		`closeDialog($("autoReleaseLayer"));`,
		`if (event.key === "Escape")`,
		`if (event.key !== "Tab") return;`,
		`focusable[focusable.length - 1].focus();`,
		`focusable[0].focus();`,
		`dialogTriggers.set(layer, document.activeElement instanceof HTMLElement ? document.activeElement : null);`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web auto release accessibility contract missing %q", want)
		}
	}
}

func TestWebAutoReleaseMobileAndRoleContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)

	for _, want := range []string{
		`class="auto-release-strip"`,
		`@media (max-width: 720px)`,
		`.auto-release-strip { align-items: stretch; flex-direction: column; }`,
		`.auto-release-actions { width: 100%; }`,
		`$("autoReleaseSummary").textContent = autoReleaseStateText(reminder, profile);`,
		`const ready = !!(profile && profileReady(profile.name));`,
		`!profile || !reminder || !ready || state.busy`,
		`Mac 已运行，可重新设置自动释放`,
		`applyWorkbenchAction("extendReminderBtn", model.actions.extend`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web auto release role/mobile contract missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`id="autoReleaseToggleBtn" class="admin-only"`,
		`$("autoReleaseToggleBtn").classList.toggle("hidden", !isAdmin());`,
		`if (!p || !reminder || !isAdmin() || state.autoReleaseSubmitting) return;`,
		`if (!isAdmin() || state.autoReleaseSubmitting) return;`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("web auto release member access remains admin-only: found %q", forbidden)
		}
	}
	if strings.Contains(html, `class="auto-release-strip local-action"`) {
		t.Fatal("auto release strip must not depend on the local agent")
	}
}

func TestAppWebAutoReleaseToggleAdminEnableDisable(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		seed    ReleaseReminder
	}{
		{
			name:    "enable does not schedule",
			enabled: true,
			seed: ReleaseReminder{
				ProfileName: "xcode-vnc", AppleEmail: "user@example.com", Status: ReleaseReminderStatusActive,
			},
		},
		{
			name:    "disable cancels scheduled cycle",
			enabled: false,
			seed: ReleaseReminder{
				ProfileName: "xcode-vnc", AppleEmail: "user@example.com", Status: ReleaseReminderStatusDueNotified,
				AutoReleaseEnabled: true, AutoReleaseAt: "2026-07-01T12:40:45Z",
				AutoReleaseState: ReleaseReminderAutoReleaseStateScheduled,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := newWebAutoReleaseTestApp(t)
			if _, err := app.MemberStore.UpsertReleaseReminder(test.seed); err != nil {
				t.Fatalf("upsert reminder: %v", err)
			}

			rec := postWebAutoRelease(t, &app, "admin", test.seed.ProfileName, test.enabled)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			reminder := mustReleaseReminder(t, app, test.seed.ProfileName)
			if reminder.AutoReleaseEnabled != test.enabled {
				t.Fatalf("enabled = %t, want %t", reminder.AutoReleaseEnabled, test.enabled)
			}
			if reminder.AutoReleaseAt != "" || reminder.AutoReleaseStartedAt != "" || reminder.AutoReleaseLastAttemptAt != "" || reminder.AutoReleaseAttempts != 0 || reminder.AutoReleaseLastError != "" || reminder.AutoReleaseState != "" {
				t.Fatalf("automatic release cycle was not clear: %+v", reminder)
			}
			if !strings.Contains(rec.Body.String(), `"auto_release_enabled":`+map[bool]string{true: "true", false: "false"}[test.enabled]) {
				t.Fatalf("response does not contain updated reminder: %s", rec.Body.String())
			}

			events, err := app.MemberStore.RecentEvents(test.seed.AppleEmail, 10)
			if err != nil {
				t.Fatalf("recent events: %v", err)
			}
			wantState := "disabled"
			if test.enabled {
				wantState = "enabled"
			}
			if len(events) != 1 || events[0].Action != "release-reminder.auto-release."+wantState || events[0].Profile != test.seed.ProfileName || events[0].AppleEmail != test.seed.AppleEmail || events[0].MemberID == "" || events[0].MemberEmail != "admin@example.com" || events[0].MemberName != "Test Admin" || events[0].Status != "success" || !strings.Contains(events[0].Message, "admin@example.com") || !strings.Contains(events[0].Message, wantState) {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}

func TestAppWebAutoReleaseReactivatesReleasedReminderForReadyMac(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Instances: []InstanceStatus{{
				InstanceID:          "i-ready",
				State:               "running",
				SystemStatus:        "ok",
				InstanceStatusCheck: "ok",
				EBSStatus:           "ok",
				Tags:                managedTestTags(),
			}},
			ElasticIP: ElasticIP{InstanceID: "i-ready", PublicIP: "54.1.2.3", AllocationID: "eipalloc-1"},
		}}, nil
	}
	if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:        "xcode-vnc",
		AppleEmail:         "user@example.com",
		HostID:             "h-1",
		ReleaseDueAt:       "2026-07-01T08:00:00Z",
		Status:             ReleaseReminderStatusReleased,
		ReleasedAt:         "2026-07-01T09:00:00Z",
		AutoReleaseEnabled: true,
		AutoReleaseState:   ReleaseReminderAutoReleaseStateReleased,
	}); err != nil {
		t.Fatalf("seed released reminder: %v", err)
	}

	body := strings.NewReader(`{"profile":"xcode-vnc","enabled":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/release-reminder/auto-release", body)
	addWebAuth(t, &app, req, "admin")
	if _, err := app.MemberStore.SetProfileOwner("xcode-vnc", "admin@example.com"); err != nil {
		t.Fatalf("set profile owner: %v", err)
	}
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := mustReleaseReminder(t, app, "xcode-vnc")
	if got.Status != ReleaseReminderStatusActive || got.ReleasedAt != "" || !got.AutoReleaseEnabled || got.AutoReleaseState != "" {
		t.Fatalf("released reminder was not reactivated: %+v", got)
	}
	if got.OwnerEmail != "admin@example.com" || got.OwnerName != "Test Admin" {
		t.Fatalf("reactivated owner = %+v", got)
	}
	wantDue := time.Date(2026, 7, 2, 12, 30, 45, 0, time.UTC).Format(time.RFC3339)
	if got.ReleaseDueAt != wantDue {
		t.Fatalf("release due at = %q, want %q", got.ReleaseDueAt, wantDue)
	}
}

func TestAppWebAutoReleaseRejectsReleasedReminderWithoutCurrentProfileOwner(t *testing.T) {
	dir := t.TempDir()
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	app.AWSService.NewClient = func(ctx context.Context, plan MacPlan) (AWSClient, error) {
		return &fakeAWSClient{status: AWSStatus{
			Instances: []InstanceStatus{{
				InstanceID:          "i-ready",
				State:               "running",
				SystemStatus:        "ok",
				InstanceStatusCheck: "ok",
				EBSStatus:           "ok",
				Tags:                managedTestTags(),
			}},
			ElasticIP: ElasticIP{InstanceID: "i-ready", PublicIP: "54.1.2.3", AllocationID: "eipalloc-1"},
		}}, nil
	}
	seed := ReleaseReminder{
		ProfileName:        "xcode-vnc",
		AppleEmail:         "user@example.com",
		HostID:             "h-1",
		OwnerEmail:         "historical-owner@example.com",
		OwnerName:          "Historical Owner",
		ReleaseDueAt:       "2026-07-01T08:00:00Z",
		Status:             ReleaseReminderStatusReleased,
		ReleasedAt:         "2026-07-01T09:00:00Z",
		AutoReleaseEnabled: false,
		AutoReleaseState:   ReleaseReminderAutoReleaseStateReleased,
	}
	saved, err := app.MemberStore.UpsertReleaseReminder(seed)
	if err != nil {
		t.Fatalf("seed released reminder: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/release-reminder/auto-release", strings.NewReader(`{"profile":"xcode-vnc","enabled":true}`))
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), errAutoReleaseOwnerMissing.Error()) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := mustReleaseReminder(t, app, seed.ProfileName)
	if !reflect.DeepEqual(got, saved) {
		t.Fatalf("released reminder changed:\n got: %+v\nwant: %+v", got, saved)
	}
	events, err := app.MemberStore.RecentEvents(seed.AppleEmail, 10)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("failed reactivation recorded events: %+v", events)
	}
}

func TestAppWebAutoReleaseToggleRoleAndValidation(t *testing.T) {
	t.Run("profile required", func(t *testing.T) {
		app := newWebAutoReleaseTestApp(t)
		rec := postWebAutoRelease(t, &app, "admin", " ", true)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "profile is required") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("reminder missing", func(t *testing.T) {
		app := newWebAutoReleaseTestApp(t)
		rec := postWebAutoRelease(t, &app, "admin", "missing", true)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "release reminder not found") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})

	for _, body := range []string{
		`{"profile":"xcode-vnc"}`,
		`{"profile":"xcode-vnc","enabled":null}`,
	} {
		t.Run("enabled required "+body, func(t *testing.T) {
			app := newWebAutoReleaseTestApp(t)
			seedWebAutoReleaseReminder(t, app, ReleaseReminderStatusActive)
			rec := postWebAutoReleaseBody(t, &app, "admin", body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "enabled is required") {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAppWebAutoReleaseToggleAssignedMemberAccess(t *testing.T) {
	for _, role := range []string{"operator", "viewer"} {
		t.Run(role+" assigned", func(t *testing.T) {
			app := newWebAutoReleaseTestApp(t)
			seedWebAutoReleaseReminderWithState(t, app, ReleaseReminderStatusActive, ReleaseReminderAutoReleaseStateScheduled)
			if _, err := app.MemberStore.UpsertManagedProfile(Profile{Name: "xcode-vnc"}); err != nil {
				t.Fatalf("upsert managed profile: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/release-reminder/auto-release", strings.NewReader(`{"profile":"xcode-vnc","enabled":false}`))
			addWebAuth(t, &app, req, role)
			if _, err := app.MemberStore.AssignProfileAccess("xcode-vnc", "admin@example.com"); err != nil {
				t.Fatalf("assign profile access: %v", err)
			}
			rec := httptest.NewRecorder()
			app.newWebHandler("").ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})

		t.Run(role+" unassigned", func(t *testing.T) {
			app := newWebAutoReleaseTestApp(t)
			seedWebAutoReleaseReminder(t, app, ReleaseReminderStatusActive)
			if _, err := app.MemberStore.UpsertManagedProfile(Profile{Name: "xcode-vnc"}); err != nil {
				t.Fatalf("upsert managed profile: %v", err)
			}

			rec := postWebAutoRelease(t, &app, role, "xcode-vnc", false)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAppWebAutoReleaseEnableSchedulesUnscheduledDueReminder(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	now := app.JobManager.Now().UTC()
	seed := ReleaseReminder{
		ProfileName: "xcode-vnc", AppleEmail: "user@example.com", HostID: "h-1",
		ReleaseDueAt: "2026-07-01T12:20:45Z", LastNotifiedAt: "2026-07-01T12:30:45Z",
		Status: ReleaseReminderStatusDueNotified, AutoReleaseEnabled: false,
		AutoReleaseStartedAt: "preserve-start", AutoReleaseLastAttemptAt: "preserve-attempt",
		AutoReleaseAttempts: 2, AutoReleaseLastError: "preserve-error",
	}
	if _, err := app.MemberStore.UpsertReleaseReminder(seed); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	rec := postWebAutoRelease(t, &app, "admin", seed.ProfileName, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := mustReleaseReminder(t, app, seed.ProfileName)
	if !got.AutoReleaseEnabled || got.AutoReleaseAt != now.Add(AutoReleaseGracePeriod).Format(time.RFC3339) || got.AutoReleaseState != ReleaseReminderAutoReleaseStateScheduled {
		t.Fatalf("due reminder was not scheduled: %+v", got)
	}
	if got.ReleaseDueAt != seed.ReleaseDueAt || got.LastNotifiedAt != seed.LastNotifiedAt || got.AutoReleaseStartedAt != seed.AutoReleaseStartedAt || got.AutoReleaseLastAttemptAt != seed.AutoReleaseLastAttemptAt || got.AutoReleaseAttempts != seed.AutoReleaseAttempts || got.AutoReleaseLastError != seed.AutoReleaseLastError {
		t.Fatalf("due cycle fields changed: got=%+v seed=%+v", got, seed)
	}
}

func TestAppWebAutoReleaseEnablePreservesRunningCycleWithActiveDestroyJob(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	seed := ReleaseReminder{
		ProfileName: "xcode-vnc", AppleEmail: "user@example.com", Status: ReleaseReminderStatusDueNotified,
		AutoReleaseEnabled: false, AutoReleaseAt: "2026-07-01T12:40:45Z",
		AutoReleaseStartedAt: "2026-07-01T12:30:45Z", AutoReleaseLastAttemptAt: "2026-07-01T12:35:45Z",
		AutoReleaseAttempts: 3, AutoReleaseLastError: "destroy in progress", AutoReleaseState: ReleaseReminderAutoReleaseStateRunning,
	}
	saved, err := app.MemberStore.UpsertReleaseReminder(seed)
	if err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	if _, err := app.JobManager.Create(Job{ID: "active-destroy", Type: "aws-destroy", Profile: seed.ProfileName, Status: JobStatusRunning, PID: 42}); err != nil {
		t.Fatalf("create active job: %v", err)
	}

	rec := postWebAutoRelease(t, &app, "admin", seed.ProfileName, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := mustReleaseReminder(t, app, seed.ProfileName)
	if !got.AutoReleaseEnabled {
		t.Fatal("auto release was not enabled")
	}
	got.AutoReleaseEnabled = saved.AutoReleaseEnabled
	got.UpdatedAt = saved.UpdatedAt
	if !reflect.DeepEqual(got, saved) {
		t.Fatalf("enable changed fields beyond flag:\n got: %+v\nwant: %+v", got, saved)
	}
}

func TestAppWebAutoReleaseToggleRejectsProtectedReleaseStatesOrJob(t *testing.T) {
	for _, test := range []struct {
		name      string
		seed      ReleaseReminder
		activeJob bool
	}{
		{
			name: "running state",
			seed: ReleaseReminder{ProfileName: "xcode-vnc", AutoReleaseEnabled: true, AutoReleaseAt: "2026-07-01T12:40:45Z", AutoReleaseAttempts: 1, AutoReleaseState: ReleaseReminderAutoReleaseStateRunning},
		},
		{
			name: "retrying state",
			seed: ReleaseReminder{ProfileName: "xcode-vnc", AutoReleaseEnabled: true, AutoReleaseAt: "2026-07-01T12:40:45Z", AutoReleaseAttempts: 2, AutoReleaseState: ReleaseReminderAutoReleaseStateRetrying},
		},
		{
			name: "notifying state with marker",
			seed: ReleaseReminder{ProfileName: "xcode-vnc", AutoReleaseEnabled: true, AutoReleaseAt: "2026-07-01T12:40:45Z", AutoReleaseAttempts: 2, AutoReleaseState: ReleaseReminderAutoReleaseStateNotifying, AutoReleaseNotifiedAt: "2026-07-01T12:45:45Z"},
		},
		{
			name:      "active destroy job",
			seed:      ReleaseReminder{ProfileName: "xcode-vnc", AutoReleaseEnabled: true, AutoReleaseAt: "2026-07-01T12:40:45Z", AutoReleaseState: ReleaseReminderAutoReleaseStateScheduled},
			activeJob: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := newWebAutoReleaseTestApp(t)
			if _, err := app.MemberStore.UpsertReleaseReminder(test.seed); err != nil {
				t.Fatalf("upsert reminder: %v", err)
			}
			if test.activeJob {
				if _, err := app.JobManager.Create(Job{ID: "active-destroy", Type: "aws-destroy", Profile: "xcode-vnc", Status: JobStatusRunning, PID: 42}); err != nil {
					t.Fatalf("create active job: %v", err)
				}
			}

			rec := postWebAutoRelease(t, &app, "admin", "xcode-vnc", false)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "automatic release") {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := mustReleaseReminder(t, app, "xcode-vnc")
			if !got.AutoReleaseEnabled || got.AutoReleaseAt != test.seed.AutoReleaseAt || got.AutoReleaseState != test.seed.AutoReleaseState || got.AutoReleaseAttempts != test.seed.AutoReleaseAttempts || got.AutoReleaseNotifiedAt != test.seed.AutoReleaseNotifiedAt {
				t.Fatalf("running release was modified: %+v", got)
			}
		})
	}
}

func TestAppWebOpenRejectsProfileWhileReleaseIsActive(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     string
		activeJob bool
	}{
		{name: "running reminder", state: ReleaseReminderAutoReleaseStateRunning},
		{name: "retrying reminder", state: ReleaseReminderAutoReleaseStateRetrying},
		{name: "notification pending", state: ReleaseReminderAutoReleaseStateNotifying},
		{name: "destroy job", activeJob: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := newWebAutoReleaseTestApp(t)
			if test.state != "" {
				if _, err := app.MemberStore.UpsertReleaseReminder(ReleaseReminder{ProfileName: "xcode-vnc", Status: ReleaseReminderStatusDueNotified, AutoReleaseEnabled: true, AutoReleaseState: test.state}); err != nil {
					t.Fatalf("upsert reminder: %v", err)
				}
			}
			if test.activeJob {
				if _, err := app.JobManager.Create(Job{ID: "destroy-active", Type: "aws-destroy", Profile: "xcode-vnc", Status: JobStatusRunning, PID: 42}); err != nil {
					t.Fatalf("create job: %v", err)
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/api/aws/open", strings.NewReader(`{"profile":"xcode-vnc","confirm":true,"background":true}`))
			addWebAuth(t, &app, req, "admin")
			rec := httptest.NewRecorder()
			app.newWebHandler("").ServeHTTP(rec, req)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "currently releasing") {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAppAutoReleaseSuccessNotificationUsesWechatWebhook(t *testing.T) {
	var markdown string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Markdown struct {
				Content string `json:"content"`
			} `json:"markdown"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode webhook payload: %v", err)
		}
		markdown = payload.Markdown.Content
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()
	t.Setenv(envWechatWebhookURL, server.URL)

	app := newWebAutoReleaseTestApp(t)
	coordinator := app.newAutoReleaseCoordinator("")
	err := coordinator.Notify(AutoReleaseNotification{Kind: AutoReleaseNotificationSuccess, Reminder: ReleaseReminder{ProfileName: "xcode-vnc", AppleEmail: "user@example.com", HostID: "h-1", HostArchitecture: "x86"}})
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if !strings.Contains(markdown, "Mac 自动释放成功，Elastic IP 分配已保留") || !strings.Contains(markdown, "Apple：user@example.com") || !strings.Contains(markdown, "Host 架构类型：x86") || strings.Contains(markdown, "ConnectMac") || strings.Contains(markdown, "Profile：") || strings.Contains(markdown, "xcode-vnc") {
		t.Fatalf("markdown = %q", markdown)
	}
}

func TestAppWebReleaseReminderExtendBoundaryAndCycleReset(t *testing.T) {
	serverNow := time.Date(2026, 7, 1, 12, 30, 45, 0, time.UTC)
	for _, test := range []struct {
		name       string
		dueAt      time.Time
		wantStatus int
	}{
		{name: "less than ten minutes", dueAt: serverNow.Add(AutoReleaseGracePeriod - time.Second), wantStatus: http.StatusBadRequest},
		{name: "exactly ten minutes", dueAt: serverNow.Add(AutoReleaseGracePeriod), wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := newWebAutoReleaseTestApp(t)
			seedWebAutoReleaseReminderWithState(t, app, ReleaseReminderStatusDueNotified, ReleaseReminderAutoReleaseStateScheduled)
			rec := postWebExtension(t, &app, "admin", test.dueAt)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, test.wantStatus, rec.Body.String())
			}
		})
	}

	app := newWebAutoReleaseTestApp(t)
	seedWebAutoReleaseReminderWithState(t, app, ReleaseReminderStatusDueNotified, ReleaseReminderAutoReleaseStateScheduled)
	dueAt := serverNow.Add(time.Hour)
	rec := postWebExtension(t, &app, "admin", dueAt)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := mustReleaseReminder(t, app, "xcode-vnc")
	if got.ReleaseDueAt != dueAt.Format(time.RFC3339) || got.LastExtendedAt != serverNow.Format(time.RFC3339) || got.Status != ReleaseReminderStatusActive || got.LastNotifiedAt != "" || !got.AutoReleaseEnabled || got.AutoReleaseAt != "" || got.AutoReleaseStartedAt != "" || got.AutoReleaseLastAttemptAt != "" || got.AutoReleaseAttempts != 0 || got.AutoReleaseLastError != "" || got.AutoReleaseState != "" {
		t.Fatalf("extended reminder = %+v", got)
	}
}

func TestAppWebReleaseReminderExtendNotificationUsesBeijingDisplayTime(t *testing.T) {
	var markdown string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Markdown struct {
				Content string `json:"content"`
			} `json:"markdown"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode webhook payload: %v", err)
		}
		markdown = payload.Markdown.Content
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()
	t.Setenv(envWechatWebhookURL, server.URL)

	app := newWebAutoReleaseTestApp(t)
	reminder := seedWebAutoReleaseReminderWithState(t, app, ReleaseReminderStatusDueNotified, ReleaseReminderAutoReleaseStateScheduled)
	reminder.ReleaseDueAt = "2026-07-17T09:17:07Z"
	if _, err := app.MemberStore.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("update reminder: %v", err)
	}

	dueAt := time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC)
	rec := postWebExtension(t, &app, "admin", dueAt)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(markdown, "释放提醒已延长（原时间：2026-07-17 17:17:07（北京时间））") {
		t.Fatalf("notification description = %q", markdown)
	}
	if strings.Contains(markdown, "T09:17:07Z") {
		t.Fatalf("notification description leaked UTC timestamp: %q", markdown)
	}

	var response struct {
		Data struct {
			Reminder ReleaseReminder `json:"reminder"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	wantDueAt := dueAt.Format(time.RFC3339)
	if response.Data.Reminder.ReleaseDueAt != wantDueAt {
		t.Fatalf("response release_due_at = %q, want %q", response.Data.Reminder.ReleaseDueAt, wantDueAt)
	}
	if persisted := mustReleaseReminder(t, app, reminder.ProfileName); persisted.ReleaseDueAt != wantDueAt {
		t.Fatalf("persisted release_due_at = %q, want %q", persisted.ReleaseDueAt, wantDueAt)
	}
}

func TestAppWebReleaseReminderExtendRejectsProtectedStatesOrJob(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     string
		activeJob bool
	}{
		{name: "running state", state: ReleaseReminderAutoReleaseStateRunning},
		{name: "retrying state", state: ReleaseReminderAutoReleaseStateRetrying},
		{name: "notifying state", state: ReleaseReminderAutoReleaseStateNotifying},
		{name: "active destroy job", state: ReleaseReminderAutoReleaseStateScheduled, activeJob: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := newWebAutoReleaseTestApp(t)
			reminder := seedWebAutoReleaseReminder(t, app, ReleaseReminderStatusDueNotified)
			var err error
			reminder, err = app.MemberStore.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
				current.AutoReleaseState = test.state
				if test.state == ReleaseReminderAutoReleaseStateNotifying {
					current.AutoReleaseNotifiedAt = "2026-07-01T12:36:45Z"
				}
				return current, nil
			})
			if err != nil {
				t.Fatalf("update reminder: %v", err)
			}
			if test.activeJob {
				if _, err := app.JobManager.Create(Job{ID: "active-destroy", Type: "aws-destroy", Profile: "xcode-vnc", Status: JobStatusRunning, PID: 42}); err != nil {
					t.Fatalf("create active job: %v", err)
				}
			}
			rec := postWebExtension(t, &app, "admin", app.JobManager.Now().Add(time.Hour))
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "automatic release") {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := mustReleaseReminder(t, app, "xcode-vnc")
			if got.ReleaseDueAt != reminder.ReleaseDueAt || got.AutoReleaseState != reminder.AutoReleaseState || got.AutoReleaseAt != reminder.AutoReleaseAt || got.AutoReleaseNotifiedAt != reminder.AutoReleaseNotifiedAt {
				t.Fatalf("running release was modified: %+v", got)
			}
		})
	}
}

func TestAppWebReleaseReminderExtensionWinsAtomicRaceWithAutoClaim(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	now := app.JobManager.Now()
	reminder := seedWebAutoReleaseReminderWithState(t, app, ReleaseReminderStatusDueNotified, ReleaseReminderAutoReleaseStateScheduled)
	reminder, err := app.MemberStore.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		current.ReleaseDueAt = now.Add(-AutoReleaseGracePeriod).Format(time.RFC3339)
		current.AutoReleaseAt = now.Format(time.RFC3339)
		return current, nil
	})
	if err != nil {
		t.Fatalf("update reminder: %v", err)
	}

	gate := &firstUpdateGateRepository{
		MemberRepository: app.MemberStore,
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	app.MemberStore = gate
	unexpectedAWS := errors.New("auto claim reached AWS after extension won")
	coordinator := AutoReleaseCoordinator{
		Now:   func() time.Time { return now },
		Store: gate,
		Jobs:  app.JobManager,
		ResolveProfile: func(context.Context, ReleaseReminder) (Profile, error) {
			return Profile{}, unexpectedAWS
		},
		Status: func(context.Context, Profile) (AWSStatus, error) {
			return AWSStatus{}, unexpectedAWS
		},
		StartDestroy: func(context.Context, Profile) (Job, error) {
			return Job{}, unexpectedAWS
		},
	}

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- postWebExtension(t, &app, "admin", now.Add(time.Hour))
	}()
	<-gate.entered
	claimDone := make(chan error, 1)
	go func() { claimDone <- coordinator.Scan(context.Background()) }()
	close(gate.release)

	rec := <-response
	if rec.Code != http.StatusOK {
		t.Fatalf("extension status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if err := <-claimDone; err != nil {
		t.Fatalf("coordinator scan: %v", err)
	}
	got := mustReleaseReminder(t, app, "xcode-vnc")
	if got.Status != ReleaseReminderStatusActive || got.AutoReleaseState != "" {
		t.Fatalf("unsafe race result: reminder=%+v", got)
	}
}

func TestAppWebReleaseReminderAutoClaimWinsBeforeExtension(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	now := app.JobManager.Now()
	reminder := seedWebAutoReleaseReminder(t, app, ReleaseReminderStatusDueNotified)
	var err error
	reminder, err = app.MemberStore.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		current.ReleaseDueAt = now.Add(-AutoReleaseGracePeriod).Format(time.RFC3339)
		current.AutoReleaseAt = now.Format(time.RFC3339)
		current.AutoReleaseState = ReleaseReminderAutoReleaseStateScheduled
		return current, nil
	})
	if err != nil {
		t.Fatalf("update reminder: %v", err)
	}

	coordinator := AutoReleaseCoordinator{Store: app.MemberStore}
	claimed, err := coordinator.claim(reminder, now)
	if err != nil {
		t.Fatalf("claim reminder: %v", err)
	}
	if claimed.AutoReleaseState != ReleaseReminderAutoReleaseStateRunning {
		t.Fatalf("claim state = %q", claimed.AutoReleaseState)
	}

	rec := postWebExtension(t, &app, "admin", now.Add(time.Hour))
	if rec.Code != http.StatusConflict {
		t.Fatalf("extension status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := mustReleaseReminder(t, app, reminder.ProfileName)
	if !reflect.DeepEqual(got, claimed) {
		t.Fatalf("extension modified claimed reminder:\n got: %+v\nwant: %+v", got, claimed)
	}
}

func TestAppWebManualDestroyCreateWinsConcurrentReminderMutation(t *testing.T) {
	for _, test := range []struct {
		name string
		post func(*testing.T, *App) *httptest.ResponseRecorder
	}{
		{
			name: "disable",
			post: func(t *testing.T, app *App) *httptest.ResponseRecorder {
				return postWebAutoRelease(t, app, "admin", "xcode-vnc", false)
			},
		},
		{
			name: "extension",
			post: func(t *testing.T, app *App) *httptest.ResponseRecorder {
				return postWebExtension(t, app, "admin", app.JobManager.Now().Add(time.Hour))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := newWebAutoReleaseTestApp(t)
			seeded := seedWebAutoReleaseReminder(t, app, ReleaseReminderStatusDueNotified)
			seeded = mustReleaseReminder(t, app, seeded.ProfileName)
			renameEntered := make(chan struct{})
			releaseRename := make(chan struct{})
			app.JobManager.Rename = func(oldPath, newPath string) error {
				close(renameEntered)
				<-releaseRename
				return os.Rename(oldPath, newPath)
			}
			createDone := make(chan error, 1)
			go func() {
				_, err := app.JobManager.Create(Job{ID: "manual-destroy", Type: "aws-destroy", Profile: seeded.ProfileName, Status: JobStatusRunning, PID: 42})
				createDone <- err
			}()
			<-renameEntered

			response := make(chan *httptest.ResponseRecorder, 1)
			go func() { response <- test.post(t, &app) }()
			close(releaseRename)
			if err := <-createDone; err != nil {
				t.Fatalf("manual destroy create: %v", err)
			}
			rec := <-response
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "active aws-destroy") {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got := mustReleaseReminder(t, app, seeded.ProfileName)
			if !reflect.DeepEqual(got, seeded) {
				t.Fatalf("handler modified reminder after destroy won:\n got: %+v\nwant: %+v", got, seeded)
			}
			events, err := app.MemberStore.RecentEvents(seeded.AppleEmail, 10)
			if err != nil || len(events) != 0 {
				t.Fatalf("failed handler recorded success event: events=%+v err=%v", events, err)
			}
		})
	}
}

type firstUpdateGateRepository struct {
	MemberRepository
	mu      sync.Mutex
	gated   bool
	entered chan struct{}
	release chan struct{}
}

type failingCleanupRepository struct {
	MemberRepository
	err error
}

func (r failingCleanupRepository) CleanupProfileRecords(string, string, string) (ReleaseReminder, bool, error) {
	return ReleaseReminder{}, false, r.err
}

func (r *firstUpdateGateRepository) UpdateReleaseReminder(profileName string, update func(ReleaseReminder) (ReleaseReminder, error)) (ReleaseReminder, error) {
	r.mu.Lock()
	gate := !r.gated
	if gate {
		r.gated = true
	}
	r.mu.Unlock()
	if !gate {
		return r.MemberRepository.UpdateReleaseReminder(profileName, update)
	}
	return r.MemberRepository.UpdateReleaseReminder(profileName, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		close(r.entered)
		<-r.release
		return update(reminder)
	})
}

func newWebAutoReleaseTestApp(t *testing.T) App {
	t.Helper()
	var out, errOut bytes.Buffer
	return testApp(&out, &errOut, t.TempDir())
}

func seedWebAutoReleaseReminder(t *testing.T, app App, status string) ReleaseReminder {
	t.Helper()
	reminder := ReleaseReminder{
		ProfileName: "xcode-vnc", AppleEmail: "user@example.com", HostID: "h-1",
		ReleaseDueAt: "2026-07-01T12:20:45Z", OwnerEmail: "admin@example.com", OwnerName: "Test Admin",
		LastNotifiedAt: "2026-07-01T12:30:45Z", Status: status, AutoReleaseEnabled: true,
		AutoReleaseAt: "2026-07-01T12:40:45Z", AutoReleaseStartedAt: "2026-07-01T12:30:45Z",
		AutoReleaseLastAttemptAt: "2026-07-01T12:35:45Z", AutoReleaseAttempts: 2,
		AutoReleaseLastError: "temporary", AutoReleaseState: ReleaseReminderAutoReleaseStateRetrying,
	}
	if _, err := app.MemberStore.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	return reminder
}

func seedWebAutoReleaseReminderWithState(t *testing.T, app App, status, state string) ReleaseReminder {
	t.Helper()
	reminder := seedWebAutoReleaseReminder(t, app, status)
	updated, err := app.MemberStore.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		current.AutoReleaseState = state
		return current, nil
	})
	if err != nil {
		t.Fatalf("set automatic release state: %v", err)
	}
	return updated
}

func postWebAutoRelease(t *testing.T, app *App, role, profile string, enabled bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"profile":"` + profile + `","enabled":` + map[bool]string{true: "true", false: "false"}[enabled] + `}`
	return postWebAutoReleaseBody(t, app, role, body)
}

func postWebAutoReleaseBody(t *testing.T, app *App, role, body string) *httptest.ResponseRecorder {
	t.Helper()
	reader := strings.NewReader(body)
	req := httptest.NewRequest(http.MethodPost, "/api/release-reminder/auto-release", reader)
	addWebAuth(t, app, req, role)
	rec := httptest.NewRecorder()
	app.newWebHandler("").ServeHTTP(rec, req)
	return rec
}

func postWebExtension(t *testing.T, app *App, role string, dueAt time.Time) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader(`{"profile":"xcode-vnc","release_due_at":"` + dueAt.UTC().Format(time.RFC3339) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/release-reminder/extend", body)
	addWebAuth(t, app, req, role)
	rec := httptest.NewRecorder()
	app.newWebHandler("").ServeHTTP(rec, req)
	return rec
}

func mustReleaseReminder(t *testing.T, app App, profile string) ReleaseReminder {
	t.Helper()
	reminder, ok, err := app.MemberStore.ReleaseReminder(profile)
	if err != nil || !ok {
		t.Fatalf("release reminder %q: ok=%t err=%v", profile, ok, err)
	}
	return reminder
}
