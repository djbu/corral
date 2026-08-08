package contract

// Real-driver helpers (CORRAL_CONTRACT=1 only). These support runReal, which
// has NEVER executed in this environment — see the note on runReal. They are
// deliberately thin: a fresh git repo is the whole environment requirement for
// the three real cases (blocked_permission, the only case that needed a forced
// permission dialog, is fake-only).

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// gitInitTempRepo creates a throwaway git repo and returns its path. Real
// claude expects to run inside a repo; a bare temp dir is enough. Fails the
// test early if git is unavailable, so a misconfigured CI runner reports the
// real cause instead of a downstream timeout.
func gitInitTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
	return dir
}

// logAutoModeConfig runs `claude auto-mode config` and prints its output into
// the test report (A.7: the real driver records the account's Auto Mode
// configuration so a divergence can be read against the exact settings that
// produced it). Best-effort: a failure is logged, not fatal — the config dump
// is diagnostic context, not a precondition.
func logAutoModeConfig(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "auto-mode", "config").CombinedOutput()
	if err != nil {
		t.Logf("auto-mode config (non-fatal): %v\n%s", err, out)
		return
	}
	t.Logf("auto-mode config:\n%s", out)
}
