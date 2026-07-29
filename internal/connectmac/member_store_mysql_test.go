package connectmac

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

type fakeMySQLReleaseReminderRow struct {
	reminder ReleaseReminder
	err      error
}

type fakeMySQLStoreLockRow struct {
	version uint64
	err     error
}

func (r fakeMySQLStoreLockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return errors.New("unexpected store lock scan destination count")
	}
	*dest[0].(*uint64) = r.version
	return nil
}

type fakeMySQLReleaseReminderRows struct {
	reminders []ReleaseReminder
	index     int
	err       error
	closeErr  error
}

func (r *fakeMySQLReleaseReminderRows) Next() bool {
	return r.index < len(r.reminders)
}

func (r *fakeMySQLReleaseReminderRows) Scan(dest ...any) error {
	if r.index >= len(r.reminders) {
		return errors.New("scan past release reminder rows")
	}
	err := (fakeMySQLReleaseReminderRow{reminder: r.reminders[r.index]}).Scan(dest...)
	r.index++
	return err
}

func (r *fakeMySQLReleaseReminderRows) Close() error {
	return r.closeErr
}

func (r *fakeMySQLReleaseReminderRows) Err() error {
	return r.err
}

type fakeMySQLTransferRows struct {
	records  []TransferRecord
	index    int
	err      error
	closeErr error
}

func (r *fakeMySQLTransferRows) Next() bool {
	return r.index < len(r.records)
}

func (r *fakeMySQLTransferRows) Scan(dest ...any) error {
	if r.index >= len(r.records) {
		return errors.New("scan past transfer rows")
	}
	record := r.records[r.index]
	r.index++
	if len(dest) != len(mysqlTransferColumnNames) {
		return errors.New("unexpected transfer scan destination count")
	}
	*dest[0].(*string) = record.ID
	*dest[1].(*string) = record.MemberID
	*dest[2].(*string) = record.MemberEmail
	*dest[3].(*string) = record.ProfileName
	*dest[4].(*string) = record.AppleEmail
	*dest[5].(*string) = record.Direction
	*dest[6].(*string) = record.LocalPath
	*dest[7].(*string) = record.RemotePath
	*dest[8].(*string) = record.LocalJobID
	*dest[9].(*string) = record.Status
	*dest[10].(*int) = record.Percent
	*dest[11].(*string) = record.ErrorSummary
	*dest[12].(*string) = record.CreatedAt
	*dest[13].(*string) = record.StartedAt
	*dest[14].(*string) = record.FinishedAt
	*dest[15].(*string) = record.UpdatedAt
	return nil
}

func (r *fakeMySQLTransferRows) Close() error {
	return r.closeErr
}

func (r *fakeMySQLTransferRows) Err() error {
	return r.err
}

func (r fakeMySQLReleaseReminderRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 22 {
		return errors.New("unexpected release reminder scan destination count")
	}
	*dest[0].(*string) = r.reminder.ProfileName
	*dest[1].(*string) = r.reminder.AppleEmail
	*dest[2].(*string) = r.reminder.HostID
	*dest[3].(*string) = r.reminder.HostCreatedAt
	*dest[4].(*string) = r.reminder.ReleaseDueAt
	*dest[5].(*string) = r.reminder.OwnerEmail
	*dest[6].(*string) = r.reminder.OwnerName
	*dest[7].(*string) = r.reminder.LastExtendedByEmail
	*dest[8].(*string) = r.reminder.LastExtendedByName
	*dest[9].(*string) = r.reminder.LastExtendedAt
	*dest[10].(*string) = r.reminder.LastNotifiedAt
	*dest[11].(*string) = r.reminder.ReleasedAt
	*dest[12].(*string) = r.reminder.Status
	*dest[13].(*bool) = r.reminder.AutoReleaseEnabled
	*dest[14].(*string) = r.reminder.AutoReleaseAt
	*dest[15].(*string) = r.reminder.AutoReleaseStartedAt
	*dest[16].(*string) = r.reminder.AutoReleaseLastAttemptAt
	*dest[17].(*int) = r.reminder.AutoReleaseAttempts
	*dest[18].(*string) = r.reminder.AutoReleaseLastError
	*dest[19].(*string) = r.reminder.AutoReleaseState
	*dest[20].(*string) = r.reminder.CreatedAt
	*dest[21].(*string) = r.reminder.UpdatedAt
	return nil
}

type fakeMySQLReleaseReminderTransaction struct {
	row          mysqlReleaseReminderScanner
	ownerRow     mysqlReleaseReminderScanner
	query        string
	queryArgs    []any
	execQuery    string
	execArgs     []any
	written      ReleaseReminder
	execErr      error
	eventExecErr error
	commitErr    error
	rollbackErr  error
	committed    bool
	rolledBack   bool
	lockVersion  uint64
	queryRows    []ReleaseReminder
	queryRowsErr error
	transferRows []TransferRecord
	operations   []string
	ownerDeleted bool
}

func (tx *fakeMySQLReleaseReminderTransaction) QueryRow(query string, args ...any) mysqlReleaseReminderScanner {
	tx.operations = append(tx.operations, "query-row:"+query)
	if query == mysqlStoreLockForUpdateQuery {
		return fakeMySQLStoreLockRow{version: tx.lockVersion}
	}
	if query == mysqlProfileOwnerForUpdateQuery {
		return tx.ownerRow
	}
	tx.query = query
	tx.queryArgs = args
	return tx.row
}

func (tx *fakeMySQLReleaseReminderTransaction) Query(query string, args ...any) (mysqlRows, error) {
	tx.operations = append(tx.operations, "query:"+query)
	if strings.Contains(query, "FROM cm_transfer_records") {
		return &fakeMySQLTransferRows{records: tx.transferRows}, nil
	}
	return &fakeMySQLReleaseReminderRows{reminders: tx.queryRows, err: tx.queryRowsErr}, nil
}

func (tx *fakeMySQLReleaseReminderTransaction) Exec(query string, args ...any) error {
	tx.operations = append(tx.operations, "exec:"+query)
	if query == mysqlOperationEventInsertQuery && tx.eventExecErr != nil {
		return tx.eventExecErr
	}
	if query == mysqlReleaseReminderInsertQuery || query == mysqlReleaseReminderUpdateQuery {
		tx.execQuery = query
		tx.execArgs = args
	}
	if query == mysqlDeleteMatchingProfileOwnerQuery {
		tx.ownerDeleted = true
	}
	if tx.execErr == nil && len(args) == 22 {
		tx.written = ReleaseReminder{
			ProfileName:              args[0].(string),
			AppleEmail:               args[1].(string),
			HostID:                   args[2].(string),
			HostCreatedAt:            args[3].(string),
			ReleaseDueAt:             args[4].(string),
			OwnerEmail:               args[5].(string),
			OwnerName:                args[6].(string),
			LastExtendedByEmail:      args[7].(string),
			LastExtendedByName:       args[8].(string),
			LastExtendedAt:           args[9].(string),
			LastNotifiedAt:           args[10].(string),
			ReleasedAt:               args[11].(string),
			Status:                   args[12].(string),
			AutoReleaseEnabled:       args[13].(bool),
			AutoReleaseAt:            args[14].(string),
			AutoReleaseStartedAt:     args[15].(string),
			AutoReleaseLastAttemptAt: args[16].(string),
			AutoReleaseAttempts:      args[17].(int),
			AutoReleaseLastError:     args[18].(string),
			AutoReleaseState:         args[19].(string),
			CreatedAt:                args[20].(string),
			UpdatedAt:                args[21].(string),
		}
	} else if tx.execErr == nil && len(args) == 21 {
		tx.written = ReleaseReminder{
			ProfileName:              args[20].(string),
			AppleEmail:               args[0].(string),
			HostID:                   args[1].(string),
			HostCreatedAt:            args[2].(string),
			ReleaseDueAt:             args[3].(string),
			OwnerEmail:               args[4].(string),
			OwnerName:                args[5].(string),
			LastExtendedByEmail:      args[6].(string),
			LastExtendedByName:       args[7].(string),
			LastExtendedAt:           args[8].(string),
			LastNotifiedAt:           args[9].(string),
			ReleasedAt:               args[10].(string),
			Status:                   args[11].(string),
			AutoReleaseEnabled:       args[12].(bool),
			AutoReleaseAt:            args[13].(string),
			AutoReleaseStartedAt:     args[14].(string),
			AutoReleaseLastAttemptAt: args[15].(string),
			AutoReleaseAttempts:      args[16].(int),
			AutoReleaseLastError:     args[17].(string),
			AutoReleaseState:         args[18].(string),
			UpdatedAt:                args[19].(string),
		}
		if row, ok := tx.row.(fakeMySQLReleaseReminderRow); ok {
			tx.written.CreatedAt = row.reminder.CreatedAt
		}
	}
	return tx.execErr
}

