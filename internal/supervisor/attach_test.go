package supervisor

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/proto"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
)

// sendHello writes a Hello frame (design doc §5.2) to conn.
func sendHello(t *testing.T, conn net.Conn, hello proto.Hello) {
	t.Helper()
	if err := proto.WriteFrame(conn, proto.Frame{Type: proto.TypeHello, Payload: mustJSON(t, hello)}); err != nil {
		t.Fatalf("WriteFrame(Hello): %v", err)
	}
}

// killAndWaitReaped kills a live session and blocks until the async
// reaper (supervisor.go's runGoroutines exit goroutine) has finished
// recording terminal state in the store. killingCheckpointer sends
// SIGKILL but — unlike a real Checkpoint — never writes
// session.StatusStopping first, so Kill returning does not mean the
// reap that follows cmd.Wait() unblocking has completed. Without this
// wait, that reap can still be running (and writing to the store) after
// the next test's t.TempDir cleanup has already closed this test's
// database, producing a harmless but noisy "database is closed" log
// line and extra concurrency that destabilizes -race detection for
// unrelated tests running immediately afterward.
func killAndWaitReaped(t *testing.T, r *Registry, st *store.Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := r.Kill(ctx, id, nil); err != nil {
		t.Logf("Kill(%q): %v", id, err)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		sess, err := st.GetSession(ctx, id)
		return err == nil && sess.Status == session.StatusExited
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := proto.EncodeJSON(v)
	if err != nil {
		t.Fatalf("EncodeJSON(%#v): %v", v, err)
	}
	return b
}

// fakeClient drains a daemon-side attachment connection on its own
// goroutine, auto-replying Pong to Ping so attachPingLoop never sees a
// missed pong for a client that is otherwise alive, and handing every
// other frame off on a buffered channel for the test to consume via
// next. Draining continuously (instead of a test reading frames one at
// a time off the raw conn) matters: a wedged reader is exactly what
// TestSlowClientIsDropped exercises on purpose, and every other test
// must NOT accidentally recreate that by going silent after its first
// couple of reads — a real corral attach client (cmd/corral/cmd_attach.go)
// never stops reading while alive.
type fakeClient struct {
	conn   net.Conn
	frames chan proto.Frame
}

func newFakeClient(t *testing.T, conn net.Conn) *fakeClient {
	t.Helper()
	fc := &fakeClient{conn: conn, frames: make(chan proto.Frame, 512)}
	br := bufio.NewReader(conn)
	go func() {
		for {
			f, err := proto.ReadFrame(br)
			if err != nil {
				close(fc.frames)
				return
			}
			if f.Type == proto.TypePing {
				_ = proto.WriteFrame(conn, proto.Frame{Type: proto.TypePong, Payload: f.Payload})
				continue
			}
			select {
			case fc.frames <- f:
			default: // test isn't asserting on this one; keep draining the wire.
			}
		}
	}()
	return fc
}

// next returns the next frame of type want, discarding any other frame
// types that arrive first (e.g. an interleaved SIGWINCH-triggered
// Output frame while waiting for a specific Ready).
func (fc *fakeClient) next(t *testing.T, want proto.Type, timeout time.Duration) proto.Frame {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-fc.frames:
			if !ok {
				t.Fatalf("connection closed while waiting for frame %#x", want)
			}
			if f.Type == want {
				return f
			}
		case <-deadline:
			t.Fatalf("no %#x frame within %v", want, timeout)
		}
	}
}

