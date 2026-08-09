package api

import (
	"context"
	"errors"

	"github.com/djbu/corral/internal/store"
)

// errForbiddenScope is resolveSession's sentinel for "the request's token
// resolved a real session, but its scope forbids acting on it" (m5.md §10).
// Distinct from store.ErrNotFound so callers can tell "doesn't exist" (404)
// apart from "exists, but you can't touch it" (403) — resolveSession looks
// the session up before checking scope specifically so this distinction is
// preserved rather than papered over.
var errForbiddenScope = errors.New("api: token scope forbids this session")

// sessionSubtree returns the set of session ids a scope='session' token
// rooted at rootSessionID may act on: the root plus its task_deps
// descendants' sessions within the same dag_id (m5.md §10). No new graph
// state is introduced — this is a plain forward-walk fixpoint over the
// existing task_deps edges.
//
// If the root session has no task (a standalone `corral new` session), the
// subtree is just {rootSessionID}.
func sessionSubtree(ctx context.Context, st *store.Store, rootSessionID string) (map[string]struct{}, error) {
	subtree := map[string]struct{}{rootSessionID: {}}

	rootTask, err := st.GetTaskBySessionID(ctx, rootSessionID)
	if errors.Is(err, store.ErrNotFound) {
		return subtree, nil
	}
	if err != nil {
		return nil, err
	}

	deps, err := st.TaskDeps(ctx, rootTask.DAGID)
	if err != nil {
		return nil, err
	}
	tasks, err := st.ListTasks(ctx, rootTask.DAGID)
	if err != nil {
		return nil, err
	}
	sessionByTask := make(map[string]string, len(tasks))
	for _, t := range tasks {
		sessionByTask[t.ID] = t.SessionID
	}

	// Grow the set of descendant task ids starting from rootTask.ID: a
	// Dep{TaskID, DependsOn} edge means TaskID depends-on DependsOn, so
	// TaskID is a descendant of DependsOn. Repeatedly add any Dep.TaskID
	// whose Dep.DependsOn is already in the set, until a full pass adds
	// nothing (fixpoint) — task_deps is a DAG (m4.md), so this always
	// terminates.
	descendantTasks := map[string]struct{}{rootTask.ID: {}}
	for {
		grew := false
		for _, d := range deps {
			if _, already := descendantTasks[d.TaskID]; already {
				continue
			}
			if _, rooted := descendantTasks[d.DependsOn]; rooted {
				descendantTasks[d.TaskID] = struct{}{}
				grew = true
			}
		}
		if !grew {
			break
		}
	}

	for taskID := range descendantTasks {
		sessID := sessionByTask[taskID]
		if sessID == "" {
			// A task with no live session (not yet spawned, or spawned and
			// long gone) contributes nothing — an empty session id must
			// never enter the subtree, since callers treat "" specially
			// (e.g. handlers_events.go's daemon-scoped frames).
			continue
		}
		subtree[sessID] = struct{}{}
	}

	return subtree, nil
}

// tokenAllowsSession reports whether the request context's token (if any)
// may act on the given session id. Absent token (unix socket) => true.
// scope=admin => true. scope=session => true iff sessionID is in the
// token's subtree (m5.md §10).
func tokenAllowsSession(ctx context.Context, st *store.Store, sessionID string) (bool, error) {
	row, ok := TokenFromContext(ctx)
	if !ok || row.Scope != "session" {
		return true, nil
	}

	subtree, err := sessionSubtree(ctx, st, row.SessionID)
	if err != nil {
		return false, err
	}
	_, allowed := subtree[sessionID]
	return allowed, nil
}

// tokenDAGScope returns (allowedDagID, isScoped): for a scope='session'
// token, allowedDagID is the dag_id of the root session's task ("" if the
// root has no task), and isScoped is true. For admin/absent tokens,
// isScoped is false (no DAG confinement). Used by the DAG handlers and
// dagSummaries' list filter.
func tokenDAGScope(ctx context.Context, st *store.Store) (allowedDagID string, isScoped bool, err error) {
	row, ok := TokenFromContext(ctx)
	if !ok || row.Scope != "session" {
		return "", false, nil
	}

	rootTask, err := st.GetTaskBySessionID(ctx, row.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		return "", true, nil
	}
	if err != nil {
		return "", true, err
	}
	return rootTask.DAGID, true, nil
}
