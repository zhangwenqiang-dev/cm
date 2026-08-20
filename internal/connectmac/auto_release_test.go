package connectmac

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAutoReleaseDueFlowSchedulesOnlyWhenEnabled(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		enabled bool
		state   string
		autoAt  string
	}{
		{name: "disabled"},
		{name: "enabled", enabled: true, state: ReleaseReminderAutoReleaseStateScheduled, autoAt: now.Add(AutoReleaseGracePeriod).Format(time.RFC3339)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newAutoReleaseTestStore(ReleaseReminder{ProfileName: "mac", AppleEmail: "apple@example.com", ReleaseDueAt: now.Format(time.RFC3339), Status: ReleaseReminderStatusActive, AutoReleaseEnabled: test.enabled})
			coordinator, notifications, _ := newAutoReleaseTestCoordinator(now, store)

			if err := coordinator.Scan(context.Background()); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got := store.get("mac")
			if got.Status != ReleaseReminderStatusDueNotified || got.LastNotifiedAt != now.Format(time.RFC3339) || got.AutoReleaseState != test.state || got.AutoReleaseAt != test.autoAt {
				t.Fatalf("due reminder = %+v", got)
			}
			if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationDue {
				t.Fatalf("notifications = %+v", *notifications)
			}
		})
	}
}

func TestAutoReleaseWaitsUntilExactGraceDeadline(t *testing.T) {
	deadline := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(deadline))
	coordinator, _, starts := newAutoReleaseTestCoordinator(deadline.Add(-time.Nanosecond), store)

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan before deadline: %v", err)
	}
	if len(*starts) != 0 {
		t.Fatalf("destroy starts before deadline = %d", len(*starts))
	}
	coordinator.Now = func() time.Time { return deadline }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan at deadline: %v", err)
	}
	if len(*starts) != 1 || store.get("mac").AutoReleaseState != ReleaseReminderAutoReleaseStateRunning {
		t.Fatalf("starts=%d reminder=%+v", len(*starts), store.get("mac"))
	}
}

func TestAutoReleaseAtomicClaimLosesToExtension(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	store.beforeUpdate = func(reminder *ReleaseReminder) {
		reminder.ReleaseDueAt = now.Add(time.Hour).Format(time.RFC3339)
		reminder.Status = ReleaseReminderStatusActive
		reminder.AutoReleaseAt = ""
		reminder.AutoReleaseState = ""
		store.beforeUpdate = nil
	}
	coordinator, _, starts := newAutoReleaseTestCoordinator(now, store)

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(*starts) != 0 || store.get("mac").Status != ReleaseReminderStatusActive {
		t.Fatalf("extension lost: starts=%d reminder=%+v", len(*starts), store.get("mac"))
	}
}

func TestAutoReleaseRechecksPersistedCycleAfterStatusBeforeDestroy(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		mutate func(*ReleaseReminder)
	}{
		{name: "disabled", mutate: func(r *ReleaseReminder) { r.AutoReleaseEnabled = false }},
		{name: "extended", mutate: func(r *ReleaseReminder) {
			r.Status = ReleaseReminderStatusActive
			r.ReleaseDueAt = now.Add(time.Hour).Format(time.RFC3339)
			r.AutoReleaseAt = ""
			r.AutoReleaseState = ""
		}},
		{name: "schedule changed", mutate: func(r *ReleaseReminder) { r.AutoReleaseAt = now.Add(time.Minute).Format(time.RFC3339) }},
		{name: "apple changed", mutate: func(r *ReleaseReminder) { r.AppleEmail = "replacement@example.com" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newAutoReleaseTestStore(scheduledAutoRelease(now))
			coordinator, _, starts := newAutoReleaseTestCoordinator(now, store)
			coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
				store.mutate("mac", test.mutate)
				return AWSStatus{Hosts: []DedicatedHostStatus{autoReleaseTestHost("available")}}, nil
			}

			if err := coordinator.Scan(context.Background()); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(*starts) != 0 {
				t.Fatalf("StartDestroy called after %s mutation", test.name)
			}
		})
	}
}

