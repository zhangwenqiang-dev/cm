package connectmac

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestWebObservabilityRequestIDAndPanicRecovery(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	handler := app.withWebObservability(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("private panic detail")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
	if strings.Contains(rec.Body.String(), "private panic detail") {
		t.Fatalf("panic detail leaked: %s", rec.Body.String())
	}
}

func TestWebGETPollingDoesNotCreateSuccessAudit(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	rec := httptest.NewRecorder()

	app.newWebHandler("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
	events, err := app.MemberStore.RecentEvents("", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("GET polling created events: %+v", events)
	}
}

func TestWebAuthLoginFailureAuditIsAttributedAndRedacted(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	if _, err := app.MemberStore.SetupAdmin("Admin", "admin@example.com", "password123"); err != nil {
		t.Fatal(err)
	}
	challenge := webChallengeForTest(t, app)
	secretPassword := "wrong-password-secret"
	body := `{"username":"ADMIN@example.com","password":"` + secretPassword +
		`","challenge_token":"` + challenge["token"] +
		`","challenge_answer":"` + challenge["answer"] + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.Header.Set("User-Agent", "Audit Test Browser token=browser-secret")
	req.RemoteAddr = "192.0.2.10:4321"
	rec := httptest.NewRecorder()

	app.newWebHandler("").ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events, err := app.MemberStore.RecentEvents("", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one", events)
	}
	event := events[0]
	if event.Action != "auth.login.failed" || event.Status != "failed" {
		t.Fatalf("event = %+v", event)
	}
	if event.MemberEmail != "" || event.TargetMemberEmail != "admin@example.com" || event.Source != "web" || event.RequestID == "" {
		t.Fatalf("event attribution = %+v", event)
	}
	raw := mustJSON(t, event)
	for _, secret := range []string{secretPassword, challenge["token"], `"challenge_`, "browser-secret"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("audit leaked secret %q: %s", secret, raw)
		}
	}
}

func TestWebMemberMutationCreatesOneTerminalAudit(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "/api/member/add", strings.NewReader(
		`{"name":"Operator","email":"operator@example.com","role":"operator","password":"member-secret-password"}`,
	))
	addWebAuth(t, &app, req, "admin")
	sessionCookie, err := req.Cookie(webSessionCookie)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()

	app.newWebHandler("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events, err := app.MemberStore.RecentEvents("", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one", events)
	}
	event := events[0]
	if event.Action != "member.created" || event.Status != "success" {
		t.Fatalf("event = %+v", event)
	}
	if event.MemberEmail != "admin@example.com" || event.TargetMemberEmail != "operator@example.com" {
		t.Fatalf("event actor/target = %+v", event)
	}
	if event.RequestID == "" || event.SessionIDHash == "" || event.Source != "web" {
		t.Fatalf("event correlation = %+v", event)
	}
	if event.SessionIDHash != hashSessionIdentifier("cookie:"+sessionCookie.Value) ||
		strings.Contains(mustJSON(t, event), sessionCookie.Value) {
		t.Fatalf("event session correlation is unsafe: %+v", event)
	}
	if strings.Contains(mustJSON(t, event), "member-secret-password") {
		t.Fatalf("audit leaked password: %+v", event)
	}
}

func TestWebSessionIdentifierHashSupportsCookieAndBearerWithoutExposingCredentials(t *testing.T) {
	cookieRequest := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	cookieRequest.AddCookie(&http.Cookie{Name: webSessionCookie, Value: "cookie-secret"})
	cookieHash := webSessionIdentifierHash(cookieRequest)
	if cookieHash == "" || cookieHash == "cookie-secret" {
		t.Fatalf("cookie session hash = %q", cookieHash)
	}

	bearerRequest := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	bearerRequest.Header.Set("Authorization", "Bearer bearer-secret")
	bearerHash := webSessionIdentifierHash(bearerRequest)
	if bearerHash == "" || bearerHash == cookieHash || strings.Contains(bearerHash, "bearer-secret") {
		t.Fatalf("bearer session hash = %q", bearerHash)
	}
}

