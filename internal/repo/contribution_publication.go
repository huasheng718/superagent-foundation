package repo

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrContributionPublicationInvalid   = errors.New("invalid contribution publication")
	ErrContributionPublicationConflict  = errors.New("immutable contribution publication conflicts")
	ErrContributionPublicationNotReady  = errors.New("contribution publication gates are not ready")
	ErrContributionPublicationLeaseLost = errors.New("contribution publication lease lost")
	ErrContributionPRCreateUncertain    = errors.New("PR creation was already authorized; reconcile only")
)

// Gates and the outbox share one DeliveryRepo-owned durable row. Integration
// rows stay immutable; no generic task scheduler or Issue transition is added.
type contributionPublicationRow struct {
	ID           string     `gorm:"column:id;primaryKey;size:64"`
	RunID        *string    `gorm:"column:run_id;index;size:64"`
	RepositoryID string     `gorm:"column:repository_id;index;size:64;not null"`
	State        string     `gorm:"column:state;index;size:32;not null"`
	Attempt      int        `gorm:"column:attempt;not null"`
	LeaseOwner   string     `gorm:"column:lease_owner;size:128;not null"`
	LeaseUntil   *time.Time `gorm:"column:lease_until"`
	Payload      string     `gorm:"column:payload;type:longtext;not null"`
	CreatedAt    time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;not null"`
}

func (contributionPublicationRow) TableName() string { return "contribution_publications" }

func publicationRow(value domain.ContributionPublication) contributionPublicationRow {
	var runID *string
	if value.RunID != "" {
		id := value.RunID
		runID = &id
	}
	return contributionPublicationRow{ID: value.ID, RunID: runID, RepositoryID: value.RepositoryID,
		State: value.State, Attempt: value.Attempt, LeaseOwner: value.LeaseOwner, LeaseUntil: value.LeaseUntil,
		Payload: string(mustJSON(value)), CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}

func decodePublication(row contributionPublicationRow) (domain.ContributionPublication, error) {
	var value domain.ContributionPublication
	if json.Unmarshal([]byte(row.Payload), &value) != nil || value.ID != row.ID {
		return value, ErrContributionPublicationInvalid
	}
	if value.SourceKind == "teamai_proposal" {
		if row.RunID != nil || value.RunID != "" || value.ProjectID == "" || !validContributionSHA256(value.SourceSHA256) {
			return value, ErrContributionPublicationInvalid
		}
	} else if value.SourceKind != "" || strings.TrimSpace(value.RunID) == "" || row.RunID == nil || *row.RunID != value.RunID {
		return value, ErrContributionPublicationInvalid
	}
	// SQL columns remain authoritative for concurrent publisher ownership.
	value.State, value.Attempt = row.State, row.Attempt
	value.LeaseOwner, value.LeaseUntil = row.LeaseOwner, row.LeaseUntil
	value.CreatedAt, value.UpdatedAt = row.CreatedAt, row.UpdatedAt
	return value, nil
}

func (r *DeliveryRepo) lockedPublication(tx *gorm.DB, id string) (domain.ContributionPublication, error) {
	var row contributionPublicationRow
	if !validContributionSHA256(id) {
		return domain.ContributionPublication{}, ErrContributionPublicationInvalid
	}
	if err := r.locking(tx).Where("id = ?", id).Take(&row).Error; err != nil {
		return domain.ContributionPublication{}, err
	}
	return decodePublication(row)
}

func writePublication(tx *gorm.DB, value domain.ContributionPublication) error {
	row := publicationRow(value)
	return tx.Model(&contributionPublicationRow{}).Where("id = ?", value.ID).Updates(map[string]any{
		"state": row.State, "attempt": row.Attempt, "lease_owner": row.LeaseOwner,
		"lease_until": row.LeaseUntil, "payload": row.Payload, "updated_at": row.UpdatedAt,
	}).Error
}

// PrepareContributionPublication freezes the Engine-validated default branch.
// It never creates/reuses gate evidence from another integration/hash.
func (r *DeliveryRepo) PrepareContributionPublication(integrationID, targetBranch string, now time.Time) (domain.ContributionPublication, error) {
	if !validContributionSHA256(integrationID) || targetBranch == "" || strings.TrimSpace(targetBranch) != targetBranch || len(targetBranch) > 255 || domain.NormalizeBranchName(targetBranch) != targetBranch || strings.HasPrefix(targetBranch, "-") || strings.ContainsAny(targetBranch, "\x00\r\n") {
		return domain.ContributionPublication{}, ErrContributionPublicationInvalid
	}
	var value domain.ContributionPublication
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var integrationRow contributionIntegrationRow
		if err := tx.Where("id = ?", integrationID).Take(&integrationRow).Error; err != nil {
			return err
		}
		var integration domain.ContributionIntegration
		if json.Unmarshal([]byte(integrationRow.Payload), &integration) != nil || integration.ID != integrationID || integration.State != "prepared" {
			return ErrContributionPublicationInvalid
		}
		created := leaseCheckTime(tx, now)
		value = domain.ContributionPublication{PhaseVersion: 1, ID: integration.ID, RunID: integration.RunID, RepositoryID: integration.RepositoryID,
			ResultSHA: integration.ResultSHA, ContextSHA256: integration.ContextSHA256, ManifestSHA256: integration.ManifestSHA256,
			TargetBranch: targetBranch, Branch: "superagent/integration/" + integration.ID, State: "gating",
			Gates: []domain.ContributionGateEvidence{}, CreatedAt: created, UpdatedAt: created}
		row := publicationRow(value)
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		existing, err := r.lockedPublication(tx, integrationID)
		if err != nil {
			return err
		}
		if existing.RunID != value.RunID || existing.RepositoryID != value.RepositoryID || existing.ResultSHA != value.ResultSHA || existing.ContextSHA256 != value.ContextSHA256 || existing.ManifestSHA256 != value.ManifestSHA256 || existing.TargetBranch != targetBranch || existing.Branch != value.Branch {
			return ErrContributionPublicationConflict
		}
		value = existing
		return nil
	})
	return value, err
}