func TestAutoReleaseRechecksDuplicateJobAfterStatus(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	jobs := &autoReleaseTestJobs{}
	coordinator, _, starts := newAutoReleaseTestCoordinator(now, store)
	coordinator.Jobs = jobs
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		jobs.jobs = append(jobs.jobs, Job{ID: "racing", Type: "aws-destroy", Profile: "mac", Status: JobStatusRunning})
		return AWSStatus{Hosts: []DedicatedHostStatus{autoReleaseTestHost("available")}}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(*starts) != 0 {
		t.Fatalf("StartDestroy calls = %d", len(*starts))
	}
}

func TestAutoReleasePreventsDuplicateDestroyJobs(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	coordinator, _, starts := newAutoReleaseTestCoordinator(now, store)
	coordinator.Jobs = &autoReleaseTestJobs{jobs: []Job{{ID: "existing", Type: "aws-destroy", Profile: "mac", Status: JobStatusRunning}}}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(*starts) != 0 || store.get("mac").AutoReleaseState != ReleaseReminderAutoReleaseStateScheduled {
		t.Fatalf("duplicate was started: starts=%d reminder=%+v", len(*starts), store.get("mac"))
	}
}

func TestAutoReleaseCancelsChangedOrDisabledCycle(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	for _, mutate := range []func(*ReleaseReminder){
		func(r *ReleaseReminder) { r.AutoReleaseEnabled = false },
		func(r *ReleaseReminder) {
			r.Status = ReleaseReminderStatusActive
			r.ReleaseDueAt = now.Add(time.Hour).Format(time.RFC3339)
		},
		func(r *ReleaseReminder) { r.Status = ReleaseReminderStatusReleased },
	} {
		reminder := scheduledAutoRelease(now)
		mutate(&reminder)
		store := newAutoReleaseTestStore(reminder)
		coordinator, _, starts := newAutoReleaseTestCoordinator(now, store)
		if err := coordinator.Scan(context.Background()); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(*starts) != 0 {
			t.Fatalf("destroy starts = %d for %+v", len(*starts), reminder)
		}
	}
}

func TestApplyReleaseReminderExtensionCancelsCycleAndRejectsRunningOrShort(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 5, 0, 0, time.UTC)
	reminder := scheduledAutoRelease(now.Add(5 * time.Minute))
	reminder.AutoReleaseStartedAt = now.Format(time.RFC3339)
	reminder.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
	reminder.AutoReleaseAttempts = 2
	reminder.AutoReleaseLastError = "temporary"

	updated, err := applyReleaseReminderExtension(reminder, now.Add(AutoReleaseGracePeriod), now, "member@example.com", "Member")
	if err != nil {
		t.Fatalf("apply extension: %v", err)
	}
	if updated.Status != ReleaseReminderStatusActive || updated.AutoReleaseState != "" || updated.AutoReleaseAt != "" || updated.AutoReleaseStartedAt != "" || updated.AutoReleaseLastAttemptAt != "" || updated.AutoReleaseAttempts != 0 || updated.AutoReleaseLastError != "" {
		t.Fatalf("extension did not cancel cycle: %+v", updated)
	}
	if _, err := applyReleaseReminderExtension(reminder, now.Add(AutoReleaseGracePeriod-time.Second), now, "member@example.com", "Member"); err == nil {
		t.Fatal("short extension error = nil")
	}
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
	if _, err := applyReleaseReminderExtension(reminder, now.Add(time.Hour), now, "member@example.com", "Member"); err == nil {
		t.Fatal("running extension error = nil")
	}
}

