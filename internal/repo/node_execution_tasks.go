package repo

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// QueueNodeExecutionTask is the only creation path for placed execution. The
// Run lock serializes author successors; all task locks precede the registry
// lock. A failed insert must roll back the author pointer as well.
func (r *RunnersRepo) QueueNodeExecutionTask(task domain.RunnerTask) error {
	prepared, err := prepareRunnerTask(task)
	if err != nil {
		return err
	}
	if r.runs == nil || prepared.ExecutionPlacement == nil || prepared.Execution == nil || prepared.MaintenanceKey != nil || prepared.Action != "" || prepared.Status != domain.RunnerTaskQueued || prepared.Attempt != 0 || prepared.LeaseOwner != "" || prepared.LeaseUntil != nil || prepared.DesiredState != "" || prepared.StartedAt != nil || prepared.CompletedAt != nil || prepared.Outputs != nil {
		return fmt.Errorf("%w: placed execution requires a new queued task", domain.ErrRunnerPlacementInvalid)
	}
	var options []*sql.TxOptions
	if r.isMySQL() {
		// As with maintenance creation, independent Runs must not retain
		// repeatable-read gaps for their absent task IDs before inserting.
		options = append(options, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		run, err := r.runs.lockRunnerExecutionRunTx(tx, prepared.RunID, prepared.UpdatedAt)
		if err != nil {
			return err
		}
		if err := r.runs.validateRunnerExecutionEnqueueState(run, prepared); err != nil {
			return err
		}
		locked, err := r.lockNodeExecutionTasksTx(tx, prepared)
		if err != nil {
			return err
		}
		if _, exists := locked[prepared.ID]; exists {
			return ErrRunnerTaskLeaseLost
		}
		if err := r.validateNodeContributionInputsTx(tx, run, prepared, locked); err != nil {
			return err
		}
		if err := r.validateRunnerPlacementTx(tx, *prepared.ExecutionPlacement, "", prepared.UpdatedAt); err != nil {
			return err
		}
		checkedAt := leaseCheckTime(tx, prepared.UpdatedAt)
		if err := r.runs.validateRunnerExecutionRunAt(run, checkedAt); err != nil {
			return err
		}
		if domain.ExecutionDeadlineExpired(prepared.ExecutionDeadline, checkedAt) {
			return ErrRunnerTaskLeaseLost
		}
		if err := r.runs.advanceRunnerExecutionStateTx(tx, run, prepared); err != nil {
			return err
		}
		inserted, err := insertIgnore(tx, "runner_tasks", runnerTaskRowValues(prepared))
		if err != nil {
			return err
		}
		if !inserted {
			return ErrRunnerTaskLeaseLost
		}
		return nil
	}, options...)
}

const nodeExecutionMaxLineageTasks = 1024

func nodeExecutionDependencyIDs(task domain.RunnerTask) []string {
	ids := map[string]bool{}
	if task.Execution != nil && task.Execution.PreviousAuthorTaskID != "" {
		ids[task.Execution.PreviousAuthorTaskID] = true
	}
	if task.Invocation != nil {
		for _, input := range task.Invocation.ContributionInputs {
			for _, source := range input.Sources {
				ids[source.TaskID] = true
			}
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Discover the immutable dependency closure without taking task locks. Then
// lock the complete set in lexical order and recheck every dependency edge.
// All tasks must belong to the already locked Run; cross-Run sources cannot
// introduce a lock-order cycle. Lifecycle/receipts are read only after locks.
func (r *RunnersRepo) lockNodeExecutionTasksTx(tx *gorm.DB, task domain.RunnerTask) (map[string]domain.RunnerTask, error) {
	peeked := map[string]domain.RunnerTask{}
	pending := []string{task.ID}
	for _, id := range nodeExecutionDependencyIDs(task) {
		pending = append(pending, id)
	}
	for len(pending) > 0 {
		id := pending[0]
		pending = pending[1:]
		if _, seen := peeked[id]; seen {
			continue
		}
		if id == "" || len(peeked) >= nodeExecutionMaxLineageTasks {
			return nil, invalidContribution("missing or excessive execution lineage")
		}
		var row runnerTaskRow
		err := tx.Where("id = ?", id).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) && id == task.ID {
			// A new task is inserted only after the author CAS. The Run lock
			// serializes same-Run creators; insertIgnore fences global ID reuse.
			peeked[id] = task
			continue
		}
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrRunnerTaskLeaseLost
			}
			return nil, err
		}
		current := runnerTaskFromRow(row)
		if current.RunID != task.RunID {
			return nil, invalidContribution("execution sources must belong to the same run")
		}
		// Only accepted terminal authors can be input dependencies. Reject
		// live sources before taking any task locks: expiry recovery may hold
		// several live tasks, whereas this closure must have at most one.
		// Revalidate lifecycle again from the locked rows below.
		if id != task.ID && (current.Status != domain.RunnerTaskSuccess || current.Execution == nil || current.Execution.Role != domain.NodeTaskRoleAuthor) {
			return nil, ErrRunnerTaskLeaseLost
		}
		peeked[id] = current
		pending = append(pending, nodeExecutionDependencyIDs(current)...)
	}
	ids := make([]string, 0, len(peeked))
	for id := range peeked {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	locked := make(map[string]domain.RunnerTask, len(ids))
	for _, id := range ids {
		var row runnerTaskRow
		err := r.locking(tx).Where("id = ?", id).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) && id == task.ID {
			continue
		}
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrRunnerTaskLeaseLost
			}
			return nil, err
		}
		current := runnerTaskFromRow(row)
		if current.RunID != task.RunID || !reflect.DeepEqual(nodeExecutionDependencyIDs(current), nodeExecutionDependencyIDs(peeked[id])) {
			return nil, ErrRunnerTaskLeaseLost
		}
		locked[id] = current
	}
	if current, exists := locked[task.ID]; exists {
		before, err := domain.RunnerContributionContextSHA(task)
		if err != nil {
			return nil, err
		}
		after, err := domain.RunnerContributionContextSHA(current)
		if err != nil || before != after {
			return nil, ErrRunnerTaskLeaseLost
		}
	}
	return locked, nil
}

