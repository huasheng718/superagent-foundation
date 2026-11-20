package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
)

func controlledTestEngine(t *testing.T) (*Engine, *repo.Store) {
	t.Helper()
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := New(s, runtime.NewManager(t.TempDir()))
	e.ConfigureExecution("policy-test", "policy-host", time.Minute, false)
	return e, s
}

func TestDeliveryPolicyFreezesRunAndPreventsChildDowngrade(t *testing.T) {
	e, s := controlledTestEngine(t)
	if e.DeliveryPolicy() != domain.DeliveryPolicyControlledPR {
		t.Fatal("new engine is not controlled")
	}
	if err := e.ConfigureDeliveryPolicy("invalid"); err == nil || e.DeliveryPolicy() != domain.DeliveryPolicyControlledPR {
		t.Fatal("invalid configuration accepted or changed policy")
	}
	wf := domain.WorkflowDefinition{ID: "policy-flow", Name: "Policy", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{{ID: "wait", Name: "Wait", Type: "approval", MaxAttempts: 1}}}
	if err := s.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	start := func(ctx domain.TelemetryContext) domain.Run {
		t.Helper()
		run, err := e.StartWithRepositoriesContext(wf.ID, "test", nil, ctx)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	parent := start(domain.TelemetryContext{DeliveryPolicy: domain.DeliveryPolicyLegacy})
	if parent.ExecutionContext.DeliveryPolicy != domain.DeliveryPolicyControlledPR {
		t.Fatal("caller downgraded deployment policy")
	}
	if err := e.ConfigureDeliveryPolicy(domain.DeliveryPolicyLegacy); err != nil {
		t.Fatal(err)
	}
	child := start(domain.TelemetryContext{ParentRunID: parent.ID, DeliveryPolicy: domain.DeliveryPolicyLegacy})
	if child.ExecutionContext.DeliveryPolicy != domain.DeliveryPolicyControlledPR || !e.controlledRun(child) {
		t.Fatal("legacy deployment downgraded controlled parent")
	}
	legacy := start(domain.TelemetryContext{DeliveryPolicy: domain.DeliveryPolicyControlledPR})
	if legacy.ExecutionContext.DeliveryPolicy != domain.DeliveryPolicyLegacy || e.controlledRun(legacy) {
		t.Fatal("explicit deployment legacy compatibility missing")
	}
	if err := e.ConfigureDeliveryPolicy(""); err != nil {
		t.Fatal(err)
	}
	strictChild := start(domain.TelemetryContext{ParentRunID: legacy.ID})
	if strictChild.ExecutionContext.DeliveryPolicy != domain.DeliveryPolicyControlledPR || !e.controlledRun(legacy) {
		t.Fatal("deployment cannot strengthen legacy run")
	}
}

func TestControlledDeliveryWithholdsExecutionAuthorityAndPersistsPolicy(t *testing.T) {
	e, s := controlledTestEngine(t)
	if err := s.PutUser(domain.User{ID: 71, LoginName: "policy-user", GitToken: "hosting-token", CodexAPIKey: "model-key"}); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: "policy-run", ExecutionContext: domain.TelemetryContext{ResponsibleUserID: 71, DeliveryPolicy: domain.DeliveryPolicyLegacy}}
	credentials := e.resolveRunCredentials(run)
	if credentials.GitToken != "" || len(credentials.MCPServers) != 0 || credentials.Codex.APIKey != "model-key" {
		t.Fatal("controlled execution credentials violate policy")
	}
	task := domain.RunnerTask{ID: "policy-task", Provider: "terminal", Status: domain.RunnerTaskQueued, Invocation: &domain.RunnerInvocation{Repository: &domain.RunnerRepository{GitToken: "hosting-token"}, MCPCallServers: []domain.MCPServerDefinition{{ID: "privileged"}}}}
	if err := e.applyRunnerTaskPolicy(run, &task); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	stored, ok := s.RunnerTask(task.ID)
	if !ok || stored.DeliveryPolicy != domain.DeliveryPolicyControlledPR || domain.ValidateRunnerDelivery(stored) != nil {
		t.Fatalf("persisted task is unsafe: %+v", stored)
	}
	if err := e.ConfigureDeliveryPolicy(domain.DeliveryPolicyLegacy); err != nil {
		t.Fatal(err)
	}
	if credentials := e.resolveRunCredentials(run); credentials.GitToken != "hosting-token" {
		t.Fatal("explicit legacy no longer binds hosting token")
	}
	run.ExecutionContext.DeliveryPolicy = ""
	if credentials := e.resolveRunCredentials(run); credentials.GitToken != "" {
		t.Fatal("missing persisted policy inferred legacy")
	}
}