func TestAutoReleaseRestartReconcilesStaleRunningThenRetriesOnSpacing(t *testing.T) {
	started := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	reminder := scheduledAutoRelease(started)
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
	reminder.AutoReleaseStartedAt = started.Format(time.RFC3339)
	reminder.AutoReleaseLastAttemptAt = started.Format(time.RFC3339)
	reminder.AutoReleaseAttempts = 1
	store := newAutoReleaseTestStore(reminder)
	coordinator, _, starts := newAutoReleaseTestCoordinator(started.Add(4*time.Minute), store)

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("restart Scan: %v", err)
	}
	if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying || len(*starts) != 0 {
		t.Fatalf("restart reconcile starts=%d reminder=%+v", len(*starts), got)
	}
	coordinator.Now = func() time.Time { return started.Add(AutoReleaseRetryInterval) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("retry Scan: %v", err)
	}
	if len(*starts) != 1 || store.get("mac").AutoReleaseAttempts != 2 {
		t.Fatalf("retry starts=%d reminder=%+v", len(*starts), store.get("mac"))
	}
}

func TestAutoReleaseStopsAfterOneHour(t *testing.T) {
	started := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	reminder := scheduledAutoRelease(started)
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
	reminder.AutoReleaseStartedAt = started.Format(time.RFC3339)
	reminder.AutoReleaseLastAttemptAt = started.Add(55 * time.Minute).Format(time.RFC3339)
	reminder.AutoReleaseAttempts = 12
	store := newAutoReleaseTestStore(reminder)
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(started.Add(AutoReleaseRetryWindow), store)

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || len(*starts) != 0 {
		t.Fatalf("timeout starts=%d reminder=%+v", len(*starts), got)
	}
	if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationFinalFailure {
		t.Fatalf("notifications = %+v", *notifications)
	}
}

func TestAutoReleaseTerminalErrorsAndAppleMismatchDoNotRetry(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		resolve func(context.Context, ReleaseReminder) (Profile, error)
	}{
		{name: "missing credentials", resolve: func(context.Context, ReleaseReminder) (Profile, error) {
			return Profile{}, TerminalAutoReleaseError(errors.New("AWS credentials missing"))
		}},
		{name: "apple mismatch", resolve: func(context.Context, ReleaseReminder) (Profile, error) {
			p := autoReleaseTestProfile()
			p.AWS.AccountEmail = "other@example.com"
			return p, nil
		}},
		{name: "apple case mismatch", resolve: func(context.Context, ReleaseReminder) (Profile, error) {
			p := autoReleaseTestProfile()
			p.AWS.AccountEmail = "Apple@example.com"
			return p, nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newAutoReleaseTestStore(scheduledAutoRelease(now))
			coordinator, notifications, starts := newAutoReleaseTestCoordinator(now, store)
			coordinator.ResolveProfile = test.resolve
			if err := coordinator.Scan(context.Background()); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || len(*starts) != 0 || !strings.Contains(strings.ToLower(got.AutoReleaseLastError), strings.Split(test.name, " ")[0]) {
				t.Fatalf("terminal starts=%d reminder=%+v", len(*starts), got)
			}
			if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationFinalFailure {
				t.Fatalf("notifications = %+v", *notifications)
			}
		})
	}
}

func TestAutoReleaseRecoverableFailureRetriesAndNotifiesOnlyFirstFailure(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(now, store)
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		return AWSStatus{}, RecoverableAutoReleaseError(errors.New("throttling: try again"))
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying || got.AutoReleaseAttempts != 1 || len(*starts) != 0 {
		t.Fatalf("first failure starts=%d reminder=%+v", len(*starts), got)
	}
	if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationFirstFailure {
		t.Fatalf("first notifications = %+v", *notifications)
	}
	coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryInterval) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if len(*notifications) != 1 || store.get("mac").AutoReleaseAttempts != 2 {
		t.Fatalf("second notifications=%+v reminder=%+v", *notifications, store.get("mac"))
	}
}

