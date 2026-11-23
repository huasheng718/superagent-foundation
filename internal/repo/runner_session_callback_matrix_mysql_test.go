package repo

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

type mysqlSessionCallbackMatrixOutcome struct {
	err      error
	desired  string
	accepted int
	control  domain.RunnerTask
	spec     domain.RunnerContributionSpec
	receipt  domain.RunnerContributionReceipt
}

func runMySQLSessionCallbackMatrixOperation(s *Store, operation string, task domain.RunnerTask, runner domain.Runner, now time.Time) mysqlSessionCallbackMatrixOutcome {
	var out mysqlSessionCallbackMatrixOutcome
	switch operation {
	case "start":
		out.err = s.StartRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, task.Attempt, now)
	case "renew":
		out.desired, out.err = s.RenewRunnerTaskLeaseForSession(task.ID, runner.ID, runner.SessionTokenHash, task.Attempt, 2*time.Minute, now)
	case "events":
		out.accepted, out.err = s.AppendRunnerTaskEventsForSession(task.ID, runner.ID, runner.SessionTokenHash, task.Attempt, []domain.RunnerTaskEvent{{Attempt: task.Attempt, Seq: 1, Type: "stdout", Data: "callback-must-not-persist", Ts: now}}, now)
	case "complete":
		out.err = s.CompleteRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, task.Attempt, domain.RunnerTaskSuccess, nil, "callback-must-not-complete", "", map[string]string{"must-not-write": "artifact"}, nil, now)
	case "control":
		out.control, out.err = s.RunnerTaskControlForSession(task.ID, runner.ID, runner.SessionTokenHash, task.Attempt, now)
	case "upload-validate":
		out.spec, out.err = s.ValidateRunnerContributionUploadForSession(task.ID, runner.ID, runner.SessionTokenHash, task.Attempt, "unassigned-repository", now)
	case "upload-stage":
		// No valid receipt/blob is needed: expired identity/lease must be rejected
		// before consulting the repository spec or accepting any receipt fields.
		receipt := domain.RunnerContributionReceipt{TaskID: task.ID, Attempt: task.Attempt, RepositoryID: "unassigned-repository", ResultSHA: strings.Repeat("1", 40), SHA256: strings.Repeat("2", 64)}
		out.receipt, out.err = s.StageRunnerContributionForSession(receipt, runner.ID, runner.SessionTokenHash, now)
	default:
		panic("unknown callback matrix operation")
	}
	return out
}

