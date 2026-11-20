package repo

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrRunnerContributionInvalid  = errors.New("invalid runner contribution")
	ErrRunnerContributionConflict = errors.New("runner contribution conflicts with an existing receipt")
	ErrRunnerContributionTooLarge = errors.New("runner contribution exceeds the 16 MiB limit")
)

// RunnersRepo owns these bytes in the same platform database. Preview expiry
// and preview garbage collection never own contribution blobs.
type runnerContributionBlobRow struct {
	SHA256  string    `gorm:"column:sha256;primaryKey;size:64"`
	Content []byte    `gorm:"column:content;type:longblob"`
	Size    int64     `gorm:"column:size;not null"`
	Created time.Time `gorm:"column:created_at;not null"`
}

func (runnerContributionBlobRow) TableName() string { return "runner_contribution_blobs" }

func validContributionSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func invalidContribution(reason string) error {
	return fmt.Errorf("%w: %s", ErrRunnerContributionInvalid, reason)
}

func contributionSpec(task domain.RunnerTask, repositoryID string) (domain.RunnerContributionSpec, error) {
	if task.Action != "" || task.RunID == "" || task.NodeID == "" || task.Invocation == nil {
		return domain.RunnerContributionSpec{}, invalidContribution("export requires a workflow execution task")
	}
	repositories := task.Invocation.Repositories
	if len(repositories) == 0 && task.Invocation.Repository != nil {
		repositories = []domain.RunnerRepository{*task.Invocation.Repository}
	}
	seen := make(map[string]bool, len(task.ContributionSpecs))
	var selected domain.RunnerContributionSpec
	for _, spec := range task.ContributionSpecs {
		if spec.RepositoryID == "" || strings.TrimSpace(spec.RepositoryID) != spec.RepositoryID || seen[spec.RepositoryID] || !domain.ValidWorkspaceBaseSHA(spec.BaseSHA) || !validContributionSHA256(spec.ContextSHA256) || (spec.Dir != "" && !domain.SafeWorkspaceRelPath(spec.Dir)) {
			return selected, invalidContribution("invalid or duplicate frozen repository spec")
		}
		seen[spec.RepositoryID] = true
		if spec.MaxBytes <= 0 || spec.MaxBytes > domain.RunnerContributionMaxBytes {
			return selected, ErrRunnerContributionTooLarge
		}
		matches := 0
		for _, repository := range repositories {
			if repository.RepositoryID == spec.RepositoryID && repository.Dir == spec.Dir && repository.BaseSHA == spec.BaseSHA {
				matches++
			}
		}
		if matches != 1 {
			return selected, invalidContribution("frozen spec does not match exactly one invocation repository")
		}
		if spec.RepositoryID == repositoryID {
			selected = spec
		}
	}
	if selected.RepositoryID == "" {
		return selected, invalidContribution("repository was not assigned for export")
	}
	return selected, nil
}

func (r *RunnersRepo) contributionTaskForUpload(tx *gorm.DB, taskID, runnerID, sessionHash string, attempt int, repositoryID string, now time.Time) (domain.RunnerTask, domain.RunnerContributionSpec, error) {
	var row runnerTaskRow
	if sessionHash != "" {
		var err error
		row, err = r.takeRunnerTaskForSession(tx, taskID, runnerID, sessionHash, attempt, now)
		if err != nil {
			return domain.RunnerTask{}, domain.RunnerContributionSpec{}, err
		}
	} else {
		var ok bool
		row, ok = r.takeRunnerTaskForLease(tx, taskID, runnerID, attempt, now)
		if !ok {
			return domain.RunnerTask{}, domain.RunnerContributionSpec{}, ErrRunnerTaskLeaseLost
		}
	}
	if row.DesiredState == domain.RunnerTaskDesiredCancel {
		return domain.RunnerTask{}, domain.RunnerContributionSpec{}, ErrRunnerTaskLeaseLost
	}
	task := runnerTaskFromRow(row)
	spec, err := contributionSpec(task, repositoryID)
	return task, spec, err
}

// ValidateRunnerContributionUpload authorizes receipt without holding a task
// lock while a potentially slow request body streams. Stage rechecks it.
func (r *RunnersRepo) ValidateRunnerContributionUpload(taskID, runnerID string, attempt int, repositoryID string, now time.Time) (domain.RunnerContributionSpec, error) {
	return r.validateRunnerContributionUpload(taskID, runnerID, "", attempt, repositoryID, now)
}

