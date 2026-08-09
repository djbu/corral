package learning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/djbu/corral/internal/claude/settings"
	"github.com/djbu/corral/internal/config"
	corralgit "github.com/djbu/corral/internal/git"
	"github.com/djbu/corral/internal/session"
	"github.com/djbu/corral/internal/store"
)

type VerifierStore interface {
	GetLearning(ctx context.Context, id string) (*store.Learning, error)
	ListPermissionHistory(ctx context.Context, startMs, endMs int64) ([]store.PermissionHistory, error)
	TransitionLearning(ctx context.Context, id string, tr store.LearningTransition) (*store.Learning, error)
	AppendEvent(ctx context.Context, sessionID string, kind session.EventKind, dataJSON string) (*session.Event, error)
}

type Verifier struct {
	store   VerifierStore
	config  config.Learn
	resolve RepoResolver
}

func NewVerifier(st VerifierStore, cfg config.Learn) *Verifier {
	return &Verifier{store: st, config: cfg, resolve: corralgit.ResolveRepo}
}

type verificationRecord struct {
	Valid              bool            `json:"valid"`
	Reason             string          `json:"reason,omitempty"`
	Rule               string          `json:"rule,omitempty"`
	Approvals          int             `json:"approvals"`
	EligibleMatches    int             `json:"eligible_matches"`
	AvoidableBlocks    int             `json:"avoidable_blocks"`
	FailedOrDenied     int             `json:"failed_or_denied"`
	UnapprovedMatches  int             `json:"unapproved_matches"`
	EvidenceCoverage   float64         `json:"evidence_coverage"`
	SkippedCWDs        int             `json:"skipped_cwds"`
	BeforeSettingsJSON json.RawMessage `json:"before_settings_json,omitempty"`
	AfterSettingsJSON  json.RawMessage `json:"after_settings_json,omitempty"`
}

// VerifyCandidate replays the candidate against its immutable baseline
// window. Success advances candidate->verified->proposed; a mechanical
// validation or evidence failure advances candidate->rejected.
func (v *Verifier) VerifyCandidate(ctx context.Context, id string) (*store.Learning, error) {
	learning, err := v.store.GetLearning(ctx, id)
	if err != nil {
		return nil, err
	}
	if learning.Status != store.LearningCandidate {
		return nil, store.ErrInvalidLearningTransition
	}

	record := verificationRecord{}
	content, window, reason := validateCandidate(learning)
	if reason != "" {
		return v.reject(ctx, learning, record, reason)
	}
	record.Rule = content.Rule
	history, err := v.store.ListPermissionHistory(ctx, window.WindowStartMs, window.WindowEndMs)
	if err != nil {
		return nil, err
	}
	for _, h := range history {
		repo, resolveErr := resolveHistoryRepo(ctx, h, v.resolve)
		if resolveErr != nil {
			record.SkippedCWDs++
			continue
		}
		if repo != learning.Repo {
			continue
		}
		for _, obs := range correlate(h.Events) {
			if obs.command != content.Command {
				continue
			}
			record.EligibleMatches++
			if obs.blockSeq > 0 {
				record.AvoidableBlocks++
			}
			if obs.outcome == "failed_or_denied" {
				record.FailedOrDenied++
			}
			if obs.outcome == "approved" && obs.blockSeq > 0 && obs.answerSeq > 0 {
				record.Approvals++
			} else {
				record.UnapprovedMatches++
			}
		}
	}
	if record.EligibleMatches > 0 {
		record.EvidenceCoverage = float64(record.Approvals) / float64(record.EligibleMatches)
	}
	if record.Approvals < v.config.MinApprovals {
		return v.reject(ctx, learning, record, "insufficient_manual_approvals")
	}
	if record.FailedOrDenied > 0 {
		return v.reject(ctx, learning, record, "negative_evidence")
	}
	if record.UnapprovedMatches > 0 {
		return v.reject(ctx, learning, record, "unapproved_exact_match")
	}
	if record.AvoidableBlocks == 0 {
		return v.reject(ctx, learning, record, "no_observed_block")
	}

	before, err := settings.BuildSettingsJSON("corral hook-relay")
	if err != nil || !json.Valid(before) {
		return nil, fmt.Errorf("learning: building baseline settings: %w", err)
	}
	after, err := settings.BuildSettingsJSONWithRules("corral hook-relay", []string{content.Rule})
	if err != nil || !json.Valid(after) {
		return nil, fmt.Errorf("learning: building proposed settings: %w", err)
	}
	record.Valid = true
	record.BeforeSettingsJSON, record.AfterSettingsJSON = before, after
	verificationJSON, _ := json.Marshal(record)

	learning, err = v.store.TransitionLearning(ctx, learning.ID, store.LearningTransition{
		From: store.LearningCandidate, To: store.LearningVerified,
		VerificationJSON: string(verificationJSON),
	})
	if err != nil {
		return nil, err
	}
	if err := v.audit(ctx, session.EventLearningVerified, learning, ""); err != nil {
		return nil, err
	}
	learning, err = v.store.TransitionLearning(ctx, learning.ID, store.LearningTransition{
		From: store.LearningVerified, To: store.LearningProposed,
	})
	if err != nil {
		return nil, err
	}
	if err := v.audit(ctx, session.EventLearningProposed, learning, ""); err != nil {
		return nil, err
	}
	return learning, nil
}

