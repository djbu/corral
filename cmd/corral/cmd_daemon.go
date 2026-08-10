package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/djbu/corral/internal/api/client"
	"github.com/djbu/corral/internal/config"
	"github.com/djbu/corral/internal/daemon"
	"github.com/djbu/corral/internal/service"
)

// daemonReadyTimeout is how long the launcher waits for the "OK"/"ERR: "
// line on the ready pipe before giving up (design doc §3.1: "Launcher waits
// ≤10s for a single line on the ready pipe").
const daemonReadyTimeout = 10 * time.Second

const (
	daemonLifecycleProbeTimeout = 300 * time.Millisecond
	daemonLifecyclePollInterval = 50 * time.Millisecond
	daemonLifecycleTimeout      = 20 * time.Second
)

// cmdDaemon implements `corral daemon [--foreground]` (design doc §3.1):
// by default, a two-hop setsid re-exec of this same binary as the hidden
// "daemon-run" subcommand, detached from the launching terminal, with a
// ready pipe on fd 3 the launcher blocks on before printing success and
// exiting. --foreground skips the re-exec entirely and runs the daemon
// body inline, for debugging; that path is not exercised by any test
// (design doc §3.1: "not a tested path").
func cmdDaemon(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "start":
			return daemonStart(args[1:], stdout, stderr)
		case "status":
			return daemonStatus(args[1:], stdout, stderr)
		case "stop":
			return daemonStop(args[1:], stdout, stderr)
		case "restart":
			return daemonRestart(args[1:], stdout, stderr)
		}
	}
	return daemonStart(args, stdout, stderr)
}

func daemonStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	foreground := fs.Bool("foreground", false, "run the daemon body inline, without the setsid re-exec (debugging only)")
	timeout := fs.Duration("timeout", daemonLifecycleTimeout, "maximum time to wait for an existing lifecycle transition")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 || *timeout <= 0 {
		fmt.Fprintln(stderr, "usage: corral daemon [start] [--foreground] [--timeout D]")
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
	deadline := time.Now().Add(*timeout)
	status, err := waitBeforeDaemonStart(cfg, *timeout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon: %v\n", err)
		return exitError
	}
	if status.State == daemon.LifecycleRunning {
		fmt.Fprintf(stdout, "daemon already running pid=%d sock=%s\n", status.PID, cfg.Socket)
		return exitOK
	}
	if mgr, installed, err := installedServiceManager(); err != nil {
		fmt.Fprintf(stderr, "corral: daemon: inspecting installed service: %v\n", err)
		return exitError
	} else if installed {
		return launchInstalledService(mgr, cfg, time.Until(deadline), 0, false, stdout, stderr)
	}

	return launchDetachedDaemon(cfg, time.Until(deadline), stdout, stderr)
}

func launchDetachedDaemon(cfg config.Daemon, timeout time.Duration, stdout, stderr io.Writer) int {
	if timeout <= 0 {
		fmt.Fprintln(stderr, "corral: daemon: lifecycle timeout expired before launch")
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

	readyTimeout := min(timeout, daemonReadyTimeout)
	line, err := readReadyLine(readyR, readyTimeout)
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

func daemonStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: corral daemon status [--json]")
		return exitUsage
	}

	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon status: %v\n", err)
		return exitError
	}
	status, err := inspectDaemonLifecycle(cfg, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon status: %v\n", err)
		return exitError
	}
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(status); err != nil {
			fmt.Fprintf(stderr, "corral: daemon status: %v\n", err)
			return exitError
		}
		return exitOK
	}
	writeDaemonStatus(stdout, status)
	return exitOK
}

func daemonStop(args []string, stdout, stderr io.Writer) int {
	grace, timeout, ok := parseDaemonTransitionFlags("daemon stop", args, stderr)
	if !ok {
		return exitUsage
	}
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon stop: %v\n", err)
		return exitError
	}
	old, already, err := stopDaemon(cfg, grace, timeout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon stop: %v\n", err)
		return exitError
	}
	if already {
		fmt.Fprintf(stdout, "daemon already stopped state=%s\n", old.State)
	} else {
		fmt.Fprintf(stdout, "daemon stopped pid=%d\n", old.PID)
	}
	return exitOK
}

