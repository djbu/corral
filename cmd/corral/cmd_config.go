package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/djbu/corral/internal/api/client"
	"github.com/djbu/corral/internal/config"
)

// cmdConfig implements `corral config [--cwd DIR] [--host HOST]`: it prints
// the fully resolved daemon+session+attach config, one "section.key = value"
// line per key annotated with its source, plus one "# ignored" line per
// repo-file key that was rejected by the trust boundary (§8.2, §8.4).
//
// With no --host (the default — this command does not go through
// newClient's single choke-point, since its whole point is to resolve
// config WITHOUT talking to a daemon), it needs no running daemon at all:
// both LoadDaemon and LoadSession read files directly, resolving THIS
// machine's config for dir. With --host (or CORRAL_CLIENT_HOST/client.host
// resolved via resolveTarget), it instead calls GET /v1/config on the
// remote daemon, so the printed config is that daemon's own resolution of
// dir (a path on ITS filesystem, not this one) — the identical
// config.Effective rendering (see effective.go's doc comment), just
// fetched over the wire instead of computed in-process.
func cmdConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cwd := fs.String("cwd", "", "directory to resolve session/repo config from (default: current directory)")
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	dir := *cwd
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "corral: config: %v\n", err)
			return exitError
		}
		dir = wd
	}

	host, _, cl, err := resolveTarget(cf)
	if err != nil {
		fmt.Fprintf(stderr, "corral: config: %v\n", err)
		return exitError
	}

	var values, sources map[string]string
	var ignored []string // pre-rendered "# ignored ..." lines

	if host == "" {
		eff, err := config.Effective(dir)
		if err != nil {
			fmt.Fprintf(stderr, "corral: config: %v\n", err)
			return exitError
		}
		values, sources = eff.Values, eff.Sources
		for _, r := range eff.Ignored {
			ignored = append(ignored, fmt.Sprintf("# ignored (repo files may not set this key): %s in %s\n", r.Key, r.File))
		}
	} else {
		rc, err := client.NewRemote(host, cl.Token, cl.CACert, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "corral: config: %v\n", err)
			return exitError
		}
		info, err := rc.Config(context.Background(), dir)
		if err != nil {
			fmt.Fprintf(stderr, "corral: config: %v\n", err)
			return exitError
		}
		values, sources = info.Values, info.Sources
		for _, r := range info.Ignored {
			ignored = append(ignored, fmt.Sprintf("# ignored (repo files may not set this key): %s in %s\n", r.Key, r.File))
		}
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		fmt.Fprintf(stdout, "%s = %s   # from %s\n", key, values[key], sources[key])
	}
	for _, line := range ignored {
		fmt.Fprint(stdout, line)
	}

	return exitOK
}
