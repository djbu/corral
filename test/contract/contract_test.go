package contract

// The fakeclaude-vs-real-claude contract suite (design doc §10, Amendment
// A.7). Each case runs under one or two drivers — the fake driver always
// (default `go test`), the real driver only under CORRAL_CONTRACT=1 — and both
// drivers are judged by ONE shared assertion set, assertExpectations.
//
// The suite's integrity claim is structural, not documentary: observe() and
// assertExpectations() take only an Observation (built purely from the
// daemon's own /v1/sessions API — the events table plus the session row) and
// an Expectations. Neither takes a *testing.T carrying driver context nor any
// driver handle, so an assertion that special-cases the fake is not
// expressible without adding a parameter that would show up in review. That
// replaces "byte-identical daemon" as the reason the fake cannot be caught
// lying: both drivers drive a real `corral daemon` subprocess and read their
// observations back the same way.

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/djbu/corral/internal/api/client"
	"github.com/djbu/corral/internal/session"
)

// Expectations is the shared, driver-agnostic contract for one case. Every
// field is a SHAPE assertion, never equality: real claude emits extra hooks
// (PreCompact, extra PreToolUse for reads we did not ask for) that a strict
// match would make permanently red.
type Expectations struct {
	// HookSubsequence must appear, in order, as a subsequence of the
	// hook.received event names the daemon recorded (extra hooks between them
	// are fine).
	HookSubsequence []string

	// RequiredFields maps a SEMANTIC event kind (permission.requested,
	// subagent.started, ...) to fields that must be present and non-empty on
	// a row of that kind. It deliberately targets semantic rows, not
	// hook.received: hook.received stores only {"event":"<Name>"}
	// (handlers_hooks.go), so the hook payload's own fields (tool_input,
	// permission_suggestions) are not in the events table and cannot be
	// asserted here — the engine's re-emitted rows are.
	RequiredFields map[string][]string

	// FinalState is the agent_state the daemon has computed by the terminal
	// condition — read from the session row (SessionInfo.AgentState), the
	// engine's own output, not re-derived by the test.
	//
	// The spec (§10) typed this as a StateTimeline []session.AgentState. There
	// is no state.changed event, so a timeline could only be reconstructed by
	// re-applying the engine's own transition rules to the event stream — a
	// tautology that cannot fail independently of HookSubsequence. The durable,
	// non-circular observable is the terminal state. If a state.changed event
	// is ever added this can grow back into a real timeline.
	FinalState session.AgentState

	// BlockedKind, when set, asserts SessionInfo.BlockedReason.kind. Only
	// meaningful for the blocked case.
	BlockedKind string
}

// Case is one contract scenario. Scenario names the fake driver's scenario
// JSON; Prompt is what the real driver types into claude (and, harmlessly, is
// also delivered to the fake via the answer endpoint so the two drivers share
// one drive path — scenarios that self-drive their turn simply ignore it).
type Case struct {
	Name     string
	Scenario string
	Prompt   string
	// FakeOnly excludes the case from the real driver. Two cases are fake-only,
	// each for a reason recorded at the case:
	FakeOnly bool
	// SettleMs, when set, overrides the daemon's CORRAL_STATE_PERMISSION_SETTLE
	// so a settle-driven block fires within the poll budget on the real clock
	// (the contract daemon is a real subprocess; there is no FakeClock to
	// advance from the test).
	SettleMs string
	Expect   Expectations
}

var contractCases = []Case{
	{
		Name:     "smoke_turn",
		Scenario: "smoke_turn.json",
		Prompt:   "hello",
		Expect: Expectations{
			HookSubsequence: []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"},
			FinalState:      session.AgentIdle,
		},
	},
	{
		Name:     "tool_use",
		Scenario: "tool_use.json",
		Prompt:   "list the files",
		Expect: Expectations{
			HookSubsequence: []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"},
			FinalState:      session.AgentIdle,
		},
	},
	{
		Name:     "subagent_attribution",
		Scenario: "subagent_turn.json",
		Prompt:   "use a subagent to grep for foo",
		Expect: Expectations{
			HookSubsequence: []string{"UserPromptSubmit", "SubagentStart", "PreToolUse", "PostToolUse", "SubagentStop", "Stop"},
			RequiredFields: map[string][]string{
				"subagent.started": {"agent_id", "agent_type"},
				"subagent.stopped": {"agent_id", "agent_type"},
			},
			FinalState: session.AgentIdle,
		},
	},
	{
		// permission_request_observed is FAKE-ONLY despite A.7 typing it
		// both-driver. Step-0 V21 established that a pinned settings file does
		// NOT deterministically produce a permission dialog, so real claude
		// does not reliably fire PermissionRequest at all — and HookSubsequence
		// containing "PermissionRequest" is a hard assertion, which V21's
		// non-determinism would leave red on any account. A.7 kept it
		// both-driver on the strength of PermissionOutcome being
		// recorded-not-asserted, but outcome-agnosticism does not rescue a hook
		// that never fires. Fake-only, per V21.
		Name:     "permission_request_observed",
		Scenario: "auto_mode_permission.json",
		Prompt:   "run the tests",
		FakeOnly: true,
		Expect: Expectations{
			HookSubsequence: []string{"UserPromptSubmit", "PreToolUse", "PermissionRequest", "PostToolUse", "Stop"},
			RequiredFields: map[string][]string{
				"permission.requested": {"prompt_id", "tool_name"},
				// A.7: assert the request reaches SOME terminal outcome (not
				// which one). A resolved row with a non-empty outcome is that
				// terminal proof.
				"permission.resolved": {"outcome", "prompt_id"},
			},
			FinalState: session.AgentIdle,
		},
	},
	{
		// blocked_permission is FAKE-ONLY (A.7): it drives the settle timer to
		// completion with no resolution, which real claude cannot be made to do
		// deterministically (V21). Here the real clock plus a tiny
		// permission_settle produces the block; the FakeClock path A.7
		// describes is exercised by the engine unit tests (step 6b).
		Name:     "blocked_permission",
		Scenario: "blocked_permission.json",
		Prompt:   "run the tests",
		FakeOnly: true,
		SettleMs: "200ms",
		Expect: Expectations{
			HookSubsequence: []string{"UserPromptSubmit", "PreToolUse", "PermissionRequest"},
			RequiredFields: map[string][]string{
				"permission.requested": {"prompt_id", "tool_name"},
				"permission.blocked":   {"prompt_id", "tool_name"},
			},
			FinalState:  session.AgentBlocked,
			BlockedKind: "permission",
		},
	},
}

