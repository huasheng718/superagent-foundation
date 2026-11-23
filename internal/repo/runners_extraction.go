package repo

import (
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func (r *RunnersRepo) lockExtractionTask(tx *gorm.DB, run domain.Run, a domain.Artifact, in domain.KnowledgeExtractionRequest) (domain.RunnerTask, error) {
	var row runnerTaskRow
	q := tx.Model(&runnerTaskRow{}).Where(r.sqlJSONText("$.runId")+" = ? AND "+r.sqlJSONText("$.nodeId")+" = ? AND COALESCE("+r.sqlJSONText("$.action")+", '') = ''", "$.runId", run.ID, "$.nodeId", in.NodeID, "$.action").Order("created_at DESC").Order("id DESC")
	if err := r.locking(q).Take(&row).Error; err != nil {
		return domain.RunnerTask{}, ErrExtractionSource
	}
	task := runnerTaskFromRow(row)
	n := run.Nodes[in.NodeID]
	inv := task.Invocation
	if task.Status != domain.RunnerTaskSuccess || task.DesiredState == domain.RunnerTaskDesiredCancel || !domain.ControlledDelivery(task.DeliveryPolicy) || task.Provider == "" || task.Provider == "terminal" || task.Provider == "mock" || inv == nil || inv.RunID != run.ID || inv.NodeID != in.NodeID || task.Action != "" || task.Attempt < 1 || n.Provider != task.Provider {
		return task, ErrExtractionSource
	}
	budgetReceipt, exists := run.ExecutionBudget.Receipts[inv.InvocationID]
	if !exists || !budgetReceipt.Finished || !budgetReceipt.UsageKnown || budgetReceipt.NodeID != n.NodeID || budgetReceipt.Attempt != n.Attempt || budgetReceipt.Provider != task.Provider || budgetReceipt.SessionID != n.SessionID {
		return task, extractionInvalid("task invocation must match the finished effective-attempt budget receipt")
	}

	if task.ExecutionPlacement == nil || task.Execution == nil || n.ExecutionPlacement == nil || task.Execution.NodeAttempt != n.Attempt || task.Execution.Role != domain.NodeTaskRoleAuthor || n.ExecutionTaskID != task.ID || n.ExecutionSequence != task.Execution.Sequence {
		return task, extractionInvalid("extraction v1 requires the current placed author task")
	}
	placement, placementErr := domain.CanonicalNodeExecutionPlacement(*task.ExecutionPlacement)
	currentPlacement, currentErr := domain.CanonicalNodeExecutionPlacement(*n.ExecutionPlacement)
	placementHash, _ := domain.IntelligenceDigest(placement)
	currentHash, _ := domain.IntelligenceDigest(currentPlacement)
	if placementErr != nil || currentErr != nil || placementHash != currentHash || placement.NodeAttempt != n.Attempt || placement.RunnerID != task.RunnerID || placement.ProjectID != run.ExecutionContext.ProjectID || placement.ResponsibleUserID != run.ExecutionContext.ResponsibleUserID || run.ExecutionContext.NodePlacements[in.NodeID].RunnerID != task.RunnerID {
		return task, ErrExtractionSource
	}

	if !domain.RunnerVersionSupportsGovernedContext(placement.RunnerVersion) || (run.Intelligence.MemoryRecallReceipt != nil && !domain.RunnerVersionSupportsMemoryRecallReceipt(placement.RunnerVersion)) {
		return task, extractionInvalid("frozen Runner version does not support the governed context/receipt")
	}
	for _, def := range run.WorkflowSnapshot.Nodes {
		if def.ID == in.NodeID && inv.Agent.ID != def.AgentID {
			return task, ErrExtractionSource
		}
	}
	if n.ExecutionDeadline == nil || !equalRunnerExecutionDeadline(task.ExecutionDeadline, n.ExecutionDeadline) || task.ExecutionDeadline == nil || task.ExecutionDeadline.Before(n.StartedAt) || task.ExecutionDeadline.Sub(n.StartedAt) > 10*time.Minute || n.FinishedAt.After(*task.ExecutionDeadline) {
		return task, extractionInvalid("a completed task within a frozen deadline of at most 10 minutes is required")
	}
	if len(inv.Inputs) > 0 || len(inv.ArchivedFiles) > 0 || len(inv.ContributionInputs) > 0 {
		return task, extractionInvalid("unmanifested upstream inputs are not supported in extraction v1")
	}
	if !inv.ReportOutputs || task.Outputs == nil {
		return task, ErrExtractionSource
	}
	outputPath := a.Path
	if a.RepositoryDir != "" {
		outputPath = a.RepositoryDir + "/" + a.Path
	}
	declared := false
	for _, p := range inv.OutputFiles {
		if p == outputPath {
			declared = true
		}
	}
	if !declared || task.Outputs.Files[outputPath] != a.Content {
		return task, ErrExtractionSource
	}
	// ReportOutputs intentionally clears the legacy artifact-upload contracts.
	// lockExtractionSource verifies the frozen workflow contract; the task must
	// declare the same output path and return the exact retained bytes above.
	descriptor, err := domain.IsolatedWorkspaceDescriptor(task)
	if err != nil {
		return task, ErrExtractionSource
	}
	hash, err := domain.RunnerContributionContextSHA(task)
	if err != nil || hash != descriptor.ContextSHA256 {
		return task, ErrExtractionSource
	}
	if err := domain.ValidateRunnerGovernedContextForRun(run, task); err != nil {
		return task, ErrExtractionSource
	}
	if err := domain.ValidateRunnerContextEvidence(task, task.Outputs); err != nil {
		return task, ErrExtractionSource
	}
	repos := inv.Repositories
	if len(repos) == 0 && inv.Repository != nil {
		repos = []domain.RunnerRepository{*inv.Repository}
	}
	rc := run.RepositoryContexts()[0]
	if len(repos) != 1 || repos[0].RepositoryID != in.RepositoryID || repos[0].CloneURL != rc.CloneURL || repos[0].Dir != rc.WorkspaceDir || repos[0].BaseSHA != in.ExpectedCommit || len(task.ContributionSpecs) != 1 || len(task.AcceptedContributions) != 1 {
		return task, ErrExtractionSource
	}
	spec := task.ContributionSpecs[0]
	receipt := task.AcceptedContributions[0]
	if spec.RepositoryID != in.RepositoryID || spec.BaseSHA != in.ExpectedCommit || spec.Dir != rc.WorkspaceDir || spec.ContextSHA256 != hash || !domain.ValidWorkspaceContextSHA256(receipt.SHA256) || receipt.Size < 1 || receipt.Size > domain.RunnerContributionMaxBytes || spec.MaxBytes < receipt.Size || spec.MaxBytes > domain.RunnerContributionMaxBytes || receipt.ID == "" || receipt.TaskID != task.ID || receipt.Attempt != task.Attempt || receipt.RepositoryID != in.RepositoryID || receipt.Dir != rc.WorkspaceDir || receipt.BaseSHA != in.ExpectedCommit || receipt.ResultSHA != in.ExpectedCommit || receipt.ContextSHA256 != hash || receipt.AcceptedAt.IsZero() {
		return task, ErrExtractionSource
	}
	if strings.TrimSpace(inv.Agent.ID) == "" {
		return task, ErrExtractionSource
	}
	if err := unrestrictedExtractionPayload(domain.TeamReleasePayload{Agents: []domain.AgentDefinition{inv.Agent}, Assets: inv.Assets}); err != nil {
		return task, err
	}
	return task, nil
}
