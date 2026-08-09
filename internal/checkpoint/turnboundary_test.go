package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastRecordIsCompletedTurn(t *testing.T) {
	cases := []struct {
		name     string
		fixture  string
		wantBool bool
	}{
		{"ends_end_turn", "ends_end_turn.jsonl", true},
		{"ends_tool_use", "ends_tool_use.jsonl", false},
		{"ends_user_toolresult", "ends_user_toolresult.jsonl", false},
		{"ends_stop_sequence", "ends_stop_sequence.jsonl", false},
		{"sidechain_last", "sidechain_last.jsonl", true},
		{"no_conv", "no_conv.jsonl", false},
		{"empty", "empty.jsonl", false},
		{"trailing_partial", "trailing_partial.jsonl", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join("testdata", tc.fixture)
			got, err := LastRecordIsCompletedTurn(path)
			if err != nil {
				t.Fatalf("LastRecordIsCompletedTurn(%q) unexpected error: %v", path, err)
			}
			if got != tc.wantBool {
				t.Fatalf("LastRecordIsCompletedTurn(%q) = %v, want %v", path, got, tc.wantBool)
			}
		})
	}
}

// buildLargeTranscript writes a transcript at path whose size comfortably
// exceeds tailWindow, padded with filler "system" bookkeeping lines, then
// appends the given tail lines. It fails the test outright if the padding
// didn't actually exceed tailWindow, so a change to tailWindow's value
// can't silently turn this into a no-op same-day test.
func buildLargeTranscript(t *testing.T, path string, tailLines ...string) {
	t.Helper()
	fillerLine := `{"type":"system","isSidechain":false,"content":"filler line padding the transcript out past the tail window so LastRecordIsCompletedTurn must seek rather than read the whole file"}` + "\n"
	var b strings.Builder
	for b.Len() < tailWindow+64*1024 {
		b.WriteString(fillerLine)
	}
	for _, line := range tailLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() <= tailWindow {
		t.Fatalf("constructed transcript size %d did not exceed tailWindow %d — test doesn't exercise the seek path", info.Size(), tailWindow)
	}
}

func TestLastRecordIsCompletedTurn_LargeFile(t *testing.T) {
	cases := []struct {
		name     string
		tail     []string
		wantBool bool
	}{
		{
			name: "ends_end_turn_beyond_window",
			tail: []string{
				`{"type":"user","isSidechain":false,"message":{"role":"user","content":"hello"}}`,
				`{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":"hi","stop_reason":"end_turn"}}`,
				`{"type":"system","isSidechain":false,"content":"trailing meta record"}`,
			},
			wantBool: true,
		},
		{
			name: "ends_tool_use_beyond_window",
			tail: []string{
				`{"type":"user","isSidechain":false,"message":{"role":"user","content":"run a command"}}`,
				`{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":"invoking tool","stop_reason":"tool_use"}}`,
			},
			wantBool: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "large.jsonl")
			buildLargeTranscript(t, path, tc.tail...)

			got, err := LastRecordIsCompletedTurn(path)
			if err != nil {
				t.Fatalf("LastRecordIsCompletedTurn(%q) unexpected error: %v", path, err)
			}
			if got != tc.wantBool {
				t.Fatalf("LastRecordIsCompletedTurn(%q) = %v, want %v", path, got, tc.wantBool)
			}
		})
	}
}

func TestLastRecordIsCompletedTurn_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.jsonl")

	got, err := LastRecordIsCompletedTurn(path)
	if err != nil {
		t.Fatalf("LastRecordIsCompletedTurn(%q) err = %v, want nil", path, err)
	}
	if got != false {
		t.Fatalf("LastRecordIsCompletedTurn(%q) = %v, want false", path, got)
	}
}
