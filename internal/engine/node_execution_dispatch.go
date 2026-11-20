package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

// preparePlacedExecutionTask is the Engine side of the placed queue boundary.
// It is deliberately private: callers cannot provide author sequence or Git
// input provenance. The queue transaction remains the final authority and
// rechecks every field after taking the Run/task locks.
func (e *Engine) preparePlacedExecutionTask(run domain.Run, task *domain.RunnerTask, role string) error {
	if task == nil || len(run.ExecutionContext.NodePlacements) == 0 {
		return nil
	}
	current, ok := e.repo.RunState(run.ID)
	if !ok {
		return fmt.Errorf("placed execution Run %s disappeared", run.ID)
	}
	node := current.Nodes[task.NodeID]
	if node == nil || node.ExecutionPlacement == nil || node.Attempt <= 0 {
		return fmt.Errorf("placed execution node %s has no frozen attempt", task.NodeID)
	}
	if task.RunnerID != node.ExecutionPlacement.RunnerID {
		return fmt.Errorf("placed execution runner differs from the frozen node target")
	}
	placement := *node.ExecutionPlacement
	task.ExecutionPlacement = &placement
	if task.Invocation == nil {
		return fmt.Errorf("placed execution requires an invocation")
	}
	previous := node.ExecutionTaskID
	sequence := node.ExecutionSequence + 1
	if role == domain.NodeTaskRoleVerification {
		if previous == "" || node.ExecutionSequence < 1 {
			return fmt.Errorf("verification requires a current author")
		}
		sequence = node.ExecutionSequence
	}
	task.Execution = &domain.NodeTaskExecution{
		Role: role, NodeAttempt: node.Attempt, Sequence: sequence,
		PreviousAuthorTaskID: previous,
	}
	inputs, err := e.preparePlacedContributionInputs(current, task.NodeID, previous)
	if err != nil {
		return err
	}
	task.Invocation.ContributionInputs = inputs
	return nil
}

