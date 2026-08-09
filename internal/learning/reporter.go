package learning

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/config"
	corralgit "github.com/danielbecerra/corral/internal/git"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
)

type ReporterStore interface {
	GetLearning(context.Context, string) (*store.Learning, error)
	ListSessionActivity(context.Context, int64, int64) ([]store.SessionActivity, error)
	ListTerminalTaskMetrics(context.Context, int64, int64) ([]store.TerminalTaskMetric, error)
	ListPermissionHistory(context.Context, int64, int64) ([]store.PermissionHistory, error)
	ListLearningMeasurements(context.Context, string) ([]store.LearningMeasurement, error)
	CreateLearningMeasurement(context.Context, store.LearningMeasurement) error
	AdoptLearning(context.Context, store.AdoptLearningParams) (*store.Learning, error)
	TransitionLearning(context.Context, string, store.LearningTransition) (*store.Learning, error)
	AppendEvent(context.Context, string, session.EventKind, string) (*session.Event, error)
}

type Metrics struct {
	BlockedEvents       int     `json:"blocked_events"`
	Sessions            int     `json:"sessions"`
	BlockedPerSession   float64 `json:"blocked_per_session"`
	TaskCostUSD         float64 `json:"task_cost_usd"`
	TerminalTasks       int     `json:"terminal_tasks"`
	CostPerTerminalTask float64 `json:"cost_per_terminal_task"`
	MatchedRequests     int     `json:"matched_requests"`
	MatchedBlocks       int     `json:"matched_blocks"`
	SkippedCWDs         int     `json:"skipped_cwds"`
}

type Report struct {
	Learning     *store.Learning
	Measurements []store.LearningMeasurement
	EligibleAtMs int64
}

type Reporter struct {
	store   ReporterStore
	clock   clock.Clock
	config  config.Learn
	resolve RepoResolver
	newID   func() string
}

func NewReporter(st ReporterStore, clk clock.Clock, cfg config.Learn) *Reporter {
	return &Reporter{store: st, clock: clk, config: cfg, resolve: corralgit.ResolveRepo, newID: uuid.NewString}
}

func (r *Reporter) Adopt(ctx context.Context, id string) (*store.Learning, error) {
	l, err := r.store.GetLearning(ctx, id)
	if err != nil {
		return nil, err
	}
	if l.Status != store.LearningProposed {
		return nil, store.ErrInvalidLearningTransition
	}
	now := r.clock.Now()
	end := now.UnixMilli()
	start := now.Add(-r.config.Window).UnixMilli()
	metrics, err := r.measure(ctx, l, start, end)
	if err != nil {
		return nil, err
	}
	metricsJSON, _ := json.Marshal(metrics)
	return r.store.AdoptLearning(ctx, store.AdoptLearningParams{
		LearningID: l.ID, AdoptedMs: end, ExpiresMs: now.Add(r.config.TTL).UnixMilli(),
		MeasurementID: r.newID(), WindowStartMs: start, MetricsJSON: string(metricsJSON),
	})
}

func (r *Reporter) Generate(ctx context.Context, id string) (Report, error) {
	l, err := r.store.GetLearning(ctx, id)
	if err != nil {
		return Report{}, err
	}
	measurements, err := r.store.ListLearningMeasurements(ctx, id)
	if err != nil {
		return Report{}, err
	}
	report := Report{Learning: l, Measurements: measurements}
	if l.AdoptedMs == nil {
		return report, nil
	}
	report.EligibleAtMs = *l.AdoptedMs + r.config.Window.Milliseconds()
	if r.clock.Now().UnixMilli() < report.EligibleAtMs {
		return report, nil
	}
	start, end := *l.AdoptedMs, report.EligibleAtMs
	post, err := r.measure(ctx, l, start, end)
	if err != nil {
		return Report{}, err
	}
	baselineMetrics, ok := measurementMetrics(measurements, "baseline")
	if !ok {
		return Report{}, fmt.Errorf("learning: adopted learning %s has no baseline measurement", id)
	}
	verdict := r.verdict(baselineMetrics, post)
	postJSON, _ := json.Marshal(post)
	if err := r.store.CreateLearningMeasurement(ctx, store.LearningMeasurement{
		ID: r.newID(), LearningID: id, Phase: "post", WindowStartMs: start,
		WindowEndMs: end, MetricsJSON: string(postJSON), Verdict: verdict,
	}); err != nil {
		return Report{}, err
	}
	if err := r.audit(ctx, session.EventLearningMeasured, l, verdict); err != nil {
		return Report{}, err
	}

	if verdict == "regression" && l.Status == store.LearningAdopted {
		l, err = r.store.TransitionLearning(ctx, id, store.LearningTransition{
			From: store.LearningAdopted, To: store.LearningRegressionFlagged,
		})
		if err != nil {
			return Report{}, err
		}
		if err := r.audit(ctx, session.EventLearningRegressionFlagged, l, verdict); err != nil {
			return Report{}, err
		}
	} else if r.clock.Now().UnixMilli() >= l.ExpiresMs && l.Status == store.LearningAdopted {
		l, err = r.store.TransitionLearning(ctx, id, store.LearningTransition{
			From: store.LearningAdopted, To: store.LearningStale,
		})
		if err != nil {
			return Report{}, err
		}
		if err := r.audit(ctx, session.EventLearningStale, l, "expired"); err != nil {
			return Report{}, err
		}
	}
	measurements, err = r.store.ListLearningMeasurements(ctx, id)
	if err != nil {
		return Report{}, err
	}
	return Report{Learning: l, Measurements: measurements, EligibleAtMs: report.EligibleAtMs}, nil
}

