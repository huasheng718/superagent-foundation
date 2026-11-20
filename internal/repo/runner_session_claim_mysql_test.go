package repo

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func newMySQLSessionClaimFixture(t *testing.T) (*Store, *Store, domain.Runner, domain.Run, domain.RunnerTask) {
	t.Helper()
	return newMySQLSessionClaimFixtureWithQueue(t, true)
}

func newMySQLSessionClaimFixtureWithQueue(t *testing.T, queue bool) (*Store, *Store, domain.Runner, domain.Run, domain.RunnerTask) {
	t.Helper()
	dsn := os.Getenv("AGENTFLOW_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_MYSQL_DSN is not set")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	other, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	_, runner, run, task, _ := newSessionPlacedClaimFixture(t)
	suffix := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	runner.ID = "mysql-session-runner-" + suffix
	run.ID = "mysql-session-run-" + suffix
	task.ID, task.RunID, task.RunnerID = "mysql-session-task-"+suffix, run.ID, runner.ID
	placement := *task.ExecutionPlacement
	placement.RunnerID, placement.CapabilitySHA256 = runner.ID, ""
	placement, err = domain.CanonicalNodeExecutionPlacement(placement)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext.RunID, run.ExecutionContext.ExecutionRunnerID = run.ID, runner.ID
	run.ExecutionContext.NodePlacements = map[string]domain.NodePlacementRequest{task.NodeID: {RunnerID: runner.ID}}
	run.Nodes[task.NodeID].ExecutionPlacement = &placement
	run.Nodes[task.NodeID].ExecutionSequence, run.Nodes[task.NodeID].ExecutionTaskID = 0, ""
	task.ExecutionPlacement = &placement
	task.Invocation.RunID, task.Invocation.WorkspaceIsolation.ContextSHA256 = run.ID, ""
	contextSHA, err := domain.RunnerContributionContextSHA(task)
	if err != nil {
		t.Fatal(err)
	}
	task.Invocation.WorkspaceIsolation.ContextSHA256 = contextSHA
	t.Cleanup(func() {
		_ = s.DB.Where("id = ?", task.ID).Delete(&runnerTaskRow{}).Error
		for _, table := range []string{"run_nodes", "run_events", "run_telemetry", "run_logs", "run_artifacts"} {
			_ = s.DB.Table(table).Where("run_id = ?", run.ID).Delete(map[string]any{}).Error
		}
		_ = s.DB.Where("id = ?", run.ID).Delete(&runRow{}).Error
		_ = s.DB.Where("id = ?", runner.ID).Delete(&runnerRow{}).Error
	})
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if queue {
		if err := s.QueueNodeExecutionTask(task); err != nil {
			t.Fatal(err)
		}
		run, _ = s.RunState(run.ID)
		task, _ = s.RunnerTask(task.ID)
	}
	return s, other, runner, run, task
}

func TestMySQLRunnerSessionClaimHasOneWinner(t *testing.T) {
	s, other, runner, _, task := newMySQLSessionClaimFixture(t)
	start := make(chan struct{})
	results := make(chan domain.RunnerTask, 8)
	errorsOut := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			store := s
			if i%2 != 0 {
				store = other
			}
			got, claimed, err := store.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, time.Now().UTC(), time.Minute)
			if err != nil {
				errorsOut <- err
				return
			}
			if claimed {
				results <- got
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsOut)
	for err := range errorsOut {
		t.Error(err)
	}
	winners := 0
	for got := range results {
		winners++
		if got.ID != task.ID || got.Attempt != 1 || got.ExecutionPlacement == nil {
			t.Errorf("invalid winner: %+v", got)
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners=%d, expected one", winners)
	}
}

