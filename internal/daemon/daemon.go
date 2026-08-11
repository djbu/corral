package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/djbu/corral/internal/api"
	"github.com/djbu/corral/internal/api/dashboard"
	"github.com/djbu/corral/internal/checkpoint"
	"github.com/djbu/corral/internal/claude/automode"
	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/config"
	"github.com/djbu/corral/internal/notify"
	"github.com/djbu/corral/internal/orchestrator"
	"github.com/djbu/corral/internal/quota"
	"github.com/djbu/corral/internal/reaper"
	"github.com/djbu/corral/internal/review"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/state"
	"github.com/djbu/corral/internal/store"
	"github.com/djbu/corral/internal/supervisor"
	"github.com/djbu/corral/internal/tlsbootstrap"
	"github.com/djbu/corral/internal/version"
)

// envSnapshotWhitelist mirrors supervisor's own whitelist (design doc §7.2)
// for the purposes of the frozen daemon-start snapshot captured in
// startup step 6; supervisor.BuildEnv filters against its own copy of this
// list again when actually building a child's environment, so a name added
// here that supervisor doesn't also whitelist is simply never copied
// through — this list only needs to be a superset.
var envSnapshotWhitelist = []string{
	"HOME", "USER", "LOGNAME", "SHELL", "PATH", "TMPDIR",
	"LANG", "LC_ALL", "LC_CTYPE", "TZ", "SSH_AUTH_SOCK",
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
}

// LiveSessionLister is the seam shutdown (and the second-signal force-kill
// escalation in signals.go) drain through to find sessions to act on.
type LiveSessionLister interface {
	ListLive() []*supervisor.LiveSession
}

type noLiveSessions struct{}

func (noLiveSessions) ListLive() []*supervisor.LiveSession { return nil }

// Daemon is corral's daemon body: everything startup (§3.2) wires together
// and the signal loop (§3.4) and shutdown (§3.6) act on.
// The reply subscriber talks to the supervisor through notify's narrow
// InputWriter seam (defined in notify to avoid a notify->supervisor cycle);
// assert the registry satisfies it here, where both packages are imported.
var _ notify.InputWriter = (*supervisor.Registry)(nil)

type Daemon struct {
	cfg config.Daemon
	clk clock.Clock
	log *slog.Logger
	lvl *slog.LevelVar

	store  *store.Store
	engine state.Engine
	// notifier is the step-8 blocked-notification Dispatcher, or nil when
	// [notify] is disabled or configures no backend. When nil it is NOT passed
	// to NewEngine (a nil *Dispatcher wrapped in a state.Notifier interface
	// would be non-nil to the engine's nil check and panic on the first block);
	// blocking is still recorded, just not delivered. Closed on shutdown.
	notifier *notify.Dispatcher
	// replySub is the step-11 ntfy reply subscriber (§8.6), or nil when the
	// reply subscriber is disabled. Closed on shutdown, before the store, since
	// an accepted reply writes session.answered to the store.
	replySub *notify.ReplySubscriber
	// reaper is step-13's idle reaper, or a no-op Reaper when state.
	// idle_timeout is 0. Closed on shutdown, before the store, since a reap
	// writes to the store (CheckpointIdle).
	reaper *reaper.Reaper
	// orchestrator is m4 step 21's task-DAG executor, or a never-started
	// (but always safe to Close) Orchestrator when there is nothing to
	// drive yet. Closed on shutdown, before the store, for the same
	// reason as reaper: a tick can write to the store (marking a task's
	// terminal outcome).
	orchestrator *orchestrator.Orchestrator
	// checkpointer is concretely typed (rather than the checkpoint.
	// Checkpointer interface) so shutdown.go can call WithGrace for a
	// per-request grace override; M1 has only this one implementation.
	checkpointer *checkpoint.ResumeCheckpointer
	// supervisor is step 9's registry seam; see LiveSessionLister's doc.
	supervisor LiveSessionLister

	lockFile *os.File
	listener net.Listener
	srv      *http.Server
	// tcpListener and tcpSrv are M5 step 30's opt-in TCP+TLS listener, or
	// nil when daemon.listen is unset (the default) — the daemon stays
	// unix-socket-only in that case. See startup's bind/serve blocks.
	tcpListener net.Listener
	tcpSrv      *http.Server

	// broker is step 32's SSE fan-out: GET /v1/events/stream subscribes to
	// it, and store.Store publishes every AppendEvent to it (wired via
	// SetEventPublisher right after d.store is opened, below). Closed in
	// shutdown.go immediately after the daemon.stopping event is appended,
	// so every live stream ends promptly instead of hanging on
	// srv.Shutdown's wait for in-flight handlers to return.
	broker *api.Broker

	sockPath string
	pidPath  string

	startedAt time.Time

	mu           sync.Mutex
	shuttingDown bool
	shutdownReq  chan time.Duration
	done         chan struct{}
}

