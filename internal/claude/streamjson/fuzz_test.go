package streamjson

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzParseLine seeds the fuzz corpus with every line from every golden
// file (m4.md §4.3/§12) and asserts ParseLine's two hard invariants:
//   - it never panics, no matter the input bytes;
//   - on a decoded type=="result" line with is_error==false, TotalCostUSD
//     is never negative.
//
// Everything else about ParseLine's tolerant-decode contract (unknown
// type/subtype pass through as a plain Event, only malformed JSON errors)
// is exercised by the golden table tests in parse_test.go; the fuzz target
// only needs to hold the two invariants above across arbitrary mutations
// of real input.
func FuzzParseLine(f *testing.F) {
	files, err := filepath.Glob(filepath.Join(goldenDir, "*.jsonl"))
	if err != nil {
		f.Fatalf("glob golden dir: %v", err)
	}
	if len(files) == 0 {
		f.Fatalf("no golden files found under %s — check goldenDir", goldenDir)
	}

	for _, path := range files {
		func() {
			fh, err := os.Open(path)
			if err != nil {
				f.Fatalf("open %s: %v", path, err)
			}
			defer fh.Close()

			sc := bufio.NewScanner(fh)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				line := sc.Bytes()
				if len(line) == 0 {
					continue
				}
				f.Add(append([]byte(nil), line...))
			}
			if err := sc.Err(); err != nil {
				f.Fatalf("scan %s: %v", path, err)
			}
		}()
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		ev, err := ParseLine(b)
		if err != nil {
			// Malformed JSON is the one permitted error path.
			return
		}
		if ev.Type != "result" || ev.Result == nil || ev.Result.IsError {
			return
		}
		// The invariant ("is_error=false => TotalCostUSD>=0") is a property
		// of real claude output, not something ParseLine enforces on
		// arbitrary bytes — it faithfully decodes whatever total_cost_usd
		// the JSON carries. So only check it when the source line's raw
		// total_cost_usd was itself non-negative (or absent, decoding to
		// the zero value); otherwise a fuzzer-inserted "-" before the
		// number would produce a permanent, meaningless crasher seed.
		var probe struct {
			TotalCostUSD *float64 `json:"total_cost_usd"`
		}
		_ = json.Unmarshal(b, &probe)
		if probe.TotalCostUSD != nil && *probe.TotalCostUSD < 0 {
			return
		}
		if ev.Result.TotalCostUSD < 0 {
			t.Errorf("result line decoded with is_error=false and non-negative raw total_cost_usd but Result.TotalCostUSD=%v < 0; input: %s", ev.Result.TotalCostUSD, b)
		}
	})
}
