package redact

import (
	"strings"
	"testing"
)

// FuzzRedact checks properties that must hold for any input, not just the
// fixed positive/negative cases in redact_test.go (design doc §11):
//
//   - never panics
//   - every Marker's [Offset, Offset+Len) lies within the returned output
//   - output[Offset:Offset+Len] is exactly the "«redacted:rule»" marker
//     text for that marker's Rule
//   - Redact is idempotent: Redact(Redact(x)) == Redact(x)
//
// It deliberately does not assert "no seeded secret survives in the
// output" as a general fuzz invariant — the mutator routinely produces
// near-miss strings that legitimately don't match any rule, so that
// property is only meaningful against the fixed corpus in
// TestRedact_Rules, not as something every mutated input must satisfy.
func FuzzRedact(f *testing.F) {
	seeds := []string{
		"",
		"nothing to see here",
		"ANTHROPIC_API_KEY=sk-ant-abcdefghijklmnopqrstuvwxyz0123456789",
		"export OPENAI_KEY=sk-abcdefghijklmnopqrstuvwxyz0123456789ABCD",
		"token: ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD",
		"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
		"SLACK_TOKEN=xoxb-1234567890-abcdefghij",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIB...\n-----END RSA PRIVATE KEY-----",
		`password = "hunter2-supersecretvalue"`,
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PYb4LddvzYWU",
		"token=ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD",
		"«redacted:anthropic_key»",               // already-redacted marker text: must be inert
		strings.Repeat("sk-ant-", 5000) + "AAAA", // pathological backtracking bait
		"\x00\x01\xff\xfe not valid utf-8 \xc0\xaf",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		input := []byte(s)
		out, markers := Redact(input)

		for _, m := range markers {
			if m.Offset < 0 || m.Len < 0 || m.Offset+m.Len > len(out) {
				t.Fatalf("marker out of range: %+v, len(out)=%d, input=%q", m, len(out), s)
			}
			want := markerFor(m.Rule)
			got := string(out[m.Offset : m.Offset+m.Len])
			if got != want {
				t.Fatalf("out[offset:offset+len] = %q, want %q (marker=%+v, input=%q)", got, want, m, s)
			}
		}

		twice, _ := Redact(out)
		if string(twice) != string(out) {
			t.Fatalf("Redact not idempotent: Redact(x)=%q, Redact(Redact(x))=%q, input=%q", out, twice, s)
		}
	})
}