// Main loads daemon config, runs the complete startup sequence (§3.2), and
// then blocks in the signal loop (§3.4) until a graceful shutdown (§3.6)
// completes. ready receives exactly one line — "OK\n" once the daemon is
// ready to accept connections, or "ERR: <message>\n" if startup failed —
// and is closed (if it implements io.Closer) before Main returns. This is
// the function cmd_daemon_run.go's hidden subcommand, and `corral daemon
// --foreground`, both call.
func Main(ctx context.Context, ready io.Writer) error {
	cfg, _, err := config.LoadDaemon()
	if err != nil {
		writeReady(ready, err)
		return err
	}
	return run(ctx, cfg, ready)
}

// run is Main's body, factored out so tests can pass an already-resolved
// config.Daemon (e.g. pointed at t.TempDir) without going through
// environment variables. It is unexported: config.LoadDaemon() honoring
// env is the only sanctioned way to reach it from outside the package.
func run(ctx context.Context, cfg config.Daemon, ready io.Writer) error {
	d, err := newDaemon(cfg)
	if err != nil {
		writeReady(ready, err)
		return err
	}
	if err := d.startup(ctx); err != nil {
		writeReady(ready, err)
		return err
	}
	writeReady(ready, nil)

	return d.runSignalLoop(ctx)
}

func writeReady(w io.Writer, err error) {
	if err != nil {
		fmt.Fprintf(w, "ERR: %v\n", err)
	} else {
		fmt.Fprint(w, "OK\n")
	}
	if c, ok := w.(io.Closer); ok {
		c.Close()
	}
}

func newDaemon(cfg config.Daemon) (*Daemon, error) {
	lvl := new(slog.LevelVar)
	lvl.Set(parseLevel(cfg.LogLevel))

	return &Daemon{
		cfg:         cfg,
		clk:         clock.Real(),
		lvl:         lvl,
		supervisor:  noLiveSessions{},
		sockPath:    cfg.Socket,
		pidPath:     filepath.Join(cfg.StateDir, "daemon.pid"),
		shutdownReq: make(chan time.Duration, 1),
		done:        make(chan struct{}),
	}, nil
}

