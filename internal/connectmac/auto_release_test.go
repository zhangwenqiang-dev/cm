package connectmac

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcceptedReleaseConvergence(t *testing.T) {
	baseReminder := ReleaseReminder{HostID: "h-1"}
	baseJob := Job{Status: JobStatusSuccess, ReleaseEvidenceRecorded: true, ReleasedHosts: []string{"h-1"}}
	baseStatus := AWSStatus{Hosts: []DedicatedHostStatus{{HostID: "h-1", State: "pending"}}}

	tests := []struct {
		name     string
		reminder ReleaseReminder
		job      Job
		status   AWSStatus
		want     bool
	}{
		{name: "structured success", reminder: baseReminder, job: baseJob, status: baseStatus, want: true},
		{name: "structured deferred", reminder: baseReminder, job: Job{Status: JobStatusDeferred, ReleaseEvidenceRecorded: true, ReleasedHosts: []string{"h-other", "h-1"}}, status: baseStatus, want: true},
		{name: "pre-marker structured match", reminder: baseReminder, job: Job{Status: JobStatusSuccess, ReleasedHosts: []string{"h-1"}}, status: baseStatus, want: true},
		{name: "pre-marker structured mismatch", reminder: baseReminder, job: Job{Status: JobStatusDeferred, ReleasedHosts: []string{"h-other"}}, status: baseStatus},
		{name: "legacy success", reminder: baseReminder, job: Job{Status: JobStatusSuccess}, status: baseStatus, want: true},
		{name: "legacy deferred", reminder: baseReminder, job: Job{Status: JobStatusDeferred}, status: baseStatus, want: true},
		{name: "modern success without accepted host", reminder: baseReminder, job: Job{Status: JobStatusSuccess, ReleaseEvidenceRecorded: true}, status: baseStatus},
		{name: "modern deferred host transition without accepted host", reminder: baseReminder, job: Job{Status: JobStatusDeferred, ReleaseEvidenceRecorded: true}, status: baseStatus},
		{name: "retained unassociated EIP", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: baseStatus.Hosts, ElasticIP: ElasticIP{AllocationID: "eipalloc-retained", PublicIP: "203.0.113.10"}}, want: true},
		{name: "remaining instance", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: baseStatus.Hosts, Instances: []InstanceStatus{{InstanceID: "i-1"}}}},
		{name: "no host", reminder: baseReminder, job: baseJob, status: AWSStatus{}},
		{name: "multiple hosts", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: []DedicatedHostStatus{{HostID: "h-1", State: "pending"}, {HostID: "h-2", State: "pending"}}}},
		{name: "associated EIP", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: baseStatus.Hosts, ElasticIP: ElasticIP{AllocationID: "eipalloc-1", AssociationID: "eipassoc-1"}}},
		{name: "EIP instance remains", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: baseStatus.Hosts, ElasticIP: ElasticIP{AllocationID: "eipalloc-1", InstanceID: "i-1"}}},
		{name: "available host", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: []DedicatedHostStatus{{HostID: "h-1", State: "available"}}}},
		{name: "blank host state", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: []DedicatedHostStatus{{HostID: "h-1"}}}},
		{name: "unknown host state", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: []DedicatedHostStatus{{HostID: "h-1", State: "unknown"}}}},
		{name: "released host state", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: []DedicatedHostStatus{{HostID: "h-1", State: "released"}}}},
		{name: "padded host state", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: []DedicatedHostStatus{{HostID: "h-1", State: " pending "}}}},
		{name: "blank reminder host", reminder: ReleaseReminder{}, job: baseJob, status: baseStatus},
		{name: "padded reminder host", reminder: ReleaseReminder{HostID: " h-1 "}, job: baseJob, status: baseStatus},
		{name: "blank status host", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: []DedicatedHostStatus{{State: "pending"}}}},
		{name: "padded status host", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: []DedicatedHostStatus{{HostID: " h-1 ", State: "pending"}}}},
		{name: "mismatched host", reminder: baseReminder, job: baseJob, status: AWSStatus{Hosts: []DedicatedHostStatus{{HostID: "h-2", State: "pending"}}}},
		{name: "failed job", reminder: baseReminder, job: Job{Status: JobStatusFailed, ReleasedHosts: []string{"h-1"}}, status: baseStatus},
		{name: "running job", reminder: baseReminder, job: Job{Status: JobStatusRunning, ReleasedHosts: []string{"h-1"}}, status: baseStatus},
		{name: "structured release excludes host", reminder: baseReminder, job: Job{Status: JobStatusSuccess, ReleaseEvidenceRecorded: true, ReleasedHosts: []string{"h-other"}}, status: baseStatus},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := acceptedReleaseConverging(test.reminder, test.job, test.status); got != test.want {
				t.Fatalf("acceptedReleaseConverging() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestAcceptedReleaseConvergenceConstants(t *testing.T) {
	if AutoReleaseConvergenceWindow != 24*time.Hour {
		t.Fatalf("AutoReleaseConvergenceWindow = %s, want 24h", AutoReleaseConvergenceWindow)
	}
	if AutoReleaseStalledStatusInterval != 15*time.Minute {
		t.Fatalf("AutoReleaseStalledStatusInterval = %s, want 15m", AutoReleaseStalledStatusInterval)
	}
}

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

func TestApplyReleaseReminderExtensionCancelsScheduledCycleAndRejectsProtectedStatesOrShort(t *testing.T) {
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
	for _, state := range []string{
		ReleaseReminderAutoReleaseStateRunning,
		ReleaseReminderAutoReleaseStateRetrying,
		ReleaseReminderAutoReleaseStateNotifying,
	} {
		t.Run(state, func(t *testing.T) {
			protected := reminder
			protected.AutoReleaseState = state
			protected.AutoReleaseNotifiedAt = "2026-07-13T08:04:00Z"
			if got, err := applyReleaseReminderExtension(protected, now.Add(time.Hour), now, "member@example.com", "Member"); err == nil {
				t.Fatalf("%s extension error = nil; reminder=%+v", state, got)
			} else if !reflect.DeepEqual(got, protected) {
				t.Fatalf("%s extension changed protected reminder:\n got: %+v\nwant: %+v", state, got, protected)
			}
		})
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
	statusCalls := 0
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		statusCalls++
		return AWSStatus{Hosts: []DedicatedHostStatus{autoReleaseTestHost("available")}}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || statusCalls != 1 || len(*starts) != 0 {
		t.Fatalf("timeout status=%d starts=%d reminder=%+v", statusCalls, len(*starts), got)
	}
	if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationFinalFailure {
		t.Fatalf("notifications = %+v", *notifications)
	}
}

func TestAutoReleaseAtRetryDeadlineCompletesCleanResourcesWithoutDestroy(t *testing.T) {
	started := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	reminder := scheduledAutoRelease(started)
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
	reminder.AutoReleaseStartedAt = started.Format(time.RFC3339)
	reminder.AutoReleaseLastAttemptAt = started.Add(59 * time.Minute).Format(time.RFC3339)
	reminder.AutoReleaseAttempts = 12
	store := newAutoReleaseTestStore(reminder)
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(started.Add(AutoReleaseRetryWindow), store)
	statusCalls := 0
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		statusCalls++
		return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got := store.get("mac")
	if got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || statusCalls != 1 || len(*starts) != 0 || store.cleanupCalls != 1 {
		t.Fatalf("deadline completion status=%d starts=%d cleanup=%d reminder=%+v", statusCalls, len(*starts), store.cleanupCalls, got)
	}
	if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationSuccess {
		t.Fatalf("notifications = %+v", *notifications)
	}
}

func TestAutoReleaseAtRetryDeadlineObservesActiveDestroyWithoutMutation(t *testing.T) {
	started := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	reminder := scheduledAutoRelease(started)
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
	reminder.AutoReleaseStartedAt = started.Format(time.RFC3339)
	reminder.AutoReleaseLastAttemptAt = started.Add(55 * time.Minute).Format(time.RFC3339)
	reminder.AutoReleaseAttempts = 12
	store := newAutoReleaseTestStore(reminder)
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(started.Add(AutoReleaseRetryWindow), store)
	coordinator.Jobs = &autoReleaseTestJobs{jobs: []Job{{
		ID: "destroy-active", Type: "aws-destroy", Profile: reminder.ProfileName,
		AppleEmail: reminder.AppleEmail, Status: JobStatusRunning, StartedAt: started.Add(55 * time.Minute),
	}}}
	statusCalls := 0
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		statusCalls++
		return AWSStatus{}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := store.get("mac"); !reflect.DeepEqual(got, reminder) || statusCalls != 0 || len(*starts) != 0 || len(*notifications) != 0 {
		t.Fatalf("active deadline status=%d starts=%d notifications=%+v reminder=%+v", statusCalls, len(*starts), *notifications, got)
	}
}

func TestAutoReleaseTerminalJobStatusReadRetriesAcrossMutationDeadline(t *testing.T) {
	for _, jobStatus := range []JobStatus{JobStatusSuccess, JobStatusDeferred} {
		t.Run(string(jobStatus), func(t *testing.T) {
			started := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
			lastAttempt := started.Add(55 * time.Minute)
			reminder := scheduledAutoRelease(started)
			reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
			reminder.AutoReleaseStartedAt = started.Format(time.RFC3339)
			reminder.AutoReleaseLastAttemptAt = lastAttempt.Format(time.RFC3339)
			reminder.AutoReleaseAttempts = 12
			store := newAutoReleaseTestStore(reminder)
			coordinator, notifications, starts := newAutoReleaseTestCoordinator(started.Add(AutoReleaseRetryWindow), store)
			coordinator.Jobs = &autoReleaseTestJobs{jobs: []Job{{
				ID: "destroy-terminal", Type: "aws-destroy", Profile: reminder.ProfileName,
				AppleEmail: reminder.AppleEmail, Status: jobStatus, StartedAt: lastAttempt,
			}}}
			statusCalls := 0
			coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
				statusCalls++
				if statusCalls == 1 {
					return AWSStatus{}, RecoverableAutoReleaseError(errors.New("temporary status timeout"))
				}
				return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}, nil
			}

			if err := coordinator.Scan(context.Background()); err != nil {
				t.Fatalf("first Scan: %v", err)
			}
			if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying || got.Status == ReleaseReminderStatusReleased || !strings.Contains(got.AutoReleaseLastError, "timeout") || len(*starts) != 0 {
				t.Fatalf("first status=%d starts=%d reminder=%+v", statusCalls, len(*starts), got)
			}

			coordinator.Now = func() time.Time { return started.Add(AutoReleaseRetryWindow + time.Minute) }
			if err := coordinator.Scan(context.Background()); err != nil {
				t.Fatalf("second Scan: %v", err)
			}
			if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || statusCalls != 2 || len(*starts) != 0 || store.cleanupCalls != 1 {
				t.Fatalf("second status=%d starts=%d cleanup=%d reminder=%+v", statusCalls, len(*starts), store.cleanupCalls, got)
			}
			if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationSuccess {
				t.Fatalf("notifications = %+v", *notifications)
			}
		})
	}
}

