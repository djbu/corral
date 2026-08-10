// This file implements step 10's attach protocol (design doc §5) on top of
// the already-hijacked net.Conn internal/api/handlers_attach.go hands us:
// Hello parsing, single-attacher/take-over (§5.5), the resize-on-attach +
// repaint sequence, the writer goroutine draining screen.Subscriber (§5.4),
// the reader loop dispatching Input/Resize/Goodbye/Ping/Pong, and the
// daemon-side ping/pong keepalive. supervisor.go's reaper goroutine sends
// the one frame that belongs to a session's exit rather than its
// attachment's lifecycle: 0x83 Exit.
package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/djbu/corral/internal/proto"
	"github.com/djbu/corral/internal/screen"
	"github.com/djbu/corral/internal/session"
)

// ErrAlreadyAttached is returned by installAttachment when a session
// already has an attacher and the new Hello did not set take_over.
var ErrAlreadyAttached = fmt.Errorf("supervisor: session already has an attached client")

// Attachment is one client's live attach-protocol session. LiveSession.
// Attachment holds at most one at a time (§5.5); Registry.mu guards that
// field, not this struct's own state.
type Attachment struct {
	conn net.Conn
	sub  *screen.Subscriber

	// writeMu serializes every frame write: the writer goroutine (Output),
	// the ping goroutine (Ping), and the reader loop (Pong/Goodbye-ack/
	// Error) all write conn concurrently once Attach's setup phase ends —
	// without this, two interleaved WriteFrame calls could corrupt framing
	// mid-header on the wire.
	writeMu sync.Mutex

	mu     sync.Mutex
	closed bool
	reason string // "" (normal), "taken_over", "client_too_slow", "ping_timeout"
	done   chan struct{}

	// missedPings counts ping ticks sent since the last Pong (reset by
	// pongReceived); attachPingLoop compares it against a timeout-derived
	// threshold instead of touching wall-clock time directly, keeping the
	// keepalive drivable entirely through the injected clock.Clock.
	missedPings atomic.Int32
}

func newAttachment(conn net.Conn, sub *screen.Subscriber) *Attachment {
	return &Attachment{conn: conn, sub: sub, done: make(chan struct{})}
}

func (a *Attachment) writeFrame(f proto.Frame) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return proto.WriteFrame(a.conn, f)
}

// forceClose closes done and conn exactly once, recording reason (the
// first call wins — later calls, including the always-run cleanup call at
// the end of Attach, are no-ops). Closing conn is what unblocks a reader
// loop parked in proto.ReadFrame, on this attachment or an incumbent's.
func (a *Attachment) forceClose(reason string) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.reason = reason
	a.mu.Unlock()
	close(a.done)
	_ = a.conn.Close()
}

func (a *Attachment) forcedReason() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reason
}