// validateNodeContributionInputsTx checks only controller-frozen provenance;
// Git bundle contents are independently verified during preparation/import.
// Same-node prior authors are historical chain interiors, so they deliberately
// do not have to remain the Node's latest author. Every chain still binds the
// current Node attempt, and every external predecessor root must remain latest.
func (r *RunnersRepo) validateNodeContributionInputsTx(tx *gorm.DB, run domain.Run, task domain.RunnerTask, locked map[string]domain.RunnerTask) error {
	visiting, verified := map[string]bool{}, map[string]bool{}
	var validate func(domain.RunnerTask) error
	validate = func(consumer domain.RunnerTask) error {
		if visiting[consumer.ID] {
			return invalidContribution("cyclic execution lineage")
		}
		if verified[consumer.ID] {
			return nil
		}
		visiting[consumer.ID] = true
		if err := validateNodeContributionAssignment(run, consumer); err != nil {
			return err
		}
		if previousID := consumer.Execution.PreviousAuthorTaskID; previousID != "" {
			previous, ok := locked[previousID]
			expectedSequence := consumer.Execution.Sequence - 1
			if consumer.Execution.Role == domain.NodeTaskRoleVerification {
				expectedSequence = consumer.Execution.Sequence
			}
			if !ok || previous.RunID != consumer.RunID || previous.NodeID != consumer.NodeID || previous.Execution == nil || previous.Execution.Role != domain.NodeTaskRoleAuthor || previous.Execution.Sequence != expectedSequence || previous.Execution.NodeAttempt != consumer.Execution.NodeAttempt || previous.Status != domain.RunnerTaskSuccess || previous.DesiredState == domain.RunnerTaskDesiredCancel || !sameNodePlacement(previous.ExecutionPlacement, consumer.ExecutionPlacement) {
				return ErrRunnerTaskLeaseLost
			}
			if err := validate(previous); err != nil {
				return err
			}
		}
		inputs := consumer.Invocation.ContributionInputs
		requiredSources, err := nodeExecutionRequiredSources(run, consumer)
		if err != nil {
			return err
		}
		if len(requiredSources) > 0 && len(inputs) != len(run.RepositoryContexts()) {
			return invalidContribution("predecessor code is required for every frozen repository")
		}
		seenRepositories := map[string]bool{}
		for _, input := range inputs {
			previousSeen := consumer.Execution.PreviousAuthorTaskID == ""
			if seenRepositories[input.RepositoryID] || input.BaseSHA != run.ExecutionContext.WorkspaceBaseSHAs[input.RepositoryID] || !domain.ValidWorkspaceBaseSHA(input.BaseSHA) || !domain.ValidWorkspaceBaseSHA(input.HeadSHA) || !domain.ValidWorkspaceContextSHA256(input.BundleSHA256) || input.BundleSize <= 0 || input.BundleSize > domain.RunnerContributionMaxBytes || len(input.Sources) == 0 || len(input.Sources) > 32 {
				return invalidContribution("invalid or duplicate frozen contribution input")
			}
			seenRepositories[input.RepositoryID] = true
			if _, err := contributionSpec(consumer, input.RepositoryID); err != nil {
				return err
			}
			if err := contributionBlobExists(tx, domain.RunnerContributionReceipt{SHA256: input.BundleSHA256, Size: input.BundleSize}); err != nil {
				return err
			}
			seenSources := map[string]bool{}
			for _, source := range input.Sources {
				parent, ok := locked[source.TaskID]
				if !ok || seenSources[source.TaskID] || source.RunID != run.ID || parent.RunID != run.ID || source.NodeID != parent.NodeID || source.NodeAttempt < 1 || parent.Execution == nil || parent.Execution.Role != domain.NodeTaskRoleAuthor || parent.Execution.NodeAttempt != source.NodeAttempt || parent.Status != domain.RunnerTaskSuccess || parent.DesiredState == domain.RunnerTaskDesiredCancel || source.TaskAttempt != parent.Attempt || parent.Attempt < 1 {
					return ErrRunnerTaskLeaseLost
				}
				seenSources[source.TaskID] = true
				node := run.Nodes[parent.NodeID]
				if node == nil || node.Attempt != source.NodeAttempt || !sameNodePlacement(node.ExecutionPlacement, parent.ExecutionPlacement) {
					return ErrRunnerTaskLeaseLost
				}
				if parent.NodeID == consumer.NodeID {
					expectedSequence := consumer.Execution.Sequence - 1
					if consumer.Execution.Role == domain.NodeTaskRoleVerification {
						expectedSequence = consumer.Execution.Sequence
					}
					if source.TaskID != consumer.Execution.PreviousAuthorTaskID || parent.Execution.Sequence != expectedSequence {
						return ErrRunnerTaskLeaseLost
					}
					previousSeen = true
					if consumer.Execution.Role == domain.NodeTaskRoleVerification && (len(input.Sources) != 1 || input.HeadSHA != source.ResultSHA) {
						return invalidContribution("verification must read the exact current author result")
					}
				} else if node.Status != "succeeded" || node.ExecutionTaskID != parent.ID || node.ExecutionSequence != parent.Execution.Sequence || !nodeExecutionPredecessor(run.WorkflowSnapshot, parent.NodeID, consumer.NodeID) {
					return ErrRunnerTaskLeaseLost
				}
				contextSHA, err := domain.RunnerContributionContextSHA(parent)
				if err != nil || source.ContextSHA256 != contextSHA || parent.Invocation == nil || parent.Invocation.WorkspaceIsolation == nil || parent.Invocation.WorkspaceIsolation.ContextSHA256 != contextSHA {
					return invalidContribution("source assignment context differs")
				}
				count := 0
				for _, receipt := range parent.AcceptedContributions {
					if receipt.RepositoryID != input.RepositoryID {
						continue
					}
					count++
					spec, err := contributionSpec(parent, input.RepositoryID)
					if err != nil {
						return err
					}
					if err := validateContributionReceipt(receipt, spec); err != nil {
						return err
					}
					if receipt.ID != source.ReceiptID || receipt.TaskID != parent.ID || receipt.Attempt != parent.Attempt || receipt.AcceptedAt.IsZero() || receipt.BaseSHA != input.BaseSHA || receipt.ContextSHA256 != source.ContextSHA256 || receipt.SHA256 != source.BundleSHA256 || receipt.ResultSHA != source.ResultSHA {
						return invalidContribution("source receipt differs from accepted contribution")
					}
					if err := contributionBlobExists(tx, receipt); err != nil {
						return err
					}
				}
				if count != 1 {
					return invalidContribution("source requires one accepted repository receipt")
				}
				if err := validate(parent); err != nil {
					return err
				}
			}
			if !previousSeen {
				return invalidContribution("successor input omits the previous author repository")
			}
			for _, id := range requiredSources {
				if !seenSources[id] {
					return invalidContribution("input omits an accepted predecessor")
				}
			}
		}
		if consumer.Execution.PreviousAuthorTaskID != "" && len(inputs) != len(run.RepositoryContexts()) {
			return invalidContribution("successor must preserve all prior author repositories")
		}
		visiting[consumer.ID], verified[consumer.ID] = false, true
		return nil
	}
	return validate(task)
}