// startup runs design doc §3.2's sequence, steps 1-10 (step 11, blocking on
// signals, is runSignalLoop).
func (d *Daemon) startup(ctx context.Context) error {
	// Step 1 (partial) / step 2: slog to daemon.log, then the setsid
	// self-check.
	if err := d.initLogging(); err != nil {
		return err
	}
	d.setsidSelfCheck()

	// Step 3: MkdirAll 0700, then unconditional Chmod 0700 (fixes a
	// pre-existing loose directory).
	if err := os.MkdirAll(d.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("daemon: creating state_dir %s: %w", d.cfg.StateDir, err)
	}
	if err := os.Chmod(d.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("daemon: chmod state_dir %s: %w", d.cfg.StateDir, err)
	}

	// Step 4: acquire the singleton flock. This must happen before the
	// store or the socket are touched.
	lockFile, err := AcquireLock(d.cfg.StateDir)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			return fmt.Errorf("another corral daemon holds the lock (%s)", filepath.Join(d.cfg.StateDir, "daemon.lock"))
		}
		return err
	}
	d.lockFile = lockFile
	if err := writeLifecycleState(d.cfg.StateDir, LifecycleStarting); err != nil {
		return err
	}

	// Step 5: open the store, migrating inside the flock.
	st, err := store.Open(filepath.Join(d.cfg.StateDir, "corral.db"), d.clk)
	if err != nil {
		return err
	}
	d.store = st
	d.broker = api.NewBroker()
	d.store.SetEventPublisher(d.broker)
	schemaVersion, err := d.store.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	d.log.Info("store opened", "schema_version", schemaVersion)

	// The daemon-default session config must load before the env snapshot is
	// frozen: session.env_passthrough names the extra env vars to forward to
	// sessions (design doc §7.2 / m1.md — "added to the whitelist"), and
	// captureEnvSnapshot has to capture those names into the frozen snapshot,
	// or supervisor.BuildEnv's passthrough filter finds nothing to copy and
	// the option silently forwards nothing. attachCfg is consumed later, at
	// registry construction.
	grace := d.cfg.ShutdownGrace
	claudeHome := filepath.Join(mustHomeDir(), ".claude")
	sessionCfg, attachCfg, _, _, err := config.LoadSession(daemonCwd(), nil)
	if err != nil {
		return fmt.Errorf("daemon: loading default session config: %w", err)
	}
	templateRegistry, err := config.LoadTemplateRegistry()
	if err != nil {
		return fmt.Errorf("daemon: loading session templates: %w", err)
	}
	templates := templateRegistry.Templates
	for _, template := range templates {
		sessionCfg.EnvPassthrough = appendUniqueEnvNames(sessionCfg.EnvPassthrough, template.EnvPassthrough)
	}

	// Step 6: freeze the child-env snapshot. Captured now (daemon
	// startup), never at spawn time — design doc §7.2's "spawn behavior
	// is a pure function of config + snapshot, testable" invariant.
	envSnapshot := captureEnvSnapshot(sessionCfg.EnvPassthrough)
	d.log.Debug("captured env snapshot", "keys", snapshotKeyNames(envSnapshot))

	// Step 7: resolve claude_bin and best-effort probe its version.
	// Session-scope config (session.claude_bin) isn't loaded here — that
	// happens per-session (design doc §8.1); the probe uses the same default
	// LoadSession would, resolved for the daemon's own cwd, purely to log
	// something useful; it is never fatal.
	d.probeClaudeBin(ctx)
	d.probeAutoMode(ctx)

	// The checkpointer, engine, and live-session registry all have to
	// exist before recovery runs (recovery resumes sessions through them),
	// so step 9 moves their construction ahead of step 8's recovery call
	// (design doc §3.2 numbers these 7 and 8 in the other order, but
	// recovery cannot do anything real without them).
	d.checkpointer = checkpoint.NewResumeCheckpointer(d.store, d.clk, grace, claudeHome, envSnapshot, sessionCfg.EnvPassthrough, sessionCfg.Term, d.sockPath)
	// [state] config governs the engine's permission timers and staleness
	// grace (user/env only — no repo-file stage; LoadState has its own
	// pipeline). Best-effort: a bad [state] value falls back to NewEngine's
	// built-in defaults rather than failing daemon startup, same discipline
	// as the other non-fatal probes here.
	stateCfg, _, stateErr := config.LoadState()
	if stateErr != nil {
		d.log.Warn("using default [state] config; load failed", "err", stateErr)
	}
	learnCfg, _, err := config.LoadLearn()
	if err != nil {
		return fmt.Errorf("daemon: loading [learn] config: %w", err)
	}

	// [notify] (step 8): build the blocked-notification Dispatcher from the
	// resolved config and hand it to the engine as its Notifier seam. Same
	// best-effort discipline — a load failure falls back to notify-disabled,
	// never fails startup. notifier is the state.Notifier interface value
	// passed to NewEngine; it MUST stay a true nil interface when no dispatcher
	// is built (see the d.notifier field doc for the nil-interface trap).
	var notifier state.Notifier
	notifyCfg, _, notifyErr := config.LoadNotify()
	if notifyErr != nil {
		d.log.Warn("using default [notify] config; load failed", "err", notifyErr)
	}
	// The ntfy reply subscriber is a remote-input capability (§8.6). Its
	// startup gate (reply topic must differ from the notify topic; a token is
	// required) is FATAL, not best-effort: a misconfigured reply subscriber
	// must stop the daemon, never fall back to a weaker posture. Checked here,
	// before anything is built, so the failure is the first thing the operator
	// sees. (notifyErr leaves notifyCfg zero-valued → reply disabled → nil.)
	if err := config.ValidateReply(notifyCfg); err != nil {
		return err
	}
	if backends := buildNotifyBackends(notifyCfg); notifyCfg.Enabled && len(backends) > 0 {
		templateBackends := make(map[string][]string)
		for name, template := range templates {
			if template.NotifyProfile != "" {
				templateBackends[name] = templateRegistry.NotifyProfiles[template.NotifyProfile].Backends
			}
		}
		d.notifier = notify.New(backends, notify.Options{
			On:               notifyCfg.On,
			Debounce:         notifyCfg.Debounce,
			Timeout:          notifyCfg.Timeout,
			Retries:          notifyCfg.Retries,
			TemplateBackends: templateBackends,
		}, d.clk, d.store, d.log)
		d.notifier.Start()
		notifier = d.notifier
		d.log.Info("notifier started", "backends", backendNames(backends), "on", notifyCfg.On)
	} else {
		d.log.Debug("notifier disabled; blocks recorded but not delivered",
			"enabled", notifyCfg.Enabled, "backends", len(backends))
	}

	d.engine = state.NewEngine(d.store, d.clk, state.EngineConfig{
		PermissionSettle:     stateCfg.PermissionSettle,
		PermissionTTL:        stateCfg.PermissionTTL,
		StaleAfter:           stateCfg.StaleAfter,
		FirstHookGrace:       stateCfg.FirstHookGrace,
		MaxEventPayloadBytes: stateCfg.MaxEventPayloadBytes,
	}, notifier, d.log)

	relayCmd, err := resolveRelayCommand()
	if err != nil {
		return fmt.Errorf("daemon: resolving relay command: %w", err)
	}

	registry := supervisor.New(d.store, d.engine, checkpointerAdapter{d.checkpointer}, d.clk, supervisor.Config{
		StateDir:               d.cfg.StateDir,
		SockPath:               d.sockPath,
		SettingSources:         sessionCfg.SettingSources,
		EnvPassthrough:         sessionCfg.EnvPassthrough,
		Term:                   sessionCfg.Term,
		OutputLogMaxBytes:      sessionCfg.OutputLogMaxBytes,
		EnvSnapshot:            envSnapshot,
		CorralVersion:          version.Version,
		APIVersion:             version.APIVersion,
		RecoveryGrace:          grace,
		PingInterval:           attachCfg.PingInterval,
		PingTimeout:            attachCfg.PingTimeout,
		RelayCommand:           relayCmd,
		ClaudeHome:             claudeHome,
		MinFreeBytes:           d.cfg.MinFreeBytes,
		MaxInteractiveSessions: d.cfg.MaxInteractiveSessions,
		MaxHeadlessTasks:       d.cfg.MaxHeadlessTasks,
	}, d.log)
	d.supervisor = registry

	// Reply subscriber (§8.6): the outbound long-poll that turns ntfy replies
	// into answer() calls, closing M2's phone-reply exit criterion with zero
	// inbound ports. Constructed here, after the registry (its InputWriter)
	// exists; its three-way startup gate was already validated (fatally) above.
	if notifyCfg.Ntfy.Reply.Enabled {
		d.replySub = notify.NewReplySubscriber(notify.ReplyConfig{
			Server: notifyCfg.Ntfy.Server,
			Topic:  notifyCfg.Ntfy.Reply.Topic,
			Token:  notifyCfg.Ntfy.Reply.Token,
		}, d.store, registry, d.clk, d.log)
		d.replySub.Start()
		d.log.Info("ntfy reply subscriber started", "topic", notifyCfg.Ntfy.Reply.Topic)
	}

	// Step 8: recovery, before the socket is up so no client observes a
	// half-recovered world (design doc §3.5).
	if err := d.runRecovery(ctx, registry); err != nil {
		return err
	}

	// Step 13: idle reaper, started only after recovery has finished so a
	// resumed session's freshly-set last_activity_ms is never observed as
	// stale by a reap running concurrently with recovery itself.
	templateIdleTimeouts := make(map[string]time.Duration, len(templates))
	for name, template := range templates {
		templateIdleTimeouts[name] = template.IdleTimeout
	}
	d.reaper = reaper.New(registry, d.store, d.clk, d.log, stateCfg.IdleTimeout, templateIdleTimeouts)
	d.reaper.Start()

	// Step 21: the task-DAG orchestrator (m4.md §8). Wired unconditionally,
	// same posture as the idle reaper above — an idle daemon with no active
	// dag costs nothing but a periodic no-op tick. claudeBin is resolved
	// once here, the same way api/handlers_sessions.go resolves it per
	// request (session.claude_bin + LookPath if not absolute); a
	// resolution failure is logged, not fatal, matching probeClaudeBin's
	// discipline above — it only prevents headless spawns from working,
	// which will surface per-task as an ordinary spawn error and retry,
	// not as a reason to refuse to start the daemon.
	orchClaudeBin := sessionCfg.ClaudeBin
	if !filepath.IsAbs(orchClaudeBin) {
		if resolved, err := exec.LookPath(orchClaudeBin); err != nil {
			d.log.Warn("orchestrator: claude_bin not resolved at startup; headless spawns will fail until this is fixed", "claude_bin", orchClaudeBin, "err", err)
		} else {
			orchClaudeBin = resolved
		}
	}
	var quotaController *quota.Controller
	if d.cfg.QuotaLimit > 0 {
		quotaController, err = quota.New(d.clk, quota.Config{Window: d.cfg.QuotaWindow, Limit: d.cfg.QuotaLimit, InteractiveReserve: d.cfg.QuotaInteractiveReserve})
		if err != nil {
			return fmt.Errorf("daemon: configuring quota controller: %w", err)
		}
		registry.SetRateLimitObserver(func(retryAfter time.Duration) {
			quotaController.PauseUntil(d.clk.Now().Add(retryAfter))
		})
	}
	d.orchestrator = orchestrator.New(registry, d.store, d.clk, d.log, orchestrator.Config{
		StateDir:      d.cfg.StateDir,
		ClaudeHome:    claudeHome,
		MaxConcurrent: d.cfg.MaxHeadlessTasks,
		Templates:     templates,
		Quota:         quotaController,
	}, orchClaudeBin)
	d.orchestrator.Start()

	// Step 9: acquire the socket, chmod it, write the pidfile.
	ln, err := Listen(d.sockPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(d.sockPath, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("daemon: chmod socket %s: %w", d.sockPath, err)
	}
	d.listener = ln

	// M5 step 30: opt-in TCP+TLS listener. Bound here — before the pidfile is
	// written — so a bind failure (port already in use) aborts startup cleanly
	// with no stale pidfile. TLS is mandatory: there is no plaintext-TCP path.
	// This is the only place corral becomes network-reachable, and it stays
	// closed unless the operator explicitly set daemon.listen.
	if d.cfg.Listen != "" {
		// Both-or-neither: a half-set override (tls_cert without tls_key, or the
		// reverse) would silently fall through to the self-signed bootstrap while
		// the operator believes their own cert is live. Fail closed instead — the
		// same both-or-neither reasoning step 27 applied to scope+session_id at
		// token mint time.
		certSet, keySet := d.cfg.TLSCert != "", d.cfg.TLSKey != ""
		if certSet != keySet {
			ln.Close()
			return fmt.Errorf("daemon: daemon.tls_cert and daemon.tls_key must both be set or both empty; got tls_cert=%q tls_key=%q", d.cfg.TLSCert, d.cfg.TLSKey)
		}

		var cert *tls.Certificate
		if certSet {
			cert, err = tlsbootstrap.LoadPair(d.cfg.TLSCert, d.cfg.TLSKey)
		} else {
			cert, err = tlsbootstrap.LoadOrGenerate(d.clk, filepath.Join(d.cfg.StateDir, "tls"), hostsFor(d.cfg.Listen))
		}
		if err != nil {
			ln.Close()
			return fmt.Errorf("daemon: tls bootstrap: %w", err)
		}

		// Best-effort SAN check: if the cert can't cover the host the operator is
		// listening on, a remote pinned-CA client (step 31) will fail hostname
		// verification with nothing to click through. Warn, never fail — the
		// operator may be reaching it by a name/IP we can't see here.
		warnIfHostUncovered(cert, d.cfg.Listen, d.log)

		tcpLn, err := tls.Listen("tcp", d.cfg.Listen, &tls.Config{
			Certificates: []tls.Certificate{*cert},
			MinVersion:   tls.VersionTLS12,
		})
		if err != nil {
			ln.Close()
			return fmt.Errorf("daemon: listen %s: %w", d.cfg.Listen, err)
		}
		d.tcpListener = tcpLn
	}

	if err := d.writePIDFile(); err != nil {
		return err
	}

	// Step 10: start the HTTP server, emit daemon.started, then the
	// caller writes "OK" to the ready pipe.
	d.startedAt = d.clk.Now()
	srv := api.New()
	srv.RegisterMeta(api.MetaDeps{
		Store:        d.store,
		Clock:        d.clk,
		StartedAt:    d.startedAt,
		DefaultGrace: grace,
		RequestShutdown: func(g time.Duration) {
			d.requestShutdown(g)
		},
	})
	srv.RegisterSessions(api.SessionsDeps{
		Store:    d.store,
		Engine:   d.engine,
		Registry: registry,
	})
	srv.RegisterDags(api.DagsDeps{
		Store:              d.store,
		Review:             review.New(d.store, d.clk),
		MaxPendingDAGTasks: d.cfg.MaxPendingDAGTasks,
		Templates:          templates,
	})
	srv.RegisterDashboard(api.DashboardDeps{
		Store:    d.store,
		Engine:   d.engine,
		Registry: registry,
		Learn:    learnCfg,
	})
	srv.RegisterTokens(api.TokensDeps{Store: d.store})
	srv.RegisterLearnings(api.LearningsDeps{Store: d.store, Clock: d.clk, Config: learnCfg})
	srv.RegisterHooks(api.HooksDeps{
		Store:   d.store,
		Engine:  d.engine,
		Secrets: registry,
	})
	srv.RegisterAttach(registry, d.store, d.log)
	srv.RegisterEvents(api.EventsDeps{Broker: d.broker, Store: d.store, Clock: d.clk, Log: d.log})
	d.srv = &http.Server{Handler: srv.Handler()}
	go func() {
		if err := d.srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("http server exited", "err", err)
		}
	}()

	if d.tcpListener != nil {
		// WriteTimeout is deliberately NOT set: step 32 mounts SSE on this same
		// server and a write deadline would kill long-lived streams.
		// ReadHeaderTimeout guards against a slow-header DoS from an
		// unauthenticated caller (bearerAuth runs only after headers are read).
		d.tcpSrv = &http.Server{
			Handler:           srv.AuthenticatedHandler(d.store, d.log, dashboard.Handler()),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := d.tcpSrv.Serve(d.tcpListener); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
				d.log.Error("tcp http server exited", "err", err)
			}
		}()
		d.log.Warn("TCP listener enabled — daemon is network-reachable", "addr", d.tcpListener.Addr().String(), "tls", map[bool]string{true: "operator-supplied", false: "self-signed"}[d.cfg.TLSCert != ""])
	}

	if _, err := d.store.AppendEvent(ctx, "", session.EventDaemonStarted, ""); err != nil {
		d.log.Warn("recording daemon.started event", "err", err)
	}
	d.log.Info("daemon started", "pid", os.Getpid(), "socket", d.sockPath)
	if err := writeLifecycleState(d.cfg.StateDir, LifecycleRunning); err != nil {
		return err
	}
	// The soak recorder is deliberately opt-in and writes only beneath the
	// daemon state directory. It gives the nightly fleet exercise a direct
	// goroutine/WAL signal without exposing a diagnostic endpoint to clients.
	d.startSoakMetrics()

	return nil
}

