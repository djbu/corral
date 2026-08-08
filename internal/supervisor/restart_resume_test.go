package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/claude/sessions"
	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/proto"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
)

// restartResumeCheckpointer mirrors internal/checkpoint.ResumeCheckpointer
// (SIGTERM/grace/SIGKILL + persist claude_session_id; Restore rebuilds a
// --resume Spec; Resumable checks for an on-disk transcript) closely
// enough to exercise recover.go's full path end to end, without importing
// internal/checkpoint (which already imports this package for
// *LiveSession — importing it back here would be a cycle). It is declared
// once per test file rather than shared with lifecycle_test.go's
// escalatingCheckpointer because it additionally needs a ClaudeHome and
// the env inputs Restore's BuildEnv call requires.
type restartResumeCheckpointer struct {
	store          *store.Store
	clk            clock.Clock
	grace          time.Duration
	claudeHome     string
	envSnapshot    map[string]string
	envPassthrough []string
}

func (c *restartResumeCheckpointer) Checkpoint(ctx context.Context, s *LiveSession, reason string) error {
	if _, err := c.store.UpdateSession(ctx, s.SessionID, func(sess *session.Session) {
		sess.Status = session.StatusStopping
	}); err != nil {
		return err
	}
	exitSignal := "SIGTERM"
	_ = killGroupTolerant(s.PGID, syscall.SIGTERM)
	select {
	case <-ctx.Done():
	case <-c.clk.After(c.grace):
		exitSignal = "SIGKILL"
		_ = killGroupTolerant(s.PGID, syscall.SIGKILL)
	}
	endedAtMs := c.clk.Now().UnixMilli()
	_, err := c.store.UpdateSession(ctx, s.SessionID, func(sess *session.Session) {
		sess.ClaudeSessionID = s.SessionID
		sess.Status = session.StatusExited
		sess.ExitSignal = exitSignal
		sess.EndedAtMs = &endedAtMs
	})
	return err
}

func (c *restartResumeCheckpointer) Restore(ctx context.Context, rec session.Session) (session.Spec, error) {
	spec := session.Spec{
		ID:             rec.ID,
		Name:           rec.Name,
		Mode:           rec.Mode,
		Cwd:            rec.Cwd,
		ClaudeBin:      rec.ClaudeBin,
		Model:          rec.Model,
		SettingSources: rec.SettingSources,
		SettingsPath:   rec.SettingsPath,
		Rows:           uint16(rec.Rows),
		Cols:           uint16(rec.Cols),
		ResumeFrom:     rec.ClaudeSessionID,
	}
	spec.Env = BuildEnv(spec, c.envSnapshot, c.envPassthrough, "xterm-256color", "/tmp/corral-test.sock")
	return spec, nil
}

func (c *restartResumeCheckpointer) Resumable(rec session.Session) (bool, string) {
	if rec.ClaudeSessionID == "" {
		return false, "no claude_session_id recorded"
	}
	path := sessions.TranscriptPath(c.claudeHome, rec.Cwd, rec.ClaudeSessionID)
	if _, err := os.Stat(path); err != nil {
		return false, "transcript not found"
	}
	return true, ""
}

