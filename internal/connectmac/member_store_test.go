package connectmac

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemberStoreCRUDAndEvents(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time {
		return time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	}

	member, err := store.AddMember("王恒辉", "WHH@example.com", "")
	if err != nil {
		t.Fatalf("add member: %v", err)
	}
	if member.Email != "whh@example.com" || member.Role != "operator" || !member.Enabled {
		t.Fatalf("member = %+v", member)
	}
	assignment, err := store.AssignMember("Apple@example.com", "whh@example.com", "")
	if err != nil {
		t.Fatalf("assign member: %v", err)
	}
	if assignment.AppleEmail != "apple@example.com" || assignment.Relation != "owner" {
		t.Fatalf("assignment = %+v", assignment)
	}
	owners, err := store.MembersForApple("APPLE@example.com")
	if err != nil {
		t.Fatalf("members for apple: %v", err)
	}
	if len(owners) != 1 || owners[0].Email != "whh@example.com" {
		t.Fatalf("owners = %+v", owners)
	}
	profileOwner, err := store.SetProfileOwner("apple-usw2", "whh@example.com")
	if err != nil {
		t.Fatalf("set profile owner: %v", err)
	}
	if profileOwner.ProfileName != "apple-usw2" || profileOwner.Owner.Email != "whh@example.com" {
		t.Fatalf("profile owner = %+v", profileOwner)
	}
	profileOwner, ok, err := store.ProfileOwner("apple-usw2")
	if err != nil {
		t.Fatalf("profile owner lookup: %v", err)
	}
	if !ok || profileOwner.Owner.Email != "whh@example.com" {
		t.Fatalf("profile owner lookup = %+v ok=%t", profileOwner, ok)
	}
	profileOwners, err := store.ProfileOwners()
	if err != nil {
		t.Fatalf("profile owners: %v", err)
	}
	if len(profileOwners) != 1 || profileOwners[0].ProfileName != "apple-usw2" {
		t.Fatalf("profile owners = %+v", profileOwners)
	}
	disabled, err := store.SetMemberEnabled("whh@example.com", false)
	if err != nil {
		t.Fatalf("disable member: %v", err)
	}
	if disabled.Enabled {
		t.Fatalf("member should be disabled: %+v", disabled)
	}
	if err := store.RecordEvent(OperationEvent{
		Action:     "open",
		Profile:    "apple-usw2",
		AppleEmail: "apple@example.com",
		Confirmed:  true,
		Status:     "success",
		Message:    "started",
	}); err != nil {
		t.Fatalf("record event: %v", err)
	}
	events, err := store.RecentEvents("apple@example.com", 10)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	if len(events) != 1 || events[0].Action != "open" || events[0].Status != "success" {
		t.Fatalf("events = %+v", events)
	}
	if err := store.UnassignMember("apple@example.com", "whh@example.com"); err != nil {
		t.Fatalf("unassign member: %v", err)
	}
	owners, err = store.MembersForApple("apple@example.com")
	if err != nil {
		t.Fatalf("members for apple after unassign: %v", err)
	}
	if len(owners) != 0 {
		t.Fatalf("owners after unassign = %+v", owners)
	}
}

