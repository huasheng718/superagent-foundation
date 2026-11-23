package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

// TestPlacedSequentialTwoRepositoryInputsPreserveEarlierHead exercises the
// complete controller-side lineage used by a placed sequential consumer. A
// second author changes repository B while importing the accepted snapshot for
// both repositories; a later verification task must therefore see A's earlier
// head and B's new head together.
func TestPlacedSequentialTwoRepositoryInputsPreserveEarlierHead(t *testing.T) {
	e, s := controlledTestEngine(t)
	now := time.Now().UTC()
	sessionHash := strings.Repeat("a", 64)
	registration, err := domain.RunnerRegistrationSHA256(sessionHash)
	if err != nil {
		t.Fatal(err)
	}
	expires := now.Add(time.Hour)
	runner := domain.Runner{ID: "fanout-runner", OwnerUserID: 42, OS: "darwin", Arch: "arm64", Status: domain.RunnerStatusOnline,
		RunnerVersion: domain.RunnerVersionNodePlacement, SessionTokenHash: sessionHash,
		SessionExpiresAt: &expires, Runtimes: []domain.RunnerRuntime{{Provider: "terminal", Status: domain.RuntimeStatusOnline, BinaryPath: "(builtin)", Capabilities: []string{"terminal"}, AuthMode: domain.RuntimeAuthLocal}}}
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	placement := domain.NodeExecutionPlacement{Version: domain.NodeExecutionPlacementVersion, NodeAttempt: 1,
		ResponsibleUserID: 42, ProjectID: "fanout-project", RunnerID: runner.ID,
		RegistrationSHA256: registration, Access: domain.NodePlacementAccessPersonal,
		OS: "darwin", Arch: "arm64", RunnerVersion: domain.RunnerVersionNodePlacement,
		Runtimes: []domain.NodeRuntimeSnapshot{{Provider: "terminal", BinaryPath: "(builtin)",
			Capabilities: []string{"terminal"}, AuthMode: domain.RuntimeAuthLocal}}}
	placement, err = domain.CanonicalNodeExecutionPlacement(placement)
	if err != nil {
		t.Fatal(err)
	}
	placement.CapabilitySHA256, err = domain.NodeExecutionPlacementCapabilitySHA256(placement)
	if err != nil {
		t.Fatal(err)
	}
	baseA, baseB := strings.Repeat("1", 40), strings.Repeat("2", 40)
	run := domain.Run{ID: "fanout-run", Status: "running", WorkflowSnapshot: domain.WorkflowDefinition{
		Nodes: []domain.WorkflowNode{{ID: "author", Type: "script"}},
	}, ExecutionContext: domain.TelemetryContext{ProjectID: "fanout-project", ResponsibleUserID: 42,
		DeliveryPolicy: domain.DeliveryPolicyControlledPR, RunnerWorkspaceLayout: domain.RunnerWorkspaceLayoutAttempt,
		NodePlacements:    map[string]domain.NodePlacementRequest{"author": {RunnerID: runner.ID}},
		WorkspaceBaseSHAs: map[string]string{"repo-a": baseA, "repo-b": baseB}},
		Repositories: []domain.RepositoryContext{{RepositoryID: "repo-a", CloneURL: "https://example.invalid/a.git", WorkspaceDir: "a"},
			{RepositoryID: "repo-b", CloneURL: "https://example.invalid/b.git", WorkspaceDir: "b"}},
		Nodes: map[string]*domain.NodeRun{"author": {NodeID: "author", Status: "running", Attempt: 1, ExecutionPlacement: &placement}}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	newTask := func(id string, sequence int, previous string) domain.RunnerTask {
		return domain.RunnerTask{ID: id, RunID: run.ID, NodeID: "author", RunnerID: runner.ID,
			Provider: "terminal", Status: domain.RunnerTaskQueued, MaxAttempts: 1,
			ExecutionPlacement: &placement, Execution: &domain.NodeTaskExecution{Role: domain.NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: sequence, PreviousAuthorTaskID: previous},
			Invocation: &domain.RunnerInvocation{RunID: run.ID, NodeID: "author", WorkspaceIsolation: &domain.RunnerWorkspaceIsolation{Version: domain.RunnerWorkspaceIsolationVersion},
				Repositories: []domain.RunnerRepository{{RepositoryID: "repo-a", Dir: "a", CloneURL: "https://example.invalid/a.git", BaseSHA: baseA},
					{RepositoryID: "repo-b", Dir: "b", CloneURL: "https://example.invalid/b.git", BaseSHA: baseB}}}}
	}
	prepare := func(task *domain.RunnerTask) {
		t.Helper()
		if err := e.applyRunnerTaskPolicy(run, task); err != nil {
			t.Fatal(err)
		}
	}
	first := newTask("author-1", 1, "")
	prepare(&first)
	if err := s.QueueNodeExecutionTask(first); err != nil {
		t.Fatal(err)
	}
	first, _ = s.RunnerTask(first.ID)
	claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, sessionHash, []string{"terminal"}, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim first: found=%v err=%v", found, err)
	}
	if claimed.Attempt != 1 {
		t.Fatalf("first attempt=%d", claimed.Attempt)
	}
	accepted := map[string]domain.RunnerContributionReceipt{}
	for _, spec := range first.ContributionSpecs {
		raw := []byte("accepted-" + spec.RepositoryID)
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		if err := s.PutRunnerContributionBlob(digest, raw); err != nil {
			t.Fatal(err)
		}
		head := strings.Repeat("a", 40)
		if spec.RepositoryID == "repo-b" {
			head = strings.Repeat("b", 40)
		}
		receipt, err := s.StageRunnerContributionForSession(domain.RunnerContributionReceipt{TaskID: first.ID, Attempt: claimed.Attempt,
			RepositoryID: spec.RepositoryID, Dir: spec.Dir, BaseSHA: spec.BaseSHA, ResultSHA: head,
			ContextSHA256: spec.ContextSHA256, SHA256: digest, Size: int64(len(raw))}, runner.ID, sessionHash, now)
		if err != nil {
			t.Fatal(err)
		}
		accepted[spec.RepositoryID] = receipt
	}
	if err := s.CompleteRunnerTaskForSession(first.ID, runner.ID, sessionHash, claimed.Attempt, domain.RunnerTaskSuccess, nil, "", "", nil,
		&domain.RunnerTaskOutputs{ContributionReceipts: []string{accepted["repo-a"].ID, accepted["repo-b"].ID}}, now); err != nil {
		t.Fatal(err)
	}
	current, ok := s.RunState(run.ID)
	if !ok {
		t.Fatal("run disappeared after first author")
	}
	second := newTask("author-2", 0, "")
	if err := e.preparePlacedExecutionTask(current, &second, domain.NodeTaskRoleAuthor); err != nil {
		t.Fatal(err)
	}
	prepare(&second)
	if len(second.Invocation.ContributionInputs) != 2 {
		t.Fatalf("second inputs=%+v", second.Invocation.ContributionInputs)
	}
	for _, input := range second.Invocation.ContributionInputs {
		if len(input.Sources) != 1 || input.Sources[0].TaskID != first.ID {
			t.Fatalf("second source lineage=%+v", input)
		}
		if input.HeadSHA != accepted[input.RepositoryID].ResultSHA {
			t.Fatalf("second input %s head=%s want=%s", input.RepositoryID, input.HeadSHA, accepted[input.RepositoryID].ResultSHA)
		}
	}
	if err := s.QueueNodeExecutionTask(second); err != nil {
		t.Fatal(err)
	}
	if _, found := s.RunnerTask(second.ID); !found {
		t.Fatal("second task was not persisted")
	}
	// The verification task reads the second author's accepted receipts for
	// both repositories. This is the final-head assertion: repo-a survives the
	// repo-b change instead of being silently reset to its original base.
	second, _ = s.RunnerTask(second.ID)
	claimed, found, err = s.ClaimRunnerTaskForSession(runner.ID, sessionHash, []string{"terminal"}, now, time.Minute)
	if err != nil || !found || claimed.ID != second.ID {
		t.Fatalf("claim second: task=%s found=%v err=%v", claimed.ID, found, err)
	}
	secondReceiptIDs := make([]string, 0, len(second.ContributionSpecs))
	for _, spec := range second.ContributionSpecs {
		raw := []byte("second-" + spec.RepositoryID)
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		if err := s.PutRunnerContributionBlob(digest, raw); err != nil {
			t.Fatal(err)
		}
		head := accepted[spec.RepositoryID].ResultSHA
		if spec.RepositoryID == "repo-b" {
			head = strings.Repeat("c", 40)
		}
		receipt, err := s.StageRunnerContributionForSession(domain.RunnerContributionReceipt{TaskID: second.ID, Attempt: claimed.Attempt,
			RepositoryID: spec.RepositoryID, Dir: spec.Dir, BaseSHA: spec.BaseSHA, ResultSHA: head,
			ContextSHA256: spec.ContextSHA256, SHA256: digest, Size: int64(len(raw))}, runner.ID, sessionHash, now)
		if err != nil {
			t.Fatal(err)
		}
		secondReceiptIDs = append(secondReceiptIDs, receipt.ID)
	}
	if err := s.CompleteRunnerTaskForSession(second.ID, runner.ID, sessionHash, claimed.Attempt, domain.RunnerTaskSuccess, nil, "", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: secondReceiptIDs}, now); err != nil {
		t.Fatal(err)
	}
	current, _ = s.RunState(run.ID)
	verification := newTask("verify-final", 0, "")
	if err := e.preparePlacedExecutionTask(current, &verification, domain.NodeTaskRoleVerification); err != nil {
		t.Fatal(err)
	}
	prepare(&verification)
	if len(verification.Invocation.ContributionInputs) != 2 {
		t.Fatalf("verification inputs=%+v", verification.Invocation.ContributionInputs)
	}
	for _, input := range verification.Invocation.ContributionInputs {
		want := strings.Repeat("c", 40)
		if input.RepositoryID == "repo-a" {
			want = accepted["repo-a"].ResultSHA
		}
		if input.HeadSHA != want || len(input.Sources) != 1 || input.Sources[0].TaskID != second.ID {
			t.Fatalf("final %s head=%s sources=%+v want=%s", input.RepositoryID, input.HeadSHA, input.Sources, want)
		}
	}
}
