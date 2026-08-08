// corral-hookspty: throwaway PTY driver for M2 Step-0 TUI-mode hook items
// (V6, V15, V16, V21, plus a free V14/V18 check on teardown). Modeled on
// spike/verify/main.go's ptyProc. Not part of corral proper.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type ptyProc struct {
	cmd *exec.Cmd
	f   *os.File
	mu  sync.Mutex
	buf bytes.Buffer
	pid int
}

func startPTY(claudeBin string, args []string, cwd string, env []string) (*ptyProc, error) {
	cmd := exec.Command(claudeBin, args...)
	cmd.Dir = cwd
	cmd.Env = env
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

// waitFor polls until any of the needles appears in the buffer, or timeout.
// Returns (found, elapsed since call start).
func (p *ptyProc) waitFor(needles [][]byte, timeout time.Duration) (bool, time.Duration) {
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		p.mu.Lock()
		b := p.buf.Bytes()
		hit := false
		for _, n := range needles {
			if bytes.Contains(b, n) {
				hit = true
				break
			}
		}
		p.mu.Unlock()
		if hit {
			return true, time.Since(start)
		}
		if time.Now().After(deadline) {
			return false, time.Since(start)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

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

func (p *ptyProc) killGroup(sig syscall.Signal) {
	_ = syscall.Kill(-p.pid, sig)
}

func childEnv(extra map[string]string) []string {
	allow := []string{
		"HOME", "USER", "LOGNAME", "SHELL", "PATH", "TMPDIR",
		"LANG", "LC_ALL", "LC_CTYPE", "TZ", "SSH_AUTH_SOCK",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS",
	}
	var env []string
	for _, k := range allow {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "TERM=xterm-256color", "COLORTERM=truecolor")
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func writeCapture(outDir, name string, b []byte) {
	_ = os.WriteFile(outDir+"/"+name, b, 0o644)
}

// gracefulExit tries /exit, then Ctrl-C, then SIGTERM, then SIGKILL.
func gracefulExit(p *ptyProc) string {
	_ = p.send("/exit\r")
	if p.waitExit(5 * time.Second) {
		return "/exit"
	}
	_ = p.send("\x03")
	if p.waitExit(5 * time.Second) {
		return "Ctrl-C"
	}
	p.killGroup(syscall.SIGTERM)
	if p.waitExit(5 * time.Second) {
		return "SIGTERM"
	}
	p.killGroup(syscall.SIGKILL)
	p.waitExit(5 * time.Second)
	return "SIGKILL"
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: corral-hookspty permission|sigtermend [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "permission":
		err = cmdPermission(os.Args[2:])
	case "sigtermend":
		err = cmdSigtermEnd(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// cmdPermission drives one interactive session in a cwd with NO allow rules,
// sends a prompt that needs the Bash tool, watches for the permission
// dialog (V21/V16), watches the hook capture file for a Notification event
// and records the timing delta (V6/V15), then approves or denies (V16/V14)
// and tears down.
func cmdPermission(args []string) error {
	fs := flag.NewFlagSet("permission", flag.ExitOnError)
	claudeBin := fs.String("claude", os.Getenv("HOME")+"/.local/share/claude/versions/2.1.224", "claude binary")
	cwd := fs.String("cwd", "", "sandbox cwd (fresh, no allow rules)")
	settings := fs.String("settings", "", "--settings file (pinned hooks)")
	capFile := fs.String("capfile", "", "hook capture jsonl the settings file writes to")
	outDir := fs.String("out", "", "output dir for raw captures")
	sid := fs.String("session-id", "", "fresh session id")
	decision := fs.String("decision", "approve", "approve|deny")
	prompt := fs.String("prompt", "Use the Bash tool to run: echo hook-test-marker-tui ; then reply with exactly the word DONE.", "prompt to send")
	settingSources := fs.String("setting-sources", "", "optional --setting-sources value")
	permMode := fs.String("permission-mode", "", "optional --permission-mode value")
	fs.Parse(args)
	if *cwd == "" || *settings == "" || *outDir == "" || *sid == "" {
		return fmt.Errorf("need --cwd --settings --out --session-id")
	}
	os.MkdirAll(*outDir, 0o755)

	// truncate cap file so we can watch for fresh events only
	if *capFile != "" {
		os.WriteFile(*capFile, nil, 0o644)
	}

	claudeArgs := []string{"--settings", *settings, "--session-id", *sid, "--model", "haiku"}
	if *settingSources != "" {
		claudeArgs = append(claudeArgs, "--setting-sources", *settingSources)
	}
	if *permMode != "" {
		claudeArgs = append(claudeArgs, "--permission-mode", *permMode)
	}
	env := childEnv(map[string]string{"CORRAL_HOOKCAP_TESTVAR": "v10-tui-marker"})
	p, err := startPTY(*claudeBin, claudeArgs, *cwd, env)
	if err != nil {
		return err
	}
	fmt.Printf("[perm] pid=%d cwd=%s sid=%s decision=%s\n", p.pid, *cwd, *sid, *decision)

	p.waitQuiet(800*time.Millisecond, 12*time.Second)
	startup := p.snapshot()
	writeCapture(*outDir, "perm-startup.raw", startup)
	if bytes.Contains(startup, []byte("rust")) || bytes.Contains(startup, []byte("Trust")) {
		fmt.Println("[perm] trust dialog detected, sending Enter")
		_ = p.send("\r")
		p.waitQuiet(800*time.Millisecond, 8*time.Second)
	}

	fmt.Println("[perm] sending prompt")
	tPromptSent := time.Now()
	_ = p.send(*prompt + "\r")

	// Watch for dialog markers -- Claude Code permission dialogs typically
	// render "Do you want" / numbered options / a bordered box naming the
	// tool. We watch broadly and record raw snapshots at intervals so the
	// exact text is captured regardless of exact wording.
	found, dialogElapsed := p.waitFor([][]byte{
		[]byte("Do you want"), []byte("permission"), []byte("1. Yes"), []byte("❯ 1"),
	}, 25*time.Second)
	tDialogSeen := time.Now()
	dialogSnap := p.snapshot()
	writeCapture(*outDir, "perm-dialog.raw", dialogSnap)
	fmt.Printf("[perm] dialog detected=%v after %v (since prompt sent)\n", found, dialogElapsed)

	// Poll the capture file for a Notification event, recording the delta
	// between dialog-seen and Notification-hook-fired (V15).
	notifSeenAt := time.Time{}
	if *capFile != "" {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			b, _ := os.ReadFile(*capFile)
			if bytes.Contains(b, []byte(`"hook_event_name":"Notification"`)) {
				notifSeenAt = time.Now()
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !notifSeenAt.IsZero() {
		fmt.Printf("[perm] Notification hook observed %v after dialog-seen, %v after prompt-sent\n",
			notifSeenAt.Sub(tDialogSeen), notifSeenAt.Sub(tPromptSent))
	} else {
		fmt.Println("[perm] Notification hook NOT observed within 10s of dialog appearing")
	}

	// approve or deny
	if *decision == "approve" {
		fmt.Println("[perm] sending 1 + Enter to approve")
		_ = p.send("1\r")
	} else {
		fmt.Println("[perm] sending Escape then 2 + Enter to deny (best-effort)")
		_ = p.send("\x1b")
		time.Sleep(300 * time.Millisecond)
	}
	p.waitQuiet(1500*time.Millisecond, 20*time.Second)
	writeCapture(*outDir, "perm-after-decision.raw", p.snapshot())

	exitMethod := gracefulExit(p)
	fmt.Printf("[perm] exit method: %s alive-after=%v\n", exitMethod, p.alive())
	writeCapture(*outDir, "perm-full.raw", p.snapshot())

	if p.alive() {
		p.killGroup(syscall.SIGKILL)
	}
	return nil
}

// cmdSigtermEnd starts an interactive session, lets SessionStart fire, sends
// no prompt (free), then SIGTERMs the group and checks whether SessionEnd
// fires and with what reason (V18), and whether the interactive/TUI
// entrypoint has the same flush behavior noted for -p in spike/verify.
func cmdSigtermEnd(args []string) error {
	fs := flag.NewFlagSet("sigtermend", flag.ExitOnError)
	claudeBin := fs.String("claude", os.Getenv("HOME")+"/.local/share/claude/versions/2.1.224", "claude binary")
	cwd := fs.String("cwd", "", "sandbox cwd")
	settings := fs.String("settings", "", "--settings file (pinned hooks)")
	capFile := fs.String("capfile", "", "hook capture jsonl")
	sid := fs.String("session-id", "", "fresh session id")
	outDir := fs.String("out", "", "output dir")
	fs.Parse(args)
	if *cwd == "" || *settings == "" || *sid == "" || *outDir == "" {
		return fmt.Errorf("need --cwd --settings --session-id --out")
	}
	os.MkdirAll(*outDir, 0o755)
	if *capFile != "" {
		os.WriteFile(*capFile, nil, 0o644)
	}
	env := childEnv(nil)
	p, err := startPTY(*claudeBin, []string{"--settings", *settings, "--session-id", *sid, "--model", "haiku", "--permission-mode", "bypassPermissions"}, *cwd, env)
	if err != nil {
		return err
	}
	fmt.Printf("[sigtermend] pid=%d\n", p.pid)
	p.waitQuiet(800*time.Millisecond, 10*time.Second)
	writeCapture(*outDir, "sigtermend-startup.raw", p.snapshot())

	t0 := time.Now()
	p.killGroup(syscall.SIGTERM)
	exited := p.waitExit(10 * time.Second)
	latency := time.Since(t0)
	fmt.Printf("[sigtermend] exited=%v latency=%v\n", exited, latency)
	if !exited {
		p.killGroup(syscall.SIGKILL)
		p.waitExit(5 * time.Second)
	}
	time.Sleep(200 * time.Millisecond)
	if *capFile != "" {
		b, _ := os.ReadFile(*capFile)
		writeCapture(*outDir, "sigtermend-capture-snapshot.jsonl", b)
	}
	if p.alive() {
		p.killGroup(syscall.SIGKILL)
	}
	return nil
}
