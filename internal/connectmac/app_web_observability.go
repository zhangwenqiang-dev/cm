package connectmac

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	maxWebMutationRequestBody = 1 << 20
	maxBufferedWebResponse    = 2 << 20
)

var errWebRequestBodyTooLarge = errors.New("request body is too large")
var errBufferedWebResponseTooLarge = errors.New("response is too large")

type webAuditState struct {
	event *OperationEvent
}

type webAuditStateKey struct{}

type webAuditSpec struct {
	action            string
	profile           string
	appleEmail        string
	targetMemberEmail string
	message           string
	identity          string
	tokenExisted      bool
}

type bufferedWebResponse struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	limit    int
	overflow bool
}

type trackingWebResponse struct {
	http.ResponseWriter
	committed bool
	status    int
}

func (w *trackingWebResponse) WriteHeader(status int) {
	if w.committed {
		return
	}
	w.committed = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackingWebResponse) Write(data []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

// Unwrap lets http.ResponseController preserve optional capabilities such as
// flushing or hijacking without making unsupported interfaces appear present.
func (w *trackingWebResponse) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func newBufferedWebResponse() *bufferedWebResponse {
	return &bufferedWebResponse{header: make(http.Header), limit: maxBufferedWebResponse}
}

func (w *bufferedWebResponse) Header() http.Header {
	return w.header
}

func (w *bufferedWebResponse) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
}

func (w *bufferedWebResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.overflow || len(data) > w.limit-w.body.Len() {
		w.overflow = true
		return 0, errBufferedWebResponseTooLarge
	}
	return w.body.Write(data)
}

func (w *bufferedWebResponse) reset() {
	w.header = make(http.Header)
	w.body.Reset()
	w.status = 0
	w.overflow = false
}

func (w *bufferedWebResponse) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *bufferedWebResponse) flushTo(dst http.ResponseWriter) {
	for key, values := range w.header {
		dst.Header().Del(key)
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
	dst.WriteHeader(w.statusCode())
	_, _ = dst.Write(w.body.Bytes())
}

func (a App) withWebObservability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID, err := newRequestID(time.Now(), rand.Reader)
		if err != nil {
			requestID = fmt.Sprintf("req-%d", time.Now().UTC().UnixNano())
		}
		w.Header().Set("X-Request-ID", requestID)

		op := OperationContext{
			RequestID: requestID,
			Source:    "web",
			Route:     r.URL.Path,
			Method:    r.Method,
		}
		state := &webAuditState{}
		ctx := context.WithValue(withOperationContext(r.Context(), op), webAuditStateKey{}, state)
		r = r.WithContext(ctx)

		action, audited := webMutationAction(r.URL.Path)
		audited = audited && isWebMutation(r.Method)
		var body []byte
		var bodyErr error
		if isWebMutation(r.Method) && strings.HasPrefix(r.URL.Path, "/api/") {
			body, bodyErr = readBoundedWebRequestBody(r)
		}
		if audited {
			if member, ok := a.currentWebMember(r); ok {
				op.Actor = auditActorForMember(member)
				r = r.WithContext(context.WithValue(withOperationContext(r.Context(), op), webAuditStateKey{}, state))
			}
		}
		if isRawWebUpgradeRoute(r.URL.Path) {
			a.serveRawWebWithRecovery(w, r, next)
			return
		}
		if !audited {
			if bodyErr != nil {
				status := http.StatusBadRequest
				message := "failed to read request body"
				if errors.Is(bodyErr, errWebRequestBodyTooLarge) {
					status = http.StatusRequestEntityTooLarge
					message = errWebRequestBodyTooLarge.Error()
				}
				writeWebErrorResponse(w, status, message)
				return
			}
			if isDynamicWebAPI(r.URL.Path) {
				response := newBufferedWebResponse()
				a.serveBufferedWebWithRecovery(response, r, next)
				if response.overflow {
					response.reset()
					writeWebError(response, http.StatusInternalServerError, errBufferedWebResponseTooLarge.Error())
				}
				response.flushTo(w)
				return
			}
			a.serveWebWithRecovery(w, r, next)
			return
		}

		spec := a.webAuditSpecFromBody(r, action, body)
		if spec.action == "" && bodyErr == nil {
			response := newBufferedWebResponse()
			a.serveBufferedWebWithRecovery(response, r, next)
			if response.overflow {
				response.reset()
				writeWebError(response, http.StatusInternalServerError, errBufferedWebResponseTooLarge.Error())
			}
			response.flushTo(w)
			return
		}
		response := newBufferedWebResponse()
		panicked := false
		if bodyErr != nil {
			status := http.StatusBadRequest
			message := "failed to read request body"
			if errors.Is(bodyErr, errWebRequestBodyTooLarge) {
				status = http.StatusRequestEntityTooLarge
				message = errWebRequestBodyTooLarge.Error()
			}
			spec.message = message
			writeWebError(response, status, message)
		} else {
			panicked = a.serveBufferedWebWithRecovery(response, r, next)
			if response.overflow {
				response.reset()
				spec.message = errBufferedWebResponseTooLarge.Error()
				writeWebError(response, http.StatusInternalServerError, errBufferedWebResponseTooLarge.Error())
			}
		}
		if state.event == nil {
			event := a.webTerminalAuditEvent(r, spec, response.statusCode(), panicked)
			state.event = &event
		} else {
			if state.event.Profile == "" {
				state.event.Profile = spec.profile
			}
			if state.event.AppleEmail == "" {
				state.event.AppleEmail = spec.appleEmail
			}
			if state.event.TargetMemberEmail == "" {
				state.event.TargetMemberEmail = spec.targetMemberEmail
			}
		}
		if err := a.recordAudit(r.Context(), *state.event); err != nil {
			classified := classifyOperationalError(err)
			a.writeRuntimeLog(LogEntry{
				Level:            "error",
				Action:           "audit.persistence.failed",
				RequestID:        requestID,
				Operation:        state.event.Action,
				Source:           "web",
				Phase:            "persist",
				ErrorCode:        classified.Code,
				ActorMemberID:    op.Actor.MemberID,
				ActorMemberEmail: op.Actor.MemberEmail,
				ActorMemberName:  op.Actor.MemberName,
				Message:          "failed to persist web audit event",
			})
			// The business mutation may already be committed. Preserve its response
			// so a client retry cannot repeat a successful password, token, or member
			// mutation. The local runtime failure log remains available for repair.
		}
		response.flushTo(w)
	})
}

