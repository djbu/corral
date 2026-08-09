package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type LearningKind string

const LearningPermissionRule LearningKind = "permission_rule"

type LearningStatus string

const (
	LearningCandidate         LearningStatus = "candidate"
	LearningVerified          LearningStatus = "verified"
	LearningProposed          LearningStatus = "proposed"
	LearningAdopted           LearningStatus = "adopted"
	LearningRejected          LearningStatus = "rejected"
	LearningRegressionFlagged LearningStatus = "regression_flagged"
	LearningStale             LearningStatus = "stale"
	LearningRetired           LearningStatus = "retired"
)

var (
	ErrInvalidLearning           = errors.New("store: invalid learning")
	ErrInvalidLearningTransition = errors.New("store: invalid learning transition")
)

type Learning struct {
	ID               string
	Repo             string
	Kind             LearningKind
	Fingerprint      string
	Status           LearningStatus
	ContentJSON      string
	EvidenceCount    int
	BaselineJSON     string
	VerificationJSON string
	CreatedMs        int64
	UpdatedMs        int64
	VerifiedMs       *int64
	ProposedMs       *int64
	AdoptedMs        *int64
	RejectedMs       *int64
	ExpiresMs        int64
}

type LearningEvidence struct {
	LearningID string
	EventSeq   int64
	Role       string
}

type UpsertLearningParams struct {
	ID            string
	Repo          string
	Kind          LearningKind
	Fingerprint   string
	ContentJSON   string
	BaselineJSON  string
	EvidenceCount int
	ExpiresMs     int64
	Evidence      []LearningEvidence
}

