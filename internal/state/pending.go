package state

import (
	"bytes"

	"github.com/danielbecerra/corral/internal/hookrelay"
)

// PendingEntry is one in-flight tool call opened by PreToolUse and closed
// by PostToolUse / PostToolUseFailure.
type PendingEntry struct {
	ToolUseID string
	ToolName  string
	ToolInput []byte // raw JSON bytes, copied; used for (prompt_id,tool_name,tool_input) matching (A.3.1)
	PromptID  string
	AgentID   string
}

// PendingTracker holds open tool calls, namespaced by agent_id (so a
// subagent's parallel tool calls don't pollute the parent turn's shape).
// Namespacing discriminates on agent_id PRESENCE only ("" == main agent),
// never on agent_type (observed empty). The zero value is NOT ready — use
// NewPendingTracker. NOT safe for concurrent use: step 6b owns one per
// session inside its single ingest goroutine.
type PendingTracker struct {
	// byAgent[agentID][toolUseID] = entry
	byAgent map[string]map[string]*PendingEntry
}

// NewPendingTracker returns an empty, ready-to-use PendingTracker.
func NewPendingTracker() *PendingTracker {
	return &PendingTracker{byAgent: make(map[string]map[string]*PendingEntry)}
}

// Open records a PreToolUse. Keyed by (agentID, toolUseID). A duplicate
// toolUseID overwrites (a re-fired PreToolUse is not an error).
func (t *PendingTracker) Open(ev hookrelay.HookPayload) {
	agentID := ev.AgentID
	inner, ok := t.byAgent[agentID]
	if !ok {
		inner = make(map[string]*PendingEntry)
		t.byAgent[agentID] = inner
	}

	toolInput := make([]byte, len(ev.ToolInput))
	copy(toolInput, ev.ToolInput)

	inner[ev.ToolUseID] = &PendingEntry{
		ToolUseID: ev.ToolUseID,
		ToolName:  ev.ToolName,
		ToolInput: toolInput,
		PromptID:  ev.PromptID,
		AgentID:   agentID,
	}
}

// Close removes and returns the entry a PostToolUse/PostToolUseFailure
// closes, matched by (agentID, toolUseID). ok=false if none matched (a
// PostToolUse with no prior PreToolUse — bounded, logged at DEBUG by the
// caller, not an error).
func (t *PendingTracker) Close(ev hookrelay.HookPayload) (entry *PendingEntry, ok bool) {
	agentID := ev.AgentID
	inner, exists := t.byAgent[agentID]
	if !exists {
		return nil, false
	}

	entry, ok = inner[ev.ToolUseID]
	if !ok {
		return nil, false
	}

	delete(inner, ev.ToolUseID)
	if len(inner) == 0 {
		delete(t.byAgent, agentID)
	}

	return entry, true
}

// MatchTriple finds an open entry for the same (promptID, toolName,
// byte-equal toolInput) within the given agent namespace — used to supply
// the tool_use_id that PermissionRequest lacks (A.3.1/A.4 job #1). Returns
// one match; order is unspecified when several entries match (iteration
// is over a Go map).
func (t *PendingTracker) MatchTriple(agentID, promptID, toolName string, toolInput []byte) (*PendingEntry, bool) {
	inner, exists := t.byAgent[agentID]
	if !exists {
		return nil, false
	}

	for _, entry := range inner {
		if entry.PromptID == promptID && entry.ToolName == toolName && bytes.Equal(entry.ToolInput, toolInput) {
			return entry, true
		}
	}

	return nil, false
}

// Reset clears everything (SessionStart).
func (t *PendingTracker) Reset() {
	t.byAgent = make(map[string]map[string]*PendingEntry)
}

// Len returns the total number of open entries across all agent namespaces
// (test/introspection helper).
func (t *PendingTracker) Len() int {
	n := 0
	for _, inner := range t.byAgent {
		n += len(inner)
	}
	return n
}
