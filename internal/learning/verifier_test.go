package learning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/config"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
)

func TestVerifierProposesCounterfactuallyValidRule(t *testing.T) {
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
		appendApproval(t, st, "s1", "npm test", "approved", true)
	}
	fc.Advance(time.Millisecond)
	cfg := config.Learn{Window: 14 * 24 * time.Hour, MinApprovals: 3, TTL: 90 * 24 * time.Hour}
	miner := NewMiner(st, fc, cfg)
	miner.newID = func() string { return "l1" }
	mined, err := miner.Scan(ctx, repo)
	if err != nil || len(mined.Candidates) != 1 {
		t.Fatalf("mining = (%+v,%v)", mined, err)
	}

	verified, err := NewVerifier(st, cfg).VerifyCandidate(ctx, "l1")
	if err != nil {
		t.Fatalf("VerifyCandidate: %v", err)
	}
	if verified.Status != store.LearningProposed || verified.VerifiedMs == nil || verified.ProposedMs == nil {
		t.Fatalf("verified learning = %+v", verified)
	}
	var record verificationRecord
	if err := json.Unmarshal([]byte(verified.VerificationJSON), &record); err != nil {
		t.Fatal(err)
	}
	if !record.Valid || record.Approvals != 3 || record.AvoidableBlocks != 3 || record.EvidenceCoverage != 1 {
		t.Fatalf("verification = %+v", record)
	}
	if !strings.Contains(string(record.AfterSettingsJSON), `"permissions":{"allow":["Bash(npm test)"]}`) {
		t.Fatalf("after settings lacks exact rule: %s", record.AfterSettingsJSON)
	}
	if strings.Contains(string(record.BeforeSettingsJSON), `"permissions"`) {
		t.Fatalf("before settings unexpectedly has permissions: %s", record.BeforeSettingsJSON)
	}
	events, err := st.ListEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(events, session.EventLearningVerified) || !hasEvent(events, session.EventLearningProposed) {
		t.Fatalf("audit events = %+v", events)
	}
}

func TestVerifierRejectsNegativeEvidence(t *testing.T) {
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
	fc.Advance(time.Millisecond)

	canonicalRepo, err := NewMiner(st, fc, config.Learn{}).resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	contentBytes, _ := json.Marshal(permissionContent{Tool: "Bash", Command: "go test ./...", Rule: "Bash(go test ./...)"})
	sum := sha256.Sum256(append([]byte(string(store.LearningPermissionRule)+"\x00"), contentBytes...))
	windowBytes, _ := json.Marshal(baselineWindow{
		WindowStartMs: fc.Now().Add(-time.Hour).UnixMilli(), WindowEndMs: fc.Now().UnixMilli(),
	})
	if _, err := st.UpsertLearningCandidate(ctx, store.UpsertLearningParams{
		ID: "negative", Repo: canonicalRepo, Kind: store.LearningPermissionRule,
		Fingerprint: hex.EncodeToString(sum[:]), ContentJSON: string(contentBytes),
		BaselineJSON: string(windowBytes), EvidenceCount: 3,
		ExpiresMs: fc.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	rejected, err := NewVerifier(st, config.Learn{MinApprovals: 3}).VerifyCandidate(ctx, "negative")
	if err != nil {
		t.Fatalf("VerifyCandidate: %v", err)
	}
	if rejected.Status != store.LearningRejected || !strings.Contains(rejected.VerificationJSON, `"reason":"negative_evidence"`) {
		t.Fatalf("rejected = %+v", rejected)
	}
}

func TestVerifierRejectsFingerprintMismatch(t *testing.T) {
	ctx := context.Background()
	fc := clocktest.NewFake(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.UpsertLearningCandidate(ctx, store.UpsertLearningParams{
		ID: "bad-fp", Repo: t.TempDir(), Kind: store.LearningPermissionRule,
		Fingerprint: "wrong", ContentJSON: `{"tool":"Bash","command":"npm test","rule":"Bash(npm test)"}`,
		BaselineJSON: `{"window_start_ms":1,"window_end_ms":2}`, EvidenceCount: 3, ExpiresMs: 3,
	}); err != nil {
		t.Fatal(err)
	}
	rejected, err := NewVerifier(st, config.Learn{MinApprovals: 3}).VerifyCandidate(ctx, "bad-fp")
	if err != nil || rejected.Status != store.LearningRejected || !strings.Contains(rejected.VerificationJSON, "fingerprint_mismatch") {
		t.Fatalf("rejection = (%+v,%v)", rejected, err)
	}
}

func hasEvent(events []*session.Event, kind session.EventKind) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}