func (tx *fakeMySQLReleaseReminderTransaction) Commit() error {
	tx.committed = true
	return tx.commitErr
}

func (tx *fakeMySQLReleaseReminderTransaction) Rollback() error {
	tx.rolledBack = true
	return tx.rollbackErr
}

func TestMySQLReleaseReminderAutoReleaseSchemaMigrations(t *testing.T) {
	wantColumns := []string{
		"auto_release_enabled",
		"auto_release_at",
		"auto_release_started_at",
		"auto_release_last_attempt_at",
		"auto_release_attempts",
		"auto_release_last_error",
		"auto_release_state",
	}
	joined := strings.Join(mysqlReleaseReminderMigrationStatements, "\n")
	for _, column := range wantColumns {
		if !strings.Contains(joined, "ADD COLUMN "+column) {
			t.Errorf("release reminder migration missing %s", column)
		}
	}
	if strings.Contains(strings.ToUpper(joined), "DROP ") || strings.Contains(strings.ToUpper(joined), "DELETE ") {
		t.Fatalf("release reminder migrations are destructive: %s", joined)
	}
}

func TestMySQLOperationEventActorSchemaMigrations(t *testing.T) {
	joined := strings.Join(mysqlOperationEventMigrationStatements, "\n")
	for _, column := range []string{"member_email", "member_name"} {
		if !strings.Contains(joined, "ADD COLUMN "+column) {
			t.Errorf("operation event migration missing %s", column)
		}
	}
	if strings.Contains(strings.ToUpper(joined), "DROP ") || strings.Contains(strings.ToUpper(joined), "DELETE ") {
		t.Fatalf("operation event migrations are destructive: %s", joined)
	}
}

func TestMySQLStoreLockSchemaIsMigrationSafe(t *testing.T) {
	if !strings.Contains(mysqlStoreLockTableStatement, "CREATE TABLE IF NOT EXISTS cm_store_locks") {
		t.Fatalf("store lock table migration = %q", mysqlStoreLockTableStatement)
	}
	if !strings.Contains(mysqlStoreLockSeedStatement, "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("store lock seed migration = %q", mysqlStoreLockSeedStatement)
	}
}

func TestMySQLTransferSchemaIncludesMemberScopedIndexes(t *testing.T) {
	joined := strings.Join(mysqlSchemaStatements(), "\n")
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS cm_transfer_records",
		"INDEX idx_cm_transfer_member_profile (member_id, profile_name, updated_at)",
		"INDEX idx_cm_transfer_member_job (member_id, local_job_id)",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("transfer schema missing %q", want)
		}
	}
}