// TestAttachRepaint verifies the core happy path (design doc §5.2-§5.3)
// straight against Registry.Attach over a net.Pipe, below the HTTP
// hijack layer (handlers_attach_test.go covers that layer separately):
// Hello -> Ready (with a non-empty repaint) -> Output carrying the
// alt-screen-enter sequence -> GoodbyeClose -> Attach returns, and the
// attached/detached events are logged.
func TestAttachRepaint(t *testing.T) {
	r, st, spec := newLifecycleTestRegistry(t, killingCheckpointer{}, nil)
	ctx := context.Background()
	if _, err := r.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { killAndWaitReaped(t, r, st, ctx, spec.ID) })

	ls, ok := r.Get(spec.ID)
	if !ok {
		t.Fatalf("Get(%q) = false, want true", spec.ID)
	}

	// fakeclaude's alt-screen-enter write races Spawn's return (Spawn
	// returns once the child process starts, not once its startup
	// output has propagated through the PTY into the Screen emulator).
	// Wait for it before attaching so Ready.AltScreen reflects reality.
	waitForCondition(t, 5*time.Second, func() bool { return ls.Screen.AltScreen() })

	client, server := net.Pipe()
	defer client.Close()
	fc := newFakeClient(t, client)
	done := make(chan struct{})
	go func() {
		_ = r.Attach(ls, server, bufio.NewReader(server))
		close(done)
	}()

	sendHello(t, client, proto.Hello{Rows: 24, Cols: 80, TakeOver: true, ClientVersion: "test"})

	ready := fc.next(t, proto.TypeReady, 5*time.Second)
	var readyPayload proto.Ready
	if err := proto.DecodeJSON(ready.Payload, &readyPayload); err != nil {
		t.Fatalf("decoding Ready: %v", err)
	}
	if !readyPayload.AltScreen {
		t.Fatalf("Ready.AltScreen = false, want true (fakeclaude enters the alt screen on startup)")
	}
	if readyPayload.RepaintBytes == 0 {
		t.Fatalf("Ready.RepaintBytes = 0, want > 0")
	}

	repaint := fc.next(t, proto.TypeOutput, 5*time.Second)
	if !strings.Contains(string(repaint.Payload), "1049h") {
		t.Fatalf("repaint Output does not contain alt-screen enter sequence:\n%q", repaint.Payload)
	}

	if err := proto.WriteFrame(client, proto.Frame{Type: proto.TypeGoodbyeClose, Payload: mustJSON(t, proto.GoodbyeClose{Reason: "detach"})}); err != nil {
		t.Fatalf("WriteFrame(GoodbyeClose): %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not return after GoodbyeClose")
	}

	events, err := st.ListEvents(ctx, spec.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventSessionAttached) {
		t.Fatalf("events = %v, want %q event", eventKinds(events), session.EventSessionAttached)
	}
	if !hasEventKind(events, session.EventSessionDetached) {
		t.Fatalf("events = %v, want %q event", eventKinds(events), session.EventSessionDetached)
	}
}

// TestTakeOver verifies design doc §5.5's single-attacher-per-session
// take-over semantics: a polite (take_over:false) second attacher
// against an occupied session is rejected; a take_over:true second
// attacher evicts the incumbent, which sees Detached{Reason:"taken_over"}
// and returns from Attach.
func TestTakeOver(t *testing.T) {
	r, st, spec := newLifecycleTestRegistry(t, killingCheckpointer{}, nil)
	ctx := context.Background()
	if _, err := r.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { killAndWaitReaped(t, r, st, ctx, spec.ID) })

	ls, ok := r.Get(spec.ID)
	if !ok {
		t.Fatalf("Get(%q) = false, want true", spec.ID)
	}
	waitForCondition(t, 5*time.Second, func() bool { return ls.Screen.AltScreen() })

	client1, server1 := net.Pipe()
	defer client1.Close()
	fc1 := newFakeClient(t, client1)
	done1 := make(chan struct{})
	go func() {
		_ = r.Attach(ls, server1, bufio.NewReader(server1))
		close(done1)
	}()
	sendHello(t, client1, proto.Hello{Rows: 24, Cols: 80, TakeOver: true, ClientVersion: "incumbent"})
	fc1.next(t, proto.TypeReady, 5*time.Second)
	fc1.next(t, proto.TypeOutput, 5*time.Second) // repaint

	// A polite second attacher (take_over:false) against an occupied
	// session gets rejected instead of evicting the incumbent.
	politeClient, politeServer := net.Pipe()
	defer politeClient.Close()
	politeFc := newFakeClient(t, politeClient)
	politeDone := make(chan struct{})
	go func() {
		_ = r.Attach(ls, politeServer, bufio.NewReader(politeServer))
		close(politeDone)
	}()
	sendHello(t, politeClient, proto.Hello{Rows: 24, Cols: 80, TakeOver: false, ClientVersion: "polite"})
	politeResp := politeFc.next(t, proto.TypeError, 5*time.Second)
	if politeResp.Type != proto.TypeError {
		t.Fatalf("polite second-attacher frame type = %#x, want Error (%#x)", politeResp.Type, proto.TypeError)
	}
	select {
	case <-politeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("polite second-attacher's Attach did not return after rejection")
	}

	// A take_over:true second attacher evicts the incumbent.
	client2, server2 := net.Pipe()
	defer client2.Close()
	fc2 := newFakeClient(t, client2)
	done2 := make(chan struct{})
	go func() {
		_ = r.Attach(ls, server2, bufio.NewReader(server2))
		close(done2)
	}()
	sendHello(t, client2, proto.Hello{Rows: 24, Cols: 80, TakeOver: true, ClientVersion: "evictor"})

	detached := fc1.next(t, proto.TypeDetached, 5*time.Second)
	var detachedPayload proto.Detached
	if err := proto.DecodeJSON(detached.Payload, &detachedPayload); err != nil {
		t.Fatalf("decoding Detached: %v", err)
	}
	if detachedPayload.Reason != "taken_over" {
		t.Fatalf("Detached.Reason = %q, want %q", detachedPayload.Reason, "taken_over")
	}

	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("client1's Attach did not return after being taken over")
	}

	f := fc2.next(t, proto.TypeReady, 5*time.Second)
	if f.Type != proto.TypeReady {
		t.Fatalf("client2 first frame type = %#x, want Ready", f.Type)
	}
	if !r.Attached(spec.ID) {
		t.Fatal("Attached() = false after take-over, want true (client2 now attached)")
	}

	events, err := st.ListEvents(ctx, spec.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventSessionDetached) {
		t.Fatalf("events = %v, want %q event after take-over", eventKinds(events), session.EventSessionDetached)
	}

	client2.Close()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
	}
}

