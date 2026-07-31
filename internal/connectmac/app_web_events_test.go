package connectmac

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebEventsDefaultNewestFiftyExcludesSystemAndKeepsCompatibilityEnvelope(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 55; i++ {
		recordWebEventFixture(t, &app, OperationEvent{
			ID:         fmt.Sprintf("event-%02d", i),
			Action:     "test.event",
			Profile:    "shared",
			AppleEmail: "shared@example.com",
			Status:     "success",
			Source:     "web",
			CreatedAt:  base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
		})
	}
	recordWebEventFixture(t, &app, OperationEvent{
		ID: "system-event", Action: "system.cleanup.completed", Profile: "shared",
		AppleEmail: "shared@example.com", Status: "success", Source: "system",
		CreatedAt: base.Add(2 * time.Minute).Format(time.RFC3339Nano),
	})

	rec := serveWebTransfer(t, &app, handler, operator, http.MethodGet, "/api/events", "")
	response := decodeWebEventAPIResponse(t, rec)
	if len(response.Data.Events) != 50 {
		t.Fatalf("events = %d, want 50", len(response.Data.Events))
	}
	if response.Data.Events[0].ID != "event-54" || response.Data.Events[49].ID != "event-05" {
		t.Fatalf("unexpected newest page bounds: first=%q last=%q", response.Data.Events[0].ID, response.Data.Events[49].ID)
	}
	for _, event := range response.Data.Events {
		if event.Source == "system" {
			t.Fatalf("default response included system event: %+v", event)
		}
	}
	if response.Data.NextCursor == "" {
		t.Fatal("first page is missing next_cursor")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatal(err)
	}
	if _, ok := data["events"]; !ok {
		t.Fatalf("legacy data.events field missing: %s", rec.Body.String())
	}

	last := decodeWebEventPage(t, serveWebTransfer(
		t, &app, handler, operator, http.MethodGet,
		"/api/events?cursor="+response.Data.NextCursor, "",
	))
	if len(last.Events) != 5 || last.NextCursor != "" {
		t.Fatalf("last page = %+v, want five events and empty next_cursor", last)
	}
}

func TestWebEventsFiltersProfileAndAppleEmail(t *testing.T) {
	app, handler, admin, _ := newWebTransferTestApp(t)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	fixtures := []OperationEvent{
		{ID: "shared-a", Profile: "shared", AppleEmail: "shared@example.com"},
		{ID: "shared-b", Profile: "shared", AppleEmail: "other@example.com"},
		{ID: "private-a", Profile: "private", AppleEmail: "private@example.com"},
	}
	for i := range fixtures {
		fixtures[i].Action = "test.event"
		fixtures[i].Status = "success"
		fixtures[i].Source = "web"
		fixtures[i].CreatedAt = base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		recordWebEventFixture(t, &app, fixtures[i])
	}

	byProfile := decodeWebEventPage(t, serveWebTransfer(
		t, &app, handler, admin, http.MethodGet, "/api/events?profile=shared", "",
	))
	if len(byProfile.Events) != 2 {
		t.Fatalf("profile events = %+v, want two shared events", byProfile.Events)
	}
	for _, event := range byProfile.Events {
		if event.Profile != "shared" {
			t.Fatalf("profile filter leaked event: %+v", event)
		}
	}

	byApple := decodeWebEventPage(t, serveWebTransfer(
		t, &app, handler, admin, http.MethodGet,
		"/api/events?apple_email="+fixtures[2].AppleEmail, "",
	))
	if len(byApple.Events) != 1 || byApple.Events[0].ID != "private-a" {
		t.Fatalf("apple_email events = %+v", byApple.Events)
	}
}

func TestWebEventsPaginationAndMemberProfileFilter(t *testing.T) {
	app, handler, admin, operator := newWebTransferTestApp(t)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	for i, profile := range []string{"shared", "private", "shared", "private", "shared"} {
		recordWebEventFixture(t, &app, OperationEvent{
			ID:        fmt.Sprintf("event-%d", i),
			Action:    "test.event",
			Profile:   profile,
			Status:    "success",
			Source:    "web",
			CreatedAt: base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
		})
	}
	recordWebEventFixture(t, &app, OperationEvent{
		ID: "system-event", Action: "system.cleanup.completed", Profile: "shared",
		Status: "success", Source: "system", CreatedAt: base.Add(10 * time.Second).Format(time.RFC3339Nano),
	})

	first := decodeWebEventPage(t, serveWebTransfer(t, &app, handler, operator, http.MethodGet, "/api/events?limit=2", ""))
	if len(first.Events) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	for _, event := range first.Events {
		if event.Profile != "shared" || event.Source == "system" {
			t.Fatalf("operator received inaccessible event: %+v", event)
		}
	}
	second := decodeWebEventPage(t, serveWebTransfer(t, &app, handler, operator, http.MethodGet, "/api/events?limit=2&cursor="+first.NextCursor, ""))
	if len(second.Events) != 1 || second.NextCursor != "" ||
		second.Events[0].ID == first.Events[0].ID || second.Events[0].ID == first.Events[1].ID {
		t.Fatalf("second page = %+v", second)
	}

	adminPage := decodeWebEventPage(t, serveWebTransfer(t, &app, handler, admin, http.MethodGet, "/api/events?limit=20&include_system=1", ""))
	if len(adminPage.Events) != 6 {
		t.Fatalf("admin events = %d, want 6", len(adminPage.Events))
	}
}

func TestWebEventsRejectsInvalidCursorLimitAndProfileAccess(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	tests := []struct {
		target string
		status int
	}{
		{target: "/api/events?limit=201", status: http.StatusBadRequest},
		{target: "/api/events?limit=invalid", status: http.StatusBadRequest},
		{target: "/api/events?cursor=not-base64", status: http.StatusBadRequest},
		{target: "/api/events?profile=private", status: http.StatusForbidden},
	}
	for _, tt := range tests {
		rec := serveWebTransfer(t, &app, handler, operator, http.MethodGet, tt.target, "")
		if rec.Code != tt.status {
			t.Fatalf("%s status=%d, want=%d body=%s", tt.target, rec.Code, tt.status, rec.Body.String())
		}
	}
}

func TestWebEventsUIIncludesAdminSystemToggleAndLabels(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for _, want := range []string{
		`id="includeSystemEventsWrap" class="admin-only form-check form-switch"`,
		`id="includeSystemEvents" class="form-check-input" type="checkbox"`,
		`class="form-check-label" for="includeSystemEvents">包含系统事件</label>`,
		`#includeSystemEventsWrap {`,
		`#includeSystemEvents {`,
		`includeSystem ? "&include_system=1" : ""`,
		`event.source === "system" ? "[系统] " : ""`,
		`$("includeSystemEvents").disabled = !isAdmin()`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("event UI is missing %q", want)
		}
	}
}

func recordWebEventFixture(t *testing.T, app *App, event OperationEvent) {
	t.Helper()
	if err := app.MemberStore.RecordEvent(event); err != nil {
		t.Fatal(err)
	}
}

type webEventAPIResponse struct {
	Data EventPage `json:"data"`
}

func decodeWebEventAPIResponse(t *testing.T, rec *httptest.ResponseRecorder) webEventAPIResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response webEventAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeWebEventPage(t *testing.T, rec *httptest.ResponseRecorder) EventPage {
	t.Helper()
	return decodeWebEventAPIResponse(t, rec).Data
}
