package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/session"
)

func testSpec() session.Spec {
	return session.Spec{
		ID:             "11111111-1111-1111-1111-111111111111",
		Name:           "corral-1",
		Mode:           session.ModeInteractive,
		Cwd:            "/home/ci/work/project",
		ClaudeBin:      "/usr/local/bin/claude",
		SettingSources: "user,project,local",
		Env:            map[string]string{"HOME": "/home/ci", "ANTHROPIC_API_KEY": "sk-secret-value"},
		Rows:           40,
		Cols:           120,
	}
}

const testRelayCommand = "'/usr/local/bin/corral' hook-relay"

// TestPinSettingsJSONByteExact is design doc §10.2's claude/settings row:
// "pinned file contents byte-exact" — M2 (Amendment A.2) generates the
// settings.json bytes via BuildSettingsJSON rather than a fixed literal, so
// Pin's own output must be byte-identical to calling BuildSettingsJSON
// directly with the same relayCommand.
func TestPinSettingsJSONByteExact(t *testing.T) {
	dir := t.TempDir()
	clk := clocktest.NewFake(time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC))

	pinned, err := Pin(dir, testSpec(), clk, "0.1.0", 1, testRelayCommand, nil)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	got, err := os.ReadFile(pinned.SettingsPath)
	if err != nil {
		t.Fatalf("ReadFile settings.json: %v", err)
	}
	want, err := BuildSettingsJSON(testRelayCommand)
	if err != nil {
		t.Fatalf("BuildSettingsJSON: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("settings.json contents = %q, want %q", got, want)
	}
}