func TestAutoReleaseSuccessfulJobKeepsLaterReadOnlyRetriesAliveAtDeadline(t *testing.T) {
	started := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	reminder := scheduledAutoRelease(started)
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
	reminder.AutoReleaseStartedAt = started.Format(time.RFC3339)
	reminder.AutoReleaseLastAttemptAt = started.Add(55 * time.Minute).Format(time.RFC3339)
	reminder.AutoReleaseAttempts = 12
	store := newAutoReleaseTestStore(reminder)
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(started.Add(AutoReleaseRetryWindow), store)
	coordinator.Jobs = &autoReleaseTestJobs{jobs: []Job{{
		ID: "destroy-success", Type: "aws-destroy", Profile: reminder.ProfileName,
		AppleEmail: reminder.AppleEmail, Status: JobStatusSuccess, StartedAt: started.Add(30 * time.Minute),
	}}}
	statusCalls := 0
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		statusCalls++
		if statusCalls == 1 {
			return AWSStatus{}, RecoverableAutoReleaseError(errors.New("temporary status timeout"))
		}
		return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying || !strings.Contains(got.AutoReleaseLastError, "timeout") || len(*starts) != 0 {
		t.Fatalf("first status=%d starts=%d reminder=%+v", statusCalls, len(*starts), got)
	}

	coordinator.Now = func() time.Time { return started.Add(AutoReleaseRetryWindow + time.Minute) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || statusCalls != 2 || len(*starts) != 0 || store.cleanupCalls != 1 {
		t.Fatalf("second status=%d starts=%d cleanup=%d reminder=%+v", statusCalls, len(*starts), store.cleanupCalls, got)
	}
	if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationSuccess {
		t.Fatalf("notifications = %+v", *notifications)
	}
}

func TestAutoReleaseCompletedJobKeepsReadOnlyReconciliationUntilResourcesClean(t *testing.T) {
	for _, jobStatus := range []JobStatus{JobStatusSuccess, JobStatusDeferred} {
		t.Run(string(jobStatus), func(t *testing.T) {
			started := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
			lastAttempt := started.Add(55 * time.Minute)
			reminder := scheduledAutoRelease(started)
			reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
			reminder.AutoReleaseStartedAt = started.Format(time.RFC3339)
			reminder.AutoReleaseLastAttemptAt = lastAttempt.Format(time.RFC3339)
			reminder.AutoReleaseAttempts = 12
			store := newAutoReleaseTestStore(reminder)
			coordinator, notifications, starts := newAutoReleaseTestCoordinator(started.Add(AutoReleaseRetryWindow), store)
			coordinator.Jobs = &autoReleaseTestJobs{jobs: []Job{{
				ID: "destroy-completed", Type: "aws-destroy", Profile: reminder.ProfileName,
				AppleEmail: reminder.AppleEmail, Status: jobStatus, StartedAt: lastAttempt,
			}}}
			statusCalls := 0
			coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
				statusCalls++
				if statusCalls == 1 {
					return AWSStatus{
						Hosts:     []DedicatedHostStatus{autoReleaseTestHost("pending")},
						ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"},
					}, nil
				}
				return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}, nil
			}

			if err := coordinator.Scan(context.Background()); err != nil {
				t.Fatalf("resources-remain Scan: %v", err)
			}
			first := store.get("mac")
			if first.Status == ReleaseReminderStatusReleased || first.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying || !strings.Contains(first.AutoReleaseLastError, "resources remain") || statusCalls != 1 || len(*starts) != 0 || store.cleanupCalls != 0 {
				t.Fatalf("resources-remain status=%d starts=%d cleanup=%d reminder=%+v", statusCalls, len(*starts), store.cleanupCalls, first)
			}
			if len(*notifications) != 0 {
				t.Fatalf("resources-remain notifications = %+v", *notifications)
			}

			coordinator.Now = func() time.Time { return started.Add(AutoReleaseRetryWindow + time.Minute) }
			if err := coordinator.Scan(context.Background()); err != nil {
				t.Fatalf("clean Scan: %v", err)
			}
			if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || statusCalls != 2 || len(*starts) != 0 || store.cleanupCalls != 1 {
				t.Fatalf("clean status=%d starts=%d cleanup=%d reminder=%+v", statusCalls, len(*starts), store.cleanupCalls, got)
			}
			if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationSuccess {
				t.Fatalf("clean notifications = %+v", *notifications)
			}
		})
	}
}

