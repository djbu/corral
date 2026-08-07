// Package screen is corral's headless terminal (design doc §4): a per-
// session wrapper around a charmbracelet/x/vt Emulator that (1) tracks DEC
// private modes independently of the emulator's own recognition of them,
// (2) can reproduce its exact visual state as a repaint byte sequence for
// a freshly attaching client, and (3) fans live bytes out to attached
// subscribers without ever letting a slow subscriber block Feed.
//
// Every method except Feed may be called from any goroutine; Feed is
// called only by the single PTY-reader goroutine that owns a given
// session. All of Screen's own state, and every touch of the wrapped
// vt.Emulator, is guarded by s.mu — including callbacks the Emulator
// invokes synchronously from inside emu.Write/Resize/Close, which is why
// those callbacks below mutate Screen fields directly instead of trying to
// re-acquire s.mu (the calling goroutine already holds it).
package screen

import (
	"io"
	"log/slog"
	"sync"

	vt "github.com/charmbracelet/x/vt"
)

// repliesChanCap bounds the internal buffer that drains the emulator's
// device-query replies (see the pumpReplies doc comment below).
const repliesChanCap = 64

// Screen wraps a vt.Emulator with mode sniffing, snapshotting, and
// subscriber fan-out.
type Screen struct {
	mu        sync.Mutex
	closeOnce sync.Once
	emu       *vt.Emulator
	log       *slog.Logger

	sniff sniffer
	modes map[int]bool

	title         string
	cursorVisible bool

	subs map[*Subscriber]struct{}

	outLog *OutputLog

	repliesCh chan []byte
}

// New creates a Screen with the given initial PTY size. log must not be
// nil (pass slog.Default() if the caller has none of its own).
//
// New also starts an internal goroutine that continuously drains the
// wrapped emulator's device-query replies (see pumpReplies) — this is not
// optional plumbing: x/vt answers queries like `ESC[?6n` by writing
// synchronously, from inside emu.Write, to an internal io.Pipe that blocks
// until something reads it. Without a permanent drain, Feed would deadlock
// on any input containing such a query — and it isn't hypothetical: the
// M0 capture used by the keystone test (§10.2) contains a real `ESC[?6n`
// during claude's own startup sequence.
func New(rows, cols int, log *slog.Logger) *Screen {
	if log == nil {
		log = slog.Default()
	}

	// vt.NewEmulator takes (width, height); Screen's own signature takes
	// (rows, cols) per design doc §4 — the conversion happens here, once.
	emu := vt.NewEmulator(cols, rows)

	s := &Screen{
		emu:           emu,
		log:           log,
		modes:         make(map[int]bool),
		cursorVisible: true, // x/vt's default cursor state is visible until a mode change says otherwise; see deviations list.
		subs:          make(map[*Subscriber]struct{}),
		repliesCh:     make(chan []byte, repliesChanCap),
	}

	// Deviation from the design's literal callback list (§4 mentions
	// "callbacks feed alt-screen/cursor-visibility"): alt-screen state is
	// read directly via emu.IsAltScreen() at snapshot time instead, since
	// that getter exists and is simpler than tracking it via callback. Only
	// Title and CursorVisibility are wired here, because IsAltScreen() has
	// no getter-free equivalent for cursor visibility (see deviations
	// list: Emulator has no public Cursor()/Hidden accessor).
	emu.SetCallbacks(vt.Callbacks{
		Title: func(t string) {
			s.title = t
		},
		CursorVisibility: func(visible bool) {
			s.cursorVisible = visible
		},
	})

	go s.pumpReplies()

	return s
}

// pumpReplies continuously calls emu.Read, which is x/vt's channel for
// device-query answers (CPR/DA/DSR/etc. — see the New doc comment), and
// forwards each chunk to repliesCh for Replies() to serve. The send is
// non-blocking with the same drop-on-overflow policy as Subscriber: a
// reply that nobody drains in time is lost, which is acceptable (§4
// already accepts a *duplicate* reply when a client is attached; losing
// one when nobody is even asking is strictly less consequential). What is
// not acceptable is this loop ever blocking on repliesCh<- and, in turn,
// leaving emu.Read uncalled — that is exactly what would let a query
// write block inside Feed.
func (s *Screen) pumpReplies() {
	buf := make([]byte, 4096)
	for {
		n, err := s.emu.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case s.repliesCh <- chunk:
			default:
				s.log.Warn("screen: dropping device-query reply, Replies() reader not keeping up")
			}
		}
		if err != nil {
			close(s.repliesCh)
			return
		}
	}
}

