package orchestrator

import (
	"testing"

	"github.com/danielbecerra/corral/internal/store"
)

func task(id, name string) *store.Task {
	return &store.Task{ID: id, DAGID: "dag1", Name: name}
}

func TestDetectCycle(t *testing.T) {
	cases := []struct {
		name    string
		tasks   []*store.Task
		deps    []store.Dep
		wantErr bool
	}{
		{
			name:  "no tasks",
			tasks: nil,
			deps:  nil,
		},
		{
			name:  "single task, no deps",
			tasks: []*store.Task{task("a", "a")},
		},
		{
			name:  "linear chain acyclic",
			tasks: []*store.Task{task("a", "a"), task("b", "b"), task("c", "c")},
			deps: []store.Dep{
				{TaskID: "b", DependsOn: "a"},
				{TaskID: "c", DependsOn: "b"},
			},
		},
		{
			name:  "diamond acyclic",
			tasks: []*store.Task{task("a", "a"), task("b", "b"), task("c", "c"), task("d", "d")},
			deps: []store.Dep{
				{TaskID: "b", DependsOn: "a"},
				{TaskID: "c", DependsOn: "a"},
				{TaskID: "d", DependsOn: "b"},
				{TaskID: "d", DependsOn: "c"},
			},
		},
		{
			name:  "self loop",
			tasks: []*store.Task{task("a", "a")},
			deps: []store.Dep{
				{TaskID: "a", DependsOn: "a"},
			},
			wantErr: true,
		},
		{
			name:  "two node cycle",
			tasks: []*store.Task{task("a", "a"), task("b", "b")},
			deps: []store.Dep{
				{TaskID: "a", DependsOn: "b"},
				{TaskID: "b", DependsOn: "a"},
			},
			wantErr: true,
		},
		{
			name:  "three node cycle",
			tasks: []*store.Task{task("a", "a"), task("b", "b"), task("c", "c")},
			deps: []store.Dep{
				{TaskID: "b", DependsOn: "a"},
				{TaskID: "c", DependsOn: "b"},
				{TaskID: "a", DependsOn: "c"},
			},
			wantErr: true,
		},
		{
			name:  "cycle with acyclic tail attached",
			tasks: []*store.Task{task("a", "a"), task("b", "b"), task("c", "c"), task("d", "d")},
			deps: []store.Dep{
				{TaskID: "a", DependsOn: "b"},
				{TaskID: "b", DependsOn: "a"},
				{TaskID: "c", DependsOn: "a"}, // c hangs off the cycle but is not itself cyclic
				{TaskID: "d", DependsOn: "c"},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := DetectCycle(tc.tasks, tc.deps)
			if tc.wantErr && err == nil {
				t.Fatalf("DetectCycle() = nil, want a cycle error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("DetectCycle() = %v, want nil", err)
			}
		})
	}
}
