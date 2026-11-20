package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

type contributionGateFixture struct {
	*acceptedContributionFixture
	integration domain.ContributionIntegration
	gateSource  *acceptedContributionFixture
}

func newContributionGateFixture(t *testing.T) *contributionGateFixture {
	t.Helper()
	f := newAcceptedContributionFixture(t)
	repository, _ := f.s.Repository("repo")
	repository.DefaultBranch = "main"
	if err := f.s.PutRepository(repository); err != nil {
		t.Fatal(err)
	}
	writer := f.accept(t, "author", "feature.py", "feature = True\n", func(task *domain.RunnerTask, _ *domain.Run) { task.Invocation.Agent.ID = "author-agent" })
	integration, err := f.e.IntegrateRunnerContributions(context.Background(), f.controller.ID, "repo", []string{writer.ID})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := f.s.RunnerContributionBlob(integration.BundleSHA256)
	bundle := filepath.Join(f.root, "integration-gate.bundle")
	if err := os.WriteFile(bundle, raw, 0600); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(f.root, "integration-gate-source")
	if err := gitwork.VerifyContribution(context.Background(), bundle, checkout, gitwork.ContributionBundle{BaseSHA: integration.BaseSHA, ResultSHA: integration.ResultSHA, SHA256: integration.BundleSHA256, Size: integration.BundleSize}, 64<<20); err != nil {
		t.Fatal(err)
	}
	gateSource := *f
	gateSource.source = checkout
	gateSource.base = integration.ResultSHA
	return &contributionGateFixture{acceptedContributionFixture: f, integration: integration, gateSource: &gateSource}
}

func (f *contributionGateFixture) gate(t *testing.T, id, kind string, changeCode bool, mutate func(*domain.RunnerTask, *domain.Run)) domain.RunnerTask {
	t.Helper()
	content := "baseline\n"
	if changeCode {
		content = "changed by gate\n"
	}
	return f.gateSource.accept(t, id, "code.txt", content, func(task *domain.RunnerTask, run *domain.Run) {
		nodeType := "script"
		task.Provider = "terminal"
		task.Command = "python3 -m unittest"
		definition := domain.WorkflowNode{ID: "implement", Type: nodeType, ScriptLanguage: "shell", Script: task.Command}
		node := run.Nodes["implement"]
		node.NodeID = "implement"
		node.Attempt = 1
		node.Provider = task.Provider
		if kind == "review" {
			task.Provider = "codex"
			task.Command = ""
			task.Invocation.Agent.ID = "independent-reviewer"
			definition.Type = "agent"
			definition.AgentID = task.Invocation.Agent.ID
			node.Provider = task.Provider
			node.SessionID = "review-session-" + id
			node.GateResult = &domain.GateResult{Decision: "pass", Reason: "reviewed exact snapshot"}
		}
		report := "Verified exact integration snapshot: " + f.integration.ResultSHA + "\n"
		sum := sha256.Sum256([]byte(report))
		run.WorkflowSnapshot = domain.WorkflowDefinition{ID: "gate-flow-" + id, Nodes: []domain.WorkflowNode{definition}}
		run.Artifacts = []domain.Artifact{{ID: "report-" + id, RunID: run.ID, NodeID: "implement", Attempt: 1, Name: "report.md", Path: ".agentflow/reports/report.md", Content: report, Size: int64(len(report)), SHA256: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC()}}
		if mutate != nil {
			mutate(task, run)
		}
	})
}

