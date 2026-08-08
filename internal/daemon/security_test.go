package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecurity_FileModes covers design doc §10.3: corral.sock must be
// 0600, state_dir 0700 (tightened even if something pre-created it
// looser), and corral.db/-wal/-shm 0600.
func TestSecurity_FileModes(t *testing.T) {
	cfg := testConfig(t)

	// Pre-loosen state_dir before startup to prove step 3's Chmod
	// actually tightens a pre-existing directory rather than only
	// getting it right when MkdirAll creates it fresh.
	if err := os.Chmod(cfg.StateDir, 0o755); err != nil {
		t.Fatalf("pre-loosening state_dir: %v", err)
	}

	_, stop := startForTest(t, cfg)
	defer stop()

	assertMode(t, cfg.StateDir, 0o700)
	assertMode(t, cfg.Socket, 0o600)

	dbPath := filepath.Join(cfg.StateDir, "corral.db")
	assertMode(t, dbPath, 0o600)
	for _, suffix := range []string{"-wal", "-shm"} {
		p := dbPath + suffix
		if _, err := os.Stat(p); os.IsNotExist(err) {
			// WAL/SHM files may not exist depending on checkpoint
			// timing; only assert the mode of ones that do.
			continue
		}
		assertMode(t, p, 0o600)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	got := info.Mode().Perm()
	if got != want {
		t.Fatalf("mode of %s = %o, want %o", path, got, want)
	}
}

// TestSecurity_DaemonLogMode confirms daemon.log itself (opened directly
// by initLogging, before state_dir's step-3 Chmod even runs) is created
// 0600, never world/group readable — it can contain hook payloads and
// other session content.
func TestSecurity_DaemonLogMode(t *testing.T) {
	cfg := testConfig(t)
	_, stop := startForTest(t, cfg)
	defer stop()

	assertMode(t, filepath.Join(cfg.StateDir, "daemon.log"), 0o600)
}
