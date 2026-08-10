package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/djbu/corral/internal/config"
	"github.com/djbu/corral/internal/daemon"
	"github.com/djbu/corral/internal/service"
)

func cmdService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printServiceUsage(stderr)
		return exitUsage
	}
	if args[0] != "install" && args[0] != "status" && args[0] != "uninstall" {
		printServiceUsage(stderr)
		return exitUsage
	}
	mgr, err := currentServiceManager()
	if err != nil {
		fmt.Fprintf(stderr, "corral: service: %v\n", err)
		return exitError
	}
	switch args[0] {
	case "install":
		return serviceInstall(mgr, args[1:], stdout, stderr)
	case "status":
		return serviceStatus(mgr, args[1:], stdout, stderr)
	case "uninstall":
		return serviceUninstall(mgr, args[1:], stdout, stderr)
	}
	return exitUsage // guarded above; keeps the switch exhaustive for Go.
}

func printServiceUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: corral service <install|status|uninstall>")
}

func currentServiceManager() (*service.Manager, error) {
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home directory: %w", err)
	}
	bin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolving executable: %w", err)
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return nil, fmt.Errorf("resolving executable path: %w", err)
	}
	digest, err := executableDigest(bin)
	if err != nil {
		return nil, err
	}
	return service.New(service.Config{
		Platform: runtime.GOOS, UID: os.Geteuid(), HomeDir: home,
		StateDir: cfg.StateDir, BinaryPath: bin, BinaryDigest: digest,
	}, service.ExecRunner{})
}

func executableDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hashing executable %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing executable %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func serviceInstall(mgr *service.Manager, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("service install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: corral service install")
		return exitUsage
	}
	if os.Getenv("CORRAL_DAEMON_STATE_DIR") != "" || os.Getenv("CORRAL_DAEMON_SOCKET") != "" {
		fmt.Fprintln(stderr, "corral: service install: daemon state/socket overrides must be persisted in ~/.corral/config.toml; the service does not copy shell environment")
		return exitError
	}
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: service install: %v\n", err)
		return exitError
	}
	// A manually detached daemon and a supervised foreground daemon cannot
	// share the singleton lock. Hand it over gracefully before asking the user
	// manager to start the unit. If the unit is already active, leave it alone
	// so a byte-identical install remains a true no-op.
	current, err := mgr.Status(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "corral: service install: %v\n", err)
		return exitError
	}
	if !current.Active {
		if _, _, stopErr := stopDaemon(cfg, 0, daemonLifecycleTimeout, stderr); stopErr != nil {
			fmt.Fprintf(stderr, "corral: service install: stopping manual daemon: %v\n", stopErr)
			return exitError
		}
	}
	status, changed, err := mgr.Install(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "corral: service install: %v\n", err)
		return exitError
	}
	if _, err := waitForDaemonRunning(cfg, daemonLifecycleTimeout, stderr); err != nil {
		fmt.Fprintf(stderr, "corral: service install: manager is active but daemon API is not ready: %v\n", err)
		return exitError
	}
	if changed {
		fmt.Fprintf(stdout, "service installed platform=%s path=%s active=%t\n", status.Platform, status.Path, status.Active)
	} else {
		fmt.Fprintf(stdout, "service already installed platform=%s path=%s active=%t\n", status.Platform, status.Path, status.Active)
	}
	return exitOK
}

func waitForDaemonRunning(cfg config.Daemon, timeout time.Duration, stderr io.Writer) (daemon.LifecycleStatus, error) {
	deadline := time.Now().Add(timeout)
	var last daemon.LifecycleStatus
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = inspectDaemonLifecycle(cfg, stderr)
		if lastErr == nil && last.State == daemon.LifecycleRunning {
			return last, nil
		}
		time.Sleep(daemonLifecyclePollInterval)
	}
	return last, fmt.Errorf("timed out after %s (state=%s, last_error=%v)", timeout, last.State, lastErr)
}

func serviceStatus(mgr *service.Manager, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("service status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: corral service status [--json]")
		return exitUsage
	}
	status, err := mgr.Status(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "corral: service status: %v\n", err)
		return exitError
	}
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(status); err != nil {
			fmt.Fprintf(stderr, "corral: service status: %v\n", err)
			return exitError
		}
	} else {
		fmt.Fprintf(stdout, "service platform=%s installed=%t enabled=%t active=%t path=%s",
			status.Platform, status.Installed, status.Enabled, status.Active, status.Path)
		if status.Detail != "" {
			fmt.Fprintf(stdout, " detail=%s", status.Detail)
		}
		fmt.Fprintln(stdout)
	}
	return exitOK
}

func serviceUninstall(mgr *service.Manager, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("service uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: corral service uninstall")
		return exitUsage
	}
	changed, err := mgr.Uninstall(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "corral: service uninstall: %v\n", err)
		return exitError
	}
	if changed {
		fmt.Fprintf(stdout, "service uninstalled path=%s (data preserved)\n", mgr.Path())
	} else {
		fmt.Fprintf(stdout, "service already uninstalled path=%s (data preserved)\n", mgr.Path())
	}
	return exitOK
}
