package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielbecerra/corral/internal/api/client"
)

// writeDagFile writes contents to a fresh tempdir file and returns its
// path, so parseDagFile can be exercised against a real file the same way
// `corral run --file <path>` would encounter one.
func writeDagFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dag.toml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestParseDagFile_HappyPath covers two nodes with a depends_on edge,
// asserting the resulting SubmitDagRequest's nodes and edges are exactly
// what the TOML describes.
func TestParseDagFile_HappyPath(t *testing.T) {
	path := writeDagFile(t, `
budget_usd = 5.0

[[node]]
name = "plan"
prompt = "plan it"
repo = "/repo"

[[node]]
name = "implement"
prompt = "implement it"
repo = "/repo"
worktree = true
model = "claude-opus"
max_attempts = 3
depends_on = ["plan"]
`)

	req, err := parseDagFile(path)
	if err != nil {
		t.Fatalf("parseDagFile: %v", err)
	}

	if req.BudgetUSD == nil || *req.BudgetUSD != 5.0 {
		t.Fatalf("BudgetUSD = %v, want 5.0", req.BudgetUSD)
	}

	wantNodes := []client.DagNode{
		{Name: "plan", Prompt: "plan it", Repo: "/repo"},
		{Name: "implement", Prompt: "implement it", Repo: "/repo", Worktree: true, Model: "claude-opus", MaxAttempts: 3},
	}
	if len(req.Nodes) != len(wantNodes) {
		t.Fatalf("len(Nodes) = %d, want %d (%+v)", len(req.Nodes), len(wantNodes), req.Nodes)
	}
	for i, want := range wantNodes {
		got := req.Nodes[i]
		if got.Name != want.Name || got.Prompt != want.Prompt || got.Repo != want.Repo ||
			got.Worktree != want.Worktree || got.Model != want.Model || got.MaxAttempts != want.MaxAttempts {
			t.Errorf("Nodes[%d] = %+v, want %+v", i, got, want)
		}
		if got.PermissionMode != "" {
			t.Errorf("Nodes[%d].PermissionMode = %q, want empty (file mode may never set it)", i, got.PermissionMode)
		}
	}

	wantEdges := []client.DagEdge{
		{Task: "implement", DependsOn: "plan"},
	}
	if len(req.Edges) != len(wantEdges) {
		t.Fatalf("Edges = %+v, want %+v", req.Edges, wantEdges)
	}
	for i, want := range wantEdges {
		if req.Edges[i] != want {
			t.Errorf("Edges[%d] = %+v, want %+v", i, req.Edges[i], want)
		}
	}
}

// TestParseDagFile_RejectsPermissionMode is the security regression test:
// a dag.toml with a permission_mode key under a node must fail to parse,
// proving permission_mode cannot leak into a SubmitDagRequest from
// repo-authored file content — the only legitimate channel is the
// --permission-mode CLI flag in bare mode.
func TestParseDagFile_RejectsPermissionMode(t *testing.T) {
	path := writeDagFile(t, `
[[node]]
name = "plan"
prompt = "plan it"
repo = "/repo"
permission_mode = "bypassPermissions"
`)

	_, err := parseDagFile(path)
	if err == nil {
		t.Fatal("parseDagFile: got nil error, want an error naming the undecoded permission_mode key")
	}
	// Assert on the exact key token BurntSushi's md.Undecoded() renders for
	// an unknown key inside a [[node]] array-of-tables ("node.permission_mode"),
	// not merely the substring "permission_mode" — the friendly prefix in
	// parseDagFile's error message also contains that substring, so a
	// Contains(err, "permission_mode") check alone would pass even if the
	// Undecoded() gate were deleted entirely. Checking for the full dotted
	// key proves the rejection actually came from md.Undecoded().
	if !strings.Contains(err.Error(), "node.permission_mode") {
		t.Errorf("parseDagFile error = %q, want it to mention the undecoded key %q", err.Error(), "node.permission_mode")
	}
}

// TestParseDagFile_RejectsUnknownTopLevelKey covers a [notify] table (an
// unrelated corral config concept that must never leak into dag.toml
// parsing) alongside a top-level unknown scalar key.
func TestParseDagFile_RejectsUnknownTopLevelKey(t *testing.T) {
	cases := []struct {
		name     string
		contents string
	}{
		{
			name: "notify table",
			contents: `
[[node]]
name = "plan"
prompt = "plan it"
repo = "/repo"

[notify]
on_blocked = true
`,
		},
		{
			name: "unknown top-level key",
			contents: `
unexpected_key = true

[[node]]
name = "plan"
prompt = "plan it"
repo = "/repo"
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeDagFile(t, tc.contents)
			_, err := parseDagFile(path)
			if err == nil {
				t.Fatal("parseDagFile: got nil error, want an error naming the unknown key")
			}
		})
	}
}

// TestParseDagFile_RequiresAtLeastOneNode guards the fail-fast validation
// path (the server also validates this, but the CLI should not round-trip
// to the daemon for an empty dag).
func TestParseDagFile_RequiresAtLeastOneNode(t *testing.T) {
	path := writeDagFile(t, `budget_usd = 1.0`)
	if _, err := parseDagFile(path); err == nil {
		t.Fatal("parseDagFile: got nil error for a dag.toml with zero nodes")
	}
}
