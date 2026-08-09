package learning

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/config"
	corralgit "github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
)

func TestReporterEarlyCloseRequiresAndPersistsSufficientSample(t *testing.T) {
	ctx := context.Background()
	fc := clocktest.NewFake(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	st, repo := newReporterStore(t, fc)
	for i := 0; i < 5; i++ {
		id := "base-" + string(rune('a'+i))
		createMiningSession(t, st, id, repo)
		_, _ = st.AppendEvent(ctx, id, session.EventPermissionBlocked, `{}`)
	}
	fc.Advance(time.Millisecond)
	l := createProposedLearning(t, st, repo, fc)
	reporter := NewReporter(st, fc, config.Learn{
		Window: 14 * 24 * time.Hour, TTL: 90 * 24 * time.Hour,
		MinSessions: 5, MinTerminalTasks: 0, CostRegressionTolerance: .10,
	})
	reporter.newID = sequentialIDs("baseline", "early-post")
	if _, err := reporter.Adopt(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		id := "post-" + string(rune('a'+i))
		createMiningSession(t, st, id, repo)
		_, _ = st.AppendEvent(ctx, id, session.EventSessionCreated, `{}`)
	}
	fc.Advance(time.Millisecond)
	report, err := reporter.GenerateEarly(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Measurements) != 2 || report.Measurements[1].Verdict != "improved" {
		t.Fatalf("early report = %+v", report)
	}
	if report.Measurements[1].WindowEndMs != fc.Now().UnixMilli() || report.EligibleAtMs != fc.Now().UnixMilli() {
		t.Fatalf("early window end=%d eligible=%d want now=%d", report.Measurements[1].WindowEndMs, report.EligibleAtMs, fc.Now().UnixMilli())
	}

	// The ordinary 14-day path sees the already-finalized post measurement
	// and stays idempotent instead of creating a second post window later.
	report, err = reporter.Generate(ctx, l.ID)
	if err != nil || len(report.Measurements) != 2 {
		t.Fatalf("Generate after early close = (%+v,%v)", report, err)
	}
}

func TestReporterEarlyCloseRejectsInconclusiveSample(t *testing.T) {
	ctx := context.Background()
	fc := clocktest.NewFake(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	st, repo := newReporterStore(t, fc)
	for i := 0; i < 5; i++ {
		createMiningSession(t, st, "base-"+string(rune('a'+i)), repo)
	}
	fc.Advance(time.Millisecond)
	l := createProposedLearning(t, st, repo, fc)
	reporter := NewReporter(st, fc, config.Learn{
		Window: 14 * 24 * time.Hour, TTL: 90 * 24 * time.Hour,
		MinSessions: 5, MinTerminalTasks: 0,
	})
	reporter.newID = sequentialIDs("baseline", "must-not-persist")
	if _, err := reporter.Adopt(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		id := "post-" + string(rune('a'+i))
		createMiningSession(t, st, id, repo)
		_, _ = st.AppendEvent(ctx, id, session.EventSessionCreated, `{}`)
	}
	fc.Advance(time.Millisecond)
	if _, err := reporter.GenerateEarly(ctx, l.ID); !errors.Is(err, ErrInsufficientPostSample) {
		t.Fatalf("GenerateEarly error = %v, want ErrInsufficientPostSample", err)
	}
	measurements, err := st.ListLearningMeasurements(ctx, l.ID)
	if err != nil || len(measurements) != 1 || measurements[0].Phase != "baseline" {
		t.Fatalf("insufficient early close persisted data: (%+v,%v)", measurements, err)
	}
}

func TestReporterBaselinePostImprovementAndIdempotence(t *testing.T) {
	ctx := context.Background()
	fc := clocktest.NewFake(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	st, repo := newReporterStore(t, fc)
	for i := 0; i < 5; i++ {
		id := "base-" + string(rune('a'+i))
		createMiningSession(t, st, id, repo)
		if _, err := st.AppendEvent(ctx, id, session.EventPermissionBlocked, `{}`); err != nil {
			t.Fatal(err)
		}
	}
	fc.Advance(time.Millisecond)
	l := createProposedLearning(t, st, repo, fc)
	cfg := config.Learn{
		Window: 14 * 24 * time.Hour, TTL: 7 * 24 * time.Hour,
		MinSessions: 5, MinTerminalTasks: 0, CostRegressionTolerance: 0.10,
	}
	reporter := NewReporter(st, fc, cfg)
	reporter.newID = sequentialIDs("baseline", "post-1", "post-2")
	l, err := reporter.Adopt(ctx, l.ID)
	if err != nil || l.Status != store.LearningAdopted {
		t.Fatalf("Adopt = (%+v,%v)", l, err)
	}
	measurements, _ := st.ListLearningMeasurements(ctx, l.ID)
	if len(measurements) != 1 || measurements[0].Phase != "baseline" {
		t.Fatalf("baseline measurements = %+v", measurements)
	}

	for i := 0; i < 5; i++ {
		id := "post-" + string(rune('a'+i))
		createMiningSession(t, st, id, repo)
		if _, err := st.AppendEvent(ctx, id, session.EventSessionCreated, `{}`); err != nil {
			t.Fatal(err)
		}
	}
	fc.Advance(14 * 24 * time.Hour)
	report, err := reporter.Generate(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Measurements) != 2 || report.Measurements[1].Verdict != "improved" {
		t.Fatalf("report measurements = %+v", report.Measurements)
	}
	if report.Learning.Status != store.LearningStale {
		t.Fatalf("expired improved learning status = %s, want stale", report.Learning.Status)
	}
	if _, err := reporter.Generate(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	measurements, _ = st.ListLearningMeasurements(ctx, l.ID)
	if len(measurements) != 2 {
		t.Fatalf("idempotent report created %d measurements", len(measurements))
	}
}

func TestReporterFlagsRegressionWithoutRetiringRule(t *testing.T) {
	ctx := context.Background()
	fc := clocktest.NewFake(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	st, repo := newReporterStore(t, fc)
	for i := 0; i < 5; i++ {
		id := "base-" + string(rune('a'+i))
		createMiningSession(t, st, id, repo)
		_, _ = st.AppendEvent(ctx, id, session.EventSessionCreated, `{}`)
	}
	fc.Advance(time.Millisecond)
	l := createProposedLearning(t, st, repo, fc)
	cfg := config.Learn{Window: 14 * 24 * time.Hour, TTL: 90 * 24 * time.Hour, MinSessions: 5, MinTerminalTasks: 0}
	reporter := NewReporter(st, fc, cfg)
	reporter.newID = sequentialIDs("baseline", "post")
	l, err := reporter.Adopt(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		id := "post-" + string(rune('a'+i))
		createMiningSession(t, st, id, repo)
		for j := 0; j < 2; j++ {
			_, _ = st.AppendEvent(ctx, id, session.EventPermissionBlocked, `{}`)
		}
	}
	fc.Advance(14 * 24 * time.Hour)
	report, err := reporter.Generate(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Learning.Status != store.LearningRegressionFlagged || report.Measurements[1].Verdict != "regression" {
		t.Fatalf("regression report = %+v", report)
	}
}

func TestReporterVerdictRequiresDenominators(t *testing.T) {
	r := &Reporter{config: config.Learn{MinSessions: 5, MinTerminalTasks: 3, CostRegressionTolerance: .1}}
	if got := r.verdict(Metrics{Sessions: 4}, Metrics{Sessions: 5}); got != "inconclusive" {
		t.Fatalf("verdict = %q", got)
	}
	base := Metrics{Sessions: 5, TerminalTasks: 3, BlockedPerSession: 2, CostPerTerminalTask: 10}
	post := Metrics{Sessions: 5, TerminalTasks: 3, BlockedPerSession: 1, CostPerTerminalTask: 12}
	if got := r.verdict(base, post); got != "regression" {
		t.Fatalf("cost regression verdict = %q", got)
	}
}

func newReporterStore(t *testing.T, fc *clocktest.FakeClock) (*store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo, err := corralgit.CanonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st, repo
}

func createProposedLearning(t *testing.T, st *store.Store, repo string, fc *clocktest.FakeClock) *store.Learning {
	t.Helper()
	l, err := st.UpsertLearningCandidate(context.Background(), store.UpsertLearningParams{
		ID: "learning", Repo: repo, Kind: store.LearningPermissionRule, Fingerprint: "fp",
		ContentJSON:  `{"tool":"Bash","command":"npm test","rule":"Bash(npm test)"}`,
		BaselineJSON: `{}`, ExpiresMs: fc.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err = st.TransitionLearning(context.Background(), l.ID, store.LearningTransition{From: store.LearningCandidate, To: store.LearningVerified})
	if err == nil {
		l, err = st.TransitionLearning(context.Background(), l.ID, store.LearningTransition{From: store.LearningVerified, To: store.LearningProposed})
	}
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func sequentialIDs(ids ...string) func() string {
	i := 0
	return func() string {
		if i >= len(ids) {
			return "extra"
		}
		id := ids[i]
		i++
		return id
	}
}
