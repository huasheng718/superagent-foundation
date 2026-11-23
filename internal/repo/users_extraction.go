package repo

import (
	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func (r *UsersRepo) lockExtractionUser(tx *gorm.DB, id int64) (domain.User, error) {
	var row userRow
	if id <= 0 {
		return domain.User{}, ErrExtractionForbidden
	}
	if err := r.locking(tx).Where("id = ?", id).Take(&row).Error; err != nil {
		return domain.User{}, ErrExtractionForbidden
	}
	return domain.User{ID: row.ID, LoginName: row.LoginName, PlatformRole: row.PlatformRole}, nil
}
