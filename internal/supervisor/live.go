package supervisor

import (
	"os"
	"os/exec"

	"github.com/danielbecerra/corral/internal/claude/streamjson"
	"github.com/danielbecerra/corral/internal/screen"
)

// LiveSession is the supervisor's live handle to a spawned session process
// (design doc §1). checkpoint.Checkpointer only ever reads SessionID/PGID;
// everything else here is this package's own bookkeeping.
type LiveSession struct {
	// SessionID is the corral session ID (session.Session.ID).
	SessionID string
	// PGID is the process group to signal — always == PID, since Spawn
	// starts the child as its own session/group leader (see spawn.go's
	// Info.PGID doc comment).
	PGID int

	// PTYMaster is the PTY master fd for the child. Owned by the reader
	// goroutine (Feed) and the reply-pump goroutine (writes); closed once
	// by cmd.Wait()'s reaper after both have observed EOF/error.
	PTYMaster *os.File
	// Cmd is the running child process handle (os/exec, via pty.StartWithSize).
	Cmd *exec.Cmd
	// Screen is the headless terminal emulator this session's PTY output
	// feeds. Attach (step 10) subscribes to it for live streaming + reads
	// DebugGrid()/a snapshot for repaint-on-attach.
	Screen *screen.Screen

	// Attachment is the currently-attached client, if any (design doc
	// §5.5: at most one at a time). nil when nobody is attached. Guarded
	// by Registry.mu — see attach.go's installAttachment/
	// clearAttachmentIfCurrent, which are the only code that reads or
	// writes this field.
	Attachment *Attachment

	// Secret is this session's per-session hook-auth secret (Amendment:
	// CORRAL_SESSION_SECRET). In-memory ONLY — never persisted, never
	// logged, never returned by any API. The hooks HTTP handler compares an
	// inbound Corral-Session-Secret header against this value to authorize
	// a hook payload as genuinely coming from this session's own claude
	// child (via the relay).
	Secret string

	// Headless is true for a session spawned via SpawnHeadless (design doc
	// §5.2): a one-shot `claude -p --output-format stream-json` invocation
	// with no PTY. When true, both PTYMaster and Screen above are nil, and
	// WriteInput/Attach on this session return an error rather than
	// dereferencing them.
	Headless bool

	// Result is the decoded body of the terminal stream-json `result` line
	// (design doc §5.2/§8.1), stashed here the instant the headless reader
	// parses it. nil until then, and forever nil for an interactive session
	// or a headless one that never produced a result line (e.g. killed
	// mid-turn — §8.1 rule 3, "no result captured"). Guarded by
	// Registry.mu, same discipline as Attachment above: readers/writers
	// must hold it.
	Result *streamjson.Result
}