// preparePlacedContributionInputs builds one immutable input per frozen
// repository. Source order is deterministic; fan-in bundles are merged into
// the existing content-addressed store, while each source receipt remains in
// the input manifest for Repo-side lineage validation.
func (e *Engine) preparePlacedContributionInputs(run domain.Run, nodeID, previousAuthorID string) ([]domain.RunnerContributionInput, error) {
	sourceIDs := []string{}
	if previousAuthorID != "" {
		sourceIDs = append(sourceIDs, previousAuthorID)
	} else {
		ids, err := executionPredecessorTaskIDs(run, nodeID)
		if err != nil {
			return nil, err
		}
		sourceIDs = append(sourceIDs, ids...)
	}
	if len(sourceIDs) == 0 {
		return nil, nil
	}
	sort.Strings(sourceIDs)
	inputs := make([]domain.RunnerContributionInput, 0, len(run.RepositoryContexts()))
	for _, repository := range run.RepositoryContexts() {
		base := run.ExecutionContext.WorkspaceBaseSHAs[repository.RepositoryID]
		if !domain.ValidWorkspaceBaseSHA(base) {
			return nil, fmt.Errorf("placed execution baseline for %q is invalid", repository.RepositoryID)
		}
		type sourceBundle struct {
			receipt domain.RunnerContributionReceipt
			raw     []byte
		}
		bundles := make([]sourceBundle, 0, len(sourceIDs))
		sources := make([]domain.RunnerContributionInputSource, 0, len(sourceIDs))
		seen := map[string]bool{}
		for _, sourceID := range sourceIDs {
			if seen[sourceID] {
				continue
			}
			seen[sourceID] = true
			sourceTask, found := e.repo.RunnerTask(sourceID)
			if !found || sourceTask.RunID != run.ID || sourceTask.Status != domain.RunnerTaskSuccess || sourceTask.Invocation == nil || sourceTask.Execution == nil || sourceTask.Execution.Role != domain.NodeTaskRoleAuthor {
				return nil, fmt.Errorf("placed execution predecessor %s is unavailable", sourceID)
			}
			sourceNode := run.Nodes[sourceTask.NodeID]
			if sourceNode == nil || sourceNode.Attempt != sourceTask.Execution.NodeAttempt || sourceNode.ExecutionPlacement == nil {
				return nil, fmt.Errorf("placed execution predecessor %s has no current attempt", sourceID)
			}
			if sourceTask.NodeID == nodeID && sourceTask.Execution.NodeAttempt != run.Nodes[nodeID].Attempt {
				return nil, fmt.Errorf("placed execution predecessor %s belongs to another node attempt", sourceID)
			}
			var receipt domain.RunnerContributionReceipt
			count := 0
			for _, candidate := range sourceTask.AcceptedContributions {
				if candidate.RepositoryID == repository.RepositoryID {
					receipt = candidate
					count++
				}
			}
			if count != 1 || receipt.AcceptedAt.IsZero() || receipt.BaseSHA != base {
				return nil, fmt.Errorf("placed execution predecessor %s lacks accepted %s contribution", sourceID, repository.RepositoryID)
			}
			raw, size, found := e.repo.RunnerContributionBlob(receipt.SHA256)
			if !found || size != receipt.Size || size <= 0 || size > domain.RunnerContributionMaxBytes || int64(len(raw)) != receipt.Size {
				return nil, fmt.Errorf("placed execution predecessor %s has an invalid %s bundle", sourceID, repository.RepositoryID)
			}
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != receipt.SHA256 {
				return nil, fmt.Errorf("placed execution predecessor %s has a corrupt %s bundle", sourceID, repository.RepositoryID)
			}
			contextSHA, err := domain.RunnerContributionContextSHA(sourceTask)
			if err != nil {
				return nil, err
			}
			sources = append(sources, domain.RunnerContributionInputSource{
				RunID: run.ID, NodeID: sourceTask.NodeID, TaskID: sourceTask.ID,
				ReceiptID: receipt.ID, NodeAttempt: sourceTask.Execution.NodeAttempt,
				TaskAttempt: receipt.Attempt, ContextSHA256: contextSHA,
				BundleSHA256: receipt.SHA256, ResultSHA: receipt.ResultSHA,
			})
			bundles = append(bundles, sourceBundle{receipt: receipt, raw: raw})
		}
		if len(bundles) == 0 {
			return nil, fmt.Errorf("placed execution predecessor set is empty for %s", repository.RepositoryID)
		}
		merged := bundles[0].receipt
		mergedRaw := bundles[0].raw
		if len(bundles) > 1 {
			root, err := os.MkdirTemp("", "agentflow-execution-input-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(root)
			parts := make([]gitwork.ContributionBundle, 0, len(bundles))
			for index, bundle := range bundles {
				path := filepath.Join(root, fmt.Sprintf("input-%d.bundle", index))
				if err := os.WriteFile(path, bundle.raw, 0600); err != nil {
					return nil, err
				}
				parts = append(parts, gitwork.ContributionBundle{Path: path, BaseSHA: bundle.receipt.BaseSHA, ResultSHA: bundle.receipt.ResultSHA, SHA256: bundle.receipt.SHA256, Size: bundle.receipt.Size})
			}
			combined, err := gitwork.IntegrateContributionBundles(context.Background(), parts, filepath.Join(root, "checkout"), filepath.Join(root, "aggregate.bundle"), domain.RunnerContributionMaxBytes)
			if err != nil {
				return nil, fmt.Errorf("merge placed execution predecessors for %s: %w", repository.RepositoryID, err)
			}
			merged = domain.RunnerContributionReceipt{BaseSHA: combined.BaseSHA, ResultSHA: combined.ResultSHA, SHA256: combined.SHA256, Size: combined.Size}
			mergedRaw, err = os.ReadFile(combined.Path)
			if err != nil {
				return nil, err
			}
		}
		if err := e.repo.PutRunnerContributionBlob(merged.SHA256, mergedRaw); err != nil {
			return nil, err
		}
		inputs = append(inputs, domain.RunnerContributionInput{RepositoryID: repository.RepositoryID, BaseSHA: base, HeadSHA: merged.ResultSHA, BundleSHA256: merged.SHA256, BundleSize: merged.Size, Sources: sources})
	}
	return inputs, nil
}

func executionPredecessorTaskIDs(run domain.Run, nodeID string) ([]string, error) {
	definitions := map[string]domain.WorkflowNode{}
	for _, node := range run.WorkflowSnapshot.Nodes {
		definitions[node.ID] = node
	}
	seenNodes := map[string]bool{nodeID: true}
	seenTasks := map[string]bool{}
	pending := []string{nodeID}
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		for _, edge := range run.WorkflowSnapshot.Edges {
			if edge.Target != current || (edge.When != "" && edge.When != "success") || seenNodes[edge.Source] {
				continue
			}
			seenNodes[edge.Source] = true
			definition, ok := definitions[edge.Source]
			if !ok {
				return nil, fmt.Errorf("workflow predecessor %s is missing", edge.Source)
			}
			if definition.Type == "approval" {
				pending = append(pending, edge.Source)
				continue
			}
			node := run.Nodes[edge.Source]
			if node == nil || node.Status != "succeeded" || node.ExecutionTaskID == "" || node.ExecutionSequence < 1 {
				return nil, fmt.Errorf("workflow predecessor %s has no accepted author", edge.Source)
			}
			seenTasks[node.ExecutionTaskID] = true
		}
	}
	ids := make([]string, 0, len(seenTasks))
	for id := range seenTasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (e *Engine) queuePlacedOrLegacyTask(task domain.RunnerTask) error {
	if task.Execution != nil || task.ExecutionPlacement != nil {
		return e.repo.QueueNodeExecutionTask(task)
	}
	return e.repo.PutRunnerTask(task)
}
