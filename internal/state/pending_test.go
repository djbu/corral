package state

import (
	"testing"

	"github.com/danielbecerra/corral/internal/hookrelay"
)

func TestPendingTracker_OpenClose(t *testing.T) {
	tr := NewPendingTracker()

	tr.Open(hookrelay.HookPayload{ToolUseID: "t1", ToolName: "Bash"})
	if got := tr.Len(); got != 1 {
		t.Fatalf("Len() after Open = %d, want 1", got)
	}

	entry, ok := tr.Close(hookrelay.HookPayload{ToolUseID: "t1"})
	if !ok {
		t.Fatalf("Close() ok = false, want true")
	}
	if entry.ToolName != "Bash" {
		t.Errorf("Close() entry.ToolName = %q, want %q", entry.ToolName, "Bash")
	}
	if got := tr.Len(); got != 0 {
		t.Fatalf("Len() after Close = %d, want 0", got)
	}
}

func TestPendingTracker_SubagentNonPollution(t *testing.T) {
	tr := NewPendingTracker()

	tr.Open(hookrelay.HookPayload{ToolUseID: "p1", ToolName: "Read"})
	tr.Open(hookrelay.HookPayload{ToolUseID: "p2", ToolName: "Read"})

	tr.Open(hookrelay.HookPayload{AgentID: "sub1", ToolUseID: "s1", ToolName: "Read"})
	tr.Open(hookrelay.HookPayload{AgentID: "sub1", ToolUseID: "s2", ToolName: "Read"})
	tr.Open(hookrelay.HookPayload{AgentID: "sub1", ToolUseID: "s3", ToolName: "Read"})

	if got := tr.Len(); got != 5 {
		t.Fatalf("Len() = %d, want 5", got)
	}

	if _, ok := tr.Close(hookrelay.HookPayload{ToolUseID: "p1"}); !ok {
		t.Fatalf("Close(p1) ok = false, want true")
	}
	if _, ok := tr.Close(hookrelay.HookPayload{ToolUseID: "p2"}); !ok {
		t.Fatalf("Close(p2) ok = false, want true")
	}

	if got := tr.Len(); got != 3 {
		t.Fatalf("Len() after closing parent entries = %d, want 3 (subagent entries should remain)", got)
	}

	if _, ok := tr.MatchTriple("", "", "Read", nil); ok {
		t.Errorf("MatchTriple in main-agent namespace found a match after all main-agent entries closed")
	}
}

func TestPendingTracker_MatchTriple(t *testing.T) {
	tr := NewPendingTracker()

	input := []byte(`{"command":"npm test"}`)
	tr.Open(hookrelay.HookPayload{
		ToolUseID: "t1",
		ToolName:  "Bash",
		ToolInput: input,
		PromptID:  "p1",
	})

	entry, ok := tr.MatchTriple("", "p1", "Bash", []byte(`{"command":"npm test"}`))
	if !ok {
		t.Fatalf("MatchTriple() ok = false, want true")
	}
	if entry.ToolUseID != "t1" {
		t.Errorf("MatchTriple() entry.ToolUseID = %q, want %q", entry.ToolUseID, "t1")
	}

	if _, ok := tr.MatchTriple("", "p1", "Bash", []byte(`{"command":"npm run build"}`)); ok {
		t.Errorf("MatchTriple() with different tool_input found a match, want none")
	}
}

func TestPendingTracker_OpenCopiesToolInput(t *testing.T) {
	tr := NewPendingTracker()

	input := []byte(`{"command":"npm test"}`)
	tr.Open(hookrelay.HookPayload{ToolUseID: "t1", ToolName: "Bash", ToolInput: input, PromptID: "p1"})

	// Mutate the caller's slice after Open returns. If Open retained the
	// slice instead of copying it, this mutation would corrupt the
	// tracked entry.
	copy(input, []byte(`{"command":"rm -rf /"}`))

	if _, ok := tr.MatchTriple("", "p1", "Bash", []byte(`{"command":"npm test"}`)); !ok {
		t.Errorf("Open retained the payload's ToolInput slice instead of copying it")
	}
}

func TestPendingTracker_CloseNoPriorOpen(t *testing.T) {
	tr := NewPendingTracker()

	entry, ok := tr.Close(hookrelay.HookPayload{ToolUseID: "nope"})
	if ok {
		t.Errorf("Close() with no prior Open: ok = true, want false")
	}
	if entry != nil {
		t.Errorf("Close() with no prior Open: entry = %+v, want nil", entry)
	}
}

func TestPendingTracker_Reset(t *testing.T) {
	tr := NewPendingTracker()

	tr.Open(hookrelay.HookPayload{ToolUseID: "t1", ToolName: "Bash"})
	tr.Open(hookrelay.HookPayload{AgentID: "sub1", ToolUseID: "t2", ToolName: "Bash"})

	if got := tr.Len(); got != 2 {
		t.Fatalf("Len() before Reset = %d, want 2", got)
	}

	tr.Reset()

	if got := tr.Len(); got != 0 {
		t.Errorf("Len() after Reset = %d, want 0", got)
	}
}
