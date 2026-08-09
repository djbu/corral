package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	xterm "github.com/charmbracelet/x/term"

	"github.com/danielbecerra/corral/internal/config"
	"github.com/danielbecerra/corral/internal/proto"
	"github.com/danielbecerra/corral/internal/version"
)

// cmdAttach implements `corral attach <name-or-id> [--no-take-over]`
// (design doc §5): dials the daemon's attach upgrade, sends Hello with the
// current terminal size, puts stdin into raw mode, and relays bytes in
// both directions until the client detaches (the §5.3 prefix-key
// sequence), the daemon evicts it (take-over), the session exits, or the
// connection is lost.
func cmdAttach(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noTakeOver := fs.Bool("no-take-over", false, "fail with already_attached instead of evicting an already-attached client")
	// addClientFlags is registered here even though attach can only ever
	// target a local daemon (design doc m5.md §7 rule 3: attach's PTY
	// hijack has no remote transport), so a stray --host parses cleanly and
	// newLocalClient below can give a clear diagnosis instead of flag's
	// generic "flag provided but not defined" error.
	cf := addClientFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: corral attach <name-or-id> [--no-take-over]")
		return exitUsage
	}
	idOrName := fs.Arg(0)

	wd, _ := os.Getwd()
	_, attachCfg, _, _, err := config.LoadSession(wd, nil)
	if err != nil {
		fmt.Fprintf(stderr, "corral: attach: %v\n", err)
		return exitError
	}
	prefixByte, err := parseKeySpec(attachCfg.PrefixKey)
	if err != nil {
		fmt.Fprintf(stderr, "corral: attach: %v\n", err)
		return exitError
	}
	detachByte, err := parseKeySpec(attachCfg.DetachKey)
	if err != nil {
		fmt.Fprintf(stderr, "corral: attach: %v\n", err)
		return exitError
	}

	c, err := newLocalClient(cf, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "corral: attach: %v\n", err)
		return exitError
	}
	conn, br, err := c.Attach(context.Background(), idOrName)
	if err != nil {
		fmt.Fprintf(stderr, "corral: attach: %v\n", err)
		return exitError
	}
	defer conn.Close()
	fw := &frameWriter{conn: conn}

	stdinFd, stdoutFd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	rows, cols := 40, 120
	if w, h, err := xterm.GetSize(uintptr(stdoutFd)); err == nil && w > 0 && h > 0 {
		rows, cols = h, w
	}

	hello := proto.Hello{Rows: rows, Cols: cols, TakeOver: !*noTakeOver, ClientVersion: version.Version}
	if err := fw.write(proto.Frame{Type: proto.TypeHello, Payload: mustEncodeAttach(hello)}); err != nil {
		fmt.Fprintf(stderr, "corral: attach: sending hello: %v\n", err)
		return exitError
	}

	// Raw mode is entered only if stdin is actually a terminal — running
	// under a test harness or a pipe must not fail attach outright, it
	// just means no local echo/line-editing suppression is needed because
	// there is no real terminal to suppress it on.
	if xterm.IsTerminal(uintptr(stdinFd)) {
		oldState, err := xterm.MakeRaw(uintptr(stdinFd))
		if err != nil {
			fmt.Fprintf(stderr, "corral: attach: entering raw mode: %v\n", err)
			return exitError
		}
		// A plain defer already runs during a panic's unwind — the only
		// way raw mode would leak past this function is if something
		// downstream called os.Exit directly, which nothing in this
		// package's attach path does; the exit code is always returned up
		// to main(), which is the only os.Exit call in the whole program.
		defer func() { _ = xterm.Restore(uintptr(stdinFd), oldState) }()
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	stopWinch := make(chan struct{})
	defer close(stopWinch)
	go func() {
		for {
			select {
			case <-stopWinch:
				return
			case <-winch:
				if w, h, err := xterm.GetSize(uintptr(stdoutFd)); err == nil && w > 0 && h > 0 {
					_ = fw.write(proto.Frame{Type: proto.TypeResize, Payload: mustEncodeAttach(proto.Resize{Rows: h, Cols: w})})
				}
			}
		}
	}()

	type outcome struct {
		stdout string
		errMsg string
		code   int
	}
	resultCh := make(chan outcome, 2)
	ackCh := make(chan struct{}, 1)

	// Frames from the daemon.
	go func() {
		for {
			f, err := proto.ReadFrame(br)
			if err != nil {
				select {
				case resultCh <- outcome{errMsg: fmt.Sprintf("connection closed: %v", err), code: exitError}:
				default:
				}
				return
			}
			if !proto.IsKnown(f.Type) {
				continue
			}
			switch f.Type {
			case proto.TypeOutput:
				_, _ = stdout.Write(f.Payload)
			case proto.TypePing:
				_ = fw.write(proto.Frame{Type: proto.TypePong, Payload: f.Payload})
			case proto.TypeGoodbye:
				select {
				case ackCh <- struct{}{}:
				default:
				}
			case proto.TypeDetached:
				var d proto.Detached
				_ = proto.DecodeJSON(f.Payload, &d)
				select {
				case resultCh <- outcome{stdout: fmt.Sprintf("[detached: %s]\n", d.Reason), code: exitOK}:
				default:
				}
				return
			case proto.TypeError:
				var e proto.ErrorPayload
				_ = proto.DecodeJSON(f.Payload, &e)
				select {
				case resultCh <- outcome{errMsg: fmt.Sprintf("%s: %s", e.Code, e.Message), code: exitError}:
				default:
				}
				return
			case proto.TypeExit:
				var ex proto.Exit
				_ = proto.DecodeJSON(f.Payload, &ex)
				msg := "[session exited"
				switch {
				case ex.Signal != "":
					msg += ", signal=" + ex.Signal
				case ex.ExitCode != nil:
					msg += fmt.Sprintf(", exit_code=%d", *ex.ExitCode)
				}
				msg += "]\n"
				select {
				case resultCh <- outcome{stdout: msg, code: exitOK}:
				default:
				}
				return
			}
		}
	}()

	// Stdin, through the §5.3 prefix-key detach state machine.
	go func() {
		pm := &prefixMachine{prefixByte: prefixByte, detachByte: detachByte}
		buf := make([]byte, 4096)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				var forward []byte
				detachNow := false
				for _, b := range buf[:n] {
					fwd, detach := pm.feed(b)
					forward = append(forward, fwd...)
					if detach {
						detachNow = true
						break
					}
				}
				if len(forward) > 0 {
					_ = fw.write(proto.Frame{Type: proto.TypeInput, Payload: forward})
				}
				if detachNow {
					_ = fw.write(proto.Frame{Type: proto.TypeGoodbyeClose, Payload: mustEncodeAttach(proto.GoodbyeClose{Reason: "detach"})})
					select {
					case <-ackCh:
					case <-time.After(2 * time.Second):
					}
					select {
					case resultCh <- outcome{stdout: "[detached]\n", code: exitOK}:
					default:
					}
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	result := <-resultCh
	if result.stdout != "" {
		fmt.Fprint(stdout, result.stdout)
	}
	if result.errMsg != "" {
		fmt.Fprintf(stderr, "corral: attach: %s\n", result.errMsg)
	}
	return result.code
}

// frameWriter serializes writes to conn: the daemon-frame reader goroutine
// (Pong replies), the SIGWINCH goroutine (Resize), and the stdin goroutine
// (Input/Goodbye) all write concurrently once attach is underway, and two
// interleaved proto.WriteFrame calls on the same net.Conn would corrupt
// framing mid-header — the same reasoning as attach.go's Attachment.writeMu
// on the daemon side.
type frameWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (fw *frameWriter) write(f proto.Frame) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return proto.WriteFrame(fw.conn, f)
}

// prefixMachine implements design doc §5.3's four-transition table: a
// tmux-style two-byte detach sequence rather than a bare control byte, so
// that byte can still reach the child in the normal (non-prefixed) case.
//
//   - !pending && b == prefix  -> swallow, become pending.
//   - pending  && b == detach  -> detach (no bytes forwarded).
//   - pending  && b == prefix  -> forward one literal prefix byte.
//   - pending  && b == other   -> forward the prefix byte, then b.
//
// pending always expires after exactly one more byte, regardless of which
// case above it took.
type prefixMachine struct {
	prefixByte byte
	detachByte byte
	pending    bool
}

// feed processes one byte and returns the bytes (if any) to forward to the
// PTY, plus whether this byte triggered a detach.
func (m *prefixMachine) feed(b byte) (forward []byte, detach bool) {
	if !m.pending {
		if b == m.prefixByte {
			m.pending = true
			return nil, false
		}
		return []byte{b}, false
	}
	m.pending = false
	switch b {
	case m.detachByte:
		return nil, true
	case m.prefixByte:
		return []byte{m.prefixByte}, false
	default:
		return []byte{m.prefixByte, b}, false
	}
}

// parseKeySpec parses a config.Attach.PrefixKey/DetachKey value: either a
// literal single character ("d") or a "C-x" control-key spec (`C-\`,
// mapped to its ASCII control code by masking to the low 5 bits, the
// standard terminal control-character encoding).
func parseKeySpec(s string) (byte, error) {
	if strings.HasPrefix(s, "C-") && len(s) == 3 {
		return s[2] & 0x1f, nil
	}
	if len(s) == 1 {
		return s[0], nil
	}
	return 0, fmt.Errorf("cmd_attach: unsupported key spec %q (want a single character or \"C-x\")", s)
}

// mustEncodeAttach marshals v for a control frame payload. See
// supervisor.mustEncode's doc comment for why a JSON-marshal failure on one
// of this package's own proto structs isn't realistically reachable.
func mustEncodeAttach(v any) []byte {
	b, err := proto.EncodeJSON(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
