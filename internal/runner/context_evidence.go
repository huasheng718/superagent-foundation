package runner

import (
	"agentflow/internal/domain"
	"agentflow/internal/runtime"
	"fmt"
)

func measureTaskContext(task domain.RunnerTask, workspace string, materialized bool) (*domain.RunnerContextEvidence, error) {
	if task.Invocation == nil || task.Invocation.GovernedContext == nil {
		return nil, nil
	}
	if err := domain.ValidateRunnerGovernedContext(task); err != nil {
		return nil, err
	}
	sha, err := domain.RunnerContributionContextSHA(task)
	if err != nil {
		return nil, err
	}
	if isolation := task.Invocation.WorkspaceIsolation; isolation != nil && isolation.ContextSHA256 != sha {
		return nil, fmt.Errorf("governed assignment context digest mismatch")
	}
	if task.Attempt < 1 {
		return nil, fmt.Errorf("governed evidence requires a claimed attempt")
	}
	files := []domain.RunnerMaterializedDigest{}
	if materialized {
		files, err = runtime.MeasureMaterializedResources(workspace, task.Invocation.Agent, task.Invocation.Assets)
		if err != nil {
			return nil, err
		}
	} else if len(task.Invocation.Assets.Skills) > 0 || len(task.Invocation.Assets.Rules) > 0 {
		return nil, fmt.Errorf("terminal governed task cannot silently omit assigned instruction resources")
	}
	return &domain.RunnerContextEvidence{Version: 1, TaskID: task.ID, Attempt: task.Attempt, IntelligenceSHA256: task.Invocation.GovernedContext.IntelligenceSHA256, ContextSHA256: sha, Files: files}, nil
}
func attachContextEvidence(outputs *domain.RunnerTaskOutputs, evidence *domain.RunnerContextEvidence) *domain.RunnerTaskOutputs {
	if evidence == nil {
		return outputs
	}
	if outputs == nil {
		outputs = &domain.RunnerTaskOutputs{}
	}
	outputs.ContextEvidence = evidence
	return outputs
}
