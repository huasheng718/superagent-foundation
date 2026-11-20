package repo

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestMySQLRunnerContributionAcceptanceRaces(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_MYSQL_DSN is not set")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	suffix := time.Now().UnixNano()
	t.Run("all-lease-mutations-recheck-after-row-lock-wait", func(t *testing.T) {
		for _, operation := range []string{"start", "renew", "events", "empty-events", "complete"} {
			t.Run(operation, func(t *testing.T) {
				now := time.Now().UTC()
				task := contributionTask(t, s, fmt.Sprintf("mysql-lease-wait-%d-%s", suffix, operation), "contribution-runner", 0, now)
				until := time.Now().UTC().Add(100 * time.Millisecond)
				if err := s.DB.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Update("lease_until", until).Error; err != nil {
					t.Fatal(err)
				}
				tx := s.DB.Begin()
				defer tx.Rollback()
				var row runnerTaskRow
				if err := s.locking(tx).Where("id = ?", task.ID).Take(&row).Error; err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					var err error
					switch operation {
					case "start":
						err = s.StartRunnerTask(task.ID, "contribution-runner", task.Attempt, now)
					case "renew":
						_, err = s.RenewRunnerTaskLease(task.ID, "contribution-runner", task.Attempt, time.Minute, now)
					case "events":
						_, err = s.AppendRunnerTaskEvents(task.ID, "contribution-runner", task.Attempt, []domain.RunnerTaskEvent{{Seq: 1, Data: "stale"}}, now)
					case "empty-events":
						_, err = s.AppendRunnerTaskEvents(task.ID, "contribution-runner", task.Attempt, nil, now)
					case "complete":
						err = s.CompleteRunnerTask(task.ID, "contribution-runner", task.Attempt, domain.RunnerTaskSuccess, nil, "stale", "", nil, nil, now)
					}
					done <- err
				}()
				time.Sleep(180 * time.Millisecond)
				if err := tx.Commit().Error; err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if !errors.Is(err, ErrRunnerTaskLeaseLost) {
						t.Fatalf("%s retained expired authority after row-lock wait: %v", operation, err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("row-lock waiter did not return")
				}
				current, _ := s.RunnerTask(task.ID)
				if current.Status != domain.RunnerTaskLeased || current.Result != "" || !current.LeaseUntil.Equal(*row.LeaseUntil) {
					t.Fatalf("expired %s mutated task: %+v", operation, current)
				}
				if events := s.RunnerTaskEventsAfter(task.ID, task.Attempt, 0); len(events) != 0 {
					t.Fatalf("expired events persisted: %+v", events)
				}
			})
		}
	})
	t.Run("staging-replay-versus-complete", func(t *testing.T) {
		for i := 0; i < 8; i++ {
			now := time.Now().UTC()
			task := contributionTask(t, s, fmt.Sprintf("mysql-contribution-%d-%d", suffix, i), "contribution-runner", 1, now)
			receipt := contributionBytes(t, s, task, 0, fmt.Sprintf("bundle-race-%d-%d", suffix, i))
			staged, err := s.StageRunnerContribution(receipt, "contribution-runner", now)
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			var stageErr, completeErr error
			go func() {
				defer wg.Done()
				<-start
				_, stageErr = s.StageRunnerContribution(receipt, "contribution-runner", time.Now().UTC())
			}()
			go func() {
				defer wg.Done()
				<-start
				completeErr = s.CompleteRunnerTask(task.ID, "contribution-runner", task.Attempt, domain.RunnerTaskSuccess, nil, "ok", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{staged.ID}}, time.Now().UTC())
			}()
			close(start)
			wg.Wait()
			if completeErr != nil || (stageErr != nil && !errors.Is(stageErr, ErrRunnerTaskLeaseLost)) {
				t.Fatalf("stage=%v complete=%v", stageErr, completeErr)
			}
			accepted, _ := s.RunnerTask(task.ID)
			if accepted.Status != domain.RunnerTaskSuccess || len(accepted.AcceptedContributions) != 1 || accepted.AcceptedContributions[0].ID != staged.ID {
				t.Fatalf("acceptance lost atomicity: %+v", accepted)
			}
		}
	})
	t.Run("cancel-versus-complete", func(t *testing.T) {
		for i := 0; i < 8; i++ {
			now := time.Now().UTC()
			task := contributionTask(t, s, fmt.Sprintf("mysql-cancel-%d-%d", suffix, i), "contribution-runner", 1, now)
			receipt := contributionBytes(t, s, task, 0, fmt.Sprintf("cancel-bundle-%d-%d", suffix, i))
			staged, err := s.StageRunnerContribution(receipt, "contribution-runner", now)
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			var cancelErr, completeErr error
			go func() { defer wg.Done(); <-start; cancelErr = s.CancelRunnerTask(task.ID, time.Now().UTC()) }()
			go func() {
				defer wg.Done()
				<-start
				completeErr = s.CompleteRunnerTask(task.ID, "contribution-runner", task.Attempt, domain.RunnerTaskSuccess, nil, "ok", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{staged.ID}}, time.Now().UTC())
			}()
			close(start)
			wg.Wait()
			if cancelErr != nil || (completeErr != nil && !errors.Is(completeErr, ErrRunnerTaskLeaseLost)) {
				t.Fatalf("cancel=%v complete=%v", cancelErr, completeErr)
			}
			current, _ := s.RunnerTask(task.ID)
			if completeErr == nil {
				if current.Status != domain.RunnerTaskSuccess || len(current.AcceptedContributions) != 1 || current.DesiredState == domain.RunnerTaskDesiredCancel {
					t.Fatalf("completed first but inconsistent: %+v", current)
				}
			} else if len(current.AcceptedContributions) != 0 || current.DesiredState != domain.RunnerTaskDesiredCancel || current.Status == domain.RunnerTaskSuccess {
				t.Fatalf("cancellation accepted contribution: %+v", current)
			}
		}
	})
	t.Run("lease-expires-while-waiting-for-row-lock", func(t *testing.T) {
		for _, completion := range []bool{false, true} {
			t.Run(fmt.Sprint(completion), func(t *testing.T) {
				now := time.Now().UTC()
				task := contributionTask(t, s, fmt.Sprintf("mysql-lock-expiry-%d-%v", suffix, completion), "contribution-runner", 1, now)
				receipt := contributionBytes(t, s, task, 0, fmt.Sprintf("lock-bundle-%d-%v", suffix, completion))
				staged, err := s.StageRunnerContribution(receipt, "contribution-runner", now)
				if err != nil {
					t.Fatal(err)
				}
				until := time.Now().UTC().Add(100 * time.Millisecond)
				if err := s.DB.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Update("lease_until", until).Error; err != nil {
					t.Fatal(err)
				}
				tx := s.DB.Begin()
				defer tx.Rollback()
				var row runnerTaskRow
				if err := s.locking(tx).Where("id = ?", task.ID).Take(&row).Error; err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					if completion {
						done <- s.CompleteRunnerTask(task.ID, "contribution-runner", task.Attempt, domain.RunnerTaskSuccess, nil, "", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{staged.ID}}, now)
						return
					}
					_, err := s.StageRunnerContribution(receipt, "contribution-runner", now)
					done <- err
				}()
				time.Sleep(180 * time.Millisecond)
				if err := tx.Commit().Error; err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if !errors.Is(err, ErrRunnerTaskLeaseLost) {
						t.Fatalf("expired authority after row-lock wait: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("row lock waiter did not return")
				}
				current, _ := s.RunnerTask(task.ID)
				if len(current.AcceptedContributions) != 0 || current.Status == domain.RunnerTaskSuccess {
					t.Fatalf("expired waiter accepted: %+v", current)
				}
			})
		}
	})
	t.Run("restart-read-accepted", func(t *testing.T) {
		now := time.Now().UTC()
		task := contributionTask(t, s, fmt.Sprintf("mysql-restart-%d", suffix), "contribution-runner", 1, now)
		receipt := contributionBytes(t, s, task, 0, fmt.Sprintf("restart-bundle-%d", suffix))
		staged, err := s.StageRunnerContribution(receipt, "contribution-runner", now)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CompleteRunnerTask(task.ID, "contribution-runner", task.Attempt, domain.RunnerTaskSuccess, nil, "ok", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{staged.ID}}, now); err != nil {
			t.Fatal(err)
		}
		s.Close()
		s, err = Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		stored, found := s.RunnerTask(task.ID)
		if !found || stored.Status != domain.RunnerTaskSuccess || len(stored.AcceptedContributions) != 1 || stored.AcceptedContributions[0].ID != staged.ID || stored.AcceptedContributions[0].ContextSHA256 != receipt.ContextSHA256 || stored.AcceptedContributions[0].StorageKey != receipt.SHA256 {
			t.Fatalf("restart lost immutable acceptance: %+v", stored)
		}
		content, size, found := s.RunnerContributionBlob(receipt.SHA256)
		if !found || size != receipt.Size || string(content) != fmt.Sprintf("restart-bundle-%d", suffix) {
			t.Fatal("restart lost contribution bytes")
		}
	})

}
