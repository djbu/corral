// Command fakeclaude is the M1 stand-in for the real claude binary
// (design doc §10.1), used by the integration harness in place of
// spec.ClaudeBin so tests never depend on a real Anthropic account. It
// deliberately does six things and nothing more:
//
//  1. Accepts corral's exact M1 flag surface without dying on unknown
//     flags.
//  2. Records every invocation (full argv, environ, cwd, and the parsed
//     --settings file contents) to
//     $CORRAL_FAKE_STATE/<session-id>/invocation-<N>.json.
//  3. Enters the alt screen with the exact private-mode set the M0
//     capture recorded, then renders a truecolor banner and a
//     cursor-addressed (never append-only) transcript region.
//  4. Persists a fake transcript at the same path real claude would use
//     (via internal/claude/sessions), and redraws prior turns on
//     --resume.
//  5. Honors CORRAL_FAKE_IGNORE_SIGTERM, CORRAL_FAKE_EXIT_AFTER, and
//     CORRAL_FAKE_EXIT_CODE.
//  6. Never answers a device query (that is the terminal emulator's job,
//     on corral's side of the PTY) and never prints a trust prompt.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// fakeSpec is fakeclaude's own parsed view of the flags it was invoked
// with — design doc §10.1 item 1's exact M1 flag surface.
type fakeSpec struct {
	SessionID      string
	Resume         string
	Settings       string
	SettingSources string
	Model          string
	Name           string
}

// parseArgs scans argv (excluding argv[0]) for the known M1 flags,
// taking each flag's next token as its value. Go's flag package errors
// and exits on an unrecognized flag, which item 1 explicitly forbids
// ("without dying on unknown flags"), so this is a hand-rolled scan
// instead. Any token that is not one of the known flag names — an
// unrecognized flag, or a bare positional argument — is skipped in
// place rather than treated as consuming the next token: since its
// arity is unknown, guessing wrong could misalign a later known flag's
// value.
func parseArgs(argv []string) fakeSpec {
	var spec fakeSpec
	targets := map[string]*string{
		"--session-id":      &spec.SessionID,
		"-r":                &spec.Resume,
		"--resume":          &spec.Resume,
		"--settings":        &spec.Settings,
		"--setting-sources": &spec.SettingSources,
		"--model":           &spec.Model,
		"-n":                &spec.Name,
		"--name":            &spec.Name,
	}
	for i := 0; i < len(argv); i++ {
		target, ok := targets[argv[i]]
		if ok && i+1 < len(argv) {
			*target = argv[i+1]
			i++
		}
	}
	return spec
}

