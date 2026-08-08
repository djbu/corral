package api

import (
	"bytes"
	"testing"
)

// TestEncodeAnswer is a byte-exact table test for encodeAnswer (design doc
// §7.2). Every case asserts on the literal bytes, not on a derived property,
// since encodeAnswer's entire job is producing an exact wire payload.
func TestEncodeAnswer(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		key     string
		newline bool
		want    []byte
		wantErr bool
	}{
		{
			name:    "single line with newline",
			text:    "1",
			newline: true,
			want:    []byte("1\r"),
		},
		{
			name:    "single line without newline",
			text:    "1",
			newline: false,
			want:    []byte("1"),
		},
		{
			name:    "multiline with newline",
			text:    "line1\nline2",
			newline: true,
			want:    []byte("\x1b[200~line1\nline2\x1b[201~\r"),
		},
		{
			name:    "multiline without newline",
			text:    "line1\nline2",
			newline: false,
			want:    []byte("\x1b[200~line1\nline2\x1b[201~"),
		},
		{
			name: "key enter",
			key:  "enter",
			want: []byte("\r"),
		},
		{
			name: "key esc",
			key:  "esc",
			want: []byte("\x1b"),
		},
		{
			name: "key up",
			key:  "up",
			want: []byte("\x1b[A"),
		},
		{
			name: "key down",
			key:  "down",
			want: []byte("\x1b[B"),
		},
		{
			name: "key tab",
			key:  "tab",
			want: []byte("\t"),
		},
		{
			name: "key ctrl-c",
			key:  "ctrl-c",
			want: []byte("\x03"),
		},
		{
			// newline must be ignored entirely when key is set.
			name:    "key ignores newline=false",
			key:     "enter",
			newline: false,
			want:    []byte("\r"),
		},
		{
			name:    "unknown key errors",
			key:     "frobnicate",
			wantErr: true,
		},
		{
			// Security case: text containing shell metacharacters must survive
			// byte-for-byte, since this path never touches a shell — a
			// naively "sanitizing" implementation would corrupt this.
			name:    "shell metacharacters pass through verbatim",
			text:    "`; rm -rf / #$(whoami)`&& echo pwned\"",
			newline: true,
			want:    []byte("`; rm -rf / #$(whoami)`&& echo pwned\"\r"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeAnswer(tc.text, tc.key, tc.newline)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("encodeAnswer(%q, %q, %v) = %q, nil; want error", tc.text, tc.key, tc.newline, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("encodeAnswer(%q, %q, %v) unexpected error: %v", tc.text, tc.key, tc.newline, err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("encodeAnswer(%q, %q, %v) = %q, want %q", tc.text, tc.key, tc.newline, got, tc.want)
			}
		})
	}
}