func (f *contributionGateFixture) record(t *testing.T, task domain.RunnerTask, kind string) domain.ContributionPublication {
	t.Helper()
	publication, err := f.e.RecordContributionGateFromRun(f.integration.ID, task.RunID, task.NodeID, kind, "report-"+task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return publication
}

func (f *contributionGateFixture) ready(t *testing.T) domain.ContributionPublication {
	t.Helper()
	var publication domain.ContributionPublication
	for _, kind := range []string{"fast", "slow", "review"} {
		publication = f.record(t, f.gate(t, kind, kind, false, nil), kind)
	}
	return publication
}

func TestContributionGatesRequireIndependentExactSHAEvidence(t *testing.T) {
	f := newContributionGateFixture(t)
	publication := f.ready(t)
	if publication.State != "ready" || len(publication.Gates) != 3 {
		t.Fatalf("publication=%+v", publication)
	}
	if err := f.e.validateContributionPublication(f.integration, publication); err != nil {
		t.Fatalf("valid publication: %v", err)
	}
	for _, gate := range publication.Gates {
		if gate.ResultSHA != f.integration.ResultSHA || gate.GateTaskAttempt != 1 || gate.EvidenceSHA256 == "" {
			t.Fatalf("gate=%+v", gate)
		}
		if gate.Kind == "review" && gate.ReviewerID != "independent-reviewer" {
			t.Fatal("review identity missing")
		}
	}
	first := publication.Gates[0]
	again, err := f.e.RecordContributionGateFromRun(f.integration.ID, first.GateRunID, first.GateNodeID, first.Kind, first.ArtifactID)
	if err != nil || len(again.Gates) != 3 || again.State != "ready" {
		t.Fatalf("gate replay=%+v err=%v", again, err)
	}
}

func TestContributionGatesRejectForgedMissingAndRelabeledEvidence(t *testing.T) {
	f := newContributionGateFixture(t)
	task := f.gate(t, "fast", "fast", false, nil)
	for _, args := range [][5]string{
		{strings.Repeat("0", 64), task.RunID, task.NodeID, "fast", "report-fast"},
		{f.integration.ID, "missing-run", task.NodeID, "fast", "report-fast"},
		{f.integration.ID, task.RunID, "missing-node", "fast", "report-fast"},
		{f.integration.ID, task.RunID, task.NodeID, "fast", "missing-artifact"},
		{f.integration.ID, task.RunID, task.NodeID, "caller-says-pass", "report-fast"},
		{f.integration.ID, f.controller.ID, task.NodeID, "fast", "report-fast"},
	} {
		if _, err := f.e.RecordContributionGateFromRun(args[0], args[1], args[2], args[3], args[4]); err == nil {
			t.Fatalf("forged gate accepted: %v", args)
		}
	}
	publication := f.record(t, task, "fast")
	if len(publication.Gates) != 1 || publication.State != "gating" {
		t.Fatal("one test gate granted readiness")
	}
	if _, err := f.e.RecordContributionGateFromRun(f.integration.ID, task.RunID, task.NodeID, "slow", "report-fast"); err == nil {
		t.Fatal("same task relabeled fast and slow")
	}
}

func TestContributionGatesRejectInvalidRunTaskAndArtifact(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		changeCode bool
		mutate     func(*domain.RunnerTask, *domain.Run)
	}{
		{"code-mutated", "fast", true, nil},
		{"wrong-sha-pin", "fast", false, func(_ *domain.RunnerTask, r *domain.Run) {
			r.ExecutionContext.WorkspaceBaseSHAs["repo"] = strings.Repeat("f", 40)
		}},
		{"missing-context", "fast", false, func(task *domain.RunnerTask, _ *domain.Run) { task.Invocation.WorkspaceIsolation = nil }},
		{"cancelled", "fast", false, func(_ *domain.RunnerTask, r *domain.Run) { r.CancelRequested = true }},
		{"cross-project", "fast", false, func(_ *domain.RunnerTask, r *domain.Run) { r.ExecutionContext.ProjectID = "other" }},
		{"wrong-node-type", "fast", false, func(_ *domain.RunnerTask, r *domain.Run) { r.WorkflowSnapshot.Nodes[0].Type = "approval" }},
		{"wrong-test-provider", "fast", false, func(task *domain.RunnerTask, _ *domain.Run) { task.Provider = "codex" }},
		{"empty-test-command", "fast", false, func(task *domain.RunnerTask, _ *domain.Run) { task.Command = "" }},
		{"corrupt-artifact-hash", "fast", false, func(_ *domain.RunnerTask, r *domain.Run) { r.Artifacts[0].SHA256 = strings.Repeat("0", 64) }},
		{"corrupt-artifact-size", "fast", false, func(_ *domain.RunnerTask, r *domain.Run) { r.Artifacts[0].Size++ }},
		{"artifact-old-attempt", "fast", false, func(_ *domain.RunnerTask, r *domain.Run) { r.Artifacts[0].Attempt = 2 }},
		{"artifact-wrong-node", "fast", false, func(_ *domain.RunnerTask, r *domain.Run) { r.Artifacts[0].NodeID = "other" }},
		{"artifact-empty", "fast", false, func(_ *domain.RunnerTask, r *domain.Run) { r.Artifacts[0].Content = "" }},
		{"node-missing-attempt", "fast", false, func(_ *domain.RunnerTask, r *domain.Run) { r.Nodes["implement"].Attempt = 0 }},
		{"review-author-reused", "review", false, func(task *domain.RunnerTask, r *domain.Run) {
			task.Invocation.Agent.ID = "author-agent"
			r.WorkflowSnapshot.Nodes[0].AgentID = "author-agent"
		}},
		{"review-wrong-agent", "review", false, func(task *domain.RunnerTask, _ *domain.Run) { task.Invocation.Agent.ID = "different-reviewer" }},
		{"review-terminal", "review", false, func(task *domain.RunnerTask, _ *domain.Run) { task.Provider = "terminal" }},
		{"review-mock", "review", false, func(task *domain.RunnerTask, _ *domain.Run) { task.Provider = "mock" }},
		{"review-no-session", "review", false, func(_ *domain.RunnerTask, r *domain.Run) { r.Nodes["implement"].SessionID = "" }},
		{"review-no-decision", "review", false, func(_ *domain.RunnerTask, r *domain.Run) { r.Nodes["implement"].GateResult = nil }},
		{"review-rework", "review", false, func(_ *domain.RunnerTask, r *domain.Run) { r.Nodes["implement"].GateResult.Decision = "rework" }},
		{"review-blocking-finding", "review", false, func(_ *domain.RunnerTask, r *domain.Run) {
			r.Nodes["implement"].GateResult.Findings = []domain.GateFinding{{Severity: " HIGH ", Title: "blocking defect"}}
		}},
		{"review-wrong-node-type", "review", false, func(_ *domain.RunnerTask, r *domain.Run) { r.WorkflowSnapshot.Nodes[0].Type = "script" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContributionGateFixture(t)
			task := f.gate(t, "invalid", tc.kind, tc.changeCode, tc.mutate)
			if _, err := f.e.RecordContributionGateFromRun(f.integration.ID, task.RunID, task.NodeID, tc.kind, "report-invalid"); err == nil {
				t.Fatal("invalid gate evidence accepted")
			}
		})
	}
}

