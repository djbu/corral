package redact

import (
	"strings"
	"testing"
)

// TestRedact_Rules is table-driven: one positive (the secret must be
// replaced, exactly one marker for the named rule) and one negative
// (superficially similar but non-matching text must survive untouched) per
// rule in the v1 table (design doc §3.4).
func TestRedact_Rules(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantRule   string // "" for negative cases: no marker at all
		wantSecret string // substring that must be gone from the output when wantRule != ""
	}{
		{
			name:       "anthropic_key positive",
			input:      "ANTHROPIC_API_KEY=sk-ant-abcdefghijklmnopqrstuvwxyz0123456789",
			wantRule:   "anthropic_key",
			wantSecret: "sk-ant-abcdefghijklmnopqrstuvwxyz0123456789",
		},
		{
			name:     "anthropic_key negative (too short)",
			input:    "prefix sk-ant-short suffix",
			wantRule: "",
		},
		{
			name:       "openai_key positive",
			input:      "export OPENAI_KEY=sk-abcdefghijklmnopqrstuvwxyz0123456789ABCD",
			wantRule:   "openai_key",
			wantSecret: "sk-abcdefghijklmnopqrstuvwxyz0123456789ABCD",
		},
		{
			name:     "openai_key negative (too short)",
			input:    "sk-tooshort",
			wantRule: "",
		},
		{
			name:       "github_token positive",
			input:      "token: ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD",
			wantRule:   "github_token",
			wantSecret: "ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD",
		},
		{
			name:     "github_token negative (bad prefix)",
			input:    "ghz_abcdefghijklmnopqrstuvwxyz0123456789ABCD",
			wantRule: "",
		},
		{
			name:       "aws_access_key positive",
			input:      "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
			wantRule:   "aws_access_key",
			wantSecret: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:     "aws_access_key negative (too short)",
			input:    "AKIASHORT",
			wantRule: "",
		},
		{
			name:       "slack_token positive",
			input:      "SLACK_TOKEN=xoxb-1234567890-abcdefghij",
			wantRule:   "slack_token",
			wantSecret: "xoxb-1234567890-abcdefghij",
		},
		{
			name:     "slack_token negative (bad prefix)",
			input:    "xoxz-1234567890-abcdefghij",
			wantRule: "",
		},
		{
			name:       "private_key_block positive",
			input:      "-----BEGIN RSA PRIVATE KEY-----\nMIIB...\n-----END RSA PRIVATE KEY-----",
			wantRule:   "private_key_block",
			wantSecret: "-----BEGIN RSA PRIVATE KEY-----",
		},
		{
			name:     "private_key_block negative (public key)",
			input:    "-----BEGIN PUBLIC KEY-----\nMIIB...\n-----END PUBLIC KEY-----",
			wantRule: "",
		},
		{
			name:       "assignment positive",
			input:      `password = "hunter2-supersecretvalue"`,
			wantRule:   "assignment",
			wantSecret: "hunter2-supersecretvalue",
		},
		{
			name:     "assignment negative (value too short)",
			input:    `password = "short"`,
			wantRule: "",
		},
		{
			name:       "jwt positive",
			input:      "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PYb4LddvzYWU",
			wantRule:   "jwt",
			wantSecret: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PYb4LddvzYWU",
		},
		{
			name:     "jwt negative (not three segments)",
			input:    "eyJhbGciOiJIUzI1NiJ9.notenoughsegments",
			wantRule: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, markers := Redact([]byte(tt.input))

			if tt.wantRule == "" {
				if len(markers) != 0 {
					t.Fatalf("Redact(%q) markers = %+v, want none", tt.input, markers)
				}
				if string(out) != tt.input {
					t.Fatalf("Redact(%q) = %q, want unchanged", tt.input, out)
				}
				return
			}

			if len(markers) != 1 {
				t.Fatalf("Redact(%q) markers = %+v, want exactly 1", tt.input, markers)
			}
			m := markers[0]
			if m.Rule != tt.wantRule {
				t.Fatalf("marker.Rule = %q, want %q", m.Rule, tt.wantRule)
			}
			if m.Offset < 0 || m.Offset+m.Len > len(out) {
				t.Fatalf("marker offset/len out of range: %+v, len(out)=%d", m, len(out))
			}
			gotMarkerText := string(out[m.Offset : m.Offset+m.Len])
			wantMarkerText := markerFor(tt.wantRule)
			if gotMarkerText != wantMarkerText {
				t.Fatalf("out[offset:offset+len] = %q, want %q", gotMarkerText, wantMarkerText)
			}
			if strings.Contains(string(out), tt.wantSecret) {
				t.Fatalf("Redact(%q) = %q, still contains secret %q", tt.input, out, tt.wantSecret)
			}
		})
	}
}

// TestRedact_Idempotent asserts Redact(Redact(x)) == Redact(x) for every
// positive case above, run once over the whole set concatenated together
// so overlaps between rules get exercised too.
func TestRedact_Idempotent(t *testing.T) {
	input := strings.Join([]string{
		"ANTHROPIC_API_KEY=sk-ant-abcdefghijklmnopqrstuvwxyz0123456789",
		"OPENAI_KEY=sk-abcdefghijklmnopqrstuvwxyz0123456789ABCD",
		"token: ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD",
		"AKIAIOSFODNN7EXAMPLE",
		"xoxb-1234567890-abcdefghij",
		"-----BEGIN RSA PRIVATE KEY-----",
		`password = "hunter2-supersecretvalue"`,
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PYb4LddvzYWU",
	}, "\n")

	once, _ := Redact([]byte(input))
	twice, _ := Redact(once)
	if string(once) != string(twice) {
		t.Fatalf("Redact is not idempotent:\nonce:  %q\ntwice: %q", once, twice)
	}
}

// TestRedact_NoSecretsReturnsCopy asserts a clean input round-trips
// byte-for-byte with zero markers.
func TestRedact_NoSecretsReturnsCopy(t *testing.T) {
	input := "just some ordinary tool output, nothing to see here"
	out, markers := Redact([]byte(input))
	if len(markers) != 0 {
		t.Fatalf("markers = %+v, want none", markers)
	}
	if string(out) != input {
		t.Fatalf("out = %q, want unchanged input", out)
	}
}

// TestRedact_OverlappingRulesPreferEarlierRule: "token=ghp_..." matches
// both github_token (the value alone) and assignment (the whole
// "token=value" span). github_token is earlier in the table, so it should
// win and assignment's overlapping match should be dropped, leaving one
// marker, not two, and not a marker that also swallows the literal "token=".
func TestRedact_OverlappingRulesPreferEarlierRule(t *testing.T) {
	input := "token=ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	out, markers := Redact([]byte(input))
	if len(markers) != 1 {
		t.Fatalf("markers = %+v, want exactly 1 (overlap must resolve to one)", markers)
	}
	if markers[0].Rule != "github_token" {
		t.Fatalf("markers[0].Rule = %q, want github_token (earlier in the table than assignment)", markers[0].Rule)
	}
	if !strings.HasPrefix(string(out), "token=«redacted:github_token»") {
		t.Fatalf("out = %q, want the literal \"token=\" preserved", out)
	}
}
