package notify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielbecerra/corral/internal/answer"
	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
)

// The reply subscriber (design doc §8.6) closes M2's exit criterion —
// "answer from the phone via ntfy reply → agent continues" — with ZERO
// inbound ports. Instead of listening, the daemon makes an *outbound*
// long-poll (`GET {server}/{reply_topic}/json`, streaming) and treats each
// published message as an `answer` call.
//
// Threat model: anyone who can publish to the reply topic can type arbitrary
// text into a live agent's PTY. That is a remote-input capability, so it is
// off by default and gated three ways:
//
//  1. reply.topic MUST differ from notify.topic, and
//  2. reply.token is REQUIRED when enabled —
//     both enforced at startup by config.ValidateReply (the daemon refuses to
//     start otherwise), so a guessable read topic never grants write access
//     and there are no anonymous reply topics; and
//  3. every message must be "<session-name> <text>" and is rejected here
//     unless the named session exists, is live, and is currently blocked.
//
// Messages are rate-limited (10/min), the text is never shell-interpreted (it
// goes through the same answer.Encode as `corral answer`), and every accepted
// reply appends session.answered with via:"ntfy". The subscriber reconnects
// with backoff on a dropped stream — but a non-retryable auth rejection
// (401/403 on the reply topic) STOPS it loudly rather than looping, so a
// misconfigured token fails closed and visibly instead of silently inert.

const (
	// replyRateLimit / replyRateWindow bound how many stream messages the
	// subscriber will *process* per window. The limit is on RECEIVED messages,
	// not accepted ones: this is a flood-control primitive against a hostile
	// publisher, so a burst of garbage must be throttled too. (A future reader
	// might be tempted to count only accepted replies — don't; that reopens the
	// flood, since rejected messages would be free.)
	replyRateLimit  = 10
	replyRateWindow = time.Minute

	// replyMaxLine caps a single stream line. ntfy wraps each message in a JSON
	// envelope, so allow the max answer text plus envelope overhead. An overlong
	// line (bufio.ErrTooLong) is treated as a dropped stream and reconnected.
	replyMaxLine = answer.MaxBytes + 4096
)

// InputWriter is the one supervisor capability the reply subscriber needs:
// deliver already-encoded bytes to a live session's PTY. Declaring it here
// (rather than importing internal/supervisor) keeps notify a leaf of the
// supervisor, not a cycle. *supervisor.Registry satisfies it.
type InputWriter interface {
	WriteInput(ctx context.Context, idOrName string, b []byte) error
}

// ReplyConfig is the resolved [notify.ntfy] subset the subscriber needs. It is
// built from config in daemon wiring, after config.ValidateReply has already
// enforced the startup gate.
type ReplyConfig struct {
	Server string
	Topic  string
	Token  string
}

// ReplySubscriber runs a single background goroutine that long-polls the reply
// topic and delivers accepted replies. All of its work — the stream, the
// rate-limit bookkeeping, the gate checks — happens on that one goroutine, so
// the mutable state (recent) needs no lock.
type ReplySubscriber struct {
	server string
	topic  string
	token  string

	store  *store.Store
	input  InputWriter
	clk    clock.Clock
	log    *slog.Logger
	client *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// recent holds the timestamps of messages processed within the rate window.
	// Touched only from run's goroutine (see the type doc), so no lock.
	recent []time.Time

	// afterStop is a TEST-ONLY hook, nil in production, fired when run returns
	// (matches the Dispatcher's afterArm/afterJob convention). It lets a test
	// observe the difference between "reconnect loop still alive" and "stopped
	// on a non-retryable error" without a timing race.
	afterStop func()
}

// newReplyClient builds the subscriber's dedicated HTTP client. It has NO
// Timeout on purpose — a long-poll connection stays open for minutes at a
// time, and http.Client.Timeout caps the whole request including the streaming
// body read, which would guillotine the stream on every cycle. Liveness comes
// from ctx cancellation (Close) and reconnect instead. Proxy is pinned nil for
// the same reason as the outbound backends (NewHTTPClient): no unaudited egress
// via ambient HTTP_PROXY.
func newReplyClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil},
	}
}

