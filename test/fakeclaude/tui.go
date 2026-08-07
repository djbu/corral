package main

import (
	"fmt"
	"io"
)

// startupSequence is design doc §10.1 item 3's exact literal, byte for
// byte: the private-mode set the M0 capture recorded real claude
// entering on startup. Matching it exactly (not just "some alt-screen
// sequence") is what lets internal/screen's mode sniffer — already
// proven against the real M0 capture in step 5's keystone test — see the
// identical mode transitions when fed fakeclaude's output instead.
const startupSequence = "\x1b[?1049h" + // enter alt screen
	"\x1b[2J" + // clear screen
	"\x1b[H" + // cursor home
	"\x1b[?1000h" + // mouse: X10 tracking
	"\x1b[?1002h" + // mouse: button-event tracking
	"\x1b[?1003h" + // mouse: any-event tracking
	"\x1b[?1006h" + // mouse: SGR extended coordinates
	"\x1b[?25l" + // hide cursor
	"\x1b[?2004h" + // bracketed paste
	"\x1b[?1004h" + // focus reporting
	"\x1b[?2031h" // reserved/unknown mode the sniffer must tolerate.

// altScreenExit undoes startupSequence's alt-screen/cursor-visibility
// effects on a graceful exit, leaving a real terminal (or a human
// watching fakeclaude directly, outside any corral emulator) in a sane
// state.
const altScreenExit = "\x1b[?25h" + "\x1b[?1049l"

const bannerTitle = "fakeclaude"

// writeStartup writes the full startup sequence to w: the private-mode
// set, an OSC 0 window title, and a truecolor box-drawn banner (design
// doc §10.1 item 3). name is shown in the banner so a human — or a test
// — reading captured output from two fakeclaude instances can tell them
// apart.
func writeStartup(w io.Writer, name string) error {
	if _, err := io.WriteString(w, startupSequence); err != nil {
		return fmt.Errorf("fakeclaude: write startup sequence: %w", err)
	}
	if _, err := fmt.Fprintf(w, "\x1b]0;%s\x07", bannerTitle); err != nil {
		return fmt.Errorf("fakeclaude: write window title: %w", err)
	}
	return writeBanner(w, name)
}

// bannerWidth is fixed in v0 rather than derived from the real terminal
// size. Design doc §10.4's TestResizePropagates (step 10) is the first
// place a fakeclaude banner needs to track an actual SIGWINCH resize —
// that is out of scope for v0's six numbered requirements (§10.1 lists
// none) and is left as an explicit note for whoever implements step 10.
const bannerWidth = 60

// writeBanner draws a fixed-width, truecolor (38;2;r;g;b) box at rows
// 1-3 of the alt screen using cursor-addressed writes.
func writeBanner(w io.Writer, name string) error {
	const r, g, b = 120, 60, 220 // an arbitrary, fixed truecolor purple.
	color := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
	const reset = "\x1b[0m"

	top := "╭" + repeatStr("─", bannerWidth-2) + "╮"
	label := fmt.Sprintf(" %s (%s) ", bannerTitle, name)
	mid := "│" + padCenter(label, bannerWidth-2) + "│"
	bot := "╰" + repeatStr("─", bannerWidth-2) + "╯"

	_, err := fmt.Fprintf(w, "\x1b[1;1H%s%s%s\x1b[2;1H%s%s%s\x1b[3;1H%s%s%s",
		color, top, reset,
		color, mid, reset,
		color, bot, reset,
	)
	if err != nil {
		return fmt.Errorf("fakeclaude: write banner: %w", err)
	}
	return nil
}

func repeatStr(s string, n int) string {
	if n < 0 {
		n = 0
	}
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// padCenter pads s with spaces to width, assuming s is ASCII (true for
// every caller in this file: fixed English labels plus a session name
// that corral itself constrains to non-control ASCII in
// supervisor.validateName).
func padCenter(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}
	left := (width - len(s)) / 2
	right := width - len(s) - left
	return repeatStr(" ", left) + s + repeatStr(" ", right)
}

// transcriptStartRow is the first row of the alt screen's transcript
// region: below the 3-row banner, with one blank separator row.
const transcriptStartRow = 5

// writeTranscriptLine renders one line of transcript content at the
// given 1-based row using a cursor-addressed differential update
// (design doc §10.1 item 3: "never append-only output"): move the
// cursor to the row, clear it (so a shorter new line never leaves stale
// characters from whatever was previously drawn at that row), then write
// the text.
func writeTranscriptLine(w io.Writer, row int, text string) error {
	if _, err := fmt.Fprintf(w, "\x1b[%d;1H\x1b[2K%s", row, text); err != nil {
		return fmt.Errorf("fakeclaude: write transcript line: %w", err)
	}
	return nil
}
