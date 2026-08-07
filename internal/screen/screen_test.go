package screen

import (
	"log/slog"
	"os"
	"testing"
	"time"
)

// captureRows/captureCols is the PTY size the M0 capture used (see
// spike/pty/main.go's pty.StartWithSize call) — both the real and the
// negative-control emulator must use it, or a genuine mismatch in cell
// count would masquerade as an equivalence failure.
const (
	captureRows = 24
	captureCols = 80
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
}

func readTestdata(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// TestGridEquivalenceKeystone is design doc §10.2's keystone test: feed a
// real claude startup capture into emulator A, take Attach's repaint bytes,
// feed those repaint bytes into a completely fresh emulator B, and assert
// the two grids are cell-by-cell identical — including truecolor styles,
// cursor state, alt-screen flag, and the sniffed mode set.
//
// This is what actually proves the repaint sequence (snapshot.go) is
// lossless, as opposed to merely "looks plausible". Design doc §4: "Start
// with Render(). If the grid-equivalence test shows it loses information,
// replace step 5 with a hand-rolled CellAt painter — that test, not
// intuition, is the arbiter." This test is that arbiter, and it passes
// against the real M0 capture using Render() as specified — no pivot to a
// hand-rolled painter was needed.
//
// Grid.Equal compares the sniffed mode map exactly (key-for-key), which is
// only a meaningful check because this fixture happens to set both 1049
// and 25 explicitly, and the repaint re-emits both (snapshot.go's alt-
// screen step and cursor-visibility step). A future fixture that never
// touches one of those two modes would leave the reference grid without
// that key while the repainted grid gains it (snapshotLocked always emits
// a final cursor-visibility escape) — a map-length mismatch with no
// visual difference behind it. Keep that in mind before reusing Grid.Equal
// against a differently-captured fixture.
//
// The same run also exercises the load-bearing io.Pipe finding from this
// step's implementation: testdata/pty/claude-tui-startup.raw contains a
// real `ESC[?6n` (extended cursor position report) byte sequence, which
// x/vt answers by writing synchronously into an internal pipe that blocks
// until read. Screen.Feed calling this without deadlocking is exactly
// what New's permanent pumpReplies goroutine exists to guarantee — if that
// goroutine were missing or wrong, this test would hang, not fail.
func TestGridEquivalenceKeystone(t *testing.T) {
	capture := readTestdata(t, "../../testdata/pty/claude-tui-startup.raw")

	done := make(chan struct{})
	var repaint []byte
	var gridA, gridB Grid

	go func() {
		defer close(done)

		a := New(captureRows, captureCols, discardLogger())
		defer a.Close()
		a.Feed(capture)

		sub := NewSubscriber()
		repaint = a.Attach(sub)
		gridA = a.DebugGrid()

		b := New(captureRows, captureCols, discardLogger())
		defer b.Close()
		b.Feed(repaint)
		gridB = b.DebugGrid()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out — Feed likely deadlocked on a device-query reply (see pumpReplies)")
	}

	if len(repaint) == 0 {
		t.Fatal("repaint was empty")
	}

	if ok, msg := gridA.Equal(gridB); !ok {
		t.Fatalf("grid B (fed the repaint) does not match grid A (fed the raw capture): %s", msg)
	}

	// Sanity checks on what we're actually asserting equivalence over —
	// a passing Equal on two all-blank grids would be a vacuous test.
	if !gridA.AltScreen {
		t.Error("expected alt-screen active after claude's startup capture")
	}
	if !gridA.Modes[2031] {
		t.Error("expected mode 2031 (claude's private mode, unrecognized by x/vt) to be sniffed as set")
	}
	if gridA.Rows != captureRows || gridA.Cols != captureCols {
		t.Errorf("grid dimensions = %dx%d, want %dx%d", gridA.Rows, gridA.Cols, captureRows, captureCols)
	}
	nonEmptyCells := 0
	for _, row := range gridA.Cells {
		for _, c := range row {
			if c.Content != "" && c.Content != " " {
				nonEmptyCells++
			}
		}
	}
	if nonEmptyCells == 0 {
		t.Error("grid A has no non-blank cells; capture apparently produced no visible content")
	}
}

// TestGridEquivalenceNegativeControl is the keystone test's required
// negative control (design doc §10.2): replaying a raw byte tail from an
// offset — rather than a proper Attach repaint — must NOT reproduce the
// same grid. This is the exact ring-buffer-replay corruption spike/pty/
// NOTES.md finding #1 documented, and it is precisely what motivates the
// whole grid-equivalence approach: if this test failed to distinguish
// (i.e. the tail happened to match), the keystone test above would be
// proving nothing.
//
// testdata/pty/claude-tui-reattach-tail.raw is the last 8192 bytes of the
// same M0 capture (spike/pty/claude-tui-capture.txt), which begins
// mid-multibyte-UTF-8-character — exactly the kind of truncated fragment a
// naive ring-buffer replay would hand a reattaching client.
func TestGridEquivalenceNegativeControl(t *testing.T) {
	fullCapture := readTestdata(t, "../../spike/pty/claude-tui-capture.txt")
	tail := readTestdata(t, "../../testdata/pty/claude-tui-reattach-tail.raw")

	if len(tail) == 0 {
		t.Fatal("tail fixture is empty")
	}
	// Confirm the fixture is in fact a raw offset-cut, not an escape
	// sequence boundary, and mid-multibyte-UTF8 as spike finding #1
	// documents (first byte 0x80 is a UTF-8 continuation byte, never a
	// valid sequence start).
	if tail[0] != 0x80 {
		t.Fatalf("expected the negative-control fixture to start mid-multibyte-UTF8 (0x80), got %#x", tail[0])
	}

	done := make(chan struct{})
	var gridReference, gridRawTail Grid

	go func() {
		defer close(done)

		reference := New(captureRows, captureCols, discardLogger())
		defer reference.Close()
		reference.Feed(fullCapture)
		gridReference = reference.DebugGrid()

		raw := New(captureRows, captureCols, discardLogger())
		defer raw.Close()
		raw.Feed(tail)
		gridRawTail = raw.DebugGrid()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}

	if ok, _ := gridReference.Equal(gridRawTail); ok {
		t.Fatal("raw byte-tail replay matched the reference grid — negative control failed to distinguish; " +
			"the grid-equivalence approach would not be proving anything")
	}
}

// TestAttachDetachAtomicity checks that Attach's repaint plus every byte
// fed afterward, delivered to the subscriber, reconstructs the same
// stream Feed was actually given — no byte lost or duplicated across the
// Attach seam (design doc §4's requirement on Attach's docstring).
func TestAttachDetachAtomicity(t *testing.T) {
	s := New(captureRows, captureCols, discardLogger())
	defer s.Close()

	preAttach := []byte("hello ")
	s.Feed(preAttach)

	sub := NewSubscriber()
	repaint := s.Attach(sub)
	if len(repaint) == 0 {
		t.Fatal("expected non-empty repaint")
	}

	live := []byte("world")
	s.Feed(live)

	select {
	case got := <-sub.Ch:
		if string(got) != string(live) {
			t.Fatalf("live bytes = %q, want %q", got, live)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live bytes on subscriber channel")
	}

	s.Detach(sub)
	select {
	case <-sub.Done:
	default:
		t.Fatal("Detach did not close Done")
	}
	if sub.Dropped() {
		t.Error("a clean Detach must not mark the subscriber dropped")
	}
}

// TestBackpressureNeverBlocksFeed is design doc §5.4: a subscriber that
// never reads its channel must not block Feed, and overflow marks it
// dropped. No timers or sleeps are needed — Feed is called synchronously
// far past the channel's capacity, and if Feed ever blocked, this test
// itself would hang (caught by `go test`'s own timeout, not a local one).
func TestBackpressureNeverBlocksFeed(t *testing.T) {
	s := New(captureRows, captureCols, discardLogger())
	defer s.Close()

	sub := NewSubscriber()
	s.Attach(sub) // never drains sub.Ch after this

	const n = subscriberChanCap + 50
	for i := 0; i < n; i++ {
		s.Feed([]byte("x"))
	}

	if !sub.Dropped() {
		t.Fatal("expected subscriber to be marked dropped after overflowing its channel")
	}
	select {
	case <-sub.Done:
	default:
		t.Fatal("expected Done to be closed after an overflow drop")
	}
	if len(sub.Ch) != subscriberChanCap {
		t.Fatalf("channel holds %d items, want exactly cap %d", len(sub.Ch), subscriberChanCap)
	}

	// A dropped subscriber must have been removed from the broadcast set:
	// Detach afterward must not panic (idempotent close), and feeding more
	// must not touch its channel again.
	s.Detach(sub)
	before := len(sub.Ch)
	s.Feed([]byte("y"))
	if len(sub.Ch) != before {
		t.Fatal("Feed after overflow+Detach should not still be delivering to this subscriber")
	}
}

// TestSizeAndResize checks the (rows,cols)<->(width,height) conversion at
// the Screen<->vt.Emulator boundary is not swapped.
func TestSizeAndResize(t *testing.T) {
	s := New(10, 30, discardLogger())
	defer s.Close()

	rows, cols := s.Size()
	if rows != 10 || cols != 30 {
		t.Fatalf("Size() = (%d,%d), want (10,30)", rows, cols)
	}

	s.Resize(12, 40)
	rows, cols = s.Size()
	if rows != 12 || cols != 40 {
		t.Fatalf("after Resize, Size() = (%d,%d), want (12,40)", rows, cols)
	}
}

// TestCloseUnblocksPumpReplies exercises the resolution documented in
// screen.go's Close doc comment: closing the emulator's InputPipe (rather
// than calling vt.Emulator.Close, which races with pumpReplies' permanent
// Read) must (1) let a device query answered concurrently with Close still
// be fed without deadlocking, (2) be idempotent, and (3) leave Feed safe to
// call afterward (its own emu.Write may itself try to write a reply into
// the now-closed pipe; that must not block or panic). This test is only
// meaningful under `-race`.
func TestCloseUnblocksPumpReplies(t *testing.T) {
	s := New(captureRows, captureCols, discardLogger())

	// A real device query (`ESC[6n`), same family as the one in the
	// keystone fixture, so pumpReplies actually has something in flight
	// around the time Close runs.
	s.Feed([]byte("\x1b[6n"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Close()
		s.Close() // idempotent: must not panic or double-close the pipe
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return — possible deadlock")
	}

	// Feed after Close must not block or panic, even though the emulator
	// may attempt to write another reply into the now-closed input pipe.
	closedFeedDone := make(chan struct{})
	go func() {
		defer close(closedFeedDone)
		s.Feed([]byte("\x1b[6n"))
	}()
	select {
	case <-closedFeedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Feed after Close did not return — possible deadlock")
	}
}