// Observation is everything the assertions may look at, derived PURELY from
// the daemon's /v1/sessions API: the events table (evs) and the session row
// (sess). It carries no driver identity — both drivers build it the same way,
// so an assertion cannot ask "am I looking at the fake?".
type Observation struct {
	Events      []client.EventInfo
	Hooks       []string           // hook.received event names, in table order
	Kinds       []string           // every event kind, in table order
	FinalState  session.AgentState // SessionInfo.AgentState at the terminal condition
	BlockedKind string             // SessionInfo.BlockedReason.kind, "" if not blocked
	PermOutcome string             // RECORDED, NEVER ASSERTED (A.7)
}

func observe(evs []client.EventInfo, sess client.SessionInfo) Observation {
	obs := Observation{
		Events:     evs,
		FinalState: session.AgentState(sess.AgentState),
	}
	for _, e := range evs {
		obs.Kinds = append(obs.Kinds, e.Kind)
		if h := hookName(e); h != "" {
			obs.Hooks = append(obs.Hooks, h)
		}
	}
	obs.PermOutcome = derivePermOutcome(evs)
	if len(sess.BlockedReason) > 0 {
		var br struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(sess.BlockedReason, &br) == nil {
			obs.BlockedKind = br.Kind
		}
	}
	return obs
}

// derivePermOutcome reports the permission story's end state for the report,
// never for an assertion (A.7): a settle-driven block ("blocked") wins,
// otherwise the last resolved outcome, otherwise "".
func derivePermOutcome(evs []client.EventInfo) string {
	outcome := ""
	for _, e := range evs {
		switch e.Kind {
		case "permission.blocked":
			return "blocked"
		case "permission.resolved":
			var v struct {
				Outcome string `json:"outcome"`
			}
			if json.Unmarshal(e.Data, &v) == nil && v.Outcome != "" {
				outcome = v.Outcome
			}
		}
	}
	return outcome
}

func assertExpectations(t *testing.T, obs Observation, exp Expectations) {
	t.Helper()

	if !isSubsequence(exp.HookSubsequence, obs.Hooks) {
		t.Errorf("hook subsequence not satisfied\n  want (in order): %v\n  got hooks:       %v", exp.HookSubsequence, obs.Hooks)
	}

	for kind, fields := range exp.RequiredFields {
		assertRowFields(t, obs.Events, kind, fields)
	}

	if exp.FinalState != "" && obs.FinalState != exp.FinalState {
		t.Errorf("final agent_state = %q, want %q (kinds: %v)", obs.FinalState, exp.FinalState, obs.Kinds)
	}

	if exp.BlockedKind != "" && obs.BlockedKind != exp.BlockedKind {
		t.Errorf("blocked reason kind = %q, want %q", obs.BlockedKind, exp.BlockedKind)
	}

	// PermissionOutcome is the honest shape of the test: recorded and printed,
	// never asserted, because the outcome is a property of the account and only
	// the event shape is a contract (A.7).
	t.Logf("PermissionOutcome=%q (recorded, not asserted) | hooks=%v", obs.PermOutcome, obs.Hooks)
}

