package connectmac

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

const mysqlOperationEventSelectColumns = `id, action, profile, COALESCE(apple_email, ''), COALESCE(member_id, ''), COALESCE(member_email, ''), COALESCE(member_name, ''), COALESCE(request_id, ''), COALESCE(job_id, ''), COALESCE(cycle_id, ''), attempt, COALESCE(session_id_hash, ''), COALESCE(source, ''), COALESCE(phase, ''), COALESCE(target_member_email, ''), COALESCE(error_code, ''), duration_ms, confirmed, status, COALESCE(message, ''), created_at`

const mysqlOperationEventInsertQuery = `INSERT INTO cm_events (id, action, profile, apple_email, member_id, member_email, member_name, request_id, job_id, cycle_id, attempt, session_id_hash, source, phase, target_member_email, error_code, duration_ms, confirmed, status, message, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const mysqlCanonicalEventTimestampPattern = `^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]\.[0-9]{9}Z$`

const mysqlOperationEventPruneBatchSize = 500

const mysqlCanonicalOperationEventPruneQuery = `DELETE FROM cm_events WHERE created_at < ? AND BINARY created_at REGEXP ? ORDER BY created_at, id LIMIT ?`

const mysqlNoncanonicalOperationEventSelectQuery = `SELECT id, created_at FROM cm_events WHERE BINARY created_at NOT REGEXP ? ORDER BY id LIMIT ? FOR UPDATE`

const mysqlOperationEventDeleteByIDQuery = `DELETE FROM cm_events WHERE id = ?`

const mysqlOperationEventCanonicalizeByIDQuery = `UPDATE cm_events SET created_at = ? WHERE id = ? AND created_at = ?`

func operationEventInsertArgs(event OperationEvent) []any {
	return []any{
		event.ID, event.Action, event.Profile, event.AppleEmail, event.MemberID,
		event.MemberEmail, event.MemberName, event.RequestID, event.JobID,
		event.CycleID, event.Attempt, event.SessionIDHash, event.Source, event.Phase, event.TargetMemberEmail, event.ErrorCode,
		event.DurationMS, event.Confirmed, event.Status, event.Message, event.CreatedAt,
	}
}

func scanMySQLOperationEvent(scanner mysqlReleaseReminderScanner, event *OperationEvent) error {
	return scanner.Scan(
		&event.ID, &event.Action, &event.Profile, &event.AppleEmail, &event.MemberID,
		&event.MemberEmail, &event.MemberName, &event.RequestID, &event.JobID,
		&event.CycleID, &event.Attempt, &event.SessionIDHash, &event.Source, &event.Phase, &event.TargetMemberEmail, &event.ErrorCode,
		&event.DurationMS, &event.Confirmed, &event.Status, &event.Message, &event.CreatedAt,
	)
}

func normalizeOperationEvent(event OperationEvent, now time.Time) (OperationEvent, error) {
	data := MemberData{}
	if err := appendOperationEvent(&data, event, now.Format(time.RFC3339)); err != nil {
		return OperationEvent{}, err
	}
	return data.Events[0], nil
}

func (s MySQLMemberStore) insertEvent(event OperationEvent) error {
	s = s.normalize()
	explicitID := strings.TrimSpace(event.ID) != ""
	event, err := normalizeOperationEvent(event, s.currentTime())
	if err != nil {
		return err
	}
	if err := s.EnsureSchema(); err != nil {
		return err
	}
	db, err := s.open()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(mysqlOperationEventInsertQuery, operationEventInsertArgs(event)...)
	var mysqlErr *mysqlDriver.MySQLError
	if explicitID && errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return nil
	}
	return err
}

func (s MySQLMemberStore) QueryEvents(query EventQuery) (EventPage, error) {
	s = s.normalize()
	query = normalizeEventQuery(query)
	cursor, err := decodeEventCursor(query.Cursor)
	if err != nil {
		return EventPage{}, err
	}
	if err := s.EnsureSchema(); err != nil {
		return EventPage{}, err
	}
	db, err := s.open()
	if err != nil {
		return EventPage{}, err
	}
	defer db.Close()

	conditions := make([]string, 0, 5)
	args := make([]any, 0, 8)
	if query.AppleEmail != "" {
		conditions = append(conditions, "apple_email = ?")
		args = append(args, query.AppleEmail)
	}
	if query.Profile != "" {
		conditions = append(conditions, "profile = ?")
		args = append(args, query.Profile)
	}
	if query.ActorEmail != "" {
		conditions = append(conditions, "member_email = ?")
		args = append(args, query.ActorEmail)
	}
	if !query.IncludeSystem {
		conditions = append(conditions, "LOWER(COALESCE(source, '')) <> 'system'")
	}
	if cursor.CreatedAt != "" {
		conditions = append(conditions, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	statement := `SELECT ` + mysqlOperationEventSelectColumns + ` FROM cm_events`
	if len(conditions) > 0 {
		statement += ` WHERE ` + strings.Join(conditions, " AND ")
	}
	statement += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, query.Limit+1)

	rows, err := db.Query(statement, args...)
	if err != nil {
		return EventPage{}, err
	}
	defer rows.Close()
	events := make([]OperationEvent, 0, query.Limit+1)
	for rows.Next() {
		var event OperationEvent
		if err := scanMySQLOperationEvent(rows, &event); err != nil {
			return EventPage{}, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, err
	}
	page := EventPage{Events: events}
	if len(page.Events) > query.Limit {
		page.Events = page.Events[:query.Limit]
		page.NextCursor = encodeEventCursor(page.Events[len(page.Events)-1])
	}
	return page, nil
}

func (s MySQLMemberStore) PruneEvents(before time.Time) (int64, error) {
	s = s.normalize()
	if before.IsZero() {
		return 0, fmt.Errorf("event prune cutoff is required")
	}
	if err := s.EnsureSchema(); err != nil {
		return 0, err
	}
	db, err := s.open()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	cutoff := before.UTC().Format(eventTimestampLayout)
	removed, err := pruneCanonicalMySQLEvents(db, cutoff)
	if err != nil {
		return 0, err
	}
	legacyRemoved, err := pruneLegacyMySQLEvents(db, before)
	if err != nil {
		return 0, err
	}
	return removed + legacyRemoved, nil
}

func pruneCanonicalMySQLEvents(db *sql.DB, cutoff string) (int64, error) {
	var removed int64
	for {
		tx, err := db.Begin()
		if err != nil {
			return removed, err
		}
		result, err := tx.Exec(
			mysqlCanonicalOperationEventPruneQuery,
			cutoff,
			mysqlCanonicalEventTimestampPattern,
			mysqlOperationEventPruneBatchSize,
		)
		if err != nil {
			_ = tx.Rollback()
			return removed, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return removed, err
		}
		if err := tx.Commit(); err != nil {
			return removed, err
		}
		removed += affected
		if affected < mysqlOperationEventPruneBatchSize {
			return removed, nil
		}
	}
}

func pruneLegacyMySQLEvents(db *sql.DB, before time.Time) (int64, error) {
	type legacyEventTimestamp struct {
		id        string
		createdAt string
	}
	var removed int64
	for {
		tx, err := db.Begin()
		if err != nil {
			return removed, err
		}
		rows, err := tx.Query(
			mysqlNoncanonicalOperationEventSelectQuery,
			mysqlCanonicalEventTimestampPattern,
			mysqlOperationEventPruneBatchSize,
		)
		if err != nil {
			_ = tx.Rollback()
			return removed, err
		}
		var candidates []legacyEventTimestamp
		for rows.Next() {
			var candidate legacyEventTimestamp
			if err := rows.Scan(&candidate.id, &candidate.createdAt); err != nil {
				rows.Close()
				_ = tx.Rollback()
				return removed, err
			}
			candidates = append(candidates, candidate)
		}
		iterationErr := rows.Err()
		closeErr := rows.Close()
		if iterationErr != nil {
			_ = tx.Rollback()
			return removed, iterationErr
		}
		if closeErr != nil {
			_ = tx.Rollback()
			return removed, closeErr
		}
		var batchRemoved int64
		for _, candidate := range candidates {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(candidate.createdAt))
			if parseErr != nil || createdAt.Before(before) {
				result, err := tx.Exec(mysqlOperationEventDeleteByIDQuery, candidate.id)
				if err != nil {
					_ = tx.Rollback()
					return removed, err
				}
				affected, err := result.RowsAffected()
				if err != nil {
					_ = tx.Rollback()
					return removed, err
				}
				batchRemoved += affected
				continue
			}
			if _, err := tx.Exec(
				mysqlOperationEventCanonicalizeByIDQuery,
				createdAt.UTC().Format(eventTimestampLayout),
				candidate.id,
				candidate.createdAt,
			); err != nil {
				_ = tx.Rollback()
				return removed, err
			}
		}
		if err := tx.Commit(); err != nil {
			return removed, err
		}
		removed += batchRemoved
		if len(candidates) < mysqlOperationEventPruneBatchSize {
			return removed, nil
		}
	}
}