func (a App) serveWebWithRecovery(w http.ResponseWriter, r *http.Request, next http.Handler) {
	tracked := &trackingWebResponse{ResponseWriter: w}
	defer func() {
		if recover() == nil {
			return
		}
		op := operationContextFrom(r.Context())
		a.writeRuntimeLog(LogEntry{
			Level:      "error",
			Action:     "web.panic",
			RequestID:  op.RequestID,
			Operation:  r.URL.Path,
			Source:     "web",
			Phase:      "handler",
			ErrorCode:  "internal_error",
			HTTPStatus: http.StatusInternalServerError,
			Message:    "web request failed unexpectedly",
		})
		if !tracked.committed {
			writeWebErrorResponse(tracked, http.StatusInternalServerError, "internal server error")
		}
	}()
	next.ServeHTTP(tracked, r)
}

func (a App) serveRawWebWithRecovery(w http.ResponseWriter, r *http.Request, next http.Handler) {
	defer func() {
		if recover() == nil {
			return
		}
		op := operationContextFrom(r.Context())
		a.writeRuntimeLog(LogEntry{
			Level:      "error",
			Action:     "web.panic",
			RequestID:  op.RequestID,
			Operation:  r.URL.Path,
			Source:     "web",
			Phase:      "handler",
			ErrorCode:  "internal_error",
			HTTPStatus: http.StatusInternalServerError,
			Message:    "web upgrade request failed unexpectedly",
		})
	}()
	next.ServeHTTP(w, r)
}

func (a App) serveBufferedWebWithRecovery(w *bufferedWebResponse, r *http.Request, next http.Handler) (panicked bool) {
	defer func() {
		if recover() == nil {
			return
		}
		panicked = true
		w.reset()
		writeWebError(w, http.StatusInternalServerError, "internal server error")
		op := operationContextFrom(r.Context())
		a.writeRuntimeLog(LogEntry{
			Level:      "error",
			Action:     "web.panic",
			RequestID:  op.RequestID,
			Operation:  r.URL.Path,
			Source:     "web",
			Phase:      "handler",
			ErrorCode:  "internal_error",
			HTTPStatus: http.StatusInternalServerError,
			Message:    "web request failed unexpectedly",
		})
	}()
	next.ServeHTTP(w, r)
	return false
}

