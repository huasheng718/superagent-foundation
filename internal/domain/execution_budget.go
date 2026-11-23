package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

var (
	ErrExecutionBudgetStopped     = errors.New("execution budget stopped")
	ErrExecutionBudgetUnsupported = errors.New("strict provider budget is unsupported")
)

// ExecutionBudgetPolicy is optional and frozen when a Run starts. Metered
// mode stops further work on observed usage; it cannot bound a provider bill
// incurred before the next report. Strict mode requires a provider-enforced
// reservation, which none of the current CLI adapters offers.
type ExecutionBudgetPolicy struct {
	Mode             string `json:"mode"`
	MaxTotalTokens   int64  `json:"maxTotalTokens,omitempty"`
	MaxCostMicrosUSD int64  `json:"maxCostMicrosUSD,omitempty"`
	Currency         string `json:"currency,omitempty"`
	PriceVersion     string `json:"priceVersion,omitempty"`
	MaxInvocations   int    `json:"maxInvocations"`
	MaxNoProgress    int    `json:"maxNoProgress,omitempty"`
}

type ExecutionBudgetReceipt struct {
	NodeID     string `json:"nodeId"`
	Attempt    int    `json:"attempt"`
	Provider   string `json:"provider"`
	UsageKnown bool   `json:"usageKnown"`
	SessionID  string `json:"sessionId,omitempty"`
	Finished   bool   `json:"finished"`
}

type ExecutionBudgetCursor struct {
	InputTokens   int64 `json:"inputTokens"`
	OutputTokens  int64 `json:"outputTokens"`
	TotalTokens   int64 `json:"totalTokens"`
	CostMicrosUSD int64 `json:"costMicrosUSD"`
}

type ExecutionBudgetProgress struct {
	Fingerprint string `json:"fingerprint"`
	Consecutive int    `json:"consecutive"`
}

// ExecutionBudgetState remains on the Run when nodes are retried/reset. The
// receipt/cursor maps are bounded by MaxInvocations and do not contain secrets.
type ExecutionBudgetState struct {
	PolicySHA256          string                             `json:"policySHA256"`
	ConsumedTokens        int64                              `json:"consumedTokens"`
	ConsumedCostMicrosUSD int64                              `json:"consumedCostMicrosUSD"`
	Invocations           int                                `json:"invocations"`
	StopReason            string                             `json:"stopReason,omitempty"`
	Receipts              map[string]ExecutionBudgetReceipt  `json:"receipts"`
	Cursors               map[string]ExecutionBudgetCursor   `json:"cursors"`
	Active                map[string]string                  `json:"active"`
	Progress              map[string]ExecutionBudgetProgress `json:"progress"`
}

func ValidateExecutionBudgetPolicy(p *ExecutionBudgetPolicy) error {
	if p == nil {
		return nil
	}
	if p.Mode != "strict" && p.Mode != "metered" {
		return errors.New("executionBudget.mode must be strict or metered")
	}
	if p.MaxTotalTokens < 0 || p.MaxCostMicrosUSD < 0 || p.MaxInvocations < 1 || p.MaxInvocations > 1000 || (p.MaxNoProgress != 0 && (p.MaxNoProgress < 2 || p.MaxNoProgress > 100)) {
		return errors.New("invalid execution budget limits")
	}
	if p.MaxTotalTokens == 0 && p.MaxCostMicrosUSD == 0 && p.MaxNoProgress == 0 {
		return errors.New("execution budget needs a token/cost cap or no-progress threshold")
	}
	if p.MaxCostMicrosUSD > 0 && (p.Currency != "USD" || strings.TrimSpace(p.PriceVersion) == "" || len(p.PriceVersion) > 128) {
		return errors.New("cost budget requires USD and an explicit priceVersion audit label")
	}
	if p.Currency != "" && p.Currency != "USD" {
		return errors.New("execution budget supports USD only; currency conversion is not inferred")
	}
	return nil
}

