package connectmac

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemberStoreTransferCreateListUpdateDelete(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }

	created, err := store.CreateTransferRecord("member-a", TransferRecord{
		MemberID:    "member-a",
		MemberEmail: "a@example.com",
		ProfileName: "iossupport-usw2",
		Direction:   TransferDirectionPush,
		LocalPath:   "/tmp/App",
		RemotePath:  "~/Documents/",
		Status:      TransferStatusCreated,
		Phase:       TransferPhasePreparing,
	})
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	if !strings.HasPrefix(created.ID, "transfer-") || created.CreatedAt != now.Format(time.RFC3339) || created.UpdatedAt != created.CreatedAt {
		t.Fatalf("created transfer = %+v", created)
	}

	second, err := store.CreateTransferRecord("member-a", TransferRecord{
		MemberID:    "member-a",
		MemberEmail: "a@example.com",
		ProfileName: "other-profile",
		Direction:   TransferDirectionPull,
		LocalPath:   "/tmp/Download",
		RemotePath:  "~/Downloads/",
		Status:      TransferStatusQueued,
		Phase:       TransferPhasePreparing,
	})
	if err != nil {
		t.Fatalf("create second transfer: %v", err)
	}
	if second.ID == created.ID {
		t.Fatalf("transfer IDs must be unique: %q", created.ID)
	}

	records, err := store.ListTransferRecords("member-a", "iossupport-usw2")
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(records) != 1 || records[0].ID != created.ID {
		t.Fatalf("records = %+v", records)
	}

	now = now.Add(time.Minute)
	updated, err := store.UpdateTransferRecord("member-a", created.ID, "local-job-1", func(current TransferRecord) (TransferRecord, error) {
		current.LocalJobID = "local-job-1"
		current.Status = TransferStatusRunning
		current.Percent = 25
		current.Phase = TransferPhaseTransferring
		current.StartedAt = now.Format(time.RFC3339)
		return current, nil
	})
	if err != nil {
		t.Fatalf("update transfer: %v", err)
	}
	if updated.Status != TransferStatusRunning || updated.Phase != TransferPhaseTransferring || updated.Percent != 25 || updated.UpdatedAt != now.Format(time.RFC3339) {
		t.Fatalf("updated transfer = %+v", updated)
	}

	if err := store.DeleteTransferRecord("member-a", created.ID); err != nil {
		t.Fatalf("delete transfer: %v", err)
	}
	records, err = store.ListTransferRecords("member-a", "")
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(records) != 1 || records[0].ID != second.ID {
		t.Fatalf("records after delete = %+v", records)
	}
}

func TestMemberStoreTransferRejectsPercentRegressionAndTerminalReactivation(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	record := mustCreateTransferRecord(t, store, "member-a", "a@example.com")

	record, err := store.UpdateTransferRecord("member-a", record.ID, "job-a", func(current TransferRecord) (TransferRecord, error) {
		current.LocalJobID = "job-a"
		current.Status = TransferStatusRunning
		current.Phase = TransferPhaseTransferring
		current.Percent = 75
		return current, nil
	})
	if err != nil {
		t.Fatalf("set running transfer: %v", err)
	}
	if _, err := store.UpdateTransferRecord("member-a", record.ID, "job-a", func(current TransferRecord) (TransferRecord, error) {
		current.Percent = 50
		return current, nil
	}); err == nil {
		t.Fatal("expected percent regression to fail")
	}

	record, err = store.UpdateTransferRecord("member-a", record.ID, "job-a", func(current TransferRecord) (TransferRecord, error) {
		current.Status = TransferStatusFailed
		current.Phase = TransferPhaseFailed
		current.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		return current, nil
	})
	if err != nil {
		t.Fatalf("finish transfer: %v", err)
	}
	if _, err := store.UpdateTransferRecord("member-a", record.ID, "job-a", func(current TransferRecord) (TransferRecord, error) {
		current.Status = TransferStatusInterrupted
		current.Percent = 99
		current.ErrorSummary = "changed after terminal"
		return current, nil
	}); err == nil {
		t.Fatal("expected terminal transfer mutation to fail")
	}
}

