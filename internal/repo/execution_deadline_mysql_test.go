package repo

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestMySQLRunnerAbsoluteDeadlineAfterRowLockWait(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_MYSQL_DSN is not set")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, operation := range []string{"start", "renew", "events", "empty-events", "stage", "complete", "claim"} {
		t.Run(operation, func(t *testing.T) {
			now := time.Now().UTC()
			task := contributionTask(t, s, fmt.Sprintf("deadline-wait-%s-%d", operation, now.UnixNano()), "runner", 1, now)
			receipt := contributionBytes(t, s, task, 0, "deadline bundle")
			staged, err := s.StageRunnerContribution(receipt, "runner", now)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().UTC().Add(100 * time.Millisecond)
			task, _ = s.RunnerTask(task.ID)
			task.ExecutionDeadline = &deadline
			if operation == "claim" {
				task.Status, task.LeaseOwner, task.LeaseUntil, task.Attempt = domain.RunnerTaskQueued, "", nil, 0
			}
			// Deliberately keep the ordinary lease valid. The persisted execution
			// deadline must be an independent fence, including legacy long leases.
			values := runnerTaskRowValues(task)
			if operation != "claim" {
				values["lease_until"] = now.Add(time.Minute)
			}
			if err := s.DB.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Updates(values).Error; err != nil {
				t.Fatal(err)
			}
			control, found := s.RunnerTaskControl(task.ID)
			if !found || control.ExecutionDeadline == nil || !control.ExecutionDeadline.Equal(deadline) {
				t.Fatalf("MySQL lean control read lost deadline: %+v", control)
			}
			tx := s.DB.Begin()
			defer tx.Rollback()
			var row runnerTaskRow
			if err := s.locking(tx).Where("id = ?", task.ID).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var mutationErr error
				switch operation {
				case "start":
					mutationErr = s.StartRunnerTask(task.ID, "runner", task.Attempt, now)
				case "renew":
					_, mutationErr = s.RenewRunnerTaskLease(task.ID, "runner", task.Attempt, time.Hour, now)
				case "events":
					_, mutationErr = s.AppendRunnerTaskEvents(task.ID, "runner", task.Attempt, []domain.RunnerTaskEvent{{Seq: 1, Data: "late"}}, now)
				case "empty-events":
					_, mutationErr = s.AppendRunnerTaskEvents(task.ID, "runner", task.Attempt, nil, now)
				case "stage":
					_, mutationErr = s.StageRunnerContribution(receipt, "runner", now)
				case "complete":
					mutationErr = s.CompleteRunnerTask(task.ID, "runner", task.Attempt, domain.RunnerTaskSuccess, nil, "late", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{staged.ID}}, now)
				case "claim":
					_, found, err := s.ClaimRunnerTask("runner", []string{task.Provider}, now, time.Minute)
					mutationErr = err
					if found {
						mutationErr = fmt.Errorf("expired task was claimed")
					}
				}
				done <- mutationErr
			}()
			time.Sleep(180 * time.Millisecond)
			if err := tx.Commit().Error; err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if operation == "claim" && err != nil || operation != "claim" && !errors.Is(err, ErrRunnerTaskLeaseLost) {
					t.Fatalf("postdeadline %s: %v", operation, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("deadline row-lock waiter did not finish")
			}
			stored, _ := s.RunnerTask(task.ID)
			if stored.ExecutionDeadline == nil || !stored.ExecutionDeadline.Equal(deadline) || len(stored.AcceptedContributions) != 0 || stored.Result != "" {
				t.Fatalf("deadline/receipt changed: %+v", stored)
			}
			if operation == "claim" && (stored.Status != domain.RunnerTaskFailed || stored.Attempt != 0 || stored.Error != domain.ErrExecutionDeadlineExceeded.Error()) {
				t.Fatalf("expired claim not durably failed: %+v", stored)
			}
		})
	}
}
