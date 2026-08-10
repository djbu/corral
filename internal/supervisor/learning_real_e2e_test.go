package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/djbu/corral/internal/clock"
	corralgit "github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
)

// TestLearningRuleE2E_RealClaude is intentionally opt-in: it spends a real
// Claude invocation and depends on the operator's installed/authenticated CLI.
func TestLearningRuleE2E_RealClaude(t *testing.T) {
	if os.Getenv("CORRAL_E2E_REAL_CLAUDE") != "1" {
		t.Skip("set CORRAL_E2E_REAL_CLAUDE=1 to verify an adopted exact rule with real Claude Code")
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not found on PATH")
	}
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "corral.db"), clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := t.TempDir()
	canonical, _ := corralgit.CanonicalPath(repo)
	l, err := st.UpsertLearningCandidate(ctx, store.UpsertLearningParams{
		ID: "real-rule", Repo: canonical, Kind: store.LearningPermissionRule, Fingerprint: "real-rule-fp",
		ContentJSON:  `{"tool":"Bash","command":"printf corral-m6-ok","rule":"Bash(printf corral-m6-ok)"}`,
		BaselineJSON: `{}`, ExpiresMs: time.Now().Add(90 * 24 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	l, _ = st.TransitionLearning(ctx, l.ID, store.LearningTransition{From: store.LearningCandidate, To: store.LearningVerified})
	l, _ = st.TransitionLearning(ctx, l.ID, store.LearningTransition{From: store.LearningVerified, To: store.LearningProposed})
	if _, err := st.TransitionLearning(ctx, l.ID, store.LearningTransition{From: store.LearningProposed, To: store.LearningAdopted}); err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.NewString()
	createLoopSession(t, st, sessionID, repo, claudeBin)
	snapshot := make(map[string]string)
	for _, key := range envWhitelist {
		if value, ok := os.LookupEnv(key); ok {
			snapshot[key] = value
		}
	}
	stateDir := t.TempDir()
	r := New(st, state.New(st), killingCheckpointer{}, clock.Real(), Config{
		StateDir: stateDir, EnvSnapshot: snapshot, CorralVersion: "test", APIVersion: 1,
		OutputLogMaxBytes: 1 << 20, RelayCommand: "/usr/bin/true",
	}, nil)
	spec := session.Spec{
		ID: sessionID, Name: "real-session", Mode: session.ModeHeadless,
		Cwd: repo, ClaudeBin: claudeBin, SettingSources: "",
		Prompt: "Use the Bash tool once with command exactly `printf corral-m6-ok` (no quotes or extra shell syntax), then report its output. Do not modify files.",
	}
	if _, err := r.Spawn(ctx, spec); err != nil {
		t.Fatalf("real Claude spawn: %v", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		sess, err := st.GetSession(ctx, spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if sess.Status == session.StatusExited || sess.Status == session.StatusFailed {
			streamPath := filepath.Join(stateDir, "sessions", spec.ID, "stream.jsonl")
			stream, readErr := os.ReadFile(streamPath)
			if readErr != nil {
				stream = []byte("<unavailable: " + readErr.Error() + ">")
			}
			if sess.ExitCode == nil || *sess.ExitCode != 0 {
				exitCode := "nil"
				if sess.ExitCode != nil {
					exitCode = fmt.Sprintf("%d", *sess.ExitCode)
				}
				t.Fatalf("real Claude status=%s exit=%s stream:\n%s", sess.Status, exitCode, stream)
			}
			if !strings.Contains(string(stream), "printf corral-m6-ok") || !strings.Contains(string(stream), "corral-m6-ok") {
				t.Fatalf("real Claude stream did not confirm exact Bash invocation:\n%s", stream)
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("real Claude learning-rule verification timed out after 3m")
}
