package repo

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func claimFencingTask(t *testing.T, s *Store, id, runner string, now time.Time) domain.RunnerTask {
	t.Helper()
	if err := s.PutRunnerTask(domain.RunnerTask{ID: id, Provider: "terminal", MaxAttempts: 2}); err != nil {
		t.Fatal(err)
	}
	task, ok, err := s.ClaimRunnerTask(runner, []string{"terminal"}, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	return task
}

func TestPlacedCleanupMaintenanceIsPerRegistrationAndFenced(t *testing.T) {
	s := cvEStore(t)
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	oldSession := fmt.Sprintf("%064d", 1)
	newSession := fmt.Sprintf("%064d", 2)
	runner := domain.Runner{ID: "cleanup-registration", Status: domain.RunnerStatusOnline, SessionTokenHash: oldSession, SessionExpiresAt: &expires, Runtimes: []domain.RunnerRuntime{{Provider: "terminal", Status: domain.RuntimeStatusOnline}}}
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRun(domain.Run{ID: "cleanup-registration-run", Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	oldRegistration, _ := domain.RunnerRegistrationSHA256(oldSession)
	newRegistration, _ := domain.RunnerRegistrationSHA256(newSession)
	for i, registration := range []string{oldRegistration, newRegistration} {
		task := domain.RunnerTask{ID: fmt.Sprintf("cleanup-registration-task-%d", i), RunID: "cleanup-registration-run", RunnerID: runner.ID, Provider: "terminal", Action: domain.RunnerTaskActionCleanupRun, Status: domain.RunnerTaskQueued, CreatedAt: now.Add(time.Duration(i) * time.Second), MaintenanceKey: &domain.RunnerMaintenanceKey{RunID: "cleanup-registration-run", Action: domain.RunnerTaskActionCleanupRun, RunnerID: runner.ID, RegistrationSHA256: registration}}
		if _, created, err := s.QueueRunnerMaintenanceTask(task); err != nil || !created {
			t.Fatalf("queue registration %d created=%v err=%v", i, created, err)
		}
	}
	if got := len(s.RunnerTasksForRun("cleanup-registration-run")); got != 2 {
		t.Fatalf("cleanup task history = %d, want two registration identities", got)
	}
	runner.SessionTokenHash = newSession
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimRunnerTaskForSession(runner.ID, newSession, []string{"terminal"}, now.Add(2*time.Second), time.Minute); err == nil || ok {
		t.Fatalf("new registration took over old cleanup: ok=%v err=%v", ok, err)
	}
	if err := s.CancelRunnerTask("cleanup-registration-task-0", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimRunnerTaskForSession(runner.ID, newSession, []string{"terminal"}, now.Add(4*time.Second), time.Minute)
	if err != nil || !ok || claimed.ID != "cleanup-registration-task-1" {
		t.Fatalf("current registration claim = %+v ok=%v err=%v", claimed, ok, err)
	}
}

func assertLeaseMutationsRejected(t *testing.T, s *Store, id, runner string, attempt int, now time.Time) {
	t.Helper()
	checks := []struct {
		name string
		call func() error
	}{
		{"start", func() error { return s.StartRunnerTask(id, runner, attempt, now) }},
		{"renew", func() error { _, err := s.RenewRunnerTaskLease(id, runner, attempt, time.Minute, now); return err }},
		{"events", func() error {
			_, err := s.AppendRunnerTaskEvents(id, runner, attempt, []domain.RunnerTaskEvent{{Seq: 1, Data: "stale"}}, now)
			return err
		}},
		{"empty-events", func() error { _, err := s.AppendRunnerTaskEvents(id, runner, attempt, nil, now); return err }},
		{"complete", func() error {
			return s.CompleteRunnerTask(id, runner, attempt, domain.RunnerTaskSuccess, nil, "stale", "", nil, nil, now)
		}},
	}
	for _, check := range checks {
		if err := check.call(); !errors.Is(err, ErrRunnerTaskLeaseLost) {
			t.Errorf("%s err=%v, want lease lost", check.name, err)
		}
	}
}

func TestRunnerTaskFenceReclaim(t *testing.T) {
	for _, nextRunner := range []string{"runner-a", "runner-b"} {
		t.Run(nextRunner, func(t *testing.T) {
			s := newTestRunner(t)
			now := time.Now().UTC()
			first := claimFencingTask(t, s, "fenced", "runner-a", now)
			if err := s.StartRunnerTask(first.ID, "runner-a", first.Attempt, now); err != nil {
				t.Fatal(err)
			}
			reclaimAt := now.Add(time.Minute)
			if recovered, err := s.ExpireRunnerTasks(reclaimAt); err != nil || len(recovered) != 1 || recovered[0].Status != domain.RunnerTaskQueued {
				t.Fatalf("expiry=%+v err=%v", recovered, err)
			}
			second, ok, err := s.ClaimRunnerTask(nextRunner, []string{"terminal"}, reclaimAt, time.Minute)
			if err != nil || !ok || second.Attempt != 2 {
				t.Fatalf("second=%+v ok=%v err=%v", second, ok, err)
			}
			assertLeaseMutationsRejected(t, s, first.ID, "runner-a", first.Attempt, reclaimAt)
			current, _ := s.RunnerTask(first.ID)
			if current.Attempt != second.Attempt || current.Status != domain.RunnerTaskLeased || current.LeaseOwner != nextRunner || !current.LeaseUntil.Equal(*second.LeaseUntil) || current.Result != "" {
				t.Fatalf("stale request changed current attempt: %+v", current)
			}
			if err := s.StartRunnerTask(second.ID, nextRunner, second.Attempt, reclaimAt); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RenewRunnerTaskLease(second.ID, nextRunner, second.Attempt, time.Minute, reclaimAt); err != nil {
				t.Fatal(err)
			}
			if err := s.CompleteRunnerTask(second.ID, nextRunner, second.Attempt, domain.RunnerTaskSuccess, nil, "current", "", nil, nil, reclaimAt); err != nil {
				t.Fatal(err)
			}
			assertLeaseMutationsRejected(t, s, second.ID, nextRunner, second.Attempt, reclaimAt)
		})
	}
}

func TestRunnerTaskFenceInvalidIdentityAndExpiryWithoutCleanup(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	task := claimFencingTask(t, s, "fenced", "runner-a", now)
	assertLeaseMutationsRejected(t, s, task.ID, "runner-a", 0, now)
	assertLeaseMutationsRejected(t, s, task.ID, "runner-a", -1, now)
	assertLeaseMutationsRejected(t, s, task.ID, "runner-a", 2, now)
	assertLeaseMutationsRejected(t, s, task.ID, "runner-b", task.Attempt, now)
	assertLeaseMutationsRejected(t, s, "missing", "runner-a", task.Attempt, now)
	assertLeaseMutationsRejected(t, s, "", "runner-a", task.Attempt, now)
	assertLeaseMutationsRejected(t, s, task.ID, "", task.Attempt, now)
	// Exact expiry is already invalid, even before a cleanup/reclaim pass.
	assertLeaseMutationsRejected(t, s, task.ID, "runner-a", task.Attempt, *task.LeaseUntil)
	assertLeaseMutationsRejected(t, s, task.ID, "runner-a", task.Attempt, task.LeaseUntil.Add(time.Second))
	current, _ := s.RunnerTask(task.ID)
	if current.Status != domain.RunnerTaskLeased || current.Result != "" || !current.LeaseUntil.Equal(*task.LeaseUntil) {
		t.Fatalf("expired mutations changed task: %+v", current)
	}
}

func TestRunnerTaskFenceCancellation(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "expiry"}[expire], func(t *testing.T) {
			s := newTestRunner(t)
			now := time.Now().UTC()
			task := claimFencingTask(t, s, "cancel", "runner-a", now)
			if err := s.CancelRunnerTask(task.ID, now); err != nil {
				t.Fatal(err)
			}
			if desired, err := s.RenewRunnerTaskLease(task.ID, "runner-a", task.Attempt, time.Minute, now); err != nil || desired != domain.RunnerTaskDesiredCancel {
				t.Fatalf("desired=%q err=%v", desired, err)
			}
			for _, status := range []string{domain.RunnerTaskSuccess, domain.RunnerTaskFailed} {
				if err := s.CompleteRunnerTask(task.ID, "runner-a", task.Attempt, status, nil, "late", "", nil, nil, now); !errors.Is(err, ErrRunnerTaskLeaseLost) {
					t.Fatalf("status=%s err=%v", status, err)
				}
			}
			if expire {
				if _, err := s.ExpireRunnerTasks(*task.LeaseUntil); err != nil {
					t.Fatal(err)
				}
				if _, ok, err := s.ClaimRunnerTask("runner-a", []string{"terminal"}, task.LeaseUntil.Add(time.Second), time.Minute); err != nil || ok {
					t.Fatalf("canceled task reclaimed: %v %v", ok, err)
				}
			} else if err := s.CompleteRunnerTask(task.ID, "runner-a", task.Attempt, domain.RunnerTaskCanceled, nil, "", "", nil, nil, now); err != nil {
				t.Fatal(err)
			}
			final, _ := s.RunnerTask(task.ID)
			if final.Status != domain.RunnerTaskCanceled || final.CompletedAt == nil || final.LeaseOwner != "" || final.LeaseUntil != nil {
				t.Fatalf("final=%+v", final)
			}
		})
	}
}

func TestRunnerTaskEventsAttemptIsolationAndHistory(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	first := claimFencingTask(t, s, "events", "runner-a", now)
	// Migration retains unknown historical provenance as attempt zero.
	if err := s.DB.Create(&runnerTaskEventRow{TaskID: first.ID, Seq: 1, Data: "legacy"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, batch := range []struct {
		events   []domain.RunnerTaskEvent
		accepted int
	}{
		{[]domain.RunnerTaskEvent{{Seq: 2, Data: "two"}, {Seq: 1, Data: "one"}}, 2},
		{[]domain.RunnerTaskEvent{{Seq: 1, Data: "replay"}, {Seq: 3, Attempt: first.Attempt, Data: "three"}}, 1},
	} {
		if n, err := s.AppendRunnerTaskEvents(first.ID, "runner-a", first.Attempt, batch.events, now); err != nil || n != batch.accepted {
			t.Fatalf("n=%d err=%v", n, err)
		}
	}
	for _, invalid := range []domain.RunnerTaskEvent{{Seq: 0}, {Seq: -1}, {Seq: 4, Attempt: 2}, {Seq: 4, Attempt: -1}} {
		if n, err := s.AppendRunnerTaskEvents(first.ID, "runner-a", first.Attempt, []domain.RunnerTaskEvent{{Seq: 5}, invalid}, now); err == nil || n != 0 {
			t.Fatalf("invalid=%+v n=%d err=%v", invalid, n, err)
		}
	}
	if got := s.RunnerTaskEventsAfter(first.ID, first.Attempt, 1); len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 || got[0].Attempt != first.Attempt {
		t.Fatalf("first=%+v", got)
	}
	if _, err := s.ExpireRunnerTasks(*first.LeaseUntil); err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.ClaimRunnerTask("runner-a", []string{"terminal"}, *first.LeaseUntil, time.Minute)
	if err != nil || !ok {
		t.Fatalf("second claim=%v %v", ok, err)
	}
	if n, err := s.AppendRunnerTaskEvents(second.ID, "runner-a", second.Attempt, []domain.RunnerTaskEvent{{Seq: 1, Data: "new"}}, *first.LeaseUntil); err != nil || n != 1 {
		t.Fatalf("second append=%d %v", n, err)
	}
	if got := s.RunnerTaskEventsAfter(second.ID, second.Attempt, 0); len(got) != 1 || got[0].Data != "new" || got[0].Attempt != 2 {
		t.Fatalf("second=%+v", got)
	}
	if got := s.RunnerTaskEventsAfter(second.ID, 0, 0); len(got) != 0 {
		t.Fatalf("legacy playback=%+v", got)
	}
	history := s.RunnerTaskEvents(second.ID)
	if len(history) != 5 || history[0].Attempt != 0 || history[1].Data != "one" || history[4].Attempt != 2 {
		t.Fatalf("history=%+v", history)
	}
}

func TestPutRunnerTaskCannotResetExecutionIdentity(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	first := claimFencingTask(t, s, "immutable", "runner-a", now)
	reset := domain.RunnerTask{ID: first.ID, Provider: "terminal", MaxAttempts: 2}
	if err := s.PutRunnerTask(reset); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatalf("active reset=%v", err)
	}
	if err := s.CompleteRunnerTask(first.ID, "runner-a", first.Attempt, domain.RunnerTaskSuccess, nil, "done", "", nil, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunnerTask(reset); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatalf("terminal reset=%v", err)
	}
	final, _ := s.RunnerTask(first.ID)
	if final.Attempt != first.Attempt || final.Status != domain.RunnerTaskSuccess || final.Result != "done" {
		t.Fatalf("reset changed identity=%+v", final)
	}
}

func TestQueueRunnerMaintenanceTaskAtomicDeduplication(t *testing.T) {
	for _, action := range []string{domain.RunnerTaskActionCleanupRun, domain.RunnerTaskActionArchivePreview} {
		t.Run(action, func(t *testing.T) {
			s := newTestRunner(t)
			if err := s.PutRun(domain.Run{ID: "maintenance"}); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			results := make(chan domain.RunnerTask, 16)
			created := make(chan bool, 16)
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					task, fresh, err := s.QueueRunnerMaintenanceTask(domain.RunnerTask{ID: fmt.Sprintf("maintenance-%d", i), RunID: "maintenance", Provider: "terminal", Action: action, MaxAttempts: 3})
					if err != nil {
						t.Errorf("queue: %v", err)
						return
					}
					results <- task
					created <- fresh
				}(i)
			}
			wg.Wait()
			close(results)
			close(created)
			freshCount := 0
			for fresh := range created {
				if fresh {
					freshCount++
				}
			}
			id := ""
			for task := range results {
				if id == "" {
					id = task.ID
				}
				if task.ID != id {
					t.Fatalf("concurrent tasks differ: %s / %s", id, task.ID)
				}
			}
			if freshCount != 1 {
				t.Fatalf("created %d tasks, want one", freshCount)
			}
			if latest, ok := s.LatestRunnerTaskForRunAction("maintenance", action); !ok || latest.ID != id {
				t.Fatalf("latest=%+v %v", latest, ok)
			}
			if err := s.CancelRunnerTask(id, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			next, fresh, err := s.QueueRunnerMaintenanceTask(domain.RunnerTask{ID: "next-maintenance", RunID: "maintenance", Provider: "terminal", Action: action, MaxAttempts: 3})
			if err != nil || !fresh || next.ID == id || next.Attempt != 0 || next.MaxAttempts != 3 {
				t.Fatalf("next=%+v fresh=%v err=%v", next, fresh, err)
			}
			if latest, ok := s.LatestRunnerTaskForRunAction("maintenance", action); !ok || latest.ID != next.ID {
				t.Fatalf("latest after terminal=%+v %v", latest, ok)
			}
		})
	}
}

func TestQueueRunnerMaintenanceTaskRequiresDurableRun(t *testing.T) {
	s := newTestRunner(t)
	if _, _, err := s.QueueRunnerMaintenanceTask(domain.RunnerTask{ID: "orphan", RunID: "missing", Provider: "terminal", Action: domain.RunnerTaskActionCleanupRun}); err == nil {
		t.Fatal("orphan maintenance accepted")
	}
	if _, ok := s.RunnerTask("orphan"); ok {
		t.Fatal("orphan task persisted")
	}
}

func TestLatestRunnerTaskForRunActionPrefersActiveTask(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(fmt.Sprint(delayed), func(t *testing.T) {
			s := newTestRunner(t)
			now := time.Now().UTC()
			terminalAt := now
			if delayed {
				terminalAt = now.Add(time.Hour)
			}
			for _, task := range []domain.RunnerTask{
				{ID: "z-terminal", RunID: "cleanup", Provider: "terminal", Action: domain.RunnerTaskActionCleanupRun, Status: domain.RunnerTaskSuccess, CreatedAt: terminalAt},
				{ID: "a-active", RunID: "cleanup", Provider: "terminal", Action: domain.RunnerTaskActionCleanupRun, Status: domain.RunnerTaskQueued, CreatedAt: now},
			} {
				if err := s.PutRunnerTask(task); err != nil {
					t.Fatal(err)
				}
			}
			if task, ok := s.LatestRunnerTaskForRunAction("cleanup", domain.RunnerTaskActionCleanupRun); !ok || task.ID != "a-active" {
				t.Fatalf("active hidden by terminal: %+v %v", task, ok)
			}
		})
	}
}
