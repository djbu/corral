package hookrelay

import "encoding/json"

// Response is the daemon's 200 response envelope for POST /v1/hooks/events
// (design doc §2.5). Decision, when non-null, is a Claude Code hook
// JSON-output object that the relay prints verbatim to stdout; in M2 the
// daemon always responds with {"decision": null} and the relay always
// prints nothing. The seam exists so a future milestone's auto-answer
// policy engine is a daemon-side change with zero relay-side changes.
type Response struct {
	Decision json.RawMessage `json:"decision"`
}

// HasDecision reports whether Decision carries an actual value the relay
// should print, as opposed to being absent or JSON null. Both "the decision
// key was never present" (Decision is nil) and "the decision key was
// present with value null" (Decision holds the four bytes "null") mean the
// same thing on this wire: nothing to print.
func (r Response) HasDecision() bool {
	if len(r.Decision) == 0 {
		return false
	}
	return string(r.Decision) != "null"
}