// runRecovery runs design doc §3.5's startup recovery against reg.
func (d *Daemon) runRecovery(ctx context.Context, reg *supervisor.Registry) error {
	return reg.Recover(ctx)
}

// daemonCwd returns the daemon process's own working directory, used only
// to resolve config.LoadSession's repo-scoped layer for the daemon-wide
// session defaults (env_passthrough, term, etc.) that the checkpointer and
// the live-session registry are frozen with at startup. "" (LoadSession
// then skips the repo-file lookup) if Getwd fails for any reason.
func daemonCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// checkpointerAdapter adapts *checkpoint.ResumeCheckpointer to
// supervisor.Checkpointer, forwarding the Token's TurnBoundaryVerified bool
// (the rest of the Token, supervisor doesn't need). It exists only here (not
// in internal/supervisor) because internal/checkpoint already imports
// internal/supervisor for *supervisor.LiveSession — supervisor importing
// checkpoint back would be a cycle; daemon imports both, so the adapter
// lives here.
type checkpointerAdapter struct {
	c *checkpoint.ResumeCheckpointer
}

func (a checkpointerAdapter) Checkpoint(ctx context.Context, s *supervisor.LiveSession, reason string) (bool, error) {
	tok, err := a.c.Checkpoint(ctx, s, reason)
	return tok.TurnBoundaryVerified, err
}