func TestWebLoginSuccessAuditUsesIssuedSessionHash(t *testing.T) {
	app, handler := newWebAuditApp(t)
	if _, err := app.MemberStore.SetupAdmin("Admin", "admin@example.com", "password123"); err != nil {
		t.Fatal(err)
	}
	challenge := webChallengeThroughHandler(t, handler)
	body := fmt.Sprintf(
		`{"username":"admin@example.com","password":"password123","challenge_token":%q,"challenge_answer":%q}`,
		challenge.token,
		challenge.answer,
	)
	rec := performWebAuditRequest(t, handler, nil, "/api/auth/login", body)
	assertWebResponseEnvelope(t, rec, http.StatusOK, true)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not issue a session cookie")
	}
	events, err := app.MemberStore.RecentEvents("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "auth.login.succeeded" {
		t.Fatalf("login events = %+v", events)
	}
	want := hashSessionIdentifier("cookie:" + cookies[0].Value)
	if events[0].SessionIDHash != want || strings.Contains(mustJSON(t, events[0]), cookies[0].Value) {
		t.Fatalf("login session correlation is unsafe: %+v", events[0])
	}
}

func TestWebTerminalOriginRequiresSameOrigin(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{name: "missing", want: false},
		{name: "same https", origin: "https://cm.example.com", want: true},
		{name: "cross origin", origin: "https://evil.example.com", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://cm.example.com/api/terminal/ws", nil)
			req.Host = "cm.example.com"
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			if got := sameWebOrigin(req); got != test.want {
				t.Fatalf("sameWebOrigin() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWebAuthorizationDeniedCreatesOneFailedAudit(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "/api/member/add", strings.NewReader(
		`{"name":"Target","email":"target@example.com","role":"operator"}`,
	))
	addWebAuth(t, &app, req, "operator")
	rec := httptest.NewRecorder()

	app.newWebHandler("").ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events, err := app.MemberStore.RecentEvents("", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one", events)
	}
	event := events[0]
	if event.Action != "authorization.denied" || event.Status != "failed" {
		t.Fatalf("event = %+v", event)
	}
	if event.MemberEmail != "admin@example.com" || event.TargetMemberEmail != "target@example.com" {
		t.Fatalf("event actor/target = %+v", event)
	}
}

func TestWebReadAuthorizationDeniedCreatesOneFailedAudit(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/api/members", nil)
	addWebAuth(t, &app, req, "operator")
	rec := httptest.NewRecorder()

	app.newWebHandler("").ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events, err := app.MemberStore.RecentEvents("", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one", events)
	}
	event := events[0]
	if event.Action != "authorization.denied" || event.MemberEmail != "admin@example.com" ||
		event.RequestID == "" || event.SessionIDHash == "" || event.Source != "web" ||
		!strings.Contains(event.Message, "required_roles=admin") {
		t.Fatalf("read authorization event = %+v", event)
	}
}

func TestWebTokenStatusPollingDoesNotCreateAudit(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "/api/auth/token", strings.NewReader(`{"action":"status"}`))
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()

	app.newWebHandler("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events, err := app.MemberStore.RecentEvents("", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("token status created audit events: %+v", events)
	}
}

