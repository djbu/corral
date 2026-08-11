package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/djbu/corral/internal/answer"
	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
)

// IncomingMessage is a provider-authenticated message normalized at the
// notification boundary. Backend adapters, not untrusted payloads, construct
// it: ReceivedAt is the local observation time and MessageID is the provider's
// stable retry identity.
type IncomingMessage struct {
	Backend    string
	MessageID  string
	SenderID   string
	ChatID     string
	ReceivedAt time.Time
	Body       string
}

// ConversationAccess is the fail-closed allowlist for one backend. A sender
// must be present in AllowedSenders AND its conversation must be present in
// AllowedChats. Values are provider identities, never display names.
type ConversationAccess struct {
	AllowedSenders map[string]struct{}
	AllowedChats   map[string]struct{}
}

// ConversationOptions controls the common remote-reply admission boundary.
// Backends absent from Access are rejected. ReplyTTL is evaluated against the
// local observation timestamp, never a provider-supplied timestamp.
type ConversationOptions struct {
	Access   map[string]ConversationAccess
	ReplyTTL time.Duration
}

// ReplyDecision describes a completed, non-error admission decision. Provider
// adapters may acknowledge every one of these values; only an error means the
// durable state is unknown and the provider should retry.
type ReplyDecision string

const (
	ReplyAccepted  ReplyDecision = "accepted"
	ReplyDuplicate ReplyDecision = "duplicate"
	ReplyRejected  ReplyDecision = "rejected"
	ReplyExpired   ReplyDecision = "expired"
)

// Conversation is M10a's sole generic path from an authenticated provider
// message to a session PTY. It deliberately owns no HTTP listener or provider
// credentials: adapters authenticate their protocol, then call Accept.
type Conversation struct {
	access   map[string]ConversationAccess
	replyTTL time.Duration
	store    *store.Store
	input    InputWriter
	clk      clock.Clock
	log      *slog.Logger
}

func NewConversation(opts ConversationOptions, st *store.Store, input InputWriter, clk clock.Clock, log *slog.Logger) *Conversation {
	if log == nil {
		log = slog.Default()
	}
	return &Conversation{
		access:   opts.Access,
		replyTTL: opts.ReplyTTL,
		store:    st,
		input:    input,
		clk:      clk,
		log:      log,
	}
}

// Accept validates and atomically claims a message before writing the encoded
// response to a live blocked session. Claiming precedes WriteInput on purpose:
// a provider retry can never create a second PTY write after a daemon restart.
// If WriteInput subsequently fails, the safe outcome is a consumed reply, not
// an ambiguous second attempt.
func (c *Conversation) Accept(ctx context.Context, msg IncomingMessage) (ReplyDecision, error) {
	access, ok := c.access[msg.Backend]
	if !ok || msg.MessageID == "" || msg.SenderID == "" || msg.ChatID == "" || msg.ReceivedAt.IsZero() || !allowed(access, msg) {
		return ReplyRejected, nil
	}
	now := c.clk.Now()
	if c.replyTTL <= 0 || msg.ReceivedAt.After(now) || now.Sub(msg.ReceivedAt) > c.replyTTL {
		return ReplyExpired, nil
	}
	name, text, ok := splitReply(msg.Body)
	if !ok || len(text) > answer.MaxBytes {
		return ReplyRejected, nil
	}
	sess, err := c.store.GetSessionByName(ctx, name)
	if err != nil || sess.AgentState != session.AgentBlocked {
		return ReplyRejected, nil
	}
	payload, err := answer.Encode(text, "", true)
	if err != nil {
		return ReplyRejected, nil
	}

	claimed, err := c.store.ClaimIncomingMessage(ctx, msg.Backend, identifierHash(msg.Backend, msg.MessageID), msg.ReceivedAt.UnixMilli(), msg.ReceivedAt.Add(c.replyTTL).UnixMilli())
	if err != nil {
		return "", fmt.Errorf("notify: claim incoming %s message: %w", msg.Backend, err)
	}
	if !claimed {
		return ReplyDuplicate, nil
	}
	if err := c.input.WriteInput(ctx, sess.ID, payload); err != nil {
		return "", fmt.Errorf("notify: deliver %s reply: %w", msg.Backend, err)
	}

	// Persist only one-way identity hashes and byte length. The answer body,
	// raw provider identifiers, and credentials never enter the event log.
	audit := map[string]any{
		"via":             msg.Backend,
		"len":             len(text),
		"message_id_hash": identifierHash(msg.Backend, msg.MessageID),
		"sender_id_hash":  identifierHash(msg.Backend, msg.SenderID),
		"chat_id_hash":    identifierHash(msg.Backend, msg.ChatID),
	}
	if requestSeq := state.PermissionRequestSeq(sess.BlockedReasonJSON); requestSeq > 0 {
		audit["permission_request_seq"] = requestSeq
	}
	if data, err := json.Marshal(audit); err == nil {
		if _, err := c.store.AppendEvent(context.Background(), sess.ID, session.EventSessionAnswered, string(data)); err != nil {
			c.log.Warn("notify: appending accepted reply audit failed", "backend", msg.Backend, "session", name, "err", err)
		}
	}
	return ReplyAccepted, nil
}

func allowed(access ConversationAccess, msg IncomingMessage) bool {
	_, senderOK := access.AllowedSenders[msg.SenderID]
	_, chatOK := access.AllowedChats[msg.ChatID]
	return senderOK && chatOK
}

func identifierHash(backend, value string) string {
	sum := sha256.Sum256([]byte(backend + "\x00" + value))
	return hex.EncodeToString(sum[:])
}
