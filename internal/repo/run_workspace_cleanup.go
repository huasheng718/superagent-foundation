package repo

import (
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func cleanupParticipantFromRow(row runWorkspaceCleanupParticipantRow) domain.RunnerWorkspaceCleanupParticipant {
	participant := domain.RunnerWorkspaceCleanupParticipant{
		RunID: row.RunID, Action: row.Action, RunnerID: row.RunnerID,
		RegistrationSHA256: row.RegistrationSHA256, Status: row.Status,
		TaskID: row.TaskID, Error: row.Error,
	}
	if row.Updated != nil {
		participant.UpdatedAt = *row.Updated
	}
	if row.Completed != nil {
		participant.CompletedAt = *row.Completed
	}
	return participant
}

// SyncWorkspaceCleanupParticipants creates the durable participant set. It
// never removes a row: old attempts remain required until their own cleanup
// registration has succeeded.
func (r *RunsRepo) SyncWorkspaceCleanupParticipants(runID string, participants []domain.RunnerWorkspaceParticipant, now time.Time) error {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		for _, participant := range participants {
			if strings.TrimSpace(participant.RunnerID) == "" || !domain.ValidWorkspaceContextSHA256(participant.RegistrationSHA256) {
				continue
			}
			values := map[string]any{
				"run_id": runID, "action": domain.RunnerTaskActionCleanupRun,
				"runner_id": participant.RunnerID, "registration_sha256": participant.RegistrationSHA256,
				"status": domain.RunnerWorkspaceCleanupPending, "updated_at": now,
				"created_at": now,
			}
			inserted, err := insertIgnore(tx, "run_workspace_cleanup_participants", values)
			if err != nil {
				return err
			}
			if !inserted {
				// A completed participant remains completed across reconciliation;
				// pending/failed rows are refreshed by Record below.
				continue
			}
		}
		return nil
	})
}

func (r *RunsRepo) WorkspaceCleanupParticipants(runID string) []domain.RunnerWorkspaceCleanupParticipant {
	var rows []runWorkspaceCleanupParticipantRow
	if err := r.DB.Where("run_id = ? AND action = ?", runID, domain.RunnerTaskActionCleanupRun).Order("runner_id, registration_sha256").Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]domain.RunnerWorkspaceCleanupParticipant, 0, len(rows))
	for _, row := range rows {
		out = append(out, cleanupParticipantFromRow(row))
	}
	return out
}

// RecordWorkspaceCleanupParticipant is an idempotent per-registration state
// transition. A success cannot be overwritten by a later stale reconciliation.
func (r *RunsRepo) RecordWorkspaceCleanupParticipant(participant domain.RunnerWorkspaceCleanupParticipant, now time.Time) error {
	if strings.TrimSpace(participant.RunID) == "" || strings.TrimSpace(participant.RunnerID) == "" || !domain.ValidWorkspaceContextSHA256(participant.RegistrationSHA256) {
		return nil
	}
	if participant.Action == "" {
		participant.Action = domain.RunnerTaskActionCleanupRun
	}
	updates := map[string]any{"status": participant.Status, "task_id": participant.TaskID, "error": participant.Error, "updated_at": now}
	if participant.Status == domain.RunnerWorkspaceCleanupSuccess || participant.Status == domain.RunnerWorkspaceCleanupFailure {
		updates["completed_at"] = now
	}
	return r.DB.Model(&runWorkspaceCleanupParticipantRow{}).
		Where("run_id = ? AND action = ? AND runner_id = ? AND registration_sha256 = ?", participant.RunID, participant.Action, participant.RunnerID, participant.RegistrationSHA256).
		Updates(updates).Error
}

// MarkPlacedWorkspaceCleaned is the final aggregate transition. It is the
// only placed cleanup path allowed to write WorkspaceCleanedAt.
func (r *RunsRepo) MarkPlacedWorkspaceCleaned(runID string, now time.Time) error {
	return r.DB.Model(&runRow{}).Where("id = ? AND status IN ? AND workspace_cleaned_at IS NULL", runID, []string{"succeeded", "cancelled"}).Updates(map[string]any{
		"workspace_cleaned_at": now,
		"sandbox_id":           "",
	}).Error
}
