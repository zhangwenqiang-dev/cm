package connectmac

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const webSessionCookie = "cm_session"
const webAPITokenPrefix = "cm_api_"

type webAuthMember struct {
	Member        PublicMember `json:"member,omitempty"`
	Authenticated bool         `json:"authenticated"`
	SetupRequired bool         `json:"setup_required"`
}

func (a App) webAuthMeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		setupRequired, err := a.webSetupRequired()
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		member, ok := a.currentWebMember(r)
		resp := webAuthMember{Authenticated: ok, SetupRequired: setupRequired}
		if ok {
			resp.Member = publicMember(member)
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: resp})
	}
}

func (a App) webAuthChallengeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		challenge, err := a.newWebChallenge()
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: challenge})
	}
}

func (a App) webAuthSetupHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		setupRequired, err := a.webSetupRequired()
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !setupRequired {
			writeWebError(w, http.StatusForbidden, "admin is already configured")
			return
		}
		var req struct {
			Name            string `json:"name"`
			Email           string `json:"email"`
			Password        string `json:"password"`
			ChallengeToken  string `json:"challenge_token"`
			ChallengeAnswer string `json:"challenge_answer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := a.verifyWebChallenge(req.ChallengeToken, req.ChallengeAnswer); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		member, err := a.MemberStore.SetupAdmin(req.Name, req.Email, req.Password)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.setWebSessionForRequest(w, r, member); err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.cleanupLocalConfigAfterLogin(configPath)
		writeWebJSON(w, webAPIResponse{OK: true, Data: webAuthMember{Authenticated: true, Member: publicMember(member)}})
	}
}

func (a App) webAuthLoginHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Username        string `json:"username"`
			Password        string `json:"password"`
			ChallengeToken  string `json:"challenge_token"`
			ChallengeAnswer string `json:"challenge_answer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := a.verifyWebChallenge(req.ChallengeToken, req.ChallengeAnswer); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		member, ok, err := a.MemberStore.VerifyMemberPassword(req.Username, req.Password)
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "invalid username or password")
			return
		}
		if err := a.setWebSessionForRequest(w, r, member); err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.cleanupLocalConfigAfterLogin(configPath)
		writeWebJSON(w, webAPIResponse{OK: true, Data: webAuthMember{Authenticated: true, Member: publicMember(member)}})
	}
}

func (a App) webAuthLogoutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		options := webSessionCookieOptions(r)
		http.SetCookie(w, &http.Cookie{
			Name:     webSessionCookie,
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: options.sameSite,
			Secure:   options.secure,
		})
		writeWebJSON(w, webAPIResponse{OK: true})
	}
}

func (a App) webAuthUpdateEmailHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.requireWebRoleValue(r, "admin")
		if !ok {
			writeWebError(w, http.StatusForbidden, "admin login required")
			return
		}
		var req struct {
			Email           string `json:"email"`
			Password        string `json:"password"`
			ChallengeToken  string `json:"challenge_token"`
			ChallengeAnswer string `json:"challenge_answer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := a.verifyWebChallenge(req.ChallengeToken, req.ChallengeAnswer); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		_, verified, err := a.MemberStore.VerifyMemberPassword(member.Email, req.Password)
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !verified {
			writeWebError(w, http.StatusUnauthorized, "current password is incorrect")
			return
		}
		updated, err := a.MemberStore.UpdateMemberEmail(member.ID, req.Email)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.setWebSessionForRequest(w, r, updated); err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: webAuthMember{Authenticated: true, Member: publicMember(updated)}})
	}
}

func (a App) webAuthChangePasswordHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.requireWebRoleValue(r, "viewer", "operator", "admin")
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		var req struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
			ConfirmPassword string `json:"confirm_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if req.NewPassword != req.ConfirmPassword {
			writeWebError(w, http.StatusBadRequest, "new passwords do not match")
			return
		}
		_, verified, err := a.MemberStore.VerifyMemberPassword(member.Email, req.CurrentPassword)
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !verified {
			writeWebError(w, http.StatusUnauthorized, "current password is incorrect")
			return
		}
		if err := a.MemberStore.SetMemberPassword(member.Email, req.NewPassword); err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true})
	}
}

