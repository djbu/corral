package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/djbu/corral/internal/session"
)

type ReviewStatus string

const (
	ReviewPending   ReviewStatus = "pending_review"
	ReviewReleased  ReviewStatus = "released"
	ReviewDiscarded ReviewStatus = "discarded"
)

var ErrReviewConflict = errors.New("store: review state conflict")

type TaskReview struct {
	TaskID       string       `json:"task_id"`
	Status       ReviewStatus `json:"status"`
	Strategy     string       `json:"strategy,omitempty"`
	TargetRef    string       `json:"target_ref,omitempty"`
	TargetBefore string       `json:"target_before,omitempty"`
	ResultCommit string       `json:"result_commit,omitempty"`
	RecoveryRef  string       `json:"recovery_ref,omitempty"`
	CreatedMs    int64        `json:"created_ms"`
	UpdatedMs    int64        `json:"updated_ms"`
}

// MarkTaskSucceeded is the atomic success transition. Worktree-backed tasks
// created by an M8 daemon receive their pending review in the same commit;
// ordinary and legacy tasks retain execution state without invented Git data.
func (s *Store) MarkTaskSucceeded(ctx context.Context, taskID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		now := s.clk.Now().UnixMilli()
		res, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_ms = ? WHERE id = ?`, string(TaskSucceeded), now, taskID)
		if err != nil {
			return fmt.Errorf("store: succeeding task %s: %w", taskID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO task_reviews (task_id, status, created_ms, updated_ms)
			SELECT id, ?, ?, ? FROM tasks
			WHERE id = ? AND worktree IS NOT NULL AND worktree != 'requested'
			  AND base_commit IS NOT NULL
			ON CONFLICT(task_id) DO NOTHING
		`, string(ReviewPending), now, now, taskID)
		if err != nil {
			return fmt.Errorf("store: creating review for task %s: %w", taskID, err)
		}
		return nil
	})
}

func (s *Store) GetTaskReview(ctx context.Context, taskID string) (*TaskReview, error) {
	var r TaskReview
	var status string
	var strategy, targetRef, targetBefore, resultCommit, recoveryRef sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT task_id, status, strategy, target_ref,
		target_before, result_commit, recovery_ref, created_ms, updated_ms
		FROM task_reviews WHERE task_id = ?`, taskID).Scan(
		&r.TaskID, &status, &strategy, &targetRef, &targetBefore,
		&resultCommit, &recoveryRef, &r.CreatedMs, &r.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: getting task review %s: %w", taskID, err)
	}
	r.Status = ReviewStatus(status)
	r.Strategy, r.TargetRef, r.TargetBefore = strategy.String, targetRef.String, targetBefore.String
	r.ResultCommit, r.RecoveryRef = resultCommit.String, recoveryRef.String
	return &r, nil
}

// CompleteTaskReview changes a pending review and appends its success event
// in one SQLite transaction. Git has already completed when this is called.
func (s *Store) CompleteTaskReview(ctx context.Context, taskID string, status ReviewStatus, strategy, targetRef, targetBefore, resultCommit, recoveryRef string, kind session.EventKind, dataJSON string) error {
	if status != ReviewReleased && status != ReviewDiscarded {
		return fmt.Errorf("%w: invalid terminal status %q", ErrReviewConflict, status)
	}
	var ev session.Event
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := s.clk.Now().UnixMilli()
		res, err := tx.ExecContext(ctx, `UPDATE task_reviews SET status=?, strategy=?, target_ref=?,
			target_before=?, result_commit=?, recovery_ref=?, updated_ms=?
			WHERE task_id=? AND status=?`, string(status), nullableStr(strategy), nullableStr(targetRef),
			nullableStr(targetBefore), nullableStr(resultCommit), nullableStr(recoveryRef), now,
			taskID, string(ReviewPending))
		if err != nil {
			return fmt.Errorf("store: completing review %s: %w", taskID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%w: task %s is not pending review", ErrReviewConflict, taskID)
		}
		if dataJSON == "" {
			dataJSON = "{}"
		}
		insert, err := tx.ExecContext(ctx, `INSERT INTO events (session_id, ts_ms, kind, data_json) VALUES (NULL, ?, ?, ?)`, now, string(kind), dataJSON)
		if err != nil {
			return fmt.Errorf("store: appending review event: %w", err)
		}
		seq, err := insert.LastInsertId()
		if err != nil {
			return err
		}
		ev = session.Event{Seq: seq, TsMs: now, Kind: kind, DataJSON: dataJSON}
		return nil
	})
	if err == nil && s.pub != nil {
		s.pub.PublishEvent(ev)
	}
	return err
}
