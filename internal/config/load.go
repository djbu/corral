package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"
)

// defaultsLayer holds the complete M1 defaults block from §8.4. Every field
// is non-nil, so after merging it in first, every subsequent resolve* call
// always has a value to parse — no source ever leaves a key unset.
func defaultsLayer() *layer {
	return &layer{
		Daemon: daemonLayer{
			Socket:                 strPtr("~/.corral/corral.sock"),
			StateDir:               strPtr("~/.corral"),
			LogLevel:               strPtr("info"),
			LogFormat:              strPtr("text"),
			ShutdownGrace:          strPtr("5s"),
			Listen:                 strPtr(""),
			TLSCert:                strPtr(""),
			TLSKey:                 strPtr(""),
			MinFreeBytes:           strPtr("256MiB"),
			MaxInteractiveSessions: intPtr(16),
			MaxHeadlessTasks:       intPtr(4),
			MaxPendingDAGTasks:     intPtr(1000),
		},
		Session: sessionLayer{
			ClaudeBin:         strPtr("claude"),
			Template:          strPtr(""),
			ClaudeVersion:     strPtr(""),
			Model:             strPtr(""),
			SettingSources:    strPtr("user,project,local"),
			EnvPassthrough:    strsPtr([]string{}),
			Term:              strPtr("xterm-256color"),
			ScrollbackLines:   intPtr(2000),
			OutputLogMaxBytes: strPtr("8MiB"),
			PermissionMode:    strPtr(""),
		},
		Attach: attachLayer{
			PrefixKey:    strPtr(`C-\`),
			DetachKey:    strPtr("d"),
			PingInterval: strPtr("15s"),
			PingTimeout:  strPtr("45s"),
		},
	}
}

// defaultsStateLayer holds the [state] defaults (design doc §8.7, Amendment
// A.3.1/A.3.2). Unlike defaultsLayer's Daemon/Session/Attach blocks, this is
// only ever merged by LoadState, never by LoadDaemon/LoadSession's merge()
// call.
func defaultsStateLayer() *stateLayer {
	return &stateLayer{
		StaleAfter:           strPtr("15m"),
		FirstHookGrace:       strPtr("60s"),
		PendingToolTTL:       strPtr("30m"),
		MaxEventPayloadBytes: strPtr("64KiB"),
		PersistHookEvents:    strPtr("transitions"),
		HookTimeout:          strPtr("2s"),
		PermissionSettle:     strPtr("15s"),
		PermissionTTL:        strPtr("6h"),
		IdleTimeout:          strPtr("0s"),
	}
}

// defaultsNotifyLayer holds the [notify] defaults (design doc §8.7).
// Unlike defaultsLayer's Daemon/Session/Attach blocks, this is only ever
// merged by LoadNotify, never by LoadDaemon/LoadSession's merge() call.
func defaultsNotifyLayer() *notifyLayer {
	return &notifyLayer{
		Enabled:  boolPtr(false),
		On:       strsPtr([]string{"blocked"}),
		Debounce: strPtr("30s"),
		Timeout:  strPtr("10s"),
		Retries:  intPtr(3),
		Ntfy: notifyNtfyLayer{
			Enabled:  boolPtr(false),
			Server:   strPtr("https://ntfy.sh"),
			Topic:    strPtr(""),
			Token:    strPtr(""),
			Priority: strPtr("default"),
			Reply: notifyNtfyReplyLayer{
				Enabled: boolPtr(false),
				Topic:   strPtr(""),
				Token:   strPtr(""),
			},
		},
		Webhook: notifyWebhookLayer{
			Enabled: boolPtr(false),
			URL:     strPtr(""),
			Headers: map[string]string{},
		},
	}
}

// defaultsClientLayer holds the [client] defaults (design doc m5.md §7): all
// empty, meaning "no remote target — use the local unix socket" (§7 rule 3:
// resolving no host must never fall back to dialing anything). Unlike
// defaultsLayer's Daemon/Session/Attach blocks, this is only ever merged by
// LoadClient, never by LoadDaemon/LoadSession's merge() call.
func defaultsClientLayer() *clientLayer {
	return &clientLayer{
		Host:   strPtr(""),
		Token:  strPtr(""),
		CACert: strPtr(""),
	}
}

func defaultsLearnLayer() *learnLayer {
	return &learnLayer{
		Window:                  strPtr("336h"),
		MinApprovals:            intPtr(3),
		TTL:                     strPtr("2160h"),
		MinSessions:             intPtr(5),
		MinTerminalTasks:        intPtr(3),
		CostRegressionTolerance: floatPtr(0.10),
	}
}

// userConfigPath returns ~/.corral/config.toml.
func userConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolving home directory: %w", err)
	}
	return filepath.Join(home, ".corral", "config.toml"), nil
}

// decodeTOMLLayerIfExists decodes path into a layer, returning (nil, nil)
// if the file does not exist. Decode errors are wrapped to name the file,
// per §10.2's "malformed TOML error message names the file".
func decodeTOMLLayerIfExists(path string) (*layer, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: stat %s: %w", path, err)
	}
	l := newLayer()
	if _, err := toml.DecodeFile(path, l); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	return l, nil
}

// findRepoFile returns the nearest .corral.toml walking up from cwd to the
// git root inclusive, or to the filesystem root if no .git marker is found
// first. It returns "" (no error) if none exists. A git "root" is any
// directory containing a .git entry, whether a directory (a normal clone)
// or a file (a worktree/submodule pointer) — the repo file at that level is
// still checked before search stops.
func findRepoFile(cwd string) (string, error) {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("config: resolving %s: %w", cwd, err)
	}
	for {
		candidate := filepath.Join(dir, ".corral.toml")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return "", nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

func osLookup(key string) (string, bool) { return os.LookupEnv(key) }

// LoadDaemon resolves daemon-scope config: defaults -> ~/.corral/config.toml
// -> env. A repo file and API request fields can never affect daemon scope
// (§8.1) — this pipeline doesn't consult either.
func LoadDaemon() (Daemon, map[string]string, error) {
	merged := newLayer()
	sources := make(map[string]string)
	merge(merged, defaultsLayer(), uniformSource(sourceDefault), sources)

	userPath, err := userConfigPath()
	if err != nil {
		return Daemon{}, nil, err
	}
	userLayer, err := decodeTOMLLayerIfExists(userPath)
	if err != nil {
		return Daemon{}, nil, err
	}
	if userLayer != nil {
		merge(merged, userLayer, uniformSource(userPath), sources)
	}

	envLayer, envSources, err := layerFromEnv(osLookup)
	if err != nil {
		return Daemon{}, nil, err
	}
	merge(merged, envLayer, mapSource(envSources), sources)

	d, err := resolveDaemon(&merged.Daemon)
	if err != nil {
		return Daemon{}, nil, err
	}
	return d, sources, nil
}

// LoadSession resolves session- and attach-scope config for a session to be
// spawned from cwd: defaults -> ~/.corral/config.toml -> nearest
// .corral.toml (repo-file trust boundary applied) -> env -> request.
// request carries per-API-request overrides and may be nil.
func LoadSession(cwd string, request *layer) (Session, Attach, map[string]string, []Rejection, error) {
	merged := newLayer()
	sources := make(map[string]string)
	merge(merged, defaultsLayer(), uniformSource(sourceDefault), sources)

	userPath, err := userConfigPath()
	if err != nil {
		return Session{}, Attach{}, nil, nil, err
	}
	userLayer, err := decodeTOMLLayerIfExists(userPath)
	if err != nil {
		return Session{}, Attach{}, nil, nil, err
	}
	if userLayer != nil {
		merge(merged, userLayer, uniformSource(userPath), sources)
	}

	var rejections []Rejection
	repoPath, err := findRepoFile(cwd)
	if err != nil {
		return Session{}, Attach{}, nil, nil, err
	}
	if repoPath != "" {
		repoLayer, err := decodeTOMLLayerIfExists(repoPath)
		if err != nil {
			return Session{}, Attach{}, nil, nil, err
		}
		if repoLayer != nil {
			filtered, rej := filterRepoLayer(repoPath, repoLayer)
			rejections = append(rejections, rej...)
			merge(merged, filtered, uniformSource(repoPath), sources)
		}
	}

	envLayer, envSources, err := layerFromEnv(osLookup)
	if err != nil {
		return Session{}, Attach{}, nil, nil, err
	}
	merge(merged, envLayer, mapSource(envSources), sources)

	if request != nil {
		merge(merged, request, uniformSource(sourceRequest), sources)
	}

	sess, err := resolveSession(&merged.Session)
	if err != nil {
		return Session{}, Attach{}, nil, nil, err
	}
	att, err := resolveAttach(&merged.Attach)
	if err != nil {
		return Session{}, Attach{}, nil, nil, err
	}
	return sess, att, sources, rejections, nil
}

func resolveDaemon(l *daemonLayer) (Daemon, error) {
	grace, err := time.ParseDuration(derefStr(l.ShutdownGrace))
	if err != nil {
		return Daemon{}, fmt.Errorf("config: daemon.shutdown_grace=%q: %w", derefStr(l.ShutdownGrace), err)
	}
	minFree, err := ParseBytes(derefStr(l.MinFreeBytes))
	if err != nil {
		return Daemon{}, fmt.Errorf("config: daemon.min_free_bytes=%q: %w", derefStr(l.MinFreeBytes), err)
	}
	maxInteractive, err := positiveDaemonInt("max_interactive_sessions", l.MaxInteractiveSessions)
	if err != nil {
		return Daemon{}, err
	}
	maxHeadless, err := positiveDaemonInt("max_headless_tasks", l.MaxHeadlessTasks)
	if err != nil {
		return Daemon{}, err
	}
	maxPending, err := positiveDaemonInt("max_pending_dag_tasks", l.MaxPendingDAGTasks)
	if err != nil {
		return Daemon{}, err
	}
	return Daemon{
		Socket:        expandHome(derefStr(l.Socket)),
		StateDir:      expandHome(derefStr(l.StateDir)),
		LogLevel:      derefStr(l.LogLevel),
		LogFormat:     derefStr(l.LogFormat),
		ShutdownGrace: grace,
		// Listen is a network address, not a path — it is never
		// home-expanded. TLSCert/TLSKey ARE file paths, so they are.
		Listen:                 derefStr(l.Listen),
		TLSCert:                expandHome(derefStr(l.TLSCert)),
		TLSKey:                 expandHome(derefStr(l.TLSKey)),
		MinFreeBytes:           minFree,
		MaxInteractiveSessions: maxInteractive,
		MaxHeadlessTasks:       maxHeadless,
		MaxPendingDAGTasks:     maxPending,
	}, nil
}

func positiveDaemonInt(key string, value *int) (int, error) {
	if value == nil || *value <= 0 {
		raw := ""
		if value != nil {
			raw = strconv.Itoa(*value)
		}
		return 0, fmt.Errorf("config: daemon.%s=%q: must be a positive integer", key, raw)
	}
	return *value, nil
}

func resolveSession(l *sessionLayer) (Session, error) {
	n, err := ParseBytes(derefStr(l.OutputLogMaxBytes))
	if err != nil {
		return Session{}, fmt.Errorf("config: session.output_log_max_bytes=%q: %w", derefStr(l.OutputLogMaxBytes), err)
	}
	var passthrough []string
	if l.EnvPassthrough != nil {
		passthrough = *l.EnvPassthrough
	}
	return Session{
		ClaudeBin:         derefStr(l.ClaudeBin),
		Template:          derefStr(l.Template),
		ClaudeVersion:     derefStr(l.ClaudeVersion),
		Model:             derefStr(l.Model),
		SettingSources:    derefStr(l.SettingSources),
		EnvPassthrough:    passthrough,
		Term:              derefStr(l.Term),
		ScrollbackLines:   derefInt(l.ScrollbackLines),
		OutputLogMaxBytes: n,
		PermissionMode:    derefStr(l.PermissionMode),
	}, nil
}

// resolveState parses a fully-merged stateLayer into State. All duration
// fields use time.ParseDuration; MaxEventPayloadBytes uses the same
// ParseBytes helper as session.output_log_max_bytes; PersistHookEvents is
// carried through as a plain string (its enum of values is validated by the
// state engine, a later step, not here).
func resolveState(l *stateLayer) (State, error) {
	staleAfter, err := time.ParseDuration(derefStr(l.StaleAfter))
	if err != nil {
		return State{}, fmt.Errorf("config: state.stale_after=%q: %w", derefStr(l.StaleAfter), err)
	}
	firstHookGrace, err := time.ParseDuration(derefStr(l.FirstHookGrace))
	if err != nil {
		return State{}, fmt.Errorf("config: state.first_hook_grace=%q: %w", derefStr(l.FirstHookGrace), err)
	}
	pendingToolTTL, err := time.ParseDuration(derefStr(l.PendingToolTTL))
	if err != nil {
		return State{}, fmt.Errorf("config: state.pending_tool_ttl=%q: %w", derefStr(l.PendingToolTTL), err)
	}
	maxEventPayloadBytes, err := ParseBytes(derefStr(l.MaxEventPayloadBytes))
	if err != nil {
		return State{}, fmt.Errorf("config: state.max_event_payload_bytes=%q: %w", derefStr(l.MaxEventPayloadBytes), err)
	}
	hookTimeout, err := time.ParseDuration(derefStr(l.HookTimeout))
	if err != nil {
		return State{}, fmt.Errorf("config: state.hook_timeout=%q: %w", derefStr(l.HookTimeout), err)
	}
	permissionSettle, err := time.ParseDuration(derefStr(l.PermissionSettle))
	if err != nil {
		return State{}, fmt.Errorf("config: state.permission_settle=%q: %w", derefStr(l.PermissionSettle), err)
	}
	permissionTTL, err := time.ParseDuration(derefStr(l.PermissionTTL))
	if err != nil {
		return State{}, fmt.Errorf("config: state.permission_ttl=%q: %w", derefStr(l.PermissionTTL), err)
	}
	idleTimeout, err := time.ParseDuration(derefStr(l.IdleTimeout))
	if err != nil {
		return State{}, fmt.Errorf("config: state.idle_timeout=%q: %w", derefStr(l.IdleTimeout), err)
	}
	return State{
		StaleAfter:           staleAfter,
		FirstHookGrace:       firstHookGrace,
		PendingToolTTL:       pendingToolTTL,
		MaxEventPayloadBytes: maxEventPayloadBytes,
		PersistHookEvents:    derefStr(l.PersistHookEvents),
		HookTimeout:          hookTimeout,
		PermissionSettle:     permissionSettle,
		PermissionTTL:        permissionTTL,
		IdleTimeout:          idleTimeout,
	}, nil
}

// LoadState resolves [state]-scope config: defaults -> ~/.corral/config.toml
// -> env. Entirely user-file/env only (design doc §8.7, Amendment A.3.1/
// A.3.2) — no repo file, no request stage — so it merges directly with
// mergeState rather than going through LoadDaemon/LoadSession's merge().
func LoadState() (State, map[string]string, error) {
	merged := newLayer()
	sources := make(map[string]string)
	mergeState(&merged.State, defaultsStateLayer(), uniformSource(sourceDefault), sources)

	userPath, err := userConfigPath()
	if err != nil {
		return State{}, nil, err
	}
	userLayer, err := decodeTOMLLayerIfExists(userPath)
	if err != nil {
		return State{}, nil, err
	}
	if userLayer != nil {
		mergeState(&merged.State, &userLayer.State, uniformSource(userPath), sources)
	}

	envLayer, envSources, err := layerFromEnv(osLookup)
	if err != nil {
		return State{}, nil, err
	}
	mergeState(&merged.State, &envLayer.State, mapSource(envSources), sources)

	st, err := resolveState(&merged.State)
	if err != nil {
		return State{}, nil, err
	}
	return st, sources, nil
}

// resolveNotify parses a fully-merged notifyLayer into Notify (design doc
// §8.7). Debounce/Timeout use time.ParseDuration; Retries is already *int
// (parsed at env-decode time in env.go, or decoded directly by TOML — see
// notifyLayer's doc comment) so it's just dereferenced here; On, and all
// string/bool fields and the Headers map, are carried through as-is (no
// cross-field validation here — see the design doc's later reply-subscriber
// step for that).
func resolveNotify(l *notifyLayer) (Notify, error) {
	debounce, err := time.ParseDuration(derefStr(l.Debounce))
	if err != nil {
		return Notify{}, fmt.Errorf("config: notify.debounce=%q: %w", derefStr(l.Debounce), err)
	}
	timeout, err := time.ParseDuration(derefStr(l.Timeout))
	if err != nil {
		return Notify{}, fmt.Errorf("config: notify.timeout=%q: %w", derefStr(l.Timeout), err)
	}
	retries := derefInt(l.Retries)
	on := []string{"blocked"}
	if l.On != nil {
		on = *l.On
	}
	return Notify{
		Enabled:  derefBool(l.Enabled),
		On:       on,
		Debounce: debounce,
		Timeout:  timeout,
		Retries:  retries,
		Ntfy: NotifyNtfy{
			Enabled:  derefBool(l.Ntfy.Enabled),
			Server:   derefStr(l.Ntfy.Server),
			Topic:    derefStr(l.Ntfy.Topic),
			Token:    derefStr(l.Ntfy.Token),
			Priority: derefStr(l.Ntfy.Priority),
			Reply: NotifyNtfyReply{
				Enabled: derefBool(l.Ntfy.Reply.Enabled),
				Topic:   derefStr(l.Ntfy.Reply.Topic),
				Token:   derefStr(l.Ntfy.Reply.Token),
			},
		},
		Webhook: NotifyWebhook{
			Enabled: derefBool(l.Webhook.Enabled),
			URL:     derefStr(l.Webhook.URL),
			Headers: l.Webhook.Headers,
		},
	}, nil
}

// ValidateReply enforces the ntfy reply subscriber's startup gate (design doc
// §8.6, gates 1 and 2). It returns a non-nil error — which the daemon treats
// as fatal, refusing to start — when the reply subscriber is enabled but
// misconfigured, so a guessable or anonymous reply topic can never grant PTY
// keystroke access. It is a no-op (nil) when the reply subscriber is disabled.
// Gate 3 (per-message: session exists, is live, is blocked) is enforced at
// delivery time by the subscriber itself, not here.
func ValidateReply(n Notify) error {
	r := n.Ntfy.Reply
	if !r.Enabled {
		return nil
	}
	if !n.Ntfy.Enabled {
		return fmt.Errorf("config: notify.ntfy.reply.enabled requires notify.ntfy.enabled")
	}
	if r.Topic == "" {
		return fmt.Errorf("config: notify.ntfy.reply.topic is required when the reply subscriber is enabled")
	}
	if r.Token == "" {
		return fmt.Errorf("config: notify.ntfy.reply.token is required when the reply subscriber is enabled")
	}
	if r.Topic == n.Ntfy.Topic {
		return fmt.Errorf("config: notify.ntfy.reply.topic must differ from notify.ntfy.topic (a shared read topic would grant PTY write access)")
	}
	return nil
}

// LoadNotify resolves [notify]-scope config: defaults ->
// ~/.corral/config.toml -> env. Entirely user-file/env only (design doc
// §8.7) — no repo file, no request stage — so it merges directly with
// mergeNotify rather than going through LoadDaemon/LoadSession's merge().
func LoadNotify() (Notify, map[string]string, error) {
	merged := newLayer()
	sources := make(map[string]string)
	mergeNotify(&merged.Notify, defaultsNotifyLayer(), uniformSource(sourceDefault), sources)

	userPath, err := userConfigPath()
	if err != nil {
		return Notify{}, nil, err
	}
	userLayer, err := decodeTOMLLayerIfExists(userPath)
	if err != nil {
		return Notify{}, nil, err
	}
	if userLayer != nil {
		mergeNotify(&merged.Notify, &userLayer.Notify, uniformSource(userPath), sources)
	}

	envLayer, envSources, err := layerFromEnv(osLookup)
	if err != nil {
		return Notify{}, nil, err
	}
	mergeNotify(&merged.Notify, &envLayer.Notify, mapSource(envSources), sources)

	n, err := resolveNotify(&merged.Notify)
	if err != nil {
		return Notify{}, nil, err
	}
	return n, sources, nil
}

// resolveClient parses a fully-merged clientLayer into Client. Host is a
// network address ("host:port"), not a path, so — like daemon.listen — it
// is never home-expanded; Token is a secret, also never home-expanded (it
// isn't a path at all); CACert IS a file path, so it is home-expanded, the
// same daemon.listen-vs-daemon.tls_cert precedent resolveDaemon follows.
func resolveClient(l *clientLayer) (Client, error) {
	return Client{
		Host:   derefStr(l.Host),
		Token:  derefStr(l.Token),
		CACert: expandHome(derefStr(l.CACert)),
	}, nil
}

// LoadClient resolves [client]-scope config: defaults -> ~/.corral/
// config.toml -> env. Entirely user-file/env only (design doc m5.md §7) —
// no repo file, no request stage — so it merges directly with mergeClient
// rather than going through LoadDaemon/LoadSession's merge().
func LoadClient() (Client, map[string]string, error) {
	merged := newLayer()
	sources := make(map[string]string)
	mergeClient(&merged.Client, defaultsClientLayer(), uniformSource(sourceDefault), sources)

	userPath, err := userConfigPath()
	if err != nil {
		return Client{}, nil, err
	}
	userLayer, err := decodeTOMLLayerIfExists(userPath)
	if err != nil {
		return Client{}, nil, err
	}
	if userLayer != nil {
		mergeClient(&merged.Client, &userLayer.Client, uniformSource(userPath), sources)
	}

	envLayer, envSources, err := layerFromEnv(osLookup)
	if err != nil {
		return Client{}, nil, err
	}
	mergeClient(&merged.Client, &envLayer.Client, mapSource(envSources), sources)

	cl, err := resolveClient(&merged.Client)
	if err != nil {
		return Client{}, nil, err
	}
	return cl, sources, nil
}

// LoadLearn resolves [learn]-scope policy: defaults -> user file -> env.
// Learning policy is never read from a repository file or API request.
func LoadLearn() (Learn, map[string]string, error) {
	merged := newLayer()
	sources := make(map[string]string)
	mergeLearn(&merged.Learn, defaultsLearnLayer(), uniformSource(sourceDefault), sources)

	userPath, err := userConfigPath()
	if err != nil {
		return Learn{}, nil, err
	}
	userLayer, err := decodeTOMLLayerIfExists(userPath)
	if err != nil {
		return Learn{}, nil, err
	}
	if userLayer != nil {
		mergeLearn(&merged.Learn, &userLayer.Learn, uniformSource(userPath), sources)
	}

	envLayer, envSources, err := layerFromEnv(osLookup)
	if err != nil {
		return Learn{}, nil, err
	}
	mergeLearn(&merged.Learn, &envLayer.Learn, mapSource(envSources), sources)

	learn, err := resolveLearn(&merged.Learn)
	if err != nil {
		return Learn{}, nil, err
	}
	return learn, sources, nil
}

func resolveLearn(l *learnLayer) (Learn, error) {
	window, err := time.ParseDuration(derefStr(l.Window))
	if err != nil {
		return Learn{}, fmt.Errorf("config: learn.window=%q: %w", derefStr(l.Window), err)
	}
	ttl, err := time.ParseDuration(derefStr(l.TTL))
	if err != nil {
		return Learn{}, fmt.Errorf("config: learn.ttl=%q: %w", derefStr(l.TTL), err)
	}
	learn := Learn{
		Window:                  window,
		MinApprovals:            derefInt(l.MinApprovals),
		TTL:                     ttl,
		MinSessions:             derefInt(l.MinSessions),
		MinTerminalTasks:        derefInt(l.MinTerminalTasks),
		CostRegressionTolerance: derefFloat(l.CostRegressionTolerance),
	}
	if learn.Window <= 0 {
		return Learn{}, fmt.Errorf("config: learn.window must be positive")
	}
	if learn.TTL <= 0 {
		return Learn{}, fmt.Errorf("config: learn.ttl must be positive")
	}
	if learn.MinApprovals <= 0 {
		return Learn{}, fmt.Errorf("config: learn.min_approvals must be positive")
	}
	if learn.MinSessions <= 0 {
		return Learn{}, fmt.Errorf("config: learn.min_sessions must be positive")
	}
	if learn.MinTerminalTasks <= 0 {
		return Learn{}, fmt.Errorf("config: learn.min_terminal_tasks must be positive")
	}
	if learn.CostRegressionTolerance < 0 {
		return Learn{}, fmt.Errorf("config: learn.cost_regression_tolerance must be non-negative")
	}
	return learn, nil
}

func resolveAttach(l *attachLayer) (Attach, error) {
	interval, err := time.ParseDuration(derefStr(l.PingInterval))
	if err != nil {
		return Attach{}, fmt.Errorf("config: attach.ping_interval=%q: %w", derefStr(l.PingInterval), err)
	}
	timeout, err := time.ParseDuration(derefStr(l.PingTimeout))
	if err != nil {
		return Attach{}, fmt.Errorf("config: attach.ping_timeout=%q: %w", derefStr(l.PingTimeout), err)
	}
	return Attach{
		PrefixKey:    derefStr(l.PrefixKey),
		DetachKey:    derefStr(l.DetachKey),
		PingInterval: interval,
		PingTimeout:  timeout,
	}, nil
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