// UpsertLearningCandidate is idempotent by repo/kind/fingerprint. Only a
// candidate row is refreshed; an operator decision or later lifecycle state
// is never silently resurrected by a subsequent scan.
func (s *Store) UpsertLearningCandidate(ctx context.Context, p UpsertLearningParams) (*Learning, error) {
	if p.ID == "" || p.Repo == "" || p.Kind != LearningPermissionRule || p.Fingerprint == "" ||
		p.EvidenceCount < 0 || p.ExpiresMs <= 0 || !json.Valid([]byte(p.ContentJSON)) || !json.Valid([]byte(p.BaselineJSON)) {
		return nil, ErrInvalidLearning
	}
	now := s.clk.Now().UnixMilli()
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO learnings (
				id, repo, kind, fingerprint, status, content_json, evidence_count,
				baseline_json, created_ms, updated_ms, expires_ms
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo, kind, fingerprint) DO UPDATE SET
				content_json=excluded.content_json,
				evidence_count=excluded.evidence_count,
				baseline_json=excluded.baseline_json,
				updated_ms=excluded.updated_ms,
				expires_ms=excluded.expires_ms
			WHERE learnings.status = 'candidate'`,
			p.ID, p.Repo, string(p.Kind), p.Fingerprint, string(LearningCandidate),
			p.ContentJSON, p.EvidenceCount, p.BaselineJSON, now, now, p.ExpiresMs)
		if err != nil {
			return fmt.Errorf("store: upserting learning: %w", err)
		}
		var id string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM learnings WHERE repo=? AND kind=? AND fingerprint=?`,
			p.Repo, string(p.Kind), p.Fingerprint).Scan(&id); err != nil {
			return fmt.Errorf("store: resolving upserted learning: %w", err)
		}
		for _, ev := range p.Evidence {
			if ev.EventSeq <= 0 || ev.Role == "" {
				return ErrInvalidLearning
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO learning_evidence (learning_id,event_seq,role) VALUES (?,?,?)`,
				id, ev.EventSeq, ev.Role); err != nil {
				return fmt.Errorf("store: adding learning evidence: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetLearningByFingerprint(ctx, p.Repo, p.Kind, p.Fingerprint)
}

const learningColumns = `SELECT id,repo,kind,fingerprint,status,content_json,evidence_count,
	baseline_json,verification_json,created_ms,updated_ms,verified_ms,proposed_ms,
	adopted_ms,rejected_ms,expires_ms FROM learnings`

func (s *Store) GetLearning(ctx context.Context, id string) (*Learning, error) {
	return scanLearning(s.db.QueryRowContext(ctx, learningColumns+` WHERE id=?`, id))
}

func (s *Store) GetLearningByFingerprint(ctx context.Context, repo string, kind LearningKind, fingerprint string) (*Learning, error) {
	return scanLearning(s.db.QueryRowContext(ctx, learningColumns+` WHERE repo=? AND kind=? AND fingerprint=?`, repo, string(kind), fingerprint))
}

func (s *Store) ListLearnings(ctx context.Context, repo string, status LearningStatus) ([]*Learning, error) {
	query := learningColumns + ` WHERE (?='' OR repo=?) AND (?='' OR status=?) ORDER BY updated_ms DESC,id ASC`
	rows, err := s.db.QueryContext(ctx, query, repo, repo, string(status), string(status))
	if err != nil {
		return nil, fmt.Errorf("store: listing learnings: %w", err)
	}
	defer rows.Close()
	var out []*Learning
	for rows.Next() {
		l, err := scanLearning(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) ListLearningEvidence(ctx context.Context, id string) ([]LearningEvidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT learning_id,event_seq,role FROM learning_evidence WHERE learning_id=? ORDER BY event_seq,role`, id)
	if err != nil {
		return nil, fmt.Errorf("store: listing learning evidence: %w", err)
	}
	defer rows.Close()
	var out []LearningEvidence
	for rows.Next() {
		var ev LearningEvidence
		if err := rows.Scan(&ev.LearningID, &ev.EventSeq, &ev.Role); err != nil {
			return nil, fmt.Errorf("store: scanning learning evidence: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

type LearningTransition struct {
	From             LearningStatus
	To               LearningStatus
	VerificationJSON string
}

func (s *Store) TransitionLearning(ctx context.Context, id string, tr LearningTransition) (*Learning, error) {
	if !validLearningTransition(tr.From, tr.To) || (tr.VerificationJSON != "" && !json.Valid([]byte(tr.VerificationJSON))) {
		return nil, ErrInvalidLearningTransition
	}
	now := s.clk.Now().UnixMilli()
	column := map[LearningStatus]string{
		LearningVerified: "verified_ms", LearningProposed: "proposed_ms",
		LearningAdopted: "adopted_ms", LearningRejected: "rejected_ms",
	}[tr.To]
	query := `UPDATE learnings SET status=?,updated_ms=?,verification_json=COALESCE(NULLIF(?,''),verification_json)`
	args := []any{string(tr.To), now, tr.VerificationJSON}
	if column != "" {
		query += `,` + column + `=?`
		args = append(args, now)
	}
	query += ` WHERE id=? AND status=?`
	args = append(args, id, string(tr.From))
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: transitioning learning: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return nil, ErrInvalidLearningTransition
	}
	return s.GetLearning(ctx, id)
}

func validLearningTransition(from, to LearningStatus) bool {
	allowed := map[LearningStatus][]LearningStatus{
		LearningCandidate:         {LearningVerified, LearningRejected},
		LearningVerified:          {LearningProposed, LearningRejected},
		LearningProposed:          {LearningAdopted, LearningRejected},
		LearningAdopted:           {LearningRegressionFlagged, LearningStale},
		LearningRegressionFlagged: {LearningRetired},
		LearningStale:             {LearningRetired},
	}
	for _, candidate := range allowed[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

type LearningMeasurement struct {
	ID, LearningID, Phase, MetricsJSON, Verdict string
	WindowStartMs, WindowEndMs, CreatedMs       int64
}

func (s *Store) CreateLearningMeasurement(ctx context.Context, m LearningMeasurement) error {
	if m.ID == "" || m.LearningID == "" || m.Phase == "" || m.Verdict == "" ||
		m.WindowEndMs <= m.WindowStartMs || !json.Valid([]byte(m.MetricsJSON)) {
		return ErrInvalidLearning
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO learning_measurements
		(id,learning_id,phase,window_start_ms,window_end_ms,metrics_json,verdict,created_ms)
		VALUES (?,?,?,?,?,?,?,?)`, m.ID, m.LearningID, m.Phase, m.WindowStartMs,
		m.WindowEndMs, m.MetricsJSON, m.Verdict, s.clk.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store: creating learning measurement: %w", err)
	}
	return nil
}

func scanLearning(row interface{ Scan(...any) error }) (*Learning, error) {
	var l Learning
	var kind, status string
	var verification sql.NullString
	var verified, proposed, adopted, rejected sql.NullInt64
	err := row.Scan(&l.ID, &l.Repo, &kind, &l.Fingerprint, &status, &l.ContentJSON,
		&l.EvidenceCount, &l.BaselineJSON, &verification, &l.CreatedMs, &l.UpdatedMs,
		&verified, &proposed, &adopted, &rejected, &l.ExpiresMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scanning learning: %w", err)
	}
	l.Kind, l.Status, l.VerificationJSON = LearningKind(kind), LearningStatus(status), verification.String
	l.VerifiedMs, l.ProposedMs = nullableInt64Ptr(verified), nullableInt64Ptr(proposed)
	l.AdoptedMs, l.RejectedMs = nullableInt64Ptr(adopted), nullableInt64Ptr(rejected)
	return &l, nil
}

func nullableInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}