type baselineWindow struct {
	WindowStartMs int64 `json:"window_start_ms"`
	WindowEndMs   int64 `json:"window_end_ms"`
}

func validateCandidate(l *store.Learning) (permissionContent, baselineWindow, string) {
	var content permissionContent
	var window baselineWindow
	if l.Kind != store.LearningPermissionRule || json.Unmarshal([]byte(l.ContentJSON), &content) != nil {
		return content, window, "invalid_content"
	}
	command, ok := eligibleCommandJSON(content)
	if !ok || content.Rule != "Bash("+command+")" {
		return content, window, "invalid_rule_syntax"
	}
	canonical, _ := json.Marshal(content)
	sum := sha256.Sum256(append([]byte(string(l.Kind)+"\x00"), canonical...))
	if hex.EncodeToString(sum[:]) != l.Fingerprint {
		return content, window, "fingerprint_mismatch"
	}
	if json.Unmarshal([]byte(l.BaselineJSON), &window) != nil || window.WindowEndMs <= window.WindowStartMs {
		return content, window, "invalid_baseline"
	}
	return content, window, ""
}

func eligibleCommandJSON(content permissionContent) (string, bool) {
	if content.Tool != "Bash" {
		return "", false
	}
	raw, _ := json.Marshal(map[string]any{
		"tool_name": "Bash", "tool_input": map[string]string{"command": content.Command},
		"redactions": []string{}, "truncated": false,
	})
	return eligibleCommand(string(raw))
}

func (v *Verifier) reject(ctx context.Context, learning *store.Learning, record verificationRecord, reason string) (*store.Learning, error) {
	record.Valid, record.Reason = false, reason
	verificationJSON, _ := json.Marshal(record)
	rejected, err := v.store.TransitionLearning(ctx, learning.ID, store.LearningTransition{
		From: store.LearningCandidate, To: store.LearningRejected,
		VerificationJSON: string(verificationJSON),
	})
	if err != nil {
		return nil, err
	}
	if err := v.audit(ctx, session.EventLearningRejected, rejected, reason); err != nil {
		return nil, err
	}
	return rejected, nil
}

func (v *Verifier) audit(ctx context.Context, kind session.EventKind, learning *store.Learning, reason string) error {
	data, _ := json.Marshal(map[string]any{
		"learning_id": learning.ID, "repo": learning.Repo,
		"fingerprint": learning.Fingerprint, "status": learning.Status, "reason": reason,
	})
	_, err := v.store.AppendEvent(ctx, "", kind, string(data))
	return err
}
