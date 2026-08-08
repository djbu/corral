// Package hookrelay implements corral's Claude Code hook transport: the
// wire types for a hook payload (this file), the daemon's inert response
// envelope (response.go), and the `corral hook-relay` client that reads a
// hook payload from stdin and forwards it to the daemon over its unix
// socket (relay.go). See design doc §2-§3 and Amendment A.1-A.3.3.
package hookrelay

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Common is the field set every hook payload carries (design doc §3.1,
// Amendment A.1): every payload has hook_event_name/session_id/
// transcript_path/cwd; PermissionMode is present on all but SessionStart,
// SessionEnd, and Notification.
type Common struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`
}

// PermissionSuggestion is one entry of PermissionRequest's
// permission_suggestions[] (Amendment A.1/A.3.3) — the "allowlist this
// directory" / "switch to acceptEdits" offers a future policy engine (M4)
// consumes. Only Type is common to every observed suggestion shape; the
// rest vary by Type, so they're all optional.
type PermissionSuggestion struct {
	Type        string   `json:"type"`
	Directories []string `json:"directories,omitempty"`
	Destination string   `json:"destination,omitempty"`
	Mode        string   `json:"mode,omitempty"`
}

// HookPayload is the full decoded shape of any of the 15 registered hook
// events (design doc §3.1, Amendment A.1). Every field beyond Common is
// only populated by some subset of events; a field absent from a given
// event's payload simply decodes to its zero value. Raw always holds the
// exact input bytes, independent of how much of the rest decoded cleanly —
// it is what gets persisted (after redaction/truncation) so a future CLI
// version's shape changes never lose data, only structure.
type HookPayload struct {
	Common

	// PromptID is the turn-correlation key (Amendment A.1.1): present on
	// every payload except SessionStart.
	PromptID string `json:"prompt_id"`

	// Tool events: PreToolUse, PostToolUse, PermissionRequest.
	// PermissionRequest carries no ToolUseID (Amendment A.3.1).
	ToolName              string                 `json:"tool_name"`
	ToolUseID             string                 `json:"tool_use_id"`
	ToolInput             json.RawMessage        `json:"tool_input"`
	ToolResponse          json.RawMessage        `json:"tool_response"`
	PermissionSuggestions []PermissionSuggestion `json:"permission_suggestions"`
	DurationMs            *int64                 `json:"duration_ms"`

	// Notification.
	Message          string `json:"message"`
	Title            string `json:"title"`
	NotificationType string `json:"notification_type"`

	// SessionStart / Stop / PreCompact / SessionEnd.
	Source         string `json:"source"`
	StopHookActive bool   `json:"stop_hook_active"`
	Reason         string `json:"reason"`
	Trigger        string `json:"trigger"`

	// Stop / SubagentStop.
	LastAssistantMessage string            `json:"last_assistant_message"`
	BackgroundTasks      []json.RawMessage `json:"background_tasks"`
	SessionCrons         []json.RawMessage `json:"session_crons"`

	// SubagentStart / SubagentStop.
	AgentID             string `json:"agent_id"`
	AgentType           string `json:"agent_type"`
	AgentTranscriptPath string `json:"agent_transcript_path"`

	// UserPromptSubmit.
	Prompt string `json:"prompt"`

	// Raw is the exact input to Decode, preserved verbatim regardless of
	// decode outcome. Not part of the JSON shape itself (json:"-"): it is
	// populated by Decode, never by unmarshaling a field named "Raw".
	Raw json.RawMessage `json:"-"`
}

// Decode parses b into a HookPayload. It is total and defensive, per design
// doc §3.1, because it runs on daemon-side input that is adversarial by
// construction (an attacker who can run a Claude Code session controls
// every byte of a hook payload):
//
//   - Unknown fields are ignored — encoding/json's default behavior, and
//     deliberately never DisallowUnknownFields: a CLI upgrade that adds a
//     field must not break ingest.
//   - A field present with the wrong JSON type (e.g. a string where a bool
//     is expected) does not fail the whole decode. encoding/json already
//     decodes every other field and only reports the first type mismatch
//     as an error; Decode treats that specific error class as non-fatal
//     and returns the partially-populated payload with a nil error, so one
//     malformed field never discards an otherwise-good payload.
//   - Genuine syntax errors (garbage bytes, truncated JSON — e.g. from
//     relay.go's 1 MiB stdin cap cutting a payload mid-token) are still
//     returned as errors; the caller (the daemon's hook-ingest handler) is
//     expected to treat a Decode error as non-fatal too (design doc §2.6:
//     "Decode failure is not an error response... records hook.undecodable
//     ... returns 200"), never as a reason to fail the request.
//   - Nesting depth is capped by encoding/json's own built-in scanner limit
//     (it refuses to recurse past a fixed depth and returns an error
//     instead) — Decode does not need its own recursion, since every
//     "shape we don't need to inspect" field (ToolInput, ToolResponse,
//     BackgroundTasks, SessionCrons) decodes into json.RawMessage rather
//     than a typed structure that would otherwise recurse into it.
//   - Decode never panics: it performs no manual indexing, no type
//     assertions, no recursion of its own.
//
// Raw is always set to a copy of b, independent of whether decoding
// succeeded, partially succeeded, or failed outright.
func Decode(b []byte) (HookPayload, error) {
	var p HookPayload
	err := json.Unmarshal(b, &p)

	raw := make([]byte, len(b))
	copy(raw, b)
	p.Raw = raw

	if err == nil {
		return p, nil
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		// A single field had the wrong JSON type. encoding/json already
		// decoded every other field before returning this error; the
		// mismatched field itself is simply left at its zero value.
		// Tolerate it: this is exactly the "wrong-typed fields tolerated"
		// case, not a reason to discard the rest of the payload.
		return p, nil
	}

	return p, fmt.Errorf("hookrelay: decoding hook payload: %w", err)
}