func (a checkpointerAdapter) Restore(ctx context.Context, rec session.Session) (session.Spec, error) {
	return a.c.Restore(ctx, rec)
}

func (a checkpointerAdapter) Resumable(rec session.Session) (bool, string) {
	return a.c.Resumable(rec)
}

// WithGrace lets supervisor.Registry.Kill honor a per-call grace override
// (design doc §9.2's DELETE /v1/sessions/{idOrName}?grace=...) without
// supervisor needing to know checkpointerAdapter's concrete type — Kill
// detects this method via an interface type-assertion.
func (a checkpointerAdapter) WithGrace(grace time.Duration) supervisor.Checkpointer {
	return checkpointerAdapter{c: a.c.WithGrace(grace)}
}

func (d *Daemon) initLogging() error {
	if err := os.MkdirAll(d.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("daemon: creating state_dir %s for log: %w", d.cfg.StateDir, err)
	}
	logPath := filepath.Join(d.cfg.StateDir, "daemon.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("daemon: opening %s: %w", logPath, err)
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: d.lvl}
	if d.cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(f, opts)
	} else {
		handler = slog.NewTextHandler(f, opts)
	}
	d.log = slog.New(handler)
	return nil
}

// setsidSelfCheck implements design doc §3.1's "daemon-run self-checks
// unix.Getsid(0) == os.Getpid(), logs WARN if not — silently-failed
// setsid degrades to 'works until terminal closes', exactly the bug class
// worth one-line assertion." It never fails startup — only logs.
func (d *Daemon) setsidSelfCheck() {
	sid, err := unix.Getsid(0)
	if err != nil {
		d.log.Warn("setsid self-check: Getsid failed", "err", err)
		return
	}
	if sid != os.Getpid() {
		d.log.Warn("setsid self-check failed: process is not its own session leader; detachment from the launching terminal may not have taken effect", "sid", sid, "pid", os.Getpid())
	}
}

