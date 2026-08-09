package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/djbu/corral/internal/claude/sessions"
)

// TranscriptEntry is one line of fakeclaude's fake transcript. It is not
// claude's real on-disk schema — M1 has no need to reproduce that
// exactly, only the *path* fakeclaude writes to needs to match claude's
// real slug rule (design doc §10.1 item 4), since that path is what
// makes "reattach after a daemon restart shows my old conversation" an
// assertable fact rather than an assumption.
type TranscriptEntry struct {
	Text string `json:"text"`
	At   string `json:"at"` // RFC3339Nano.
}

// transcriptPath returns the exact path claude itself would use for
// sessionID's transcript under cwd, rooted at fakeHome/.claude instead of
// the real ~/.claude (design doc §10.1 item 4: "$CORRAL_FAKE_HOME keeps
// tests off the real ~/.claude"). It reuses internal/claude/sessions
// verbatim rather than re-implementing the slug rule, so fakeclaude's
// redraw-on-resume exercises the identical path-computation logic the
// real daemon (and step 9's spawn wiring) depends on.
func transcriptPath(fakeHome, cwd, sessionID string) string {
	return sessions.TranscriptPath(filepath.Join(fakeHome, ".claude"), cwd, sessionID)
}

// appendPrompt appends one TranscriptEntry as a single JSON line to
// sessionID's transcript file (one line per prompt, per item 4),
// creating its parent directory and the file itself if this is the
// first prompt of the session.
//
// At uses time.Now() rather than an injected clock: fakeclaude is a test
// double process standing in for the real claude binary, not corral's
// own daemon code, so it is not subject to the "no time.Now() outside
// internal/clock" rule that applies to corral's non-test production
// code — there is no daemon-side determinism requirement on what a
// simulated third-party binary's own transcript timestamps say.
func appendPrompt(fakeHome, cwd, sessionID, text string) error {
	path := transcriptPath(fakeHome, cwd, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("fakeclaude: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("fakeclaude: open %s: %w", path, err)
	}
	defer f.Close()

	entry := TranscriptEntry{Text: text, At: time.Now().UTC().Format(time.RFC3339Nano)}
	b, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("fakeclaude: marshal transcript entry: %w", err)
	}
	b = append(b, '\n')
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("fakeclaude: write %s: %w", path, err)
	}
	return nil
}

// loadTranscript reads sessionID's transcript file line by line,
// returning the prior turns in file order. A missing file (a session
// that has never had a prompt yet) is not an error: it returns a nil
// slice.
func loadTranscript(fakeHome, cwd, sessionID string) ([]TranscriptEntry, error) {
	path := transcriptPath(fakeHome, cwd, sessionID)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fakeclaude: open %s: %w", path, err)
	}
	defer f.Close()

	var entries []TranscriptEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e TranscriptEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("fakeclaude: parse transcript line in %s: %w", path, err)
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("fakeclaude: scan %s: %w", path, err)
	}
	return entries, nil
}