func (r *RunnersRepo) ValidateRunnerContributionUploadForSession(taskID, runnerID, sessionHash string, attempt int, repositoryID string, now time.Time) (domain.RunnerContributionSpec, error) {
	if !domain.ValidWorkspaceContextSHA256(sessionHash) {
		return domain.RunnerContributionSpec{}, ErrRunnerPlacementSessionInvalid
	}
	return r.validateRunnerContributionUpload(taskID, runnerID, sessionHash, attempt, repositoryID, now)
}

// RunnerContributionInputForSession returns one immutable, controller-frozen
// input bundle for the currently leased placed task. The task transaction
// remains the authorization boundary: callers cannot read an arbitrary blob
// by digest or bypass the task's source-lineage validation.
func (r *RunnersRepo) RunnerContributionInputForSession(taskID, runnerID, sessionHash string, attempt int, repositoryID string, now time.Time) (domain.RunnerContributionInput, []byte, bool, error) {
	if !domain.ValidWorkspaceContextSHA256(sessionHash) {
		return domain.RunnerContributionInput{}, nil, false, ErrRunnerPlacementSessionInvalid
	}
	var input domain.RunnerContributionInput
	var content []byte
	found := false
	// Authenticate before the not-found probe, but in a short transaction of
	// its own. Keeping the registry lock while the task-plane transaction
	// acquires Run/task locks reverses the callback lock order and can deadlock
	// with a concurrent claim (Run/task -> registry).
	if err := r.DB.Transaction(func(tx *gorm.DB) error {
		return r.validateRunnerClaimSessionTx(tx, runnerID, sessionHash, now)
	}); err != nil {
		return input, nil, false, err
	}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var probe runnerTaskRow
		if err := tx.Where("id = ?", taskID).Take(&probe).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		row, err := r.takeRunnerTaskForSession(tx, taskID, runnerID, sessionHash, attempt, now)
		if err != nil {
			return err
		}
		task := runnerTaskFromRow(row)
		if task.Invocation == nil || task.ExecutionPlacement == nil || len(task.Invocation.ContributionInputs) == 0 {
			return nil
		}
		for _, candidate := range task.Invocation.ContributionInputs {
			if candidate.RepositoryID != repositoryID {
				continue
			}
			if found {
				return invalidContribution("duplicate frozen contribution input")
			}
			found = true
			if !validContributionSHA256(candidate.BundleSHA256) || candidate.BundleSize <= 0 || candidate.BundleSize > domain.RunnerContributionMaxBytes {
				return invalidContribution("frozen contribution input has invalid bundle identity")
			}
			var blob runnerContributionBlobRow
			if err := tx.Where("sha256 = ?", candidate.BundleSHA256).Take(&blob).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrRunnerTaskLeaseLost
				}
				return err
			}
			if blob.Size != candidate.BundleSize || int64(len(blob.Content)) != candidate.BundleSize {
				return ErrRunnerTaskLeaseLost
			}
			sum := sha256.Sum256(blob.Content)
			if hex.EncodeToString(sum[:]) != candidate.BundleSHA256 {
				return ErrRunnerTaskLeaseLost
			}
			input = candidate
			content = append([]byte(nil), blob.Content...)
		}
		return nil
	})
	return input, content, found, err
}

func (r *RunnersRepo) validateRunnerContributionUpload(taskID, runnerID, sessionHash string, attempt int, repositoryID string, now time.Time) (domain.RunnerContributionSpec, error) {
	var spec domain.RunnerContributionSpec
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		_, spec, err = r.contributionTaskForUpload(tx, taskID, runnerID, sessionHash, attempt, repositoryID, now)
		return err
	})
	return spec, err
}