func TestAutoReleaseExpiredReadOnlyStatusErrorsNotifyFirstFailureOnceThenComplete(t *testing.T) {
	started := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	lastAttempt := started.Add(55 * time.Minute)
	reminder := scheduledAutoRelease(started)
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
	reminder.AutoReleaseStartedAt = started.Format(time.RFC3339)
	reminder.AutoReleaseLastAttemptAt = lastAttempt.Format(time.RFC3339)
	reminder.AutoReleaseAttempts = 1
	store := newAutoReleaseTestStore(reminder)
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(started.Add(AutoReleaseRetryWindow), store)
	coordinator.Jobs = &autoReleaseTestJobs{jobs: []Job{{
		ID: "destroy-success", Type: "aws-destroy", Profile: reminder.ProfileName,
		AppleEmail: reminder.AppleEmail, Status: JobStatusSuccess, StartedAt: lastAttempt,
	}}}
	statusCalls := 0
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		statusCalls++
		if statusCalls <= 2 {
			return AWSStatus{}, RecoverableAutoReleaseError(fmt.Errorf("temporary status timeout %d", statusCalls))
		}
		return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("first status-error Scan: %v", err)
	}
	coordinator.Now = func() time.Time { return started.Add(AutoReleaseRetryWindow + time.Minute) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("second status-error Scan: %v", err)
	}
	if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying || got.AutoReleaseAttempts != 1 || !strings.Contains(got.AutoReleaseLastError, "timeout 2") || statusCalls != 2 || len(*starts) != 0 {
		t.Fatalf("status errors status=%d starts=%d reminder=%+v", statusCalls, len(*starts), got)
	}
	if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationFirstFailure {
		t.Fatalf("status-error notifications = %+v", *notifications)
	}

	coordinator.Now = func() time.Time { return started.Add(AutoReleaseRetryWindow + 2*time.Minute) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("clean Scan: %v", err)
	}
	if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || statusCalls != 3 || len(*starts) != 0 || store.cleanupCalls != 1 {
		t.Fatalf("clean status=%d starts=%d cleanup=%d reminder=%+v", statusCalls, len(*starts), store.cleanupCalls, got)
	}
	if len(*notifications) != 2 || (*notifications)[0].Kind != AutoReleaseNotificationFirstFailure || (*notifications)[1].Kind != AutoReleaseNotificationSuccess {
		t.Fatalf("completion notifications = %+v", *notifications)
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

func TestAutoReleaseCleanupFailureRetriesThenSucceedsWithoutResendingNotification(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	store.cleanupErrors = []error{errors.New("local store temporarily unavailable"), nil}
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(now, store)
	events := make([]AutoReleaseEvent, 0, 4)
	coordinator.Emit = func(event AutoReleaseEvent) { events = append(events, event) }
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-1"}}, nil
	}

	if err := coordinator.Scan(context.Background()); err == nil || autoReleaseErrorCategoryOf(err) != autoReleaseErrorRecoverable {
		t.Fatalf("first Scan error = %v, want recoverable", err)
	}
	if got := store.get("mac"); got.Status == ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying || got.AutoReleaseNotifiedAt == "" || store.cleanupCalls != 1 || len(*starts) != 0 {
		t.Fatalf("failed cleanup persisted terminal state: reminder=%+v cleanup=%d starts=%d", got, store.cleanupCalls, len(*starts))
	}
	if len(*notifications) != 1 || (*notifications)[0].Kind != AutoReleaseNotificationSuccess {
		t.Fatalf("first notifications = %+v", *notifications)
	}
	pendingEvent := -1
	cleanupRetryEvent := -1
	for i, event := range events {
		if event.Action == "notification-pending" {
			pendingEvent = i
		}
		if event.Action == "cleanup-retrying" {
			cleanupRetryEvent = i
		}
	}
	if pendingEvent < 0 || cleanupRetryEvent <= pendingEvent {
		t.Fatalf("first events = %+v", events)
	}
	coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryInterval) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("retry Scan: %v", err)
	}
	if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || store.cleanupCalls != 2 || len(*notifications) != 1 {
		t.Fatalf("cleanup retry did not complete: reminder=%+v cleanup=%d", got, store.cleanupCalls)
	}
}

