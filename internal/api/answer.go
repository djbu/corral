package api

import "fmt"

// answerKeys is the fixed lookup table for `corral answer --key` (design doc
// §7.2). Unknown keys are a caller error, not silently ignored.
var answerKeys = map[string]string{
	"enter":  "\r",
	"esc":    "\x1b",
	"up":     "\x1b[A",
	"down":   "\x1b[B",
	"tab":    "\t",
	"ctrl-c": "\x03",
}

// encodeAnswer turns an answer request into the exact bytes to write to a
// session's PTY master, in a single []byte so the caller can deliver them in
// one PTYMaster.Write call (design doc §7.2). Exactly one of text/key is
// expected to be non-empty; that invariant is enforced by the caller
// (handleAnswer), not here.
//
// text is treated as opaque data, never shell-interpolated or otherwise
// parsed: the returned bytes are text's literal UTF-8 bytes (or, for
// multi-line text, text wrapped in a bracketed-paste envelope), optionally
// followed by a trailing "\r". newline is ignored when key is set.
func encodeAnswer(text, key string, newline bool) ([]byte, error) {
	if key != "" {
		seq, ok := answerKeys[key]
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
