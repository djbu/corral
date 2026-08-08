// Package answer holds the canonical encoder that turns an answer request
// (free text or a named key) into the exact bytes written to a session's PTY
// master (design doc §7.2).
//
// It lives in its own leaf package — importing nothing but the standard
// library — so every delivery path can share one implementation:
//   - the HTTP handler (internal/api, `corral answer`), and
//   - the ntfy reply subscriber (internal/notify, §8.6).
//
// Both are security-critical: the text is treated as opaque data and is never
// shell-interpolated or otherwise parsed. Having a single encoder means the
// bracketed-paste framing and key table cannot silently drift between the two
// remote-input surfaces. The byte-exact test lives here (answer_test.go)
// beside the code it guards, not beside a shim.
package answer

import "fmt"

// MaxBytes caps an answer's text so a runaway or hostile caller can't wedge a
// session's PTY (or the daemon's memory) with an unbounded write. Every
// delivery path must enforce this before calling Encode.
const MaxBytes = 64 << 10 // 65536

// Keys is the fixed lookup table for `corral answer --key` (design doc §7.2).
// Unknown keys are a caller error, not silently ignored.
var Keys = map[string]string{
	"enter":  "\r",
	"esc":    "\x1b",
	"up":     "\x1b[A",
	"down":   "\x1b[B",
	"tab":    "\t",
	"ctrl-c": "\x03",
}

// Encode turns an answer request into the exact bytes to write to a session's
// PTY master, in a single []byte so the caller can deliver them in one
// PTYMaster.Write call (design doc §7.2). Exactly one of text/key is expected
// to be non-empty; that invariant is enforced by the caller, not here.
//
// text is treated as opaque data, never shell-interpolated or otherwise
// parsed: the returned bytes are text's literal UTF-8 bytes (or, for
// multi-line text, text wrapped in a bracketed-paste envelope), optionally
// followed by a trailing "\r". newline is ignored when key is set.
func Encode(text, key string, newline bool) ([]byte, error) {
	if key != "" {
		seq, ok := Keys[key]
		if !ok {
			return nil, fmt.Errorf("unknown key %q", key)
		}
		return []byte(seq), nil
	}

	var b []byte
	if containsNewline(text) {
		b = append(b, "\x1b[200~"...)
		b = append(b, text...)
		b = append(b, "\x1b[201~"...)
	} else {
		b = append(b, text...)
	}
	if newline {
		b = append(b, '\r')
	}
	return b, nil
}

func containsNewline(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return true
		}
	}
	return false
}
