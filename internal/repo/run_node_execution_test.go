package repo

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// The Run already names author-1; the task is not inserted. Claim tests can
// persist runnerTaskRowValues(task) directly; queue tests first clear the Node
// pointer to sequence zero, then use the normal queue transaction.
func newRunnerExecutionFixture(t *testing.T) (*Store, domain.Run, domain.RunnerTask, time.Time) {
	t.Helper()
	s, _, p, now := newRegistryPlacementFixture(t, false)
	p.Runtimes = p.Runtimes[1:]
	p.CapabilitySHA256 = ""
	p, err := domain.CanonicalNodeExecutionPlacement(p)
	if err != nil {
		t.Fatal(err)
	}
	deadline := now.Add(time.Hour)
	run := domain.Run{ID: "placed-execution-run", WorkflowID: "placed-workflow", Status: "running", WorkflowSnapshot: domain.WorkflowDefinition{ID: "placed-workflow", Nodes: []domain.WorkflowNode{{ID: "writer", Type: "script"}}}, ExecutionContext: domain.TelemetryContext{RunID: "placed-execution-run", ProjectID: p.ProjectID, ResponsibleUserID: p.ResponsibleUserID, DeliveryPolicy: domain.DeliveryPolicyControlledPR, ExecutionMode: domain.ExecutionModeRunner, ExecutionRunnerID: p.RunnerID, RunnerWorkspaceLayout: domain.RunnerWorkspaceLayoutAttempt, NodePlacements: map[string]domain.NodePlacementRequest{"writer": {RunnerID: p.RunnerID, OS: p.OS, Arch: p.Arch, RequiredCapabilities: []string{"terminal"}}}}, Nodes: map[string]*domain.NodeRun{"writer": {NodeID: "writer", Status: "running", Attempt: 1, ExecutionPlacement: &p, ExecutionSequence: 0, ExecutionTaskID: "", ExecutionDeadline: &deadline}}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	var ok bool
	run, ok = s.RunState(run.ID)
	if !ok {
		t.Fatal("fixture Run not persisted")
	}
	task := domain.RunnerTask{ID: "author-1", RunID: run.ID, NodeID: "writer", RunnerID: p.RunnerID, Provider: "terminal", Command: "true", Status: domain.RunnerTaskQueued, MaxAttempts: 1, DeliveryPolicy: domain.DeliveryPolicyControlledPR, ExecutionPlacement: &p, Execution: &domain.NodeTaskExecution{Role: domain.NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: 1}, ExecutionDeadline: &deadline, Invocation: &domain.RunnerInvocation{RunID: run.ID, NodeID: "writer", WorkspaceIsolation: &domain.RunnerWorkspaceIsolation{Version: domain.RunnerWorkspaceIsolationVersion}}}
	freezeRunnerExecutionFixtureContext(t, &task)
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		locked, err := s.RunsRepo.lockRunnerExecutionRunTx(tx, run.ID, now)
		if err != nil {
			return err
		}
		return s.RunsRepo.advanceRunnerExecutionStateTx(tx, locked, task)
	}); err != nil {
		t.Fatal(err)
	}
	run, ok = s.RunState(run.ID)
	if !ok {
		t.Fatal("fixture author not persisted")
	}
	return s, run, task, now
}
func freezeRunnerExecutionFixtureContext(t *testing.T, task *domain.RunnerTask) {
	t.Helper()
	sha, err := domain.RunnerContributionContextSHA(*task)
	if err != nil {
		t.Fatal(err)
	}
	task.Invocation.WorkspaceIsolation.ContextSHA256 = sha
}
func lockedRunnerExecutionFixture(t *testing.T, s *Store, runID string, now time.Time) domain.Run {
	t.Helper()
	var run domain.Run
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		run, err = s.RunsRepo.lockRunnerExecutionRunTx(tx, runID, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRunnerExecutionRunLoadsStrictLockedNodeState(t *testing.T) {
	s, run, task, now := newRunnerExecutionFixture(t)
	locked := lockedRunnerExecutionFixture(t, s, run.ID, now)
	if !reflect.DeepEqual(locked.Nodes, run.Nodes) || locked.ExecutionContext.ProjectID != run.ExecutionContext.ProjectID {
		t.Fatal("locked authority lost Node state or context")
	}
	if err := s.RunsRepo.validateRunnerExecutionState(locked, task); err != nil {
		t.Fatal(err)
	}
	// Recovery may acquire a new Engine generation without replacing a live
	// Runner's durable Node attempt. The task is still the current author.
	if err := s.DB.Model(&runRow{}).Where("id = ?", run.ID).Update("execution_generation", 99).Error; err != nil {
		t.Fatal(err)
	}
	locked = lockedRunnerExecutionFixture(t, s, run.ID, now)
	if locked.Generation != 99 || s.RunsRepo.validateRunnerExecutionState(locked, task) != nil {
		t.Fatal("Engine recovery invalidated unchanged Node attempt")
	}
}

func TestRunnerExecutionRunRejectsMalformedAndIndexedDrift(t *testing.T) {
	for _, tc := range []struct {
		name, column string
		value        any
		node         bool
	}{
		{"context", "execution_context", []byte(`{"nodePlacements":`), false},
		{"contextNull", "execution_context", []byte(`null`), false},
		{"snapshot", "snapshot", []byte(`[]`), false},
		{"repository", "repository", []byte(`{"repositoryId":`), false},
		{"repositories", "repositories", []byte(`{}`), false},
		{"nodeJSON", "payload", []byte(`{"nodeId":`), true},
		{"nodeNull", "payload", []byte(`null`), true},
		{"indexedAttempt", "attempt", 2, true},
		{"indexedStatus", "status", "pending", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, run, _, now := newRunnerExecutionFixture(t)
			table := "runs"
			query := s.DB.Table(table).Where("id = ?", run.ID)
			if tc.node {
				query = s.DB.Table("run_nodes").Where("run_id = ? AND node_id = ?", run.ID, "writer")
			}
			if err := query.Update(tc.column, tc.value).Error; err != nil {
				t.Fatal(err)
			}
			err := s.DB.Transaction(func(tx *gorm.DB) error { _, err := s.RunsRepo.lockRunnerExecutionRunTx(tx, run.ID, now); return err })
			if !errors.Is(err, ErrRunnerTaskLeaseLost) {
				t.Fatalf("malformed authority accepted: %v", err)
			}
		})
	}
}

func TestRunnerExecutionRunLifecycleAndPostLockTime(t *testing.T) {
	for name, edit := range map[string]func(*domain.Run){
		"terminal":           func(run *domain.Run) { run.Status = "succeeded" },
		"waiting":            func(run *domain.Run) { run.Status = "waiting" },
		"cancel":             func(run *domain.Run) { run.CancelRequested = true },
		"pause":              func(run *domain.Run) { run.PauseRequested = true },
		"cleanup":            func(run *domain.Run) { run.WorkspaceCleanedAt = time.Now() },
		"editor":             func(run *domain.Run) { run.WorkspaceEditUntil = time.Now().Add(time.Hour) },
		"expiredEngineLease": func(run *domain.Run) { run.OwnerInstanceID = "engine"; run.LeaseUntil = time.Now().Add(-time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			s, run, task, now := newRunnerExecutionFixture(t)
			edit(&run)
			if err := s.RunsRepo.validateRunnerExecutionRunAt(run, now); !errors.Is(err, ErrRunnerTaskLeaseLost) {
				t.Fatalf("nonexecuting Run accepted: %v", err)
			}
			if err := s.RunsRepo.validateRunnerExecutionState(run, task); !errors.Is(err, ErrRunnerTaskLeaseLost) {
				t.Fatalf("task ignored Run lifecycle: %v", err)
			}
		})
	}
	s, run, _, now := newRunnerExecutionFixture(t)
	run.OwnerInstanceID = "engine"
	run.LeaseUntil = now.Add(time.Second)
	if err := s.RunsRepo.validateRunnerExecutionRunAt(run, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RunsRepo.validateRunnerExecutionRunAt(run, now.Add(time.Second)); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatal("post-lock Engine expiry ignored")
	}
}

func TestRunnerExecutionStateRejectsSupersessionAndIdentityDrift(t *testing.T) {
	for name, edit := range map[string]func(*domain.Run, *domain.RunnerTask){
		"currentAuthor":   func(run *domain.Run, _ *domain.RunnerTask) { run.Nodes["writer"].ExecutionTaskID = "new-author" },
		"currentSequence": func(run *domain.Run, _ *domain.RunnerTask) { run.Nodes["writer"].ExecutionSequence++ },
		"nodeAttempt":     func(run *domain.Run, _ *domain.RunnerTask) { run.Nodes["writer"].Attempt++ },
		"nodePending":     func(run *domain.Run, _ *domain.RunnerTask) { run.Nodes["writer"].Status = "pending" },
		"target":          func(_ *domain.Run, task *domain.RunnerTask) { task.RunnerID = "other" },
		"snapshot":        func(_ *domain.Run, task *domain.RunnerTask) { task.ExecutionPlacement.NodeAttempt++ },
		"project":         func(run *domain.Run, _ *domain.RunnerTask) { run.ExecutionContext.ProjectID = "other" },
		"responsible":     func(run *domain.Run, _ *domain.RunnerTask) { run.ExecutionContext.ResponsibleUserID++ },
		"request": func(run *domain.Run, _ *domain.RunnerTask) {
			run.ExecutionContext.NodePlacements["writer"] = domain.NodePlacementRequest{RunnerID: "other"}
		},
		"requestCapabilities": func(run *domain.Run, _ *domain.RunnerTask) {
			request := run.ExecutionContext.NodePlacements["writer"]
			request.RequiredCapabilities = []string{"missing"}
			run.ExecutionContext.NodePlacements["writer"] = request
		},
		"provider":        func(_ *domain.Run, task *domain.RunnerTask) { task.Provider = "codex" },
		"role":            func(_ *domain.Run, task *domain.RunnerTask) { task.Execution.Role = "unknown" },
		"missingPrevious": func(_ *domain.Run, task *domain.RunnerTask) { task.Execution.Sequence = 2 },
		"invocationRun":   func(_ *domain.Run, task *domain.RunnerTask) { task.Invocation.RunID = "other" },
		"invocationNode":  func(_ *domain.Run, task *domain.RunnerTask) { task.Invocation.NodeID = "other" },
		"layout":          func(_ *domain.Run, task *domain.RunnerTask) { task.Invocation.WorkspaceIsolation.Version++ },
		"contextSHA": func(_ *domain.Run, task *domain.RunnerTask) {
			task.Invocation.WorkspaceIsolation.ContextSHA256 = strings.Repeat("a", 64)
		},
		"resume": func(_ *domain.Run, task *domain.RunnerTask) {
			task.Invocation.ResumeSessionID = "unproven-native-session"
		},
		"deadline": func(_ *domain.Run, task *domain.RunnerTask) { task.ExecutionDeadline = nil },
		"expiredNode": func(run *domain.Run, task *domain.RunnerTask) {
			past := time.Now().Add(-time.Second)
			run.Nodes["writer"].ExecutionDeadline = &past
			task.ExecutionDeadline = &past
		},
		"repositoryInjection": func(_ *domain.Run, task *domain.RunnerTask) {
			task.Invocation.Repositories = []domain.RunnerRepository{{RepositoryID: "outside"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, run, task, _ := newRunnerExecutionFixture(t)
			edit(&run, &task)
			if err := s.RunsRepo.validateRunnerExecutionState(run, task); err == nil {
				t.Fatal("changed execution authority accepted")
			}
		})
	}
}

func TestRunnerExecutionAdvanceAuthorAndVerificationAreDistinct(t *testing.T) {
	s, run, first, now := newRunnerExecutionFixture(t)
	next := domain.PublicRunnerTask(first)
	next.ID = "author-2"
	next.Execution.Sequence = 2
	next.Execution.PreviousAuthorTaskID = first.ID
	freezeRunnerExecutionFixtureContext(t, &next)
	if err := s.RunsRepo.validateRunnerExecutionState(run, next); err == nil {
		t.Fatal("unqueued successor treated as current author")
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		locked, err := s.RunsRepo.lockRunnerExecutionRunTx(tx, run.ID, now)
		if err != nil {
			return err
		}
		return s.RunsRepo.advanceRunnerExecutionStateTx(tx, locked, next)
	}); err != nil {
		t.Fatal(err)
	}
	after := lockedRunnerExecutionFixture(t, s, run.ID, now)
	if after.Nodes["writer"].ExecutionSequence != 2 || after.Nodes["writer"].ExecutionTaskID != next.ID || after.Revision != run.Revision+1 {
		t.Fatal("author pointer/revision did not advance atomically")
	}
	if err := s.RunsRepo.validateRunnerExecutionState(after, next); err != nil {
		t.Fatal(err)
	}
	if err := s.RunsRepo.validateRunnerExecutionState(after, first); err == nil {
		t.Fatal("old author remained authoritative")
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error { return s.RunsRepo.advanceRunnerExecutionStateTx(tx, run, next) }); err == nil {
		t.Fatal("stale succession snapshot advanced twice")
	}
	verify := domain.PublicRunnerTask(next)
	verify.ID = "verify-2"
	verify.Execution.Role = domain.NodeTaskRoleVerification
	verify.Execution.PreviousAuthorTaskID = next.ID
	freezeRunnerExecutionFixtureContext(t, &verify)
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		locked, err := s.RunsRepo.lockRunnerExecutionRunTx(tx, run.ID, now)
		if err != nil {
			return err
		}
		return s.RunsRepo.advanceRunnerExecutionStateTx(tx, locked, verify)
	}); err != nil {
		t.Fatal(err)
	}
	verified := lockedRunnerExecutionFixture(t, s, run.ID, now)
	if verified.Revision != after.Revision || !reflect.DeepEqual(verified.Nodes, after.Nodes) {
		t.Fatal("verification replaced author pointer or bumped unrelated revision")
	}
	if err := s.RunsRepo.validateRunnerExecutionState(verified, verify); err != nil {
		t.Fatal(err)
	}
	verify.Execution.PreviousAuthorTaskID = first.ID
	freezeRunnerExecutionFixtureContext(t, &verify)
	if err := s.RunsRepo.validateRunnerExecutionState(verified, verify); err == nil {
		t.Fatal("verification accepted superseded author")
	}
}