func TestWebAuditPersistenceFailurePreservesCommittedMutationResponse(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	app.MemberStore = failingAuditRepository{MemberRepository: app.MemberStore}
	req := httptest.NewRequest(http.MethodPost, "/api/member/add", strings.NewReader(
		`{"name":"Operator","email":"operator@example.com","role":"operator"}`,
	))
	addWebAuth(t, &app, req, "admin")
	rec := httptest.NewRecorder()

	app.newWebHandler("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if db, err := app.MemberStore.Load(); err != nil {
		t.Fatal(err)
	} else if _, ok := findMemberByEmailOrUsername(db, "operator@example.com"); !ok {
		t.Fatal("business mutation should remain visible even when non-atomic audit persistence fails")
	}
	logs, err := app.LogManager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		t.Fatal("audit persistence failure did not create a runtime log")
	}
	data, err := os.ReadFile(logs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"action":"audit.persistence.failed"`)) {
		t.Fatalf("audit persistence failure was not logged: %s", data)
	}
}

func TestWebAuthMutationsAuditEndToEnd(t *testing.T) {
	t.Run("setup", func(t *testing.T) {
		app, handler := newWebAuditApp(t)
		challenge := webChallengeThroughHandler(t, handler)
		body := fmt.Sprintf(
			`{"name":"Admin","email":"admin@example.com","password":"setup-secret","challenge_token":%q,"challenge_answer":%q}`,
			challenge.token,
			challenge.answer,
		)
		assertWebMutationAudit(t, &app, handler, nil, "/api/auth/setup", body, http.StatusOK, true, 0, "auth.setup.succeeded", "admin@example.com", "admin@example.com", "")
	})

	t.Run("setup failure", func(t *testing.T) {
		app, handler := newWebAuditApp(t)
		body := `{"name":"Admin","email":"admin@example.com","password":"setup-failure-secret","challenge_token":"invalid-challenge","challenge_answer":"invalid-answer"}`
		assertWebMutationAudit(t, &app, handler, nil, "/api/auth/setup", body, http.StatusBadRequest, false, 0, "auth.setup.failed", "", "admin@example.com", "")
	})

	for _, tt := range []struct {
		name       string
		password   string
		status     int
		ok         bool
		wantAction string
	}{
		{"login success", "password123", http.StatusOK, true, "auth.login.succeeded"},
		{"login failure", "wrong-login-secret", http.StatusUnauthorized, false, "auth.login.failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			app, handler := newWebAuditApp(t)
			if _, err := app.MemberStore.SetupAdmin("Admin", "admin@example.com", "password123"); err != nil {
				t.Fatal(err)
			}
			challenge := webChallengeThroughHandler(t, handler)
			body := fmt.Sprintf(
				`{"username":"admin@example.com","password":%q,"challenge_token":%q,"challenge_answer":%q}`,
				tt.password,
				challenge.token,
				challenge.answer,
			)
			wantActor := "admin@example.com"
			if !tt.ok {
				wantActor = ""
			}
			assertWebMutationAudit(t, &app, handler, nil, "/api/auth/login", body, tt.status, tt.ok, 0, tt.wantAction, wantActor, "admin@example.com", "")
		})
	}

	t.Run("logout", func(t *testing.T) {
		app, handler, cookie := newAuthenticatedWebAuditApp(t, "admin")
		baseline := webAuditEventCount(t, app.MemberStore)
		assertWebMutationAudit(t, &app, handler, cookie, "/api/auth/logout", `{}`, http.StatusOK, true, baseline, "auth.logout.succeeded", "admin@example.com", "admin@example.com", "changed_fields=session")
	})

	t.Run("email change", func(t *testing.T) {
		app, handler, cookie := newAuthenticatedWebAuditApp(t, "admin")
		challenge := webChallengeThroughHandler(t, handler)
		baseline := webAuditEventCount(t, app.MemberStore)
		body := fmt.Sprintf(
			`{"email":"new-admin@example.com","password":"password123","challenge_token":%q,"challenge_answer":%q}`,
			challenge.token,
			challenge.answer,
		)
		assertWebMutationAudit(t, &app, handler, cookie, "/api/auth/update-email", body, http.StatusOK, true, baseline, "auth.email.changed", "admin@example.com", "new-admin@example.com", "changed_fields=email")
	})

	t.Run("own password", func(t *testing.T) {
		app, handler, cookie := newAuthenticatedWebAuditApp(t, "admin")
		baseline := webAuditEventCount(t, app.MemberStore)
		body := `{"current_password":"password123","new_password":"new-password-secret","confirm_password":"new-password-secret"}`
		assertWebMutationAudit(t, &app, handler, cookie, "/api/auth/change-password", body, http.StatusOK, true, baseline, "auth.password.changed", "admin@example.com", "admin@example.com", "changed_fields=password")
	})
}

func TestWebTokenMutationsAuditEndToEnd(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		body       string
		target     string
		prepare    func(t *testing.T, app *App)
		wantAction string
	}{
		{"own generate", "/api/auth/token", `{"action":"generate"}`, "admin@example.com", nil, "auth.token.generated"},
		{"own regenerate", "/api/auth/token", `{"action":"generate"}`, "admin@example.com", func(t *testing.T, app *App) {
			if _, err := app.applyWebAPITokenAction("admin@example.com", "generate"); err != nil {
				t.Fatal(err)
			}
		}, "auth.token.regenerated"},
		{"own delete", "/api/auth/token", `{"action":"delete"}`, "admin@example.com", func(t *testing.T, app *App) {
			if _, err := app.applyWebAPITokenAction("admin@example.com", "generate"); err != nil {
				t.Fatal(err)
			}
		}, "auth.token.deleted"},
		{"admin generate", "/api/member/token", `{"email":"target@example.com","action":"generate"}`, "target@example.com", addAuditTargetMember, "auth.token.generated"},
		{"admin regenerate", "/api/member/token", `{"email":"target@example.com","action":"generate"}`, "target@example.com", func(t *testing.T, app *App) {
			addAuditTargetMember(t, app)
			if _, err := app.applyWebAPITokenAction("target@example.com", "generate"); err != nil {
				t.Fatal(err)
			}
		}, "auth.token.regenerated"},
		{"admin delete", "/api/member/token", `{"email":"target@example.com","action":"delete"}`, "target@example.com", func(t *testing.T, app *App) {
			addAuditTargetMember(t, app)
			if _, err := app.applyWebAPITokenAction("target@example.com", "generate"); err != nil {
				t.Fatal(err)
			}
		}, "auth.token.deleted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, handler, cookie := newAuthenticatedWebAuditApp(t, "admin")
			if tt.prepare != nil {
				tt.prepare(t, &app)
			}
			baseline := webAuditEventCount(t, app.MemberStore)
			assertWebMutationAudit(t, &app, handler, cookie, tt.path, tt.body, http.StatusOK, true, baseline, tt.wantAction, "admin@example.com", tt.target, "changed_fields=token")
		})
	}
}

func TestWebAdminAndProfileMutationsAuditEndToEnd(t *testing.T) {
	newProfileYAML := auditProfileYAML("new-usw2")
	existingProfileYAML := auditProfileYAML("existing-usw2")
	tests := []struct {
		name        string
		path        string
		body        string
		wantAction  string
		wantTarget  string
		wantMessage string
		prepare     func(t *testing.T, app *App)
	}{
		{"partial settings", "/api/settings", `{"default_status_filter":"ready","auth_secret":"submitted-auth-secret"}`, "settings.updated", "", "changed_fields=default_status_filter", nil},
		{"member create without password", "/api/member/add", `{"name":"Target","email":"target@example.com","role":"operator"}`, "member.created", "target@example.com", "changed_fields=name,email,role", nil},
		{"member update", "/api/member/update", `{"original_email":"target@example.com","name":"Updated","email":"updated@example.com","role":"viewer"}`, "member.updated", "target@example.com", "changed_fields=name,email,role", addAuditTargetMember},
		{"member password", "/api/member/password", `{"email":"target@example.com","password":"member-password-secret"}`, "member.password.changed", "target@example.com", "changed_fields=password", addAuditTargetMember},
		{"member enable", "/api/member/enable", `{"email":"target@example.com"}`, "member.enabled", "target@example.com", "changed_fields=enabled", func(t *testing.T, app *App) {
			addAuditTargetMember(t, app)
			if _, err := app.MemberStore.SetMemberEnabled("target@example.com", false); err != nil {
				t.Fatal(err)
			}
		}},
		{"member disable", "/api/member/disable", `{"email":"target@example.com"}`, "member.disabled", "target@example.com", "changed_fields=enabled", addAuditTargetMember},
		{"assignment grant", "/api/member/assign", `{"apple_email":"apple@example.com","member_email":"target@example.com","relation":"owner"}`, "member.assignment.granted", "target@example.com", "changed_fields=apple_email,member_email,relation", addAuditTargetMember},
		{"assignment remove", "/api/member/unassign", `{"apple_email":"apple@example.com","member_email":"target@example.com"}`, "member.assignment.removed", "target@example.com", "changed_fields=apple_email,member_email", func(t *testing.T, app *App) {
			addAuditTargetMember(t, app)
			if _, err := app.MemberStore.AssignMember("apple@example.com", "target@example.com", "owner"); err != nil {
				t.Fatal(err)
			}
		}},
		{"profile access replace", "/api/member/profiles", `{"member_email":"target@example.com","profiles":["existing-usw2"]}`, "profile.access.replaced", "target@example.com", "changed_fields=profile_access", func(t *testing.T, app *App) {
			addAuditTargetMember(t, app)
			addAuditManagedProfile(t, app, existingProfileYAML)
		}},
		{"profile create", "/api/managed-profile/save", mustJSON(t, map[string]string{"profile_yaml": newProfileYAML}), "profile.created", "", "changed_fields=profile", nil},
		{"profile update", "/api/managed-profile/save", mustJSON(t, map[string]string{"profile_yaml": existingProfileYAML}), "profile.updated", "", "changed_fields=profile", func(t *testing.T, app *App) {
			addAuditManagedProfile(t, app, existingProfileYAML)
		}},
		{"profile enable", "/api/managed-profile/status", `{"profile":"existing-usw2","enabled":true}`, "profile.enabled", "", "changed_fields=enabled", func(t *testing.T, app *App) {
			addAuditManagedProfile(t, app, existingProfileYAML)
			if _, err := app.MemberStore.SetManagedProfileEnabled("existing-usw2", false); err != nil {
				t.Fatal(err)
			}
		}},
		{"profile disable", "/api/managed-profile/status", `{"profile":"existing-usw2","enabled":false}`, "profile.disabled", "", "changed_fields=enabled", func(t *testing.T, app *App) {
			addAuditManagedProfile(t, app, existingProfileYAML)
		}},
		{"profile delete", "/api/managed-profile/delete", `{"profile":"existing-usw2"}`, "profile.deleted", "", "operation=profile.deleted", func(t *testing.T, app *App) {
			addAuditManagedProfile(t, app, existingProfileYAML)
		}},
		{"profile access grant", "/api/managed-profile/access", `{"profile":"existing-usw2","member_email":"target@example.com","grant":true}`, "profile.access.granted", "target@example.com", "changed_fields=profile_access", func(t *testing.T, app *App) {
			addAuditTargetMember(t, app)
			addAuditManagedProfile(t, app, existingProfileYAML)
		}},
		{"profile access remove", "/api/managed-profile/access", `{"profile":"existing-usw2","member_email":"target@example.com","grant":false}`, "profile.access.removed", "target@example.com", "changed_fields=profile_access", func(t *testing.T, app *App) {
			addAuditTargetMember(t, app)
			addAuditManagedProfile(t, app, existingProfileYAML)
			if _, err := app.MemberStore.AssignProfileAccess("existing-usw2", "target@example.com"); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, handler, cookie := newAuthenticatedWebAuditApp(t, "admin")
			if tt.prepare != nil {
				tt.prepare(t, &app)
			}
			baseline := webAuditEventCount(t, app.MemberStore)
			assertWebMutationAudit(t, &app, handler, cookie, tt.path, tt.body, http.StatusOK, true, baseline, tt.wantAction, "admin@example.com", tt.wantTarget, tt.wantMessage)
		})
	}
}

func TestWebSettingsInnerRoleDenialAuditEndToEnd(t *testing.T) {
	app, handler, cookie := newAuthenticatedWebAuditApp(t, "viewer")
	baseline := webAuditEventCount(t, app.MemberStore)
	assertWebMutationAudit(
		t,
		&app,
		handler,
		cookie,
		"/api/settings",
		`{"default_status_filter":"ready"}`,
		http.StatusForbidden,
		false,
		baseline,
		"authorization.denied",
		"admin@example.com",
		"",
		"administrator role is required",
	)
}

func TestWebMutationRequestBodyLimitCreatesOneFailedAudit(t *testing.T) {
	app, handler, cookie := newAuthenticatedWebAuditApp(t, "admin")
	baseline := webAuditEventCount(t, app.MemberStore)
	body := `{"name":"Target","email":"target@example.com","role":"operator","padding":"` +
		strings.Repeat("x", maxWebMutationRequestBody+1) + `"}`
	assertWebMutationAudit(
		t,
		&app,
		handler,
		cookie,
		"/api/member/add",
		body,
		http.StatusRequestEntityTooLarge,
		false,
		baseline,
		"member.created",
		"admin@example.com",
		"",
		"request body is too large",
	)
}

func TestWebDynamicAPIResponseLimit(t *testing.T) {
	app, _ := newWebAuditApp(t)
	handler := app.withWebObservability(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeWebJSON(w, webAPIResponse{
			OK:   true,
			Data: map[string]string{"payload": strings.Repeat("x", maxBufferedWebResponse+1)},
		})
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/oversized", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body bytes = %d", rec.Code, rec.Body.Len())
	}
	if rec.Body.Len() > 1024 {
		t.Fatalf("oversized response was retained: %d bytes", rec.Body.Len())
	}
	if !strings.Contains(rec.Body.String(), "response is too large") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWebAuditedMutationResponseLimitCreatesOneFailedAudit(t *testing.T) {
	app, _, cookie := newAuthenticatedWebAuditApp(t, "admin")
	baseline := webAuditEventCount(t, app.MemberStore)
	handler := app.withWebObservability(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeWebJSON(w, webAPIResponse{
			OK:   true,
			Data: map[string]string{"payload": strings.Repeat("x", maxBufferedWebResponse+1)},
		})
	}))
	assertWebMutationAudit(
		t,
		&app,
		handler,
		cookie,
		"/api/member/add",
		`{"name":"Target","email":"target@example.com","role":"operator"}`,
		http.StatusInternalServerError,
		false,
		baseline,
		"member.created",
		"admin@example.com",
		"target@example.com",
		"response is too large",
	)
}

