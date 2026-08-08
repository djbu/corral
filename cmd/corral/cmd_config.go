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

	eff, err := config.Effective(dir)
	if err != nil {
		fmt.Fprintf(stderr, "corral: config: %v\n", err)
		return 1
	}

	keys := make([]string, 0, len(eff.Values))
	for k := range eff.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		fmt.Fprintf(stdout, "%s = %s   # from %s\n", key, eff.Values[key], eff.Sources[key])
	}
	for _, r := range eff.Ignored {
		fmt.Fprintf(stdout, "# ignored (repo files may not set this key): %s in %s\n", r.Key, r.File)
	}

	return 0
}
