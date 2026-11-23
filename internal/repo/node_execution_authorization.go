package repo

import (
	"encoding/json"
	"errors"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// takeRunnerTaskForSession is the task-plane transaction boundary. A session
// resolved before decoding/streaming is only a captured principal: recheck it
// after Run, sorted source/current tasks and registry locks before any effect.
// The caller must retain this transaction through its mutation or byte read.
func (r *RunnersRepo) takeRunnerTaskForSession(tx *gorm.DB, id, runnerID, sessionHash string, attempt int, now time.Time) (runnerTaskRow, error) {
	return r.takeRunnerTaskForSessionMode(tx, id, runnerID, sessionHash, attempt, now, false)
}

// takeRunnerTaskForSessionMode is identical to the normal callback fence,
// except that canceled terminal callbacks may observe the owning Run's
// durable cancel transition. No lease, session, placement or lineage check is
// relaxed by this mode.
func (r *RunnersRepo) takeRunnerTaskForSessionMode(tx *gorm.DB, id, runnerID, sessionHash string, attempt int, now time.Time, allowCanceled bool) (runnerTaskRow, error) {
	if !domain.ValidWorkspaceContextSHA256(sessionHash) {
		return runnerTaskRow{}, ErrRunnerPlacementSessionInvalid
	}
	if id == "" || runnerID == "" || attempt < 1 {
		return runnerTaskRow{}, ErrRunnerTaskLeaseLost
	}
	var peek runnerTaskRow
	if err := tx.Where("id = ?", id).Take(&peek).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return runnerTaskRow{}, ErrRunnerTaskLeaseLost
		}
		return runnerTaskRow{}, err
	}
	var payload runnerTaskPayload
	if json.Unmarshal(peek.Payload, &payload) != nil {
		return runnerTaskRow{}, ErrRunnerTaskLeaseLost
	}
	task := runnerTaskFromRow(peek)
	var run domain.Run
	var locked map[string]domain.RunnerTask
	if task.ExecutionPlacement != nil {
		if r.runs == nil || task.Action != "" || task.MaintenanceKey != nil || task.RunnerID != runnerID {
			return runnerTaskRow{}, ErrRunnerTaskLeaseLost
		}
		var err error
		run, err = r.runs.lockRunnerExecutionRunTxMode(tx, task.RunID, now, allowCanceled)
		if err != nil {
			return runnerTaskRow{}, err
		}
		locked, err = r.lockNodeExecutionTasksTx(tx, task)
		if err != nil {
			return runnerTaskRow{}, err
		}
	} else if task.MaintenanceKey != nil {
		if err := r.validateRunnerMaintenanceTaskTx(tx, task, runnerID, sessionHash, now); err != nil {
			return runnerTaskRow{}, err
		}
	} else if runnerTaskHasNodeAuthority(task) {
		return runnerTaskRow{}, ErrRunnerTaskLeaseLost
	}
	var row runnerTaskRow
	if err := r.locking(tx).Where("id = ?", id).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return runnerTaskRow{}, ErrRunnerTaskLeaseLost
		}
		return runnerTaskRow{}, err
	}
	current := runnerTaskFromRow(row)
	// A new assignment after the peek could require a different Run/source
	// lock set. Never acquire the additional locks out of order; retry instead.
	before, err := domain.RunnerContributionContextSHA(task)
	if err != nil {
		return runnerTaskRow{}, err
	}
	after, err := domain.RunnerContributionContextSHA(current)
	if err != nil || before != after || current.RunID != task.RunID || current.RunnerID != task.RunnerID {
		return runnerTaskRow{}, ErrRunnerTaskLeaseLost
	}
	if current.ExecutionPlacement != nil {
		if err := r.runs.validateRunnerExecutionStateMode(run, current, allowCanceled && current.DesiredState == domain.RunnerTaskDesiredCancel); err != nil {
			return runnerTaskRow{}, err
		}
		if err := r.validateNodeContributionInputsTx(tx, run, current, locked); err != nil {
			return runnerTaskRow{}, err
		}
		if err := r.validateRunnerPlacementTx(tx, *current.ExecutionPlacement, sessionHash, now); err != nil {
			return runnerTaskRow{}, err
		}
	} else if current.MaintenanceKey != nil {
		if err := r.validateRunnerMaintenanceTaskTx(tx, current, runnerID, sessionHash, now); err != nil {
			return runnerTaskRow{}, err
		}
	} else {
		if err := r.validateRunnerClaimSessionTx(tx, runnerID, sessionHash, now); err != nil {
			return runnerTaskRow{}, err
		}
	}
	checkedAt := leaseCheckTime(tx, now)
	if current.ExecutionPlacement != nil {
		if !allowCanceled || current.DesiredState != domain.RunnerTaskDesiredCancel {
			if err := r.runs.validateRunnerExecutionRunAt(run, checkedAt); err != nil {
				return runnerTaskRow{}, err
			}
		}
	}
	if row.LeaseOwner != runnerID || row.Attempt != attempt || (row.Status != domain.RunnerTaskLeased && row.Status != domain.RunnerTaskRunning) || row.LeaseUntil == nil || !row.LeaseUntil.After(checkedAt) || domain.ExecutionDeadlineExpired(current.ExecutionDeadline, checkedAt) {
		return runnerTaskRow{}, ErrRunnerTaskLeaseLost
	}
	return row, nil
}

// validateRunnerMaintenanceTaskTx authorizes durable placed cleanup callbacks
// against the frozen runner registration. Maintenance tasks carry no node
// execution authority, but their registration key must remain immutable across
// Start/Renew/Event/Complete just like a placed task's execution placement.
func (r *RunnersRepo) validateRunnerMaintenanceTaskTx(tx *gorm.DB, task domain.RunnerTask, runnerID, sessionHash string, now time.Time) error {
	key := task.MaintenanceKey
	if key == nil || task.Action != domain.RunnerTaskActionCleanupRun || task.RunID == "" || key.RunID != task.RunID || key.Action != task.Action || key.RunnerID != runnerID || key.RegistrationSHA256 == "" {
		return ErrRunnerTaskLeaseLost
	}
	if err := r.validateRunnerClaimSessionTx(tx, runnerID, sessionHash, now); err != nil {
		return err
	}
	registration, err := domain.RunnerRegistrationSHA256(sessionHash)
	if err != nil || key.RegistrationSHA256 != registration {
		return ErrRunnerTaskLeaseLost
	}
	return nil
}

// RunnerTaskControlForSession returns the small control projection under the
// same current-session/attempt/lineage fence as callbacks. This read never
// returns invocation credentials or contribution bytes.
func (r *RunnersRepo) RunnerTaskControlForSession(id, runnerID, sessionHash string, attempt int, now time.Time) (domain.RunnerTask, error) {
	var control domain.RunnerTask
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		row, err := r.takeRunnerTaskForSession(tx, id, runnerID, sessionHash, attempt, now)
		if err != nil {
			return err
		}
		control = domain.RunnerTask{ID: row.ID, DesiredState: row.DesiredState, Status: row.Status, LeaseOwner: row.LeaseOwner, Attempt: row.Attempt, LeaseUntil: row.LeaseUntil, ExecutionDeadline: runnerTaskFromRow(row).ExecutionDeadline}
		return nil
	})
	return control, err
}
