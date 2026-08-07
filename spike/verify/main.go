// corral-verify: throwaway driver for M1 Step-0 verifications (V1/V2/V3).
// Not part of corral proper. See spike/verify/NOTES.md.
package main

import (
	"bytes"
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: corral-verify v1 [--claude PATH] [--cwd DIR] [--out DIR]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "v1":
		err = cmdV1(os.Args[2:])
	case "v3":
		err = cmdV3(os.Args[2:])
	case "resumepeek":
		err = cmdResumePeek(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// --------------------------------------------------------------------------
// PTY process wrapper: foreground (no daemon fork), single Go process
// owns the pty master directly. Setsid so the child is its own session
// and process-group leader (pgid == pid), same as real corral spawn.
// --------------------------------------------------------------------------

type ptyProc struct {
	cmd *exec.Cmd
	f   *os.File
	mu  sync.Mutex
	buf bytes.Buffer
	pid int
}

func startPTY(claudeBin string, args []string, cwd string) (*ptyProc, error) {
	cmd := exec.Command(claudeBin, args...)
	cmd.Dir = cwd
	cmd.Env = childEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		return nil, err
	}
	p := &ptyProc{cmd: cmd, f: f, pid: cmd.Process.Pid}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				p.mu.Lock()
				p.buf.Write(buf[:n])
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return p, nil
}

func (p *ptyProc) send(s string) error {
	_, err := p.f.Write([]byte(s))
	return err
}

func (p *ptyProc) snapshot() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.buf.Bytes()...)
}

