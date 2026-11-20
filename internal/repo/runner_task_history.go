package repo

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// RunnerTaskForSessionHistory authorizes a task/history read against the
// current registration identity. It intentionally does not require an active
// Run or Node: terminal history remains readable to the same registration,
// while a re-registered machine cannot read the previous machine's payload.
func (r *RunnersRepo) RunnerTaskForSessionHistory(id, runnerID, sessionHash string, now time.Time) (domain.RunnerTask, bool, error) {
	var task domain.RunnerTask
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		task, _, err = r.runnerTaskForSessionHistoryTx(tx, id, runnerID, sessionHash, now)
		return err
	})
	return task, task.ID != "", err
}

func (r *RunnersRepo) runnerTaskForSessionHistoryTx(tx *gorm.DB, id, runnerID, sessionHash string, now time.Time) (domain.RunnerTask, bool, error) {
	var task domain.RunnerTask
	var row runnerTaskRow
	if err := r.locking(tx).Where("id = ?", id).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return task, false, nil
		}
		return task, false, err
	}
	candidate := runnerTaskFromRow(row)
	var payload runnerTaskPayload
	_ = json.Unmarshal(row.Payload, &payload)
	// Legacy payloads have no frozen registration digest. They remain
	// readable only while the same session owns a live lease; terminal or
	// queued legacy payloads cannot be proven to belong to a re-registration.
	if candidate.ExecutionPlacement == nil {
		registration, _ := domain.RunnerRegistrationSHA256(sessionHash)
		if payload.RegistrationSHA256 != "" && payload.RegistrationSHA256 != registration {
			return task, false, nil
		}
		if payload.RegistrationSHA256 == "" && candidate.RunnerID != "" && (candidate.LeaseOwner != runnerID || (candidate.Status != domain.RunnerTaskLeased && candidate.Status != domain.RunnerTaskRunning)) {
			return task, false, nil
		}
	}
	if candidate.LeaseOwner != "" && candidate.LeaseOwner != runnerID {
		return task, false, nil
	}
	if candidate.LeaseOwner == "" && candidate.RunnerID != "" && candidate.RunnerID != runnerID {
		return task, false, nil
	}
	// History access is registration-bound and may remain available after a
	// daemon disconnects. Claim/preflight additionally require ONLINE, but a
	// terminal task must not become unreadable merely because its runner is
	// offline; expiry and token rotation are still checked under the lock.
	if err := r.validateRunnerHistorySessionTx(tx, runnerID, sessionHash, now); err != nil {
		return task, false, err
	}
	if candidate.ExecutionPlacement != nil {
		if candidate.ExecutionPlacement.RunnerID != runnerID {
			return task, false, nil
		}
		registration, err := domain.RunnerRegistrationSHA256(sessionHash)
		if err != nil || candidate.ExecutionPlacement.RegistrationSHA256 != registration {
			return task, false, nil
		}
	}
	task = candidate
	return task, true, nil
}

func (r *RunnersRepo) validateRunnerHistorySessionTx(tx *gorm.DB, runnerID, sessionHash string, now time.Time) error {
	var row runnerRow
	if err := r.locking(tx).Where("id = ?", runnerID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrRunnerPlacementSessionInvalid
		}
		return err
	}
	if !domain.ValidWorkspaceContextSHA256(sessionHash) || row.SessionExpiresAt == nil || !row.SessionExpiresAt.After(leaseCheckTime(tx, now)) || subtle.ConstantTimeCompare([]byte(row.SessionTokenHash), []byte(sessionHash)) != 1 {
		return ErrRunnerPlacementSessionInvalid
	}
	return nil
}

func (r *RunnersRepo) RunnerTaskEventsForSession(id, runnerID, sessionHash string, now time.Time) ([]domain.RunnerTaskEvent, bool, error) {
	var events []domain.RunnerTaskEvent
	found := false
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		task, ok, err := r.runnerTaskForSessionHistoryTx(tx, id, runnerID, sessionHash, now)
		if err != nil || !ok {
			return err
		}
		found = true
		events = runnerTaskEventsFromQuery(tx.Where("task_id = ?", task.ID).Order("attempt").Order("seq"))
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return events, found, nil
}

