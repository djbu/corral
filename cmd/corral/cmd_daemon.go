package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/djbu/corral/internal/config"
	"github.com/djbu/corral/internal/daemon"
)

// daemonReadyTimeout is how long the launcher waits for the "OK"/"ERR: "
// line on the ready pipe before giving up (design doc §3.1: "Launcher waits
// ≤10s for a single line on the ready pipe").
const daemonReadyTimeout = 10 * time.Second

// cmdDaemon implements `corral daemon [--foreground]` (design doc §3.1):
// by default, a two-hop setsid re-exec of this same binary as the hidden
// "daemon-run" subcommand, detached from the launching terminal, with a
// ready pipe on fd 3 the launcher blocks on before printing success and
// exiting. --foreground skips the re-exec entirely and runs the daemon
// body inline, for debugging; that path is not exercised by any test
// (design doc §3.1: "not a tested path").
func cmdDaemon(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	foreground := fs.Bool("foreground", false, "run the daemon body inline, without the setsid re-exec (debugging only)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *foreground {
		if err := daemon.Main(context.Background(), stdout); err != nil {
			fmt.Fprintf(stderr, "corral: daemon: %v\n", err)
			return exitError
		}
		return exitOK
	}

	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon: %v\n", err)
		return exitError
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon: resolving own executable: %v\n", err)
		return exitError
	}

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "corral: daemon: %v\n", err)
		return exitError
	}
	logPath := filepath.Join(cfg.StateDir, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon: opening %s: %v\n", logPath, err)
		return exitError
	}
	defer logFile.Close()

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon: %v\n", err)
		return exitError
	}
	defer devNull.Close()

	readyR, readyW, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon: creating ready pipe: %v\n", err)
		return exitError
	}

	cmd := exec.Command(self, "daemon-run")
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{readyW} // becomes fd 3 in the child
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		readyR.Close()
		readyW.Close()
		fmt.Fprintf(stderr, "corral: daemon: starting: %v\n", err)
		return exitError
	}
	readyW.Close() // parent's copy; the child keeps its own via ExtraFiles

	line, err := readReadyLine(readyR, daemonReadyTimeout)
	readyR.Close()
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon: %v\n", err)
		return exitError
	}
	if strings.HasPrefix(line, "ERR: ") {
		fmt.Fprintf(stderr, "corral: daemon: %s\n", strings.TrimPrefix(line, "ERR: "))
		return exitError
	}

	fmt.Fprintf(stdout, "daemon started pid=%d sock=%s\n", cmd.Process.Pid, cfg.Socket)
	_ = cmd.Process.Release()
	return exitOK
}

// readReadyLine reads a single newline-terminated line from r, or times out
// after timeout.
func readReadyLine(r io.Reader, timeout time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		if scanner.Scan() {
			ch <- result{line: scanner.Text()}
			return
		}
		if err := scanner.Err(); err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{err: io.EOF}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			return "", fmt.Errorf("reading ready pipe: %w", res.err)
		}
		return res.line, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out after %s waiting for daemon ready signal", timeout)
	}
}
