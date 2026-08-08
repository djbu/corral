// Package automode best-effort detects whether Claude Code's Auto Mode is
// active by running `claude auto-mode config` and inspecting its output.
// Corral never toggles or configures Auto Mode — it only observes it, so a
// human can be told (at daemon startup and via `corral doctor`) when
// permission requests may auto-resolve without them (design doc Amendment
// A.6). The probe is read-only and non-fatal: it never mutates ~/.claude and
// never returns an error, matching the discipline of the daemon's other
// startup probes.
package automode

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

// ActiveMessage is the exact human-facing sentence design doc Amendment A.6
// requires whenever Auto Mode is reported active — emitted by both the daemon
// startup log and `corral doctor`, kept here as the single source of truth.
const ActiveMessage = "Auto Mode is active; permission requests may auto-resolve without a human, so blocked will be rare and permission_settle governs detection."

// probeTimeout mirrors probeClaudeBin's 3s budget (design doc §3.2 step 7).
const probeTimeout = 3 * time.Second

// Status is the (deliberately bi-state) result of a best-effort probe.
// Only Active is positively observable: `claude auto-mode config` emits a
// well-formed policy document exactly when Auto Mode is engaged. The
// off-state's output shape is unverified (the account this was built on keeps
// Auto Mode on), so everything else — binary absent, command error/timeout,
// unrecognizable output — collapses to Unknown rather than a fabricated
// "inactive". A false "you are unsupervised" is worse than silence (A.6), so
// callers must never render Unknown as active.
type Status int

const (
	// StatusUnknown: the probe could not determine Auto Mode's state.
	StatusUnknown Status = iota
	// StatusActive: the probe returned a well-formed policy document.
	StatusActive
)

// Result is a classified probe outcome.
type Result struct {
	Status Status

	// Raw is the trimmed combined output of the probe, retained ONLY on
	// StatusUnknown so `corral doctor` can print it verbatim and a human can
	// see why detection failed (the failure output is a short error string).
	// Left empty on StatusActive: the policy document embeds environment/repo
	// context and long narrative deny rules that are noisy and needlessly
	// sensitive to echo — A.6 asks only that we report *that* Auto Mode is
	// active, never that we dump its policy.
	Raw string

	// Allow/SoftDeny/HardDenyCount summarize the policy arrays on
	// StatusActive (a one-line "allow=N soft_deny=N hard_deny=N"); all zero on
	// StatusUnknown.
	AllowCount    int
	SoftDenyCount int
	HardDenyCount int
}

// Detect runs `<bin> auto-mode config` with a 3s timeout and classifies the
// result. bin defaults to "claude" (resolved via PATH) when empty. It never
// errors: an unresolvable/failing/unrecognized probe is StatusUnknown.
func Detect(ctx context.Context, bin string) Result {
	if bin == "" {
		bin = "claude"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return Result{Status: StatusUnknown}
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// CombinedOutput so a diagnostic error printed to stderr is captured for
	// the StatusUnknown Raw path.
	out, runErr := exec.CommandContext(probeCtx, path, "auto-mode", "config").CombinedOutput()
	return classify(string(out), runErr)
}

// classify is the pure decision Detect delegates to, split out so the
// output-shape handling is unit-testable without a real claude binary.
func classify(out string, runErr error) Result {
	raw := strings.TrimSpace(out)
	if runErr != nil {
		return Result{Status: StatusUnknown, Raw: raw}
	}
	var policy struct {
		Allow    []json.RawMessage `json:"allow"`
		SoftDeny []json.RawMessage `json:"soft_deny"`
		HardDeny []json.RawMessage `json:"hard_deny"`
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return Result{Status: StatusUnknown, Raw: raw}
	}
	// A parseable but empty object ({}) is not a policy — require at least one
	// populated gate before claiming Auto Mode is active.
	if len(policy.Allow) == 0 && len(policy.SoftDeny) == 0 && len(policy.HardDeny) == 0 {
		return Result{Status: StatusUnknown, Raw: raw}
	}
	return Result{
		Status:        StatusActive,
		AllowCount:    len(policy.Allow),
		SoftDenyCount: len(policy.SoftDeny),
		HardDenyCount: len(policy.HardDeny),
	}
}