// PutRunnerContributionBlob verifies bounded content-addressed bytes. It never
// accepts them as task output; a rejected transfer may leave an orphan blob.
func (r *RunnersRepo) PutRunnerContributionBlob(sha256Hex string, content []byte) error {
	if int64(len(content)) > domain.RunnerContributionMaxBytes {
		return ErrRunnerContributionTooLarge
	}
	if len(content) == 0 || !validContributionSHA256(sha256Hex) {
		return invalidContribution("missing content or invalid SHA256")
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != sha256Hex {
		return invalidContribution("content SHA256 mismatch")
	}
	row := runnerContributionBlobRow{SHA256: sha256Hex, Content: content, Size: int64(len(content)), Created: time.Now().UTC()}
	return r.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

func (r *RunnersRepo) RunnerContributionBlob(sha256Hex string) ([]byte, int64, bool) {
	if !validContributionSHA256(sha256Hex) {
		return nil, 0, false
	}
	var row runnerContributionBlobRow
	if err := r.DB.Where("sha256 = ?", sha256Hex).Take(&row).Error; err != nil {
		return nil, 0, false
	}
	return row.Content, row.Size, true
}

// RunnerContributionBlobReferences returns every contribution digest still
// reachable from durable Runner task payloads. It is intentionally a read-only
// snapshot; callers performing collection must use a conservative retention
// window and re-run the scan before deleting anything.
func (r *RunnersRepo) RunnerContributionBlobReferences() ([]string, error) {
	var rows []runnerTaskRow
	if err := r.DB.Select("payload").Find(&rows).Error; err != nil {
		return nil, err
	}
	refs := make(map[string]struct{})
	for _, row := range rows {
		var payload runnerTaskPayload
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode runner task payload %s: %w", row.ID, err)
		}
		for _, receipt := range append(append([]domain.RunnerContributionReceipt{}, payload.PendingContributions...), payload.AcceptedContributions...) {
			if validContributionSHA256(receipt.SHA256) {
				refs[receipt.SHA256] = struct{}{}
			}
		}
		if payload.Invocation != nil {
			for _, input := range payload.Invocation.ContributionInputs {
				if validContributionSHA256(input.BundleSHA256) {
					refs[input.BundleSHA256] = struct{}{}
				}
			}
		}
	}
	return sortedContributionReferences(refs), nil
}

func sortedContributionReferences(refs map[string]struct{}) []string {
	values := make([]string, 0, len(refs))
	for ref := range refs {
		values = append(values, ref)
	}
	sort.Strings(values)
	return values
}

// DeleteUnreferencedContributionBlobs removes only old blobs absent from the
// caller's durable reference snapshot. Candidate rows are locked before the
// delete so durable reference writers and collection serialize on the blob
// row. A concurrent writer that loses the race observes a missing blob and
// must retry its immutable write.
func (r *RunnersRepo) DeleteUnreferencedContributionBlobs(references []string, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("contribution blob retention cutoff is required")
	}
	keep := make(map[string]struct{}, len(references))
	for _, ref := range references {
		if validContributionSHA256(ref) {
			keep[ref] = struct{}{}
		}
	}
	var deleted int64
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var rows []runnerContributionBlobRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("created_at < ?", before.UTC()).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if _, ok := keep[row.SHA256]; ok {
				continue
			}
			result := tx.Where("sha256 = ? AND created_at < ?", row.SHA256, before.UTC()).Delete(&runnerContributionBlobRow{})
			if result.Error != nil {
				return result.Error
			}
			deleted += result.RowsAffected
		}
		return nil
	})
	return deleted, err
}

func sameContribution(a, b domain.RunnerContributionReceipt) bool {
	return a.TaskID == b.TaskID && a.Attempt == b.Attempt && a.RepositoryID == b.RepositoryID && a.Dir == b.Dir && a.BaseSHA == b.BaseSHA && a.ResultSHA == b.ResultSHA && a.ContextSHA256 == b.ContextSHA256 && a.SHA256 == b.SHA256 && a.Size == b.Size
}

func validateContributionReceipt(receipt domain.RunnerContributionReceipt, spec domain.RunnerContributionSpec) error {
	if receipt.RepositoryID != spec.RepositoryID || receipt.Dir != spec.Dir || receipt.BaseSHA != spec.BaseSHA || receipt.ContextSHA256 != spec.ContextSHA256 || !domain.ValidWorkspaceBaseSHA(receipt.ResultSHA) || !validContributionSHA256(receipt.SHA256) || receipt.Size <= 0 {
		return invalidContribution("receipt does not match frozen repository/context or complete commit identity")
	}
	if receipt.Size > spec.MaxBytes || receipt.Size > domain.RunnerContributionMaxBytes {
		return ErrRunnerContributionTooLarge
	}
	return nil
}

