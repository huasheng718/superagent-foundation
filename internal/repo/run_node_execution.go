package repo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// lockRunnerExecutionRunTx belongs to RunsRepo. Call it before any task or
// registry locks in the same transaction. NodeAttempt, rather than the Engine
// lease generation, is the durable Runner authority: an Engine takeover can
// retain an already executing Node attempt.
func (r *RunsRepo) lockRunnerExecutionRunTx(tx *gorm.DB, runID string, now time.Time) (domain.Run, error) {
	return r.lockRunnerExecutionRunTxMode(tx, runID, now, false)
}

// lockRunnerExecutionRunTxMode keeps the normal execution fence strict while
// allowing the already-authorized runner to report a durable cancellation
// after the owning Run has transitioned out of running. The relaxed mode is
// only used by the canceled terminal callback; task/session/lease checks stay
// in RunnersRepo.
func (r *RunsRepo) lockRunnerExecutionRunTxMode(tx *gorm.DB, runID string, now time.Time, allowCanceled bool) (domain.Run, error) {
	row, err := r.takeRunForUpdate(tx, runID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return domain.Run{}, runnerExecutionConflict("owning Run is missing")
		}
		return domain.Run{}, err
	}
	run := runFromRow(row)
	// Unlike management's best-effort readers, authorization must not ignore
	// malformed JSON and silently interpret partially decoded zero values.
	if err := decodeRunnerExecutionObject(row.Snapshot, &run.WorkflowSnapshot); err != nil {
		return domain.Run{}, runnerExecutionConflict("invalid owning workflow snapshot")
	}
	if err := decodeRunnerExecutionObject(row.ExecutionContext, &run.ExecutionContext); err != nil {
		return domain.Run{}, runnerExecutionConflict("invalid owning execution context")
	}
	if len(row.Repository) > 0 && !bytes.Equal(bytes.TrimSpace(row.Repository), []byte("null")) {
		if err := decodeRunnerExecutionObject(row.Repository, &run.Repository); err != nil {
			return domain.Run{}, runnerExecutionConflict("invalid owning repository")
		}
	}
	if len(row.Repositories) > 0 && json.Unmarshal(row.Repositories, &run.Repositories) != nil {
		return domain.Run{}, runnerExecutionConflict("invalid owning repositories")
	}
	if !allowCanceled || !run.CancelRequested {
		if err := r.validateRunnerExecutionRunAt(run, leaseCheckTime(tx, now)); err != nil {
			return domain.Run{}, err
		}
	}
	var rows []runNodeRow
	if err := r.locking(tx).Where("run_id = ?", runID).Order("node_id ASC").Find(&rows).Error; err != nil {
		return domain.Run{}, err
	}
	run.Nodes = make(map[string]*domain.NodeRun, len(rows))
	for _, stored := range rows {
		var node domain.NodeRun
		if err := decodeRunnerExecutionObject(stored.Payload, &node); err != nil || node.NodeID != stored.NodeID || node.Status != stored.Status || node.Attempt != stored.Attempt {
			return domain.Run{}, runnerExecutionConflict("node payload differs from indexed identity or state")
		}
		run.Nodes[stored.NodeID] = &node
	}
	// Flags were locked before Node rows. A selected Node's deadline is checked
	// by the task validator, not globally: an expired sibling cannot revoke a
	// healthy branch. Task callers recheck their post-lock clock as well.
	return run, nil
}

func (r *RunsRepo) validateRunnerExecutionRunAt(run domain.Run, now time.Time) error {
	if run.ID == "" || run.Status != "running" || run.CancelRequested || run.PauseRequested || !run.WorkspaceCleanedAt.IsZero() {
		return runnerExecutionConflict("owning Run is not executing")
	}
	if run.OwnerInstanceID != "" && (run.LeaseUntil.IsZero() || !run.LeaseUntil.After(now)) {
		return runnerExecutionConflict("owning Engine lease is not active")
	}
	if !run.WorkspaceEditUntil.IsZero() && run.WorkspaceEditUntil.After(now) {
		return runnerExecutionConflict("owning workspace is under an editor lease")
	}
	return nil
}

