package connectmac

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMemberStoreQueryEventsRetentionAndCursorPagination(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time { return now }
	data := MemberData{Events: []OperationEvent{{
		ID: "expired", Action: "old", MemberEmail: "actor@example.com",
		Status: "success", CreatedAt: now.AddDate(0, 0, -91).Format(time.RFC3339),
	}}}
	for i := 0; i < eventFileMaxRows+25; i++ {
		data.Events = append(data.Events, OperationEvent{
			ID: fmt.Sprintf("event-%05d", i), Action: "test", Profile: "iossupport-usw2",
			AppleEmail: "apple@example.com", MemberEmail: "actor@example.com",
			Status: "success", CreatedAt: now.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
		})
	}
	if err := store.Save(data); err != nil {
		t.Fatalf("save events: %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	if len(loaded.Events) != eventFileMaxRows {
		t.Fatalf("retained events = %d, want %d", len(loaded.Events), eventFileMaxRows)
	}

	first, err := store.QueryEvents(EventQuery{AppleEmail: "apple@example.com", Limit: 50, IncludeSystem: true})
	if err != nil {
		t.Fatalf("query first page: %v", err)
	}
	if len(first.Events) != 50 || first.NextCursor == "" {
		t.Fatalf("first page len=%d cursor=%q", len(first.Events), first.NextCursor)
	}
	if first.Events[0].ID != fmt.Sprintf("event-%05d", eventFileMaxRows+24) {
		t.Fatalf("newest event = %q", first.Events[0].ID)
	}
	second, err := store.QueryEvents(EventQuery{
		AppleEmail: "apple@example.com", Limit: 50, Cursor: first.NextCursor, IncludeSystem: true,
	})
	if err != nil {
		t.Fatalf("query second page: %v", err)
	}
	seen := map[string]bool{}
	for _, event := range first.Events {
		seen[event.ID] = true
	}
	for _, event := range second.Events {
		if seen[event.ID] {
			t.Fatalf("duplicate event across pages: %s", event.ID)
		}
	}
}

func TestMemberStorePruneEvents(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time { return now }
	if err := store.Save(MemberData{Events: []OperationEvent{
		{ID: "old", Action: "test", Status: "success", CreatedAt: now.Add(-48 * time.Hour).Format(time.RFC3339)},
		{ID: "new", Action: "test", Status: "success", CreatedAt: now.Format(time.RFC3339)},
	}}); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	removed, err := store.PruneEvents(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("prune events: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	page, err := store.QueryEvents(EventQuery{Limit: 10, IncludeSystem: true})
	if err != nil || len(page.Events) != 1 || page.Events[0].ID != "new" {
		t.Fatalf("events after prune = %+v err=%v", page.Events, err)
	}
}

func TestMemberStoreRecordEventGeneratesCollisionSafeIDs(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time { return now }
	event := OperationEvent{
		Action: "aws.open.ready", Profile: "iossupport-usw2",
		AppleEmail: "apple@example.com", Status: "success",
	}
	if err := store.RecordEvent(event); err != nil {
		t.Fatalf("record first event: %v", err)
	}
	if err := store.RecordEvent(event); err != nil {
		t.Fatalf("record second event: %v", err)
	}
	page, err := store.QueryEvents(EventQuery{Limit: 10, IncludeSystem: true})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(page.Events))
	}
	if page.Events[0].ID == page.Events[1].ID {
		t.Fatalf("event IDs collided: %q", page.Events[0].ID)
	}
}

func TestMemberStoreRecordEventExplicitIDIsIdempotent(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	first := OperationEvent{
		ID: "event-idempotent", Action: "aws.open.ready", Profile: "iossupport-usw2",
		Status: "success", Message: "first",
	}
	if err := store.RecordEvent(first); err != nil {
		t.Fatalf("record first event: %v", err)
	}
	duplicate := first
	duplicate.Message = "duplicate must not replace first"
	if err := store.RecordEvent(duplicate); err != nil {
		t.Fatalf("record duplicate event: %v", err)
	}
	page, err := store.QueryEvents(EventQuery{Limit: 10, IncludeSystem: true})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].ID != first.ID || page.Events[0].Message != "first" {
		t.Fatalf("idempotent events = %+v", page.Events)
	}
}

func TestMemberStoreRecordEventCanonicalizesCreatedAt(t *testing.T) {
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	err := store.RecordEvent(OperationEvent{
		Action: "test", Status: "success", CreatedAt: "2026-07-30T20:30:45.123456+08:00",
	})
	if err != nil {
		t.Fatalf("record offset event: %v", err)
	}
	page, err := store.QueryEvents(EventQuery{Limit: 10, IncludeSystem: true})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if got, want := page.Events[0].CreatedAt, "2026-07-30T12:30:45.123456000Z"; got != want {
		t.Fatalf("created_at = %q, want %q", got, want)
	}
	if err := store.RecordEvent(OperationEvent{
		Action: "test", Status: "success", CreatedAt: "not-a-timestamp",
	}); err == nil {
		t.Fatal("invalid explicit created_at was accepted")
	}
}

