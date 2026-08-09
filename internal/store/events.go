package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/danielbecerra/corral/internal/session"
)

// The events table is append-only by construction (§6.2, §5.4 invariant):
// this file exports only AppendEvent and ListEvents. There is no
// UpdateEvent, no DeleteEvent, no generic Exec escape hatch — nothing else
// in this package writes to the events table.

// EventPublisher receives a copy of every event as it's durably appended.
// Step 32's api.Broker implements this so GET /v1/events/stream can fan out
// live events without this package importing api (which would cycle back
// through api -> store). PublishEvent must not block: AppendEvent holds no
// lock while calling it, but a slow/blocking implementation would still
// stall every caller of AppendEvent, corral-wide. api.Broker's
// implementation is non-blocking by construction (buffered channel + a
// drop-and-flag-resync default case).
type EventPublisher interface {
	PublishEvent(ev session.Event)
}

// SetEventPublisher wires p to receive every event AppendEvent durably
// commits, from this call onward (events already committed are not
// replayed — callers needing history use ListEvents). Call this once at
// startup, before any goroutine can call AppendEvent; Store does not
// synchronize access to pub itself.
func (s *Store) SetEventPublisher(p EventPublisher) {
	s.pub = p
}

// AppendEvent inserts a new event and returns it with its assigned Seq and
// TsMs populated. sessionID is "" for a daemon-scoped event (persisted as
// NULL). dataJSON must be valid JSON; "" is treated as "{}" (the column
// default and the DDL's own default for a reason: `data_json` is NOT
// NULL).
func (s *Store) AppendEvent(ctx context.Context, sessionID string, kind session.EventKind, dataJSON string) (*session.Event, error) {
	if dataJSON == "" {
		dataJSON = "{}"
	}
	now := s.clk.Now().UnixMilli()

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (session_id, ts_ms, kind, data_json) VALUES (?, ?, ?, ?)`,
		nullableStr(sessionID), now, string(kind), dataJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("store: appending event %s: %w", kind, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("store: reading appended event seq: %w", err)
	}

	ev := &session.Event{
		Seq:       seq,
		SessionID: sessionID,
		TsMs:      now,
		Kind:      kind,
		DataJSON:  dataJSON,
	}
	if s.pub != nil {
		s.pub.PublishEvent(*ev)
	}
	return ev, nil
}

// ListEvents returns every event for sessionID, ordered by seq ascending
// (append order, matching the events_session_seq index). Pass "" to list
// daemon-scoped events (session_id IS NULL).
func (s *Store) ListEvents(ctx context.Context, sessionID string) ([]*session.Event, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if sessionID == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT seq, session_id, ts_ms, kind, data_json FROM events WHERE session_id IS NULL ORDER BY seq ASC`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT seq, session_id, ts_ms, kind, data_json FROM events WHERE session_id = ? ORDER BY seq ASC`, sessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("store: listing events: %w", err)
	}
	defer rows.Close()

	var out []*session.Event
	for rows.Next() {
		var (
			ev   session.Event
			sid  sql.NullString
			kind string
		)
		if err := rows.Scan(&ev.Seq, &sid, &ev.TsMs, &kind, &ev.DataJSON); err != nil {
			return nil, fmt.Errorf("store: scanning event: %w", err)
		}
		ev.SessionID = sid.String
		ev.Kind = session.EventKind(kind)
		out = append(out, &ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing events: %w", err)
	}
	return out, nil
}
