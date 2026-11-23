package repo

import (
	"errors"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestRunnerExecutionDeadlinePersistsCapsLeaseAndNeverRequeues(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	deadline := now.Add(time.Second)
	task := domain.RunnerTask{ID: "deadline", Provider: "terminal", MaxAttempts: 8, ExecutionDeadline: &deadline}
	if err := s.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := s.ClaimRunnerTask("runner", []string{"terminal"}, now, time.Minute)
	if err != nil || !found || claimed.ExecutionDeadline == nil || !claimed.ExecutionDeadline.Equal(deadline) || !claimed.LeaseUntil.Equal(deadline) {
		t.Fatalf("claim=%+v found=%v err=%v", claimed, found, err)
	}
	if _, err := s.RenewRunnerTaskLease(task.ID, "runner", claimed.Attempt, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.RunnerTask(task.ID)
	if !stored.LeaseUntil.Equal(deadline) {
		t.Fatal("lease renewal extended execution deadline")
	}
	if err := s.CompleteRunnerTask(task.ID, "runner", claimed.Attempt, domain.RunnerTaskSuccess, nil, "late", "", nil, nil, deadline); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatalf("deadline accepted success: %v", err)
	}
	if _, err := s.ExpireRunnerTasks(deadline); err != nil {
		t.Fatal(err)
	}
	stored, _ = s.RunnerTask(task.ID)
	if stored.Status != domain.RunnerTaskFailed || stored.Error != domain.ErrExecutionDeadlineExceeded.Error() || stored.Attempt != claimed.Attempt {
		t.Fatalf("expired task reauthorized: %+v", stored)
	}
	if _, found, err := s.ClaimRunnerTask("runner", []string{"terminal"}, deadline.Add(time.Second), time.Minute); err != nil || found {
		t.Fatalf("reclaimed after absolute deadline: %v %v", found, err)
	}
}

func TestQueuedTaskPastDeadlineNeverClaims(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	deadline := now.Add(-time.Second)
	if err := s.PutRunnerTask(domain.RunnerTask{ID: "expired-queued", Provider: "terminal", ExecutionDeadline: &deadline}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ClaimRunnerTask("runner", []string{"terminal"}, now, time.Minute); err != nil || found {
		t.Fatalf("expired queue claimed: %v %v", found, err)
	}
	task, _ := s.RunnerTask("expired-queued")
	if task.Status != domain.RunnerTaskFailed || task.Attempt != 0 || task.Error != domain.ErrExecutionDeadlineExceeded.Error() {
		t.Fatalf("queued expiration not durable: %+v", task)
	}
	if err := s.PutWorkflow(domain.WorkflowDefinition{ID: "invalid-limit", Nodes: []domain.WorkflowNode{{ID: "work", MaxDurationSec: 86401}}}); err == nil {
		t.Fatal("catalog accepted invalid limit")
	}
}
