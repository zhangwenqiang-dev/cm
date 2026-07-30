package connectmac

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebBackgroundAWSJobTempConfigLifecycle(t *testing.T) {
	t.Run("create failure", func(t *testing.T) {
		app, config, tempDir := newWebBackgroundJobTestApp(t)
		if err := app.JobManager.BeginDrain(); err != nil {
			t.Fatalf("begin drain: %v", err)
		}
		resp := runWebBackgroundAWSJob(t, &app, config, "open")
		if !strings.Contains(resp.Body.String(), ErrJobsDraining.Error()) {
			t.Fatalf("response = %s", resp.Body.String())
		}
		assertNoWebAWSRequestEvents(t, app)
		assertNoWebTempConfigs(t, tempDir)
	})

	t.Run("runner startup failure", func(t *testing.T) {
		app, config, tempDir := newWebBackgroundJobTestApp(t)
		app.JobManager.Executable = filepath.Join(t.TempDir(), "missing-cm")
		resp := runWebBackgroundAWSJob(t, &app, config, "destroy")
		if !strings.Contains(resp.Body.String(), "no such file or directory") {
			t.Fatalf("response = %s", resp.Body.String())
		}
		events, err := app.MemberStore.QueryEvents(EventQuery{
			Profile:       "xcode-vnc",
			Limit:         20,
			IncludeSystem: true,
		})
		if err != nil {
			t.Fatalf("query events: %v", err)
		}
		counts := map[string]int{}
		for _, event := range events.Events {
			counts[event.Action]++
		}
		if counts["aws.release.requested"] != 1 || counts["aws.release.started"] != 0 ||
			counts["aws.release.failed"] != 1 {
			t.Fatalf("runner failure lifecycle events = %+v; events=%+v", counts, events.Events)
		}
		if err := app.reconcileWebAWSLifecycles(context.Background(), config); err != nil {
			t.Fatalf("repeat lifecycle reconciliation: %v", err)
		}
		repeated, err := app.MemberStore.QueryEvents(EventQuery{
			Profile:       "xcode-vnc",
			Limit:         20,
			IncludeSystem: true,
		})
		if err != nil {
			t.Fatalf("query repeated events: %v", err)
		}
		failed := 0
		for _, event := range repeated.Events {
			if event.Action == "aws.release.failed" {
				failed++
			}
		}
		if failed != 1 {
			t.Fatalf("failed terminal events after repeat = %d; events=%+v", failed, repeated.Events)
		}
		assertNoWebTempConfigs(t, tempDir)
	})

	t.Run("duplicate destroy failure", func(t *testing.T) {
		app, config, tempDir := newWebBackgroundJobTestApp(t)
		if _, err := app.JobManager.Create(Job{
			ID:      "existing-destroy",
			Type:    "aws-destroy",
			Profile: "xcode-vnc",
			Status:  JobStatusRunning,
		}); err != nil {
			t.Fatalf("create existing destroy job: %v", err)
		}
		resp := runWebBackgroundAWSJob(t, &app, config, "destroy")
		if !strings.Contains(resp.Body.String(), "active aws-destroy job already exists") {
			t.Fatalf("response = %s", resp.Body.String())
		}
		assertNoWebAWSRequestEvents(t, app)
		assertNoWebTempConfigs(t, tempDir)
	})

	for _, command := range []string{"open", "destroy"} {
		for _, terminal := range []string{"success", "failure"} {
			t.Run(command+" "+terminal, func(t *testing.T) {
				app, config, tempDir := newWebBackgroundJobTestApp(t)
				resp := runWebBackgroundAWSJob(t, &app, config, command)
				if !strings.Contains(resp.Body.String(), "Started background AWS "+command+" job") {
					t.Fatalf("response = %s", resp.Body.String())
				}
				job := onlyWebBackgroundJob(t, app.JobManager)
				if job.RequestID == "" || job.Source != "web" || job.ActorMemberID == "" ||
					job.ActorEmail != "admin@example.com" || job.ActorName == "" {
					t.Fatalf("job correlation metadata = %+v", job)
				}
				if job.LifecycleState != JobLifecyclePending {
					t.Fatalf("lifecycle state = %q", job.LifecycleState)
				}
				if command == "open" {
					if job.LifecycleOwnerEmail != "admin@example.com" {
						t.Fatalf("lifecycle owner email = %q", job.LifecycleOwnerEmail)
					}
				} else if job.LifecycleOwnerEmail != "" {
					t.Fatalf("destroy lifecycle owner email = %q", job.LifecycleOwnerEmail)
				}
				if len(job.CleanupPaths) != 1 {
					t.Fatalf("cleanup paths = %#v", job.CleanupPaths)
				}
				configPath := job.CleanupPaths[0]
				if _, err := os.Stat(configPath); err != nil {
					t.Fatalf("temp config unavailable before child run: %v", err)
				}
				job.Status = JobStatusRunning
				job.RunnerToken = "web-temp-token"
				job.Command = []string{"/bin/sh", "-c", `test -f "$1" || exit 9; test "$2" = success`, "sh", configPath, terminal}
				if err := app.JobManager.Save(job); err != nil {
					t.Fatalf("save runnable job: %v", err)
				}
				t.Setenv(jobRunnerTokenEnv, "web-temp-token")
				completed, err := app.JobManager.RunJob(context.Background(), job.ID)
				if terminal == "success" && err != nil {
					t.Fatalf("run successful job: %v", err)
				}
				if terminal == "failure" && err == nil {
					t.Fatal("failed job error = nil")
				}
				expectedStatus := JobStatusSuccess
				if terminal == "failure" {
					expectedStatus = JobStatusFailed
				}
				if completed.Status != expectedStatus {
					t.Fatalf("status = %s", completed.Status)
				}
				events, err := app.MemberStore.RecentEvents("user@example.com", 20)
				if err != nil {
					t.Fatalf("recent events: %v", err)
				}
				var requested, started *OperationEvent
				for i := range events {
					switch events[i].Action {
					case "aws.open.requested", "aws.release.requested":
						requested = &events[i]
					case "aws.open.started", "aws.release.started":
						started = &events[i]
					}
				}
				if requested == nil || started == nil {
					t.Fatalf("missing requested/started events: %+v", events)
				}
				if requested.RequestID != job.RequestID || started.RequestID != job.RequestID ||
					requested.JobID != job.ID || started.JobID != job.ID ||
					requested.MemberEmail != job.ActorEmail || started.MemberEmail != job.ActorEmail {
					t.Fatalf("event correlation mismatch: job=%+v requested=%+v started=%+v", job, requested, started)
				}
				assertNoWebTempConfigs(t, tempDir)
			})
		}
	}

	t.Run("stale reconciliation", func(t *testing.T) {
		app, config, tempDir := newWebBackgroundJobTestApp(t)
		resp := runWebBackgroundAWSJob(t, &app, config, "destroy")
		if !strings.Contains(resp.Body.String(), "Started background AWS destroy job") {
			t.Fatalf("response = %s", resp.Body.String())
		}
		job := onlyWebBackgroundJob(t, app.JobManager)
		if len(job.CleanupPaths) != 1 {
			t.Fatalf("cleanup paths = %#v", job.CleanupPaths)
		}
		app.JobManager.IsRunning = func(int) bool { return false }
		if _, err := app.JobManager.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertNoWebTempConfigs(t, tempDir)
	})

	t.Run("request cancellation does not cancel runner startup", func(t *testing.T) {
		app, config, _ := newWebBackgroundJobTestApp(t)
		body := `{"profile":"xcode-vnc","confirm":true,"background":true,"notify":true,"owner_email":"admin@example.com"}`
		base := httptest.NewRequest(http.MethodPost, "/api/aws/open", strings.NewReader(body))
		addWebAuth(t, &app, base, "admin")
		ctx, cancel := context.WithCancel(base.Context())
		cancel()
		req := base.WithContext(ctx)
		rec := httptest.NewRecorder()
		app.newWebHandler(config).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Started background AWS open job") {
			t.Fatalf("canceled request response: status=%d body=%s", rec.Code, rec.Body.String())
		}
		job := onlyWebBackgroundJob(t, app.JobManager)
		if job.Status != JobStatusRunning || job.PID == 0 {
			t.Fatalf("runner did not survive request cancellation: %+v", job)
		}
	})
}

