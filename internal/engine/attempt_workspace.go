package engine

import (
	"fmt"
	"strings"

	"agentflow/internal/domain"
)

func (e *Engine) freezeWorkspaceIsolation(context *domain.TelemetryContext, repositories []domain.RepositoryContext, interactive bool) error {
	layout := strings.TrimSpace(context.RunnerWorkspaceLayout)
	if layout != "" && layout != domain.RunnerWorkspaceLayoutShared && layout != domain.RunnerWorkspaceLayoutAttempt {
		return fmt.Errorf("unsupported runner workspace layout %q", layout)
	}
	context.RunnerWorkspaceLayout = layout
	if layout != domain.RunnerWorkspaceLayoutAttempt {
		if len(context.WorkspaceBaseSHAs) > 0 {
			return fmt.Errorf("workspaceBaseSHAs requires the explicit attempt workspace layout")
		}
		return nil
	}
	if !domain.ControlledDelivery(context.DeliveryPolicy) || context.ExecutionMode != domain.ExecutionModeRunner {
		return fmt.Errorf("attempt workspace layout requires controlled delivery on a Runner")
	}
	if interactive || context.ParentRunID != "" {
		return fmt.Errorf("attempt workspace layout does not yet support interactive or child-SOP continuation")
	}
	if context.ExecutionRunnerID != "" {
		if err := e.requireWorkspaceIsolation(context.ExecutionRunnerID); err != nil {
			return err
		}
	}
	pins := make(map[string]string, len(repositories))
	for _, repository := range repositories {
		if strings.TrimSpace(repository.CloneURL) == "" {
			continue
		}
		id := strings.TrimSpace(repository.RepositoryID)
		sha := context.WorkspaceBaseSHAs[id]
		if id == "" || pins[id] != "" || !domain.ValidWorkspaceBaseSHA(sha) {
			return fmt.Errorf("attempt workspace requires one explicit full SHA-1 base per repository: %q", id)
		}
		pins[id] = sha
	}
	if len(pins) != len(context.WorkspaceBaseSHAs) {
		return fmt.Errorf("workspaceBaseSHAs contains a repository outside the run's frozen allowlist")
	}
	context.WorkspaceBaseSHAs = pins
	return nil
}

func (e *Engine) requireWorkspaceIsolation(runnerID string) error {
	runner, ok := e.repo.Runner(runnerID)
	if !ok || !runner.SupportsWorkspaceIsolation() {
		return fmt.Errorf("attempt workspace layout requires Runner %s or newer", domain.RunnerVersionWorkspaceIsolation)
	}
	return nil
}

func (e *Engine) bindWorkspaceIsolation(run domain.Run, task *domain.RunnerTask) error {
	if run.ExecutionContext.RunnerWorkspaceLayout != domain.RunnerWorkspaceLayoutAttempt {
		return nil
	}
	if task.Action != "" {
		if task.Action == domain.RunnerTaskActionCleanupRun {
			task.Workspace = domain.IsolatedRunWorkspaceRel(run.ID)
			return nil
		}
		return fmt.Errorf("attempt workspace action %q requires an accepted integrated snapshot", task.Action)
	}
	if err := e.requireWorkspaceIsolation(task.RunnerID); err != nil {
		return err
	}
	inv := task.Invocation
	if inv == nil || inv.RunID != run.ID || inv.NodeID != task.NodeID || task.RunID != run.ID || task.NodeID == "" {
		return fmt.Errorf("isolated assignment must carry the frozen run and node identity")
	}
	inv.WorkspaceIsolation = &domain.RunnerWorkspaceIsolation{Version: domain.RunnerWorkspaceIsolationVersion}
	inv.ArchivedFiles = nil
	bind := func(repository *domain.RunnerRepository) error {
		sha := run.ExecutionContext.WorkspaceBaseSHAs[repository.RepositoryID]
		if !domain.ValidWorkspaceBaseSHA(sha) {
			return fmt.Errorf("isolated assignment is missing frozen baseSHA for %q", repository.RepositoryID)
		}
		repository.BaseSHA = sha
		return nil
	}
	for i := range inv.Repositories {
		if err := bind(&inv.Repositories[i]); err != nil {
			return err
		}
	}
	if inv.Repository != nil {
		if err := bind(inv.Repository); err != nil {
			return err
		}
	}
	task.Workspace = domain.IsolatedRunWorkspaceRel(run.ID)
	contextSHA, err := domain.RunnerContributionContextSHA(*task)
	if err != nil {
		return err
	}
	inv.WorkspaceIsolation.ContextSHA256 = contextSHA
	// Isolated source is handed off as accepted immutable commits. Every
	// assigned repository must produce a receipt, including a no-change commit.
	task.ContributionSpecs = nil
	repositories := inv.Repositories
	if len(repositories) == 0 && inv.Repository != nil {
		repositories = []domain.RunnerRepository{*inv.Repository}
	}
	for _, repository := range repositories {
		task.ContributionSpecs = append(task.ContributionSpecs, domain.RunnerContributionSpec{
			RepositoryID: repository.RepositoryID, Dir: repository.Dir,
			BaseSHA: repository.BaseSHA, ContextSHA256: contextSHA,
			MaxBytes: domain.RunnerContributionMaxBytes,
		})
	}
	return nil
}