func TestAutoReleaseFailSafeErrorClassification(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		statusErr error
		startErr  error
		wantState string
	}{
		{name: "tag mismatch terminal", statusErr: TerminalAutoReleaseError(errors.New("required safety tags do not match")), wantState: ReleaseReminderAutoReleaseStateFailed},
		{name: "ownership terminal", statusErr: TerminalAutoReleaseError(errors.New("ambiguous resource ownership")), wantState: ReleaseReminderAutoReleaseStateFailed},
		{name: "unknown status fails safe", statusErr: errors.New("unclassified status failure"), wantState: ReleaseReminderAutoReleaseStateFailed},
		{name: "explicit network retries", statusErr: RecoverableAutoReleaseError(errors.New("temporary network failure")), wantState: ReleaseReminderAutoReleaseStateRetrying},
		{name: "unknown destroy retries after safety check", startErr: errors.New("unclassified destroy failure"), wantState: ReleaseReminderAutoReleaseStateRetrying},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newAutoReleaseTestStore(scheduledAutoRelease(now))
			coordinator, _, _ := newAutoReleaseTestCoordinator(now, store)
			coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
				if test.statusErr != nil {
					return AWSStatus{}, test.statusErr
				}
				return AWSStatus{Hosts: []DedicatedHostStatus{autoReleaseTestHost("available")}}, nil
			}
			coordinator.StartDestroy = func(context.Context, Profile) (Job, error) {
				return Job{}, test.startErr
			}

			if err := coordinator.Scan(context.Background()); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if got := store.get("mac"); got.AutoReleaseState != test.wantState {
				t.Fatalf("state = %q, want %q; reminder=%+v", got.AutoReleaseState, test.wantState, got)
			}
		})
	}
}

func TestAutoReleaseOwnershipSafetyErrorsAreTerminal(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		status AWSStatus
	}{
		{name: "tag mismatch", status: AWSStatus{Hosts: []DedicatedHostStatus{{HostID: "h-1", State: "available", Tags: []AWSTagConfig{{Key: "cm-managed", Value: "true"}}}}}},
		{name: "ambiguous hosts", status: AWSStatus{Hosts: []DedicatedHostStatus{autoReleaseTestHost("available"), {HostID: "h-2", State: "available", Tags: autoReleaseTestManagedTags()}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newAutoReleaseTestStore(scheduledAutoRelease(now))
			coordinator, _, starts := newAutoReleaseTestCoordinator(now, store)
			coordinator.Status = func(context.Context, Profile) (AWSStatus, error) { return test.status, nil }

			if err := coordinator.Scan(context.Background()); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || len(*starts) != 0 {
				t.Fatalf("safety failure starts=%d reminder=%+v", len(*starts), got)
			}
		})
	}
}

func TestClassifyAWSAutoReleaseErrorUsesTypedCategories(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want autoReleaseErrorCategory
	}{
		{name: "throttle", err: autoReleaseTestAPIError{code: "Throttling", message: "opaque"}, want: autoReleaseErrorRecoverable},
		{name: "authorization", err: autoReleaseTestAPIError{code: "AccessDenied", message: "opaque"}, want: autoReleaseErrorTerminal},
		{name: "unknown API code", err: autoReleaseTestAPIError{code: "FutureError", message: "throttling words do not classify"}, want: autoReleaseErrorUnknown},
		{name: "partial destroy", err: AWSDestroyPartialError{Cause: errors.New("opaque")}, want: autoReleaseErrorRecoverable},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := autoReleaseErrorCategoryOf(classifyAWSAutoReleaseError(test.err)); got != test.want {
				t.Fatalf("category = %v, want %v", got, test.want)
			}
		})
	}
}

type autoReleaseTestAPIError struct {
	code    string
	message string
}

func (e autoReleaseTestAPIError) Error() string     { return e.message }
func (e autoReleaseTestAPIError) ErrorCode() string { return e.code }

func TestAutoReleaseCleanupFailureRetriesThenSucceeds(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	store.cleanupErrors = []error{errors.New("local store temporarily unavailable"), nil}
	coordinator, _, starts := newAutoReleaseTestCoordinator(now, store)
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-1"}}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if got := store.get("mac"); got.Status == ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying || store.cleanupCalls != 1 || len(*starts) != 0 {
		t.Fatalf("failed cleanup persisted terminal state: reminder=%+v cleanup=%d starts=%d", got, store.cleanupCalls, len(*starts))
	}
	coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryInterval) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("retry Scan: %v", err)
	}
	if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || store.cleanupCalls != 2 {
		t.Fatalf("cleanup retry did not complete: reminder=%+v cleanup=%d", got, store.cleanupCalls)
	}
}