func TestWebAWSPreviewUsesExplicitPreviewEventNames(t *testing.T) {
	for _, command := range []string{"open", "destroy"} {
		t.Run(command, func(t *testing.T) {
			app, config, _ := newWebBackgroundJobTestApp(t)
			body := `{"profile":"xcode-vnc","confirm":false,"background":false}`
			req := httptest.NewRequest(http.MethodPost, "/api/aws/"+command, strings.NewReader(body))
			addWebAuth(t, &app, req, "admin")
			rec := httptest.NewRecorder()
			app.newWebHandler(config).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			events, err := app.MemberStore.RecentEvents("user@example.com", 10)
			if err != nil {
				t.Fatalf("recent events: %v", err)
			}
			want := "aws.open.previewed"
			if command == "destroy" {
				want = "aws.release.previewed"
			}
			if len(events) != 1 || events[0].Action != want || events[0].Confirmed {
				t.Fatalf("events = %+v, want one %s preview", events, want)
			}
		})
	}
}

func TestWebAWSStatusCancellationDoesNotCreateErrorLog(t *testing.T) {
	app, config, _ := newWebBackgroundJobTestApp(t)
	app.AWSService.NewClient = func(context.Context, MacPlan) (AWSClient, error) {
		return nil, context.Canceled
	}
	req := httptest.NewRequest(http.MethodGet, "/api/aws/status?profile=xcode-vnc", nil)
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	files, err := app.LogManager.List()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("list logs: %v", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file.Path)
		if err != nil {
			t.Fatalf("read log: %v", err)
		}
		if strings.Contains(string(data), `"action":"web.aws.status"`) {
			t.Fatalf("canceled status request was logged as an AWS error: %s", data)
		}
	}
}

