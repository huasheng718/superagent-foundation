package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

var ErrTeamImportRefConflict = errors.New("registered TeamAI source binding changed")

// CreateImportedTeamRelease publishes an already frozen, reviewed import.
// Editable Catalog rows are never created or overwritten by this path.
func (r *CatalogRepo) CreateImportedTeamRelease(projectID, name, actor string, expectedBaseVersion int, payload domain.TeamReleasePayload, updates ...domain.TeamImportUpdateFrom) (domain.TeamRelease, error) {
	var result domain.TeamRelease
	if strings.TrimSpace(name) == "" || len(name) > 200 || strings.TrimSpace(actor) == "" || expectedBaseVersion < 0 || payload.GitImport == nil {
		return result, fmt.Errorf("reviewed Git payload, bounded name, actor and expected base required")
	}
	if len(updates) > 1 || len(updates) == 0 && payload.GitImport.Ref != "" || len(updates) == 1 && (updates[0].ReleaseID == "" || len(updates[0].ReleaseSHA256) != 64 || !domain.ValidTeamImportRef(updates[0].Ref) || updates[0].Ref != payload.GitImport.Ref) {
		return result, fmt.Errorf("matching reviewed update source required")
	}
	if err := domain.ValidateTeamReleasePayload(payload, projectID); err != nil {
		return result, err
	}
	digest, err := domain.IntelligenceDigest(payload)
	if err != nil {
		return result, err
	}
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		var project projectRow
		if err := r.locking(tx).Where("id = ?", projectID).Take(&project).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		var row repositoryRow
		if err := r.locking(tx).Where("id = ? AND project_id = ?", payload.GitImport.RepositoryID, projectID).Take(&row).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		var repository domain.Repository
		if json.Unmarshal(row.Payload, &repository) != nil || repository.ProjectID != projectID || repository.CloneURL != payload.Source.Repository {
			if len(updates) == 1 {
				return ErrTeamImportRefConflict
			}
			return ErrIntelligenceUnavailable
		}
		if len(updates) == 1 {
			update := updates[0]
			if "refs/heads/"+repository.DefaultBranch != update.Ref {
				return ErrTeamImportRefConflict
			}
			var sourceRow teamReleaseRow
			if err := r.locking(tx).Where("id = ? AND project_id = ?", update.ReleaseID, projectID).Take(&sourceRow).Error; err != nil {
				return ErrIntelligenceConflict
			}
			source, err := decodeTeamRelease(sourceRow)
			if err != nil || source.State != domain.TeamReleasePublished || source.SHA256 != update.ReleaseSHA256 || source.Payload.GitImport == nil {
				return ErrIntelligenceConflict
			}
			original, m := source.Payload.GitImport, payload.GitImport
			if original.RepositoryID != m.RepositoryID || source.Payload.Source.Repository != payload.Source.Repository || original.Root != m.Root || !domain.EqualTeamImportSelection(original.Selection, m.Selection) || original.Commit == m.Commit {
				return ErrIntelligenceConflict
			}
			// A new Git SHA is not authority to discard native restrictions. The
			// approved preview must already contain the exact inherited policies.
			raw, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			var preserved domain.TeamReleasePayload
			if err := json.Unmarshal(raw, &preserved); err != nil {
				return err
			}
			if err := domain.PreserveTeamDistribution(source.Payload, &preserved); err != nil {
				return err
			}
			if retained, err := domain.IntelligenceDigest(preserved); err != nil || retained != digest {
				return ErrIntelligenceConflict
			}
		}
		var version int
		if err := tx.Model(&teamReleaseRow{}).Select("COALESCE(MAX(version),0)").Where("project_id = ?", projectID).Scan(&version).Error; err != nil {
			return err
		}
		if version != expectedBaseVersion {
			return ErrIntelligenceConflict
		}
		id, err := intelligenceID("team-")
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		result = domain.TeamRelease{ID: id, ProjectID: projectID, Name: strings.TrimSpace(name), Version: version + 1, Revision: 1, State: domain.TeamReleasePublished, SHA256: digest, Payload: payload, CreatedBy: actor, CreatedAt: now, Decisions: []domain.IntelligenceDecision{{Decision: domain.TeamReleasePublished, Actor: actor, Reason: "approved Git import", At: now}}}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return tx.Create(&teamReleaseRow{ID: id, ProjectID: projectID, Version: result.Version, Revision: 1, State: result.State, Payload: raw, Created: now}).Error
	})
	return result, err
}
