package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SlackBackend sends one redacted notification to one configured Slack
// conversation through chat.postMessage. The endpoint is fixed by the
// constructor; repositories cannot select it.
type SlackBackend struct {
	endpoint string
	token    string
	channel  string
	client   *http.Client
}

func NewSlackBackend(token, channel string, client *http.Client) *SlackBackend {
	return &SlackBackend{endpoint: "https://slack.com/api/chat.postMessage", token: token, channel: channel, client: client}
}

func (b *SlackBackend) Name() string { return "slack" }

func (b *SlackBackend) Send(ctx context.Context, n Notification) error {
	body, err := json.Marshal(map[string]any{"channel": b.channel, "text": n.Title() + "\n" + n.Detail, "mrkdwn": false})
	if err != nil {
		return &sendError{retry: false, msg: "slack: marshal: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
	if err != nil {
		return &sendError{retry: false, msg: "slack: build request: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := b.client.Do(req)
	if err != nil {
		return &sendError{retry: true, msg: "slack: post: " + err.Error()}
	}
	if err := classifyStatus("slack", resp.StatusCode); err != nil {
		drainAndClose(resp)
		return err
	}
	return chatResult("slack", resp)
}

// TelegramBackend sends one redacted notification with sendMessage. It uses
// the Bot API endpoint derived from the operator token and never accepts an
// arbitrary base URL in production (tests replace endpoint directly).
type TelegramBackend struct {
	endpoint string
	chatID   string
	client   *http.Client
}

func NewTelegramBackend(token, chatID string, client *http.Client) *TelegramBackend {
	return &TelegramBackend{endpoint: "https://api.telegram.org/bot" + token + "/sendMessage", chatID: chatID, client: client}
}

func (b *TelegramBackend) Name() string { return "telegram" }

func (b *TelegramBackend) Send(ctx context.Context, n Notification) error {
	body, err := json.Marshal(map[string]any{"chat_id": b.chatID, "text": n.Title() + "\n" + n.Detail, "disable_web_page_preview": true})
	if err != nil {
		return &sendError{retry: false, msg: "telegram: marshal: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
	if err != nil {
		return &sendError{retry: false, msg: "telegram: build request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return &sendError{retry: true, msg: "telegram: post: " + err.Error()}
	}
	if err := classifyStatus("telegram", resp.StatusCode); err != nil {
		drainAndClose(resp)
		return err
	}
	return chatResult("telegram", resp)
}

// chatError deliberately omits response bodies: upstream responses can echo
// private destination data and are not useful to an operator-facing event.
func chatError(backend string, err error) error {
	return &sendError{retry: false, msg: fmt.Sprintf("%s: %s", backend, strings.TrimSpace(err.Error()))}
}

func chatResult(backend string, resp *http.Response) error {
	defer resp.Body.Close()
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return chatError(backend, fmt.Errorf("decode response: %w", err))
	}
	if !result.OK {
		return &sendError{retry: false, msg: backend + ": API rejected request"}
	}
	return nil
}