func TestAutoReleaseCleanupFailureRetriesBeyondAWSRetryWindow(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	store.cleanupErrors = []error{errors.New("local cleanup failed"), nil}
	coordinator, notifications, _ := newAutoReleaseTestCoordinator(now, store)
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) { return AWSStatus{}, nil }

	if err := coordinator.Scan(context.Background()); err == nil || autoReleaseErrorCategoryOf(err) != autoReleaseErrorRecoverable {
		t.Fatalf("first Scan error = %v, want recoverable", err)
	}
	coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryWindow) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("timeout Scan: %v", err)
	}
	if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || store.cleanupCalls != 2 || len(*notifications) != 1 {
		t.Fatalf("cleanup after retry window reminder=%+v cleanup=%d notifications=%+v", got, store.cleanupCalls, *notifications)
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
		RequestID:           "req-destroy-aaronjasonall-use1",
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
	events := make([]AutoReleaseEvent, 0, 3)
	coordinator.Status = func(_ context.Context, profile Profile) (AWSStatus, error) {
		if len(events) != 1 || events[0].Action != "job.observed" || events[0].JobID != jobs.jobs[0].ID || events[0].RequestID != jobs.jobs[0].RequestID || !strings.Contains(events[0].Message, "destroy-aaronjasonall-use1") || !strings.Contains(events[0].Message, string(JobStatusSuccess)) {
			t.Fatalf("events before AWS status = %+v", events)
		}
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
	if notification := (*notifications)[0]; notification.Reminder.ProfileName != reminder.ProfileName || notification.Reminder.AppleEmail != reminder.AppleEmail || notification.Reminder.HostID != reminder.HostID || notification.Reminder.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying || notification.Reminder.AutoReleaseNotifiedAt != "" || releaseReminderCycleFromReminder(notification.Reminder) != wantCycle {
		t.Fatalf("success notification reminder = %+v, want cycle %+v", notification.Reminder, wantCycle)
	}
	if statusCalls != 1 || statusProfile.Name != reminder.ProfileName || statusProfile.AWS.AccountEmail != reminder.AppleEmail || statusProfile.AWS.Region != "us-east-1" {
		t.Fatalf("Status calls = %d, profile = %+v", statusCalls, statusProfile)
	}
	if len(observedStatus.Hosts) != 0 || len(observedStatus.Instances) != 0 || observedStatus.ElasticIP.AllocationID != "eipalloc-aaronjasonall-use1" || observedStatus.ElasticIP.AssociationID != "" || observedStatus.ElasticIP.InstanceID != "" {
		t.Fatalf("observed status was not clean with retained EIP: %+v", observedStatus)
	}
	if store.markCalls != 1 || store.cleanupCalls != 1 || len(store.completeCycles) != 1 || store.completeCycles[0] != wantCycle {
		t.Fatalf("marker calls = %d, completed cycles = %+v, cleanup calls = %d, want exactly %+v", store.markCalls, store.completeCycles, store.cleanupCalls, wantCycle)
	}
	observedEvents := 0
	releasedEvents := 0
	for _, event := range events {
		if event.Action == "job.observed" {
			observedEvents++
		}
		if event.Action == "released" {
			releasedEvents++
			if event.Reminder.ProfileName != reminder.ProfileName || !strings.Contains(event.Message, "eip_retained=true") {
				t.Fatalf("released event = %+v", event)
			}
		}
	}
	if observedEvents != 1 || releasedEvents != 1 {
		t.Fatalf("observed events = %d, released events = %d, events = %+v", observedEvents, releasedEvents, events)
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
	var notificationAttempts []int
	var notificationRetries []bool
	var notificationCycleIDs []string
	var notificationErrors []string
	coordinator.Notify = func(notification AutoReleaseNotification) error {
		if notification.Kind != AutoReleaseNotificationSuccess {
			t.Fatalf("notification = %+v", notification)
		}
		calls++
		notificationAttempts = append(notificationAttempts, notification.Attempt)
		notificationRetries = append(notificationRetries, notification.Retrying)
		notificationCycleIDs = append(notificationCycleIDs, notification.CycleID)
		notificationErrors = append(notificationErrors, notification.Error)
		if calls == 1 {
			return errors.New("wechat temporarily unavailable")
		}
		return nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	first := store.get("mac")
	if first.Status == ReleaseReminderStatusReleased || first.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying || first.AutoReleaseNotifiedAt != "" || first.AutoReleaseAttempts != 1 || !strings.Contains(first.AutoReleaseLastError, "wechat") {
		t.Fatalf("first reminder = %+v", first)
	}
	if calls != 1 || store.markCalls != 0 || store.cleanupCalls != 0 {
		t.Fatalf("first calls=%d marks=%d cleanup=%d", calls, store.markCalls, store.cleanupCalls)
	}
	coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryWindow) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("retry Scan: %v", err)
	}
	if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || got.AutoReleaseAttempts != 2 || got.AutoReleaseStartedAt != first.AutoReleaseStartedAt || calls != 2 || store.markCalls != 1 || store.cleanupCalls != 1 {
		t.Fatalf("retry reminder=%+v calls=%d marks=%d cleanup=%d", got, calls, store.markCalls, store.cleanupCalls)
	}
	if !reflect.DeepEqual(notificationAttempts, []int{1, 2}) || !reflect.DeepEqual(notificationRetries, []bool{false, true}) || len(notificationCycleIDs) != 2 || notificationCycleIDs[0] == "" || notificationCycleIDs[0] != notificationCycleIDs[1] {
		t.Fatalf("notification retry context attempts=%v retrying=%v cycles=%v", notificationAttempts, notificationRetries, notificationCycleIDs)
	}
	if notificationErrors[0] != "" || !strings.Contains(notificationErrors[1], "wechat") {
		t.Fatalf("notification retry errors = %v", notificationErrors)
	}
}

func TestAutoReleaseUnmarkedNotificationBlocksWhenResourcesReappear(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	coordinator, _, starts := newAutoReleaseTestCoordinator(now, store)
	statusCalls := 0
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		statusCalls++
		if statusCalls == 1 {
			return AWSStatus{}, nil
		}
		return AWSStatus{Hosts: []DedicatedHostStatus{autoReleaseTestHost("available")}}, nil
	}
	notificationCalls := 0
	coordinator.Notify = func(notification AutoReleaseNotification) error {
		if notification.Kind != AutoReleaseNotificationSuccess {
			t.Fatalf("notification = %+v", notification)
		}
		notificationCalls++
		return errors.New("wechat temporarily unavailable")
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("notification Scan: %v", err)
	}
	if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying || got.AutoReleaseNotifiedAt != "" || notificationCalls != 1 {
		t.Fatalf("notification failure reminder=%+v calls=%d", got, notificationCalls)
	}

	coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryInterval) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("resource reappearance Scan: %v", err)
	}
	if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || got.AutoReleaseNotifiedAt != "" || !strings.Contains(got.AutoReleaseLastError, "reappeared") || notificationCalls != 1 || len(*starts) != 0 || store.cleanupCalls != 0 {
		t.Fatalf("resource reappearance reminder=%+v notifications=%d starts=%d", got, notificationCalls, len(*starts))
	}

	coordinator.Now = func() time.Time { return now.Add(2 * AutoReleaseRetryInterval) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("blocked follow-up Scan: %v", err)
	}
	if got := store.get("mac"); got.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || notificationCalls != 1 || len(*starts) != 0 || store.cleanupCalls != 0 || statusCalls != 2 {
		t.Fatalf("blocked follow-up reminder=%+v notifications=%d starts=%d cleanup=%d status=%d", got, notificationCalls, len(*starts), store.cleanupCalls, statusCalls)
	}
}