func TestWebAWSJobPlanFailureDoesNotRecordRequestedOrStarted(t *testing.T) {
	app, _, _ := newWebBackgroundJobTestApp(t)
	profile := validAWSProfile()
	profile.AWS.InstanceTypePriority = []string{"unsupported.metal"}
	_, _, err := app.startAWSJobForResolvedProfileJob(
		context.Background(),
		filepath.Join(t.TempDir(), "config.yaml"),
		"open",
		profile,
		false,
		Job{
			RequestID:     "req-plan-failure",
			Source:        "web",
			ActorMemberID: "member-admin",
			ActorEmail:    "admin@example.com",
			ActorName:     "Admin",
		},
	)
	if err == nil || !strings.Contains(err.Error(), "unsupported aws instance type") {
		t.Fatalf("plan error = %v", err)
	}
	assertNoWebAWSRequestEvents(t, app)
}

func newWebBackgroundJobTestApp(t *testing.T) (App, string, string) {
	t.Helper()
	dir := t.TempDir()
	tempDir := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Setenv("TMPDIR", tempDir)
	key := writeSSHKey(t, 0o600)
	config := writeConfig(t, dir, key)
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	return app, config, tempDir
}

func runWebBackgroundAWSJob(t *testing.T, app *App, config, command string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"profile":"xcode-vnc","confirm":true,"background":true,"notify":true,"owner_email":"admin@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/aws/"+command, strings.NewReader(body))
	addWebAuth(t, app, req, "admin")
	if command == "destroy" {
		if _, err := app.MemberStore.SetProfileOwner("xcode-vnc", "admin@example.com"); err != nil {
			t.Fatalf("set profile owner: %v", err)
		}
	}
	rec := httptest.NewRecorder()
	app.newWebHandler(config).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func onlyWebBackgroundJob(t *testing.T, manager JobManager) Job {
	t.Helper()
	jobs, err := manager.listRaw()
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	return jobs[0]
}

func assertNoWebTempConfigs(t *testing.T, tempDir string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(tempDir, "cm-web-config-*.yaml"))
	if err != nil {
		t.Fatalf("glob temp configs: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("web temp configs remain: %v", paths)
	}
}

func assertNoWebAWSRequestEvents(t *testing.T, app App) {
	t.Helper()
	events, err := app.MemberStore.QueryEvents(EventQuery{
		Profile:       "xcode-vnc",
		Limit:         20,
		IncludeSystem: true,
	})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	for _, event := range events.Events {
		switch event.Action {
		case "aws.open.requested", "aws.open.started", "aws.release.requested", "aws.release.started":
			t.Fatalf("failed job startup recorded request/start event: %+v", event)
		}
	}
}
