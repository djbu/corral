package store

import (
	"context"
	"fmt"

	"github.com/djbu/corral/internal/session"
)

type SessionActivity struct {
	SessionID     string
	Cwd           string
	TaskRepo      string
	BlockedEvents int
}

func (s *Store) ListSessionActivity(ctx context.Context, startMs, endMs int64) ([]SessionActivity, error) {
	if endMs <= startMs {
		return nil, ErrInvalidLearning
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.cwd,COALESCE((
		SELECT t.repo FROM tasks t WHERE t.session_id=s.id ORDER BY t.updated_ms DESC,t.id LIMIT 1
	),''),SUM(CASE WHEN e.kind=? THEN 1 ELSE 0 END)
	FROM sessions s JOIN events e ON e.session_id=s.id
	WHERE e.ts_ms>=? AND e.ts_ms<? GROUP BY s.id,s.cwd ORDER BY s.id`,
		string(session.EventPermissionBlocked), startMs, endMs)
	if err != nil {
		return nil, fmt.Errorf("store: listing session activity: %w", err)
	}
	defer rows.Close()
	var out []SessionActivity
	for rows.Next() {
		var row SessionActivity
		if err := rows.Scan(&row.SessionID, &row.Cwd, &row.TaskRepo, &row.BlockedEvents); err != nil {
			return nil, fmt.Errorf("store: scanning session activity: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

type TerminalTaskMetric struct {
	Repo    string
	CostUSD float64
}

func (s *Store) ListTerminalTaskMetrics(ctx context.Context, startMs, endMs int64) ([]TerminalTaskMetric, error) {
	if endMs <= startMs {
		return nil, ErrInvalidLearning
	}
	rows, err := s.db.QueryContext(ctx, `SELECT repo,cost_usd FROM tasks
		WHERE updated_ms>=? AND updated_ms<? AND status IN ('succeeded','failed','cancelled')
		ORDER BY id`, startMs, endMs)
	if err != nil {
		return nil, fmt.Errorf("store: listing terminal task metrics: %w", err)
	}
	defer rows.Close()
	var out []TerminalTaskMetric
	for rows.Next() {
		var row TerminalTaskMetric
		if err := rows.Scan(&row.Repo, &row.CostUSD); err != nil {
			return nil, fmt.Errorf("store: scanning terminal task metric: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
