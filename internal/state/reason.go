package state

import "encoding/json"

// BlockedReason is the JSON persisted to sessions.blocked_reason_json when a
// permission request settles unresolved (Amendment A.3.3). Stored opaquely by
// the store; owned here.
type BlockedReason struct {
	Kind                  string          `json:"kind"` // permission | plan_approval | question | unknown
	Summary               string          `json:"summary"`
	ToolName              string          `json:"tool_name"`
	ToolUseID             string          `json:"tool_use_id"` // supplied by tracker MatchTriple; "" if unmatched
	ToolInput             json.RawMessage `json:"tool_input,omitempty"`
	PermissionSuggestions json.RawMessage `json:"permission_suggestions,omitempty"` // raw passthrough
	PromptID              string          `json:"prompt_id"`
	AgentID               string          `json:"agent_id"`
	AgentType             string          `json:"agent_type"`
	PermissionMode        string          `json:"permission_mode"`
	NotificationMessage   string          `json:"notification_message"` // "" unless a permission_prompt Notification corroborated
	NotificationType      string          `json:"notification_type"`
	Confidence            string          `json:"confidence"` // "unresolved_settled" normal; "notified" if a permission_prompt Notification for same prompt_id was seen
	OpenedAt              string          `json:"opened_at"`  // RFC3339
	BlockedAt             string          `json:"blocked_at"` // RFC3339
	HookSeq               int64           `json:"hook_seq"`
	Redactions            []string        `json:"redactions"`
}

// kindForTool classifies a tool name into the BlockedReason.Kind vocabulary
// (Amendment A.3.3). ExitPlanMode is a plan-approval prompt, not a
// permission prompt; AskUserQuestion is a question prompt; everything else
// defaults to a generic permission prompt.
func kindForTool(toolName string) string {
	switch toolName {
	case "ExitPlanMode":
		return "plan_approval"
	case "AskUserFor", "AskUserQuestion":
		return "question"
	default:
		return "permission"
	}
}

// marshalBlockedReason marshals r to JSON, returning "{}" (never an error)
// on marshal failure — blocked_reason_json is best-effort persistence, and a
// marshal failure here must never abort the session loop.
func marshalBlockedReason(r BlockedReason) string {
	b, err := json.Marshal(r)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// summaryForTool builds a short, best-effort human-readable summary of a
// permission request. It never fails on unparseable tool_input — a
// generic fallback is always available.
func summaryForTool(toolName string, toolInput []byte) string {
	if toolName == "Bash" {
		var v struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(toolInput, &v); err == nil && v.Command != "" {
			cmd := v.Command
			if len(cmd) > 80 {
				cmd = cmd[:80]
			}
			return "permission to run " + cmd
		}
	}
	if toolName == "" {
		return "permission for unknown tool"
	}
	return "permission for " + toolName
}
