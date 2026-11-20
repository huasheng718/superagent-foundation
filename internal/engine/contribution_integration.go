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
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

// runnerContributionHandoff is a private, task-scoped projection of an
// already accepted contribution. It is intentionally not a public API or a
// second store; callers still obtain bytes from the existing content-addressed
// RunnerContributionBlob table.
type runnerContributionHandoff struct {
	Input  domain.RunnerContributionInput
	Bytes  []byte
	Source domain.RunnerContributionInputSource
}

// acceptedRunnerContributionHandoff validates and projects accepted source
// receipts for a controller-selected placed consumer. It refuses
// stale/superseded sources, corrupt or oversized blobs and cross-repository
// lineage. Credentials never enter the projection. The normal placed dispatch
// path constructs its per-repository inputs in node_execution_dispatch.go;
// this seam remains useful for controller flows that need to project one
// accepted source set without exposing storage or credentials.
func (e *Engine) acceptedRunnerContributionHandoff(controller domain.Run, repositoryID string, taskIDs []string) ([]runnerContributionHandoff, error) {
	receipts, err := e.acceptedIntegrationInputs(controller, repositoryID, taskIDs)
	if err != nil {
		return nil, err
	}
	out := make([]runnerContributionHandoff, 0, len(receipts))
	for _, receipt := range receipts {
		raw, size, ok := e.repo.RunnerContributionBlob(receipt.SHA256)
		if !ok || size != receipt.Size || size <= 0 || size > domain.RunnerContributionMaxBytes || int64(len(raw)) != receipt.Size {
			return nil, errors.New("accepted contribution bundle is missing, oversized or truncated")
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != receipt.SHA256 {
			return nil, errors.New("accepted contribution bundle hash mismatch")
		}
		sourceTask, ok := e.repo.RunnerTask(receipt.TaskID)
		if !ok || sourceTask.RunID == "" || sourceTask.NodeID == "" || sourceTask.Invocation == nil {
			return nil, errors.New("accepted contribution source task is unavailable")
		}
		sourceRun, ok := e.repo.RunState(sourceTask.RunID)
		if !ok || sourceRun.Nodes[sourceTask.NodeID] == nil || sourceRun.Nodes[sourceTask.NodeID].Attempt <= 0 {
			return nil, errors.New("accepted contribution source Node attempt is unavailable")
		}
		contextSHA, err := domain.RunnerContributionContextSHA(sourceTask)
		if err != nil || contextSHA != receipt.ContextSHA256 {
			return nil, errors.New("accepted contribution source context changed")
		}
		source := domain.RunnerContributionInputSource{RunID: sourceTask.RunID, NodeID: sourceTask.NodeID, TaskID: sourceTask.ID, ReceiptID: receipt.ID, NodeAttempt: sourceRun.Nodes[sourceTask.NodeID].Attempt, TaskAttempt: receipt.Attempt, ContextSHA256: receipt.ContextSHA256, BundleSHA256: receipt.SHA256, ResultSHA: receipt.ResultSHA}
		input := domain.RunnerContributionInput{RepositoryID: receipt.RepositoryID, BaseSHA: receipt.BaseSHA, HeadSHA: receipt.ResultSHA, BundleSHA256: receipt.SHA256, BundleSize: receipt.Size, Sources: []domain.RunnerContributionInputSource{source}}
		out = append(out, runnerContributionHandoff{Input: input, Bytes: append([]byte(nil), raw...), Source: source})
	}
	return out, nil
}

// IntegrateRunnerContributions performs a controller action over immutable,
// already accepted task results. It does not publish or finish any work item.
func (e *Engine) IntegrateRunnerContributions(ctx context.Context, runID, repositoryID string, taskIDs []string) (domain.ContributionIntegration, error) {
	var result domain.ContributionIntegration
	if len(taskIDs) == 0 || len(taskIDs) > 32 {
		return result, errors.New("integration requires 1..32 accepted tasks")
	}
	run, err := e.contributionIntegrationController(runID, repositoryID)
	if err != nil {
		return result, err
	}
	unlock, err := e.repo.LockIntegration(ctx, "contribution:"+repositoryID, 30*time.Second)
	if err != nil {
		return result, err
	}
	defer unlock()
	inputs, err := e.acceptedIntegrationInputs(run, repositoryID, taskIDs)
	if err != nil {
		return result, err
	}
	dir, err := os.MkdirTemp("", "agentflow-controller-integration-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	bundles := make([]gitwork.ContributionBundle, 0, len(inputs))
	for i, input := range inputs {
		raw, size, ok := e.repo.RunnerContributionBlob(input.SHA256)
		if !ok || size != input.Size {
			return result, errors.New("accepted contribution bytes unavailable")
		}
		path := filepath.Join(dir, fmt.Sprintf("input-%d.bundle", i))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			return result, err
		}
		bundles = append(bundles, gitwork.ContributionBundle{Path: path, BaseSHA: input.BaseSHA, ResultSHA: input.ResultSHA, SHA256: input.SHA256, Size: input.Size})
	}
	merged, err := gitwork.IntegrateContributionBundles(ctx, bundles, filepath.Join(dir, "checkout"), filepath.Join(dir, "aggregate.bundle"), 64<<20)
	if err != nil {
		return result, err
	}
	// Re-read source lifecycle after the potentially slow Git operation. A
	// cancelled or superseded Run must not acquire a prepared delivery record.
	current, err := e.contributionIntegrationController(runID, repositoryID)
	if err != nil {
		return result, err
	}
	currentInputs, err := e.acceptedIntegrationInputs(current, repositoryID, taskIDs)
	if err != nil {
		return result, err
	}
	before, _ := json.Marshal(inputs)
	after, _ := json.Marshal(currentInputs)
	if string(before) != string(after) {
		return result, errors.New("accepted integration inputs changed")
	}
	raw, err := os.ReadFile(merged.Path)
	if err != nil {
		return result, err
	}
	if err := e.repo.PutRunnerContributionBlob(merged.SHA256, raw); err != nil {
		return result, err
	}
	contextRaw, _ := json.Marshal(inputs)
	contextHash := sha256.Sum256(contextRaw)
	result = domain.ContributionIntegration{RunID: runID, RepositoryID: repositoryID, BaseSHA: merged.BaseSHA, ResultSHA: merged.ResultSHA, ContextSHA256: hex.EncodeToString(contextHash[:]), BundleSHA256: merged.SHA256, BundleSize: merged.Size, Inputs: inputs, State: "prepared"}
	manifestRaw, _ := json.Marshal(result)
	manifestHash := sha256.Sum256(manifestRaw)
	result.ID = hex.EncodeToString(manifestHash[:])
	result.ManifestSHA256 = result.ID
	return e.repo.PrepareContributionIntegration(result)
}

func (e *Engine) acceptedIntegrationInputs(controller domain.Run, repositoryID string, taskIDs []string) ([]domain.RunnerContributionReceipt, error) {
	seen := map[string]bool{}
	inputs := []domain.RunnerContributionReceipt{}
	for _, id := range taskIDs {
		if seen[id] {
			return nil, errors.New("duplicate integration task")
		}
		seen[id] = true
		task, ok := e.repo.RunnerTask(id)
		if !ok || task.Status != domain.RunnerTaskSuccess || task.DesiredState == domain.RunnerTaskDesiredCancel || !domain.ControlledDelivery(task.DeliveryPolicy) || task.Invocation == nil || task.Action != "" {
			return nil, errors.New("integration task is not accepted controlled execution")
		}
		source, ok := e.repo.RunState(task.RunID)
		if !ok || source.Status != "succeeded" || source.CancelRequested || source.ExecutionContext.ProjectID != controller.ExecutionContext.ProjectID {
			return nil, errors.New("source run is unavailable, cancelled or outside controller project")
		}
		if err := domain.ValidateRunExecutionBudget(source); err != nil {
			return nil, err
		}
		if err := e.ValidateRunIntelligence(source); err != nil {
			return nil, err
		}
		if source.ExecutionContext.RunnerWorkspaceLayout != domain.RunnerWorkspaceLayoutAttempt || !domain.ControlledDelivery(source.ExecutionContext.DeliveryPolicy) {
			return nil, errors.New("source run lacks frozen isolated contribution delivery")
		}
		frozen, err := contributionFrozenRepository(source, repositoryID)
		if err != nil {
			return nil, err
		}
		controllerRepository, err := contributionFrozenRepository(controller, repositoryID)
		if err != nil || frozen.CloneURL != controllerRepository.CloneURL || source.ExecutionContext.WorkspaceBaseSHAs[repositoryID] != controller.ExecutionContext.WorkspaceBaseSHAs[repositoryID] {
			return nil, errors.New("source repository differs from controller frozen repository or base")
		}
		node := source.Nodes[task.NodeID]
		if node == nil || node.Status != "succeeded" {
			return nil, errors.New("source node was not accepted")
		}
		descriptor, err := domain.IsolatedWorkspaceDescriptor(task)
		if err != nil {
			return nil, err
		}
		repositories := task.Invocation.Repositories
		if len(repositories) == 0 && task.Invocation.Repository != nil {
			repositories = []domain.RunnerRepository{*task.Invocation.Repository}
		}
		matches := 0
		for _, repository := range repositories {
			if repository.RepositoryID != repositoryID {
				continue
			}
			if repository.CloneURL != frozen.CloneURL || repository.Dir != frozen.WorkspaceDir || repository.BaseSHA != source.ExecutionContext.WorkspaceBaseSHAs[repositoryID] {
				return nil, errors.New("task repository differs from frozen source repository")
			}
			matches++
		}
		if matches != 1 {
			return nil, errors.New("task requires exactly one frozen contribution repository")
		}
		// Never use the capped management task list as an authority check.
		latest, err := e.repo.LatestRunnerExecutionTaskForNode(task.RunID, task.NodeID)
		if err != nil || latest.ID != task.ID {
			return nil, errors.New("integration task was superseded or latest execution is unavailable")
		}

		contextSHA, err := domain.RunnerContributionContextSHA(task)
		if err != nil {
			return nil, err
		}
		if contextSHA != descriptor.ContextSHA256 {
			return nil, errors.New("task differs from frozen isolated execution context")
		}
		count := 0
		for _, receipt := range task.AcceptedContributions {
			if receipt.RepositoryID != repositoryID {
				continue
			}
			if receipt.TaskID != id || receipt.Attempt != task.Attempt || receipt.AcceptedAt.IsZero() || receipt.ContextSHA256 != contextSHA || receipt.BaseSHA != source.ExecutionContext.WorkspaceBaseSHAs[repositoryID] || receipt.Dir != frozen.WorkspaceDir {
				return nil, errors.New("accepted contribution context or attempt mismatch")
			}
			if len(inputs) > 0 && inputs[0].BaseSHA != receipt.BaseSHA {
				return nil, errors.New("accepted contribution bases differ")
			}
			inputs = append(inputs, receipt)
			count++
		}
		if count != 1 {
			return nil, errors.New("exactly one accepted repository contribution required per task")
		}
	}
	return inputs, nil
}

func contributionFrozenRepository(run domain.Run, repositoryID string) (domain.RepositoryContext, error) {
	var selected domain.RepositoryContext
	count := 0
	for _, repository := range run.RepositoryContexts() {
		if repository.RepositoryID == repositoryID {
			selected = repository
			count++
		}
	}
	if count != 1 || selected.CloneURL == "" || !domain.ValidWorkspaceBaseSHA(run.ExecutionContext.WorkspaceBaseSHAs[repositoryID]) {
		return selected, errors.New("contribution repository or exact base is not frozen in run")
	}
	return selected, nil
}

func (e *Engine) contributionIntegrationController(runID, repositoryID string) (domain.Run, error) {
	run, ok := e.repo.RunState(runID)
	if !ok || run.Status != "succeeded" || run.CancelRequested || !e.controlledRun(run) || run.ExecutionContext.RunnerWorkspaceLayout != domain.RunnerWorkspaceLayoutAttempt {
		return run, errors.New("controller integration requires a successful controlled isolated run")
	}
	if err := domain.ValidateRunExecutionBudget(run); err != nil {
		return run, err
	}
	if err := e.ValidateRunIntelligence(run); err != nil {
		return run, err
	}
	repository, ok := e.repo.Repository(repositoryID)
	if !ok || repositoryID == "" || repository.ProjectID != run.ExecutionContext.ProjectID {
		return run, errors.New("integration repository is outside the controller run project")
	}
	if _, err := contributionFrozenRepository(run, repositoryID); err != nil {
		return run, err
	}
	return run, nil
}

// ValidateContributionIntegration rechecks provenance and current lifecycle
// before a prepared snapshot is reused. Callers must repeat this at any future
// publication boundary; a prepared record is not enduring publication authority.
// This read check does not serialize concurrent cancellation with an external
// write, nor does it establish review, test or business acceptance.
func (e *Engine) ValidateContributionIntegration(value domain.ContributionIntegration) error {
	if value.State != "prepared" || len(value.Inputs) == 0 || len(value.Inputs) > 32 {
		return errors.New("integration is not a prepared contribution snapshot")
	}
	controller, err := e.contributionIntegrationController(value.RunID, value.RepositoryID)
	if err != nil {
		return err
	}
	taskIDs := make([]string, 0, len(value.Inputs))
	for _, input := range value.Inputs {
		taskIDs = append(taskIDs, input.TaskID)
	}
	inputs, err := e.acceptedIntegrationInputs(controller, value.RepositoryID, taskIDs)
	if err != nil {
		return err
	}
	expected, _ := json.Marshal(value.Inputs)
	actual, _ := json.Marshal(inputs)
	contextHash := sha256.Sum256(actual)
	if string(expected) != string(actual) || value.ContextSHA256 != hex.EncodeToString(contextHash[:]) || value.BaseSHA != controller.ExecutionContext.WorkspaceBaseSHAs[value.RepositoryID] {
		return errors.New("prepared integration source context changed")
	}
	manifest := value
	manifest.ID, manifest.ManifestSHA256 = "", ""
	raw, _ := json.Marshal(manifest)
	manifestHash := sha256.Sum256(raw)
	if value.ID != hex.EncodeToString(manifestHash[:]) || value.ID != value.ManifestSHA256 {
		return errors.New("prepared integration manifest changed")
	}
	return nil
}