// Feed feeds p through the mode sniffer, the emulator, the output log (if
// set), and every attached subscriber, in that order, all under s.mu. It
// never blocks: emu.Write's own device-query replies are drained by the
// permanent pumpReplies goroutine rather than by any reader of Replies(),
// and each subscriber send is non-blocking with overflow-drop (§5.4).
func (s *Screen) Feed(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sniff.feed(p, func(mode int, enabled bool) {
		s.modes[mode] = enabled
	})

	_, _ = s.emu.Write(p)

	if s.outLog != nil {
		_, _ = s.outLog.Write(p)
	}

	if len(s.subs) == 0 {
		return
	}

	// One copy, shared read-only across every subscriber's channel send —
	// p itself is Feed's caller's reused PTY-read buffer and must not be
	// retained past this call.
	cp := append([]byte(nil), p...)
	for sub := range s.subs {
		select {
		case sub.Ch <- cp:
		default:
			delete(s.subs, sub)
			sub.markDropped()
			s.log.Warn("screen: subscriber too slow, dropping")
		}
	}
}

// Attach atomically snapshots the current visual state and registers sub
// to receive subsequent live bytes — no byte fed after this call returns
// can be lost or duplicated relative to the returned repaint, because both
// happen under the same s.mu critical section as Feed.
func (s *Screen) Attach(sub *Subscriber) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	repaint := s.snapshotLocked()
	s.subs[sub] = struct{}{}
	return repaint
}

// Detach removes sub from the broadcast set and closes its Done channel.
// Safe to call even if sub was already overflow-dropped by Feed — the
// close is idempotent (see Subscriber.close).
func (s *Screen) Detach(sub *Subscriber) {
	s.mu.Lock()
	delete(s.subs, sub)
	s.mu.Unlock()
	sub.close()
}

// Resize resizes the underlying emulator.
func (s *Screen) Resize(rows, cols int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emu.Resize(cols, rows)
}

// Size returns the current (rows, cols).
func (s *Screen) Size() (rows, cols int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emu.Height(), s.emu.Width()
}

// Replies returns an io.Reader of device-query answers the emulator has
// generated (e.g. in response to `ESC[6n`), meant to be copied to the PTY
// master by the session's owner (supervisor, from M1 step 9 onward) so
// the child gets an answer even with nobody attached. It is a thin view
// onto the internal buffer pumpReplies already maintains — Screen's
// correctness does not depend on anyone ever reading it (see New).
func (s *Screen) Replies() io.Reader {
	return &repliesReader{ch: s.repliesCh}
}

type repliesReader struct {
	ch  <-chan []byte
	buf []byte
}

func (r *repliesReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		chunk, ok := <-r.ch
		if !ok {
			return 0, io.EOF
		}
		r.buf = chunk
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// SetOutputLog wires an OutputLog that Feed will tee every fed byte
// through. New's signature (design doc §4) has no room for one — the
// per-session output.log path isn't known until step 6's spawn plumbing
// creates the session directory — so this is a separate setter, callable
// once before Feed is ever called. A nil log (the default) means Feed
// simply skips the tee.
func (s *Screen) SetOutputLog(l *OutputLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outLog = l
}

// Close unblocks pumpReplies and lets its goroutine exit, without going
// anywhere near Emulator.Close/Emulator.Read's unsynchronized `closed`
// bool (confirmed by reading emulator.go in the pinned pseudo-version —
// calling Emulator.Close concurrently with the Read that pumpReplies runs
// forever is a genuine data race, caught by `go test -race` the first time
// this wrapper tried that route). Instead, Close closes the *io.PipeWriter*
// returned by Emulator.InputPipe() — the same object x/vt's CSI/DSR
// handlers write query replies into. Closing a pipe writer is safe to do
// concurrently with a blocked reader (io.Pipe's documented contract): the
// parked Read in pumpReplies returns (0, io.EOF), pumpReplies closes
// repliesCh and returns, and any reply write racing after Close gets
// io.ErrClosedPipe instead of blocking (Feed already discards emu.Write's
// return value, so this is silent and safe). Idempotent via closeOnce; a
// failed type assertion (only possible if a future x/vt version changes
// InputPipe's concrete type) logs and no-ops rather than panicking — see
// the deviations list.
func (s *Screen) Close() {
	s.closeOnce.Do(func() {
		c, ok := s.emu.InputPipe().(interface{ Close() error })
		if !ok {
			s.log.Warn("screen: InputPipe is not an io.Closer in this x/vt version; pumpReplies goroutine will not be unblocked")
			return
		}
		if err := c.Close(); err != nil {
			s.log.Warn("screen: closing input pipe", "err", err)
		}
	})
}
