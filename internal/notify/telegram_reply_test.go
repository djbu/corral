package notify

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTelegramSubscriberPoll_UsesConversationAndOffset(t *testing.T) {
	c, writer, clk := testConversation(t)
	var gotOffset string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOffset = r.URL.Query().Get("offset")
		_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":41,"message":{"text":"blocked continue","from":{"id":7},"chat":{"id":9}}}]}`))
	}))
	defer srv.Close()
	s := NewTelegramSubscriber("token", c, clk, discardLogger())
	s.endpoint = srv.URL
	s.client = srv.Client()
	if err := s.poll(); err != nil {
		t.Fatal(err)
	}
	if writer.count() != 1 {
		t.Fatalf("writes = %d, want 1", writer.count())
	}
	if err := s.poll(); err != nil {
		t.Fatal(err)
	}
	if gotOffset != "42" {
		t.Fatalf("second poll offset = %q, want 42", gotOffset)
	}
	if writer.count() != 1 {
		t.Fatalf("duplicate update wrote %d times, want 1", writer.count())
	}
}
