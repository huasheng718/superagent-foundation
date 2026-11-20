package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
)

func governedFixture(t *testing.T) (*repo.Store, *Engine, domain.TeamRelease, domain.GovernedMemory, domain.Run) {
	t.Helper()
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, p := range []string{"governed-a", "governed-b"} {
		if err := s.PutProject(domain.Project{ID: p, Name: p, OwnerID: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutRule(domain.RuleDefinition{ID: "pinned-rule", Name: "Pinned rule", ProjectID: "governed-a", Content: "PINNED_ORIGINAL_RULE"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSkill(domain.SkillDefinition{ID: "pinned-skill", Name: "Pinned skill", Description: "Pinned skill", ProjectID: "governed-a", Content: "# PINNED_ORIGINAL_SKILL", Files: map[string]string{"nested/resource.txt": "PINNED_ORIGINAL_RESOURCE"}}); err != nil {
		t.Fatal(err)
	}
	initialSkill, _ := s.Skill("pinned-skill")
	if err := s.PutSkillFiles("pinned-skill", []repo.SkillFileInput{{Path: "nested/data.bin", Content: []byte{0, 255, 1}}}, initialSkill.SourceRevision); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAgent(domain.AgentDefinition{ID: "pinned-author", Name: "Pinned author", ProjectID: "governed-a", ExecutorID: "mock", Prompt: "PINNED_ORIGINAL_PROMPT", Rules: []string{"pinned-rule"}, Skills: []string{"pinned-skill"}}); err != nil {
		t.Fatal(err)
	}
	release, err := s.CreateTeamRelease("governed-a", domain.TeamReleaseRequest{Name: "Reviewed team", AgentIDs: []string{"pinned-author"}}, "human-owner")
	if err != nil {
		t.Fatal(err)
	}
	memory, err := s.CreateMemoryCandidate("governed-a", domain.GovernedMemory{Title: "Verified evidence", Content: "PINNED_APPROVED_MEMORY", Source: domain.IntelligenceSource{Path: "docs/verified.md"}}, "author")
	if err != nil {
		t.Fatal(err)
	}
	memory, err = s.DecideGovernedMemory("governed-a", memory.ID, 1, domain.MemoryApproved, "human-owner", "verified against code")
	if err != nil {
		t.Fatal(err)
	}
	engine := New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces")))
	run := domain.Run{ID: "governed-run", Status: "running", WorkflowID: "governed-workflow", WorkflowSnapshot: domain.WorkflowDefinition{ID: "governed-workflow", ProjectID: "governed-a", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{{ID: "author", Type: "agent", AgentID: "pinned-author", ExecutorID: "mock"}}}, Nodes: map[string]*domain.NodeRun{"author": {NodeID: "author", Status: "pending", Attempt: 1}}, ExecutionContext: domain.TelemetryContext{ProjectID: "governed-a", TeamContext: &domain.TeamContextSelection{ReleaseID: release.ID, MemoryIDs: []string{memory.ID}}}}
	if err := engine.FreezeRunIntelligence(&run); err != nil {
		t.Fatal(err)
	}
	return s, engine, release, memory, run
}
func TestGovernedExecutionContextPinsActualInvocationResources(t *testing.T) {
	s, e, _, memory, run := governedFixture(t)
	originalHash := run.Intelligence.SHA256
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	// Rewrite every live catalog source after assignment. Reopening the Run
	// models a later process/retry, not reuse of the original in-memory pointer.
	role, _ := s.Agent("pinned-author")
	role.Prompt = "LATEST_UNAPPROVED_PROMPT"
	role.Rules = nil
	role.Skills = nil
	if err := s.PutAgent(role); err != nil {
		t.Fatal(err)
	}
	rule, _ := s.Rule("pinned-rule")
	rule.Content = "LATEST_UNAPPROVED_RULE"
	if err := s.PutRule(rule); err != nil {
		t.Fatal(err)
	}
	skill, _ := s.Skill("pinned-skill")
	skill.Content = "# LATEST_UNAPPROVED_SKILL"
	skill.Files["nested/resource.txt"] = "LATEST_UNAPPROVED_RESOURCE"
	if err := s.PutSkill(skill); err != nil {
		t.Fatal(err)
	}
	updatedSkill, _ := s.Skill(skill.ID)
	if err := s.PutSkillFiles(skill.ID, []repo.SkillFileInput{{Path: "nested/data.bin", Content: []byte("latest")}}, updatedSkill.SourceRevision); err != nil {
		t.Fatal(err)
	}
	restored, ok := s.RunState(run.ID)
	if !ok || restored.Intelligence == nil || restored.Intelligence.SHA256 != originalHash {
		t.Fatal("Run persistence lost frozen intelligence")
	}
	agent, ok := e.RunAgent(restored, restored.WorkflowSnapshot, "pinned-author")
	if !ok || agent.Prompt != "PINNED_ORIGINAL_PROMPT" {
		t.Fatal("retry followed live role")
	}
	assets, err := e.RunAssets(restored, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets.Rules) != 2 || assets.Rules[0].Content != "PINNED_ORIGINAL_RULE" || !strings.Contains(assets.Rules[1].Content, memory.Content) {
		t.Fatal("pinned policy and reviewed memory not selected")
	}
	// Exercise the same actual materialization and process adapter used by the
	// runtime. A local shell reads exactly what a CLI receives; no model service
	// or a fabricated model response is involved in this contract test.
	manager := runtime.NewManager(t.TempDir())
	sb, err := manager.Create(context.Background(), "pinned-process", "author", domain.SandboxProfile{Driver: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close(context.Background())
	ex := domain.ExecutorDefinition{ID: "resource-reader", Kind: "resource-reader", Binary: "sh", Args: []string{"-c", `cat .agentflow/pinned-author/agent-system.md; cat .agentflow/skills/pinned-skill/nested/resource.txt`}}
	out, err := runtime.NewExecutorRunner().Run(context.Background(), sb, "pinned-invocation", ex, agent, assets, "read pinned context", runtime.InvocationOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"PINNED_ORIGINAL_RULE", "PINNED_APPROVED_MEMORY", "PINNED_ORIGINAL_RESOURCE"} {
		if !strings.Contains(out, expected) {
			t.Fatalf("runtime did not consume %s: %s", expected, out)
		}
	}
	if strings.Contains(out, "LATEST_UNAPPROVED") {
		t.Fatal("runtime received latest assets")
	}
	bytes, err := os.ReadFile(filepath.Join(sb.Workspace(), ".agentflow", "skills", "pinned-skill", "nested", "data.bin"))
	if err != nil || string(bytes) != string([]byte{0, 255, 1}) {
		t.Fatal("binary release resource changed")
	}
	// Materialization sorts/sanitizes its bundle; it must not mutate the pinned
	// snapshot shared by subsequent attempts.
	if got, _ := restored.Intelligence.Digest(); got != originalHash {
		t.Fatal("invocation mutated immutable snapshot")
	}
}
func TestGovernedExecutionContextFailsClosedOnRevocationAndTampering(t *testing.T) {
	s, e, release, memory, run := governedFixture(t)

	missing := run
	missing.Intelligence = nil
	if _, err := e.RunAssets(missing, release.Payload.Agents[0]); err == nil {
		t.Fatal("missing selected snapshot fell back to live catalog")
	}
	if _, ok := e.RunAgent(missing, run.WorkflowSnapshot, "pinned-author"); ok {
		t.Fatal("missing selected role fell back to live catalog")
	}
	bad := run
	raw, _ := json.Marshal(run.Intelligence)
	bad.Intelligence = nil
	_ = json.Unmarshal(raw, &bad.Intelligence)
	bad.Intelligence.Configuration.Agents[0].Prompt = "forged"
	if err := e.ValidateRunIntelligence(bad); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	bad = run
	bad.ExecutionContext.ProjectID = "governed-b"
	if err := e.ValidateRunIntelligence(bad); err == nil {
		t.Fatal("foreign scope accepted")
	}
	if _, err := s.DecideGovernedMemory("governed-a", memory.ID, 2, domain.MemoryRevoked, "human-owner", "outdated"); err != nil {
		t.Fatal(err)
	}
	if err := e.ValidateRunIntelligence(run); err == nil {
		t.Fatal("revoked memory still executable")
	}
	withoutMemory := run
	withoutMemory.ExecutionContext.TeamContext = &domain.TeamContextSelection{ReleaseID: release.ID}
	if err := e.FreezeRunIntelligence(&withoutMemory); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeTeamRelease("governed-a", release.ID, 1, "human-owner", "rollback"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunAssets(withoutMemory, release.Payload.Agents[0]); err == nil {
		t.Fatal("revoked release still executable")
	}
}
func TestGovernedExecutionContextRefusesCandidatesAndUnpinnedRepositories(t *testing.T) {
	s, e, release, _, run := governedFixture(t)
	candidate, err := s.CreateMemoryCandidate("governed-a", domain.GovernedMemory{Title: "Candidate", Content: "not approved", Source: domain.IntelligenceSource{Path: "docs/candidate.md"}}, "author")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResolveIntelligenceContext("governed-a", domain.TeamContextSelection{ReleaseID: release.ID, MemoryIDs: []string{candidate.ID}}, nil); err == nil {
		t.Fatal("candidate selected into execution context")
	}
	run.Repositories = []domain.RepositoryContext{{RepositoryID: "repo-unpinned"}}
	if err := e.FreezeRunIntelligence(&run); err == nil {
		t.Fatal("repository accepted without exact SHA")
	}
}

func TestGovernedSOPActuallyExecutesFrozenRoleAfterCatalogChange(t *testing.T) {
	s, e, release, memory, _ := governedFixture(t)
	wf := domain.WorkflowDefinition{ID: "governed-sop", Name: "Governed SOP", ProjectID: "governed-a", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{
		{ID: "before", Name: "Before", Type: "approval", MaxAttempts: 1},
		{ID: "author", Name: "Author", Type: "agent", AgentID: "pinned-author", ExecutorID: "mock", MaxAttempts: 1},
		{ID: "after", Name: "After", Type: "approval", MaxAttempts: 1},
	}, Edges: []domain.WorkflowEdge{{ID: "before-author", Source: "before", Target: "author", When: "success"}, {ID: "author-after", Source: "author", Target: "after", When: "success"}}}
	if err := s.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	run, err := e.StartWithExecutionContext(wf.ID, "exercise frozen context", domain.RepositoryContext{}, domain.TelemetryContext{ProjectID: "governed-a", TeamContext: &domain.TeamContextSelection{ReleaseID: release.ID, MemoryIDs: []string{memory.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, run.ID, "waiting")
	role, _ := s.Agent("pinned-author")
	role.Prompt = "LATEST_UNAPPROVED_PROMPT"
	role.Rules = nil
	role.Skills = nil
	if err := s.PutAgent(role); err != nil {
		t.Fatal(err)
	}
	rule, _ := s.Rule("pinned-rule")
	rule.Content = "LATEST_UNAPPROVED_RULE"
	if err := s.PutRule(rule); err != nil {
		t.Fatal(err)
	}
	if err := e.Approve(run.ID, "before", ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var final domain.Run
	for time.Now().Before(deadline) {
		final, _ = s.Run(run.ID)
		if final.Nodes["after"].Status == "waiting_approval" {
			break
		}
		if final.Status == "failed" {
			t.Fatalf("governed SOP failed: %+v", final.Nodes)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if final.Nodes["after"].Status != "waiting_approval" || final.Nodes["author"].Status != "succeeded" {
		t.Fatalf("governed SOP did not reach second gate: %+v", final.Nodes)
	}
	if !strings.Contains(final.Nodes["author"].Output, "PINNED_ORIGINAL_PROMPT") || strings.Contains(final.Nodes["author"].Output, "LATEST_UNAPPROVED") {
		t.Fatalf("actual Engine role prompt followed live catalog: %s", final.Nodes["author"].Output)
	}
	raw, err := os.ReadFile(filepath.Join(e.sandboxes.Root(), run.ID, ".agentflow", "config", "pinned-author", "agent-system.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "PINNED_ORIGINAL_RULE") || !strings.Contains(string(raw), "PINNED_APPROVED_MEMORY") || strings.Contains(string(raw), "LATEST_UNAPPROVED") {
		t.Fatalf("Engine runtime materialized wrong context: %s", raw)
	}
	if _, err := s.DecideGovernedMemory("governed-a", memory.ID, 2, domain.MemoryRevoked, "human-owner", "invalidate before continuing"); err != nil {
		t.Fatal(err)
	}
	if err := e.ValidateRunIntelligence(final); err == nil {
		t.Fatal("executed Run retained authority after memory revocation")
	}
	_ = e.Cancel(run.ID)
}

func TestGovernedScriptActuallyUsesFrozenMCPAfterCatalogChange(t *testing.T) {
	s, e, _, _, _ := governedFixture(t)
	// Controlled delivery deliberately strips every MCP; legacy execution is
	// used here solely to verify snapshot resolution through the actual shell.
	if err := e.ConfigureDeliveryPolicy(domain.DeliveryPolicyLegacy); err != nil {
		t.Fatal(err)
	}
	mcp := domain.MCPServerDefinition{ID: "approved-mcp", Name: "Approved MCP", Scope: "project", ProjectID: "governed-a", Transport: "http", URL: "https://approved.example.invalid/mcp"}
	if err := s.PutMCPServer(mcp); err != nil {
		t.Fatal(err)
	}
	mcp, _ = s.MCPServer(mcp.ID)
	release, err := s.CreateTeamRelease("governed-a", domain.TeamReleaseRequest{Name: "Scripts", AgentIDs: []string{"pinned-author"}, MCPServerIDs: []string{mcp.ID}, ExpectedBaseVersion: 1}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	wf := domain.WorkflowDefinition{ID: "governed-script", Name: "Governed script", ProjectID: "governed-a", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{
		{ID: "before", Type: "approval"},
		{ID: "script", Type: "script", ScriptLanguage: "shell", Script: "agentflow mcp list > approved-mcp.json", ArtifactOutputs: []domain.ArtifactContract{{Name: "mcp", Path: "approved-mcp.json", Required: true}}},
	}, Edges: []domain.WorkflowEdge{{Source: "before", Target: "script", When: "success"}}}
	if err := s.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	run, err := e.StartWithExecutionContext(wf.ID, "", domain.RepositoryContext{}, domain.TelemetryContext{ProjectID: "governed-a", TeamContext: &domain.TeamContextSelection{ReleaseID: release.ID}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, run.ID, "waiting")
	mcp.URL = "https://latest.example.invalid/mcp"
	if err := s.PutMCPServer(mcp); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMCPServer(domain.MCPServerDefinition{ID: "unapproved-mcp", Name: "Unapproved", Scope: "project", ProjectID: "governed-a", Transport: "http", URL: "https://unapproved.example.invalid/mcp"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Approve(run.ID, "before", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, run.ID, "succeeded")
	final, _ := s.Run(run.ID)
	body := ""
	for _, artifact := range final.Artifacts {
		if artifact.Name == "mcp" {
			body = artifact.Content
		}
	}
	if !strings.Contains(body, "approved.example.invalid") || strings.Contains(body, "latest.example.invalid") || strings.Contains(body, "unapproved.example.invalid") {
		t.Fatalf("actual script did not consume only approved snapshot MCP: %s", body)
	}
}

func TestGovernedRevocationBlocksScriptAfterHumanApproval(t *testing.T) {
	for _, kind := range []string{"script", "command"} {
		for _, revoked := range []string{"release", "memory"} {
			t.Run(kind+"/"+revoked, func(t *testing.T) {
				s, e, release, memory, _ := governedFixture(t)
				marker := filepath.Join(t.TempDir(), "must-not-execute")
				wf := domain.WorkflowDefinition{ID: "revoke-before-script", Name: "Revoke script", ProjectID: "governed-a", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{
					{ID: "before", Type: "approval"},
					{ID: "script", Type: kind, ScriptLanguage: "shell", Script: "touch " + shellSingleQuote(marker), Command: "touch " + shellSingleQuote(marker)},
				}, Edges: []domain.WorkflowEdge{{Source: "before", Target: "script", When: "success"}}}
				if err := s.PutWorkflow(wf); err != nil {
					t.Fatal(err)
				}
				run, err := e.StartWithExecutionContext(wf.ID, "", domain.RepositoryContext{}, domain.TelemetryContext{ProjectID: "governed-a", TeamContext: &domain.TeamContextSelection{ReleaseID: release.ID, MemoryIDs: []string{memory.ID}}})
				if err != nil {
					t.Fatal(err)
				}
				waitFor(t, s, run.ID, "waiting")
				if revoked == "release" {
					_, err = s.RevokeTeamRelease("governed-a", release.ID, release.Revision, "owner", "revoke before execution")
				} else {
					_, err = s.DecideGovernedMemory("governed-a", memory.ID, memory.Revision, domain.MemoryRevoked, "owner", "revoke before execution")
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := e.Approve(run.ID, "before", ""); err != nil {
					t.Fatal(err)
				}
				waitFor(t, s, run.ID, "failed")
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("revoked %s still executed %s: %v", revoked, kind, err)
				}
			})
		}
	}
}

func TestGovernedRunnerPreparationAndGatePinMCPAuthority(t *testing.T) {
	s, e, _, memory, run := governedFixture(t)
	if err := e.ConfigureDeliveryPolicy(domain.DeliveryPolicyLegacy); err != nil {
		t.Fatal(err)
	}
	mcp := domain.MCPServerDefinition{ID: "approved-script-mcp", Name: "Approved", Scope: "project", ProjectID: "governed-a", Transport: "http", URL: "https://approved.example.invalid/mcp"}
	if err := s.PutMCPServer(mcp); err != nil {
		t.Fatal(err)
	}
	mcp, _ = s.MCPServer(mcp.ID)
	release, err := s.CreateTeamRelease("governed-a", domain.TeamReleaseRequest{Name: "Runner scripts", AgentIDs: []string{"pinned-author"}, MCPServerIDs: []string{mcp.ID}, ExpectedBaseVersion: 1}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext.TeamContext.ReleaseID = release.ID
	run.ExecutionContext.DeliveryPolicy = domain.DeliveryPolicyLegacy
	if err := e.FreezeRunIntelligence(&run); err != nil {
		t.Fatal(err)
	}
	mcp.URL = "https://latest.example.invalid/mcp"
	if err := s.PutMCPServer(mcp); err != nil {
		t.Fatal(err)
	}
	assertPinned := func(servers []domain.MCPServerDefinition) {
		t.Helper()
		if len(servers) != 1 || servers[0].URL != "https://approved.example.invalid/mcp" {
			t.Fatalf("unfrozen MCP authority: %+v", servers)
		}
	}
	assertPinned(e.resolveRunCredentials(run).MCPServers)
	invocation, err := e.gateCommandInvocation(run, "author")
	if err != nil {
		t.Fatal(err)
	}
	assertPinned(invocation.Assets.MCPServers)
	assertPinned(invocation.MCPCallServers)
	request, err := e.preparedWorkspaceRequest(run, domain.RunnerWorkspaceRequest{Operation: domain.RunnerWorkspaceOperationPrepare})
	if err != nil {
		t.Fatal(err)
	}
	assertPinned(request.PreparationInvocation.Assets.MCPServers)
	assertPinned(request.PreparationInvocation.MCPCallServers)
	// The pinned resource set never grants the stronger controlled delivery
	// path any MCP authority: its existing deny-all policy still wins.
	run.ExecutionContext.DeliveryPolicy = domain.DeliveryPolicyControlledPR
	invocation, err = e.gateCommandInvocation(run, "author")
	if err != nil {
		t.Fatal(err)
	}
	if len(invocation.Assets.MCPServers) != 0 || len(invocation.MCPCallServers) != 0 {
		t.Fatal("snapshot bypassed controlled MCP restriction")
	}
	if _, err := s.DecideGovernedMemory("governed-a", memory.ID, memory.Revision, domain.MemoryRevoked, "owner", "stop pending command"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.gateCommandInvocation(run, "author"); err == nil {
		t.Fatal("revoked memory retained Runner gate authority")
	}
	if _, err := e.preparedWorkspaceRequest(run, domain.RunnerWorkspaceRequest{Operation: domain.RunnerWorkspaceOperationPrepare}); err == nil {
		t.Fatal("revoked memory retained Runner preparation authority")
	}
}
