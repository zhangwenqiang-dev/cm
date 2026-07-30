package connectmac

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const webTransferLogAction = "web.transfer-record"

var errTransferProfileAccessDenied = errors.New("transfer profile access denied")

func (a App) webTransferRecordsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			a.logTransferAuthRejection("", "", "", "login required")
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		records, err := a.MemberStore.ListTransferRecords(member.ID, strings.TrimSpace(r.URL.Query().Get("profile")))
		if err != nil {
			a.logTransferPersistenceError(member, TransferRecord{}, err)
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for index := range records {
			if records[index].Phase == "" {
				records[index].Phase = inferredTransferPhase(records[index].Status, records[index].Percent)
			}
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"records": records}})
	}
}

func (a App) webTransferRecordStartHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			a.logTransferAuthRejection("", "", "", "login required")
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		var req struct {
			Profile    string `json:"profile"`
			Direction  string `json:"direction"`
			LocalPath  string `json:"local_path"`
			RemotePath string `json:"remote_path"`
		}
		if err := decodeStrictWebJSON(r, &req); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Profile = strings.TrimSpace(req.Profile)
		if req.Profile == "" {
			writeWebError(w, http.StatusBadRequest, "profile is required")
			return
		}
		profile, err := a.transferProfileForMember(member, req.Profile)
		if err != nil {
			if errors.Is(err, errTransferProfileAccessDenied) {
				a.logTransferAuthRejection(member.Email, req.Profile, req.Direction, err.Error())
				writeWebError(w, http.StatusForbidden, err.Error())
				return
			}
			a.logTransferPersistenceError(member, TransferRecord{ProfileName: req.Profile, Direction: req.Direction}, err)
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		attemptedRecord := TransferRecord{
			MemberID: member.ID, MemberEmail: member.Email, ProfileName: profile.Name,
			AppleEmail: profile.AWS.AccountEmail, Direction: req.Direction,
			LocalPath: strings.TrimSpace(req.LocalPath), RemotePath: strings.TrimSpace(req.RemotePath),
			Status: TransferStatusCreated, Phase: TransferPhasePreparing,
		}
		record, err := a.MemberStore.CreateTransferRecord(member.ID, attemptedRecord)
		if err != nil {
			if isTransferRecordDomainError(err) {
				writeWebError(w, http.StatusBadRequest, err.Error())
				return
			}
			a.logTransferPersistenceError(member, attemptedRecord, err)
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.writeTransferLog("info", "created", member, record, 0, "")
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"record": record}})
	}
}

func (a App) webTransferRecordUpdateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			a.logTransferAuthRejection("", "", "", "login required")
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		var req struct {
			ID           string `json:"id"`
			LocalJobID   string `json:"local_job_id"`
			Status       string `json:"status"`
			Phase        string `json:"phase"`
			Percent      int    `json:"percent"`
			ErrorSummary string `json:"error_summary"`
			ElapsedMS    int64  `json:"elapsed_ms"`
		}
		if err := decodeStrictWebJSON(r, &req); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		now := time.Now().Format(time.RFC3339)
		record, err := a.MemberStore.UpdateTransferRecord(member.ID, req.ID, req.LocalJobID, func(current TransferRecord) (TransferRecord, error) {
			wasTerminal := isTerminalTransferStatus(current.Status)
			current.LocalJobID = strings.TrimSpace(req.LocalJobID)
			current.Status = strings.TrimSpace(req.Status)
			current.Percent = req.Percent
			current.Phase = strings.TrimSpace(req.Phase)
			if current.Phase == "" {
				current.Phase = inferredTransferPhase(current.Status, current.Percent)
			}
			current.ErrorSummary = strings.TrimSpace(req.ErrorSummary)
			if !wasTerminal && current.StartedAt == "" && current.Status != TransferStatusCreated {
				current.StartedAt = now
			}
			if !wasTerminal && isTerminalTransferStatus(current.Status) {
				current.FinishedAt = now
			}
			return current, nil
		})
		if err != nil {
			if errors.Is(err, ErrTransferRecordNotFound) {
				a.logTransferAuthRejection(member.Email, "", "", "transfer record not found")
				writeWebError(w, http.StatusNotFound, "transfer record not found")
				return
			}
			if isTransferRecordDomainError(err) {
				writeWebError(w, http.StatusBadRequest, err.Error())
				return
			}
			a.logTransferPersistenceError(member, TransferRecord{ID: req.ID, LocalJobID: req.LocalJobID, Status: req.Status, Phase: req.Phase, Percent: req.Percent}, err)
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		message := "milestone"
		if isTerminalTransferStatus(record.Status) {
			message = "terminal"
		}
		a.writeTransferLog("info", message, member, record, req.ElapsedMS, record.ErrorSummary)
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"record": record}})
	}
}

func (a App) webTransferRecordDeleteHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			a.logTransferAuthRejection("", "", "", "login required")
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := decodeStrictWebJSON(r, &req); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.MemberStore.DeleteTransferRecord(member.ID, req.ID); err != nil {
			if errors.Is(err, ErrTransferRecordNotFound) {
				a.logTransferAuthRejection(member.Email, "", "", "transfer record not found")
				writeWebError(w, http.StatusNotFound, "transfer record not found")
				return
			}
			a.logTransferPersistenceError(member, TransferRecord{ID: req.ID}, err)
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true})
	}
}

func decodeStrictWebJSON(r *http.Request, dest interface{}) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return fmt.Errorf("invalid json body")
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid json body")
	}
	return nil
}

func isTransferRecordDomainError(err error) bool {
	switch err.Error() {
	case "transfer record ID is required",
		"terminal transfer record cannot be updated",
		"transfer record local job ID does not match",
		"transfer record local job ID cannot change",
		"transfer direction must be push or pull",
		"invalid transfer status",
		"invalid transfer phase",
		"transfer percent must be between 0 and 100",
		"invalid transfer status/phase/percent combination",
		"transfer percent cannot regress",
		"terminal transfer status cannot return to active":
		return true
	default:
		return false
	}
}

func (a App) transferProfileForMember(member Member, name string) (Profile, error) {
	records, err := a.MemberStore.ListManagedProfiles(member.Email)
	if err != nil {
		return Profile{}, err
	}
	for _, record := range records {
		if record.Name != name {
			continue
		}
		profile, err := ParseSingleProfileYAML(record.ProfileYAML)
		if err != nil {
			return Profile{}, err
		}
		return profile, nil
	}
	return Profile{}, fmt.Errorf("%w: profile %s is not assigned to %s", errTransferProfileAccessDenied, name, member.Email)
}

func (a App) writeTransferLog(level, message string, member Member, record TransferRecord, elapsedMS int64, detail string) {
	_ = a.LogManager.Write(LogEntry{
		Level: level, Action: webTransferLogAction, Message: strings.TrimSpace(message + " " + detail),
		Profile: record.ProfileName, AppleEmail: record.AppleEmail, MemberEmail: member.Email,
		TransferID: record.ID, LocalJobID: record.LocalJobID, Direction: record.Direction,
		Status: record.Status, Percent: record.Percent, ElapsedMS: elapsedMS,
		Phase: record.Phase,
	})
}

func (a App) logTransferAuthRejection(memberEmail, profile, direction, message string) {
	_ = a.LogManager.Write(LogEntry{Level: "warn", Action: webTransferLogAction, Message: "authorization rejected: " + message, MemberEmail: memberEmail, Profile: profile, Direction: direction})
}

func (a App) logTransferPersistenceError(member Member, record TransferRecord, err error) {
	a.writeTransferLog("error", "persistence error", member, record, 0, err.Error())
}
