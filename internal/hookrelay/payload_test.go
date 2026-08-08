package hookrelay

import (
	"os"
	"path/filepath"
	"testing"
)

// fixturesDir is testdata/hooks/2.1.224/ at the repo root — real Claude
// Code 2.1.224 hook payloads captured during design-doc step 0 (see design
// doc §11, Amendment A.1). Every one of the 15 files there gets exactly one
// case below, asserting the load-bearing fields Amendment A.1's table says
// that event carries extract correctly.
func fixturesDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "testdata", "hooks", "2.1.224"))
	if err != nil {
		t.Fatalf("resolving fixtures dir: %v", err)
	}
	return dir
}

func readFixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return b
}

// TestDecode_Fixtures is table-driven, one case per file in
// testdata/hooks/2.1.224/ (15 files, verified by TestDecode_AllFixturesCovered
// below), asserting the fields Amendment A.1 says that hook_event_name
// carries.
func TestDecode_Fixtures(t *testing.T) {
	dir := fixturesDir(t)

	tests := []struct {
		file  string
		check func(t *testing.T, p HookPayload)
	}{
		{
			file: "notification-permission.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "Notification" {
					t.Errorf("HookEventName = %q, want Notification", p.HookEventName)
				}
				if p.SessionID != "4ab7095e-44cd-45ea-b850-3ba9e8db2b0f" {
					t.Errorf("SessionID = %q", p.SessionID)
				}
				if p.PromptID != "94be636c-8ed0-457c-9255-8e49c58bd8fa" {
					t.Errorf("PromptID = %q", p.PromptID)
				}
				if p.Message != "Claude needs your permission" {
					t.Errorf("Message = %q", p.Message)
				}
				if p.NotificationType != "permission_prompt" {
					t.Errorf("NotificationType = %q", p.NotificationType)
				}
				if p.PermissionMode != "" {
					t.Errorf("PermissionMode = %q, want empty (Notification carries none)", p.PermissionMode)
				}
			},
		},
		{
			file: "permissionrequest-bash.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "PermissionRequest" {
					t.Errorf("HookEventName = %q, want PermissionRequest", p.HookEventName)
				}
				if p.ToolName != "Bash" {
					t.Errorf("ToolName = %q, want Bash", p.ToolName)
				}
				if p.ToolUseID != "" {
					t.Errorf("ToolUseID = %q, want empty (PermissionRequest carries none, Amendment A.3.1)", p.ToolUseID)
				}
				if p.PermissionMode != "default" {
					t.Errorf("PermissionMode = %q, want default", p.PermissionMode)
				}
				if len(p.ToolInput) == 0 {
					t.Errorf("ToolInput is empty, want raw JSON object")
				}
				if len(p.PermissionSuggestions) != 2 {
					t.Fatalf("PermissionSuggestions = %+v, want 2 entries", p.PermissionSuggestions)
				}
				if p.PermissionSuggestions[0].Type != "addDirectories" {
					t.Errorf("PermissionSuggestions[0].Type = %q, want addDirectories", p.PermissionSuggestions[0].Type)
				}
				if len(p.PermissionSuggestions[0].Directories) != 1 {
					t.Errorf("PermissionSuggestions[0].Directories = %v, want 1 entry", p.PermissionSuggestions[0].Directories)
				}
				if p.PermissionSuggestions[1].Type != "setMode" || p.PermissionSuggestions[1].Mode != "acceptEdits" {
					t.Errorf("PermissionSuggestions[1] = %+v, want setMode/acceptEdits", p.PermissionSuggestions[1])
				}
			},
		},
		{
			file: "posttooluse-agent-task.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "PostToolUse" {
					t.Errorf("HookEventName = %q, want PostToolUse", p.HookEventName)
				}
				if p.ToolName != "Agent" {
					t.Errorf("ToolName = %q, want Agent", p.ToolName)
				}
				if p.ToolUseID != "toolu_01HEqsjojJV5h8sDqZTdrnbw" {
					t.Errorf("ToolUseID = %q", p.ToolUseID)
				}
				if p.DurationMs == nil || *p.DurationMs != 2022 {
					t.Errorf("DurationMs = %v, want 2022", p.DurationMs)
				}
				if len(p.ToolResponse) == 0 {
					t.Errorf("ToolResponse is empty, want raw JSON object")
				}
				if p.PermissionMode != "bypassPermissions" {
					t.Errorf("PermissionMode = %q, want bypassPermissions", p.PermissionMode)
				}
			},
		},
		{
			file: "posttooluse-bash.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "PostToolUse" {
					t.Errorf("HookEventName = %q, want PostToolUse", p.HookEventName)
				}
				if p.ToolName != "Bash" {
					t.Errorf("ToolName = %q, want Bash", p.ToolName)
				}
				if p.ToolUseID != "toolu_01LjYJv1nneLAZXt6Chdc5LS" {
					t.Errorf("ToolUseID = %q", p.ToolUseID)
				}
				if p.DurationMs == nil || *p.DurationMs != 1184 {
					t.Errorf("DurationMs = %v, want 1184", p.DurationMs)
				}
			},
		},
		{
			file: "pretooluse-agent-task.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "PreToolUse" {
					t.Errorf("HookEventName = %q, want PreToolUse", p.HookEventName)
				}
				if p.ToolName != "Agent" {
					t.Errorf("ToolName = %q, want Agent", p.ToolName)
				}
				if p.ToolUseID != "toolu_01HEqsjojJV5h8sDqZTdrnbw" {
					t.Errorf("ToolUseID = %q", p.ToolUseID)
				}
				if len(p.ToolInput) == 0 {
					t.Errorf("ToolInput is empty, want raw JSON object")
				}
			},
		},
		{
			file: "pretooluse-bash.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "PreToolUse" {
					t.Errorf("HookEventName = %q, want PreToolUse", p.HookEventName)
				}
				if p.ToolName != "Bash" {
					t.Errorf("ToolName = %q, want Bash", p.ToolName)
				}
				if p.ToolUseID != "toolu_01LjYJv1nneLAZXt6Chdc5LS" {
					t.Errorf("ToolUseID = %q", p.ToolUseID)
				}
			},
		},
		{
			file: "sessionend-other.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "SessionEnd" {
					t.Errorf("HookEventName = %q, want SessionEnd", p.HookEventName)
				}
				if p.Reason != "other" {
					t.Errorf("Reason = %q, want other", p.Reason)
				}
				if p.PromptID == "" {
					t.Errorf("PromptID is empty, want set (SessionEnd carries it per Amendment A.1)")
				}
			},
		},
		{
			file: "sessionstart-resume.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "SessionStart" {
					t.Errorf("HookEventName = %q, want SessionStart", p.HookEventName)
				}
				if p.Source != "resume" {
					t.Errorf("Source = %q, want resume", p.Source)
				}
				if p.PromptID != "" {
					t.Errorf("PromptID = %q, want empty (SessionStart carries none)", p.PromptID)
				}
			},
		},
		{
			file: "sessionstart-startup.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "SessionStart" {
					t.Errorf("HookEventName = %q, want SessionStart", p.HookEventName)
				}
				if p.Source != "startup" {
					t.Errorf("Source = %q, want startup", p.Source)
				}
			},
		},
		{
			file: "stop-hook-active-true.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "Stop" {
					t.Errorf("HookEventName = %q, want Stop", p.HookEventName)
				}
				if !p.StopHookActive {
					t.Errorf("StopHookActive = false, want true")
				}
				if p.LastAssistantMessage != "AGAIN. Testing complete." {
					t.Errorf("LastAssistantMessage = %q", p.LastAssistantMessage)
				}
				if p.BackgroundTasks == nil || len(p.BackgroundTasks) != 0 {
					t.Errorf("BackgroundTasks = %v, want empty slice, not nil", p.BackgroundTasks)
				}
			},
		},
		{
			file: "stop-normal.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "Stop" {
					t.Errorf("HookEventName = %q, want Stop", p.HookEventName)
				}
				if p.StopHookActive {
					t.Errorf("StopHookActive = true, want false")
				}
				if p.LastAssistantMessage != "DONE" {
					t.Errorf("LastAssistantMessage = %q", p.LastAssistantMessage)
				}
			},
		},
		{
			file: "subagentstart.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "SubagentStart" {
					t.Errorf("HookEventName = %q, want SubagentStart", p.HookEventName)
				}
				if p.AgentID != "a6e8ae4378e325857" {
					t.Errorf("AgentID = %q", p.AgentID)
				}
				if p.AgentType != "general-purpose" {
					t.Errorf("AgentType = %q", p.AgentType)
				}
			},
		},
		{
			file: "subagentstop-empty-agenttype.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "SubagentStop" {
					t.Errorf("HookEventName = %q, want SubagentStop", p.HookEventName)
				}
				if p.AgentType != "" {
					t.Errorf("AgentType = %q, want empty", p.AgentType)
				}
				if p.AgentID != "a0b2232d48f38e959" {
					t.Errorf("AgentID = %q", p.AgentID)
				}
				if p.AgentTranscriptPath == "" {
					t.Errorf("AgentTranscriptPath is empty, want set")
				}
			},
		},
		{
			file: "subagentstop.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "SubagentStop" {
					t.Errorf("HookEventName = %q, want SubagentStop", p.HookEventName)
				}
				if p.AgentType != "general-purpose" {
					t.Errorf("AgentType = %q", p.AgentType)
				}
				if p.LastAssistantMessage != "SUBOK" {
					t.Errorf("LastAssistantMessage = %q", p.LastAssistantMessage)
				}
			},
		},
		{
			file: "userpromptsubmit.json",
			check: func(t *testing.T, p HookPayload) {
				if p.HookEventName != "UserPromptSubmit" {
					t.Errorf("HookEventName = %q, want UserPromptSubmit", p.HookEventName)
				}
				if p.Prompt == "" {
					t.Errorf("Prompt is empty, want set")
				}
				if p.PromptID == "" {
					t.Errorf("PromptID is empty, want set")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			b := readFixture(t, dir, tt.file)
			p, err := Decode(b)
			if err != nil {
				t.Fatalf("Decode(%s): %v", tt.file, err)
			}
			if string(p.Raw) != string(b) {
				t.Errorf("Raw does not match input bytes verbatim")
			}
			tt.check(t, p)
		})
	}
}

