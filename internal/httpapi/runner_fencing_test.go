package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

func TestRunnerHTTPAttemptFencing(t *testing.T) {
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, engine.New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
	runnerID, session := registerTestRunner(t, h)
	auth := map[string]string{"Authorization": "Bearer " + session, "X-Runner-Task-Protocol": "2"}
	now := time.Now().UTC()
	if err := s.PutRunnerTask(domain.RunnerTask{ID: "fenced", Provider: "mock", Status: domain.RunnerTaskQueued, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	old, ok, err := s.ClaimRunnerTask(runnerID, []string{"mock"}, now.Add(-2*time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("old claim %v %v", ok, err)
	}
	// An expired attempt is rejected before the recovery sweep too.
	expired := requestWithHeaders(t, h, http.MethodPost, "/runners/tasks/fenced/start", map[string]any{"attempt": old.Attempt}, auth)
	if expired.Code != http.StatusConflict {
		t.Fatalf("expired start %d %s", expired.Code, expired.Body.String())
	}
	if _, err := s.ExpireRunnerTasks(now); err != nil {
		t.Fatal(err)
	}
	current, ok, err := s.ClaimRunnerTask(runnerID, []string{"mock"}, now, time.Minute)
	if err != nil || !ok || current.Attempt <= old.Attempt {
		t.Fatalf("new claim %+v %v %v", current, ok, err)
	}
	for _, suffix := range []string{"start", "lease/renew", "events", "complete", "control"} {
		t.Run(suffix, func(t *testing.T) {
			for _, attempt := range []int{0, -1, old.Attempt, current.Attempt + 1} {
				method := http.MethodPost
				path := "/runners/tasks/fenced/" + suffix
				body := map[string]any{"attempt": attempt, "status": "success", "events": []domain.RunnerTaskEvent{}}
				if suffix == "control" {
					method = http.MethodGet
					path += "?attempt=" + jsonNumber(attempt)
					body = nil
				}
				rec := requestWithHeaders(t, h, method, path, body, auth)
				want := http.StatusConflict
				if attempt <= 0 {
					want = http.StatusBadRequest
				}
				if rec.Code != want {
					t.Fatalf("attempt %d got %d want %d: %s", attempt, rec.Code, want, rec.Body.String())
				}
			}
			method := http.MethodPost
			path := "/runners/tasks/fenced/" + suffix
			var body any = map[string]any{}
			if suffix == "control" {
				method = http.MethodGet
				body = nil
			}
			rec := requestWithHeaders(t, h, method, path, body, auth)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("missing attempt accepted: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	// Rejected empty event batches and completions must not mutate the new lease.
	persisted, _ := s.RunnerTask(current.ID)
	if persisted.Attempt != current.Attempt || persisted.Status != domain.RunnerTaskLeased {
		t.Fatalf("stale request mutated current lease: %+v", persisted)
	}
	for _, suffix := range []string{"start", "lease/renew", "events", "control"} {
		method := http.MethodPost
		path := "/runners/tasks/fenced/" + suffix
		var body any = map[string]any{"attempt": current.Attempt, "events": []domain.RunnerTaskEvent{{Seq: 1, Type: "stdout", Data: "current"}}}
		if suffix == "control" {
			method = http.MethodGet
			path += "?attempt=" + jsonNumber(current.Attempt)
			body = nil
		}
		rec := requestWithHeaders(t, h, method, path, body, auth)
		if rec.Code != http.StatusOK {
			t.Fatalf("current %s: %d %s", suffix, rec.Code, rec.Body.String())
		}
	}
	for _, embeddedAttempt := range []int{-1, old.Attempt, current.Attempt + 1} {
		rec := requestWithHeaders(t, h, http.MethodPost, "/runners/tasks/fenced/events", map[string]any{"attempt": current.Attempt, "events": []domain.RunnerTaskEvent{{Attempt: embeddedAttempt, Seq: 2, Type: "stdout", Data: "invalid generation"}}}, auth)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("embedded attempt %d: %d %s", embeddedAttempt, rec.Code, rec.Body.String())
		}
	}
	if events := s.RunnerTaskEvents(current.ID); len(events) != 1 {
		t.Fatalf("invalid event attempt persisted: %+v", events)
	}
	if err := s.CancelRunnerTask(current.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{domain.RunnerTaskSuccess, domain.RunnerTaskCanceled} {
		rec := requestWithHeaders(t, h, http.MethodPost, "/runners/tasks/fenced/complete", map[string]any{"attempt": current.Attempt, "status": status}, auth)
		want := http.StatusOK
		if status == domain.RunnerTaskSuccess {
			want = http.StatusConflict
		}
		if rec.Code != want {
			t.Fatalf("complete %s %d want %d: %s", status, rec.Code, want, rec.Body.String())
		}
	}
}

func jsonNumber(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestRunnerClaimRequiresFencingHandshakeBeforeLease(t *testing.T) {
	h := newRunnerTestServer(t)
	_, session := registerTestRunner(t, h)
	created := requestJSON[domain.RunnerTask](t, h, http.MethodPost, "/api/runners/tasks", map[string]any{"provider": "mock", "prompt": "must remain queued"}, http.StatusCreated)
	req := map[string]any{"availableRuntimes": []string{"mock"}, "capacity": 1, "noWait": true}
	for _, version := range []string{"", "1"} {
		auth := map[string]string{"Authorization": "Bearer " + session, "X-Runner-Task-Protocol": version}
		rec := requestWithHeaders(t, h, http.MethodPost, "/runners/tasks/claim", req, auth)
		if rec.Code != http.StatusUpgradeRequired {
			t.Fatalf("protocol %q got %d: %s", version, rec.Code, rec.Body.String())
		}
	}
	auth := map[string]string{"Authorization": "Bearer " + session, "X-Runner-Task-Protocol": "2"}
	rec := requestWithHeaders(t, h, http.MethodPost, "/runners/tasks/claim", req, auth)
	var out struct {
		Task domain.RunnerTask `json:"task"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Runner-Task-Protocol") != "2" || out.Task.ID != created.ID || out.Task.Attempt != 1 {
		t.Fatalf("handshake claim %d %s", rec.Code, rec.Body.String())
	}
	rec = requestWithHeaders(t, h, http.MethodPost, "/runners/tasks/claim", req, auth)
	if rec.Code != http.StatusNoContent || rec.Header().Get("X-Runner-Task-Protocol") != "2" {
		t.Fatalf("empty handshake %d %v", rec.Code, rec.Header())
	}
}
