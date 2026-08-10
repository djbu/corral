package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/session"
)

func TestLearningStoreLifecycleAndProvenance(t *testing.T) {
	fc := clocktest.NewFake(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	st, err := Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if _, err := st.CreateSession(ctx, CreateSessionParams{
		ID: "s1", Name: "s1", Mode: session.ModeInteractive, Cwd: "/repo",
		ClaudeBin: "/bin/true", DesiredState: session.DesiredRunning,
		Status: session.StatusRunning, SettingSources: "user,project,local",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	event, err := st.AppendEvent(ctx, "s1", session.EventPermissionRequested, `{"tool_name":"Bash"}`)
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	p := UpsertLearningParams{
		ID: "l1", Repo: "/repo", Kind: LearningPermissionRule, Fingerprint: "fp",
		ContentJSON:  `{"tool":"Bash","command":"npm test","rule":"Bash(npm test)"}`,
		BaselineJSON: `{"approvals":3}`, EvidenceCount: 1,
		ExpiresMs: fc.Now().Add(90 * 24 * time.Hour).UnixMilli(),
		Evidence:  []LearningEvidence{{EventSeq: event.Seq, Role: "request"}},
	}
	l, err := st.UpsertLearningCandidate(ctx, p)
	if err != nil {
		t.Fatalf("UpsertLearningCandidate: %v", err)
	}
	if l.Status != LearningCandidate || l.EvidenceCount != 1 {
		t.Fatalf("learning = %+v, want candidate with one evidence", l)
	}

	p.ID = "different-id-is-ignored-on-conflict"
	p.EvidenceCount = 2
	if _, err := st.UpsertLearningCandidate(ctx, p); err != nil {
		t.Fatalf("idempotent UpsertLearningCandidate: %v", err)
	}
	l, _ = st.GetLearning(ctx, "l1")
	if l.EvidenceCount != 2 {
		t.Fatalf("EvidenceCount = %d, want refreshed 2", l.EvidenceCount)
	}
	evidence, err := st.ListLearningEvidence(ctx, l.ID)
	if err != nil || len(evidence) != 1 {
		t.Fatalf("ListLearningEvidence = (%v,%v), want one deduplicated row", evidence, err)
	}

	fc.Advance(time.Hour)
	l, err = st.TransitionLearning(ctx, l.ID, LearningTransition{
		From: LearningCandidate, To: LearningVerified, VerificationJSON: `{"avoidable_blocks":3}`,
	})
	if err != nil || l.VerifiedMs == nil {
		t.Fatalf("verify transition = (%+v,%v)", l, err)
	}
	p.Evidence = append(p.Evidence, LearningEvidence{EventSeq: event.Seq, Role: "late"})
	if _, err := st.UpsertLearningCandidate(ctx, p); err != nil {
		t.Fatalf("re-scan verified learning: %v", err)
	}
	evidence, _ = st.ListLearningEvidence(ctx, l.ID)
	if len(evidence) != 1 {
		t.Fatalf("verified learning evidence mutated by re-scan: %+v", evidence)
	}
	l, err = st.TransitionLearning(ctx, l.ID, LearningTransition{From: LearningVerified, To: LearningProposed})
	if err != nil || l.ProposedMs == nil {
		t.Fatalf("propose transition = (%+v,%v)", l, err)
	}
	if _, err := st.TransitionLearning(ctx, l.ID, LearningTransition{From: LearningProposed, To: LearningRetired}); !errors.Is(err, ErrInvalidLearningTransition) {
		t.Fatalf("invalid transition error = %v", err)
	}

	if err := st.CreateLearningMeasurement(ctx, LearningMeasurement{
		ID: "m1", LearningID: l.ID, Phase: "baseline", WindowStartMs: 1,
		WindowEndMs: 2, MetricsJSON: `{"blocked":3}`, Verdict: "baseline",
	}); err != nil {
		t.Fatalf("CreateLearningMeasurement: %v", err)
	}
	measurements, err := st.ListLearningMeasurements(ctx, l.ID)
	if err != nil || len(measurements) != 1 || measurements[0].ID != "m1" || measurements[0].CreatedMs == 0 {
		t.Fatalf("ListLearningMeasurements = (%+v,%v)", measurements, err)
	}
	if err := st.CreateLearningMeasurement(ctx, LearningMeasurement{
		ID: "m2", LearningID: l.ID, Phase: "baseline", WindowStartMs: 2,
		WindowEndMs: 3, MetricsJSON: `{"blocked":2}`, Verdict: "baseline",
	}); err != nil {
		t.Fatalf("idempotent phase measurement: %v", err)
	}
	measurements, _ = st.ListLearningMeasurements(ctx, l.ID)
	if len(measurements) != 1 || measurements[0].ID != "m1" {
		t.Fatalf("second measurement for same phase was persisted: %+v", measurements)
	}
}

func TestLearningStoreRejectsInvalidJSONAndForeignEvidence(t *testing.T) {
	fc := clocktest.NewFake(time.Now())
	st, err := Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	_, err = st.UpsertLearningCandidate(ctx, UpsertLearningParams{
		ID: "bad", Repo: "/repo", Kind: LearningPermissionRule, Fingerprint: "fp",
		ContentJSON: `{`, BaselineJSON: `{}`, ExpiresMs: 1,
	})
	if !errors.Is(err, ErrInvalidLearning) {
		t.Fatalf("invalid JSON error = %v", err)
	}
	_, err = st.UpsertLearningCandidate(ctx, UpsertLearningParams{
		ID: "fk", Repo: "/repo", Kind: LearningPermissionRule, Fingerprint: "fp2",
		ContentJSON: `{}`, BaselineJSON: `{}`, ExpiresMs: 1,
		Evidence: []LearningEvidence{{EventSeq: 99999, Role: "request"}},
	})
	if err == nil {
		t.Fatal("foreign evidence insert succeeded, want FK failure")
	}
	if _, err := st.GetLearning(ctx, "fk"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed transaction left learning row: %v", err)
	}
}
