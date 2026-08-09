package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/supervisor"
)

// shutdown runs design doc §3.6's sequence. It is called exactly once,
// either from a SIGTERM/SIGINT or from POST /v1/daemon/shutdown (both
// funneled through beginShutdown in signals.go).
func (d *Daemon) shutdown(ctx context.Context, grace time.Duration) error {
	d.log.Info("shutdown: draining", "grace", grace)
	if _, err := d.store.AppendEvent(ctx, "", session.EventDaemonStopping, ""); err != nil {
		d.log.Warn("recording daemon.stopping event", "err", err)
	}

	// Close the SSE broker before anything else below: closing it unblocks
	// every GET /v1/events/stream handler's select loop (broker.Done())
	// immediately, so those long-lived handlers return well before we get
	// to srv.Shutdown/tcpSrv.Shutdown further down — otherwise Shutdown's
	// wait for in-flight handlers to finish would hang on a stream that has
	// no other reason to end (its request context only cancels once the
	// underlying connection drops, which a live client may never do on its
	// own). No delivery ordering is promised by this: a subscriber may or
	// may not see this event stopping frame before its stream ends.
	if d.broker != nil {
		d.broker.Close()
	}

	// Stop the idle reaper before checkpointing live sessions, so it can't
	// launch a reap that races the shutdown checkpoint. Close joins the
	// goroutine; after it returns no reap is in flight. It writes to the store
	// (CheckpointIdle), so like the notifier/replySub it must stop before the
	// store closes below — stopping it here (earliest) trivially satisfies that.
	if d.reaper != nil {
		d.reaper.Close()
	}

	// Same reasoning as the reaper immediately above: the orchestrator's
	// tick writes to the store (marking a task's terminal outcome,
	// scheduling a retry, ...), so it must stop before the store closes
	// below. Close joins the tick-loop goroutine, so no tick is in flight
	// once this returns.
	if d.orchestrator != nil {
		d.orchestrator.Close()
	}

	// Step 1: stop accepting new connections.
	if err := d.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		d.log.Warn("shutdown: closing listener", "err", err)
	}

	// Step 2: attached clients would be told the daemon is shutting down
	// here.
	// TODO(step10): no attach handler exists yet (step 8 has no attached
	// clients to notify), so this is a genuine no-op today.

	// Step 3: checkpoint every live session concurrently.
	//
	// TODO(step9): d.supervisor is the LiveSessionLister seam
	// (daemon.go); it always reports zero live sessions until step 9's
	// registry replaces the noLiveSessions{} stub, so this loop currently
	// never iterates, but the concurrent-checkpoint machinery itself is
	// real and exercised by internal/checkpoint's own tests.
	d.checkpointLive(ctx, d.supervisor.ListLive(), grace)

	// Step 4: wait for output-log/screen goroutines and close output
	// logs.
	// TODO(step9/step10): no such goroutines exist yet in step 8.

	// Step 4b: stop the notifier. Close cancels any in-flight backend Send and
	// drops queued-but-undelivered jobs, so it returns promptly even against a
	// black-holed host. It must run before store.Close below — the Dispatcher's
	// final notify.* event writes go to the store on context.Background().
	if d.notifier != nil {
		d.notifier.Close()
	}
	// The reply subscriber's accept path writes session.answered on
	// context.Background(), so like the Dispatcher it must close before the
	// store. Close cancels the in-flight long-poll and joins the goroutine.
	if d.replySub != nil {
		d.replySub.Close()
	}

	// Step 5: wal_checkpoint(TRUNCATE), then close the DB.
	if err := d.store.WalCheckpointTruncate(ctx); err != nil {
		d.log.Warn("shutdown: wal checkpoint", "err", err)
	}
	if err := d.store.Close(); err != nil {
		d.log.Warn("shutdown: closing store", "err", err)
	}

	// Step 6: remove socket + pid file, release the flock.
	if err := removeIfExists(d.sockPath); err != nil {
		d.log.Warn("shutdown: removing socket", "err", err)
	}
	if err := removeIfExists(d.pidPath); err != nil {
		d.log.Warn("shutdown: removing pidfile", "err", err)
	}
	if err := ReleaseLock(d.lockFile); err != nil {
		d.log.Warn("shutdown: releasing lock", "err", err)
	}

	if d.tcpSrv != nil {
		if err := d.tcpSrv.Shutdown(context.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Warn("shutdown: tcp http server shutdown", "err", err)
		}
	}
	if err := d.srv.Shutdown(context.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		d.log.Warn("shutdown: http server shutdown", "err", err)
	}

	d.log.Info("shutdown: complete")
	return nil
}

// checkpointLive calls d.checkpointer.Checkpoint concurrently for every
// entry in live, waiting for all of them before returning (design doc
// §3.6 step 3: "live session, concurrently").
func (d *Daemon) checkpointLive(ctx context.Context, live []*supervisor.LiveSession, grace time.Duration) {
	cp := d.checkpointer
	if grace != d.cfg.ShutdownGrace {
		cp = cp.WithGrace(grace)
	}
	var wg sync.WaitGroup
	for _, ls := range live {
		wg.Add(1)
		go func(ls *supervisor.LiveSession) {
			defer wg.Done()
			tok, err := cp.Checkpoint(ctx, ls, "daemon_shutdown")
			if err != nil {
				d.log.Error("checkpointing session at shutdown", "session_id", ls.SessionID, "err", err)
				return
			}
			d.log.Info("checkpointed session at shutdown", "session_id", ls.SessionID, "turn_boundary_verified", tok.TurnBoundaryVerified)
		}(ls)
	}
	wg.Wait()
}

func removeIfExists(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}