func TestMemberStorePruneEventsCountsRetentionAndCapRemovals(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time { return now }
	data := MemberData{Events: make([]OperationEvent, 0, eventFileMaxRows+3)}
	data.Events = append(data.Events, OperationEvent{
		ID: "expired", Action: "test", Status: "success",
		CreatedAt: now.Add(-48 * time.Hour).Format(time.RFC3339),
	})
	for i := 0; i < eventFileMaxRows+2; i++ {
		data.Events = append(data.Events, OperationEvent{
			ID: fmt.Sprintf("recent-%05d", i), Action: "test", Status: "success",
			CreatedAt: now.Add(time.Duration(i) * time.Nanosecond).Format(time.RFC3339Nano),
		})
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(store.Path, raw, 0o600); err != nil {
		t.Fatalf("write legacy seed: %v", err)
	}
	removed, err := store.PruneEvents(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("prune events: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("load pruned events: %v", err)
	}
	if len(loaded.Events) != eventFileMaxRows {
		t.Fatalf("events after prune = %d, want %d", len(loaded.Events), eventFileMaxRows)
	}
}

func TestMemberStorePaginationWhileConcurrentNewerEventsAppend(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time { return now }
	const baselineCount = 100
	for i := 0; i < baselineCount; i++ {
		if err := store.RecordEvent(OperationEvent{
			ID: fmt.Sprintf("baseline-%03d", i), Action: "baseline.test",
			Profile: "iossupport-usw2", Status: "success",
			CreatedAt: now.Add(-time.Duration(baselineCount-i) * time.Second).Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatalf("record baseline event: %v", err)
		}
	}

	first, err := store.QueryEvents(EventQuery{Limit: 17, IncludeSystem: true})
	if err != nil {
		t.Fatalf("query first page: %v", err)
	}
	seen := make(map[string]bool, baselineCount)
	for _, event := range first.Events {
		seen[event.ID] = true
	}

	const appendedCount = 50
	var wg sync.WaitGroup
	errs := make(chan error, appendedCount)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < appendedCount; i++ {
			errs <- store.RecordEvent(OperationEvent{
				Action: "concurrent.newer", Profile: "iossupport-usw2", Status: "success",
				CreatedAt: now.Add(time.Duration(i+1) * time.Second).Format(time.RFC3339Nano),
			})
		}
		close(errs)
	}()

	cursor := first.NextCursor
	for {
		page, err := store.QueryEvents(EventQuery{Limit: 17, Cursor: cursor, IncludeSystem: true})
		if err != nil {
			t.Fatalf("query page: %v", err)
		}
		for _, event := range page.Events {
			if seen[event.ID] {
				t.Fatalf("duplicate paginated event %q", event.ID)
			}
			seen[event.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	wg.Wait()
	for err := range errs {
		if err != nil {
			t.Fatalf("append concurrent event: %v", err)
		}
	}
	for i := 0; i < baselineCount; i++ {
		id := fmt.Sprintf("baseline-%03d", i)
		if !seen[id] {
			t.Fatalf("baseline event missing during concurrent pagination: %s", id)
		}
	}
}

func TestRetainFileEventsDropsMalformedLegacyTimestamp(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	malformed := OperationEvent{
		ID: "legacy-malformed", Action: "legacy", Status: "success", CreatedAt: "not-rfc3339",
	}
	retained := retainFileEvents([]OperationEvent{malformed}, now)
	if len(retained) != 0 {
		t.Fatalf("malformed legacy event retained: %+v", retained)
	}
}

func TestFixedPrecisionEventTimestampsSortAndPruneBySubsecond(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 5, 100000000, time.UTC)
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time { return now }
	if err := store.RecordEvent(OperationEvent{
		ID: "whole-second", Action: "test", Status: "success",
		CreatedAt: "2026-07-30T12:00:05Z",
	}); err != nil {
		t.Fatalf("record whole-second event: %v", err)
	}
	if err := store.RecordEvent(OperationEvent{
		ID: "subsecond", Action: "test", Status: "success",
		CreatedAt: "2026-07-30T12:00:05.1Z",
	}); err != nil {
		t.Fatalf("record subsecond event: %v", err)
	}
	page, err := store.QueryEvents(EventQuery{Limit: 10, IncludeSystem: true})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(page.Events) != 2 || page.Events[0].ID != "subsecond" {
		t.Fatalf("subsecond ordering = %+v", page.Events)
	}
	if page.Events[0].CreatedAt != "2026-07-30T12:00:05.100000000Z" ||
		page.Events[1].CreatedAt != "2026-07-30T12:00:05.000000000Z" {
		t.Fatalf("timestamps are not fixed precision: %+v", page.Events)
	}
	removed, err := store.PruneEvents(now)
	if err != nil {
		t.Fatalf("prune events: %v", err)
	}
	if removed != 1 {
		t.Fatalf("subsecond prune removed = %d, want 1", removed)
	}
}

func TestMemberStorePruneCountsMalformedLegacyTimestamp(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	store := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	store.Now = func() time.Time { return now }
	raw, err := json.Marshal(MemberData{Events: []OperationEvent{
		{ID: "malformed", Action: "legacy", Status: "success", CreatedAt: "invalid"},
		{ID: "valid", Action: "test", Status: "success", CreatedAt: now.Format(time.RFC3339)},
	}})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(store.Path, raw, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	removed, err := store.PruneEvents(now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("prune events: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want malformed row count 1", removed)
	}
}
