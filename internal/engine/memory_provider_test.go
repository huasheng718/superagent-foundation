package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestRecallReceiptFreezesExactNativePinsAndResponsibleUser(t *testing.T) {
	store, e, release, m, run := governedFixture(t)
	receipt, err := store.SaveMemoryRecallReceipt(domain.MemoryRecallReceipt{Scope: domain.MemoryProviderScope{ProjectID: m.ProjectID, UserID: 7, AgentID: "project-ui"}, Hits: []domain.MemoryRecallHit{{MemoryID: m.ID, Revision: m.Revision, SHA256: m.SHA256, RecordIDs: []string{"msg-fixed"}, RecordVersions: []string{"v1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	selection := domain.TeamContextSelection{ReleaseID: release.ID, MemoryIDs: []string{m.ID}, MemoryRecallReceiptID: receipt.ID}
	if _, err := e.ResolveIntelligenceContext(m.ProjectID, selection, nil); err == nil {
		t.Fatal("receipt accepted without authenticated user")
	}
	if _, err := e.ResolveIntelligenceContext(m.ProjectID, selection, nil, 8); err == nil {
		t.Fatal("other user's receipt accepted")
	}
	selection.MemoryIDs = nil
	if _, err := e.ResolveIntelligenceContext(m.ProjectID, selection, nil, 7); err == nil {
		t.Fatal("receipt selected different memory set")
	}
	selection.MemoryIDs = []string{m.ID}
	run.ExecutionContext.TeamContext = &selection
	run.ExecutionContext.ResponsibleUserID = 7
	if err := e.FreezeRunIntelligence(&run); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(run)
	var replay domain.Run
	json.Unmarshal(raw, &replay)
	if replay.Intelligence.MemoryRecallReceipt == nil || replay.Intelligence.MemoryRecallReceipt.ID != receipt.ID {
		t.Fatal("receipt not frozen")
	}
	runner := domain.Runner{ID: "receipt-runner", RunnerVersion: domain.RunnerVersionGovernedContext}
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	task := domain.RunnerTask{ID: "receipt-task", RunID: run.ID, NodeID: "author", RunnerID: runner.ID, Provider: "terminal", Command: "true", Invocation: &domain.RunnerInvocation{RunID: run.ID, NodeID: "author"}}
	if err := e.applyRunnerTaskPolicy(run, &task); err == nil || !strings.Contains(err.Error(), "0.13.0") {
		t.Fatalf("old receipt Runner admitted: %v", err)
	}
	runner.RunnerVersion = domain.RunnerVersionMemoryRecallReceipt
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if err := e.applyRunnerTaskPolicy(run, &task); err != nil {
		t.Fatal(err)
	}
	if err := domain.ValidateRunnerGovernedContextForRun(run, task); err != nil {
		t.Fatal(err)
	}
	if task.Invocation.GovernedContext.MemoryRecallReceipt == nil || task.Invocation.GovernedContext.MemoryRecallReceipt.SHA256 != receipt.SHA256 {
		t.Fatal("Worker receipt source missing")
	}
	task.Invocation.GovernedContext.MemoryRecallReceipt.Binding.Generation = "forged-worker-source"
	task.Invocation.GovernedContext.MemoryRecallReceipt.SHA256, _ = task.Invocation.GovernedContext.MemoryRecallReceipt.Digest()
	if err := domain.ValidateRunnerGovernedContextForRun(run, task); err == nil {
		t.Fatal("Worker swapped provider source")
	}
	if run.Intelligence.MemoryRecallReceipt.Binding.Generation == "forged-worker-source" {
		t.Fatal("Worker binding aliases frozen Run receipt")
	}
	// Replay deliberately has no provider client or HTTP/config dependency.
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_URL", "http://127.0.0.1:1")
	if err := e.ValidateRunIntelligence(replay); err != nil {
		t.Fatal(err)
	}
	replay.Intelligence.MemoryRecallReceipt.Binding.Generation = "changed"
	replay.Intelligence.SHA256, _ = replay.Intelligence.Digest()
	if err := e.ValidateRunIntelligence(replay); err == nil {
		t.Fatal("receipt tamper survived recomputed outer digest")
	}
	replay = run
	replay.ExecutionContext.ResponsibleUserID = 8
	if err := e.ValidateRunIntelligence(replay); err == nil {
		t.Fatal("responsible user substituted")
	}
}
func TestOldContextWireOmitsReceiptFields(t *testing.T) {
	_, _, _, _, run := governedFixture(t)
	raw, _ := json.Marshal(run)
	if strings.Contains(string(raw), "memoryRecallReceipt") {
		t.Fatal("optional receipt changed old protocol")
	}
}
