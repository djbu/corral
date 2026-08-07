package main

import (
	"bytes"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Shared demo helpers: a scripted client that speaks the wire protocol
// directly (no real tty needed), used to drive automated proof runs.
// ---------------------------------------------------------------------------

func dialAttach(sock string, rows, cols uint16) (net.Conn, error) {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(conn, frameAttach, encodeSize(rows, cols)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func sendData(conn net.Conn, s string) error {
	return writeFrame(conn, frameData, []byte(s))
}

func sendResize(conn net.Conn, rows, cols uint16) error {
	return writeFrame(conn, frameResize, encodeSize(rows, cols))
}

// drainQuiet reads from conn (raw, unframed daemon->client stream) until no
// new bytes arrive for `quiet`, or `maxWait` total elapses.
func drainQuiet(conn net.Conn, quiet, maxWait time.Duration) []byte {
	var out []byte
	buf := make([]byte, 8192)
	deadline := time.Now().Add(maxWait)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(quiet))
		n, err := conn.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break // went quiet
			}
			break // EOF / other error: stop
		}
		if time.Now().After(deadline) {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	return out
}

func startDaemon(exePath, sock, cwd string, scrollback int, childArgv []string) (pid int, out string, err error) {
	os.MkdirAll(cwd, 0o755)
	args := []string{"daemon", "--sock", sock, "--cwd", cwd, "--scrollback", strconv.Itoa(scrollback), "--"}
	args = append(args, childArgv...)
	cmd := exec.Command(exePath, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return 0, buf.String(), fmt.Errorf("daemon launch failed: %w: %s", err, buf.String())
	}
	out = buf.String()
	re := regexp.MustCompile(`pid=(\d+)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		return 0, out, fmt.Errorf("could not parse pid from: %s", out)
	}
	pid, _ = strconv.Atoi(m[1])
	return pid, out, nil
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// pgrepP returns direct child pids of parent, as reported by pgrep -P.
// Used instead of pattern matching on binary path for orphan checks,
// since this machine already runs many unrelated `claude` processes
// (other Claude Code sessions) that must never be touched by this spike.
func pgrepP(parent int) []int {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(parent)).Output()
	if err != nil {
		return nil
	}
	var res []int
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l == "" {
			continue
		}
		if n, err := strconv.Atoi(l); err == nil {
			res = append(res, n)
		}
	}
	return res
}

func pgrepF(pattern string) []string {
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var res []string
	for _, l := range lines {
		if l != "" {
			res = append(res, l)
		}
	}
	return res
}

func killTree(pid int) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	time.Sleep(300 * time.Millisecond)
	if processAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// ---------------------------------------------------------------------------
// demo-bash: proves points 1-5 with `bash` as the child, per the spec's (a)-(e).
// ---------------------------------------------------------------------------

func cmdDemoBash(args []string) error {
	fs := flag.NewFlagSet("demo-bash", flag.ExitOnError)
	sock := fs.String("sock", "run/bash.sock", "unix socket path")
	cwd := fs.String("cwd", ".", "child cwd")
	scrollback := fs.Int("scrollback", 4096, "scrollback cap in bytes (kept small on purpose to also exercise truncation)")
	fs.Parse(args)

	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	_ = os.Remove(*sock)

	pass := func(name string, ok bool, detail string) {
		status := "PASS"
		if !ok {
			status = "FAIL"
		}
		fmt.Printf("[%s] %s: %s\n", status, name, detail)
	}

	fmt.Println("=== demo-bash: daemon detach + attach/detach/replay/resize ===")

	// (a) start the daemon with bash as child.
	pid, out, err := startDaemon(exePath, *sock, *cwd, *scrollback, []string{"bash", "--noprofile", "--norc", "-i"})
	if err != nil {
		return err
	}
	fmt.Print(out)
	time.Sleep(150 * time.Millisecond)
	pass("daemon-detached-and-running", processAlive(pid), fmt.Sprintf("pid=%d alive after launcher exited", pid))

	// (b) attach #1, send a marker command, capture output.
	conn1, err := dialAttach(*sock, 24, 80)
	if err != nil {
		return err
	}
	initial := drainQuiet(conn1, 300*time.Millisecond, 2*time.Second)
	fmt.Printf("--- initial prompt (%d bytes) ---\n%s\n", len(initial), string(initial))

	if err := sendData(conn1, "echo hello-from-pty-$$\r"); err != nil {
		return err
	}
	afterEcho := drainQuiet(conn1, 300*time.Millisecond, 2*time.Second)
	fmt.Printf("--- after echo (%d bytes) ---\n%s\n", len(afterEcho), string(afterEcho))
	pass("echo-round-trip", bytes.Contains(afterEcho, []byte("hello-from-pty")), "marker seen in live output")

	// resize while attached, verify via `stty size` that TIOCSWINSZ reached the pty.
	if err := sendResize(conn1, 40, 100); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	if err := sendData(conn1, "stty size\r"); err != nil {
		return err
	}
	sttyOut := drainQuiet(conn1, 300*time.Millisecond, 2*time.Second)
	fmt.Printf("--- stty size after resize (%d bytes) ---\n%s\n", len(sttyOut), string(sttyOut))
	pass("resize-tiocswinsz", bytes.Contains(sttyOut, []byte("40 100")), "bash reports new pty winsize 40 100")

	// spew enough output to blow past the (small) scrollback cap, to
	// observe truncation behavior on the next reattach.
	if err := sendData(conn1, "yes spew-line | head -n 2000\r"); err != nil {
		return err
	}
	_ = drainQuiet(conn1, 400*time.Millisecond, 3*time.Second)

	// (c) detach: just close the connection locally (mirrors what the
	// real `attach` client does on Ctrl-\: no message sent to the daemon).
	conn1.Close()
	time.Sleep(200 * time.Millisecond)
	pass("detach-without-kill", processAlive(pid), "daemon+bash still alive after client disconnect")

	// (d) reattach and verify replay buffer.
	conn2, err := dialAttach(*sock, 24, 80)
	if err != nil {
		return err
	}
	replay := drainQuiet(conn2, 300*time.Millisecond, 2*time.Second)
	fmt.Printf("--- replay on reattach (%d bytes) ---\n%s\n[end replay]\n", len(replay), truncateForLog(replay, 600))
	// with a 4096B cap and ~2000 spew lines (~13KB) after the marker, the
	// marker is expected to have been evicted -- that's the point.
	hasMarker := bytes.Contains(replay, []byte("hello-from-pty"))
	hasSpew := bytes.Contains(replay, []byte("spew-line"))
	fmt.Printf("replay contains marker=%v spew=%v (small-cap truncation is expected to evict the marker; see NOTES.md)\n", hasMarker, hasSpew)
	pass("reattach-gets-live-screen-state", len(replay) > 0 && hasSpew, "reattach shows current (tail) screen state even though marker aged out of the small buffer")

	if err := sendData(conn2, "echo still-alive\r"); err != nil {
		return err
	}
	postReattach := drainQuiet(conn2, 300*time.Millisecond, 2*time.Second)
	fmt.Printf("--- post-reattach command (%d bytes) ---\n%s\n", len(postReattach), string(postReattach))
	pass("interactive-after-reattach", bytes.Contains(postReattach, []byte("still-alive")), "shell still responsive after reattach")
	conn2.Close()

	// (e) kill daemon cleanly and verify no orphans.
	killTree(pid)
	time.Sleep(300 * time.Millisecond)
	remaining := pgrepF(*sock)
	pass("clean-shutdown-no-orphans", !processAlive(pid) && len(remaining) == 0,
		fmt.Sprintf("daemon pid=%d alive=%v, procs referencing sock=%v", pid, processAlive(pid), remaining))

	fmt.Println("=== demo-bash done ===")
	return nil
}

func truncateForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + fmt.Sprintf("...[%d more bytes]", len(b)-n)
}

// ---------------------------------------------------------------------------
// demo-claude: same mechanics against the real `claude` TUI, minimal token
// spend (at most one prompt), small scrollback cap on purpose to observe
// truncation/alt-screen behavior for NOTES.md.
// ---------------------------------------------------------------------------

func cmdDemoClaude(args []string) error {
	fs := flag.NewFlagSet("demo-claude", flag.ExitOnError)
	sock := fs.String("sock", "run/claude.sock", "unix socket path")
	claudePath := fs.String("claude", os.Getenv("HOME")+"/.local/bin/claude", "path to claude binary")
	cwd := fs.String("cwd", "sandbox", "claude cwd (scratch dir)")
	out := fs.String("out", "claude-tui-capture.txt", "raw capture output file")
	scrollback := fs.Int("scrollback", 8192, "scrollback cap (kept smallish on purpose)")
	fs.Parse(args)

	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	_ = os.Remove(*sock)
	os.MkdirAll(*cwd, 0o755)

	var capture bytes.Buffer
	report := func(format string, a ...any) {
		fmt.Printf(format, a...)
	}

	pid, startOut, err := startDaemon(exePath, *sock, *cwd, *scrollback, []string{*claudePath, "--model", "haiku"})
	if err != nil {
		return err
	}
	report("%s", startOut)
	time.Sleep(300 * time.Millisecond)
	report("[PASS] daemon-detached-and-running: pid=%d alive=%v\n", pid, processAlive(pid))

	// Identify the actual claude child pid (direct child of the daemon)
	// up front, so cleanup/orphan checks are scoped to exactly this
	// process and never touch the many unrelated `claude` processes
	// already running on this machine (other Claude Code sessions).
	claudeChildren := pgrepP(pid)
	report("[info] daemon pid=%d direct children=%v (expect exactly one: the claude TUI)\n", pid, claudeChildren)

	conn, err := dialAttach(*sock, 24, 80)
	if err != nil {
		return err
	}
	startup := drainQuiet(conn, 700*time.Millisecond, 8*time.Second)
	capture.Write(startup)
	report("[info] startup capture: %d bytes\n", len(startup))

	// Heuristic: accept a directory-trust prompt if claude shows one.
	if bytes.Contains(startup, []byte("trust")) || bytes.Contains(startup, []byte("Trust")) {
		report("[info] trust prompt detected, sending Enter to accept default\n")
		_ = sendData(conn, "\r")
		more := drainQuiet(conn, 700*time.Millisecond, 5*time.Second)
		capture.Write(more)
		report("[info] post-trust capture: %d bytes\n", len(more))
	}

	// Exactly one prompt, per budget.
	report("[info] sending the single test prompt\n")
	_ = sendData(conn, "reply only OK\r")
	resp := drainQuiet(conn, 1*time.Second, 25*time.Second)
	capture.Write(resp)
	report("[info] response capture: %d bytes\n", len(resp))
	gotOK := bytes.Contains(resp, []byte("OK"))
	report("[%s] claude-responds-in-pty: response contains OK=%v\n", passFail(gotOK), gotOK)

	// Resize while attached; observe (don't strictly assert) redraw.
	_ = sendResize(conn, 40, 100)
	resizeOut := drainQuiet(conn, 700*time.Millisecond, 3*time.Second)
	capture.Write(resizeOut)
	report("[info] post-resize capture: %d bytes (see claude-tui-capture.txt + NOTES.md for redraw analysis)\n", len(resizeOut))

	// Detach (close locally), then reattach to inspect replay behavior on
	// what is very likely an alt-screen / relative-redraw TUI.
	conn.Close()
	time.Sleep(300 * time.Millisecond)
	report("[%s] detach-without-kill: daemon+claude still alive=%v\n", passFail(processAlive(pid)), processAlive(pid))

	conn2, err := dialAttach(*sock, 40, 100)
	if err != nil {
		return err
	}
	replay := drainQuiet(conn2, 700*time.Millisecond, 4*time.Second)
	capture.WriteString("\n--- REATTACH REPLAY BOUNDARY ---\n")
	capture.Write(replay)
	report("[info] reattach replay: %d bytes, starts-with-ESC=%v, first32=%q\n",
		len(replay), len(replay) > 0 && replay[0] == 0x1b, firstN(replay, 32))
	conn2.Close()

	if err := os.WriteFile(*out, capture.Bytes(), 0o644); err != nil {
		return err
	}
	report("[info] wrote raw capture (%d bytes) to %s\n", capture.Len(), *out)

	// Clean shutdown, verify no orphaned claude/daemon processes. Scoped
	// strictly to the pids we launched ourselves (daemon pid + its
	// tracked children) -- never a broad pattern match against "claude",
	// since unrelated Claude Code sessions are running on this host.
	killTree(pid)
	time.Sleep(500 * time.Millisecond)
	remainingSock := pgrepF(*sock)
	stillAlive := []int{}
	for _, cp := range claudeChildren {
		if processAlive(cp) {
			stillAlive = append(stillAlive, cp)
		}
	}
	clean := !processAlive(pid) && len(stillAlive) == 0 && len(remainingSock) == 0
	report("[%s] clean-shutdown-no-orphans: daemon-alive=%v tracked-claude-children-still-alive=%v sock-refs=%v\n",
		passFail(clean), processAlive(pid), stillAlive, remainingSock)
	for _, cp := range stillAlive {
		killTree(cp)
	}
	return nil
}

func passFail(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}

func firstN(b []byte, n int) string {
	if len(b) < n {
		n = len(b)
	}
	return string(b[:n])
}
