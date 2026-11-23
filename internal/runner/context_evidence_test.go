package runner

import (
	"agentflow/internal/domain"
	"strings"
	"testing"
)

func TestGovernedMeasurementRejectsChangedManifestAndUnmaterializedInstructions(t *testing.T) {
	task := isolatedFixtureTask("context", 2)
	task.Invocation.GovernedContext = &domain.RunnerGovernedContext{Version: 1, IntelligenceSHA256: strings.Repeat("1", 64), ReleaseSHA256: strings.Repeat("2", 64), ProjectID: "project", ReleaseID: "release"}
	task.Invocation.WorkspaceIsolation.ContextSHA256, _ = domain.RunnerContributionContextSHA(task)
	evidence, err := measureTaskContext(task, t.TempDir(), false)
	if err != nil || evidence.TaskID != task.ID || evidence.Attempt != 2 {
		t.Fatalf("evidence=%v err=%v", evidence, err)
	}
	task.Prompt = "tampered assignment"
	if _, err := measureTaskContext(task, t.TempDir(), false); err == nil {
		t.Fatal("changed task context accepted")
	}
	task.Invocation.Assets.Rules = []domain.RuleDefinition{{ID: "memory", Content: "approved"}}
	task.Invocation.WorkspaceIsolation.ContextSHA256, _ = domain.RunnerContributionContextSHA(task)
	if _, err := measureTaskContext(task, t.TempDir(), false); err == nil {
		t.Fatal("script omitted assigned instructions")
	}
	if _, err := measureTaskContext(task, t.TempDir(), true); err == nil {
		t.Fatal("missing canonical resources accepted")
	}
}
