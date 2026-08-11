package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/session"
)

func testConversation(t *testing.T) (*Conversation, *fakeInputWriter, *clocktest.FakeClock) {
	t.Helper()
	clk := clocktest.NewFake(time.Unix(1_700_000_000, 0))
	st := openTestStore(t, clk)
	makeSession(t, st, "blocked-id", "blocked", "/tmp/blocked")
	setAgentState(t, st, "blocked-id", session.AgentBlocked)
	writer := newFakeWriter()
	return NewConversation(ConversationOptions{
		ReplyTTL:  10 * time.Minute,
		DedupeTTL: 24 * time.Hour,
		Access: map[string]ConversationAccess{
			"slack":    {AllowedSenders: map[string]struct{}{"user-1": {}, "private-user": {}}, AllowedChats: map[string]struct{}{"chat-1": {}, "private-chat": {}}},
			"telegram": {AllowedSenders: map[string]struct{}{"7": {}}, AllowedChats: map[string]struct{}{"9": {}}},
		},
	}, st, writer, clk, discardLogger()), writer, clk
}

func TestConversationAccept_GatesAndDeduplicates(t *testing.T) {
	c, writer, clk := testConversation(t)
	now := clk.Now()
	base := IncomingMessage{Backend: "slack", MessageID: "message-1", SenderID: "user-1", ChatID: "chat-1", ReceivedAt: now, Body: "blocked `; rm -rf / #$(whoami)`"}

	decision, err := c.Accept(context.Background(), base)
	if err != nil || decision != ReplyAccepted {
		t.Fatalf("Accept = %q, %v; want accepted, nil", decision, err)
	}
	if writer.count() != 1 {
		t.Fatalf("writes = %d, want 1", writer.count())
	}
	decision, err = c.Accept(context.Background(), base)
	if err != nil || decision != ReplyDuplicate {
		t.Fatalf("duplicate Accept = %q, %v; want duplicate, nil", decision, err)
	}
	if writer.count() != 1 {
		t.Fatalf("writes after duplicate = %d, want 1", writer.count())
	}

	for _, msg := range []IncomingMessage{
		{Backend: "slack", MessageID: "bad-user", SenderID: "other", ChatID: "chat-1", ReceivedAt: now, Body: "blocked hello"},
		{Backend: "slack", MessageID: "bad-chat", SenderID: "user-1", ChatID: "other", ReceivedAt: now, Body: "blocked hello"},
		{Backend: "telegram", MessageID: "unknown", SenderID: "user-1", ChatID: "chat-1", ReceivedAt: now, Body: "blocked hello"},
		{Backend: "slack", MessageID: "malformed", SenderID: "user-1", ChatID: "chat-1", ReceivedAt: now, Body: "blocked"},
		{Backend: "slack", MessageID: "stale", SenderID: "user-1", ChatID: "chat-1", ReceivedAt: now.Add(-11 * time.Minute), Body: "blocked hello"},
	} {
		got, err := c.Accept(context.Background(), msg)
		want := ReplyRejected
		if msg.MessageID == "stale" {
			want = ReplyExpired
		}
		if err != nil || got != want {
			t.Fatalf("Accept(%s) = %q, %v; want %q, nil", msg.MessageID, got, err, want)
		}
	}
	if writer.count() != 1 {
		t.Fatalf("writes after rejected messages = %d, want 1", writer.count())
	}
}

func TestConversationAccept_AuditHashesOnly(t *testing.T) {
	c, _, clk := testConversation(t)
	const rawID = "provider-message-secret"
	const rawSender = "private-user"
	const rawChat = "private-chat"
	const rawText = "answer with private content"
	decision, err := c.Accept(context.Background(), IncomingMessage{
		Backend: "slack", MessageID: rawID, SenderID: rawSender, ChatID: rawChat, ReceivedAt: clk.Now(), Body: "blocked " + rawText,
	})
	if err != nil || decision != ReplyAccepted {
		t.Fatalf("Accept = %q, %v", decision, err)
	}
	// The event is appended after WriteInput, synchronously in Accept.
	st := c.store
	evs, err := st.ListEvents(context.Background(), "blocked-id")
	if err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	for _, ev := range evs {
		if ev.Kind == session.EventSessionAnswered {
			if err := json.Unmarshal([]byte(ev.DataJSON), &audit); err != nil {
				t.Fatal(err)
			}
		}
	}
	if audit == nil || audit["message_id_hash"] == rawID || audit["sender_id_hash"] == rawSender || audit["chat_id_hash"] == rawChat {
		t.Fatalf("audit leaks raw provider identity: %#v", audit)
	}
	if got, _ := json.Marshal(audit); strings.Contains(string(got), rawText) {
		t.Fatalf("audit leaks reply text: %s", got)
	}
}
