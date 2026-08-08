package hookrelay

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzDecodeHookPayload checks the properties design doc §3.1 requires of
// Decode for any input, adversarial or not (design doc §11): it must never
// panic, and Raw must always be an exact copy of the input regardless of
// whether decoding succeeded. It is seeded from all 15 real fixtures plus a
// set of adversarial shapes (deep nesting, huge strings, invalid UTF-8,
// nulls in every field) chosen to exercise the "wrong-typed fields
// tolerated" and "nesting depth capped" guarantees Decode's doc comment
// makes.
func FuzzDecodeHookPayload(f *testing.F) {
	dir := filepath.Join("..", "..", "testdata", "hooks", "2.1.224")
	entries, err := os.ReadDir(dir)
	if err != nil {
		f.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			f.Fatalf("reading %s: %v", e.Name(), err)
		}
		f.Add(b)
	}

	adversarial := [][]byte{
		[]byte(``),
		[]byte(`null`),
		[]byte(`{}`),
		[]byte(`[]`),
		[]byte(`"just a string"`),
		[]byte(`42`),
		[]byte(`{"hook_event_name":123}`),
		[]byte(`{"session_id":null,"cwd":true,"stop_hook_active":"yes"}`),
		[]byte(`{"tool_input":"not an object","tool_response":123}`),
		[]byte(`{"permission_suggestions":"not an array"}`),
		[]byte(`{"background_tasks":{"not":"an array"}}`),
		[]byte(`{"duration_ms":"not a number"}`),
		[]byte(`{"prompt": "\x00\x01\xff\xfe not valid utf-8 \xc0\xaf"}`),
		[]byte("{\"message\": \"" + string(make([]byte, 4096)) + "\"}"),
		[]byte(deepNesting(20000)),
		[]byte(`this is not json at all`),
		[]byte(`{"hook_event_name": "Stop", "stop_hook_active": [1,2,3]}`),
	}
	for _, b := range adversarial {
		f.Add(b)
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		p, _ := Decode(b)
		if string(p.Raw) != string(b) {
			t.Fatalf("Raw = %q, want exact copy of input %q", p.Raw, b)
		}
	})
}

// deepNesting builds a pathologically nested JSON array
// ("[[[...[]...]]]") n levels deep, to exercise Decode's reliance on
// encoding/json's built-in max-nesting-depth guard.
func deepNesting(n int) string {
	b := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		b = append(b, '[')
	}
	for i := 0; i < n; i++ {
		b = append(b, ']')
	}
	return string(b)
}