// TestClientCrashIsNotDetach verifies design doc §5.4's crash-vs-detach
// distinction: a connection that simply dies (no GoodbyeClose) logs
// session.client_crashed, never session.detached.
func TestClientCrashIsNotDetach(t *testing.T) {
	r, st, spec := newLifecycleTestRegistry(t, killingCheckpointer{}, nil)
	ctx := context.Background()
	if _, err := r.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { killAndWaitReaped(t, r, st, ctx, spec.ID) })

	ls, ok := r.Get(spec.ID)
	if !ok {
		t.Fatalf("Get(%q) = false, want true", spec.ID)
	}
	waitForCondition(t, 5*time.Second, func() bool { return ls.Screen.AltScreen() })

	client, server := net.Pipe()
	fc := newFakeClient(t, client)
	done := make(chan struct{})
	go func() {
		_ = r.Attach(ls, server, bufio.NewReader(server))
		close(done)
	}()

	sendHello(t, client, proto.Hello{Rows: 24, Cols: 80, TakeOver: true, ClientVersion: "crasher"})
	fc.next(t, proto.TypeReady, 5*time.Second)
	fc.next(t, proto.TypeOutput, 5*time.Second) // repaint

	if err := client.Close(); err != nil {
		t.Fatalf("client.Close: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not return after client crash")
	}

	events, err := st.ListEvents(ctx, spec.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventSessionClientCrashed) {
		t.Fatalf("events = %v, want %q event", eventKinds(events), session.EventSessionClientCrashed)
	}
	if hasEventKind(events, session.EventSessionDetached) {
		t.Fatalf("events = %v, a crashed client must never log %q", eventKinds(events), session.EventSessionDetached)
	}
}

// TestSlowClientIsDropped verifies design doc §5.4's overflow policy: a
// client that stops reading must eventually be dropped instead of
// wedging the session's PTY-reader/broadcast path forever. It feeds
// screen.Screen.Feed directly (skipping the real PTY) so the number of
// chunks reaching the subscriber doesn't depend on how the kernel
// happens to chunk the real child's output across reads. This test
// deliberately goes silent on the connection after Ready — that's the
// scenario under test, unlike every other test here, which uses
// fakeClient to keep draining.
func TestSlowClientIsDropped(t *testing.T) {
	r, st, spec := newLifecycleTestRegistry(t, killingCheckpointer{}, nil)
	ctx := context.Background()
	if _, err := r.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { killAndWaitReaped(t, r, st, ctx, spec.ID) })

	ls, ok := r.Get(spec.ID)
	if !ok {
		t.Fatalf("Get(%q) = false, want true", spec.ID)
	}

	client, server := net.Pipe()
	defer client.Close()
	br := bufio.NewReader(client)
	done := make(chan struct{})
	go func() {
		_ = r.Attach(ls, server, bufio.NewReader(server))
		close(done)
	}()

	sendHello(t, client, proto.Hello{Rows: 24, Cols: 80, TakeOver: true, ClientVersion: "slow"})
	if f, err := proto.ReadFrame(br); err != nil || f.Type != proto.TypeReady {
		t.Fatalf("first frame: err=%v type=%#x, want Ready", err, f.Type)
	}
	if f, err := proto.ReadFrame(br); err != nil || f.Type != proto.TypeOutput {
		t.Fatalf("second frame (repaint): err=%v type=%#x, want Output", err, f.Type)
	}

	// Client goes silent from here on; feed enough chunks directly into
	// the screen to overflow the subscriber's bounded channel.
	for i := 0; i < 300; i++ {
		ls.Screen.Feed([]byte{'x'})
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Attach did not return for an overflowed slow client")
	}

	if r.Attached(spec.ID) {
		t.Fatal("Attached() = true after overflow-drop, want false")
	}
	events, err := st.ListEvents(ctx, spec.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventSessionClientDropped) {
		t.Fatalf("events = %v, want %q event", eventKinds(events), session.EventSessionClientDropped)
	}
}

