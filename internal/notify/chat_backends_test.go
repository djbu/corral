package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSlackBackend_Send(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-secret" {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["channel"] != "D1" || body["mrkdwn"] != false {
			t.Errorf("body = %#v", body)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	b := NewSlackBackend("xoxb-secret", "D1", srv.Client())
	b.endpoint = srv.URL
	if err := b.Send(context.Background(), Notification{SessionName: "x", Event: "blocked", Detail: "redacted"}); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramBackend_RejectsAPIFailureWithoutLeakingBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["chat_id"] != "42" {
			t.Errorf("body = %#v", body)
		}
		_, _ = w.Write([]byte(`{"ok":false,"description":"private upstream detail"}`))
	}))
	defer srv.Close()
	b := NewTelegramBackend("token", "42", srv.Client())
	b.endpoint = srv.URL
	err := b.Send(context.Background(), Notification{SessionName: "x", Event: "blocked", Detail: "redacted"})
	if err == nil || err.Error() != "telegram: API rejected request" {
		t.Fatalf("Send error = %v", err)
	}
}

func TestChatBackend_RetriesServerFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer srv.Close()
	b := NewSlackBackend("token", "D1", srv.Client())
	b.endpoint = srv.URL
	err := b.Send(context.Background(), Notification{})
	if err == nil || !retryable(err) {
		t.Fatalf("Send error = %v, want retryable", err)
	}
}
