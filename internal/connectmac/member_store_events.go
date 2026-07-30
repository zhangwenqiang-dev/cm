package connectmac

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const (
	eventRetentionDays   = 90
	eventFileMaxRows     = 10000
	defaultEventLimit    = 50
	maxEventLimit        = 500
	eventTimestampLayout = "2006-01-02T15:04:05.000000000Z"
)

type EventQuery struct {
	AppleEmail    string
	Profile       string
	ActorEmail    string
	Limit         int
	Cursor        string
	IncludeSystem bool
}

type EventPage struct {
	Events     []OperationEvent `json:"events"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type eventCursor struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func canonicalEventTimestamp(value string) (string, error) {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	return parsed.UTC().Format(eventTimestampLayout), nil
}

func normalizeEventQuery(query EventQuery) EventQuery {
	query.AppleEmail = normalizeEmail(query.AppleEmail)
	query.Profile = strings.TrimSpace(query.Profile)
	query.ActorEmail = normalizeEmail(query.ActorEmail)
	if query.Limit <= 0 {
		query.Limit = defaultEventLimit
	}
	if query.Limit > maxEventLimit {
		query.Limit = maxEventLimit
	}
	return query
}

func encodeEventCursor(event OperationEvent) string {
	raw, _ := json.Marshal(eventCursor{CreatedAt: event.CreatedAt, ID: event.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeEventCursor(value string) (eventCursor, error) {
	if strings.TrimSpace(value) == "" {
		return eventCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return eventCursor{}, errors.New("invalid event cursor")
	}
	var cursor eventCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.CreatedAt == "" || cursor.ID == "" {
		return eventCursor{}, errors.New("invalid event cursor")
	}
	return cursor, nil
}

func eventBeforeCursor(event OperationEvent, cursor eventCursor) bool {
	if cursor.CreatedAt == "" {
		return true
	}
	return compareEventPosition(event, OperationEvent{CreatedAt: cursor.CreatedAt, ID: cursor.ID}) > 0
}

func eventMatchesQuery(event OperationEvent, query EventQuery) bool {
	if query.AppleEmail != "" && !strings.EqualFold(event.AppleEmail, query.AppleEmail) {
		return false
	}
	if query.Profile != "" && event.Profile != query.Profile {
		return false
	}
	if query.ActorEmail != "" && !strings.EqualFold(event.MemberEmail, query.ActorEmail) {
		return false
	}
	if !query.IncludeSystem && strings.EqualFold(strings.TrimSpace(event.Source), "system") {
		return false
	}
	return true
}

func sortEventsNewest(events []OperationEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		return compareEventPosition(events[i], events[j]) < 0
	})
}

func compareEventPosition(left, right OperationEvent) int {
	leftTime, leftErr := time.Parse(time.RFC3339Nano, left.CreatedAt)
	rightTime, rightErr := time.Parse(time.RFC3339Nano, right.CreatedAt)
	switch {
	case leftErr == nil && rightErr != nil:
		return -1
	case leftErr != nil && rightErr == nil:
		return 1
	case leftErr == nil && rightErr == nil && !leftTime.Equal(rightTime):
		if leftTime.After(rightTime) {
			return -1
		}
		return 1
	case left.CreatedAt != right.CreatedAt:
		if left.CreatedAt > right.CreatedAt {
			return -1
		}
		return 1
	case left.ID > right.ID:
		return -1
	case left.ID < right.ID:
		return 1
	default:
		return 0
	}
}

func retainFileEvents(events []OperationEvent, now time.Time) []OperationEvent {
	return retainFileEventsWithPolicy(events, now, true)
}

func retainFileEventsForSave(events []OperationEvent, now time.Time) []OperationEvent {
	return retainFileEventsWithPolicy(events, now, false)
}

func retainFileEventsWithPolicy(events []OperationEvent, now time.Time, dropMalformed bool) []OperationEvent {
	cutoff := now.AddDate(0, 0, -eventRetentionDays)
	retained := make([]OperationEvent, 0, len(events))
	for _, event := range events {
		createdAt, err := time.Parse(time.RFC3339Nano, event.CreatedAt)
		if err != nil {
			if !dropMalformed {
				retained = append(retained, event)
			}
			continue
		}
		if createdAt.Before(cutoff) {
			continue
		}
		retained = append(retained, event)
	}
	sortEventsNewest(retained)
	if len(retained) > eventFileMaxRows {
		retained = retained[:eventFileMaxRows]
	}
	sort.SliceStable(retained, func(i, j int) bool {
		return compareEventPosition(retained[i], retained[j]) > 0
	})
	return retained
}

func mergeOperationEvents(current, incoming []OperationEvent) []OperationEvent {
	merged := make([]OperationEvent, 0, len(current)+len(incoming))
	seen := make(map[string]struct{}, len(current)+len(incoming))
	for _, events := range [][]OperationEvent{current, incoming} {
		for _, event := range events {
			key := event.ID
			if key == "" {
				key = event.CreatedAt + "\x00" + event.Action + "\x00" + event.Profile
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, event)
		}
	}
	return merged
}

func (s MemberStore) QueryEvents(query EventQuery) (EventPage, error) {
	query = normalizeEventQuery(query)
	cursor, err := decodeEventCursor(query.Cursor)
	if err != nil {
		return EventPage{}, err
	}
	db, err := s.Load()
	if err != nil {
		return EventPage{}, err
	}
	events := append([]OperationEvent(nil), db.Events...)
	sortEventsNewest(events)
	page := EventPage{Events: make([]OperationEvent, 0, query.Limit)}
	for _, event := range events {
		if !eventBeforeCursor(event, cursor) || !eventMatchesQuery(event, query) {
			continue
		}
		if len(page.Events) == query.Limit {
			page.NextCursor = encodeEventCursor(page.Events[len(page.Events)-1])
			break
		}
		page.Events = append(page.Events, event)
	}
	return page, nil
}

func (s MemberStore) PruneEvents(before time.Time) (int64, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return 0, err
	}
	defer unlock()
	db, err := s.Load()
	if err != nil {
		return 0, err
	}
	originalCount := len(db.Events)
	kept := make([]OperationEvent, 0, originalCount)
	for _, event := range db.Events {
		createdAt, parseErr := time.Parse(time.RFC3339Nano, event.CreatedAt)
		if parseErr == nil && createdAt.Before(before) {
			continue
		}
		kept = append(kept, event)
	}
	kept = retainFileEvents(kept, s.normalize().Now())
	removed := int64(originalCount - len(kept))
	if removed == 0 {
		return 0, nil
	}
	db.Events = kept
	return removed, s.saveUnlocked(db)
}
