package repo

import (
	"encoding/json"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func (r *MemoryRepo) validateExtractionMemories(tx *gorm.DB, run domain.Run) error {
	ctx := run.Intelligence
	ids, err := domain.CanonicalIntelligenceIDs(run.ExecutionContext.TeamContext.MemoryIDs, domain.MemoryMaxRecallItems)
	if err != nil || len(ids) != len(ctx.Memories) {
		return ErrExtractionSource
	}
	for i, pinned := range ctx.Memories {
		if ids[i] != pinned.ID {
			return ErrExtractionSource
		}
		var row governedMemoryRow
		if err := r.locking(tx).Where("project_id = ? AND id = ?", ctx.ProjectID, pinned.ID).Take(&row).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		current, err := decodeGovernedMemory(row)
		if err != nil {
			return err
		}
		hash, err := domain.MemoryContentDigest(pinned)
		if err != nil || hash != pinned.SHA256 || current.State != domain.MemoryApproved || current.Revision != pinned.Revision || current.SHA256 != pinned.SHA256 || current.ExpiresAt != nil && !current.ExpiresAt.After(time.Now().UTC()) {
			return ErrIntelligenceUnavailable
		}
		if err := r.validateExtractionMemorySource(tx, current); err != nil {
			return err
		}
	}
	selected := run.ExecutionContext.TeamContext.MemoryRecallReceiptID
	if selected == "" {
		if ctx.MemoryRecallReceipt != nil {
			return ErrExtractionSource
		}
		return nil
	}
	pinned := ctx.MemoryRecallReceipt
	if pinned == nil || pinned.ID != selected || pinned.Scope.ProjectID != ctx.ProjectID || pinned.Scope.UserID != run.ExecutionContext.ResponsibleUserID {
		return ErrExtractionSource
	}
	var row memoryRecallReceiptRow
	if err := r.locking(tx).Where("id = ? AND project_id = ? AND user_id = ?", selected, ctx.ProjectID, pinned.Scope.UserID).Take(&row).Error; err != nil {
		return ErrIntelligenceUnavailable
	}
	var current domain.MemoryRecallReceipt
	if json.Unmarshal(row.Payload, &current) != nil {
		return ErrExtractionSource
	}
	hash, err := pinned.Digest()
	currentHash, currentErr := current.Digest()
	if err != nil || currentErr != nil || hash != pinned.SHA256 || currentHash != current.SHA256 || hash != currentHash {
		return ErrExtractionSource
	}
	return pinned.ValidatePins(ctx.Memories)
}

// V1 has no general Memory distribution floor. A known Run source may only
// re-enter extraction through a verified unrestricted extraction receipt; an
// arbitrary manually registered Run reference cannot prove that input closure.
// No source Run is locked recursively. Historical receipts are the retained
// proof, while the caller has already locked the current approved memory.
func (r *MemoryRepo) validateExtractionMemorySource(tx *gorm.DB, memory domain.GovernedMemory) error {
	source := memory.Source
	if source.ExtractionID == "" {
		if source.RunID != "" {
			return ErrExtractionRestricted
		}
		return nil
	}
	var row knowledgeExtractionRow
	if err := r.locking(tx).Where("project_id = ? AND id = ?", memory.ProjectID, source.ExtractionID).Take(&row).Error; err != nil {
		return ErrIntelligenceUnavailable
	}
	receipt, err := decodeKnowledgeExtraction(row)
	if err != nil {
		return err
	}
	if receipt.SchemaVersion != 1 || receipt.Policy != "v1-unrestricted-inputs-only" {
		return ErrExtractionRestricted
	}
	actual := receipt.Source
	if source.RunID != actual.RunID || source.ArtifactID != actual.ArtifactID || source.Repository != actual.Repository || source.Commit != actual.Commit || source.Path != actual.ArtifactPath {
		return ErrExtractionSource
	}
	matches := 0
	for _, item := range receipt.Items {
		if item.Kind == "memory" && item.CandidateID == memory.ID {
			if item.CandidateSHA256 != memory.SHA256 {
				return ErrExtractionSource
			}
			matches++
		}
	}
	if matches != 1 {
		return ErrExtractionSource
	}
	return nil
}

// Transaction-scoped primitive; source authorization is owned by the caller's
// locked extraction transaction. The public manual writer rejects this link.
func (r *MemoryRepo) insertMemoryCandidateTx(tx *gorm.DB, project string, in domain.GovernedMemory, actor string, now time.Time) (domain.GovernedMemory, error) {
	id, err := intelligenceID("memory-")
	if err != nil {
		return domain.GovernedMemory{}, err
	}
	out := domain.GovernedMemory{ID: id, ProjectID: project, Title: in.Title, Content: in.Content, Source: in.Source, ExpiresAt: in.ExpiresAt, State: domain.MemoryCandidate, Revision: 1, CreatedBy: actor, CreatedAt: now, Decisions: []domain.IntelligenceDecision{}}
	out.SHA256, err = domain.MemoryContentDigest(out)
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	err = tx.Create(&governedMemoryRow{ID: id, ProjectID: project, State: out.State, Revision: 1, Payload: raw, Created: now}).Error
	return out, err
}
