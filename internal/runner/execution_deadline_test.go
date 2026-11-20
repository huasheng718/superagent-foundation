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
	"time"

	"agentflow/internal/domain"
)

func TestRunnerNativeTaskDeadlineStopsProcessAndGateWait(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "native-process", true: "workspace-writer"}[blocked], func(t *testing.T) {
			var completed CompleteRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/complete") {
					_ = json.NewDecoder(r.Body).Decode(&completed)
				}
				if strings.HasSuffix(r.URL.Path, "/lease") {
					_ = json.NewEncoder(w).Encode(RenewResponse{LeaseUntil: time.Now().Add(time.Minute)})
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			workspace := t.TempDir()
			d, err := New(Config{ServerURL: server.URL, BootstrapToken: "test", StatePath: filepath.Join(t.TempDir(), "state"), DefaultWorkspaceRoot: workspace})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().UTC().Add(250 * time.Millisecond)
			task := domain.RunnerTask{ID: "deadline-task", RunID: "deadline-run", Attempt: 1, Provider: "terminal", Workspace: workspace, ExecutionDeadline: &deadline, Command: "printf started > started; sleep 5; printf late > late"}
			if blocked {
				gate := d.workspaceGate(d.taskGateKey(task))
				gate.Lock()
				defer gate.Unlock()
			}
			d.running = 1
			start := time.Now()
			d.executeTask(context.Background(), task)
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("deadline did not stop work: %s", elapsed)
			}
			if completed.Status != domain.RunnerTaskFailed || completed.Error != domain.ErrExecutionDeadlineExceeded.Error() {
				t.Fatalf("completion=%+v", completed)
			}
			_, startedErr := os.Stat(filepath.Join(workspace, "started"))
			if blocked && !os.IsNotExist(startedErr) || !blocked && startedErr != nil {
				t.Fatalf("script execution mismatch: %v", startedErr)
			}
			if _, err := os.Stat(filepath.Join(workspace, "late")); !os.IsNotExist(err) {
				t.Fatalf("process continued after deadline: %v", err)
			}
		})
	}
}
