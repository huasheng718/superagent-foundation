package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
)

func (e *Engine) freezeExecutionBudget(run *domain.Run) error {
	policy := run.ExecutionContext.ExecutionBudget
	if policy == nil {
		return nil
	}
	copy := *policy
	run.ExecutionContext.ExecutionBudget = &copy
	state, err := domain.NewExecutionBudgetState(&copy)
	if err != nil {
		return err
	}
	if run.ExecutionContext.ParentRunID != "" {
		return errors.New("budgeted child SOP requires shared parent reservations and is not supported")
	}
	for _, node := range run.WorkflowSnapshot.Nodes {
		if node.Type == "sop" {
			return errors.New("budgeted Run cannot dispatch child SOP outside the shared ledger")
		}
		if copy.MaxTotalTokens > 0 || copy.MaxCostMicrosUSD > 0 {
			if node.Type == "script" || node.Type == "command" {
				return fmt.Errorf("%w: opaque script/command nodes have no trusted model usage ledger and cannot carry token/cost caps", domain.ErrExecutionBudgetUnsupported)
			}
			if copy.Mode == "strict" && node.Type != "approval" {
				return fmt.Errorf("%w: current Agent CLI adapters do not enforce provider-side reservations; select metered observed-usage control or omit the budget", domain.ErrExecutionBudgetUnsupported)
			}
		}
	}
	run.ExecutionBudget = state
	return nil
}

// mutateExecutionBudget serializes reservations with the existing Engine
// state writer and Run generation fence. PutRun's budget-aware revision fence
// also rejects a concurrent writer instead of losing consumed allowance.
func (e *Engine) mutateExecutionBudget(runID, nodeID string, attempt int, mutate func(*domain.Run) error) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	pending := e.takePendingObservations(runID)
	for retry := 0; retry < 3; retry++ {
		run, ok := e.repo.RunState(runID)
		if !ok {
			return errors.New("budget Run not found")
		}
		if run.CancelRequested || (run.OwnerInstanceID != "" && !e.ownsRun(run)) {
			return errors.New("budget Run authority unavailable")
		}
		if nodeID != "" && !nodeExecutionLive(run.Nodes[nodeID], attempt) {
			return errors.New("budget Node attempt is no longer active")
		}
		for _, item := range pending {
			e.applyObservation(&run, item.nodeID, item.event)
		}
		err := mutate(&run)
		if putErr := e.repo.PutRun(run); putErr != nil {
			if errors.Is(putErr, repo.ErrRunDecisionStale) {
				continue
			}
			e.restorePendingObservations(pending)
			return fmt.Errorf("persist execution budget: %w", putErr)
		}
		e.publish(runID)
		e.stopBudgetExecutions(run)
		return err
	}
	e.restorePendingObservations(pending)
	return fmt.Errorf("persist execution budget: %w", repo.ErrRunDecisionStale)
}

func (e *Engine) beginBudgetInvocation(runID, nodeID string, attempt int, id, provider string) error {
	run, ok := e.repo.RunState(runID)
	if !ok {
		return errors.New("budget Run not found")
	}
	if run.ExecutionContext.ExecutionBudget == nil {
		return nil
	}
	return e.mutateExecutionBudget(runID, nodeID, attempt, func(run *domain.Run) error {
		return run.ExecutionBudget.Begin(run.ExecutionContext.ExecutionBudget, id, nodeID, provider, attempt)
	})
}

func (e *Engine) finishBudgetInvocation(runID, nodeID string, attempt int, id string) error {
	run, ok := e.repo.RunState(runID)
	if !ok {
		return errors.New("budget Run not found")
	}
	if run.ExecutionContext.ExecutionBudget == nil {
		return nil
	}
	return e.mutateExecutionBudget(runID, nodeID, attempt, func(run *domain.Run) error {
		return run.ExecutionBudget.Finish(run.ExecutionContext.ExecutionBudget, id)
	})
}