func writeWebErrorResponse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(webAPIResponse{OK: false, Code: status, Error: message})
}

func (a App) operationContextForRequest(r *http.Request) OperationContext {
	op := operationContextFrom(r.Context())
	if op.RequestID == "" {
		op.RequestID = strings.TrimSpace(r.Header.Get("X-Request-ID"))
	}
	if op.Source == "" {
		op.Source = "web"
	}
	if op.Route == "" {
		op.Route = r.URL.Path
	}
	if op.Method == "" {
		op.Method = r.Method
	}
	if op.Actor.MemberEmail == "" {
		if member, ok := a.currentWebMember(r); ok {
			op.Actor = auditActorForMember(member)
		}
	}
	return op
}

func (a App) recordAudit(ctx context.Context, event OperationEvent) error {
	op := operationContextFrom(ctx)
	if event.RequestID == "" {
		event.RequestID = op.RequestID
	}
	if event.JobID == "" {
		event.JobID = op.JobID
	}
	if event.Source == "" {
		event.Source = op.Source
	}
	failedAuthentication := event.Action == "auth.login.failed" || event.Action == "auth.setup.failed"
	if !failedAuthentication {
		if event.MemberID == "" {
			event.MemberID = op.Actor.MemberID
		}
		if event.MemberEmail == "" {
			event.MemberEmail = op.Actor.MemberEmail
		}
		if event.MemberName == "" {
			event.MemberName = op.Actor.MemberName
		}
	}
	return a.MemberStore.RecordEvent(event)
}

func (a App) recordAuthorizationDenied(r *http.Request, reason string) {
	if !isWebMutation(r.Method) {
		return
	}
	state, _ := r.Context().Value(webAuditStateKey{}).(*webAuditState)
	if state == nil || state.event != nil {
		return
	}
	op := a.operationContextForRequest(r)
	state.event = &OperationEvent{
		Action:      "authorization.denied",
		MemberID:    op.Actor.MemberID,
		MemberEmail: op.Actor.MemberEmail,
		MemberName:  op.Actor.MemberName,
		RequestID:   op.RequestID,
		Source:      "web",
		Phase:       "authorize",
		Status:      "failed",
		ErrorCode:   "authorization_denied",
		Message:     strings.TrimSpace(reason),
	}
}

func auditActorForMember(member Member) AuditActor {
	return AuditActor{
		MemberID:    member.ID,
		MemberEmail: normalizeEmail(member.Email),
		MemberName:  strings.TrimSpace(member.Name),
	}
}

