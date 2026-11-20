package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func newNodeQueueFixture(t *testing.T, withRepository bool) (*Store, domain.Run, domain.RunnerTask, domain.Runner, time.Time) {
	t.Helper()
	s, runner, placement, now := newRegistryPlacementFixture(t, false)
	run := domain.Run{ID: "placed-queue", Status: "running", WorkflowSnapshot: domain.WorkflowDefinition{Nodes: []domain.WorkflowNode{{ID: "author", Type: "agent", AgentID: "developer"}}}, ExecutionContext: domain.TelemetryContext{ProjectID: "project", ResponsibleUserID: 42, DeliveryPolicy: domain.DeliveryPolicyControlledPR, RunnerWorkspaceLayout: domain.RunnerWorkspaceLayoutAttempt, NodePlacements: map[string]domain.NodePlacementRequest{"author": {RunnerID: runner.ID}}}, Nodes: map[string]*domain.NodeRun{"author": {NodeID: "author", Status: "running", Attempt: 1, ExecutionPlacement: &placement}}}
	task := domain.RunnerTask{ID: "author-task", RunID: run.ID, NodeID: "author", RunnerID: runner.ID, Provider: "codex", DeliveryPolicy: domain.DeliveryPolicyControlledPR, ExecutionPlacement: &placement, Execution: &domain.NodeTaskExecution{Role: domain.NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: 1}, Invocation: &domain.RunnerInvocation{RunID: run.ID, NodeID: "author", WorkspaceIsolation: &domain.RunnerWorkspaceIsolation{Version: 1}}}
	if withRepository {
		base := strings.Repeat("1", 40)
		run.Repositories = []domain.RepositoryContext{{RepositoryID: "repo", CloneURL: "https://example.invalid/test.git", WorkspaceDir: "code"}}
		run.ExecutionContext.WorkspaceBaseSHAs = map[string]string{"repo": base}
		task.Invocation.Repositories = []domain.RunnerRepository{{RepositoryID: "repo", CloneURL: run.Repositories[0].CloneURL, Dir: "code", BaseSHA: base}}
	}
	freezeNodeQueueTask(t, &task)
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	run, _ = s.RunState(run.ID)
	return s, run, task, runner, now
}

func freezeNodeQueueTask(t *testing.T, task *domain.RunnerTask) {
	t.Helper()
	// PublicRunnerTask sanitizes credentials; it is not a general deep clone
	// of legacy invocation fields. Give this mutable fixture its own stamp.
	isolation := *task.Invocation.WorkspaceIsolation
	task.Invocation.WorkspaceIsolation = &isolation
	contextSHA, err := domain.RunnerContributionContextSHA(*task)
	if err != nil {
		t.Fatal(err)
	}
	task.Invocation.WorkspaceIsolation.ContextSHA256 = contextSHA
	task.ContributionSpecs = nil
	for _, repository := range task.Invocation.Repositories {
		task.ContributionSpecs = append(task.ContributionSpecs, domain.RunnerContributionSpec{RepositoryID: repository.RepositoryID, Dir: repository.Dir, BaseSHA: repository.BaseSHA, ContextSHA256: contextSHA, MaxBytes: domain.RunnerContributionMaxBytes})
	}
}

