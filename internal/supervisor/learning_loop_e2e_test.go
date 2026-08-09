package supervisor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock/clocktest"
	"github.com/djbu/corral/internal/config"
	"github.com/djbu/corral/internal/learning"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
)

// TestLearningLoopE2E_FakeClaude closes M6's deterministic plumbing loop:
// repeated manual approvals -> scan/verify/propose -> explicit adoption ->
// a real Registry.Spawn of fakeclaude receives the exact pinned rule -> a
// synthetic full post window reports improvement. The managed repo tree is
// hashed before and after to prove zero unapproved writes.
func TestLearningLoopE2E_FakeClaude(t *testing.T) {
	ctx := context.Background()
	fc := clocktest.NewFake(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "corral.db"), fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := hashLearningTree(t, repo)

	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("baseline-%d", i)
		createLoopSession(t, st, id, repo, "/bin/true")
		if i < 3 {
			appendLoopApproval(t, st, id, "npm test")
		} else {
			_, _ = st.AppendEvent(ctx, id, session.EventSessionCreated, `{}`)
		}
	}
	for i := 0; i < 3; i++ {
		createLoopTask(t, st, fmt.Sprintf("baseline-task-%d", i), repo, 1.0)
	}
	fc.Advance(time.Millisecond)
	cfg := config.Learn{
		Window: 14 * 24 * time.Hour, MinApprovals: 3, TTL: 90 * 24 * time.Hour,
		MinSessions: 5, MinTerminalTasks: 3, CostRegressionTolerance: .10,
	}
	mined, err := learning.NewMiner(st, fc, cfg).Scan(ctx, repo)
	if err != nil || len(mined.Candidates) != 1 {
		t.Fatalf("Scan = (%+v,%v)", mined, err)
	}
	proposed, err := learning.NewVerifier(st, cfg).VerifyCandidate(ctx, mined.Candidates[0].ID)
	if err != nil || proposed.Status != store.LearningProposed {
		t.Fatalf("VerifyCandidate = (%+v,%v)", proposed, err)
	}
	reporter := learning.NewReporter(st, fc, cfg)
	adopted, err := reporter.Adopt(ctx, proposed.ID)
	if err != nil || adopted.Status != store.LearningAdopted {
		t.Fatalf("Adopt = (%+v,%v)", adopted, err)
	}

	fakeClaude := buildFakeClaude(t)
	createLoopSession(t, st, "future", repo, fakeClaude)
	stateDir := t.TempDir()
	r := New(st, state.New(st), killingCheckpointer{}, fc, Config{
		StateDir: stateDir,
		EnvSnapshot: map[string]string{
			"PATH": os.Getenv("PATH"), "CORRAL_FAKE_HEADLESS_SCENARIO": "success",
		},
		EnvPassthrough: []string{"CORRAL_FAKE_HEADLESS_SCENARIO"},
		CorralVersion:  "test", APIVersion: 1, OutputLogMaxBytes: 1 << 20,
	}, nil)
	spec := session.Spec{
		ID: "future", Name: "future", Mode: session.ModeHeadless, Cwd: repo,
		ClaudeBin: fakeClaude, SettingSources: "user,project,local", Prompt: "run",
	}
	if _, err := r.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn fakeclaude: %v", err)
	}
	settingsBytes, err := os.ReadFile(filepath.Join(stateDir, "sessions", "future", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(settingsBytes), `"permissions":{"allow":["Bash(npm test)"]}`) {
		t.Fatalf("future settings missing adopted rule: %s", settingsBytes)
	}
	waitForStatus(t, st, "future", session.StatusExited)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("post-%d", i)
		createLoopSession(t, st, id, repo, "/bin/true")
		_, _ = st.AppendEvent(ctx, id, session.EventSessionCreated, `{}`)
	}
	for i := 0; i < 3; i++ {
		createLoopTask(t, st, fmt.Sprintf("post-task-%d", i), repo, .5)
	}
	fc.Advance(14 * 24 * time.Hour)
	report, err := reporter.Generate(ctx, adopted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Measurements) != 2 || report.Measurements[1].Verdict != "improved" {
		t.Fatalf("post report = %+v", report)
	}
	after := hashLearningTree(t, repo)
	if before != after {
		t.Fatalf("managed repo tree changed: before=%x after=%x", before, after)
	}
}

func createLoopSession(t *testing.T, st *store.Store, id, repo, claudeBin string) {
	t.Helper()
	_, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID: id, Name: id, Mode: session.ModeHeadless, Cwd: repo, ClaudeBin: claudeBin,
		SettingSources: "user,project,local", DesiredState: session.DesiredRunning,
		Status: session.StatusStarting,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func appendLoopApproval(t *testing.T, st *store.Store, sessionID, command string) {
	t.Helper()
	ctx := context.Background()
	req, err := st.AppendEvent(ctx, sessionID, session.EventPermissionRequested,
		fmt.Sprintf(`{"tool_name":"Bash","tool_input":{"command":%q},"redactions":[],"truncated":false}`, command))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		kind session.EventKind
		data string
	}{
		{session.EventPermissionBlocked, fmt.Sprintf(`{"request_seq":%d}`, req.Seq)},
		{session.EventSessionAnswered, fmt.Sprintf(`{"via":"http","permission_request_seq":%d}`, req.Seq)},
		{session.EventPermissionResolved, fmt.Sprintf(`{"request_seq":%d,"outcome":"approved"}`, req.Seq)},
	} {
		if _, err := st.AppendEvent(ctx, sessionID, item.kind, item.data); err != nil {
			t.Fatal(err)
		}
	}
}

func createLoopTask(t *testing.T, st *store.Store, id, repo string, cost float64) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateTask(ctx, store.CreateTaskParams{
		ID: id, DAGID: id, Name: id, Prompt: "p", Repo: repo, Cwd: repo, Status: store.TaskSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, id, func(task *store.Task) { task.CostUSD = cost }); err != nil {
		t.Fatal(err)
	}
}

func hashLearningTree(t *testing.T, root string) [32]byte {
	t.Helper()
	h := sha256.New()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		_, _ = h.Write([]byte(rel))
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, _ = h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
