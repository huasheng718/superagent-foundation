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

func TestRunnerHTTPAbsoluteExecutionDeadline(t *testing.T) {
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, engine.New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
	runnerID, session := registerTestRunner(t, h)
	auth := map[string]string{"Authorization": "Bearer " + session, "X-Runner-Task-Protocol": "2"}
	now := time.Now().UTC()
	deadline := now.Add(time.Minute)
	if err := s.PutRunnerTask(domain.RunnerTask{ID: "http-deadline", Provider: "mock", ExecutionDeadline: &deadline}); err != nil {
		t.Fatal(err)
	}
	task, found, err := s.ClaimRunnerTask(runnerID, []string{"mock"}, now, time.Hour)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	rec := requestWithHeaders(t, h, http.MethodPost, "/runners/tasks/http-deadline/lease/renew", map[string]any{"attempt": task.Attempt}, auth)
	var renewed struct {
		LeaseUntil time.Time `json:"leaseUntil"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &renewed) != nil || !renewed.LeaseUntil.Equal(deadline) {
		t.Fatalf("renew response extended frozen deadline: %d %s", rec.Code, rec.Body.String())
	}
	// Keep the ordinary lease alive to prove HTTP control independently checks
	// persisted execution expiry, rather than depending on capped lease values.
	past := now.Add(-time.Second)
	task.ExecutionDeadline = &past
	raw, _ := json.Marshal(task)
	if err := s.DB.Table("runner_tasks").Where("id = ?", task.ID).Updates(map[string]any{"payload": raw, "lease_until": now.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"control", "start", "lease/renew", "events", "complete"} {
		method, path := http.MethodPost, "/runners/tasks/http-deadline/"+endpoint
		var body any = map[string]any{"attempt": task.Attempt, "status": "success"}
		if endpoint == "control" {
			method, path, body = http.MethodGet, path+"?attempt=1", nil
		}
		rec := requestWithHeaders(t, h, method, path, body, auth)
		if rec.Code != http.StatusConflict {
			t.Fatalf("expired %s accepted: %d %s", endpoint, rec.Code, rec.Body.String())
		}
	}
}
