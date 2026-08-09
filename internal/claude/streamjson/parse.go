// Package streamjson decodes the line-oriented JSON protocol emitted by
// `claude -p --output-format stream-json` (m4.md §4). It is deliberately
// tolerant: the real CLI emits far more line shapes than the handful the
// design doc calls out by name (hook_started/hook_response/thinking_tokens
// system subtypes, a top-level rate_limit_event type, MCP/plugin noise
// embedded in system:init, …) and new claude releases add more without
// notice. ParseLine's contract is that only malformed JSON is an error —
// every recognized-or-not type/subtype decodes into an Event, never a
// panic, so the headless spawn path (§5) can tee+parse stdout forever
// without a single unexpected line killing the pipe reader.
package streamjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Event is one decoded stream-json line. Type/Subtype/Raw are populated for
// every line that is at least valid JSON; the rest are convenience fields
// populated only where the line's Type makes them meaningful (see field
// comments). Fields left unpopulated take their zero value — callers must
// not treat e.g. an empty Text as an error.
type Event struct {
	Type    string // system|assistant|user|result|rate_limit_event|<unknown>
	Subtype string // init, hook_started, thinking_tokens, success, … ("" if absent)

	// Raw is the exact input line, unmodified, for audit/tee (spike-notes:
	// the on-disk transcript and the stdout stream diverge — deltas never
	// persist — so callers that need the wire-exact record keep this).
	Raw json.RawMessage

	// SessionID is populated whenever the line carries a session_id field,
	// regardless of Type — every observed line in the golden corpus
	// carries one.
	SessionID string

	// Result is non-nil only when Type == "result".
	Result *Result

	// ToolUses holds the tool_use content blocks on an assistant message.
	// In the real corpus each assistant record carries exactly one content
	// block (spike-notes.md:53-54: "one assistant record per completed
	// content block"), but this concatenates across the whole content
	// array regardless, so a future/different CLI build that batches
	// blocks per record still decodes correctly.
	ToolUses []ToolUse

	// Text is the concatenation of all text content blocks on an assistant
	// message, in array order. Empty for non-text-bearing lines.
	Text string
}

// ToolUse is one tool_use content block from an assistant message.
type ToolUse struct {
	ID    string          // tool_use_id — joins to the matching user tool_result / a Denial
	Name  string          // tool name, e.g. "Bash", "Write"
	Input json.RawMessage // tool-specific args, passed through undecoded
}

// Denial is one entry from a result line's permission_denials array —
// already joined tool_use_id + tool_name by the CLI itself; Denial just
// carries that join into Go.
type Denial struct {
	ToolUseID string
	ToolName  string
}

// Usage carries the token-accounting fields off a result line's usage
// object. Field names/tags match the real JSON (see golden corpus
// simple-text-reply.jsonl's result line) — the CLI's usage object carries
// several more fields (server_tool_use, cache_creation, iterations,
// service_tier, …) that Result does not surface; add them here if a
// consumer needs them, following the same tolerant-decode rule.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// Result is the decoded body of a type=="result" line — the terminal
// record of a `claude -p` invocation, carrying the authoritative
// per-invocation cost (m4.md §7).
type Result struct {
	IsError      bool
	TotalCostUSD float64
	Usage        Usage
	Denials      []Denial // tool_use_id + tool_name pairs; empty, not nil, when none
	StopReason   string
	NumTurns     int
}

// resultWire is the wire shape of a type=="result" line. Only the fields
// Result surfaces are declared; the real line carries many more
// (modelUsage, terminal_reason, fast_mode_state, ttft_ms, duration_ms,
// uuid, api_error_status, …) that json.Unmarshal silently drops — that
// drop is the tolerance mechanism, not an oversight.
type resultWire struct {
	IsError           bool     `json:"is_error"`
	TotalCostUSD      float64  `json:"total_cost_usd"`
	Usage             Usage    `json:"usage"`
	PermissionDenials []denial `json:"permission_denials"`
	StopReason        string   `json:"stop_reason"`
	NumTurns          int      `json:"num_turns"`
}

