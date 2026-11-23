package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestControlledTaskRejectedBeforeCommandStarts(t *testing.T) {
	for _, tc := range []struct {
		name string
		task domain.RunnerTask
	}{
		{"missing policy with token", domain.RunnerTask{Invocation: &domain.RunnerInvocation{Repository: &domain.RunnerRepository{GitToken: "secret"}}}},
		{"builtin push", domain.RunnerTask{Action: domain.RunnerTaskActionPushWorkBranch}},
		{"unknown policy", domain.RunnerTask{DeliveryPolicy: "unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var completed CompleteRequest
			started := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/complete") {
					if err := json.NewDecoder(r.Body).Decode(&completed); err != nil {
						t.Error(err)
					}
				} else if strings.HasSuffix(r.URL.Path, "/start") || strings.HasSuffix(r.URL.Path, "/events") {
					started = true
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			marker := filepath.Join(t.TempDir(), "started")
			task := tc.task
			task.ID, task.Attempt, task.Provider = "denied", 1, "terminal"
			task.Command = "touch '" + marker + "'"
			d, err := New(Config{ServerURL: server.URL, BootstrapToken: "test", StatePath: filepath.Join(t.TempDir(), "state"), DefaultWorkspaceRoot: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			d.running = 1
			d.executeTask(context.Background(), task)
			if completed.Status != domain.RunnerTaskFailed || completed.Error == "" || started {
				t.Fatalf("unsafe execution started=%v completion=%+v", started, completed)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("command marker exists: %v", err)
			}
		})
	}
}