func validatePublicationGate(value domain.ContributionPublication, gate domain.ContributionGateEvidence) error {
	if value.SourceKind != "" {
		return ErrContributionPublicationInvalid
	}
	if strings.TrimSpace(gate.ID) == "" || strings.TrimSpace(gate.ID) != gate.ID || len(gate.ID) > 128 || gate.IntegrationID != value.ID || gate.ResultSHA != value.ResultSHA || gate.ContextSHA256 != value.ContextSHA256 || gate.ManifestSHA256 != value.ManifestSHA256 || strings.TrimSpace(gate.EvidenceRef) == "" || len(gate.EvidenceRef) > 2048 || !validContributionSHA256(gate.EvidenceSHA256) {
		return ErrContributionPublicationInvalid
	}
	switch gate.Kind {
	case "fast", "slow":
	case "review":
		if strings.TrimSpace(gate.ReviewerID) == "" || len(gate.ReviewerID) > 256 {
			return ErrContributionPublicationInvalid
		}
	default:
		return ErrContributionPublicationInvalid
	}
	if gate.Status != "passed" && gate.Status != "failed" && gate.Status != "rejected" {
		return ErrContributionPublicationInvalid
	}
	return nil
}

func samePublicationGate(a, b domain.ContributionGateEvidence) bool {
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	return a == b
}

// RecordContributionGate is an internal controller seam, never a Runner route.
// Each kind/ID is immutable; failure is permanently rejected for this ID.
func (r *DeliveryRepo) RecordContributionGate(integrationID string, gate domain.ContributionGateEvidence, now time.Time) (domain.ContributionPublication, error) {
	var value domain.ContributionPublication
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		value, err = r.lockedPublication(tx, integrationID)
		if err != nil {
			return err
		}
		if err := validatePublicationGate(value, gate); err != nil {
			return err
		}
		for _, previous := range value.Gates {
			if previous.GateTaskID != "" && previous.GateTaskID == gate.GateTaskID && previous.Kind != gate.Kind {
				return ErrContributionPublicationConflict
			}
			if previous.ID == gate.ID || previous.Kind == gate.Kind {
				if samePublicationGate(previous, gate) {
					return nil
				}
				return ErrContributionPublicationConflict
			}
		}
		if value.State != "gating" && value.State != "rejected" {
			return ErrContributionPublicationConflict
		}
		gate.CreatedAt = leaseCheckTime(tx, now)
		value.Gates = append(value.Gates, gate)
		if gate.Status != "passed" {
			value.State = "rejected"
		} else if value.State != "rejected" && len(value.Gates) == 3 {
			value.State = "ready"
		}
		value.UpdatedAt = gate.CreatedAt
		return writePublication(tx, value)
	})
	return value, err
}

func (r *DeliveryRepo) ContributionPublication(id string) (domain.ContributionPublication, bool) {
	if !validContributionSHA256(id) {
		return domain.ContributionPublication{}, false
	}
	var row contributionPublicationRow
	if err := r.DB.Where("id = ?", id).Take(&row).Error; err != nil {
		return domain.ContributionPublication{}, false
	}
	value, err := decodePublication(row)
	return value, err == nil
}

