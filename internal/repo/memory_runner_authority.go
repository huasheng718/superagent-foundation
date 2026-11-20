package repo

import (
	"agentflow/internal/domain"
	"gorm.io/gorm"
	"sort"
	"time"
)

func (r *MemoryRepo) validateRunnerMemoriesTx(tx *gorm.DB, binding domain.RunnerGovernedContext, now time.Time) error {
	selected := append([]domain.RunnerGovernedMemory{}, binding.Memories...)
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	for _, m := range selected {
		var row governedMemoryRow
		if err := r.locking(tx).Where("project_id = ? AND id = ?", binding.ProjectID, m.ID).Take(&row).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		memory, err := decodeGovernedMemory(row)
		if err != nil {
			return err
		}
		if memory.State != domain.MemoryApproved || memory.Revision != m.Revision || memory.SHA256 != m.SHA256 || (memory.ExpiresAt != nil && !memory.ExpiresAt.After(leaseCheckTime(tx, now))) {
			return ErrIntelligenceUnavailable
		}
	}
	return nil
}
