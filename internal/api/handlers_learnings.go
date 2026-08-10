package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/djbu/corral/internal/clock"
	"github.com/djbu/corral/internal/config"
	corralgit "github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/learning"
	"github.com/djbu/corral/internal/redact"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
)

type LearningsDeps struct {
	Store  *store.Store
	Clock  clock.Clock
	Config config.Learn
}

func (s *Server) RegisterLearnings(deps LearningsDeps) {
	s.Handle("POST /v1/learnings/scan", deps.handleScanLearnings)
	s.Handle("GET /v1/learnings", deps.handleListLearnings)
	s.Handle("GET /v1/learnings/{id}", deps.handleGetLearning)
	s.Handle("GET /v1/learnings/{id}/report", deps.handleLearningReport)
	s.Handle("POST /v1/learnings/{id}/adopt", deps.handleAdoptLearning)
	s.Handle("POST /v1/learnings/{id}/reject", deps.handleRejectLearning)
	s.Handle("POST /v1/learnings/{id}/retire", deps.handleRetireLearning)
}

type scanLearningsRequest struct {
	Repo string `json:"repo"`
}

type learningResponse struct {
	ID            string             `json:"id"`
	Repo          string             `json:"repo"`
	Kind          string             `json:"kind"`
	Fingerprint   string             `json:"fingerprint"`
	Status        string             `json:"status"`
	Content       json.RawMessage    `json:"content"`
	EvidenceCount int                `json:"evidence_count"`
	Baseline      json.RawMessage    `json:"baseline"`
	Verification  json.RawMessage    `json:"verification,omitempty"`
	CreatedMs     int64              `json:"created_ms"`
	UpdatedMs     int64              `json:"updated_ms"`
	VerifiedMs    *int64             `json:"verified_ms,omitempty"`
	ProposedMs    *int64             `json:"proposed_ms,omitempty"`
	AdoptedMs     *int64             `json:"adopted_ms,omitempty"`
	RejectedMs    *int64             `json:"rejected_ms,omitempty"`
	ExpiresMs     int64              `json:"expires_ms"`
	Evidence      []evidenceResponse `json:"evidence,omitempty"`
}

type evidenceResponse struct {
	EventSeq int64  `json:"event_seq"`
	Role     string `json:"role"`
}

type listLearningsResponse struct {
	Learnings []learningResponse `json:"learnings"`
}

type scanLearningsResponse struct {
	WindowStartMs int64              `json:"window_start_ms"`
	WindowEndMs   int64              `json:"window_end_ms"`
	Sessions      int                `json:"sessions"`
	SkippedCWDs   int                `json:"skipped_cwds"`
	Eligible      int                `json:"eligible_requests"`
	Learnings     []learningResponse `json:"learnings"`
}

func toLearningResponse(l *store.Learning) learningResponse {
	r := learningResponse{
		ID: l.ID, Repo: l.Repo, Kind: string(l.Kind), Fingerprint: l.Fingerprint,
		Status: string(l.Status), Content: json.RawMessage(l.ContentJSON),
		EvidenceCount: l.EvidenceCount, Baseline: json.RawMessage(l.BaselineJSON),
		CreatedMs: l.CreatedMs, UpdatedMs: l.UpdatedMs, VerifiedMs: l.VerifiedMs,
		ProposedMs: l.ProposedMs, AdoptedMs: l.AdoptedMs, RejectedMs: l.RejectedMs,
		ExpiresMs: l.ExpiresMs,
	}
	if l.VerificationJSON != "" {
		r.Verification = json.RawMessage(l.VerificationJSON)
	}
	return r
}

