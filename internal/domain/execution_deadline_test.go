package domain

import (
	"testing"
	"time"
)

func TestExecutionDeadlineContract(t *testing.T) {
	for _, limit := range []int{-1, 0, 1, 86400, 86401} {
		err := ValidateWorkflowExecutionLimits(WorkflowDefinition{Nodes: []WorkflowNode{{ID: "work", MaxDurationSec: limit}}})
		if (err != nil) != (limit < 0 || limit > 86400) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	now := time.Now().UTC()
	if ExecutionDeadlineExpired(nil, now) || ExecutionDeadlineExpired(&now, now.Add(-time.Nanosecond)) || !ExecutionDeadlineExpired(&now, now) {
		t.Fatal("deadline must be optional and exclusive at the exact boundary")
	}
}
