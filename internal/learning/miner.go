// Package learning implements M6's deterministic, evidence-backed learning
// loop. The permission-rule path deliberately has no model dependency.
package learning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/danielbecerra/corral/internal/clock"
	"github.com/danielbecerra/corral/internal/config"
	corralgit "github.com/danielbecerra/corral/internal/git"
	"github.com/danielbecerra/corral/internal/session"
	"github.com/danielbecerra/corral/internal/store"
)

type MinerStore interface {
	ListPermissionHistory(ctx context.Context, startMs, endMs int64) ([]store.PermissionHistory, error)
	UpsertLearningCandidate(ctx context.Context, p store.UpsertLearningParams) (*store.Learning, error)
	AppendEvent(ctx context.Context, sessionID string, kind session.EventKind, dataJSON string) (*session.Event, error)
}

type RepoResolver func(context.Context, string) (string, error)

type Miner struct {
	store   MinerStore
	clock   clock.Clock
	config  config.Learn
	resolve RepoResolver
	newID   func() string
}

type ScanResult struct {
	WindowStartMs int64
	WindowEndMs   int64
	Sessions      int
	SkippedCWDs   int
	Eligible      int
	Candidates    []*store.Learning
}

func NewMiner(st MinerStore, clk clock.Clock, cfg config.Learn) *Miner {
	return &Miner{store: st, clock: clk, config: cfg, resolve: corralgit.ResolveRepo, newID: uuid.NewString}
}

type permissionContent struct {
	Tool    string `json:"tool"`
	Command string `json:"command"`
	Rule    string `json:"rule"`
}

type baseline struct {
	WindowStartMs int64 `json:"window_start_ms"`
	WindowEndMs   int64 `json:"window_end_ms"`
	Approvals     int   `json:"approvals"`
}

type requestObservation struct {
	command       string
	requestSeq    int64
	blockSeq      int64
	answerSeq     int64
	resolutionSeq int64
	outcome       string
}

type candidateKey struct{ repo, command string }

type candidateEvidence struct {
	approvals []requestObservation
	negative  bool
}

// Scan mines exact Bash rules from correlated manual approvals in the current
// configured window. It writes candidate projections, never settings.
func (m *Miner) Scan(ctx context.Context, onlyRepo string) (ScanResult, error) {
	end := m.clock.Now()
	start := end.Add(-m.config.Window)
	result := ScanResult{WindowStartMs: start.UnixMilli(), WindowEndMs: end.UnixMilli()}
	history, err := m.store.ListPermissionHistory(ctx, result.WindowStartMs, result.WindowEndMs)
	if err != nil {
		return result, err
	}

	filterRepo := ""
	if onlyRepo != "" {
		filterRepo, err = m.resolve(ctx, onlyRepo)
		if err != nil {
			return result, fmt.Errorf("learning: resolving scan repository: %w", err)
		}
	}

	grouped := make(map[candidateKey]*candidateEvidence)
	for _, h := range history {
		repoPath := h.TaskRepo
		if repoPath != "" {
			repoPath, err = corralgit.CanonicalPath(repoPath)
		} else {
			repoPath, err = m.resolve(ctx, h.Cwd)
		}
		if err != nil {
			result.SkippedCWDs++
			continue
		}
		if filterRepo != "" && repoPath != filterRepo {
			continue
		}
		result.Sessions++
		for _, obs := range correlate(h.Events) {
			if obs.command == "" {
				continue
			}
			result.Eligible++
			key := candidateKey{repo: repoPath, command: obs.command}
			bucket := grouped[key]
			if bucket == nil {
				bucket = &candidateEvidence{}
				grouped[key] = bucket
			}
			if obs.outcome == "failed_or_denied" {
				bucket.negative = true
			}
			if obs.outcome == "approved" && obs.blockSeq > 0 && obs.answerSeq > 0 {
				bucket.approvals = append(bucket.approvals, obs)
			}
		}
	}

	keys := make([]candidateKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo != keys[j].repo {
			return keys[i].repo < keys[j].repo
		}
		return keys[i].command < keys[j].command
	})
	for _, key := range keys {
		evidence := grouped[key]
		if evidence.negative || len(evidence.approvals) < m.config.MinApprovals {
			continue
		}
		contentBytes, err := json.Marshal(permissionContent{
			Tool: "Bash", Command: key.command, Rule: "Bash(" + key.command + ")",
		})
		if err != nil {
			return result, fmt.Errorf("learning: marshaling permission candidate: %w", err)
		}
		sum := sha256.Sum256(append([]byte(string(store.LearningPermissionRule)+"\x00"), contentBytes...))
		baselineBytes, _ := json.Marshal(baseline{
			WindowStartMs: result.WindowStartMs, WindowEndMs: result.WindowEndMs,
			Approvals: len(evidence.approvals),
		})
		provenance := make([]store.LearningEvidence, 0, len(evidence.approvals)*4)
		for _, approval := range evidence.approvals {
			provenance = append(provenance,
				store.LearningEvidence{EventSeq: approval.requestSeq, Role: "request"},
				store.LearningEvidence{EventSeq: approval.blockSeq, Role: "block"},
				store.LearningEvidence{EventSeq: approval.answerSeq, Role: "answer"},
				store.LearningEvidence{EventSeq: approval.resolutionSeq, Role: "resolution"},
			)
		}
		learning, err := m.store.UpsertLearningCandidate(ctx, store.UpsertLearningParams{
			ID: m.newID(), Repo: key.repo, Kind: store.LearningPermissionRule,
			Fingerprint: hex.EncodeToString(sum[:]), ContentJSON: string(contentBytes),
			BaselineJSON: string(baselineBytes), EvidenceCount: len(evidence.approvals),
			ExpiresMs: end.Add(m.config.TTL).UnixMilli(), Evidence: provenance,
		})
		if err != nil {
			return result, err
		}
		result.Candidates = append(result.Candidates, learning)
		data, _ := json.Marshal(map[string]any{
			"learning_id": learning.ID, "repo": learning.Repo,
			"fingerprint": learning.Fingerprint, "evidence_count": learning.EvidenceCount,
		})
		if _, err := m.store.AppendEvent(ctx, "", session.EventLearningMined, string(data)); err != nil {
			return result, err
		}
	}
	return result, nil
}