func (e *Engine) stopBudgetExecutions(run domain.Run) {
	if run.ExecutionBudget == nil || run.ExecutionBudget.StopReason == "" {
		return
	}
	nodes := make(map[string]bool, len(run.Nodes))
	for id := range run.Nodes {
		nodes[id] = true
	}
	e.cancelNodeExecutions(run.ID, nodes)
}

func (e *Engine) budgetProgress(ctx context.Context, runID, nodeID, workspace string, executionErr error) error {
	run, ok := e.repo.RunState(runID)
	if !ok {
		return errors.New("budget Run not found")
	}
	policy := run.ExecutionContext.ExecutionBudget
	if policy == nil || policy.MaxNoProgress == 0 {
		return nil
	}
	attempt := run.Nodes[nodeID].Attempt
	fingerprint := ""
	if executionErr != nil {
		var err error
		fingerprint, err = e.progressFingerprint(ctx, run, nodeID, workspace, executionErr)
		if err != nil {
			fingerprint = ""
		}
	}
	return e.mutateExecutionBudget(runID, nodeID, attempt, func(run *domain.Run) error {
		return run.ExecutionBudget.RecordProgress(policy, nodeID, fingerprint, executionErr != nil)
	})
}

func (e *Engine) progressFingerprint(ctx context.Context, run domain.Run, nodeID, workspace string, _ error) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	trees := map[string]string{}
	if e.defaultExecutionRunner(run) == "" {
		for _, repository := range run.RepositoryContexts() {
			tree, err := gitwork.ProgressTreeSHA(ctx, repositoryWorkspacePath(workspace, repository))
			if err != nil {
				return "", err
			}
			trees[repository.RepositoryID] = tree
		}
	} else {
		var selected *domain.RunnerTask
		for _, task := range e.repo.RunnerTasksForRun(run.ID) {
			if task.NodeID != nodeID || task.Invocation == nil || task.Action != "" || task.Provider == "terminal" {
				continue
			}
			receipt, ok := run.ExecutionBudget.Receipts[task.Invocation.InvocationID]
			if !ok || !receipt.Finished || receipt.NodeID != nodeID || run.Nodes[nodeID] == nil || receipt.Attempt != run.Nodes[nodeID].Attempt {
				continue
			}
			if selected == nil || task.CreatedAt.After(selected.CreatedAt) {
				copy := task
				selected = &copy
			}
		}
		if selected == nil || selected.Status != domain.RunnerTaskSuccess || len(selected.AcceptedContributions) == 0 {
			return "", errors.New("accepted code tree unavailable for progress")
		}
		dir, err := os.MkdirTemp("", "agentflow-budget-progress-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		for i, receipt := range selected.AcceptedContributions {
			raw, size, ok := e.repo.RunnerContributionBlob(receipt.SHA256)
			if !ok || size != receipt.Size {
				return "", errors.New("progress bundle unavailable")
			}
			path := filepath.Join(dir, fmt.Sprintf("%d.bundle", i))
			if err := os.WriteFile(path, raw, 0600); err != nil {
				return "", err
			}
			checkout := filepath.Join(dir, fmt.Sprintf("%d-checkout", i))
			bundle := gitwork.ContributionBundle{Path: path, BaseSHA: receipt.BaseSHA, ResultSHA: receipt.ResultSHA, SHA256: receipt.SHA256, Size: receipt.Size}
			if err := gitwork.VerifyContribution(ctx, path, checkout, bundle, domain.RunnerContributionMaxExpandedBytes); err != nil {
				return "", err
			}
			tree, err := gitwork.VerifiedProgressTreeSHA(ctx, checkout)
			if err != nil {
				return "", err
			}
			trees[receipt.RepositoryID] = tree
		}
	}
	if len(trees) == 0 || len(trees) != len(run.RepositoryContexts()) {
		return "", errors.New("complete repository progress identity unavailable")
	}
	// Only independently read product content counts as progress. A changed
	// verifier error, diagnostic prose or exit code cannot buy another Loop.
	keys := make([]string, 0, len(trees))
	for k := range trees {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var code []string
	for _, k := range keys {
		code = append(code, k+":"+trees[k])
	}
	raw, _ := json.Marshal(code)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