// captureEnvSnapshot freezes the subset of the daemon's own environment that
// child sessions may inherit: the fixed envSnapshotWhitelist plus every name
// in passthrough (session.env_passthrough). The passthrough names MUST be
// captured here — supervisor.BuildEnv only copies a passthrough name that is
// already present in this snapshot, so omitting them makes env_passthrough a
// silent no-op despite being documented (design doc §7.2 / m1.md) as "extra
// env var NAMES added to the whitelist". Widening the snapshot this way is not
// a trust-boundary regression: session.env_passthrough is user-config/env only
// and rejected from repo files (config/repo_allowlist.go), so a cloned repo
// cannot name env vars for a session to exfiltrate.
func captureEnvSnapshot(passthrough []string) map[string]string {
	snapshot := make(map[string]string, len(envSnapshotWhitelist)+len(passthrough))
	capture := func(name string) {
		if v, ok := os.LookupEnv(name); ok {
			snapshot[name] = v
		}
	}
	for _, name := range envSnapshotWhitelist {
		capture(name)
	}
	for _, name := range passthrough {
		capture(name)
	}
	return snapshot
}

func snapshotKeyNames(snapshot map[string]string) []string {
	names := make([]string, 0, len(snapshot))
	for k := range snapshot {
		names = append(names, k)
	}
	return names
}

