package streamjson

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// goldenDir is the corpus m4.md §0 calls "golden corpus ready": 5 real
// `claude -p --output-format stream-json` transcripts (claude 2.1.224),
// one per scenario. Path is repo-root-relative from this package.
const goldenDir = "../../../testdata/golden/streamjson"

// readLines reads a golden file into its raw lines, skipping any blank
// trailing line a text editor might have appended.
func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		lines = append(lines, append([]byte(nil), line...))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return lines
}

// TestParseLine_Golden runs every line of every golden corpus file through
// ParseLine and asserts zero errors — the tolerant-decode contract (m4.md
// §4.1/§4.3): real `system` lines carry many subtypes beyond "init"
// (hook_started, hook_response, thinking_tokens, permission_denied, …),
// and there's a top-level "rate_limit_event" type never mentioned by name
// in the design doc's §4.2 sketch. None of that is malformed JSON, so none
// of it may surface as a ParseLine error.
func TestParseLine_Golden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenDir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob golden dir: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no golden files found under %s — check goldenDir", goldenDir)
	}
	if len(files) < 5 {
		// m4.md §12 plans a 6th golden capture (real headless `-p` run
		// behind CORRAL_E2E_REAL_CLAUDE) landing later in M4 — this must
		// not break when that shows up, only if the corpus shrinks.
		t.Fatalf("expected at least 5 golden corpus files, found %d: %v", len(files), files)
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			lines := readLines(t, path)
			if len(lines) == 0 {
				t.Fatalf("%s: no lines read", path)
			}
			for i, line := range lines {
				ev, err := ParseLine(line)
				if err != nil {
					t.Fatalf("%s: line %d: ParseLine error: %v\nline: %s", path, i, err, line)
				}
				if ev.Type == "" {
					t.Errorf("%s: line %d: empty Event.Type", path, i)
				}
			}
		})
	}
}

// TestParseLine_ResultInvariants checks the result line of every golden
// file — the terminal record each `claude -p` invocation always emits
// (m4.md §5.2) — decodes into a populated Result with sane cost/turn
// fields, and that Result is populated ONLY on type=="result".
func TestParseLine_ResultInvariants(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenDir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob golden dir: %v", err)
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			lines := readLines(t, path)
			sawResult := false
			for i, line := range lines {
				ev, err := ParseLine(line)
				if err != nil {
					t.Fatalf("line %d: %v", i, err)
				}

				if ev.Type != "result" {
					if ev.Result != nil {
						t.Errorf("line %d: type=%q but Result populated", i, ev.Type)
					}
					continue
				}

				sawResult = true
				if ev.Result == nil {
					t.Fatalf("line %d: type==result but Result is nil", i)
				}
				if ev.SessionID == "" {
					t.Errorf("line %d: result line missing SessionID", i)
				}
				if !ev.Result.IsError && ev.Result.TotalCostUSD < 0 {
					t.Errorf("line %d: !IsError but TotalCostUSD=%v < 0", i, ev.Result.TotalCostUSD)
				}
				if ev.Result.NumTurns <= 0 {
					t.Errorf("line %d: NumTurns=%d, want > 0", i, ev.Result.NumTurns)
				}
				if ev.Result.StopReason == "" {
					t.Errorf("line %d: result missing StopReason", i)
				}
			}
			if !sawResult {
				t.Fatalf("%s: no result line found", path)
			}
		})
	}
}

// TestParseLine_ToolUse asserts assistant lines carrying a tool_use
// content block populate ToolUses with the tool's id/name — exercised
// against both tool-use golden files (bash, write+read).
func TestParseLine_ToolUse(t *testing.T) {
	for _, name := range []string{"tool-use-bash.jsonl", "tool-use-write-read.jsonl"} {
		t.Run(name, func(t *testing.T) {
			lines := readLines(t, filepath.Join(goldenDir, name))
			var sawToolUse bool
			for i, line := range lines {
				ev, err := ParseLine(line)
				if err != nil {
					t.Fatalf("line %d: %v", i, err)
				}
				if ev.Type != "assistant" || len(ev.ToolUses) == 0 {
					continue
				}
				sawToolUse = true
				for _, tu := range ev.ToolUses {
					if tu.ID == "" {
						t.Errorf("line %d: tool_use with empty ID", i)
					}
					if tu.Name == "" {
						t.Errorf("line %d: tool_use with empty Name", i)
					}
				}
			}
			if !sawToolUse {
				t.Fatalf("%s: expected at least one assistant line with ToolUses populated", name)
			}
		})
	}
}

// TestParseLine_Text asserts the simple-text-reply corpus's final
// assistant turn decodes its text content block into Event.Text.
func TestParseLine_Text(t *testing.T) {
	lines := readLines(t, filepath.Join(goldenDir, "simple-text-reply.jsonl"))
	var sawText bool
	for i, line := range lines {
		ev, err := ParseLine(line)
		if err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if ev.Type == "assistant" && ev.Text != "" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatal("expected at least one assistant line with non-empty Text")
	}
}

// TestParseLine_Denials asserts the error-turn corpus (a permission
// denial) surfaces the denial in the result line's Denials, joined by
// tool_use_id + tool_name — matching m4.md §0's "denials join via
// tool_use_id" and §4.2's Denial contract.
func TestParseLine_Denials(t *testing.T) {
	lines := readLines(t, filepath.Join(goldenDir, "error-turn.jsonl"))

	// The system:permission_denied line names the denied tool_use_id —
	// collect it so we can cross-check the result line's Denials joins to
	// the same id, not just that Denials is non-empty.
	var deniedIDs []string
	var result *Result
	for i, line := range lines {
		ev, err := ParseLine(line)
		if err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if ev.Type == "system" && ev.Subtype == "permission_denied" {
			var sys struct {
				ToolUseID string `json:"tool_use_id"`
			}
			if err := json.Unmarshal(line, &sys); err != nil {
				t.Fatalf("line %d: decode system permission_denied: %v", i, err)
			}
			deniedIDs = append(deniedIDs, sys.ToolUseID)
		}
		if ev.Type == "result" {
			result = ev.Result
		}
	}

	if len(deniedIDs) == 0 {
		t.Fatal("expected at least one system:permission_denied line in error-turn.jsonl")
	}
	if result == nil {
		t.Fatal("expected a result line in error-turn.jsonl")
	}
	if len(result.Denials) == 0 {
		t.Fatal("expected result.Denials to be non-empty")
	}
	for _, d := range result.Denials {
		if d.ToolUseID == "" || d.ToolName == "" {
			t.Errorf("denial with empty field: %+v", d)
		}
	}
	if result.Denials[0].ToolUseID != deniedIDs[0] {
		t.Errorf("result.Denials[0].ToolUseID = %q, want %q (from system:permission_denied line)", result.Denials[0].ToolUseID, deniedIDs[0])
	}
}