func validateNodeContributionAssignment(run domain.Run, task domain.RunnerTask) error {
	if task.RunID != run.ID || task.Invocation == nil || task.Invocation.RunID != run.ID || task.Invocation.NodeID != task.NodeID || task.Invocation.WorkspaceIsolation == nil || task.Invocation.WorkspaceIsolation.Version != domain.RunnerWorkspaceIsolationVersion || task.Execution == nil || task.ExecutionPlacement == nil || task.Action != "" || task.MaintenanceKey != nil || !domain.ControlledDelivery(task.DeliveryPolicy) {
		return invalidContribution("input requires a frozen controlled node assignment")
	}
	contextSHA, err := domain.RunnerContributionContextSHA(task)
	if err != nil || task.Invocation.WorkspaceIsolation.ContextSHA256 != contextSHA {
		return invalidContribution("execution assignment differs from its frozen context")
	}
	repositories := task.Invocation.Repositories
	if len(repositories) == 0 && task.Invocation.Repository != nil {
		repositories = []domain.RunnerRepository{*task.Invocation.Repository}
	}
	if len(repositories) != len(run.RepositoryContexts()) || len(task.ContributionSpecs) != len(repositories) {
		return invalidContribution("execution must export every frozen repository")
	}
	seen := map[string]bool{}
	for _, repository := range repositories {
		if seen[repository.RepositoryID] {
			return invalidContribution("duplicate execution repository")
		}
		seen[repository.RepositoryID] = true
		matches := 0
		for _, frozen := range run.RepositoryContexts() {
			if frozen.RepositoryID == repository.RepositoryID && frozen.CloneURL == repository.CloneURL && frozen.WorkspaceDir == repository.Dir && run.ExecutionContext.WorkspaceBaseSHAs[frozen.RepositoryID] == repository.BaseSHA {
				matches++
			}
		}
		if matches != 1 {
			return invalidContribution("execution repository differs from run baseline")
		}
		spec, err := contributionSpec(task, repository.RepositoryID)
		if err != nil {
			return err
		}
		if spec.ContextSHA256 != contextSHA {
			return invalidContribution("export spec differs from assignment context")
		}
	}
	return nil
}