// isSubsequence reports whether want appears, in order, as a subsequence of
// got. An empty want is trivially satisfied.
func isSubsequence(want, got []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// assertRowFields finds a row of kind and asserts each field is present and
// non-empty in its Data JSON. A missing row is itself a failure — the field
// contract cannot hold for a kind that never appeared.
func assertRowFields(t *testing.T, evs []client.EventInfo, kind string, fields []string) {
	t.Helper()
	for _, e := range evs {
		if e.Kind != kind {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(e.Data, &m); err != nil {
			t.Errorf("%s row: data is not a JSON object: %v (data=%s)", kind, err, e.Data)
			return
		}
		for _, f := range fields {
			raw, ok := m[f]
			if !ok || isEmptyJSON(raw) {
				t.Errorf("%s row: field %q missing or empty (data=%s)", kind, f, e.Data)
			}
		}
		return // first matching row is enough
	}
	t.Errorf("no %q row found in events table (want fields %v)", kind, fields)
}

// isEmptyJSON reports whether raw is a JSON null, empty string, or literally
// absent — the "non-empty" bar RequiredFields sets.
func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "" || s == "null" || s == `""`
}

// drive runs one case against an already-started daemon: create a session,
// deliver the prompt, wait for the case's terminal condition, then read the
// events table and session row into an Observation. Identical for both drivers
// — the only difference between them is how the daemon was started.
func drive(t *testing.T, h *daemonHandle, tc Case, budget time.Duration) Observation {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := h.client.CreateSession(ctx, client.CreateSessionRequest{
		Name: tc.Name, Cwd: t.TempDir(), Model: modelFor(tc),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if tc.Prompt != "" {
		if _, err := h.client.Answer(ctx, sess.ID, tc.Prompt+"\n", "", true); err != nil {
			t.Fatalf("Answer: %v", err)
		}
	}

	// Terminal condition: a blocked case ends when the settle timer emits
	// permission.blocked; every other case ends at the turn's Stop hook.
	terminal := func(evs []client.EventInfo) bool { return containsHook(evs, "Stop") }
	if tc.Expect.FinalState == session.AgentBlocked {
		terminal = func(evs []client.EventInfo) bool { return containsKind(evs, "permission.blocked") }
	}
	evs := pollEvents(t, h.client, sess.ID, budget, terminal)

	// agent_state is recomputed on the engine's own goroutine, which lags the
	// hook.received row the HTTP handler wrote — poll the session row up to the
	// expected state rather than reading once and racing the recompute.
	final := pollState(t, h.client, sess.ID, 5*time.Second, tc.Expect.FinalState)

	return observe(evs, final)
}

func modelFor(tc Case) string {
	// The real driver pins haiku for cost (A.7); the fake ignores model.
	if os.Getenv("CORRAL_CONTRACT") == "1" && !tc.FakeOnly {
		return "haiku"
	}
	return ""
}

// pollState reads the session row until AgentState == want or the budget
// elapses, returning the last snapshot seen either way (assertExpectations
// reports the mismatch if it never converged).
func pollState(t *testing.T, c *client.Client, id string, budget time.Duration, want session.AgentState) client.SessionInfo {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last client.SessionInfo
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		s, err := c.GetSession(ctx, id)
		cancel()
		if err == nil {
			last = s
			if want == "" || session.AgentState(s.AgentState) == want {
				return s
			}
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// runFake drives the case through a real daemon that spawns fakeclaude with the
// case's scenario. The env-passthrough plumbing (CORRAL_SESSION_ENV_PASSTHROUGH
// + the daemon carrying CORRAL_FAKE_SCENARIO in its own env) is what the
// env-snapshot bugfix made work; SettleMs is a daemon config var, not a
// passthrough name, so it rides in extraEnv alongside — never in the
// passthrough list.
func runFake(t *testing.T, tc Case) Observation {
	extra := fakeDaemonEnv(t, tc.Scenario)
	if tc.SettleMs != "" {
		extra = append(extra, "CORRAL_STATE_PERMISSION_SETTLE="+tc.SettleMs)
	}
	h := startDaemon(t, extra...)
	return drive(t, h, tc, 20*time.Second)
}

// runReal drives the case through a real daemon that spawns the real `claude`
// binary on PATH (no CORRAL_FAKE_* env) in a fresh git repo, pinning haiku for
// cost. A fresh `git init` cwd is the whole environment requirement for the
// three real cases; no pinned-settings machinery is needed, since the only
// case that required forcing a permission dialog (blocked_permission) is
// fake-only.
//
// NOTE: runReal has NEVER been executed — this environment has no claude
// account. It is gated behind CORRAL_CONTRACT=1 and runs nightly / on a
// claude-version-change trigger in CI, never on push. Treat it as unverified
// until that CI job first goes green.
func runReal(t *testing.T, tc Case) Observation {
	gitInitTempRepo(t) // fail early if git is unavailable
	logAutoModeConfig(t)
	h := startDaemon(t)
	return drive(t, h, tc, realBudget())
}

func realBudget() time.Duration {
	if s := os.Getenv("CORRAL_CONTRACT_TIMEOUT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 120 * time.Second
}

func TestContract(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real binaries and spawns a real daemon; skipped in -short")
	}
	realEnabled := os.Getenv("CORRAL_CONTRACT") == "1"

	for _, tc := range contractCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Run("fake", func(t *testing.T) {
				assertExpectations(t, runFake(t, tc), tc.Expect)
			})
			if realEnabled && !tc.FakeOnly {
				t.Run("real", func(t *testing.T) {
					assertExpectations(t, runReal(t, tc), tc.Expect)
				})
			}
		})
	}
}
