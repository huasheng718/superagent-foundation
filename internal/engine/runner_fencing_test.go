package engine

import (
	"context"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
)

func TestAwaitRunnerTaskResetsEventWatermarkOnReclaim(t *testing.T) {
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	e := New(s, runtime.NewManager(t.TempDir()))
	now := time.Now().UTC()
	if err := s.PutRunnerTask(domain.RunnerTask{ID: "reclaimed", Provider: "terminal", MaxAttempts: 2}); err != nil {
		t.Fatal(err)
	}
	first, ok, err := s.ClaimRunnerTask("runner", []string{"terminal"}, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if _, err := s.AppendRunnerTaskEvents(first.ID, "runner", first.Attempt, []domain.RunnerTaskEvent{{Seq: 10, Type: "stdout", Data: "first"}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("INSERT INTO runner_task_events (task_id, attempt, seq, type, data) VALUES (?, 0, 100, 'stdout', 'unknown historical provenance')", first.ID).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observed := make(chan domain.TelemetryEvent, 20)
	done := make(chan error, 1)
	go func() {
		_, err := e.awaitRunnerTask(ctx, first.ID, func(event domain.TelemetryEvent) { observed <- event })
		done <- err
	}()
	for {
		select {
		case event := <-observed:
			if event.Message == "unknown historical provenance" {
				t.Fatal("legacy event entered playback")
			}
			if event.Message == "first" {
				goto firstObserved
			}
		case <-ctx.Done():
			t.Fatal("first attempt was not played")
		}
	}
firstObserved:
	if _, err := s.ExpireRunnerTasks(*first.LeaseUntil); err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.ClaimRunnerTask("runner", []string{"terminal"}, *first.LeaseUntil, time.Minute)
	if err != nil || !ok {
		t.Fatalf("second claim: %v %v", ok, err)
	}
	if _, err := s.AppendRunnerTaskEvents(second.ID, "runner", second.Attempt, []domain.RunnerTaskEvent{{Seq: 1, Type: "stdout", Data: "second"}}, *first.LeaseUntil); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteRunnerTask(second.ID, "runner", second.Attempt, domain.RunnerTaskSuccess, nil, "done", "", nil, nil, *first.LeaseUntil); err != nil {
		t.Fatal(err)
	}
	e.NotifyRunnerTaskChanged(second.ID)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("reclaimed task did not finish")
	}
	secondCount := 0
	for len(observed) > 0 {
		event := <-observed
		if event.Message == "first" || event.Message == "unknown historical provenance" {
			t.Fatalf("history replayed into newer attempt: %+v", event)
		}
		if event.Message == "second" {
			secondCount++
		}
	}
	if secondCount != 1 {
		t.Fatalf("second attempt output count=%d", secondCount)
	}
}

func TestRunnerMaintenanceTasksUseFreshIdentityAfterCompletion(t *testing.T) {
	for _, action := range []string{domain.RunnerTaskActionCleanupRun, domain.RunnerTaskActionArchivePreview} {
		t.Run(action, func(t *testing.T) {
			s, err := repo.OpenTest()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			e := New(s, runtime.NewManager(t.TempDir()))
			runnerID := "runner"
			if err := s.PutRunner(domain.Runner{ID: runnerID}); err != nil {
				t.Fatal(err)
			}
			run := domain.Run{ID: "maintenance", ExecutionContext: domain.TelemetryContext{ExecutionMode: domain.ExecutionModeRunner, ExecutionRunnerID: runnerID}}
			if err := s.PutRun(run); err != nil {
				t.Fatal(err)
			}
			queue := func() error {
				if action == domain.RunnerTaskActionCleanupRun {
					return e.queueRunnerRunCleanup(run)
				}
				return e.queueRunnerPreviewArchive(run, domain.RunnerPreviewArchiveSpec{RunID: run.ID})
			}
			if err := queue(); err != nil {
				t.Fatal(err)
			}
			if err := queue(); err != nil {
				t.Fatal(err)
			}
			if tasks, err := s.ActiveRunnerTasksForRun(run.ID); err != nil || len(tasks) != 1 {
				t.Fatalf("active idempotency=%+v %v", tasks, err)
			}
			now := time.Now().UTC()
			first, ok, err := s.ClaimRunnerTask(runnerID, []string{"terminal"}, now, time.Minute)
			if err != nil || !ok {
				t.Fatalf("claim=%v %v", ok, err)
			}
			if err := s.CompleteRunnerTask(first.ID, runnerID, first.Attempt, domain.RunnerTaskSuccess, nil, "old", "", nil, nil, now); err != nil {
				t.Fatal(err)
			}
			if err := queue(); err != nil {
				t.Fatal(err)
			}
			second, ok, err := s.ClaimRunnerTask(runnerID, []string{"terminal"}, now, time.Minute)
			if err != nil || !ok || first.ID == second.ID || second.Attempt != 1 || second.MaxAttempts != 3 {
				t.Fatalf("new identity=%+v first=%+v ok=%v err=%v", second, first, ok, err)
			}
			if err := s.CompleteRunnerTask(first.ID, runnerID, first.Attempt, domain.RunnerTaskSuccess, nil, "stale", "", nil, nil, now); err != repo.ErrRunnerTaskLeaseLost {
				t.Fatalf("old completion=%v", err)
			}
			history, _ := s.RunnerTask(first.ID)
			if history.Status != domain.RunnerTaskSuccess || history.Result != "old" {
				t.Fatalf("history lost=%+v", history)
			}
		})
	}
}
