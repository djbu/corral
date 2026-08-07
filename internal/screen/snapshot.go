package screen

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// snapshotLocked renders the exact byte sequence that reproduces the
// Screen's current visual state on a fresh terminal (design doc §4,
// "Repaint sequence"), in the exact order specified there:
//
//  1. ESC[?1049h if alt-screen is active.
//  2. ESC[2J ESC[H — clear and home.
//  3. Every sniffed private mode except 1049 and 25, re-emitted as
//     ESC[?<n>h / ESC[?<n>l, sorted for determinism.
//  4. ESC]0;<title>BEL, if a title was seen.
//  5. emu.Render()'s styled grid, one ESC[<row>;1H per line and always
//     ESC[0m at the end of each line.
//  6. ESC[<cy+1>;<cx+1>H, then ESC[?25h/l per cursor visibility.
//
// Caller must hold s.mu.
func (s *Screen) snapshotLocked() []byte {
	var buf bytes.Buffer

	if s.emu.IsAltScreen() {
		buf.WriteString("\x1b[?1049h")
	}
	buf.WriteString("\x1b[2J\x1b[H")

	modeNums := make([]int, 0, len(s.modes))
	for m := range s.modes {
		if m == 1049 || m == 25 {
			continue
		}
		modeNums = append(modeNums, m)
	}
	sort.Ints(modeNums)
	for _, m := range modeNums {
		if s.modes[m] {
			fmt.Fprintf(&buf, "\x1b[?%dh", m)
		} else {
			fmt.Fprintf(&buf, "\x1b[?%dl", m)
		}
	}

	if s.title != "" {
		fmt.Fprintf(&buf, "\x1b]0;%s\x07", s.title)
	}

	rows := s.emu.Height()
	lines := strings.Split(s.emu.Render(), "\n")
	for i := 0; i < rows; i++ {
		var line string
		if i < len(lines) {
			line = lines[i]
		}
		fmt.Fprintf(&buf, "\x1b[%d;1H%s\x1b[0m", i+1, line)
	}

	pos := s.emu.CursorPosition()
	fmt.Fprintf(&buf, "\x1b[%d;%dH", pos.Y+1, pos.X+1)
	if s.cursorVisible {
		buf.WriteString("\x1b[?25h")
	} else {
		buf.WriteString("\x1b[?25l")
	}

	return buf.Bytes()
}
