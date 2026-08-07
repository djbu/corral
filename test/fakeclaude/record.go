package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Invocation is fakeclaude's recorded record of exactly what it was
// started with (design doc §10.1 item 2): "the single highest-value
// thing fakeclaude does in M1" — it is what turns the env-whitelist,
// settings-pinning, and resume-argv assertions in the integration suite
// (§10.4's TestGracefulRestartResumes reads invocation-2.json directly)
// into checkable facts instead of assumptions.
type Invocation struct {
	Argv     []string        `json:"argv"`
	Environ  []string        `json:"environ"`
	Cwd      string          `json:"cwd"`
	Settings json.RawMessage `json:"settings"`
}

// recordInvocation writes the next invocation-<N>.json for sessionID
// under <fakeState>/<sessionID>/, returning the path it wrote. N starts
// at 1 and increments per call for the same (fakeState, sessionID) pair,
// so a graceful-restart-and-resume scenario (fakeclaude invoked twice
// against the same CORRAL_FAKE_STATE, once fresh and once with --resume)
// produces invocation-1.json then invocation-2.json — exactly what
// §10.4's TestGracefulRestartResumes names.
func recordInvocation(fakeState, sessionID string, inv Invocation) (string, error) {
	dir := filepath.Join(fakeState, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("fakeclaude: mkdir %s: %w", dir, err)
	}

	n, err := nextInvocationNumber(dir)
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, fmt.Sprintf("invocation-%d.json", n))
	b, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return "", fmt.Errorf("fakeclaude: marshal invocation: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", fmt.Errorf("fakeclaude: write %s: %w", path, err)
	}
	return path, nil
}

// nextInvocationNumber scans dir for existing invocation-<N>.json files
// and returns one past the largest N found (1 if none exist yet).
func nextInvocationNumber(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("fakeclaude: readdir %s: %w", dir, err)
	}
	maxN := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "invocation-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "invocation-"), ".json")
		n, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		if n > maxN {
			maxN = n
		}
	}
	return maxN + 1, nil
}

// settingsRawMessage reads path and returns its contents as a
// json.RawMessage suitable for embedding in Invocation.Settings: if the
// file parses as JSON, its exact bytes are embedded verbatim (so the
// recorded invocation shows byte-for-byte what claude/settings.Pin
// wrote — the whole point of item 2's "parsed contents of the --settings
// file"); otherwise (missing file, invalid JSON), a JSON string
// describing the problem is embedded instead, so a bad --settings value
// never crashes fakeclaude's own recording step.
func settingsRawMessage(path string) json.RawMessage {
	b, err := os.ReadFile(path)
	if err != nil {
		msg, _ := json.Marshal(fmt.Sprintf("fakeclaude: could not read settings file %q: %v", path, err))
		return msg
	}
	if !json.Valid(b) {
		msg, _ := json.Marshal(fmt.Sprintf("fakeclaude: settings file %q is not valid JSON", path))
		return msg
	}
	return json.RawMessage(b)
}
