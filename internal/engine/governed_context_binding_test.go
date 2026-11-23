package engine

import (
	"agentflow/internal/domain"
	"strings"
	"testing"
)

func TestGovernedRunnerContextBindingRejectsLegacyAndCoversTerminal(t *testing.T) {
	s, e, _, _, run := governedFixture(t)
	runner := domain.Runner{ID: "context-runner", RunnerVersion: "0.11.0"}
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	task := domain.RunnerTask{ID: "context-task", RunID: run.ID, NodeID: "author", RunnerID: runner.ID, Provider: "terminal", Command: "true", Invocation: &domain.RunnerInvocation{RunID: run.ID, NodeID: "author"}}
	if err := e.applyRunnerTaskPolicy(run, &task); err == nil || !strings.Contains(err.Error(), "0.12.0") {
		t.Fatalf("old Runner error=%v", err)
	}
	runner.RunnerVersion = domain.RunnerVersionGovernedContext
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if err := e.applyRunnerTaskPolicy(run, &task); err != nil {
		t.Fatal(err)
	}
	if err := domain.ValidateRunnerGovernedContextForRun(run, task); err != nil {
		t.Fatal(err)
	}
	task.Invocation.GovernedContext = nil
	missingSnapshot := run
	missingSnapshot.Intelligence = nil
	if e.applyRunnerTaskPolicy(missingSnapshot, &task) == nil {
		t.Fatal("missing selected snapshot escaped common governed binding")
	}
	if domain.ValidateRunnerGovernedContextForRun(run, task) == nil {
		t.Fatal("missing approved binding admitted")
	}
	task.Action = domain.RunnerTaskActionCleanupRun
	if err := e.applyRunnerTaskPolicy(run, &task); err != nil {
		t.Fatal(err)
	}
	if task.Invocation.GovernedContext != nil {
		t.Fatal("cleanup bound governed execution")
	}
}
