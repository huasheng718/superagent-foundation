package engine

import (
	"crypto/rand"
	"fmt"
	"log"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// placedWorkspaceParticipants is derived from the complete immutable task
// history. It deliberately includes failed, cancelled and older attempts so a
// retry cannot make an old Runner workspace disappear from the cleanup set.
func (e *Engine) placedWorkspaceParticipants(runID string) []domain.RunnerWorkspaceParticipant {
	seen := map[string]bool{}
	var out []domain.RunnerWorkspaceParticipant
	for _, task := range e.repo.RunnerTasksForRun(runID) {
		if task.ExecutionPlacement == nil {
			continue
		}
		p := *task.ExecutionPlacement
		if strings.TrimSpace(p.RunnerID) == "" || !domain.ValidWorkspaceContextSHA256(p.RegistrationSHA256) {
			continue
		}
		key := p.RunnerID + "\x00" + p.RegistrationSHA256
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, domain.RunnerWorkspaceParticipant{RunnerID: p.RunnerID, RegistrationSHA256: p.RegistrationSHA256, Placement: p})
	}
	return out
}

func (e *Engine) cleanupPlacedWorkspace(run domain.Run, now time.Time, participants []domain.RunnerWorkspaceParticipant) {
	if len(participants) == 0 {
		return
	}
	if err := e.repo.SyncWorkspaceCleanupParticipants(run.ID, participants, now); err != nil {
		log.Printf("sync placed workspace cleanup participants %s: %v", run.ID, err)
		return
	}

	// Release local staging before any aggregate success is recorded. A failed
	// local release leaves every participant pending for the next sweep.
	if err := e.releaseRunEnvironment(run.ID, true); err != nil {
		log.Printf("release placed workspace staging %s: %v", run.ID, err)
		return
	}

	history := e.repo.RunnerTasksForRun(run.ID)
	previous := map[string]domain.RunnerWorkspaceCleanupParticipant{}
	for _, state := range e.repo.WorkspaceCleanupParticipants(run.ID) {
		previous[state.RunnerID+"\x00"+state.RegistrationSHA256] = state
	}
	allDone := true
	for _, participant := range participants {
		key := &domain.RunnerMaintenanceKey{RunID: run.ID, Action: domain.RunnerTaskActionCleanupRun, RunnerID: participant.RunnerID, RegistrationSHA256: participant.RegistrationSHA256}
		if _, runnerExists := e.repo.Runner(participant.RunnerID); !runnerExists {
			// Runner deregistration performs its own workspace removal before the
			// registry row disappears, so there is no endpoint left to queue.
			_ = e.repo.RecordWorkspaceCleanupParticipant(domain.RunnerWorkspaceCleanupParticipant{RunID: run.ID, Action: key.Action, RunnerID: key.RunnerID, RegistrationSHA256: key.RegistrationSHA256, Status: domain.RunnerWorkspaceCleanupSuccess}, now)
			continue
		}

		var active *domain.RunnerTask
		success := false
		terminalFailure := false
		latestPlacementAt := time.Time{}
		latestCleanupSuccessAt := time.Time{}
		for i := range history {
			task := history[i]
			if task.ExecutionPlacement != nil && task.ExecutionPlacement.RunnerID == participant.RunnerID && task.ExecutionPlacement.RegistrationSHA256 == participant.RegistrationSHA256 && task.CreatedAt.After(latestPlacementAt) {
				latestPlacementAt = task.CreatedAt
			}
			if task.Action != domain.RunnerTaskActionCleanupRun || task.MaintenanceKey == nil || !sameRunnerMaintenanceKey(task.MaintenanceKey, key) {
				continue
			}
			switch task.Status {
			case domain.RunnerTaskSuccess:
				success = true
				if task.CompletedAt != nil && task.CompletedAt.After(latestCleanupSuccessAt) {
					latestCleanupSuccessAt = *task.CompletedAt
				}
			case domain.RunnerTaskQueued, domain.RunnerTaskLeased, domain.RunnerTaskRunning:
				copy := task
				active = &copy
			case domain.RunnerTaskFailed, domain.RunnerTaskCanceled:
				terminalFailure = true
			}
		}
		if success && (latestPlacementAt.IsZero() || latestCleanupSuccessAt.IsZero() || !latestPlacementAt.After(latestCleanupSuccessAt)) {
			_ = e.repo.RecordWorkspaceCleanupParticipant(domain.RunnerWorkspaceCleanupParticipant{RunID: run.ID, Action: key.Action, RunnerID: key.RunnerID, RegistrationSHA256: key.RegistrationSHA256, Status: domain.RunnerWorkspaceCleanupSuccess}, now)
			continue
		}
		if active != nil && !latestPlacementAt.IsZero() && active.CreatedAt.Before(latestPlacementAt) {
			// A new attempt owns the current workspace generation. Do not let a
			// cleanup queued for an older attempt remove it.
			if err := e.repo.CancelRunnerTask(active.ID, now); err != nil {
				log.Printf("cancel stale placed cleanup %s: %v", active.ID, err)
				allDone = false
				continue
			}
			if updated, ok := e.repo.RunnerTask(active.ID); ok && (updated.Status == domain.RunnerTaskLeased || updated.Status == domain.RunnerTaskRunning) {
				allDone = false
				continue
			}
			active = nil
		}
		if active == nil {
			stateKey := participant.RunnerID + "\x00" + participant.RegistrationSHA256
			if terminalFailure && previous[stateKey].Status != domain.RunnerWorkspaceCleanupFailure {
				_ = e.repo.RecordWorkspaceCleanupParticipant(domain.RunnerWorkspaceCleanupParticipant{RunID: run.ID, Action: key.Action, RunnerID: key.RunnerID, RegistrationSHA256: key.RegistrationSHA256, Status: domain.RunnerWorkspaceCleanupFailure, Error: "cleanup task failed or was canceled"}, now)
				allDone = false
				continue
			}
			task := domain.RunnerTask{
				ID: "rt-cleanup-" + rand.Text(), RunnerID: participant.RunnerID, Provider: "terminal",
				Action: domain.RunnerTaskActionCleanupRun, Workspace: domain.IsolatedRunWorkspaceRel(run.ID),
				Status: domain.RunnerTaskQueued, MaxAttempts: 3, RunID: run.ID, Sandbox: run.WorkflowSnapshot.SandboxSnapshot,
				MaintenanceKey: key,
			}
			if err := e.applyRunnerTaskPolicy(run, &task); err != nil {
				_ = e.repo.RecordWorkspaceCleanupParticipant(domain.RunnerWorkspaceCleanupParticipant{RunID: run.ID, Action: key.Action, RunnerID: key.RunnerID, RegistrationSHA256: key.RegistrationSHA256, Status: domain.RunnerWorkspaceCleanupFailure, Error: err.Error()}, now)
				allDone = false
				continue
			}
			queued, created, err := e.repo.QueueRunnerMaintenanceTask(task)
			if err != nil {
				_ = e.repo.RecordWorkspaceCleanupParticipant(domain.RunnerWorkspaceCleanupParticipant{RunID: run.ID, Action: key.Action, RunnerID: key.RunnerID, RegistrationSHA256: key.RegistrationSHA256, Status: domain.RunnerWorkspaceCleanupFailure, Error: err.Error()}, now)
				allDone = false
				continue
			}
			active = &queued
			if created {
				e.notifyRunnerDispatch("terminal")
			}
		}
		state := domain.RunnerWorkspaceCleanupPending
		taskID := ""
		if active != nil {
			taskID = active.ID
			if active.Status == domain.RunnerTaskFailed || active.Status == domain.RunnerTaskCanceled {
				state = domain.RunnerWorkspaceCleanupFailure
			}
		}
		_ = e.repo.RecordWorkspaceCleanupParticipant(domain.RunnerWorkspaceCleanupParticipant{RunID: run.ID, Action: key.Action, RunnerID: key.RunnerID, RegistrationSHA256: key.RegistrationSHA256, Status: state, TaskID: taskID}, now)
		allDone = false
	}

	for _, state := range e.repo.WorkspaceCleanupParticipants(run.ID) {
		if state.Status != domain.RunnerWorkspaceCleanupSuccess {
			allDone = false
			break
		}
	}
	if allDone {
		if err := e.repo.MarkPlacedWorkspaceCleaned(run.ID, now); err != nil {
			log.Printf("mark placed workspace cleaned %s: %v", run.ID, err)
		}
	}
}

