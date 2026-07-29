package connectmac

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const DefaultMemberDataPath = "~/.connectmac/members.json"

type MemberStore struct {
	Path          string
	Now           func() time.Time
	Rename        func(oldPath, newPath string) error
	mutationGuard *storeMutationGuard
}

type storeMutationGuard struct {
	// Serializes repository mutations in this process; registry-backed pointers keep store copies coordinated.
	mu sync.Mutex
}

var storeMutationGuards sync.Map

func sharedStoreMutationGuard(key string) *storeMutationGuard {
	guard, _ := storeMutationGuards.LoadOrStore(key, &storeMutationGuard{})
	return guard.(*storeMutationGuard)
}

type MemberRepository interface {
	Load() (MemberData, error)
	Save(MemberData) error
	CreateTransferRecord(memberID string, record TransferRecord) (TransferRecord, error)
	ListTransferRecords(memberID, profileName string) ([]TransferRecord, error)
	UpdateTransferRecord(memberID, recordID, localJobID string, update func(TransferRecord) (TransferRecord, error)) (TransferRecord, error)
	DeleteTransferRecord(memberID, recordID string) error
	ListMembers() ([]MemberWithAssignments, error)
	AddMember(name, email, role string) (Member, error)
	AddMemberWithPassword(name, email, role, password string) (Member, error)
	SetupAdmin(name, email, password string) (Member, error)
	SetMemberPassword(emailOrUsername, password string) error
	UpdateMember(emailOrUsername, name, email, role string) (Member, error)
	UpdateMemberEmail(memberID, newEmail string) (Member, error)
	VerifyMemberPassword(emailOrUsername, password string) (Member, bool, error)
	HasPasswordMembers() (bool, error)
	SetMemberEnabled(email string, enabled bool) (Member, error)
	AssignMember(appleEmail, memberEmail, relation string) (AppleAccountMember, error)
	UnassignMember(appleEmail, memberEmail string) error
	MembersForApple(appleEmail string) ([]PublicMember, error)
	ProfileOwners() ([]PublicProfileOwner, error)
	ProfileOwner(profileName string) (PublicProfileOwner, bool, error)
	SetProfileOwner(profileName, memberEmail string) (PublicProfileOwner, error)
	SetProfileOwnerAndRecordEvent(profileName, memberEmail string, event OperationEvent) (PublicProfileOwner, error)
	ClearProfileOwner(profileName string) error
	ListManagedProfiles(memberEmail string) ([]ManagedProfile, error)
	UpsertManagedProfile(profile Profile) (ManagedProfile, error)
	SetManagedProfileEnabled(profileName string, enabled bool) (ManagedProfile, error)
	DeleteManagedProfile(profileName string) error
	AssignProfileAccess(profileName, memberEmail string) (ProfileAccess, error)
	UnassignProfileAccess(profileName, memberEmail string) error
	SetMemberProfileAccess(memberEmail string, profileNames []string) ([]ProfileAccess, error)
	MembersForProfile(profileName string) ([]PublicMember, error)
	ListReleaseReminders(memberEmail string) ([]ReleaseReminder, error)
	ReleaseReminder(profileName string) (ReleaseReminder, bool, error)
	UpsertReleaseReminder(reminder ReleaseReminder) (ReleaseReminder, error)
	UpdateReleaseReminder(profileName string, update func(ReleaseReminder) (ReleaseReminder, error)) (ReleaseReminder, error)
	UpdateReleaseReminderAndRecordEvent(profileName string, update func(ReleaseReminder) (ReleaseReminder, error), event OperationEvent) (ReleaseReminder, error)
	CleanupProfileRecords(profileName, releasedAt, reason string) (ReleaseReminder, bool, error)
	CompleteAutoRelease(cycle ReleaseReminderCycle, releasedAt string) (ReleaseReminder, error)
	MarkReleaseReminderDue(profileName, notifiedAt string) (ReleaseReminder, error)
	MarkReleaseReminderReleased(profileName, releasedAt string) (ReleaseReminder, error)
	WebSettings() (WebSettings, error)
	UpdateWebSettings(update WebSettings) (WebSettings, error)
	EnsureAuthSecret() (string, error)
	RecordEvent(event OperationEvent) error
	RecentEvents(appleEmail string, limit int) ([]OperationEvent, error)
}

type MemberData struct {
	Members          []Member             `json:"members"`
	Assignments      []AppleAccountMember `json:"assignments"`
	ProfileOwners    []ProfileOwner       `json:"profile_owners"`
	Profiles         []ManagedProfile     `json:"profiles"`
	ProfileAccess    []ProfileAccess      `json:"profile_access"`
	Reminders        []ReleaseReminder    `json:"release_reminders"`
	TransferRecords  []TransferRecord     `json:"transfer_records"`
	Events           []OperationEvent     `json:"events"`
	Settings         WebSettings          `json:"settings"`
	mutationRevision uint64
}

type Member struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Username     string `json:"username,omitempty"`
	Role         string `json:"role"`
	Enabled      bool   `json:"enabled"`
	PasswordHash string `json:"password_hash,omitempty"`
	PasswordSalt string `json:"password_salt,omitempty"`
	APITokenHash string `json:"api_token_hash,omitempty"`
	APITokenAt   string `json:"api_token_at,omitempty"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type AppleAccountMember struct {
	AppleEmail string `json:"apple_email"`
	MemberID   string `json:"member_id"`
	Relation   string `json:"relation"`
	CreatedAt  string `json:"created_at"`
}

type ProfileOwner struct {
	ProfileName string `json:"profile_name"`
	MemberID    string `json:"member_id"`
	UpdatedAt   string `json:"updated_at"`
}

type PublicProfileOwner struct {
	ProfileName string       `json:"profile_name"`
	Owner       PublicMember `json:"owner"`
	UpdatedAt   string       `json:"updated_at"`
}

type ManagedProfile struct {
	Name        string `json:"name"`
	AppleEmail  string `json:"apple_email,omitempty"`
	Enabled     bool   `json:"enabled"`
	ProfileYAML string `json:"profile_yaml"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type ProfileAccess struct {
	ProfileName string `json:"profile_name"`
	MemberID    string `json:"member_id"`
	CreatedAt   string `json:"created_at"`
}

const (
	TransferDirectionPush = "push"
	TransferDirectionPull = "pull"

	TransferStatusCreated     = "created"
	TransferStatusQueued      = "queued"
	TransferStatusRunning     = "running"
	TransferStatusSucceeded   = "succeeded"
	TransferStatusFailed      = "failed"
	TransferStatusInterrupted = "interrupted"
	TransferStatusUnconfirmed = "unconfirmed"
)

type TransferRecord struct {
	ID           string `json:"id"`
	MemberID     string `json:"member_id"`
	MemberEmail  string `json:"member_email"`
	ProfileName  string `json:"profile_name"`
	AppleEmail   string `json:"apple_email,omitempty"`
	Direction    string `json:"direction"`
	LocalPath    string `json:"local_path"`
	RemotePath   string `json:"remote_path"`
	LocalJobID   string `json:"local_job_id,omitempty"`
	Status       string `json:"status"`
	Percent      int    `json:"percent"`
	ErrorSummary string `json:"error_summary,omitempty"`
	CreatedAt    string `json:"created_at"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

var (
	ErrTransferRecordNotFound = errors.New("transfer record not found")
)

const (
	ReleaseReminderStatusActive      = "active"
	ReleaseReminderStatusDueNotified = "due_notified"
	ReleaseReminderStatusReleased    = "released"

	ReleaseReminderAutoReleaseStateScheduled = "scheduled"
	ReleaseReminderAutoReleaseStateRunning   = "running"
	ReleaseReminderAutoReleaseStateRetrying  = "retrying"
	ReleaseReminderAutoReleaseStateNotifying = "notifying"
	ReleaseReminderAutoReleaseStateFailed    = "failed"
	ReleaseReminderAutoReleaseStateReleased  = "released"
)

type ReleaseReminder struct {
	ProfileName              string `json:"profile_name"`
	AppleEmail               string `json:"apple_email,omitempty"`
	HostID                   string `json:"host_id,omitempty"`
	HostCreatedAt            string `json:"host_created_at,omitempty"`
	ReleaseDueAt             string `json:"release_due_at,omitempty"`
	OwnerEmail               string `json:"owner_email,omitempty"`
	OwnerName                string `json:"owner_name,omitempty"`
	LastExtendedByEmail      string `json:"last_extended_by_email,omitempty"`
	LastExtendedByName       string `json:"last_extended_by_name,omitempty"`
	LastExtendedAt           string `json:"last_extended_at,omitempty"`
	LastNotifiedAt           string `json:"last_notified_at,omitempty"`
	ReleasedAt               string `json:"released_at,omitempty"`
	Status                   string `json:"status"`
	AutoReleaseEnabled       bool   `json:"auto_release_enabled"`
	AutoReleaseAt            string `json:"auto_release_at,omitempty"`
	AutoReleaseStartedAt     string `json:"auto_release_started_at,omitempty"`
	AutoReleaseLastAttemptAt string `json:"auto_release_last_attempt_at,omitempty"`
	AutoReleaseAttempts      int    `json:"auto_release_attempts,omitempty"`
	AutoReleaseLastError     string `json:"auto_release_last_error,omitempty"`
	AutoReleaseState         string `json:"auto_release_state,omitempty"`
	CreatedAt                string `json:"created_at"`
	UpdatedAt                string `json:"updated_at"`
}

type ReleaseReminderCycle struct {
	ProfileName          string
	AutoReleaseAt        string
	AutoReleaseStartedAt string
	HostID               string
	AppleEmail           string
	OwnerEmail           string
}

var (
	ErrReleaseReminderCycleChanged = errors.New("release reminder cycle changed")
	ErrReleaseReminderNotFound     = errors.New("release reminder not found")
)

type OperationEvent struct {
	ID          string `json:"id"`
	Action      string `json:"action"`
	Profile     string `json:"profile"`
	AppleEmail  string `json:"apple_email,omitempty"`
	MemberID    string `json:"member_id,omitempty"`
	MemberEmail string `json:"member_email,omitempty"`
	MemberName  string `json:"member_name,omitempty"`
	Confirmed   bool   `json:"confirmed"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	CreatedAt   string `json:"created_at"`
}

