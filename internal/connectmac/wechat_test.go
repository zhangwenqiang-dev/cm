package connectmac

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWechatNotifierSendsMarkdown(t *testing.T) {
	var got map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	notifier := WechatNotifier{WebhookURL: server.URL, WebBaseURL: "https://cm.example.com"}
	result, err := notifier.Send(WechatNotification{
		Event:            "open",
		AppleEmail:       "apple@example.com",
		Owner:            "Profile Owner",
		Operator:         "Operation Actor",
		HostID:           "h-123",
		HostArchitecture: "arm64",
		HostCreatedAt:    "2026-07-16T08:03:24Z",
		DueAt:            "2026-07-17T16:00:00Z",
		Management:       true,
		Description:      "打开成功",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Skipped {
		t.Fatalf("result skipped: %+v", result)
	}
	if result.HTTPStatus != http.StatusOK || result.ErrorCode != 0 || result.ErrorMessage != "ok" {
		t.Fatalf("result metadata = %+v", result)
	}
	if got["msgtype"] != "markdown" {
		t.Fatalf("msgtype = %#v", got["msgtype"])
	}
	markdown := got["markdown"].(map[string]interface{})
	content := markdown["content"].(string)
	for _, want := range []string{
		"apple@example.com",
		"操作人：Operation Actor",
		"Host 架构类型：arm64",
		"https://cm.example.com",
		"Host 创建时间：2026-07-16 16:03:24（北京时间）",
		"释放提醒时间：2026-07-18 00:00:00（北京时间）",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("content missing %q:\n%s", want, content)
		}
	}
	for _, forbidden := range []string{"ConnectMac", "Profile：", "apple-usw2", "负责人", "Profile Owner"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("content contains forbidden owner value %q:\n%s", forbidden, content)
		}
	}
	for _, unexpected := range []string{"T08:03:24Z", "T16:00:00Z"} {
		if strings.Contains(content, unexpected) {
			t.Fatalf("content leaked UTC timestamp %q:\n%s", unexpected, content)
		}
	}
}

func TestWechatNotifierOmitsHostArchitectureFromUnrelatedEvents(t *testing.T) {
	notifier := WechatNotifier{}
	for _, event := range []string{"extend", "due", "release", "auto-release-failure", "auto-release-failed"} {
		content := notifier.markdown(WechatNotification{Event: event, HostArchitecture: "arm64"})
		if strings.Contains(content, "Host 架构类型") {
			t.Fatalf("event %q unexpectedly includes architecture: %s", event, content)
		}
	}
}

func TestWechatNotifierReturnsFailureMetadataWithoutLeakingBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"errcode":93000,"errmsg":"temporary failure","secret":"must-not-appear"}`))
	}))
	defer server.Close()
	result, err := (WechatNotifier{WebhookURL: server.URL}).Send(WechatNotification{Event: "open"})
	if err == nil {
		t.Fatal("send should fail")
	}
	if result.HTTPStatus != http.StatusBadGateway || result.ErrorCode != 93000 || result.ErrorMessage != "temporary failure" {
		t.Fatalf("failure metadata = %+v", result)
	}
	if strings.Contains(err.Error(), "must-not-appear") {
		t.Fatalf("raw response body leaked: %v", err)
	}
}

func TestGeneralWechatDeliveryFailureIsSingleAttemptExhausted(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	err := app.deliverWechatNotification(wechatDeliveryContext{
		RequestID:  "req-due",
		Profile:    "apple-usw2",
		AppleEmail: "apple@example.com",
		Source:     "system",
		Event:      "due",
		Attempt:    1,
	}, func() (WechatNotifyResult, error) {
		return WechatNotifyResult{HTTPStatus: http.StatusBadGateway, ErrorCode: 93000}, errors.New("temporary webhook failure")
	})
	if err == nil {
		t.Fatal("delivery should fail")
	}
	page, err := app.MemberStore.QueryEvents(EventQuery{Profile: "apple-usw2", IncludeSystem: true, Limit: 20})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	counts := map[string]int{}
	for _, event := range page.Events {
		counts[event.Action]++
		if !strings.Contains(event.Message, "notification_event=due") {
			t.Fatalf("notification type missing from audit event: %+v", event)
		}
	}
	if counts["wechat.pending"] != 1 || counts["wechat.failed"] != 1 || counts["wechat.retrying"] != 0 {
		t.Fatalf("general delivery events = %+v", counts)
	}
	for _, entry := range readTestLogEntries(t, app.LogManager) {
		if entry.Operation != "wechat.notify.due" {
			t.Fatalf("notification type missing from runtime log: %+v", entry)
		}
	}
}

func TestWechatNotifierMissingWebhookSkips(t *testing.T) {
	notifier := WechatNotifier{}
	result, err := notifier.Send(WechatNotification{Event: "due"})
	if !errors.Is(err, errWechatWebhookNotConfigured) {
		t.Fatalf("missing webhook error = %v", err)
	}
	if !result.Skipped {
		t.Fatalf("result should be skipped: %+v", result)
	}
}

func TestGeneralWechatDeliveryMissingWebhookFailsWithoutSent(t *testing.T) {
	app := newWebAutoReleaseTestApp(t)
	err := app.deliverWechatNotification(wechatDeliveryContext{
		RequestID:  "req-missing-webhook",
		Profile:    "apple-usw2",
		AppleEmail: "apple@example.com",
		Source:     "system",
		Event:      "due",
		Attempt:    1,
	}, func() (WechatNotifyResult, error) {
		return WechatNotifyResult{Skipped: true, Message: "wechat webhook not configured"}, nil
	})
	if err == nil {
		t.Fatal("missing webhook must be a terminal notification failure")
	}
	page, err := app.MemberStore.QueryEvents(EventQuery{Profile: "apple-usw2", IncludeSystem: true, Limit: 20})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	counts := map[string]int{}
	for _, event := range page.Events {
		counts[event.Action]++
		if event.Action == "wechat.failed" && event.ErrorCode != "notification_error" {
			t.Fatalf("missing webhook error code = %q", event.ErrorCode)
		}
	}
	if counts["wechat.pending"] != 1 || counts["wechat.failed"] != 1 || counts["wechat.sent"] != 0 {
		t.Fatalf("missing webhook events = %+v", counts)
	}
}

func TestRedactWechatWebhookURL(t *testing.T) {
	raw := "post https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=secret failed"
	got := redactWechatWebhookURL(raw)
	if strings.Contains(got, "secret") || !strings.Contains(got, "key=[redacted]") {
		t.Fatalf("redacted = %s", got)
	}
}
