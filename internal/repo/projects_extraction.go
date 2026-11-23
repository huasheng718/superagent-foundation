package repo

import (
	"strings"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// Identity is passed by the authenticated HTTP owner, never decoded from the
// request. Local is the explicit auth-disabled development mode only.
type KnowledgeExtractionActor struct {
	UserID    int64
	LoginName string
	Local     bool
}

func (r *ProjectsRepo) lockExtractionProject(tx *gorm.DB, project string, actor KnowledgeExtractionActor, users *UsersRepo) error {
	var p projectRow
	if err := r.locking(tx).Where("id = ?", project).Take(&p).Error; err != nil {
		return ErrIntelligenceUnavailable
	}
	if actor.Local {
		if actor.UserID == 0 && actor.LoginName == "local" {
			return nil
		}
		return ErrExtractionForbidden
	}
	current, err := users.lockExtractionUser(tx, actor.UserID)
	if err != nil {
		return err
	}
	if !strings.EqualFold(current.LoginName, actor.LoginName) {
		return ErrExtractionForbidden
	}
	if domain.IsPlatformAdmin(current) || IsAllowlistedPlatformAdmin(current.LoginName) || p.OwnerUserID != nil && *p.OwnerUserID == current.ID {
		return nil
	}
	var member projectMemberRow
	if err := r.locking(tx).Where("project_id = ? AND user_id = ?", project, current.ID).Take(&member).Error; err != nil {
		return ErrExtractionForbidden
	}
	return nil
}