func main() {
	// Install the SIGTERM handler before anything else — parseArgs,
	// recordInvocation's disk I/O, disableEcho, and writeStartup's PTY
	// write all take real (if normally small) wall-clock time, and
	// corral's Kill can send SIGTERM the instant Spawn returns. Under
	// load, that window is enough for the OS's default (terminate)
	// disposition to kill this process before signal.Notify below ever
	// runs, defeating CORRAL_FAKE_IGNORE_SIGTERM regardless of its value
	// (design doc §10.1 item 5; this exact race caused a flaky
	// TestGraceEscalation).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	ignoreSIGTERM := os.Getenv("CORRAL_FAKE_IGNORE_SIGTERM") == "1"

	// design doc §10.4's TestResizePropagates (step 10): corral's PTY
	// resize (pty.Setsize on the master) delivers SIGWINCH to this
	// process same as it would to real claude. Registered here, next to
	// the SIGTERM handler above, for the same reason: everything below
	// this point takes real wall-clock time before the main select loop
	// is even reachable.
	sigWinch := make(chan os.Signal, 1)
	signal.Notify(sigWinch, syscall.SIGWINCH)

	spec := parseArgs(os.Args[1:])

	sessionID := spec.SessionID
	if sessionID == "" {
		sessionID = spec.Resume
	}

	fakeHome := os.Getenv("CORRAL_FAKE_HOME")
	fakeState := os.Getenv("CORRAL_FAKE_STATE")
	scenarioPath := os.Getenv("CORRAL_FAKE_SCENARIO") // M2 scenario engine (scenario.go); empty = M1 interactive behavior.

	cwd, err := os.Getwd()
	if err != nil {
		fatalf("getwd: %v", err)
	}

	if sessionID != "" && fakeState != "" {
		inv := Invocation{
			Argv:     os.Args,
			Environ:  os.Environ(),
			Cwd:      cwd,
			Settings: settingsRawMessage(spec.Settings),
		}
		if _, err := recordInvocation(fakeState, sessionID, inv); err != nil {
			fatalf("%v", err)
		}
	}

	// Real claude disables local echo when it takes over the terminal;
	// fakeclaude does the same so the PTY's line discipline does not
	// echo raw keystrokes into the same output stream as fakeclaude's
	// own cursor-addressed rendering of them. Best-effort: not fatal if
	// stdin is not a real tty.
	if err := disableEcho(int(os.Stdin.Fd())); err != nil {
		fatalf("%v", err)
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	if err := writeStartup(out, spec.Name); err != nil {
		fatalf("%v", err)
	}

	row := transcriptStartRow
	if spec.Resume != "" && fakeHome != "" {
		prior, err := loadTranscript(fakeHome, cwd, sessionID)
		if err != nil {
			fatalf("%v", err)
		}
		for _, e := range prior {
			if err := writeTranscriptLine(out, row, e.Text); err != nil {
				fatalf("%v", err)
			}
			row++
		}
	}
	if err := out.Flush(); err != nil {
		fatalf("flush: %v", err)
	}

	// CORRAL_FAKE_BURST=<n> writes n filler bytes in a single Write right
	// after startup — a deliberately oversized, non-differential blast of
	// output step 10's TestSlowClientIsDropped uses to make a subscriber
	// fall behind its bounded channel fast enough to exercise the
	// overflow/client_too_slow path without a slow-consuming test double.
	if b := os.Getenv("CORRAL_FAKE_BURST"); b != "" {
		n, err := strconv.Atoi(b)
		if err != nil {
			fatalf("CORRAL_FAKE_BURST=%q: %v", b, err)
		}
		if n > 0 {
			buf := make([]byte, n)
			for i := range buf {
				buf[i] = 'x'
			}
			if _, err := out.Write(buf); err != nil {
				fatalf("write burst: %v", err)
			}
			if err := out.Flush(); err != nil {
				fatalf("flush burst: %v", err)
			}
		}
	}

	// --- M2 scenario engine (design doc §9) --------------------------------
	//
	// When CORRAL_FAKE_SCENARIO is set, the scenario drives the session: it
	// owns stdin (its own wait_stdin reader) and terminates via an `exit`
	// step, so it runs *instead of* the M1 interactive select loop below —
	// which keeps every M1 test (spawned with no scenario) behaving exactly
	// as before. It runs after writeStartup/resume/burst so a scenario still
	// starts from the same on-screen state real supervision would see.
	if scenarioPath != "" {
		code := runScenario(scenarioPath, &scenarioRunner{
			out:          out,
			stdin:        os.Stdin,
			settingsPath: spec.Settings,
			sessionID:    sessionID,
			cwd:          cwd,
			fakeHome:     fakeHome,
			fakeState:    fakeState,
		})
		exitGracefully(out, code)
	}

	// --- Exit knobs (design doc §10.1 item 5; sigCh/ignoreSIGTERM are set
	// up at the very top of main, above) ------------------------------------

	exitCh := make(chan int, 1)
	if d := os.Getenv("CORRAL_FAKE_EXIT_AFTER"); d != "" {
		dur, err := time.ParseDuration(d)
		if err != nil {
			fatalf("CORRAL_FAKE_EXIT_AFTER=%q: %v", d, err)
		}
		code := 0
		if c := os.Getenv("CORRAL_FAKE_EXIT_CODE"); c != "" {
			code, err = strconv.Atoi(c)
			if err != nil {
				fatalf("CORRAL_FAKE_EXIT_CODE=%q: %v", c, err)
			}
		}
		time.AfterFunc(dur, func() { exitCh <- code })
	}

	lineCh := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
		close(lineCh)
	}()

	for {
		select {
		case <-sigCh:
			if ignoreSIGTERM {
				continue // must be SIGKILLed to die; design doc item 5.
			}
			exitGracefully(out, 0)

		case code := <-exitCh:
			exitGracefully(out, code)

		case <-sigWinch:
			rows, cols, err := pty.Getsize(os.Stdout)
			if err != nil {
				continue // not a real tty (e.g. under `go test` directly); nothing to redraw.
			}
			if err := writeBannerWidth(out, spec.Name, cols); err != nil {
				fatalf("%v", err)
			}
			if err := writeResizeMarker(out, rows, cols); err != nil {
				fatalf("%v", err)
			}
			if err := out.Flush(); err != nil {
				fatalf("flush: %v", err)
			}

		case line, ok := <-lineCh:
			if !ok {
				// stdin closed (e.g. the PTY slave side went away):
				// exit cleanly instead of spinning on a closed channel.
				exitGracefully(out, 0)
			}
			if sessionID != "" && fakeHome != "" {
				if err := appendPrompt(fakeHome, cwd, sessionID, line); err != nil {
					fatalf("%v", err)
				}
			}
			if err := writeTranscriptLine(out, row, "echo: "+line); err != nil {
				fatalf("%v", err)
			}
			row++
			if err := out.Flush(); err != nil {
				fatalf("flush: %v", err)
			}
		}
	}
}

// exitGracefully leaves the alt screen before exiting, so a real
// terminal is not left in a broken state.
func exitGracefully(out *bufio.Writer, code int) {
	_, _ = io.WriteString(out, altScreenExit)
	_ = out.Flush()
	os.Exit(code)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fakeclaude: "+format+"\n", args...)
	os.Exit(1)
}