func (a App) webAuthTokenHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		member, ok := a.requireWebRoleValue(r, "viewer", "operator", "admin")
		if !ok {
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data, err := a.applyWebAPITokenAction(member.Email, req.Action)
		if err != nil {
			writeWebError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: data})
	}
}

func (a App) webSettingsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			settings, err := a.MemberStore.WebSettings()
			if err != nil {
				writeWebError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"settings": settings}})
		case http.MethodPost:
			if _, ok := a.requireWebRoleValue(r, "admin"); !ok {
				a.recordAuthorizationDenied(r, "administrator role is required")
				writeWebError(w, http.StatusForbidden, "admin login required")
				return
			}
			var req WebSettings
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeWebError(w, http.StatusBadRequest, "invalid json body")
				return
			}
			settings, err := a.MemberStore.UpdateWebSettings(req)
			if err != nil {
				writeWebError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeWebJSON(w, webAPIResponse{OK: true, Data: map[string]interface{}{"settings": settings}})
		default:
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func (a App) requireWebRole(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.RemoteUserAPI {
			next(w, r)
			return
		}
		if _, ok := a.requireWebRoleValue(r, roles...); !ok {
			a.recordAuthorizationDenied(r, "login or required role is missing")
			writeWebError(w, http.StatusUnauthorized, "login required")
			return
		}
		next(w, r)
	}
}

func (a App) requireWebRoleValue(r *http.Request, roles ...string) (Member, bool) {
	member, ok := a.currentWebMember(r)
	if !ok || !member.Enabled {
		return Member{}, false
	}
	for _, role := range roles {
		if member.Role == role {
			return member, true
		}
	}
	return Member{}, false
}

func (a App) currentWebMember(r *http.Request) (Member, bool) {
	if member, ok := a.currentWebTokenMember(r); ok {
		return member, true
	}
	cookie, err := r.Cookie(webSessionCookie)
	if err != nil || cookie.Value == "" {
		return Member{}, false
	}
	email, ok := a.verifyWebSession(cookie.Value)
	if !ok {
		return Member{}, false
	}
	db, err := a.MemberStore.Load()
	if err != nil {
		return Member{}, false
	}
	member, ok := findMemberByEmailOrUsername(db, email)
	if !ok || !member.Enabled {
		return Member{}, false
	}
	return member, true
}

func (a App) currentWebTokenMember(r *http.Request) (Member, bool) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return Member{}, false
	}
	parts := strings.Fields(auth)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return Member{}, false
	}
	db, err := a.MemberStore.Load()
	if err != nil {
		return Member{}, false
	}
	member, ok := verifyMemberAPIToken(db, parts[1])
	if !ok || !member.Enabled {
		return Member{}, false
	}
	return member, true
}

func (a App) webSetupRequired() (bool, error) {
	hasMembers, err := a.MemberStore.HasPasswordMembers()
	if err != nil {
		return false, err
	}
	return !hasMembers, nil
}

type webSessionOptions struct {
	secure   bool
	sameSite http.SameSite
}

func (a App) setWebSession(w http.ResponseWriter, member Member) error {
	return a.setWebSessionWithOptions(w, member, webSessionOptions{sameSite: http.SameSiteLaxMode})
}

func (a App) setWebSessionForRequest(w http.ResponseWriter, r *http.Request, member Member) error {
	return a.setWebSessionWithOptions(w, member, webSessionCookieOptions(r))
}

func (a App) setWebSessionWithOptions(w http.ResponseWriter, member Member, options webSessionOptions) error {
	secret, err := a.MemberStore.EnsureAuthSecret()
	if err != nil {
		return err
	}
	expires := time.Now().Add(12 * time.Hour).Unix()
	payload := normalizeEmail(member.Email) + "|" + strconv.FormatInt(expires, 10)
	sig := signWebValue(secret, payload)
	http.SetCookie(w, &http.Cookie{
		Name:     webSessionCookie,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + sig)),
		Path:     "/",
		Expires:  time.Unix(expires, 0),
		HttpOnly: true,
		SameSite: options.sameSite,
		Secure:   options.secure,
	})
	return nil
}

