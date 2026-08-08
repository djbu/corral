package supervisor

// LiveSession is the supervisor's live handle to a spawned session process
// (design doc §1: "liveseession.go → live.go"). Step 8 defines only the
// fields checkpoint.Checkpointer needs to stop a session's process group;
// step 9 (TODO(step9)) adds the PTY master *os.File, the *exec.Cmd, the
// *screen.Screen, and the single-attachment slot, and wires Spawn to
// register/deregister instances of this type in a registry owned by this
// package.
type LiveSession struct {
	// SessionID is the corral session ID (session.Session.ID).
	SessionID string
	// PGID is the process group to signal — always == PID, since Spawn
	// starts the child as its own session/group leader (see spawn.go's
	// Info.PGID doc comment).
	PGID int
}
