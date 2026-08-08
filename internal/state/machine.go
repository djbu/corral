package state

// machine.go implements M2's pure hook-event classification (design doc
// §2, Amendment A.1-A.4): the 15-event transition classifier. pending.go
// holds the per-session pending-tool tracker step 6b's ingest goroutine
// owns. engine.go (untouched by this file) holds the seam from M1.

import "github.com/danielbecerra/corral/internal/hookrelay"

// Target is the agent-state a transition drives toward. TargetNone means
// the event is record-only and drives no state change. These are named
// symbolically here rather than importing session.AgentState directly:
// machine.go only classifies, it never sets state itself — step 6b maps
// Target to the real session.AgentState (AgentWorking/AgentIdle/etc.)
// when it acts on a Transition.
type Target int

const (
	TargetNone    Target = iota // record-only (Notification, and unknown events)
	TargetIdle                  // SessionStart, Stop
	TargetWorking               // UserPromptSubmit, PreToolUse, PostToolUse, SubagentStart, PreCompact, etc.
)

// PermOp is the permission-request lifecycle effect an event carries. The
// timer/state machine that acts on these lives in step 6b's ingest
// goroutine; machine.go only names the effect.
type PermOp int

const (
	PermNone              PermOp = iota
	PermOpen                     // PermissionRequest: open a request, arm settle timer
	PermResolveApproved          // PostToolUse: resolve matching request approved
	PermResolveFailed            // PostToolUseFailure: resolve matching request failed_or_denied
	PermResolveTurnEnded         // Stop: resolve all requests for this prompt_id
	PermResolveSuperseded        // UserPromptSubmit: resolve all open requests superseded
)

// Transition is the pure classification of a single hook event. Classify
// does not mutate anything and does not itself manage the permission
// lifecycle or the pending-tool tracker — it only classifies. Step 6b
// consumes a Transition and acts on it (updating session.AgentState,
// PendingTracker, and the permission-request timers).
type Transition struct {
	Target Target
	// Reset is true for SessionStart: clear pending-tool tracker and
	// blocked reason.
	Reset bool
	// OpensTool is true for PreToolUse: add an entry to the pending-tool
	// tracker.
	OpensTool bool
	// ClosesTool is true for PostToolUse / PostToolUseFailure: close the
	// matching tracker entry.
	ClosesTool bool
	// Perm is the permission-request lifecycle effect (see PermOp).
	Perm PermOp
	// Heartbeat is true when the event should bump last_hook_at_ms as
	// proof of life. Every recognized event is a heartbeat; unknown
	// events are heartbeats too (proof of life) but Unknown=true.
	Heartbeat bool
	// RecordOnly true means: persist the event, bump heartbeat, drive no
	// transition (Notification, SessionEnd, TeammateIdle, and unknown
	// events).
	RecordOnly bool
	// Unknown true means hook_event_name was not one of the 15 registered
	// events (a future CLI's event #16).
	Unknown bool
}

// Classify maps a decoded hook payload to its Transition (Amendment A.1
// table). It is a pure function: no I/O, no timers, no mutation of ev or
// any shared state.
//
// Amendment A.1.2 note: Stop with StopHookActive==true (a user's merged
// blocking Stop hook forced continuation) is classified IDENTICALLY to a
// normal Stop (→ idle, PermResolveTurnEnded). StopHookActive does not
// change the Transition; the next PreToolUse re-enters working on its own.
func Classify(ev hookrelay.HookPayload) Transition {
	switch ev.HookEventName {
	case "SessionStart":
		return Transition{Target: TargetIdle, Reset: true, Heartbeat: true}
	case "UserPromptSubmit":
		return Transition{Target: TargetWorking, Perm: PermResolveSuperseded, Heartbeat: true}
	case "PreToolUse":
		return Transition{Target: TargetWorking, OpensTool: true, Heartbeat: true}
	case "PermissionRequest":
		// Does NOT itself set working — a PreToolUse already did. Carries
		// no ToolUseID (Amendment A.3.1); the join key used in 6b is
		// (prompt_id, tool_name, tool_input).
		return Transition{Target: TargetNone, Perm: PermOpen, Heartbeat: true}
	case "PostToolUse":
		return Transition{Target: TargetWorking, ClosesTool: true, Perm: PermResolveApproved, Heartbeat: true}
	case "PostToolUseFailure":
		return Transition{Target: TargetWorking, ClosesTool: true, Perm: PermResolveFailed, Heartbeat: true}
	case "Notification":
		return Transition{Target: TargetNone, RecordOnly: true, Heartbeat: true}
	case "Stop":
		return Transition{Target: TargetIdle, Perm: PermResolveTurnEnded, Heartbeat: true}
	case "StopFailure":
		// Treated like Stop (see note above): idle, resolve all requests
		// for this prompt_id.
		return Transition{Target: TargetIdle, Perm: PermResolveTurnEnded, Heartbeat: true}
	case "SubagentStart":
		return Transition{Target: TargetWorking, Heartbeat: true}
	case "SubagentStop":
		// Subagent finishing != session idle; the main agent continues.
		return Transition{Target: TargetWorking, Heartbeat: true}
	case "PreCompact":
		return Transition{Target: TargetWorking, Heartbeat: true}
	case "SessionEnd":
		return Transition{Target: TargetNone, RecordOnly: true, Heartbeat: true}
	case "TeammateIdle":
		return Transition{Target: TargetNone, RecordOnly: true, Heartbeat: true}
	case "PostToolBatch":
		return Transition{Target: TargetWorking, Heartbeat: true}
	default:
		// Tripwire for a future CLI's event #16: still a heartbeat (proof
		// of life), no transition.
		return Transition{Target: TargetNone, Unknown: true, Heartbeat: true, RecordOnly: true}
	}
}