func TestMySQLCreateTransferUsesTransactionAndCryptographicID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	store := mysqlTransferTestStore(db, now)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(mysqlStoreLockForUpdateQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(4))
	mock.ExpectExec(regexp.QuoteMeta(mysqlTransferInsertQuery)).
		WithArgs(
			sqlmock.AnyArg(), "member-a", "a@example.com", "iossupport-usw2", "",
			TransferDirectionPush, "/tmp/App", "~/Documents/", "", TransferStatusCreated,
			0, "", now.Format(time.RFC3339), "", "", now.Format(time.RFC3339),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	created, err := store.CreateTransferRecord("member-a", TransferRecord{
		MemberID:    "member-a",
		MemberEmail: " A@EXAMPLE.COM ",
		ProfileName: "iossupport-usw2",
		Direction:   TransferDirectionPush,
		LocalPath:   "/tmp/App",
		RemotePath:  "~/Documents/",
		Status:      TransferStatusCreated,
	})
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	if !strings.HasPrefix(created.ID, "transfer-") || len(created.ID) < len("transfer-")+16 {
		t.Fatalf("created transfer ID = %q", created.ID)
	}
	if created.MemberEmail != "a@example.com" || created.CreatedAt != now.Format(time.RFC3339) || created.UpdatedAt != created.CreatedAt {
		t.Fatalf("created transfer = %+v", created)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLListTransferRecordsScopesAndOrdersByMember(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := mysqlTransferTestStore(db, time.Time{})
	rows := sqlmock.NewRows(mysqlTransferColumnNames).
		AddRow("transfer-2", "member-a", "a@example.com", "iossupport-usw2", "", TransferDirectionPush, "/tmp/App", "~/Documents/", "job-2", TransferStatusRunning, 50, "", "2026-07-16T08:00:00Z", "2026-07-16T08:01:00Z", "", "2026-07-16T08:02:00Z").
		AddRow("transfer-1", "member-a", "a@example.com", "iossupport-usw2", "", TransferDirectionPush, "/tmp/App", "~/Documents/", "", TransferStatusCreated, 0, "", "2026-07-16T07:00:00Z", "", "", "2026-07-16T07:00:00Z")
	mock.ExpectQuery(regexp.QuoteMeta(mysqlTransferListByProfileQuery)).
		WithArgs("member-a", "iossupport-usw2").
		WillReturnRows(rows)

	records, err := store.ListTransferRecords("member-a", "iossupport-usw2")
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(records) != 2 || records[0].ID != "transfer-2" || records[1].ID != "transfer-1" {
		t.Fatalf("records = %+v", records)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLListTransferRecordsWithOnlyMemberIDUsesMemberQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := mysqlTransferTestStore(db, time.Time{})
	record := TransferRecord{
		ID: "transfer-1", MemberID: "member-a", MemberEmail: "a@example.com",
		ProfileName: "iossupport-usw2", AppleEmail: "apple@example.com",
		Direction: TransferDirectionPull, LocalPath: "/tmp/App", RemotePath: "~/Documents/",
		LocalJobID: "job-1", Status: TransferStatusSucceeded, Percent: 100,
		CreatedAt: "2026-07-16T07:00:00Z", StartedAt: "2026-07-16T07:01:00Z",
		FinishedAt: "2026-07-16T07:02:00Z", UpdatedAt: "2026-07-16T07:02:00Z",
	}
	mock.ExpectQuery(regexp.QuoteMeta(mysqlTransferListQuery)).
		WithArgs("member-a").
		WillReturnRows(mysqlTransferRows(record))

	records, err := store.ListTransferRecords("member-a", "")
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if !reflect.DeepEqual(records, []TransferRecord{record}) {
		t.Fatalf("records = %+v, want %+v", records, record)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLMemberStoreLoadScansMembersAndTransferRecords(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := mysqlTransferTestStore(db, time.Time{})
	member := Member{
		ID: "member-a", Name: "Member A", Email: "a@example.com", Username: "member-a",
		Role: "member", Enabled: true, PasswordHash: "password-hash", PasswordSalt: "password-salt",
		APITokenHash: "token-hash", APITokenAt: "2026-07-16T06:00:00Z",
		CreatedAt: "2026-07-16T05:00:00Z", UpdatedAt: "2026-07-16T06:00:00Z",
	}
	record := TransferRecord{
		ID: "transfer-1", MemberID: member.ID, MemberEmail: member.Email,
		ProfileName: "iossupport-usw2", AppleEmail: "apple@example.com",
		Direction: TransferDirectionPush, LocalPath: "/tmp/App", RemotePath: "~/Documents/",
		LocalJobID: "job-1", Status: TransferStatusRunning, Percent: 50,
		ErrorSummary: "still running", CreatedAt: "2026-07-16T07:00:00Z",
		StartedAt: "2026-07-16T07:01:00Z", UpdatedAt: "2026-07-16T07:02:00Z",
	}

	mock.ExpectQuery(regexp.QuoteMeta(mysqlStoreLockVersionQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(9))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, name, email, username, role, enabled, COALESCE(password_hash, ''), COALESCE(password_salt, ''), COALESCE(api_token_hash, ''), COALESCE(api_token_at, ''), created_at, updated_at FROM cm_members ORDER BY email`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "email", "username", "role", "enabled", "password_hash", "password_salt", "api_token_hash", "api_token_at", "created_at", "updated_at"}).
			AddRow(member.ID, member.Name, member.Email, member.Username, member.Role, member.Enabled, member.PasswordHash, member.PasswordSalt, member.APITokenHash, member.APITokenAt, member.CreatedAt, member.UpdatedAt))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT apple_email, member_id, relation, created_at FROM cm_assignments ORDER BY apple_email, member_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"apple_email", "member_id", "relation", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT profile_name, member_id, updated_at FROM cm_profile_owners ORDER BY profile_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"profile_name", "member_id", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT name, COALESCE(apple_email, ''), enabled, profile_yaml, created_at, updated_at FROM cm_profiles ORDER BY name`)).
		WillReturnRows(sqlmock.NewRows([]string{"name", "apple_email", "enabled", "profile_yaml", "created_at", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT profile_name, member_id, created_at FROM cm_profile_members ORDER BY profile_name, member_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"profile_name", "member_id", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + mysqlReleaseReminderSelectColumns + ` FROM cm_release_reminders ORDER BY profile_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"profile_name"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + mysqlTransferSelectColumns + ` FROM cm_transfer_records ORDER BY updated_at DESC, id DESC`)).
		WillReturnRows(mysqlTransferRows(record))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT setting_key, COALESCE(setting_value, '') FROM cm_settings`)).
		WillReturnRows(sqlmock.NewRows([]string{"setting_key", "setting_value"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, action, profile, COALESCE(apple_email, ''), COALESCE(member_id, ''), COALESCE(member_email, ''), COALESCE(member_name, ''), confirmed, status, COALESCE(message, ''), created_at FROM cm_events ORDER BY created_at ASC LIMIT 500`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	data, err := store.Load()
	if err != nil {
		t.Fatalf("load member store: %v", err)
	}
	if !reflect.DeepEqual(data.Members, []Member{member}) {
		t.Fatalf("members = %+v, want %+v", data.Members, member)
	}
	if !reflect.DeepEqual(data.TransferRecords, []TransferRecord{record}) {
		t.Fatalf("transfer records = %+v, want %+v", data.TransferRecords, record)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLMemberStoreLoadReturnsTransferIterationError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := mysqlTransferTestStore(db, time.Time{})
	wantErr := errors.New("transfer rows interrupted")

	mock.ExpectQuery(regexp.QuoteMeta(mysqlStoreLockVersionQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(9))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, name, email, username, role, enabled, COALESCE(password_hash, ''), COALESCE(password_salt, ''), COALESCE(api_token_hash, ''), COALESCE(api_token_at, ''), created_at, updated_at FROM cm_members ORDER BY email`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "email", "username", "role", "enabled", "password_hash", "password_salt", "api_token_hash", "api_token_at", "created_at", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT apple_email, member_id, relation, created_at FROM cm_assignments ORDER BY apple_email, member_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"apple_email", "member_id", "relation", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT profile_name, member_id, updated_at FROM cm_profile_owners ORDER BY profile_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"profile_name", "member_id", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT name, COALESCE(apple_email, ''), enabled, profile_yaml, created_at, updated_at FROM cm_profiles ORDER BY name`)).
		WillReturnRows(sqlmock.NewRows([]string{"name", "apple_email", "enabled", "profile_yaml", "created_at", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT profile_name, member_id, created_at FROM cm_profile_members ORDER BY profile_name, member_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"profile_name", "member_id", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + mysqlReleaseReminderSelectColumns + ` FROM cm_release_reminders ORDER BY profile_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"profile_name"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + mysqlTransferSelectColumns + ` FROM cm_transfer_records ORDER BY updated_at DESC, id DESC`)).
		WillReturnRows(mysqlTransferRows(TransferRecord{
			ID: "transfer-1", MemberID: "member-a", MemberEmail: "a@example.com",
			ProfileName: "iossupport-usw2", Direction: TransferDirectionPush,
			LocalPath: "/tmp/App", RemotePath: "~/Documents/", Status: TransferStatusRunning,
			CreatedAt: "2026-07-16T07:00:00Z", UpdatedAt: "2026-07-16T07:00:00Z",
		}).RowError(0, wantErr))

	if _, err := store.Load(); !errors.Is(err, wantErr) {
		t.Fatalf("load error = %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLMemberStoreSaveDeletesAndReinsertsMembersAndTransferRecords(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)
	store := mysqlTransferTestStore(db, time.Time{})
	member := Member{
		ID: "member-a", Name: "Member A", Email: "a@example.com", Username: "member-a",
		Role: "admin", Enabled: true, PasswordHash: "password-hash", PasswordSalt: "password-salt",
		APITokenHash: "token-hash", APITokenAt: "2026-07-16T06:00:00Z",
		CreatedAt: "2026-07-16T05:00:00Z", UpdatedAt: "2026-07-16T06:00:00Z",
	}
	record := TransferRecord{
		ID: "transfer-1", MemberID: member.ID, MemberEmail: member.Email,
		ProfileName: "iossupport-usw2", AppleEmail: "apple@example.com",
		Direction: TransferDirectionPush, LocalPath: "/tmp/App", RemotePath: "~/Documents/",
		LocalJobID: "job-1", Status: TransferStatusSucceeded, Percent: 100,
		ErrorSummary: "", CreatedAt: "2026-07-16T07:00:00Z",
		StartedAt: "2026-07-16T07:01:00Z", FinishedAt: "2026-07-16T07:02:00Z",
		UpdatedAt: "2026-07-16T07:02:00Z",
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(mysqlStoreLockForUpdateQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(12))
	for _, table := range []string{"cm_events", "cm_transfer_records", "cm_release_reminders", "cm_profile_members", "cm_profiles", "cm_profile_owners", "cm_assignments", "cm_members", "cm_settings"} {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM " + table)).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO cm_members (id, name, email, username, role, enabled, password_hash, password_salt, api_token_hash, api_token_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)).
		WithArgs(member.ID, member.Name, member.Email, member.Username, member.Role, member.Enabled, member.PasswordHash, member.PasswordSalt, member.APITokenHash, member.APITokenAt, member.CreatedAt, member.UpdatedAt).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(mysqlTransferInsertQuery)).
		WithArgs(
			record.ID, record.MemberID, record.MemberEmail, record.ProfileName, record.AppleEmail,
			record.Direction, record.LocalPath, record.RemotePath, record.LocalJobID, record.Status,
			record.Percent, record.ErrorSummary, record.CreatedAt, record.StartedAt, record.FinishedAt, record.UpdatedAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	for _, setting := range []struct {
		key   string
		value string
	}{
		{key: "auth_secret", value: ""},
		{key: "default_owner_email", value: ""},
		{key: "default_status_filter", value: ""},
		{key: "background_confirm", value: "true"},
		{key: "show_released", value: "false"},
	} {
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO cm_settings (setting_key, setting_value) VALUES (?, ?)`)).
			WithArgs(setting.key, setting.value).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).
		WithArgs(mysqlStoreLockName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = store.Save(MemberData{
		Members:          []Member{member},
		TransferRecords:  []TransferRecord{record},
		Settings:         defaultWebSettings(),
		mutationRevision: 12,
	})
	if err != nil {
		t.Fatalf("save member store: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLUpdateTransferLocksMemberRowAndEnforcesSemantics(t *testing.T) {
	current := TransferRecord{
		ID: "transfer-1", MemberID: "member-a", MemberEmail: "a@example.com",
		ProfileName: "iossupport-usw2", Direction: TransferDirectionPush,
		LocalPath: "/tmp/App", RemotePath: "~/Documents/", Status: TransferStatusRunning,
		Percent: 50, LocalJobID: "job-a", CreatedAt: "2026-07-16T08:00:00Z",
		StartedAt: "2026-07-16T08:01:00Z", UpdatedAt: "2026-07-16T08:02:00Z",
	}
	now := time.Date(2026, 7, 16, 8, 3, 0, 0, time.UTC)

	t.Run("success", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()
		store := mysqlTransferTestStore(db, now)
		unbound := current
		unbound.LocalJobID = ""
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta(mysqlStoreLockForUpdateQuery)).
			WithArgs(mysqlStoreLockName).
			WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(5))
		mock.ExpectQuery(regexp.QuoteMeta(mysqlTransferSelectForUpdateQuery)).
			WithArgs(current.ID, current.MemberID).
			WillReturnRows(mysqlTransferRows(unbound))
		mock.ExpectExec(regexp.QuoteMeta(mysqlTransferUpdateQuery)).
			WithArgs(
				current.MemberEmail, current.ProfileName, current.AppleEmail, current.Direction,
				current.LocalPath, current.RemotePath, current.LocalJobID, TransferStatusSucceeded,
				100, "", current.StartedAt, now.Format(time.RFC3339), now.Format(time.RFC3339),
				current.ID, current.MemberID,
			).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).
			WithArgs(mysqlStoreLockName).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		updated, err := store.UpdateTransferRecord(current.MemberID, current.ID, current.LocalJobID, func(record TransferRecord) (TransferRecord, error) {
			record.LocalJobID = current.LocalJobID
			record.Status = TransferStatusSucceeded
			record.Percent = 100
			record.FinishedAt = now.Format(time.RFC3339)
			return record, nil
		})
		if err != nil {
			t.Fatalf("update transfer: %v", err)
		}
		if updated.Status != TransferStatusSucceeded || updated.Percent != 100 || updated.UpdatedAt != now.Format(time.RFC3339) {
			t.Fatalf("updated transfer = %+v", updated)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	for _, test := range []struct {
		name       string
		memberID   string
		localJobID string
		current    TransferRecord
		update     func(TransferRecord) (TransferRecord, error)
		wantErr    string
	}{
		{
			name: "wrong member", memberID: "member-b", localJobID: "job-a",
			wantErr: ErrTransferRecordNotFound.Error(),
		},
		{
			name: "wrong local job", memberID: "member-a", localJobID: "job-b", current: current,
			wantErr: ErrTransferRecordNotFound.Error(),
		},
		{
			name: "percent regression", memberID: "member-a", localJobID: "job-a", current: current,
			update: func(record TransferRecord) (TransferRecord, error) {
				record.Percent = 49
				return record, nil
			},
			wantErr: "transfer percent cannot regress",
		},
		{
			name: "terminal immutable", memberID: "member-a", localJobID: "job-a",
			current: func() TransferRecord {
				record := current
				record.Status = TransferStatusFailed
				return record
			}(),
			wantErr: "terminal transfer record cannot be updated",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			store := mysqlTransferTestStore(db, now)
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta(mysqlStoreLockForUpdateQuery)).
				WithArgs(mysqlStoreLockName).
				WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(5))
			query := mock.ExpectQuery(regexp.QuoteMeta(mysqlTransferSelectForUpdateQuery)).
				WithArgs(current.ID, test.memberID)
			if test.name == "wrong member" {
				query.WillReturnError(sql.ErrNoRows)
			} else {
				query.WillReturnRows(mysqlTransferRows(test.current))
			}
			mock.ExpectRollback()
			update := test.update
			if update == nil {
				update = func(record TransferRecord) (TransferRecord, error) { return record, nil }
			}
			_, err = store.UpdateTransferRecord(test.memberID, current.ID, test.localJobID, update)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("update error = %v, want %q", err, test.wantErr)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLDeleteTransferScopesByMemberAndReturnsNotFound(t *testing.T) {
	for _, test := range []struct {
		name     string
		affected int64
		wantErr  error
	}{
		{name: "deleted", affected: 1},
		{name: "wrong member", affected: 0, wantErr: ErrTransferRecordNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			store := mysqlTransferTestStore(db, time.Time{})
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta(mysqlStoreLockForUpdateQuery)).
				WithArgs(mysqlStoreLockName).
				WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(6))
			mock.ExpectExec(regexp.QuoteMeta(mysqlTransferDeleteQuery)).
				WithArgs("transfer-1", "member-a").
				WillReturnResult(sqlmock.NewResult(0, test.affected))
			if test.wantErr == nil {
				mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).
					WithArgs(mysqlStoreLockName).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			err = store.DeleteTransferRecord("member-a", "transfer-1")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("delete error = %v, want %v", err, test.wantErr)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func mysqlTransferTestStore(db *sql.DB, now time.Time) MySQLMemberStore {
	return MySQLMemberStore{
		DSN:         "sqlmock",
		Now:         func() time.Time { return now },
		schemaGuard: &mysqlSchemaGuard{success: true},
		openDB:      func() (*sql.DB, error) { return db, nil },
	}
}

func mysqlTransferRows(record TransferRecord) *sqlmock.Rows {
	return sqlmock.NewRows(mysqlTransferColumnNames).AddRow(
		record.ID, record.MemberID, record.MemberEmail, record.ProfileName, record.AppleEmail,
		record.Direction, record.LocalPath, record.RemotePath, record.LocalJobID, record.Status,
		record.Percent, record.ErrorSummary, record.CreatedAt, record.StartedAt, record.FinishedAt, record.UpdatedAt,
	)
}

func TestMySQLReleaseReminderSelectColumnsIncludeAutoReleaseState(t *testing.T) {
	wantColumns := `profile_name, COALESCE(apple_email, ''), COALESCE(host_id, ''), COALESCE(host_created_at, ''), COALESCE(release_due_at, ''), COALESCE(owner_email, ''), COALESCE(owner_name, ''), COALESCE(last_extended_by_email, ''), COALESCE(last_extended_by_name, ''), COALESCE(last_extended_at, ''), COALESCE(last_notified_at, ''), COALESCE(released_at, ''), status, auto_release_enabled, COALESCE(auto_release_at, ''), COALESCE(auto_release_started_at, ''), COALESCE(auto_release_last_attempt_at, ''), auto_release_attempts, COALESCE(auto_release_last_error, ''), COALESCE(auto_release_state, ''), created_at, updated_at`
	if mysqlReleaseReminderSelectColumns != wantColumns {
		t.Fatalf("release reminder SELECT columns = %q, want %q", mysqlReleaseReminderSelectColumns, wantColumns)
	}
	wantQuery := `SELECT ` + wantColumns + ` FROM cm_release_reminders WHERE profile_name = ? FOR UPDATE`
	if mysqlReleaseReminderSelectForUpdate != wantQuery {
		t.Fatalf("release reminder lock query = %q, want %q", mysqlReleaseReminderSelectForUpdate, wantQuery)
	}
}

func TestMySQLSchemaGuardRetriesFailureThenCachesSuccess(t *testing.T) {
	wantErr := errors.New("migration failed")
	guard := &mysqlSchemaGuard{}
	calls := 0
	if err := guard.run(func() error {
		calls++
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("first schema error = %v, want %v", err, wantErr)
	}
	if err := guard.run(func() error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("retry schema migration: %v", err)
	}
	if err := guard.run(func() error {
		calls++
		return errors.New("must not run")
	}); err != nil {
		t.Fatalf("cached schema migration: %v", err)
	}
	if calls != 2 {
		t.Fatalf("schema migration calls = %d, want 2", calls)
	}
}

func TestMySQLSchemaGuardCoalescesConcurrentSuccess(t *testing.T) {
	guard := &mysqlSchemaGuard{}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- guard.run(func() error {
				if calls.Add(1) == 1 {
					close(started)
				}
				<-release
				return nil
			})
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent schema migration: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent schema migration calls = %d, want 1", calls.Load())
	}
}

func TestStoreMutationGuardSharedAcrossCopies(t *testing.T) {
	fileStore := NewMemberStore(filepath.Join(t.TempDir(), "members.json"))
	fileCopy := fileStore
	if fileStore.normalize().mutationGuard != fileCopy.normalize().mutationGuard {
		t.Fatal("copied file stores do not share mutation guard")
	}

	mysqlStore := MySQLMemberStore{DSN: "mutation-guard-test"}
	mysqlCopy := mysqlStore
	if mysqlStore.normalize().mutationGuard != mysqlCopy.normalize().mutationGuard {
		t.Fatal("copied MySQL stores do not share mutation guard")
	}
	if mysqlStore.normalize().schemaGuard != mysqlCopy.normalize().schemaGuard {
		t.Fatal("copied MySQL stores do not share schema guard")
	}
}

func TestMySQLSaveReleaseReminderInsertRoundTripsThroughScan(t *testing.T) {
	want := ReleaseReminder{
		ProfileName:              "apple-usw2",
		AppleEmail:               "apple@example.com",
		HostID:                   "h-123",
		HostCreatedAt:            "2026-07-01T08:00:00Z",
		ReleaseDueAt:             "2026-07-02T08:00:00Z",
		OwnerEmail:               "owner@example.com",
		OwnerName:                "Owner",
		LastExtendedByEmail:      "admin@example.com",
		LastExtendedByName:       "Admin",
		LastExtendedAt:           "2026-07-01T09:00:00Z",
		LastNotifiedAt:           "2026-07-01T10:00:00Z",
		ReleasedAt:               "2026-07-02T09:00:00Z",
		Status:                   ReleaseReminderStatusReleased,
		AutoReleaseEnabled:       true,
		AutoReleaseAt:            "2026-07-02T08:00:00Z",
		AutoReleaseStartedAt:     "2026-07-02T08:01:00Z",
		AutoReleaseLastAttemptAt: "2026-07-02T08:02:00Z",
		AutoReleaseAttempts:      3,
		AutoReleaseLastError:     "previous failure",
		AutoReleaseState:         ReleaseReminderAutoReleaseStateReleased,
		CreatedAt:                "2026-07-01T08:00:00Z",
		UpdatedAt:                "2026-07-02T09:00:00Z",
	}
	tx := &fakeMySQLReleaseReminderTransaction{}
	if err := insertMySQLReleaseReminder(tx, want); err != nil {
		t.Fatalf("insert reminder: %v", err)
	}
	wantQuery := `INSERT INTO cm_release_reminders (profile_name, apple_email, host_id, host_created_at, release_due_at, owner_email, owner_name, last_extended_by_email, last_extended_by_name, last_extended_at, last_notified_at, released_at, status, auto_release_enabled, auto_release_at, auto_release_started_at, auto_release_last_attempt_at, auto_release_attempts, auto_release_last_error, auto_release_state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if tx.execQuery != wantQuery {
		t.Fatalf("INSERT query = %q, want %q", tx.execQuery, wantQuery)
	}
	if len(tx.execArgs) != 22 {
		t.Fatalf("INSERT arg count = %d, want 22", len(tx.execArgs))
	}
	var got ReleaseReminder
	if err := scanMySQLReleaseReminder(fakeMySQLReleaseReminderRow{reminder: tx.written}, &got); err != nil {
		t.Fatalf("scan inserted reminder: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inserted reminder round trip = %+v, want %+v", got, want)
	}
}

func TestMySQLMarkReleaseReminderReleasedConvergesAutoReleaseState(t *testing.T) {
	current := ReleaseReminder{
		ProfileName:          "apple-usw2",
		AppleEmail:           "apple@example.com",
		Status:               ReleaseReminderStatusReleased,
		ReleasedAt:           "2026-07-02T08:00:00Z",
		AutoReleaseEnabled:   true,
		AutoReleaseState:     ReleaseReminderAutoReleaseStateRunning,
		AutoReleaseLastError: "stale release error",
		CreatedAt:            "2026-07-01T08:00:00Z",
		UpdatedAt:            "2026-07-02T08:00:00Z",
	}
	tx := &fakeMySQLReleaseReminderTransaction{row: fakeMySQLReleaseReminderRow{reminder: current}}
	now := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)

	got, err := updateReleaseReminderInMySQLTransaction(
		tx,
		current.ProfileName,
		now,
		markReleaseReminderReleased(now.Format(time.RFC3339)),
	)
	if err != nil {
		t.Fatalf("mark MySQL reminder released: %v", err)
	}
	if got.Status != ReleaseReminderStatusReleased ||
		got.ReleasedAt != current.ReleasedAt ||
		got.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased ||
		got.AutoReleaseLastError != "" {
		t.Fatalf("released MySQL reminder did not converge: %+v", got)
	}
	if !reflect.DeepEqual(tx.written, got) {
		t.Fatalf("persisted MySQL reminder = %+v, want %+v", tx.written, got)
	}
}

func TestMySQLCleanupProfileRecordsRollsBackOwnerReminderAndEventTogether(t *testing.T) {
	current := ReleaseReminder{
		ProfileName:          "apple-usw2",
		AppleEmail:           "apple@example.com",
		Status:               ReleaseReminderStatusReleased,
		ReleasedAt:           "2026-07-02T08:00:00Z",
		AutoReleaseState:     ReleaseReminderAutoReleaseStateRunning,
		AutoReleaseLastError: "stale release error",
		CreatedAt:            "2026-07-01T08:00:00Z",
		UpdatedAt:            "2026-07-02T08:00:00Z",
	}
	tx := &fakeMySQLReleaseReminderTransaction{
		row:          fakeMySQLReleaseReminderRow{reminder: current},
		ownerRow:     fakeMySQLProfileOwnerRow{memberID: "member-1", email: "owner@example.com"},
		eventExecErr: errors.New("event insert failed"),
	}
	now := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)

	_, _, err := cleanupProfileRecordsInMySQLTransaction(
		tx,
		current.ProfileName,
		now.Format(time.RFC3339),
		"auto-status",
		now,
	)
	if err == nil || !strings.Contains(err.Error(), "event insert failed") {
		t.Fatalf("cleanup error = %v", err)
	}
	if !tx.rolledBack || tx.committed {
		t.Fatalf("cleanup transaction committed=%t rolledBack=%t", tx.committed, tx.rolledBack)
	}
	if !tx.ownerDeleted {
		t.Fatal("test did not reach owner deletion before rollback")
	}
}

func TestMySQLWholeStoreSaveLocksAndReloadsStaleRemindersBeforeDeletes(t *testing.T) {
	current := ReleaseReminder{
		ProfileName:         "apple-usw2",
		AutoReleaseEnabled:  true,
		AutoReleaseAttempts: 3,
		AutoReleaseState:    ReleaseReminderAutoReleaseStateRetrying,
	}
	tx := &fakeMySQLReleaseReminderTransaction{
		lockVersion: 7,
		queryRows:   []ReleaseReminder{current},
		transferRows: []TransferRecord{{
			ID: "transfer-current", MemberID: "member-a", Status: TransferStatusRunning,
		}},
	}
	data := MemberData{
		Reminders:        []ReleaseReminder{{ProfileName: "apple-usw2"}},
		TransferRecords:  []TransferRecord{{ID: "transfer-stale", MemberID: "member-a"}},
		mutationRevision: 6,
	}
	got, err := prepareMySQLWholeStoreSave(tx, data)
	if err != nil {
		t.Fatalf("prepare whole-store save: %v", err)
	}
	if !reflect.DeepEqual(got.Reminders, []ReleaseReminder{current}) {
		t.Fatalf("stale save reminders = %+v, want %+v", got.Reminders, current)
	}
	if !reflect.DeepEqual(got.TransferRecords, tx.transferRows) {
		t.Fatalf("stale save transfers = %+v, want %+v", got.TransferRecords, tx.transferRows)
	}
	if len(tx.operations) < 4 {
		t.Fatalf("whole-store operations = %v", tx.operations)
	}
	if tx.operations[0] != "query-row:"+mysqlStoreLockForUpdateQuery {
		t.Fatalf("first whole-store operation = %q", tx.operations[0])
	}
	wantReminderQuery := "query:SELECT " + mysqlReleaseReminderSelectColumns + " FROM cm_release_reminders ORDER BY profile_name"
	if tx.operations[1] != wantReminderQuery {
		t.Fatalf("second whole-store operation = %q, want %q", tx.operations[1], wantReminderQuery)
	}
	wantTransferQuery := "query:SELECT " + mysqlTransferSelectColumns + " FROM cm_transfer_records ORDER BY updated_at DESC, id DESC"
	if tx.operations[2] != wantTransferQuery {
		t.Fatalf("third whole-store operation = %q, want %q", tx.operations[2], wantTransferQuery)
	}
	if tx.operations[3] != "exec:DELETE FROM cm_events" {
		t.Fatalf("first delete operation = %q", tx.operations[3])
	}
}

func TestMySQLWholeStoreSaveIterationErrorRollsBackBeforeDeletes(t *testing.T) {
	wantErr := errors.New("rows iteration failed")
	tx := &fakeMySQLReleaseReminderTransaction{
		lockVersion: 2,
		queryRows: []ReleaseReminder{{
			ProfileName:        "apple-usw2",
			AutoReleaseEnabled: true,
		}},
		queryRowsErr: wantErr,
	}
	_, err := prepareMySQLWholeStoreSave(tx, MemberData{mutationRevision: 1})
	if !errors.Is(err, wantErr) {
		t.Fatalf("iteration error = %v, want %v", err, wantErr)
	}
	if !tx.rolledBack {
		t.Fatal("iteration error did not roll back transaction")
	}
	for _, operation := range tx.operations {
		if strings.HasPrefix(operation, "exec:DELETE ") || strings.HasPrefix(operation, "exec:INSERT ") {
			t.Fatalf("destructive operation after iteration error: %v", tx.operations)
		}
	}
}

func TestMySQLUpdateReleaseReminderSelectScanAndWriteRoundTrip(t *testing.T) {
	current := ReleaseReminder{
		ProfileName:              "apple-usw2",
		AppleEmail:               "apple@example.com",
		HostID:                   "h-123",
		HostCreatedAt:            "2026-07-01T08:00:00Z",
		ReleaseDueAt:             "2026-07-02T08:00:00Z",
		OwnerEmail:               "owner@example.com",
		OwnerName:                "Owner",
		LastExtendedByEmail:      "admin@example.com",
		LastExtendedByName:       "Admin",
		LastExtendedAt:           "2026-07-01T09:00:00Z",
		LastNotifiedAt:           "2026-07-01T10:00:00Z",
		ReleasedAt:               "",
		Status:                   ReleaseReminderStatusActive,
		AutoReleaseEnabled:       true,
		AutoReleaseAt:            "2026-07-02T08:00:00Z",
		AutoReleaseStartedAt:     "2026-07-02T08:01:00Z",
		AutoReleaseLastAttemptAt: "2026-07-02T08:02:00Z",
		AutoReleaseAttempts:      2,
		AutoReleaseLastError:     "pending",
		AutoReleaseState:         ReleaseReminderAutoReleaseStateRetrying,
		CreatedAt:                "2026-07-01T08:00:00Z",
		UpdatedAt:                "2026-07-02T08:02:00Z",
	}
	tx := &fakeMySQLReleaseReminderTransaction{row: fakeMySQLReleaseReminderRow{reminder: current}}
	now := time.Date(2026, 7, 2, 8, 3, 0, 0, time.UTC)

	got, err := updateReleaseReminderInMySQLTransaction(tx, current.ProfileName, now, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		if !reflect.DeepEqual(reminder, current) {
			t.Fatalf("scanned reminder = %+v, want %+v", reminder, current)
		}
		reminder.ProfileName = "changed"
		reminder.CreatedAt = "changed"
		reminder.AppleEmail = " APPLE@EXAMPLE.COM "
		reminder.OwnerEmail = " OWNER@EXAMPLE.COM "
		reminder.LastExtendedByEmail = " ADMIN@EXAMPLE.COM "
		reminder.Status = ""
		reminder.AutoReleaseAttempts++
		reminder.AutoReleaseLastError = ""
		reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateReleased
		return reminder, nil
	})
	if err != nil {
		t.Fatalf("update reminder: %v", err)
	}
	want := current
	want.AutoReleaseAttempts = 3
	want.AutoReleaseLastError = ""
	want.AutoReleaseState = ReleaseReminderAutoReleaseStateReleased
	want.Status = ReleaseReminderStatusActive
	want.UpdatedAt = now.Format(time.RFC3339)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("updated reminder = %+v, want %+v", got, want)
	}
	if tx.query != mysqlReleaseReminderSelectForUpdate || len(tx.queryArgs) != 1 || tx.queryArgs[0] != current.ProfileName {
		t.Fatalf("SELECT query = %q args=%v", tx.query, tx.queryArgs)
	}
	if !strings.HasPrefix(tx.execQuery, "UPDATE cm_release_reminders SET ") {
		t.Fatalf("UPDATE query = %q", tx.execQuery)
	}
	if len(tx.execArgs) != 21 {
		t.Fatalf("UPDATE arg count = %d, want 21", len(tx.execArgs))
	}
	wantUpdateQuery := `UPDATE cm_release_reminders SET apple_email = ?, host_id = ?, host_created_at = ?, release_due_at = ?, owner_email = ?, owner_name = ?, last_extended_by_email = ?, last_extended_by_name = ?, last_extended_at = ?, last_notified_at = ?, released_at = ?, status = ?, auto_release_enabled = ?, auto_release_at = ?, auto_release_started_at = ?, auto_release_last_attempt_at = ?, auto_release_attempts = ?, auto_release_last_error = ?, auto_release_state = ?, updated_at = ? WHERE profile_name = ?`
	if tx.execQuery != wantUpdateQuery {
		t.Fatalf("UPDATE query = %q, want %q", tx.execQuery, wantUpdateQuery)
	}
	if !reflect.DeepEqual(tx.written, want) {
		t.Fatalf("written reminder = %+v, want %+v", tx.written, want)
	}
	wantOperations := []string{
		"query-row:" + mysqlStoreLockForUpdateQuery,
		"query-row:" + mysqlReleaseReminderSelectForUpdate,
		"exec:" + mysqlReleaseReminderUpdateQuery,
		"exec:" + mysqlStoreLockAdvanceQuery,
	}
	if !reflect.DeepEqual(tx.operations, wantOperations) {
		t.Fatalf("transaction operations = %v, want %v", tx.operations, wantOperations)
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("transaction committed=%t rolledBack=%t", tx.committed, tx.rolledBack)
	}
}

func TestMySQLUpsertReleaseReminderPreservesAutomaticReleaseState(t *testing.T) {
	current := ReleaseReminder{
		ProfileName:              "apple-usw2",
		AppleEmail:               "apple@example.com",
		HostID:                   "h-original",
		Status:                   ReleaseReminderStatusActive,
		AutoReleaseEnabled:       true,
		AutoReleaseAt:            "2026-07-02T08:00:00Z",
		AutoReleaseStartedAt:     "2026-07-02T08:01:00Z",
		AutoReleaseLastAttemptAt: "2026-07-02T08:02:00Z",
		AutoReleaseAttempts:      2,
		AutoReleaseLastError:     "pending",
		AutoReleaseState:         ReleaseReminderAutoReleaseStateRetrying,
		CreatedAt:                "2026-07-01T08:00:00Z",
		UpdatedAt:                "2026-07-02T08:02:00Z",
	}
	tx := &fakeMySQLReleaseReminderTransaction{row: fakeMySQLReleaseReminderRow{reminder: current}}
	now := time.Date(2026, 7, 2, 8, 3, 0, 0, time.UTC)
	got, err := upsertReleaseReminderInMySQLTransaction(tx, ReleaseReminder{
		ProfileName:         current.ProfileName,
		AppleEmail:          " APPLE@EXAMPLE.COM ",
		HostID:              "h-original",
		OwnerEmail:          " OWNER@EXAMPLE.COM ",
		LastExtendedByEmail: " ADMIN@EXAMPLE.COM ",
	}, now)
	if err != nil {
		t.Fatalf("upsert reminder: %v", err)
	}
	want := current
	want.AppleEmail = "apple@example.com"
	want.HostID = "h-original"
	want.OwnerEmail = "owner@example.com"
	want.LastExtendedByEmail = "admin@example.com"
	want.Status = ReleaseReminderStatusActive
	want.UpdatedAt = now.Format(time.RFC3339)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(tx.written, want) {
		t.Fatalf("upserted reminder got=%+v written=%+v want=%+v", got, tx.written, want)
	}
	wantOperations := []string{
		"query-row:" + mysqlStoreLockForUpdateQuery,
		"query-row:" + mysqlReleaseReminderSelectForUpdate,
		"exec:" + mysqlReleaseReminderUpdateQuery,
		"exec:" + mysqlStoreLockAdvanceQuery,
	}
	if !reflect.DeepEqual(tx.operations, wantOperations) {
		t.Fatalf("upsert operations = %v, want %v", tx.operations, wantOperations)
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("upsert transaction committed=%t rolledBack=%t", tx.committed, tx.rolledBack)
	}
}

func TestMySQLUpsertReleaseReminderInsertsMissingRow(t *testing.T) {
	tx := &fakeMySQLReleaseReminderTransaction{row: fakeMySQLReleaseReminderRow{err: sql.ErrNoRows}}
	now := time.Date(2026, 7, 2, 8, 3, 0, 0, time.UTC)
	want := ReleaseReminder{
		ProfileName:        "apple-usw2",
		Status:             ReleaseReminderStatusActive,
		AutoReleaseEnabled: true,
		AutoReleaseAt:      "2026-07-02T08:00:00Z",
		AutoReleaseState:   ReleaseReminderAutoReleaseStateScheduled,
		CreatedAt:          now.Format(time.RFC3339),
		UpdatedAt:          now.Format(time.RFC3339),
	}
	got, err := upsertReleaseReminderInMySQLTransaction(tx, ReleaseReminder{
		ProfileName:        want.ProfileName,
		Status:             want.Status,
		AutoReleaseEnabled: want.AutoReleaseEnabled,
		AutoReleaseAt:      want.AutoReleaseAt,
		AutoReleaseState:   want.AutoReleaseState,
	}, now)
	if err != nil {
		t.Fatalf("insert reminder: %v", err)
	}
	if tx.execQuery != mysqlReleaseReminderInsertQuery || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(tx.written, want) {
		t.Fatalf("inserted reminder query=%q got=%+v written=%+v want=%+v", tx.execQuery, got, tx.written, want)
	}
	wantOperations := []string{
		"query-row:" + mysqlStoreLockForUpdateQuery,
		"query-row:" + mysqlReleaseReminderSelectForUpdate,
		"exec:" + mysqlReleaseReminderInsertQuery,
		"exec:" + mysqlStoreLockAdvanceQuery,
	}
	if !reflect.DeepEqual(tx.operations, wantOperations) {
		t.Fatalf("insert operations = %v, want %v", tx.operations, wantOperations)
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("insert transaction committed=%t rolledBack=%t", tx.committed, tx.rolledBack)
	}
}

func TestMySQLUpdateReleaseReminderMissingRollsBack(t *testing.T) {
	tx := &fakeMySQLReleaseReminderTransaction{row: fakeMySQLReleaseReminderRow{err: sql.ErrNoRows}}
	called := false
	_, err := updateReleaseReminderInMySQLTransaction(tx, "missing", time.Time{}, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		called = true
		return reminder, nil
	})
	if !errors.Is(err, ErrReleaseReminderNotFound) {
		t.Fatalf("missing reminder error = %v", err)
	}
	if called || tx.committed || !tx.rolledBack || tx.execQuery != "" {
		t.Fatalf("missing transaction called=%t committed=%t rolledBack=%t exec=%q", called, tx.committed, tx.rolledBack, tx.execQuery)
	}
}

func TestMySQLUpdateReleaseReminderCallbackErrorRollsBack(t *testing.T) {
	wantErr := errors.New("stop update")
	tx := &fakeMySQLReleaseReminderTransaction{row: fakeMySQLReleaseReminderRow{reminder: ReleaseReminder{ProfileName: "apple-usw2"}}}
	_, err := updateReleaseReminderInMySQLTransaction(tx, "apple-usw2", time.Time{}, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.HostID = "changed"
		return reminder, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("callback error = %v, want %v", err, wantErr)
	}
	if tx.committed || !tx.rolledBack || tx.execQuery != "" {
		t.Fatalf("callback error committed=%t rolledBack=%t exec=%q", tx.committed, tx.rolledBack, tx.execQuery)
	}
}

func TestMySQLUpdateReleaseReminderWriteAndCommitErrorsRollBack(t *testing.T) {
	for _, test := range []struct {
		name      string
		execErr   error
		commitErr error
	}{
		{name: "write", execErr: errors.New("write failed")},
		{name: "commit", commitErr: errors.New("commit failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := &fakeMySQLReleaseReminderTransaction{
				row:       fakeMySQLReleaseReminderRow{reminder: ReleaseReminder{ProfileName: "apple-usw2"}},
				execErr:   test.execErr,
				commitErr: test.commitErr,
			}
			_, err := updateReleaseReminderInMySQLTransaction(tx, "apple-usw2", time.Time{}, func(reminder ReleaseReminder) (ReleaseReminder, error) {
				return reminder, nil
			})
			wantErr := test.execErr
			if wantErr == nil {
				wantErr = test.commitErr
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("transaction error = %v, want %v", err, wantErr)
			}
			if !tx.rolledBack {
				t.Fatal("failed transaction was not rolled back")
			}
		})
	}
}

func TestMySQLUpdateReleaseReminderAndRecordEventRollsBackEventFailure(t *testing.T) {
	wantErr := errors.New("event insert failed")
	current := ReleaseReminder{ProfileName: "apple-usw2", AppleEmail: "apple@example.com", AutoReleaseEnabled: false}
	tx := &fakeMySQLReleaseReminderTransaction{
		row:          fakeMySQLReleaseReminderRow{reminder: current},
		eventExecErr: wantErr,
	}
	_, err := updateReleaseReminderAndRecordEventInMySQLTransaction(tx, current.ProfileName, time.Date(2026, 7, 2, 8, 3, 0, 0, time.UTC), func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.AutoReleaseEnabled = true
		return reminder, nil
	}, OperationEvent{Action: "release-reminder.auto-release.enabled", MemberID: "member-1", MemberEmail: "admin@example.com", MemberName: "Admin", Status: "success"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("event failure = %v, want %v", err, wantErr)
	}
	if tx.committed || !tx.rolledBack {
		t.Fatalf("event failure committed=%t rolledBack=%t", tx.committed, tx.rolledBack)
	}
	wantOperations := []string{
		"query-row:" + mysqlStoreLockForUpdateQuery,
		"query-row:" + mysqlReleaseReminderSelectForUpdate,
		"exec:" + mysqlReleaseReminderUpdateQuery,
		"exec:" + mysqlOperationEventInsertQuery,
	}
	if !reflect.DeepEqual(tx.operations, wantOperations) {
		t.Fatalf("event failure operations = %v, want %v", tx.operations, wantOperations)
	}
}

func TestMySQLSetProfileOwnerAndRecordEventTransaction(t *testing.T) {
	now := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	member := Member{
		ID: "member-owner", Name: "Owner", Email: "owner@example.com", Username: "owner@example.com",
		Role: "operator", Enabled: true, CreatedAt: "2026-07-01T00:00:00Z", UpdatedAt: "2026-07-01T00:00:00Z",
	}
	event := OperationEvent{
		Action: "profile-owner.set", MemberID: "member-admin", MemberEmail: "admin@example.com",
		MemberName: "Admin", Confirmed: true, Status: "success", Message: "owner changed",
	}

	for _, test := range []struct {
		name     string
		eventErr error
	}{
		{name: "success"},
		{name: "event insert failure rolls back", eventErr: errors.New("event insert failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			store := mysqlTransferTestStore(db, now)

			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta(mysqlStoreLockForUpdateQuery)).
				WithArgs(mysqlStoreLockName).
				WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(9))
			mock.ExpectQuery(regexp.QuoteMeta(mysqlManagedProfileForUpdateQuery)).
				WithArgs("apple-usw2").
				WillReturnRows(sqlmock.NewRows([]string{"name", "apple_email"}).AddRow("apple-usw2", "apple@example.com"))
			mock.ExpectQuery(regexp.QuoteMeta(mysqlMemberByEmailForUpdateQuery)).
				WithArgs("owner@example.com").
				WillReturnRows(sqlmock.NewRows([]string{"id", "name", "email", "username", "role", "enabled", "password_hash", "password_salt", "api_token_hash", "api_token_at", "created_at", "updated_at"}).
					AddRow(member.ID, member.Name, member.Email, member.Username, member.Role, member.Enabled, "", "", "", "", member.CreatedAt, member.UpdatedAt))
			mock.ExpectExec(regexp.QuoteMeta(mysqlProfileOwnerUpsertQuery)).
				WithArgs("apple-usw2", member.ID, now.Format(time.RFC3339)).
				WillReturnResult(sqlmock.NewResult(1, 1))
			eventInsert := mock.ExpectExec(regexp.QuoteMeta(mysqlOperationEventInsertQuery)).
				WithArgs(sqlmock.AnyArg(), event.Action, "apple-usw2", "apple@example.com", event.MemberID, event.MemberEmail, event.MemberName, event.Confirmed, event.Status, event.Message, now.Format(time.RFC3339))
			if test.eventErr != nil {
				eventInsert.WillReturnError(test.eventErr)
				mock.ExpectRollback()
			} else {
				eventInsert.WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectExec(regexp.QuoteMeta(mysqlStoreLockAdvanceQuery)).
					WithArgs(mysqlStoreLockName).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			}

			owner, err := store.SetProfileOwnerAndRecordEvent("apple-usw2", " OWNER@EXAMPLE.COM ", event)
			if test.eventErr != nil {
				if !errors.Is(err, test.eventErr) {
					t.Fatalf("event failure = %v, want %v", err, test.eventErr)
				}
			} else if err != nil {
				t.Fatalf("atomic owner update: %v", err)
			} else if owner.ProfileName != "apple-usw2" || owner.Owner.Email != member.Email {
				t.Fatalf("owner = %+v", owner)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
