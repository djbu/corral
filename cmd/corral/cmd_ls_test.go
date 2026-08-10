package main

import (
	"encoding/json"
	"testing"

	"github.com/djbu/corral/internal/api/client"
)

// TestStateColumn covers the §4.5 STATE-cell decorations: agent_state drives
// the cell, unknown gets the hooks hint, a stale working session renders
// "working?", blocked appends its reason summary, and an empty agent_state
// falls back to the process status.
func TestStateColumn(t *testing.T) {
	tests := []struct {
		name string
		in   client.SessionInfo
		want string
	}{
		{
			name: "empty agent_state falls back to status",
			in:   client.SessionInfo{AgentState: "", Status: "running"},
			want: "running",
		},
		{
			name: "unknown gets hooks hint",
			in:   client.SessionInfo{AgentState: "unknown", Status: "running"},
			want: "unknown (hooks not firing?)",
		},
		{
			name: "working fresh",
			in:   client.SessionInfo{AgentState: "working", Stale: false},
			want: "working",
		},
		{
			name: "working stale renders working?",
			in:   client.SessionInfo{AgentState: "working", Stale: true},
			want: "working?",
		},
		{
			name: "blocked with summary",
			in: client.SessionInfo{
				AgentState:    "blocked",
				BlockedReason: json.RawMessage(`{"summary":"Bash: rm -rf /tmp/x"}`),
			},
			want: "blocked: Bash: rm -rf /tmp/x",
		},
		{
			name: "blocked without reason",
			in:   client.SessionInfo{AgentState: "blocked"},
			want: "blocked",
		},
		{
			name: "blocked with malformed reason falls back to bare blocked",
			in: client.SessionInfo{
				AgentState:    "blocked",
				BlockedReason: json.RawMessage(`{not json`),
			},
			want: "blocked",
		},
		{
			name: "idle passthrough",
			in:   client.SessionInfo{AgentState: "idle"},
			want: "idle",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stateColumn(tc.in); got != tc.want {
				t.Fatalf("stateColumn = %q, want %q", got, tc.want)
			}
		})
	}
}
