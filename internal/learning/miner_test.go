package learning

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock/clocktest"
	"github.com/danielbecerra/corral/internal/config"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
)

func TestMinerCreatesExactCandidateWithCorrelatedEvidence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	fc := clocktest.NewFake(now)
	st, err := store.Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := t.TempDir()
	createMiningSession(t, st, "s1", repo)

	for i := 0; i < 3; i++ {
		appendApproval(t, st, "s1", "npm test", "approved", true)
	}
	fc.Advance(time.Millisecond)
	miner := NewMiner(st, fc, config.Learn{
		Window: 14 * 24 * time.Hour, MinApprovals: 3, TTL: 90 * 24 * time.Hour,
	})
	miner.newID = func() string { return "learning-1" }

	result, err := miner.Scan(ctx, repo)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].EvidenceCount != 3 {
		t.Fatalf("candidates = %+v", result.Candidates)
	}
	l := result.Candidates[0]
	if l.ContentJSON != `{"tool":"Bash","command":"npm test","rule":"Bash(npm test)"}` {
		t.Fatalf("ContentJSON = %s", l.ContentJSON)
	}
	evidence, err := st.ListLearningEvidence(ctx, l.ID)
	if err != nil || len(evidence) != 12 {
		t.Fatalf("evidence = (%+v,%v), want 12 rows", evidence, err)
	}

	// Repeating the same scan keeps one materialized candidate and one exact
	// evidence projection even though the audit log records both scans.
	miner.newID = func() string { return "ignored-on-conflict" }
	if _, err := miner.Scan(ctx, repo); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	rows, err := st.ListLearnings(ctx, l.Repo, "")
	if err != nil || len(rows) != 1 || rows[0].ID != "learning-1" {
		t.Fatalf("learnings after repeat = (%+v,%v)", rows, err)
	}
	evidence, _ = st.ListLearningEvidence(ctx, l.ID)
	if len(evidence) != 12 {
		t.Fatalf("repeat evidence rows = %d, want 12", len(evidence))
	}
}

func TestMinerRejectsNegativeOrUncorrelatedEvidence(t *testing.T) {
	ctx := context.Background()
	fc := clocktest.NewFake(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := t.TempDir()
	createMiningSession(t, st, "s1", repo)
	for i := 0; i < 3; i++ {
		appendApproval(t, st, "s1", "go test ./...", "approved", true)
	}
	appendApproval(t, st, "s1", "go test ./...", "failed_or_denied", false)
	for i := 0; i < 3; i++ {
		appendApproval(t, st, "s1", "git status", "approved", false)
	}
	fc.Advance(time.Millisecond)

	miner := NewMiner(st, fc, config.Learn{Window: 14 * 24 * time.Hour, MinApprovals: 3, TTL: time.Hour})
	result, err := miner.Scan(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 0 {
		t.Fatalf("unsafe candidates mined: %+v", result.Candidates)
	}
}

func TestEligibleCommandSafetyBoundary(t *testing.T) {
	tests := []struct {
		command string
		ok      bool
	}{
		{"npm test", true},
		{"", false},
		{"npm test && deploy", false},
		{"echo hi | tee out", false},
		{"echo )", false},
		{"line1\nline2", false},
	}
	for _, tt := range tests {
		raw := fmt.Sprintf(`{"tool_name":"Bash","tool_input":{"command":%q},"redactions":[],"truncated":false}`, tt.command)
		_, ok := eligibleCommand(raw)
		if ok != tt.ok {
			t.Errorf("eligibleCommand(%q) = %v, want %v", tt.command, ok, tt.ok)
		}
	}
	if _, ok := eligibleCommand(`{"tool_name":"Bash","tool_input":{"command":"npm test"},"redactions":["token"],"truncated":false}`); ok {
		t.Error("redacted input was eligible")
	}
	if _, ok := eligibleCommand(`{"tool_name":"Bash","tool_input":{"command":"npm test"},"redactions":[],"truncated":true}`); ok {
		t.Error("truncated input was eligible")
	}
}

func createMiningSession(t *testing.T, st *store.Store, id, cwd string) {
	t.Helper()
	_, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID: id, Name: id, Mode: session.ModeInteractive, Cwd: cwd,
		ClaudeBin: "/bin/true", SettingSources: "user,project,local",
		DesiredState: session.DesiredRunning, Status: session.StatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func appendApproval(t *testing.T, st *store.Store, sessionID, command, outcome string, answer bool) {
	t.Helper()
	ctx := context.Background()
	requested, err := st.AppendEvent(ctx, sessionID, session.EventPermissionRequested,
		fmt.Sprintf(`{"tool_name":"Bash","tool_input":{"command":%q},"redactions":[],"truncated":false}`, command))
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := st.AppendEvent(ctx, sessionID, session.EventPermissionBlocked,
		fmt.Sprintf(`{"request_seq":%d}`, requested.Seq))
	if err != nil {
		t.Fatal(err)
	}
	_ = blocked
	if answer {
		if _, err := st.AppendEvent(ctx, sessionID, session.EventSessionAnswered,
			fmt.Sprintf(`{"via":"http","permission_request_seq":%d}`, requested.Seq)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AppendEvent(ctx, sessionID, session.EventPermissionResolved,
		fmt.Sprintf(`{"request_seq":%d,"outcome":%q}`, requested.Seq, outcome)); err != nil {
		t.Fatal(err)
	}
}
