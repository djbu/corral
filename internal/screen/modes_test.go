package screen

import (
	"reflect"
	"testing"
)

// feedAll runs sn.feed over each chunk in order, accumulating a
// last-write-wins map[int]bool exactly like Screen.Feed does, and returns
// the final map.
func feedAll(sn *sniffer, chunks ...[]byte) map[int]bool {
	got := make(map[int]bool)
	for _, c := range chunks {
		sn.feed(c, func(mode int, enabled bool) {
			got[mode] = enabled
		})
	}
	return got
}

func TestSnifferSingleMode(t *testing.T) {
	sn := &sniffer{}
	got := feedAll(sn, []byte("\x1b[?1049h"))
	want := map[int]bool{1049: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSnifferMultiParam(t *testing.T) {
	sn := &sniffer{}
	got := feedAll(sn, []byte("\x1b[?1000;1002;1003;1006h"))
	want := map[int]bool{1000: true, 1002: true, 1003: true, 1006: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSnifferReset(t *testing.T) {
	sn := &sniffer{}
	got := feedAll(sn, []byte("\x1b[?25h\x1b[?25l"))
	want := map[int]bool{25: false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSnifferUnknownMode2031(t *testing.T) {
	// This is the mode x/vt's own Callbacks.EnableMode does not
	// recognize (design doc §4) — the whole reason this sniffer exists.
	sn := &sniffer{}
	got := feedAll(sn, []byte("\x1b[?2031h"))
	want := map[int]bool{2031: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSnifferLastWriteWins(t *testing.T) {
	sn := &sniffer{}
	got := feedAll(sn, []byte("\x1b[?1000h\x1b[?1000l\x1b[?1000h"))
	want := map[int]bool{1000: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSnifferSplitAcrossChunkBoundary(t *testing.T) {
	full := "\x1b[?1000;1002h"
	for split := 0; split <= len(full); split++ {
		sn := &sniffer{}
		got := feedAll(sn, []byte(full[:split]), []byte(full[split:]))
		want := map[int]bool{1000: true, 1002: true}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split at %d: got %v, want %v", split, got, want)
		}
	}
}

func TestSnifferSplitByteByByte(t *testing.T) {
	full := []byte("\x1b[?1049h\x1b[2J\x1b[H\x1b[?1000;1002;1003;1006h\x1b[?25l\x1b[?2004h\x1b[?1004h\x1b[?2031h")
	sn := &sniffer{}
	got := make(map[int]bool)
	for _, b := range full {
		sn.feed([]byte{b}, func(mode int, enabled bool) {
			got[mode] = enabled
		})
	}
	want := map[int]bool{
		1049: true,
		1000: true,
		1002: true,
		1003: true,
		1006: true,
		25:   false,
		2004: true,
		1004: true,
		2031: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSnifferIgnoresNonPrivateCSI(t *testing.T) {
	sn := &sniffer{}
	// SGR (no '?'), cursor movement, and an erase-in-display sequence, none
	// of which are private mode set/reset — none should register.
	got := feedAll(sn, []byte("\x1b[38;2;215;119;87m\x1b[10;20H\x1b[2J"))
	if len(got) != 0 {
		t.Fatalf("expected no modes recorded, got %v", got)
	}
}

func TestSnifferPrivateModeWithNonHLFinalByte(t *testing.T) {
	sn := &sniffer{}
	// "CSI ? 1 r" is not a set/reset; must not be mistaken for one, and
	// must not corrupt parsing of what follows.
	got := feedAll(sn, []byte("\x1b[?1r\x1b[?1000h"))
	want := map[int]bool{1000: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSnifferRealCaptureModeSet(t *testing.T) {
	// The exact mode set design doc §4 states claude's startup produces.
	sn := &sniffer{}
	got := feedAll(sn, []byte("\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h\x1b[?25l\x1b[?2004h\x1b[?1004h\x1b[?2031h"))
	want := map[int]bool{
		1000: true,
		1002: true,
		1003: true,
		1006: true,
		25:   false,
		2004: true,
		1004: true,
		2031: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
