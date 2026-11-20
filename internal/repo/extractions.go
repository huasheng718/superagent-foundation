package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

var ErrExtractionForbidden = errors.New("current authenticated project membership is required for extraction")

var ErrExtractionSource = errors.New("extraction requires a current successful isolated model task, unchanged exact commit and matching declared output")
var ErrExtractionRestricted = errors.New("extraction v1 supports only unrestricted frozen inputs: deny policies or inherited origins are not supported")

type ExtractionsRepo struct {
	*deps
	runs     *RunsRepo
	runners  *RunnersRepo
	catalog  *CatalogRepo
	memory   *MemoryRepo
	projects *ProjectsRepo
	users    *UsersRepo
}
type knowledgeExtractionRow struct {
	ID        string    `gorm:"column:id;primaryKey;size:64"`
	ProjectID string    `gorm:"column:project_id;size:64;not null;index:idx_knowledge_extraction_project"`
	Payload   []byte    `gorm:"column:payload;type:json;not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
}

func (knowledgeExtractionRow) TableName() string { return "knowledge_extractions" }
func decodeKnowledgeExtraction(row knowledgeExtractionRow) (domain.KnowledgeExtractionReceipt, error) {
	var v domain.KnowledgeExtractionReceipt
	if json.Unmarshal(row.Payload, &v) != nil || v.ID != row.ID || v.ProjectID != row.ProjectID || v.ID != domain.KnowledgeExtractionID(v.ProjectID, v.Source) {
		return v, ErrIntelligenceConflict
	}
	hash, err := v.Digest()
	if err != nil || hash != v.SHA256 {
		return v, ErrIntelligenceConflict
	}
	return v, nil
}
func (r *ExtractionsRepo) KnowledgeExtraction(project, id string) (domain.KnowledgeExtractionReceipt, error) {
	var row knowledgeExtractionRow
	if err := r.DB.Where("project_id = ? AND id = ?", project, id).Take(&row).Error; err != nil {
		return domain.KnowledgeExtractionReceipt{}, ErrIntelligenceUnavailable
	}
	return decodeKnowledgeExtraction(row)
}

// Import consumes a completed Engine result; it never runs a model, schedules
// work, publishes resources or indexes candidates. All DB effects are atomic.
func (r *ExtractionsRepo) CreateKnowledgeExtraction(ctx context.Context, project string, in domain.KnowledgeExtractionRequest, actor KnowledgeExtractionActor) (domain.KnowledgeExtractionReceipt, bool, error) {
	var out domain.KnowledgeExtractionReceipt
	replayed := false
	if err := in.Validate(); err != nil {
		return out, false, err
	}
	if project == "" || strings.TrimSpace(actor.LoginName) == "" {
		return out, false, domain.ErrKnowledgeExtractionInvalid
	}
	err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := r.projects.lockExtractionProject(tx, project, actor, r.users); err != nil {
			return err
		}
		// Project serializes ingestion/approval. Source Run precedes node/task just
		// as placed admission and callback do. Consume only locking/current reads.
		run, artifact, err := r.runs.lockExtractionSource(tx, project, in)
		if err != nil {
			return err
		}
		task, err := r.runners.lockExtractionTask(tx, run, artifact, in)
		if err != nil {
			return err
		}
		if err := r.catalog.validateExtractionTeam(tx, run, task); err != nil {
			return err
		}
		if err := r.memory.validateExtractionMemories(tx, run); err != nil {
			return err
		}
		source := extractionSource(run, task, artifact, in.RepositoryID)
		id := domain.KnowledgeExtractionID(project, source)
		var existing knowledgeExtractionRow
		err = r.locking(tx).Where("id = ? AND project_id = ?", id, project).Take(&existing).Error
		if err == nil {
			out, err = decodeKnowledgeExtraction(existing)
			replayed = err == nil
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		parsed, err := domain.ParseKnowledgeSuggestions([]byte(artifact.Content), project)
		if err != nil {
			return err
		}
		out = domain.KnowledgeExtractionReceipt{ID: id, ProjectID: project, SchemaVersion: 1, Source: source, Policy: "v1-unrestricted-inputs-only", CreatedBy: actor.LoginName, CreatedByUserID: actor.UserID, CreatedAt: time.Now().UTC(), Items: []domain.KnowledgeExtractionItem{}}
		// Candidate writers belong to their original domains and use this same
		// transaction. They neither start nested transactions nor obtain authority.
		nativeSource := domain.IntelligenceSource{RunID: run.ID, ArtifactID: artifact.ID, Repository: source.Repository, Commit: source.Commit, Path: artifact.Path, ExtractionID: id}
		for _, s := range parsed.Suggestions {
			item := domain.KnowledgeExtractionItem{Key: s.Key, Kind: s.Kind, LicenseClaim: s.LicenseClaim, EvidencePaths: s.EvidencePaths, EvidenceStatus: "model-claimed-unverified"}
			if s.Kind == "memory" {
				v, err := r.memory.insertMemoryCandidateTx(tx, project, domain.GovernedMemory{Title: s.Title, Content: s.Content, Source: nativeSource}, actor.LoginName, out.CreatedAt)
				if err != nil {
					return err
				}
				item.CandidateID, item.CandidateSHA256 = v.ID, v.SHA256
			} else {
				v, err := r.catalog.insertSkillCandidateTx(tx, project, s.SkillPackage(project, nativeSource, "generated:"+source.AgentID), actor.LoginName, out.CreatedAt)
				if err != nil {
					return err
				}
				item.CandidateID, item.CandidateSHA256 = v.ID, v.SHA256
			}
			out.Items = append(out.Items, item)
		}
		out.SHA256, err = out.Digest()
		if err != nil {
			return err
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return tx.Create(&knowledgeExtractionRow{ID: id, ProjectID: project, Payload: raw, CreatedAt: out.CreatedAt}).Error
	})
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return out, replayed, err
}
func extractionSource(run domain.Run, task domain.RunnerTask, a domain.Artifact, repositoryID string) domain.KnowledgeExtractionSource {
	n := run.Nodes[task.NodeID]
	rc := run.RepositoryContexts()[0]
	receipt := task.AcceptedContributions[0]
	usage := n.Usage
	if usage.CostStatus == "" {
		usage.CostStatus = "unknown"
	}
	s := domain.KnowledgeExtractionSource{ResponsibleUserID: run.ExecutionContext.ResponsibleUserID, AcceptedContribution: receipt, InvocationID: task.Invocation.InvocationID, BudgetPolicy: *run.ExecutionContext.ExecutionBudget, BudgetReceipt: run.ExecutionBudget.Receipts[task.Invocation.InvocationID], RunID: run.ID, Generation: run.Generation, RunRevision: run.Revision, NodeID: n.NodeID, NodeAttempt: n.Attempt, TaskID: task.ID, TaskAttempt: task.Attempt, RunnerID: task.RunnerID, ArtifactID: a.ID, ArtifactSHA256: a.SHA256, ArtifactPath: a.Path, ArtifactSize: a.Size, RepositoryID: repositoryID, Repository: rc.CloneURL, Commit: receipt.ResultSHA, ContributionID: receipt.ID, ContributionContextSHA256: receipt.ContextSHA256, AgentID: task.Invocation.Agent.ID, ReleaseID: run.Intelligence.ReleaseID, ReleaseSHA256: run.Intelligence.ReleaseSHA256, IntelligenceSHA256: run.Intelligence.SHA256, Provider: task.Provider, ConfiguredModel: task.Invocation.Agent.Model, ObservedModel: n.Model, ModelObservation: "unknown", SessionID: n.SessionID, Usage: usage, BudgetPolicySHA256: domain.ExecutionBudgetPolicySHA(run.ExecutionContext.ExecutionBudget), StartedAt: n.StartedAt, FinishedAt: n.FinishedAt, Deadline: *task.ExecutionDeadline}
	if n.Model != "" {
		s.ModelObservation = "node-telemetry"
	}
	if task.ExecutionPlacement != nil {
		for _, rt := range task.ExecutionPlacement.Runtimes {
			if rt.Provider == task.Provider {
				s.RuntimeVersion = rt.Version
			}
		}
	}
	return s
}
func unrestrictedExtractionResource(v any) error {
	s := domain.DistributionOf(v)
	if err := s.Validate(); err != nil {
		return err
	}
	p := s.Effective()
	if len(s.DistributionOrigins) != 0 || p.Copy != "allow" || p.Edit != "allow" || p.Export != "allow" {
		return ErrExtractionRestricted
	}
	return nil
}

// Every known frozen input is checked, including the concrete invocation. No
// restriction is guessed away merely because the model says it did not copy.
func unrestrictedExtractionPayload(p domain.TeamReleasePayload) error {
	for _, a := range p.Agents {
		if err := unrestrictedExtractionResource(a); err != nil {
			return err
		}
	}
	for _, a := range p.Assets.Rules {
		if err := unrestrictedExtractionResource(a); err != nil {
			return err
		}
	}
	for _, a := range p.Assets.Skills {
		if err := unrestrictedExtractionResource(a); err != nil {
			return err
		}
	}
	for _, a := range p.Assets.MCPServers {
		if err := unrestrictedExtractionResource(a); err != nil {
			return err
		}
	}
	return nil
}
func extractionInvalid(why string) error { return fmt.Errorf("%w: %s", ErrExtractionSource, why) }