func TestAutoReleaseCleanupFailureStopsAtRetryWindow(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	store.cleanupErrors = []error{errors.New("local cleanup failed")}
	coordinator, _, _ := newAutoReleaseTestCoordinator(now, store)
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) { return AWSStatus{}, nil }

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryWindow) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("timeout Scan: %v", err)
	}
	if got := store.get("mac"); got.Status == ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || store.cleanupCalls != 1 {
		t.Fatalf("cleanup timeout reminder=%+v cleanup=%d", got, store.cleanupCalls)
	}
}

func TestAutoReleaseObservesSuccessfulJobAndRetainsEIPAllocation(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	jobs := &autoReleaseTestJobs{}
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(now, store)
	events := make([]AutoReleaseEvent, 0, 1)
	coordinator.Emit = func(event AutoReleaseEvent) { events = append(events, event) }
	coordinator.Jobs = jobs
	statuses := []AWSStatus{
		{Hosts: []DedicatedHostStatus{autoReleaseTestHost("available")}, Instances: []InstanceStatus{autoReleaseTestInstance("running")}, ElasticIP: ElasticIP{AllocationID: "eipalloc-1", AssociationID: "eipassoc-1", InstanceID: "i-1"}},
		{ElasticIP: ElasticIP{AllocationID: "eipalloc-1"}},
	}
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		status := statuses[0]
		statuses = statuses[1:]
		return status, nil
	}
	coordinator.StartDestroy = func(_ context.Context, profile Profile) (Job, error) {
		*starts = append(*starts, profile)
		job := Job{ID: "destroy-1", Type: "aws-destroy", Profile: profile.Name, AppleEmail: profile.AWS.AccountEmail, Status: JobStatusRunning, StartedAt: now}
		jobs.jobs = []Job{job}
		return job, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("start Scan: %v", err)
	}
	jobs.jobs[0].Status = JobStatusSuccess
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("observe Scan: %v", err)
	}
	got := store.get("mac")
	if got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || store.cleanupCalls != 1 {
		t.Fatalf("success reminder=%+v cleanup=%d", got, store.cleanupCalls)
	}
	if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationSuccess {
		t.Fatalf("notifications = %+v", *notifications)
	}
	if len(events) == 0 || events[len(events)-1].Action != "released" || !strings.Contains(events[len(events)-1].Message, "eip_retained=true") {
		t.Fatalf("events = %+v", events)
	}
}

