// corral-spike-pty: throwaway spike proving that a detached background
// process can own a TUI child inside a PTY and support attach/detach from
// separate client processes over a unix socket. See NOTES.md.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const detachByte = 0x1c // Ctrl-\ (FS). Chosen because claude/bash don't bind it.

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	rest := os.Args[2:]
	var err error
	switch sub {
	case "daemon":
		err = cmdDaemon(rest)
	case "_run": // internal: the actual detached daemon body, re-exec target only
		err = cmdRun(rest)
	case "attach":
		err = cmdAttach(rest)
	case "demo-bash":
		err = cmdDemoBash(rest)
	case "demo-claude":
		err = cmdDemoClaude(rest)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  corral-spike-pty daemon --sock PATH --cwd DIR [--scrollback N] -- CMD [ARGS...]
  corral-spike-pty attach --sock PATH
  corral-spike-pty demo-bash --sock PATH
  corral-spike-pty demo-claude --sock PATH --claude PATH --cwd DIR --out FILE`)
}

// ---------------------------------------------------------------------------
// "daemon" subcommand: the launcher. Re-execs itself as "_run" with
// SysProcAttr.Setsid so the resulting process has no controlling terminal
// and survives the launching terminal disconnecting, then exits itself
// once the detached process reports it is listening.
// ---------------------------------------------------------------------------

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	sock := fs.String("sock", "", "unix socket path")
	cwd := fs.String("cwd", ".", "child working directory")
	scrollback := fs.Int("scrollback", 256*1024, "scrollback buffer size in bytes")
	logPath := fs.String("log", "", "daemon log file (default: <sock>.log)")
	fs.Parse(args)
	cmdArgs := fs.Args()
	if *sock == "" || len(cmdArgs) == 0 {
		return fmt.Errorf("need --sock and -- CMD [ARGS...]")
	}
	if *logPath == "" {
		*logPath = *sock + ".log"
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	readyR, readyW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readyR.Close()

	logFile, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return err
	}

	runArgs := []string{"_run", "--sock", *sock, "--cwd", *cwd,
		"--scrollback", fmt.Sprint(*scrollback), "--ready-fd", "3"}
	runArgs = append(runArgs, "--")
	runArgs = append(runArgs, cmdArgs...)

	child := exec.Command(exePath, runArgs...)
	child.Stdin = devnull
	child.Stdout = logFile
	child.Stderr = logFile
	child.ExtraFiles = []*os.File{readyW} // becomes fd 3 in child
	// The key detach primitive: new session, no controlling terminal.
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := child.Start(); err != nil {
		return err
	}
	// Parent no longer needs its copy of the write end; the detached
	// child keeps its own (already dup'd into fd 3 there).
	readyW.Close()
	devnull.Close()
	logFile.Close()

	// Don't reap the detached process from here: it must outlive us.
	// Release it from the exec.Cmd bookkeeping so nothing waits on it.
	pid := child.Process.Pid
	child.Process.Release()

	line, rerr := waitReady(readyR, 5*time.Second)
	if rerr != nil {
		return fmt.Errorf("daemon (pid %d) did not report readiness: %w (see %s)", pid, rerr, *logPath)
	}
	if line != "OK" {
		return fmt.Errorf("daemon (pid %d) failed to start: %s (see %s)", pid, line, *logPath)
	}
	fmt.Printf("daemon started pid=%d sock=%s log=%s\n", pid, *sock, *logPath)
	return nil
}

func waitReady(r *os.File, timeout time.Duration) (string, error) {
	done := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		br := bufio.NewReader(r)
		line, err := br.ReadString('\n')
		if err != nil {
			errc <- err
			return
		}
		done <- line[:len(line)-1]
	}()
	select {
	case l := <-done:
		return l, nil
	case e := <-errc:
		return "", e
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout")
	}
}

// ---------------------------------------------------------------------------
// "_run": the actual detached daemon. Opens a PTY, spawns the child TUI in
// it, listens on a unix socket, and relays bytes between whichever client
// is currently attached and the PTY master.
// ---------------------------------------------------------------------------

type daemon struct {
	sb       *scrollback
	ptyM     *os.File
	mu       sync.Mutex
	curConn  net.Conn
	sockPath string
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("_run", flag.ExitOnError)
	sock := fs.String("sock", "", "unix socket path")
	cwd := fs.String("cwd", ".", "child working directory")
	scrollbackSz := fs.Int("scrollback", 256*1024, "scrollback buffer size")
	readyFd := fs.Int("ready-fd", -1, "fd to report readiness on")
	fs.Parse(args)
	cmdArgs := fs.Args()

	var readyW *os.File
	if *readyFd >= 0 {
		readyW = os.NewFile(uintptr(*readyFd), "ready")
	}
	reportErr := func(e error) error {
		if readyW != nil {
			fmt.Fprintf(readyW, "ERR: %v\n", e)
			readyW.Close()
		}
		return e
	}

	if len(cmdArgs) == 0 {
		return reportErr(fmt.Errorf("no child command given"))
	}

	// Confirm we really have no controlling terminal now (sanity check
	// logged for NOTES.md purposes).
	fmt.Printf("[_run] pid=%d ppid=%d\n", os.Getpid(), os.Getppid())
	if sid, err := syscall.Getsid(0); err == nil {
		fmt.Printf("[_run] sid=%d (== pid means new session, i.e. detached)\n", sid)
	}

	c := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	c.Dir = *cwd
	c.Env = os.Environ()
	ptyMaster, err := pty.StartWithSize(c, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return reportErr(fmt.Errorf("spawn child in pty: %w", err))
	}
	fmt.Printf("[_run] child pid=%d spawned in pty, argv=%v cwd=%s\n", c.Process.Pid, cmdArgs, *cwd)

	_ = os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		return reportErr(fmt.Errorf("listen %s: %w", *sock, err))
	}
	fmt.Printf("[_run] listening on %s\n", *sock)

	d := &daemon{
		sb:       newScrollback(*scrollbackSz),
		ptyM:     ptyMaster,
		sockPath: *sock,
	}

	if readyW != nil {
		fmt.Fprintln(readyW, "OK")
		readyW.Close()
	}

	// Reader: PTY master -> scrollback + live client. Always draining
	// regardless of attachment, so the child never blocks on output.
	shutdown := make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptyMaster.Read(buf)
			if n > 0 {
				d.sb.Feed(buf[:n])
			}
			if err != nil {
				fmt.Printf("[_run] pty master read ended: %v\n", err)
				close(shutdown)
				return
			}
		}
	}()

	// Reap the child; when it exits, the spike daemon has no reason to
	// keep running either (a real corral daemon would differ).
	waitDone := make(chan struct{})
	go func() {
		_ = c.Wait()
		fmt.Printf("[_run] child exited\n")
		close(waitDone)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	acceptDone := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(acceptDone)
				return
			}
			go d.handleConn(conn)
		}
	}()

	select {
	case <-shutdown:
	case <-waitDone:
	case s := <-sig:
		fmt.Printf("[_run] got signal %v, shutting down\n", s)
	}

	ln.Close()
	_ = os.Remove(*sock)
	if c.Process != nil {
		// Child was made a session/process-group leader via Setctty+Setsid;
		// kill its whole group so nothing orphans.
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
		time.Sleep(200 * time.Millisecond)
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	fmt.Printf("[_run] exiting\n")
	return nil
}

func (d *daemon) handleConn(conn net.Conn) {
	t, payload, err := readFrame(conn)
	if err != nil || t != frameAttach {
		conn.Close()
		return
	}
	rows, cols, err := decodeSize(payload)
	if err == nil && rows > 0 && cols > 0 {
		_ = pty.Setsize(d.ptyM, &pty.Winsize{Rows: rows, Cols: cols})
		fmt.Printf("[_run] client attached, size=%dx%d\n", cols, rows)
	}

	d.mu.Lock()
	old := d.curConn
	d.curConn = conn
	d.mu.Unlock()
	if old != nil {
		fmt.Printf("[_run] kicking previous client (new attach)\n")
		old.Close()
	}

	snap := d.sb.Attach(conn)
	if len(snap) > 0 {
		_, _ = conn.Write(snap)
	}

	defer func() {
		d.sb.Detach(conn)
		d.mu.Lock()
		if d.curConn == conn {
			d.curConn = nil
		}
		d.mu.Unlock()
		conn.Close()
		fmt.Printf("[_run] client detached\n")
	}()

	for {
		t, payload, err := readFrame(conn)
		if err != nil {
			return
		}
		switch t {
		case frameData:
			if _, err := d.ptyM.Write(payload); err != nil {
				return
			}
		case frameResize:
			if rows, cols, err := decodeSize(payload); err == nil {
				_ = pty.Setsize(d.ptyM, &pty.Winsize{Rows: rows, Cols: cols})
				fmt.Printf("[_run] resize -> %dx%d\n", cols, rows)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// "attach": interactive client. Raw-mode stdin -> socket, socket -> stdout.
// Detaches locally on Ctrl-\ without telling the daemon to kill anything.
// ---------------------------------------------------------------------------

func cmdAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	sock := fs.String("sock", "", "unix socket path")
	fs.Parse(args)
	if *sock == "" {
		return fmt.Errorf("need --sock")
	}
	conn, err := net.Dial("unix", *sock)
	if err != nil {
		return err
	}
	defer conn.Close()

	rows, cols := uint16(24), uint16(80)
	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	if isTTY {
		if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
			cols, rows = uint16(w), uint16(h)
		}
	}
	if err := writeFrame(conn, frameAttach, encodeSize(rows, cols)); err != nil {
		return err
	}

	var oldState *term.State
	if isTTY {
		oldState, err = term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)
	}

	if isTTY {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		go func() {
			for range winch {
				if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
					_ = writeFrame(conn, frameResize, encodeSize(uint16(h), uint16(w)))
				}
			}
		}()
	}

	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(os.Stdout, conn)
		close(copyDone)
	}()

	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if idx := indexByte(chunk, detachByte); idx >= 0 {
				if idx > 0 {
					_ = writeFrame(conn, frameData, chunk[:idx])
				}
				fmt.Fprintln(os.Stderr, "\r\n[detached]")
				return nil
			}
			if werr := writeFrame(conn, frameData, chunk); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	<-copyDone
	return nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
