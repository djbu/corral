package main

import (
	"encoding/json"
	"os"
	"time"
)

// runHeadless is fakeclaude's stand-in for a real `claude -p
// --output-format stream-json` invocation (design doc §5, M4 step 19). It
// is a wholly separate code path from the M1/M2 interactive engine above
// it in main(): main() branches here (on spec.OutputFormat ==
// "stream-json") before disableEcho/writeStartup/the alt-screen dance ever
// run, since none of that belongs in a plain-pipe, no-PTY invocation.
//
// It writes every line directly to os.Stdout (never through a bufio.Writer
// left unflushed) and calls os.Exit(0) itself right after the terminal
// `result` line — matching real claude's one-shot lifecycle (§5.1: the
// child exits at result, no persistent stdin loop). Writing unbuffered is
// load-bearing: this is exactly the tail-of-stream loss corral's own
// headless reader (bufio.Reader.ReadBytes, not Scanner) is built to
// tolerate, but there is no reason for fakeclaude to manufacture that
// failure by buffering and then skipping the flush on exit.
//
// CORRAL_FAKE_HEADLESS_SCENARIO selects the scripted transcript:
//   - "" or "success" (default): system:init, one assistant text turn, a
//     result line with is_error=false.
//   - "error-turn": same shape, but the terminal result line has
//     is_error=true — synthesized, since (per streamjson's own doc
//     comment) every result line in the real golden corpus is
//     is_error==false; this is the corpus's one known gap.
//   - "slow": sleeps briefly between the assistant line and the result
//     line, for tests that need to observe a headless session still
//     running.
//
// This is a new, headless-only env var, deliberately distinct from
// CORRAL_FAKE_SCENARIO (the M2 interactive scenario-script path) — the two
// scenario engines are unrelated and must never be confused.
func runHeadless(spec fakeSpec, sessionID, cwd string) {
	scenario := os.Getenv("CORRAL_FAKE_HEADLESS_SCENARIO")
	if scenario == "" {
		scenario = "success"
	}

	writeHeadlessLine(map[string]any{
		"type":                "system",
		"subtype":             "init",
		"cwd":                 cwd,
		"session_id":          sessionID,
		"tools":               []string{},
		"mcp_servers":         []string{},
		"model":               fakeModelOr(spec.Model),
		"permissionMode":      fakePermissionModeOr(spec.PermissionMode),
		"slash_commands":      []string{},
		"apiKeySource":        "none",
		"claude_code_version": "0.0.0-fakeclaude",
	})

	writeHeadlessLine(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "fakeclaude headless reply to: " + spec.Prompt},
			},
		},
		"session_id": sessionID,
	})

	if scenario == "slow" {
		time.Sleep(300 * time.Millisecond)
	}

	isError := scenario == "error-turn"
	stopReason := "end_turn"
	if isError {
		stopReason = "error_during_execution"
	}
	writeHeadlessLine(map[string]any{
		"type":            "result",
		"subtype":         resultSubtype(isError),
		"is_error":        isError,
		"duration_api_ms": 10,
		"num_turns":       1,
		"stop_reason":     stopReason,
		"session_id":      sessionID,
		"total_cost_usd":  0.001,
		"usage": map[string]any{
			"input_tokens":                10,
			"output_tokens":               5,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		},
	})

	os.Exit(0)
}

func resultSubtype(isError bool) string {
	if isError {
		return "error_during_execution"
	}
	return "success"
}

func fakeModelOr(model string) string {
	if model == "" {
		return "claude-fake"
	}
	return model
}

func fakePermissionModeOr(mode string) string {
	if mode == "" {
		return "default"
	}
	return mode
}

// writeHeadlessLine marshals v and writes it to os.Stdout as one
// newline-terminated line via a single unbuffered os.Stdout.Write — see
// runHeadless's doc comment on why this must never go through a
// bufio.Writer that could be left unflushed at os.Exit.
func writeHeadlessLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fatalf("headless: marshaling line: %v", err)
	}
	b = append(b, '\n')
	if _, err := os.Stdout.Write(b); err != nil {
		fatalf("headless: writing line: %v", err)
	}
}
