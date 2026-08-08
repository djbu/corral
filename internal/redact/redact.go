// Package redact scans arbitrary agent-supplied bytes for secret-shaped
// substrings and replaces them with an in-place marker before anything
// persists them (design doc §3.4, RUNBOOK §7): "Event ingest runs a secret
// scan and redacts before persisting — the event store is append-only and
// long-lived; storing leaked credentials forever is a liability corral must
// not create. Redactions are marked in place, never silent."
//
// M2 is the first milestone that persists agent-supplied content
// (tool_input for a Bash call is arbitrary text and routinely contains
// `export TOKEN=...`), so Redact is called on RawRedacted before
// AppendEvent, and on every string that reaches a BlockedReason or a
// notification body. It is deliberately not applied to the in-memory
// payload used for state-machine transition logic — redaction is a
// persistence/egress concern, not a semantics concern.
package redact

import (
	"fmt"
	"regexp"
	"sort"
)

// Marker records one redaction. Offset and Len describe the marker text's
// own position in Redact's *output* (not the original input): b[Offset:
// Offset+Len] of the returned []byte is exactly the marker string
// "«redacted:Rule»". Output offsets, not input offsets, are what's useful
// to an auditor reading the persisted (already-redacted) event — the
// original secret's input position is discarded along with the secret
// itself.
type Marker struct {
	Rule   string
	Offset int
	Len    int
}

// rule is one named pattern. Order matters: when two rules' matches
// overlap, the earlier-listed rule wins and the later one is dropped for
// that span (see resolveMatches) — this is what keeps `assignment`, the
// broadest/last rule, from re-claiming a span a more specific rule (e.g.
// github_token) already matched.
type rule struct {
	name string
	re   *regexp.Regexp
}

// rules is the v1 table (design doc §3.4). Each has one positive and one
// negative test case in redact_test.go.
var rules = []rule{
	{"anthropic_key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`)},
	{"openai_key", regexp.MustCompile(`sk-[A-Za-z0-9]{32,}`)},
	{"github_token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`)},
	{"aws_access_key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"slack_token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"private_key_block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"assignment", regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|passwd|bearer)\s*[=:]\s*["']?[A-Za-z0-9_\-\.]{16,}`)},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`)},
}

// match is one accepted (non-overlapping, post-priority-resolution) hit
// against the original input.
type match struct {
	start, end int // byte offsets into the original input, end exclusive
	rule       string
}

// Redact scans b for secret-shaped substrings and returns a copy with each
// one replaced by "«redacted:rule»", plus one Marker per replacement
// (sorted by output Offset ascending). Redact never panics and never
// returns an error: an unmatched or malformed input simply comes back with
// no markers. Redact(Redact(x)) always equals Redact(x) — running it twice
// is a no-op, since none of the marker text itself matches any rule.
func Redact(b []byte) ([]byte, []Marker) {
	matches := resolveMatches(b)
	if len(matches) == 0 {
		out := make([]byte, len(b))
		copy(out, b)
		return out, nil
	}

	out := make([]byte, 0, len(b))
	markers := make([]Marker, 0, len(matches))
	cursor := 0
	for _, m := range matches {
		out = append(out, b[cursor:m.start]...)
		markerText := markerFor(m.rule)
		markers = append(markers, Marker{Rule: m.rule, Offset: len(out), Len: len(markerText)})
		out = append(out, markerText...)
		cursor = m.end
	}
	out = append(out, b[cursor:]...)

	return out, markers
}

// markerFor renders the in-place marker text for rule name — always
// "«redacted:name»".
func markerFor(name string) string {
	return fmt.Sprintf("«redacted:%s»", name)
}

// resolveMatches runs every rule against b and returns the accepted,
// non-overlapping match set in left-to-right order (by start). Rules are
// tried in table order; a later rule's match is dropped whenever it
// overlaps a span an earlier rule (or an earlier match of the same rule)
// already claimed.
func resolveMatches(b []byte) []match {
	var accepted []match
	for _, r := range rules {
		for _, loc := range r.re.FindAllIndex(b, -1) {
			start, end := loc[0], loc[1]
			if overlapsAny(accepted, start, end) {
				continue
			}
			accepted = append(accepted, match{start: start, end: end, rule: r.name})
		}
	}
	sort.Slice(accepted, func(i, j int) bool { return accepted[i].start < accepted[j].start })
	return accepted
}

func overlapsAny(accepted []match, start, end int) bool {
	for _, a := range accepted {
		if start < a.end && a.start < end {
			return true
		}
	}
	return false
}