type WebSettings struct {
	AuthSecret          string `json:"auth_secret,omitempty"`
	DefaultOwnerEmail   string `json:"default_owner_email,omitempty"`
	DefaultStatusFilter string `json:"default_status_filter,omitempty"`
	BackgroundConfirm   bool   `json:"background_confirm"`
	ShowReleased        bool   `json:"show_released"`
}

type PublicMember struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	Username    string `json:"username,omitempty"`
	Role        string `json:"role"`
	Enabled     bool   `json:"enabled"`
	HasPassword bool   `json:"has_password"`
	HasAPIToken bool   `json:"has_api_token"`
	APITokenAt  string `json:"api_token_at,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type MemberWithAssignments struct {
	PublicMember
	Assignments []AppleAccountMember `json:"assignments"`
	Profiles    []string             `json:"profiles,omitempty"`
}

func NewMemberStore(path string) MemberStore {
	return MemberStore{Path: path, Now: time.Now}.normalize()
}

func (s MemberStore) normalize() MemberStore {
	if s.Path == "" {
		s.Path = DefaultMemberDataPath
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Rename == nil {
		s.Rename = os.Rename
	}
	if s.mutationGuard == nil {
		path, err := ExpandPath(s.Path)
		if err != nil {
			path = s.Path
		}
		s.mutationGuard = sharedStoreMutationGuard("file:" + filepath.Clean(path))
	}
	return s
}

func (s MemberStore) lockMutation() (func(), error) {
	s = s.normalize()
	s.mutationGuard.mu.Lock()
	path, err := ExpandPath(s.Path)
	if err != nil {
		s.mutationGuard.mu.Unlock()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.mutationGuard.mu.Unlock()
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		s.mutationGuard.mu.Unlock()
		return nil, fmt.Errorf("open lock file %s: %w", path+".lock", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		file.Close()
		s.mutationGuard.mu.Unlock()
		return nil, fmt.Errorf("lock %s: %w", path+".lock", err)
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		s.mutationGuard.mu.Unlock()
	}, nil
}

func (s MemberStore) Load() (MemberData, error) {
	s = s.normalize()
	path, err := ExpandPath(s.Path)
	if err != nil {
		return MemberData{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return MemberData{Members: []Member{}, Assignments: []AppleAccountMember{}, ProfileOwners: []ProfileOwner{}, Profiles: []ManagedProfile{}, ProfileAccess: []ProfileAccess{}, Reminders: []ReleaseReminder{}, TransferRecords: []TransferRecord{}, Events: []OperationEvent{}, Settings: defaultWebSettings()}, nil
		}
		return MemberData{}, err
	}
	var db MemberData
	if err := json.Unmarshal(data, &db); err != nil {
		return MemberData{}, fmt.Errorf("parse members data %s: %w", path, err)
	}
	normalizeMemberData(&db)
	db.mutationRevision = memberDataRevision(data)
	return db, nil
}

func (s MemberStore) Save(db MemberData) error {
	s = s.normalize()
	unlock, err := s.lockMutation()
	if err != nil {
		return err
	}
	defer unlock()
	current, err := s.Load()
	if err != nil {
		return err
	}
	if db.mutationRevision != current.mutationRevision {
		db.Reminders = current.Reminders
		db.TransferRecords = current.TransferRecords
	}
	return s.saveUnlocked(db)
}

func (s MemberStore) CreateTransferRecord(memberID string, record TransferRecord) (TransferRecord, error) {
	s = s.normalize()
	unlock, err := s.lockMutation()
	if err != nil {
		return TransferRecord{}, err
	}
	defer unlock()
	return createTransferRecordInStore(s.unlocked(), memberID, record)
}

func (s MemberStore) ListTransferRecords(memberID, profileName string) ([]TransferRecord, error) {
	return listTransferRecordsInStore(s, memberID, profileName)
}

func (s MemberStore) UpdateTransferRecord(memberID, recordID, localJobID string, update func(TransferRecord) (TransferRecord, error)) (TransferRecord, error) {
	s = s.normalize()
	unlock, err := s.lockMutation()
	if err != nil {
		return TransferRecord{}, err
	}
	defer unlock()
	return updateTransferRecordInStore(s.unlocked(), memberID, recordID, localJobID, update)
}

func (s MemberStore) DeleteTransferRecord(memberID, recordID string) error {
	s = s.normalize()
	unlock, err := s.lockMutation()
	if err != nil {
		return err
	}
	defer unlock()
	return deleteTransferRecordInStore(s.unlocked(), memberID, recordID)
}

func (s MemberStore) saveUnlocked(db MemberData) error {
	s = s.normalize()
	normalizeMemberData(&db)
	path, err := ExpandPath(s.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := s.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

func memberDataRevision(data []byte) uint64 {
	sum := sha256.Sum256(data)
	return binary.BigEndian.Uint64(sum[:8])
}

func (s MemberStore) ListMembers() ([]MemberWithAssignments, error) {
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

func (s MemberStore) AddMember(name, email, role string) (Member, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return Member{}, err
	}
	defer unlock()
	name = strings.TrimSpace(name)
	email = normalizeEmail(email)
	if strings.TrimSpace(role) == "" {
		role = "operator"
	}
	role = normalizeMemberRole(role)
	if name == "" {
		return Member{}, errors.New("name is required")
	}
	if email == "" || !strings.Contains(email, "@") {
		return Member{}, errors.New("valid email is required")
	}
	if role == "" {
		return Member{}, errors.New("role must be admin, operator, or viewer")
	}
	db, err := s.Load()
	if err != nil {
		return Member{}, err
	}
	if _, ok := findMemberByEmail(db, email); ok {
		return Member{}, fmt.Errorf("member %s already exists", email)
	}
	now := s.normalize().Now().Format(time.RFC3339)
	member := Member{
		ID:        "member-" + slugPart(email),
		Name:      name,
		Email:     email,
		Username:  email,
		Role:      role,
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	db.Members = append(db.Members, member)
	return member, s.saveUnlocked(db)
}

func (s MemberStore) AddMemberWithPassword(name, email, role, password string) (Member, error) {
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

func (s MemberStore) SetupAdmin(name, email, password string) (Member, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return Member{}, err
	}
	defer unlock()
	return setupAdminInStore(s.unlocked(), name, email, password)
}

func (s MemberStore) SetMemberPassword(emailOrUsername, password string) error {
	unlock, err := s.lockMutation()
	if err != nil {
		return err
	}
	defer unlock()
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	db, err := s.Load()
	if err != nil {
		return err
	}
	idx, ok := findMemberIndexByEmailOrUsername(db, emailOrUsername)
	if !ok {
		return fmt.Errorf("member %s not found", emailOrUsername)
	}
	salt, err := randomToken(24)
	if err != nil {
		return err
	}
	db.Members[idx].PasswordSalt = salt
	db.Members[idx].PasswordHash = hashPassword(password, salt)
	db.Members[idx].UpdatedAt = s.normalize().Now().Format(time.RFC3339)
	return s.saveUnlocked(db)
}

func (s MemberStore) UpdateMemberEmail(memberID, newEmail string) (Member, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return Member{}, err
	}
	defer unlock()
	newEmail = normalizeEmail(newEmail)
	if newEmail == "" || !strings.Contains(newEmail, "@") {
		return Member{}, errors.New("valid email is required")
	}
	db, err := s.Load()
	if err != nil {
		return Member{}, err
	}
	targetIdx := -1
	for i, member := range db.Members {
		if member.ID == memberID {
			targetIdx = i
			continue
		}
		if strings.EqualFold(member.Email, newEmail) || strings.EqualFold(member.Username, newEmail) {
			return Member{}, fmt.Errorf("member %s already exists", newEmail)
		}
	}
	if targetIdx < 0 {
		return Member{}, fmt.Errorf("member %s not found", memberID)
	}
	oldEmail := db.Members[targetIdx].Email
	db.Members[targetIdx].Email = newEmail
	db.Members[targetIdx].Username = newEmail
	db.Members[targetIdx].UpdatedAt = s.normalize().Now().Format(time.RFC3339)
	if strings.EqualFold(db.Settings.DefaultOwnerEmail, oldEmail) {
		db.Settings.DefaultOwnerEmail = newEmail
	}
	return db.Members[targetIdx], s.saveUnlocked(db)
}

func (s MemberStore) UpdateMember(emailOrUsername, name, email, role string) (Member, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return Member{}, err
	}
	defer unlock()
	return updateMemberInStore(s.unlocked(), emailOrUsername, name, email, role)
}

func (s MemberStore) VerifyMemberPassword(emailOrUsername, password string) (Member, bool, error) {
	db, err := s.Load()
	if err != nil {
		return Member{}, false, err
	}
	member, ok := findMemberByEmailOrUsername(db, emailOrUsername)
	if !ok || !member.Enabled || member.PasswordHash == "" || member.PasswordSalt == "" {
		return Member{}, false, nil
	}
	got := hashPassword(password, member.PasswordSalt)
	if subtle.ConstantTimeCompare([]byte(got), []byte(member.PasswordHash)) != 1 {
		return Member{}, false, nil
	}
	return member, true, nil
}

func (s MemberStore) HasPasswordMembers() (bool, error) {
	db, err := s.Load()
	if err != nil {
		return false, err
	}
	for _, member := range db.Members {
		if member.Enabled && member.PasswordHash != "" && member.PasswordSalt != "" {
			return true, nil
		}
	}
	return false, nil
}

func (s MemberStore) SetMemberEnabled(email string, enabled bool) (Member, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return Member{}, err
	}
	defer unlock()
	email = normalizeEmail(email)
	db, err := s.Load()
	if err != nil {
		return Member{}, err
	}
	idx, ok := findMemberIndexByEmail(db, email)
	if !ok {
		return Member{}, fmt.Errorf("member %s not found", email)
	}
	db.Members[idx].Enabled = enabled
	db.Members[idx].UpdatedAt = s.normalize().Now().Format(time.RFC3339)
	return db.Members[idx], s.saveUnlocked(db)
}

func (s MemberStore) AssignMember(appleEmail, memberEmail, relation string) (AppleAccountMember, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return AppleAccountMember{}, err
	}
	defer unlock()
	appleEmail = normalizeEmail(appleEmail)
	memberEmail = normalizeEmail(memberEmail)
	relation = strings.TrimSpace(strings.ToLower(relation))
	if appleEmail == "" || !strings.Contains(appleEmail, "@") {
		return AppleAccountMember{}, errors.New("valid apple email is required")
	}
	if relation == "" {
		relation = "owner"
	}
	db, err := s.Load()
	if err != nil {
		return AppleAccountMember{}, err
	}
	member, ok := findMemberByEmail(db, memberEmail)
	if !ok {
		return AppleAccountMember{}, fmt.Errorf("member %s not found", memberEmail)
	}
	for _, assignment := range db.Assignments {
		if strings.EqualFold(assignment.AppleEmail, appleEmail) && assignment.MemberID == member.ID {
			return assignment, nil
		}
	}
	assignment := AppleAccountMember{
		AppleEmail: appleEmail,
		MemberID:   member.ID,
		Relation:   relation,
		CreatedAt:  s.normalize().Now().Format(time.RFC3339),
	}
	db.Assignments = append(db.Assignments, assignment)
	return assignment, s.saveUnlocked(db)
}

func (s MemberStore) UnassignMember(appleEmail, memberEmail string) error {
	unlock, err := s.lockMutation()
	if err != nil {
		return err
	}
	defer unlock()
	appleEmail = normalizeEmail(appleEmail)
	memberEmail = normalizeEmail(memberEmail)
	db, err := s.Load()
	if err != nil {
		return err
	}
	member, ok := findMemberByEmail(db, memberEmail)
	if !ok {
		return fmt.Errorf("member %s not found", memberEmail)
	}
	out := db.Assignments[:0]
	removed := false
	for _, assignment := range db.Assignments {
		if strings.EqualFold(assignment.AppleEmail, appleEmail) && assignment.MemberID == member.ID {
			removed = true
			continue
		}
		out = append(out, assignment)
	}
	if !removed {
		return fmt.Errorf("assignment not found")
	}
	db.Assignments = out
	return s.saveUnlocked(db)
}

func (s MemberStore) MembersForApple(appleEmail string) ([]PublicMember, error) {
	appleEmail = normalizeEmail(appleEmail)
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	var members []PublicMember
	for _, assignment := range db.Assignments {
		if !strings.EqualFold(assignment.AppleEmail, appleEmail) {
			continue
		}
		if member, ok := findMemberByID(db, assignment.MemberID); ok {
			members = append(members, publicMember(member))
		}
	}
	return members, nil
}

func (s MemberStore) ProfileOwners() ([]PublicProfileOwner, error) {
	return profileOwnersInStore(s)
}

func (s MemberStore) ProfileOwner(profileName string) (PublicProfileOwner, bool, error) {
	return profileOwnerInStore(s, profileName)
}

func (s MemberStore) SetProfileOwner(profileName, memberEmail string) (PublicProfileOwner, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return PublicProfileOwner{}, err
	}
	defer unlock()
	return setProfileOwnerInStore(s.unlocked(), profileName, memberEmail)
}

func (s MemberStore) SetProfileOwnerAndRecordEvent(profileName, memberEmail string, event OperationEvent) (PublicProfileOwner, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return PublicProfileOwner{}, err
	}
	defer unlock()
	return setProfileOwnerAndRecordEventInStore(s.unlocked(), profileName, memberEmail, event)
}

func (s MemberStore) ClearProfileOwner(profileName string) error {
	unlock, err := s.lockMutation()
	if err != nil {
		return err
	}
	defer unlock()
	return clearProfileOwnerInStore(s.unlocked(), profileName)
}

func (s MemberStore) ListManagedProfiles(memberEmail string) ([]ManagedProfile, error) {
	return listManagedProfilesInStore(s, memberEmail)
}

func (s MemberStore) UpsertManagedProfile(profile Profile) (ManagedProfile, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return ManagedProfile{}, err
	}
	defer unlock()
	return upsertManagedProfileInStore(s.unlocked(), profile)
}

func (s MemberStore) SetManagedProfileEnabled(profileName string, enabled bool) (ManagedProfile, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return ManagedProfile{}, err
	}
	defer unlock()
	return setManagedProfileEnabledInStore(s.unlocked(), profileName, enabled)
}

func (s MemberStore) DeleteManagedProfile(profileName string) error {
	unlock, err := s.lockMutation()
	if err != nil {
		return err
	}
	defer unlock()
	return deleteManagedProfileInStore(s.unlocked(), profileName)
}

func (s MemberStore) AssignProfileAccess(profileName, memberEmail string) (ProfileAccess, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return ProfileAccess{}, err
	}
	defer unlock()
	return assignProfileAccessInStore(s.unlocked(), profileName, memberEmail)
}

func (s MemberStore) UnassignProfileAccess(profileName, memberEmail string) error {
	unlock, err := s.lockMutation()
	if err != nil {
		return err
	}
	defer unlock()
	return unassignProfileAccessInStore(s.unlocked(), profileName, memberEmail)
}

func (s MemberStore) SetMemberProfileAccess(memberEmail string, profileNames []string) ([]ProfileAccess, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return setMemberProfileAccessInStore(s.unlocked(), memberEmail, profileNames)
}

func (s MemberStore) MembersForProfile(profileName string) ([]PublicMember, error) {
	return membersForProfileInStore(s, profileName)
}

func (s MemberStore) ListReleaseReminders(memberEmail string) ([]ReleaseReminder, error) {
	return listReleaseRemindersInStore(s, memberEmail)
}

func (s MemberStore) ReleaseReminder(profileName string) (ReleaseReminder, bool, error) {
	return releaseReminderInStore(s, profileName)
}

func (s MemberStore) UpsertReleaseReminder(reminder ReleaseReminder) (ReleaseReminder, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return ReleaseReminder{}, err
	}
	defer unlock()
	reminder = normalizeReleaseReminder(reminder)
	if reminder.ProfileName == "" {
		return ReleaseReminder{}, errors.New("profile is required")
	}
	return s.mutateReleaseReminders(func(db *MemberData, now string) (ReleaseReminder, error) {
		for i := range db.Reminders {
			if db.Reminders[i].ProfileName != reminder.ProfileName {
				continue
			}
			reminder = mergeReleaseReminderUpsert(db.Reminders[i], reminder, now)
			db.Reminders[i] = reminder
			return reminder, nil
		}
		if reminder.CreatedAt == "" {
			reminder.CreatedAt = now
		}
		reminder.UpdatedAt = now
		db.Reminders = append(db.Reminders, reminder)
		return reminder, nil
	})
}

func (s MemberStore) UpdateReleaseReminder(profileName string, update func(ReleaseReminder) (ReleaseReminder, error)) (ReleaseReminder, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return ReleaseReminder{}, err
	}
	defer unlock()
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return ReleaseReminder{}, errors.New("profile is required")
	}
	if update == nil {
		return ReleaseReminder{}, errors.New("release reminder update is required")
	}
	return s.mutateReleaseReminders(func(db *MemberData, now string) (ReleaseReminder, error) {
		for i := range db.Reminders {
			if db.Reminders[i].ProfileName != profileName {
				continue
			}
			updated, err := update(db.Reminders[i])
			if err != nil {
				return ReleaseReminder{}, err
			}
			updated = normalizeReleaseReminderCallback(db.Reminders[i], updated, now)
			db.Reminders[i] = updated
			return updated, nil
		}
		return ReleaseReminder{}, releaseReminderNotFoundError(profileName)
	})
}

func (s MemberStore) UpdateReleaseReminderAndRecordEvent(profileName string, update func(ReleaseReminder) (ReleaseReminder, error), event OperationEvent) (ReleaseReminder, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return ReleaseReminder{}, err
	}
	defer unlock()
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return ReleaseReminder{}, errors.New("profile is required")
	}
	if update == nil {
		return ReleaseReminder{}, errors.New("release reminder update is required")
	}
	return s.mutateReleaseReminders(func(db *MemberData, now string) (ReleaseReminder, error) {
		for i := range db.Reminders {
			if db.Reminders[i].ProfileName != profileName {
				continue
			}
			updated, err := update(db.Reminders[i])
			if err != nil {
				return ReleaseReminder{}, err
			}
			updated = normalizeReleaseReminderCallback(db.Reminders[i], updated, now)
			event.Profile = updated.ProfileName
			event.AppleEmail = updated.AppleEmail
			if err := appendOperationEvent(db, event, now); err != nil {
				return ReleaseReminder{}, err
			}
			db.Reminders[i] = updated
			return updated, nil
		}
		return ReleaseReminder{}, releaseReminderNotFoundError(profileName)
	})
}

func (s MemberStore) MarkReleaseReminderDue(profileName, notifiedAt string) (ReleaseReminder, error) {
	if notifiedAt == "" {
		notifiedAt = s.currentTime().Format(time.RFC3339)
	}
	return s.UpdateReleaseReminder(profileName, func(reminder ReleaseReminder) (ReleaseReminder, error) {
		reminder.Status = ReleaseReminderStatusDueNotified
		reminder.LastNotifiedAt = notifiedAt
		return reminder, nil
	})
}

func (s MemberStore) MarkReleaseReminderReleased(profileName, releasedAt string) (ReleaseReminder, error) {
	if releasedAt == "" {
		releasedAt = s.currentTime().Format(time.RFC3339)
	}
	return s.UpdateReleaseReminder(profileName, markReleaseReminderReleased(releasedAt))
}

func markReleaseReminderReleased(releasedAt string) func(ReleaseReminder) (ReleaseReminder, error) {
	return func(reminder ReleaseReminder) (ReleaseReminder, error) {
		if reminder.Status != ReleaseReminderStatusReleased || strings.TrimSpace(reminder.ReleasedAt) == "" {
			reminder.ReleasedAt = releasedAt
		}
		reminder.Status = ReleaseReminderStatusReleased
		reminder.AutoReleaseState = ReleaseReminderAutoReleaseStateReleased
		reminder.AutoReleaseLastError = ""
		return reminder, nil
	}
}

func (s MemberStore) CleanupProfileRecords(profileName, releasedAt, reason string) (ReleaseReminder, bool, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return ReleaseReminder{}, false, err
	}
	defer unlock()
	if releasedAt == "" {
		releasedAt = s.currentTime().Format(time.RFC3339)
	}
	db, err := s.Load()
	if err != nil {
		return ReleaseReminder{}, false, err
	}
	reminder, changed, err := cleanupProfileRecordsInData(&db, profileName, releasedAt, reason, s.currentTime().Format(time.RFC3339))
	if err != nil || !changed {
		return reminder, changed, err
	}
	if err := s.saveUnlocked(db); err != nil {
		return ReleaseReminder{}, false, err
	}
	return reminder, true, nil
}

func cleanupProfileRecordsInData(db *MemberData, profileName, releasedAt, reason, updatedAt string) (ReleaseReminder, bool, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return ReleaseReminder{}, false, errors.New("profile is required")
	}
	ownerIndex := -1
	for i, owner := range db.ProfileOwners {
		if owner.ProfileName == profileName {
			ownerIndex = i
			break
		}
	}
	reminderIndex := -1
	reminder := ReleaseReminder{ProfileName: profileName, Status: ReleaseReminderStatusReleased}
	for i, current := range db.Reminders {
		if current.ProfileName == profileName {
			reminderIndex = i
			reminder = current
			break
		}
	}
	reminderChanged := reminderIndex >= 0 && releaseReminderNeedsConvergence(reminder)
	ownerChanged := ownerIndex >= 0
	if !ownerChanged && !reminderChanged {
		return reminder, false, nil
	}
	if ownerChanged {
		db.ProfileOwners = append(db.ProfileOwners[:ownerIndex], db.ProfileOwners[ownerIndex+1:]...)
	}
	if reminderChanged {
		var err error
		reminder, err = markReleaseReminderReleased(releasedAt)(reminder)
		if err != nil {
			return ReleaseReminder{}, false, err
		}
		reminder.UpdatedAt = updatedAt
		db.Reminders[reminderIndex] = reminder
	}
	event := cleanupProfileRecordsEvent(reminder, reminderIndex >= 0, ownerChanged, reminderChanged, reason)
	if err := appendOperationEvent(db, event, updatedAt); err != nil {
		return ReleaseReminder{}, false, err
	}
	return reminder, true, nil
}

func releaseReminderNeedsConvergence(reminder ReleaseReminder) bool {
	return reminder.Status != ReleaseReminderStatusReleased ||
		reminder.AutoReleaseState != ReleaseReminderAutoReleaseStateReleased ||
		reminder.AutoReleaseLastError != ""
}

func cleanupProfileRecordsEvent(reminder ReleaseReminder, reminderFound, ownerChanged, reminderChanged bool, reason string) OperationEvent {
	changes := make([]string, 0, 2)
	if ownerChanged {
		changes = append(changes, "cleared profile owner")
	}
	if reminderChanged {
		changes = append(changes, "marked release reminder released")
	}
	message := strings.Join(changes, " and ")
	if !reminderFound {
		message += "; no release reminder found"
	}
	return OperationEvent{
		Action:     "cleanup-records",
		Profile:    reminder.ProfileName,
		AppleEmail: reminder.AppleEmail,
		Confirmed:  true,
		Status:     "success",
		Message:    message + " (" + reason + ")",
	}
}

func (s MemberStore) CompleteAutoRelease(cycle ReleaseReminderCycle, releasedAt string) (ReleaseReminder, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return ReleaseReminder{}, err
	}
	defer unlock()
	if releasedAt == "" {
		releasedAt = s.currentTime().Format(time.RFC3339)
	}
	s = s.normalize()
	db, err := s.Load()
	if err != nil {
		return ReleaseReminder{}, err
	}
	completed, err := completeAutoReleaseInData(&db, cycle, releasedAt, s.currentTime().Format(time.RFC3339))
	if err != nil {
		return ReleaseReminder{}, err
	}
	if err := s.saveUnlocked(db); err != nil {
		return ReleaseReminder{}, err
	}
	return completed, nil
}

func completeAutoReleaseInData(db *MemberData, cycle ReleaseReminderCycle, releasedAt, updatedAt string) (ReleaseReminder, error) {
	for i := range db.Reminders {
		current := db.Reminders[i]
		if current.ProfileName != cycle.ProfileName {
			continue
		}
		if !releaseReminderMatchesCycle(current, cycle) || current.Status != ReleaseReminderStatusDueNotified || (current.AutoReleaseState != ReleaseReminderAutoReleaseStateRunning && current.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying) || !current.AutoReleaseEnabled {
			return ReleaseReminder{}, ErrReleaseReminderCycleChanged
		}
		ownerIndex := -1
		for j, owner := range db.ProfileOwners {
			if owner.ProfileName != cycle.ProfileName {
				continue
			}
			member, ok := findMemberByID(*db, owner.MemberID)
			if !ok || normalizeEmail(member.Email) != normalizeEmail(cycle.OwnerEmail) {
				return ReleaseReminder{}, ErrReleaseReminderCycleChanged
			}
			ownerIndex = j
			break
		}
		if ownerIndex >= 0 {
			db.ProfileOwners = append(db.ProfileOwners[:ownerIndex], db.ProfileOwners[ownerIndex+1:]...)
		}
		current.Status = ReleaseReminderStatusReleased
		current.ReleasedAt = releasedAt
		current.AutoReleaseState = ReleaseReminderAutoReleaseStateReleased
		current.AutoReleaseLastError = ""
		current.UpdatedAt = updatedAt
		db.Reminders[i] = current
		return current, nil
	}
	return ReleaseReminder{}, releaseReminderNotFoundError(cycle.ProfileName)
}

func releaseReminderMatchesCycle(reminder ReleaseReminder, cycle ReleaseReminderCycle) bool {
	return reminder.ProfileName == cycle.ProfileName &&
		reminder.AutoReleaseAt == cycle.AutoReleaseAt &&
		reminder.AutoReleaseStartedAt == cycle.AutoReleaseStartedAt &&
		reminder.HostID == cycle.HostID &&
		reminder.AppleEmail == cycle.AppleEmail &&
		normalizeEmail(reminder.OwnerEmail) == normalizeEmail(cycle.OwnerEmail)
}

func (s MemberStore) mutateReleaseReminders(mutate func(*MemberData, string) (ReleaseReminder, error)) (ReleaseReminder, error) {
	s = s.normalize()
	db, err := s.Load()
	if err != nil {
		return ReleaseReminder{}, err
	}
	result, err := mutate(&db, s.currentTime().Format(time.RFC3339))
	if err != nil {
		return ReleaseReminder{}, err
	}
	return result, s.saveUnlocked(db)
}

func (s MemberStore) WebSettings() (WebSettings, error) {
	db, err := s.Load()
	if err != nil {
		return WebSettings{}, err
	}
	return publicWebSettings(db.Settings), nil
}

func (s MemberStore) UpdateWebSettings(update WebSettings) (WebSettings, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return WebSettings{}, err
	}
	defer unlock()
	db, err := s.Load()
	if err != nil {
		return WebSettings{}, err
	}
	db.Settings.DefaultOwnerEmail = normalizeEmail(update.DefaultOwnerEmail)
	db.Settings.DefaultStatusFilter = strings.TrimSpace(update.DefaultStatusFilter)
	db.Settings.BackgroundConfirm = update.BackgroundConfirm
	db.Settings.ShowReleased = update.ShowReleased
	if err := s.saveUnlocked(db); err != nil {
		return WebSettings{}, err
	}
	return publicWebSettings(db.Settings), nil
}

func (s MemberStore) EnsureAuthSecret() (string, error) {
	unlock, err := s.lockMutation()
	if err != nil {
		return "", err
	}
	defer unlock()
	db, err := s.Load()
	if err != nil {
		return "", err
	}
	if db.Settings.AuthSecret == "" {
		secret, err := randomToken(32)
		if err != nil {
			return "", err
		}
		db.Settings.AuthSecret = secret
		if err := s.saveUnlocked(db); err != nil {
			return "", err
		}
	}
	return db.Settings.AuthSecret, nil
}

func (s MemberStore) RecordEvent(event OperationEvent) error {
	unlock, err := s.lockMutation()
	if err != nil {
		return err
	}
	defer unlock()
	db, err := s.Load()
	if err != nil {
		return err
	}
	if err := appendOperationEvent(&db, event, s.normalize().Now().Format(time.RFC3339)); err != nil {
		return err
	}
	return s.saveUnlocked(db)
}

func appendOperationEvent(db *MemberData, event OperationEvent, now string) error {
	event.Action = strings.TrimSpace(event.Action)
	if event.Action == "" {
		return errors.New("operation event action is required")
	}
	event.Profile = strings.TrimSpace(event.Profile)
	event.AppleEmail = normalizeEmail(event.AppleEmail)
	event.MemberEmail = normalizeEmail(event.MemberEmail)
	event.MemberName = strings.TrimSpace(event.MemberName)
	if event.ID == "" {
		event.ID = "event-" + strings.ReplaceAll(now, ":", "") + "-" + slugPart(event.Action+"-"+event.Profile)
	}
	if event.CreatedAt == "" {
		event.CreatedAt = now
	}
	db.Events = append(db.Events, event)
	if len(db.Events) > 500 {
		db.Events = db.Events[len(db.Events)-500:]
	}
	return nil
}

func (s MemberStore) RecentEvents(appleEmail string, limit int) ([]OperationEvent, error) {
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	appleEmail = normalizeEmail(appleEmail)
	var out []OperationEvent
	for i := len(db.Events) - 1; i >= 0; i-- {
		event := db.Events[i]
		if appleEmail != "" && !strings.EqualFold(event.AppleEmail, appleEmail) {
			continue
		}
		out = append(out, event)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func normalizeMemberData(db *MemberData) {
	if db.Members == nil {
		db.Members = []Member{}
	}
	if db.Assignments == nil {
		db.Assignments = []AppleAccountMember{}
	}
	if db.ProfileOwners == nil {
		db.ProfileOwners = []ProfileOwner{}
	}
	if db.Profiles == nil {
		db.Profiles = []ManagedProfile{}
	}
	if db.ProfileAccess == nil {
		db.ProfileAccess = []ProfileAccess{}
	}
	if db.Reminders == nil {
		db.Reminders = []ReleaseReminder{}
	}
	if db.TransferRecords == nil {
		db.TransferRecords = []TransferRecord{}
	}
	if db.Events == nil {
		db.Events = []OperationEvent{}
	}
	if db.Settings.AuthSecret == "" {
		db.Settings.BackgroundConfirm = true
	}
	for i := range db.Members {
		if db.Members[i].Username == "" {
			db.Members[i].Username = db.Members[i].Email
		}
	}
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func normalizeMemberRole(role string) string {
	role = strings.TrimSpace(strings.ToLower(role))
	switch role {
	case "admin", "operator", "viewer":
		return role
	default:
		return ""
	}
}

func findMemberByEmail(db MemberData, email string) (Member, bool) {
	idx, ok := findMemberIndexByEmail(db, email)
	if !ok {
		return Member{}, false
	}
	return db.Members[idx], true
}

func findMemberByEmailOrUsername(db MemberData, emailOrUsername string) (Member, bool) {
	idx, ok := findMemberIndexByEmailOrUsername(db, emailOrUsername)
	if !ok {
		return Member{}, false
	}
	return db.Members[idx], true
}

func findMemberIndexByEmailOrUsername(db MemberData, emailOrUsername string) (int, bool) {
	value := normalizeEmail(emailOrUsername)
	for i, member := range db.Members {
		if strings.EqualFold(member.Email, value) || strings.EqualFold(member.Username, value) {
			return i, true
		}
	}
	return 0, false
}

func findMemberIndexByEmail(db MemberData, email string) (int, bool) {
	email = normalizeEmail(email)
	for i, member := range db.Members {
		if strings.EqualFold(member.Email, email) {
			return i, true
		}
	}
	return 0, false
}

func findMemberByID(db MemberData, id string) (Member, bool) {
	for _, member := range db.Members {
		if member.ID == id {
			return member, true
		}
	}
	return Member{}, false
}

func findManagedProfileByName(db MemberData, name string) (ManagedProfile, bool) {
	for _, profile := range db.Profiles {
		if profile.Name == name {
			return profile, true
		}
	}
	return ManagedProfile{}, false
}

func publicMember(member Member) PublicMember {
	return PublicMember{
		ID:          member.ID,
		Name:        member.Name,
		Email:       member.Email,
		Username:    member.Username,
		Role:        member.Role,
		Enabled:     member.Enabled,
		HasPassword: member.PasswordHash != "" && member.PasswordSalt != "",
		HasAPIToken: member.APITokenHash != "",
		APITokenAt:  member.APITokenAt,
		CreatedAt:   member.CreatedAt,
		UpdatedAt:   member.UpdatedAt,
	}
}

func defaultWebSettings() WebSettings {
	return WebSettings{BackgroundConfirm: true}
}

func publicWebSettings(settings WebSettings) WebSettings {
	settings.AuthSecret = ""
	return settings
}

func randomToken(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashPassword(password, salt string) string {
	sum := []byte(salt + "\x00" + password)
	for i := 0; i < 120000; i++ {
		hash := sha256.Sum256(sum)
		sum = hash[:]
	}
	return base64.RawURLEncoding.EncodeToString(sum)
}

type memberDataStore interface {
	Load() (MemberData, error)
	Save(MemberData) error
	currentTime() time.Time
}

func createTransferRecordInStore(s memberDataStore, memberID string, record TransferRecord) (TransferRecord, error) {
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
	if err := validateTransferRecord(record); err != nil {
		return TransferRecord{}, err
	}
	suffix, err := randomToken(12)
	if err != nil {
		return TransferRecord{}, fmt.Errorf("generate transfer record ID: %w", err)
	}
	now := s.currentTime().Format(time.RFC3339)
	record.ID = fmt.Sprintf("transfer-%d-%s", s.currentTime().UnixNano(), suffix)
	record.CreatedAt = now
	record.UpdatedAt = now
	db, err := s.Load()
	if err != nil {
		return TransferRecord{}, err
	}
	db.TransferRecords = append(db.TransferRecords, record)
	return record, s.Save(db)
}

func listTransferRecordsInStore(s memberDataStore, memberID, profileName string) ([]TransferRecord, error) {
	memberID = strings.TrimSpace(memberID)
	if memberID == "" {
		return nil, errors.New("member ID is required")
	}
	profileName = strings.TrimSpace(profileName)
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	records := make([]TransferRecord, 0)
	for _, record := range db.TransferRecords {
		if record.MemberID == memberID && (profileName == "" || record.ProfileName == profileName) {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].UpdatedAt == records[j].UpdatedAt {
			return records[i].ID > records[j].ID
		}
		return records[i].UpdatedAt > records[j].UpdatedAt
	})
	return records, nil
}

func updateTransferRecordInStore(s memberDataStore, memberID, recordID, localJobID string, update func(TransferRecord) (TransferRecord, error)) (TransferRecord, error) {
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
	db, err := s.Load()
	if err != nil {
		return TransferRecord{}, err
	}
	for i, current := range db.TransferRecords {
		if current.ID != recordID || current.MemberID != memberID {
			continue
		}
		if isTerminalTransferStatus(current.Status) {
			return TransferRecord{}, errors.New("terminal transfer record cannot be updated")
		}
		if current.LocalJobID != "" && current.LocalJobID != localJobID {
			return TransferRecord{}, ErrTransferRecordNotFound
		}
		next, err := update(current)
		if err != nil {
			return TransferRecord{}, err
		}
		if next.ID != current.ID {
			return TransferRecord{}, errors.New("transfer record ID cannot change")
		}
		if next.MemberID != memberID {
			return TransferRecord{}, errors.New("transfer record member ID does not match")
		}
		next.LocalJobID = strings.TrimSpace(next.LocalJobID)
		if current.LocalJobID == "" {
			if next.LocalJobID != localJobID {
				return TransferRecord{}, errors.New("transfer record local job ID does not match")
			}
		} else if next.LocalJobID != current.LocalJobID {
			return TransferRecord{}, errors.New("transfer record local job ID cannot change")
		}
		next.Direction = strings.TrimSpace(strings.ToLower(next.Direction))
		next.Status = strings.TrimSpace(strings.ToLower(next.Status))
		if err := validateTransferRecord(next); err != nil {
			return TransferRecord{}, err
		}
		if next.Percent < current.Percent {
			return TransferRecord{}, errors.New("transfer percent cannot regress")
		}
		if isTerminalTransferStatus(current.Status) && isActiveTransferStatus(next.Status) {
			return TransferRecord{}, errors.New("terminal transfer status cannot return to active")
		}
		next.CreatedAt = current.CreatedAt
		next.UpdatedAt = s.currentTime().Format(time.RFC3339)
		db.TransferRecords[i] = next
		return next, s.Save(db)
	}
	return TransferRecord{}, ErrTransferRecordNotFound
}

func deleteTransferRecordInStore(s memberDataStore, memberID, recordID string) error {
	memberID = strings.TrimSpace(memberID)
	recordID = strings.TrimSpace(recordID)
	if memberID == "" {
		return errors.New("member ID is required")
	}
	db, err := s.Load()
	if err != nil {
		return err
	}
	for i, record := range db.TransferRecords {
		if record.ID == recordID && record.MemberID == memberID {
			db.TransferRecords = append(db.TransferRecords[:i], db.TransferRecords[i+1:]...)
			return s.Save(db)
		}
	}
	return ErrTransferRecordNotFound
}

func validateTransferRecord(record TransferRecord) error {
	switch record.Direction {
	case TransferDirectionPush, TransferDirectionPull:
	default:
		return errors.New("transfer direction must be push or pull")
	}
	switch record.Status {
	case TransferStatusCreated, TransferStatusQueued, TransferStatusRunning, TransferStatusSucceeded, TransferStatusFailed, TransferStatusInterrupted, TransferStatusUnconfirmed:
	default:
		return errors.New("invalid transfer status")
	}
	if record.Percent < 0 || record.Percent > 100 {
		return errors.New("transfer percent must be between 0 and 100")
	}
	return nil
}

func isActiveTransferStatus(status string) bool {
	return status == TransferStatusCreated || status == TransferStatusQueued || status == TransferStatusRunning
}

func isTerminalTransferStatus(status string) bool {
	return status == TransferStatusSucceeded || status == TransferStatusFailed || status == TransferStatusInterrupted || status == TransferStatusUnconfirmed
}

type unlockedMemberStore struct {
	store MemberStore
}

func (s MemberStore) unlocked() unlockedMemberStore {
	return unlockedMemberStore{store: s}
}

func (s unlockedMemberStore) Load() (MemberData, error) {
	return s.store.Load()
}

func (s unlockedMemberStore) Save(db MemberData) error {
	return s.store.saveUnlocked(db)
}

func (s unlockedMemberStore) currentTime() time.Time {
	return s.store.currentTime()
}

func addMemberToStore(s memberDataStore, name, email, role string) (Member, error) {
	name = strings.TrimSpace(name)
	email = normalizeEmail(email)
	if strings.TrimSpace(role) == "" {
		role = "operator"
	}
	role = normalizeMemberRole(role)
	if name == "" {
		return Member{}, errors.New("name is required")
	}
	if email == "" || !strings.Contains(email, "@") {
		return Member{}, errors.New("valid email is required")
	}
	if role == "" {
		return Member{}, errors.New("role must be admin, operator, or viewer")
	}
	db, err := s.Load()
	if err != nil {
		return Member{}, err
	}
	if _, ok := findMemberByEmail(db, email); ok {
		return Member{}, fmt.Errorf("member %s already exists", email)
	}
	now := s.currentTime().Format(time.RFC3339)
	member := Member{
		ID:        "member-" + slugPart(email),
		Name:      name,
		Email:     email,
		Username:  email,
		Role:      role,
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	db.Members = append(db.Members, member)
	return member, s.Save(db)
}

func setupAdminInStore(s memberDataStore, name, email, password string) (Member, error) {
	name = strings.TrimSpace(name)
	email = normalizeEmail(email)
	if name == "" {
		return Member{}, errors.New("name is required")
	}
	if email == "" || !strings.Contains(email, "@") {
		return Member{}, errors.New("valid email is required")
	}
	if len(password) < 8 {
		return Member{}, errors.New("password must be at least 8 characters")
	}
	db, err := s.Load()
	if err != nil {
		return Member{}, err
	}
	now := s.currentTime().Format(time.RFC3339)
	member, ok := findMemberByEmailOrUsername(db, email)
	if ok {
		idx, _ := findMemberIndexByEmailOrUsername(db, email)
		db.Members[idx].Name = name
		db.Members[idx].Email = email
		db.Members[idx].Username = email
		db.Members[idx].Role = "admin"
		db.Members[idx].Enabled = true
		db.Members[idx].UpdatedAt = now
		member = db.Members[idx]
	} else {
		member = Member{
			ID:        "member-" + slugPart(email),
			Name:      name,
			Email:     email,
			Username:  email,
			Role:      "admin",
			Enabled:   true,
			CreatedAt: now,
			UpdatedAt: now,
		}
		db.Members = append(db.Members, member)
	}
	if err := s.Save(db); err != nil {
		return Member{}, err
	}
	if err := setMemberPasswordInStore(s, email, password); err != nil {
		return Member{}, err
	}
	db, err = s.Load()
	if err != nil {
		return Member{}, err
	}
	member, _ = findMemberByEmailOrUsername(db, email)
	return member, nil
}

func setMemberPasswordInStore(s memberDataStore, emailOrUsername, password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	db, err := s.Load()
	if err != nil {
		return err
	}
	idx, ok := findMemberIndexByEmailOrUsername(db, emailOrUsername)
	if !ok {
		return fmt.Errorf("member %s not found", emailOrUsername)
	}
	salt, err := randomToken(24)
	if err != nil {
		return err
	}
	db.Members[idx].PasswordSalt = salt
	db.Members[idx].PasswordHash = hashPassword(password, salt)
	db.Members[idx].UpdatedAt = s.currentTime().Format(time.RFC3339)
	return s.Save(db)
}

func updateMemberEmailInStore(s memberDataStore, memberID, newEmail string) (Member, error) {
	newEmail = normalizeEmail(newEmail)
	if newEmail == "" || !strings.Contains(newEmail, "@") {
		return Member{}, errors.New("valid email is required")
	}
	db, err := s.Load()
	if err != nil {
		return Member{}, err
	}
	targetIdx := -1
	for i, member := range db.Members {
		if member.ID == memberID {
			targetIdx = i
			continue
		}
		if strings.EqualFold(member.Email, newEmail) || strings.EqualFold(member.Username, newEmail) {
			return Member{}, fmt.Errorf("member %s already exists", newEmail)
		}
	}
	if targetIdx < 0 {
		return Member{}, fmt.Errorf("member %s not found", memberID)
	}
	oldEmail := db.Members[targetIdx].Email
	db.Members[targetIdx].Email = newEmail
	db.Members[targetIdx].Username = newEmail
	db.Members[targetIdx].UpdatedAt = s.currentTime().Format(time.RFC3339)
	if strings.EqualFold(db.Settings.DefaultOwnerEmail, oldEmail) {
		db.Settings.DefaultOwnerEmail = newEmail
	}
	return db.Members[targetIdx], s.Save(db)
}

func updateMemberInStore(s memberDataStore, emailOrUsername, name, email, role string) (Member, error) {
	emailOrUsername = strings.TrimSpace(emailOrUsername)
	name = strings.TrimSpace(name)
	email = normalizeEmail(email)
	role = normalizeMemberRole(role)
	if emailOrUsername == "" {
		return Member{}, errors.New("member email is required")
	}
	if name == "" {
		return Member{}, errors.New("name is required")
	}
	if email == "" || !strings.Contains(email, "@") {
		return Member{}, errors.New("valid email is required")
	}
	if role == "" {
		return Member{}, errors.New("role must be admin, operator, or viewer")
	}
	db, err := s.Load()
	if err != nil {
		return Member{}, err
	}
	targetIdx := -1
	for i, member := range db.Members {
		if strings.EqualFold(member.Email, emailOrUsername) || strings.EqualFold(member.Username, emailOrUsername) || member.ID == emailOrUsername {
			targetIdx = i
			continue
		}
		if strings.EqualFold(member.Email, email) || strings.EqualFold(member.Username, email) {
			return Member{}, fmt.Errorf("member %s already exists", email)
		}
	}
	if targetIdx < 0 {
		return Member{}, fmt.Errorf("member %s not found", emailOrUsername)
	}
	oldEmail := db.Members[targetIdx].Email
	db.Members[targetIdx].Name = name
	db.Members[targetIdx].Email = email
	db.Members[targetIdx].Username = email
	db.Members[targetIdx].Role = role
	db.Members[targetIdx].UpdatedAt = s.currentTime().Format(time.RFC3339)
	if strings.EqualFold(db.Settings.DefaultOwnerEmail, oldEmail) {
		db.Settings.DefaultOwnerEmail = email
	}
	return db.Members[targetIdx], s.Save(db)
}

func verifyMemberPasswordInStore(s memberDataStore, emailOrUsername, password string) (Member, bool, error) {
	db, err := s.Load()
	if err != nil {
		return Member{}, false, err
	}
	member, ok := findMemberByEmailOrUsername(db, emailOrUsername)
	if !ok || !member.Enabled || member.PasswordHash == "" || member.PasswordSalt == "" {
		return Member{}, false, nil
	}
	got := hashPassword(password, member.PasswordSalt)
	if subtle.ConstantTimeCompare([]byte(got), []byte(member.PasswordHash)) != 1 {
		return Member{}, false, nil
	}
	return member, true, nil
}

func hasPasswordMembersInStore(s memberDataStore) (bool, error) {
	db, err := s.Load()
	if err != nil {
		return false, err
	}
	for _, member := range db.Members {
		if member.Enabled && member.PasswordHash != "" && member.PasswordSalt != "" {
			return true, nil
		}
	}
	return false, nil
}

func setMemberEnabledInStore(s memberDataStore, email string, enabled bool) (Member, error) {
	email = normalizeEmail(email)
	db, err := s.Load()
	if err != nil {
		return Member{}, err
	}
	idx, ok := findMemberIndexByEmail(db, email)
	if !ok {
		return Member{}, fmt.Errorf("member %s not found", email)
	}
	db.Members[idx].Enabled = enabled
	db.Members[idx].UpdatedAt = s.currentTime().Format(time.RFC3339)
	return db.Members[idx], s.Save(db)
}

func assignMemberInStore(s memberDataStore, appleEmail, memberEmail, relation string) (AppleAccountMember, error) {
	appleEmail = normalizeEmail(appleEmail)
	memberEmail = normalizeEmail(memberEmail)
	relation = strings.TrimSpace(strings.ToLower(relation))
	if appleEmail == "" || !strings.Contains(appleEmail, "@") {
		return AppleAccountMember{}, errors.New("valid apple email is required")
	}
	if relation == "" {
		relation = "owner"
	}
	db, err := s.Load()
	if err != nil {
		return AppleAccountMember{}, err
	}
	member, ok := findMemberByEmail(db, memberEmail)
	if !ok {
		return AppleAccountMember{}, fmt.Errorf("member %s not found", memberEmail)
	}
	for _, assignment := range db.Assignments {
		if strings.EqualFold(assignment.AppleEmail, appleEmail) && assignment.MemberID == member.ID {
			return assignment, nil
		}
	}
	assignment := AppleAccountMember{
		AppleEmail: appleEmail,
		MemberID:   member.ID,
		Relation:   relation,
		CreatedAt:  s.currentTime().Format(time.RFC3339),
	}
	db.Assignments = append(db.Assignments, assignment)
	return assignment, s.Save(db)
}

func unassignMemberInStore(s memberDataStore, appleEmail, memberEmail string) error {
	appleEmail = normalizeEmail(appleEmail)
	memberEmail = normalizeEmail(memberEmail)
	db, err := s.Load()
	if err != nil {
		return err
	}
	member, ok := findMemberByEmail(db, memberEmail)
	if !ok {
		return fmt.Errorf("member %s not found", memberEmail)
	}
	out := db.Assignments[:0]
	removed := false
	for _, assignment := range db.Assignments {
		if strings.EqualFold(assignment.AppleEmail, appleEmail) && assignment.MemberID == member.ID {
			removed = true
			continue
		}
		out = append(out, assignment)
	}
	if !removed {
		return fmt.Errorf("assignment not found")
	}
	db.Assignments = out
	return s.Save(db)
}

func membersForAppleInStore(s memberDataStore, appleEmail string) ([]PublicMember, error) {
	appleEmail = normalizeEmail(appleEmail)
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	var members []PublicMember
	for _, assignment := range db.Assignments {
		if !strings.EqualFold(assignment.AppleEmail, appleEmail) {
			continue
		}
		if member, ok := findMemberByID(db, assignment.MemberID); ok {
			members = append(members, publicMember(member))
		}
	}
	return members, nil
}

func profileOwnersInStore(s memberDataStore) ([]PublicProfileOwner, error) {
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	out := make([]PublicProfileOwner, 0, len(db.ProfileOwners))
	for _, owner := range db.ProfileOwners {
		if member, ok := findMemberByID(db, owner.MemberID); ok {
			out = append(out, PublicProfileOwner{
				ProfileName: owner.ProfileName,
				Owner:       publicMember(member),
				UpdatedAt:   owner.UpdatedAt,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].ProfileName) < strings.ToLower(out[j].ProfileName)
	})
	return out, nil
}

func profileOwnerInStore(s memberDataStore, profileName string) (PublicProfileOwner, bool, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return PublicProfileOwner{}, false, nil
	}
	db, err := s.Load()
	if err != nil {
		return PublicProfileOwner{}, false, err
	}
	for _, owner := range db.ProfileOwners {
		if owner.ProfileName != profileName {
			continue
		}
		member, ok := findMemberByID(db, owner.MemberID)
		if !ok {
			return PublicProfileOwner{}, false, nil
		}
		return PublicProfileOwner{
			ProfileName: owner.ProfileName,
			Owner:       publicMember(member),
			UpdatedAt:   owner.UpdatedAt,
		}, true, nil
	}
	return PublicProfileOwner{}, false, nil
}

func setProfileOwnerInStore(s memberDataStore, profileName, memberEmail string) (PublicProfileOwner, error) {
	profileName = strings.TrimSpace(profileName)
	memberEmail = normalizeEmail(memberEmail)
	if profileName == "" {
		return PublicProfileOwner{}, errors.New("profile is required")
	}
	if memberEmail == "" || !strings.Contains(memberEmail, "@") {
		return PublicProfileOwner{}, errors.New("valid member email is required")
	}
	db, err := s.Load()
	if err != nil {
		return PublicProfileOwner{}, err
	}
	member, ok := findMemberByEmail(db, memberEmail)
	if !ok {
		return PublicProfileOwner{}, fmt.Errorf("member %s not found", memberEmail)
	}
	record := ProfileOwner{
		ProfileName: profileName,
		MemberID:    member.ID,
		UpdatedAt:   s.currentTime().Format(time.RFC3339),
	}
	updated := false
	for i := range db.ProfileOwners {
		if db.ProfileOwners[i].ProfileName == profileName {
			db.ProfileOwners[i] = record
			updated = true
			break
		}
	}
	if !updated {
		db.ProfileOwners = append(db.ProfileOwners, record)
	}
	if err := s.Save(db); err != nil {
		return PublicProfileOwner{}, err
	}
	return PublicProfileOwner{
		ProfileName: record.ProfileName,
		Owner:       publicMember(member),
		UpdatedAt:   record.UpdatedAt,
	}, nil
}

func setProfileOwnerAndRecordEventInStore(s memberDataStore, profileName, memberEmail string, event OperationEvent) (PublicProfileOwner, error) {
	profileName = strings.TrimSpace(profileName)
	memberEmail = normalizeEmail(memberEmail)
	if profileName == "" {
		return PublicProfileOwner{}, errors.New("profile is required")
	}
	if memberEmail == "" || !strings.Contains(memberEmail, "@") {
		return PublicProfileOwner{}, errors.New("valid member email is required")
	}
	db, err := s.Load()
	if err != nil {
		return PublicProfileOwner{}, err
	}
	profile, ok := findManagedProfileByName(db, profileName)
	if !ok {
		return PublicProfileOwner{}, fmt.Errorf("profile %s not found", profileName)
	}
	member, ok := findMemberByEmail(db, memberEmail)
	if !ok {
		return PublicProfileOwner{}, fmt.Errorf("member %s not found", memberEmail)
	}
	now := s.currentTime().Format(time.RFC3339)
	record := ProfileOwner{ProfileName: profileName, MemberID: member.ID, UpdatedAt: now}
	updated := false
	for i := range db.ProfileOwners {
		if db.ProfileOwners[i].ProfileName == profileName {
			db.ProfileOwners[i] = record
			updated = true
			break
		}
	}
	if !updated {
		db.ProfileOwners = append(db.ProfileOwners, record)
	}
	event.Profile = profileName
	event.AppleEmail = profile.AppleEmail
	if err := appendOperationEvent(&db, event, now); err != nil {
		return PublicProfileOwner{}, err
	}
	if err := s.Save(db); err != nil {
		return PublicProfileOwner{}, err
	}
	return PublicProfileOwner{
		ProfileName: record.ProfileName,
		Owner:       publicMember(member),
		UpdatedAt:   record.UpdatedAt,
	}, nil
}

func clearProfileOwnerInStore(s memberDataStore, profileName string) error {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return errors.New("profile is required")
	}
	db, err := s.Load()
	if err != nil {
		return err
	}
	out := db.ProfileOwners[:0]
	for _, owner := range db.ProfileOwners {
		if owner.ProfileName != profileName {
			out = append(out, owner)
		}
	}
	db.ProfileOwners = out
	return s.Save(db)
}

func listManagedProfilesInStore(s memberDataStore, memberEmail string) ([]ManagedProfile, error) {
	memberEmail = normalizeEmail(memberEmail)
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	var member Member
	isAdmin := false
	if memberEmail != "" {
		if found, ok := findMemberByEmailOrUsername(db, memberEmail); ok {
			member = found
			isAdmin = found.Role == "admin"
		}
	}
	out := make([]ManagedProfile, 0, len(db.Profiles))
	for _, profile := range db.Profiles {
		if !profile.Enabled && !isAdmin {
			continue
		}
		if isAdmin || memberEmail == "" {
			out = append(out, profile)
			continue
		}
		for _, access := range db.ProfileAccess {
			if access.ProfileName == profile.Name && access.MemberID == member.ID {
				out = append(out, profile)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func upsertManagedProfileInStore(s memberDataStore, profile Profile) (ManagedProfile, error) {
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Name == "" {
		return ManagedProfile{}, errors.New("profile name is required")
	}
	if profile.AWS.AccountEmail == "" && profile.AWS.ElasticIPOwnerTag.Value != "" {
		profile.AWS.AccountEmail = profile.AWS.ElasticIPOwnerTag.Value
	}
	if profile.Description == "" && profile.AWS.AccountEmail != "" {
		profile.Description = "Apple account: " + profile.AWS.AccountEmail
	}
	if profile.AWS.ElasticIPOwnerTag.Key == "" && profile.AWS.AccountEmail != "" {
		profile.AWS.ElasticIPOwnerTag = AWSTagConfig{Key: "Apple", Value: profile.AWS.AccountEmail}
	}
	db, err := s.Load()
	if err != nil {
		return ManagedProfile{}, err
	}
	now := s.currentTime().Format(time.RFC3339)
	record := ManagedProfile{
		Name:        profile.Name,
		AppleEmail:  profile.AWS.AccountEmail,
		Enabled:     true,
		ProfileYAML: FormatProfileFile(profile),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	for i := range db.Profiles {
		if db.Profiles[i].Name == profile.Name {
			record.CreatedAt = db.Profiles[i].CreatedAt
			if record.CreatedAt == "" {
				record.CreatedAt = now
			}
			record.Enabled = db.Profiles[i].Enabled
			db.Profiles[i] = record
			return record, s.Save(db)
		}
	}
	db.Profiles = append(db.Profiles, record)
	return record, s.Save(db)
}

func setManagedProfileEnabledInStore(s memberDataStore, profileName string, enabled bool) (ManagedProfile, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return ManagedProfile{}, errors.New("profile is required")
	}
	db, err := s.Load()
	if err != nil {
		return ManagedProfile{}, err
	}
	for i := range db.Profiles {
		if db.Profiles[i].Name == profileName {
			db.Profiles[i].Enabled = enabled
			db.Profiles[i].UpdatedAt = s.currentTime().Format(time.RFC3339)
			if err := s.Save(db); err != nil {
				return ManagedProfile{}, err
			}
			return db.Profiles[i], nil
		}
	}
	return ManagedProfile{}, fmt.Errorf("profile %s not found", profileName)
}

func deleteManagedProfileInStore(s memberDataStore, profileName string) error {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return errors.New("profile is required")
	}
	db, err := s.Load()
	if err != nil {
		return err
	}
	profiles := db.Profiles[:0]
	removed := false
	for _, profile := range db.Profiles {
		if profile.Name == profileName {
			removed = true
			continue
		}
		profiles = append(profiles, profile)
	}
	if !removed {
		return fmt.Errorf("profile %s not found", profileName)
	}
	db.Profiles = profiles
	access := db.ProfileAccess[:0]
	for _, item := range db.ProfileAccess {
		if item.ProfileName != profileName {
			access = append(access, item)
		}
	}
	db.ProfileAccess = access
	return s.Save(db)
}

func assignProfileAccessInStore(s memberDataStore, profileName, memberEmail string) (ProfileAccess, error) {
	profileName = strings.TrimSpace(profileName)
	memberEmail = normalizeEmail(memberEmail)
	if profileName == "" {
		return ProfileAccess{}, errors.New("profile is required")
	}
	db, err := s.Load()
	if err != nil {
		return ProfileAccess{}, err
	}
	if _, ok := findManagedProfileByName(db, profileName); !ok {
		return ProfileAccess{}, fmt.Errorf("profile %s not found", profileName)
	}
	member, ok := findMemberByEmail(db, memberEmail)
	if !ok {
		return ProfileAccess{}, fmt.Errorf("member %s not found", memberEmail)
	}
	for _, access := range db.ProfileAccess {
		if access.ProfileName == profileName && access.MemberID == member.ID {
			return access, nil
		}
	}
	access := ProfileAccess{ProfileName: profileName, MemberID: member.ID, CreatedAt: s.currentTime().Format(time.RFC3339)}
	db.ProfileAccess = append(db.ProfileAccess, access)
	return access, s.Save(db)
}

func unassignProfileAccessInStore(s memberDataStore, profileName, memberEmail string) error {
	profileName = strings.TrimSpace(profileName)
	memberEmail = normalizeEmail(memberEmail)
	db, err := s.Load()
	if err != nil {
		return err
	}
	member, ok := findMemberByEmail(db, memberEmail)
	if !ok {
		return fmt.Errorf("member %s not found", memberEmail)
	}
	out := db.ProfileAccess[:0]
	removed := false
	for _, access := range db.ProfileAccess {
		if access.ProfileName == profileName && access.MemberID == member.ID {
			removed = true
			continue
		}
		out = append(out, access)
	}
	if !removed {
		return fmt.Errorf("profile access not found")
	}
	db.ProfileAccess = out
	return s.Save(db)
}

func setMemberProfileAccessInStore(s memberDataStore, memberEmail string, profileNames []string) ([]ProfileAccess, error) {
	memberEmail = normalizeEmail(memberEmail)
	if memberEmail == "" {
		return nil, errors.New("member email is required")
	}
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	member, ok := findMemberByEmail(db, memberEmail)
	if !ok {
		return nil, fmt.Errorf("member %s not found", memberEmail)
	}
	desired := map[string]bool{}
	for _, name := range profileNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := findManagedProfileByName(db, name); !ok {
			return nil, fmt.Errorf("profile %s not found", name)
		}
		desired[name] = true
	}
	now := s.currentTime().Format(time.RFC3339)
	out := db.ProfileAccess[:0]
	existing := map[string]ProfileAccess{}
	for _, access := range db.ProfileAccess {
		if access.MemberID != member.ID {
			out = append(out, access)
			continue
		}
		if desired[access.ProfileName] {
			existing[access.ProfileName] = access
			out = append(out, access)
		}
	}
	result := make([]ProfileAccess, 0, len(desired))
	for profileName := range desired {
		access, ok := existing[profileName]
		if !ok {
			access = ProfileAccess{ProfileName: profileName, MemberID: member.ID, CreatedAt: now}
			out = append(out, access)
		}
		result = append(result, access)
	}
	db.ProfileAccess = out
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].ProfileName) < strings.ToLower(result[j].ProfileName)
	})
	return result, s.Save(db)
}

func membersForProfileInStore(s memberDataStore, profileName string) ([]PublicMember, error) {
	profileName = strings.TrimSpace(profileName)
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	var members []PublicMember
	for _, access := range db.ProfileAccess {
		if access.ProfileName != profileName {
			continue
		}
		if member, ok := findMemberByID(db, access.MemberID); ok {
			members = append(members, publicMember(member))
		}
	}
	sort.Slice(members, func(i, j int) bool {
		return strings.ToLower(members[i].Email) < strings.ToLower(members[j].Email)
	})
	return members, nil
}

func listReleaseRemindersInStore(s memberDataStore, memberEmail string) ([]ReleaseReminder, error) {
	memberEmail = normalizeEmail(memberEmail)
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	var member Member
	isAdmin := false
	if memberEmail != "" {
		if found, ok := findMemberByEmailOrUsername(db, memberEmail); ok {
			member = found
			isAdmin = found.Role == "admin"
		}
	}
	out := make([]ReleaseReminder, 0, len(db.Reminders))
	for _, reminder := range db.Reminders {
		if isAdmin || memberEmail == "" || memberHasProfileAccess(db, member, reminder.ProfileName) {
			out = append(out, reminder)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].ProfileName) < strings.ToLower(out[j].ProfileName)
	})
	return out, nil
}

func releaseReminderInStore(s memberDataStore, profileName string) (ReleaseReminder, bool, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return ReleaseReminder{}, false, nil
	}
	db, err := s.Load()
	if err != nil {
		return ReleaseReminder{}, false, err
	}
	for _, reminder := range db.Reminders {
		if reminder.ProfileName == profileName {
			return reminder, true, nil
		}
	}
	return ReleaseReminder{}, false, nil
}

func normalizeReleaseReminder(reminder ReleaseReminder) ReleaseReminder {
	reminder.ProfileName = strings.TrimSpace(reminder.ProfileName)
	reminder.AppleEmail = normalizeEmail(reminder.AppleEmail)
	reminder.OwnerEmail = normalizeEmail(reminder.OwnerEmail)
	reminder.LastExtendedByEmail = normalizeEmail(reminder.LastExtendedByEmail)
	if reminder.Status == "" {
		reminder.Status = ReleaseReminderStatusActive
	}
	return reminder
}

func normalizeReleaseReminderCallback(current, updated ReleaseReminder, now string) ReleaseReminder {
	updated.ProfileName = current.ProfileName
	updated.CreatedAt = current.CreatedAt
	updated = normalizeReleaseReminder(updated)
	updated.UpdatedAt = now
	return updated
}

func mergeReleaseReminderUpsert(current, updated ReleaseReminder, now string) ReleaseReminder {
	updated = normalizeReleaseReminderCallback(current, updated, now)
	if current.HostID != updated.HostID ||
		current.AppleEmail != updated.AppleEmail ||
		(current.Status == ReleaseReminderStatusReleased && updated.Status != ReleaseReminderStatusReleased) {
		return resetAutoReleaseForNewCycle(updated)
	}
	updated.AutoReleaseEnabled = current.AutoReleaseEnabled
	updated.AutoReleaseAt = current.AutoReleaseAt
	updated.AutoReleaseStartedAt = current.AutoReleaseStartedAt
	updated.AutoReleaseLastAttemptAt = current.AutoReleaseLastAttemptAt
	updated.AutoReleaseAttempts = current.AutoReleaseAttempts
	updated.AutoReleaseLastError = current.AutoReleaseLastError
	updated.AutoReleaseState = current.AutoReleaseState
	return updated
}

func resetAutoReleaseForNewCycle(reminder ReleaseReminder) ReleaseReminder {
	reminder.AutoReleaseEnabled = false
	reminder.AutoReleaseAt = ""
	reminder.AutoReleaseStartedAt = ""
	reminder.AutoReleaseLastAttemptAt = ""
	reminder.AutoReleaseAttempts = 0
	reminder.AutoReleaseLastError = ""
	reminder.AutoReleaseState = ""
	return reminder
}

func releaseReminderNotFoundError(profileName string) error {
	return fmt.Errorf("%w for profile %s", ErrReleaseReminderNotFound, profileName)
}

func memberHasProfileAccess(db MemberData, member Member, profileName string) bool {
	if member.ID == "" {
		return false
	}
	for _, access := range db.ProfileAccess {
		if access.ProfileName == profileName && access.MemberID == member.ID {
			return true
		}
	}
	return false
}

func webSettingsInStore(s memberDataStore) (WebSettings, error) {
	db, err := s.Load()
	if err != nil {
		return WebSettings{}, err
	}
	return publicWebSettings(db.Settings), nil
}

func updateWebSettingsInStore(s memberDataStore, update WebSettings) (WebSettings, error) {
	db, err := s.Load()
	if err != nil {
		return WebSettings{}, err
	}
	db.Settings.DefaultOwnerEmail = normalizeEmail(update.DefaultOwnerEmail)
	db.Settings.DefaultStatusFilter = strings.TrimSpace(update.DefaultStatusFilter)
	db.Settings.BackgroundConfirm = update.BackgroundConfirm
	db.Settings.ShowReleased = update.ShowReleased
	if err := s.Save(db); err != nil {
		return WebSettings{}, err
	}
	return publicWebSettings(db.Settings), nil
}

func ensureAuthSecretInStore(s memberDataStore) (string, error) {
	db, err := s.Load()
	if err != nil {
		return "", err
	}
	if db.Settings.AuthSecret == "" {
		secret, err := randomToken(32)
		if err != nil {
			return "", err
		}
		db.Settings.AuthSecret = secret
		if err := s.Save(db); err != nil {
			return "", err
		}
	}
	return db.Settings.AuthSecret, nil
}

func recordEventInStore(s memberDataStore, event OperationEvent) error {
	db, err := s.Load()
	if err != nil {
		return err
	}
	if err := appendOperationEvent(&db, event, s.currentTime().Format(time.RFC3339)); err != nil {
		return err
	}
	return s.Save(db)
}

func recentEventsInStore(s memberDataStore, appleEmail string, limit int) ([]OperationEvent, error) {
	db, err := s.Load()
	if err != nil {
		return nil, err
	}
	appleEmail = normalizeEmail(appleEmail)
	var out []OperationEvent
	for i := len(db.Events) - 1; i >= 0; i-- {
		event := db.Events[i]
		if appleEmail != "" && !strings.EqualFold(event.AppleEmail, appleEmail) {
			continue
		}
		out = append(out, event)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s MemberStore) currentTime() time.Time {
	return s.normalize().Now()
}