func TestWebDynamicAPIPartialWritePanicReturnsClean500(t *testing.T) {
	app, _ := newWebAuditApp(t)
	handler := app.withWebObservability(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"partial":"sensitive"`))
		panic("private panic detail")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/panic", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, forbidden := range []string{"partial", "sensitive", "private panic detail"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("panic response leaked %q: %s", forbidden, rec.Body.String())
		}
	}
}

func TestWebNonAPIPartialWritePanicDoesNotAppendErrorBody(t *testing.T) {
	app, _ := newWebAuditApp(t)
	handler := app.withWebObservability(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("committed static response"))
		panic("private panic detail")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "committed static response" {
		t.Fatalf("body = %q, want only committed response", got)
	}
	logs, err := app.LogManager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		t.Fatal("committed panic was not logged")
	}
	data, err := os.ReadFile(logs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"action":"web.panic"`)) {
		t.Fatalf("panic runtime log missing: %s", data)
	}
}

func TestTrackingWebResponseDoesNotInventOptionalInterfaces(t *testing.T) {
	base := httptest.NewRecorder()
	tracked := &trackingWebResponse{ResponseWriter: base}
	tests := []struct {
		name       string
		baseHas    bool
		trackedHas bool
	}{
		{"hijacker", implementsHijacker(base), implementsHijacker(tracked)},
		{"pusher", implementsPusher(base), implementsPusher(tracked)},
		{"readerFrom", implementsReaderFrom(base), implementsReaderFrom(tracked)},
	}
	for _, tt := range tests {
		if !tt.baseHas && tt.trackedHas {
			t.Fatalf("tracking writer invented unsupported %s capability", tt.name)
		}
	}
}