func TestRunnerExecutionAdvanceRollsBackWithOuterTransaction(t *testing.T) {
	s, run, first, now := newRunnerExecutionFixture(t)
	next := domain.PublicRunnerTask(first)
	next.ID = "author-2"
	next.Execution.Sequence = 2
	next.Execution.PreviousAuthorTaskID = first.ID
	freezeRunnerExecutionFixtureContext(t, &next)
	rollback := errors.New("task insert failed")
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		locked, err := s.RunsRepo.lockRunnerExecutionRunTx(tx, run.ID, now)
		if err != nil {
			return err
		}
		if err := s.RunsRepo.advanceRunnerExecutionStateTx(tx, locked, next); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	after := lockedRunnerExecutionFixture(t, s, run.ID, now)
	if after.Revision != run.Revision || !reflect.DeepEqual(after.Nodes, run.Nodes) {
		t.Fatal("failed task enqueue left authoritative author pointer behind")
	}
}

func TestRunnerExecutionExpiredSiblingDoesNotRevokeHealthyNode(t *testing.T) {
	s, run, task, now := newRunnerExecutionFixture(t)
	past := now.Add(-time.Second)
	run.Nodes["expired"] = &domain.NodeRun{NodeID: "expired", Status: "failed", Attempt: 1, ExecutionDeadline: &past}
	run.WorkflowSnapshot.Nodes = append(run.WorkflowSnapshot.Nodes, domain.WorkflowNode{ID: "expired", Type: "script"})
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	locked := lockedRunnerExecutionFixture(t, s, run.ID, now)
	if err := s.RunsRepo.validateRunnerExecutionState(locked, task); err != nil {
		t.Fatalf("unrelated sibling revoked healthy current author: %v", err)
	}
}