func (d LearningsDeps) handleScanLearnings(w http.ResponseWriter, r *http.Request) {
	if _, ok := scopedLearningRepo(r.Context(), d.Store); ok {
		writeError(w, http.StatusForbidden, CodeForbidden, "session-scoped tokens cannot scan learnings", nil)
		return
	}
	var req scanLearningsRequest
	if r.Body != nil && r.ContentLength != 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error(), nil)
			return
		}
	}
	mined, err := learning.NewMiner(d.Store, d.Clock, d.Config).Scan(r.Context(), req.Repo)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
		return
	}
	verifier := learning.NewVerifier(d.Store, d.Config)
	out := scanLearningsResponse{
		WindowStartMs: mined.WindowStartMs, WindowEndMs: mined.WindowEndMs,
		Sessions: mined.Sessions, SkippedCWDs: mined.SkippedCWDs, Eligible: mined.Eligible,
		Learnings: make([]learningResponse, 0, len(mined.Candidates)),
	}
	for _, candidate := range mined.Candidates {
		resolved := candidate
		if candidate.Status == store.LearningCandidate {
			resolved, err = verifier.VerifyCandidate(r.Context(), candidate.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
				return
			}
		}
		out.Learnings = append(out.Learnings, toLearningResponse(resolved))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d LearningsDeps) handleListLearnings(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	status := store.LearningStatus(r.URL.Query().Get("status"))
	if !store.ValidLearningStatus(status) {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid learning status", nil)
		return
	}
	if scopedRepo, scoped := scopedLearningRepo(r.Context(), d.Store); scoped {
		if scopedRepo == "" {
			writeError(w, http.StatusInternalServerError, CodeInternal, "cannot resolve token repository scope", nil)
			return
		}
		if repo != "" {
			requested, err := corralgit.ResolveRepo(r.Context(), repo)
			if err != nil || requested != scopedRepo {
				writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this repository", nil)
				return
			}
		}
		repo = scopedRepo
	} else if repo != "" {
		var err error
		repo, err = corralgit.ResolveRepo(r.Context(), repo)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
			return
		}
	}
	rows, err := d.Store.ListLearnings(r.Context(), repo, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	out := make([]learningResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, toLearningResponse(row))
	}
	writeJSON(w, http.StatusOK, listLearningsResponse{Learnings: out})
}

