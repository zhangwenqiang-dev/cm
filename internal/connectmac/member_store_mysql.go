package connectmac

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

type MySQLMemberStore struct {
	DSN           string
	Now           func() time.Time
	schemaGuard   *mysqlSchemaGuard
	mutationGuard *storeMutationGuard
	openDB        func() (*sql.DB, error)
}

var mysqlTransferColumnNames = []string{
	"id", "member_id", "member_email", "profile_name", "apple_email", "direction",
	"local_path", "remote_path", "local_job_id", "status", "percent", "error_summary",
	"created_at", "started_at", "finished_at", "updated_at",
}

const mysqlTransferSelectColumns = `id, member_id, member_email, profile_name, COALESCE(apple_email, ''), direction, local_path, remote_path, COALESCE(local_job_id, ''), CONCAT(status, '|', COALESCE(phase, '')), percent, COALESCE(error_summary, ''), created_at, COALESCE(started_at, ''), COALESCE(finished_at, ''), updated_at`

const mysqlTransferInsertQuery = `INSERT INTO cm_transfer_records (id, member_id, member_email, profile_name, apple_email, direction, local_path, remote_path, local_job_id, status, percent, error_summary, created_at, started_at, finished_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const mysqlTransferInsertWithPhaseQuery = `INSERT INTO cm_transfer_records (id, member_id, member_email, profile_name, apple_email, direction, local_path, remote_path, local_job_id, status, phase, percent, error_summary, created_at, started_at, finished_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const mysqlTransferListQuery = `SELECT ` + mysqlTransferSelectColumns + ` FROM cm_transfer_records WHERE member_id = ? ORDER BY updated_at DESC, id DESC`

const mysqlTransferListByProfileQuery = `SELECT ` + mysqlTransferSelectColumns + ` FROM cm_transfer_records WHERE member_id = ? AND profile_name = ? ORDER BY updated_at DESC, id DESC`

const mysqlTransferSelectForUpdateQuery = `SELECT ` + mysqlTransferSelectColumns + ` FROM cm_transfer_records WHERE id = ? AND member_id = ? FOR UPDATE`

const mysqlTransferUpdateQuery = `UPDATE cm_transfer_records SET member_email = ?, profile_name = ?, apple_email = ?, direction = ?, local_path = ?, remote_path = ?, local_job_id = ?, status = ?, percent = ?, error_summary = ?, started_at = ?, finished_at = ?, updated_at = ? WHERE id = ? AND member_id = ?`

const mysqlTransferPhaseUpdateQuery = `UPDATE cm_transfer_records SET phase = ? WHERE id = ? AND member_id = ?`

const mysqlTransferDeleteQuery = `DELETE FROM cm_transfer_records WHERE id = ? AND member_id = ?`

const mysqlReleaseReminderSelectColumns = `profile_name, COALESCE(apple_email, ''), COALESCE(host_id, ''), COALESCE(host_created_at, ''), COALESCE(release_due_at, ''), COALESCE(owner_email, ''), COALESCE(owner_name, ''), COALESCE(last_extended_by_email, ''), COALESCE(last_extended_by_name, ''), COALESCE(last_extended_at, ''), COALESCE(last_notified_at, ''), COALESCE(released_at, ''), status, auto_release_enabled, COALESCE(auto_release_at, ''), COALESCE(auto_release_started_at, ''), COALESCE(auto_release_last_attempt_at, ''), COALESCE(auto_release_notified_at, ''), auto_release_attempts, COALESCE(auto_release_last_error, ''), COALESCE(auto_release_state, ''), created_at, updated_at`

const mysqlReleaseReminderSelectForUpdate = `SELECT ` + mysqlReleaseReminderSelectColumns + ` FROM cm_release_reminders WHERE profile_name = ? FOR UPDATE`

const mysqlReleaseReminderInsertQuery = `INSERT INTO cm_release_reminders (profile_name, apple_email, host_id, host_created_at, release_due_at, owner_email, owner_name, last_extended_by_email, last_extended_by_name, last_extended_at, last_notified_at, released_at, status, auto_release_enabled, auto_release_at, auto_release_started_at, auto_release_last_attempt_at, auto_release_notified_at, auto_release_attempts, auto_release_last_error, auto_release_state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const mysqlReleaseReminderUpdateQuery = `UPDATE cm_release_reminders SET apple_email = ?, host_id = ?, host_created_at = ?, release_due_at = ?, owner_email = ?, owner_name = ?, last_extended_by_email = ?, last_extended_by_name = ?, last_extended_at = ?, last_notified_at = ?, released_at = ?, status = ?, auto_release_enabled = ?, auto_release_at = ?, auto_release_started_at = ?, auto_release_last_attempt_at = ?, auto_release_notified_at = ?, auto_release_attempts = ?, auto_release_last_error = ?, auto_release_state = ?, updated_at = ? WHERE profile_name = ?`

const mysqlProfileOwnerForUpdateQuery = `SELECT o.member_id, COALESCE(m.email, '') FROM cm_profile_owners o LEFT JOIN cm_members m ON m.id = o.member_id WHERE o.profile_name = ? FOR UPDATE`

const mysqlDeleteMatchingProfileOwnerQuery = `DELETE FROM cm_profile_owners WHERE profile_name = ? AND member_id = ?`

const mysqlManagedProfileForUpdateQuery = `SELECT name, COALESCE(apple_email, '') FROM cm_profiles WHERE name = ? FOR UPDATE`

const mysqlMemberByEmailForUpdateQuery = `SELECT id, name, email, username, role, enabled, COALESCE(password_hash, ''), COALESCE(password_salt, ''), COALESCE(api_token_hash, ''), COALESCE(api_token_at, ''), created_at, updated_at FROM cm_members WHERE LOWER(email) = LOWER(?) FOR UPDATE`

const mysqlProfileOwnerUpsertQuery = `INSERT INTO cm_profile_owners (profile_name, member_id, updated_at) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE member_id = VALUES(member_id), updated_at = VALUES(updated_at)`

const mysqlStoreLockName = "member_store"

const mysqlStoreLockForUpdateQuery = `SELECT version FROM cm_store_locks WHERE lock_name = ? FOR UPDATE`

const mysqlStoreLockVersionQuery = `SELECT version FROM cm_store_locks WHERE lock_name = ?`

const mysqlStoreLockAdvanceQuery = `UPDATE cm_store_locks SET version = version + 1 WHERE lock_name = ?`

const mysqlStoreLockTableStatement = `CREATE TABLE IF NOT EXISTS cm_store_locks (
			lock_name VARCHAR(128) PRIMARY KEY,
			version BIGINT UNSIGNED NOT NULL DEFAULT 0
		) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`

const mysqlStoreLockSeedStatement = `INSERT INTO cm_store_locks (lock_name, version) VALUES (?, 0) ON DUPLICATE KEY UPDATE lock_name = VALUES(lock_name)`

var mysqlReleaseReminderMigrationStatements = []string{
	`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_enabled BOOLEAN NOT NULL DEFAULT FALSE`,
	`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_at VARCHAR(64) NULL`,
	`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_started_at VARCHAR(64) NULL`,
	`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_last_attempt_at VARCHAR(64) NULL`,
	`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_notified_at VARCHAR(64) NULL`,
	`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_attempts INT NOT NULL DEFAULT 0`,
	`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_last_error TEXT NULL`,
	`ALTER TABLE cm_release_reminders ADD COLUMN auto_release_state VARCHAR(64) NULL`,
}

var mysqlOperationEventMigrationStatements = []string{
	`ALTER TABLE cm_events ADD COLUMN member_email VARCHAR(255) NULL`,
	`ALTER TABLE cm_events ADD COLUMN member_name VARCHAR(255) NULL`,
	`ALTER TABLE cm_events ADD COLUMN request_id VARCHAR(160) NULL`,
	`ALTER TABLE cm_events ADD COLUMN job_id VARCHAR(160) NULL`,
	`ALTER TABLE cm_events ADD COLUMN cycle_id VARCHAR(80) NULL`,
	`ALTER TABLE cm_events ADD COLUMN session_id_hash VARCHAR(80) NULL`,
	`ALTER TABLE cm_events ADD COLUMN source VARCHAR(64) NULL`,
	`ALTER TABLE cm_events ADD COLUMN phase VARCHAR(64) NULL`,
	`ALTER TABLE cm_events ADD COLUMN target_member_email VARCHAR(255) NULL`,
	`ALTER TABLE cm_events ADD COLUMN error_code VARCHAR(128) NULL`,
	`ALTER TABLE cm_events ADD COLUMN duration_ms BIGINT NOT NULL DEFAULT 0`,
}

var mysqlTransferMigrationStatements = []string{
	`ALTER TABLE cm_transfer_records ADD COLUMN phase VARCHAR(32) NOT NULL DEFAULT ''`,
}

var mysqlOperationEventIndexStatements = []string{
	`CREATE INDEX idx_cm_events_apple_created_id ON cm_events (apple_email, created_at, id)`,
	`CREATE INDEX idx_cm_events_profile_created_id ON cm_events (profile, created_at, id)`,
	`CREATE INDEX idx_cm_events_member_email_created_id ON cm_events (member_email, created_at, id)`,
	`CREATE INDEX idx_cm_events_created_id ON cm_events (created_at, id)`,
}

type mysqlSchemaGuard struct {
	mu      sync.Mutex
	success bool
}

func (g *mysqlSchemaGuard) run(migrate func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.success {
		return nil
	}
	if err := migrate(); err != nil {
		return err
	}
	g.success = true
	return nil
}

var mysqlSchemaGuards sync.Map

func sharedMySQLSchemaGuard(dsn string) *mysqlSchemaGuard {
	guard, _ := mysqlSchemaGuards.LoadOrStore(strings.TrimSpace(dsn), &mysqlSchemaGuard{})
	return guard.(*mysqlSchemaGuard)
}

func NewMySQLMemberStoreFromEnv() (MySQLMemberStore, bool, error) {
	host := strings.TrimSpace(os.Getenv("CONNECTMAC_DB_HOST"))
	port := strings.TrimSpace(os.Getenv("CONNECTMAC_DB_PORT"))
	database := strings.TrimSpace(os.Getenv("CONNECTMAC_DB_DATABASE"))
	username := strings.TrimSpace(os.Getenv("CONNECTMAC_DB_USERNAME"))
	password := os.Getenv("CONNECTMAC_DB_PASSWORD")
	if host == "" && database == "" && username == "" && password == "" {
		return MySQLMemberStore{}, false, nil
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "3306"
	}
	if database == "" || username == "" {
		return MySQLMemberStore{}, true, errors.New("CONNECTMAC_DB_DATABASE and CONNECTMAC_DB_USERNAME are required when MySQL member store is configured")
	}
	dsn := (&mysql.Config{
		User:                 username,
		Passwd:               password,
		Net:                  "tcp",
		Addr:                 host + ":" + port,
		DBName:               database,
		ParseTime:            true,
		Collation:            "utf8mb4_unicode_ci",
		AllowNativePasswords: true,
	}).FormatDSN()
	return MySQLMemberStore{DSN: dsn, Now: time.Now, schemaGuard: sharedMySQLSchemaGuard(dsn), mutationGuard: sharedStoreMutationGuard("mysql:" + strings.TrimSpace(dsn))}, true, nil
}

func (s MySQLMemberStore) normalize() MySQLMemberStore {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.schemaGuard == nil {
		s.schemaGuard = sharedMySQLSchemaGuard(s.DSN)
	}
	if s.mutationGuard == nil {
		s.mutationGuard = sharedStoreMutationGuard("mysql:" + strings.TrimSpace(s.DSN))
	}
	return s
}

func (s MySQLMemberStore) lockMutation() func() {
	s = s.normalize()
	s.mutationGuard.mu.Lock()
	return s.mutationGuard.mu.Unlock
}

func (s MySQLMemberStore) open() (*sql.DB, error) {
	if s.openDB != nil {
		return s.openDB()
	}
	if strings.TrimSpace(s.DSN) == "" {
		return nil, errors.New("mysql dsn is empty")
	}
	db, err := sql.Open("mysql", s.DSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func mysqlColumnExistsError(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1060
}

func mysqlIndexExistsError(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1061
}

func (s MySQLMemberStore) EnsureSchema() error {
	s = s.normalize()
	return s.schemaGuard.run(s.ensureSchema)
}

func (s MySQLMemberStore) ensureSchema() error {
	db, err := s.open()
	if err != nil {
		return err
	}
	defer db.Close()
	for _, stmt := range mysqlSchemaStatements() {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	migrations := []string{
		`ALTER TABLE cm_members ADD COLUMN api_token_hash TEXT NULL`,
		`ALTER TABLE cm_members ADD COLUMN api_token_at VARCHAR(64) NULL`,
	}
	migrations = append(migrations, mysqlReleaseReminderMigrationStatements...)
	migrations = append(migrations, mysqlOperationEventMigrationStatements...)
	migrations = append(migrations, mysqlTransferMigrationStatements...)
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil && !mysqlColumnExistsError(err) {
			return err
		}
	}
	for _, stmt := range mysqlOperationEventIndexStatements {
		if _, err := db.Exec(stmt); err != nil && !mysqlIndexExistsError(err) {
			return err
		}
	}
	if _, err := db.Exec(mysqlStoreLockSeedStatement, mysqlStoreLockName); err != nil {
		return err
	}
	return nil
}

func mysqlSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS cm_members (
			id VARCHAR(128) PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			email VARCHAR(255) NOT NULL UNIQUE,
			username VARCHAR(255) NOT NULL UNIQUE,
			role VARCHAR(32) NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			password_hash TEXT NULL,
			password_salt TEXT NULL,
			api_token_hash TEXT NULL,
			api_token_at VARCHAR(64) NULL,
			created_at VARCHAR(64) NOT NULL,
			updated_at VARCHAR(64) NOT NULL
		) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS cm_assignments (
			apple_email VARCHAR(255) NOT NULL,
			member_id VARCHAR(128) NOT NULL,
			relation VARCHAR(64) NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			PRIMARY KEY (apple_email, member_id),
			INDEX idx_cm_assignments_member_id (member_id)
		) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS cm_profile_owners (
			profile_name VARCHAR(255) PRIMARY KEY,
			member_id VARCHAR(128) NOT NULL,
			updated_at VARCHAR(64) NOT NULL,
			INDEX idx_cm_profile_owners_member_id (member_id)
		) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS cm_profiles (
			name VARCHAR(255) PRIMARY KEY,
			apple_email VARCHAR(255) NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			profile_yaml MEDIUMTEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			updated_at VARCHAR(64) NOT NULL,
			INDEX idx_cm_profiles_apple_email (apple_email)
		) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS cm_profile_members (
			profile_name VARCHAR(255) NOT NULL,
			member_id VARCHAR(128) NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			PRIMARY KEY (profile_name, member_id),
			INDEX idx_cm_profile_members_member_id (member_id)
		) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS cm_release_reminders (
			profile_name VARCHAR(255) PRIMARY KEY,
			apple_email VARCHAR(255) NULL,
			host_id VARCHAR(255) NULL,
			host_created_at VARCHAR(64) NULL,
			release_due_at VARCHAR(64) NULL,
			owner_email VARCHAR(255) NULL,
			owner_name VARCHAR(255) NULL,
			last_extended_by_email VARCHAR(255) NULL,
			last_extended_by_name VARCHAR(255) NULL,
			last_extended_at VARCHAR(64) NULL,
			last_notified_at VARCHAR(64) NULL,
			released_at VARCHAR(64) NULL,
			status VARCHAR(64) NOT NULL,
			auto_release_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			auto_release_at VARCHAR(64) NULL,
			auto_release_started_at VARCHAR(64) NULL,
			auto_release_last_attempt_at VARCHAR(64) NULL,
			auto_release_notified_at VARCHAR(64) NULL,
			auto_release_attempts INT NOT NULL DEFAULT 0,
			auto_release_last_error TEXT NULL,
			auto_release_state VARCHAR(64) NULL,
			created_at VARCHAR(64) NOT NULL,
			updated_at VARCHAR(64) NOT NULL,
			INDEX idx_cm_release_reminders_due (status, release_due_at),
			INDEX idx_cm_release_reminders_apple_email (apple_email)
		) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS cm_transfer_records (
			id VARCHAR(160) PRIMARY KEY,
			member_id VARCHAR(128) NOT NULL,
			member_email VARCHAR(255) NOT NULL,
			profile_name VARCHAR(255) NOT NULL,
			apple_email VARCHAR(255) NULL,
			direction VARCHAR(16) NOT NULL,
			local_path TEXT NOT NULL,
			remote_path TEXT NOT NULL,
			local_job_id VARCHAR(255) NULL,
			status VARCHAR(32) NOT NULL,
			phase VARCHAR(32) NOT NULL DEFAULT '',
			percent INT NOT NULL DEFAULT 0,
			error_summary TEXT NULL,
			created_at VARCHAR(64) NOT NULL,
			started_at VARCHAR(64) NULL,
			finished_at VARCHAR(64) NULL,
			updated_at VARCHAR(64) NOT NULL,
			INDEX idx_cm_transfer_member_profile (member_id, profile_name, updated_at),
			INDEX idx_cm_transfer_member_job (member_id, local_job_id)
		) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`,
		mysqlStoreLockTableStatement,
		`CREATE TABLE IF NOT EXISTS cm_settings (
			setting_key VARCHAR(128) PRIMARY KEY,
			setting_value TEXT NULL
		) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS cm_events (
			id VARCHAR(160) PRIMARY KEY,
			action VARCHAR(128) NOT NULL,
			profile VARCHAR(255) NOT NULL,
			apple_email VARCHAR(255) NULL,
			member_id VARCHAR(128) NULL,
			member_email VARCHAR(255) NULL,
			member_name VARCHAR(255) NULL,
			request_id VARCHAR(160) NULL,
			job_id VARCHAR(160) NULL,
			cycle_id VARCHAR(80) NULL,
			session_id_hash VARCHAR(80) NULL,
			source VARCHAR(64) NULL,
			phase VARCHAR(64) NULL,
			target_member_email VARCHAR(255) NULL,
			error_code VARCHAR(128) NULL,
			duration_ms BIGINT NOT NULL DEFAULT 0,
			confirmed BOOLEAN NOT NULL DEFAULT FALSE,
			status VARCHAR(64) NOT NULL,
			message TEXT NULL,
			created_at VARCHAR(64) NOT NULL,
			INDEX idx_cm_events_apple_email_created_at (apple_email, created_at),
			INDEX idx_cm_events_created_at (created_at),
			INDEX idx_cm_events_apple_created_id (apple_email, created_at, id),
			INDEX idx_cm_events_profile_created_id (profile, created_at, id),
			INDEX idx_cm_events_member_email_created_id (member_email, created_at, id),
			INDEX idx_cm_events_created_id (created_at, id)
		) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`,
	}
}

