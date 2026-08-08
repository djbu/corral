package automode

import (
	"errors"
	"testing"
)

// TestClassify covers the output-shape decision Detect delegates to: a
// well-formed policy → Active with counts; every failure mode (run error,
// non-JSON, empty object) → Unknown with the raw output retained for the
// doctor diagnostic. Modeled on the real `claude auto-mode config` shape
// observed during the M2 spike: a JSON object with allow/soft_deny/hard_deny
// arrays.
func TestClassify(t *testing.T) {
	const realShape = `{"allow":["a","b"],"soft_deny":["x"],"hard_deny":["z"],"environment":["e"]}`

	tests := []struct {
		name      string
		out       string
		runErr    error
		want      Status
		wantAllow int
		wantSoft  int
		wantHard  int
		wantRaw   string
	}{
		{
			name:      "well-formed policy is active with counts",
			out:       realShape,
			want:      StatusActive,
			wantAllow: 2, wantSoft: 1, wantHard: 1,
		},
		{
			name:    "run error is unknown, raw retained",
			out:     "claude: unknown command \"auto-mode\"",
			runErr:  errors.New("exit status 1"),
			want:    StatusUnknown,
			wantRaw: `claude: unknown command "auto-mode"`,
		},
		{
			name:    "non-json output is unknown, raw retained",
			out:     "some banner text\n",
			want:    StatusUnknown,
			wantRaw: "some banner text",
		},
		{
			name:    "empty object is not a policy",
			out:     "{}",
			want:    StatusUnknown,
			wantRaw: "{}",
		},
		{
			name: "only allow populated still counts as active",
			out:  `{"allow":["a"],"soft_deny":[],"hard_deny":[]}`,
			want: StatusActive, wantAllow: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.out, tc.runErr)
			if got.Status != tc.want {
				t.Fatalf("Status = %v, want %v", got.Status, tc.want)
			}
			if got.AllowCount != tc.wantAllow || got.SoftDenyCount != tc.wantSoft || got.HardDenyCount != tc.wantHard {
				t.Errorf("counts = allow%d soft%d hard%d, want allow%d soft%d hard%d",
					got.AllowCount, got.SoftDenyCount, got.HardDenyCount, tc.wantAllow, tc.wantSoft, tc.wantHard)
			}
			if tc.want == StatusActive && got.Raw != "" {
				t.Errorf("active result leaked Raw = %q, want empty (policy is noisy/sensitive)", got.Raw)
			}
			if tc.want == StatusUnknown && got.Raw != tc.wantRaw {
				t.Errorf("Raw = %q, want %q", got.Raw, tc.wantRaw)
			}
		})
	}
}