func TestRunnerExecutionRepositoryPinsAreFrozen(t *testing.T) {
	s, run, task, _ := newRunnerExecutionFixture(t)
	base := strings.Repeat("1", 40)
	run.Repositories = []domain.RepositoryContext{{RepositoryID: "repo", CloneURL: "https://example.com/repo.git", WorkspaceDir: "source"}}
	run.ExecutionContext.WorkspaceBaseSHAs = map[string]string{"repo": base}
	task.Invocation.Repositories = []domain.RunnerRepository{{RepositoryID: "repo", CloneURL: "https://example.com/repo.git", Dir: "source", BaseSHA: base}}
	freezeRunnerExecutionFixtureContext(t, &task)
	if err := s.RunsRepo.validateRunnerExecutionState(run, task); err != nil {
		t.Fatal(err)
	}
	task.Invocation.Repositories[0].Dir = "other"
	freezeRunnerExecutionFixtureContext(t, &task)
	if err := s.RunsRepo.validateRunnerExecutionState(run, task); err == nil {
		t.Fatal("repository directory drift accepted")
	}
	task.Invocation.Repositories[0].Dir = "source"
	task.Invocation.Repositories[0].BaseSHA = strings.Repeat("2", 40)
	freezeRunnerExecutionFixtureContext(t, &task)
	if err := s.RunsRepo.validateRunnerExecutionState(run, task); err == nil {
		t.Fatal("repository base drift accepted")
	}
}

func TestRunnerExecutionInitialAuthorRequiresEmptyPreviousPointer(t *testing.T) {
	s, run, task, now := newRunnerExecutionFixture(t)
	run.Nodes["writer"].ExecutionSequence = 0
	run.Nodes["writer"].ExecutionTaskID = ""
	payload, err := json.Marshal(run.Nodes["writer"])
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&runNodeRow{}).Where("run_id = ? AND node_id = ?", run.ID, "writer").Update("payload", payload).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		locked, err := s.RunsRepo.lockRunnerExecutionRunTx(tx, run.ID, now)
		if err != nil {
			return err
		}
		return s.RunsRepo.advanceRunnerExecutionStateTx(tx, locked, task)
	}); err != nil {
		t.Fatal(err)
	}
	after := lockedRunnerExecutionFixture(t, s, run.ID, now)
	if after.Nodes["writer"].ExecutionTaskID != task.ID || after.Nodes["writer"].ExecutionSequence != 1 {
		t.Fatal("initial author did not acquire current pointer")
	}
}