func TestWebTerminalWSReceivesUnderlyingHijacker(t *testing.T) {
	app, _ := newWebAuditApp(t)
	receivedHijacker := false
	receivedRawWriter := false
	handler := app.withWebObservability(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, receivedHijacker = w.(http.Hijacker)
		_, receivedRawWriter = w.(testRawWriterMarker)
		w.WriteHeader(http.StatusNoContent)
	}))
	writer := &hijackingResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/terminal/ws", nil))
	if !receivedHijacker {
		t.Fatal("terminal websocket route did not receive the underlying Hijacker")
	}
	if !receivedRawWriter {
		t.Fatal("terminal websocket route did not receive the raw response writer")
	}
	if writer.Code != http.StatusNoContent {
		t.Fatalf("status = %d", writer.Code)
	}
}

func TestWebObservabilityDefersMemberLookupForStaticAndOrdinaryGET(t *testing.T) {
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	counting := &countingMemberRepository{MemberRepository: app.MemberStore, failOnLoad: true}
	app.MemberStore = counting
	handler := app.newWebHandler("")
	for _, path := range []string{"/", "/assets/connectmac-mark.svg"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
	}
	if counting.loads != 0 {
		t.Fatalf("member store loads = %d, want 0", counting.loads)
	}
}

func TestWebClientIPTrustsOnlyLoopbackProxy(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{"spoofed direct peer", "198.51.100.20:4321", "203.0.113.99", "198.51.100.20"},
		{"loopback proxy", "127.0.0.1:4321", "203.0.113.99", "203.0.113.99"},
		{"loopback proxy chain uses real last", "127.0.0.1:4321", "192.0.2.66, invalid-value, 203.0.113.42", "203.0.113.42"},
		{"loopback proxy ignores invalid tail", "127.0.0.1:4321", "192.0.2.66, invalid-value", "192.0.2.66"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
			req.RemoteAddr = tt.remoteAddr
			req.Header.Set("X-Forwarded-For", tt.forwarded)
			if got := webClientIP(req); got != tt.want {
				t.Fatalf("webClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

type webAuditChallenge struct {
	token  string
	answer string
}

func newWebAuditApp(t *testing.T) (App, http.Handler) {
	t.Helper()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, t.TempDir())
	app.LoginConfigCleanup = false
	return app, app.newWebHandler("")
}

func newAuthenticatedWebAuditApp(t *testing.T, role string) (App, http.Handler, *http.Cookie) {
	t.Helper()
	app, handler := newWebAuditApp(t)
	challenge := webChallengeThroughHandler(t, handler)
	body := fmt.Sprintf(
		`{"name":"Admin","email":"admin@example.com","password":"password123","challenge_token":%q,"challenge_answer":%q}`,
		challenge.token,
		challenge.answer,
	)
	rec := performWebAuditRequest(t, handler, nil, "/api/auth/setup", body)
	assertWebResponseEnvelope(t, rec, http.StatusOK, true)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("setup did not return a session cookie")
	}
	db, err := app.MemberStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	admin, ok := findMemberByEmailOrUsername(db, "admin@example.com")
	if !ok {
		t.Fatal("setup admin was not persisted")
	}
	if role != "" && role != "admin" {
		for i := range db.Members {
			if db.Members[i].ID == admin.ID {
				db.Members[i].Role = role
			}
		}
		if err := app.MemberStore.Save(db); err != nil {
			t.Fatal(err)
		}
	}
	if count := webAuditEventCount(t, app.MemberStore); count != 1 {
		t.Fatalf("setup seed audit count = %d, want 1", count)
	}
	return app, handler, cookies[0]
}

func webChallengeThroughHandler(t *testing.T, handler http.Handler) webAuditChallenge {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/challenge", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("challenge status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		OK   bool `json:"ok"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Data.Token == "" {
		t.Fatalf("challenge response = %s", rec.Body.String())
	}
	raw, err := base64.RawURLEncoding.DecodeString(response.Data.Token)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 {
		t.Fatalf("challenge token parts = %#v", parts)
	}
	return webAuditChallenge{token: response.Data.Token, answer: parts[1]}
}

func assertWebMutationAudit(
	t *testing.T,
	app *App,
	handler http.Handler,
	cookie *http.Cookie,
	path string,
	body string,
	wantHTTPStatus int,
	wantOK bool,
	baseline int,
	wantAction string,
	wantActor string,
	wantTarget string,
	wantMessage string,
) {
	t.Helper()
	rec := performWebAuditRequest(t, handler, cookie, path, body)
	assertWebResponseEnvelope(t, rec, wantHTTPStatus, wantOK)
	requestID := rec.Header().Get("X-Request-ID")
	if requestID == "" {
		t.Fatal("response is missing X-Request-ID")
	}
	page, err := app.MemberStore.QueryEvents(EventQuery{Limit: 200, IncludeSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != baseline+1 {
		t.Fatalf("event count = %d, want %d; events=%+v", len(page.Events), baseline+1, page.Events)
	}
	var event OperationEvent
	for _, candidate := range page.Events {
		if candidate.RequestID == requestID {
			event = candidate
			break
		}
	}
	if event.RequestID == "" {
		t.Fatalf("event for request %q not found: %+v", requestID, page.Events)
	}
	wantStatus := "success"
	if !wantOK {
		wantStatus = "failed"
	}
	if event.Action != wantAction || event.Status != wantStatus {
		t.Fatalf("event action/status = %+v, want %s/%s", event, wantAction, wantStatus)
	}
	if event.RequestID != requestID || event.Source != "web" {
		t.Fatalf("event correlation = %+v, response request ID = %q", event, requestID)
	}
	if event.MemberEmail != wantActor || event.TargetMemberEmail != wantTarget {
		t.Fatalf("event actor/target = %+v, want %q/%q", event, wantActor, wantTarget)
	}
	if (event.Action == "auth.login.failed" || event.Action == "auth.setup.failed") &&
		!strings.Contains(event.Message, "identity="+wantTarget) {
		t.Fatalf("failed authentication message = %q, want normalized attempted identity %q", event.Message, wantTarget)
	}
	if event.Confirmed {
		t.Fatalf("ordinary Task 3 audit event must not be confirmed: %+v", event)
	}
	if wantMessage != "" && event.Message != wantMessage {
		t.Fatalf("event message = %q, want %q", event.Message, wantMessage)
	}
	assertAuditExcludesSensitiveRequest(t, event, body)
	assertAuditExcludesResponseToken(t, event, rec.Body.Bytes())
}

func performWebAuditRequest(t *testing.T, handler http.Handler, cookie *http.Cookie, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func assertWebResponseEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantOK bool) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("content type = %q", contentType)
	}
	var response webAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if response.OK != wantOK {
		t.Fatalf("response OK = %t, want %t; body = %s", response.OK, wantOK, rec.Body.String())
	}
}

func webAuditEventCount(t *testing.T, store MemberRepository) int {
	t.Helper()
	page, err := store.QueryEvents(EventQuery{Limit: 200, IncludeSystem: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Events)
}

func addAuditTargetMember(t *testing.T, app *App) {
	t.Helper()
	if _, err := app.MemberStore.AddMember("Target", "target@example.com", "operator"); err != nil {
		t.Fatal(err)
	}
}

func auditProfileYAML(name string) string {
	return FormatProfileFile(Profile{
		Name:         name,
		User:         "ec2-user",
		IdentityFile: "~/.ssh/private-audit.pem",
		AWS: AWSConfig{
			Profile:      "audit",
			Region:       "us-west-2",
			AccountEmail: "apple@example.com",
		},
	})
}

func addAuditManagedProfile(t *testing.T, app *App, profileYAML string) {
	t.Helper()
	profile, err := ParseSingleProfileYAML(profileYAML)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.MemberStore.UpsertManagedProfile(profile); err != nil {
		t.Fatal(err)
	}
}

func assertAuditExcludesSensitiveRequest(t *testing.T, event OperationEvent, body string) {
	t.Helper()
	raw := mustJSON(t, event)
	var values map[string]interface{}
	_ = json.Unmarshal([]byte(body), &values)
	for key, value := range values {
		lower := strings.ToLower(key)
		if !strings.Contains(lower, "password") &&
			!strings.Contains(lower, "challenge") &&
			!strings.Contains(lower, "token") &&
			!strings.Contains(lower, "secret") &&
			lower != "profile_yaml" {
			continue
		}
		text, _ := value.(string)
		if len(text) > 6 && strings.Contains(raw, text) {
			t.Fatalf("audit leaked sensitive %s value: %s", key, raw)
		}
	}
	for _, forbidden := range []string{"private-audit.pem", "profile_yaml", "BEGIN PRIVATE KEY", "password123", "new-password-secret", "member-password-secret", "submitted-auth-secret"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("audit leaked %q: %s", forbidden, raw)
		}
	}
}

func assertAuditExcludesResponseToken(t *testing.T, event OperationEvent, responseBody []byte) {
	t.Helper()
	var response struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return
	}
	token, _ := response.Data["token"].(string)
	if token != "" && strings.Contains(mustJSON(t, event), token) {
		t.Fatalf("audit leaked generated token")
	}
}

type failingAuditRepository struct {
	MemberRepository
}

func (f failingAuditRepository) RecordEvent(OperationEvent) error {
	return errors.New("forced audit persistence failure")
}

type countingMemberRepository struct {
	MemberRepository
	loads      int
	failOnLoad bool
}

type hijackingResponseRecorder struct {
	*httptest.ResponseRecorder
}

type testRawWriterMarker interface {
	isRawTestWriter()
}

func (w *hijackingResponseRecorder) isRawTestWriter() {}

func (w *hijackingResponseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("test hijack")
}

func implementsHijacker(value interface{}) bool {
	_, ok := value.(http.Hijacker)
	return ok
}

func implementsPusher(value interface{}) bool {
	_, ok := value.(http.Pusher)
	return ok
}

func implementsReaderFrom(value interface{}) bool {
	_, ok := value.(io.ReaderFrom)
	return ok
}

func (r *countingMemberRepository) Load() (MemberData, error) {
	r.loads++
	if r.failOnLoad {
		return MemberData{}, errors.New("member store must not be loaded")
	}
	return r.MemberRepository.Load()
}

func mustJSON(t *testing.T, value interface{}) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