func TestAutoReleaseRestartNotifyingWithoutMarkerRetriesNotification(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	reminder := notifyingAutoRelease(now.Add(-AutoReleaseRetryInterval), "")
	store := newAutoReleaseTestStore(reminder)
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(now, store)
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got := store.get("mac")
	if got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || got.AutoReleaseNotifiedAt == "" {
		t.Fatalf("reminder = %+v", got)
	}
	if len(*notifications) != 1 || store.markCalls != 1 || store.cleanupCalls != 1 || len(*starts) != 0 {
		t.Fatalf("notifications=%+v marks=%d cleanup=%d starts=%d", *notifications, store.markCalls, store.cleanupCalls, len(*starts))
	}
}

func TestAutoReleaseRestartNotifyingWithMarkerSkipsNotification(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	reminder := notifyingAutoRelease(now.Add(-AutoReleaseRetryInterval), now.Add(-time.Minute).Format(time.RFC3339))
	store := newAutoReleaseTestStore(reminder)
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(now, store)
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) { return AWSStatus{}, nil }

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased {
		t.Fatalf("reminder = %+v", got)
	}
	if len(*notifications) != 0 || store.markCalls != 0 || store.cleanupCalls != 1 || len(*starts) != 0 {
		t.Fatalf("notifications=%+v marks=%d cleanup=%d starts=%d", *notifications, store.markCalls, store.cleanupCalls, len(*starts))
	}
}

