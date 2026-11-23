package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
)

type acceptedContributionFixture struct {
	e                  *Engine
	s                  *repo.Store
	root, source, base string
	controller         domain.Run
}

func newAcceptedContributionFixture(t *testing.T) *acceptedContributionFixture {
	t.Helper()
	e, s := controlledTestEngine(t)
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	testGit(t, source, "init", "-q", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(source, "code.txt"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	testGit(t, source, "add", ".")
	testGit(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-qm", "baseline")
	f := &acceptedContributionFixture{e: e, s: s, root: root, source: source, base: testGit(t, source, "rev-parse", "HEAD")}
	if err := s.PutProject(domain.Project{ID: "project", Key: "CONTRIBUTION", Name: "Contribution"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRepository(domain.Repository{ID: "repo", ProjectID: "project", CloneURL: source}); err != nil {
		t.Fatal(err)
	}
	f.controller = f.run("controller")
	if err := s.PutRun(f.controller); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *acceptedContributionFixture) run(id string) domain.Run {
	return domain.Run{ID: id, Status: "succeeded", CreatedAt: time.Now().UTC(),
		ExecutionContext: domain.TelemetryContext{ProjectID: "project", DeliveryPolicy: domain.DeliveryPolicyControlledPR, RunnerWorkspaceLayout: domain.RunnerWorkspaceLayoutAttempt, WorkspaceBaseSHAs: map[string]string{"repo": f.base}},
		Repositories:     []domain.RepositoryContext{{RepositoryID: "repo", CloneURL: f.source, WorkspaceDir: "repo", BaseBranch: "main"}},
		Nodes:            map[string]*domain.NodeRun{"implement": {Status: "succeeded"}},
	}
}

// Use actual independent Git clones and the same stage/complete transaction as
// a Runner. No test writes AcceptedContributions directly into task storage.
func (f *acceptedContributionFixture) accept(t *testing.T, id, path, content string, mutate func(*domain.RunnerTask, *domain.Run)) domain.RunnerTask {
	t.Helper()
	run := f.run("run-" + id)
	task := domain.RunnerTask{ID: id, RunID: run.ID, NodeID: "implement", RunnerID: "runner", Provider: "test-" + id, DeliveryPolicy: domain.DeliveryPolicyControlledPR, MaxAttempts: 1,
		Invocation: &domain.RunnerInvocation{RunID: run.ID, NodeID: "implement", WorkspaceIsolation: &domain.RunnerWorkspaceIsolation{Version: domain.RunnerWorkspaceIsolationVersion}, Repositories: []domain.RunnerRepository{{RepositoryID: "repo", Dir: "repo", CloneURL: f.source, Ref: "main", BaseSHA: f.base}}},
	}
	if mutate != nil {
		mutate(&task, &run)
	}
	contextSHA, err := domain.RunnerContributionContextSHA(task)
	if err != nil {
		t.Fatal(err)
	}
	if task.Invocation.WorkspaceIsolation != nil {
		task.Invocation.WorkspaceIsolation.ContextSHA256 = contextSHA
	}
	task.ContributionSpecs = []domain.RunnerContributionSpec{{RepositoryID: "repo", Dir: task.Invocation.Repositories[0].Dir, BaseSHA: f.base, ContextSHA256: contextSHA, MaxBytes: domain.RunnerContributionMaxBytes}}
	if err := f.s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, found, err := f.s.ClaimRunnerTask("runner", []string{task.Provider}, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim: found=%v err=%v", found, err)
	}
	clone := filepath.Join(f.root, id)
	testGit(t, f.root, "clone", "-q", "--no-hardlinks", f.source, clone)
	if err := os.WriteFile(filepath.Join(clone, path), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	bundle, err := gitwork.ExportContribution(context.Background(), clone, filepath.Join(f.root, id+".bundle"), f.base, "writer "+id, domain.RunnerContributionMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(bundle.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutRunnerContributionBlob(bundle.SHA256, raw); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.s.StageRunnerContribution(domain.RunnerContributionReceipt{TaskID: id, Attempt: claimed.Attempt, RepositoryID: "repo", Dir: task.Invocation.Repositories[0].Dir, BaseSHA: f.base, ResultSHA: bundle.ResultSHA, ContextSHA256: contextSHA, SHA256: bundle.SHA256, Size: bundle.Size}, "runner", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.CompleteRunnerTask(id, "runner", claimed.Attempt, domain.RunnerTaskSuccess, nil, "ok", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{receipt.ID}}, now); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(clone); err != nil {
		t.Fatal(err)
	}
	accepted, _ := f.s.RunnerTask(id)
	return accepted
}

func TestContributionIntegrationControllerUsesAcceptedIndependentBundles(t *testing.T) {
	f := newAcceptedContributionFixture(t)
	left := f.accept(t, "left", "left.py", "left = True\n", nil)
	right := f.accept(t, "right", "right.py", "right = True\n", nil)
	ids := []string{left.ID, right.ID}
	result, err := f.e.IntegrateRunnerContributions(context.Background(), f.controller.ID, "repo", ids)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "prepared" || result.BaseSHA != f.base || result.ResultSHA == f.base || len(result.Inputs) != 2 {
		t.Fatalf("result=%+v", result)
	}
	if err := f.e.ValidateContributionIntegration(result); err != nil {
		t.Fatalf("fresh prepared snapshot cannot be reused: %v", err)
	}
	changed := result
	changed.ResultSHA = f.base
	if err := f.e.ValidateContributionIntegration(changed); err == nil {
		t.Fatal("changed prepared manifest was reusable")
	}
	again, err := f.e.IntegrateRunnerContributions(context.Background(), f.controller.ID, "repo", ids)
	if err != nil || again.ID != result.ID || again.ResultSHA != result.ResultSHA {
		t.Fatalf("replay=%+v err=%v", again, err)
	}
	raw, size, ok := f.s.RunnerContributionBlob(result.BundleSHA256)
	if !ok || size != result.BundleSize {
		t.Fatal("prepared bytes unavailable")
	}
	path := filepath.Join(f.root, "controller.bundle")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(f.root, "controller-checkout")
	if err := gitwork.VerifyContribution(context.Background(), path, destination, gitwork.ContributionBundle{BaseSHA: result.BaseSHA, ResultSHA: result.ResultSHA, SHA256: result.BundleSHA256, Size: result.BundleSize}, 64<<20); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"left.py", "right.py"} {
		if _, err := os.Stat(filepath.Join(destination, file)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.e.IntegrateRunnerContributions(context.Background(), f.controller.ID, "repo", []string{left.ID, left.ID}); err == nil {
		t.Fatal("duplicate task accepted")
	}
}

func TestAcceptedRunnerContributionHandoffValidatesAllowlistedBytesAndLineage(t *testing.T) {
	f := newAcceptedContributionFixture(t)
	task := f.accept(t, "handoff", "safe.py", "safe = True\n", nil)
	sourceRun, _ := f.s.RunState(task.RunID)
	sourceRun.Nodes[task.NodeID].Attempt = 1
	if err := f.s.PutRun(sourceRun); err != nil {
		t.Fatal(err)
	}
	handoff, err := f.e.acceptedRunnerContributionHandoff(f.controller, "repo", []string{task.ID})
	if err != nil || len(handoff) != 1 {
		t.Fatalf("handoff=%+v err=%v", handoff, err)
	}
	got := handoff[0]
	if got.Input.RepositoryID != "repo" || got.Input.BaseSHA != f.base || got.Input.HeadSHA == "" || got.Input.BundleSHA256 == "" || got.Input.BundleSize != int64(len(got.Bytes)) || len(got.Input.Sources) != 1 || got.Source.ReceiptID == "" {
		t.Fatalf("invalid frozen handoff: %+v", got)
	}
	if got.Input.Sources[0].TaskID != task.ID || got.Input.Sources[0].TaskAttempt != task.Attempt || got.Input.Sources[0].ResultSHA != got.Input.HeadSHA {
		t.Fatalf("invalid source lineage: %+v", got.Input.Sources[0])
	}
	if strings.Contains(string(got.Bytes), "git-token") || strings.Contains(string(got.Bytes), "apiKey") {
		t.Fatal("handoff bytes contained credential-like payload")
	}
	// Corruption is rejected before a consumer could receive bytes.
	if err := f.s.DB.Model(&struct{}{}).Table("runner_contribution_blobs").Where("sha256 = ?", got.Input.BundleSHA256).Update("content", []byte("corrupt")).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.acceptedRunnerContributionHandoff(f.controller, "repo", []string{task.ID}); err == nil {
		t.Fatal("corrupt accepted bundle was handed off")
	}
}

func TestAcceptedRunnerContributionHandoffRejectsSupersededSource(t *testing.T) {
	f := newAcceptedContributionFixture(t)
	task := f.accept(t, "handoff-stale", "safe.py", "safe = True\n", nil)
	if err := f.s.PutRunnerTask(domain.RunnerTask{ID: "handoff-newer", RunID: task.RunID, NodeID: task.NodeID, Provider: "terminal", CreatedAt: task.CreatedAt.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.acceptedRunnerContributionHandoff(f.controller, "repo", []string{task.ID}); err == nil {
		t.Fatal("superseded source was handed off")
	}
}

func TestContributionIntegrationRejectsMissingFrozenContext(t *testing.T) {
	f := newAcceptedContributionFixture(t)
	task := f.accept(t, "no-context", "safe.py", "safe = True\n", func(task *domain.RunnerTask, run *domain.Run) { task.Invocation.WorkspaceIsolation = nil })
	if _, err := f.e.IntegrateRunnerContributions(context.Background(), f.controller.ID, "repo", []string{task.ID}); err == nil {
		t.Fatal("accepted contribution without frozen isolation context was integrated")
	}
}

func TestContributionIntegrationRejectsInvalidFrozenRunBindings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*domain.RunnerTask, *domain.Run)
	}{
		{"shared-layout", func(_ *domain.RunnerTask, r *domain.Run) { r.ExecutionContext.RunnerWorkspaceLayout = "shared" }},
		{"wrong-invocation-node", func(task *domain.RunnerTask, _ *domain.Run) { task.Invocation.NodeID = "other" }},
		{"unsupported-isolation-version", func(task *domain.RunnerTask, _ *domain.Run) { task.Invocation.WorkspaceIsolation.Version++ }},
		{"wrong-base-pin", func(_ *domain.RunnerTask, r *domain.Run) {
			r.ExecutionContext.WorkspaceBaseSHAs["repo"] = strings.Repeat("f", 40)
		}},
		{"cross-project", func(_ *domain.RunnerTask, r *domain.Run) { r.ExecutionContext.ProjectID = "other" }},
		{"cancelled-source", func(_ *domain.RunnerTask, r *domain.Run) { r.CancelRequested = true }},
		{"wrong-repository-url", func(task *domain.RunnerTask, _ *domain.Run) {
			task.Invocation.Repositories[0].CloneURL = "https://other.test/repo.git"
		}},
		{"wrong-repository-directory", func(task *domain.RunnerTask, _ *domain.Run) {
			task.Invocation.Repositories[0].Dir = "worker-selected"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAcceptedContributionFixture(t)
			task := f.accept(t, "writer", "safe.py", "safe = True\n", tc.mutate)
			if _, err := f.e.IntegrateRunnerContributions(context.Background(), f.controller.ID, "repo", []string{task.ID}); err == nil {
				t.Fatal("invalid frozen binding accepted")
			}
		})
	}
}

func TestContributionIntegrationRejectsSupersededAndConflictingInputs(t *testing.T) {
	t.Run("superseded", func(t *testing.T) {
		f := newAcceptedContributionFixture(t)
		task := f.accept(t, "writer", "safe.py", "safe = True\n", nil)
		if err := f.s.PutRunnerTask(domain.RunnerTask{ID: "newer", RunID: task.RunID, NodeID: task.NodeID, Provider: "terminal", CreatedAt: task.CreatedAt.Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.e.IntegrateRunnerContributions(context.Background(), f.controller.ID, "repo", []string{task.ID}); err == nil {
			t.Fatal("superseded input accepted")
		}
	})
	t.Run("conflicting", func(t *testing.T) {
		f := newAcceptedContributionFixture(t)
		left := f.accept(t, "left", "code.txt", "left\n", nil)
		right := f.accept(t, "right", "code.txt", "right\n", nil)
		if _, err := f.e.IntegrateRunnerContributions(context.Background(), f.controller.ID, "repo", []string{left.ID, right.ID}); err == nil {
			t.Fatal("conflicting accepted code silently merged")
		}
	})
}