func lockContributionBlob(tx *gorm.DB, sha256Hex string, size int64) error {
	var row runnerContributionBlobRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("sha256", "size").Where("sha256 = ? AND size = ?", sha256Hex, size).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return invalidContribution("durable bundle bytes are missing")
		}
		return err
	}
	return nil
}

func contributionBlobExists(tx *gorm.DB, receipt domain.RunnerContributionReceipt) error {
	return lockContributionBlob(tx, receipt.SHA256, receipt.Size)
}

// StageRunnerContribution atomically binds a durable blob to the still-live
// attempt. IDs and timestamps are server-owned; identical replay returns the
// original receipt while conflicting replay cannot replace it.
func (r *RunnersRepo) StageRunnerContribution(receipt domain.RunnerContributionReceipt, runnerID string, now time.Time) (domain.RunnerContributionReceipt, error) {
	return r.stageRunnerContribution(receipt, runnerID, "", now)
}

func (r *RunnersRepo) StageRunnerContributionForSession(receipt domain.RunnerContributionReceipt, runnerID, sessionHash string, now time.Time) (domain.RunnerContributionReceipt, error) {
	if !domain.ValidWorkspaceContextSHA256(sessionHash) {
		return domain.RunnerContributionReceipt{}, ErrRunnerPlacementSessionInvalid
	}
	return r.stageRunnerContribution(receipt, runnerID, sessionHash, now)
}

func (r *RunnersRepo) stageRunnerContribution(receipt domain.RunnerContributionReceipt, runnerID, sessionHash string, now time.Time) (domain.RunnerContributionReceipt, error) {
	var staged domain.RunnerContributionReceipt
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		task, spec, err := r.contributionTaskForUpload(tx, receipt.TaskID, runnerID, sessionHash, receipt.Attempt, receipt.RepositoryID, now)
		if err != nil {
			return err
		}
		if err := validateContributionReceipt(receipt, spec); err != nil {
			return err
		}
		if err := contributionBlobExists(tx, receipt); err != nil {
			return err
		}
		for _, pending := range task.PendingContributions {
			if pending.Attempt == receipt.Attempt && pending.RepositoryID == receipt.RepositoryID {
				if !sameContribution(pending, receipt) {
					return ErrRunnerContributionConflict
				}
				staged = pending
				return nil
			}
		}
		receipt.ID = "rc-" + rand.Text()
		receipt.CreatedAt, receipt.AcceptedAt = now.UTC(), time.Time{}
		receipt.StorageKey = receipt.SHA256
		task.PendingContributions = append(task.PendingContributions, receipt)
		task.UpdatedAt = now
		if err := tx.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Update("payload", runnerTaskPayloadBytes(task)).Error; err != nil {
			return err
		}
		staged = receipt
		return nil
	})
	return staged, err
}

// acceptRunnerContributions runs only while CompleteRunnerTask holds the live
// task lock; accepted metadata and terminal success use the same transaction.
func acceptRunnerContributions(tx *gorm.DB, task *domain.RunnerTask, outputs *domain.RunnerTaskOutputs, now time.Time) error {
	var ids []string
	if outputs != nil {
		ids = outputs.ContributionReceipts
	}
	if len(ids) != len(task.ContributionSpecs) {
		return invalidContribution("successful completion requires one server receipt per assigned repository")
	}
	accepted := make([]domain.RunnerContributionReceipt, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		var receipt domain.RunnerContributionReceipt
		for _, pending := range task.PendingContributions {
			if pending.ID == id && pending.TaskID == task.ID && pending.Attempt == task.Attempt {
				receipt = pending
				break
			}
		}
		if receipt.ID == "" || seen[receipt.RepositoryID] {
			return invalidContribution("unknown, duplicate or previous-attempt receipt")
		}
		seen[receipt.RepositoryID] = true
		spec, err := contributionSpec(*task, receipt.RepositoryID)
		if err != nil {
			return err
		}
		if err := validateContributionReceipt(receipt, spec); err != nil {
			return err
		}
		if err := contributionBlobExists(tx, receipt); err != nil {
			return err
		}
		receipt.AcceptedAt = now.UTC()
		receipt.StorageKey = receipt.SHA256
		accepted = append(accepted, receipt)
	}
	task.AcceptedContributions = accepted
	return nil
}