func TestAutoReleaseNotificationClaimRejectsConcurrentStaleCoordinator(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(notifyingAutoRelease(now.Add(-AutoReleaseRetryInterval), ""))
	statusEntered := make(chan struct{}, 2)
	releaseStatus := make(chan struct{})
	var releaseStatusOnce sync.Once
	t.Cleanup(func() { releaseStatusOnce.Do(func() { close(releaseStatus) }) })
	firstNotificationStarted := make(chan struct{})
	releaseFirstNotification := make(chan struct{})
	var releaseFirstOnce sync.Once
	t.Cleanup(func() { releaseFirstOnce.Do(func() { close(releaseFirstNotification) }) })
	var notificationCalls atomic.Int32

	newCoordinator := func() *AutoReleaseCoordinator {
		coordinator, _, _ := newAutoReleaseTestCoordinator(now, store)
		coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
			statusEntered <- struct{}{}
			<-releaseStatus
			return AWSStatus{}, nil
		}
		coordinator.Notify = func(notification AutoReleaseNotification) error {
			if notification.Kind != AutoReleaseNotificationSuccess {
				t.Errorf("notification = %+v", notification)
			}
			if notificationCalls.Add(1) == 1 {
				close(firstNotificationStarted)
				<-releaseFirstNotification
			}
			return nil
		}
		return coordinator
	}

	results := make(chan error, 2)
	for range 2 {
		coordinator := newCoordinator()
		go func() { results <- coordinator.Scan(context.Background()) }()
	}
	for range 2 {
		select {
		case <-statusEntered:
		case <-time.After(time.Second):
			t.Fatal("coordinator did not reach AWS status barrier")
		}
	}
	releaseStatusOnce.Do(func() { close(releaseStatus) })
	select {
	case <-firstNotificationStarted:
	case <-time.After(time.Second):
		t.Fatal("first notification did not start")
	}
	var firstResult error
	select {
	case firstResult = <-results:
	case <-time.After(time.Second):
		t.Fatal("stale coordinator did not finish while first notification was held")
	}
	observedCalls := notificationCalls.Load()
	releaseFirstOnce.Do(func() { close(releaseFirstNotification) })
	var secondResult error
	select {
	case secondResult = <-results:
	case <-time.After(time.Second):
		t.Fatal("first coordinator did not finish")
	}
	if firstResult != nil || secondResult != nil {
		t.Fatalf("Scan errors = %v, %v", firstResult, secondResult)
	}
	if observedCalls != 1 || notificationCalls.Load() != 1 {
		t.Fatalf("notification calls while claimed=%d final=%d, want exactly 1", observedCalls, notificationCalls.Load())
	}
	if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || store.markCalls != 1 || store.cleanupCalls != 1 {
		t.Fatalf("reminder=%+v marks=%d cleanup=%d", got, store.markCalls, store.cleanupCalls)
	}
}

func TestAutoReleaseNotificationMarkerFailureIsExplicitAndMayDuplicate(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 10, 0, 0, time.UTC)
	store := newAutoReleaseTestStore(scheduledAutoRelease(now))
	store.markErrors = []error{errors.New("marker database unavailable"), nil}
	coordinator, notifications, _ := newAutoReleaseTestCoordinator(now, store)
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) { return AWSStatus{}, nil }
	events := make([]AutoReleaseEvent, 0, 4)
	coordinator.Emit = func(event AutoReleaseEvent) { events = append(events, event) }

	err := coordinator.Scan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "may be duplicated") {
		t.Fatalf("first Scan error = %v", err)
	}
	if got := store.get("mac"); got.Status == ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying || got.AutoReleaseNotifiedAt != "" {
		t.Fatalf("first reminder = %+v", got)
	}
	if len(*notifications) != 1 || store.cleanupCalls != 0 {
		t.Fatalf("first notifications=%+v cleanup=%d", *notifications, store.cleanupCalls)
	}
	if len(events) == 0 || events[len(events)-1].Action != "notification-persistence-ambiguous" || !strings.Contains(events[len(events)-1].Message, "may be duplicated") {
		t.Fatalf("first events = %+v", events)
	}

	coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryInterval) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("retry Scan: %v", err)
	}
	if got := store.get("mac"); got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || len(*notifications) != 2 || store.markCalls != 2 || store.cleanupCalls != 1 {
		t.Fatalf("retry reminder=%+v notifications=%+v marks=%d cleanup=%d", got, *notifications, store.markCalls, store.cleanupCalls)
	}
}