func (d LearningsDeps) handleGetLearning(w http.ResponseWriter, r *http.Request) {
	l, ok := d.authorizedLearning(w, r)
	if !ok {
		return
	}
	out := toLearningResponse(l)
	evidence, err := d.Store.ListLearningEvidence(r.Context(), l.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	out.Evidence = make([]evidenceResponse, 0, len(evidence))
	for _, ev := range evidence {
		out.Evidence = append(out.Evidence, evidenceResponse{EventSeq: ev.EventSeq, Role: ev.Role})
	}
	writeJSON(w, http.StatusOK, out)
}

type measurementResponse struct {
	ID            string          `json:"id"`
	Phase         string          `json:"phase"`
	WindowStartMs int64           `json:"window_start_ms"`
	WindowEndMs   int64           `json:"window_end_ms"`
	Metrics       json.RawMessage `json:"metrics"`
	Verdict       string          `json:"verdict"`
	CreatedMs     int64           `json:"created_ms"`
}

type learningReportResponse struct {
	Learning     learningResponse      `json:"learning"`
	Measurements []measurementResponse `json:"measurements"`
	EligibleAtMs int64                 `json:"eligible_at_ms,omitempty"`
}

func (d LearningsDeps) handleLearningReport(w http.ResponseWriter, r *http.Request) {
	if _, scoped := scopedLearningRepo(r.Context(), d.Store); scoped {
		writeError(w, http.StatusForbidden, CodeForbidden, "session-scoped tokens may only list or show learnings", nil)
		return
	}
	l, ok := d.authorizedLearning(w, r)
	if !ok {
		return
	}
	early := false
	switch r.URL.Query().Get("early") {
	case "", "false":
	case "true":
		early = true
	default:
		writeError(w, http.StatusBadRequest, CodeBadRequest, "early must be true or false", nil)
		return
	}
	reporter := learning.NewReporter(d.Store, d.Clock, d.Config)
	var report learning.Report
	var err error
	if early {
		report, err = reporter.GenerateEarly(r.Context(), l.ID)
	} else {
		report, err = reporter.Generate(r.Context(), l.ID)
	}
	if err != nil {
		if errors.Is(err, learning.ErrInsufficientPostSample) || errors.Is(err, store.ErrInvalidLearningTransition) {
			writeError(w, http.StatusConflict, CodeLearningConflict, err.Error(), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	out := learningReportResponse{Learning: toLearningResponse(report.Learning), Measurements: make([]measurementResponse, 0, len(report.Measurements)), EligibleAtMs: report.EligibleAtMs}
	for _, m := range report.Measurements {
		out.Measurements = append(out.Measurements, measurementResponse{
			ID: m.ID, Phase: m.Phase, WindowStartMs: m.WindowStartMs,
			WindowEndMs: m.WindowEndMs, Metrics: json.RawMessage(m.MetricsJSON),
			Verdict: m.Verdict, CreatedMs: m.CreatedMs,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (d LearningsDeps) authorizedLearning(w http.ResponseWriter, r *http.Request) (*store.Learning, bool) {
	l, err := d.Store.GetLearning(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeLearningNotFound, "learning not found", nil)
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return nil, false
	}
	if repo, scoped := scopedLearningRepo(r.Context(), d.Store); scoped && repo != l.Repo {
		writeError(w, http.StatusForbidden, CodeForbidden, "token is not scoped to this learning", nil)
		return nil, false
	}
	return l, true
}

func scopedLearningRepo(ctx context.Context, st *store.Store) (string, bool) {
	row, ok := TokenFromContext(ctx)
	if !ok || row.Scope != "session" {
		return "", false
	}
	if task, err := st.GetTaskBySessionID(ctx, row.SessionID); err == nil {
		repo, err := corralgit.CanonicalPath(task.Repo)
		if err != nil {
			return "", true
		}
		return repo, true
	}
	sess, err := st.GetSession(ctx, row.SessionID)
	if err != nil {
		return "", true
	}
	repo, err := corralgit.ResolveRepo(ctx, sess.Cwd)
	if err != nil {
		return "", true
	}
	return repo, true
}

func (d LearningsDeps) handleAdoptLearning(w http.ResponseWriter, r *http.Request) {
	if d.rejectScopedMutation(w, r) {
		return
	}
	l, ok := d.authorizedLearning(w, r)
	if !ok {
		return
	}
	updated, err := learning.NewReporter(d.Store, d.Clock, d.Config).Adopt(r.Context(), l.ID)
	if errors.Is(err, store.ErrInvalidLearningTransition) {
		writeError(w, http.StatusConflict, CodeLearningConflict, "only a proposed learning may be adopted", nil)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	if err := d.appendLearningAudit(r.Context(), session.EventLearningAdopted, updated, ""); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, toLearningResponse(updated))
}

type rejectLearningRequest struct {
	Reason string `json:"reason"`
}

func (d LearningsDeps) handleRejectLearning(w http.ResponseWriter, r *http.Request) {
	if d.rejectScopedMutation(w, r) {
		return
	}
	l, ok := d.authorizedLearning(w, r)
	if !ok {
		return
	}
	var req rejectLearningRequest
	if r.Body != nil && r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error(), nil)
			return
		}
	}
	note, _ := redact.Redact([]byte(req.Reason))
	if len(note) > 1024 {
		note = note[:1024]
	}
	decision, _ := json.Marshal(map[string]string{"reason_category": "operator_rejected", "note": string(note)})
	updated, err := d.Store.TransitionLearning(r.Context(), l.ID, store.LearningTransition{
		From: l.Status, To: store.LearningRejected, VerificationJSON: string(decision),
	})
	if errors.Is(err, store.ErrInvalidLearningTransition) {
		writeError(w, http.StatusConflict, CodeLearningConflict, "learning cannot be rejected from its current status", nil)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	if err := d.appendLearningAudit(r.Context(), session.EventLearningRejected, updated, "operator_rejected"); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, toLearningResponse(updated))
}

func (d LearningsDeps) handleRetireLearning(w http.ResponseWriter, r *http.Request) {
	if d.rejectScopedMutation(w, r) {
		return
	}
	l, ok := d.authorizedLearning(w, r)
	if !ok {
		return
	}
	updated, err := d.Store.TransitionLearning(r.Context(), l.ID, store.LearningTransition{
		From: l.Status, To: store.LearningRetired,
	})
	if errors.Is(err, store.ErrInvalidLearningTransition) {
		writeError(w, http.StatusConflict, CodeLearningConflict, "only a stale or regression-flagged learning may be retired", nil)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	if err := d.appendLearningAudit(r.Context(), session.EventLearningRetired, updated, ""); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, toLearningResponse(updated))
}

func (d LearningsDeps) rejectScopedMutation(w http.ResponseWriter, r *http.Request) bool {
	if _, scoped := scopedLearningRepo(r.Context(), d.Store); scoped {
		writeError(w, http.StatusForbidden, CodeForbidden, "session-scoped tokens cannot mutate learnings", nil)
		return true
	}
	return false
}

func (d LearningsDeps) appendLearningAudit(ctx context.Context, kind session.EventKind, l *store.Learning, reason string) error {
	data, _ := json.Marshal(map[string]any{
		"learning_id": l.ID, "repo": l.Repo, "fingerprint": l.Fingerprint,
		"status": l.Status, "reason": reason,
	})
	_, err := d.Store.AppendEvent(ctx, "", kind, string(data))
	return err
}