// validateRunnerExecutionState validates an existing task. Receipt lineage,
// registration/session and task lease checks remain RunnersRepo's authority.
func (r *RunsRepo) validateRunnerExecutionState(run domain.Run, task domain.RunnerTask) error {
	return r.validateRunnerExecutionStateMode(run, task, false)
}

func (r *RunsRepo) validateRunnerExecutionStateMode(run domain.Run, task domain.RunnerTask, allowCanceled bool) error {
	node, err := r.validateRunnerExecutionTaskShapeMode(run, task, allowCanceled)
	if err != nil {
		return err
	}
	execution := task.Execution
	switch execution.Role {
	case domain.NodeTaskRoleAuthor:
		if node.ExecutionTaskID != task.ID || node.ExecutionSequence != execution.Sequence {
			return runnerExecutionConflict("author is no longer current")
		}
	case domain.NodeTaskRoleVerification:
		if node.ExecutionSequence < 1 || node.ExecutionTaskID == "" || node.ExecutionTaskID == task.ID || execution.Sequence != node.ExecutionSequence || execution.PreviousAuthorTaskID != node.ExecutionTaskID {
			return runnerExecutionConflict("verification author is no longer current")
		}
	}
	return nil
}

// validateRunnerExecutionEnqueueState is separate from callback validation:
// a new author must advance, whereas an existing author must already own the
// exact persisted pointer. A verification uses the current author's sequence.
func (r *RunsRepo) validateRunnerExecutionEnqueueState(run domain.Run, task domain.RunnerTask) error {
	node, err := r.validateRunnerExecutionTaskShape(run, task)
	if err != nil {
		return err
	}
	if task.Status != domain.RunnerTaskQueued || task.Attempt != 0 || task.LeaseOwner != "" || task.LeaseUntil != nil {
		return runnerExecutionConflict("new execution task already has a lease or attempt")
	}
	execution := task.Execution
	if execution.Role == domain.NodeTaskRoleVerification {
		return r.validateRunnerExecutionState(run, task)
	}
	if node.ExecutionSequence < 0 || (node.ExecutionSequence == 0) != (node.ExecutionTaskID == "") || execution.Sequence <= node.ExecutionSequence || execution.Sequence-1 != node.ExecutionSequence || execution.PreviousAuthorTaskID != node.ExecutionTaskID || task.ID == node.ExecutionTaskID {
		return runnerExecutionConflict("new author does not succeed the frozen current author")
	}
	return nil
}

// advanceRunnerExecutionStateTx is called after the transaction has locked
// Run/Nodes, involved tasks and the Runner in that order. It only owns Node
// state; inserting the task and validating accepted predecessors are the
// caller's responsibilities. Verification never changes the author pointer.
func (r *RunsRepo) advanceRunnerExecutionStateTx(tx *gorm.DB, run domain.Run, task domain.RunnerTask) error {
	if err := r.validateRunnerExecutionEnqueueState(run, task); err != nil {
		return err
	}
	if task.Execution.Role == domain.NodeTaskRoleVerification {
		return nil
	}
	expected := run.Nodes[task.NodeID]
	var stored runNodeRow
	if err := r.locking(tx).Where("run_id = ? AND node_id = ?", run.ID, task.NodeID).Take(&stored).Error; err != nil {
		return err
	}
	var current domain.NodeRun
	if decodeRunnerExecutionObject(stored.Payload, &current) != nil || current.NodeID != stored.NodeID || current.Status != stored.Status || current.Attempt != stored.Attempt || !reflect.DeepEqual(current, *expected) {
		return runnerExecutionConflict("node changed before author advance")
	}
	updated := current
	updated.ExecutionSequence, updated.ExecutionTaskID = task.Execution.Sequence, task.ID
	payload, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	result := tx.Model(&runNodeRow{}).Where("run_id = ? AND node_id = ? AND attempt = ? AND status = ?", run.ID, task.NodeID, current.Attempt, current.Status).Update("payload", payload)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return runnerExecutionConflict("node changed before author advance")
	}
	result = tx.Model(&runRow{}).Where("id = ? AND revision = ? AND status = ? AND cancel_requested = ? AND pause_requested = ?", run.ID, run.Revision, "running", false, false).Updates(map[string]any{"revision": run.Revision + 1, "updated_at": leaseCheckTime(tx, time.Time{})})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return runnerExecutionConflict("Run changed before author advance")
	}
	return nil
}