func daemonRestart(args []string, stdout, stderr io.Writer) int {
	grace, timeout, ok := parseDaemonTransitionFlags("daemon restart", args, stderr)
	if !ok {
		return exitUsage
	}
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon restart: %v\n", err)
		return exitError
	}
	deadline := time.Now().Add(timeout)
	mgr, managed, err := installedServiceManager()
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon restart: inspecting installed service: %v\n", err)
		return exitError
	}
	old, _, err := stopDaemon(cfg, grace, timeout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: daemon restart: %v\n", err)
		return exitError
	}
	if managed {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			fmt.Fprintf(stderr, "corral: daemon restart: timed out after %s\n", timeout)
			return exitError
		}
		return launchInstalledService(mgr, cfg, remaining, old.PID, true, stdout, stderr)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		fmt.Fprintf(stderr, "corral: daemon restart: timed out after %s\n", timeout)
		return exitError
	}
	if status, err := waitBeforeDaemonStart(cfg, remaining, stderr); err != nil {
		fmt.Fprintf(stderr, "corral: daemon restart: %v\n", err)
		return exitError
	} else if status.State == daemon.LifecycleRunning {
		if old.PID != 0 && status.PID == old.PID {
			fmt.Fprintf(stderr, "corral: daemon restart: daemon did not change pid (%d)\n", old.PID)
			return exitError
		}
		fmt.Fprintf(stdout, "daemon restarted old_pid=%d new_pid=%d sock=%s\n", old.PID, status.PID, cfg.Socket)
		return exitOK
	}

	var startOut strings.Builder
	remaining = time.Until(deadline)
	if code := launchDetachedDaemon(cfg, remaining, &startOut, stderr); code != exitOK {
		return code
	}
	status, err := inspectDaemonLifecycle(cfg, stderr)
	if err != nil || status.State != daemon.LifecycleRunning {
		fmt.Fprintf(stderr, "corral: daemon restart: new daemon is not running: status=%s err=%v\n", status.State, err)
		return exitError
	}
	if old.PID != 0 && status.PID == old.PID {
		fmt.Fprintf(stderr, "corral: daemon restart: daemon did not change pid (%d)\n", old.PID)
		return exitError
	}
	fmt.Fprintf(stdout, "daemon restarted old_pid=%d new_pid=%d sock=%s\n", old.PID, status.PID, cfg.Socket)
	return exitOK
}

// installedServiceManager deliberately ignores environment-overridden daemon
// locations. The generated plist/unit contains no environment by design, so it
// can only own the daemon resolved from defaults and persistent config.
func installedServiceManager() (*service.Manager, bool, error) {
	if os.Getenv("CORRAL_DAEMON_STATE_DIR") != "" || os.Getenv("CORRAL_DAEMON_SOCKET") != "" {
		return nil, false, nil
	}
	mgr, err := currentServiceManager()
	if err != nil {
		return nil, false, err
	}
	if _, err := os.Lstat(mgr.Path()); err != nil {
		if os.IsNotExist(err) {
			return mgr, false, nil
		}
		return nil, false, err
	}
	status, err := mgr.Status(context.Background())
	if err != nil {
		return nil, false, err
	}
	return mgr, status.Installed, nil
}

func launchInstalledService(mgr *service.Manager, cfg config.Daemon, timeout time.Duration, oldPID int, restarting bool, stdout, stderr io.Writer) int {
	if timeout <= 0 {
		fmt.Fprintln(stderr, "corral: daemon: lifecycle timeout expired before service launch")
		return exitError
	}
	deadline := time.Now().Add(timeout)
	// stopDaemon returns after lock release, just before the foreground service
	// process necessarily disappears from its manager. Wait for that tail so
	// Install observes inactive and explicitly starts it again.
	for restarting {
		status, err := mgr.Status(context.Background())
		if err != nil {
			fmt.Fprintf(stderr, "corral: daemon: service status: %v\n", err)
			return exitError
		}
		if !status.Active {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(stderr, "corral: daemon: timed out after %s waiting for service to stop\n", timeout)
			return exitError
		}
		time.Sleep(daemonLifecyclePollInterval)
	}
	if _, _, err := mgr.Install(context.Background()); err != nil {
		fmt.Fprintf(stderr, "corral: daemon: starting installed service: %v\n", err)
		return exitError
	}
	for time.Now().Before(deadline) {
		status, err := inspectDaemonLifecycle(cfg, stderr)
		if err == nil && status.State == daemon.LifecycleRunning {
			if oldPID != 0 && status.PID == oldPID {
				fmt.Fprintf(stderr, "corral: daemon restart: daemon did not change pid (%d)\n", oldPID)
				return exitError
			}
			if restarting {
				fmt.Fprintf(stdout, "daemon restarted old_pid=%d new_pid=%d sock=%s service=managed\n", oldPID, status.PID, cfg.Socket)
			} else {
				fmt.Fprintf(stdout, "daemon started pid=%d sock=%s service=managed\n", status.PID, cfg.Socket)
			}
			return exitOK
		}
		time.Sleep(daemonLifecyclePollInterval)
	}
	fmt.Fprintf(stderr, "corral: daemon: timed out after %s waiting for installed service\n", timeout)
	return exitError
}

