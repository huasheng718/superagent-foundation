package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrTeamGitProposalConflict = errors.New("TeamAI proposal state or reviewed source changed")
var ErrTeamGitProposalInvalid = errors.New("invalid retained TeamAI proposal")

type teamGitProposalRow struct {
	ID           string    `gorm:"column:id;primaryKey;size:64"`
	ProjectID    string    `gorm:"column:project_id;index;size:64;not null"`
	RepositoryID string    `gorm:"column:repository_id;index;size:64;not null"`
	State        string    `gorm:"column:state;size:32;not null"`
	Revision     int       `gorm:"column:revision;not null"`
	Payload      string    `gorm:"column:payload;type:longtext;not null"`
	Metadata     string    `gorm:"column:metadata;type:longtext;not null"`
	Bundle       []byte    `gorm:"column:bundle;type:longblob;not null"`
	CreatedAt    time.Time `gorm:"column:created_at;not null"`
	UpdatedAt    time.Time `gorm:"column:updated_at;not null"`
}

func (teamGitProposalRow) TableName() string { return "team_git_proposals" }

func teamProposalMetadata(v domain.TeamGitProposal) string {
	v.Patch = ""
	v.Files = nil
	v.Publication = nil
	return string(mustJSON(v))
}
func decodeTeamProposal(row teamGitProposalRow) (domain.TeamGitProposal, error) {
	var v domain.TeamGitProposal
	if json.Unmarshal([]byte(row.Payload), &v) != nil || v.ID != row.ID || v.ProjectID != row.ProjectID || v.RepositoryID != row.RepositoryID || v.State != row.State || v.Revision != row.Revision {
		return v, ErrTeamGitProposalInvalid
	}
	digest, err := domain.TeamGitProposalDigest(v)
	if err != nil || digest != v.SHA256 {
		return v, ErrTeamGitProposalInvalid
	}
	return v, nil
}

// The source/release locks are taken before proposal/outbox locks throughout.
func (r *DeliveryRepo) validateTeamProposalSourceTx(tx *gorm.DB, v domain.TeamGitProposal) error {
	var repository repositoryRow
	if err := r.locking(tx).Where("id = ? AND project_id = ?", v.RepositoryID, v.ProjectID).Take(&repository).Error; err != nil {
		return ErrIntelligenceUnavailable
	}
	var registered domain.Repository
	if json.Unmarshal(repository.Payload, &registered) != nil || registered.CloneURL != v.RepositoryURL || registered.Provider != "github" || registered.GongfengProjectID != v.ProviderProject || "refs/heads/"+registered.DefaultBranch != v.TargetRef {
		return ErrTeamGitProposalConflict
	}
	var release teamReleaseRow
	if err := r.locking(tx).Where("id = ? AND project_id = ?", v.ReleaseID, v.ProjectID).Take(&release).Error; err != nil {
		return ErrIntelligenceUnavailable
	}
	formal, err := decodeTeamRelease(release)
	if err != nil {
		return err
	}
	if formal.State != domain.TeamReleasePublished || formal.SHA256 != v.ReleaseSHA256 || formal.Revision != v.ReleaseRevision || formal.Payload.GitImport == nil || formal.Payload.GitImport.Commit != v.BaseCommit || formal.Payload.GitImport.RepositoryID != v.RepositoryID || formal.Payload.Source.Repository != v.RepositoryURL {
		return ErrTeamGitProposalConflict
	}
	if ref := formal.Payload.GitImport.Ref; ref != "" && ref != v.TargetRef {
		return ErrTeamGitProposalConflict
	}
	return domain.TeamGitWritebackPolicy(formal.Payload)
}

