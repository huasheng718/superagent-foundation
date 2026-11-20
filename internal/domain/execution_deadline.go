package domain

import (
	"errors"
	"fmt"
	"time"
)

var ErrExecutionDeadlineExceeded = errors.New("execution deadline exceeded")

// Zero preserves legacy unlimited execution. Positive limits are frozen into
// an absolute node-attempt deadline, never restarted by a process takeover.
func ValidateWorkflowExecutionLimits(workflow WorkflowDefinition) error {
	for _, node := range workflow.Nodes {
		if node.MaxDurationSec < 0 || node.MaxDurationSec > 86400 {
			return fmt.Errorf("workflow node %q maxDurationSec must be 0 or 1..86400", node.ID)
		}
	}
	return nil
}

func ExecutionDeadlineExpired(deadline *time.Time, now time.Time) bool {
	return deadline != nil && !now.Before(*deadline)
}
