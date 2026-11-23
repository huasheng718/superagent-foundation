package repo

import (
	"agentflow/internal/domain"
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"strings"
	"time"
)

func decodeSkillCandidate(row skillCandidateRow) (domain.SkillCandidate, error) {
	var v domain.SkillCandidate
	if err := json.Unmarshal(row.Payload, &v); err != nil {
		return v, err
	}
	if row.ID != v.ID || row.ProjectID != v.ProjectID || row.State != v.State || row.Revision != v.Revision {
		return v, ErrIntelligenceConflict
	}
	if err := domain.ValidateSkillCandidatePackage(v.Package, v.ProjectID); err != nil {
		return v, err
	}
	hash, err := domain.IntelligenceDigest(v.Package)
	if err != nil || hash != v.SHA256 {
		return v, fmt.Errorf("candidate retained package digest mismatch")
	}
	v.Resources = domain.SkillCandidateResources(v.Package)
	return v, nil
}
func (r *CatalogRepo) CreateSkillCandidate(projectID string, input domain.SkillCandidatePackage, actor string) (domain.SkillCandidate, error) {
	return r.createSkillCandidate(projectID, input, actor, nil)
}

// The HTTP owner resolves the actual scoped Run. The request body can never
// supply this snapshot or its policy; native governance remains server-owned.
func (r *CatalogRepo) CreateSkillCandidateFromRun(projectID string, input domain.SkillCandidatePackage, actor string, source domain.Run) (domain.SkillCandidate, error) {
	return r.createSkillCandidate(projectID, input, actor, &source)
}

func (r *CatalogRepo) createSkillCandidate(projectID string, input domain.SkillCandidatePackage, actor string, source *domain.Run) (domain.SkillCandidate, error) {
	var v domain.SkillCandidate
	if input.Source.ExtractionID != "" {
		return v, fmt.Errorf("extractionId is server-owned; use knowledge-extractions")
	}
	// The authenticated route supplies project and actor; body governance fields
	// have no corresponding request fields. Preserve exact package resource bytes.
	input.Skill.ProjectID = projectID
	if input.Skill.Distribution != nil || len(input.Skill.DistributionOrigins) != 0 {
		return v, fmt.Errorf("%w: candidate distribution is server-derived", domain.ErrDistributionInvalid)
	}
	if actor == "" {
		return v, fmt.Errorf("authenticated actor required")
	}
	if err := domain.ValidateSkillCandidatePackage(input, projectID); err != nil {
		return v, err
	}
	if input.Source.RunID != "" && (source == nil || source.ID != input.Source.RunID || source.ExecutionContext.ProjectID != projectID) {
		return v, ErrIntelligenceUnavailable
	}
	var p projectRow
	if err := r.DB.Where("id = ?", projectID).Take(&p).Error; err != nil {
		return v, ErrIntelligenceUnavailable
	}
	var previous *domain.SkillCandidate
	if input.SupersedesID != "" {
		prior, err := r.SkillCandidate(projectID, input.SupersedesID)
		if err != nil {
			return v, err
		}
		previous = &prior
	}
	var err error
	input.Skill.DistributionState, err = candidateDistribution(input, previous, source)
	if err != nil {
		return v, err
	}
	if err := domain.ValidateSkillCandidatePackage(input, projectID); err != nil {
		return v, err
	}
	return r.insertSkillCandidateTx(r.DB, projectID, input, actor, time.Now().UTC())
}
func (r *CatalogRepo) SkillCandidate(projectID, id string) (domain.SkillCandidate, error) {
	var row skillCandidateRow
	if err := r.DB.Where("project_id = ? AND id = ?", projectID, id).Take(&row).Error; err != nil {
		return domain.SkillCandidate{}, ErrIntelligenceUnavailable
	}
	return decodeSkillCandidate(row)
}
func (r *CatalogRepo) SkillCandidates(projectID string) ([]domain.SkillCandidate, error) {
	var rows []skillCandidateRow
	if err := r.DB.Where("project_id = ?", projectID).Order("created_at DESC,id DESC").Limit(100).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []domain.SkillCandidate{}
	for _, row := range rows {
		v, err := decodeSkillCandidate(row)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (r *CatalogRepo) DecideSkillCandidate(projectID, id string, revision int, decision, actor, reason string) (domain.SkillCandidate, error) {
	var out domain.SkillCandidate
	if revision < 1 || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(reason) > 2000 || (decision != "approved" && decision != "rejected" && decision != "withdrawn") {
		return out, fmt.Errorf("expectedRevision, explicit decision, authenticated actor and bounded reason required")
	}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		// Serialize project approvals before candidate/Skill rows. The active Skill
		// insert additionally enforces its global unique ID against other writers.
		var project projectRow
		if err := r.locking(tx).Where("id = ?", projectID).Take(&project).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		var row skillCandidateRow
		if err := r.locking(tx).Where("project_id = ? AND id = ?", projectID, id).Take(&row).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		v, err := decodeSkillCandidate(row)
		if err != nil {
			return err
		}
		if v.Revision != revision || v.State != domain.SkillCandidateQuarantined {
			return ErrIntelligenceConflict
		}
		if decision == "approved" {
			scoped := &CatalogRepo{deps: &deps{DB: tx}}
			for _, active := range scoped.Skills() {
				if domain.RuntimeResourceName(active.ID) == v.Package.Skill.ID {
					return ErrIntelligenceConflict
				}
			}
			skill := v.Package.Skill
			now := time.Now().UTC()
			skill.Source = &domain.SkillSource{Provider: "reviewed-candidate", Namespace: v.Package.Publisher, Slug: v.ID, Version: v.SHA256, Homepage: v.Package.Source.Repository, InstalledAt: now, UpdatedAt: now, ReviewedCandidate: &domain.ReviewedSkillProvenance{CandidateID: v.ID, PackageSHA256: v.SHA256, Source: v.Package.Source, Publisher: v.Package.Publisher, License: v.Package.License, NoticePaths: append([]string{}, v.Package.NoticePaths...)}}
			files := []SkillFileInput{}
			for _, ref := range skill.FileRefs {
				files = append(files, SkillFileInput{Path: ref.Path, Content: v.Package.Binary[ref.Path], Mode: ref.Mode})
			}
			// Existing snapshot writer requires a nonempty binary file set. Promote
			// text SKILL.md through that same writer without altering its source bytes.
			if len(files) == 0 {
				files = append(files, SkillFileInput{Path: "SKILL.md", Content: domain.SkillCandidateEntry(v.Package), Mode: 0o644})
				textFiles := map[string]string{}
				for path, text := range skill.Files {
					if path != "SKILL.md" {
						textFiles[path] = text
					}
				}
				skill.Files = textFiles
			}
			if err := scoped.PutSkillSnapshot(skill, files, true); err != nil {
				return err
			}
			v.PromotedSkillID = skill.ID
		}
		v.State = decision
		v.Revision++
		v.Decisions = append(v.Decisions, domain.IntelligenceDecision{Decision: decision, Actor: actor, Reason: reason, At: time.Now().UTC()})
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		result := tx.Model(&skillCandidateRow{}).Where("id = ? AND revision = ?", id, revision).Updates(map[string]any{"state": v.State, "revision": v.Revision, "payload": raw})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrIntelligenceConflict
		}
		out = v
		return nil
	})
	return out, err
}
