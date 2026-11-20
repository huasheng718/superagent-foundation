package repo

import (
	"agentflow/internal/domain"
	"errors"
)

// Budgeted Runs use a revision fence for every header write. A stale usage,
// retry or observation snapshot must not reset already consumed allowances.
func preserveExecutionBudgetWrite(previous, next domain.Run) error {
	if previous.ExecutionContext.ExecutionBudget == nil && previous.ExecutionBudget == nil {
		if next.ExecutionContext.ExecutionBudget != nil || next.ExecutionBudget != nil {
			return errors.New("execution budget cannot be added after Run creation")
		}
		return nil
	}
	if domain.ExecutionBudgetPolicySHA(previous.ExecutionContext.ExecutionBudget) != domain.ExecutionBudgetPolicySHA(next.ExecutionContext.ExecutionBudget) {
		return errors.New("execution budget policy is immutable")
	}
	if next.Revision != previous.Revision+1 {
		return ErrRunDecisionStale
	}
	if previous.ExecutionBudget == nil || next.ExecutionBudget == nil {
		return errors.New("execution budget state cannot be removed")
	}
	a, b := previous.ExecutionBudget, next.ExecutionBudget
	if a.PolicySHA256 != b.PolicySHA256 || b.ConsumedTokens < a.ConsumedTokens || b.ConsumedCostMicrosUSD < a.ConsumedCostMicrosUSD || b.Invocations < a.Invocations || (a.StopReason != "" && a.StopReason != b.StopReason) {
		return errors.New("execution budget state cannot move backwards")
	}
	for id, receipt := range a.Receipts {
		n, ok := b.Receipts[id]
		if !ok || n.NodeID != receipt.NodeID || n.Attempt != receipt.Attempt || n.Provider != receipt.Provider || (receipt.Finished && !n.Finished) || (receipt.UsageKnown && !n.UsageKnown) || (receipt.SessionID != "" && receipt.SessionID != n.SessionID) {
			return errors.New("execution budget invocation receipt cannot be removed or rewritten")
		}
	}
	for node, id := range a.Active {
		if receipt, ok := b.Receipts[id]; !ok || (!receipt.Finished && b.Active[node] != id) {
			return errors.New("unfinished execution budget invocation cannot be detached")
		}
	}
	for key, cursor := range a.Cursors {
		n, ok := b.Cursors[key]
		if !ok || n.InputTokens < cursor.InputTokens || n.OutputTokens < cursor.OutputTokens || n.TotalTokens < cursor.TotalTokens || n.CostMicrosUSD < cursor.CostMicrosUSD {
			return errors.New("execution budget usage watermark cannot move backwards")
		}
	}
	return nil
}
