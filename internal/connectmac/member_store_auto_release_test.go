package connectmac

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
	if len(db.Reminders) != 1 || db.Reminders[0].AutoReleaseNotifiedAt != "" {
		t.Fatalf("legacy reminder = %+v", db.Reminders)
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
			if got.AutoReleaseEnabled || got.AutoReleaseAt != "" || got.AutoReleaseStartedAt != "" || got.AutoReleaseLastAttemptAt != "" || got.AutoReleaseNotifiedAt != "" || got.AutoReleaseAttempts != 0 || got.AutoReleaseLastError != "" || got.AutoReleaseState != "" {
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
	old.AutoReleaseNotifiedAt = "2026-07-13T09:00:00Z"
	if _, err := store.UpsertReleaseReminder(old); err != nil {
		t.Fatalf("upsert old reminder: %v", err)
	}

	updated := old
	updated.HostCreatedAt = "2026-07-13T08:00:00Z"
	updated.OwnerName = "Updated Owner"
	updated.AutoReleaseNotifiedAt = ""
	got, err := store.UpsertReleaseReminder(updated)
	if err != nil {
		t.Fatalf("upsert same-cycle reminder: %v", err)
	}
	if got.HostID != old.HostID || got.AppleEmail != old.AppleEmail || got.OwnerEmail != old.OwnerEmail || got.OwnerName != updated.OwnerName || got.Status != old.Status {
		t.Fatalf("same-cycle fields not updated: %+v", got)
	}
	if !got.AutoReleaseEnabled || got.AutoReleaseAt != old.AutoReleaseAt || got.AutoReleaseStartedAt != old.AutoReleaseStartedAt || got.AutoReleaseLastAttemptAt != old.AutoReleaseLastAttemptAt || got.AutoReleaseNotifiedAt != old.AutoReleaseNotifiedAt || got.AutoReleaseAttempts != old.AutoReleaseAttempts || got.AutoReleaseLastError != old.AutoReleaseLastError || got.AutoReleaseState != old.AutoReleaseState {
		t.Fatalf("same-cycle auto-release state was not preserved: %+v", got)
	}

	got, err = store.UpdateReleaseReminder(old.ProfileName, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.ReleaseDueAt = "2026-07-15T08:00:00Z"
		return reminder, nil
	})
	if err != nil {
		t.Fatalf("update unrelated reminder field: %v", err)
	}
	if got.ReleaseDueAt != "2026-07-15T08:00:00Z" || got.AutoReleaseNotifiedAt != old.AutoReleaseNotifiedAt {
		t.Fatalf("unrelated callback update changed marker: %+v", got)
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
	if got.AutoReleaseEnabled || got.AutoReleaseAt != "" || got.AutoReleaseStartedAt != "" || got.AutoReleaseLastAttemptAt != "" || got.AutoReleaseNotifiedAt != "" || got.AutoReleaseAttempts != 0 || got.AutoReleaseLastError != "" || got.AutoReleaseState != "" {
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
		ProfileName:          reminder.ProfileName,
		AutoReleaseAt:        reminder.AutoReleaseAt,
		AutoReleaseStartedAt: reminder.AutoReleaseStartedAt,
		HostID:               reminder.HostID,
		AppleEmail:           reminder.AppleEmail,
		OwnerEmail:           reminder.OwnerEmail,
	}
}

func TestMySQLCompleteAutoReleaseTransactionClearsOnlyMatchingOwner(t *testing.T) {
	reminder := runningAutoReleaseReminder("owner@example.com")
	reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateNotifying
	tx := &fakeMySQLReleaseReminderTransaction{
		row:      fakeMySQLReleaseReminderRow{reminder: reminder},
		ownerRow: fakeMySQLProfileOwnerRow{memberID: "member-1", email: "owner@example.com"},
	}
	got, err := completeAutoReleaseInMySQLTransaction(tx, releaseReminderCycle(reminder), time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("complete transaction: %v", err)
	}
	if got.Status != ReleaseReminderStatusReleased || !tx.ownerDeleted || !tx.committed {
		t.Fatalf("result=%+v ownerDeleted=%t committed=%t", got, tx.ownerDeleted, tx.committed)
	}
}

func TestMySQLCompleteAutoReleaseTransactionRejectsNewOwner(t *testing.T) {
	reminder := runningAutoReleaseReminder("old@example.com")
	tx := &fakeMySQLReleaseReminderTransaction{
		row:      fakeMySQLReleaseReminderRow{reminder: reminder},
		ownerRow: fakeMySQLProfileOwnerRow{memberID: "member-new", email: "new@example.com"},
	}
	if _, err := completeAutoReleaseInMySQLTransaction(tx, releaseReminderCycle(reminder), time.Now()); !errors.Is(err, ErrReleaseReminderCycleChanged) {
		t.Fatalf("error = %v", err)
	}
	if tx.ownerDeleted || tx.committed || !tx.rolledBack {
		t.Fatalf("ownerDeleted=%t committed=%t rolledBack=%t", tx.ownerDeleted, tx.committed, tx.rolledBack)
	}
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
