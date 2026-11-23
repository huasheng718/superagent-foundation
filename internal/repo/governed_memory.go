package repo

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// MemoryRepo owns governed knowledge candidates and decisions. It does not
// schedule runs or publish formal TeamAI releases.
type MemoryRepo struct {
	*deps
	runs *RunsRepo
}

type governedMemoryRow struct {
	ID        string    `gorm:"column:id;primaryKey;size:64"`
	ProjectID string    `gorm:"column:project_id;index:idx_governed_memory_scope,priority:1;size:64;not null"`
	State     string    `gorm:"column:state;index:idx_governed_memory_scope,priority:2;size:32;not null"`
	Revision  int       `gorm:"column:revision;not null"`
	Payload   []byte    `gorm:"column:payload;type:json;not null"`
	Created   time.Time `gorm:"column:created_at;not null"`
}

func (governedMemoryRow) TableName() string { return "governed_memories" }

func (r *MemoryRepo) CreateMemoryCandidate(projectID string, in domain.GovernedMemory, actor string) (domain.GovernedMemory, error) {
	out := domain.GovernedMemory{}
	if in.Source.ExtractionID != "" {
		return out, fmt.Errorf("extractionId is server-owned; use knowledge-extractions")
	}
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(in.Title) == "" || len(in.Title) > 200 || strings.TrimSpace(in.Content) == "" || len(in.Content) > domain.MemoryMaxContentBytes {
		return out, fmt.Errorf("project, actor, bounded title/content are required")
	}
	if err := domain.ValidateIntelligenceSource(in.Source); err != nil {
		return out, err
	}
	if in.Source.RunID == "" && in.Source.Repository == "" && in.Source.Path == "" {
		return out, fmt.Errorf("memory source provenance required")
	}
	if in.Source.ArtifactID != "" && in.Source.RunID == "" {
		return out, fmt.Errorf("artifact provenance requires runId")
	}
	now := time.Now().UTC()
	if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
		return out, fmt.Errorf("expiry must be in the future")
	}
	var project projectRow
	if err := r.DB.Where("id = ?", projectID).Take(&project).Error; err != nil {
		return out, ErrIntelligenceUnavailable
	}
	if in.Source.RunID != "" {
		if r.runs == nil {
			return out, ErrIntelligenceUnavailable
		}
		run, ok := r.runs.RunState(in.Source.RunID)
		if !ok || run.ExecutionContext.ProjectID != projectID {
			return out, ErrIntelligenceUnavailable
		}
		if in.Source.ArtifactID != "" {
			if _, found := r.runs.Artifact(in.Source.RunID, in.Source.ArtifactID); !found {
				return out, ErrIntelligenceUnavailable
			}
		}
	}
	in.Title = strings.TrimSpace(in.Title)
	return r.insertMemoryCandidateTx(r.DB, projectID, in, actor, now)
}
func decodeGovernedMemory(row governedMemoryRow) (domain.GovernedMemory, error) {
	var out domain.GovernedMemory
	if err := json.Unmarshal(row.Payload, &out); err != nil {
		return out, err
	}
	if row.ID != out.ID || row.ProjectID != out.ProjectID || row.State != out.State || row.Revision != out.Revision {
		return out, ErrIntelligenceConflict
	}
	digest, err := domain.MemoryContentDigest(out)
	if err != nil || digest != out.SHA256 {
		return out, fmt.Errorf("memory digest mismatch")
	}
	return out, nil
}
func (r *MemoryRepo) GovernedMemory(projectID, id string) (domain.GovernedMemory, error) {
	var row governedMemoryRow
	if err := r.DB.Where("project_id = ? AND id = ?", projectID, id).Take(&row).Error; err != nil {
		return domain.GovernedMemory{}, ErrIntelligenceUnavailable
	}
	return decodeGovernedMemory(row)
}
func (r *MemoryRepo) GovernedMemories(projectID string) ([]domain.GovernedMemory, error) {
	var rows []governedMemoryRow
	if err := r.DB.Where("project_id = ?", projectID).Order("created_at DESC, id DESC").Limit(100).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []domain.GovernedMemory{}
	for _, row := range rows {
		v, err := decodeGovernedMemory(row)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (r *MemoryRepo) DecideGovernedMemory(projectID, id string, revision int, decision, actor, reason string) (domain.GovernedMemory, error) {
	var out domain.GovernedMemory
	if revision <= 0 || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(reason) > 2000 {
		return out, fmt.Errorf("expectedRevision, authenticated actor and bounded reason required")
	}
	if decision != domain.MemoryApproved && decision != domain.MemoryRejected && decision != domain.MemoryRevoked {
		return out, fmt.Errorf("decision must be approved, rejected or revoked")
	}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var row governedMemoryRow
		if err := r.locking(tx).Where("project_id = ? AND id = ?", projectID, id).Take(&row).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		v, err := decodeGovernedMemory(row)
		if err != nil {
			return err
		}
		if v.Revision != revision {
			return ErrIntelligenceConflict
		}
		if !(v.State == domain.MemoryCandidate && (decision == domain.MemoryApproved || decision == domain.MemoryRejected) || v.State == domain.MemoryApproved && decision == domain.MemoryRevoked) {
			return ErrIntelligenceConflict
		}
		if decision == domain.MemoryApproved && v.ExpiresAt != nil && !v.ExpiresAt.After(time.Now().UTC()) {
			return ErrIntelligenceConflict
		}
		v.State = decision
		v.Revision++
		v.Decisions = append(v.Decisions, domain.IntelligenceDecision{Decision: decision, Actor: actor, Reason: reason, At: time.Now().UTC()})
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		res := tx.Model(&governedMemoryRow{}).Where("id = ? AND revision = ?", id, revision).Updates(map[string]any{"state": v.State, "revision": v.Revision, "payload": raw})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrIntelligenceConflict
		}
		out = v
		return nil
	})
	return out, err
}
func (r *MemoryRepo) RecallGovernedMemory(projectID, query string, limit int) ([]domain.GovernedMemory, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(query) == "" || len(query) > 1000 {
		return nil, fmt.Errorf("project and bounded nonempty query required")
	}
	if limit == 0 {
		limit = 5
	}
	if limit < 1 || limit > domain.MemoryMaxRecallItems {
		return nil, fmt.Errorf("recall limit must be 1..%d", domain.MemoryMaxRecallItems)
	}
	var rows []governedMemoryRow
	if err := r.DB.Where("project_id = ? AND state = ?", projectID, domain.MemoryApproved).Order("created_at DESC, id DESC").Limit(500).Find(&rows).Error; err != nil {
		return nil, err
	}
	type candidate struct {
		m     domain.GovernedMemory
		score int
	}
	matches := []candidate{}
	terms := strings.Fields(strings.ToLower(query))
	now := time.Now().UTC()
	for _, row := range rows {
		m, err := decodeGovernedMemory(row)
		if err != nil {
			return nil, err
		}
		if m.ExpiresAt != nil && !m.ExpiresAt.After(now) {
			continue
		}
		text := strings.ToLower(m.Title + "\n" + m.Content)
		score := 0
		for _, term := range terms {
			if strings.Contains(text, term) {
				score++
			}
		}
		if score > 0 {
			matches = append(matches, candidate{m, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	out := []domain.GovernedMemory{}
	bytes := 0
	for _, candidate := range matches {
		raw, _ := json.Marshal(candidate.m)
		if bytes+len(raw) > domain.MemoryMaxRecallBytes {
			continue
		}
		out = append(out, candidate.m)
		bytes += len(raw)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