func TestMemberStoreTransferTerminalUpdateIsIdempotent(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	record := mustCreateTransferRecord(t, store, "member-a", "a@example.com")
	finishedAt := "2026-07-16T08:03:00Z"
	record, err := store.UpdateTransferRecord("member-a", record.ID, "job-a", func(current TransferRecord) (TransferRecord, error) {
		current.LocalJobID = "job-a"
		current.Status = TransferStatusFailed
		current.Phase = TransferPhaseFailed
		current.Percent = 48
		current.ErrorSummary = "exit status 23"
		current.StartedAt = "2026-07-16T08:01:00Z"
		current.FinishedAt = finishedAt
		return current, nil
	})
	if err != nil {
		t.Fatalf("finish transfer: %v", err)
	}

	retried, err := store.UpdateTransferRecord("member-a", record.ID, "job-a", func(current TransferRecord) (TransferRecord, error) {
		current.Status = TransferStatusFailed
		current.Phase = TransferPhaseFailed
		current.Percent = 48
		current.ErrorSummary = "exit status 23"
		current.StartedAt = "2026-07-16T08:01:00+00:00"
		current.FinishedAt = "2026-07-16T16:03:00+08:00"
		return current, nil
	})
	if err != nil {
		t.Fatalf("retry identical terminal transfer: %v", err)
	}
	if retried.ID != record.ID || retried.UpdatedAt != record.UpdatedAt || retried.FinishedAt != finishedAt {
		t.Fatalf("retried transfer = %+v, want unchanged %+v", retried, record)
	}

	for _, test := range []struct {
		name   string
		change func(*TransferRecord)
		want   string
	}{
		{name: "status", change: func(record *TransferRecord) {
			record.Status = TransferStatusInterrupted
			record.Phase = TransferPhaseInterrupted
		}, want: "terminal transfer record cannot be updated"},
		{name: "phase", change: func(record *TransferRecord) { record.Phase = TransferPhaseInterrupted }, want: "invalid transfer status/phase/percent combination"},
		{name: "percent", change: func(record *TransferRecord) { record.Percent = 49 }, want: "terminal transfer record cannot be updated"},
		{name: "error", change: func(record *TransferRecord) { record.ErrorSummary = "different error" }, want: "terminal transfer record cannot be updated"},
		{name: "started timestamp", change: func(record *TransferRecord) { record.StartedAt = "2026-07-16T08:02:00Z" }, want: "terminal transfer record cannot be updated"},
		{name: "finished timestamp", change: func(record *TransferRecord) { record.FinishedAt = "2026-07-16T08:04:00Z" }, want: "terminal transfer record cannot be updated"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.UpdateTransferRecord("member-a", record.ID, "job-a", func(current TransferRecord) (TransferRecord, error) {
				test.change(&current)
				return current, nil
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("conflicting terminal update error = %v", err)
			}
		})
	}
}

func TestMemberStoreTransferValidatesPhaseIndependently(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	record := mustCreateTransferRecord(t, store, "member-a", "a@example.com")

	updated, err := store.UpdateTransferRecord("member-a", record.ID, "job-a", func(current TransferRecord) (TransferRecord, error) {
		current.LocalJobID = "job-a"
		current.Status = TransferStatusRunning
		current.Phase = TransferPhaseFinalizing
		current.Percent = 99
		return current, nil
	})
	if err != nil {
		t.Fatalf("set independent phase: %v", err)
	}
	if updated.Phase != TransferPhaseFinalizing {
		t.Fatalf("phase = %q", updated.Phase)
	}

	if _, err := store.UpdateTransferRecord("member-a", record.ID, "job-a", func(current TransferRecord) (TransferRecord, error) {
		current.Phase = "almost-done"
		return current, nil
	}); err == nil || !strings.Contains(err.Error(), "invalid transfer phase") {
		t.Fatalf("invalid phase error = %v", err)
	}
}

func TestMemberStoreTransferValidatesStatusPhasePercentCombinations(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		phase   string
		percent int
		valid   bool
	}{
		{name: "created preparing zero", status: TransferStatusCreated, phase: TransferPhasePreparing, percent: 0, valid: true},
		{name: "queued preparing zero", status: TransferStatusQueued, phase: TransferPhasePreparing, percent: 0, valid: true},
		{name: "running preparing zero", status: TransferStatusRunning, phase: TransferPhasePreparing, percent: 0, valid: true},
		{name: "running transferring one", status: TransferStatusRunning, phase: TransferPhaseTransferring, percent: 1, valid: true},
		{name: "running transferring ninety five", status: TransferStatusRunning, phase: TransferPhaseTransferring, percent: 95, valid: true},
		{name: "running transferring ninety eight", status: TransferStatusRunning, phase: TransferPhaseTransferring, percent: 98, valid: true},
		{name: "running finalizing ninety nine", status: TransferStatusRunning, phase: TransferPhaseFinalizing, percent: 99, valid: true},
		{name: "succeeded exact", status: TransferStatusSucceeded, phase: TransferPhaseSucceeded, percent: 100, valid: true},
		{name: "failed preserves progress", status: TransferStatusFailed, phase: TransferPhaseFailed, percent: 48, valid: true},
		{name: "interrupted preserves progress", status: TransferStatusInterrupted, phase: TransferPhaseInterrupted, percent: 48, valid: true},
		{name: "succeeded at ninety nine", status: TransferStatusSucceeded, phase: TransferPhaseSucceeded, percent: 99},
		{name: "succeeded wrong phase", status: TransferStatusSucceeded, phase: TransferPhaseFinalizing, percent: 100},
		{name: "failed at one hundred", status: TransferStatusFailed, phase: TransferPhaseFailed, percent: 100},
		{name: "failed wrong phase", status: TransferStatusFailed, phase: TransferPhaseInterrupted, percent: 48},
		{name: "interrupted at one hundred", status: TransferStatusInterrupted, phase: TransferPhaseInterrupted, percent: 100},
		{name: "queued transferring", status: TransferStatusQueued, phase: TransferPhaseTransferring, percent: 1},
		{name: "running preparing with progress", status: TransferStatusRunning, phase: TransferPhasePreparing, percent: 1},
		{name: "running transferring zero", status: TransferStatusRunning, phase: TransferPhaseTransferring, percent: 0},
		{name: "running transferring ninety nine", status: TransferStatusRunning, phase: TransferPhaseTransferring, percent: 99},
		{name: "running finalizing ninety five", status: TransferStatusRunning, phase: TransferPhaseFinalizing, percent: 95},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateTransferRecord(TransferRecord{
				Direction: TransferDirectionPush,
				Status:    test.status,
				Phase:     test.phase,
				Percent:   test.percent,
			})
			if test.valid && err != nil {
				t.Fatalf("valid combination rejected: %v", err)
			}
			if !test.valid && (err == nil || !strings.Contains(err.Error(), "invalid transfer status/phase/percent combination")) {
				t.Fatalf("invalid combination error = %v", err)
			}
		})
	}
}