// waitQuiet blocks until no new bytes have arrived for `quiet`, or until
// `maxWait` total has elapsed.
func (p *ptyProc) waitQuiet(quiet, maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	lastLen := -1
	stableSince := time.Now()
	for {
		p.mu.Lock()
		n := p.buf.Len()
		p.mu.Unlock()
		if n != lastLen {
			lastLen = n
			stableSince = time.Now()
		} else if time.Since(stableSince) >= quiet {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitExit waits up to `timeout` for the process to exit on its own.
// Returns true if it exited within the timeout.
func (p *ptyProc) waitExit(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (p *ptyProc) alive() bool {
	return syscall.Kill(p.pid, 0) == nil
}

// killGroup sends sig to the process group (pgid == pid, since Setsid).
// pid-scoped only -- never a name-based pkill, per the spike's explicit
// warning that this machine runs unrelated claude processes.
func (p *ptyProc) killGroup(sig syscall.Signal) {
	_ = syscall.Kill(-p.pid, sig)
}

// childEnv builds the spawned claude's environment from an explicit
// allowlist only (never os.Environ() wholesale). This driver itself runs
// inside a real Claude Code session, whose own os.Environ() carries
// CLAUDE_CODE_CHILD_SESSION -- passing that through to a nested claude
// disables transcript persistence entirely ("Transcript saving is off —
// inherited CLAUDE_CODE_CHILD_SESSION marker"), exactly the leak m1.md
// §7.2 calls out as finding #3. Mirrors the design's whitelist.
func childEnv() []string {
	allow := []string{
		"HOME", "USER", "LOGNAME", "SHELL", "PATH", "TMPDIR",
		"LANG", "LC_ALL", "LC_CTYPE", "TZ", "SSH_AUTH_SOCK",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	}
	var env []string
	for _, k := range allow {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "TERM=xterm-256color", "COLORTERM=truecolor")
	return env
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func slugFor(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}

// --------------------------------------------------------------------------
// V1: interactive session-id + resume semantics.
// --------------------------------------------------------------------------

func cmdV1(args []string) error {
	fs := flag.NewFlagSet("v1", flag.ExitOnError)
	claudeBin := fs.String("claude", os.Getenv("HOME")+"/.local/bin/claude", "claude binary")
	cwd := fs.String("cwd", "", "absolute sandbox cwd")
	outDir := fs.String("out", "", "output dir for raw captures")
	fs.Parse(args)
	if *cwd == "" || *outDir == "" {
		return fmt.Errorf("need --cwd and --out (absolute paths)")
	}
	if err := os.MkdirAll(*cwd, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	sid := newUUID()
	fmt.Printf("[v1] fresh session-id: %s\n", sid)
	fmt.Printf("[v1] cwd: %s\n", *cwd)

	// --- phase 1: fresh interactive session with --session-id ---
	p1, err := startPTY(*claudeBin, []string{"--session-id", sid, "--model", "haiku"}, *cwd)
	if err != nil {
		return fmt.Errorf("phase1 start: %w", err)
	}
	fmt.Printf("[v1] phase1 pid=%d\n", p1.pid)

	p1.waitQuiet(800*time.Millisecond, 12*time.Second)
	startup := p1.snapshot()
	writeCapture(*outDir, "v1-phase1-startup.raw", startup)

	if bytes.Contains(startup, []byte("rust")) || bytes.Contains(startup, []byte("Trust")) {
		fmt.Println("[v1] trust prompt detected, sending Enter")
		_ = p1.send("\r")
		p1.waitQuiet(800*time.Millisecond, 8*time.Second)
	}

	fmt.Println("[v1] sending prompt: reply only OK")
	_ = p1.send("reply only OK\r")
	p1.waitQuiet(1500*time.Millisecond, 30*time.Second)
	afterPrompt := p1.snapshot()
	writeCapture(*outDir, "v1-phase1-after-prompt.raw", afterPrompt)
	gotOK := bytes.Contains(afterPrompt, []byte("OK"))
	fmt.Printf("[v1] response contains OK=%v\n", gotOK)

	// --- graceful exit: try /exit first, then Ctrl-C, then SIGTERM/KILL ---
	exitMethod := "none"
	fmt.Println("[v1] sending /exit")
	_ = p1.send("/exit\r")
	if p1.waitExit(5 * time.Second) {
		exitMethod = "/exit"
	} else {
		fmt.Println("[v1] /exit did not exit process within 5s, trying Ctrl-C")
		_ = p1.send("\x03")
		if p1.waitExit(5 * time.Second) {
			exitMethod = "Ctrl-C"
		} else {
			fmt.Println("[v1] Ctrl-C did not exit, sending SIGTERM to group")
			p1.killGroup(syscall.SIGTERM)
			if p1.waitExit(5 * time.Second) {
				exitMethod = "SIGTERM"
			} else {
				fmt.Println("[v1] SIGTERM did not exit, sending SIGKILL to group")
				p1.killGroup(syscall.SIGKILL)
				p1.waitExit(5 * time.Second)
				exitMethod = "SIGKILL"
			}
		}
	}
	fmt.Printf("[v1] phase1 exit method that worked: %s, alive-after=%v\n", exitMethod, p1.alive())
	writeCapture(*outDir, "v1-phase1-full.raw", p1.snapshot())

	// --- verify session file exists ---
	slug := slugFor(*cwd)
	projDir := os.Getenv("HOME") + "/.claude/projects/" + slug
	sessFile := projDir + "/" + sid + ".jsonl"
	fi, statErr := os.Stat(sessFile)
	if statErr != nil {
		fmt.Printf("[v1] FAIL: session file not found: %s (%v)\n", sessFile, statErr)
	} else {
		fmt.Printf("[v1] PASS: session file exists: %s (%d bytes)\n", sessFile, fi.Size())
		copyFile(sessFile, *outDir+"/v1-session-post-phase1.jsonl")
	}

	time.Sleep(300 * time.Millisecond)

	// --- phase 2: --resume <sid>, observe direct-load vs picker, no input sent ---
	p2, err := startPTY(*claudeBin, []string{"--resume", sid, "--model", "haiku"}, *cwd)
	if err != nil {
		return fmt.Errorf("phase2 start: %w", err)
	}
	fmt.Printf("[v1] phase2 (resume) pid=%d\n", p2.pid)
	p2.waitQuiet(1200*time.Millisecond, 12*time.Second)
	resumeCap := p2.snapshot()
	writeCapture(*outDir, "v1-phase2-resume.raw", resumeCap)
	fmt.Printf("[v1] phase2 resume capture: %d bytes (see %s/v1-phase2-resume.raw + NOTES.md for picker-vs-direct-load verdict)\n", len(resumeCap), *outDir)

	// graceful exit of phase2, same escalation
	exitMethod2 := "none"
	_ = p2.send("/exit\r")
	if p2.waitExit(5 * time.Second) {
		exitMethod2 = "/exit"
	} else {
		_ = p2.send("\x03")
		if p2.waitExit(5 * time.Second) {
			exitMethod2 = "Ctrl-C"
		} else {
			p2.killGroup(syscall.SIGTERM)
			if p2.waitExit(5 * time.Second) {
				exitMethod2 = "SIGTERM"
			} else {
				p2.killGroup(syscall.SIGKILL)
				p2.waitExit(5 * time.Second)
				exitMethod2 = "SIGKILL"
			}
		}
	}
	fmt.Printf("[v1] phase2 exit method: %s, alive-after=%v\n", exitMethod2, p2.alive())
	writeCapture(*outDir, "v1-phase2-full.raw", p2.snapshot())

	if statErr == nil {
		copyFile(sessFile, *outDir+"/v1-session-post-phase2.jsonl")
	}

	// orphan check, pid-scoped only
	fmt.Printf("[v1] orphan check: p1(pid=%d) alive=%v, p2(pid=%d) alive=%v\n", p1.pid, p1.alive(), p2.pid, p2.alive())
	if p1.alive() {
		fmt.Println("[v1] cleaning up p1 group with SIGKILL")
		p1.killGroup(syscall.SIGKILL)
	}
	if p2.alive() {
		fmt.Println("[v1] cleaning up p2 group with SIGKILL")
		p2.killGroup(syscall.SIGKILL)
	}
	return nil
}

// --------------------------------------------------------------------------
// V3: SIGTERM flush semantics. Replicates spike/resume/02-kill-midturn.sh's
// method (background -p stream-json, wait for a real text_delta, signal
// mid-turn) but with SIGTERM to the process group instead of SIGKILL, and
// records exit latency. No pty needed -- headless -p mode.
// --------------------------------------------------------------------------

func cmdV3(args []string) error {
	fs := flag.NewFlagSet("v3", flag.ExitOnError)
	claudeBin := fs.String("claude", os.Getenv("HOME")+"/.local/bin/claude", "claude binary")
	cwd := fs.String("cwd", "", "absolute sandbox cwd")
	outDir := fs.String("out", "", "output dir for raw captures")
	fs.Parse(args)
	if *cwd == "" || *outDir == "" {
		return fmt.Errorf("need --cwd and --out (absolute paths)")
	}
	if err := os.MkdirAll(*cwd, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	prompt := "Count from 1 to 30, one number per line, thinking briefly about each before saying it"
	cmd := exec.Command(*claudeBin, "-p", prompt, "--model", "haiku",
		"--output-format", "stream-json", "--verbose", "--include-partial-messages")
	cmd.Dir = *cwd
	cmd.Env = childEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // own pgid == pid, like real corral spawn

	stdoutR, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderrF, err := os.Create(*outDir + "/v3-stderr.log")
	if err != nil {
		return err
	}
	cmd.Stderr = stderrF

	var mu sync.Mutex
	var buf bytes.Buffer
	streamFile, err := os.Create(*outDir + "/v3-stream-raw.jsonl")
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	fmt.Printf("[v3] started pid=%d (pgid=%d, own session leader)\n", pid, pid)

	go func() {
		r := make([]byte, 8192)
		for {
			n, err := stdoutR.Read(r)
			if n > 0 {
				mu.Lock()
				buf.Write(r[:n])
				mu.Unlock()
				streamFile.Write(r[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	fmt.Println("[v3] polling for a genuine text_delta stream_event...")
	deadline := time.Now().Add(30 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		mu.Lock()
		hit := bytes.Contains(buf.Bytes(), []byte(`"type":"text_delta"`))
		mu.Unlock()
		if hit {
			found = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !found {
		fmt.Println("[v3] FAIL: never observed a text_delta within 30s")
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		cmd.Wait()
		streamFile.Close()
		return fmt.Errorf("no text_delta observed")
	}

	mu.Lock()
	linesAtSignal := bytes.Count(buf.Bytes(), []byte("\n"))
	mu.Unlock()
	t0 := time.Now()
	fmt.Printf("[v3] text_delta observed after %v, %d stream lines captured; sending SIGTERM to pgid=-%d NOW\n",
		t0, linesAtSignal, pid)
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		fmt.Printf("[v3] SIGTERM send error: %v\n", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var exitLatency time.Duration
	var exitedOnTerm bool
	select {
	case werr := <-done:
		exitLatency = time.Since(t0)
		exitedOnTerm = true
		fmt.Printf("[v3] process exited %v after SIGTERM (wait err=%v)\n", exitLatency, werr)
	case <-time.After(10 * time.Second):
		fmt.Println("[v3] process did NOT exit within 10s of SIGTERM; escalating to SIGKILL")
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-done
		exitLatency = time.Since(t0)
		exitedOnTerm = false
		fmt.Printf("[v3] process exited %v after SIGTERM+SIGKILL escalation\n", exitLatency)
	}
	streamFile.Close()
	stderrF.Close()

	fmt.Printf("[v3] RESULT: exited-on-sigterm-alone=%v total-exit-latency=%v\n", exitedOnTerm, exitLatency)

	// locate + copy session file
	slug := slugFor(*cwd)
	projDir := os.Getenv("HOME") + "/.claude/projects/" + slug
	entries, _ := os.ReadDir(projDir)
	fmt.Printf("[v3] project dir %s entries: %d\n", projDir, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			src := projDir + "/" + e.Name()
			b, rerr := os.ReadFile(src)
			if rerr == nil {
				writeCapture(*outDir, "v3-session-post-sigterm.jsonl", b)
				lines := bytes.Count(b, []byte("\n"))
				hasAssistant := bytes.Contains(b, []byte(`"type":"assistant"`))
				fmt.Printf("[v3] session file %s: %d bytes, %d lines, has assistant record=%v\n", src, len(b), lines, hasAssistant)
			}
		}
	}

	// orphan check, pid-scoped only
	alive := syscall.Kill(pid, 0) == nil
	fmt.Printf("[v3] orphan check: pid=%d alive=%v\n", pid, alive)
	if alive {
		fmt.Println("[v3] cleaning up leftover pid with SIGKILL")
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return nil
}

// --------------------------------------------------------------------------
// resumepeek: --resume <sid> against a session id that ended on a SIGTERM
// interrupt marker (not a clean /exit), to check corral's actual §3.5
// restart scenario. No prompt is sent -- same zero-billed-turn trick as
// V1 phase2 -- only observing direct-load vs picker and whether the
// interrupted turn renders.
// --------------------------------------------------------------------------

func cmdResumePeek(args []string) error {
	fs := flag.NewFlagSet("resumepeek", flag.ExitOnError)
	claudeBin := fs.String("claude", os.Getenv("HOME")+"/.local/bin/claude", "claude binary")
	cwd := fs.String("cwd", "", "absolute sandbox cwd (must match the session's original cwd)")
	sid := fs.String("session-id", "", "session id to resume")
	outDir := fs.String("out", "", "output dir for raw captures")
	fs.Parse(args)
	if *cwd == "" || *sid == "" || *outDir == "" {
		return fmt.Errorf("need --cwd, --session-id, and --out (absolute paths)")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	p, err := startPTY(*claudeBin, []string{"--resume", *sid, "--model", "haiku"}, *cwd)
	if err != nil {
		return fmt.Errorf("resumepeek start: %w", err)
	}
	fmt.Printf("[resumepeek] pid=%d sid=%s cwd=%s\n", p.pid, *sid, *cwd)
	p.waitQuiet(1200*time.Millisecond, 12*time.Second)
	cap := p.snapshot()
	writeCapture(*outDir, "resumepeek.raw", cap)
	fmt.Printf("[resumepeek] capture: %d bytes\n", len(cap))

	exitMethod := "none"
	_ = p.send("/exit\r")
	if p.waitExit(5 * time.Second) {
		exitMethod = "/exit"
	} else {
		_ = p.send("\x03")
		if p.waitExit(5 * time.Second) {
			exitMethod = "Ctrl-C"
		} else {
			p.killGroup(syscall.SIGTERM)
			if p.waitExit(5 * time.Second) {
				exitMethod = "SIGTERM"
			} else {
				p.killGroup(syscall.SIGKILL)
				p.waitExit(5 * time.Second)
				exitMethod = "SIGKILL"
			}
		}
	}
	fmt.Printf("[resumepeek] exit method: %s, alive-after=%v\n", exitMethod, p.alive())

	if p.alive() {
		p.killGroup(syscall.SIGKILL)
	}
	return nil
}

func writeCapture(outDir, name string, b []byte) {
	_ = os.WriteFile(outDir+"/"+name, b, 0o644)
}

func copyFile(src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		return
	}
	_ = os.WriteFile(dst, b, 0o644)
}