type denial struct {
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
}

// assistantWire is the wire shape of a type=="assistant" line, down to its
// content blocks. Both a "text" block and a "tool_use" block are decoded
// with the same struct; the fields that don't apply to a given block's
// Type are simply absent/zero in the source JSON.
type assistantWire struct {
	Message struct {
		Content []contentBlock `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type  string          `json:"type"` // text|tool_use|thinking|<unknown>
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// lineHead is the minimal envelope every line shares, decoded first so
// ParseLine can dispatch on Type without committing to a stricter shape
// that unknown-typed lines might not satisfy.
type lineHead struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
}

// ParseLine decodes one stream-json line. Per m4.md §4.2: an unrecognized
// Type or Subtype is never an error — it comes back as an Event carrying
// just Type/Subtype/Raw/SessionID, with Result nil and ToolUses/Text at
// their zero value. Only malformed JSON (the line does not parse at all)
// returns a non-nil error; callers are expected to log-and-continue on
// error, not abort the stream.
func ParseLine(b []byte) (Event, error) {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return Event{}, fmt.Errorf("streamjson: empty line")
	}

	var head lineHead
	if err := json.Unmarshal(trimmed, &head); err != nil {
		return Event{}, fmt.Errorf("streamjson: decode line: %w", err)
	}

	ev := Event{
		Type:      head.Type,
		Subtype:   head.Subtype,
		Raw:       json.RawMessage(append([]byte(nil), trimmed...)),
		SessionID: head.SessionID,
	}

	switch head.Type {
	case "assistant":
		decodeAssistant(trimmed, &ev)
	case "result":
		decodeResult(trimmed, &ev)
	}

	return ev, nil
}

// decodeAssistant populates ev.Text and ev.ToolUses from an assistant
// line's content blocks. A decode failure here is tolerated, not
// propagated: the outer line was already valid JSON (ParseLine got this
// far), so a mismatch just means this particular assistant shape doesn't
// carry the content array we expect — Text/ToolUses stay at zero value
// rather than turning a recognized-but-odd line into an error.
func decodeAssistant(b []byte, ev *Event) {
	var wire assistantWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return
	}

	var text strings.Builder
	for _, blk := range wire.Message.Content {
		switch blk.Type {
		case "text":
			text.WriteString(blk.Text)
		case "tool_use":
			ev.ToolUses = append(ev.ToolUses, ToolUse{
				ID:    blk.ID,
				Name:  blk.Name,
				Input: blk.Input,
			})
		}
	}
	ev.Text = text.String()
}

// decodeResult populates ev.Result from a type=="result" line. Same
// tolerance rule as decodeAssistant: a shape mismatch leaves ev.Result nil
// rather than erroring, though in practice every observed result line
// decodes cleanly. NOTE: every result line in the golden corpus is
// is_error==false / subtype=="success" — the one is_error==true in the
// corpus is a nested tool_result block (a failed Bash command), decoded via
// the assistant/user path, NOT a terminal Result. The result-line failure
// branch (is_error==true here) is therefore uncovered by golden data; the
// orchestrator's failure mapping (m4.md §8.1) must not lean on it untested.
func decodeResult(b []byte, ev *Event) {
	var wire resultWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return
	}

	denials := make([]Denial, 0, len(wire.PermissionDenials))
	for _, d := range wire.PermissionDenials {
		denials = append(denials, Denial{ToolUseID: d.ToolUseID, ToolName: d.ToolName})
	}

	ev.Result = &Result{
		IsError:      wire.IsError,
		TotalCostUSD: wire.TotalCostUSD,
		Usage:        wire.Usage,
		Denials:      denials,
		StopReason:   wire.StopReason,
		NumTurns:     wire.NumTurns,
	}
}
