package connectmac

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	envWechatWebhookURL = "CONNECTMAC_WECHAT_WEBHOOK_URL"
	envWebBaseURL       = "CONNECTMAC_WEB_BASE_URL"
)

var errWechatWebhookNotConfigured = fmt.Errorf("wechat webhook not configured")

func wechatWebhookConfigFingerprint(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return fmt.Sprintf("sha256:%x", sum[:])
}

type WechatNotifier struct {
	WebhookURL string
	WebBaseURL string
	Client     *http.Client
}

type WechatNotification struct {
	Event         string
	Profile       string
	AppleEmail    string
	Owner         string
	Operator      string
	HostID        string
	HostCreatedAt string
	DueAt         string
	Management    bool
	Description   string
}

type WechatNotifyResult struct {
	Skipped      bool   `json:"skipped"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	ErrorCode    int    `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	Message      string `json:"message,omitempty"`
}

func NewWechatNotifierFromEnv() WechatNotifier {
	return WechatNotifier{
		WebhookURL: strings.TrimSpace(os.Getenv(envWechatWebhookURL)),
		WebBaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv(envWebBaseURL)), "/"),
		Client:     &http.Client{Timeout: 10 * time.Second},
	}
}

func (n WechatNotifier) Send(notification WechatNotification) (WechatNotifyResult, error) {
	webhook := strings.TrimSpace(n.WebhookURL)
	if webhook == "" {
		return WechatNotifyResult{Skipped: true, Message: errWechatWebhookNotConfigured.Error()}, errWechatWebhookNotConfigured
	}
	payload := map[string]interface{}{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"content": n.markdown(notification),
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return WechatNotifyResult{}, err
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Post(webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		return WechatNotifyResult{}, fmt.Errorf("send wechat webhook %s: %w", redactWechatWebhookURL(webhook), err)
	}
	defer resp.Body.Close()
	var response struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return WechatNotifyResult{HTTPStatus: resp.StatusCode}, fmt.Errorf("decode wechat webhook response: %w", err)
	}
	result := WechatNotifyResult{
		HTTPStatus:   resp.StatusCode,
		ErrorCode:    response.ErrCode,
		ErrorMessage: response.ErrMsg,
		Message:      response.ErrMsg,
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || response.ErrCode != 0 {
		return result, fmt.Errorf("wechat webhook failed status=%d errcode=%d errmsg=%s", resp.StatusCode, response.ErrCode, response.ErrMsg)
	}
	return result, nil
}

func (n WechatNotifier) markdown(notification WechatNotification) string {
	var b strings.Builder
	title := wechatEventTitle(notification.Event)
	if notification.Description != "" {
		title = notification.Description
	}
	fmt.Fprintf(&b, "## ConnectMac %s\n", title)
	writeWechatField(&b, "Profile", notification.Profile)
	writeWechatField(&b, "Apple", notification.AppleEmail)
	writeWechatField(&b, "操作人", notification.Operator)
	writeWechatField(&b, "Host", notification.HostID)
	writeWechatField(&b, "Host 创建时间", formatBeijingDisplayTime(notification.HostCreatedAt))
	writeWechatField(&b, "释放提醒时间", formatBeijingDisplayTime(notification.DueAt))
	if notification.Management && strings.TrimSpace(n.WebBaseURL) != "" {
		writeWechatField(&b, "管理页", strings.TrimRight(n.WebBaseURL, "/"))
	}
	return strings.TrimSpace(b.String())
}

func writeWechatField(b *strings.Builder, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	fmt.Fprintf(b, "\n> %s：%s", label, value)
}

func wechatEventTitle(event string) string {
	switch strings.TrimSpace(event) {
	case "open":
		return "打开成功"
	case "extend":
		return "释放提醒已延长"
	case "due":
		return "释放提醒已到期"
	case "release":
		return "释放成功"
	default:
		return "通知"
	}
}

func redactWechatWebhookURL(value string) string {
	idx := strings.Index(value, "key=")
	if idx < 0 {
		return value
	}
	end := idx + len("key=")
	for end < len(value) {
		ch := value[end]
		if ch == '&' || ch == ' ' || ch == '\n' || ch == '\t' {
			break
		}
		end++
	}
	return value[:idx] + "key=[redacted]" + value[end:]
}
