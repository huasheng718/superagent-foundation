package engine

import (
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestAttemptWorkspaceStartFreezesPinsAndRejectsMissingExtraAndOldRunner(t *testing.T) {
	e, s := controlledTestEngine(t)
	runner := domain.Runner{ID: "isolated-runner", OwnerUserID: 71, RunnerVersion: domain.RunnerVersionWorkspaceIsolation}
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefaultRunner(71, runner.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	wf := domain.WorkflowDefinition{ID: "isolated-workflow", Name: "Isolated", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{{ID: "wait", Name: "Wait", Type: "approval", MaxAttempts: 1}}}
	if err := s.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	repositories := []domain.RepositoryContext{{RepositoryID: "repo", Name: "Repo", CloneURL: t.TempDir(), BaseBranch: "main"}}
	ctx := domain.TelemetryContext{ResponsibleUserID: 71, RunnerWorkspaceLayout: domain.RunnerWorkspaceLayoutAttempt, WorkspaceBaseSHAs: map[string]string{"repo": strings.Repeat("a", 40)}}
	run, err := e.StartWithRepositoriesContext(wf.ID, "test", repositories, ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx.WorkspaceBaseSHAs["repo"] = strings.Repeat("b", 40)
	if run.ExecutionContext.RunnerWorkspaceLayout != domain.RunnerWorkspaceLayoutAttempt || run.ExecutionContext.WorkspaceBaseSHAs["repo"] != strings.Repeat("a", 40) {
		t.Fatal("layout or pins were not frozen")
	}
	if _, _, err := e.workspaceSandboxProfile(run); err != nil {
		t.Fatalf("pre-assignment sandbox preparation must remain available: %v", err)
	}
	for _, pins := range []map[string]string{nil, {"repo": "main"}, {"repo": strings.Repeat("a", 40), "outside": strings.Repeat("a", 40)}} {
		ctx.WorkspaceBaseSHAs = pins
		if _, err := e.StartWithRepositoriesContext(wf.ID, "test", repositories, ctx); err == nil {
			t.Fatal("missing/invalid/outside repository pin accepted")
		}
	}
	ctx.WorkspaceBaseSHAs = map[string]string{"repo": strings.Repeat("a", 40)}
	runner.RunnerVersion = "0.9.2"
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartWithRepositoriesContext(wf.ID, "test", repositories, ctx); err == nil {
		t.Fatal("old Runner silently downgraded isolation")
	}
	legacy, err := e.StartWithRepositoriesContext(wf.ID, "shared", repositories, domain.TelemetryContext{ResponsibleUserID: 71})
	if err != nil || legacy.ExecutionContext.RunnerWorkspaceLayout != domain.RunnerWorkspaceLayoutShared {
		t.Fatalf("default shared compatibility changed: %v", err)
	}
}