func TestMySQLRunnerSessionClaimWaitExpiry(t *testing.T) {
	for _, mode := range []string{"session", "engine-lease"} {
		t.Run(mode, func(t *testing.T) {
			s, other, runner, run, task := newMySQLSessionClaimFixture(t)
			expires := time.Now().UTC().Add(600 * time.Millisecond)
			if mode == "engine-lease" {
				if err := s.DB.Model(&runRow{}).Where("id = ?", run.ID).Update("lease_until", expires).Error; err != nil {
					t.Fatal(err)
				}
			}
			blocker := other.DB.Begin()
			if blocker.Error != nil {
				t.Fatal(blocker.Error)
			}
			defer blocker.Rollback()
			var locked runnerRow
			if err := other.locking(blocker).Where("id = ?", runner.ID).Take(&locked).Error; err != nil {
				t.Fatal(err)
			}
			if mode == "session" {
				if err := blocker.Model(&runnerRow{}).Where("id = ?", runner.ID).Update("session_expires_at", expires).Error; err != nil {
					t.Fatal(err)
				}
			}
			aboutToWait := make(chan struct{})
			var once sync.Once
			callback := "session-claim-wait-" + mode
			if err := s.DB.Callback().Query().Before("gorm:query").Register(callback, func(db *gorm.DB) {
				if db.Statement.Table == "runners" {
					once.Do(func() { close(aboutToWait) })
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer s.DB.Callback().Query().Remove(callback)
			type outcome struct {
				task    domain.RunnerTask
				claimed bool
				err     error
			}
			result := make(chan outcome, 1)
			capturedAt := time.Now().UTC()
			go func() {
				got, claimed, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, capturedAt, time.Minute)
				result <- outcome{got, claimed, err}
			}()
			select {
			case <-aboutToWait:
			case early := <-result:
				t.Fatalf("claim did not wait for registry lock: %+v", early)
			case <-time.After(5 * time.Second):
				t.Fatal("claim did not reach registry lock")
			}
			if remaining := time.Until(expires.Add(150 * time.Millisecond)); remaining > 0 {
				time.Sleep(remaining)
			}
			if err := blocker.Commit().Error; err != nil {
				t.Fatal(err)
			}
			var got outcome
			select {
			case got = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("claim did not finish after registry lock release")
			}
			want := ErrRunnerPlacementSessionInvalid
			if mode == "engine-lease" {
				want = ErrRunnerTaskLeaseLost
			}
			if got.claimed || got.task.ID != "" || !errors.Is(got.err, want) {
				t.Fatalf("expired post-lock authority returned: task=%+v claimed=%v error=%v", got.task, got.claimed, got.err)
			}
			stored, _ := s.RunnerTask(task.ID)
			if stored.Attempt != 0 || stored.Status != domain.RunnerTaskQueued {
				t.Fatal("expired claim changed task lease")
			}
		})
	}
}

func TestMySQLRunnerSessionClaimLegacyInsertConcurrency(t *testing.T) {
	s, other, _, _, _ := newMySQLSessionClaimFixture(t)
	suffix := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	start := make(chan struct{})
	errorsOut := make(chan error, 16)
	ids := make([]string, 16)
	var wg sync.WaitGroup
	for i := range ids {
		ids[i] = fmt.Sprintf("mysql-legacy-insert-%s-%02d", suffix, i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			store := s
			if i%2 != 0 {
				store = other
			}
			errorsOut <- store.PutRunnerTask(domain.RunnerTask{ID: ids[i], Provider: "codex"})
		}(i)
	}
	defer s.DB.Where("id IN ?", ids).Delete(&runnerTaskRow{})
	close(start)
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Error(err)
		}
	}
	var count int64
	if err := s.DB.Model(&runnerTaskRow{}).Where("id IN ?", ids).Count(&count).Error; err != nil || count != 16 {
		t.Fatalf("inserted=%d error=%v", count, err)
	}
}

func TestMySQLRunnerSessionClaimLegacyQueueIdentityRace(t *testing.T) {
	// Either creator may win the global task ID. A legacy loser must never
	// replace the winning placement; a placed loser must roll back its author
	// pointer. Separate connections exercise the absent-ID insert race.
	for round := 0; round < 8; round++ {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			s, other, runner, run, task := newMySQLSessionClaimFixtureWithQueue(t, false)
			legacy := domain.RunnerTask{ID: task.ID, RunID: run.ID, NodeID: task.NodeID, RunnerID: runner.ID, Provider: "codex"}
			start := make(chan struct{})
			legacyResult, queueResult := make(chan error, 1), make(chan error, 1)
			go func() { <-start; legacyResult <- other.PutRunnerTask(legacy) }()
			go func() { <-start; queueResult <- s.QueueNodeExecutionTask(task) }()
			close(start)
			legacyErr, queueErr := <-legacyResult, <-queueResult
			stored, ok := s.RunnerTask(task.ID)
			if !ok {
				t.Fatal("neither creator retained the task")
			}
			persistedRun, ok := s.RunState(run.ID)
			if !ok {
				t.Fatal("run disappeared")
			}
			node := persistedRun.Nodes[task.NodeID]
			if queueErr == nil {
				if !errors.Is(legacyErr, domain.ErrRunnerPlacementInvalid) || stored.ExecutionPlacement == nil || stored.Execution == nil || node.ExecutionTaskID != task.ID || node.ExecutionSequence != 1 {
					t.Fatalf("legacy stripped placed winner: legacy=%v task=%+v node=%+v", legacyErr, stored, node)
				}
			} else {
				if legacyErr != nil || (!errors.Is(queueErr, ErrRunnerTaskLeaseLost) && !errors.Is(queueErr, domain.ErrRunnerPlacementInvalid)) || stored.ExecutionPlacement != nil || stored.Execution != nil || node.ExecutionTaskID != "" || node.ExecutionSequence != 0 {
					t.Fatalf("failed placed insert retained authority: legacy=%v queue=%v task=%+v node=%+v", legacyErr, queueErr, stored, node)
				}
			}
		})
	}
}