func (r *DeliveryRepo) CreateTeamGitProposal(v domain.TeamGitProposal, bundle []byte) (domain.TeamGitProposal, error) {
	var result domain.TeamGitProposal
	digest, err := domain.TeamGitProposalDigest(v)
	sum := sha256.Sum256(bundle)
	if err != nil || digest != v.SHA256 || v.ID != domain.TeamGitProposalID(v) || !validContributionSHA256(v.SHA256) || v.State != "pending_review" || v.Revision != 1 || v.CreatedBy == "" || v.BundleSize != int64(len(bundle)) || v.BundleSize <= 0 || v.BundleSize > domain.RunnerContributionMaxBytes || hex.EncodeToString(sum[:]) != v.BundleSHA256 || !domain.ValidGitCommitSHA(v.BaseCommit) || !domain.ValidGitCommitSHA(v.ResultSHA) || !domain.ValidTeamImportRef(v.TargetRef) || v.Validation.Policy != "teamai-configuration-proposal-v1" {
		return result, ErrTeamGitProposalInvalid
	}
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		if err := r.validateTeamProposalSourceTx(tx, v); err != nil {
			return err
		}
		row := teamGitProposalRow{ID: v.ID, ProjectID: v.ProjectID, RepositoryID: v.RepositoryID, State: v.State, Revision: 1, Payload: string(mustJSON(v)), Metadata: teamProposalMetadata(v), Bundle: bundle, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		var stored teamGitProposalRow
		if err := r.locking(tx).Omit("bundle").Where("id = ? AND project_id = ?", v.ID, v.ProjectID).Take(&stored).Error; err != nil {
			return err
		}
		var err error
		result, err = decodeTeamProposal(stored)
		// ID is the exact reviewed input content key, independent of a racing
		// server's preparation timestamp. Return the winning retained artifact.
		return err
	})
	return result, err
}

func (r *DeliveryRepo) TeamGitProposal(projectID, id string) (domain.TeamGitProposal, error) {
	var row teamGitProposalRow
	if !validContributionSHA256(id) {
		return domain.TeamGitProposal{}, ErrIntelligenceUnavailable
	}
	if err := r.DB.Omit("bundle").Where("id = ? AND project_id = ?", id, projectID).Take(&row).Error; err != nil {
		return domain.TeamGitProposal{}, ErrIntelligenceUnavailable
	}
	v, err := decodeTeamProposal(row)
	if err == nil {
		if p, ok := r.ContributionPublication(id); ok {
			v.Publication = &p
		}
	}
	return v, err
}
func (r *DeliveryRepo) TeamGitProposals(projectID string) ([]domain.TeamGitProposal, error) {
	var rows []teamGitProposalRow
	if err := r.DB.Select("id", "metadata").Where("project_id = ?", projectID).Order("created_at DESC, id DESC").Limit(25).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]domain.TeamGitProposal, 0, len(rows))
	for _, row := range rows {
		var v domain.TeamGitProposal
		if json.Unmarshal([]byte(row.Metadata), &v) != nil || v.ID != row.ID || v.ProjectID != projectID {
			return nil, ErrTeamGitProposalInvalid
		}
		if p, ok := r.ContributionPublication(v.ID); ok {
			v.Publication = &p
		}
		values = append(values, v)
	}
	return values, nil
}
func (r *DeliveryRepo) TeamGitProposalBundle(projectID, id string) ([]byte, error) {
	v, err := r.TeamGitProposal(projectID, id)
	if err != nil {
		return nil, err
	}
	var row teamGitProposalRow
	if err = r.DB.Select("bundle").Where("id = ? AND project_id = ?", id, projectID).Take(&row).Error; err != nil {
		return nil, err
	}
	sum := sha256.Sum256(row.Bundle)
	if int64(len(row.Bundle)) != v.BundleSize || hex.EncodeToString(sum[:]) != v.BundleSHA256 {
		return nil, ErrTeamGitProposalInvalid
	}
	return row.Bundle, nil
}
func (r *DeliveryRepo) ValidateTeamGitProposalSource(v domain.TeamGitProposal) error {
	return r.DB.Transaction(func(tx *gorm.DB) error { return r.validateTeamProposalSourceTx(tx, v) })
}