func ExecutionBudgetPolicySHA(p *ExecutionBudgetPolicy) string {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func NewExecutionBudgetState(p *ExecutionBudgetPolicy) (*ExecutionBudgetState, error) {
	if err := ValidateExecutionBudgetPolicy(p); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	return &ExecutionBudgetState{PolicySHA256: ExecutionBudgetPolicySHA(p), Receipts: map[string]ExecutionBudgetReceipt{}, Cursors: map[string]ExecutionBudgetCursor{}, Active: map[string]string{}, Progress: map[string]ExecutionBudgetProgress{}}, nil
}

func (s *ExecutionBudgetState) stop(reason string) error {
	if s.StopReason == "" {
		s.StopReason = reason
	}
	return fmt.Errorf("%w: %s", ErrExecutionBudgetStopped, s.StopReason)
}

func (s *ExecutionBudgetState) Check(p *ExecutionBudgetPolicy) error {
	if p == nil {
		if s != nil {
			return fmt.Errorf("%w: budget policy was removed", ErrExecutionBudgetStopped)
		}
		return nil
	}
	if err := ValidateExecutionBudgetPolicy(p); err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("%w: budget state unavailable", ErrExecutionBudgetStopped)
	}
	if s.Receipts == nil || s.Cursors == nil || s.Active == nil || s.Progress == nil || s.ConsumedTokens < 0 || s.ConsumedCostMicrosUSD < 0 || s.Invocations < 0 || len(s.Receipts) != s.Invocations || s.Invocations > p.MaxInvocations || len(s.Cursors) > s.Invocations {
		return s.stop("budget_state_invalid")
	}
	for id, receipt := range s.Receipts {
		if receipt.NodeID == "" || receipt.Attempt < 1 || (!receipt.Finished && s.Active[receipt.NodeID] != id) {
			return s.stop("budget_state_invalid")
		}
	}
	for node, id := range s.Active {
		if receipt, ok := s.Receipts[id]; !ok || receipt.Finished || receipt.NodeID != node {
			return s.stop("budget_state_invalid")
		}
	}
	if s.PolicySHA256 != ExecutionBudgetPolicySHA(p) {
		return s.stop("budget_policy_changed")
	}
	if s.StopReason != "" {
		return fmt.Errorf("%w: %s", ErrExecutionBudgetStopped, s.StopReason)
	}
	if p.MaxTotalTokens > 0 && s.ConsumedTokens >= p.MaxTotalTokens {
		return s.stop("token_budget_exhausted")
	}
	if p.MaxCostMicrosUSD > 0 && s.ConsumedCostMicrosUSD >= p.MaxCostMicrosUSD {
		return s.stop("cost_budget_exhausted")
	}
	return nil
}

// Begin reserves one invocation durably before the caller starts a provider.
// Replaying a started invocation after a crash is deliberately unknown; it
// never grants the same allowance twice or silently erases possible spend.
func (s *ExecutionBudgetState) Begin(p *ExecutionBudgetPolicy, id, nodeID, provider string, attempt int) error {
	if err := s.Check(p); err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	if id == "" || nodeID == "" || attempt < 1 {
		return s.stop("budget_identity_missing")
	}
	if p.Mode == "strict" && (p.MaxTotalTokens > 0 || p.MaxCostMicrosUSD > 0) {
		return fmt.Errorf("%w: %s has no provider-enforced reservation", ErrExecutionBudgetUnsupported, provider)
	}
	if _, exists := s.Receipts[id]; exists {
		return s.stop("budget_invocation_uncertain")
	}
	if previous := s.Active[nodeID]; previous != "" && !s.Receipts[previous].Finished {
		return s.stop("budget_invocation_uncertain")
	}
	if s.Invocations >= p.MaxInvocations {
		return s.stop("invocation_budget_exhausted")
	}
	s.Invocations++
	s.Receipts[id] = ExecutionBudgetReceipt{NodeID: nodeID, Attempt: attempt, Provider: provider}
	s.Active[nodeID] = id
	return nil
}