// Every task-plane operation must recheck its captured session after a real
// registry-row lock wait. The same matrix separately crosses session, task
// lease, Node deadline and current owning Engine lease expiry. These are
// opt-in MySQL tests; the shared fixture skips before opening a database when
// AGENTFLOW_TEST_MYSQL_DSN is absent.
func TestMySQLRunnerSessionCallbackMatrixWaitFences(t *testing.T) {
	for _, operation := range []string{"start", "renew", "events", "complete", "control", "upload-validate", "upload-stage"} {
		t.Run(operation, func(t *testing.T) {
			for _, mode := range []string{"session-rotation", "session-expiry", "task-lease", "task-deadline", "engine-lease"} {
				t.Run(mode, func(t *testing.T) {
					s, other, runner, run, task := newMySQLSessionClaimFixture(t)
					t.Cleanup(func() { _ = s.DB.Where("task_id = ?", task.ID).Delete(&runnerTaskEventRow{}).Error })
					var expires time.Time
					if mode == "task-deadline" {
						// Configure the immutable fixture before claim. Both Node and task pin
						// the same absolute deadline, and the assignment digest binds it.
						expires = time.Now().UTC().Add(1200 * time.Millisecond)
						task.ExecutionDeadline = &expires
						run.Nodes[task.NodeID].ExecutionDeadline = &expires
						freezeNodeQueueTask(t, &task)
						if err := s.DB.Transaction(func(tx *gorm.DB) error {
							if err := tx.Model(&runNodeRow{}).Where("run_id = ? AND node_id = ?", run.ID, task.NodeID).Update("payload", jsonBytes(run.Nodes[task.NodeID])).Error; err != nil {
								return err
							}
							return tx.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Update("payload", runnerTaskPayloadBytes(task)).Error
						}); err != nil {
							t.Fatal(err)
						}
					}
					claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, time.Now().UTC(), time.Minute)
					if err != nil || !found || claimed.ID != task.ID {
						t.Fatalf("fixture claim found=%v error=%v", found, err)
					}
					task = claimed
					if mode != "task-deadline" {
						expires = time.Now().UTC().Add(600 * time.Millisecond)
					}
					switch mode {
					case "task-lease":
						if err := s.DB.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Update("lease_until", expires).Error; err != nil {
							t.Fatal(err)
						}
					case "engine-lease":
						if err := s.DB.Model(&runRow{}).Where("id = ?", run.ID).Updates(map[string]any{"owner_instance_id": "matrix-engine", "lease_until": expires}).Error; err != nil {
							t.Fatal(err)
						}
					}
					// Snapshot the exact durable row after fixture configuration. Comparing
					// the full row also covers pending/accepted receipt payloads and leases.
					var before runnerTaskRow
					if err := s.DB.Where("id = ?", task.ID).Take(&before).Error; err != nil {
						t.Fatal(err)
					}
					var eventsBefore []runnerTaskEventRow
					if err := s.DB.Where("task_id = ?", task.ID).Order("attempt, seq").Find(&eventsBefore).Error; err != nil {
						t.Fatal(err)
					}
					blocker := other.DB.Begin()
					if blocker.Error != nil {
						t.Fatal(blocker.Error)
					}
					defer blocker.Rollback()
					var registry runnerRow
					if err := other.locking(blocker).Where("id = ?", runner.ID).Take(&registry).Error; err != nil {
						t.Fatal(err)
					}
					waiting := make(chan struct{})
					var once sync.Once
					callbackName := fmt.Sprintf("callback-matrix-%s-%s-%d", operation, mode, time.Now().UnixNano())
					if err := s.DB.Callback().Query().Before("gorm:query").Register(callbackName, func(db *gorm.DB) {
						if db.Statement.Table == "runners" {
							once.Do(func() { close(waiting) })
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer s.DB.Callback().Query().Remove(callbackName)
					capturedAt := time.Now().UTC()
					if !capturedAt.Before(expires) {
						t.Fatal("fixture authority expired before callback started")
					}
					result := make(chan mysqlSessionCallbackMatrixOutcome, 1)
					go func() { result <- runMySQLSessionCallbackMatrixOperation(s, operation, task, runner, capturedAt) }()
					select {
					case <-waiting:
					case early := <-result:
						t.Fatalf("callback returned before registry lock wait: %v", early.err)
					case <-time.After(5 * time.Second):
						t.Fatal("callback did not reach registry lock")
					}
					// The callback is now at the registry query, with Run/task locks already
					// acquired. Rotate/expire the session inside the held registry lock.
					if mode == "session-rotation" {
						if err := blocker.Model(&runnerRow{}).Where("id = ?", runner.ID).Update("session_token_hash", strings.Repeat("e", 64)).Error; err != nil {
							t.Fatal(err)
						}
					} else if mode == "session-expiry" {
						if err := blocker.Model(&runnerRow{}).Where("id = ?", runner.ID).Update("session_expires_at", expires).Error; err != nil {
							t.Fatal(err)
						}
					}
					holdUntil := expires.Add(150 * time.Millisecond)
					if mode == "session-rotation" {
						holdUntil = time.Now().Add(40 * time.Millisecond)
					}
					if remaining := time.Until(holdUntil); remaining > 0 {
						timer := time.NewTimer(remaining)
						select {
						case early := <-result:
							timer.Stop()
							t.Fatalf("callback bypassed held registry lock: %v", early.err)
						case <-timer.C:
						}
					}
					if err := blocker.Commit().Error; err != nil {
						t.Fatal(err)
					}
					var out mysqlSessionCallbackMatrixOutcome
					select {
					case out = <-result:
					case <-time.After(5 * time.Second):
						t.Fatal("callback did not finish after registry lock release")
					}
					want := ErrRunnerTaskLeaseLost
					if mode == "session-rotation" || mode == "session-expiry" {
						want = ErrRunnerPlacementSessionInvalid
					}
					if !errors.Is(out.err, want) {
						t.Fatalf("post-lock authority error=%v, want %v", out.err, want)
					}
					if want == ErrRunnerTaskLeaseLost && errors.Is(out.err, ErrRunnerPlacementSessionInvalid) {
						t.Fatal("task/Engine expiry was misclassified as session401")
					}
					if want == ErrRunnerPlacementSessionInvalid && errors.Is(out.err, ErrRunnerTaskLeaseLost) {
						t.Fatal("session rotation/expiry was misclassified as attempt409")
					}
					if out.desired != "" || out.accepted != 0 || !reflect.DeepEqual(out.control, domain.RunnerTask{}) || !reflect.DeepEqual(out.spec, domain.RunnerContributionSpec{}) || !reflect.DeepEqual(out.receipt, domain.RunnerContributionReceipt{}) {
						t.Fatal("denied callback returned control/spec/receipt authority or accepted events")
					}
					var after runnerTaskRow
					if err := s.DB.Where("id = ?", task.ID).Take(&after).Error; err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before, after) {
						t.Fatal("denied callback changed task/lease/receipt payload")
					}
					var eventsAfter []runnerTaskEventRow
					if err := s.DB.Where("task_id = ?", task.ID).Order("attempt, seq").Find(&eventsAfter).Error; err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(eventsBefore, eventsAfter) {
						t.Fatal("denied callback persisted events")
					}
				})
			}
		})
	}
}
