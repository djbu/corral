package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// defaultsLayer holds the complete M1 defaults block from §8.4. Every field
// is non-nil, so after merging it in first, every subsequent resolve* call
// always has a value to parse — no source ever leaves a key unset.
func defaultsLayer() *layer {
	return &layer{
		Daemon: daemonLayer{
			Socket:        strPtr("~/.corral/corral.sock"),
			StateDir:      strPtr("~/.corral"),
			LogLevel:      strPtr("info"),
			LogFormat:     strPtr("text"),
			ShutdownGrace: strPtr("5s"),
		},
		Session: sessionLayer{
			ClaudeBin:         strPtr("claude"),
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
	return Daemon{
		Socket:        expandHome(derefStr(l.Socket)),
		StateDir:      expandHome(derefStr(l.StateDir)),
		LogLevel:      derefStr(l.LogLevel),
		LogFormat:     derefStr(l.LogFormat),
		ShutdownGrace: grace,
	}, nil
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
		Model:             derefStr(l.Model),
		SettingSources:    derefStr(l.SettingSources),
		EnvPassthrough:    passthrough,
		Term:              derefStr(l.Term),
		ScrollbackLines:   derefInt(l.ScrollbackLines),
		OutputLogMaxBytes: n,
		PermissionMode:    derefStr(l.PermissionMode),
	}, nil
}

// resolveState parses a fully-merged stateLayer into State. All six duration
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
	return State{
		StaleAfter:           staleAfter,
		FirstHookGrace:       firstHookGrace,
		PendingToolTTL:       pendingToolTTL,
		MaxEventPayloadBytes: maxEventPayloadBytes,
		PersistHookEvents:    derefStr(l.PersistHookEvents),
		HookTimeout:          hookTimeout,
		PermissionSettle:     permissionSettle,
		PermissionTTL:        permissionTTL,
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