func TestAutoReleaseRecoversSuccessfulLegacyDestroyJobAndNotifies(t *testing.T) {
	now := time.Date(2026, 8, 20, 8, 35, 0, 0, time.UTC)
	reminder := ReleaseReminder{
		ProfileName:              "aaronjasonall-use1",
		AppleEmail:               "aaronjasonall@example.com",
		HostID:                   "h-0123456789abcdef0",
		HostCreatedAt:            "2026-08-19T20:00:00Z",
		ReleaseDueAt:             "2026-08-20T08:20:00Z",
		OwnerEmail:               "owner@example.com",
		LastNotifiedAt:           "2026-08-20T08:20:00Z",
		Status:                   ReleaseReminderStatusDueNotified,
		AutoReleaseEnabled:       true,
		AutoReleaseAt:            "2026-08-20T08:30:00Z",
		AutoReleaseStartedAt:     "2026-08-20T08:30:00Z",
		AutoReleaseLastAttemptAt: "2026-08-20T08:30:01Z",
		AutoReleaseAttempts:      1,
		AutoReleaseState:         ReleaseReminderAutoReleaseStateRunning,
		CreatedAt:                "2026-08-19T20:00:00Z",
		UpdatedAt:                "2026-08-20T08:30:01Z",
	}
	store := newAutoReleaseTestStore(reminder)
	jobs := &autoReleaseTestJobs{jobs: []Job{{
		ID:                  "destroy-aaronjasonall-use1",
		Type:                "aws-destroy",
		Profile:             reminder.ProfileName,
		AppleEmail:          reminder.AppleEmail,
		Status:              JobStatusSuccess,
		StartedAt:           time.Date(2026, 8, 20, 8, 30, 2, 0, time.UTC),
		FinishedAt:          time.Date(2026, 8, 20, 8, 34, 30, 0, time.UTC),
		LifecycleState:      "",
		LifecycleOwnerEmail: "",
	}}}
	fakeStatus := AWSStatus{
		Hosts:     []DedicatedHostStatus{},
		Instances: []InstanceStatus{},
		ElasticIP: ElasticIP{AllocationID: "eipalloc-aaronjasonall-use1"},
	}
	coordinator, notifications, _ := newAutoReleaseTestCoordinator(now, store)
	coordinator.Jobs = jobs
	coordinator.ResolveProfile = func(context.Context, ReleaseReminder) (Profile, error) {
		return Profile{
			Name: reminder.ProfileName,
			AWS: AWSConfig{
				AccountEmail: reminder.AppleEmail,
				Profile:      "aws-aaronjasonall-use1",
				Region:       "us-east-1",
			},
		}, nil
	}
	statusCalls := 0
	var statusProfile Profile
	var observedStatus AWSStatus
	coordinator.Status = func(_ context.Context, profile Profile) (AWSStatus, error) {
		statusCalls++
		statusProfile = profile
		observedStatus = fakeStatus
		return observedStatus, nil
	}
	startDestroyCalls := 0
	coordinator.StartDestroy = func(context.Context, Profile) (Job, error) {
		startDestroyCalls++
		return Job{}, errors.New("StartDestroy must not be called for a successful legacy destroy job")
	}
	events := make([]AutoReleaseEvent, 0, 2)
	coordinator.Emit = func(event AutoReleaseEvent) { events = append(events, event) }

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if startDestroyCalls != 0 {
		t.Fatalf("StartDestroy calls = %d, want 0", startDestroyCalls)
	}
	if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationSuccess {
		t.Fatalf("notifications = %+v", *notifications)
	}
	wantCycle := releaseReminderCycleFromReminder(reminder)
	if notification := (*notifications)[0]; notification.Reminder.ProfileName != reminder.ProfileName || notification.Reminder.AppleEmail != reminder.AppleEmail || notification.Reminder.HostID != reminder.HostID || releaseReminderCycleFromReminder(notification.Reminder) != wantCycle {
		t.Fatalf("success notification reminder = %+v, want cycle %+v", notification.Reminder, wantCycle)
	}
	if statusCalls != 1 || statusProfile.Name != reminder.ProfileName || statusProfile.AWS.AccountEmail != reminder.AppleEmail || statusProfile.AWS.Region != "us-east-1" {
		t.Fatalf("Status calls = %d, profile = %+v", statusCalls, statusProfile)
	}
	if len(observedStatus.Hosts) != 0 || len(observedStatus.Instances) != 0 || observedStatus.ElasticIP.AllocationID != "eipalloc-aaronjasonall-use1" || observedStatus.ElasticIP.AssociationID != "" || observedStatus.ElasticIP.InstanceID != "" {
		t.Fatalf("observed status was not clean with retained EIP: %+v", observedStatus)
	}
	if store.cleanupCalls != 1 || len(store.completeCycles) != 1 || store.completeCycles[0] != wantCycle {
		t.Fatalf("completed cycles = %+v, cleanup calls = %d, want exactly %+v", store.completeCycles, store.cleanupCalls, wantCycle)
	}
	releasedEvents := 0
	for _, event := range events {
		if event.Action == "released" {
			releasedEvents++
			if event.Reminder.ProfileName != reminder.ProfileName || !strings.Contains(event.Message, "eip_retained=true") {
				t.Fatalf("released event = %+v", event)
			}
		}
	}
	if releasedEvents != 1 {
		t.Fatalf("released events = %d, events = %+v", releasedEvents, events)
	}
}

