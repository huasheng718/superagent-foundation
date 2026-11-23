package repo

import (
	"encoding/json"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// Only the locked extraction path may associate a server-owned receipt. Manual
// callers still use createSkillCandidate's source/distribution checks.
func (r *CatalogRepo) insertSkillCandidateTx(tx *gorm.DB, project string, input domain.SkillCandidatePackage, actor string, now time.Time) (domain.SkillCandidate, error) {
	var v domain.SkillCandidate
	if err := domain.ValidateSkillCandidatePackage(input, project); err != nil {
		return v, err
	}
	id, err := intelligenceID("skill-candidate-")
	if err != nil {
		return v, err
	}
	hash, err := domain.IntelligenceDigest(input)
	if err != nil {
		return v, err
	}
	v = domain.SkillCandidate{ID: id, ProjectID: project, State: domain.SkillCandidateQuarantined, Revision: 1, SHA256: hash, Package: input, CreatedBy: actor, CreatedAt: now, Decisions: []domain.IntelligenceDecision{}}
	raw, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	v.Resources = domain.SkillCandidateResources(input)
	err = tx.Create(&skillCandidateRow{ID: id, ProjectID: project, State: v.State, Revision: 1, Payload: raw, CreatedAt: now}).Error
	return v, err
}