func TestAutoReleaseMarkedNotificationBlocksWhenResourcesReappear(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	marker := now.Add(-time.Minute).Format(time.RFC3339)
	store := newAutoReleaseTestStore(notifyingAutoRelease(now.Add(-AutoReleaseRetryInterval), marker))
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(now, store)
	statusCalls := 0
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		statusCalls++
		if statusCalls == 1 {
			return AWSStatus{Hosts: []DedicatedHostStatus{autoReleaseTestHost("available")}}, nil
		}
		return AWSStatus{ElasticIP: ElasticIP{AllocationID: "eipalloc-retained"}}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if got := store.get("mac"); got.Status == ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || got.AutoReleaseNotifiedAt != marker || !strings.Contains(got.AutoReleaseLastError, "reappeared") || store.cleanupCalls != 0 {
		t.Fatalf("first reminder=%+v cleanup=%d", got, store.cleanupCalls)
	}
	coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryInterval) }
	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("blocked follow-up Scan: %v", err)
	}
	if got := store.get("mac"); got.Status == ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || got.AutoReleaseNotifiedAt != marker || len(*notifications) != 0 || len(*starts) != 0 || store.cleanupCalls != 0 || statusCalls != 1 {
		t.Fatalf("blocked follow-up reminder=%+v notifications=%+v starts=%d cleanup=%d status=%d", got, *notifications, len(*starts), store.cleanupCalls, statusCalls)
	}
}

func TestAutoReleaseNotificationPendingSafetyBlocksAllNonCleanResources(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	statuses := []struct {
		name   string
		status AWSStatus
	}{
		{
			name: "wrong tags",
			status: AWSStatus{Hosts: []DedicatedHostStatus{{
				HostID: "h-wrong-tags", State: "available",
				Tags: []AWSTagConfig{{Key: "cm-managed", Value: "true"}},
			}}},
		},
		{
			name: "ambiguous multiple resources",
			status: AWSStatus{Hosts: []DedicatedHostStatus{
				autoReleaseTestHost("available"),
				{HostID: "h-2", State: "available", Tags: autoReleaseTestManagedTags()},
			}},
		},
		{
			name: "unmanaged eip association",
			status: AWSStatus{ElasticIP: ElasticIP{
				AllocationID: "eipalloc-unmanaged", AssociationID: "eipassoc-unmanaged", InstanceID: "i-unmanaged",
			}},
		},
	}
	markers := []struct {
		name  string
		value string
	}{
		{name: "without marker"},
		{name: "with marker", value: now.Add(-time.Minute).Format(time.RFC3339)},
	}

	for _, statusCase := range statuses {
		for _, markerCase := range markers {
			t.Run(statusCase.name+"/"+markerCase.name, func(t *testing.T) {
				reminder := notifyingAutoRelease(now.Add(-AutoReleaseRetryInterval), markerCase.value)
				store := newAutoReleaseTestStore(reminder)
				coordinator, notifications, starts := newAutoReleaseTestCoordinator(now, store)
				statusCalls := 0
				coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
					statusCalls++
					return statusCase.status, nil
				}

				if err := coordinator.Scan(context.Background()); err != nil {
					t.Fatalf("safety-block Scan: %v", err)
				}
				blocked := store.get("mac")
				if blocked.Status == ReleaseReminderStatusReleased || blocked.AutoReleaseState != ReleaseReminderAutoReleaseStateFailed || blocked.AutoReleaseNotifiedAt != markerCase.value || !strings.Contains(blocked.AutoReleaseLastError, "reappeared") {
					t.Fatalf("blocked reminder = %+v", blocked)
				}
				if statusCalls != 1 || len(*notifications) != 0 || len(*starts) != 0 || store.markCalls != 0 || store.cleanupCalls != 0 {
					t.Fatalf("blocked effects status=%d notifications=%+v starts=%d marks=%d cleanup=%d", statusCalls, *notifications, len(*starts), store.markCalls, store.cleanupCalls)
				}

				coordinator.Now = func() time.Time { return now.Add(AutoReleaseRetryInterval) }
				if err := coordinator.Scan(context.Background()); err != nil {
					t.Fatalf("durable safety-block Scan: %v", err)
				}
				if got := store.get("mac"); !reflect.DeepEqual(got, blocked) || statusCalls != 1 || len(*notifications) != 0 || len(*starts) != 0 || store.markCalls != 0 || store.cleanupCalls != 0 {
					t.Fatalf("durable block reminder=%+v status=%d notifications=%+v starts=%d marks=%d cleanup=%d", got, statusCalls, *notifications, len(*starts), store.markCalls, store.cleanupCalls)
				}
			})
		}
	}
}

