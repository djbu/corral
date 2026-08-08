package main

import (
	"testing"
)

// TestPrefixMachine is design doc §5.3's four-transition table (see
// prefixMachine's doc comment in cmd_attach.go), driven byte-by-byte so
// each transition — and the "pending always expires after exactly one
// more byte" rule — is pinned down independently of cmdAttach's own
// stdin-reading loop.
func TestPrefixMachine(t *testing.T) {
	const prefix, detach byte = 0x01, 'd' // e.g. C-a as prefix, 'd' to detach.

	type step struct {
		in          byte
		wantForward []byte
		wantDetach  bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{
			name: "plain byte forwards unchanged, no state change",
			steps: []step{
				{in: 'x', wantForward: []byte{'x'}, wantDetach: false},
				{in: 'y', wantForward: []byte{'y'}, wantDetach: false},
			},
		},
		{
			name: "prefix then detach byte detaches, forwards nothing",
			steps: []step{
				{in: prefix, wantForward: nil, wantDetach: false},
				{in: detach, wantForward: nil, wantDetach: true},
			},
		},
		{
			name: "prefix then prefix forwards one literal prefix byte",
			steps: []step{
				{in: prefix, wantForward: nil, wantDetach: false},
				{in: prefix, wantForward: []byte{prefix}, wantDetach: false},
			},
		},
		{
			name: "prefix then other byte forwards both, prefix first",
			steps: []step{
				{in: prefix, wantForward: nil, wantDetach: false},
				{in: 'z', wantForward: []byte{prefix, 'z'}, wantDetach: false},
			},
		},
		{
			name: "pending expires after exactly one byte: a second plain byte does not re-trigger it",
			steps: []step{
				{in: prefix, wantForward: nil, wantDetach: false},
				{in: 'z', wantForward: []byte{prefix, 'z'}, wantDetach: false},
				{in: 'z', wantForward: []byte{'z'}, wantDetach: false}, // pending already cleared: plain byte again.
			},
		},
		{
			name: "detach byte with no preceding prefix is just a plain byte",
			steps: []step{
				{in: detach, wantForward: []byte{detach}, wantDetach: false},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pm := &prefixMachine{prefixByte: prefix, detachByte: detach}
			for i, s := range tc.steps {
				forward, detached := pm.feed(s.in)
				if string(forward) != string(s.wantForward) {
					t.Fatalf("step %d: feed(%#x) forward = %q, want %q", i, s.in, forward, s.wantForward)
				}
				if detached != s.wantDetach {
					t.Fatalf("step %d: feed(%#x) detach = %v, want %v", i, s.in, detached, s.wantDetach)
				}
			}
		})
	}
}

// TestParseKeySpec covers cmd_attach.go's config-key parsing: a bare
// single character, and the "C-x" control-key notation attach.PrefixKey/
// DetachKey use in config.
func TestParseKeySpec(t *testing.T) {
	cases := []struct {
		spec    string
		want    byte
		wantErr bool
	}{
		{spec: "d", want: 'd'},
		{spec: "C-a", want: 0x01}, // 'a' & 0x1f
		{spec: "C-\\", want: 0x1c},
		{spec: "", wantErr: true},
		{spec: "ab", wantErr: true},
		{spec: "C-ab", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			got, err := parseKeySpec(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseKeySpec(%q) = %#x, nil, want error", tc.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseKeySpec(%q): %v", tc.spec, err)
			}
			if got != tc.want {
				t.Fatalf("parseKeySpec(%q) = %#x, want %#x", tc.spec, got, tc.want)
			}
		})
	}
}
