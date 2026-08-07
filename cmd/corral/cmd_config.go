package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/danielbecerra/corral/internal/config"
)

// cmdConfig implements `corral config [--cwd DIR]`: it prints the fully
// resolved daemon+session+attach config, one "section.key = value" line per
// key annotated with its source, plus one "# ignored" line per repo-file key
// that was rejected by the trust boundary (§8.2, §8.4). It needs no running
// daemon — both LoadDaemon and LoadSession read files directly.
func cmdConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cwd := fs.String("cwd", "", "directory to resolve session/repo config from (default: current directory)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir := *cwd
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "corral: config: %v\n", err)
			return 1
		}
		dir = wd
	}

	daemon, daemonSources, err := config.LoadDaemon()
	if err != nil {
		fmt.Fprintf(stderr, "corral: config: %v\n", err)
		return 1
	}
	session, attach, sessionSources, rejections, err := config.LoadSession(dir, nil)
	if err != nil {
		fmt.Fprintf(stderr, "corral: config: %v\n", err)
		return 1
	}

	values := map[string]string{
		"daemon.socket":                daemon.Socket,
		"daemon.state_dir":             daemon.StateDir,
		"daemon.log_level":             daemon.LogLevel,
		"daemon.log_format":            daemon.LogFormat,
		"daemon.shutdown_grace":        daemon.ShutdownGrace.String(),
		"session.claude_bin":           session.ClaudeBin,
		"session.model":                session.Model,
		"session.setting_sources":      session.SettingSources,
		"session.env_passthrough":      fmt.Sprintf("%v", session.EnvPassthrough),
		"session.term":                 session.Term,
		"session.scrollback_lines":     fmt.Sprintf("%d", session.ScrollbackLines),
		"session.output_log_max_bytes": fmt.Sprintf("%d", session.OutputLogMaxBytes),
		"attach.prefix_key":            attach.PrefixKey,
		"attach.detach_key":            attach.DetachKey,
		"attach.ping_interval":         attach.PingInterval.String(),
		"attach.ping_timeout":          attach.PingTimeout.String(),
	}

	sources := make(map[string]string, len(daemonSources)+len(sessionSources))
	for k, v := range daemonSources {
		sources[k] = v
	}
	for k, v := range sessionSources {
		sources[k] = v
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		fmt.Fprintf(stdout, "%s = %s   # from %s\n", key, values[key], sources[key])
	}
	for _, r := range rejections {
		fmt.Fprintf(stdout, "# ignored (repo files may not set this key): %s in %s\n", r.Key, r.File)
	}

	return 0
}