// TestPinSettingsJSONOnlyKnownKeys parses settings.json as JSON and
// asserts it contains only documented Claude Code settings keys — exactly
// "hooks" — per §10.2 and the package doc comment's rationale (an
// unrecognized key risks the whole file being silently ignored by claude in
// non-interactive mode).
func TestPinSettingsJSONOnlyKnownKeys(t *testing.T) {
	dir := t.TempDir()
	clk := clocktest.NewFake(time.Now())

	pinned, err := Pin(dir, testSpec(), clk, "0.1.0", 1, testRelayCommand, nil)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	raw, err := os.ReadFile(pinned.SettingsPath)
	if err != nil {
		t.Fatalf("ReadFile settings.json: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("settings.json content does not parse as JSON: %v", err)
	}
	knownM1Keys := map[string]bool{"hooks": true}
	for k := range m {
		if !knownM1Keys[k] {
			t.Errorf("settings.json contains unrecognized key %q", k)
		}
	}
}

// TestPinModesAndLayout checks §7.4's/§10.2's exact mode requirements:
// directory 0700, settings.json and meta.json both 0600.
func TestPinModesAndLayout(t *testing.T) {
	dir := t.TempDir()
	clk := clocktest.NewFake(time.Now())

	pinned, err := Pin(dir, testSpec(), clk, "0.1.0", 1, testRelayCommand, nil)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	wantDir := filepath.Join(dir, "sessions", "11111111-1111-1111-1111-111111111111")
	if pinned.Dir != wantDir {
		t.Fatalf("Dir = %q, want %q", pinned.Dir, wantDir)
	}

	di, err := os.Stat(pinned.Dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
	}

	for _, p := range []string{pinned.SettingsPath, pinned.MetaPath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("Stat %s: %v", p, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
		}
	}
}

// TestPinTightensPreexistingLooseModes mirrors
// TestOutputLogTightensPreexistingLooseMode's precedent in
// internal/screen: a pre-existing directory/files at a looser mode (a
// repo checkout or umask oddity) must be tightened, not merely left
// alone because they already existed.
func TestPinTightensPreexistingLooseModes(t *testing.T) {
	dir := t.TempDir()
	spec := testSpec()
	sessDir := filepath.Join(dir, "sessions", spec.ID)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatalf("pre-creating dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "settings.json"), nil, 0o644); err != nil {
		t.Fatalf("pre-creating settings.json: %v", err)
	}

	clk := clocktest.NewFake(time.Now())
	pinned, err := Pin(dir, spec, clk, "0.1.0", 1, testRelayCommand, nil)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	di, err := os.Stat(pinned.Dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("pre-existing dir mode = %v, want tightened to 0700", di.Mode().Perm())
	}
	fi, err := os.Stat(pinned.SettingsPath)
	if err != nil {
		t.Fatalf("Stat settings.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("pre-existing settings.json mode = %v, want tightened to 0600", fi.Mode().Perm())
	}
}

// TestPinMetaJSONWrittenAndProvenance checks meta.json is written, is
// valid JSON, contains every field design doc §7.4 names, uses the
// injected Clock (never a real timestamp), and is never passed to claude
// (it is not settings.json — a separate, distinctly-named file).
func TestPinMetaJSONWrittenAndProvenance(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC)
	clk := clocktest.NewFake(fixed)

	spec := testSpec()
	pinned, err := Pin(dir, spec, clk, "0.1.0", 1, testRelayCommand, nil)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if pinned.MetaPath == pinned.SettingsPath {
		t.Fatal("meta.json and settings.json must be distinct files")
	}

	raw, err := os.ReadFile(pinned.MetaPath)
	if err != nil {
		t.Fatalf("ReadFile meta.json: %v", err)
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("meta.json does not parse: %v", err)
	}

	if m.CorralVersion != "0.1.0" {
		t.Errorf("corral_version = %q, want %q", m.CorralVersion, "0.1.0")
	}
	if m.APIVersion != 1 {
		t.Errorf("api_version = %d, want 1", m.APIVersion)
	}
	if m.SessionID != spec.ID {
		t.Errorf("session_id = %q, want %q", m.SessionID, spec.ID)
	}
	if m.Name != spec.Name {
		t.Errorf("name = %q, want %q", m.Name, spec.Name)
	}
	if m.CreatedAt == "" {
		t.Error("created_at is empty")
	}
	if got, err := time.Parse(time.RFC3339Nano, m.CreatedAt); err != nil {
		t.Errorf("created_at %q does not parse as RFC3339Nano: %v", m.CreatedAt, err)
	} else if !got.Equal(fixed) {
		t.Errorf("created_at = %v, want %v (the injected fake clock's time, never a real timestamp)", got, fixed)
	}
	if m.SpecHash == "" {
		t.Error("spec_hash is empty")
	}

	// The secret value in spec.Env must never appear in the plaintext
	// meta.json (spec_hash is a one-way digest of it, not an encoding).
	if strings.Contains(string(raw), "sk-secret-value") {
		t.Error("meta.json leaks a raw secret value from spec.Env")
	}
}

// TestPinSpecHashChangesWithSpec checks spec_hash is actually a function
// of the spec's content (not a constant or purely ID-derived value) —
// otherwise it could not detect "something about this resumed session's
// spawn spec changed since it was first pinned", which is its whole
// purpose.
func TestPinSpecHashChangesWithSpec(t *testing.T) {
	dir := t.TempDir()
	clk := clocktest.NewFake(time.Now())

	specA := testSpec()
	pinnedA, err := Pin(dir, specA, clk, "0.1.0", 1, testRelayCommand, nil)
	if err != nil {
		t.Fatalf("Pin A: %v", err)
	}

	specB := testSpec()
	specB.Model = "opus"
	dirB := t.TempDir()
	pinnedB, err := Pin(dirB, specB, clk, "0.1.0", 1, testRelayCommand, nil)
	if err != nil {
		t.Fatalf("Pin B: %v", err)
	}

	if pinnedA.Meta.SpecHash == pinnedB.Meta.SpecHash {
		t.Fatal("spec_hash did not change when Spec.Model changed")
	}

	// Same spec, pinned twice, must hash identically (determinism).
	dirC := t.TempDir()
	pinnedC, err := Pin(dirC, specA, clk, "0.1.0", 1, testRelayCommand, nil)
	if err != nil {
		t.Fatalf("Pin C: %v", err)
	}
	if pinnedA.Meta.SpecHash != pinnedC.Meta.SpecHash {
		t.Fatal("spec_hash is not deterministic for an identical Spec")
	}
}
