package screen

// sniffer is an independent, library-agnostic scanner for DEC private mode
// sequences: `CSI ? <params> h` (set) and `CSI ? <params> l` (reset),
// including the multi-param form `CSI ?1000;1002h`. It exists because
// x/vt's own Callbacks.EnableMode/DisableMode only fire for modes the
// library's ansi.Mode type recognizes — and it does not recognize mode
// 2031, which claude sets on startup (design doc §4). The sniffer tracks
// its own last-write-wins map[int]bool, populated via feed's callback, so
// it never depends on the emulator's opinion of what a "known" mode is.
//
// feed is safe to call repeatedly with successive chunks of a byte stream:
// all parser state (which escape-sequence prefix has been seen so far, and
// any partially-accumulated parameter digits) is carried in the sniffer
// itself, so a sequence split across two Feed calls — e.g. one call ending
// right after "\x1b[?1000" and the next starting with ";1002h" — is still
// recognized correctly.
type sniffer struct {
	st     parseState
	params []int
	cur    int
}

type parseState int

const (
	stGround  parseState = iota
	stEsc                // saw ESC
	stCsi                // saw ESC [
	stPrivate            // saw ESC [ ?, accumulating params up to a final h/l (or other) byte
)

// feed scans p and calls set(mode, enabled) once for every parameter of
// every complete `CSI ? ... h|l` sequence found — enabled is true for 'h'
// (set), false for 'l' (reset). Any other CSI sequence (no leading '?', or
// a final byte other than h/l) is recognized only far enough to be skipped
// correctly; its content is never interpreted.
func (sn *sniffer) feed(p []byte, set func(mode int, enabled bool)) {
	for _, b := range p {
		switch sn.st {
		case stGround:
			if b == 0x1b {
				sn.st = stEsc
			}

		case stEsc:
			switch {
			case b == '[':
				sn.st = stCsi
			case b == 0x1b:
				// A run of ESCs; stay put and keep waiting for '['.
			default:
				sn.st = stGround
			}

		case stCsi:
			switch {
			case b == '?':
				sn.st = stPrivate
				sn.params = sn.params[:0]
				sn.cur = 0
			case b >= 0x40 && b <= 0x7e:
				// A non-private CSI sequence (e.g. SGR, cursor movement)
				// terminated without ever seeing '?'. Not ours; done.
				sn.st = stGround
			}
			// Any other byte here (e.g. another intermediate before the
			// parameter bytes) is tolerated by staying in stCsi.

		case stPrivate:
			switch {
			case b >= '0' && b <= '9':
				sn.cur = sn.cur*10 + int(b-'0')
			case b == ';':
				sn.params = append(sn.params, sn.cur)
				sn.cur = 0
			case b == 'h' || b == 'l':
				sn.params = append(sn.params, sn.cur)
				enabled := b == 'h'
				for _, m := range sn.params {
					set(m, enabled)
				}
				sn.st = stGround
			case b >= 0x40 && b <= 0x7e:
				// A private-mode sequence with a final byte other than
				// h/l (e.g. 'r' — restore). Not a set/reset; done.
				sn.st = stGround
			default:
				// Unexpected byte inside what looked like a parameter
				// list (not digit/';'/final byte) — bail out defensively
				// rather than mis-parse.
				sn.st = stGround
			}
		}
	}
}
