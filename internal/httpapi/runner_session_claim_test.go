package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
	"gorm.io/gorm"
)

func TestRunnerClaimLongPollRejectsSessionChangedAfterAuthentication(t *testing.T) {
	for _, path := range []string{"/api/runners/tasks/claim", "/runners/tasks/claim"} {
		for _, preflight := range []bool{false, true} {
			for _, change := range []string{"rotate", "expire"} {
				name := path + "/" + change
				if preflight {
					name += "/preflight"
				}
				t.Run(name, func(t *testing.T) {
					t.Setenv("AGENTFLOW_RUNNER_LONG_POLL_TIMEOUT", "2s")
					t.Setenv("AGENTFLOW_RUNNER_DB_POLL_INTERVAL", "20ms")
					s, err := repo.OpenTest()
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(s.Close)
					e := engine.New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces")))
					h := New(s, e, tracker.NewService(), "", "")
					runnerID, session := registerTestRunner(t, h)
					old, _ := s.Runner(runnerID)
					// Observe both empty candidate checks; the request has already
					// authenticated and registered its waiter before the second.
					waiting := make(chan struct{})
					var checks atomic.Int32
					callbackName := "session-claim-test"
					if err := s.DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
						if tx.Statement.Table == "runner_tasks" && checks.Add(1) == 2 {
							close(waiting)
						}
					}); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { s.DB.Callback().Query().Remove(callbackName) })
					auth := map[string]string{"Authorization": "Bearer " + session, "X-Runner-Task-Protocol": "2"}
					done := make(chan *httptest.ResponseRecorder, 1)
					go func() {
						done <- requestWithHeaders(t, h, http.MethodPost, path, map[string]any{"availableRuntimes": []string{"mock"}, "capacity": 1, "preflight": preflight}, auth)
					}()
					select {
					case <-waiting:
					case <-time.After(time.Second):
						t.Fatal("request did not reach its empty long-poll wait")
					}
					current := old
					newSession, newHash, err := newRunnerSessionToken()
					if err != nil {
						t.Fatal(err)
					}
					if change == "rotate" {
						current.SessionTokenHash = newHash
					} else {
						expired := time.Now().UTC().Add(-time.Second)
						current.SessionExpiresAt = &expired
					}
					if err := s.PutRunner(current); err != nil {
						t.Fatal(err)
					}
					const privatePrompt = "fixture-invocation-must-never-reach-the-old-session"
					task := domain.RunnerTask{ID: "after-session-change", RunnerID: runnerID, Provider: "mock", Prompt: privatePrompt}
					if err := s.PutRunnerTask(task); err != nil {
						t.Fatal(err)
					}
					select {
					case response := <-done:
						if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") == "" || response.Header().Get("X-Runner-Task-Protocol") != "2" {
							t.Fatalf("stale session response=%d %s", response.Code, response.Body.String())
						}
						for _, secret := range []string{privatePrompt, session, old.SessionTokenHash, newHash} {
							if strings.Contains(response.Body.String(), secret) {
								t.Fatal("stale session response leaked assignment or session identity")
							}
						}
					case <-time.After(3 * time.Second):
						t.Fatal("stale long poll did not terminate")
					}
					stored, _ := s.RunnerTask(task.ID)
					if stored.Status != domain.RunnerTaskQueued || stored.Attempt != 0 || stored.LeaseOwner != "" {
						t.Fatal("stale session changed task lease")
					}
					if change == "rotate" {
						auth["Authorization"] = "Bearer " + newSession
						response := requestWithHeaders(t, h, http.MethodPost, path, map[string]any{"availableRuntimes": []string{"mock"}, "noWait": true}, auth)
						var result struct {
							Task domain.RunnerTask `json:"task"`
						}
						if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Task.ID != task.ID || result.Task.Attempt != 1 {
							t.Fatalf("new session claim=%d %s", response.Code, response.Body.String())
						}
					}
				})
			}
		}
	}
}

func TestRunnerTaskCallbacksRejectSessionChangedWhileDecoding(t *testing.T) {
	for _, prefix := range []string{"/runners/tasks/", "/api/runners/tasks/"} {
		for _, action := range []string{"start", "lease/renew", "events", "complete"} {
			for _, change := range []string{"rotate", "expire"} {
				t.Run(prefix+action+"/"+change, func(t *testing.T) {
					s, err := repo.OpenTest()
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(s.Close)
					h := New(s, engine.New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
					runnerID, session := registerTestRunner(t, h)
					runner, _ := s.Runner(runnerID)
					if err := s.PutRunnerTask(domain.RunnerTask{ID: "callback-session-task", RunnerID: runnerID, Provider: "mock"}); err != nil {
						t.Fatal(err)
					}
					task, found, err := s.ClaimRunnerTaskForSession(runnerID, runner.SessionTokenHash, []string{"mock"}, time.Now().UTC(), time.Minute)
					if err != nil || !found {
						t.Fatalf("fixture claim: found=%v err=%v", found, err)
					}
					payload, _ := json.Marshal(map[string]any{"attempt": task.Attempt, "status": domain.RunnerTaskSuccess, "events": []domain.RunnerTaskEvent{{Seq: 1, Type: "stdout", Data: "must not persist"}}})
					read := false
					body := &contributionReadHook{Reader: strings.NewReader(string(payload)), once: func() {
						read = true
						if change == "rotate" {
							runner.SessionTokenHash = strings.Repeat("3", 64)
						} else {
							expired := time.Now().UTC().Add(-time.Second)
							runner.SessionExpiresAt = &expired
						}
						if err := s.PutRunner(runner); err != nil {
							t.Fatal(err)
						}
					}}
					req := httptest.NewRequest(http.MethodPost, prefix+task.ID+"/"+action, body)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+session)
					response := httptest.NewRecorder()
					h.ServeHTTP(response, req)
					if !read || response.Code != http.StatusUnauthorized {
						t.Fatalf("stale callback=%d read=%v body=%s", response.Code, read, response.Body.String())
					}
					stored, _ := s.RunnerTask(task.ID)
					if stored.Status != task.Status || stored.Attempt != task.Attempt || stored.StartedAt != nil || stored.CompletedAt != nil || !stored.LeaseUntil.Equal(*task.LeaseUntil) || len(s.RunnerTaskEvents(task.ID)) != 0 {
						t.Fatal("rejected session mutated task or events")
					}
				})
			}
		}
	}
}

func TestRunnerTaskControlRechecksSessionAfterInitialAuthentication(t *testing.T) {
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	h := New(s, engine.New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
	runnerID, session := registerTestRunner(t, h)
	runner, _ := s.Runner(runnerID)
	if err := s.PutRunnerTask(domain.RunnerTask{ID: "control-session-task", RunnerID: runnerID, Provider: "mock"}); err != nil {
		t.Fatal(err)
	}
	task, found, err := s.ClaimRunnerTaskForSession(runnerID, runner.SessionTokenHash, []string{"mock"}, time.Now().UTC(), time.Minute)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	var changed atomic.Bool
	const callbackName = "control-session-change"
	if err := s.DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "runner_tasks" && changed.CompareAndSwap(false, true) {
			runner.SessionTokenHash = strings.Repeat("4", 64)
			if err := s.PutRunner(runner); err != nil {
				t.Error(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Callback().Query().Remove(callbackName) })
	response := requestWithHeaders(t, h, http.MethodGet, "/runners/tasks/"+task.ID+"/control?attempt=1", nil, map[string]string{"Authorization": "Bearer " + session})
	if !changed.Load() || response.Code != http.StatusUnauthorized {
		t.Fatalf("stale control=%d %s", response.Code, response.Body.String())
	}
}
