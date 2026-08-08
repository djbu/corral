package daemon

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danielbecerra/corral/internal/config"
	"github.com/danielbecerra/corral/internal/procinfo"
)

// runSignalLoop is design doc §3.2 step 11 and the whole of §3.4: it blocks
// until a graceful shutdown (triggered by SIGTERM/SIGINT or by POST
// /v1/daemon/shutdown, all funneled into d.shutdownReq) completes, with a
// second SIGTERM/SIGINT while draining escalating to an immediate,
// unconditional force-kill of every tracked session and os.Exit(1).
func (d *Daemon) runSignalLoop(ctx context.Context) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				d.reloadConfig()
			case syscall.SIGTERM, syscall.SIGINT:
				if d.beginShutdown(ctx, d.cfg.ShutdownGrace) {
					continue
				}
				d.log.Warn("second SIGTERM/SIGINT received while draining; force-killing tracked sessions")
				d.forceKillAll()
				os.Exit(1)
			}
			// SIGPIPE: left to Go's default (ignored on non-stdio fds,
			// design doc §3.4) — never registered above.
			// SIGCHLD: unused — each session gets its own cmd.Wait()
			// goroutine (design doc §3.4); not registered above.
		case grace := <-d.shutdownReq:
			d.beginShutdown(ctx, grace)
		case <-d.done:
			return nil
		}
	}
}

// beginShutdown starts a graceful shutdown in a background goroutine the
// first time it's called (returning true); a later call while already
// draining is a no-op (returning false), which the signal loop uses to
// detect "this is the second SIGTERM/SIGINT".
func (d *Daemon) beginShutdown(ctx context.Context, grace time.Duration) bool {
	d.mu.Lock()
	if d.shuttingDown {
		d.mu.Unlock()
		return false
	}
	d.shuttingDown = true
	d.mu.Unlock()

	go func() {
		if err := d.shutdown(ctx, grace); err != nil {
			d.log.Error("shutdown failed", "err", err)
		}
		close(d.done)
	}()
	return true
}

// reloadConfig implements design doc §3.4's SIGHUP row: re-read the user
// config and apply log_level only; everything else logged "ignored on
// reload".
func (d *Daemon) reloadConfig() {
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		d.log.Warn("SIGHUP: reloading config failed", "err", err)
		return
	}
	if cfg.LogLevel != d.cfg.LogLevel {
		d.lvl.Set(parseLevel(cfg.LogLevel))
		d.log.Info("SIGHUP: applied log_level", "log_level", cfg.LogLevel)
	} else {
		d.log.Info("SIGHUP: log_level unchanged", "log_level", cfg.LogLevel)
	}
	d.log.Info("SIGHUP: all other daemon config keys ignored on reload (socket, state_dir, log_format, shutdown_grace)")
}

// forceKillAll unconditionally SIGKILLs every tracked session's process
// group (design doc §3.4's second-signal escalation). It is pid-scoped via
// procinfo.KillGroup, never name-matched.
func (d *Daemon) forceKillAll() {
	for _, live := range d.supervisor.ListLive() {
		if err := procinfo.KillGroup(live.PGID, syscall.SIGKILL); err != nil {
			d.log.Error("force-kill failed", "session_id", live.SessionID, "pgid", live.PGID, "err", err)
		}
	}
}