// TestResizePropagates verifies a Resize frame (design doc §5.4)
// propagates end to end: the real PTY's winsize, the Screen emulator's
// dimensions, the persisted session row/col, and — since the PTY resize
// delivers SIGWINCH to the child — fakeclaude's own re-render, observed
// via its FAKECLAUDE_RESIZED marker (test/fakeclaude/tui.go). Logs
// session.resized.
func TestResizePropagates(t *testing.T) {
	r, st, spec := newLifecycleTestRegistry(t, killingCheckpointer{}, nil)
	ctx := context.Background()
	if _, err := r.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { killAndWaitReaped(t, r, st, ctx, spec.ID) })

	ls, ok := r.Get(spec.ID)
	if !ok {
		t.Fatalf("Get(%q) = false, want true", spec.ID)
	}
	waitForCondition(t, 5*time.Second, func() bool { return ls.Screen.AltScreen() })

	client, server := net.Pipe()
	defer client.Close()
	fc := newFakeClient(t, client)
	done := make(chan struct{})
	go func() {
		_ = r.Attach(ls, server, bufio.NewReader(server))
		close(done)
	}()

	sendHello(t, client, proto.Hello{Rows: 24, Cols: 80, TakeOver: true, ClientVersion: "resizer"})
	fc.next(t, proto.TypeReady, 5*time.Second)
	fc.next(t, proto.TypeOutput, 5*time.Second) // repaint

	if err := proto.WriteFrame(client, proto.Frame{Type: proto.TypeResize, Payload: mustJSON(t, proto.Resize{Rows: 50, Cols: 200})}); err != nil {
		t.Fatalf("WriteFrame(Resize): %v", err)
	}

	// The PTY resize delivers SIGWINCH to fakeclaude, which re-renders
	// and writes a grep-able marker; wait for it to round-trip through
	// Screen and back out as an Output frame. This is deliberately the
	// *only* winsize check in this test: fakeclaude observes rows/cols
	// via pty.Getsize on its own (slave-side) stdout after SIGWINCH, so
	// a matching marker is strictly stronger end-to-end proof than a
	// test-side pty.Getsize(ls.PTYMaster) read would be — and unlike
	// that read, it can never race the supervisor's reaper, which owns
	// and closes the PTY master on child exit (that read is exactly
	// what internal/poll's Fd()-vs-Close() shape flags as a data race
	// under -race, since os.File.Fd() bypasses the poll.FD refcount).
	found := false
	var seen strings.Builder
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		out := fc.next(t, proto.TypeOutput, 5*time.Second)
		seen.Write(out.Payload)
		if strings.Contains(string(out.Payload), "FAKECLAUDE_RESIZED rows=50 cols=200") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("never saw FAKECLAUDE_RESIZED rows=50 cols=200 in an Output frame after resize; accumulated output:\n%q", seen.String())
	}

	if rows, cols := ls.Screen.Size(); rows != 50 || cols != 200 {
		t.Fatalf("Screen.Size() = (%d, %d), want (50, 200)", rows, cols)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		sess, err := st.GetSession(ctx, spec.ID)
		if err != nil {
			return false
		}
		return sess.Rows == 50 && sess.Cols == 200
	})

	if err := proto.WriteFrame(client, proto.Frame{Type: proto.TypeGoodbyeClose, Payload: mustJSON(t, proto.GoodbyeClose{Reason: "detach"})}); err != nil {
		t.Fatalf("WriteFrame(GoodbyeClose): %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not return after GoodbyeClose")
	}

	events, err := st.ListEvents(ctx, spec.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventSessionResized) {
		t.Fatalf("events = %v, want %q event", eventKinds(events), session.EventSessionResized)
	}
}