// retireObsoletePlacedTask retires only a queued task whose frozen authority
// can no longer become valid. It authenticates the current session before any
// mutation, and leaves active Engine lease/reclaim conflicts untouched.
func (r *RunnersRepo) retireObsoletePlacedTask(runnerID, sessionHash string, providers []string, now time.Time) (bool, error) {
	if err := r.DB.Transaction(func(tx *gorm.DB) error {
		return r.validateRunnerClaimSessionTx(tx, runnerID, sessionHash, now)
	}); err != nil {
		return false, err
	}
	retired := false
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var candidates []runnerTaskRow
		q := tx.Where("status = ? AND (runner_id = '' OR runner_id = ?) AND provider IN ?", domain.RunnerTaskQueued, runnerID, providers)
		var candidateCount int64
		if err := q.Model(&runnerTaskRow{}).Count(&candidateCount).Error; err != nil || candidateCount < 2 {
			return err
		}
		if err := q.Order("created_at, id").Limit(1).Find(&candidates).Error; err != nil || len(candidates) == 0 {
			return err
		}
		row := candidates[0]
		var payload runnerTaskPayload
		if json.Unmarshal(row.Payload, &payload) != nil {
			return nil
		}
		task := runnerTaskFromRow(row)
		if task.ExecutionPlacement == nil || task.ExecutionPlacement.RunnerID != runnerID || task.Execution == nil {
			return nil
		}
		obsolete := false
		// Lock Run first when it exists. An active Engine lease is never
		// retired by this cleanup path; that conflict may be transient.
		if r.runs != nil && task.RunID != "" {
			var runRowValue runRow
			if err := r.locking(tx).Where("id = ?", task.RunID).Take(&runRowValue).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					// A missing Run is permanently obsolete.
					obsolete = true
				} else {
					return err
				}
			} else {
				run := runFromRow(runRowValue)
				if run.Status == "running" && !run.CancelRequested && !run.PauseRequested && run.WorkspaceCleanedAt.IsZero() && (run.OwnerInstanceID == "" || (!run.LeaseUntil.IsZero() && run.LeaseUntil.After(leaseCheckTime(tx, now)))) {
					var nodeRow runNodeRow
					if err := r.locking(tx).Where("run_id = ? AND node_id = ?", task.RunID, task.NodeID).Take(&nodeRow).Error; err == nil {
						var node domain.NodeRun
						if json.Unmarshal(nodeRow.Payload, &node) == nil && node.Attempt == task.Execution.NodeAttempt {
							if task.Execution.Role == domain.NodeTaskRoleAuthor && node.ExecutionTaskID == task.ID && node.ExecutionSequence == task.Execution.Sequence {
								// Current author; registry drift is evaluated below.
							}
							if task.Execution.Role == domain.NodeTaskRoleVerification && node.ExecutionTaskID != "" && node.ExecutionTaskID != task.ID && node.ExecutionSequence == task.Execution.Sequence && task.Execution.PreviousAuthorTaskID == node.ExecutionTaskID {
								// Current verification; registry drift is evaluated below.
							}
							if !(task.Execution.Role == domain.NodeTaskRoleAuthor && node.ExecutionTaskID == task.ID && node.ExecutionSequence == task.Execution.Sequence) && !(task.Execution.Role == domain.NodeTaskRoleVerification && node.ExecutionTaskID != "" && node.ExecutionTaskID != task.ID && node.ExecutionSequence == task.Execution.Sequence && task.Execution.PreviousAuthorTaskID == node.ExecutionTaskID) {
								obsolete = true
							}
						} else {
							obsolete = true
						}
					} else {
						obsolete = true
					}
					// Active run with a superseded Node is permanently obsolete.
				} else if run.Status == "running" && !run.CancelRequested && !run.PauseRequested && run.WorkspaceCleanedAt.IsZero() && run.OwnerInstanceID != "" && (run.LeaseUntil.IsZero() || !run.LeaseUntil.After(leaseCheckTime(tx, now))) {
					return nil
				} else {
					obsolete = true
				}
			}
		}
		var registry runnerRow
		if err := r.locking(tx).Where("id = ?", runnerID).Take(&registry).Error; err != nil {
			return err
		}
		if !domain.ValidWorkspaceContextSHA256(sessionHash) || registry.SessionExpiresAt == nil || !registry.SessionExpiresAt.After(leaseCheckTime(tx, now)) || registry.SessionTokenHash != sessionHash {
			return ErrRunnerPlacementSessionInvalid
		}
		registration, err := domain.RunnerRegistrationSHA256(sessionHash)
		if err != nil {
			return err
		}
		if !obsolete && task.ExecutionPlacement.RegistrationSHA256 == registration {
			return nil
		}
		task.Status = domain.RunnerTaskFailed
		task.Error = "frozen runner placement is obsolete"
		completed := leaseCheckTime(tx, now)
		task.CompletedAt, task.UpdatedAt = &completed, completed
		task.LeaseOwner, task.LeaseUntil, task.DesiredState = "", nil, ""
		scrubRunnerTaskCredentials(&task)
		if err := tx.Model(&runnerTaskRow{}).Where("id = ? AND status = ?", task.ID, domain.RunnerTaskQueued).Updates(runnerTaskRowValues(task)).Error; err != nil {
			return err
		}
		retired = true
		return nil
	})
	return retired, err
}