// queuePlacedWorkspaceCleanup is used by DeleteRun before the Run row is
// removed. Task snapshots remain durable after deletion, so each frozen
// registration receives its own cleanup identity.
func (e *Engine) queuePlacedWorkspaceCleanup(run domain.Run, now time.Time, participants []domain.RunnerWorkspaceParticipant) error {
	if err := e.repo.SyncWorkspaceCleanupParticipants(run.ID, participants, now); err != nil {
		return err
	}
	for _, participant := range participants {
		key := &domain.RunnerMaintenanceKey{RunID: run.ID, Action: domain.RunnerTaskActionCleanupRun, RunnerID: participant.RunnerID, RegistrationSHA256: participant.RegistrationSHA256}
		if _, ok := e.repo.Runner(participant.RunnerID); !ok {
			_ = e.repo.RecordWorkspaceCleanupParticipant(domain.RunnerWorkspaceCleanupParticipant{RunID: run.ID, Action: key.Action, RunnerID: key.RunnerID, RegistrationSHA256: key.RegistrationSHA256, Status: domain.RunnerWorkspaceCleanupSuccess}, now)
			continue
		}
		active := false
		for _, existing := range e.repo.RunnerTasksForRun(run.ID) {
			if existing.Action == key.Action && existing.MaintenanceKey != nil && sameRunnerMaintenanceKey(existing.MaintenanceKey, key) && (existing.Status == domain.RunnerTaskQueued || existing.Status == domain.RunnerTaskLeased || existing.Status == domain.RunnerTaskRunning) {
				active = true
				break
			}
		}
		if active {
			continue
		}
		task := domain.RunnerTask{ID: "rt-cleanup-" + rand.Text(), RunnerID: participant.RunnerID, Provider: "terminal", Action: key.Action, Workspace: domain.IsolatedRunWorkspaceRel(run.ID), Status: domain.RunnerTaskQueued, MaxAttempts: 3, RunID: run.ID, Sandbox: run.WorkflowSnapshot.SandboxSnapshot, MaintenanceKey: key}
		if err := e.applyRunnerTaskPolicy(run, &task); err != nil {
			return err
		}
		queued, created, err := e.repo.QueueRunnerMaintenanceTask(task)
		if err != nil {
			return err
		}
		if created {
			e.notifyRunnerDispatch("terminal")
		}
		_ = e.repo.RecordWorkspaceCleanupParticipant(domain.RunnerWorkspaceCleanupParticipant{RunID: run.ID, Action: key.Action, RunnerID: key.RunnerID, RegistrationSHA256: key.RegistrationSHA256, Status: domain.RunnerWorkspaceCleanupPending, TaskID: queued.ID}, now)
	}
	return nil
}

// drainPlacedCleanup fences a retry/workspace write against every participant
// cleanup task, including registrations from older attempts.
func (e *Engine) drainPlacedCleanup(runID string) error {
	now := time.Now().UTC()
	for _, task := range e.repo.RunnerTasksForRun(runID) {
		if task.Action != domain.RunnerTaskActionCleanupRun || task.MaintenanceKey == nil {
			continue
		}
		if task.Status != domain.RunnerTaskQueued && task.Status != domain.RunnerTaskLeased && task.Status != domain.RunnerTaskRunning {
			continue
		}
		if err := e.repo.CancelRunnerTask(task.ID, now); err != nil {
			return err
		}
		e.NotifyRunnerTaskChanged(task.ID)
		if updated, ok := e.repo.RunnerTask(task.ID); ok && (updated.Status == domain.RunnerTaskLeased || updated.Status == domain.RunnerTaskRunning) {
			return fmt.Errorf("run workspace cleanup is still active; retry after it finishes")
		}
	}
	return nil
}

func sameRunnerMaintenanceKey(a, b *domain.RunnerMaintenanceKey) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.RunID == b.RunID && a.Action == b.Action && a.RunnerID == b.RunnerID && a.RegistrationSHA256 == b.RegistrationSHA256
}
