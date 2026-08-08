package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// ForeignHookEvents best-effort reads <claudeHome>/settings.json and returns
// the sorted event names under its top-level "hooks" object — hooks a user
// configured outside corral. Read-only, never mutates the file, and never
// errors: a missing/unreadable/unparseable file simply yields nil. Corral's
// own pinned settings.json lives per-session under stateDir, never here, so
// anything found is genuinely foreign (design doc Amendments A.2/A.5).
//
// Scope: this reads user-scope settings (~/.claude/settings.json) only. A.5's
// "effective set per setting_sources" also spans project/local settings;
// `corral doctor` reports this user-scope subset and says so explicitly,
// rather than silently implying it covers project/local too.
func ForeignHookEvents(claudeHome string) []string {
	if claudeHome == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(claudeHome, "settings.json"))
	if err != nil {
		return nil
	}
	var parsed struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil
	}
	if len(parsed.Hooks) == 0 {
		return nil
	}
	names := make([]string, 0, len(parsed.Hooks))
	for k := range parsed.Hooks {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
