package connectmac

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type webSyncRequest struct {
	Profile    string `json:"profile"`
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
}

func (a App) webLocalIntentHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Profile   string `json:"profile"`
			Operation string `json:"operation"`
			RequestID string `json:"request_id"`
		}
		if err := decodeWebJSON(r, &req); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Operation = strings.TrimSpace(req.Operation)
		switch req.Operation {
		case "connect", "vnc", "transfer":
		default:
			writeWebError(w, http.StatusBadRequest, "operation must be connect, vnc, or transfer")
			return
		}
		requestID, err := validateLocalAgentRequestID(req.RequestID)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		member, ok := a.currentWebMember(r)
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		profile, err := a.transferProfileForMember(member, strings.TrimSpace(req.Profile))
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errTransferProfileAccessDenied) {
				status = http.StatusForbidden
			}
			writeWebError(w, status, err.Error())
			return
		}
		event := OperationEvent{
			Action:      "local." + req.Operation + ".requested",
			Profile:     profile.Name,
			AppleEmail:  profile.AWS.AccountEmail,
			MemberID:    member.ID,
			MemberEmail: member.Email,
			MemberName:  member.Name,
			RequestID:   requestID,
			Source:      "web",
			Phase:       "requested",
			Status:      "requested",
			Message:     "local " + req.Operation + " requested",
		}
		if err := a.MemberStore.RecordEvent(event); err != nil {
			writeWebError(w, http.StatusInternalServerError, "record local operation intent failed")
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]string{"request_id": requestID}})
	}
}

func (a App) webSyncHistoryHandler() http.HandlerFunc {
	// Legacy global sync history is retained for compatibility with older clients.
	// The current Web UI uses the authenticated, member-scoped transfer record APIs.
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		items, err := a.SyncHistory.List(strings.TrimSpace(r.URL.Query().Get("profile")), 50)
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"items": items}})
	}
}

func (a App) webSyncHistoryDeleteHandler() http.HandlerFunc {
	// Legacy global sync history is retained for compatibility with older clients.
	// The current Web UI never calls this unscoped delete endpoint.
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := decodeWebJSON(r, &req); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.SyncHistory.Delete(req.ID); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true})
	}
}

func (a App) webSyncPushHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req webSyncRequest
		if err := decodeWebJSON(r, &req); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		profile, localPath, remotePath, err := a.prepareWebSync(r, configPath, req, "push")
		if err != nil {
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: err.Error()})
			return
		}
		if _, err := os.Stat(localPath); err != nil {
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: fmt.Sprintf("read local path %s: %v", localPath, err)})
			return
		}
		rsyncArgs, err := RsyncPushArgs(profile, localPath, remotePath, mergeSyncFilters(profile.Sync.Push, SyncFilters{}))
		if err != nil {
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: err.Error()})
			return
		}
		if err := a.Runner.RunRsync(r.Context(), rsyncArgs); err != nil {
			resp := webAPIResponse{OK: false, Code: 1, Error: fmt.Sprintf("rsync failed: %v", err)}
			a.logWebResponse("web.sync.push", profile.Name, resp)
			a.recordWebEvent(configPath, profile.Name, "push", true, resp)
			writeWebJSON(w, resp)
			return
		}
		item, err := a.SyncHistory.Upsert(SyncHistoryItem{
			Profile:    profile.Name,
			AppleEmail: profile.AWS.AccountEmail,
			Direction:  "push",
			LocalPath:  localPath,
			RemotePath: remotePath,
		})
		if err != nil {
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: err.Error()})
			return
		}
		output := fmt.Sprintf("Push: %s -> %s\n", localPath, RemoteTarget(profile, remotePath))
		resp := webAPIResponse{OK: true, Output: output, Data: map[string]interface{}{"item": item}}
		a.logWebResponse("web.sync.push", profile.Name, resp)
		a.recordWebEvent(configPath, profile.Name, "push", true, resp)
		writeWebJSON(w, resp)
	}
}

func (a App) webSyncPullHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req webSyncRequest
		if err := decodeWebJSON(r, &req); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		profile, localPath, remotePath, err := a.prepareWebSync(r, configPath, req, "pull")
		if err != nil {
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: err.Error()})
			return
		}
		if err := os.MkdirAll(localPath, 0o755); err != nil {
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: fmt.Sprintf("create local dir %s: %v", localPath, err)})
			return
		}
		rsyncArgs, err := RsyncPullArgs(profile, remotePath, localPath, mergeSyncFilters(profile.Sync.Pull, SyncFilters{}))
		if err != nil {
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: err.Error()})
			return
		}
		if err := a.Runner.RunRsync(r.Context(), rsyncArgs); err != nil {
			resp := webAPIResponse{OK: false, Code: 1, Error: fmt.Sprintf("rsync failed: %v", err)}
			a.logWebResponse("web.sync.pull", profile.Name, resp)
			a.recordWebEvent(configPath, profile.Name, "pull", true, resp)
			writeWebJSON(w, resp)
			return
		}
		item, err := a.SyncHistory.Upsert(SyncHistoryItem{
			Profile:    profile.Name,
			AppleEmail: profile.AWS.AccountEmail,
			Direction:  "pull",
			LocalPath:  localPath,
			RemotePath: remotePath,
		})
		if err != nil {
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: err.Error()})
			return
		}
		output := fmt.Sprintf("Pull: %s -> %s\n", RemoteTarget(profile, remotePath), localPath)
		resp := webAPIResponse{OK: true, Output: output, Data: map[string]interface{}{"item": item}}
		a.logWebResponse("web.sync.pull", profile.Name, resp)
		a.recordWebEvent(configPath, profile.Name, "pull", true, resp)
		writeWebJSON(w, resp)
	}
}

func (a App) prepareWebSync(r *http.Request, configPath string, req webSyncRequest, direction string) (Profile, string, string, error) {
	profile, err := a.prepareWebTerminal(r, configPath, req.Profile)
	if err != nil {
		return Profile{}, "", "", err
	}
	if a.Validator.CheckRsync != nil {
		if err := a.Validator.CheckRsync(); err != nil {
			return Profile{}, "", "", err
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return Profile{}, "", "", err
	}
	localPath := strings.TrimSpace(req.LocalPath)
	remotePath := strings.TrimSpace(req.RemotePath)
	switch direction {
	case "push":
		if localPath == "" {
			localPath = cwd
		}
		if remotePath == "" {
			remotePath = "~/Downloads/"
		}
		remotePath = NormalizeRemotePath(remotePath)
	case "pull":
		if localPath == "" {
			localPath = cwd
		}
		if remotePath == "" {
			remotePath = "~/Downloads/"
		}
	default:
		return Profile{}, "", "", fmt.Errorf("unknown sync direction %q", direction)
	}
	return profile, localPath, remotePath, nil
}

func decodeWebJSON(r *http.Request, dest interface{}) error {
	if err := json.NewDecoder(r.Body).Decode(dest); err != nil {
		return fmt.Errorf("invalid json body")
	}
	return nil
}