func nodeExecutionPredecessor(workflow domain.WorkflowDefinition, source, consumer string) bool {
	seen := map[string]bool{consumer: true}
	pending := []string{consumer}
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		for _, edge := range workflow.Edges {
			if edge.Target != current || (edge.When != "" && edge.When != "success") {
				continue
			}
			if edge.Source == source {
				return true
			}
			if !seen[edge.Source] {
				seen[edge.Source] = true
				pending = append(pending, edge.Source)
			}
		}
	}
	return false
}

// Approval nodes carry no code. Walk through them to the nearest executable
// predecessors; a later same-node author already carries that input through
// its immutable prior-author chain instead of rediscovering a different head.
func nodeExecutionRequiredSources(run domain.Run, task domain.RunnerTask) ([]string, error) {
	if task.Execution.PreviousAuthorTaskID != "" || len(run.RepositoryContexts()) == 0 {
		return nil, nil
	}
	definitions := map[string]domain.WorkflowNode{}
	for _, node := range run.WorkflowSnapshot.Nodes {
		definitions[node.ID] = node
	}
	seen := map[string]bool{task.NodeID: true}
	ids := map[string]bool{}
	pending := []string{task.NodeID}
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		for _, edge := range run.WorkflowSnapshot.Edges {
			if edge.Target != current || (edge.When != "" && edge.When != "success") || seen[edge.Source] {
				continue
			}
			seen[edge.Source] = true
			definition, exists := definitions[edge.Source]
			if !exists {
				return nil, invalidContribution("workflow predecessor is missing")
			}
			if definition.Type == "approval" {
				pending = append(pending, edge.Source)
				continue
			}
			node := run.Nodes[edge.Source]
			if node == nil || node.Status != "succeeded" || node.ExecutionTaskID == "" || node.ExecutionSequence < 1 {
				return nil, ErrRunnerTaskLeaseLost
			}
			ids[node.ExecutionTaskID] = true
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// sameNodePlacement compares canonical snapshots, independent of JSON field
// order or input capability order, without changing a persisted snapshot.
func sameNodePlacement(a, b *domain.NodeExecutionPlacement) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ca, ea := domain.CanonicalNodeExecutionPlacement(*a)
	cb, eb := domain.CanonicalNodeExecutionPlacement(*b)
	if ea != nil || eb != nil || a.CapabilitySHA256 == "" || b.CapabilitySHA256 == "" {
		return false
	}
	left, _ := json.Marshal(ca)
	right, _ := json.Marshal(cb)
	return string(left) == string(right)
}