// CompleteTeamGitProposalPublication serializes the final local authorization
// check with cancellation/revocation. External provider effects remain outside
// this transaction; it only makes the receipt's local authority check atomic.
func (r *DeliveryRepo) CompleteTeamGitProposalPublication(projectID, id, sha string, revision int, owner string, attempt int, prURL string, prNumber int, now time.Time) (domain.ContributionPublication, error) {
	var out domain.ContributionPublication
	v, err := r.TeamGitProposal(projectID, id)
	if err != nil {
		return out, err
	}
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		if err := r.validateTeamProposalSourceTx(tx, v); err != nil {
			return err
		}
		var row teamGitProposalRow
		if err := r.locking(tx).Omit("bundle").Where("id = ? AND project_id = ?", id, projectID).Take(&row).Error; err != nil {
			return err
		}
		current, err := decodeTeamProposal(row)
		if err != nil {
			return err
		}
		if current.State != "approved" || current.Revision != revision || current.SHA256 != sha {
			return ErrTeamGitProposalConflict
		}
		scoped := &DeliveryRepo{deps: &deps{DB: tx}}
		publication, err := scoped.lockedPublication(tx, id)
		if err != nil {
			return err
		}
		if publication.SourceKind != "teamai_proposal" || publication.ProjectID != projectID || publication.SourceSHA256 != sha || publication.ResultSHA != current.ResultSHA || publication.RunID != "" {
			return ErrTeamGitProposalConflict
		}
		out, err = scoped.CompleteContributionPublication(id, owner, attempt, prURL, prNumber, now)
		return err
	})
	return out, err
}

func (r *DeliveryRepo) DecideTeamGitProposal(projectID, id, sha, actor, decision, reason string, revision int) (domain.TeamGitProposal, error) {
	v, err := r.TeamGitProposal(projectID, id)
	if err != nil {
		return v, err
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(reason) > 2000 || (decision != "approved" && decision != "rejected" && decision != "cancelled") {
		return v, ErrTeamGitProposalInvalid
	}
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		if decision != "cancelled" {
			if err := r.validateTeamProposalSourceTx(tx, v); err != nil {
				return err
			}
		}
		var row teamGitProposalRow
		if err := r.locking(tx).Omit("bundle").Where("id = ? AND project_id = ?", id, projectID).Take(&row).Error; err != nil {
			return err
		}
		var err error
		v, err = decodeTeamProposal(row)
		if err != nil {
			return err
		}
		if v.Revision != revision || v.SHA256 != sha || v.State != "pending_review" && (decision != "cancelled" || v.State != "approved") {
			return ErrTeamGitProposalConflict
		}
		v.State = decision
		v.Revision++
		v.UpdatedAt = time.Now().UTC()
		v.Decisions = append(v.Decisions, domain.IntelligenceDecision{Decision: decision, Actor: actor, Reason: strings.TrimSpace(reason), At: v.UpdatedAt})
		v.Publication = nil
		if err := tx.Model(&teamGitProposalRow{}).Where("id = ?", id).Updates(map[string]any{"state": v.State, "revision": v.Revision, "payload": string(mustJSON(v)), "metadata": teamProposalMetadata(v), "updated_at": v.UpdatedAt}).Error; err != nil {
			return err
		}
		if decision == "approved" {
			p := domain.ContributionPublication{PhaseVersion: 1, SourceKind: "teamai_proposal", ProjectID: v.ProjectID, SourceSHA256: v.SHA256, ID: v.ID, RepositoryID: v.RepositoryID, ResultSHA: v.ResultSHA, ManifestSHA256: v.SHA256, ContextSHA256: v.ReleaseSHA256, TargetBranch: strings.TrimPrefix(v.TargetRef, "refs/heads/"), Branch: "superagent/integration/" + v.ID, State: "ready", Gates: []domain.ContributionGateEvidence{}, CreatedAt: v.UpdatedAt, UpdatedAt: v.UpdatedAt}
			row := publicationRow(p)
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			v.Publication = &p
		}
		return nil
	})
	return v, err
}
