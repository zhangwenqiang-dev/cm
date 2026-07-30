package connectmac

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestWebObservabilitySmoke is the single, repeatable smoke entry for the
// observability flow. It uses temporary file stores and mocked AWS state only.
func TestWebObservabilitySmoke(t *testing.T) {
	t.Run("login events are sanitized", func(t *testing.T) {
		app, handler := newWebAuditApp(t)
		if _, err := app.MemberStore.SetupAdmin("Admin", "admin@example.com", "password123"); err != nil {
			t.Fatal(err)
		}

		failed := webChallengeThroughHandler(t, handler)
		failedBody := fmt.Sprintf(
			`{"username":"admin@example.com","password":"smoke-login-secret","challenge_token":%q,"challenge_answer":%q}`,
			failed.token,
			failed.answer,
		)
		assertWebMutationAudit(
			t, &app, handler, nil, "/api/auth/login", failedBody,
			http.StatusUnauthorized, false, 0, "auth.login.failed", "", "admin@example.com", "",
		)

		success := webChallengeThroughHandler(t, handler)
		successBody := fmt.Sprintf(
			`{"username":"admin@example.com","password":"password123","challenge_token":%q,"challenge_answer":%q}`,
			success.token,
			success.answer,
		)
		assertWebMutationAudit(
			t, &app, handler, nil, "/api/auth/login", successBody,
			http.StatusOK, true, 1, "auth.login.succeeded", "admin@example.com", "admin@example.com", "",
		)
	})

	t.Run("member and profile mutations are correlated", func(t *testing.T) {
		app, handler, cookie := newAuthenticatedWebAuditApp(t, "admin")
		baseline := webAuditEventCount(t, app.MemberStore)
		assertWebMutationAudit(
			t, &app, handler, cookie, "/api/member/add",
			`{"name":"Smoke Member","email":"smoke-member@example.com","role":"operator"}`,
			http.StatusOK, true, baseline, "member.created", "admin@example.com", "smoke-member@example.com",
			"changed_fields=name,email,role",
		)

		baseline++
		assertWebMutationAudit(
			t, &app, handler, cookie, "/api/managed-profile/save",
			mustJSON(t, map[string]string{"profile_yaml": auditProfileYAML("smoke-usw2")}),
			http.StatusOK, true, baseline, "profile.created", "admin@example.com", "",
			"changed_fields=profile",
		)
	})

	t.Run("missing local config is silent", func(t *testing.T) {
		app, _ := newWebAuditApp(t)
		app.LoginConfigCleanup = true
		app.cleanupLocalConfigAfterLogin(filepath.Join(t.TempDir(), "missing", "config.yaml"))
		files, err := app.LogManager.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Fatalf("missing config created warning logs: %+v", files)
		}
	})

	t.Run("event pagination has no duplicates", func(t *testing.T) {
		app, handler, _, operator := newWebTransferTestApp(t)
		base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		for i := 0; i < 5; i++ {
			recordWebEventFixture(t, &app, OperationEvent{
				ID:        fmt.Sprintf("smoke-event-%d", i),
				Action:    "smoke.event",
				Profile:   "shared",
				Status:    "success",
				Source:    "web",
				CreatedAt: base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
			})
		}
		first := decodeWebEventPage(t, serveWebTransfer(
			t, &app, handler, operator, http.MethodGet, "/api/events?limit=3", "",
		))
		second := decodeWebEventPage(t, serveWebTransfer(
			t, &app, handler, operator, http.MethodGet,
			"/api/events?limit=3&cursor="+first.NextCursor, "",
		))
		seen := make(map[string]struct{}, len(first.Events)+len(second.Events))
		for _, event := range append(first.Events, second.Events...) {
			if _, exists := seen[event.ID]; exists {
				t.Fatalf("duplicate paginated event %q", event.ID)
			}
			seen[event.ID] = struct{}{}
		}
		if len(seen) != 5 || second.NextCursor != "" {
			t.Fatalf("pagination result count=%d second=%+v", len(seen), second)
		}
	})

	t.Run("mocked AWS completion records terminal state", func(t *testing.T) {
		app, configPath := newWebAWSLifecycleTestApp(t)
		app.AWSService.NewClient = func(context.Context, MacPlan) (AWSClient, error) {
			return &fakeAWSClient{status: readyWebAWSLifecycleStatus()}, nil
		}
		app.WebAWSLifecycleNotifier = func(string, ReleaseReminder, string, string) error {
			return nil
		}
		job := createWebAWSLifecycleJob(t, &app, "open", JobStatusSuccess)
		if err := app.reconcileWebAWSLifecycleJob(context.Background(), configPath, job); err != nil {
			t.Fatal(err)
		}
		stored, err := app.JobManager.Load(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.LifecycleState != JobLifecycleFinalized {
			t.Fatalf("lifecycle state = %q, want finalized", stored.LifecycleState)
		}
		events, err := app.MemberStore.QueryEvents(EventQuery{Limit: 20, IncludeSystem: true})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range events.Events {
			if event.Action == "aws.open.ready" && event.JobID == job.ID && event.RequestID == job.RequestID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("terminal AWS event not found: %+v", events.Events)
		}
	})

	t.Run("transfer finalizes at ninety nine and succeeds at one hundred", func(t *testing.T) {
		phase, percent := mapRsyncProgress(100, false)
		if phase != TransferPhaseFinalizing || percent != 99 {
			t.Fatalf("active completion = %s/%d, want finalizing/99", phase, percent)
		}
		phase, percent = mapRsyncProgress(100, true)
		if phase != TransferPhaseSucceeded || percent != 100 {
			t.Fatalf("process completion = %s/%d, want succeeded/100", phase, percent)
		}

		app, handler, _, operator := newWebTransferTestApp(t)
		record := startWebTransferRecord(
			t, &app, handler, operator,
			`{"profile":"shared","direction":"push","local_path":"/tmp","remote_path":"~/tmp"}`,
		)
		finalizing := serveWebTransfer(
			t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update",
			`{"id":"`+record.ID+`","local_job_id":"smoke-job","status":"running","phase":"finalizing","percent":99}`,
		)
		if finalizing.Code != http.StatusOK {
			t.Fatalf("finalizing status=%d body=%s", finalizing.Code, finalizing.Body.String())
		}
		succeeded := serveWebTransfer(
			t, &app, handler, operator, http.MethodPost, "/api/transfer-record/update",
			`{"id":"`+record.ID+`","local_job_id":"smoke-job","status":"succeeded","phase":"succeeded","percent":100}`,
		)
		if succeeded.Code != http.StatusOK {
			t.Fatalf("succeeded status=%d body=%s", succeeded.Code, succeeded.Body.String())
		}
		records := decodeWebTransferRecords(t, serveWebTransfer(
			t, &app, handler, operator, http.MethodGet, "/api/transfer-records", "",
		))
		if len(records) != 1 || records[0].Phase != TransferPhaseSucceeded || records[0].Percent != 100 {
			t.Fatalf("terminal transfer record = %+v", records)
		}
	})
}
