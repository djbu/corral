# ADR-0002: Headless terminal emulator — `charmbracelet/x/vt`

Status: accepted · Date: 2026-08-07

## Context

A session's PTY output must be reproducible for a client attaching after the
fact: design doc §4's whole reattach model is "repaint the grid, then stream
raw bytes live." That requires a headless VT100/xterm-class emulator that
tracks the full grid (cells, styles, cursor, alt-screen, modes) from a raw
byte stream, without any real terminal attached. `claude`'s TUI is an
alt-screen, truecolor, mouse-tracking, differential-update application (see
`spike/pty/claude-tui-capture.txt`), so the emulator must handle 24-bit SGR
color and DEC private modes faithfully — a naive ring-buffer replay of raw
bytes was shown by `spike/pty/NOTES.md` finding #1 to corrupt such a TUI on
reattach.

Candidates evaluated:

- **`hinshun/vt10x`** — rejected. `Glyph.FG/BG` is `Color uint32` covering
  only ANSI-16 and xterm-256; every truecolor cell (`ESC[38;2;r;g;bm`,
  which appears throughout the capture) would be flattened on repaint. No
  bracketed-paste mode support. Frozen since March 2022.
- **`charmbracelet/x/vt`** — chosen. Truecolor cells via `uv.Cell`/
  `uv.Style`; `Render() string` produces an already-ANSI-encoded styled
  snapshot; `IsAltScreen()`; `Resize(w,h)`; and — decisively —
  `Emulator.Read()` generates the device-query replies (CPR/DA/DSR) a real
  terminal would send back to the child, closing an unknown the spike
  explicitly flagged (unanswered queries with nobody attached).
- **`gitpod-io/xterm-go`** — not chosen; more spec-complete on paper, but
  heavier, and it has no ANSI-render path, so a hand-rolled painter would be
  needed regardless of which emulator won.

## Decision

`github.com/charmbracelet/x/vt`, pinned to the exact pseudo-version
`v0.0.0-20260803091719-3755ebad01b1` (pulling in
`github.com/charmbracelet/ultraviolet v0.0.0-20260303162955-0b88c25f3fff` as
its core buffer/cell/style layer). Both modules are untagged — there is no
release to pin to instead. `internal/screen.Screen` wraps the emulator
rather than exposing it, so a future swap only touches one package.

## Risk and mitigation

`x/vt` has no tagged release; pinning an exact pseudo-version makes upgrades
a deliberate, manual `go get module@<new-pseudo-version>` act, never an
implicit bump. The mitigation is the grid-equivalence test
(`internal/screen/screen_test.go`'s `TestGridEquivalenceKeystone`): it feeds
a real `claude` startup capture into one emulator, takes `Screen.Attach`'s
repaint bytes, feeds *those* into a second, independently-constructed
emulator, and asserts the two grids are cell-by-cell identical — cursor,
styles, alt-screen flag, and the independently-sniffed private-mode set all
included. A version bump that silently changes rendering, cell semantics, or
mode handling fails this test before it fails anything else. It is paired
with a negative control (`TestGridEquivalenceNegativeControl`) that replays
a raw, mid-escape-sequence byte tail instead of a proper repaint and asserts
that does *not* match — proving the test can actually tell the difference,
not just pass vacuously.

## Deviations from design doc §4's assumed API, found while implementing

- **No direct `Cursor()` accessor.** The design assumed
  `Emulator.Cursor()` returning position + visibility + style. The real
  API exposes only `CursorPosition() uv.Position` publicly; the full
  `Cursor` struct (with `Hidden bool`) lives on an unexported `vt.Screen`
  field reachable only from inside package `vt`. `internal/screen.Screen`
  gets cursor visibility from `Callbacks.CursorVisibility` instead, seeded
  to `true` at construction (the emulator's own default before any mode
  change) and updated whenever the callback fires.
- **`IsAltScreen()` is a working direct getter**, so alt-screen state does
  not need `Callbacks.AltScreen` tracking as design doc §4's callback list
  implied — `Screen` calls it directly at snapshot/DebugGrid time instead
  of caching a callback-fed field. Fewer moving parts, same result.
- **`Emulator.Close()` and `Emulator.Read()` share an unsynchronized
  internal `closed` bool.** Calling `Close` from one goroutine while
  another is in `Read` (exactly the shape `Screen`'s permanent
  reply-drain goroutine needs) is a genuine data race, caught by
  `go test -race` during this implementation. `internal/screen.Screen`
  does not call `Emulator.Close()` for this reason. Instead, `Screen.Close`
  closes the `*io.PipeWriter` returned by `Emulator.InputPipe()` — the
  same object the CSI/DSR handlers write query replies into, and the
  reader side pumpReplies parks on. `io.Pipe` explicitly documents Read
  and Close as safe to call concurrently: closing the writer makes the
  reader's blocked `Read` return `(0, io.EOF)`, and any reply write racing
  after Close gets `io.ErrClosedPipe` (already discarded by `Feed`)
  instead of blocking. Verified with a standalone `-race` probe
  (`e.InputPipe().(interface{ Close() error })`, concurrent Read/Write/
  Close) before wiring it into `Screen.Close`, and covered here by
  `TestCloseUnblocksPumpReplies`. `Close` is idempotent via `sync.Once`
  and no-ops with a log warning if a future `x/vt` version changes
  `InputPipe`'s concrete type away from `*io.PipeWriter`.
- **`Emulator.Read()` is load-bearing, not theoretical.** The device-query
  reply path this ADR credited as "closing an unknown" is exercised by the
  keystone test's own fixture: `testdata/pty/claude-tui-startup.raw`
  contains a real `ESC[?6n` from claude's own startup sequence. Answering
  it requires a goroutine draining `Emulator.Read` for the emulator's
  entire lifetime, independent of whether anything ever calls
  `Screen.Replies()` — without it, `Screen.Feed` deadlocks on that exact
  byte sequence, confirmed by reading `x/vt`'s CSI/DSR handlers (they write
  the reply synchronously, via a blocking `io.Pipe`, from inside the same
  call that `Feed` makes into `Emulator.Write`).

## Revisit trigger

Any `x/vt`/`ultraviolet` version bump must re-run
`TestGridEquivalenceKeystone` before merging; a failure there is the
signal design doc §4 names to replace the `Render()`-based repaint with a
hand-rolled `CellAt` painter — that test, not intuition, is the arbiter. No
pivot was needed for this pinned version: `Render()` passed on the first
run against the real M0 capture.
