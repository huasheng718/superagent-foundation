package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

func contributionAPIGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type contributionAPIFixture struct {
	s                  *repo.Store
	h                  http.Handler
	task               domain.RunnerTask
	controller, source domain.Run
}

func newContributionAPIFixture(t *testing.T, missingContext bool) contributionAPIFixture {
	t.Helper()
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "false")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	root := t.TempDir()
	worker := filepath.Join(root, "worker")
	if err := os.Mkdir(worker, 0700); err != nil {
		t.Fatal(err)
	}
	contributionAPIGit(t, worker, "init", "-q", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(worker, "code.txt"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	contributionAPIGit(t, worker, "add", ".")
	contributionAPIGit(t, worker, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-qm", "baseline")
	base := contributionAPIGit(t, worker, "rev-parse", "HEAD")
	if err := s.PutProject(domain.Project{ID: "contribution-project", Key: "CONTRIBUTION", Name: "Contribution"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRepository(domain.Repository{ID: "repo", ProjectID: "contribution-project", CloneURL: worker}); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: "source", Status: "succeeded", CreatedAt: time.Now().UTC(), ExecutionContext: domain.TelemetryContext{ProjectID: "contribution-project", DeliveryPolicy: domain.DeliveryPolicyControlledPR, RunnerWorkspaceLayout: "attempt", WorkspaceBaseSHAs: map[string]string{"repo": base}}, Repositories: []domain.RepositoryContext{{RepositoryID: "repo", CloneURL: worker, WorkspaceDir: "repo", BaseBranch: "main"}}, Nodes: map[string]*domain.NodeRun{"implement": {Status: "succeeded"}}}
	controller := run
	controller.ID = "controller"
	for _, r := range []domain.Run{run, controller} {
		if err := s.PutRun(r); err != nil {
			t.Fatal(err)
		}
	}
	task := domain.RunnerTask{ID: "writer", RunID: run.ID, NodeID: "implement", RunnerID: "runner", Provider: "contribution-api", DeliveryPolicy: domain.DeliveryPolicyControlledPR, MaxAttempts: 1, Invocation: &domain.RunnerInvocation{RunID: run.ID, NodeID: "implement", WorkspaceIsolation: &domain.RunnerWorkspaceIsolation{Version: 1}, Repositories: []domain.RunnerRepository{{RepositoryID: "repo", CloneURL: worker, Dir: "repo", Ref: "main", BaseSHA: base}}}}
	if missingContext {
		task.Invocation.WorkspaceIsolation = nil
	}
	contextSHA, err := domain.RunnerContributionContextSHA(task)
	if err != nil {
		t.Fatal(err)
	}
	if task.Invocation.WorkspaceIsolation != nil {
		task.Invocation.WorkspaceIsolation.ContextSHA256 = contextSHA
	}
	task.ContributionSpecs = []domain.RunnerContributionSpec{{RepositoryID: "repo", Dir: "repo", BaseSHA: base, ContextSHA256: contextSHA, MaxBytes: domain.RunnerContributionMaxBytes}}
	if err := s.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, ok, err := s.ClaimRunnerTask("runner", []string{task.Provider}, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(worker, "result.py"), []byte("result = True\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bundle, err := gitwork.ExportContribution(context.Background(), worker, filepath.Join(root, "worker.bundle"), base, "writer", domain.RunnerContributionMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(bundle.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunnerContributionBlob(bundle.SHA256, raw); err != nil {
		t.Fatal(err)
	}
	staged, err := s.StageRunnerContribution(domain.RunnerContributionReceipt{TaskID: task.ID, Attempt: claimed.Attempt, RepositoryID: "repo", Dir: "repo", BaseSHA: base, ResultSHA: bundle.ResultSHA, ContextSHA256: contextSHA, SHA256: bundle.SHA256, Size: bundle.Size}, "runner", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteRunnerTask(task.ID, "runner", claimed.Attempt, domain.RunnerTaskSuccess, nil, "ok", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{staged.ID}}, now); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(worker); err != nil {
		t.Fatal(err)
	}
	task, _ = s.RunnerTask(task.ID)
	e := engine.New(s, runtime.NewManager(t.TempDir()))
	e.ConfigureExecution("contribution-api", "test", time.Minute, false)
	return contributionAPIFixture{s: s, h: New(s, e, tracker.NewService(), "", ""), task: task, controller: controller, source: run}
}

func (f contributionAPIFixture) prepare(t *testing.T) domain.ContributionIntegration {
	t.Helper()
	return requestJSON[domain.ContributionIntegration](t, f.h, http.MethodPost, "/api/runs/controller/contributions/integrate", map[string]any{"repositoryId": "repo", "taskIds": []string{f.task.ID}}, http.StatusOK)
}

func TestContributionIntegrationHTTPPreparedBundleAndReplay(t *testing.T) {
	f := newContributionAPIFixture(t, false)
	result := f.prepare(t)
	again := f.prepare(t)
	if result.State != "prepared" || again.ID != result.ID {
		t.Fatalf("prepared replay: %+v %+v", result, again)
	}
	path := "/api/runs/controller/contributions/" + result.ID + "/content"
	response := httptest.NewRecorder()
	f.h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("download: %d %s", response.Code, response.Body.String())
	}
	sum := sha256.Sum256(response.Body.Bytes())
	if hex.EncodeToString(sum[:]) != result.BundleSHA256 || response.Header().Get("X-Contribution-Result-SHA") != result.ResultSHA || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("download did not preserve immutable prepared content")
	}
	requestEmptyJSON[map[string]any](t, f.h, http.MethodGet, "/api/runs/source/contributions/"+result.ID+"/content", http.StatusNotFound)
}

func TestContributionIntegrationHTTPRejectsMissingContext(t *testing.T) {
	f := newContributionAPIFixture(t, true)
	requestJSON[map[string]any](t, f.h, http.MethodPost, "/api/runs/controller/contributions/integrate", map[string]any{"repositoryId": "repo", "taskIds": []string{f.task.ID}}, http.StatusConflict)
}

func TestContributionIntegrationHTTPRequiresControllerAdmin(t *testing.T) {
	f := newContributionAPIFixture(t, false)
	result := f.prepare(t)
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_PLATFORM_ADMINS", "controller-admin")
	member := domain.User{ID: 501, LoginName: "contribution-member"}
	admin := domain.User{ID: 502, LoginName: "controller-admin"}
	for _, user := range []domain.User{member, admin} {
		if err := f.s.PutUser(user); err != nil {
			t.Fatal(err)
		}
	}
	body := map[string]any{"repositoryId": "repo", "taskIds": []string{f.task.ID}}
	requestJSON[map[string]any](t, f.h, http.MethodPost, "/api/runs/controller/contributions/integrate", body, http.StatusUnauthorized)
	requestJSONAuthed[map[string]any](t, f.h, member, http.MethodPost, "/api/runs/controller/contributions/integrate", body, http.StatusForbidden)
	requestJSONAuthed[map[string]any](t, f.h, member, http.MethodGet, "/api/runs/controller/contributions/"+result.ID+"/content", nil, http.StatusForbidden)
	accepted := requestJSONAuthed[domain.ContributionIntegration](t, f.h, admin, http.MethodPost, "/api/runs/controller/contributions/integrate", body, http.StatusOK)
	if accepted.ID != result.ID {
		t.Fatal("admin replay changed prepared snapshot")
	}
}

func TestContributionIntegrationHTTPRejectsInvalidatedPreparedSource(t *testing.T) {
	for _, change := range []string{"source-cancelled", "controller-cancelled", "source-superseded", "catalog-project-changed", "source-base-changed"} {
		t.Run(change, func(t *testing.T) {
			f := newContributionAPIFixture(t, false)
			result := f.prepare(t)
			switch change {
			case "source-cancelled":
				f.source.Status = "running"
				if err := f.s.PutRun(f.source); err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.RequestRunCancel(f.source.ID); err != nil {
					t.Fatal(err)
				}
				f.source.Status = "succeeded"
				if err := f.s.PutRun(f.source); err != nil {
					t.Fatal(err)
				}
			case "controller-cancelled":
				f.controller.Status = "running"
				if err := f.s.PutRun(f.controller); err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.RequestRunCancel(f.controller.ID); err != nil {
					t.Fatal(err)
				}
				f.controller.Status = "succeeded"
				if err := f.s.PutRun(f.controller); err != nil {
					t.Fatal(err)
				}
			case "source-superseded":
				if err := f.s.PutRunnerTask(domain.RunnerTask{ID: "replacement", RunID: f.task.RunID, NodeID: f.task.NodeID, Provider: "terminal", CreatedAt: f.task.CreatedAt.Add(time.Second)}); err != nil {
					t.Fatal(err)
				}
			case "catalog-project-changed":
				if err := f.s.PutProject(domain.Project{ID: "different", Key: "DIFFERENT", Name: "Different"}); err != nil {
					t.Fatal(err)
				}
				repository, _ := f.s.Repository("repo")
				repository.ProjectID = "different"
				if err := f.s.PutRepository(repository); err != nil {
					t.Fatal(err)
				}
			case "source-base-changed":
				f.source.ExecutionContext.WorkspaceBaseSHAs = map[string]string{"repo": strings.Repeat("f", 40)}
				if err := f.s.PutRun(f.source); err != nil {
					t.Fatal(err)
				}
			}
			requestEmptyJSON[map[string]any](t, f.h, http.MethodGet, "/api/runs/controller/contributions/"+result.ID+"/content", http.StatusConflict)
		})
	}
}
