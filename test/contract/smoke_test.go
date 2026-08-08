package contract

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/api/client"
)

// scenarioPath returns the absolute path to a fakeclaude scenario JSON, so it
// survives being handed to a daemon subprocess with a different cwd.
func scenarioPath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "fakeclaude", "testdata", "scenarios", name))
	if err != nil {
		t.Fatalf("resolving scenario %s: %v", name, err)
	}
	return p
}

// fakeDaemonEnv is the daemon-side environment for the fake driver: point the
// daemon at the fakeclaude binary and BOTH (a) set CORRAL_FAKE_SCENARIO in the
// daemon's own env AND (b) list it in CORRAL_SESSION_ENV_PASSTHROUGH so the
// daemon forwards it to the spawned fakeclaude child (two layers — the
// daemon can only forward vars it actually has). CORRAL_FAKE_STATE/_HOME are
// forwarded too so the fake behaves deterministically.
func fakeDaemonEnv(t *testing.T, scenario string) []string {
	b := buildBinaries(t)
	fakeState := t.TempDir()
	return []string{
		"CORRAL_SESSION_CLAUDE_BIN=" + b.fakeclaude,
		"CORRAL_SESSION_ENV_PASSTHROUGH=CORRAL_FAKE_SCENARIO,CORRAL_FAKE_STATE,CORRAL_FAKE_HOME",
		"CORRAL_FAKE_SCENARIO=" + scenarioPath(t, scenario),
		"CORRAL_FAKE_STATE=" + fakeState,
	}
}

// pollEvents polls GET /v1/sessions/{id}/events until pred is satisfied or the
// budget elapses, returning the full event slice seen last. The terminal
// condition is an EVENT in the daemon's table, never process exit — fake
// scenarios exit 0 while real claude parks in its TUI, so waiting on the
// process would diverge the two drivers in the waiting code.
func pollEvents(t *testing.T, c *client.Client, id string, budget time.Duration, pred func([]client.EventInfo) bool) []client.EventInfo {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last []client.EventInfo
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		evs, err := c.ListEvents(ctx, id)
		cancel()
		if err == nil {
			last = evs
			if pred(evs) {
				return evs
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

// hookName extracts the "event" field from a hook.received row's data JSON.
func hookName(e client.EventInfo) string {
	if e.Kind != "hook.received" {
		return ""
	}
	var v struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal(e.Data, &v)
	return v.Event
}

func containsKind(evs []client.EventInfo, kind string) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func containsHook(evs []client.EventInfo, hook string) bool {
	for _, e := range evs {
		if hookName(e) == hook {
			return true
		}
	}
	return false
}

// TestContractSmokePlumbing is the one-case smoke run the harness design calls
// for BEFORE the full table: it proves the two env layers and the relay
// resolution actually deliver hooks into the events table. If this fails with
// an empty table, suspect CORRAL_SESSION_ENV_PASSTHROUGH first and the relay
// command second.
func TestContractSmokePlumbing(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real binaries and spawns a real daemon; skipped in -short")
	}
	h := startDaemon(t, fakeDaemonEnv(t, "smoke_turn.json")...)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := h.client.CreateSession(ctx, client.CreateSessionRequest{
		Name: "smoke", Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// The scenario blocks on wait_stdin; deliver the prompt exactly the way a
	// human would — via the answer endpoint. Both drivers use this same call.
	if _, err := h.client.Answer(ctx, sess.ID, "hello\n", "", true); err != nil {
		t.Fatalf("Answer: %v", err)
	}

	evs := pollEvents(t, h.client, sess.ID, 15*time.Second, func(evs []client.EventInfo) bool {
		return containsHook(evs, "Stop")
	})

	var kinds []string
	for _, e := range evs {
		if h := hookName(e); h != "" {
			kinds = append(kinds, e.Kind+"("+h+")")
		} else {
			kinds = append(kinds, e.Kind)
		}
	}
	t.Logf("observed %d events: %v", len(evs), kinds)

	for _, want := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"} {
		if !containsHook(evs, want) {
			t.Errorf("missing hook %q in events table", want)
		}
	}
}