func TestContributionGatesRevalidateStaleAndTamperedPublication(t *testing.T) {
	f := newContributionGateFixture(t)
	publication := f.ready(t)
	for _, mutate := range []func(*domain.ContributionPublication){
		func(p *domain.ContributionPublication) { p.ID = strings.Repeat("0", 64) },
		func(p *domain.ContributionPublication) { p.RunID = "other" },
		func(p *domain.ContributionPublication) { p.RepositoryID = "other" },
		func(p *domain.ContributionPublication) { p.Branch = "main" },
		func(p *domain.ContributionPublication) { p.TargetBranch = "other" },
		func(p *domain.ContributionPublication) { p.Gates[0].Status = "failed" },
		func(p *domain.ContributionPublication) { p.Gates[0].ResultSHA = strings.Repeat("0", 40) },
		func(p *domain.ContributionPublication) { p.Gates[0].EvidenceRef = "caller-attested" },
		func(p *domain.ContributionPublication) { p.Gates[2].ReviewerID = "author-agent" },
	} {
		changed := publication
		changed.Gates = append([]domain.ContributionGateEvidence(nil), publication.Gates...)
		mutate(&changed)
		if err := f.e.validateContributionPublication(f.integration, changed); err == nil {
			t.Fatal("tampered publication accepted")
		}
	}
	fast := publication.Gates[0]
	relabeled, err := f.e.contributionGateFromRun(f.integration, fast.GateRunID, fast.GateNodeID, "slow", fast.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	duplicated := publication
	duplicated.Gates = append([]domain.ContributionGateEvidence(nil), publication.Gates...)
	duplicated.Gates[1] = relabeled
	if err := f.e.validateContributionPublication(f.integration, duplicated); err == nil {
		t.Fatal("one task reused across test gate kinds at publication boundary")
	}
	gate := publication.Gates[0]
	if err := f.s.PutRunnerTask(domain.RunnerTask{ID: "superseding-gate", RunID: gate.GateRunID, NodeID: gate.GateNodeID, Provider: "terminal", CreatedAt: time.Now().UTC().Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.validateContributionPublication(f.integration, publication); err == nil {
		t.Fatal("superseded gate publication accepted")
	}
}

func TestContributionGatesRejectReportChangedAfterRecording(t *testing.T) {
	f := newContributionGateFixture(t)
	publication := f.ready(t)
	gate := publication.Gates[0]
	run, _ := f.s.RunState(gate.GateRunID)
	artifact, _ := f.s.Artifact(gate.GateRunID, gate.ArtifactID)
	artifact.Content = "Replacement report with a valid content hash\n"
	sum := sha256.Sum256([]byte(artifact.Content))
	artifact.SHA256 = hex.EncodeToString(sum[:])
	artifact.Size = int64(len(artifact.Content))
	run.Artifacts = []domain.Artifact{artifact}
	if err := f.s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if err := f.e.validateContributionPublication(f.integration, publication); err == nil {
		t.Fatal("publication reused a report that changed after recording")
	}
}