func TestControlledRunSuccessDoesNotAcceptIssue(t *testing.T) {
	e, s := controlledTestEngine(t)
	issue := domain.Issue{ID: "policy-issue", ProjectID: "p", Title: "Await human", Status: "todo", Kind: "requirement", AutomationState: "queued"}
	if err := s.PutIssue(issue); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Issue(issue.ID)
	run := domain.Run{ID: "controlled-success", Status: "running", ExecutionContext: domain.TelemetryContext{IssueID: issue.ID}, Nodes: map[string]*domain.NodeRun{}, CreatedAt: time.Now().UTC()}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	e.finish(run.ID, "succeeded", "execution complete")
	after, _ := s.Issue(issue.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("execution success changed business issue: before=%+v after=%+v", before, after)
	}
	finished, _ := s.RunState(run.ID)
	if finished.Status != "succeeded" {
		t.Fatalf("execution did not succeed: %s", finished.Status)
	}
}

func TestControlledDeliveryNeverCallsMergeOrPushFallback(t *testing.T) {
	e, s := controlledTestEngine(t)
	called := false
	e.MergeIntoBranch = func(string, string, string, string, string) (string, error) { called = true; return "", nil }
	run := domain.Run{ID: "no-publication", Status: "running", ExecutionContext: domain.TelemetryContext{ScheduleJobID: "schedule", DeliveryPolicy: domain.DeliveryPolicyLegacy}, Nodes: map[string]*domain.NodeRun{}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	for _, rc := range []domain.RepositoryContext{{GongfengProjectID: "project"}, {}} {
		if err := e.mergeWorkBranchIntoIntegrate(run.ID, run, rc, "work", "main", func() error { called = true; return nil }); !errors.Is(err, ErrControlledDelivery) {
			t.Fatalf("merge/fallback error=%v", err)
		}
	}
	if _, err := e.integrateScheduledRun(run.ID); !errors.Is(err, ErrControlledDelivery) {
		t.Fatalf("scheduled integration error=%v", err)
	}
	if _, err := e.integrateOnRunner(context.Background(), domain.Runner{}, run, domain.RepositoryContext{}, true); !errors.Is(err, ErrControlledDelivery) {
		t.Fatalf("runner integration error=%v", err)
	}
	if err := e.AlignBaseline(domain.RepositoryContext{CloneURL: "https://example.com/repo.git", DefaultBranch: "main", IntegrateBranch: "integration"}, "token"); !errors.Is(err, ErrControlledDelivery) {
		t.Fatalf("baseline alignment error=%v", err)
	}
	if _, err := e.PushConversationWorkspace(context.Background(), run.ID, "token", "", "", "", "main"); !errors.Is(err, ErrControlledDelivery) {
		t.Fatalf("conversation push error=%v", err)
	}
	if called {
		t.Fatal("controlled delivery invoked publication callback")
	}
}

func TestControlledCheckoutDoesNotInitializeRemoteBranches(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(map[bool]string{true: "empty remote", false: "missing integrate branch"}[empty], func(t *testing.T) {
			e, _ := controlledTestEngine(t)
			root := t.TempDir()
			remote := filepath.Join(root, "remote.git")
			testGit(t, root, "init", "--bare", "--initial-branch=main", remote)
			if !empty {
				source := filepath.Join(root, "source")
				testGit(t, root, "clone", remote, source)
				if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("baseline"), 0o600); err != nil {
					t.Fatal(err)
				}
				testGit(t, source, "add", ".")
				testGit(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "baseline")
				testGit(t, source, "push", "origin", "main")
			}
			before := testGit(t, root, "ls-remote", "--heads", remote)
			ctx := e.deliveryContext(context.Background(), domain.Run{})
			err := prepareRepository(ctx, filepath.Join(root, "workspace"), domain.RepositoryContext{RepositoryID: "repo", CloneURL: remote, DefaultBranch: "main", BaseBranch: "integration", IntegrateBranch: "integration", WorkBranch: "work"})
			if err == nil {
				t.Fatal("controlled checkout unexpectedly initialized missing remote branch")
			}
			if after := testGit(t, root, "ls-remote", "--heads", remote); after != before {
				t.Fatalf("controlled preparation mutated remote: before=%q after=%q", before, after)
			}
		})
	}
}

func TestControlledDeliveryRejectsEmbeddedCheckoutCredentials(t *testing.T) {
	e, _ := controlledTestEngine(t)
	run := domain.Run{Repository: domain.RepositoryContext{RepositoryID: "repo", CloneURL: "https://user:secret@example.com/repo.git"}}
	if err := e.validateDeliveryRepositories(run); err == nil {
		t.Fatal("controller checkout accepted embedded authority")
	}
	if err := e.ConfigureDeliveryPolicy(domain.DeliveryPolicyLegacy); err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext.DeliveryPolicy = domain.DeliveryPolicyLegacy
	if err := e.validateDeliveryRepositories(run); err != nil {
		t.Fatal(err)
	}
}
