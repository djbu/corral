package daemon

import (
	"context"
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
	"syscall"
	"time"

	"github.com/danielbecerra/corral/internal/api"
	"github.com/danielbecerra/corral/internal/checkpoint"
	"github.com/danielbecerra/corral/internal/claude/automode"
	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/config"
	"github.com/danielbecerra/corral/internal/notify"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/state"
	"github.com/danielbecerra/corral/internal/store"
	"github.com/danielbecerra/corral/internal/supervisor"
	"github.com/danielbecerra/corral/internal/version"
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
//
// TODO(step9): internal/supervisor's registry (supervisor.go, not yet
// written) implements this by listing its actually-tracked LiveSessions.
// Step 8 wires noLiveSessions{}, which always reports none, since nothing
// spawns a session yet — daemon.go's `supervisor` field below is exactly
// where step 9 plugs the real registry in.
type LiveSessionLister interface {
	ListLive() []*supervisor.LiveSession
}

type noLiveSessions struct{}

func (noLiveSessions) ListLive() []*supervisor.LiveSession { return nil }

// Daemon is corral's daemon body: everything startup (§3.2) wires together
// and the signal loop (§3.4) and shutdown (§3.6) act on.
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
	// checkpointer is concretely typed (rather than the checkpoint.
	// Checkpointer interface) so shutdown.go can call WithGrace for a
	// per-request grace override; M1 has only this one implementation.
	checkpointer *checkpoint.ResumeCheckpointer
	// supervisor is step 9's registry seam; see LiveSessionLister's doc.
	supervisor LiveSessionLister

	lockFile *os.File
	listener net.Listener
	srv      *http.Server

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

	// Step 5: open the store, migrating inside the flock.
	st, err := store.Open(filepath.Join(d.cfg.StateDir, "corral.db"), d.clk)
	if err != nil {
		return err
	}
	d.store = st
	schemaVersion, err := d.store.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	d.log.Info("store opened", "schema_version", schemaVersion)

	// Step 6: freeze the child-env snapshot. Captured now (daemon
	// startup), never at spawn time — design doc §7.2's "spawn behavior
	// is a pure function of config + snapshot, testable" invariant.
	envSnapshot := captureEnvSnapshot()
	d.log.Debug("captured env snapshot", "keys", snapshotKeyNames(envSnapshot))

	// Step 7: resolve claude_bin and best-effort probe its version.
	// Session-scope config (session.claude_bin) isn't loaded here — that
	// happens per-session (design doc §8.1) — so this uses the same
	// default LoadSession would, resolved for the daemon's own cwd, purely
	// to log something useful; it is never fatal.
	d.probeClaudeBin(ctx)
	d.probeAutoMode(ctx)

	// The checkpointer, engine, and live-session registry all have to
	// exist before recovery runs (recovery resumes sessions through them),
	// so step 9 moves their construction ahead of step 8's recovery call
	// (design doc §3.2 numbers these 7 and 8 in the other order, but
	// recovery cannot do anything real without them).
	grace := d.cfg.ShutdownGrace
	claudeHome := filepath.Join(mustHomeDir(), ".claude")
	sessionCfg, attachCfg, _, _, err := config.LoadSession(daemonCwd(), nil)
	if err != nil {
		return fmt.Errorf("daemon: loading default session config: %w", err)
	}
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
	if backends := buildNotifyBackends(notifyCfg); notifyCfg.Enabled && len(backends) > 0 {
		d.notifier = notify.New(backends, notify.Options{
			On:       notifyCfg.On,
			Debounce: notifyCfg.Debounce,
			Timeout:  notifyCfg.Timeout,
			Retries:  notifyCfg.Retries,
		}, d.clk, d.store, d.log)
		d.notifier.Start()
		notifier = d.notifier
		d.log.Info("notifier started", "backends", backendNames(backends), "on", notifyCfg.On)
	} else {
		d.log.Debug("notifier disabled; blocks recorded but not delivered",
			"enabled", notifyCfg.Enabled, "backends", len(backends))
	}

	d.engine = state.NewEngine(d.store, d.clk, state.EngineConfig{
		PermissionSettle: stateCfg.PermissionSettle,
		PermissionTTL:    stateCfg.PermissionTTL,
		StaleAfter:       stateCfg.StaleAfter,
		FirstHookGrace:   stateCfg.FirstHookGrace,
	}, notifier, d.log)

	relayCmd, err := resolveRelayCommand()
	if err != nil {
		return fmt.Errorf("daemon: resolving relay command: %w", err)
	}

	registry := supervisor.New(d.store, d.engine, checkpointerAdapter{d.checkpointer}, d.clk, supervisor.Config{
		StateDir:          d.cfg.StateDir,
		SockPath:          d.sockPath,
		SettingSources:    sessionCfg.SettingSources,
		EnvPassthrough:    sessionCfg.EnvPassthrough,
		Term:              sessionCfg.Term,
		OutputLogMaxBytes: sessionCfg.OutputLogMaxBytes,
		EnvSnapshot:       envSnapshot,
		CorralVersion:     version.Version,
		APIVersion:        version.APIVersion,
		RecoveryGrace:     grace,
		PingInterval:      attachCfg.PingInterval,
		PingTimeout:       attachCfg.PingTimeout,
		RelayCommand:      relayCmd,
		ClaudeHome:        claudeHome,
	}, d.log)
	d.supervisor = registry

	// Step 8: recovery, before the socket is up so no client observes a
	// half-recovered world (design doc §3.5).
	if err := d.runRecovery(ctx, registry); err != nil {
		return err
	}

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
	srv.RegisterHooks(api.HooksDeps{
		Store:   d.store,
		Engine:  d.engine,
		Secrets: registry,
	})
	srv.RegisterAttach(registry, d.log)
	d.srv = &http.Server{Handler: srv.Handler()}
	go func() {
		if err := d.srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("http server exited", "err", err)
		}
	}()

	if _, err := d.store.AppendEvent(ctx, "", session.EventDaemonStarted, ""); err != nil {
		d.log.Warn("recording daemon.started event", "err", err)
	}
	d.log.Info("daemon started", "pid", os.Getpid(), "socket", d.sockPath)

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
// supervisor.Checkpointer, dropping the Token that supervisor never needs.
// It exists only here (not in internal/supervisor) because
// internal/checkpoint already imports internal/supervisor for
// *supervisor.LiveSession — supervisor importing checkpoint back would be
// a cycle; daemon imports both, so the adapter lives here.
type checkpointerAdapter struct {
	c *checkpoint.ResumeCheckpointer
}

func (a checkpointerAdapter) Checkpoint(ctx context.Context, s *supervisor.LiveSession, reason string) error {
	_, err := a.c.Checkpoint(ctx, s, reason)
	return err
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
// syscall.Getsid(0) == os.Getpid(), logs WARN if not — silently-failed
// setsid degrades to 'works until terminal closes', exactly the bug class
// worth one-line assertion." It never fails startup — only logs.
func (d *Daemon) setsidSelfCheck() {
	sid, err := syscall.Getsid(0)
	if err != nil {
		d.log.Warn("setsid self-check: Getsid failed", "err", err)
		return
	}
	if sid != os.Getpid() {
		d.log.Warn("setsid self-check failed: process is not its own session leader; detachment from the launching terminal may not have taken effect", "sid", sid, "pid", os.Getpid())
	}
}

func captureEnvSnapshot() map[string]string {
	snapshot := make(map[string]string, len(envSnapshotWhitelist))
	for _, name := range envSnapshotWhitelist {
		if v, ok := os.LookupEnv(name); ok {
			snapshot[name] = v
		}
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
