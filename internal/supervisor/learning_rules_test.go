package supervisor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	corralgit "github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
)

func TestAdoptedPermissionRulesForFutureSpawn(t *testing.T) {
	ctx := context.Background()
	fc := clocktest.NewFake(time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := t.TempDir()
	canonical, _ := corralgit.CanonicalPath(repo)
	for _, item := range []struct{ id, fp, command, rule string }{
		{"a", "fp-a", "npm test", "Bash(npm test)"},
		{"b", "fp-b", "go test ./...", "Bash(go test ./...)"},
	} {
		l, err := st.UpsertLearningCandidate(ctx, store.UpsertLearningParams{
			ID: item.id, Repo: canonical, Kind: store.LearningPermissionRule, Fingerprint: item.fp,
			ContentJSON:  `{"tool":"Bash","command":"` + item.command + `","rule":"` + item.rule + `"}`,
			BaselineJSON: `{}`, ExpiresMs: fc.Now().Add(time.Hour).UnixMilli(),
		})
		if err != nil {
			t.Fatal(err)
		}
		l, _ = st.TransitionLearning(ctx, l.ID, store.LearningTransition{From: store.LearningCandidate, To: store.LearningVerified})
		l, _ = st.TransitionLearning(ctx, l.ID, store.LearningTransition{From: store.LearningVerified, To: store.LearningProposed})
		if item.id == "a" {
			if _, err := st.TransitionLearning(ctx, l.ID, store.LearningTransition{From: store.LearningProposed, To: store.LearningAdopted}); err != nil {
				t.Fatal(err)
			}
		}
	}

	r := New(st, nil, nil, fc, Config{}, nil)
	rules, err := r.adoptedPermissionRules(ctx, session.Spec{ID: "future", Cwd: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0] != "Bash(npm test)" {
		t.Fatalf("rules = %v", rules)
	}
	if _, err := st.TransitionLearning(ctx, "a", store.LearningTransition{From: store.LearningAdopted, To: store.LearningStale}); err != nil {
		t.Fatal(err)
	}
	rules, err = r.adoptedPermissionRules(ctx, session.Spec{ID: "resume", Cwd: repo})
	if err != nil || len(rules) != 1 {
		t.Fatalf("stale active rules = (%v,%v)", rules, err)
	}
}