// NewReplySubscriber builds a subscriber. log defaults to slog.Default(). The
// caller (daemon) must have validated the reply config with
// config.ValidateReply first — this constructor assumes topic/token are set.
func NewReplySubscriber(cfg ReplyConfig, st *store.Store, iw InputWriter, clk clock.Clock, log *slog.Logger) *ReplySubscriber {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &ReplySubscriber{
		server: strings.TrimRight(cfg.Server, "/"),
		topic:  cfg.Topic,
		token:  cfg.Token,
		store:  st,
		input:  iw,
		clk:    clk,
		log:    log,
		client: newReplyClient(),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start launches the subscriber goroutine.
func (s *ReplySubscriber) Start() {
	s.wg.Add(1)
	go s.run()
}

// Close cancels the in-flight long-poll and waits for the goroutine to drain.
// It must run before the store is closed (the accept path writes
// session.answered on context.Background(), like the Dispatcher's events).
func (s *ReplySubscriber) Close() {
	s.cancel()
	s.wg.Wait()
}

// run is the reconnect loop. It reconnects with capped backoff on a transient
// drop (clean EOF, 5xx, transport error) but STOPS on a non-retryable error
// (a 4xx auth rejection on the reply topic): reconnecting on a 401 would loop
// forever while reporting success, so it fails loud instead.
func (s *ReplySubscriber) run() {
	defer s.wg.Done()
	if s.afterStop != nil {
		defer s.afterStop()
	}
	attempt := 0
	for {
		if s.ctx.Err() != nil {
			return
		}
		opened, err := s.stream()
		if s.ctx.Err() != nil {
			return
		}
		if err != nil && !retryable(err) {
			s.log.Error("notify: reply subscriber stopping — reply topic rejected (non-retryable); check notify.ntfy.reply.token/topic", "err", err)
			return
		}
		if opened {
			// A stream that produced data resets the backoff, so a
			// long-lived connection that finally drops reconnects promptly.
			attempt = 0
		}
		attempt++
		wait := reconnectBackoff(attempt)
		if err != nil {
			s.log.Warn("notify: reply stream dropped, reconnecting", "err", err, "backoff", wait)
		} else {
			s.log.Debug("notify: reply stream ended, reconnecting", "backoff", wait)
		}
		if !s.sleep(wait) {
			return
		}
	}
}

// stream opens one long-poll connection and processes messages until it drops.
// It returns opened=true if the connection delivered at least one line (used to
// reset backoff), and a non-nil error classified as retryable/non-retryable via
// the shared sendError machinery.
func (s *ReplySubscriber) stream() (opened bool, err error) {
	url := s.server + "/" + s.topic + "/json"
	req, reqErr := http.NewRequestWithContext(s.ctx, http.MethodGet, url, nil)
	if reqErr != nil {
		// A malformed URL is a config error, not transient.
		return false, &sendError{retry: false, msg: "ntfy-reply: build request: " + reqErr.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, doErr := s.client.Do(req)
	if doErr != nil {
		// Transport failure (DNS, connection reset, ctx cancel) — transient.
		return false, &sendError{retry: true, msg: "ntfy-reply: get: " + doErr.Error()}
	}
	defer resp.Body.Close()

	if statusErr := classifyStatus("ntfy-reply", resp.StatusCode); statusErr != nil {
		return false, statusErr
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 4096), replyMaxLine)
	for sc.Scan() {
		if s.ctx.Err() != nil {
			return opened, nil
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		opened = true
		s.handleLine(line)
	}
	if scErr := sc.Err(); scErr != nil {
		// Includes bufio.ErrTooLong for an overlong line: treat as a dropped
		// stream and reconnect rather than trusting a truncated body.
		return opened, &sendError{retry: true, msg: "ntfy-reply: read: " + scErr.Error()}
	}
	// Clean EOF: the server closed an idle long-poll. Normal; reconnect.
	return opened, nil
}

// handleLine decodes one ntfy stream line. The stream carries open/keepalive/
// poll_request control events interleaved with the actual "message" events; a
// line that is not a message (or does not decode) is ignored.
func (s *ReplySubscriber) handleLine(line []byte) {
	var msg struct {
		Event   string `json:"event"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		s.log.Debug("notify: reply: undecodable stream line", "err", err)
		return
	}
	if msg.Event != "message" {
		return
	}
	s.handleMessage(msg.Message)
}

// handleMessage applies the rate limit and the three-way gate to one published
// message, and delivers it if it passes. Rejections are logged (no session row
// exists for most of them) rather than recorded as events; only an accepted
// reply appends session.answered, mirroring the HTTP answer handler.
func (s *ReplySubscriber) handleMessage(raw string) {
	if !s.allow() {
		s.log.Warn("notify: reply rate-limited, dropping message", "limit_per_min", replyRateLimit)
		return
	}

	name, text, ok := splitReply(raw)
	if !ok {
		s.log.Warn("notify: reply malformed, dropping (want '<session-name> <text>')")
		return
	}
	if len(text) > answer.MaxBytes {
		s.log.Warn("notify: reply text too large, dropping", "session", name, "len", len(text))
		return
	}

	sess, err := s.store.GetSessionByName(s.ctx, name)
	if err != nil {
		// store.ErrNotFound → no such session; any other lookup error is also
		// non-actionable from a remote reply. Never echo raw text.
		s.log.Warn("notify: reply for unknown session, dropping", "session", name)
		return
	}

	// Gate #3 (blocked). NOTE: this reads the durable session row, but the
	// engine recomputes agent_state on its own goroutine, so there is a small
	// race where a reply admitted here lands as a keystroke into a session that
	// just left "blocked". The consequence is a stray keystroke into a live
	// agent the operator already owns — not privilege escalation — so it is
	// accepted for M2. This is deliberately NOT presented as a checked
	// invariant; it is a named race.
	if sess.AgentState != session.AgentBlocked {
		s.log.Warn("notify: reply for non-blocked session, dropping", "session", name, "state", string(sess.AgentState))
		return
	}

	// Same encoder as `corral answer`: opaque bytes, never shell-interpreted.
	// newline=true so the reply is submitted, matching a phone user pressing
	// send/return.
	payload, err := answer.Encode(text, "", true)
	if err != nil {
		s.log.Warn("notify: reply encode failed, dropping", "session", name, "err", err)
		return
	}

	// Gate #3 (live): WriteInput returns ErrNotLive if the session is no longer
	// live, closing the last half of the exists/live/blocked gate.
	if err := s.input.WriteInput(s.ctx, sess.ID, payload); err != nil {
		s.log.Warn("notify: reply delivery failed, dropping", "session", name, "err", err)
		return
	}

	// Durable audit trail of the accepted reply. Records the raw byte length
	// (not the redacted length — redacting first would leak secret-shape via
	// the length) and via:"ntfy" so this reply is distinguishable from an HTTP
	// `corral answer`. Uses context.Background() so the write survives Close's
	// ctx cancellation, matching the Dispatcher's event writes.
	data, mErr := json.Marshal(map[string]any{"via": "ntfy", "len": len(text)})
	if mErr == nil {
		if _, aErr := s.store.AppendEvent(context.Background(), sess.ID, session.EventSessionAnswered, string(data)); aErr != nil {
			s.log.Warn("notify: reply appending session.answered failed", "session", name, "err", aErr)
		}
	}
	s.log.Info("notify: reply delivered", "session", name, "len", len(text))
}

// allow implements the received-message rate limit (see replyRateLimit's doc).
// Called only from run's goroutine, so no lock. It prunes timestamps older than
// the window, then admits (and records) the current message if under the limit.
func (s *ReplySubscriber) allow() bool {
	now := s.clk.Now()
	cutoff := now.Add(-replyRateWindow)
	kept := s.recent[:0]
	for _, t := range s.recent {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	s.recent = kept
	if len(s.recent) >= replyRateLimit {
		return false
	}
	s.recent = append(s.recent, now)
	return true
}

// sleep waits d on the injected clock, returning false if the subscriber is
// closing (so the reconnect loop exits promptly on shutdown).
func (s *ReplySubscriber) sleep(d time.Duration) bool {
	select {
	case <-s.clk.After(d):
		return true
	case <-s.ctx.Done():
		return false
	}
}

// splitReply parses a message body into "<session-name> <text>". The name is
// the leading whitespace-delimited token; text is the remainder (which may
// contain spaces), with only the separating whitespace trimmed. Returns
// ok=false if either side is empty.
func splitReply(raw string) (name, text string, ok bool) {
	raw = strings.TrimRight(raw, "\r\n")
	raw = strings.TrimLeft(raw, " \t")
	i := strings.IndexAny(raw, " \t")
	if i <= 0 {
		return "", "", false
	}
	name = raw[:i]
	text = strings.TrimLeft(raw[i+1:], " \t")
	if name == "" || text == "" {
		return "", "", false
	}
	return name, text, true
}

// reconnectBackoff is the wait before the (attempt+1)th connection after
// attempt consecutive failures: 1s, 2s, 4s, ... capped at 30s. attempt is >= 1.
func reconnectBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Second << (attempt - 1)
	if d <= 0 || d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}