func (a App) webAuditSpecFromBody(r *http.Request, action string, body []byte) webAuditSpec {
	values := map[string]interface{}{}
	_ = json.Unmarshal(body, &values)

	spec := webAuditSpec{
		action:            action,
		profile:           stringValue(values, "profile"),
		appleEmail:        normalizeEmail(stringValue(values, "apple_email")),
		targetMemberEmail: normalizeEmail(firstStringValue(values, "member_email", "email", "original_email")),
		identity:          normalizeEmail(firstStringValue(values, "username", "email")),
	}
	if r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/auth/setup" {
		spec.targetMemberEmail = spec.identity
	}
	switch r.URL.Path {
	case "/api/auth/logout":
		spec.targetMemberEmail = operationContextFrom(r.Context()).Actor.MemberEmail
	case "/api/auth/change-password":
		spec.targetMemberEmail = operationContextFrom(r.Context()).Actor.MemberEmail
	case "/api/auth/token", "/api/member/token":
		if r.URL.Path == "/api/auth/token" {
			spec.targetMemberEmail = operationContextFrom(r.Context()).Actor.MemberEmail
		}
		tokenAction := strings.ToLower(strings.TrimSpace(stringValue(values, "action")))
		if tokenAction == "status" {
			return webAuditSpec{}
		}
		spec.tokenExisted = a.webMemberTokenExists(spec.targetMemberEmail)
		if tokenAction == "delete" {
			spec.action = "auth.token.deleted"
		} else if spec.tokenExisted {
			spec.action = "auth.token.regenerated"
		} else {
			spec.action = "auth.token.generated"
		}
	case "/api/member/enable":
		spec.action = "member.enabled"
	case "/api/member/disable":
		spec.action = "member.disabled"
	case "/api/member/assign":
		spec.action = "member.assignment.granted"
	case "/api/member/unassign":
		spec.action = "member.assignment.removed"
	case "/api/member/profiles":
		spec.action = "profile.access.replaced"
	case "/api/member/update":
		spec.targetMemberEmail = normalizeEmail(firstStringValue(values, "original_email", "email"))
	case "/api/managed-profile/save":
		yaml := stringValue(values, "profile_yaml")
		if profile, parseErr := ParseSingleProfileYAML(yaml); parseErr == nil {
			spec.profile = profile.Name
			spec.appleEmail = normalizeEmail(profile.AWS.AccountEmail)
			if a.webManagedProfileExists(profile.Name) {
				spec.action = "profile.updated"
			} else {
				spec.action = "profile.created"
			}
		}
	case "/api/managed-profile/status":
		if boolValue(values, "enabled") {
			spec.action = "profile.enabled"
		} else {
			spec.action = "profile.disabled"
		}
	case "/api/managed-profile/access":
		if boolValue(values, "grant") {
			spec.action = "profile.access.granted"
		} else {
			spec.action = "profile.access.removed"
		}
	}
	spec.message = webAuditMessage(spec.action, values)
	return spec
}

func (a App) webTerminalAuditEvent(r *http.Request, spec webAuditSpec, status int, panicked bool) OperationEvent {
	op := a.operationContextForRequest(r)
	success := status >= 200 && status < 400 && !panicked
	action := spec.action
	if action == "auth.setup" || action == "auth.login" {
		if success {
			action += ".succeeded"
		} else {
			action += ".failed"
		}
	}
	actor := op.Actor
	authenticationAction := strings.HasPrefix(action, "auth.login.") || strings.HasPrefix(action, "auth.setup.")
	if authenticationAction && success && spec.identity != "" {
		actor = AuditActor{}
		if db, err := a.MemberStore.Load(); err == nil {
			if member, ok := findMemberByEmailOrUsername(db, spec.identity); ok {
				actor = auditActorForMember(member)
			}
		}
	} else if authenticationAction && !success {
		actor = AuditActor{}
	}
	eventStatus := "success"
	errorCode := ""
	if !success {
		eventStatus = "failed"
		errorCode = webAuditErrorCode(status)
	}
	message := spec.message
	if strings.HasPrefix(action, "auth.login.") {
		message = fmt.Sprintf(
			"identity=%s client_ip=%s user_agent=%s",
			spec.identity,
			webClientIP(r),
			truncateAuditValue(r.UserAgent(), 160),
		)
	} else if action == "auth.setup.failed" {
		message = "identity=" + spec.identity
	}
	return OperationEvent{
		Action:            action,
		Profile:           spec.profile,
		AppleEmail:        spec.appleEmail,
		MemberID:          actor.MemberID,
		MemberEmail:       actor.MemberEmail,
		MemberName:        actor.MemberName,
		RequestID:         op.RequestID,
		Source:            "web",
		Phase:             "completed",
		TargetMemberEmail: spec.targetMemberEmail,
		ErrorCode:         errorCode,
		Confirmed:         false,
		Status:            eventStatus,
		Message:           message,
	}
}

func webMutationAction(path string) (string, bool) {
	actions := map[string]string{
		"/api/auth/setup":             "auth.setup",
		"/api/auth/login":             "auth.login",
		"/api/auth/logout":            "auth.logout.succeeded",
		"/api/auth/update-email":      "auth.email.changed",
		"/api/auth/change-password":   "auth.password.changed",
		"/api/auth/token":             "auth.token.generated",
		"/api/settings":               "settings.updated",
		"/api/member/add":             "member.created",
		"/api/member/update":          "member.updated",
		"/api/member/password":        "member.password.changed",
		"/api/member/token":           "auth.token.generated",
		"/api/member/enable":          "member.enabled",
		"/api/member/disable":         "member.disabled",
		"/api/member/assign":          "member.assignment.granted",
		"/api/member/unassign":        "member.assignment.removed",
		"/api/member/profiles":        "profile.access.replaced",
		"/api/managed-profile/save":   "profile.created",
		"/api/managed-profile/status": "profile.enabled",
		"/api/managed-profile/delete": "profile.deleted",
		"/api/managed-profile/access": "profile.access.granted",
	}
	action, ok := actions[path]
	return action, ok
}

func isWebMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isDynamicWebAPI(path string) bool {
	return strings.HasPrefix(path, "/api/") && !isRawWebUpgradeRoute(path)
}

func isRawWebUpgradeRoute(path string) bool {
	return path == "/api/terminal/ws"
}

func readBoundedWebRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	if r.ContentLength > maxWebMutationRequestBody {
		return nil, errWebRequestBodyTooLarge
	}
	reader := io.LimitReader(r.Body, maxWebMutationRequestBody+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(body) > maxWebMutationRequestBody {
		return nil, errWebRequestBodyTooLarge
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func webAuditMessage(action string, values map[string]interface{}) string {
	var fields []string
	switch action {
	case "auth.setup":
		fields = presentAuditFields(values, "name", "email", "password")
	case "auth.logout.succeeded":
		fields = []string{"session"}
	case "auth.email.changed":
		fields = presentAuditFields(values, "email")
	case "auth.token.generated", "auth.token.regenerated", "auth.token.deleted":
		fields = []string{"token"}
	case "settings.updated":
		fields = presentAuditFields(values, "default_owner_email", "default_status_filter", "background_confirm", "show_released")
	case "member.created":
		fields = presentAuditFields(values, "name", "email", "role", "password")
	case "member.updated":
		fields = presentAuditFields(values, "name", "email", "role")
	case "member.password.changed", "auth.password.changed":
		fields = []string{"password"}
	case "member.enabled", "member.disabled":
		fields = []string{"enabled"}
	case "member.assignment.granted", "member.assignment.removed":
		fields = presentAuditFields(values, "apple_email", "member_email", "relation")
	case "profile.created", "profile.updated":
		fields = []string{"profile"}
	case "profile.enabled", "profile.disabled":
		fields = []string{"enabled"}
	case "profile.access.replaced", "profile.access.granted", "profile.access.removed":
		fields = []string{"profile_access"}
	}
	if len(fields) == 0 {
		return "operation=" + action
	}
	return "changed_fields=" + strings.Join(fields, ",")
}

func presentAuditFields(values map[string]interface{}, candidates ...string) []string {
	fields := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		value, exists := values[candidate]
		if !exists || value == nil {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		fields = append(fields, candidate)
	}
	return fields
}

func webAuditErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusMethodNotAllowed:
		return "validation_error"
	case http.StatusUnauthorized:
		return "authentication_failed"
	case http.StatusForbidden:
		return "authorization_denied"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "operation_failed"
	}
}

func webClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	peerIP := net.ParseIP(host)
	if peerIP != nil && peerIP.IsLoopback() {
		forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for i := len(forwarded) - 1; i >= 0; i-- {
			candidate := strings.TrimSpace(forwarded[i])
			if candidate != "" && net.ParseIP(candidate) != nil {
				return truncateAuditValue(candidate, 64)
			}
		}
	}
	return truncateAuditValue(host, 64)
}

func truncateAuditValue(value string, limit int) string {
	value = sanitizeLogText(value)
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(value))
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func stringValue(values map[string]interface{}, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func firstStringValue(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(values, key); value != "" {
			return value
		}
	}
	return ""
}

func boolValue(values map[string]interface{}, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func (a App) webMemberTokenExists(email string) bool {
	if email == "" {
		return false
	}
	db, err := a.MemberStore.Load()
	if err != nil {
		return false
	}
	member, ok := findMemberByEmailOrUsername(db, email)
	return ok && member.APITokenHash != ""
}

func (a App) webManagedProfileExists(profileName string) bool {
	if profileName == "" {
		return false
	}
	db, err := a.MemberStore.Load()
	if err != nil {
		return false
	}
	for _, profile := range db.Profiles {
		if profile.Name == profileName {
			return true
		}
	}
	return false
}
