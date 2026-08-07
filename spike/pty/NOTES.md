# corral PTY spike — findings

Code is throwaway; this file is the deliverable. Everything below was
observed empirically on macOS 15 (darwin/arm64), Go 1.26.4, using
`github.com/creack/pty` v1.1.24, against `bash` (macOS system bash 3.2),
`vi`, and `claude` (Claude Code CLI 2.1.224, `--model haiku`).

Raw evidence: `demo-output.txt` (bash mechanics run), `claude-tui-capture.txt`
(raw PTY bytes from the claude run), `claude-demo-report.txt` (pass/fail
log for the claude run).

## Result summary

| # | Requirement | Result |
|---|---|---|
| 1 | Daemon detaches (setsid, no controlling tty), survives launcher exit | **PASS** |
| 2 | Client attach over unix socket, raw stdin → PTY, PTY → stdout, interactive | **PASS** (mechanics proven; real raw-mode/tty behavior not exercisable in this sandboxed environment — see below) |
| 3 | Detach without killing child; reattach; TUI state visible again | **PASS**, with a major caveat for alt-screen apps (see finding #1) |
| 4 | Resize propagates via TIOCSWINSZ, TUI redraws | **PASS** |
| 5 | Scrollback/replay buffer; note what breaks for alt-screen apps | **PASS** (bash) / **BREAKS** (vi, claude) — documented in detail below |

## Top 5 findings

### 1. Naive "replay last N raw bytes" corrupts alt-screen / relative-redraw TUIs. This is the big one.

The scrollback buffer in this spike is a plain ring buffer of PTY output
bytes: `Feed()` appends and evicts from the front once the byte cap is
exceeded. That's fine for `bash`, whose output is basically append-only
plain text. It is **not fine** for `vi` or `claude`, both of which:

- switch to the alternate screen buffer (`\x1b[?1049h`) once, near startup,
- and thereafter emit cursor-addressed, differential updates
  (`\x1b[R;CH<text>`, `\x1b[K`, SGR color codes) that only make sense given
  the exact prior screen state and cursor/attribute state the app has been
  tracking client-side since the alt-screen was entered.

If the ring buffer's cap is smaller than the total output since alt-screen
entry (or the app simply runs long enough), the alt-screen-enter sequence
and any SGR/color state set early on ages out of the buffer. Reattach then
replays a byte stream that:

- **starts mid-multi-byte-UTF-8-character.** Confirmed directly:
  `claude-tui-capture.txt`'s replay section starts with byte `0x80` — a lone
  UTF-8 continuation byte, invalid as a leading byte on its own. It's the
  tail half of a box-drawing `─` (U+2500, `\xe2\x94\x80`) character that got
  cut in half by the ring buffer's byte-oriented (not char-oriented)
  eviction. A real terminal would render this as `�` and then resync a few
  bytes later.
- **starts mid-ANSI-escape-sequence.** Confirmed directly with `vi`: reattach
  replay began with the bytes `;1H11\x1b[11;3H...` — the tail of a
  `\x1b[11;1H` cursor-position sequence with the leading `\x1b[1` chopped
  off. A real terminal would print the literal characters `;1H11` onto the
  screen at whatever the cursor's last position was, then recover once a
  complete sequence arrives.
- **never replays the alt-screen-enter itself**, so the client's terminal
  emulator is left in whatever mode it was already in (if it's a fresh
  terminal, it's in the *primary* screen, not the alternate one the app
  thinks it owns) and is missing SGR/color state that was set before the
  truncation point (borders that should be orange render in default
  foreground color instead, etc. — visible qualitatively in the
  stripped-text dump of `claude-tui-capture.txt`, where a stray `\x1b[>0q`
  fragment and missing color prefixes appear right after the boundary).

For `bash` this class of bug is invisible because plain text has no
persistent parse state across a truncation point — worst case you lose the
first partial line, which is what `demo-output.txt`'s reattach check shows
(`marker=false spew=true`: the `hello-from-pty` echo aged out of a
deliberately tiny 4096-byte cap, but the tail content is still perfectly
readable). **Do not let bash-shaped testing convince you replay is solved.**
It is a different, much easier problem than replaying for a differential
TUI.

**Implication for corral:** raw-byte ring-buffer replay is not viable for
the real target (claude's TUI is alt-screen + differential). Real
solutions, in increasing order of effort: (a) never evict mid-multibyte-char
or mid-escape-sequence (cheap, still leaves the "missing SGR/mode state"
problem), (b) snapshot-and-diff: on evicting past the alt-screen-enter
marker, remember it and always replay `\x1b[?1049h\x1b[2J\x1b[H` first, then
whatever tail bytes remain (better, still doesn't reconstruct exact
content), (c) the tmux/screen answer: maintain a real headless terminal
emulator (a VT100/xterm state machine, e.g. a Go port of libvterm) server-side
that tracks the actual screen grid + cursor + SGR state, and on attach
*paint from the grid state*, not from raw byte history. (c) is what a real
corral daemon needs; it is a materially bigger build than this spike.

### 2. `pty.StartWithSize` (creack/pty) already does the hard part of PTY ownership for the *child*; the *daemon* still needs its own explicit setsid re-exec.

`pty.StartWithSize` opens a pty pair and sets `Setsid: true, Setctty: true`
on the child's `SysProcAttr` for you, so the spawned child becomes a new
session leader with the new pty as its controlling terminal — that part is
one function call. It does **not** detach the calling process itself. If
the daemon logic runs directly under the terminal that launched it, that
terminal remaining open/closing still matters to the daemon's own session
(SIGHUP risk on close, and it's not really a background daemon).

This spike's `daemon` subcommand re-execs itself as `_run` with
`SysProcAttr{Setsid: true}` and stdio redirected to `/dev/null`/a log file,
then the launcher process exits once it reads an "OK" readiness signal over
a pipe (`ExtraFiles`). This two-hop shape (launcher exec's a Setsid'd child,
then exits) is necessary because Go can't safely `fork()` without `exec()`
(the runtime has multiple OS threads) — there's no single-process
"daemonize in place" available the way there is in C. Confirmed working:
`demo-output.txt` shows `daemon-detached-and-running: pid=71550 alive after
launcher exited` — the `daemon` command that launched it had already
exited by the time this check ran.

Verified `Getsid(0) == Getpid()` inside `_run` (printed to the daemon log),
which is the actual proof of "new session, no controlling terminal" — worth
keeping as a startup self-check in the real daemon, since a failed setsid
silently degrades to "acts fine until the terminal closes."

### 3. The child inherits the daemon's *entire* environment — including the orchestrator's own Claude Code env vars — and that visibly changes claude's behavior.

`_run` spawns the child with `c.Env = os.Environ()`, i.e. whatever
environment the daemon process has. Because I ran this spike from inside a
Claude Code session, that environment already had things like
`CLAUDE_CODE_CHILD_SESSION` set, and the spawned `claude --model haiku`
picked it up and changed behavior accordingly: its own TUI banner printed
`⚠ Transcript saving is off — inherited CLAUDE_CODE_CHILD_SESSION marker`
and `[CAVEMAN]` / `⏸ manual mode on` banners from this environment's shell
plugins, none of which are things a supervised session should silently
inherit. See `claude-tui-capture.txt` bytes ~9700-9820.

**Implication:** a real corral daemon must construct an explicit, minimal
environment for spawned sessions rather than `os.Environ()`-passthrough,
or every supervised session will silently inherit whatever agent/session
context happened to be present when the daemon itself was started — which
is exactly the kind of thing that's invisible until it isn't.

### 4. No directory-trust prompt appeared; TUI startup is otherwise heavy on terminal capability negotiation.

Expected to see a "do you trust the files in this folder" interactive
prompt on first run in a brand-new `sandbox/` directory; it never appeared
in the raw capture. (Possibly gated on account/global state rather than
per-directory in this version, or the prompt logic short-circuits when
`CLAUDE_CODE_CHILD_SESSION` is set — unconfirmed, worth checking against
claude's source/changelog if this matters for corral's UX.) Actual startup
sequence observed, in order:
`\x1b7` (save cursor) `\x1b[r` (reset scroll region) `\x1b8` (restore
cursor) `\x1b[?25h` (show cursor) `\x1b[?1049h` (**alt screen**) `\x1b[2J`
(clear) `\x1b[H` (home) `\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h`
(mouse tracking, several modes/encodings) `\x1b[?25l` (hide cursor)
`\x1b[?2004h` (**bracketed paste**) `\x1b[?1004h` (focus-in/out reporting)
`\x1b[?2031h` (an unusual one — DEC private mode 2031 is the newer
"in-band window resize notification" mode some terminals support) then an
OSC `\x1b]0;✳ Claude Code\x07` window-title set. No cursor-position-report
query (`\x1b[6n`) and no OSC background/foreground color queries were seen
— contrast with `vi`, which did query cursor position (`\x1b[6n`) and
terminal background/foreground color (`\x1b]11;?` / `\x1b]10;?`) on startup.
A real corral daemon that wants to answer those queries transparently
(so the child doesn't hang waiting for a response the daemon-side PTY
master never sends back) needs to either forward color/cursor queries to
whatever's actually attached, or fabricate plausible answers — untested
here, but `vi`'s queries went unanswered in this spike (no client was
faking terminal responses) and it didn't visibly hang, so it's evidently
tolerant of no reply within the observed timeframe. Don't assume that's
universal.

### 5. Detach-by-local-intercept (Ctrl-`\`, byte `0x1c`) works cleanly and needs no protocol support from the daemon at all — but that's also its limitation.

Detach is implemented entirely in the **client**: the `attach` client scans
its own stdin stream for byte `0x1c` and, on finding it, stops forwarding
and returns without sending anything to the daemon. The daemon just sees
the socket connection close (EOF on its read loop) and clears the
"currently attached" pointer while leaving the child + PTY completely
untouched. Confirmed via both the scripted demo (`detach-without-kill: PASS`
in both `demo-output.txt` and `claude-demo-report.txt`) and the ad hoc `vi`
test (`pgrep` showed the daemon and vi still running after a Ctrl-`\`
detach). This is simple and robust — there is no server-side "detach"
concept to get wrong — but it means the daemon can never distinguish "the
client detached on purpose" from "the client's local process crashed" or
"the network blipped": both look identical (socket closed). It also means
byte `0x1c` can never reach the child app; if a real target app binds
Ctrl-`\`, corral needs a different/configurable escape (tmux solves this
with a prefix-key sequence, not a single raw byte — worth adopting that
shape rather than a bare control byte, precisely because bare control
bytes collide with app keybindings).

## Other things worth knowing before building on this

- **macOS system `bash` (3.2, from 2007) prints a one-time zsh-migration
  warning banner on interactive startup** (`The default interactive shell
  is now zsh...`) and also emits `\x1b[?1034h` (an xterm "metaSendsEscape"
  related mode, largely a legacy annoyance) — visible in
  `demo-output.txt`'s initial-prompt capture. Harmless, but it's PTY output
  a naive "wait for a prompt" heuristic would have to skip past; don't test
  session-readiness heuristics against macOS's stock `bash` and assume
  they'll look the same on Linux/a real target shell.
- **Killing the daemon's own process (SIGTERM to the `_run` pid) is
  sufficient to reap the child cleanly**, because the child was started via
  `pty.StartWithSize` with `Setsid: true`, making it leader of its own new
  process group (`pgid == pid`). The daemon's shutdown path does
  `syscall.Kill(-childPid, SIGTERM)` then, after a grace period,
  `SIGKILL` on the same negative pgid. Verified zero orphans after both
  demo runs and the ad hoc `vi` run via `pgrep`. **Caveat**: this spike
  scopes all cleanup/orphan checks to pids it explicitly tracked (the
  daemon's own pid and its direct child, found via `pgrep -P <daemonpid>`)
  rather than pattern-matching on `claude` or `bash` by name — this
  machine already runs several unrelated `claude` processes (other Claude
  Code sessions) and a name/path-based pattern match would have been
  dangerous. Any future tooling built on this should keep doing pid-based
  scoping, never name-based.
- **Resize is a plain `ioctl(TIOCSWINSZ)` on the PTY master** via
  `pty.Setsize`, applied both on initial attach (client sends its current
  size) and on live resize (client's `SIGWINCH` handler re-queries and
  sends). Verified two ways: deterministically with `bash` (`stty size`
  printed `40 100` immediately after a resize frame, see
  `demo-output.txt`), and qualitatively with `claude` (the box-drawing
  border width visibly changed between an 80-wide and a 100-wide render in
  `claude-tui-capture.txt`, and `Run /init to create a CLAUDE.md file with
  instru…` wraps differently at the wider size) — TUI redraw on resize
  works for both a dumb shell and a real ink-style TUI without any special
  handling beyond the ioctl.
- **The wire protocol used here is deliberately asymmetric**: client→daemon
  is framed (`[1 byte type][4 byte length][payload]`, types `A`ttach,
  `D`ata, `R`esize) because that direction multiplexes keystrokes and
  control messages; daemon→client is unframed raw bytes, because it only
  ever carries one thing (PTY output, replay-then-live). Keeping the
  daemon→client path unframed made the replay-snapshot-then-subscribe
  handoff (`scrollback.Attach()`, in `protocol.go`) trivial to make
  race-free under a single mutex — there's no framing state to reconcile
  between "bytes written before the lock" and "bytes written after."
- **No real controlling terminal was available in this environment to test
  actual raw-mode stdin behavior** (`term.MakeRaw`/`term.IsTerminal`) or
  real interactive human typing. The `attach` client code path for that
  (raw mode, `SIGWINCH` on a real tty) is implemented and unexercised by
  the automated demos, which instead speak the same wire protocol directly
  over the socket (see `demo.go`) for deterministic, scriptable
  assertions. This is a real gap: raw-mode edge cases (e.g. what happens
  to `ICANON`/`ISIG` state on the *client's* terminal if `attach` panics
  and skips `term.Restore`) are unverified.

## Process/environment notes specific to this spike

- Unix socket paths used absolute paths under `spike/pty/run/`
  (well under macOS's ~104-char `sockaddr_un` limit).
- One prompt was sent to the live `claude --model haiku` session
  (`reply only OK`), and it answered `OK` (visible as `⏺ OK` in
  `claude-tui-capture.txt`, Claude Code's assistant-message bullet
  followed by the reply, then a `✻ Cooked for 2s` status line). Budget of
  "at most 2 prompts total" was not otherwise used.
- All processes spawned during this spike (2× daemon+bash runs, 1×
  daemon+vi run, 1× daemon+claude run) were explicitly torn down and
  verified gone via `pgrep`/`pgrep -P` before finishing; no `claude` or
  `corral-spike-pty` processes were left running.
