package repo

import (
	"crypto/sha256"
	"fmt"
	"path"
	"strings"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func (r *RunsRepo) lockExtractionSource(tx *gorm.DB, project string, in domain.KnowledgeExtractionRequest) (domain.Run, domain.Artifact, error) {
	var run domain.Run
	var artifact domain.Artifact
	row, err := r.takeRunForUpdate(tx, in.RunID)
	if err != nil {
		return run, artifact, ErrIntelligenceUnavailable
	}
	run = runFromRow(row)
	if run.ExecutionContext.ProjectID != project {
		return run, artifact, ErrIntelligenceUnavailable
	}
	if run.Status != "succeeded" || run.CancelRequested || run.PauseRequested || !domain.ControlledDelivery(run.ExecutionContext.DeliveryPolicy) || run.ExecutionContext.RunnerWorkspaceLayout != domain.RunnerWorkspaceLayoutAttempt {
		return run, artifact, ErrExtractionSource
	}
	if err := r.loadRunNodesFrom(r.locking(tx), &run); err != nil {
		return run, artifact, err
	}
	n := run.Nodes[in.NodeID]
	if n == nil || n.NodeID != in.NodeID || n.Status != "succeeded" || n.Attempt < 1 || n.SessionID == "" || n.StartedAt.IsZero() || n.FinishedAt.Before(n.StartedAt) {
		return run, artifact, ErrExtractionSource
	}
	if len(run.RepositoryContexts()) != 1 || run.RepositoryContexts()[0].RepositoryID != in.RepositoryID || run.ExecutionContext.WorkspaceBaseSHAs[in.RepositoryID] != in.ExpectedCommit || len(run.ExecutionContext.WorkspaceBaseSHAs) != 1 {
		return run, artifact, ErrExtractionSource
	}
	if err := domain.ValidateIntelligenceSource(domain.IntelligenceSource{Repository: run.RepositoryContexts()[0].CloneURL, Commit: in.ExpectedCommit}); err != nil {
		return run, artifact, ErrExtractionSource
	}
	if run.ExecutionContext.TeamContext == nil || run.Intelligence == nil {
		return run, artifact, extractionInvalid("a frozen approved TeamAI context is required")
	}
	policy := run.ExecutionContext.ExecutionBudget
	if policy == nil || policy.MaxInvocations > 8 || policy.MaxTotalTokens < 1 || policy.MaxTotalTokens > 262144 {
		return run, artifact, extractionInvalid("extraction requires a frozen budget of 1..8 invocations and 1..262144 tokens")
	}
	if err := domain.ValidateRunExecutionBudget(run); err != nil {
		return run, artifact, err
	}
	var a runArtifactRow
	if err := r.locking(tx).Where("run_id = ? AND id = ?", run.ID, in.ArtifactID).Take(&a).Error; err != nil {
		return run, artifact, ErrIntelligenceUnavailable
	}
	artifact = artifactFromRow(a, true)
	sum := sha256.Sum256([]byte(artifact.Content))
	if artifact.NodeID != in.NodeID || artifact.Attempt != n.Attempt || artifact.Size != int64(len(artifact.Content)) || artifact.Size < 1 || artifact.Size > domain.KnowledgeExtractionMaxBytes || artifact.SHA256 != fmt.Sprintf("%x", sum) || artifact.SHA256 != in.ExpectedArtifactSHA256 || artifact.MediaType != "application/json" || path.Base(artifact.Path) != "knowledge-candidates.json" || !domain.SafeWorkspaceRelPath(artifact.Path) {
		return run, artifact, ErrExtractionSource
	}
	found := false
	for _, def := range run.WorkflowSnapshot.Nodes {
		if def.ID != in.NodeID {
			continue
		}
		if found || def.Type != "agent" || def.AgentID == "" || def.MaxAttempts != 1 {
			return run, artifact, ErrExtractionSource
		}
		for _, c := range def.ArtifactOutputs {
			if c.Name == artifact.Name && c.Path == artifact.Path && c.MediaType == artifact.MediaType && c.Required {
				found = true
			}
		}
	}
	if !found {
		return run, artifact, ErrExtractionSource
	}
	// Upstream files have no governed distribution manifest in v1. Refuse that
	// unprovable input seam rather than dropping possible known provenance.
	for _, edge := range run.WorkflowSnapshot.Edges {
		if edge.Target == in.NodeID {
			return run, artifact, extractionInvalid("upstream node inputs are not supported in extraction v1")
		}
	}
	if artifact.RepositoryDir != "" && artifact.RepositoryDir != run.RepositoryContexts()[0].WorkspaceDir {
		return run, artifact, ErrExtractionSource
	}
	if strings.TrimSpace(n.Provider) == "terminal" || n.Provider == "mock" {
		return run, artifact, ErrExtractionSource
	}
	return run, artifact, nil
}