func TestAutoReleaseReleasedReminderIsNoOp(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	reminder := notifyingAutoRelease(now.Add(-AutoReleaseRetryInterval), now.Add(-time.Minute).Format(time.RFC3339))
	reminder.Status = ReleaseReminderStatusReleased
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateReleased
	reminder.ReleasedAt = now.Add(-time.Minute).Format(time.RFC3339)
	store := newAutoReleaseTestStore(reminder)
	coordinator, notifications, starts := newAutoReleaseTestCoordinator(now, store)
	statusCalls := 0
	coordinator.Status = func(context.Context, Profile) (AWSStatus, error) {
		statusCalls++
		return AWSStatus{}, nil
	}

	if err := coordinator.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(*notifications) != 0 || statusCalls != 0 || store.markCalls != 0 || store.cleanupCalls != 0 || len(*starts) != 0 {
		t.Fatalf("notifications=%+v status=%d marks=%d cleanup=%d starts=%d", *notifications, statusCalls, store.markCalls, store.cleanupCalls, len(*starts))
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

func TestAutoReleaseCycleIDUsesExactCycleTuple(t *testing.T) {
	base := ReleaseReminder{
		ProfileName:          "mac",
		AppleEmail:           "apple@example.com",
		HostID:               "h-1",
		OwnerEmail:           "owner@example.com",
		AutoReleaseAt:        "2026-08-20T08:30:00Z",
		AutoReleaseStartedAt: "2026-08-20T08:31:00Z",
		AutoReleaseAttempts:  1,
	}
	want := autoReleaseCycleID(base)
	if want == "" || strings.Contains(want, base.ProfileName) || strings.Contains(want, base.AppleEmail) || strings.Contains(want, base.HostID) {
		t.Fatalf("cycle ID is empty or exposes tuple data: %q", want)
	}
	nonCycleChange := base
	nonCycleChange.AutoReleaseAttempts = 99
	nonCycleChange.AutoReleaseLastAttemptAt = "2026-08-20T09:00:00Z"
	nonCycleChange.AutoReleaseLastError = "temporary"
	if got := autoReleaseCycleID(nonCycleChange); got != want {
		t.Fatalf("non-cycle state changed cycle ID: got %q want %q", got, want)
	}

	mutations := map[string]func(*ReleaseReminder){
		"profile":    func(reminder *ReleaseReminder) { reminder.ProfileName += "-new" },
		"auto at":    func(reminder *ReleaseReminder) { reminder.AutoReleaseAt = "2026-08-20T08:35:00Z" },
		"started at": func(reminder *ReleaseReminder) { reminder.AutoReleaseStartedAt = "2026-08-20T08:32:00Z" },
		"host":       func(reminder *ReleaseReminder) { reminder.HostID = "h-2" },
		"apple":      func(reminder *ReleaseReminder) { reminder.AppleEmail = "other@example.com" },
		"owner":      func(reminder *ReleaseReminder) { reminder.OwnerEmail = "other-owner@example.com" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if got := autoReleaseCycleID(changed); got == want {
				t.Fatalf("cycle tuple change did not change ID: %q", got)
			}
		})
	}
}

func TestAutoReleaseJobOutcomeSanitizesComprehensiveSecrets(t *testing.T) {
	secrets := []string{
		"task5-webhook-key",
		"task5-bearer-token",
		"task5-session-token",
		"/Users/test/.ssh/task5-private.pem",
		"AKIAIOSFODNN7EXAMPLE",
		"task5-aws-secret",
	}
	raw := errors.New(strings.Join([]string{
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + secrets[0],
		"Authorization: Bearer " + secrets[1],
		"session_token=" + secrets[2],
		"pem_path=" + secrets[3],
		"AWS_ACCESS_KEY_ID=" + secrets[4],
		"AWS_SECRET_ACCESS_KEY=" + secrets[5],
	}, "\n"))
	outcome := autoReleaseJobOutcome(RecoverableAutoReleaseError(raw), true, "")
	if outcome.ErrorCategory != JobErrorCategoryRecoverable || outcome.Reason == "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	for _, secret := range secrets {
		if strings.Contains(outcome.Reason, secret) {
			t.Fatalf("job outcome leaked %q: %s", secret, outcome.Reason)
		}
	}
	if strings.Contains(outcome.Reason, "qyapi.weixin.qq.com") {
		t.Fatalf("job outcome retained full webhook URL: %s", outcome.Reason)
	}
}

func scheduledAutoRelease(deadline time.Time) ReleaseReminder {
	return ReleaseReminder{ProfileName: "mac", AppleEmail: "apple@example.com", ReleaseDueAt: deadline.Add(-AutoReleaseGracePeriod).Format(time.RFC3339), LastNotifiedAt: deadline.Add(-AutoReleaseGracePeriod).Format(time.RFC3339), Status: ReleaseReminderStatusDueNotified, AutoReleaseEnabled: true, AutoReleaseAt: deadline.Format(time.RFC3339), AutoReleaseState: ReleaseReminderAutoReleaseStateScheduled}
}

func notifyingAutoRelease(lastAttempt time.Time, notifiedAt string) ReleaseReminder {
	reminder := scheduledAutoRelease(lastAttempt.Add(-time.Hour))
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateNotifying
	reminder.AutoReleaseStartedAt = lastAttempt.Add(-time.Hour).Format(time.RFC3339)
	reminder.AutoReleaseLastAttemptAt = lastAttempt.Format(time.RFC3339)
	reminder.AutoReleaseNotifiedAt = notifiedAt
	reminder.AutoReleaseAttempts = 1
	return reminder
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
	markCalls      int
	markErrors     []error
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
	if !releaseReminderMatchesCycle(reminder, cycle) || reminder.Status != ReleaseReminderStatusDueNotified || !reminder.AutoReleaseEnabled || reminder.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying || reminder.AutoReleaseNotifiedAt == "" {
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

func (s *autoReleaseTestStore) MarkAutoReleaseNotified(cycle ReleaseReminderCycle, notifiedAt string) (ReleaseReminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markCalls++
	if len(s.markErrors) > 0 {
		err := s.markErrors[0]
		s.markErrors = s.markErrors[1:]
		if err != nil {
			return ReleaseReminder{}, err
		}
	}
	reminder := s.reminders[cycle.ProfileName]
	if !releaseReminderMatchesCycle(reminder, cycle) || reminder.Status != ReleaseReminderStatusDueNotified || !reminder.AutoReleaseEnabled || reminder.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying {
		return ReleaseReminder{}, ErrReleaseReminderCycleChanged
	}
	if reminder.AutoReleaseNotifiedAt != "" {
		return reminder, nil
	}
	reminder.AutoReleaseNotifiedAt = notifiedAt
	s.reminders[cycle.ProfileName] = reminder
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