func parseDaemonTransitionFlags(name string, args []string, stderr io.Writer) (time.Duration, time.Duration, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	grace := fs.Duration("grace", 0, "override graceful session checkpoint duration")
	timeout := fs.Duration("timeout", daemonLifecycleTimeout, "maximum time to wait for completion")
	if err := fs.Parse(args); err != nil {
		return 0, 0, false
	}
	if fs.NArg() != 0 || *grace < 0 || *timeout <= 0 {
		fmt.Fprintf(stderr, "usage: corral %s [--grace D] [--timeout D]\n", name)
		return 0, 0, false
	}
	return *grace, *timeout, true
}

func stopDaemon(cfg config.Daemon, grace, timeout time.Duration, stderr io.Writer) (daemon.LifecycleStatus, bool, error) {
	deadline := time.Now().Add(timeout)
	shutdownRequested := false
	var original daemon.LifecycleStatus
	for {
		if time.Now().After(deadline) {
			return original, false, fmt.Errorf("timed out after %s waiting for graceful shutdown", timeout)
		}
		status, err := inspectDaemonLifecycle(cfg, stderr)
		if err != nil {
			return original, false, err
		}
		if original.State == "" {
			original = status
		}
		switch status.State {
		case daemon.LifecycleStopped, daemon.LifecycleStalePID, daemon.LifecycleStaleFiles:
			return original, !shutdownRequested, nil
		case daemon.LifecycleRunning:
			if !shutdownRequested {
				remaining := time.Until(deadline)
				ctx, cancel := context.WithTimeout(context.Background(), remaining)
				err := client.New(cfg.Socket, stderr).Shutdown(ctx, grace)
				cancel()
				if err != nil {
					return original, false, err
				}
				shutdownRequested = true
			}
		case daemon.LifecycleStarting, daemon.LifecycleStopping:
			// Wait for startup to expose the API, or for shutdown to release the lock.
		case daemon.LifecycleLockHeld:
			// Daemons before M7C do not write daemon.state. After their API has
			// accepted shutdown there is therefore a short, legitimate interval
			// where the socket is already closed but the singleton lock has not
			// been released. It is safe to wait only because this client sent and
			// received the shutdown request itself; an initially opaque lock still
			// fails closed below.
			if shutdownRequested {
				break
			}
			return original, false, fmt.Errorf("lock is held but the local API is unavailable; refusing to signal pid %d", status.PID)
		default:
			return original, false, fmt.Errorf("unknown lifecycle state %q", status.State)
		}
		time.Sleep(daemonLifecyclePollInterval)
	}
}

func waitBeforeDaemonStart(cfg config.Daemon, timeout time.Duration, stderr io.Writer) (daemon.LifecycleStatus, error) {
	deadline := time.Now().Add(timeout)
	for {
		status, err := inspectDaemonLifecycle(cfg, stderr)
		if err != nil {
			return status, err
		}
		switch status.State {
		case daemon.LifecycleRunning, daemon.LifecycleStopped, daemon.LifecycleStalePID, daemon.LifecycleStaleFiles:
			return status, nil
		case daemon.LifecycleStarting, daemon.LifecycleStopping:
			if time.Now().After(deadline) {
				return status, fmt.Errorf("timed out after %s waiting for daemon state %s", timeout, status.State)
			}
			time.Sleep(daemonLifecyclePollInterval)
		case daemon.LifecycleLockHeld:
			return status, fmt.Errorf("lock is held but the local API is unavailable; state cannot be taken over")
		default:
			return status, fmt.Errorf("unknown lifecycle state %q", status.State)
		}
	}
}

func inspectDaemonLifecycle(cfg config.Daemon, stderr io.Writer) (daemon.LifecycleStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), daemonLifecycleProbeTimeout)
	defer cancel()
	return daemon.InspectLifecycle(ctx, cfg.StateDir, cfg.Socket, stderr)
}

func writeDaemonStatus(w io.Writer, status daemon.LifecycleStatus) {
	if status.State == daemon.LifecycleRunning {
		fmt.Fprintf(w, "daemon running pid=%d version=%s api=%d uptime=%s sock=%s\n",
			status.PID, status.Version, status.APIVersion, time.Duration(status.UptimeMs)*time.Millisecond, status.Socket)
		return
	}
	if status.PID != 0 {
		fmt.Fprintf(w, "daemon %s pid=%d sock=%s\n", status.State, status.PID, status.Socket)
		return
	}
	fmt.Fprintf(w, "daemon %s sock=%s\n", status.State, status.Socket)
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