func webSessionCookieOptions(r *http.Request) webSessionOptions {
	if webRequestIsHTTPS(r) {
		return webSessionOptions{secure: true, sameSite: http.SameSiteNoneMode}
	}
	return webSessionOptions{sameSite: http.SameSiteLaxMode}
}

func webRequestIsHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Ssl"), "on") {
		return true
	}
	return false
}

func (a App) verifyWebSession(value string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return "", false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return "", false
	}
	secret, err := a.MemberStore.EnsureAuthSecret()
	if err != nil {
		return "", false
	}
	payload := parts[0] + "|" + parts[1]
	expected := signWebValue(secret, payload)
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return "", false
	}
	return parts[0], true
}

func (a App) applyWebAPITokenAction(email, action string) (map[string]interface{}, error) {
	action = strings.TrimSpace(action)
	if action == "" {
		action = "generate"
	}
	db, err := a.MemberStore.Load()
	if err != nil {
		return nil, err
	}
	idx, ok := findMemberIndexByEmailOrUsername(db, email)
	if !ok || !db.Members[idx].Enabled {
		return nil, errors.New("member not found or disabled")
	}
	member := db.Members[idx]
	data := map[string]interface{}{"member": publicMember(member)}
	switch action {
	case "status":
		return data, nil
	case "delete":
		member.APITokenHash = ""
		member.APITokenAt = ""
	case "generate":
		token, err := newMemberAPIToken(member)
		if err != nil {
			return nil, err
		}
		member.APITokenHash = hashAPIToken(token)
		member.APITokenAt = time.Now().Format(time.RFC3339)
		data["token"] = token
	default:
		return nil, fmt.Errorf("unknown token action %q", action)
	}
	member.UpdatedAt = time.Now().Format(time.RFC3339)
	db.Members[idx] = member
	if err := a.MemberStore.Save(db); err != nil {
		return nil, err
	}
	data["member"] = publicMember(member)
	return data, nil
}

func newMemberAPIToken(member Member) (string, error) {
	nonce, err := randomToken(32)
	if err != nil {
		return "", err
	}
	payload := member.ID + "|" + nonce
	return webAPITokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(payload)), nil
}

func verifyMemberAPIToken(db MemberData, value string) (Member, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, webAPITokenPrefix) {
		return Member{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, webAPITokenPrefix))
	if err != nil {
		return Member{}, false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 2 {
		return Member{}, false
	}
	member, ok := findMemberByID(db, parts[0])
	if !ok || member.APITokenHash == "" {
		return Member{}, false
	}
	if !hmac.Equal([]byte(member.APITokenHash), []byte(hashAPIToken(value))) {
		return Member{}, false
	}
	return member, true
}

func hashAPIToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a App) newWebChallenge() (map[string]string, error) {
	secret, err := a.MemberStore.EnsureAuthSecret()
	if err != nil {
		return nil, err
	}
	left, err := randomSmallInt()
	if err != nil {
		return nil, err
	}
	right, err := randomSmallInt()
	if err != nil {
		return nil, err
	}
	answer := strconv.Itoa(left + right)
	nonce, err := randomToken(12)
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(5 * time.Minute).Unix()
	payload := nonce + "|" + answer + "|" + strconv.FormatInt(expires, 10)
	token := base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + signWebValue(secret, payload)))
	return map[string]string{
		"question": fmt.Sprintf("%d + %d = ?", left, right),
		"token":    token,
	}, nil
}

func (a App) verifyWebChallenge(token, answer string) error {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return errors.New("invalid challenge")
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 {
		return errors.New("invalid challenge")
	}
	expires, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return errors.New("challenge expired")
	}
	secret, err := a.MemberStore.EnsureAuthSecret()
	if err != nil {
		return err
	}
	payload := parts[0] + "|" + parts[1] + "|" + parts[2]
	if !hmac.Equal([]byte(signWebValue(secret, payload)), []byte(parts[3])) {
		return errors.New("invalid challenge")
	}
	if strings.TrimSpace(answer) != parts[1] {
		return errors.New("challenge answer is incorrect")
	}
	return nil
}

func signWebValue(secret, value string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func randomSmallInt() (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(9))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()) + 1, nil
}