// Observe accepts one monotonic provider-session snapshot. Lower/duplicate
// counters never reduce spend. Cost uses conservative integer micro-USD
// rounding, and only provider-reported USD is eligible for a monetary cap.
func (s *ExecutionBudgetState) Observe(p *ExecutionBudgetPolicy, nodeID, sessionID string, usage TokenUsage) error {
	if p == nil {
		return nil
	}
	if s == nil {
		return fmt.Errorf("%w: budget state unavailable", ErrExecutionBudgetStopped)
	}
	if s.PolicySHA256 != ExecutionBudgetPolicySHA(p) {
		return s.stop("budget_policy_changed")
	}
	id := s.Active[nodeID]
	receipt, ok := s.Receipts[id]
	if !ok || receipt.Finished {
		return nil
	}
	if sessionID == "" || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.TotalTokens < 0 || usage.CostUSD < 0 || math.IsNaN(usage.CostUSD) || math.IsInf(usage.CostUSD, 0) {
		return s.stop("budget_usage_unknown")
	}
	if p.MaxCostMicrosUSD > 0 && usage.CostStatus != "reported" {
		return s.stop("budget_cost_unknown")
	}
	if usage.InputTokens > math.MaxInt64-usage.OutputTokens {
		return s.stop("budget_usage_overflow")
	}
	input, output := usage.InputTokens, usage.OutputTokens
	micros := int64(0)
	if usage.CostStatus == "reported" {
		if usage.CostUSD >= float64(math.MaxInt64)/1_000_000 {
			return s.stop("budget_usage_overflow")
		}
		micros = int64(math.Ceil(usage.CostUSD * 1_000_000))
	}
	if receipt.SessionID != "" && receipt.SessionID != sessionID {
		return s.stop("budget_session_changed")
	}
	key := nodeID + "\x00" + sessionID
	previous := s.Cursors[key]
	if previous.InputTokens < 0 || previous.OutputTokens < 0 || previous.TotalTokens < 0 || previous.CostMicrosUSD < 0 || s.Cursors == nil || s.ConsumedTokens < 0 || s.ConsumedCostMicrosUSD < 0 {
		return s.stop("budget_state_invalid")
	}
	current := ExecutionBudgetCursor{InputTokens: max(previous.InputTokens, input), OutputTokens: max(previous.OutputTokens, output), CostMicrosUSD: max(previous.CostMicrosUSD, micros)}
	if current.InputTokens > math.MaxInt64-current.OutputTokens {
		return s.stop("budget_usage_overflow")
	}
	current.TotalTokens = max(previous.TotalTokens, usage.TotalTokens, current.InputTokens+current.OutputTokens)
	deltaTokens := current.TotalTokens - previous.TotalTokens
	deltaCost := current.CostMicrosUSD - previous.CostMicrosUSD
	if deltaTokens > math.MaxInt64-s.ConsumedTokens || deltaCost > math.MaxInt64-s.ConsumedCostMicrosUSD {
		return s.stop("budget_usage_overflow")
	}
	s.ConsumedTokens += deltaTokens
	s.ConsumedCostMicrosUSD += deltaCost
	s.Cursors[key] = current
	receipt.UsageKnown = true
	receipt.SessionID = sessionID
	s.Receipts[id] = receipt
	return s.Check(p)
}

func (s *ExecutionBudgetState) Finish(p *ExecutionBudgetPolicy, id string) error {
	if p == nil {
		return nil
	}
	if s == nil {
		return fmt.Errorf("%w: budget state unavailable", ErrExecutionBudgetStopped)
	}
	receipt, ok := s.Receipts[id]
	if !ok {
		return s.stop("budget_identity_missing")
	}
	if (p.MaxTotalTokens > 0 || p.MaxCostMicrosUSD > 0) && !receipt.UsageKnown {
		return s.stop("budget_usage_unknown")
	}
	receipt.Finished = true
	s.Receipts[id] = receipt
	delete(s.Active, receipt.NodeID)
	return s.Check(p)
}

// RecordProgress receives a controller-derived complete product-tree digest,
// never an Agent's assertion or a filename-only patch inventory.
func (s *ExecutionBudgetState) RecordProgress(p *ExecutionBudgetPolicy, nodeID, fingerprint string, failed bool) error {
	if p == nil || p.MaxNoProgress == 0 {
		return nil
	}
	if err := s.Check(p); err != nil {
		return err
	}
	if !failed {
		delete(s.Progress, nodeID)
		return nil
	}
	if !ValidWorkspaceContextSHA256(fingerprint) {
		return s.stop("progress_evidence_unavailable")
	}
	progress := s.Progress[nodeID]
	if progress.Fingerprint == fingerprint {
		progress.Consecutive++
	} else {
		progress = ExecutionBudgetProgress{Fingerprint: fingerprint, Consecutive: 1}
	}
	s.Progress[nodeID] = progress
	if progress.Consecutive >= p.MaxNoProgress {
		return s.stop("no_progress")
	}
	return nil
}

func ValidateRunExecutionBudget(run Run) error {
	if err := run.ExecutionBudget.Check(run.ExecutionContext.ExecutionBudget); err != nil {
		return err
	}
	if run.ExecutionBudget != nil && len(run.ExecutionBudget.Active) > 0 {
		return fmt.Errorf("%w: unfinished invocation", ErrExecutionBudgetStopped)
	}
	return nil
}