func (s MySQLMemberStore) Load() (MemberData, error) {
	s = s.normalize()
	if err := s.EnsureSchema(); err != nil {
		return MemberData{}, err
	}
	db, err := s.open()
	if err != nil {
		return MemberData{}, err
	}
	defer db.Close()
	out := MemberData{Members: []Member{}, Assignments: []AppleAccountMember{}, ProfileOwners: []ProfileOwner{}, Profiles: []ManagedProfile{}, ProfileAccess: []ProfileAccess{}, Reminders: []ReleaseReminder{}, TransferRecords: []TransferRecord{}, Events: []OperationEvent{}, Settings: defaultWebSettings()}
	if err := db.QueryRow(mysqlStoreLockVersionQuery, mysqlStoreLockName).Scan(&out.mutationRevision); err != nil {
		return MemberData{}, err
	}
	rows, err := db.Query(`SELECT id, name, email, username, role, enabled, COALESCE(password_hash, ''), COALESCE(password_salt, ''), COALESCE(api_token_hash, ''), COALESCE(api_token_at, ''), created_at, updated_at FROM cm_members ORDER BY email`)
	if err != nil {
		return MemberData{}, err
	}
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.ID, &member.Name, &member.Email, &member.Username, &member.Role, &member.Enabled, &member.PasswordHash, &member.PasswordSalt, &member.APITokenHash, &member.APITokenAt, &member.CreatedAt, &member.UpdatedAt); err != nil {
			rows.Close()
			return MemberData{}, err
		}
		out.Members = append(out.Members, member)
	}
	if err := rows.Close(); err != nil {
		return MemberData{}, err
	}
	rows, err = db.Query(`SELECT apple_email, member_id, relation, created_at FROM cm_assignments ORDER BY apple_email, member_id`)
	if err != nil {
		return MemberData{}, err
	}
	for rows.Next() {
		var assignment AppleAccountMember
		if err := rows.Scan(&assignment.AppleEmail, &assignment.MemberID, &assignment.Relation, &assignment.CreatedAt); err != nil {
			rows.Close()
			return MemberData{}, err
		}
		out.Assignments = append(out.Assignments, assignment)
	}
	if err := rows.Close(); err != nil {
		return MemberData{}, err
	}
	rows, err = db.Query(`SELECT profile_name, member_id, updated_at FROM cm_profile_owners ORDER BY profile_name`)
	if err != nil {
		return MemberData{}, err
	}
	for rows.Next() {
		var owner ProfileOwner
		if err := rows.Scan(&owner.ProfileName, &owner.MemberID, &owner.UpdatedAt); err != nil {
			rows.Close()
			return MemberData{}, err
		}
		out.ProfileOwners = append(out.ProfileOwners, owner)
	}
	if err := rows.Close(); err != nil {
		return MemberData{}, err
	}
	rows, err = db.Query(`SELECT name, COALESCE(apple_email, ''), enabled, profile_yaml, created_at, updated_at FROM cm_profiles ORDER BY name`)
	if err != nil {
		return MemberData{}, err
	}
	for rows.Next() {
		var profile ManagedProfile
		if err := rows.Scan(&profile.Name, &profile.AppleEmail, &profile.Enabled, &profile.ProfileYAML, &profile.CreatedAt, &profile.UpdatedAt); err != nil {
			rows.Close()
			return MemberData{}, err
		}
		out.Profiles = append(out.Profiles, profile)
	}
	if err := rows.Close(); err != nil {
		return MemberData{}, err
	}
	rows, err = db.Query(`SELECT profile_name, member_id, created_at FROM cm_profile_members ORDER BY profile_name, member_id`)
	if err != nil {
		return MemberData{}, err
	}
	for rows.Next() {
		var access ProfileAccess
		if err := rows.Scan(&access.ProfileName, &access.MemberID, &access.CreatedAt); err != nil {
			rows.Close()
			return MemberData{}, err
		}
		out.ProfileAccess = append(out.ProfileAccess, access)
	}
	if err := rows.Close(); err != nil {
		return MemberData{}, err
	}
	rows, err = db.Query(`SELECT ` + mysqlReleaseReminderSelectColumns + ` FROM cm_release_reminders ORDER BY profile_name`)
	if err != nil {
		return MemberData{}, err
	}
	for rows.Next() {
		var reminder ReleaseReminder
		if err := scanMySQLReleaseReminder(rows, &reminder); err != nil {
			rows.Close()
			return MemberData{}, err
		}
		out.Reminders = append(out.Reminders, reminder)
	}
	if err := rows.Close(); err != nil {
		return MemberData{}, err
	}
	rows, err = db.Query(`SELECT ` + mysqlTransferSelectColumns + ` FROM cm_transfer_records ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return MemberData{}, err
	}
	for rows.Next() {
		var record TransferRecord
		if err := scanMySQLTransferRecord(rows, &record); err != nil {
			rows.Close()
			return MemberData{}, err
		}
		out.TransferRecords = append(out.TransferRecords, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return MemberData{}, err
	}
	if err := rows.Close(); err != nil {
		return MemberData{}, err
	}
	rows, err = db.Query(`SELECT setting_key, COALESCE(setting_value, '') FROM cm_settings`)
	if err != nil {
		return MemberData{}, err
	}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return MemberData{}, err
		}
		switch key {
		case "auth_secret":
			out.Settings.AuthSecret = value
		case "default_owner_email":
			out.Settings.DefaultOwnerEmail = value
		case "default_status_filter":
			out.Settings.DefaultStatusFilter = value
		case "background_confirm":
			out.Settings.BackgroundConfirm = value == "true"
		case "show_released":
			out.Settings.ShowReleased = value == "true"
		}
	}
	if err := rows.Close(); err != nil {
		return MemberData{}, err
	}
	normalizeMemberData(&out)
	return out, nil
}

func (s MySQLMemberStore) Save(data MemberData) error {
	s = s.normalize()
	defer s.lockMutation()()
	return s.saveUnlocked(data)
}

func (s MySQLMemberStore) saveUnlocked(data MemberData) error {
	s = s.normalize()
	normalizeMemberData(&data)
	if err := s.EnsureSchema(); err != nil {
		return err
	}
	db, err := s.open()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	storeTx := sqlMySQLReleaseReminderTransaction{tx: tx}
	data, err = prepareMySQLWholeStoreSave(storeTx, data)
	if err != nil {
		return err
	}
	for _, member := range data.Members {
		if _, err := tx.Exec(`INSERT INTO cm_members (id, name, email, username, role, enabled, password_hash, password_salt, api_token_hash, api_token_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			member.ID, member.Name, member.Email, member.Username, member.Role, member.Enabled, member.PasswordHash, member.PasswordSalt, member.APITokenHash, member.APITokenAt, member.CreatedAt, member.UpdatedAt); err != nil {
			return err
		}
	}
	for _, assignment := range data.Assignments {
		if _, err := tx.Exec(`INSERT INTO cm_assignments (apple_email, member_id, relation, created_at) VALUES (?, ?, ?, ?)`,
			assignment.AppleEmail, assignment.MemberID, assignment.Relation, assignment.CreatedAt); err != nil {
			return err
		}
	}
	for _, owner := range data.ProfileOwners {
		if _, err := tx.Exec(`INSERT INTO cm_profile_owners (profile_name, member_id, updated_at) VALUES (?, ?, ?)`,
			owner.ProfileName, owner.MemberID, owner.UpdatedAt); err != nil {
			return err
		}
	}
	for _, profile := range data.Profiles {
		if _, err := tx.Exec(`INSERT INTO cm_profiles (name, apple_email, enabled, profile_yaml, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			profile.Name, profile.AppleEmail, profile.Enabled, profile.ProfileYAML, profile.CreatedAt, profile.UpdatedAt); err != nil {
			return err
		}
	}
	for _, access := range data.ProfileAccess {
		if _, err := tx.Exec(`INSERT INTO cm_profile_members (profile_name, member_id, created_at) VALUES (?, ?, ?)`,
			access.ProfileName, access.MemberID, access.CreatedAt); err != nil {
			return err
		}
	}
	for _, reminder := range data.Reminders {
		if err := insertMySQLReleaseReminder(sqlMySQLReleaseReminderTransaction{tx: tx}, reminder); err != nil {
			return err
		}
	}
	for _, record := range data.TransferRecords {
		if err := insertMySQLTransferRecord(sqlMySQLReleaseReminderTransaction{tx: tx}, record); err != nil {
			return err
		}
	}
	settings := map[string]string{
		"auth_secret":           data.Settings.AuthSecret,
		"default_owner_email":   data.Settings.DefaultOwnerEmail,
		"default_status_filter": data.Settings.DefaultStatusFilter,
		"background_confirm":    fmt.Sprintf("%t", data.Settings.BackgroundConfirm),
		"show_released":         fmt.Sprintf("%t", data.Settings.ShowReleased),
	}
	for key, value := range settings {
		if _, err := tx.Exec(`INSERT INTO cm_settings (setting_key, setting_value) VALUES (?, ?)`, key, value); err != nil {
			return err
		}
	}
	if err := advanceMySQLStoreLock(storeTx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s MySQLMemberStore) CreateTransferRecord(memberID string, record TransferRecord) (created TransferRecord, err error) {
	s = s.normalize()
	memberID = strings.TrimSpace(memberID)
	if memberID == "" {
		return TransferRecord{}, errors.New("member ID is required")
	}
	if strings.TrimSpace(record.MemberID) != memberID {
		return TransferRecord{}, errors.New("transfer record member ID does not match")
	}
	record.MemberID = memberID
	record.MemberEmail = normalizeEmail(record.MemberEmail)
	record.ProfileName = strings.TrimSpace(record.ProfileName)
	record.AppleEmail = normalizeEmail(record.AppleEmail)
	record.Direction = strings.TrimSpace(strings.ToLower(record.Direction))
	record.Status = strings.TrimSpace(strings.ToLower(record.Status))
	record.Phase = strings.TrimSpace(strings.ToLower(record.Phase))
	if err := validateTransferRecord(record); err != nil {
		return TransferRecord{}, err
	}
	suffix, err := randomToken(12)
	if err != nil {
		return TransferRecord{}, fmt.Errorf("generate transfer record ID: %w", err)
	}
	now := s.currentTime()
	record.ID = fmt.Sprintf("transfer-%d-%s", now.UnixNano(), suffix)
	record.CreatedAt = now.Format(time.RFC3339)
	record.UpdatedAt = record.CreatedAt

	if err := s.EnsureSchema(); err != nil {
		return TransferRecord{}, err
	}
	db, err := s.open()
	if err != nil {
		return TransferRecord{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return TransferRecord{}, err
	}
	return createTransferRecordInMySQLTransaction(sqlMySQLReleaseReminderTransaction{tx: tx}, record)
}

func (s MySQLMemberStore) ListTransferRecords(memberID, profileName string) ([]TransferRecord, error) {
	s = s.normalize()
	memberID = strings.TrimSpace(memberID)
	if memberID == "" {
		return nil, errors.New("member ID is required")
	}
	profileName = strings.TrimSpace(profileName)
	if err := s.EnsureSchema(); err != nil {
		return nil, err
	}
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := mysqlTransferListQuery
	args := []any{memberID}
	if profileName != "" {
		query = mysqlTransferListByProfileQuery
		args = append(args, profileName)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]TransferRecord, 0)
	for rows.Next() {
		var record TransferRecord
		if err := scanMySQLTransferRecord(rows, &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (s MySQLMemberStore) UpdateTransferRecord(memberID, recordID, localJobID string, update func(TransferRecord) (TransferRecord, error)) (TransferRecord, error) {
	s = s.normalize()
	memberID = strings.TrimSpace(memberID)
	recordID = strings.TrimSpace(recordID)
	localJobID = strings.TrimSpace(localJobID)
	if memberID == "" {
		return TransferRecord{}, errors.New("member ID is required")
	}
	if recordID == "" {
		return TransferRecord{}, errors.New("transfer record ID is required")
	}
	if update == nil {
		return TransferRecord{}, errors.New("transfer record update is required")
	}
	if err := s.EnsureSchema(); err != nil {
		return TransferRecord{}, err
	}
	db, err := s.open()
	if err != nil {
		return TransferRecord{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return TransferRecord{}, err
	}
	return updateTransferRecordInMySQLTransaction(sqlMySQLReleaseReminderTransaction{tx: tx}, memberID, recordID, localJobID, s.currentTime(), update)
}

func (s MySQLMemberStore) DeleteTransferRecord(memberID, recordID string) error {
	s = s.normalize()
	memberID = strings.TrimSpace(memberID)
	recordID = strings.TrimSpace(recordID)
	if memberID == "" {
		return errors.New("member ID is required")
	}
	if recordID == "" {
		return errors.New("transfer record ID is required")
	}
	if err := s.EnsureSchema(); err != nil {
		return err
	}
	db, err := s.open()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	return deleteTransferRecordInMySQLTransaction(sqlMySQLReleaseReminderTransaction{tx: tx}, memberID, recordID)
}

func (s MySQLMemberStore) ListMembers() ([]MemberWithAssignments, error) {
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	out := make([]MemberWithAssignments, 0, len(db.Members))
	for _, member := range db.Members {
		item := MemberWithAssignments{PublicMember: publicMember(member)}
		for _, assignment := range db.Assignments {
			if assignment.MemberID == member.ID {
				item.Assignments = append(item.Assignments, assignment)
			}
		}
		for _, access := range db.ProfileAccess {
			if access.MemberID == member.ID {
				item.Profiles = append(item.Profiles, access.ProfileName)
			}
		}
		sort.Strings(item.Profiles)
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Email) < strings.ToLower(out[j].Email)
	})
	return out, nil
}

type unlockedMySQLMemberStore struct {
	store MySQLMemberStore
}

func (s MySQLMemberStore) unlocked() unlockedMySQLMemberStore {
	return unlockedMySQLMemberStore{store: s}
}

func (s unlockedMySQLMemberStore) Load() (MemberData, error) {
	return s.store.Load()
}

func (s unlockedMySQLMemberStore) Save(data MemberData) error {
	return s.store.saveUnlocked(data)
}

func (s unlockedMySQLMemberStore) currentTime() time.Time {
	return s.store.currentTime()
}

func (s MySQLMemberStore) AddMember(name, email, role string) (Member, error) {
	defer s.lockMutation()()
	return addMemberToStore(s.unlocked(), name, email, role)
}

func (s MySQLMemberStore) AddMemberWithPassword(name, email, role, password string) (Member, error) {
	member, err := s.AddMember(name, email, role)
	if err != nil {
		return Member{}, err
	}
	if err := s.SetMemberPassword(member.Email, password); err != nil {
		return Member{}, err
	}
	db, err := s.Load()
	if err != nil {
		return Member{}, err
	}
	member, _ = findMemberByEmailOrUsername(db, member.Email)
	return member, nil
}

func (s MySQLMemberStore) SetupAdmin(name, email, password string) (Member, error) {
	defer s.lockMutation()()
	return setupAdminInStore(s.unlocked(), name, email, password)
}

func (s MySQLMemberStore) SetMemberPassword(emailOrUsername, password string) error {
	defer s.lockMutation()()
	return setMemberPasswordInStore(s.unlocked(), emailOrUsername, password)
}

func (s MySQLMemberStore) UpdateMember(emailOrUsername, name, email, role string) (Member, error) {
	defer s.lockMutation()()
	return updateMemberInStore(s.unlocked(), emailOrUsername, name, email, role)
}

func (s MySQLMemberStore) UpdateMemberEmail(memberID, newEmail string) (Member, error) {
	defer s.lockMutation()()
	return updateMemberEmailInStore(s.unlocked(), memberID, newEmail)
}

func (s MySQLMemberStore) VerifyMemberPassword(emailOrUsername, password string) (Member, bool, error) {
	return verifyMemberPasswordInStore(s, emailOrUsername, password)
}

func (s MySQLMemberStore) HasPasswordMembers() (bool, error) {
	return hasPasswordMembersInStore(s)
}

func (s MySQLMemberStore) SetMemberEnabled(email string, enabled bool) (Member, error) {
	defer s.lockMutation()()
	return setMemberEnabledInStore(s.unlocked(), email, enabled)
}

func (s MySQLMemberStore) AssignMember(appleEmail, memberEmail, relation string) (AppleAccountMember, error) {
	defer s.lockMutation()()
	return assignMemberInStore(s.unlocked(), appleEmail, memberEmail, relation)
}

func (s MySQLMemberStore) UnassignMember(appleEmail, memberEmail string) error {
	defer s.lockMutation()()
	return unassignMemberInStore(s.unlocked(), appleEmail, memberEmail)
}

func (s MySQLMemberStore) MembersForApple(appleEmail string) ([]PublicMember, error) {
	return membersForAppleInStore(s, appleEmail)
}

func (s MySQLMemberStore) ProfileOwners() ([]PublicProfileOwner, error) {
	return profileOwnersInStore(s)
}

func (s MySQLMemberStore) ProfileOwner(profileName string) (PublicProfileOwner, bool, error) {
	return profileOwnerInStore(s, profileName)
}

func (s MySQLMemberStore) SetProfileOwner(profileName, memberEmail string) (PublicProfileOwner, error) {
	defer s.lockMutation()()
	return setProfileOwnerInStore(s.unlocked(), profileName, memberEmail)
}

func (s MySQLMemberStore) SetProfileOwnerAndRecordEvent(profileName, memberEmail string, event OperationEvent) (PublicProfileOwner, error) {
	defer s.lockMutation()()
	if err := s.EnsureSchema(); err != nil {
		return PublicProfileOwner{}, err
	}
	db, err := s.open()
	if err != nil {
		return PublicProfileOwner{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return PublicProfileOwner{}, err
	}
	return setProfileOwnerAndRecordEventInMySQLTransaction(
		sqlMySQLReleaseReminderTransaction{tx: tx},
		profileName,
		memberEmail,
		event,
		s.currentTime(),
	)
}

func (s MySQLMemberStore) ClearProfileOwner(profileName string) error {
	defer s.lockMutation()()
	return clearProfileOwnerInStore(s.unlocked(), profileName)
}

func (s MySQLMemberStore) ListManagedProfiles(memberEmail string) ([]ManagedProfile, error) {
	return listManagedProfilesInStore(s, memberEmail)
}

func (s MySQLMemberStore) UpsertManagedProfile(profile Profile) (ManagedProfile, error) {
	defer s.lockMutation()()
	return upsertManagedProfileInStore(s.unlocked(), profile)
}

func (s MySQLMemberStore) SetManagedProfileEnabled(profileName string, enabled bool) (ManagedProfile, error) {
	defer s.lockMutation()()
	return setManagedProfileEnabledInStore(s.unlocked(), profileName, enabled)
}

func (s MySQLMemberStore) DeleteManagedProfile(profileName string) error {
	defer s.lockMutation()()
	return deleteManagedProfileInStore(s.unlocked(), profileName)
}

func (s MySQLMemberStore) AssignProfileAccess(profileName, memberEmail string) (ProfileAccess, error) {
	defer s.lockMutation()()
	return assignProfileAccessInStore(s.unlocked(), profileName, memberEmail)
}

func (s MySQLMemberStore) UnassignProfileAccess(profileName, memberEmail string) error {
	defer s.lockMutation()()
	return unassignProfileAccessInStore(s.unlocked(), profileName, memberEmail)
}

func (s MySQLMemberStore) SetMemberProfileAccess(memberEmail string, profileNames []string) ([]ProfileAccess, error) {
	defer s.lockMutation()()
	return setMemberProfileAccessInStore(s.unlocked(), memberEmail, profileNames)
}

func (s MySQLMemberStore) MembersForProfile(profileName string) ([]PublicMember, error) {
	return membersForProfileInStore(s, profileName)
}

func (s MySQLMemberStore) ListReleaseReminders(memberEmail string) ([]ReleaseReminder, error) {
	return listReleaseRemindersInStore(s, memberEmail)
}

func (s MySQLMemberStore) ReleaseReminder(profileName string) (ReleaseReminder, bool, error) {
	return releaseReminderInStore(s, profileName)
}

func (s MySQLMemberStore) UpsertReleaseReminder(reminder ReleaseReminder) (ReleaseReminder, error) {
	defer s.lockMutation()()
	reminder = normalizeReleaseReminder(reminder)
	if reminder.ProfileName == "" {
		return ReleaseReminder{}, errors.New("profile is required")
	}
	if err := s.EnsureSchema(); err != nil {
		return ReleaseReminder{}, err
	}
	db, err := s.open()
	if err != nil {
		return ReleaseReminder{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return ReleaseReminder{}, err
	}
	result, err := upsertReleaseReminderInMySQLTransaction(sqlMySQLReleaseReminderTransaction{tx: tx}, reminder, s.currentTime())
	if err != nil {
		return ReleaseReminder{}, err
	}
	return result, nil
}

func (s MySQLMemberStore) UpdateReleaseReminder(profileName string, update func(ReleaseReminder) (ReleaseReminder, error)) (ReleaseReminder, error) {
	defer s.lockMutation()()
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return ReleaseReminder{}, errors.New("profile is required")
	}
	if update == nil {
		return ReleaseReminder{}, errors.New("release reminder update is required")
	}
	if err := s.EnsureSchema(); err != nil {
		return ReleaseReminder{}, err
	}
	db, err := s.open()
	if err != nil {
		return ReleaseReminder{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return ReleaseReminder{}, err
	}
	result, err := updateReleaseReminderInMySQLTransaction(sqlMySQLReleaseReminderTransaction{tx: tx}, profileName, s.currentTime(), update)
	if err != nil {
		return ReleaseReminder{}, err
	}
	return result, nil
}

func (s MySQLMemberStore) UpdateReleaseReminderAndRecordEvent(profileName string, update func(ReleaseReminder) (ReleaseReminder, error), event OperationEvent) (ReleaseReminder, error) {
	defer s.lockMutation()()
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return ReleaseReminder{}, errors.New("profile is required")
	}
	if update == nil {
		return ReleaseReminder{}, errors.New("release reminder update is required")
	}
	if err := s.EnsureSchema(); err != nil {
		return ReleaseReminder{}, err
	}
	db, err := s.open()
	if err != nil {
		return ReleaseReminder{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return ReleaseReminder{}, err
	}
	return updateReleaseReminderAndRecordEventInMySQLTransaction(sqlMySQLReleaseReminderTransaction{tx: tx}, profileName, s.currentTime(), update, event)
}

func (s MySQLMemberStore) CompleteAutoRelease(cycle ReleaseReminderCycle, releasedAt string) (ReleaseReminder, error) {
	defer s.lockMutation()()
	if err := s.EnsureSchema(); err != nil {
		return ReleaseReminder{}, err
	}
	db, err := s.open()
	if err != nil {
		return ReleaseReminder{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return ReleaseReminder{}, err
	}
	if releasedAt == "" {
		releasedAt = s.currentTime().Format(time.RFC3339)
	}
	releasedTime, err := time.Parse(time.RFC3339, releasedAt)
	if err != nil {
		_ = tx.Rollback()
		return ReleaseReminder{}, err
	}
	return completeAutoReleaseInMySQLTransaction(sqlMySQLReleaseReminderTransaction{tx: tx}, cycle, releasedTime)
}

func (s MySQLMemberStore) MarkAutoReleaseNotified(cycle ReleaseReminderCycle, notifiedAt string) (ReleaseReminder, error) {
	defer s.lockMutation()()
	now := s.currentTime()
	notifiedAt, err := resolveAutoReleaseNotifiedAt(notifiedAt, now)
	if err != nil {
		return ReleaseReminder{}, err
	}
	if err := s.EnsureSchema(); err != nil {
		return ReleaseReminder{}, err
	}
	db, err := s.open()
	if err != nil {
		return ReleaseReminder{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return ReleaseReminder{}, err
	}
	return markAutoReleaseNotifiedInMySQLTransaction(
		sqlMySQLReleaseReminderTransaction{tx: tx},
		cycle,
		notifiedAt,
		now,
	)
}

func (s MySQLMemberStore) CleanupProfileRecords(profileName, releasedAt, reason string) (ReleaseReminder, bool, error) {
	return s.cleanupProfileRecordsAndMaybeRecordEvent(profileName, releasedAt, reason, nil)
}

func (s MySQLMemberStore) CleanupProfileRecordsAndRecordEvent(profileName, releasedAt, reason string, event OperationEvent) (ReleaseReminder, bool, error) {
	return s.cleanupProfileRecordsAndMaybeRecordEvent(profileName, releasedAt, reason, &event)
}

func (s MySQLMemberStore) cleanupProfileRecordsAndMaybeRecordEvent(profileName, releasedAt, reason string, event *OperationEvent) (ReleaseReminder, bool, error) {
	defer s.lockMutation()()
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return ReleaseReminder{}, false, errors.New("profile is required")
	}
	if err := s.EnsureSchema(); err != nil {
		return ReleaseReminder{}, false, err
	}
	db, err := s.open()
	if err != nil {
		return ReleaseReminder{}, false, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return ReleaseReminder{}, false, err
	}
	if releasedAt == "" {
		releasedAt = s.currentTime().Format(time.RFC3339)
	}
	return cleanupProfileRecordsAndMaybeEventInMySQLTransaction(
		sqlMySQLReleaseReminderTransaction{tx: tx},
		profileName,
		releasedAt,
		reason,
		s.currentTime(),
		event,
	)
}

type mysqlReleaseReminderExecer interface {
	Exec(query string, args ...any) error
}

type mysqlReleaseReminderTransaction interface {
	mysqlReleaseReminderExecer
	QueryRow(query string, args ...any) mysqlReleaseReminderScanner
	Commit() error
	Rollback() error
}

type mysqlRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

type mysqlStoreTransaction interface {
	mysqlReleaseReminderTransaction
	Query(query string, args ...any) (mysqlRows, error)
}

type mysqlTransferDeleteTransaction interface {
	mysqlReleaseReminderTransaction
	ExecResult(query string, args ...any) (sql.Result, error)
}

type sqlMySQLReleaseReminderTransaction struct {
	tx *sql.Tx
}

func (tx sqlMySQLReleaseReminderTransaction) QueryRow(query string, args ...any) mysqlReleaseReminderScanner {
	return tx.tx.QueryRow(query, args...)
}

func (tx sqlMySQLReleaseReminderTransaction) Query(query string, args ...any) (mysqlRows, error) {
	return tx.tx.Query(query, args...)
}

func (tx sqlMySQLReleaseReminderTransaction) Exec(query string, args ...any) error {
	_, err := tx.tx.Exec(query, args...)
	return err
}

func (tx sqlMySQLReleaseReminderTransaction) ExecResult(query string, args ...any) (sql.Result, error) {
	return tx.tx.Exec(query, args...)
}

func (tx sqlMySQLReleaseReminderTransaction) Commit() error {
	return tx.tx.Commit()
}

func (tx sqlMySQLReleaseReminderTransaction) Rollback() error {
	return tx.tx.Rollback()
}

func lockMySQLStore(tx mysqlReleaseReminderTransaction) (uint64, error) {
	var version uint64
	if err := tx.QueryRow(mysqlStoreLockForUpdateQuery, mysqlStoreLockName).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

func advanceMySQLStoreLock(tx mysqlReleaseReminderExecer) error {
	return tx.Exec(mysqlStoreLockAdvanceQuery, mysqlStoreLockName)
}

func prepareMySQLWholeStoreSave(tx mysqlStoreTransaction, data MemberData) (result MemberData, err error) {
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	version, err := lockMySQLStore(tx)
	if err != nil {
		return MemberData{}, err
	}
	if data.mutationRevision != version {
		rows, err := tx.Query(`SELECT ` + mysqlReleaseReminderSelectColumns + ` FROM cm_release_reminders ORDER BY profile_name`)
		if err != nil {
			return MemberData{}, err
		}
		var reminders []ReleaseReminder
		for rows.Next() {
			var reminder ReleaseReminder
			if err := scanMySQLReleaseReminder(rows, &reminder); err != nil {
				rows.Close()
				return MemberData{}, err
			}
			reminders = append(reminders, reminder)
		}
		iterationErr := rows.Err()
		closeErr := rows.Close()
		if iterationErr != nil {
			return MemberData{}, iterationErr
		}
		if closeErr != nil {
			return MemberData{}, closeErr
		}
		data.Reminders = reminders
		rows, err = tx.Query(`SELECT ` + mysqlTransferSelectColumns + ` FROM cm_transfer_records ORDER BY updated_at DESC, id DESC`)
		if err != nil {
			return MemberData{}, err
		}
		var transferRecords []TransferRecord
		for rows.Next() {
			var record TransferRecord
			if err := scanMySQLTransferRecord(rows, &record); err != nil {
				rows.Close()
				return MemberData{}, err
			}
			transferRecords = append(transferRecords, record)
		}
		iterationErr = rows.Err()
		closeErr = rows.Close()
		if iterationErr != nil {
			return MemberData{}, iterationErr
		}
		if closeErr != nil {
			return MemberData{}, closeErr
		}
		data.TransferRecords = transferRecords
	}
	for _, table := range []string{"cm_transfer_records", "cm_release_reminders", "cm_profile_members", "cm_profiles", "cm_profile_owners", "cm_assignments", "cm_members", "cm_settings"} {
		if err := tx.Exec("DELETE FROM " + table); err != nil {
			return MemberData{}, err
		}
	}
	return data, nil
}

func insertMySQLTransferRecord(execer mysqlReleaseReminderExecer, record TransferRecord) error {
	if record.Phase != "" {
		return execer.Exec(mysqlTransferInsertWithPhaseQuery,
			record.ID, record.MemberID, record.MemberEmail, record.ProfileName, record.AppleEmail,
			record.Direction, record.LocalPath, record.RemotePath, record.LocalJobID, record.Status,
			record.Phase, record.Percent, record.ErrorSummary, record.CreatedAt, record.StartedAt, record.FinishedAt, record.UpdatedAt)
	}
	return execer.Exec(mysqlTransferInsertQuery,
		record.ID, record.MemberID, record.MemberEmail, record.ProfileName, record.AppleEmail,
		record.Direction, record.LocalPath, record.RemotePath, record.LocalJobID, record.Status,
		record.Percent, record.ErrorSummary, record.CreatedAt, record.StartedAt, record.FinishedAt, record.UpdatedAt)
}

func createTransferRecordInMySQLTransaction(tx mysqlReleaseReminderTransaction, record TransferRecord) (created TransferRecord, err error) {
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); err == nil && rollbackErr != nil {
			err = rollbackErr
		}
	}()
	if _, err = lockMySQLStore(tx); err != nil {
		return TransferRecord{}, err
	}
	if err = insertMySQLTransferRecord(tx, record); err != nil {
		return TransferRecord{}, err
	}
	if err = advanceMySQLStoreLock(tx); err != nil {
		return TransferRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return TransferRecord{}, err
	}
	committed = true
	return record, nil
}

func updateTransferRecordInMySQLTransaction(tx mysqlReleaseReminderTransaction, memberID, recordID, localJobID string, now time.Time, update func(TransferRecord) (TransferRecord, error)) (updated TransferRecord, err error) {
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); err == nil && rollbackErr != nil {
			err = rollbackErr
		}
	}()
	if _, err = lockMySQLStore(tx); err != nil {
		return TransferRecord{}, err
	}
	var current TransferRecord
	if err = scanMySQLTransferRecord(tx.QueryRow(mysqlTransferSelectForUpdateQuery, recordID, memberID), &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TransferRecord{}, ErrTransferRecordNotFound
		}
		return TransferRecord{}, err
	}
	if current.LocalJobID != "" && current.LocalJobID != localJobID {
		return TransferRecord{}, ErrTransferRecordNotFound
	}
	updated, err = update(current)
	if err != nil {
		return TransferRecord{}, err
	}
	if updated.ID != current.ID {
		return TransferRecord{}, errors.New("transfer record ID cannot change")
	}
	if updated.MemberID != memberID {
		return TransferRecord{}, errors.New("transfer record member ID does not match")
	}
	updated.MemberEmail = normalizeEmail(updated.MemberEmail)
	updated.ProfileName = strings.TrimSpace(updated.ProfileName)
	updated.AppleEmail = normalizeEmail(updated.AppleEmail)
	updated.LocalJobID = strings.TrimSpace(updated.LocalJobID)
	if current.LocalJobID == "" {
		if updated.LocalJobID != localJobID {
			return TransferRecord{}, errors.New("transfer record local job ID does not match")
		}
	} else if updated.LocalJobID != current.LocalJobID {
		return TransferRecord{}, errors.New("transfer record local job ID cannot change")
	}
	updated.Direction = strings.TrimSpace(strings.ToLower(updated.Direction))
	updated.Status = strings.TrimSpace(strings.ToLower(updated.Status))
	updated.Phase = strings.TrimSpace(strings.ToLower(updated.Phase))
	updated.ErrorSummary = strings.TrimSpace(updated.ErrorSummary)
	if err = validateTransferRecord(updated); err != nil {
		return TransferRecord{}, err
	}
	if isTerminalTransferStatus(current.Status) {
		if !terminalTransferSemanticallyEqual(current, updated) {
			return TransferRecord{}, errors.New("terminal transfer record cannot be updated")
		}
		if err = tx.Commit(); err != nil {
			return TransferRecord{}, err
		}
		committed = true
		return current, nil
	}
	if updated.Percent < current.Percent {
		return TransferRecord{}, errors.New("transfer percent cannot regress")
	}
	if isTerminalTransferStatus(current.Status) && isActiveTransferStatus(updated.Status) {
		return TransferRecord{}, errors.New("terminal transfer status cannot return to active")
	}
	updated.CreatedAt = current.CreatedAt
	updated.UpdatedAt = now.Format(time.RFC3339)
	if err = tx.Exec(mysqlTransferUpdateQuery,
		updated.MemberEmail, updated.ProfileName, updated.AppleEmail, updated.Direction,
		updated.LocalPath, updated.RemotePath, updated.LocalJobID, updated.Status,
		updated.Percent, updated.ErrorSummary, updated.StartedAt, updated.FinishedAt, updated.UpdatedAt,
		recordID, memberID); err != nil {
		return TransferRecord{}, err
	}
	if updated.Phase != "" {
		if err = tx.Exec(mysqlTransferPhaseUpdateQuery, updated.Phase, recordID, memberID); err != nil {
			return TransferRecord{}, err
		}
	}
	if err = advanceMySQLStoreLock(tx); err != nil {
		return TransferRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return TransferRecord{}, err
	}
	committed = true
	return updated, nil
}

func deleteTransferRecordInMySQLTransaction(tx mysqlTransferDeleteTransaction, memberID, recordID string) (err error) {
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); err == nil && rollbackErr != nil {
			err = rollbackErr
		}
	}()
	if _, err = lockMySQLStore(tx); err != nil {
		return err
	}
	result, err := tx.ExecResult(mysqlTransferDeleteQuery, recordID, memberID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrTransferRecordNotFound
	}
	if err = advanceMySQLStoreLock(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func insertMySQLReleaseReminder(execer mysqlReleaseReminderExecer, reminder ReleaseReminder) error {
	return execer.Exec(mysqlReleaseReminderInsertQuery,
		reminder.ProfileName, reminder.AppleEmail, reminder.HostID, reminder.HostCreatedAt, reminder.ReleaseDueAt, reminder.OwnerEmail, reminder.OwnerName, reminder.LastExtendedByEmail, reminder.LastExtendedByName, reminder.LastExtendedAt, reminder.LastNotifiedAt, reminder.ReleasedAt, reminder.Status, reminder.AutoReleaseEnabled, reminder.AutoReleaseAt, reminder.AutoReleaseStartedAt, reminder.AutoReleaseLastAttemptAt, reminder.AutoReleaseNotifiedAt, reminder.AutoReleaseAttempts, reminder.AutoReleaseLastError, reminder.AutoReleaseState, reminder.CreatedAt, reminder.UpdatedAt)
}

func updateMySQLReleaseReminder(execer mysqlReleaseReminderExecer, profileName string, reminder ReleaseReminder) error {
	return execer.Exec(mysqlReleaseReminderUpdateQuery,
		reminder.AppleEmail, reminder.HostID, reminder.HostCreatedAt, reminder.ReleaseDueAt, reminder.OwnerEmail, reminder.OwnerName, reminder.LastExtendedByEmail, reminder.LastExtendedByName, reminder.LastExtendedAt, reminder.LastNotifiedAt, reminder.ReleasedAt, reminder.Status, reminder.AutoReleaseEnabled, reminder.AutoReleaseAt, reminder.AutoReleaseStartedAt, reminder.AutoReleaseLastAttemptAt, reminder.AutoReleaseNotifiedAt, reminder.AutoReleaseAttempts, reminder.AutoReleaseLastError, reminder.AutoReleaseState, reminder.UpdatedAt, profileName)
}

func upsertReleaseReminderInMySQLTransaction(tx mysqlReleaseReminderTransaction, reminder ReleaseReminder, now time.Time) (result ReleaseReminder, err error) {
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); err == nil && rollbackErr != nil {
			err = rollbackErr
		}
	}()

	if _, err = lockMySQLStore(tx); err != nil {
		return ReleaseReminder{}, err
	}
	var current ReleaseReminder
	err = scanMySQLReleaseReminder(tx.QueryRow(mysqlReleaseReminderSelectForUpdate, reminder.ProfileName), &current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if reminder.CreatedAt == "" {
			reminder.CreatedAt = now.Format(time.RFC3339)
		}
		reminder.UpdatedAt = now.Format(time.RFC3339)
		if err = insertMySQLReleaseReminder(tx, reminder); err != nil {
			return ReleaseReminder{}, err
		}
		result = reminder
	case err != nil:
		return ReleaseReminder{}, err
	default:
		result = mergeReleaseReminderUpsert(current, reminder, now.Format(time.RFC3339))
		if err = updateMySQLReleaseReminder(tx, current.ProfileName, result); err != nil {
			return ReleaseReminder{}, err
		}
	}
	if err = advanceMySQLStoreLock(tx); err != nil {
		return ReleaseReminder{}, err
	}
	if err = tx.Commit(); err != nil {
		return ReleaseReminder{}, err
	}
	committed = true
	return result, nil
}

func updateReleaseReminderInMySQLTransaction(tx mysqlReleaseReminderTransaction, profileName string, now time.Time, update func(ReleaseReminder) (ReleaseReminder, error)) (updated ReleaseReminder, err error) {
	return updateReleaseReminderAndMaybeRecordEventInMySQLTransaction(tx, profileName, now, update, nil)
}

func setProfileOwnerAndRecordEventInMySQLTransaction(tx mysqlReleaseReminderTransaction, profileName, memberEmail string, event OperationEvent, now time.Time) (owner PublicProfileOwner, err error) {
	profileName = strings.TrimSpace(profileName)
	memberEmail = normalizeEmail(memberEmail)
	if profileName == "" {
		return PublicProfileOwner{}, profileOwnerValidationError("profile is required")
	}
	if memberEmail == "" || !strings.Contains(memberEmail, "@") {
		return PublicProfileOwner{}, profileOwnerValidationError("valid member email is required")
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); err == nil && rollbackErr != nil {
			err = rollbackErr
		}
	}()
	if _, err = lockMySQLStore(tx); err != nil {
		return PublicProfileOwner{}, err
	}
	var profile ManagedProfile
	if err = tx.QueryRow(mysqlManagedProfileForUpdateQuery, profileName).Scan(&profile.Name, &profile.AppleEmail); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PublicProfileOwner{}, profileOwnerValidationError("profile %s not found", profileName)
		}
		return PublicProfileOwner{}, err
	}
	var member Member
	if err = tx.QueryRow(mysqlMemberByEmailForUpdateQuery, memberEmail).Scan(
		&member.ID,
		&member.Name,
		&member.Email,
		&member.Username,
		&member.Role,
		&member.Enabled,
		&member.PasswordHash,
		&member.PasswordSalt,
		&member.APITokenHash,
		&member.APITokenAt,
		&member.CreatedAt,
		&member.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PublicProfileOwner{}, profileOwnerValidationError("member %s not found", memberEmail)
		}
		return PublicProfileOwner{}, err
	}
	updatedAt := now.Format(time.RFC3339)
	if err = tx.Exec(mysqlProfileOwnerUpsertQuery, profileName, member.ID, updatedAt); err != nil {
		return PublicProfileOwner{}, err
	}
	event.Profile = profileName
	event.AppleEmail = profile.AppleEmail
	data := MemberData{}
	if err = appendOperationEvent(&data, event, updatedAt); err != nil {
		return PublicProfileOwner{}, err
	}
	normalized := data.Events[0]
	if err = tx.Exec(mysqlOperationEventInsertQuery, operationEventInsertArgs(normalized)...); err != nil {
		return PublicProfileOwner{}, err
	}
	if err = advanceMySQLStoreLock(tx); err != nil {
		return PublicProfileOwner{}, err
	}
	if err = tx.Commit(); err != nil {
		return PublicProfileOwner{}, err
	}
	committed = true
	return PublicProfileOwner{
		ProfileName: profileName,
		Owner:       publicMember(member),
		UpdatedAt:   updatedAt,
	}, nil
}

func updateReleaseReminderAndRecordEventInMySQLTransaction(tx mysqlReleaseReminderTransaction, profileName string, now time.Time, update func(ReleaseReminder) (ReleaseReminder, error), event OperationEvent) (updated ReleaseReminder, err error) {
	return updateReleaseReminderAndMaybeRecordEventInMySQLTransaction(tx, profileName, now, update, &event)
}

func updateReleaseReminderAndMaybeRecordEventInMySQLTransaction(tx mysqlReleaseReminderTransaction, profileName string, now time.Time, update func(ReleaseReminder) (ReleaseReminder, error), event *OperationEvent) (updated ReleaseReminder, err error) {
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); err == nil && rollbackErr != nil {
			err = rollbackErr
		}
	}()

	if _, err = lockMySQLStore(tx); err != nil {
		return ReleaseReminder{}, err
	}
	var current ReleaseReminder
	row := tx.QueryRow(mysqlReleaseReminderSelectForUpdate, profileName)
	if err := scanMySQLReleaseReminder(row, &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReleaseReminder{}, releaseReminderNotFoundError(profileName)
		}
		return ReleaseReminder{}, err
	}
	updated, err = update(current)
	if err != nil {
		return ReleaseReminder{}, err
	}
	updated = normalizeReleaseReminderCallback(current, updated, now.Format(time.RFC3339))
	if err = updateMySQLReleaseReminder(tx, profileName, updated); err != nil {
		return ReleaseReminder{}, err
	}
	if event != nil {
		event.Profile = updated.ProfileName
		event.AppleEmail = updated.AppleEmail
		data := MemberData{}
		if err = appendOperationEvent(&data, *event, now.Format(time.RFC3339)); err != nil {
			return ReleaseReminder{}, err
		}
		normalized := data.Events[0]
		if err = tx.Exec(mysqlOperationEventInsertQuery, operationEventInsertArgs(normalized)...); err != nil {
			return ReleaseReminder{}, err
		}
	}
	if err = advanceMySQLStoreLock(tx); err != nil {
		return ReleaseReminder{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReleaseReminder{}, err
	}
	committed = true
	return updated, nil
}

func cleanupProfileRecordsInMySQLTransaction(tx mysqlReleaseReminderTransaction, profileName, releasedAt, reason string, now time.Time) (reminder ReleaseReminder, changed bool, err error) {
	return cleanupProfileRecordsAndMaybeEventInMySQLTransaction(tx, profileName, releasedAt, reason, now, nil)
}

func cleanupProfileRecordsAndMaybeEventInMySQLTransaction(tx mysqlReleaseReminderTransaction, profileName, releasedAt, reason string, now time.Time, explicitEvent *OperationEvent) (reminder ReleaseReminder, changed bool, err error) {
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); err == nil && rollbackErr != nil {
			err = rollbackErr
		}
	}()
	if _, err = lockMySQLStore(tx); err != nil {
		return ReleaseReminder{}, false, err
	}

	ownerMemberID := ""
	ownerEmail := ""
	ownerErr := tx.QueryRow(mysqlProfileOwnerForUpdateQuery, profileName).Scan(&ownerMemberID, &ownerEmail)
	if ownerErr != nil && !errors.Is(ownerErr, sql.ErrNoRows) {
		return ReleaseReminder{}, false, ownerErr
	}
	ownerChanged := ownerErr == nil

	reminderFound := true
	if err = scanMySQLReleaseReminder(tx.QueryRow(mysqlReleaseReminderSelectForUpdate, profileName), &reminder); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return ReleaseReminder{}, false, err
		}
		reminderFound = false
		reminder = ReleaseReminder{ProfileName: profileName, Status: ReleaseReminderStatusReleased}
	}
	reminderChanged := reminderFound && releaseReminderNeedsConvergence(reminder)
	if !ownerChanged && !reminderChanged {
		if err = tx.Commit(); err != nil {
			return ReleaseReminder{}, false, err
		}
		committed = true
		return reminder, false, nil
	}

	if ownerChanged {
		if err = tx.Exec(mysqlDeleteMatchingProfileOwnerQuery, profileName, ownerMemberID); err != nil {
			return ReleaseReminder{}, false, err
		}
	}
	if reminderChanged {
		reminder, err = markReleaseReminderReleased(releasedAt)(reminder)
		if err != nil {
			return ReleaseReminder{}, false, err
		}
		reminder.UpdatedAt = now.Format(time.RFC3339)
		if err = updateMySQLReleaseReminder(tx, profileName, reminder); err != nil {
			return ReleaseReminder{}, false, err
		}
	}
	if explicitEvent != nil {
		eventData := MemberData{}
		event := *explicitEvent
		event.Profile = reminder.ProfileName
		event.AppleEmail = reminder.AppleEmail
		if err = appendOperationEvent(&eventData, event, now.Format(time.RFC3339)); err != nil {
			return ReleaseReminder{}, false, err
		}
		if err = tx.Exec(mysqlOperationEventInsertQuery, operationEventInsertArgs(eventData.Events[0])...); err != nil {
			return ReleaseReminder{}, false, err
		}
	} else if shouldRecordAutomaticCleanupEvent(reason) {
		eventData := MemberData{}
		event := cleanupProfileRecordsEvent(reminder, reminderFound, ownerChanged, reminderChanged, reason)
		if err = appendOperationEvent(&eventData, event, now.Format(time.RFC3339)); err != nil {
			return ReleaseReminder{}, false, err
		}
		normalized := eventData.Events[0]
		if err = tx.Exec(mysqlOperationEventInsertQuery, operationEventInsertArgs(normalized)...); err != nil {
			return ReleaseReminder{}, false, err
		}
	}
	if err = advanceMySQLStoreLock(tx); err != nil {
		return ReleaseReminder{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return ReleaseReminder{}, false, err
	}
	committed = true
	return reminder, true, nil
}

func markAutoReleaseNotifiedInMySQLTransaction(tx mysqlReleaseReminderTransaction, cycle ReleaseReminderCycle, notifiedAt string, now time.Time) (updated ReleaseReminder, err error) {
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); err == nil && rollbackErr != nil {
			err = rollbackErr
		}
	}()
	if _, err = lockMySQLStore(tx); err != nil {
		return ReleaseReminder{}, err
	}
	var current ReleaseReminder
	if err = scanMySQLReleaseReminder(tx.QueryRow(mysqlReleaseReminderSelectForUpdate, cycle.ProfileName), &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReleaseReminder{}, releaseReminderNotFoundError(cycle.ProfileName)
		}
		return ReleaseReminder{}, err
	}
	if !releaseReminderMatchesCycle(current, cycle) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying {
		return ReleaseReminder{}, ErrReleaseReminderCycleChanged
	}
	if current.AutoReleaseNotifiedAt != "" {
		if err = tx.Commit(); err != nil {
			return ReleaseReminder{}, err
		}
		committed = true
		return current, nil
	}
	current.AutoReleaseNotifiedAt = notifiedAt
	current.UpdatedAt = now.Format(time.RFC3339)
	if err = updateMySQLReleaseReminder(tx, cycle.ProfileName, current); err != nil {
		return ReleaseReminder{}, err
	}
	if err = advanceMySQLStoreLock(tx); err != nil {
		return ReleaseReminder{}, err
	}
	if err = tx.Commit(); err != nil {
		return ReleaseReminder{}, err
	}
	committed = true
	return current, nil
}

func completeAutoReleaseInMySQLTransaction(tx mysqlReleaseReminderTransaction, cycle ReleaseReminderCycle, releasedAt time.Time) (updated ReleaseReminder, err error) {
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); err == nil && rollbackErr != nil {
			err = rollbackErr
		}
	}()
	if _, err = lockMySQLStore(tx); err != nil {
		return ReleaseReminder{}, err
	}
	var current ReleaseReminder
	if err = scanMySQLReleaseReminder(tx.QueryRow(mysqlReleaseReminderSelectForUpdate, cycle.ProfileName), &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReleaseReminder{}, releaseReminderNotFoundError(cycle.ProfileName)
		}
		return ReleaseReminder{}, err
	}
	if !releaseReminderMatchesCycle(current, cycle) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying || current.AutoReleaseNotifiedAt == "" {
		return ReleaseReminder{}, ErrReleaseReminderCycleChanged
	}
	ownerMemberID := ""
	ownerEmail := ""
	ownerErr := tx.QueryRow(mysqlProfileOwnerForUpdateQuery, cycle.ProfileName).Scan(&ownerMemberID, &ownerEmail)
	if ownerErr != nil && !errors.Is(ownerErr, sql.ErrNoRows) {
		return ReleaseReminder{}, ownerErr
	}
	if ownerErr == nil && normalizeEmail(ownerEmail) != normalizeEmail(cycle.OwnerEmail) {
		return ReleaseReminder{}, ErrReleaseReminderCycleChanged
	}
	current.Status = ReleaseReminderStatusReleased
	current.ReleasedAt = releasedAt.Format(time.RFC3339)
	current.AutoReleaseState = ReleaseReminderAutoReleaseStateReleased
	current.AutoReleaseLastError = ""
	current.UpdatedAt = releasedAt.Format(time.RFC3339)
	if err = updateMySQLReleaseReminder(tx, cycle.ProfileName, current); err != nil {
		return ReleaseReminder{}, err
	}
	if ownerErr == nil {
		if err = tx.Exec(mysqlDeleteMatchingProfileOwnerQuery, cycle.ProfileName, ownerMemberID); err != nil {
			return ReleaseReminder{}, err
		}
	}
	if err = advanceMySQLStoreLock(tx); err != nil {
		return ReleaseReminder{}, err
	}
	if err = tx.Commit(); err != nil {
		return ReleaseReminder{}, err
	}
	committed = true
	return current, nil
}

func (s MySQLMemberStore) MarkReleaseReminderDue(profileName, notifiedAt string) (ReleaseReminder, error) {
	if notifiedAt == "" {
		notifiedAt = s.currentTime().Format(time.RFC3339)
	}
	return s.UpdateReleaseReminder(profileName, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.Status = ReleaseReminderStatusDueNotified
		reminder.LastNotifiedAt = notifiedAt
		return reminder, nil
	})
}

func (s MySQLMemberStore) MarkReleaseReminderReleased(profileName, releasedAt string) (ReleaseReminder, error) {
	if releasedAt == "" {
		releasedAt = s.currentTime().Format(time.RFC3339)
	}
	return s.UpdateReleaseReminder(profileName, markReleaseReminderReleased(releasedAt))
}

func (s MySQLMemberStore) WebSettings() (WebSettings, error) {
	return webSettingsInStore(s)
}

func (s MySQLMemberStore) UpdateWebSettings(update WebSettings) (WebSettings, error) {
	defer s.lockMutation()()
	return updateWebSettingsInStore(s.unlocked(), update)
}

func (s MySQLMemberStore) EnsureAuthSecret() (string, error) {
	defer s.lockMutation()()
	return ensureAuthSecretInStore(s.unlocked())
}

func (s MySQLMemberStore) RecordEvent(event OperationEvent) error {
	return s.insertEvent(event)
}

func (s MySQLMemberStore) RecentEvents(appleEmail string, limit int) ([]OperationEvent, error) {
	page, err := s.QueryEvents(EventQuery{AppleEmail: appleEmail, Limit: limit, IncludeSystem: true})
	return page.Events, err
}

func (s MySQLMemberStore) currentTime() time.Time {
	return s.normalize().Now()
}

type mysqlReleaseReminderScanner interface {
	Scan(dest ...any) error
}

func scanMySQLTransferRecord(scanner mysqlReleaseReminderScanner, record *TransferRecord) error {
	if err := scanner.Scan(
		&record.ID,
		&record.MemberID,
		&record.MemberEmail,
		&record.ProfileName,
		&record.AppleEmail,
		&record.Direction,
		&record.LocalPath,
		&record.RemotePath,
		&record.LocalJobID,
		&record.Status,
		&record.Percent,
		&record.ErrorSummary,
		&record.CreatedAt,
		&record.StartedAt,
		&record.FinishedAt,
		&record.UpdatedAt,
	); err != nil {
		return err
	}
	if status, phase, ok := strings.Cut(record.Status, "|"); ok {
		record.Status = status
		record.Phase = phase
	}
	return nil
}

func inferredTransferPhase(status string, percent int) string {
	switch status {
	case TransferStatusSucceeded:
		return TransferPhaseSucceeded
	case TransferStatusFailed:
		return TransferPhaseFailed
	case TransferStatusInterrupted, TransferStatusUnconfirmed:
		return TransferPhaseInterrupted
	}
	if percent >= 99 {
		return TransferPhaseFinalizing
	}
	if percent > 0 {
		return TransferPhaseTransferring
	}
	return TransferPhasePreparing
}

func scanMySQLReleaseReminder(scanner mysqlReleaseReminderScanner, reminder *ReleaseReminder) error {
	return scanner.Scan(
		&reminder.ProfileName,
		&reminder.AppleEmail,
		&reminder.HostID,
		&reminder.HostCreatedAt,
		&reminder.ReleaseDueAt,
		&reminder.OwnerEmail,
		&reminder.OwnerName,
		&reminder.LastExtendedByEmail,
		&reminder.LastExtendedByName,
		&reminder.LastExtendedAt,
		&reminder.LastNotifiedAt,
		&reminder.ReleasedAt,
		&reminder.Status,
		&reminder.AutoReleaseEnabled,
		&reminder.AutoReleaseAt,
		&reminder.AutoReleaseStartedAt,
		&reminder.AutoReleaseLastAttemptAt,
		&reminder.AutoReleaseNotifiedAt,
		&reminder.AutoReleaseAttempts,
		&reminder.AutoReleaseLastError,
		&reminder.AutoReleaseState,
		&reminder.CreatedAt,
		&reminder.UpdatedAt,
	)
}