// ClaimContributionPublication leases the existing outbox, not a second
// execution queue. The Engine checks source lifecycle before this call and
// serializes remote effects with the existing repository integration lock.
func (r *DeliveryRepo) ClaimContributionPublication(id, owner string, ttl time.Duration, now time.Time) (domain.ContributionPublication, error) {
	if strings.TrimSpace(owner) == "" || len(owner) > 128 || ttl <= 0 || ttl > 10*time.Minute {
		return domain.ContributionPublication{}, ErrContributionPublicationInvalid
	}
	var value domain.ContributionPublication
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		value, err = r.lockedPublication(tx, id)
		if err != nil {
			return err
		}
		checked := leaseCheckTime(tx, now)
		if value.State == "publishing" && value.LeaseUntil != nil && value.LeaseUntil.After(checked) {
			return ErrContributionPublicationLeaseLost
		}
		if value.State != "ready" && value.State != "publishing" {
			return ErrContributionPublicationNotReady
		}
		if value.PhaseVersion < 0 || value.PhaseVersion > 1 {
			return ErrContributionPublicationInvalid
		}
		// An old publisher could have sent POST before losing its local receipt.
		// Absence of the new marker is not evidence that creation never happened.
		if value.PhaseVersion == 0 && value.State == "publishing" {
			value.PRCreateAttempted = true
		}
		value.PhaseVersion = 1
		value.State, value.LeaseOwner = "publishing", owner
		value.Attempt++
		until := checked.Add(ttl)
		value.LeaseUntil, value.UpdatedAt = &until, checked
		return writePublication(tx, value)
	})
	return value, err
}

// CompleteContributionPublication persists an Engine-validated create-only
// provider receipt. Engine must verify exact PR head SHA and auto-merge off.
// This method never records merged/done or updates Issues/MR reconciliation.
func (r *DeliveryRepo) CompleteContributionPublication(id, owner string, attempt int, prURL string, prNumber int, now time.Time) (domain.ContributionPublication, error) {
	parsed, err := url.Parse(prURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || prNumber < 1 || attempt < 1 || len(prURL) > 2048 {
		return domain.ContributionPublication{}, ErrContributionPublicationInvalid
	}
	var value domain.ContributionPublication
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		value, err = r.lockedPublication(tx, id)
		if err != nil {
			return err
		}
		if value.State == "pr_ready" {
			if value.Attempt == attempt && value.PRURL == prURL && value.PRNumber == prNumber && value.LeaseOwner == owner {
				return nil
			}
			return ErrContributionPublicationConflict
		}
		checked := leaseCheckTime(tx, now)
		if value.State != "publishing" || value.LeaseOwner != owner || value.Attempt != attempt || value.LeaseUntil == nil || !value.LeaseUntil.After(checked) {
			return ErrContributionPublicationLeaseLost
		}
		value.State, value.PRURL, value.PRNumber = "pr_ready", prURL, prNumber
		// Retain the winning owner/attempt for idempotent callback attribution;
		// the terminal state and cleared lease grant no ongoing capability.
		value.LeaseUntil, value.UpdatedAt = nil, checked
		return writePublication(tx, value)
	})
	return value, err
}

// MarkContributionPRCreateAttempted consumes automatic PR-create authority
// under the same outbox row/lease/attempt fence. It is never cleared on reclaim,
// cancellation, timeouts or absent provider discovery. No retry reset API exists.
func (r *DeliveryRepo) MarkContributionPRCreateAttempted(id, owner string, attempt int, now time.Time) (domain.ContributionPublication, error) {
	var value domain.ContributionPublication
	if owner == "" || attempt < 1 {
		return value, ErrContributionPublicationInvalid
	}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		value, err = r.lockedPublication(tx, id)
		if err != nil {
			return err
		}
		checked := leaseCheckTime(tx, now)
		if value.State != "publishing" || value.LeaseOwner != owner || value.Attempt != attempt || value.LeaseUntil == nil || !value.LeaseUntil.After(checked) {
			return ErrContributionPublicationLeaseLost
		}
		if value.PhaseVersion != 1 {
			return ErrContributionPublicationInvalid
		}
		if value.PRCreateAttempted {
			return ErrContributionPRCreateUncertain
		}
		value.PRCreateAttempted = true
		value.UpdatedAt = checked
		return writePublication(tx, value)
	})
	return value, err
}