func appendUniqueEnvNames(dst, extra []string) []string {
	seen := make(map[string]bool, len(dst)+len(extra))
	for _, name := range dst {
		seen[name] = true
	}
	for _, name := range extra {
		if !seen[name] {
			dst = append(dst, name)
			seen[name] = true
		}
	}
	return dst
}

// probeClaudeBin resolves the default claude_bin and runs `claude
// --version` with a 3s timeout, purely for a startup log line (design doc
// §3.2 step 7: "best-effort ... 3s timeout, logged, non-fatal").
func (d *Daemon) probeClaudeBin(ctx context.Context) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		d.log.Debug("claude_bin not resolved at startup", "err", err)
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, bin, "--version").Output()
	if err != nil {
		d.log.Debug("claude --version probe failed", "claude_bin", bin, "err", err)
		return
	}
	d.log.Info("resolved claude_bin", "claude_bin", bin, "version", string(out))
}

// probeAutoMode best-effort detects Claude Code's Auto Mode at startup and
// logs it (design doc Amendment A.6). Same discipline as probeClaudeBin:
// non-fatal, 3s budget inside automode.Detect, never blocks startup. When
// active it logs at INFO so an operator scanning startup logs sees that
// `blocked` will be rare on this account; otherwise it is silent at Debug
// (Auto Mode's off-state shape is unverified, so "not detected" is not a
// claim that it is off — see automode.StatusUnknown).
func (d *Daemon) probeAutoMode(ctx context.Context) {
	res := automode.Detect(ctx, "")
	if res.Status == automode.StatusActive {
		d.log.Info(automode.ActiveMessage,
			"allow", res.AllowCount, "soft_deny", res.SoftDenyCount, "hard_deny", res.HardDenyCount)
		return
	}
	d.log.Debug("Auto Mode not detected at startup", "raw", res.Raw)
}

