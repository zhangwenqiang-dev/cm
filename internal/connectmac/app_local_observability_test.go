package connectmac

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLocalAgentRequestIDValidationAndCorrelation(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(`{}`))
	req.Header.Set("X-Request-ID", "local-browser-123")
	got, err := validatedLocalAgentRequestID(req, "local-browser-123")
	if err != nil || got != "local-browser-123" {
		t.Fatalf("validated request ID = %q, %v", got, err)
	}
	if _, err := validatedLocalAgentRequestID(req, "different"); err == nil {
		t.Fatal("expected mismatched request ID rejection")
	}
	if _, err := validateLocalAgentRequestID("bad request id"); err == nil {
		t.Fatal("expected invalid character rejection")
	}

	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := testApp(&out, &errOut, dir)
	ctx := withOperationContext(context.Background(), OperationContext{
		RequestID: "local-browser-123",
		Source:    "web-local",
	})
	app.logLocalCommand(ctx, "ssh.succeeded", Profile{Name: "shared"}, 0, time.Now(), LogEntry{})
	entries := readTestLogEntries(t, app.LogManager)
	if len(entries) != 1 || entries[0].RequestID != "local-browser-123" || entries[0].Source != "web-local" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestWebLocalIntentRecordsActorWithoutLocalExecutionClaim(t *testing.T) {
	app, handler, _, operator := newWebTransferTestApp(t)
	rec := serveWebTransfer(t, &app, handler, operator, http.MethodPost, "/api/local-intent",
		`{"profile":"shared","operation":"connect","request_id":"local-connect-123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	events, err := app.MemberStore.RecentEvents("shared@example.com", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events=%+v", events)
	}
	event := events[0]
	if event.Action != "local.connect.requested" || event.MemberEmail != operator.Email ||
		event.RequestID != "local-connect-123" || event.Status != "requested" {
		t.Fatalf("event=%+v", event)
	}
}
