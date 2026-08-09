// Package orchestrator drives task DAGs to completion: it launches headless
// claude sessions for ready tasks, tracks their outcomes, retries failures
// with backoff, enforces a per-task timeout, and reconciles state left
// behind by a daemon restart (design doc m4.md §8, §9, step 21).
package orchestrator

import (
	"fmt"
	"sort"

	"github.com/danielbecerra/corral/internal/store"
)

// DetectCycle reports whether tasks+deps (deps expected to already be
// scoped to the same dag as tasks, e.g. store.TaskDeps(dagID)'s output)
// contain a dependency cycle, via Kahn's algorithm: repeatedly remove tasks
// with no unresolved dependency; if any task is never removed, it is part
// of (or depends on) a cycle.
//
// Kept pure (no I/O, no clock) so the executor's per-tick safety check
// (m4.md §8.1: log and skip a cyclic dag rather than spin forever trying to
// find a ready task in one) and this package's own tests can both use it
// against a plain in-memory task/dep slice.
//
// A self-loop (a task depending on itself) and any longer cycle are both
// caught the same way: Kahn's algorithm never reduces such a task's
// in-degree to zero, so it is never dequeued.
func DetectCycle(tasks []*store.Task, deps []store.Dep) error {
	known := make(map[string]*store.Task, len(tasks))
	for _, t := range tasks {
		known[t.ID] = t
	}

	indegree := make(map[string]int, len(tasks))
	successors := make(map[string][]string, len(tasks))
	for _, t := range tasks {
		indegree[t.ID] = 0
	}
	for _, d := range deps {
		// A dep edge naming a task outside this set is ignored rather than
		// erroring: callers are expected to pass deps already scoped to
		// tasks (store.TaskDeps(dagID) joins through tasks WHERE dag_id =
		// ?), so this is defense against a mismatched-slice caller, not an
		// expected path.
		if _, ok := known[d.TaskID]; !ok {
			continue
		}
		if _, ok := known[d.DependsOn]; !ok {
			continue
		}
		successors[d.DependsOn] = append(successors[d.DependsOn], d.TaskID)
		indegree[d.TaskID]++
	}

	var queue []string
	for id, deg := range indegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	sort.Strings(queue) // deterministic starting order; does not affect correctness

	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range successors[id] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if visited == len(tasks) {
		return nil
	}

	var stuck []string
	for id, deg := range indegree {
		if deg > 0 {
			stuck = append(stuck, id)
		}
	}
	sort.Strings(stuck)
	name := stuck[0]
	if t, ok := known[name]; ok {
		name = fmt.Sprintf("%s (%s)", t.Name, t.ID)
	}
	return fmt.Errorf("orchestrator: dependency cycle detected involving task %s", name)
}