func (r *Reporter) measure(ctx context.Context, l *store.Learning, start, end int64) (Metrics, error) {
	var metrics Metrics
	activity, err := r.store.ListSessionActivity(ctx, start, end)
	if err != nil {
		return metrics, err
	}
	for _, row := range activity {
		repo, resolveErr := resolveActivityRepo(ctx, row.TaskRepo, row.Cwd, r.resolve)
		if resolveErr != nil {
			metrics.SkippedCWDs++
			continue
		}
		if repo == l.Repo {
			metrics.Sessions++
			metrics.BlockedEvents += row.BlockedEvents
		}
	}
	if metrics.Sessions > 0 {
		metrics.BlockedPerSession = float64(metrics.BlockedEvents) / float64(metrics.Sessions)
	}
	tasks, err := r.store.ListTerminalTaskMetrics(ctx, start, end)
	if err != nil {
		return metrics, err
	}
	for _, task := range tasks {
		repo, resolveErr := corralgit.CanonicalPath(task.Repo)
		if resolveErr != nil {
			continue
		}
		if repo == l.Repo {
			metrics.TerminalTasks++
			metrics.TaskCostUSD += task.CostUSD
		}
	}
	if metrics.TerminalTasks > 0 {
		metrics.CostPerTerminalTask = metrics.TaskCostUSD / float64(metrics.TerminalTasks)
	}
	var content permissionContent
	if json.Unmarshal([]byte(l.ContentJSON), &content) != nil {
		return metrics, store.ErrInvalidLearning
	}
	history, err := r.store.ListPermissionHistory(ctx, start, end)
	if err != nil {
		return metrics, err
	}
	for _, h := range history {
		repo, resolveErr := resolveHistoryRepo(ctx, h, r.resolve)
		if resolveErr != nil || repo != l.Repo {
			continue
		}
		for _, obs := range correlate(h.Events) {
			if obs.command == content.Command {
				metrics.MatchedRequests++
				if obs.blockSeq > 0 {
					metrics.MatchedBlocks++
				}
			}
		}
	}
	return metrics, nil
}

func resolveActivityRepo(ctx context.Context, taskRepo, cwd string, resolver RepoResolver) (string, error) {
	if taskRepo != "" {
		return corralgit.CanonicalPath(taskRepo)
	}
	return resolver(ctx, cwd)
}

func measurementMetrics(rows []store.LearningMeasurement, phase string) (Metrics, bool) {
	for _, row := range rows {
		if row.Phase != phase {
			continue
		}
		var metrics Metrics
		if json.Unmarshal([]byte(row.MetricsJSON), &metrics) == nil {
			return metrics, true
		}
	}
	return Metrics{}, false
}

func (r *Reporter) verdict(baseline, post Metrics) string {
	if baseline.Sessions < r.config.MinSessions || post.Sessions < r.config.MinSessions ||
		baseline.TerminalTasks < r.config.MinTerminalTasks || post.TerminalTasks < r.config.MinTerminalTasks {
		return "inconclusive"
	}
	costLimit := baseline.CostPerTerminalTask * (1 + r.config.CostRegressionTolerance)
	if post.BlockedPerSession < baseline.BlockedPerSession && post.CostPerTerminalTask <= costLimit {
		return "improved"
	}
	if post.BlockedPerSession > baseline.BlockedPerSession || post.CostPerTerminalTask > costLimit {
		return "regression"
	}
	return "no_change"
}

func (r *Reporter) audit(ctx context.Context, kind session.EventKind, l *store.Learning, verdict string) error {
	data, _ := json.Marshal(map[string]any{
		"learning_id": l.ID, "repo": l.Repo, "status": l.Status, "verdict": verdict,
	})
	_, err := r.store.AppendEvent(ctx, "", kind, string(data))
	return err
}
