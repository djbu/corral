package store

import (
	"context"
	"fmt"

	"github.com/danielbecerra/corral/internal/session"
)

// PermissionHistory groups the immutable permission lifecycle events for one
// session with the paths needed to assign canonical repository scope.
type PermissionHistory struct {
	SessionID string
	Cwd       string
	TaskRepo  string
	Events    []session.Event
}

// ListPermissionHistory returns only the four event kinds required by M6's
// manual-approval contract in the half-open event-time window [start,end).
func (s *Store) ListPermissionHistory(ctx context.Context, startMs, endMs int64) ([]PermissionHistory, error) {
	if endMs <= startMs {
		return nil, fmt.Errorf("store: invalid permission history window")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id,s.cwd,COALESCE((
			SELECT t.repo FROM tasks t WHERE t.session_id=s.id
			ORDER BY t.updated_ms DESC,t.id ASC LIMIT 1
		),''),e.seq,e.ts_ms,e.kind,e.data_json
		FROM sessions s JOIN events e ON e.session_id=s.id
		WHERE e.ts_ms>=? AND e.ts_ms<? AND e.kind IN (?,?,?,?)
		ORDER BY s.id,e.seq`, startMs, endMs,
		string(session.EventPermissionRequested), string(session.EventPermissionBlocked),
		string(session.EventSessionAnswered), string(session.EventPermissionResolved))
	if err != nil {
		return nil, fmt.Errorf("store: listing permission history: %w", err)
	}
	defer rows.Close()

	var out []PermissionHistory
	for rows.Next() {
		var sid, cwd, taskRepo, kind, data string
		var seq, ts int64
		if err := rows.Scan(&sid, &cwd, &taskRepo, &seq, &ts, &kind, &data); err != nil {
			return nil, fmt.Errorf("store: scanning permission history: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].SessionID != sid {
			out = append(out, PermissionHistory{SessionID: sid, Cwd: cwd, TaskRepo: taskRepo})
		}
		out[len(out)-1].Events = append(out[len(out)-1].Events, session.Event{
			Seq: seq, SessionID: sid, TsMs: ts, Kind: session.EventKind(kind), DataJSON: data,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing permission history: %w", err)
	}
	return out, nil
}
