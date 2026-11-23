package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/observability"
)

func TestExecutionBudgetStrictStopsBeforeExecution(t *testing.T) {
	e, s := newRecoveryEngine(t)
	marker := filepath.Join(t.TempDir(), "must-not-run")
	putGateTestAgent(t, s, "budget-agent", nil)
	wf := domain.WorkflowDefinition{ID: "strict-budget", Name: "strict", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{{ID: "before", Type: "command", Command: "touch " + marker}, {ID: "agent", Type: "agent", AgentID: "budget-agent"}}, Edges: []domain.WorkflowEdge{{Source: "before", Target: "agent", When: "success"}}}
	if err := s.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	_, err := e.StartWithExecutionContext(wf.ID, "", domain.RepositoryContext{}, domain.TelemetryContext{ExecutionBudget: &domain.ExecutionBudgetPolicy{Mode: "strict", MaxInvocations: 3, MaxTotalTokens: 100}})
	if !errors.Is(err, domain.ErrExecutionBudgetUnsupported) {
		t.Fatalf("strict provider guard: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("admission ran command before rejecting unsupported model cap")
	}
	if len(s.Runs()) != 0 {
		t.Fatal("rejected Run was persisted")
	}
}
func TestExecutionBudgetMeteredMissingUsageFailsAndCannotRetry(t *testing.T) {
	e, s := newRecoveryEngine(t)
	putGateTestAgent(t, s, "budget-agent", nil)
	wf := domain.WorkflowDefinition{ID: "missing-budget-usage", Name: "usage", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{{ID: "agent", Type: "agent", AgentID: "budget-agent", MaxAttempts: 3}}}
	if err := s.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	run, err := e.StartWithExecutionContext(wf.ID, "", domain.RepositoryContext{}, domain.TelemetryContext{ExecutionBudget: &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 3, MaxTotalTokens: 100}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, run.ID, "failed")
	got, _ := s.RunState(run.ID)
	if got.ExecutionBudget.StopReason != "budget_usage_unknown" || got.ExecutionBudget.Invocations != 1 || got.Nodes["agent"].Attempt != 1 {
		t.Fatalf("unknown mock usage did not stop: %+v %+v", got.ExecutionBudget, got.Nodes["agent"])
	}
	if err := e.Retry(run.ID, "agent", "retry", "authorised"); !errors.Is(err, domain.ErrExecutionBudgetStopped) {
		t.Fatalf("retry reset ledger: %v", err)
	}
}
func TestExecutionBudgetConcurrentReservationsAndUsageAuthority(t *testing.T) {
	e, s := newRecoveryEngine(t)
	policy := &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 1, MaxTotalTokens: 10}
	state, _ := domain.NewExecutionBudgetState(policy)
	run := domain.Run{ID: "concurrent-budget", Status: "running", CreatedAt: time.Now().UTC(), ExecutionContext: domain.TelemetryContext{ExecutionBudget: policy}, ExecutionBudget: state, Nodes: map[string]*domain.NodeRun{"a": {NodeID: "a", Status: "running", Attempt: 1}, "b": {NodeID: "b", Status: "running", Attempt: 1}}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, node := range []string{"a", "b"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			results <- e.beginBudgetInvocation(run.ID, n, 1, "invocation-"+n, "codex")
		}(node)
	}
	wg.Wait()
	close(results)
	successful := 0
	for err := range results {
		if err == nil {
			successful++
		} else if !errors.Is(err, domain.ErrExecutionBudgetStopped) {
			t.Fatal(err)
		}
	}
	got, _ := s.RunState(run.ID)
	if successful != 1 || got.ExecutionBudget.Invocations != 1 || got.ExecutionBudget.StopReason != "invocation_budget_exhausted" {
		t.Fatalf("double reservation: %d %+v", successful, got.ExecutionBudget)
	}
	// Agent-controlled Hooks/OTel cannot provide authoritative budget usage.
	policy = &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 2, MaxTotalTokens: 10}
	state, _ = domain.NewExecutionBudgetState(policy)
	_ = state.Begin(policy, "i", "a", "codex", 1)
	got.ExecutionContext.ExecutionBudget = policy
	got.ExecutionBudget = state
	for _, adapter := range []string{observability.AdapterHooks, observability.AdapterLegacyHooks, observability.AdapterOTEL} {
		e.applyObservation(&got, "a", domain.TelemetryEvent{SessionID: "s", Usage: &domain.TokenUsage{TotalTokens: 10}, Source: domain.TelemetrySource{Layer: "executor", Adapter: adapter}})
	}
	if got.ExecutionBudget.ConsumedTokens != 0 {
		t.Fatal("untrusted metrics spent ledger")
	}
	e.applyObservation(&got, "a", domain.TelemetryEvent{SessionID: "s", Usage: &domain.TokenUsage{TotalTokens: 10}, Source: domain.TelemetrySource{Layer: "executor", Adapter: "codex-events-v1"}})
	if got.ExecutionBudget.ConsumedTokens != 10 || got.ExecutionBudget.StopReason != "token_budget_exhausted" {
		t.Fatal("provider usage was not enforced", got.ExecutionBudget)
	}
}
func TestExecutionBudgetNoProgressStopsRealGitGateLoop(t *testing.T) {
	e, s := newRecoveryEngine(t)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.invalid"}} {
		if _, err := gitwork.C(ctx, source, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "code.txt"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "code.txt"}, {"commit", "-qm", "baseline"}} {
		if _, err := gitwork.C(ctx, source, args...); err != nil {
			t.Fatal(err)
		}
	}
	agent := domain.AgentDefinition{ID: "no-progress-agent", Name: "repair", ExecutorID: "mock", Loop: domain.AgentLoopPolicy{Enabled: true, MaxIterations: 5, OnExhausted: "fail"}, Verification: []domain.VerificationGate{{ID: "failing", Name: "unchanged diagnostic", Mode: "command", Required: true, Commands: []string{"false"}, OnFailure: "loop", ExecutionScope: domain.ExecutionScopeRun}}}
	if err := s.PutAgent(agent); err != nil {
		t.Fatal(err)
	}
	wf := domain.WorkflowDefinition{ID: "no-progress", Name: "repair", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{{ID: "repair", Type: "agent", AgentID: agent.ID, MaxAttempts: 3}}}
	if err := s.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	run, err := e.StartWithExecutionContext(wf.ID, "", domain.RepositoryContext{RepositoryID: "repo", Name: "repo", LocalPath: source, BaseBranch: "main"}, domain.TelemetryContext{ExecutionBudget: &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 5, MaxNoProgress: 2}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, run.ID, "failed")
	got, _ := s.RunState(run.ID)
	if got.ExecutionBudget.StopReason != "no_progress" || got.ExecutionBudget.Invocations != 2 || got.ExecutionBudget.Progress["repair"].Consecutive != 2 {
		t.Fatalf("bounded loop did not stop after identical code/diagnostic: %+v node=%+v", got.ExecutionBudget, got.Nodes["repair"])
	}
	if err := e.Retry(run.ID, "repair", "continue", "new prompt"); !errors.Is(err, domain.ErrExecutionBudgetStopped) {
		t.Fatalf("manual retry bypassed no-progress stop: %v", err)
	}
}

