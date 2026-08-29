package connectmac

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestAutoReleaseNotificationMarkerLoadsLegacyJSONAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "members.json")
	legacy := `{
  "release_reminders": [
    {
      "profile_name": "mac",
      "status": "due_notified",
      "auto_release_enabled": true,
      "auto_release_state": "notifying",
      "created_at": "2026-07-13T08:00:00Z",
      "updated_at": "2026-07-13T08:10:00Z"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy member data: %v", err)
	}

	db, err := NewMemberStore(path).Load()
	if err != nil {
		t.Fatalf("load legacy member data: %v", err)
	}
	if len(db.Reminders) != 1 || db.Reminders[0].AutoReleaseAcceptedAt != "" || db.Reminders[0].AutoReleaseStalledNotifiedAt != "" || db.Reminders[0].AutoReleaseNotifiedAt != "" {
		t.Fatalf("legacy reminder = %+v", db.Reminders)
	}
}

func TestAutoReleaseConvergenceMetadataPersistsThroughReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "members.json")
	store := NewMemberStore(path)
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseAcceptedAt = "2026-07-13T08:30:00Z"
	reminder.AutoReleaseStalledNotifiedAt = "2026-07-13T08:45:00Z"
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}

	reloaded, ok, err := NewMemberStore(path).ReleaseReminder(reminder.ProfileName)
	if err != nil || !ok || reloaded.AutoReleaseAcceptedAt != reminder.AutoReleaseAcceptedAt || reloaded.AutoReleaseStalledNotifiedAt != reminder.AutoReleaseStalledNotifiedAt {
		t.Fatalf("reloaded reminder = %+v ok=%t err=%v", reloaded, ok, err)
	}
}

func TestMemberStoreMarksAutoReleaseConvergenceAcceptedAtomically(t *testing.T) {
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "members.json")
	store := NewMemberStore(path)
	store.Now = func() time.Time { return now }
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseLastError = "old failure"
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}

	marked, transitioned, err := store.MarkAutoReleaseConvergenceAccepted(releaseReminderCycle(reminder), "")
	if err != nil {
		t.Fatalf("mark convergence accepted: %v", err)
	}
	wantAcceptedAt := now.Format(time.RFC3339)
	if !transitioned || marked.AutoReleaseAcceptedAt != wantAcceptedAt || marked.AutoReleaseLastError != "" || marked.AutoReleaseState != ReleaseReminderAutoReleaseStateRunning || marked.UpdatedAt != wantAcceptedAt {
		t.Fatalf("marked=%+v transitioned=%t", marked, transitioned)
	}
	reloaded, ok, err := NewMemberStore(path).ReleaseReminder(reminder.ProfileName)
	if err != nil || !ok || !reflect.DeepEqual(reloaded, marked) {
		t.Fatalf("reloaded=%+v ok=%t err=%v", reloaded, ok, err)
	}

	repeated, transitioned, err := store.MarkAutoReleaseConvergenceAccepted(releaseReminderCycle(reminder), now.Add(time.Minute).Format(time.RFC3339))
	if err != nil || transitioned || !reflect.DeepEqual(repeated, marked) {
		t.Fatalf("repeat=%+v transitioned=%t err=%v", repeated, transitioned, err)
	}
}

func TestMemberStoreMarksRetryingConvergenceAndRejectsStaleCycle(t *testing.T) {
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time { return now }
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
	reminder.AutoReleaseLastError = "retry failure"
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	stale := releaseReminderCycle(reminder)
	stale.HostID = "h-stale"
	if _, _, err := store.MarkAutoReleaseConvergenceAccepted(stale, now.Format(time.RFC3339)); !errors.Is(err, ErrReleaseReminderCycleChanged) {
		t.Fatalf("stale cycle error = %v", err)
	}
	marked, transitioned, err := store.MarkAutoReleaseConvergenceAccepted(releaseReminderCycle(reminder), now.Format(time.RFC3339))
	if err != nil || !transitioned || marked.AutoReleaseState != ReleaseReminderAutoReleaseStateRunning || marked.AutoReleaseLastError != "" {
		t.Fatalf("marked=%+v transitioned=%t err=%v", marked, transitioned, err)
	}
}

func TestMySQLMarksAutoReleaseConvergenceAcceptedAtomically(t *testing.T) {
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
	reminder.AutoReleaseLastError = "retry failure"
	reminder.CreatedAt = "2026-08-20T08:00:00Z"
	reminder.UpdatedAt = "2026-08-20T08:30:00Z"
	want := reminder
	want.AutoReleaseAcceptedAt = now.Format(time.RFC3339)
	want.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
	want.AutoReleaseLastError = ""
	want.UpdatedAt = now.Format(time.RFC3339)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	store := mysqlAutoReleaseTestStore(db, now)
	mock.ExpectBegin()
	expectMySQLAutoReleaseLockedReminder(mock, reminder)
	expectMySQLAutoReleaseReminderUpdate(mock, want)
	mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).WithArgs(mysqlStoreLockName).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, transitioned, err := store.MarkAutoReleaseConvergenceAccepted(releaseReminderCycle(reminder), "")
	if err != nil || !transitioned || !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%+v transitioned=%t err=%v", got, transitioned, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAutoReleaseStalledMarkerAndStatusClaimPersistAtomically(t *testing.T) {
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "members.json")
	store := MemberStore{Path: path, Now: func() time.Time { return now }}
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseAcceptedAt = now.Add(-24 * time.Hour).Format(time.RFC3339)
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatal(err)
	}
	marked, transitioned, err := store.MarkAutoReleaseStalledNotified(releaseReminderCycle(reminder), "")
	if err != nil || !transitioned || marked.AutoReleaseStalledNotifiedAt != now.Format(time.RFC3339) {
		t.Fatalf("marked=%+v transitioned=%t err=%v", marked, transitioned, err)
	}
	claimed, claimedOK, err := store.ClaimAutoReleaseConvergenceStatusCheck(releaseReminderCycle(reminder), "", AutoReleaseStalledStatusInterval)
	if err != nil || !claimedOK || claimed.AutoReleaseLastAttemptAt != now.Format(time.RFC3339) {
		t.Fatalf("claimed=%+v ok=%t err=%v", claimed, claimedOK, err)
	}
	reloaded, ok, err := store.ReleaseReminder(reminder.ProfileName)
	if err != nil || !ok || reloaded.AutoReleaseStalledNotifiedAt == "" || reloaded.AutoReleaseLastAttemptAt == "" {
		t.Fatalf("reloaded=%+v ok=%t err=%v", reloaded, ok, err)
	}
	if _, transitioned, err := store.MarkAutoReleaseStalledNotified(releaseReminderCycle(reminder), now.Add(time.Minute).Format(time.RFC3339)); err != nil || transitioned {
		t.Fatalf("repeat transition=%t err=%v", transitioned, err)
	}
	if _, claimedOK, err := store.ClaimAutoReleaseConvergenceStatusCheck(releaseReminderCycle(reminder), now.Add(time.Minute).Format(time.RFC3339), AutoReleaseStalledStatusInterval); err != nil || claimedOK {
		t.Fatalf("repeat claim=%t err=%v", claimedOK, err)
	}
}

func TestAutoReleaseAcceptedStoreOperationsRejectInvalidCycleState(t *testing.T) {
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		mutate func(*ReleaseReminder, *ReleaseReminderCycle)
	}{
		{name: "stale cycle", mutate: func(_ *ReleaseReminder, cycle *ReleaseReminderCycle) { cycle.HostID = "stale" }},
		{name: "stale accepted at", mutate: func(_ *ReleaseReminder, cycle *ReleaseReminderCycle) {
			cycle.AutoReleaseAcceptedAt = "2026-08-20T08:59:59Z"
		}},
		{name: "disabled", mutate: func(reminder *ReleaseReminder, _ *ReleaseReminderCycle) { reminder.AutoReleaseEnabled = false }},
		{name: "non-running", mutate: func(reminder *ReleaseReminder, _ *ReleaseReminderCycle) {
			reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "members.json")
			store := MemberStore{Path: path, Now: func() time.Time { return now }}
			reminder := runningAutoReleaseReminder("owner@example.com")
			reminder.AutoReleaseAcceptedAt = now.Add(-24 * time.Hour).Format(time.RFC3339)
			cycle := releaseReminderCycle(reminder)
			test.mutate(&reminder, &cycle)
			if _, err := store.UpsertReleaseReminder(reminder); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.MarkAutoReleaseStalledNotified(cycle, ""); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("marker err=%v", err)
			}
			if _, _, err := store.ClaimAutoReleaseConvergenceStatusCheck(cycle, "", AutoReleaseStalledStatusInterval); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("claim err=%v", err)
			}
		})
	}
}

func TestAutoReleaseFileStoreConvergenceStatusClaimHasSingleConcurrentWinner(t *testing.T) {
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "members.json")
	seed := MemberStore{Path: path, Now: func() time.Time { return now }}
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseAcceptedAt = now.Add(-AutoReleaseConvergenceWindow).Format(time.RFC3339)
	if _, err := seed.UpsertReleaseReminder(reminder); err != nil {
		t.Fatal(err)
	}
	cycle := releaseReminderCycle(reminder)
	start := make(chan struct{})
	results := make(chan struct {
		claimed bool
		err     error
	}, 2)
	for range 2 {
		go func() {
			<-start
			store := MemberStore{Path: path, Now: func() time.Time { return now }}
			_, claimed, err := store.ClaimAutoReleaseConvergenceStatusCheck(cycle, "", AutoReleaseStalledStatusInterval)
			results <- struct {
				claimed bool
				err     error
			}{claimed: claimed, err: err}
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners=%d, want 1", winners)
	}
}

func TestMySQLMarksAutoReleaseStalledNotificationAtomically(t *testing.T) {
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseAcceptedAt = now.Add(-24 * time.Hour).Format(time.RFC3339)
	want := reminder
	want.AutoReleaseStalledNotifiedAt = now.Format(time.RFC3339)
	want.UpdatedAt = now.Format(time.RFC3339)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	store := mysqlAutoReleaseTestStore(db, now)
	mock.ExpectBegin()
	expectMySQLAutoReleaseLockedReminder(mock, reminder)
	expectMySQLAutoReleaseReminderUpdate(mock, want)
	mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).WithArgs(mysqlStoreLockName).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, transitioned, err := store.MarkAutoReleaseStalledNotified(releaseReminderCycle(reminder), "")
	if err != nil || !transitioned || !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%+v transitioned=%t err=%v", got, transitioned, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLClaimsAutoReleaseConvergenceStatusCheckAtInterval(t *testing.T) {
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		lastAttempt string
		wantClaimed bool
	}{
		{name: "first claim", wantClaimed: true},
		{name: "repeat before interval", lastAttempt: now.Add(-AutoReleaseStalledStatusInterval + time.Second).Format(time.RFC3339)},
		{name: "exact interval boundary", lastAttempt: now.Add(-AutoReleaseStalledStatusInterval).Format(time.RFC3339), wantClaimed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reminder := runningAutoReleaseReminder("owner@example.com")
			reminder.AutoReleaseAcceptedAt = now.Add(-AutoReleaseConvergenceWindow).Format(time.RFC3339)
			reminder.AutoReleaseLastAttemptAt = test.lastAttempt
			want := reminder
			if test.wantClaimed {
				want.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
				want.UpdatedAt = now.Format(time.RFC3339)
			}

			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			store := mysqlAutoReleaseTestStore(db, now)
			mock.ExpectBegin()
			expectMySQLAutoReleaseLockedReminder(mock, reminder)
			if test.wantClaimed {
				expectMySQLAutoReleaseReminderUpdate(mock, want)
				mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).WithArgs(mysqlStoreLockName).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectCommit()

			got, claimed, err := store.ClaimAutoReleaseConvergenceStatusCheck(releaseReminderCycle(reminder), "", AutoReleaseStalledStatusInterval)
			if err != nil || claimed != test.wantClaimed || !reflect.DeepEqual(got, want) {
				t.Fatalf("got=%+v claimed=%t err=%v want=%+v", got, claimed, err, want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLAutoReleaseConvergenceStatusClaimRejectsInvalidCycleState(t *testing.T) {
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		mutate func(*ReleaseReminder, *ReleaseReminderCycle)
	}{
		{name: "stale cycle", mutate: func(_ *ReleaseReminder, cycle *ReleaseReminderCycle) { cycle.HostID = "h-stale" }},
		{name: "disabled", mutate: func(reminder *ReleaseReminder, _ *ReleaseReminderCycle) { reminder.AutoReleaseEnabled = false }},
		{name: "non-running", mutate: func(reminder *ReleaseReminder, _ *ReleaseReminderCycle) {
			reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
		}},
		{name: "accepted at mismatch", mutate: func(_ *ReleaseReminder, cycle *ReleaseReminderCycle) {
			cycle.AutoReleaseAcceptedAt = now.Add(-AutoReleaseConvergenceWindow - time.Second).Format(time.RFC3339)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reminder := runningAutoReleaseReminder("owner@example.com")
			reminder.AutoReleaseAcceptedAt = now.Add(-AutoReleaseConvergenceWindow).Format(time.RFC3339)
			cycle := releaseReminderCycle(reminder)
			test.mutate(&reminder, &cycle)

			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			store := mysqlAutoReleaseTestStore(db, now)
			mock.ExpectBegin()
			expectMySQLAutoReleaseLockedReminder(mock, reminder)
			mock.ExpectRollback()

			got, claimed, err := store.ClaimAutoReleaseConvergenceStatusCheck(cycle, "", AutoReleaseStalledStatusInterval)
			if !errors.Is(err, ErrReleaseReminderCycleChanged) || claimed || !reflect.DeepEqual(got, ReleaseReminder{}) {
				t.Fatalf("got=%+v claimed=%t err=%v", got, claimed, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLAutoReleaseConvergenceStatusClaimUpdateFailureRollsBack(t *testing.T) {
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseAcceptedAt = now.Add(-AutoReleaseConvergenceWindow).Format(time.RFC3339)
	want := reminder
	want.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
	want.UpdatedAt = now.Format(time.RFC3339)
	wantErr := errors.New("update failed")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	store := mysqlAutoReleaseTestStore(db, now)
	mock.ExpectBegin()
	expectMySQLAutoReleaseLockedReminder(mock, reminder)
	mock.ExpectExec(regexp.QuoteMeta(mysqlReleaseReminderUpdateQuery)).WithArgs(
		want.AppleEmail, want.HostID, want.HostArchitecture, want.HostCreatedAt, want.ReleaseDueAt,
		want.OwnerEmail, want.OwnerName, want.LastExtendedByEmail, want.LastExtendedByName,
		want.LastExtendedAt, want.LastNotifiedAt, want.ReleasedAt, want.Status,
		want.AutoReleaseEnabled, want.AutoReleaseAt, want.AutoReleaseStartedAt,
		want.AutoReleaseLastAttemptAt, want.AutoReleaseAcceptedAt, want.AutoReleaseStalledNotifiedAt,
		want.AutoReleaseNotifiedAt, want.AutoReleaseAttempts,
		want.AutoReleaseLastError, want.AutoReleaseState, want.UpdatedAt, want.ProfileName,
	).WillReturnError(wantErr)
	mock.ExpectRollback()
	got, claimed, err := store.ClaimAutoReleaseConvergenceStatusCheck(releaseReminderCycle(reminder), "", AutoReleaseStalledStatusInterval)
	if !errors.Is(err, wantErr) || claimed || !reflect.DeepEqual(got, ReleaseReminder{}) {
		t.Fatalf("got=%+v claimed=%t err=%v", got, claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLConvergenceAcceptanceIsIdempotentAndRejectsStaleCycle(t *testing.T) {
	now := time.Date(2026, 8, 20, 9, 5, 0, 0, time.UTC)
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseAcceptedAt = "2026-08-20T09:00:00Z"
	reminder.CreatedAt = "2026-08-20T08:00:00Z"
	reminder.UpdatedAt = "2026-08-20T09:00:00Z"
	for _, test := range []struct {
		name  string
		cycle ReleaseReminderCycle
		stale bool
	}{
		{name: "repeat", cycle: releaseReminderCycle(reminder)},
		{name: "stale", cycle: func() ReleaseReminderCycle {
			cycle := releaseReminderCycle(reminder)
			cycle.HostID = "h-stale"
			return cycle
		}(), stale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			store := mysqlAutoReleaseTestStore(db, now)
			mock.ExpectBegin()
			expectMySQLAutoReleaseLockedReminder(mock, reminder)
			if test.stale {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}
			got, transitioned, err := store.MarkAutoReleaseConvergenceAccepted(test.cycle, now.Format(time.RFC3339))
			if test.stale {
				if !errors.Is(err, ErrReleaseReminderCycleChanged) || transitioned {
					t.Fatalf("got=%+v transitioned=%t err=%v", got, transitioned, err)
				}
			} else if err != nil || transitioned || !reflect.DeepEqual(got, reminder) {
				t.Fatalf("got=%+v transitioned=%t err=%v", got, transitioned, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAutoReleaseNotificationMarkerPersistsThroughReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "members.json")
	store := NewMemberStore(path)
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}

	const notifiedAt = "2026-07-13T09:00:00Z"
	marked, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), notifiedAt)
	if err != nil {
		t.Fatalf("mark automatic release notified: %v", err)
	}
	if marked.AutoReleaseNotifiedAt != notifiedAt {
		t.Fatalf("marked reminder = %+v", marked)
	}
	reloaded, ok, err := NewMemberStore(path).ReleaseReminder(reminder.ProfileName)
	if err != nil || !ok || reloaded.AutoReleaseNotifiedAt != notifiedAt {
		t.Fatalf("reloaded reminder = %+v ok=%t err=%v", reloaded, ok, err)
	}
}

func TestAutoReleaseNotificationMarkerDefaultsEmptyTimestamp(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time { return now }
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}

	marked, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), "")
	if err != nil {
		t.Fatalf("mark with default timestamp: %v", err)
	}
	wantTimestamp := now.Format(time.RFC3339)
	if marked.AutoReleaseNotifiedAt != wantTimestamp || marked.UpdatedAt != wantTimestamp {
		t.Fatalf("defaulted marker = %+v, want timestamp %s", marked, wantTimestamp)
	}
	reloaded, ok, err := store.ReleaseReminder(reminder.ProfileName)
	if err != nil || !ok || !reflect.DeepEqual(reloaded, marked) {
		t.Fatalf("reloaded marker = %+v ok=%t err=%v", reloaded, ok, err)
	}
}

func TestAutoReleaseNotificationMarkerRejectsMalformedTimestamp(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	marked, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), "2026-07-13T09:00:00Z")
	if err != nil {
		t.Fatalf("seed valid marker: %v", err)
	}

	if _, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), "not-rfc3339"); err == nil {
		t.Fatal("malformed notification timestamp was accepted")
	} else {
		var parseErr *time.ParseError
		if !errors.As(err, &parseErr) {
			t.Fatalf("malformed timestamp error = %T %v, want *time.ParseError", err, err)
		}
	}
	persisted, ok, err := store.ReleaseReminder(reminder.ProfileName)
	if err != nil || !ok || !reflect.DeepEqual(persisted, marked) {
		t.Fatalf("marker changed after malformed timestamp: reminder=%+v ok=%t err=%v", persisted, ok, err)
	}
}

func TestAutoReleaseNotificationMarkerIsIdempotentForExactCycle(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}

	const firstNotifiedAt = "2026-07-13T09:00:00Z"
	first, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), firstNotifiedAt)
	if err != nil {
		t.Fatalf("first mark: %v", err)
	}
	store.Rename = func(string, string) error { return errors.New("idempotent mark unexpectedly saved") }
	second, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), "2026-07-13T09:05:00Z")
	if err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if second.AutoReleaseNotifiedAt != firstNotifiedAt || second.UpdatedAt != first.UpdatedAt {
		t.Fatalf("idempotent mark changed timestamps: first=%+v second=%+v", first, second)
	}
}

func TestAutoReleaseNotificationMarkerRejectsStaleCycle(t *testing.T) {
	mutations := map[string]func(*ReleaseReminderCycle){
		"auto release at":         func(cycle *ReleaseReminderCycle) { cycle.AutoReleaseAt = "2026-07-14T08:10:00Z" },
		"auto release started at": func(cycle *ReleaseReminderCycle) { cycle.AutoReleaseStartedAt = "2026-07-14T08:10:00Z" },
		"host":                    func(cycle *ReleaseReminderCycle) { cycle.HostID = "h-stale" },
		"apple email":             func(cycle *ReleaseReminderCycle) { cycle.AppleEmail = "stale-apple@example.com" },
		"owner email":             func(cycle *ReleaseReminderCycle) { cycle.OwnerEmail = "stale-owner@example.com" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
			reminder := notifyingAutoReleaseReminder("owner@example.com")
			if _, err := store.UpsertReleaseReminder(reminder); err != nil {
				t.Fatalf("upsert reminder: %v", err)
			}
			cycle := releaseReminderCycle(reminder)
			mutate(&cycle)
			if _, err := store.MarkAutoReleaseNotified(cycle, "2026-07-13T09:00:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("mark error = %v", err)
			}
		})
	}
}

func TestAutoReleaseNotificationMarkerCycleKeyChangeClearsAndRejectsStaleWork(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*ReleaseReminder)
		ownerEmail string
	}{
		{name: "auto release at", mutate: func(reminder *ReleaseReminder) { reminder.AutoReleaseAt = "2026-07-14T08:10:00Z" }},
		{name: "auto release started at", mutate: func(reminder *ReleaseReminder) { reminder.AutoReleaseStartedAt = "2026-07-14T08:10:00Z" }},
		{name: "host", mutate: func(reminder *ReleaseReminder) { reminder.HostID = "h-new" }},
		{name: "apple email", mutate: func(reminder *ReleaseReminder) { reminder.AppleEmail = "new-apple@example.com" }},
		{name: "owner email", mutate: func(reminder *ReleaseReminder) { reminder.OwnerEmail = "new-owner@example.com" }, ownerEmail: "new-owner@example.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
			for _, member := range []struct{ name, email string }{{"Old", "owner@example.com"}, {"New", "new-owner@example.com"}} {
				if _, err := store.AddMember(member.name, member.email, "operator"); err != nil {
					t.Fatalf("add member: %v", err)
				}
			}
			old := notifyingAutoReleaseReminder("owner@example.com")
			if _, err := store.SetProfileOwner(old.ProfileName, old.OwnerEmail); err != nil {
				t.Fatalf("set old owner: %v", err)
			}
			if _, err := store.UpsertReleaseReminder(old); err != nil {
				t.Fatalf("upsert old reminder: %v", err)
			}
			if _, err := store.MarkAutoReleaseNotified(releaseReminderCycle(old), "2026-07-13T09:00:00Z"); err != nil {
				t.Fatalf("mark old cycle: %v", err)
			}
			if test.ownerEmail != "" {
				if _, err := store.SetProfileOwner(old.ProfileName, test.ownerEmail); err != nil {
					t.Fatalf("set new owner: %v", err)
				}
			}

			updated, err := store.UpdateReleaseReminder(old.ProfileName, func(reminder ReleaseReminder) (ReleaseReminder, error) {
				test.mutate(&reminder)
				return reminder, nil
			})
			if err != nil {
				t.Fatalf("change cycle key: %v", err)
			}
			if updated.AutoReleaseNotifiedAt != "" {
				t.Fatalf("notification marker survived cycle change: %+v", updated)
			}

			oldCycle := releaseReminderCycle(old)
			if _, err := store.MarkAutoReleaseNotified(oldCycle, "2026-07-13T09:05:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("stale mark error = %v", err)
			}
			if _, err := store.CompleteAutoRelease(oldCycle, "2026-07-13T09:06:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("stale completion error = %v", err)
			}

			persisted, ok, err := store.ReleaseReminder(old.ProfileName)
			if err != nil || !ok || !releaseReminderMatchesCycle(persisted, releaseReminderCycle(updated)) || persisted.AutoReleaseNotifiedAt != "" || persisted.Status == ReleaseReminderStatusReleased {
				t.Fatalf("new cycle changed by stale work: reminder=%+v ok=%t err=%v", persisted, ok, err)
			}
			owner, ok, err := store.ProfileOwner(old.ProfileName)
			wantOwner := old.OwnerEmail
			if test.ownerEmail != "" {
				wantOwner = test.ownerEmail
			}
			if err != nil || !ok || owner.Owner.Email != wantOwner {
				t.Fatalf("owner changed by stale work: owner=%+v ok=%t err=%v", owner, ok, err)
			}
		})
	}
}

func TestAutoReleaseNotificationMarkerRequiresNotifyingReminder(t *testing.T) {
	tests := map[string]func(*ReleaseReminder){
		"status":    func(reminder *ReleaseReminder) { reminder.Status = ReleaseReminderStatusActive },
		"enabled":   func(reminder *ReleaseReminder) { reminder.AutoReleaseEnabled = false },
		"notifying": func(reminder *ReleaseReminder) { reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
			reminder := notifyingAutoReleaseReminder("owner@example.com")
			mutate(&reminder)
			if _, err := store.UpsertReleaseReminder(reminder); err != nil {
				t.Fatalf("upsert reminder: %v", err)
			}
			if _, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), "2026-07-13T09:00:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("mark error = %v", err)
			}
		})
	}
}

func TestAutoReleaseNotificationMarkerRejectsMarkedInvalidState(t *testing.T) {
	tests := map[string]func(*ReleaseReminder){
		"released":    func(reminder *ReleaseReminder) { reminder.Status = ReleaseReminderStatusReleased },
		"disabled":    func(reminder *ReleaseReminder) { reminder.AutoReleaseEnabled = false },
		"wrong state": func(reminder *ReleaseReminder) { reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
			reminder := notifyingAutoReleaseReminder("owner@example.com")
			reminder.AutoReleaseNotifiedAt = "2026-07-13T09:00:00Z"
			mutate(&reminder)
			stored, err := store.UpsertReleaseReminder(reminder)
			if err != nil {
				t.Fatalf("upsert marked reminder: %v", err)
			}
			if _, err := store.MarkAutoReleaseNotified(releaseReminderCycle(stored), "2026-07-13T09:05:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("mark error = %v", err)
			}
			persisted, ok, err := store.ReleaseReminder(reminder.ProfileName)
			if err != nil || !ok || !reflect.DeepEqual(persisted, stored) {
				t.Fatalf("marked reminder changed: reminder=%+v ok=%t err=%v", persisted, ok, err)
			}
		})
	}
}

func TestMemberStoreCompleteAutoReleasePreservesNotificationMarker(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	if _, err := store.AddMember("Owner", "owner@example.com", "operator"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if _, err := store.SetProfileOwner("mac", "owner@example.com"); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	const notifiedAt = "2026-07-13T09:00:00Z"
	if _, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), notifiedAt); err != nil {
		t.Fatalf("mark notified: %v", err)
	}

	completed, err := store.CompleteAutoRelease(releaseReminderCycle(reminder), "2026-07-13T09:01:00Z")
	if err != nil {
		t.Fatalf("complete automatic release: %v", err)
	}
	if completed.Status != ReleaseReminderStatusReleased || completed.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || completed.AutoReleaseNotifiedAt != notifiedAt {
		t.Fatalf("completed reminder = %+v", completed)
	}
	if _, ok, err := store.ProfileOwner(reminder.ProfileName); err != nil || ok {
		t.Fatalf("owner after completion ok=%t err=%v", ok, err)
	}
}

func TestMemberStoreAutomaticCleanupProtectsCoordinatorOwnedRecords(t *testing.T) {
	for _, state := range []string{
		ReleaseReminderAutoReleaseStateScheduled,
		ReleaseReminderAutoReleaseStateRunning,
		ReleaseReminderAutoReleaseStateRetrying,
		ReleaseReminderAutoReleaseStateNotifying,
		ReleaseReminderAutoReleaseStateFailed,
	} {
		t.Run(state, func(t *testing.T) {
			store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
			if _, err := store.AddMember("Owner", "owner@example.com", "operator"); err != nil {
				t.Fatalf("add owner: %v", err)
			}
			if _, err := store.SetProfileOwner("mac", "owner@example.com"); err != nil {
				t.Fatalf("set owner: %v", err)
			}
			reminder := notifyingAutoReleaseReminder("owner@example.com")
			reminder.AutoReleaseState = state
			if state == ReleaseReminderAutoReleaseStateNotifying {
				reminder.AutoReleaseNotifiedAt = "2026-07-13T08:59:00Z"
			}
			before, err := store.UpsertReleaseReminder(reminder)
			if err != nil {
				t.Fatalf("upsert reminder: %v", err)
			}

			got, changed, err := store.CleanupProfileRecords("mac", "2026-07-13T09:00:00Z", "auto-status")
			if err != nil {
				t.Fatalf("automatic cleanup: %v", err)
			}
			if changed || !reflect.DeepEqual(got, before) {
				t.Fatalf("automatic cleanup changed protected reminder: changed=%t\n got: %+v\nwant: %+v", changed, got, before)
			}
			if owner, ok, err := store.ProfileOwner("mac"); err != nil || !ok || owner.Owner.Email != "owner@example.com" {
				t.Fatalf("automatic cleanup changed owner: owner=%+v ok=%t err=%v", owner, ok, err)
			}
		})
	}
}

func TestMemberStoreManualCleanupCanOverrideCoordinatorOwnedRecords(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	if _, err := store.AddMember("Owner", "owner@example.com", "operator"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if _, err := store.SetProfileOwner("mac", "owner@example.com"); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseNotifiedAt = "2026-07-13T08:59:00Z"
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}

	got, changed, err := store.CleanupProfileRecords("mac", "2026-07-13T09:00:00Z", "manual")
	if err != nil {
		t.Fatalf("manual cleanup: %v", err)
	}
	if !changed || got.Status != ReleaseReminderStatusReleased || got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased || got.AutoReleaseNotifiedAt != reminder.AutoReleaseNotifiedAt {
		t.Fatalf("manual cleanup reminder: changed=%t reminder=%+v", changed, got)
	}
	if owner, ok, err := store.ProfileOwner("mac"); err != nil || ok {
		t.Fatalf("owner after manual cleanup: owner=%+v ok=%t err=%v", owner, ok, err)
	}
}

func TestMemberStoreCompleteAutoReleaseSaveFailurePreservesDurableNotificationMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "members.json")
	store := NewMemberStore(path)
	if _, err := store.AddMember("Owner", "owner@example.com", "operator"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if _, err := store.SetProfileOwner("mac", "owner@example.com"); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	const notifiedAt = "2026-07-13T09:00:00Z"
	if _, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), notifiedAt); err != nil {
		t.Fatalf("mark notified: %v", err)
	}

	wantErr := errors.New("replace failed")
	failingStore := NewMemberStore(path)
	failingStore.Rename = func(string, string) error { return wantErr }
	if _, err := failingStore.CompleteAutoRelease(releaseReminderCycle(reminder), "2026-07-13T09:01:00Z"); !errors.Is(err, wantErr) {
		t.Fatalf("completion error = %v, want %v", err, wantErr)
	}
	reloaded, ok, err := NewMemberStore(path).ReleaseReminder(reminder.ProfileName)
	if err != nil || !ok {
		t.Fatalf("reload reminder: reminder=%+v ok=%t err=%v", reloaded, ok, err)
	}
	if reloaded.AutoReleaseNotifiedAt != notifiedAt || reloaded.Status != ReleaseReminderStatusDueNotified || reloaded.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying || reloaded.ReleasedAt != "" {
		t.Fatalf("durable reminder changed after failed completion: %+v", reloaded)
	}
	owner, ok, err := NewMemberStore(path).ProfileOwner(reminder.ProfileName)
	if err != nil || !ok || owner.Owner.Email != reminder.OwnerEmail {
		t.Fatalf("durable owner changed after failed completion: owner=%+v ok=%t err=%v", owner, ok, err)
	}
}

func TestMemberStoreCompleteAutoReleaseAtomicallyClearsMatchingOwner(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	if _, err := store.AddMember("Owner", "owner@example.com", "operator"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if _, err := store.SetProfileOwner("mac", "owner@example.com"); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateNotifying
	reminder.AutoReleaseNotifiedAt = "2026-07-13T08:59:00Z"
	if _, err := store.UpsertReleaseReminder(reminder); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}

	completed, err := store.CompleteAutoRelease(releaseReminderCycle(reminder), "2026-07-13T09:00:00Z")
	if err != nil {
		t.Fatalf("complete automatic release: %v", err)
	}
	if completed.Status != ReleaseReminderStatusReleased || completed.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased {
		t.Fatalf("completed reminder = %+v", completed)
	}
	if _, ok, err := store.ProfileOwner("mac"); err != nil || ok {
		t.Fatalf("owner after completion ok=%t err=%v", ok, err)
	}
}

func TestMemberStoreCompleteAutoReleaseRequiresPersistedNotification(t *testing.T) {
	tests := map[string]func(*ReleaseReminder){
		"running with marker": func(reminder *ReleaseReminder) {
			reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
		},
		"notifying without marker": func(reminder *ReleaseReminder) {
			reminder.AutoReleaseNotifiedAt = ""
		},
		"wrong status": func(reminder *ReleaseReminder) {
			reminder.Status = ReleaseReminderStatusActive
		},
		"disabled": func(reminder *ReleaseReminder) {
			reminder.AutoReleaseEnabled = false
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
			if _, err := store.AddMember("Owner", "owner@example.com", "operator"); err != nil {
				t.Fatalf("add owner: %v", err)
			}
			if _, err := store.SetProfileOwner("mac", "owner@example.com"); err != nil {
				t.Fatalf("set owner: %v", err)
			}
			reminder := notifyingAutoReleaseReminder("owner@example.com")
			reminder.AutoReleaseNotifiedAt = "2026-07-13T08:59:00Z"
			mutate(&reminder)
			stored, err := store.UpsertReleaseReminder(reminder)
			if err != nil {
				t.Fatalf("upsert reminder: %v", err)
			}

			if _, err := store.CompleteAutoRelease(releaseReminderCycle(stored), "2026-07-13T09:00:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("completion error = %v", err)
			}
			persisted, ok, err := store.ReleaseReminder(reminder.ProfileName)
			if err != nil || !ok || !reflect.DeepEqual(persisted, stored) {
				t.Fatalf("rejected reminder changed: reminder=%+v ok=%t err=%v", persisted, ok, err)
			}
			owner, ok, err := store.ProfileOwner(reminder.ProfileName)
			if err != nil || !ok || owner.Owner.Email != reminder.OwnerEmail {
				t.Fatalf("owner changed after rejection: owner=%+v ok=%t err=%v", owner, ok, err)
			}
		})
	}
}

func TestMemberStoreCompleteAutoReleaseNeverClearsNewCycleOwner(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	for _, member := range []struct{ name, email string }{{"Old", "old@example.com"}, {"New", "new@example.com"}} {
		if _, err := store.AddMember(member.name, member.email, "operator"); err != nil {
			t.Fatalf("add member: %v", err)
		}
	}
	old := runningAutoReleaseReminder("old@example.com")
	if _, err := store.SetProfileOwner("mac", old.OwnerEmail); err != nil {
		t.Fatalf("set old owner: %v", err)
	}
	if _, err := store.UpsertReleaseReminder(old); err != nil {
		t.Fatalf("upsert old reminder: %v", err)
	}
	newCycle := old
	newCycle.HostID = "h-new"
	newCycle.AppleEmail = "new-apple@example.com"
	newCycle.OwnerEmail = "new@example.com"
	newCycle.AutoReleaseAt = "2026-07-14T08:10:00Z"
	newCycle.AutoReleaseStartedAt = "2026-07-14T08:10:00Z"
	newCycle.Status = ReleaseReminderStatusActive
	newCycle.AutoReleaseState = ""
	if _, err := store.SetProfileOwner("mac", newCycle.OwnerEmail); err != nil {
		t.Fatalf("set new owner: %v", err)
	}
	if _, err := store.UpsertReleaseReminder(newCycle); err != nil {
		t.Fatalf("upsert new reminder: %v", err)
	}

	if _, err := store.CompleteAutoRelease(releaseReminderCycle(old), "2026-07-13T09:00:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
		t.Fatalf("completion error = %v", err)
	}
	owner, ok, err := store.ProfileOwner("mac")
	if err != nil || !ok || owner.Owner.Email != newCycle.OwnerEmail {
		t.Fatalf("new owner was changed: owner=%+v ok=%t err=%v", owner, ok, err)
	}
	got, _, err := store.ReleaseReminder("mac")
	if err != nil || got.HostID != newCycle.HostID || got.Status == ReleaseReminderStatusReleased {
		t.Fatalf("new reminder was changed: reminder=%+v err=%v", got, err)
	}
}

func TestMemberStoreCompleteAutoReleaseRaceWithNewOpenIsSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "members.json")
	seed := NewMemberStore(path)
	for _, member := range []struct{ name, email string }{{"Old", "old@example.com"}, {"New", "new@example.com"}} {
		if _, err := seed.AddMember(member.name, member.email, "operator"); err != nil {
			t.Fatalf("add member: %v", err)
		}
	}
	old := runningAutoReleaseReminder("old@example.com")
	if _, err := seed.SetProfileOwner("mac", old.OwnerEmail); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	if _, err := seed.UpsertReleaseReminder(old); err != nil {
		t.Fatalf("upsert old: %v", err)
	}
	newCycle := old
	newCycle.HostID = "h-new"
	newCycle.OwnerEmail = "new@example.com"
	newCycle.AutoReleaseAt = "2026-07-14T08:10:00Z"
	newCycle.AutoReleaseStartedAt = "2026-07-14T08:10:00Z"
	newCycle.Status = ReleaseReminderStatusActive
	newCycle.AutoReleaseState = ""

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		store := NewMemberStore(path)
		_, _ = store.CompleteAutoRelease(releaseReminderCycle(old), "2026-07-13T09:00:00Z")
	}()
	go func() {
		defer wg.Done()
		<-start
		store := NewMemberStore(path)
		_, _ = store.SetProfileOwner("mac", newCycle.OwnerEmail)
		_, _ = store.UpsertReleaseReminder(newCycle)
	}()
	close(start)
	wg.Wait()

	owner, ok, err := seed.ProfileOwner("mac")
	if err != nil || !ok || owner.Owner.Email != newCycle.OwnerEmail {
		t.Fatalf("new owner missing after race: owner=%+v ok=%t err=%v", owner, ok, err)
	}
	reminder, _, err := seed.ReleaseReminder("mac")
	if err != nil || reminder.HostID != newCycle.HostID || reminder.Status == ReleaseReminderStatusReleased {
		t.Fatalf("new cycle lost after race: reminder=%+v err=%v", reminder, err)
	}
}

func TestMemberStoreOwnerAndCycleKeyChangesResetAutoReleaseForNewCycle(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       string
		changeHost  bool
		changeApple bool
		changeOwner bool
	}{
		{name: "enabled-new-host", state: "", changeHost: true},
		{name: "running-new-host", state: ReleaseReminderAutoReleaseStateRunning, changeHost: true},
		{name: "retrying-new-host", state: ReleaseReminderAutoReleaseStateRetrying, changeHost: true},
		{name: "enabled-new-apple", state: "", changeApple: true},
		{name: "running-new-apple", state: ReleaseReminderAutoReleaseStateRunning, changeApple: true},
		{name: "retrying-new-apple", state: ReleaseReminderAutoReleaseStateRetrying, changeApple: true},
		{name: "notifying-new-owner", state: ReleaseReminderAutoReleaseStateNotifying, changeOwner: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
			old := runningAutoReleaseReminder("owner@example.com")
			old.AutoReleaseAcceptedAt = "2026-07-13T08:30:00Z"
			old.AutoReleaseStalledNotifiedAt = "2026-07-13T08:45:00Z"
			old.AutoReleaseNotifiedAt = "2026-07-13T09:00:00Z"
			old.AutoReleaseState = test.state
			if test.state == "" {
				old.AutoReleaseAt = "2026-07-13T08:10:00Z"
				old.AutoReleaseStartedAt = ""
				old.AutoReleaseLastAttemptAt = ""
				old.AutoReleaseAttempts = 0
			}
			if _, err := store.UpsertReleaseReminder(old); err != nil {
				t.Fatalf("upsert old reminder: %v", err)
			}

			updated := old
			if test.changeHost {
				updated.HostID = "h-new"
			}
			if test.changeApple {
				updated.AppleEmail = "new-apple@example.com"
			}
			if test.changeOwner {
				updated.OwnerEmail = "new-owner@example.com"
			}
			updated.Status = ReleaseReminderStatusActive
			updated.AutoReleaseAt = "2026-07-14T08:10:00Z"
			updated.AutoReleaseStartedAt = "stale-start"
			updated.AutoReleaseLastAttemptAt = "stale-attempt"
			updated.AutoReleaseAttempts = 99
			updated.AutoReleaseLastError = "stale error"
			updated.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
			got, err := store.UpsertReleaseReminder(updated)
			if err != nil {
				t.Fatalf("upsert new reminder: %v", err)
			}
			if got.HostID != updated.HostID || got.AppleEmail != updated.AppleEmail || got.OwnerEmail != updated.OwnerEmail || got.Status != ReleaseReminderStatusActive {
				t.Fatalf("new cycle fields not retained: %+v", got)
			}
			if got.AutoReleaseEnabled || got.AutoReleaseAt != "" || got.AutoReleaseStartedAt != "" || got.AutoReleaseLastAttemptAt != "" || got.AutoReleaseAcceptedAt != "" || got.AutoReleaseStalledNotifiedAt != "" || got.AutoReleaseNotifiedAt != "" || got.AutoReleaseAttempts != 0 || got.AutoReleaseLastError != "" || got.AutoReleaseState != "" {
				t.Fatalf("auto-release state leaked into new cycle: %+v", got)
			}
		})
	}
}

func TestAutoReleaseNotificationMarkerPreservedOnUnrelatedUpdates(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	old := runningAutoReleaseReminder("owner@example.com")
	old.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
	old.AutoReleaseLastError = "host is pending"
	old.AutoReleaseAcceptedAt = "2026-07-13T08:30:00Z"
	old.AutoReleaseStalledNotifiedAt = "2026-07-13T08:45:00Z"
	old.AutoReleaseNotifiedAt = "2026-07-13T09:00:00Z"
	if _, err := store.UpsertReleaseReminder(old); err != nil {
		t.Fatalf("upsert old reminder: %v", err)
	}

	updated := old
	updated.HostCreatedAt = "2026-07-13T08:00:00Z"
	updated.OwnerName = "Updated Owner"
	updated.AutoReleaseAcceptedAt = ""
	updated.AutoReleaseStalledNotifiedAt = ""
	updated.AutoReleaseNotifiedAt = ""
	got, err := store.UpsertReleaseReminder(updated)
	if err != nil {
		t.Fatalf("upsert same-cycle reminder: %v", err)
	}
	if got.HostID != old.HostID || got.AppleEmail != old.AppleEmail || got.OwnerEmail != old.OwnerEmail || got.OwnerName != updated.OwnerName || got.Status != old.Status {
		t.Fatalf("same-cycle fields not updated: %+v", got)
	}
	if !got.AutoReleaseEnabled || got.AutoReleaseAt != old.AutoReleaseAt || got.AutoReleaseStartedAt != old.AutoReleaseStartedAt || got.AutoReleaseLastAttemptAt != old.AutoReleaseLastAttemptAt || got.AutoReleaseAcceptedAt != old.AutoReleaseAcceptedAt || got.AutoReleaseStalledNotifiedAt != old.AutoReleaseStalledNotifiedAt || got.AutoReleaseNotifiedAt != old.AutoReleaseNotifiedAt || got.AutoReleaseAttempts != old.AutoReleaseAttempts || got.AutoReleaseLastError != old.AutoReleaseLastError || got.AutoReleaseState != old.AutoReleaseState {
		t.Fatalf("same-cycle auto-release state was not preserved: %+v", got)
	}

	got, err = store.UpdateReleaseReminder(old.ProfileName, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.ReleaseDueAt = "2026-07-15T08:00:00Z"
		return reminder, nil
	})
	if err != nil {
		t.Fatalf("update unrelated reminder field: %v", err)
	}
	if got.ReleaseDueAt != "2026-07-15T08:00:00Z" || got.AutoReleaseAcceptedAt != old.AutoReleaseAcceptedAt || got.AutoReleaseStalledNotifiedAt != old.AutoReleaseStalledNotifiedAt || got.AutoReleaseNotifiedAt != old.AutoReleaseNotifiedAt {
		t.Fatalf("unrelated callback update changed marker: %+v", got)
	}
}

func TestAutoReleaseNotificationMarkerCannotBeErasedOrForgedByCallback(t *testing.T) {
	tests := map[string]string{
		"reconstructed reminder": "",
		"forged marker":          "2026-07-13T09:30:00Z",
	}
	for name, attemptedMarker := range tests {
		t.Run(name, func(t *testing.T) {
			store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
			current := notifyingAutoReleaseReminder("owner@example.com")
			current.AutoReleaseNotifiedAt = "2026-07-13T09:00:00Z"
			stored, err := store.UpsertReleaseReminder(current)
			if err != nil {
				t.Fatalf("upsert current reminder: %v", err)
			}

			updated, err := store.UpdateReleaseReminder(current.ProfileName, func(ReleaseReminder) (ReleaseReminder, error) {
				return ReleaseReminder{
					ProfileName:              stored.ProfileName,
					AppleEmail:               stored.AppleEmail,
					HostID:                   stored.HostID,
					OwnerEmail:               stored.OwnerEmail,
					Status:                   stored.Status,
					AutoReleaseEnabled:       stored.AutoReleaseEnabled,
					AutoReleaseAt:            stored.AutoReleaseAt,
					AutoReleaseStartedAt:     stored.AutoReleaseStartedAt,
					AutoReleaseLastAttemptAt: stored.AutoReleaseLastAttemptAt,
					AutoReleaseNotifiedAt:    attemptedMarker,
					AutoReleaseState:         stored.AutoReleaseState,
				}, nil
			})
			if err != nil {
				t.Fatalf("update reconstructed reminder: %v", err)
			}
			if updated.AutoReleaseNotifiedAt != stored.AutoReleaseNotifiedAt {
				t.Fatalf("callback changed marker: got %q want %q", updated.AutoReleaseNotifiedAt, stored.AutoReleaseNotifiedAt)
			}
		})
	}
}

func TestMemberStoreUpsertReleaseReminderReactivatesReleasedSameHost(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	old := runningAutoReleaseReminder("old-owner@example.com")
	old.Status = ReleaseReminderStatusReleased
	old.ReleasedAt = "2026-07-14T09:00:00Z"
	old.AutoReleaseState = ReleaseReminderAutoReleaseStateReleased
	if _, err := store.UpsertReleaseReminder(old); err != nil {
		t.Fatalf("upsert released reminder: %v", err)
	}

	updated := ReleaseReminder{
		ProfileName:   old.ProfileName,
		AppleEmail:    old.AppleEmail,
		HostID:        old.HostID,
		HostCreatedAt: "2026-07-13T08:00:00Z",
		ReleaseDueAt:  "2026-07-17T08:00:00Z",
		OwnerEmail:    "new-owner@example.com",
		OwnerName:     "New Owner",
		Status:        ReleaseReminderStatusActive,
	}
	got, err := store.UpsertReleaseReminder(updated)
	if err != nil {
		t.Fatalf("reactivate released reminder: %v", err)
	}
	if got.Status != ReleaseReminderStatusActive || got.ReleasedAt != "" || got.OwnerEmail != updated.OwnerEmail || got.ReleaseDueAt != updated.ReleaseDueAt {
		t.Fatalf("released reminder was not reactivated: %+v", got)
	}
	if got.AutoReleaseEnabled || got.AutoReleaseAt != "" || got.AutoReleaseStartedAt != "" || got.AutoReleaseLastAttemptAt != "" || got.AutoReleaseAcceptedAt != "" || got.AutoReleaseStalledNotifiedAt != "" || got.AutoReleaseNotifiedAt != "" || got.AutoReleaseAttempts != 0 || got.AutoReleaseLastError != "" || got.AutoReleaseState != "" {
		t.Fatalf("released auto-release state leaked into active cycle: %+v", got)
	}
}

func notifyingAutoReleaseReminder(ownerEmail string) ReleaseReminder {
	reminder := runningAutoReleaseReminder(ownerEmail)
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateNotifying
	return reminder
}

func runningAutoReleaseReminder(ownerEmail string) ReleaseReminder {
	return ReleaseReminder{
		ProfileName:              "mac",
		AppleEmail:               "apple@example.com",
		HostID:                   "h-old",
		OwnerEmail:               ownerEmail,
		ReleaseDueAt:             "2026-07-13T08:00:00Z",
		Status:                   ReleaseReminderStatusDueNotified,
		AutoReleaseEnabled:       true,
		AutoReleaseAt:            "2026-07-13T08:10:00Z",
		AutoReleaseStartedAt:     "2026-07-13T08:10:00Z",
		AutoReleaseLastAttemptAt: "2026-07-13T08:10:00Z",
		AutoReleaseAttempts:      1,
		AutoReleaseState:         ReleaseReminderAutoReleaseStateRunning,
	}
}

func releaseReminderCycle(reminder ReleaseReminder) ReleaseReminderCycle {
	return ReleaseReminderCycle{
		ProfileName:           reminder.ProfileName,
		AutoReleaseAt:         reminder.AutoReleaseAt,
		AutoReleaseStartedAt:  reminder.AutoReleaseStartedAt,
		AutoReleaseAcceptedAt: reminder.AutoReleaseAcceptedAt,
		HostID:                reminder.HostID,
		AppleEmail:            reminder.AppleEmail,
		OwnerEmail:            reminder.OwnerEmail,
	}
}

func TestMySQLAutoReleaseNotificationMarkerSchemaAndLegacyEmptyValue(t *testing.T) {
	for _, wantMigration := range []string{
		`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_accepted_at VARCHAR(64) NULL`,
		`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_stalled_notified_at VARCHAR(64) NULL`,
		`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_notified_at VARCHAR(64) NULL`,
	} {
		if !containsString(mysqlReleaseReminderMigrationStatements, wantMigration) {
			t.Fatalf("release reminder migrations do not contain %q", wantMigration)
		}
	}
	joinedSchema := strings.Join(mysqlSchemaStatements(), "\n")
	wantSchemaOrder := "auto_release_last_attempt_at VARCHAR(64) NULL,\n\t\t\tauto_release_accepted_at VARCHAR(64) NULL,\n\t\t\tauto_release_stalled_notified_at VARCHAR(64) NULL,\n\t\t\tauto_release_notified_at VARCHAR(64) NULL,\n\t\t\tauto_release_attempts INT NOT NULL DEFAULT 0"
	if !strings.Contains(joinedSchema, wantSchemaOrder) {
		t.Fatalf("release reminder schema marker ordering missing from:\n%s", joinedSchema)
	}

	wantColumns := `profile_name, COALESCE(apple_email, ''), COALESCE(host_id, ''), COALESCE(host_architecture, ''), COALESCE(host_created_at, ''), COALESCE(release_due_at, ''), COALESCE(owner_email, ''), COALESCE(owner_name, ''), COALESCE(last_extended_by_email, ''), COALESCE(last_extended_by_name, ''), COALESCE(last_extended_at, ''), COALESCE(last_notified_at, ''), COALESCE(released_at, ''), status, auto_release_enabled, COALESCE(auto_release_at, ''), COALESCE(auto_release_started_at, ''), COALESCE(auto_release_last_attempt_at, ''), COALESCE(auto_release_accepted_at, ''), COALESCE(auto_release_stalled_notified_at, ''), COALESCE(auto_release_notified_at, ''), auto_release_attempts, COALESCE(auto_release_last_error, ''), COALESCE(auto_release_state, ''), created_at, updated_at`
	if mysqlReleaseReminderSelectColumns != wantColumns {
		t.Fatalf("release reminder SELECT columns = %q, want %q", mysqlReleaseReminderSelectColumns, wantColumns)
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	legacy := notifyingAutoReleaseReminder("owner@example.com")
	legacy.CreatedAt = "2026-07-13T08:00:00Z"
	legacy.UpdatedAt = "2026-07-13T08:10:00Z"
	mock.ExpectQuery("SELECT legacy_release_reminder").
		WillReturnRows(mysqlAutoReleaseReminderRows(legacy))
	rows, err := db.Query("SELECT legacy_release_reminder")
	if err != nil {
		t.Fatalf("query legacy reminder: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("legacy reminder row missing")
	}
	var got ReleaseReminder
	if err := scanMySQLReleaseReminder(rows, &got); err != nil {
		t.Fatalf("scan legacy reminder: %v", err)
	}
	if got.AutoReleaseAcceptedAt != "" || got.AutoReleaseStalledNotifiedAt != "" || got.AutoReleaseNotifiedAt != "" || !reflect.DeepEqual(got, legacy) {
		t.Fatalf("legacy reminder = %+v, want %+v", got, legacy)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLAutoReleaseNotificationMarkerSQLColumnAndArgumentOrdering(t *testing.T) {
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseAcceptedAt = "2026-07-13T08:30:00Z"
	reminder.AutoReleaseStalledNotifiedAt = "2026-07-13T08:45:00Z"
	reminder.AutoReleaseNotifiedAt = "2026-07-13T09:00:00Z"
	reminder.CreatedAt = "2026-07-13T08:00:00Z"
	reminder.UpdatedAt = "2026-07-13T09:01:00Z"

	wantInsertQuery := `INSERT INTO cm_release_reminders (profile_name, apple_email, host_id, host_architecture, host_created_at, release_due_at, owner_email, owner_name, last_extended_by_email, last_extended_by_name, last_extended_at, last_notified_at, released_at, status, auto_release_enabled, auto_release_at, auto_release_started_at, auto_release_last_attempt_at, auto_release_accepted_at, auto_release_stalled_notified_at, auto_release_notified_at, auto_release_attempts, auto_release_last_error, auto_release_state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	wantInsertArgs := []any{
		reminder.ProfileName, reminder.AppleEmail, reminder.HostID, reminder.HostArchitecture, reminder.HostCreatedAt, reminder.ReleaseDueAt,
		reminder.OwnerEmail, reminder.OwnerName, reminder.LastExtendedByEmail, reminder.LastExtendedByName,
		reminder.LastExtendedAt, reminder.LastNotifiedAt, reminder.ReleasedAt, reminder.Status,
		reminder.AutoReleaseEnabled, reminder.AutoReleaseAt, reminder.AutoReleaseStartedAt,
		reminder.AutoReleaseLastAttemptAt, reminder.AutoReleaseAcceptedAt, reminder.AutoReleaseStalledNotifiedAt,
		reminder.AutoReleaseNotifiedAt, reminder.AutoReleaseAttempts,
		reminder.AutoReleaseLastError, reminder.AutoReleaseState, reminder.CreatedAt, reminder.UpdatedAt,
	}
	insertRecorder := &mysqlAutoReleaseRecordingExecer{}
	if err := insertMySQLReleaseReminder(insertRecorder, reminder); err != nil {
		t.Fatalf("insert reminder: %v", err)
	}
	if insertRecorder.query != wantInsertQuery || !reflect.DeepEqual(insertRecorder.args, wantInsertArgs) {
		t.Fatalf("insert query/args = %q %#v, want %q %#v", insertRecorder.query, insertRecorder.args, wantInsertQuery, wantInsertArgs)
	}

	wantUpdateQuery := `UPDATE cm_release_reminders SET apple_email = ?, host_id = ?, host_architecture = ?, host_created_at = ?, release_due_at = ?, owner_email = ?, owner_name = ?, last_extended_by_email = ?, last_extended_by_name = ?, last_extended_at = ?, last_notified_at = ?, released_at = ?, status = ?, auto_release_enabled = ?, auto_release_at = ?, auto_release_started_at = ?, auto_release_last_attempt_at = ?, auto_release_accepted_at = ?, auto_release_stalled_notified_at = ?, auto_release_notified_at = ?, auto_release_attempts = ?, auto_release_last_error = ?, auto_release_state = ?, updated_at = ? WHERE profile_name = ?`
	wantUpdateArgs := []any{
		reminder.AppleEmail, reminder.HostID, reminder.HostArchitecture, reminder.HostCreatedAt, reminder.ReleaseDueAt, reminder.OwnerEmail,
		reminder.OwnerName, reminder.LastExtendedByEmail, reminder.LastExtendedByName, reminder.LastExtendedAt,
		reminder.LastNotifiedAt, reminder.ReleasedAt, reminder.Status, reminder.AutoReleaseEnabled,
		reminder.AutoReleaseAt, reminder.AutoReleaseStartedAt, reminder.AutoReleaseLastAttemptAt,
		reminder.AutoReleaseAcceptedAt, reminder.AutoReleaseStalledNotifiedAt,
		reminder.AutoReleaseNotifiedAt, reminder.AutoReleaseAttempts, reminder.AutoReleaseLastError,
		reminder.AutoReleaseState, reminder.UpdatedAt, reminder.ProfileName,
	}
	updateRecorder := &mysqlAutoReleaseRecordingExecer{}
	if err := updateMySQLReleaseReminder(updateRecorder, reminder.ProfileName, reminder); err != nil {
		t.Fatalf("update reminder: %v", err)
	}
	if updateRecorder.query != wantUpdateQuery || !reflect.DeepEqual(updateRecorder.args, wantUpdateArgs) {
		t.Fatalf("update query/args = %q %#v, want %q %#v", updateRecorder.query, updateRecorder.args, wantUpdateQuery, wantUpdateArgs)
	}
}

