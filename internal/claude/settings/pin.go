// Package settings writes the per-session pinned settings directory
// design doc §7.4 describes: a settings.json handed to claude via
// --settings, and a sibling meta.json that is corral's own provenance
// record and is never passed to claude.
package settings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/session"
)

// Content is the exact, byte-literal content written to settings.json.
// M1's whole content is fixed (design doc §7.4: "M1 content is exactly
// {"hooks": {}}") — this is a Go string constant rather than something
// produced by json.Marshal specifically so the bytes on disk are exactly
// these bytes, not merely JSON-equivalent to them (encoding/json would
// drop the space after the colon and after "hooks":, producing
// {"hooks":{}} instead — both parse identically, but §10.2 asks for
// byte-exact contents, and matching the design doc's literal exactly
// means a future diff against the doc's own text is trivially checkable).
//
// Why this file may contain *only* documented Claude Code settings keys,
// never a corral-specific one: claude silently ignores an entire settings
// file that fails validation in non-interactive mode (per --help; spike
// finding #3) — a "$corral" key here would risk silently disabling this
// whole pin, the exact failure mode finding #3 says must be eliminated.
// Provenance therefore lives in the sibling meta.json instead.
const Content = `{"hooks": {}}`

// dirMode/fileMode are §7.4's exact required modes: the per-session
// directory is 0700, every file inside it (settings.json, meta.json,
// output.log) is 0600.
const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// Meta is corral's own provenance record (design doc §7.4's exact field
// list), written to meta.json. Field order here is also JSON key order,
// chosen to match the doc's listing.
type Meta struct {
	CorralVersion string `json:"corral_version"`
	APIVersion    int    `json:"api_version"`
	SessionID     string `json:"session_id"`
	Name          string `json:"name"`
	CreatedAt     string `json:"created_at"` // RFC3339Nano, from the injected Clock — never time.Now().
	SpecHash      string `json:"spec_hash"`
}

// Pinned is the result of a successful Pin call: the paths of the
// directory and the two files it wrote, plus the output.log path it
// reserved but did not open (see Pin's doc comment).
type Pinned struct {
	Dir           string
	SettingsPath  string
	MetaPath      string
	OutputLogPath string
	Meta          Meta
}

// Pin creates <stateDir>/sessions/<spec.ID>/ at mode 0700 (tightening it
// if it already exists at a looser mode) and writes settings.json and
// meta.json inside it, both at mode 0600 (design doc §7.4). corralVersion
// and apiVersion are threaded in by the caller (internal/version) rather
// than imported directly, so this package never depends on internal/
// version's build-time ldflags plumbing.
//
// Pin does not open output.log itself: design doc §1 already places the
// size-capped-tee logic in internal/screen/outputlog.go, with its own
// tests. Duplicating that logic here would mean two independent
// implementations of the same cap-and-warn behavior to keep in sync; Pin
// instead only reserves and returns OutputLogPath, and the caller (once
// it has a *screen.Screen for this session) calls
// screen.OpenOutputLog(pinned.OutputLogPath, ...) and Screen.SetOutputLog
// itself.
func Pin(stateDir string, spec session.Spec, clk clock.Clock, corralVersion string, apiVersion int) (*Pinned, error) {
	if spec.ID == "" {
		return nil, fmt.Errorf("settings: Pin: spec.ID is empty")
	}

	dir := filepath.Join(stateDir, "sessions", spec.ID)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("settings: mkdir %s: %w", dir, err)
	}
	// MkdirAll does not tighten an already-existing directory's mode —
	// mirrors screen.OpenOutputLog's precedent of an explicit chmod after
	// creation.
	if err := os.Chmod(dir, dirMode); err != nil {
		return nil, fmt.Errorf("settings: chmod %s: %w", dir, err)
	}

	settingsPath := filepath.Join(dir, "settings.json")
	if err := writeFile(settingsPath, []byte(Content)); err != nil {
		return nil, err
	}

	meta := Meta{
		CorralVersion: corralVersion,
		APIVersion:    apiVersion,
		SessionID:     spec.ID,
		Name:          spec.Name,
		CreatedAt:     clk.Now().Format("2006-01-02T15:04:05.000000000Z07:00"),
		SpecHash:      specHash(spec),
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("settings: marshal meta.json: %w", err)
	}
	metaPath := filepath.Join(dir, "meta.json")
	if err := writeFile(metaPath, metaJSON); err != nil {
		return nil, err
	}

	return &Pinned{
		Dir:           dir,
		SettingsPath:  settingsPath,
		MetaPath:      metaPath,
		OutputLogPath: filepath.Join(dir, "output.log"),
		Meta:          meta,
	}, nil
}

// writeFile creates (or truncates) path with content at fileMode,
// chmod'ing explicitly afterward in case path already existed at a
// looser mode — same reasoning as OpenOutputLog's precedent in
// internal/screen: O_CREATE does not tighten an existing file.
func writeFile(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf("settings: create %s: %w", path, err)
	}
	defer f.Close()
	if err := f.Chmod(fileMode); err != nil {
		return fmt.Errorf("settings: chmod %s: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("settings: write %s: %w", path, err)
	}
	return nil
}

// specHash returns a hex sha256 digest of a canonical JSON encoding of
// spec (encoding/json sorts map keys, so this is deterministic across
// calls for an identical Spec). Design doc §7.4 does not define exactly
// what spec_hash is a hash of; hashing the entire spawn Spec — including
// Env, whose values may include secrets like ANTHROPIC_API_KEY — is a
// deliberate choice noted in the deviations list: a one-way hash cannot
// leak the plaintext, and hashing the complete Spec (rather than some
// hand-picked subset of fields) is what makes spec_hash actually useful
// for its stated purpose, detecting whether *anything* about a resumed
// session's spawn spec changed since it was first pinned.
func specHash(spec session.Spec) string {
	b, err := json.Marshal(spec)
	if err != nil {
		// Spec contains no channels/funcs/cyclic types; unreachable
		// outside a change to Spec's own field types.
		panic(fmt.Sprintf("settings: marshal spec for hashing: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
