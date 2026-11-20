package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

func TestNativeScriptAbsoluteExecutionDeadline(t *testing.T) {
	st, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := leaseEngine(t, st, "deadline-engine")
	// executeNode is invoked directly here, without execute's run heartbeat.
	// Keep its independent Run lease alive longer than the execution budget.
	e.ConfigureExecution("deadline-engine", "deadline-engine-host", time.Minute, true)
	marker := filepath.Join(t.TempDir(), "started")
	node := domain.WorkflowNode{ID: "sleep", Name: "Sleep", Type: "script", ScriptLanguage: "shell", Script: "printf started > '" + marker + "'; exec sleep 5", MaxDurationSec: 1, MaxAttempts: 3}
	wf := domain.WorkflowDefinition{ID: "deadline-workflow", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{node}}
	run := domain.Run{ID: "native-deadline", Status: "running", WorkflowID: wf.ID, WorkflowSnapshot: wf, Nodes: map[string]*domain.NodeRun{node.ID: {NodeID: node.ID, Status: "pending"}}}
	if err := st.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if _, err := e.acquireLease(run); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	e.executeNode(context.Background(), run.ID, wf, node)
	got, _ := st.RunState(run.ID)
	state := got.Nodes[node.ID]
	if elapsed := time.Since(started); elapsed > 4*time.Second || elapsed < 800*time.Millisecond {
		t.Fatalf("native deadline elapsed=%s", elapsed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("actual native script did not start: %v", err)
	}
	if state.Status != "failed" || state.Error != domain.ErrExecutionDeadlineExceeded.Error() || state.Attempt != 1 || state.ExecutionDeadline == nil || !state.ExecutionDeadline.Equal(state.StartedAt.Add(time.Second)) {
		t.Fatalf("timeout not frozen/failed: %+v", state)
	}
}

func TestRecoveredNodeKeepsExpiredAttemptDeadline(t *testing.T) {
	e, st := newRecoveryEngine(t)
	now := time.Now().UTC()
	deadline := now.Add(-time.Second)
	node := domain.WorkflowNode{ID: "work", Type: "script", ScriptLanguage: "shell", Script: "exit 0", MaxDurationSec: 60}
	wf := domain.WorkflowDefinition{ID: "recover-deadline", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{node}}
	run := domain.Run{ID: "recover-deadline", WorkflowID: wf.ID, WorkflowSnapshot: wf, Status: "running", Nodes: map[string]*domain.NodeRun{node.ID: {NodeID: node.ID, Status: "pending", Attempt: 2, StartedAt: now.Add(-time.Minute), ExecutionDeadline: &deadline, Activity: "执行实例退出，等待恢复"}}}
	if err := st.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if _, err := e.acquireLease(run); err != nil {
		t.Fatal(err)
	}
	e.executeNode(context.Background(), run.ID, wf, node)
	got, _ := st.RunState(run.ID)
	nr := got.Nodes[node.ID]
	if nr.Attempt != 2 || nr.ExecutionDeadline == nil || !nr.ExecutionDeadline.Equal(deadline) || nr.Status != "failed" || !strings.Contains(nr.Error, "deadline exceeded") {
		t.Fatalf("recovery extended authority: %+v", nr)
	}
}

func TestAwaitRunnerTaskUsesFrozenDeadline(t *testing.T) {
	e, st := newRecoveryEngine(t)
	deadline := time.Now().UTC().Add(80 * time.Millisecond)
	if err := st.PutRunnerTask(domain.RunnerTask{ID: "await-deadline", Provider: "terminal", ExecutionDeadline: &deadline}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := e.awaitRunnerTask(context.Background(), "await-deadline", nil)
	if !errors.Is(err, domain.ErrExecutionDeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("await extended deadline: %v (%s)", err, time.Since(start))
	}
	got, _ := st.RunnerTask("await-deadline")
	if got.Status != domain.RunnerTaskCanceled {
		t.Fatalf("queued task not canceled: %+v", got)
	}
}
