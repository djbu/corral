package daemon

import "testing"

// TestCaptureEnvSnapshot_Passthrough documents the fixed contract: a name in
// session.env_passthrough that is NOT in the fixed envSnapshotWhitelist is
// still captured into the frozen snapshot, so supervisor.BuildEnv can forward
// it to a session child. Before this fix, captureEnvSnapshot ignored
// passthrough entirely and env_passthrough was a silent no-op.
func TestCaptureEnvSnapshot_Passthrough(t *testing.T) {
	const name = "CORRAL_TEST_PASSTHROUGH_X"
	t.Setenv(name, "value-x")

	// Sanity: the whole point is that this name is NOT already whitelisted,
	// so its capture can only come from the passthrough argument.
	for _, w := range envSnapshotWhitelist {
		if w == name {
			t.Fatalf("test var %q is in envSnapshotWhitelist; pick a name that isn't", name)
		}
	}

	snap := captureEnvSnapshot([]string{name})
	if got, ok := snap[name]; !ok || got != "value-x" {
		t.Errorf("captureEnvSnapshot passthrough: snapshot[%q] = %q (present=%v), want %q present",
			name, got, ok, "value-x")
	}

	// A passthrough name that is unset in the environment must not appear as
	// an empty string — capture is LookupEnv-gated, not blind.
	snap2 := captureEnvSnapshot([]string{"CORRAL_TEST_UNSET_Y"})
	if _, ok := snap2["CORRAL_TEST_UNSET_Y"]; ok {
		t.Errorf("captureEnvSnapshot captured an unset passthrough name; want absent")
	}

	// The fixed whitelist is still captured regardless of passthrough.
	if _, ok := captureEnvSnapshot(nil)["PATH"]; !ok {
		t.Errorf("captureEnvSnapshot(nil) dropped whitelisted PATH")
	}
}
