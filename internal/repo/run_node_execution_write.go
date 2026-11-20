package repo

import (
	"encoding/json"
	"fmt"
	"reflect"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// Placed author pointers are written by QueueNodeExecutionTask, not by a
// worker's previously loaded Run. Keep those fields when ordinary observations
// save the same attempt, and never roll an attempt backwards. The caller holds
// the Run row before any Node locks, just like the execution queue.
func (r *RunsRepo) preserveNodeExecutionAuthorityTx(tx *gorm.DB, run *domain.Run, current runRow, exists bool) error {
	var previous domain.TelemetryContext
	if exists {
		if err := json.Unmarshal(current.ExecutionContext, &previous); err != nil {
			if len(run.ExecutionContext.NodePlacements) != 0 {
				return fmt.Errorf("%w: stored placement context is invalid", domain.ErrRunnerPlacementInvalid)
			}
			return nil
		}
	}
	if len(previous.NodePlacements) == 0 && len(run.ExecutionContext.NodePlacements) == 0 {
		return nil
	}
	if exists {
		before, err := canonicalStoredNodePlacementRequests(previous.NodePlacements)
		if err != nil {
			return err
		}
		after, err := canonicalStoredNodePlacementRequests(run.ExecutionContext.NodePlacements)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			return fmt.Errorf("%w: node placement requests are frozen at run creation", domain.ErrRunnerPlacementChanged)
		}
	}
	if exists && run.Revision <= current.Revision {
		run.Revision = current.Revision + 1
	}
	var rows []runNodeRow
	if exists {
		if err := r.locking(tx).Where("run_id = ?", run.ID).Order("node_id").Find(&rows).Error; err != nil {
			return err
		}
	}
	old := make(map[string]domain.NodeRun, len(rows))
	for _, row := range rows {
		var node domain.NodeRun
		if err := json.Unmarshal(row.Payload, &node); err != nil || node.NodeID != row.NodeID || node.Attempt != row.Attempt || node.Status != row.Status {
			return fmt.Errorf("%w: stored node execution is invalid", domain.ErrRunnerPlacementInvalid)
		}
		old[row.NodeID] = node
	}
	// Copy incoming Node values before preserving fields; mutating the input
	// map would also mutate a caller's in-memory snapshot on a failed write.
	nodes := make(map[string]*domain.NodeRun, len(run.Nodes))
	for id, node := range run.Nodes {
		if node == nil {
			nodes[id] = nil
			continue
		}
		copy := *node
		prior, found := old[id]
		if found && copy.Attempt < prior.Attempt {
			return ErrRunDecisionStale
		}
		if found && copy.Attempt == prior.Attempt {
			if prior.ExecutionPlacement != nil {
				if copy.ExecutionPlacement != nil && !sameNodePlacement(prior.ExecutionPlacement, copy.ExecutionPlacement) {
					return fmt.Errorf("%w: effective node placement is immutable within an attempt", domain.ErrRunnerPlacementChanged)
				}
				copy.ExecutionPlacement = prior.ExecutionPlacement
			}
			copy.ExecutionSequence, copy.ExecutionTaskID = prior.ExecutionSequence, prior.ExecutionTaskID
		} else {
			copy.ExecutionSequence, copy.ExecutionTaskID = 0, ""
		}
		if copy.ExecutionPlacement != nil {
			if copy.ExecutionPlacement.NodeAttempt != copy.Attempt || copy.ExecutionPlacement.CapabilitySHA256 == "" {
				return fmt.Errorf("%w: effective placement must bind the current attempt", domain.ErrRunnerPlacementInvalid)
			}
			canonical, err := domain.CanonicalNodeExecutionPlacement(*copy.ExecutionPlacement)
			if err != nil {
				return err
			}
			copy.ExecutionPlacement = &canonical
		}
		nodes[id] = &copy
	}
	run.Nodes = nodes
	return nil
}

func canonicalStoredNodePlacementRequests(requests map[string]domain.NodePlacementRequest) (map[string]domain.NodePlacementRequest, error) {
	canonical := make(map[string]domain.NodePlacementRequest, len(requests))
	for id, request := range requests {
		value, err := domain.CanonicalNodePlacementRequest(request)
		if err != nil {
			return nil, err
		}
		canonical[id] = value
	}
	return canonical, nil
}
