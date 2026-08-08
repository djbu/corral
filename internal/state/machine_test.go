package state

import (
	"testing"

	"github.com/danielbecerra/corral/internal/hookrelay"
)

func TestClassify_AllEvents(t *testing.T) {
	cases := []struct {
		name string
		want Transition
	}{
		{"SessionStart", Transition{Target: TargetIdle, Reset: true, Heartbeat: true}},
		{"UserPromptSubmit", Transition{Target: TargetWorking, Perm: PermResolveSuperseded, Heartbeat: true}},
		{"PreToolUse", Transition{Target: TargetWorking, OpensTool: true, Heartbeat: true}},
		{"PermissionRequest", Transition{Target: TargetNone, Perm: PermOpen, Heartbeat: true}},
		{"PostToolUse", Transition{Target: TargetWorking, ClosesTool: true, Perm: PermResolveApproved, Heartbeat: true}},
		{"PostToolUseFailure", Transition{Target: TargetWorking, ClosesTool: true, Perm: PermResolveFailed, Heartbeat: true}},
		{"Notification", Transition{Target: TargetNone, RecordOnly: true, Heartbeat: true}},
		{"Stop", Transition{Target: TargetIdle, Perm: PermResolveTurnEnded, Heartbeat: true}},
		{"StopFailure", Transition{Target: TargetIdle, Perm: PermResolveTurnEnded, Heartbeat: true}},
		{"SubagentStart", Transition{Target: TargetWorking, Heartbeat: true}},
		{"SubagentStop", Transition{Target: TargetWorking, Heartbeat: true}},
		{"PreCompact", Transition{Target: TargetWorking, Heartbeat: true}},
		{"SessionEnd", Transition{Target: TargetNone, RecordOnly: true, Heartbeat: true}},
		{"TeammateIdle", Transition{Target: TargetNone, RecordOnly: true, Heartbeat: true}},
		{"PostToolBatch", Transition{Target: TargetWorking, Heartbeat: true}},
	}

	if len(cases) != 15 {
		t.Fatalf("expected 15 registered events in table, got %d", len(cases))
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: c.name}}
			got := Classify(ev)
			if got != c.want {
				t.Errorf("Classify(%q) = %+v, want %+v", c.name, got, c.want)
			}
		})
	}
}

func TestClassify_UnknownEvent(t *testing.T) {
	ev := hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "SomeFutureEventV2"}}
	got := Classify(ev)
	want := Transition{Target: TargetNone, Unknown: true, Heartbeat: true, RecordOnly: true}
	if got != want {
		t.Errorf("Classify(unknown) = %+v, want %+v", got, want)
	}
}

func TestClassify_StopHookActiveIdenticalToStop(t *testing.T) {
	active := hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "Stop"}, StopHookActive: true}
	inactive := hookrelay.HookPayload{Common: hookrelay.Common{HookEventName: "Stop"}, StopHookActive: false}

	gotActive := Classify(active)
	gotInactive := Classify(inactive)

	if gotActive != gotInactive {
		t.Errorf("Classify(Stop, StopHookActive=true) = %+v, want identical to Classify(Stop, StopHookActive=false) = %+v", gotActive, gotInactive)
	}
}