// TestGracefulRestartResumes is the headline step-9 test (design doc
// §3.5/§3.6): spawn a session, simulate a daemon shutdown (checkpoint
// every live session the way internal/daemon/shutdown.go's
// checkpointLive does), build a second Registry against the same store
// (standing in for a daemon restart) and call Recover — it must resume
// the session with --resume (never --session-id again), bump
// resume_count to 1, and emit session.resumed.
func TestGracefulRestartResumes(t *testing.T) {
	claudeBin := buildFakeClaude(t)

	dbDir := t.TempDir()
	st, err := store.Open(filepath.Join(dbDir, "corral.db"), clock.Real())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	homeDir := t.TempDir()   // stands in for $HOME; claudeHome = homeDir/.claude
	fakeState := t.TempDir() // $CORRAL_FAKE_STATE: where invocation-N.json lands
	cwd := t.TempDir()
	stateDir := t.TempDir() // corral's own StateDir (settings.Pin's root)

	snapshot := map[string]string{
		"PATH":              os.Getenv("PATH"),
		"CORRAL_FAKE_HOME":  homeDir,
		"CORRAL_FAKE_STATE": fakeState,
	}
	passthrough := []string{"CORRAL_FAKE_HOME", "CORRAL_FAKE_STATE"}

	cp := &restartResumeCheckpointer{
		store:          st,
		clk:            clock.Real(),
		grace:          100 * time.Millisecond,
		claudeHome:     filepath.Join(homeDir, ".claude"),
		envSnapshot:    snapshot,
		envPassthrough: passthrough,
	}

	spec := session.Spec{
		ID:             newTestUUID(),
		Name:           "resume-1",
		Mode:           session.ModeInteractive,
		Cwd:            cwd,
		ClaudeBin:      claudeBin,
		SettingSources: "user,project,local",
		Rows:           24,
		Cols:           80,
	}
	if _, err := st.CreateSession(context.Background(), store.CreateSessionParams{
		ID:             spec.ID,
		Name:           spec.Name,
		Mode:           spec.Mode,
		Cwd:            spec.Cwd,
		ClaudeBin:      spec.ClaudeBin,
		SettingSources: spec.SettingSources,
		DesiredState:   session.DesiredRunning,
		Status:         session.StatusStarting,
		Rows:           24,
		Cols:           80,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	engine := state.New(st)
	reg1 := New(st, engine, cp, clock.Real(), Config{
		StateDir:       stateDir,
		EnvSnapshot:    snapshot,
		EnvPassthrough: passthrough,
		CorralVersion:  "test",
		APIVersion:     1,
	}, nil)

	if _, err := reg1.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	ls, ok := reg1.Get(spec.ID)
	if !ok {
		t.Fatal("Get(id) after Spawn = false, want true")
	}

	// Send one line of "input" so fakeclaude's appendPrompt creates the
	// transcript file Resumable checks for (test/fakeclaude/transcript.go:
	// the file does not exist until the first prompt).
	if _, err := ls.PTYMaster.Write([]byte("hello\n")); err != nil {
		t.Fatalf("writing to PTY master: %v", err)
	}
	transcriptPath := sessions.TranscriptPath(cp.claudeHome, cwd, spec.ID)
	waitForFile(t, transcriptPath, 5*time.Second)

	// Simulate a daemon shutdown: checkpoint every live session, exactly
	// as internal/daemon/shutdown.go's checkpointLive does via
	// d.supervisor.ListLive().
	for _, live := range reg1.ListLive() {
		if err := cp.Checkpoint(context.Background(), live, "daemon_shutdown"); err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
	}

	waitForCondition(t, 5*time.Second, func() bool {
		rec, err := st.GetSession(context.Background(), spec.ID)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		return rec.Status == session.StatusExited
	})

	preRestart, err := st.GetSession(context.Background(), spec.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if preRestart.DesiredState != session.DesiredRunning {
		t.Fatalf("DesiredState after shutdown = %q, want %q (shutdown never changes intent)", preRestart.DesiredState, session.DesiredRunning)
	}
	if preRestart.Status != session.StatusExited {
		t.Fatalf("Status after shutdown = %q, want %q", preRestart.Status, session.StatusExited)
	}

	// "Restart": a fresh Registry against the same store, standing in for
	// a new daemon process.
	reg2 := New(st, engine, cp, clock.Real(), Config{
		StateDir:       stateDir,
		EnvSnapshot:    snapshot,
		EnvPassthrough: passthrough,
		CorralVersion:  "test",
		APIVersion:     1,
	}, nil)

	if err := reg2.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	var resumed *session.Session
	waitForCondition(t, 5*time.Second, func() bool {
		rec, err := st.GetSession(context.Background(), spec.ID)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		resumed = rec
		return rec.Status == session.StatusRunning
	})

	if resumed.ResumeCount != 1 {
		t.Fatalf("ResumeCount = %d, want 1", resumed.ResumeCount)
	}

	events, err := st.ListEvents(context.Background(), spec.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !hasEventKind(events, session.EventSessionResumed) {
		t.Fatalf("events = %v, want a %q event", eventKinds(events), session.EventSessionResumed)
	}

	invPath := filepath.Join(fakeState, spec.ID, "invocation-2.json")
	waitForFile(t, invPath, 5*time.Second)
	b, err := os.ReadFile(invPath)
	if err != nil {
		t.Fatalf("reading %s: %v", invPath, err)
	}
	var inv struct {
		Argv    []string `json:"argv"`
		Environ []string `json:"environ"`
	}
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatalf("unmarshaling %s: %v", invPath, err)
	}
	if !containsArg(inv.Argv, "--resume") {
		t.Fatalf("invocation-2 argv = %v, want it to contain --resume", inv.Argv)
	}
	if containsArg(inv.Argv, "--session-id") {
		t.Fatalf("invocation-2 argv = %v, want it to NOT contain --session-id on resume", inv.Argv)
	}

	allowed := map[string]bool{}
	for k := range snapshot {
		allowed[k] = true
	}
	for _, extra := range []string{"TERM", "COLORTERM", "PWD", "CORRAL_SESSION_ID", "CORRAL_SOCK"} {
		allowed[extra] = true
	}
	for _, kv := range inv.Environ {
		k, _, _ := strings.Cut(kv, "=")
		if !allowed[k] {
			t.Fatalf("invocation-2 environ contains unexpected key %q (env passthrough is a strict allowlist)", k)
		}
	}

	// Deferred reattach (design doc step 10): confirms Attach's
	// Hello/Ready/repaint path works against a session that came back
	// through Recover, not just one that was freshly Spawned — reg2's
	// LiveSession is a distinct *LiveSession from reg1's, wired up by
	// recover.go rather than Spawn directly. The repaint must contain
	// the "hello" prompt line fakeclaude redraws from the on-disk
	// transcript loadTranscript reads on --resume (see the write to
	// ls.PTYMaster and the transcriptPath wait near the top of this
	// test), proving the resumed process's redraw — not just its
	// startup banner — reaches an attaching client.
	ls2, ok := reg2.Get(spec.ID)
	if !ok {
		t.Fatal("reg2.Get(id) after Recover = false, want true")
	}
	waitForCondition(t, 5*time.Second, func() bool { return ls2.Screen.AltScreen() })

	client, server := net.Pipe()
	defer client.Close()
	fc := newFakeClient(t, client)
	attachDone := make(chan struct{})
	go func() {
		_ = reg2.Attach(ls2, server, bufio.NewReader(server))
		close(attachDone)
	}()

	sendHello(t, client, proto.Hello{Rows: 24, Cols: 80, TakeOver: true, ClientVersion: "reattach"})
	readyFrame := fc.next(t, proto.TypeReady, 5*time.Second)
	var readyPayload proto.Ready
	if err := proto.DecodeJSON(readyFrame.Payload, &readyPayload); err != nil {
		t.Fatalf("decoding Ready: %v", err)
	}
	if readyPayload.RepaintBytes == 0 {
		t.Fatalf("Ready.RepaintBytes = 0, want > 0 after resume")
	}

	repaint := fc.next(t, proto.TypeOutput, 5*time.Second)
	// A bare Contains(payload, "hello") would also pass if "hello" turned
	// up anywhere else in a full-screen dump (a common word, ClientVersion
	// echoes, etc.) without proving fakeclaude actually redrew it at the
	// right place. Ready's RepaintBytes comes from internal/screen's own
	// synthesized full-screen redraw (a per-cell re-render of Screen's
	// buffer, not a byte-for-byte replay of fakeclaude's original
	// "\x1b[2K"-clearing writes), so pin the cursor address instead: row 5
	// (tui.go's transcriptStartRow) col 1, immediately followed by
	// "hello" — confirmed against this exact repaint's bytes
	// ("\x1b[5;1Hhello\x1b[0m...").
	const wantRedraw = "\x1b[5;1Hhello"
	if !strings.Contains(string(repaint.Payload), wantRedraw) {
		t.Fatalf("post-resume repaint does not contain cursor-addressed redraw %q:\n%q", wantRedraw, repaint.Payload)
	}

	if err := proto.WriteFrame(client, proto.Frame{Type: proto.TypeGoodbyeClose, Payload: mustJSON(t, proto.GoodbyeClose{Reason: "detach"})}); err != nil {
		t.Fatalf("WriteFrame(GoodbyeClose): %v", err)
	}
	select {
	case <-attachDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not return after GoodbyeClose")
	}

	// Clean up the resumed child.
	_, _ = reg2.Kill(context.Background(), spec.ID, nil)
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	waitForCondition(t, timeout, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %v", timeout)
	}
}
