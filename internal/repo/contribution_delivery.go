package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DeliveryRepo owns durable controller preparation, separate from Runner
// staging/acceptance. No startup data migration or reconciliation creates it.
type contributionIntegrationRow struct {
	ID           string    `gorm:"column:id;primaryKey;size:64"`
	RunID        string    `gorm:"column:run_id;index;size:64;not null"`
	RepositoryID string    `gorm:"column:repository_id;index;size:64;not null"`
	State        string    `gorm:"column:state;size:32;not null"`
	Payload      string    `gorm:"column:payload;type:longtext;not null"`
	CreatedAt    time.Time `gorm:"column:created_at;not null"`
}

func (contributionIntegrationRow) TableName() string { return "contribution_integrations" }

func (r *DeliveryRepo) PrepareContributionIntegration(value domain.ContributionIntegration) (domain.ContributionIntegration, error) {
	if !validContributionSHA256(value.ID) || value.ID != value.ManifestSHA256 || !validContributionSHA256(value.ContextSHA256) || !validContributionSHA256(value.BundleSHA256) || !domain.ValidWorkspaceBaseSHA(value.BaseSHA) || !domain.ValidWorkspaceBaseSHA(value.ResultSHA) || value.RunID == "" || value.RepositoryID == "" || value.State != "prepared" || len(value.Inputs) == 0 || value.BundleSize <= 0 || value.BundleSize > domain.RunnerContributionMaxBytes {
		return domain.ContributionIntegration{}, fmt.Errorf("invalid controller integration")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return domain.ContributionIntegration{}, err
	}
	row := contributionIntegrationRow{ID: value.ID, RunID: value.RunID, RepositoryID: value.RepositoryID, State: value.State, Payload: string(raw), CreatedAt: time.Now().UTC()}
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockContributionBlob(tx, value.BundleSHA256, value.BundleSize); err != nil {
			return err
		}
		for _, input := range value.Inputs {
			if err := lockContributionBlob(tx, input.SHA256, input.Size); err != nil {
				return err
			}
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		var existing contributionIntegrationRow
		if err := tx.Where("id = ?", value.ID).Take(&existing).Error; err != nil {
			return err
		}
		if existing.Payload != string(raw) {
			return errors.New("immutable controller integration conflicts")
		}
		return nil
	})
	return value, err
}

func (r *DeliveryRepo) ContributionIntegration(id string) (domain.ContributionIntegration, bool) {
	if !validContributionSHA256(id) {
		return domain.ContributionIntegration{}, false
	}
	var row contributionIntegrationRow
	if err := r.DB.Where("id = ?", id).Take(&row).Error; err != nil {
		return domain.ContributionIntegration{}, false
	}
	var value domain.ContributionIntegration
	if json.Unmarshal([]byte(row.Payload), &value) != nil || value.ID != id {
		return value, false
	}
	return value, true
}

// ContributionIntegrationsForRun returns only the controller snapshots owned by
// this Run. Decode errors fail closed instead of hiding a corrupt integration.
func (r *DeliveryRepo) ContributionIntegrationsForRun(runID string) ([]domain.ContributionIntegration, error) {
	var rows []contributionIntegrationRow
	if err := r.DB.Where("run_id = ?", runID).Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]domain.ContributionIntegration, 0, len(rows))
	for _, row := range rows {
		var value domain.ContributionIntegration
		if err := json.Unmarshal([]byte(row.Payload), &value); err != nil || value.ID != row.ID || value.RunID != runID {
			return nil, errors.New("invalid stored controller integration")
		}
		values = append(values, value)
	}
	return values, nil
}

// DeliveryContributionBlobReferences returns bundle digests retained by the
// immutable controller integration records. Publication rows refer to their
// integration ID, so retaining the integration reference also protects the
// publication's source bundle.
func (r *DeliveryRepo) DeliveryContributionBlobReferences() ([]string, error) {
	var rows []contributionIntegrationRow
	if err := r.DB.Select("payload").Find(&rows).Error; err != nil {
		return nil, err
	}
	refs := make(map[string]struct{})
	for _, row := range rows {
		var value domain.ContributionIntegration
		if err := json.Unmarshal([]byte(row.Payload), &value); err != nil {
			return nil, fmt.Errorf("decode contribution integration: %w", err)
		}
		if validContributionSHA256(value.BundleSHA256) {
			refs[value.BundleSHA256] = struct{}{}
		}
		for _, input := range value.Inputs {
			if validContributionSHA256(input.SHA256) {
				refs[input.SHA256] = struct{}{}
			}
		}
	}
	return sortedContributionReferences(refs), nil
}