func (r *RunsRepo) validateRunnerExecutionTaskShape(run domain.Run, task domain.RunnerTask) (*domain.NodeRun, error) {
	return r.validateRunnerExecutionTaskShapeMode(run, task, false)
}

func (r *RunsRepo) validateRunnerExecutionTaskShapeMode(run domain.Run, task domain.RunnerTask, allowCanceled bool) (*domain.NodeRun, error) {
	if !allowCanceled || !run.CancelRequested {
		if err := r.validateRunnerExecutionRunAt(run, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	context := run.ExecutionContext
	if !domain.ControlledDelivery(context.DeliveryPolicy) || task.DeliveryPolicy != context.DeliveryPolicy || run.IsWorkspace() || context.ParentRunID != "" || context.RunnerWorkspaceLayout != domain.RunnerWorkspaceLayoutAttempt || len(context.NodePlacements) == 0 {
		return nil, runnerExecutionConflict("owning Run does not authorize isolated node execution")
	}
	if task.ID == "" || strings.TrimSpace(task.ID) != task.ID || task.RunID != run.ID || task.NodeID == "" || task.Action != "" || task.MaintenanceKey != nil || task.ExecutionPlacement == nil || task.Execution == nil {
		return nil, runnerExecutionConflict("execution identity is missing or inconsistent")
	}
	node := run.Nodes[task.NodeID]
	nodeExecuting := node != nil && node.Status == "running"
	nodeCanceled := allowCanceled && run.CancelRequested && node != nil && node.Status == "cancelled"
	if node == nil || node.NodeID != task.NodeID || (!nodeExecuting && !nodeCanceled) || node.Attempt <= 0 || node.ExecutionPlacement == nil || task.Execution.NodeAttempt != node.Attempt {
		return nil, runnerExecutionConflict("owning Node attempt is not executing")
	}
	if domain.ExecutionDeadlineExpired(node.ExecutionDeadline, time.Now().UTC()) || !equalRunnerExecutionDeadline(task.ExecutionDeadline, node.ExecutionDeadline) {
		return nil, runnerExecutionConflict("execution deadline is expired or differs from Node attempt")
	}
	var definition *domain.WorkflowNode
	for i := range run.WorkflowSnapshot.Nodes {
		if run.WorkflowSnapshot.Nodes[i].ID == task.NodeID {
			if definition != nil {
				return nil, runnerExecutionConflict("ambiguous workflow Node")
			}
			definition = &run.WorkflowSnapshot.Nodes[i]
		}
	}
	if definition == nil || (definition.Type != "agent" && definition.Type != "script" && definition.Type != "command") {
		return nil, runnerExecutionConflict("Node is outside executable workflow scope")
	}
	if !domain.ValidWorkspaceContextSHA256(task.ExecutionPlacement.CapabilitySHA256) || !domain.ValidWorkspaceContextSHA256(node.ExecutionPlacement.CapabilitySHA256) {
		return nil, runnerExecutionConflict("frozen placement digest is missing")
	}
	placement, err := domain.CanonicalNodeExecutionPlacement(*task.ExecutionPlacement)
	if err != nil {
		return nil, err
	}
	effective, err := domain.CanonicalNodeExecutionPlacement(*node.ExecutionPlacement)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(placement, effective) || placement.NodeAttempt != node.Attempt || task.RunnerID != placement.RunnerID || placement.ProjectID != context.ProjectID || placement.ResponsibleUserID != context.ResponsibleUserID {
		return nil, runnerExecutionConflict("task placement differs from effective Node authority")
	}
	request, ok := context.NodePlacements[task.NodeID]
	if !ok {
		return nil, runnerExecutionConflict("frozen Node request is missing")
	}
	request, err = domain.CanonicalNodePlacementRequest(request)
	if err != nil {
		return nil, err
	}
	if request.RunnerID != placement.RunnerID || (request.OS != "" && request.OS != placement.OS) || (request.Arch != "" && request.Arch != placement.Arch) {
		return nil, runnerExecutionConflict("placement differs from frozen Node request")
	}
	primary := "terminal"
	if definition.Type == "agent" {
		primary = ""
		for _, runtime := range placement.Runtimes {
			if runtime.Provider != "terminal" {
				if primary != "" {
					return nil, runnerExecutionConflict("ambiguous primary runtime")
				}
				primary = runtime.Provider
			}
		}
	}
	primaryFound, terminalFound := false, false
	for _, runtime := range placement.Runtimes {
		terminalFound = terminalFound || runtime.Provider == "terminal"
		if runtime.Provider == primary {
			primaryFound = true
			for _, required := range request.RequiredCapabilities {
				found := false
				for _, capability := range runtime.Capabilities {
					found = found || capability == required
				}
				if !found {
					return nil, runnerExecutionConflict("snapshot lacks a required primary capability")
				}
			}
		}
	}
	if !primaryFound || !terminalFound || (primary == "terminal" && len(placement.Runtimes) != 1) {
		return nil, runnerExecutionConflict("snapshot runtime selection differs from Node")
	}
	execution := task.Execution
	if execution.NodeAttempt <= 0 || execution.Sequence <= 0 || (execution.Role != domain.NodeTaskRoleAuthor && execution.Role != domain.NodeTaskRoleVerification) {
		return nil, runnerExecutionConflict("invalid execution role or sequence")
	}
	if execution.Role == domain.NodeTaskRoleAuthor {
		if task.Provider != primary || (execution.Sequence == 1 && execution.PreviousAuthorTaskID != "") || (execution.Sequence > 1 && execution.PreviousAuthorTaskID == "") {
			return nil, runnerExecutionConflict("author provider or succession is invalid")
		}
	} else if task.Provider != "terminal" || execution.PreviousAuthorTaskID == "" {
		return nil, runnerExecutionConflict("verification provider or author reference is invalid")
	}
	if err := domain.ValidateRunnerGovernedContextForRun(run, task); err != nil {
		return nil, runnerExecutionConflict(err.Error())
	}
	invocation := task.Invocation
	if invocation == nil || invocation.RunID != run.ID || invocation.NodeID != task.NodeID || invocation.WorkspaceIsolation == nil || invocation.WorkspaceIsolation.Version != domain.RunnerWorkspaceIsolationVersion || !domain.ValidWorkspaceContextSHA256(invocation.WorkspaceIsolation.ContextSHA256) || invocation.ResumeSessionID != "" {
		return nil, runnerExecutionConflict("invocation lacks exact isolated execution identity")
	}
	if err := validateRunnerExecutionRepositories(run, *invocation); err != nil {
		return nil, err
	}
	contextSHA, err := domain.RunnerContributionContextSHA(task)
	if err != nil {
		return nil, err
	}
	if contextSHA != invocation.WorkspaceIsolation.ContextSHA256 {
		return nil, runnerExecutionConflict("assignment digest differs from frozen invocation")
	}
	return node, nil
}

func validateRunnerExecutionRepositories(run domain.Run, invocation domain.RunnerInvocation) error {
	repositories := invocation.Repositories
	if len(repositories) == 0 && invocation.Repository != nil {
		repositories = []domain.RunnerRepository{*invocation.Repository}
	}
	expected := run.RepositoryContexts()
	if len(repositories) != len(expected) {
		return runnerExecutionConflict("invocation repositories differ from Run")
	}
	seen := make(map[string]bool, len(repositories))
	for _, repository := range repositories {
		if repository.RepositoryID == "" || seen[repository.RepositoryID] || !domain.ValidWorkspaceBaseSHA(repository.BaseSHA) || run.ExecutionContext.WorkspaceBaseSHAs[repository.RepositoryID] != repository.BaseSHA {
			return runnerExecutionConflict("invocation repository base differs from frozen Run")
		}
		seen[repository.RepositoryID] = true
		matches := 0
		for _, frozen := range expected {
			if frozen.RepositoryID == repository.RepositoryID && frozen.CloneURL == repository.CloneURL && frozen.WorkspaceDir == repository.Dir {
				matches++
			}
		}
		if matches != 1 {
			return runnerExecutionConflict("invocation repository is outside frozen Run")
		}
	}
	return nil
}

func equalRunnerExecutionDeadline(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}
func decodeRunnerExecutionObject(raw []byte, value any) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("expected JSON object")
	}
	return json.Unmarshal(raw, value)
}
func runnerExecutionConflict(reason string) error {
	return fmt.Errorf("%w: %s", ErrRunnerTaskLeaseLost, reason)
}
