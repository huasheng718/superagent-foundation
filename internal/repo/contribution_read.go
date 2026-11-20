package repo

import (
	"agentflow/internal/domain"
)

// LatestRunnerExecutionTaskForNode reads full history; management's capped
// recent-task list must never decide whether a result was superseded.
func (r *RunnersRepo) LatestRunnerExecutionTaskForNode(runID, nodeID string) (domain.RunnerTask, error) {
	var row runnerTaskRow
	query := r.DB.Model(&runnerTaskRow{}).Where(r.sqlJSONText("$.runId")+" = ? AND "+r.sqlJSONText("$.nodeId")+" = ? AND COALESCE("+r.sqlJSONText("$.action")+", '') = ''", "$.runId", runID, "$.nodeId", nodeID, "$.action")
	if err := query.Order("created_at DESC").Order("id DESC").Take(&row).Error; err != nil {
		return domain.RunnerTask{}, err
	}
	return runnerTaskFromRow(row), nil
}