func TestMySQLAutoReleaseNotificationMarkerPersistsAndReloads(t *testing.T) {
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	reminder.CreatedAt = "2026-07-13T08:00:00Z"
	reminder.UpdatedAt = "2026-07-13T08:10:00Z"
	const notifiedAt = "2026-07-13T09:00:00Z"
	now := time.Date(2026, 7, 13, 9, 1, 0, 0, time.UTC)
	want := reminder
	want.AutoReleaseNotifiedAt = notifiedAt
	want.UpdatedAt = now.Format(time.RFC3339)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	store := mysqlAutoReleaseTestStore(db, now)
	mock.ExpectBegin()
	expectMySQLAutoReleaseLockedReminder(mock, reminder)
	expectMySQLAutoReleaseReminderUpdate(mock, want)
	mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), notifiedAt)
	if err != nil {
		t.Fatalf("mark automatic release notified: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("marked reminder = %+v, want %+v", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	reloadDB, reloadMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("reload sqlmock: %v", err)
	}
	reloadStore := mysqlAutoReleaseTestStore(reloadDB, now)
	expectMySQLAutoReleaseReminderLoad(reloadMock, want)
	data, err := reloadStore.Load()
	if err != nil {
		t.Fatalf("reload MySQL member data: %v", err)
	}
	if len(data.Reminders) != 1 || !reflect.DeepEqual(data.Reminders[0], want) {
		t.Fatalf("reloaded reminders = %+v, want %+v", data.Reminders, want)
	}
	if err := reloadMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLAutoReleaseNotificationMarkerDefaultsEmptyTimestamp(t *testing.T) {
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	reminder.CreatedAt = "2026-07-13T08:00:00Z"
	reminder.UpdatedAt = "2026-07-13T08:10:00Z"
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	want := reminder
	want.AutoReleaseNotifiedAt = now.Format(time.RFC3339)
	want.UpdatedAt = now.Format(time.RFC3339)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	store := mysqlAutoReleaseTestStore(db, now)
	mock.ExpectBegin()
	expectMySQLAutoReleaseLockedReminder(mock, reminder)
	expectMySQLAutoReleaseReminderUpdate(mock, want)
	mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), "")
	if err != nil {
		t.Fatalf("mark with default timestamp: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaulted marker = %+v, want %+v", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLAutoReleaseNotificationMarkerRejectsMalformedTimestamp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := mysqlAutoReleaseTestStore(db, time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC))
	if _, err := store.MarkAutoReleaseNotified(releaseReminderCycle(notifyingAutoReleaseReminder("owner@example.com")), "not-rfc3339"); err == nil {
		t.Fatal("malformed notification timestamp was accepted")
	} else {
		var parseErr *time.ParseError
		if !errors.As(err, &parseErr) {
			t.Fatalf("malformed timestamp error = %T %v, want *time.ParseError", err, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLAutoReleaseNotificationMarkerIsIdempotentForExactCycle(t *testing.T) {
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseNotifiedAt = "2026-07-13T09:00:00Z"
	reminder.CreatedAt = "2026-07-13T08:00:00Z"
	reminder.UpdatedAt = "2026-07-13T09:01:00Z"
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	store := mysqlAutoReleaseTestStore(db, time.Date(2026, 7, 13, 9, 5, 0, 0, time.UTC))
	mock.ExpectBegin()
	expectMySQLAutoReleaseLockedReminder(mock, reminder)
	mock.ExpectCommit()
	got, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), "2026-07-13T09:05:00Z")
	if err != nil {
		t.Fatalf("repeat marker: %v", err)
	}
	if !reflect.DeepEqual(got, reminder) {
		t.Fatalf("idempotent marker changed reminder: got=%+v want=%+v", got, reminder)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLAutoReleaseNotificationMarkerRejectsStaleOrNewOwnerCycle(t *testing.T) {
	current := notifyingAutoReleaseReminder("owner@example.com")
	current.CreatedAt = "2026-07-13T08:00:00Z"
	current.UpdatedAt = "2026-07-13T08:10:00Z"
	tests := map[string]func(*ReleaseReminderCycle){
		"auto release at":         func(cycle *ReleaseReminderCycle) { cycle.AutoReleaseAt = "2026-07-14T08:10:00Z" },
		"auto release started at": func(cycle *ReleaseReminderCycle) { cycle.AutoReleaseStartedAt = "2026-07-14T08:10:00Z" },
		"host":                    func(cycle *ReleaseReminderCycle) { cycle.HostID = "h-new" },
		"apple email":             func(cycle *ReleaseReminderCycle) { cycle.AppleEmail = "new-apple@example.com" },
		"new owner":               func(cycle *ReleaseReminderCycle) { cycle.OwnerEmail = "new-owner@example.com" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			store := mysqlAutoReleaseTestStore(db, time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC))
			cycle := releaseReminderCycle(current)
			mutate(&cycle)
			mock.ExpectBegin()
			expectMySQLAutoReleaseLockedReminder(mock, current)
			mock.ExpectRollback()
			if _, err := store.MarkAutoReleaseNotified(cycle, "2026-07-13T09:00:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("marker error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLAutoReleaseNotificationMarkerRequiresNotifyingReminder(t *testing.T) {
	tests := map[string]func(*ReleaseReminder){
		"due notified": func(reminder *ReleaseReminder) { reminder.Status = ReleaseReminderStatusActive },
		"enabled":      func(reminder *ReleaseReminder) { reminder.AutoReleaseEnabled = false },
		"notifying":    func(reminder *ReleaseReminder) { reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			reminder := notifyingAutoReleaseReminder("owner@example.com")
			mutate(&reminder)
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			store := mysqlAutoReleaseTestStore(db, time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC))
			mock.ExpectBegin()
			expectMySQLAutoReleaseLockedReminder(mock, reminder)
			mock.ExpectRollback()
			if _, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), "2026-07-13T09:00:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("marker error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLAutoReleaseNotificationMarkerRejectsMarkedInvalidState(t *testing.T) {
	tests := map[string]func(*ReleaseReminder){
		"released":    func(reminder *ReleaseReminder) { reminder.Status = ReleaseReminderStatusReleased },
		"disabled":    func(reminder *ReleaseReminder) { reminder.AutoReleaseEnabled = false },
		"wrong state": func(reminder *ReleaseReminder) { reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			reminder := notifyingAutoReleaseReminder("owner@example.com")
			reminder.AutoReleaseNotifiedAt = "2026-07-13T09:00:00Z"
			mutate(&reminder)
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			store := mysqlAutoReleaseTestStore(db, time.Date(2026, 7, 13, 9, 5, 0, 0, time.UTC))
			mock.ExpectBegin()
			expectMySQLAutoReleaseLockedReminder(mock, reminder)
			mock.ExpectRollback()
			if _, err := store.MarkAutoReleaseNotified(releaseReminderCycle(reminder), "2026-07-13T09:05:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("marker error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLCompleteAutoReleaseTransactionClearsOnlyMatchingOwner(t *testing.T) {
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseNotifiedAt = "2026-07-13T08:59:00Z"
	reminder.CreatedAt = "2026-07-13T08:00:00Z"
	reminder.UpdatedAt = "2026-07-13T08:59:00Z"
	releasedAt := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	want := reminder
	want.Status = ReleaseReminderStatusReleased
	want.ReleasedAt = releasedAt.Format(time.RFC3339)
	want.AutoReleaseState = ReleaseReminderAutoReleaseStateReleased
	want.AutoReleaseLastError = ""
	want.UpdatedAt = releasedAt.Format(time.RFC3339)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	store := mysqlAutoReleaseTestStore(db, releasedAt)
	mock.ExpectBegin()
	expectMySQLAutoReleaseLockedReminder(mock, reminder)
	mock.ExpectQuery(regexp.QuoteMeta(mysqlProfileOwnerForUpdateQuery)).
		WithArgs(reminder.ProfileName).
		WillReturnRows(sqlmock.NewRows([]string{"member_id", "email"}).AddRow("member-1", reminder.OwnerEmail))
	expectMySQLAutoReleaseReminderUpdate(mock, want)
	mock.ExpectExec(regexp.QuoteMeta(mysqlDeleteMatchingProfileOwnerQuery)).
		WithArgs(reminder.ProfileName, "member-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := store.CompleteAutoRelease(releaseReminderCycle(reminder), releasedAt.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("complete transaction: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("completed reminder = %+v, want %+v", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLCompleteAutoReleaseRequiresPersistedNotification(t *testing.T) {
	tests := map[string]func(*ReleaseReminder){
		"running with marker": func(reminder *ReleaseReminder) {
			reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
		},
		"notifying without marker": func(reminder *ReleaseReminder) {
			reminder.AutoReleaseNotifiedAt = ""
		},
		"wrong status": func(reminder *ReleaseReminder) {
			reminder.Status = ReleaseReminderStatusActive
		},
		"disabled": func(reminder *ReleaseReminder) {
			reminder.AutoReleaseEnabled = false
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			reminder := notifyingAutoReleaseReminder("owner@example.com")
			reminder.AutoReleaseNotifiedAt = "2026-07-13T08:59:00Z"
			mutate(&reminder)
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			store := mysqlAutoReleaseTestStore(db, time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC))
			mock.ExpectBegin()
			expectMySQLAutoReleaseLockedReminder(mock, reminder)
			mock.ExpectRollback()
			if _, err := store.CompleteAutoRelease(releaseReminderCycle(reminder), "2026-07-13T09:00:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
				t.Fatalf("completion error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLCompleteAutoReleaseTransactionRejectsNewOwner(t *testing.T) {
	reminder := notifyingAutoReleaseReminder("old@example.com")
	reminder.AutoReleaseNotifiedAt = "2026-07-13T08:59:00Z"
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	store := mysqlAutoReleaseTestStore(db, time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC))
	mock.ExpectBegin()
	expectMySQLAutoReleaseLockedReminder(mock, reminder)
	mock.ExpectQuery(regexp.QuoteMeta(mysqlProfileOwnerForUpdateQuery)).
		WithArgs(reminder.ProfileName).
		WillReturnRows(sqlmock.NewRows([]string{"member_id", "email"}).AddRow("member-new", "new@example.com"))
	mock.ExpectRollback()
	if _, err := store.CompleteAutoRelease(releaseReminderCycle(reminder), "2026-07-13T09:00:00Z"); !errors.Is(err, ErrReleaseReminderCycleChanged) {
		t.Fatalf("error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLCompleteAutoReleaseRollbackPreservesNotificationMarker(t *testing.T) {
	reminder := notifyingAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseNotifiedAt = "2026-07-13T08:59:00Z"
	reminder.CreatedAt = "2026-07-13T08:00:00Z"
	reminder.UpdatedAt = "2026-07-13T08:59:00Z"
	releasedAt := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	updated := reminder
	updated.Status = ReleaseReminderStatusReleased
	updated.ReleasedAt = releasedAt.Format(time.RFC3339)
	updated.AutoReleaseState = ReleaseReminderAutoReleaseStateReleased
	updated.UpdatedAt = releasedAt.Format(time.RFC3339)
	wantErr := errors.New("owner cleanup failed")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	store := mysqlAutoReleaseTestStore(db, releasedAt)
	mock.ExpectBegin()
	expectMySQLAutoReleaseLockedReminder(mock, reminder)
	mock.ExpectQuery(regexp.QuoteMeta(mysqlProfileOwnerForUpdateQuery)).
		WithArgs(reminder.ProfileName).
		WillReturnRows(sqlmock.NewRows([]string{"member_id", "email"}).AddRow("member-1", reminder.OwnerEmail))
	expectMySQLAutoReleaseReminderUpdate(mock, updated)
	mock.ExpectExec(regexp.QuoteMeta(mysqlDeleteMatchingProfileOwnerQuery)).
		WithArgs(reminder.ProfileName, "member-1").
		WillReturnError(wantErr)
	mock.ExpectRollback()
	if _, err := store.CompleteAutoRelease(releaseReminderCycle(reminder), releasedAt.Format(time.RFC3339)); !errors.Is(err, wantErr) {
		t.Fatalf("completion error = %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	reloadDB, reloadMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("reload sqlmock: %v", err)
	}
	reloadStore := mysqlAutoReleaseTestStore(reloadDB, releasedAt)
	expectMySQLAutoReleaseReminderLoad(reloadMock, reminder)
	data, err := reloadStore.Load()
	if err != nil {
		t.Fatalf("reload after rollback: %v", err)
	}
	if len(data.Reminders) != 1 || !reflect.DeepEqual(data.Reminders[0], reminder) {
		t.Fatalf("durable reminder after rollback = %+v, want %+v", data.Reminders, reminder)
	}
	if err := reloadMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type mysqlAutoReleaseRecordingExecer struct {
	query string
	args  []any
}

func (e *mysqlAutoReleaseRecordingExecer) Exec(query string, args ...any) error {
	e.query = query
	e.args = append([]any(nil), args...)
	return nil
}

var mysqlAutoReleaseReminderColumnNames = []string{
	"profile_name", "apple_email", "host_id", "host_architecture", "host_created_at", "release_due_at", "owner_email", "owner_name",
	"last_extended_by_email", "last_extended_by_name", "last_extended_at", "last_notified_at", "released_at", "status",
	"auto_release_enabled", "auto_release_at", "auto_release_started_at", "auto_release_last_attempt_at", "auto_release_accepted_at",
	"auto_release_stalled_notified_at", "auto_release_notified_at",
	"auto_release_attempts", "auto_release_last_error", "auto_release_state", "created_at", "updated_at",
}

func mysqlAutoReleaseReminderRows(reminder ReleaseReminder) *sqlmock.Rows {
	return sqlmock.NewRows(mysqlAutoReleaseReminderColumnNames).AddRow(
		reminder.ProfileName, reminder.AppleEmail, reminder.HostID, reminder.HostArchitecture, reminder.HostCreatedAt, reminder.ReleaseDueAt,
		reminder.OwnerEmail, reminder.OwnerName, reminder.LastExtendedByEmail, reminder.LastExtendedByName,
		reminder.LastExtendedAt, reminder.LastNotifiedAt, reminder.ReleasedAt, reminder.Status,
		reminder.AutoReleaseEnabled, reminder.AutoReleaseAt, reminder.AutoReleaseStartedAt,
		reminder.AutoReleaseLastAttemptAt, reminder.AutoReleaseAcceptedAt, reminder.AutoReleaseStalledNotifiedAt,
		reminder.AutoReleaseNotifiedAt, reminder.AutoReleaseAttempts,
		reminder.AutoReleaseLastError, reminder.AutoReleaseState, reminder.CreatedAt, reminder.UpdatedAt,
	)
}

func mysqlAutoReleaseTestStore(db *sql.DB, now time.Time) MySQLMemberStore {
	return MySQLMemberStore{
		DSN:         "sqlmock",
		Now:         func() time.Time { return now },
		schemaGuard: &mysqlSchemaGuard{success: true},
		openDB:      func() (*sql.DB, error) { return db, nil },
	}
}

func expectMySQLAutoReleaseLockedReminder(mock sqlmock.Sqlmock, reminder ReleaseReminder) {
	mock.ExpectQuery(regexp.QuoteMeta(mysqlStoreLockForUpdateQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(9))
	mock.ExpectQuery(regexp.QuoteMeta(mysqlReleaseReminderSelectForUpdate)).
		WithArgs(reminder.ProfileName).
		WillReturnRows(mysqlAutoReleaseReminderRows(reminder))
}

func expectMySQLAutoReleaseReminderUpdate(mock sqlmock.Sqlmock, reminder ReleaseReminder) {
	mock.ExpectExec(regexp.QuoteMeta(mysqlReleaseReminderUpdateQuery)).
		WithArgs(
			reminder.AppleEmail, reminder.HostID, reminder.HostArchitecture, reminder.HostCreatedAt, reminder.ReleaseDueAt,
			reminder.OwnerEmail, reminder.OwnerName, reminder.LastExtendedByEmail, reminder.LastExtendedByName,
			reminder.LastExtendedAt, reminder.LastNotifiedAt, reminder.ReleasedAt, reminder.Status,
			reminder.AutoReleaseEnabled, reminder.AutoReleaseAt, reminder.AutoReleaseStartedAt,
			reminder.AutoReleaseLastAttemptAt, reminder.AutoReleaseAcceptedAt, reminder.AutoReleaseStalledNotifiedAt,
			reminder.AutoReleaseNotifiedAt, reminder.AutoReleaseAttempts,
			reminder.AutoReleaseLastError, reminder.AutoReleaseState, reminder.UpdatedAt, reminder.ProfileName,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectMySQLAutoReleaseReminderLoad(mock sqlmock.Sqlmock, reminder ReleaseReminder) {
	mock.ExpectQuery(regexp.QuoteMeta(mysqlStoreLockVersionQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(10))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, name, email, username, role, enabled, COALESCE(password_hash, ''), COALESCE(password_salt, ''), COALESCE(api_token_hash, ''), COALESCE(api_token_at, ''), created_at, updated_at FROM cm_members ORDER BY email`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT apple_email, member_id, relation, created_at FROM cm_assignments ORDER BY apple_email, member_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"apple_email"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT profile_name, member_id, updated_at FROM cm_profile_owners ORDER BY profile_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"profile_name"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT name, COALESCE(apple_email, ''), enabled, profile_yaml, created_at, updated_at FROM cm_profiles ORDER BY name`)).
		WillReturnRows(sqlmock.NewRows([]string{"name"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT profile_name, member_id, created_at FROM cm_profile_members ORDER BY profile_name, member_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"profile_name"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + mysqlReleaseReminderSelectColumns + ` FROM cm_release_reminders ORDER BY profile_name`)).
		WillReturnRows(mysqlAutoReleaseReminderRows(reminder))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + mysqlTransferSelectColumns + ` FROM cm_transfer_records ORDER BY updated_at DESC, id DESC`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT setting_key, COALESCE(setting_value, '') FROM cm_settings`)).
		WillReturnRows(sqlmock.NewRows([]string{"setting_key"}))
}

type fakeMySQLProfileOwnerRow struct {
	memberID string
	email    string
	err      error
}

func (r fakeMySQLProfileOwnerRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = r.memberID
	*dest[1].(*string) = r.email
	return nil
}
