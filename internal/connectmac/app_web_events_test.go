package connectmac

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWebEventsPaginationAndMemberProfileFilter(t *testing.T) {
	app, handler, admin, operator := newWebTransferTestApp(t)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	for i, profile := range []string{"shared", "private", "shared", "private", "shared"} {
		if err := app.MemberStore.RecordEvent(OperationEvent{
			ID:        fmt.Sprintf("event-%d", i),
			Action:    "test.event",
			Profile:   profile,
			Status:    "success",
			Source:    "web",
			CreatedAt: base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.MemberStore.RecordEvent(OperationEvent{
		ID: "system-event", Action: "system.cleanup.completed", Profile: "shared",
		Status: "success", Source: "system", CreatedAt: base.Add(10 * time.Second).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}

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
	if len(second.Events) != 1 || second.Events[0].ID == first.Events[0].ID || second.Events[0].ID == first.Events[1].ID {
		t.Fatalf("second page = %+v", second)
	}

	adminPage := decodeWebEventPage(t, serveWebTransfer(t, &app, handler, admin, http.MethodGet, "/api/events?limit=20&include_system=1", ""))
	if len(adminPage.Events) != 6 {
		t.Fatalf("admin events = %d, want 6", len(adminPage.Events))
	}
}

func TestWebEventsRejectsInvalidCursorLimitAndProfileAccess(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	for _, target := range []string{
		"/api/events?limit=201",
		"/api/events?cursor=not-base64",
		"/api/events?profile=private",
	} {
		rec := serveWebTransfer(t, &app, handler, operator, http.MethodGet, target, "")
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d body=%s", target, rec.Code, rec.Body.String())
		}
	}
}

func decodeWebEventPage(t *testing.T, rec *httptest.ResponseRecorder) EventPage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data EventPage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Data
}