// TestDecode_AllFixturesCovered guards against the table above silently
// drifting out of sync with testdata/hooks/2.1.224/ — every file there must
// have exactly one case in TestDecode_Fixtures.
func TestDecode_AllFixturesCovered(t *testing.T) {
	dir := fixturesDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	want := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			want[e.Name()] = true
		}
	}
	if len(want) != 15 {
		t.Fatalf("found %d fixture files in %s, want 15: %v", len(want), dir, want)
	}

	got := map[string]bool{
		"notification-permission.json":      true,
		"permissionrequest-bash.json":       true,
		"posttooluse-agent-task.json":       true,
		"posttooluse-bash.json":             true,
		"pretooluse-agent-task.json":        true,
		"pretooluse-bash.json":              true,
		"sessionend-other.json":             true,
		"sessionstart-resume.json":          true,
		"sessionstart-startup.json":         true,
		"stop-hook-active-true.json":        true,
		"stop-normal.json":                  true,
		"subagentstart.json":                true,
		"subagentstop-empty-agenttype.json": true,
		"subagentstop.json":                 true,
		"userpromptsubmit.json":             true,
	}

	for name := range want {
		if !got[name] {
			t.Errorf("fixture %s has no corresponding TestDecode_Fixtures case", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("TestDecode_Fixtures has a case for %s, which no longer exists in testdata", name)
		}
	}
}
