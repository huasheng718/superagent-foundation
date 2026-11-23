package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

// Exercise the real dispatch assembler and durable placed queue without a
// Daemon, subprocess, network service or model. Completion/source integrity is
// separately tested by the Repo extraction contract and live E2E cohort.
func TestReportedExtractionDispatchKeepsDeclaredOutputAuthority(t *testing.T) {
	e, s, run, runner := placementEngineFixture(t)
	runner.Runtimes = append(runner.Runtimes, domain.RunnerRuntime{Provider: "codex", Version: "unexecuted-fixture-1", BinaryPath: "/unexecuted/fixture/codex", Status: domain.RuntimeStatusOnline, Capabilities: []string{"non_interactive"}})
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	agent := domain.AgentDefinition{ID: "dispatch-extractor", Name: "Extractor", ExecutorID: "codex", ProjectID: run.ExecutionContext.ProjectID}
	if err := s.PutAgent(agent); err != nil {
		t.Fatal(err)
	}
	contract := domain.ArtifactContract{Name: "knowledge-candidates", Path: ".agentflow/artifacts/knowledge-candidates.json", MediaType: "application/json", Required: true}
	node := domain.WorkflowNode{ID: "writer", Type: "agent", AgentID: agent.ID, ExecutorID: "codex", MaxAttempts: 1, MaxDurationSec: 120, ArtifactOutputs: []domain.ArtifactContract{contract}}
	run.Status = "running"
	run.WorkflowSnapshot.SandboxProfileID = "local"
	run.WorkflowSnapshot.Nodes = []domain.WorkflowNode{node}
	run.ExecutionContext.ExecutionRunnerID = runner.ID
	run.ExecutionContext.NodePlacements = map[string]domain.NodePlacementRequest{node.ID: {RunnerID: runner.ID}}
	run.Nodes[node.ID].Status = "running"
	run.Nodes[node.ID].StartedAt = time.Now().UTC()
	deadline := time.Now().UTC().Add(120 * time.Second)
	run.Nodes[node.ID].ExecutionDeadline = &deadline
	if err := e.freezeNodeExecutionPlacement(&run, node, run.Nodes[node.ID]); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	environment := &runEnvironment{}
	environment.beginReportedOutputs(node.ID, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var assigned domain.RunnerTask
	e.SetRunnerDispatchNotify(func(string) {
		for _, task := range s.RunnerTasksForRun(run.ID) {
			if task.Action == "" {
				assigned = task
			}
		}
		cancel() // Stop at queue assembly; no Runner ever claims or executes it.
	})
	executor, _ := s.Executor("codex")
	_, err := e.dispatchRunnerTask(ctx, runner.ID, run, node, environment, runnerWorkspaceRel(run, node.ID), executor, agent, domain.AgentAssetBundle{}, "fixture", "writer-1-1", runtime.InvocationOptions{GateResultPath: ".agentflow/gate-results/writer/attempt-1.json"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dispatch did not reach unclaimed queue boundary: %v", err)
	}
	if assigned.ID == "" || assigned.Attempt != 0 || assigned.Execution == nil || assigned.ExecutionPlacement == nil || assigned.Invocation == nil || !assigned.Invocation.ReportOutputs || len(assigned.Invocation.Artifacts) != 0 {
		t.Fatalf("unexpected reported assignment: %+v", assigned)
	}
	if !reflect.DeepEqual(assigned.Invocation.OutputFiles, []string{contract.Path, ".agentflow/gate-results/writer/attempt-1.json"}) {
		t.Fatalf("declared output path lost: %+v", assigned.Invocation.OutputFiles)
	}
	stored, _ := s.RunState(run.ID)
	if !reflect.DeepEqual(stored.WorkflowSnapshot.Nodes[0].ArtifactOutputs, []domain.ArtifactContract{contract}) {
		t.Fatal("dispatch changed the frozen required contract")
	}
}