func TestMySQLTransferPhaseMigrationIsAdditive(t *testing.T) {
	if len(mysqlTransferMigrationStatements) != 1 ||
		!strings.Contains(mysqlTransferMigrationStatements[0], "ADD COLUMN phase") {
		t.Fatalf("transfer migrations = %#v", mysqlTransferMigrationStatements)
	}
	if !strings.Contains(mysqlTransferInsertWithPhaseQuery, "phase") ||
		!strings.Contains(mysqlTransferPhaseUpdateQuery, "phase = ?") {
		t.Fatalf("phase persistence queries missing: insert=%q update=%q",
			mysqlTransferInsertWithPhaseQuery, mysqlTransferPhaseUpdateQuery)
	}
}

func TestMemberStoreTransferSurvivesStaleWholeStoreSave(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	stale, err := store.Load()
	if err != nil {
		t.Fatalf("load stale snapshot: %v", err)
	}
	created := mustCreateTransferRecord(t, store, "member-a", "a@example.com")

	stale.Settings.DefaultStatusFilter = "ready"
	if err := store.Save(stale); err != nil {
		t.Fatalf("save stale snapshot: %v", err)
	}

	records, err := store.ListTransferRecords("member-a", "")
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(records) != 1 || records[0].ID != created.ID {
		t.Fatalf("transfer record lost after stale save: %+v", records)
	}
}

func TestMemberStoreTransferTwoMemberIsolation(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	first := mustCreateTransferRecord(t, store, "member-a", "a@example.com")
	second := mustCreateTransferRecord(t, store, "member-b", "b@example.com")

	records, err := store.ListTransferRecords("member-a", "")
	if err != nil {
		t.Fatalf("list member-a: %v", err)
	}
	if len(records) != 1 || records[0].ID != first.ID {
		t.Fatalf("member-a records = %+v", records)
	}
	if _, err := store.UpdateTransferRecord("member-b", first.ID, "", func(current TransferRecord) (TransferRecord, error) {
		current.Status = TransferStatusRunning
		return current, nil
	}); err == nil {
		t.Fatal("expected cross-member update to fail")
	}
	if err := store.DeleteTransferRecord("member-b", first.ID); err == nil {
		t.Fatal("expected cross-member delete to fail")
	}
	if _, err := store.UpdateTransferRecord("member-a", first.ID, "wrong-job", func(current TransferRecord) (TransferRecord, error) {
		current.LocalJobID = "different-job"
		return current, nil
	}); err == nil {
		t.Fatal("expected local job ID mismatch to fail")
	}
	if err := store.DeleteTransferRecord("member-b", second.ID); err != nil {
		t.Fatalf("delete member-b transfer: %v", err)
	}
}

func mustCreateTransferRecord(t *testing.T, store MemberStore, memberID, memberEmail string) TransferRecord {
	t.Helper()
	record, err := store.CreateTransferRecord(memberID, TransferRecord{
		MemberID:    memberID,
		MemberEmail: memberEmail,
		ProfileName: "iossupport-usw2",
		Direction:   TransferDirectionPush,
		LocalPath:   "/tmp/App",
		RemotePath:  "~/Documents/",
		Status:      TransferStatusCreated,
		Phase:       TransferPhasePreparing,
	})
	if err != nil {
		t.Fatalf("create transfer for %s: %v", memberID, err)
	}
	return record
}
