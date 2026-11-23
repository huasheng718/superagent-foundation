package repo

import (
	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// Called after Run/task locks, before registration/lease commit. Keep release
// then sorted-memory lock order; revocation locks these same authoritative rows.
func (r *CatalogRepo) validateRunnerReleaseTx(tx *gorm.DB, binding domain.RunnerGovernedContext) error {
	var row teamReleaseRow
	if err := r.locking(tx).Where("project_id = ? AND id = ?", binding.ProjectID, binding.ReleaseID).Take(&row).Error; err != nil {
		return ErrIntelligenceUnavailable
	}
	release, err := decodeTeamRelease(row)
	if err != nil {
		return err
	}
	if release.State != domain.TeamReleasePublished || release.SHA256 != binding.ReleaseSHA256 {
		return ErrIntelligenceUnavailable
	}
	return nil
}