func (d *Daemon) writePIDFile() error {
	return os.WriteFile(d.pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
}

// warnIfHostUncovered logs a Warn if cert's leaf certificate does not cover
// every non-loopback host implied by listen (via hostsFor). Best-effort: any
// parse failure or empty host set is silently skipped — this only ever warns,
// never blocks startup.
func warnIfHostUncovered(cert *tls.Certificate, listen string, log *slog.Logger) {
	if cert == nil || len(cert.Certificate) == 0 {
		return
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return
	}
	var uncovered []string
	for _, h := range hostsFor(listen) {
		if leaf.VerifyHostname(h) != nil {
			uncovered = append(uncovered, h)
		}
	}
	if len(uncovered) > 0 {
		log.Warn("TLS cert does not cover configured listen host(s); remote clients pinning this cert will fail hostname verification", "uncovered", uncovered, "listen", listen)
	}
}

func mustHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// resolveRelayCommand returns the shell command prefix the pinned hooks{}
// block invokes: the absolute path to the running corral binary followed by
// "hook-relay". Resolved once at startup via os.Executable + EvalSymlinks so
// the child never PATH-resolves a security-relevant helper (Amendment A.2).
func resolveRelayCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return shellQuote(exe) + " hook-relay", nil
}

// shellQuote single-quotes s for safe embedding in the shell command string
// settings.json's hooks{} block runs, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// requestShutdown is called by the /v1/daemon/shutdown handler; it never
// blocks, and is safe to call more than once (only the first call has any
// effect).
func (d *Daemon) requestShutdown(grace time.Duration) {
	select {
	case d.shutdownReq <- grace:
	default:
		// Already a shutdown pending; drop, the loop is already draining.
	}
}

// buildNotifyBackends turns the resolved [notify] config into the concrete
// backends the Dispatcher fans out to. All backends share one http.Client
// whose Timeout is notify.timeout and whose Transport pins Proxy: nil (see
// notify.NewHTTPClient — no ambient HTTP_PROXY egress). A backend is included
// only when its own Enabled flag is set; the reply subscriber (§8.6) is a
// separate step-11 concern and is not a delivery backend.
func buildNotifyBackends(cfg config.Notify) []notify.Backend {
	if !cfg.Enabled {
		return nil
	}
	client := notify.NewHTTPClient(cfg.Timeout)
	var backends []notify.Backend
	if cfg.Ntfy.Enabled {
		backends = append(backends, notify.NewNtfyBackend(
			cfg.Ntfy.Server, cfg.Ntfy.Topic, cfg.Ntfy.Token, cfg.Ntfy.Priority, client))
	}
	if cfg.Webhook.Enabled {
		backends = append(backends, notify.NewWebhookBackend(
			cfg.Webhook.URL, cfg.Webhook.Headers, client))
	}
	return backends
}

// backendNames lists backend names for a startup log line. It never logs a
// URL, topic, token, or header — only the backend kind ("ntfy"/"webhook").
func backendNames(backends []notify.Backend) []string {
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		names = append(names, b.Name())
	}
	return names
}
