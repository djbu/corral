package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// disableEcho turns off the tty line discipline's local echo on fd
// (fakeclaude's own stdin), mirroring what real claude does when it
// takes over the terminal for its interactive TUI: without this, the
// PTY's cooked-mode line discipline echoes every typed byte back to the
// output stream itself, before fakeclaude's own cursor-addressed writes
// — polluting captured output with the raw keystrokes in addition to
// fakeclaude's differential rendering of them. It intentionally leaves
// every other termios flag (ICANON in particular) untouched: this is
// the minimal change that fixes the echo-duplication problem without
// altering how stdin is otherwise buffered.
//
// If fd is not a real tty (e.g. running under `go test` directly with
// stdin redirected from /dev/null, or piped), IoctlGetTermios fails and
// disableEcho is a deliberate no-op: fakeclaude still runs, it simply
// has nothing to echo-suppress.
func disableEcho(fd int) error {
	t, err := unix.IoctlGetTermios(fd, getTermiosReq)
	if err != nil {
		return nil //nolint:nilerr // not a tty; nothing to do, not a fatal condition.
	}
	raw := *t
	raw.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, setTermiosReq, &raw); err != nil {
		return fmt.Errorf("fakeclaude: disable tty echo: %w", err)
	}
	return nil
}