// The receipt fixture is deliberately only storage/provenance evidence. Its
// bounded bytes are not a Git bundle; Git verification has a separate owner.
func completeNodeQueueFixture(t *testing.T, s *Store, task domain.RunnerTask, runner domain.Runner, now time.Time) (domain.RunnerTask, domain.RunnerContributionInput) {
	t.Helper()
	claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
	if err != nil || !found || claimed.ID != task.ID {
		t.Fatalf("claim=%s found=%v err=%v", claimed.ID, found, err)
	}
	var input domain.RunnerContributionInput
	var receipts []string
	if len(task.ContributionSpecs) > 0 {
		raw := []byte("bounded provenance fixture for " + task.ID)
		hash := sha256.Sum256(raw)
		digest := hex.EncodeToString(hash[:])
		if err := s.PutRunnerContributionBlob(digest, raw); err != nil {
			t.Fatal(err)
		}
		spec := task.ContributionSpecs[0]
		receipt, err := s.StageRunnerContributionForSession(domain.RunnerContributionReceipt{TaskID: task.ID, Attempt: claimed.Attempt, RepositoryID: spec.RepositoryID, Dir: spec.Dir, BaseSHA: spec.BaseSHA, ResultSHA: strings.Repeat("2", 40), ContextSHA256: spec.ContextSHA256, SHA256: digest, Size: int64(len(raw))}, runner.ID, runner.SessionTokenHash, now)
		if err != nil {
			t.Fatal(err)
		}
		receipts = []string{receipt.ID}
		input = domain.RunnerContributionInput{RepositoryID: spec.RepositoryID, BaseSHA: spec.BaseSHA, HeadSHA: receipt.ResultSHA, BundleSHA256: digest, BundleSize: int64(len(raw)), Sources: []domain.RunnerContributionInputSource{{RunID: task.RunID, NodeID: task.NodeID, TaskID: task.ID, ReceiptID: receipt.ID, NodeAttempt: task.Execution.NodeAttempt, TaskAttempt: claimed.Attempt, ContextSHA256: spec.ContextSHA256, BundleSHA256: digest, ResultSHA: receipt.ResultSHA}}}
	}
	if err := s.CompleteRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, domain.RunnerTaskSuccess, nil, "", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: receipts}, now); err != nil {
		t.Fatal(err)
	}
	accepted, _ := s.RunnerTask(task.ID)
	return accepted, input
}

func TestQueueNodeExecutionTaskAtomicAuthorAndLegacyIsolation(t *testing.T) {
	s, run, task, runner, now := newNodeQueueFixture(t, false)
	if err := s.QueueNodeExecutionTask(task); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.RunState(run.ID)
	if stored.Nodes[task.NodeID].ExecutionTaskID != task.ID || stored.Nodes[task.NodeID].ExecutionSequence != 1 || stored.Revision <= run.Revision {
		t.Fatalf("author state=%+v revision=%d", stored.Nodes[task.NodeID], stored.Revision)
	}
	if _, found, err := s.ClaimRunnerTask(runner.ID, []string{"codex"}, now, time.Minute); err != nil || found {
		t.Fatalf("legacy claim leased placed task: found=%v err=%v", found, err)
	}
	claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
	if err != nil || !found || claimed.ID != task.ID || claimed.Execution == nil || claimed.ExecutionPlacement == nil {
		t.Fatalf("session claim=%+v found=%v err=%v", domain.PublicRunnerTask(claimed), found, err)
	}
	if err := s.QueueNodeExecutionTask(task); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatalf("duplicate author identity: %v", err)
	}
}

func TestQueueNodeExecutionTaskCompetingAuthorsHaveOneWinner(t *testing.T) {
	s, run, task, _, _ := newNodeQueueFixture(t, false)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"first", "second"} {
		copy := domain.PublicRunnerTask(task)
		copy.ID = id
		freezeNodeQueueTask(t, &copy)
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.QueueNodeExecutionTask(copy) }()
	}
	wg.Wait()
	close(results)
	passed, rejected := 0, 0
	for err := range results {
		if err == nil {
			passed++
		} else if errors.Is(err, ErrRunnerTaskLeaseLost) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if passed != 1 || rejected != 1 {
		t.Fatalf("winners=%d conflicts=%d", passed, rejected)
	}
	stored, _ := s.RunState(run.ID)
	var count int64
	if err := s.DB.Model(&runnerTaskRow{}).Count(&count).Error; err != nil || count != 1 || stored.Nodes[task.NodeID].ExecutionSequence != 1 {
		t.Fatalf("tasks=%d author=%+v err=%v", count, stored.Nodes[task.NodeID], err)
	}
}