func TestAutoReleaseCompletionNotificationFailureRetries(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	coordinator, _, _ := newAutoReleaseTestCoordinator(now, store)
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}, nil
	}
	calls := 0
	coordinator.Notify = func(notification AutoReleaseNotification) error {
		if notification.Kind != AutoReleaseNotificationSuccess {
			t.Fatalf("notification = %+v", notification)
		}
		calls++
		if calls == 1 {
			return errors.New("wechat temporarily unavailable")
		}
		return nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if got := store.get("mac"); got.Status == ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying || !strings.Contains(got.AutoReleaseLastError, "wechat") {
		t.Fatalf("first reminder = %+v", got)
	}
	coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryInterval) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("retry Scan: %v", err)
	}
	if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || calls != 2 {
		t.Fatalf("retry reminder=%+v calls=%d", got, calls)
	}
}

func TestAutoReleaseDeferredJobWithResourcesRetries(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	reminder := scheduledAutoRelease(now)
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
	reminder.AutoReleaseStartedAt = now.Format(time.RFC3339)
	reminder.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
	reminder.AutoReleaseAttempts = 1
	store := newAutoReleaseTestStore(reminder)
	coordinator, _, _ := newAutoReleaseTestCoordinator(now.Add(time.Minute), store)
	coordinator.Jobs = &autoReleaseTestJobs{jobs: []Job{{ID: "destroy-1", Type: "aws-destroy", Profile: "mac", AppleEmail: "apple@example.com", Status: JobStatusDeferred, StartedAt: now, LastError: "host pending"}}}
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		return AWSStatus{Hosts: []DedicatedHostStatus{autoReleaseTestHost("pending")}, ElasticIP: ElasticIP{AllocationID: "eipalloc-1"}}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying || !strings.Contains(got.AutoReleaseLastError, "pending") {
		t.Fatalf("partial reminder = %+v", got)
	}
}

func scheduledAutoRelease(deadline time.Time) ReleaseReminder {
	return ReleaseReminder{ProfileName: "mac", AppleEmail: "apple@example.com", ReleaseDueAt: deadline.Add(-AutoReleaseGracePeriod).Format(time.RFC3339), LastNotifiedAt: deadline.Add(-AutoReleaseGracePeriod).Format(time.RFC3339), Status: ReleaseReminderStatusDueNotified, AutoReleaseEnabled: true, AutoReleaseAt: deadline.Format(time.RFC3339), AutoReleaseState: ReleaseReminderAutoReleaseStateScheduled}
}

func autoReleaseTestProfile() Profile {
	return Profile{Name: "mac", AWS: AWSConfig{AccountEmail: "apple@example.com", Profile: "aws-test", Region: "us-west-2"}}
}

func autoReleaseTestManagedTags() []AWSTagConfig {
	return []AWSTagConfig{
		{Key: "cm-managed", Value: "true"},
		{Key: "cm-profile", Value: "mac"},
		{Key: "cm-account-email", Value: "apple@example.com"},
	}
}

func autoReleaseTestHost(state string) DedicatedHostStatus {
	return DedicatedHostStatus{HostID: "h-1", State: state, Tags: autoReleaseTestManagedTags()}
}

func autoReleaseTestInstance(state string) InstanceStatus {
	return InstanceStatus{InstanceID: "i-1", HostID: "h-1", State: state, Tags: autoReleaseTestManagedTags()}
}

type autoReleaseTestStore struct {
	mu             sync.Mutex
	reminders      map[string]ReleaseReminder
	beforeUpdate   func(*ReleaseReminder)
	cleanupCalls   int
	completeCycles []ReleaseReminderCycle
	cleanupErrors  []error
}

func newAutoReleaseTestStore(reminders ...ReleaseReminder) *autoReleaseTestStore {
	s := &autoReleaseTestStore{reminders: map[string]ReleaseReminder{}}
	for _, reminder := range reminders {
		s.reminders[reminder.ProfileName] = reminder
	}
	return s
}