func correlate(events []session.Event) []requestObservation {
	requests := make(map[int64]*requestObservation)
	order := make([]int64, 0)
	for _, ev := range events {
		switch ev.Kind {
		case session.EventPermissionRequested:
			if command, ok := eligibleCommand(ev.DataJSON); ok {
				requests[ev.Seq] = &requestObservation{command: command, requestSeq: ev.Seq}
				order = append(order, ev.Seq)
			}
		case session.EventPermissionBlocked:
			seq := requestSeq(ev.DataJSON, "request_seq")
			if req := requests[seq]; req != nil && req.resolutionSeq == 0 && req.blockSeq == 0 {
				req.blockSeq = ev.Seq
			}
		case session.EventSessionAnswered:
			seq := requestSeq(ev.DataJSON, "permission_request_seq")
			if req := requests[seq]; req != nil && req.resolutionSeq == 0 && req.blockSeq > 0 && req.answerSeq == 0 {
				req.answerSeq = ev.Seq
			}
		case session.EventPermissionResolved:
			seq := requestSeq(ev.DataJSON, "request_seq")
			if req := requests[seq]; req != nil && req.resolutionSeq == 0 {
				var payload struct {
					Outcome string `json:"outcome"`
				}
				if json.Unmarshal([]byte(ev.DataJSON), &payload) == nil {
					req.resolutionSeq, req.outcome = ev.Seq, payload.Outcome
				}
			}
		}
	}
	out := make([]requestObservation, 0, len(order))
	for _, seq := range order {
		if requests[seq].resolutionSeq > 0 {
			out = append(out, *requests[seq])
		}
	}
	return out
}

func requestSeq(raw, field string) int64 {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return 0
	}
	var seq int64
	_ = json.Unmarshal(payload[field], &seq)
	return seq
}

func eligibleCommand(raw string) (string, bool) {
	var payload struct {
		ToolName   string          `json:"tool_name"`
		ToolInput  json.RawMessage `json:"tool_input"`
		Redactions []string        `json:"redactions"`
		Truncated  bool            `json:"truncated"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil || payload.ToolName != "Bash" ||
		payload.Truncated || len(payload.Redactions) > 0 {
		return "", false
	}
	var input struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(payload.ToolInput, &input) != nil {
		return "", false
	}
	command := input.Command
	if command == "" || len(command) > 1024 || !utf8.ValidString(command) || strings.ContainsAny(command, "\x00\r\n;&|") || strings.Contains(command, ")") {
		return "", false
	}
	return command, true
}
