package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/screen"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
)

// fakeClaudeBinOnce/fakeClaudeBinPath cache one build of test/fakeclaude
// across every test in this package that needs it — building a Go binary
// per test would dominate the suite's runtime.
var (
	fakeClaudeBinOnce sync.Once
	fakeClaudeBinPath string
	fakeClaudeBinErr  error
)

func buildFakeClaude(t *testing.T) string {
	t.Helper()
	fakeClaudeBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "corral-fakeclaude-")
		if err != nil {
			fakeClaudeBinErr = err
			return
		}
		bin := filepath.Join(dir, "fakeclaude")
		cmd := exec.Command("go", "build", "-o", bin, "github.com/djbu/corral/test/fakeclaude")
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeClaudeBinErr = err
			t.Logf("go build fakeclaude output:\n%s", out)
			return
		}
		fakeClaudeBinPath = bin
	})
	if fakeClaudeBinErr != nil {
		t.Fatalf("building test/fakeclaude: %v", fakeClaudeBinErr)
	}
	return fakeClaudeBinPath
}

// TestScreenBridge spawns fakeclaude in-process via Registry.Spawn (no
// daemon binary, no socket) and polls the live session's Screen for the
// alt-screen transition fakeclaude's startup sequence produces (design
// doc §10.1 item 3 / test/fakeclaude/tui.go's startupSequence): this is
// the PTY-reader-goroutine -> Screen.Feed wiring runGoroutines sets up in
// supervisor.go, exercised end to end for the first time outside a pure
// argv/env unit test.
func TestScreenBridge(t *testing.T) {
	claudeBin := buildFakeClaude(t)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corral.db"), clock.Real())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cwd := t.TempDir()
	spec := session.Spec{
		ID:             "33333333-3333-3333-3333-333333333333",
		Name:           "screen-bridge-1",
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

	r := New(st, state.New(st), killingCheckpointer{}, clock.Real(), Config{
		StateDir:      t.TempDir(),
		EnvSnapshot:   map[string]string{"PATH": os.Getenv("PATH")},
		CorralVersion: "test",
		APIVersion:    1,
	}, nil)

	if _, err := r.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _, _ = r.Kill(context.Background(), spec.ID, nil) })

	ls, ok := r.Get(spec.ID)
	if !ok {
		t.Fatal("Get(id) after Spawn = false, want true")
	}

	var last screen.Grid
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		last = ls.Screen.DebugGrid()
		if last.AltScreen && gridHasNonEmptyContent(last) && len(last.Modes) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Screen never reached alt-screen + non-empty content + sniffed modes within 5s; last grid: AltScreen=%v Modes=%v content=%q",
		last.AltScreen, last.Modes, renderGridText(last))
}

func gridHasNonEmptyContent(g screen.Grid) bool {
	return strings.TrimSpace(renderGridText(g)) != ""
}

func renderGridText(g screen.Grid) string {
	var b strings.Builder
	for _, row := range g.Cells {
		for _, c := range row {
			b.WriteString(c.Content)
		}
	}
	return b.String()
}