func TestMemberStoreManagedProfilesAccess(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time {
		return time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	}
	if _, err := store.AddMember("Admin", "admin@example.com", "admin"); err != nil {
		t.Fatalf("add admin: %v", err)
	}
	if _, err := store.AddMember("User", "user@example.com", "operator"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	profile := Profile{Name: "apple-usw2", Description: "Apple account: apple@example.com"}
	profile.AWS.AccountEmail = "apple@example.com"
	profile.AWS.Profile = "cm-xcode"
	profile.AWS.Region = "us-west-2"
	record, err := store.UpsertManagedProfile(profile)
	if err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
	if record.Name != "apple-usw2" || !record.Enabled || !strings.Contains(record.ProfileYAML, "apple-usw2") {
		t.Fatalf("record = %+v", record)
	}
	memberProfiles, err := store.ListManagedProfiles("user@example.com")
	if err != nil {
		t.Fatalf("list member profiles: %v", err)
	}
	if len(memberProfiles) != 0 {
		t.Fatalf("member profiles before grant = %+v", memberProfiles)
	}
	if _, err := store.AssignProfileAccess("apple-usw2", "user@example.com"); err != nil {
		t.Fatalf("grant profile: %v", err)
	}
	memberProfiles, err = store.ListManagedProfiles("user@example.com")
	if err != nil {
		t.Fatalf("list member profiles after grant: %v", err)
	}
	if len(memberProfiles) != 1 || memberProfiles[0].Name != "apple-usw2" {
		t.Fatalf("member profiles after grant = %+v", memberProfiles)
	}
	members, err := store.ListMembers()
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	var userProfiles []string
	for _, member := range members {
		if member.Email == "user@example.com" {
			userProfiles = member.Profiles
			break
		}
	}
	if len(userProfiles) != 1 || userProfiles[0] != "apple-usw2" {
		t.Fatalf("member profile names = %+v", userProfiles)
	}
	adminProfiles, err := store.ListManagedProfiles("admin@example.com")
	if err != nil {
		t.Fatalf("list admin profiles: %v", err)
	}
	if len(adminProfiles) != 1 {
		t.Fatalf("admin profiles = %+v", adminProfiles)
	}
}

func TestMemberStoreReleaseReminders(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time {
		return time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	}
	if _, err := store.AddMember("Admin", "admin@example.com", "admin"); err != nil {
		t.Fatalf("add admin: %v", err)
	}
	if _, err := store.AddMember("User", "user@example.com", "operator"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	profile := Profile{Name: "apple-usw2", Description: "Apple account: apple@example.com"}
	profile.AWS.AccountEmail = "apple@example.com"
	profile.AWS.Profile = "cm-xcode"
	profile.AWS.Region = "us-west-2"
	if _, err := store.UpsertManagedProfile(profile); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
	if _, err := store.AssignProfileAccess("apple-usw2", "user@example.com"); err != nil {
		t.Fatalf("grant profile: %v", err)
	}
	reminder := ReleaseReminder{
		ProfileName:         "apple-usw2",
		AppleEmail:          "apple@example.com",
		HostID:              "h-123",
		HostCreatedAt:       "2026-07-01T08:00:00Z",
		ReleaseDueAt:        "2026-07-02T08:00:00Z",
		OwnerEmail:          "user@example.com",
		OwnerName:           "User",
		LastExtendedByEmail: "admin@example.com",
		LastExtendedByName:  "Admin",
		LastExtendedAt:      "2026-07-01T09:00:00Z",
		Status:              ReleaseReminderStatusActive,
	}
	saved, err := store.UpsertReleaseReminder(reminder)
	if err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	if saved.ProfileName != "apple-usw2" || saved.Status != ReleaseReminderStatusActive {
		t.Fatalf("saved reminder = %+v", saved)
	}
	memberReminders, err := store.ListReleaseReminders("user@example.com")
	if err != nil {
		t.Fatalf("list member reminders: %v", err)
	}
	if len(memberReminders) != 1 || memberReminders[0].ProfileName != "apple-usw2" {
		t.Fatalf("member reminders = %+v", memberReminders)
	}
	adminReminders, err := store.ListReleaseReminders("admin@example.com")
	if err != nil {
		t.Fatalf("list admin reminders: %v", err)
	}
	if len(adminReminders) != 1 {
		t.Fatalf("admin reminders = %+v", adminReminders)
	}
	found, ok, err := store.ReleaseReminder("apple-usw2")
	if err != nil {
		t.Fatalf("lookup reminder: %v", err)
	}
	if !ok || found.HostID != "h-123" {
		t.Fatalf("lookup reminder = %+v ok=%t", found, ok)
	}
	due, err := store.MarkReleaseReminderDue("apple-usw2", "2026-07-02T08:01:00Z")
	if err != nil {
		t.Fatalf("mark due: %v", err)
	}
	if due.Status != ReleaseReminderStatusDueNotified || due.LastNotifiedAt != "2026-07-02T08:01:00Z" {
		t.Fatalf("due reminder = %+v", due)
	}
	released, err := store.MarkReleaseReminderReleased("apple-usw2", "2026-07-02T09:00:00Z")
	if err != nil {
		t.Fatalf("mark released: %v", err)
	}
	if released.Status != ReleaseReminderStatusReleased ||
		released.ReleasedAt != "2026-07-02T09:00:00Z" ||
		released.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased ||
		released.AutoReleaseLastError != "" {
		t.Fatalf("released reminder = %+v", released)
	}
}

func TestMemberStoreReleaseReminderLegacyJSONDefaultsAutoRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "members.json")
	legacy := `{"release_reminders":[{"profile_name":"apple-usw2","status":"active","created_at":"2026-07-01T08:00:00Z","updated_at":"2026-07-01T08:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy data: %v", err)
	}

	reminder, ok, err := NewMemberStore(path).ReleaseReminder("apple-usw2")
	if err != nil {
		t.Fatalf("load legacy reminder: %v", err)
	}
	if !ok {
		t.Fatal("legacy reminder not found")
	}
	if reminder.AutoReleaseEnabled || reminder.AutoReleaseAt != "" || reminder.AutoReleaseAttempts != 0 || reminder.AutoReleaseState != "" {
		t.Fatalf("legacy auto-release defaults = %+v", reminder)
	}
}

func TestMemberStoreReleaseReminderAutoReleaseRoundTrip(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	want := ReleaseReminder{
		ProfileName:              "apple-usw2",
		Status:                   ReleaseReminderStatusActive,
		AutoReleaseEnabled:       true,
		AutoReleaseAt:            "2026-07-02T08:00:00Z",
		AutoReleaseStartedAt:     "2026-07-02T08:01:00Z",
		AutoReleaseLastAttemptAt: "2026-07-02T08:02:00Z",
		AutoReleaseAttempts:      2,
		AutoReleaseLastError:     "host is pending",
		AutoReleaseState:         ReleaseReminderAutoReleaseStateRetrying,
	}
	if _, err := store.UpsertReleaseReminder(want); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	got, ok, err := store.ReleaseReminder(want.ProfileName)
	if err != nil {
		t.Fatalf("load reminder: %v", err)
	}
	if !ok {
		t.Fatal("saved reminder not found")
	}
	if !got.AutoReleaseEnabled || got.AutoReleaseAt != want.AutoReleaseAt || got.AutoReleaseStartedAt != want.AutoReleaseStartedAt || got.AutoReleaseLastAttemptAt != want.AutoReleaseLastAttemptAt || got.AutoReleaseAttempts != want.AutoReleaseAttempts || got.AutoReleaseLastError != want.AutoReleaseLastError || got.AutoReleaseState != want.AutoReleaseState {
		t.Fatalf("auto-release round trip = %+v", got)
	}
}

func TestMemberStoreUpdateReleaseReminderPreservesFields(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time { return time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC) }
	original, err := store.UpsertReleaseReminder(ReleaseReminder{
		ProfileName: "apple-usw2",
		AppleEmail:  "apple@example.com",
		HostID:      "h-123",
		OwnerEmail:  "owner@example.com",
		Status:      ReleaseReminderStatusActive,
	})
	if err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}

	updated, err := store.UpdateReleaseReminder("apple-usw2", func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.AutoReleaseEnabled = true
		reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateScheduled
		return reminder, nil
	})
	if err != nil {
		t.Fatalf("update reminder: %v", err)
	}
	if updated.HostID != original.HostID || updated.AppleEmail != original.AppleEmail || updated.OwnerEmail != original.OwnerEmail || updated.Status != original.Status || updated.CreatedAt != original.CreatedAt {
		t.Fatalf("unrelated fields changed: before=%+v after=%+v", original, updated)
	}
	if !updated.AutoReleaseEnabled || updated.AutoReleaseState != ReleaseReminderAutoReleaseStateScheduled {
		t.Fatalf("auto-release fields not updated: %+v", updated)
	}
}

func TestMemberStoreUpdateReleaseReminderErrorsDoNotPersist(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	if _, err := store.UpsertReleaseReminder(ReleaseReminder{ProfileName: "apple-usw2"}); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	wantErr := errors.New("stop update")
	_, err := store.UpdateReleaseReminder("apple-usw2", func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.AutoReleaseAttempts = 99
		return reminder, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("update error = %v, want %v", err, wantErr)
	}
	reminder, _, err := store.ReleaseReminder("apple-usw2")
	if err != nil {
		t.Fatalf("load reminder: %v", err)
	}
	if reminder.AutoReleaseAttempts != 0 {
		t.Fatalf("failed update persisted: %+v", reminder)
	}

	for _, profileName := range []string{"", "missing"} {
		called := false
		_, err := store.UpdateReleaseReminder(profileName, func(reminder ReleaseReminder) (ReleaseReminder, error) {
			called = true
			return reminder, nil
		})
		if err == nil {
			t.Fatalf("UpdateReleaseReminder(%q) error = nil", profileName)
		}
		if profileName == "missing" && !errors.Is(err, ErrReleaseReminderNotFound) {
			t.Fatalf("UpdateReleaseReminder missing error = %v", err)
		}
		if called {
			t.Fatalf("UpdateReleaseReminder(%q) invoked callback", profileName)
		}
	}
}

func TestMemberStoreUpdateReleaseReminderAndRecordEventIsAtomic(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	seed, err := store.UpsertReleaseReminder(ReleaseReminder{ProfileName: "apple-usw2", AppleEmail: "apple@example.com"})
	if err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	_, err = store.UpdateReleaseReminderAndRecordEvent(seed.ProfileName, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.AutoReleaseEnabled = true
		return reminder, nil
	}, OperationEvent{})
	if err == nil || !strings.Contains(err.Error(), "event action") {
		t.Fatalf("invalid event error = %v", err)
	}
	got, _, err := store.ReleaseReminder(seed.ProfileName)
	if err != nil {
		t.Fatalf("load reminder: %v", err)
	}
	if !reflect.DeepEqual(got, seed) {
		t.Fatalf("failed event persisted reminder: got=%+v want=%+v", got, seed)
	}
	events, err := store.RecentEvents(seed.AppleEmail, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("events after rollback = %+v err=%v", events, err)
	}

	updated, err := store.UpdateReleaseReminderAndRecordEvent(seed.ProfileName, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.AutoReleaseEnabled = true
		return reminder, nil
	}, OperationEvent{Action: "release-reminder.auto-release.enabled", MemberID: "member-1", MemberEmail: " ADMIN@EXAMPLE.COM ", MemberName: " Admin ", Status: "success"})
	if err != nil {
		t.Fatalf("atomic update: %v", err)
	}
	events, err = store.RecentEvents(seed.AppleEmail, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events after success = %+v err=%v", events, err)
	}
	event := events[0]
	if !updated.AutoReleaseEnabled || event.Profile != seed.ProfileName || event.AppleEmail != seed.AppleEmail || event.MemberID != "member-1" || event.MemberEmail != "admin@example.com" || event.MemberName != "Admin" || event.Action != "release-reminder.auto-release.enabled" {
		t.Fatalf("atomic result reminder=%+v event=%+v", updated, event)
	}
}

func TestMemberStoreUpdateReleaseReminderAndRecordEventRollsBackSaveFailure(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	seed, err := store.UpsertReleaseReminder(ReleaseReminder{ProfileName: "apple-usw2", AppleEmail: "apple@example.com"})
	if err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	wantErr := errors.New("replace failed")
	store.Rename = func(string, string) error { return wantErr }
	_, err = store.UpdateReleaseReminderAndRecordEvent(seed.ProfileName, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.AutoReleaseEnabled = true
		return reminder, nil
	}, OperationEvent{Action: "release-reminder.auto-release.enabled", Status: "success"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("save failure = %v, want %v", err, wantErr)
	}
	got, _, err := store.ReleaseReminder(seed.ProfileName)
	if err != nil {
		t.Fatalf("load reminder: %v", err)
	}
	if !reflect.DeepEqual(got, seed) {
		t.Fatalf("failed save persisted reminder: got=%+v want=%+v", got, seed)
	}
	events, err := store.RecentEvents(seed.AppleEmail, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("events after failed save = %+v err=%v", events, err)
	}
}

func TestMemberStoreSetProfileOwnerAndRecordEventIsAtomic(t *testing.T) {
	newStore := func(t *testing.T) MemberStore {
		t.Helper()
		store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
		if _, err := store.AddMember("Old Owner", "old@example.com", "operator"); err != nil {
			t.Fatalf("add old owner: %v", err)
		}
		if _, err := store.AddMember("New Owner", "new@example.com", "operator"); err != nil {
			t.Fatalf("add new owner: %v", err)
		}
		profile := Profile{Name: "apple-usw2"}
		profile.AWS.AccountEmail = "apple@example.com"
		if _, err := store.UpsertManagedProfile(profile); err != nil {
			t.Fatalf("upsert profile: %v", err)
		}
		return store
	}

	t.Run("success persists owner and normalized event", func(t *testing.T) {
		store := newStore(t)
		owner, err := store.SetProfileOwnerAndRecordEvent("apple-usw2", " NEW@EXAMPLE.COM ", OperationEvent{
			Action:      "profile-owner.set",
			MemberID:    "member-admin",
			MemberEmail: " ADMIN@EXAMPLE.COM ",
			MemberName:  " Admin ",
			Confirmed:   true,
			Status:      "success",
			Message:     "owner changed",
		})
		if err != nil {
			t.Fatalf("atomic owner update: %v", err)
		}
		if owner.ProfileName != "apple-usw2" || owner.Owner.Email != "new@example.com" {
			t.Fatalf("owner = %+v", owner)
		}
		events, err := store.RecentEvents("apple@example.com", 10)
		if err != nil || len(events) != 1 {
			t.Fatalf("events = %+v err=%v", events, err)
		}
		event := events[0]
		if event.Profile != "apple-usw2" || event.AppleEmail != "apple@example.com" ||
			event.MemberEmail != "admin@example.com" || event.MemberName != "Admin" ||
			event.Action != "profile-owner.set" || event.Status != "success" {
			t.Fatalf("event = %+v", event)
		}
	})

	t.Run("save failure preserves old owner and no event", func(t *testing.T) {
		store := newStore(t)
		oldOwner, err := store.SetProfileOwner("apple-usw2", "old@example.com")
		if err != nil {
			t.Fatalf("seed old owner: %v", err)
		}
		wantErr := errors.New("replace failed")
		store.Rename = func(string, string) error { return wantErr }
		_, err = store.SetProfileOwnerAndRecordEvent("apple-usw2", "new@example.com", OperationEvent{
			Action: "profile-owner.set",
			Status: "success",
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("save failure = %v, want %v", err, wantErr)
		}
		got, found, err := store.ProfileOwner("apple-usw2")
		if err != nil || !found || !reflect.DeepEqual(got, oldOwner) {
			t.Fatalf("owner after failed save = %+v found=%t err=%v, want %+v", got, found, err, oldOwner)
		}
		events, err := store.RecentEvents("apple@example.com", 10)
		if err != nil || len(events) != 0 {
			t.Fatalf("events after failed save = %+v err=%v", events, err)
		}
	})
}

func TestMemberStoreUpdateReleaseReminderSerializesConcurrentUpdates(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	if _, err := store.UpsertReleaseReminder(ReleaseReminder{ProfileName: "apple-usw2"}); err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}

	const updates = 10
	errs := make(chan error, updates)
	var wg sync.WaitGroup
	for range updates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.UpdateReleaseReminder("apple-usw2", func(reminder ReleaseReminder) (ReleaseReminder, error) {
				reminder.AutoReleaseAttempts++
				return reminder, nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}
	reminder, _, err := store.ReleaseReminder("apple-usw2")
	if err != nil {
		t.Fatalf("load reminder: %v", err)
	}
	if reminder.AutoReleaseAttempts != updates {
		t.Fatalf("auto-release attempts = %d, want %d", reminder.AutoReleaseAttempts, updates)
	}
}

func TestMemberStoreReleaseReminderLegacyWritersShareAtomicLock(t *testing.T) {
	for _, test := range []struct {
		name   string
		writer func(MemberStore) error
		check  func(*testing.T, ReleaseReminder)
	}{
		{
			name: "upsert",
			writer: func(store MemberStore) error {
				_, err := store.UpsertReleaseReminder(ReleaseReminder{
					ProfileName: "apple-usw2",
					HostID:      "h-original",
					Status:      ReleaseReminderStatusActive,
				})
				return err
			},
			check: func(t *testing.T, reminder ReleaseReminder) {
				if reminder.HostID != "h-original" {
					t.Fatalf("legacy upsert host ID = %q", reminder.HostID)
				}
			},
		},
		{
			name: "due status",
			writer: func(store MemberStore) error {
				_, err := store.MarkReleaseReminderDue("apple-usw2", "2026-07-02T08:05:00Z")
				return err
			},
			check: func(t *testing.T, reminder ReleaseReminder) {
				if reminder.Status != ReleaseReminderStatusDueNotified || reminder.LastNotifiedAt != "2026-07-02T08:05:00Z" {
					t.Fatalf("due status mutation = %+v", reminder)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
			if _, err := store.UpsertReleaseReminder(ReleaseReminder{
				ProfileName:        "apple-usw2",
				HostID:             "h-original",
				AutoReleaseEnabled: true,
				AutoReleaseAt:      "2026-07-02T08:00:00Z",
				AutoReleaseState:   ReleaseReminderAutoReleaseStateScheduled,
			}); err != nil {
				t.Fatalf("seed reminder: %v", err)
			}

			callbackEntered := make(chan struct{})
			releaseCallback := make(chan struct{})
			updateDone := make(chan error, 1)
			go func() {
				_, err := store.UpdateReleaseReminder("apple-usw2", func(reminder ReleaseReminder) (ReleaseReminder, error) {
					close(callbackEntered)
					<-releaseCallback
					reminder.AutoReleaseAttempts++
					return reminder, nil
				})
				updateDone <- err
			}()
			<-callbackEntered

			writerDone := make(chan error, 1)
			go func() { writerDone <- test.writer(store) }()
			select {
			case err := <-writerDone:
				close(releaseCallback)
				<-updateDone
				t.Fatalf("legacy writer bypassed reminder lock: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			close(releaseCallback)
			if err := <-updateDone; err != nil {
				t.Fatalf("automatic-release update: %v", err)
			}
			if err := <-writerDone; err != nil {
				t.Fatalf("legacy writer: %v", err)
			}
			reminder, _, err := store.ReleaseReminder("apple-usw2")
			if err != nil {
				t.Fatalf("load reminder: %v", err)
			}
			if !reminder.AutoReleaseEnabled || reminder.AutoReleaseAt != "2026-07-02T08:00:00Z" || reminder.AutoReleaseState != ReleaseReminderAutoReleaseStateScheduled || reminder.AutoReleaseAttempts != 1 {
				t.Fatalf("automatic-release state was overwritten: %+v", reminder)
			}
			test.check(t, reminder)
		})
	}
}

func TestMemberStoreUpdateReleaseReminderNormalizesCallbackOutput(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	if _, err := store.UpsertReleaseReminder(ReleaseReminder{ProfileName: "apple-usw2", CreatedAt: "created"}); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	got, err := store.UpdateReleaseReminder("apple-usw2", func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.ProfileName = "changed"
		reminder.CreatedAt = "changed"
		reminder.AppleEmail = " APPLE@EXAMPLE.COM "
		reminder.OwnerEmail = " OWNER@EXAMPLE.COM "
		reminder.LastExtendedByEmail = " ADMIN@EXAMPLE.COM "
		reminder.Status = ""
		return reminder, nil
	})
	if err != nil {
		t.Fatalf("update reminder: %v", err)
	}
	if got.ProfileName != "apple-usw2" || got.CreatedAt != "created" || got.AppleEmail != "apple@example.com" || got.OwnerEmail != "owner@example.com" || got.LastExtendedByEmail != "admin@example.com" || got.Status != ReleaseReminderStatusActive {
		t.Fatalf("normalized reminder = %+v", got)
	}
}

func TestMemberStoreUnrelatedWritersShareReminderMutationBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		writer func(MemberStore) error
		check  func(*testing.T, MemberData)
	}{
		{
			name: "record event",
			writer: func(store MemberStore) error {
				return store.RecordEvent(OperationEvent{ID: "event-race", Action: "test", Profile: "apple-usw2"})
			},
			check: func(t *testing.T, db MemberData) {
				if len(db.Events) != 1 || db.Events[0].ID != "event-race" {
					t.Fatalf("events after race = %+v", db.Events)
				}
			},
		},
		{
			name: "update settings",
			writer: func(store MemberStore) error {
				_, err := store.UpdateWebSettings(WebSettings{DefaultStatusFilter: "active"})
				return err
			},
			check: func(t *testing.T, db MemberData) {
				if db.Settings.DefaultStatusFilter != "active" {
					t.Fatalf("settings after race = %+v", db.Settings)
				}
			},
		},
		{
			name: "add member",
			writer: func(store MemberStore) error {
				_, err := store.AddMember("User", "user@example.com", "operator")
				return err
			},
			check: func(t *testing.T, db MemberData) {
				if len(db.Members) != 1 || db.Members[0].Email != "user@example.com" {
					t.Fatalf("members after race = %+v", db.Members)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
			if _, err := store.UpsertReleaseReminder(ReleaseReminder{
				ProfileName:        "apple-usw2",
				AutoReleaseEnabled: true,
				AutoReleaseState:   ReleaseReminderAutoReleaseStateScheduled,
			}); err != nil {
				t.Fatalf("seed reminder: %v", err)
			}
			store.mutationGuard = &storeMutationGuard{}
			storeCopy := NewMemberStore(store.Path)
			storeCopy.mutationGuard = &storeMutationGuard{}
			callbackEntered := make(chan struct{})
			releaseCallback := make(chan struct{})
			updateDone := make(chan error, 1)
			go func() {
				_, err := store.UpdateReleaseReminder("apple-usw2", func(reminder ReleaseReminder) (ReleaseReminder, error) {
					close(callbackEntered)
					<-releaseCallback
					reminder.AutoReleaseAttempts++
					return reminder, nil
				})
				updateDone <- err
			}()
			<-callbackEntered

			writerDone := make(chan error, 1)
			go func() { writerDone <- test.writer(storeCopy) }()
			select {
			case err := <-writerDone:
				close(releaseCallback)
				<-updateDone
				t.Fatalf("unrelated writer bypassed mutation boundary: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			close(releaseCallback)
			if err := <-updateDone; err != nil {
				t.Fatalf("automatic-release update: %v", err)
			}
			if err := <-writerDone; err != nil {
				t.Fatalf("unrelated writer: %v", err)
			}

			db, err := store.Load()
			if err != nil {
				t.Fatalf("load store: %v", err)
			}
			if len(db.Reminders) != 1 || !db.Reminders[0].AutoReleaseEnabled || db.Reminders[0].AutoReleaseState != ReleaseReminderAutoReleaseStateScheduled || db.Reminders[0].AutoReleaseAttempts != 1 {
				t.Fatalf("automatic-release state after unrelated writer = %+v", db.Reminders)
			}
			test.check(t, db)
		})
	}
}

func TestMemberStoreStaleSavePreservesCurrentReminders(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	if _, err := store.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:        "apple-usw2",
		AutoReleaseEnabled: true,
		AutoReleaseState:   ReleaseReminderAutoReleaseStateScheduled,
	}); err != nil {
		t.Fatalf("seed reminder: %v", err)
	}
	store.mutationGuard = &storeMutationGuard{}
	independentStore := NewMemberStore(store.Path)
	independentStore.mutationGuard = &storeMutationGuard{}
	stale, err := independentStore.Load()
	if err != nil {
		t.Fatalf("load stale snapshot: %v", err)
	}
	if _, err := store.UpdateReleaseReminder("apple-usw2", func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.AutoReleaseAttempts++
		return reminder, nil
	}); err != nil {
		t.Fatalf("update reminder: %v", err)
	}
	stale.Events = append(stale.Events, OperationEvent{ID: "event-stale-save"})
	if err := independentStore.Save(stale); err != nil {
		t.Fatalf("save stale snapshot: %v", err)
	}
	db, err := store.Load()
	if err != nil {
		t.Fatalf("load saved data: %v", err)
	}
	if len(db.Reminders) != 1 || db.Reminders[0].AutoReleaseAttempts != 1 || !db.Reminders[0].AutoReleaseEnabled {
		t.Fatalf("reminders after stale save = %+v", db.Reminders)
	}
	if len(db.Events) != 1 || db.Events[0].ID != "event-stale-save" {
		t.Fatalf("events after stale save = %+v", db.Events)
	}
}
