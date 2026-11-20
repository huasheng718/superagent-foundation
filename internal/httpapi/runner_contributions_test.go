package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

type contributionReadHook struct {
	io.Reader
	once func()
}

func (r *contributionReadHook) Read(p []byte) (int, error) {
	if r.once != nil {
		f := r.once
		r.once = nil
		f()
	}
	return r.Reader.Read(p)
}

func TestRunnerContributionHTTP(t *testing.T) {
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, engine.New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
	runnerID, session := registerTestRunner(t, h)
	_, otherSession := registerTestRunner(t, h)
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "code.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "code.txt")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "code.txt"), []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := gitwork.ExportContribution(context.Background(), root, filepath.Join(t.TempDir(), "source.bundle"), base, "fixture result", domain.RunnerContributionMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(bundle.Path)
	if err != nil {
		t.Fatal(err)
	}
	spec := domain.RunnerContributionSpec{RepositoryID: "repo", BaseSHA: base, ContextSHA256: strings.Repeat("a", 64), MaxBytes: domain.RunnerContributionMaxBytes}
	claim := func(id string, limit int64) domain.RunnerTask {
		t.Helper()
		frozen := spec
		frozen.MaxBytes = limit
		if err := s.PutRunnerTask(domain.RunnerTask{ID: id, RunID: "run-" + id, NodeID: "fix", Provider: "mock", Status: domain.RunnerTaskQueued, MaxAttempts: 3, ContributionSpecs: []domain.RunnerContributionSpec{frozen}, Invocation: &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{{RepositoryID: spec.RepositoryID, Dir: spec.Dir, BaseSHA: spec.BaseSHA}}}}); err != nil {
			t.Fatal(err)
		}
		task, ok, err := s.ClaimRunnerTask(runnerID, []string{"mock"}, time.Now().UTC(), time.Minute)
		if err != nil || !ok || task.ID != id {
			t.Fatalf("claim %s: %+v %v", id, task, err)
		}
		return task
	}
	upload := func(task domain.RunnerTask, token string, body io.Reader, configure func(*http.Request)) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/runners/tasks/"+task.ID+"/contributions/repo/content?attempt="+jsonNumber(task.Attempt), body)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-git-bundle")
		req.Header.Set("X-Contribution-Base-SHA", base)
		req.Header.Set("X-Contribution-Result-SHA", bundle.ResultSHA)
		req.Header.Set("X-Contribution-Context-SHA256", spec.ContextSHA256)
		if configure != nil {
			configure(req)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	task := claim("contribution-good", domain.RunnerContributionMaxBytes)
	var receipt domain.RunnerContributionReceipt
	for i := 0; i < 2; i++ {
		rec := upload(task, session, bytes.NewReader(content), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("upload %d: %d %s", i, rec.Code, rec.Body)
		}
		var current domain.RunnerContributionReceipt
		if err := json.Unmarshal(rec.Body.Bytes(), &current); err != nil {
			t.Fatal(err)
		}
		if current.ID == "" || current.SHA256 != bundle.SHA256 || current.Size != bundle.Size || !current.AcceptedAt.IsZero() || (i > 0 && current.ID != receipt.ID) {
			t.Fatalf("invalid/non-idempotent staged receipt: %+v", current)
		}
		receipt = current
	}
	staged, _ := s.RunnerTask(task.ID)
	if staged.Status != domain.RunnerTaskLeased || len(staged.PendingContributions) != 1 || len(staged.AcceptedContributions) != 0 {
		t.Fatal("upload accepted contribution before terminal success")
	}
	missing := requestWithHeaders(t, h, http.MethodPost, "/runners/tasks/"+task.ID+"/complete", map[string]any{"attempt": task.Attempt, "status": domain.RunnerTaskSuccess}, map[string]string{"Authorization": "Bearer " + session})
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing contribution receipt must reject completion: %d %s", missing.Code, missing.Body)
	}
	if err := s.CompleteRunnerTask(task.ID, runnerID, task.Attempt, domain.RunnerTaskSuccess, nil, "done", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{receipt.ID}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	accepted, _ := s.RunnerTask(task.ID)
	if len(accepted.AcceptedContributions) != 1 || accepted.AcceptedContributions[0].ID != receipt.ID {
		t.Fatal("completion did not atomically accept staged receipt")
	}

	for _, tc := range []struct {
		name      string
		token     string
		want      int
		configure func(*http.Request)
	}{
		{"unauthenticated", "", http.StatusUnauthorized, nil},
		{"wrong-runner", otherSession, http.StatusConflict, nil},
		{"stale-attempt", session, http.StatusConflict, func(r *http.Request) { r.URL.RawQuery = "attempt=2" }},
		{"missing-attempt", session, http.StatusBadRequest, func(r *http.Request) { r.URL.RawQuery = "" }},
		{"unknown-repository", session, http.StatusBadRequest, func(r *http.Request) { r.URL.Path = strings.Replace(r.URL.Path, "/repo/", "/unknown/", 1) }},
		{"wrong-context", session, http.StatusBadRequest, func(r *http.Request) { r.Header.Set("X-Contribution-Context-SHA256", strings.Repeat("b", 64)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := claim("contribution-"+tc.name, domain.RunnerContributionMaxBytes)
			body := &contributionReadHook{Reader: bytes.NewReader(content), once: func() { t.Error("rejected request body was read") }}
			rec := upload(task, tc.token, body, tc.configure)
			if rec.Code != tc.want {
				t.Fatalf("got %d want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
	t.Run("chunked-limit", func(t *testing.T) {
		task := claim("contribution-limit", int64(len(content)-1))
		rec := upload(task, session, &contributionReadHook{Reader: bytes.NewReader(content)}, nil)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
	})
	t.Run("malformed-bundle", func(t *testing.T) {
		task := claim("contribution-malformed", domain.RunnerContributionMaxBytes)
		rec := upload(task, session, strings.NewReader("not a Git bundle"), nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
	})
	t.Run("cancel-during-body", func(t *testing.T) {
		task := claim("contribution-cancel", domain.RunnerContributionMaxBytes)
		body := &contributionReadHook{Reader: bytes.NewReader(content), once: func() {
			if err := s.CancelRunnerTask(task.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		}}
		rec := upload(task, session, body, nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
		stored, _ := s.RunnerTask(task.ID)
		if len(stored.PendingContributions) != 0 || len(stored.AcceptedContributions) != 0 {
			t.Fatal("canceled in-flight upload produced receipt")
		}
	})
	for _, reclaim := range []bool{false, true} {
		name := "natural-expiry-during-body"
		if reclaim {
			name = "reclaim-during-body"
		}
		t.Run(name, func(t *testing.T) {
			task := claim("contribution-"+name, domain.RunnerContributionMaxBytes)
			if _, err := s.RenewRunnerTaskLease(task.ID, runnerID, task.Attempt, 100*time.Millisecond, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			short, _ := s.RunnerTask(task.ID)
			body := &contributionReadHook{Reader: bytes.NewReader(content), once: func() {
				time.Sleep(time.Until(*short.LeaseUntil) + time.Millisecond)
				if reclaim {
					if _, err := s.ExpireRunnerTasks(time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					current, ok, err := s.ClaimRunnerTask(runnerID, []string{"mock"}, time.Now().UTC(), time.Minute)
					if err != nil || !ok || current.ID != task.ID || current.Attempt != task.Attempt+1 {
						t.Fatalf("same-Runner reclaim: %+v %v", current, err)
					}
				}
			}}
			rec := upload(task, session, body, nil)
			if rec.Code != http.StatusConflict {
				t.Fatalf("got %d: %s", rec.Code, rec.Body)
			}
			stored, _ := s.RunnerTask(task.ID)
			if len(stored.PendingContributions) != 0 || len(stored.AcceptedContributions) != 0 || (reclaim && stored.Status != domain.RunnerTaskLeased) {
				t.Fatal("expired upload produced receipt or mutated reclaimed task")
			}
			if !reclaim {
				if err := s.CancelRunnerTask(task.ID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	for _, change := range []string{"rotate", "expire"} {
		t.Run("session-"+change+"-during-body", func(t *testing.T) {
			original, _ := s.Runner(runnerID)
			defer func() {
				if err := s.PutRunner(original); err != nil {
					t.Error(err)
				}
			}()
			task := claim("contribution-session-"+change, domain.RunnerContributionMaxBytes)
			body := &contributionReadHook{Reader: bytes.NewReader(content), once: func() {
				changed := original
				if change == "rotate" {
					changed.SessionTokenHash = strings.Repeat("c", 64)
				} else {
					expired := time.Now().UTC().Add(-time.Second)
					changed.SessionExpiresAt = &expired
				}
				if err := s.PutRunner(changed); err != nil {
					t.Fatal(err)
				}
			}}
			rec := upload(task, session, body, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("session changed during upload: %d %s", rec.Code, rec.Body)
			}
			stored, _ := s.RunnerTask(task.ID)
			if len(stored.PendingContributions) != 0 || len(stored.AcceptedContributions) != 0 || stored.Attempt != task.Attempt || stored.Status != domain.RunnerTaskLeased {
				t.Fatal("old session staged a receipt or mutated task")
			}
		})
	}

}