func (s *autoReleaseTestStore) ListReleaseReminders(string) ([]ReleaseReminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ReleaseReminder, 0, len(s.reminders))
	for _, reminder := range s.reminders {
		out = append(out, reminder)
	}
	return out, nil
}

func (s *autoReleaseTestStore) ReleaseReminder(profile string) (ReleaseReminder, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reminder, ok := s.reminders[profile]
	return reminder, ok, nil
}

func (s *autoReleaseTestStore) UpdateReleaseReminder(profile string, update func(ReleaseReminder) (ReleaseReminder, error)) (ReleaseReminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reminder, ok := s.reminders[profile]
	if !ok {
		return ReleaseReminder{}, fmt.Errorf("missing reminder %s", profile)
	}
	if s.beforeUpdate != nil {
		s.beforeUpdate(&reminder)
		s.reminders[profile] = reminder
	}
	updated, err := update(reminder)
	if err != nil {
		return ReleaseReminder{}, err
	}
	s.reminders[profile] = updated
	return updated, nil
}

func (s *autoReleaseTestStore) CompleteAutoRelease(cycle ReleaseReminderCycle, releasedAt string) (ReleaseReminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupCalls++
	if len(s.cleanupErrors) > 0 {
		err := s.cleanupErrors[0]
		s.cleanupErrors = s.cleanupErrors[1:]
		if err != nil {
			return ReleaseReminder{}, err
		}
	}
	reminder := s.reminders[cycle.ProfileName]
	if !releaseReminderMatchesCycle(reminder, cycle) || (reminder.AutoReleaseState != ReleaseReminderAutoReleaseStateRunning && reminder.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying) {
		return ReleaseReminder{}, ErrReleaseReminderCycleChanged
	}
	reminder.Status = ReleaseReminderStatusReleased
	reminder.ReleasedAt = releasedAt
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateReleased
	reminder.AutoReleaseLastError = ""
	s.reminders[cycle.ProfileName] = reminder
	s.completeCycles = append(s.completeCycles, cycle)
	return reminder, nil
}

func (s *autoReleaseTestStore) mutate(profile string, mutate func(*ReleaseReminder)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reminder := s.reminders[profile]
	mutate(&reminder)
	s.reminders[profile] = reminder
}

func (s *autoReleaseTestStore) get(profile string) ReleaseReminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reminders[profile]
}

type autoReleaseTestJobs struct{ jobs []Job }

func (j *autoReleaseTestJobs) Active() ([]Job, error) {
	active := make([]Job, 0)
	for _, job := range j.jobs {
		if job.Status == JobStatusStarting || job.Status == JobStatusRunning {
			active = append(active, job)
		}
	}
	return active, nil
}
func (j *autoReleaseTestJobs) List() ([]Job, error) { return append([]Job(nil), j.jobs...), nil }

func newAutoReleaseTestCoordinator(now time.Time, store *autoReleaseTestStore) (*AutoReleaseCoordinator, *[]AutoReleaseNotification, *[]Profile) {
	notifications := []AutoReleaseNotification{}
	starts := []Profile{}
	coordinator := &AutoReleaseCoordinator{
		Now:            func() time.Time { return now },
		Store:          store,
		Jobs:           &autoReleaseTestJobs{},
		ResolveProfile: func(context.Context, ReleaseReminder) (Profile, error) { return autoReleaseTestProfile(), nil },
		Status: func(context.Context, Profile) (AWSStatus, error) {
			return AWSStatus{Hosts: []DedicatedHostStatus{autoReleaseTestHost("available")}}, nil
		},
		StartDestroy: func(_ context.Context, profile Profile) (Job, error) {
			starts = append(starts, profile)
			return Job{ID: "destroy", Type: "aws-destroy", Profile: profile.Name, Status: JobStatusRunning, StartedAt: now}, nil
		},
		Notify: func(notification AutoReleaseNotification) error {
			notifications = append(notifications, notification)
			return nil
		},
	}
	return coordinator, &notifications, &starts
}