// mustEncode marshals v for a control frame payload. Encoding one of this
// package's own proto structs cannot realistically fail (no channels,
// funcs, or cyclic pointers); falling back to "{}" rather than propagating
// an error keeps every write-site above from needing its own error path
// for a case that isn't reachable in practice.
func mustEncode(v any) []byte {
	b, err := proto.EncodeJSON(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// installAttachment atomically checks for an incumbent and installs att as
// ls's new Attachment, under Registry.mu — the single critical section
// §5.5 needs so two concurrent take_over:false attaches can never both
// "win". Returns the incumbent (nil if there wasn't one) that the caller
// must now evict.
func (r *Registry) installAttachment(ls *LiveSession, att *Attachment, takeOver bool) (*Attachment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	incumbent := ls.Attachment
	if incumbent != nil && !takeOver {
		return nil, ErrAlreadyAttached
	}
	ls.Attachment = att
	return incumbent, nil
}

// clearAttachmentIfCurrent removes att from ls.Attachment, but only if it
// is still the current one — a take-over may already have replaced it
// with a newer Attachment by the time the evicted attach's Attach() call
// gets around to its own cleanup defer.
func (r *Registry) clearAttachmentIfCurrent(ls *LiveSession, att *Attachment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ls.Attachment == att {
		ls.Attachment = nil
	}
}

// Attached reports whether idOrName currently has a live Attachment. Used
// by handlers_sessions.go to populate sessionResponse.Attached — the M1
// wire field that was always false before step 10 (design doc §9.2).
func (r *Registry) Attached(idOrName string) bool {
	ls, ok := r.Get(idOrName)
	if !ok {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return ls.Attachment != nil
}

// lookupName resolves id back to the name it was registered under, for
// Ready.Name — Registry tracks the name->ID direction (for Get) but not
// the reverse, so this is a short linear scan over what is in practice a
// handful of live sessions.
func (r *Registry) lookupName(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, sid := range r.names {
		if sid == id {
			return name, true
		}
	}
	return "", false
}

// applyResize resizes both the emulator and the PTY to rows x cols and
// persists them to the store. Zero rows/cols (a Hello or Resize frame that
// omitted them) is ignored rather than collapsing the session to a 0x0
// screen.
func (r *Registry) applyResize(ctx context.Context, ls *LiveSession, rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	ls.Screen.Resize(int(rows), int(cols))
	if err := setPTYSize(ls.PTYMaster, rows, cols); err != nil {
		r.log.Warn("supervisor: attach: resizing PTY", "session_id", ls.SessionID, "err", err)
	}
	if _, err := r.store.UpdateSession(ctx, ls.SessionID, func(sess *session.Session) {
		sess.Rows = int(rows)
		sess.Cols = int(cols)
	}); err != nil {
		r.log.Warn("supervisor: attach: persisting resize", "session_id", ls.SessionID, "err", err)
	}
}

// defaultPingInterval/defaultPingTimeout mirror config's own attach.*
// defaults (internal/config/load.go's defaultsLayer) — used whenever a
// Registry is built with a zero-value Config.PingInterval/PingTimeout,
// which every pre-step-10 test that constructs Config{} still does.
const (
	defaultPingInterval = 15 * time.Second
	defaultPingTimeout  = 45 * time.Second
)

func (r *Registry) pingInterval() time.Duration {
	if r.cfg.PingInterval > 0 {
		return r.cfg.PingInterval
	}
	return defaultPingInterval
}

func (r *Registry) pingTimeout() time.Duration {
	if r.cfg.PingTimeout > 0 {
		return r.cfg.PingTimeout
	}
	return defaultPingTimeout
}

// Attach runs the daemon side of the attach protocol (design doc §5) over
// conn/br — an already-upgraded connection from handlers_attach.go's
// hijack, br being the bufio.Reader that may already hold client bytes the
// HTTP layer buffered ahead of the hijack. It blocks until the client
// detaches, crashes, is taken over, or is dropped for falling behind; the
// caller (the HTTP handler) then closes conn and returns.
func (r *Registry) Attach(ls *LiveSession, conn net.Conn, br *bufio.Reader) error {
	ctx := context.Background()

	if ls.Headless {
		// design doc §5.2: a headless LiveSession carries no PTYMaster and
		// no Screen (both nil) — there is nothing to attach a terminal
		// client to. Reject before even reading Hello, same as the
		// already_attached case below.
		_ = proto.WriteFrame(conn, proto.Frame{Type: proto.TypeError, Payload: mustEncode(proto.ErrorPayload{
			Code:    "headless_no_attach",
			Message: "headless sessions have no PTY to attach to",
		})})
		return nil
	}

	f, err := proto.ReadFrame(br)
	if err != nil {
		return fmt.Errorf("supervisor: attach: reading Hello: %w", err)
	}
	if f.Type != proto.TypeHello {
		return fmt.Errorf("supervisor: attach: expected Hello (0x01), got %#x", f.Type)
	}
	var hello proto.Hello
	if err := proto.DecodeJSON(f.Payload, &hello); err != nil {
		return fmt.Errorf("supervisor: attach: decoding Hello: %w", err)
	}

	sub := screen.NewSubscriber()
	att := newAttachment(conn, sub)

	incumbent, err := r.installAttachment(ls, att, hello.TakeOver)
	if err != nil {
		_ = proto.WriteFrame(conn, proto.Frame{Type: proto.TypeError, Payload: mustEncode(proto.ErrorPayload{
			Code:    "already_attached",
			Message: "session already has an attached client; retry with take_over:true to evict it",
		})})
		return nil
	}
	if incumbent != nil {
		_ = incumbent.writeFrame(proto.Frame{Type: proto.TypeDetached, Payload: mustEncode(proto.Detached{Reason: "taken_over"})})
		incumbent.forceClose("taken_over")
		r.appendEvent(ctx, ls.SessionID, session.EventSessionDetached, map[string]any{"reason": "taken_over"})
	}
	defer r.clearAttachmentIfCurrent(ls, att)

	// A client attaching is human/external activity (design doc's
	// activity-tracking step) — bump last_activity_ms now that the
	// attachment has been successfully installed.
	r.touchActivity(ls.SessionID)

	rows, cols := normalizeSize(uint16(hello.Rows), uint16(hello.Cols))
	r.applyResize(ctx, ls, rows, cols)

	repaint := ls.Screen.Attach(sub)
	defer ls.Screen.Detach(sub)

	r.appendEvent(ctx, ls.SessionID, session.EventSessionAttached, map[string]any{"client_version": hello.ClientVersion})
	attachedAtMs := r.clock.Now().UnixMilli()
	if _, err := r.store.UpdateSession(ctx, ls.SessionID, func(sess *session.Session) {
		sess.LastAttachedAtMs = &attachedAtMs
	}); err != nil {
		r.log.Warn("supervisor: attach: recording last_attached_at", "session_id", ls.SessionID, "err", err)
	}

	name := ls.SessionID
	if n, ok := r.lookupName(ls.SessionID); ok {
		name = n
	}
	ready := proto.Ready{
		SessionID:    ls.SessionID,
		Name:         name,
		Rows:         int(rows),
		Cols:         int(cols),
		AltScreen:    ls.Screen.AltScreen(),
		RepaintBytes: len(repaint),
	}
	// Ready, then the repaint as one Output frame, written here — before
	// the writer goroutine below starts draining sub.Ch — so no live byte
	// fed after Screen.Attach can ever be written ahead of the snapshot
	// that was taken at the same instant sub was registered.
	if err := att.writeFrame(proto.Frame{Type: proto.TypeReady, Payload: mustEncode(ready)}); err != nil {
		return nil
	}
	if len(repaint) > 0 {
		if err := att.writeFrame(proto.Frame{Type: proto.TypeOutput, Payload: repaint}); err != nil {
			return nil
		}
	}

	writerDone := make(chan struct{})
	go r.attachWriterLoop(att, sub, writerDone)

	// attachOverflowWatchdog runs alongside the writer loop rather than
	// sharing its select: once a slow client has filled sub.Ch, the writer
	// loop is parked inside a blocking att.writeFrame (the client isn't
	// reading, so the conn's send buffer fills too) and never reaches a
	// case that could notice sub.Done firing. This goroutine's only job is
	// to force-close the conn from the outside so that blocked write
	// returns an error and the writer loop unwinds on its own next line.
	watchdogDone := make(chan struct{})
	go r.attachOverflowWatchdog(ls, att, sub, watchdogDone)

	pingDone := make(chan struct{})
	go r.attachPingLoop(att, pingDone)

	goodbye := false
	var readErr error
readLoop:
	for {
		frm, err := proto.ReadFrame(br)
		if err != nil {
			readErr = err
			break
		}
		if !proto.IsKnown(frm.Type) {
			continue
		}
		switch frm.Type {
		case proto.TypeInput:
			_, _ = ls.PTYMaster.Write(frm.Payload)
		case proto.TypeResize:
			var rs proto.Resize
			if err := proto.DecodeJSON(frm.Payload, &rs); err == nil {
				r.applyResize(ctx, ls, uint16(rs.Rows), uint16(rs.Cols))
				r.appendEvent(ctx, ls.SessionID, session.EventSessionResized, map[string]any{"rows": rs.Rows, "cols": rs.Cols})
			}
		case proto.TypeGoodbyeClose:
			goodbye = true
			_ = att.writeFrame(proto.Frame{Type: proto.TypeGoodbye, Payload: mustEncode(proto.Goodbye{Reason: "detach_ack"})})
			break readLoop
		case proto.TypePing:
			_ = att.writeFrame(proto.Frame{Type: proto.TypePong, Payload: frm.Payload})
		case proto.TypePong:
			att.pongReceived()
		}
	}

	att.forceClose("") // no-op if take-over/slow-client/ping-timeout already closed it
	<-writerDone
	<-watchdogDone
	<-pingDone

	switch att.forcedReason() {
	case "taken_over", "client_too_slow", "session_exited":
		// Already logged (or, for session_exited, intentionally not
		// logged as a distinct event — the reaper's own session lifecycle
		// event already covers the child exiting) by whichever code
		// forced the close.
	default:
		if goodbye {
			r.appendEvent(ctx, ls.SessionID, session.EventSessionDetached, map[string]any{"reason": "detach"})
		} else {
			msg := ""
			if readErr != nil {
				msg = readErr.Error()
			}
			r.appendEvent(ctx, ls.SessionID, session.EventSessionClientCrashed, map[string]any{"error": msg})
		}
	}
	return nil
}

// attachWriterLoop drains sub.Ch to Output frames until the attachment
// closes or Screen overflow-drops sub (§5.4) — the one place that knows a
// drop happened, so it alone sends Error{client_too_slow} and logs
// session.client_dropped before forcing the close.
func (r *Registry) attachWriterLoop(att *Attachment, sub *screen.Subscriber, done chan struct{}) {
	defer close(done)
	for {
		select {
		case buf := <-sub.Ch:
			if err := att.writeFrame(proto.Frame{Type: proto.TypeOutput, Payload: buf}); err != nil {
				att.forceClose("")
				return
			}
		case <-att.done:
			return
		}
	}
}

// attachOverflowWatchdog is the only thing watching sub.Done: sharing that
// case with attachWriterLoop's select doesn't work, because once a slow
// client has filled sub.Ch, the writer loop is parked inside a blocking
// att.writeFrame — the client isn't reading, so the conn's send buffer is
// full too — and never gets back around to its select to notice sub.Done
// fire. This goroutine force-closes the conn from the outside instead,
// which unblocks that pending write with an error and lets the writer loop
// unwind on its own next line.
func (r *Registry) attachOverflowWatchdog(ls *LiveSession, att *Attachment, sub *screen.Subscriber, done chan struct{}) {
	defer close(done)
	select {
	case <-sub.Done:
		if sub.Dropped() {
			r.appendEvent(context.Background(), ls.SessionID, session.EventSessionClientDropped, nil)
			// forceClose first, and skip the courtesy client_too_slow
			// Error frame (a deliberate deviation from a literal reading
			// of design doc §5.4): the client has by definition stopped
			// reading, so writeFrame here risks blocking on the same
			// full send buffer that got it dropped in the first place —
			// exactly the wedge this goroutine exists to avoid. The
			// client sees a reset connection instead of an Error frame.
			att.forceClose("client_too_slow")
		}
	case <-att.done:
	}
}

// attachPingLoop sends a Ping frame every ping interval and force-closes
// the attachment with reason "ping_timeout" (treated as a crash by
// Attach's own bookkeeping — §5.2: "no pong within ping_timeout ->
// crash-disconnect") if pingsSinceLastPong ever exceeds the configured
// timeout's multiple of the interval. att.pongReceived resets the counter
// from Attach's reader loop.
func (r *Registry) attachPingLoop(att *Attachment, done chan struct{}) {
	defer close(done)

	interval := r.pingInterval()
	timeout := r.pingTimeout()
	if interval <= 0 {
		return
	}
	maxMissed := int32(1)
	if timeout > interval {
		maxMissed = int32(timeout / interval)
	}

	ticker := r.clock.NewTicker(interval)
	defer ticker.Stop()

	n := 0
	for {
		select {
		case <-att.done:
			return
		case <-ticker.C():
			n++
			if err := att.writeFrame(proto.Frame{Type: proto.TypePing, Payload: mustEncode(proto.PingPong{N: n})}); err != nil {
				att.forceClose("")
				return
			}
			if att.missedPings.Add(1) > maxMissed {
				att.forceClose("ping_timeout")
				return
			}
		}
	}
}

// pongReceived resets the missed-ping counter attachPingLoop maintains.
func (a *Attachment) pongReceived() {
	a.missedPings.Store(0)
}