func TestQueueNodeExecutionTaskRejectsReusedIdentity(t *testing.T) {
	s, run, task, _, _ := newNodeQueueFixture(t, false)
	// Same global ID belongs to unrelated legacy work; it must not be edited.
	if err := s.PutRunnerTask(domain.RunnerTask{ID: task.ID, RunID: "another-run", Provider: "mock"}); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueNodeExecutionTask(task); err == nil {
		t.Fatal("reused ID accepted")
	}
	stored, _ := s.RunState(run.ID)
	if stored.Nodes[task.NodeID].ExecutionSequence != 0 || stored.Nodes[task.NodeID].ExecutionTaskID != "" || stored.Revision != run.Revision {
		t.Fatal("failed queue changed author state")
	}
}

func TestQueueNodeExecutionTaskFailedInsertRollsBackAuthor(t *testing.T) {
	s, run, task, _, _ := newNodeQueueFixture(t, false)
	// Fail the actual INSERT, after the Node pointer/revision were advanced.
	if err := s.DB.Exec(`CREATE TRIGGER reject_placed_task BEFORE INSERT ON runner_tasks BEGIN SELECT RAISE(ABORT, 'injected task insert failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.QueueNodeExecutionTask(task); err == nil {
		t.Fatal("injected insert unexpectedly succeeded")
	}
	stored, _ := s.RunState(run.ID)
	if stored.Nodes[task.NodeID].ExecutionSequence != 0 || stored.Nodes[task.NodeID].ExecutionTaskID != "" || stored.Revision != run.Revision {
		t.Fatal("failed insert committed author CAS or revision")
	}
	if _, exists := s.RunnerTask(task.ID); exists {
		t.Fatal("failed insert persisted task")
	}
}

func TestPlacedRunSavePreservesQueuedAuthorAuthority(t *testing.T) {
	s, stale, task, _, _ := newNodeQueueFixture(t, false)
	if err := s.QueueNodeExecutionTask(task); err != nil {
		t.Fatal(err)
	}
	queued, _ := s.RunState(stale.ID)
	stale.Nodes[task.NodeID].Output = "later observation from an older loaded Run"
	stale.Nodes[task.NodeID].ExecutionPlacement = nil
	if err := s.PutRun(stale); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.RunState(stale.ID)
	node := stored.Nodes[task.NodeID]
	if node.ExecutionTaskID != task.ID || node.ExecutionSequence != 1 || node.ExecutionPlacement == nil || node.Output != stale.Nodes[task.NodeID].Output || stored.Revision <= queued.Revision {
		t.Fatalf("stale write lost authority: %+v", node)
	}
	if stale.Nodes[task.NodeID].ExecutionPlacement != nil || stale.Nodes[task.NodeID].ExecutionTaskID != "" {
		t.Fatal("save mutated caller snapshot")
	}
	stored.Nodes[task.NodeID].Attempt = 2
	stored.Nodes[task.NodeID].ExecutionPlacement = nil
	if err := s.PutRun(stored); err != nil {
		t.Fatal(err)
	}
	next, _ := s.RunState(stored.ID)
	if next.Nodes[task.NodeID].ExecutionSequence != 0 || next.Nodes[task.NodeID].ExecutionTaskID != "" {
		t.Fatal("new attempt retained old author")
	}
	if err := s.PutRun(stale); !errors.Is(err, ErrRunDecisionStale) {
		t.Fatalf("old attempt overwrote new: %v", err)
	}
}

func TestPlacedRunSaveRejectsFrozenPlacementChanges(t *testing.T) {
	for _, mode := range []string{"request", "effective"} {
		t.Run(mode, func(t *testing.T) {
			s, run, _, _, _ := newNodeQueueFixture(t, false)
			if mode == "request" {
				run.ExecutionContext.NodePlacements["author"] = domain.NodePlacementRequest{RunnerID: "other"}
			} else {
				copy := *run.Nodes["author"].ExecutionPlacement
				copy.RunnerID = "other"
				copy.CapabilitySHA256 = ""
				run.Nodes["author"].ExecutionPlacement = &copy
			}
			if err := s.PutRun(run); !errors.Is(err, domain.ErrRunnerPlacementChanged) {
				t.Fatalf("frozen %s changed: %v", mode, err)
			}
		})
	}
}

func TestQueueNodeExecutionTaskVerificationAndHistoricalCorrection(t *testing.T) {
	s, run, task, runner, now := newNodeQueueFixture(t, true)
	if err := s.QueueNodeExecutionTask(task); err != nil {
		t.Fatal(err)
	}
	_, firstInput := completeNodeQueueFixture(t, s, task, runner, now)
	verify := domain.PublicRunnerTask(task)
	verify.ID, verify.Provider = "verify-first", "terminal"
	verify.Execution = &domain.NodeTaskExecution{Role: domain.NodeTaskRoleVerification, NodeAttempt: 1, Sequence: 1, PreviousAuthorTaskID: task.ID}
	verify.Invocation.ContributionInputs = []domain.RunnerContributionInput{firstInput}
	freezeNodeQueueTask(t, &verify)
	if err := s.QueueNodeExecutionTask(verify); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.RunState(run.ID)
	if stored.Nodes["author"].ExecutionTaskID != task.ID || stored.Nodes["author"].ExecutionSequence != 1 {
		t.Fatal("verification replaced author")
	}
	second := domain.PublicRunnerTask(task)
	second.ID = "second-author"
	second.Execution.Sequence, second.Execution.PreviousAuthorTaskID = 2, task.ID
	second.Invocation.ContributionInputs = []domain.RunnerContributionInput{firstInput}
	freezeNodeQueueTask(t, &second)
	if err := s.QueueNodeExecutionTask(second); err != nil {
		t.Fatal(err)
	}
	// Once its author is superseded, the queued verifier must not be leased.
	if _, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"terminal"}, now, time.Minute); !errors.Is(err, ErrRunnerTaskLeaseLost) || found {
		t.Fatalf("stale verifier found=%v err=%v", found, err)
	}
	_, secondInput := completeNodeQueueFixture(t, s, second, runner, now)
	third := domain.PublicRunnerTask(second)
	third.ID, third.Execution.Sequence, third.Execution.PreviousAuthorTaskID = "third-author", 3, second.ID
	third.Invocation.ContributionInputs = []domain.RunnerContributionInput{secondInput}
	freezeNodeQueueTask(t, &third)
	if err := s.QueueNodeExecutionTask(third); err != nil {
		t.Fatalf("historical interior should remain valid: %v", err)
	}
	stored, _ = s.RunState(run.ID)
	if stored.Nodes["author"].ExecutionTaskID != third.ID {
		t.Fatal("third author not current")
	}
}

func TestRunnerContributionInputForSessionIsFrozenAndAllowlisted(t *testing.T) {
	s, _, author, runner, now := newNodeQueueFixture(t, true)
	if err := s.QueueNodeExecutionTask(author); err != nil {
		t.Fatal(err)
	}
	_, input := completeNodeQueueFixture(t, s, author, runner, now)
	consumer := domain.PublicRunnerTask(author)
	consumer.ID = "consumer-input"
	consumer.Execution = &domain.NodeTaskExecution{Role: domain.NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: 2, PreviousAuthorTaskID: author.ID}
	consumer.Invocation.ContributionInputs = []domain.RunnerContributionInput{input}
	freezeNodeQueueTask(t, &consumer)
	if err := s.QueueNodeExecutionTask(consumer); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
	if err != nil || !found || claimed.ID != consumer.ID {
		t.Fatalf("claim=%+v found=%v err=%v", claimed, found, err)
	}
	got, content, found, err := s.RunnerContributionInputForSession(consumer.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, "repo", now)
	if err != nil || !found || got.BundleSHA256 != input.BundleSHA256 || string(content) != "bounded provenance fixture for "+author.ID {
		t.Fatalf("input=%+v bytes=%q found=%v err=%v", got, content, found, err)
	}
	if _, _, found, err := s.RunnerContributionInputForSession(consumer.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, "other", now); err != nil || found {
		t.Fatalf("unallowlisted repository exposed: found=%v err=%v", found, err)
	}
	if err := s.DB.Model(&runnerContributionBlobRow{}).Where("sha256 = ?", input.BundleSHA256).Update("content", []byte("corrupt")).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.RunnerContributionInputForSession(consumer.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, "repo", now); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatalf("corrupt input err=%v", err)
	}
}

func TestRunnerContributionInputAuthenticatesBeforeMissingTaskProbe(t *testing.T) {
	s, _, _, runner, now := newNodeQueueFixture(t, true)
	if _, _, found, err := s.RunnerContributionInputForSession("missing-input-task", runner.ID, strings.Repeat("a", 64), 1, "repo", now); !errors.Is(err, ErrRunnerPlacementSessionInvalid) || found {
		t.Fatalf("missing task with invalid session found=%v err=%v", found, err)
	}
}

func TestQueueNodeExecutionTaskRejectsUnacceptedPreviousAndMalformedInput(t *testing.T) {
	for _, mode := range []string{"not-complete", "missing-input", "wrong-receipt", "wrong-head-verification", "missing-bytes", "stale-node-attempt"} {
		t.Run(mode, func(t *testing.T) {
			s, run, task, runner, now := newNodeQueueFixture(t, true)
			if err := s.QueueNodeExecutionTask(task); err != nil {
				t.Fatal(err)
			}
			var input domain.RunnerContributionInput
			if mode != "not-complete" {
				_, input = completeNodeQueueFixture(t, s, task, runner, now)
			}
			next := domain.PublicRunnerTask(task)
			next.ID, next.Execution.Sequence, next.Execution.PreviousAuthorTaskID = "successor", 2, task.ID
			next.Invocation.ContributionInputs = []domain.RunnerContributionInput{input}
			switch mode {
			case "not-complete", "missing-input":
				next.Invocation.ContributionInputs = nil
			case "wrong-receipt":
				next.Invocation.ContributionInputs[0].Sources[0].ReceiptID = "forged"
			case "wrong-head-verification":
				next.Provider = "terminal"
				next.Execution.Role, next.Execution.Sequence = domain.NodeTaskRoleVerification, 1
				next.Invocation.ContributionInputs[0].HeadSHA = strings.Repeat("3", 40)
			case "missing-bytes":
				if err := s.DB.Where("sha256 = ?", input.BundleSHA256).Delete(&runnerContributionBlobRow{}).Error; err != nil {
					t.Fatal(err)
				}
			case "stale-node-attempt":
				run, _ = s.RunState(run.ID)
				run.Nodes["author"].Attempt = 2
				run.Nodes["author"].ExecutionPlacement = nil
				if err := s.PutRun(run); err != nil {
					t.Fatal(err)
				}
			}
			freezeNodeQueueTask(t, &next)
			if err := s.QueueNodeExecutionTask(next); err == nil {
				t.Fatal("invalid successor queued")
			}
			if _, exists := s.RunnerTask(next.ID); exists {
				t.Fatal("rejected successor persisted")
			}
		})
	}
}

func TestQueueNodeExecutionTaskRechecksTimeAfterRegistryLock(t *testing.T) {
	s, run, task, _, now := newNodeQueueFixture(t, false)
	deadline := now.Add(time.Minute)
	run.Nodes["author"].ExecutionDeadline, task.ExecutionDeadline = &deadline, &deadline
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	freezeNodeQueueTask(t, &task)
	// An injected database clock beyond deadline must win over request time.
	original := s.RunnersRepo.DB
	s.RunnersRepo.DB = original.Session(&gorm.Session{NowFunc: func() time.Time { return now.Add(2 * time.Minute) }})
	if err := s.QueueNodeExecutionTask(task); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatalf("post-lock deadline: %v", err)
	}
	if _, exists := s.RunnerTask(task.ID); exists {
		t.Fatal("expired task persisted")
	}
}

func TestPlacedRunSaveCanonicalEmptyCapabilitiesRoundTrip(t *testing.T) {
	s, run, _, runner, _ := newNodeQueueFixture(t, false)
	request, err := domain.CanonicalNodePlacementRequest(domain.NodePlacementRequest{RunnerID: runner.ID})
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext.NodePlacements["author"] = request
	// Canonical [] serializes through omitempty as absence, then reads nil.
	if err := s.PutRun(run); err != nil {
		t.Fatalf("equivalent canonical request rejected: %v", err)
	}
	if err := s.PutRun(run); err != nil {
		t.Fatalf("second canonical save rejected: %v", err)
	}
}

func TestQueueNodeExecutionTaskExternalPredecessorLineage(t *testing.T) {
	for _, mode := range []string{"valid", "missing-code", "unrelated-node", "old-source-attempt", "source-retried-after-queue"} {
		t.Run(mode, func(t *testing.T) {
			s, run, author, runner, now := newNodeQueueFixture(t, true)
			// A fresh Run freezes the complete map before either node executes.
			run.ID = "predecessor-queue"
			run.Revision = 0
			run.WorkflowSnapshot.Nodes = append(run.WorkflowSnapshot.Nodes, domain.WorkflowNode{ID: "approval", Type: "approval"}, domain.WorkflowNode{ID: "consumer", Type: "agent", AgentID: "developer"})
			run.WorkflowSnapshot.Edges = []domain.WorkflowEdge{{Source: "author", Target: "approval", When: "success"}, {Source: "approval", Target: "consumer", When: "success"}}
			if mode == "unrelated-node" {
				run.WorkflowSnapshot.Edges = nil
			}
			run.ExecutionContext.NodePlacements["consumer"] = domain.NodePlacementRequest{RunnerID: runner.ID}
			placement := *author.ExecutionPlacement
			run.Nodes["consumer"] = &domain.NodeRun{NodeID: "consumer", Status: "running", Attempt: 1, ExecutionPlacement: &placement}
			run.Nodes["approval"] = &domain.NodeRun{NodeID: "approval", Status: "succeeded", Attempt: 1}
			author.RunID, author.Invocation.RunID = run.ID, run.ID
			freezeNodeQueueTask(t, &author)
			if err := s.PutRun(run); err != nil {
				t.Fatal(err)
			}
			if err := s.QueueNodeExecutionTask(author); err != nil {
				t.Fatal(err)
			}
			_, input := completeNodeQueueFixture(t, s, author, runner, now)
			run, _ = s.RunState(run.ID)
			run.Nodes["author"].Status = "succeeded"
			if err := s.PutRun(run); err != nil {
				t.Fatal(err)
			}
			child := domain.PublicRunnerTask(author)
			child.ID, child.NodeID, child.Invocation.NodeID = "consumer-task", "consumer", "consumer"
			child.Invocation.ContributionInputs = []domain.RunnerContributionInput{input}
			if mode == "missing-code" {
				child.Invocation.ContributionInputs = nil
			}
			if mode == "old-source-attempt" {
				child.Invocation.ContributionInputs[0].Sources[0].NodeAttempt++
			}
			freezeNodeQueueTask(t, &child)
			err := s.QueueNodeExecutionTask(child)
			if mode != "valid" && mode != "source-retried-after-queue" {
				if err == nil {
					t.Fatal("invalid predecessor accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "source-retried-after-queue" {
				run, _ = s.RunState(run.ID)
				run.Nodes["author"].Status, run.Nodes["author"].Attempt, run.Nodes["author"].ExecutionPlacement = "running", 2, nil
				if err := s.PutRun(run); err != nil {
					t.Fatal(err)
				}
				if _, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute); err == nil || found {
					t.Fatalf("superseded predecessor leaked task: found=%v err=%v", found, err)
				}
				parent, _ := s.RunnerTask(author.ID)
				if len(parent.AcceptedContributions) != 1 || parent.AcceptedContributions[0].ID != input.Sources[0].ReceiptID {
					t.Fatal("source retry modified historical accepted receipt")
				}
			}
		})
	}
}
