package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// hookCmd is one entry of a hookEntry's "hooks" array: the command Claude
// Code execs for a matched event.
type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// hookEntry is one element of an event's top-level array value. Matcher is
// omitted entirely (via omitempty) for non-tool events, which Claude Code's
// settings.json schema does not expect a matcher on.
type hookEntry struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []hookCmd `json:"hooks"`
}

// hookEventSpec is one of the 15 registered Claude Code hook events
// (Amendment A.2) and whether it takes a "matcher":"*" (tool events) or
// omits matcher entirely (non-tool events).
type hookEventSpec struct {
	name       string
	hasMatcher bool
}

// hookEvents is the design doc's exact top-level key order — the golden
// test in hooks_test.go asserts byte-stable output, so this order must
// never change without also regenerating the golden file.
var hookEvents = []hookEventSpec{
	{"SessionStart", false},
	{"UserPromptSubmit", false},
	{"Notification", false},
	{"Stop", false},
	{"StopFailure", false},
	{"SubagentStart", false},
	{"SubagentStop", false},
	{"TeammateIdle", false},
	{"PreCompact", false},
	{"SessionEnd", false},
	{"PreToolUse", true},
	{"PermissionRequest", true},
	{"PostToolUse", true},
	{"PostToolUseFailure", true},
	{"PostToolBatch", true},
}

// BuildSettingsJSON returns the exact bytes to write to settings.json for a
// supervised session (Amendment A.2): a hooks{} block registering all 15
// Claude Code hook events, each invoking the corral relay. relayCommand is
// the already-resolved, shell-safe command prefix (absolute corral binary
// path + " hook-relay"); this function appends " --event <Name>" per event.
// Tool events (PreToolUse, PermissionRequest, PostToolUse, PostToolUseFailure,
// PostToolBatch) get "matcher":"*"; non-tool events omit matcher. Each hook
// carries "timeout":5 (seconds). Output is deterministic (stable key order)
// so the golden test is a byte comparison.
func BuildSettingsJSON(relayCommand string) ([]byte, error) {
	return BuildSettingsJSONWithRules(relayCommand, nil)
}

// BuildSettingsJSONWithRules adds exact, verified permission allow rules to
// the pinned settings. Sorting and deduplication make the output stable across
// store/query order. The compatibility wrapper above preserves M1-M5 bytes
// when no rules are present.
func BuildSettingsJSONWithRules(relayCommand string, rules []string) ([]byte, error) {
	rules = append([]string(nil), rules...)
	sort.Strings(rules)
	unique := rules[:0]
	for _, rule := range rules {
		if rule == "" || (len(unique) > 0 && unique[len(unique)-1] == rule) {
			continue
		}
		unique = append(unique, rule)
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	if len(unique) > 0 {
		permissions, err := json.Marshal(struct {
			Allow []string `json:"allow"`
		}{Allow: unique})
		if err != nil {
			return nil, fmt.Errorf("settings: marshaling permission rules: %w", err)
		}
		buf.WriteString(`"permissions":`)
		buf.Write(permissions)
		buf.WriteByte(',')
	}
	buf.WriteString(`"hooks":{`)

	for i, ev := range hookEvents {
		if i > 0 {
			buf.WriteByte(',')
		}

		entry := hookEntry{
			Hooks: []hookCmd{{
				Type:    "command",
				Command: fmt.Sprintf("%s --event %s", relayCommand, ev.name),
				Timeout: 5,
			}},
		}
		if ev.hasMatcher {
			entry.Matcher = "*"
		}

		entryJSON, err := json.Marshal([]hookEntry{entry})
		if err != nil {
			return nil, fmt.Errorf("settings: marshaling hook entry for %s: %w", ev.name, err)
		}

		keyJSON, err := json.Marshal(ev.name)
		if err != nil {
			return nil, fmt.Errorf("settings: marshaling event name %s: %w", ev.name, err)
		}

		buf.Write(keyJSON)
		buf.WriteByte(':')
		buf.Write(entryJSON)
	}

	buf.WriteString(`}}`)
	return buf.Bytes(), nil
}