func TestExecutionBudgetBlocksContributionPreparationAndRevalidation(t *testing.T) {
	for _, kind := range []string{"source-stopped", "source-uncertain", "controller-stopped", "revoked-after-prepare"} {
		t.Run(kind, func(t *testing.T) {
			f := newAcceptedContributionFixture(t)
			policy := &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 3, MaxNoProgress: 2}
			task := f.accept(t, "budget-source", "change.txt", "code change\n", func(task *domain.RunnerTask, run *domain.Run) {
				run.ExecutionContext.ExecutionBudget = policy
				run.ExecutionBudget, _ = domain.NewExecutionBudgetState(policy)
				if kind == "source-stopped" {
					run.ExecutionBudget.StopReason = "no_progress"
				}
				if kind == "source-uncertain" {
					_ = run.ExecutionBudget.Begin(policy, "unfinished", "implement", "test", 1)
				}
			})
			controller := f.controller
			if kind == "controller-stopped" {
				controller.ID = "stopped-controller"
				controller.ExecutionContext.ExecutionBudget = policy
				controller.ExecutionBudget, _ = domain.NewExecutionBudgetState(policy)
				controller.ExecutionBudget.StopReason = "no_progress"
				if err := f.s.PutRun(controller); err != nil {
					t.Fatal(err)
				}
			}
			prepared, err := f.e.IntegrateRunnerContributions(context.Background(), controller.ID, "repo", []string{task.ID})
			if kind != "revoked-after-prepare" {
				if !errors.Is(err, domain.ErrExecutionBudgetStopped) {
					t.Fatalf("budget state allowed delivery preparation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			run, _ := f.s.RunState(task.RunID)
			run.ExecutionBudget.StopReason = "no_progress"
			if err := f.s.PutRun(run); err != nil {
				t.Fatal(err)
			}
			if err := f.e.ValidateContributionIntegration(prepared); !errors.Is(err, domain.ErrExecutionBudgetStopped) {
				t.Fatalf("previously prepared bundle bypassed later budget stop: %v", err)
			}
		})
	}
}

func TestExecutionBudgetRejectsOpaqueShellSpendBeforeMarker(t *testing.T) {
	for _, mode := range []string{"strict", "metered"} {
		for _, typ := range []string{"command", "script"} {
			t.Run(mode+"-"+typ, func(t *testing.T) {
				e, s := newRecoveryEngine(t)
				marker := filepath.Join(t.TempDir(), "model-launch-marker")
				wf := domain.WorkflowDefinition{ID: "opaque", Name: "opaque", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{{ID: "shell", Type: typ, Command: "touch " + marker, Script: "touch " + marker, ScriptLanguage: "shell"}}}
				if err := s.PutWorkflow(wf); err != nil {
					t.Fatal(err)
				}
				_, err := e.StartWithExecutionContext(wf.ID, "", domain.RepositoryContext{}, domain.TelemetryContext{ExecutionBudget: &domain.ExecutionBudgetPolicy{Mode: mode, MaxInvocations: 2, MaxTotalTokens: 100}})
				if !errors.Is(err, domain.ErrExecutionBudgetUnsupported) {
					t.Fatalf("opaque spend accepted: %v", err)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("opaque shell ran before rejection")
				}
			})
		}
	}
}

func TestExecutionBudgetUsageSurvivesRevisionConflict(t *testing.T) {
	e, s := newRecoveryEngine(t)
	policy := &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 3, MaxTotalTokens: 100}
	state, _ := domain.NewExecutionBudgetState(policy)
	_ = state.Begin(policy, "invocation", "n", "codex", 1)
	run := domain.Run{ID: "budget-race", Status: "running", ExecutionContext: domain.TelemetryContext{ExecutionBudget: policy}, ExecutionBudget: state, Nodes: map[string]*domain.NodeRun{"n": {NodeID: "n", Status: "running", Attempt: 1}}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	e.obsPending = []pendingObservation{{runID: run.ID, nodeID: "n", event: domain.TelemetryEvent{SessionID: "s", Usage: &domain.TokenUsage{TotalTokens: 10}, Source: domain.TelemetrySource{Layer: "executor", Adapter: "codex-events-v1"}}}}
	// Simulate a concurrent transactional Runner/API update between the state
	// read and PutRun. The failed update must not discard its pending usage.
	updated := e.updateRunAt(run.ID, false, func(_ *domain.Run) {
		fresh, _ := s.RunState(run.ID)
		if err := s.PutRun(fresh); err != nil {
			t.Fatal(err)
		}
	})
	if updated || len(e.obsPending) != 1 {
		t.Fatal("conflicting writer dropped pending usage")
	}
	attempts := 0
	err := e.mutateExecutionBudget(run.ID, "n", 1, func(r *domain.Run) error {
		attempts++
		if attempts == 1 {
			fresh, _ := s.RunState(run.ID)
			if err := s.PutRun(fresh); err != nil {
				t.Fatal(err)
			}
		}
		return r.ExecutionBudget.Finish(policy, "invocation")
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.RunState(run.ID)
	if attempts != 2 || got.ExecutionBudget.ConsumedTokens != 10 || !got.ExecutionBudget.Receipts["invocation"].Finished || len(e.obsPending) != 0 {
		t.Fatalf("usage conflict retry double-counted/lost accounting: attempts=%d %+v pending=%d", attempts, got.ExecutionBudget, len(e.obsPending))
	}
}

func TestExecutionBudgetNoProgressIgnoresAlternatingVerifierErrors(t *testing.T) {
	e, _ := newRecoveryEngine(t)
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "repo")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.invalid"}} {
		if _, err := gitwork.C(ctx, repository, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "code.txt"), []byte("unchanged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "code.txt"}, {"commit", "-qm", "baseline"}} {
		if _, err := gitwork.C(ctx, repository, args...); err != nil {
			t.Fatal(err)
		}
	}
	run := domain.Run{Repositories: []domain.RepositoryContext{{RepositoryID: "repo", WorkspaceDir: "repo"}}}
	a, err := e.progressFingerprint(ctx, run, "repair", workspace, errors.New("verification lint: exit status 1 (random text)"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.progressFingerprint(ctx, run, "repair", workspace, errors.New("Runner verification disconnected: exit status 2 (different random text)"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("unchanged product tree acquired progress by changing verifier errors")
	}
	policy, state := budgetFixtureEngine(t)
	if err := state.RecordProgress(policy, "repair", a, true); err != nil {
		t.Fatal(err)
	}
	if err := state.RecordProgress(policy, "repair", b, true); !errors.Is(err, domain.ErrExecutionBudgetStopped) || state.StopReason != "no_progress" {
		t.Fatal("alternating verifier diagnostics reset the Loop", err)
	}
	if err := os.WriteFile(filepath.Join(repository, "code.txt"), []byte("actual code change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := e.progressFingerprint(ctx, run, "repair", workspace, errors.New("verification lint: exit status 1"))
	if err != nil || changed == a {
		t.Fatal("actual product progress ignored", err)
	}
}
func budgetFixtureEngine(t *testing.T) (*domain.ExecutionBudgetPolicy, *domain.ExecutionBudgetState) {
	t.Helper()
	policy := &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 5, MaxNoProgress: 2}
	state, err := domain.NewExecutionBudgetState(policy)
	if err != nil {
		t.Fatal(err)
	}
	return policy, state
}
