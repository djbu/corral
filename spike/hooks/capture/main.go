// corral-hookcap: throwaway hook-capture harness for M2 Step-0 (spike/hooks/).
// Wired as the `command` for every hook event in a pinned --settings file.
// Appends one JSON line per invocation to the file named by --out, recording
// argv, env vars of interest, raw stdin, best-effort parsed stdin, pid/ppid,
// and a timestamp. Always exits 0 with empty stdout (per V9's inertness
// check) unless CORRAL_HOOKCAP_STDOUT is set (used for the one V9 probe of
// hook stdout JSON-output handling) or CORRAL_HOOKCAP_EXIT is set (used for
// the one V14 stop_hook_active probe).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type record struct {
	Timestamp string            `json:"timestamp"`
	Argv      []string          `json:"argv"`
	Pid       int               `json:"pid"`
	Ppid      int               `json:"ppid"`
	Env       map[string]string `json:"env"`
	StdinRaw  string            `json:"stdin_raw"`
	StdinJSON json.RawMessage   `json:"stdin_json,omitempty"`
	StdinErr  string            `json:"stdin_decode_err,omitempty"`
}

func main() {
	out := flag.String("out", "", "capture file to append to")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "corral-hookcap: --out required")
		os.Exit(0) // never break the agent even if misconfigured
	}

	stdin, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))

	env := map[string]string{}
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k := parts[0]
		if strings.HasPrefix(k, "CORRAL_") || strings.HasPrefix(k, "CLAUDE_") ||
			strings.HasPrefix(k, "ANTHROPIC_") {
			env[k] = parts[1]
		}
	}

	rec := record{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Argv:      os.Args,
		Pid:       os.Getpid(),
		Ppid:      os.Getppid(),
		Env:       env,
		StdinRaw:  string(stdin),
	}
	var generic json.RawMessage
	if err := json.Unmarshal(stdin, &generic); err != nil {
		rec.StdinErr = err.Error()
	} else {
		rec.StdinJSON = generic
	}

	line, _ := json.Marshal(rec)
	f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		f.Write(line)
		f.Write([]byte("\n"))
		f.Close()
	}

	// V9 probe: if CORRAL_HOOKCAP_STDOUT is set, print it verbatim to stdout
	// instead of staying inert, so we can observe how claude treats hook
	// stdout JSON output.
	if s := os.Getenv("CORRAL_HOOKCAP_STDOUT"); s != "" {
		fmt.Print(s)
	}

	// V14 probe: if CORRAL_HOOKCAP_EXIT is set to a marker file path, and
	// that marker file does not yet exist, create it and exit with the code
	// in CORRAL_HOOKCAP_EXITCODE (default 2, "block") -- used exactly once
	// to see if a blocking Stop produces a second Stop with
	// stop_hook_active:true. Second invocation (marker exists) is inert.
	if markerPath := os.Getenv("CORRAL_HOOKCAP_EXIT"); markerPath != "" {
		scopeEvent := os.Getenv("CORRAL_HOOKCAP_EXIT_EVENT")
		var thisEvent string
		if generic != nil {
			var m map[string]json.RawMessage
			if json.Unmarshal(generic, &m) == nil {
				var s string
				if json.Unmarshal(m["hook_event_name"], &s) == nil {
					thisEvent = s
				}
			}
		}
		if scopeEvent == "" || scopeEvent == thisEvent {
			if _, err := os.Stat(markerPath); os.IsNotExist(err) {
				os.WriteFile(markerPath, []byte("fired\n"), 0o644)
				code := 2
				if c := os.Getenv("CORRAL_HOOKCAP_EXITCODE"); c != "" {
					if n, err := strconv.Atoi(c); err == nil {
						code = n
					}
				}
				if es := os.Getenv("CORRAL_HOOKCAP_STDERR"); es != "" {
					fmt.Fprint(os.Stderr, es)
				}
				os.Exit(code)
			}
		}
	}

	os.Exit(0)
}
