package settings

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const goldenRelayCommand = "'/usr/local/bin/corral' hook-relay"

// wantHookEventNames mirrors hookEvents' name/hasMatcher pairs so the test
// doesn't just re-import the same slice under test.
var wantHookEventNames = []struct {
	name       string
	hasMatcher bool
}{
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

// TestBuildSettingsJSON_Golden is a byte-comparison golden test (step 5.7):
// BuildSettingsJSON's output must exactly match
// testdata/settings.golden.json for a fixed relayCommand. Regenerate the
// golden file by deleting it and re-running the test once (it will fail and
// print the actual bytes) — or by running with -update if that flag is
// ever added; today it's a straight byte comparison against the checked-in
// file.
func TestBuildSettingsJSON_Golden(t *testing.T) {
	got, err := BuildSettingsJSON(goldenRelayCommand)
	if err != nil {
		t.Fatalf("BuildSettingsJSON: %v", err)
	}

	if !json.Valid(got) {
		t.Fatalf("BuildSettingsJSON output is not valid JSON: %s", got)
	}

	want, err := os.ReadFile("testdata/settings.golden.json")
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("BuildSettingsJSON output does not match golden file.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestBuildSettingsJSON_Shape checks the substantive invariants the golden
// test alone would only catch as an opaque byte diff: all 15 events
// present, matcher present only on tool events, every command references
// hook-relay --event <Name>, every hook has timeout:5.
func TestBuildSettingsJSON_Shape(t *testing.T) {
	out, err := BuildSettingsJSON(goldenRelayCommand)
	if err != nil {
		t.Fatalf("BuildSettingsJSON: %v", err)
	}

	var parsed struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(parsed.Hooks) != len(wantHookEventNames) {
		t.Fatalf("got %d event keys, want %d", len(parsed.Hooks), len(wantHookEventNames))
	}

	for _, ev := range wantHookEventNames {
		entries, ok := parsed.Hooks[ev.name]
		if !ok {
			t.Fatalf("missing event key %q", ev.name)
		}
		if len(entries) != 1 {
			t.Fatalf("event %q: got %d entries, want 1", ev.name, len(entries))
		}
		entry := entries[0]
		if ev.hasMatcher && entry.Matcher != "*" {
			t.Errorf("event %q: matcher = %q, want \"*\"", ev.name, entry.Matcher)
		}
		if !ev.hasMatcher && entry.Matcher != "" {
			t.Errorf("event %q: matcher = %q, want omitted (non-tool event)", ev.name, entry.Matcher)
		}
		if len(entry.Hooks) != 1 {
			t.Fatalf("event %q: got %d hooks, want 1", ev.name, len(entry.Hooks))
		}
		h := entry.Hooks[0]
		if h.Type != "command" {
			t.Errorf("event %q: type = %q, want command", ev.name, h.Type)
		}
		wantCmd := goldenRelayCommand + " --event " + ev.name
		if h.Command != wantCmd {
			t.Errorf("event %q: command = %q, want %q", ev.name, h.Command, wantCmd)
		}
		if !strings.Contains(h.Command, "hook-relay --event "+ev.name) {
			t.Errorf("event %q: command %q does not contain %q", ev.name, h.Command, "hook-relay --event "+ev.name)
		}
		if h.Timeout != 5 {
			t.Errorf("event %q: timeout = %d, want 5", ev.name, h.Timeout)
		}
	}
}

func TestBuildSettingsJSONWithRulesSortedAndDeduplicated(t *testing.T) {
	got, err := BuildSettingsJSONWithRules(goldenRelayCommand, []string{
		"Bash(npm test)", "Bash(go test ./...)", "Bash(npm test)", "",
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, got)
	}
	want := []string{"Bash(go test ./...)", "Bash(npm test)"}
	if len(parsed.Permissions.Allow) != len(want) {
		t.Fatalf("allow = %v, want %v", parsed.Permissions.Allow, want)
	}
	for i := range want {
		if parsed.Permissions.Allow[i] != want[i] {
			t.Fatalf("allow = %v, want %v", parsed.Permissions.Allow, want)
		}
	}
	if len(parsed.Hooks) != len(wantHookEventNames) {
		t.Fatalf("hooks = %d, want %d", len(parsed.Hooks), len(wantHookEventNames))
	}
}
