package repo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func extractionArtifactFixture() string {
	return `{"schemaVersion":1,"suggestions":[{"key":"m","kind":"memory","title":"Budget recovery","content":"Keep cumulative usage across retry.","evidencePaths":["src/budget.go"],"licenseClaim":"unreviewed"},{"key":"s","kind":"skill","skillId":"extracted-skill","description":"Review budget recovery","files":{"SKILL.md":"---\nname: extracted-skill\ndescription: Review budget recovery\n---\nPreserve cumulative usage.\n","NOTICE.md":"Generated; human license review required."},"noticePaths":["NOTICE.md"],"evidencePaths":["src/budget.go"],"licenseClaim":"unreviewed"}]}`
}

// A durable completed execution fixture, not a claimed real model invocation.
// Mutation tests recompute manifests only when deliberately testing a valid
// changed source, never use provider calls or install a second execution loop.
func extractionFixture(t *testing.T, s *Store) (domain.KnowledgeExtractionRequest, domain.Run, domain.RunnerTask) {
	t.Helper()
	fixtureHash := sha256.Sum256([]byte(t.Name()))
	suffix := fmt.Sprintf("%x", fixtureHash[:4])
	runID, taskID, artifactID := "extraction-run-"+suffix, "extraction-task-"+suffix, "extraction-artifact-"+suffix
	now := time.Now().UTC().Truncate(time.Microsecond)
	started := now.Add(-30 * time.Second)
	deadline := now.Add(2 * time.Minute)
	if err := s.PutProject(domain.Project{ID: "team-a", Name: "Extraction", OwnerID: 1}); err != nil {
		t.Fatal(err)
	}
	agent := domain.AgentDefinition{ID: "extraction-author", Name: "Author", ProjectID: "team-a", Model: "configured-model"}
	if old, exists := s.Agent(agent.ID); exists {
		agent.SourceRevision = old.SourceRevision
	}
	if err := s.PutAgent(agent); err != nil {
		t.Fatal(err)
	}
	releases, _ := s.TeamReleases("team-a")
	release, err := s.CreateTeamRelease("team-a", domain.TeamReleaseRequest{Name: "Extraction", AgentIDs: []string{agent.ID}, ExpectedBaseVersion: len(releases)}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("1", 40)
	contract := domain.ArtifactContract{Name: "knowledge-candidates", Path: ".agentflow/artifacts/knowledge-candidates.json", MediaType: "application/json", Required: true}
	raw := extractionArtifactFixture()
	sum := sha256.Sum256([]byte(raw))
	artifact := domain.Artifact{ID: artifactID, RunID: runID, NodeID: "extract", Attempt: 1, Name: contract.Name, Path: contract.Path, MediaType: contract.MediaType, Size: int64(len(raw)), SHA256: fmt.Sprintf("%x", sum), Content: raw, CreatedAt: now}
	policy := &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 1, MaxTotalTokens: 8192}
	state, err := domain.NewExecutionBudgetState(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Begin(policy, "extraction-invocation", "extract", "codex", 1); err != nil {
		t.Fatal(err)
	}
	usage := domain.TokenUsage{TotalTokens: 100, CostStatus: "unknown"}
	if err := state.Observe(policy, "extract", "actual-session", usage); err != nil {
		t.Fatal(err)
	}
	if err := state.Finish(policy, "extraction-invocation"); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: runID, Status: "succeeded", Generation: 1, CreatedAt: started, UpdatedAt: now, ExecutionBudget: state, Repository: domain.RepositoryContext{RepositoryID: "extraction-repo", CloneURL: "https://example.com/team/repo.git", WorkspaceDir: "repo"}, ExecutionContext: domain.TelemetryContext{ProjectID: "team-a", ResponsibleUserID: 1, DeliveryPolicy: domain.DeliveryPolicyControlledPR, RunnerWorkspaceLayout: domain.RunnerWorkspaceLayoutAttempt, WorkspaceBaseSHAs: map[string]string{"extraction-repo": sha}, TeamContext: &domain.TeamContextSelection{ReleaseID: release.ID}, ExecutionBudget: policy}, WorkflowSnapshot: domain.WorkflowDefinition{Nodes: []domain.WorkflowNode{{ID: "extract", Type: "agent", AgentID: agent.ID, MaxAttempts: 1, ArtifactOutputs: []domain.ArtifactContract{contract}}}}, Nodes: map[string]*domain.NodeRun{"extract": {NodeID: "extract", Status: "succeeded", Attempt: 1, Provider: "codex", Model: "observed-model", SessionID: "actual-session", Usage: usage, StartedAt: started, FinishedAt: now}}, Artifacts: []domain.Artifact{artifact}}
	run.Intelligence = &domain.ExecutionIntelligenceContext{Version: 1, ProjectID: "team-a", ReleaseID: release.ID, ReleaseSHA256: release.SHA256, Configuration: release.Payload, Memories: []domain.GovernedMemory{}, RepositoryBaseSHAs: run.ExecutionContext.WorkspaceBaseSHAs}
	run.Intelligence.SHA256, err = run.Intelligence.Digest()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := domain.GovernedContextForRun(run)
	if err != nil {
		t.Fatal(err)
	}
	task := domain.RunnerTask{ID: taskID, RunID: run.ID, NodeID: "extract", RunnerID: "local-worker", Provider: "codex", Status: domain.RunnerTaskSuccess, Attempt: 1, CreatedAt: started, StartedAt: &started, CompletedAt: &now, MaxAttempts: 1, DeliveryPolicy: domain.DeliveryPolicyControlledPR, ExecutionDeadline: &deadline, Invocation: &domain.RunnerInvocation{InvocationID: "extraction-invocation", RunID: run.ID, NodeID: "extract", Agent: release.Payload.Agents[0], GovernedContext: binding, WorkspaceIsolation: &domain.RunnerWorkspaceIsolation{Version: 1}, ReportOutputs: true, OutputFiles: []string{contract.Path}, Artifacts: []domain.ArtifactContract{contract}, Repositories: []domain.RunnerRepository{{RepositoryID: "extraction-repo", CloneURL: run.Repository.CloneURL, Dir: "repo", BaseSHA: sha}}}, Outputs: &domain.RunnerTaskOutputs{Files: map[string]string{contract.Path: raw}}}
	// ReportOutputs uses declared workspace paths, not the legacy named upload
	// channel. Match actual Engine dispatch rather than inventing that channel.
	task.Invocation.Artifacts = nil
	placement, err := domain.CanonicalNodeExecutionPlacement(domain.NodeExecutionPlacement{Version: 1, NodeAttempt: 1, RunnerID: task.RunnerID, RegistrationSHA256: strings.Repeat("c", 64), ResponsibleUserID: 1, ProjectID: "team-a", Access: domain.NodePlacementAccessPersonal, OS: "darwin", Arch: "arm64", RunnerVersion: "0.13.0", Runtimes: []domain.NodeRuntimeSnapshot{{Provider: "codex", Version: "fixture-cli", BinaryPath: "/fixture/codex", AuthMode: domain.RuntimeAuthLocal}}})
	if err != nil {
		t.Fatal(err)
	}
	task.ExecutionPlacement = &placement
	task.Execution = &domain.NodeTaskExecution{Role: domain.NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: 1}
	run.Nodes["extract"].ExecutionDeadline = &deadline
	run.Nodes["extract"].ExecutionPlacement = &placement
	run.Nodes["extract"].ExecutionTaskID = task.ID
	run.Nodes["extract"].ExecutionSequence = 1
	run.ExecutionContext.NodePlacements = map[string]domain.NodePlacementRequest{"extract": {RunnerID: task.RunnerID}}
	task.ContributionSpecs = []domain.RunnerContributionSpec{{RepositoryID: "extraction-repo", Dir: "repo", BaseSHA: sha, MaxBytes: domain.RunnerContributionMaxBytes}}
	hash, err := domain.RunnerContributionContextSHA(task)
	if err != nil {
		t.Fatal(err)
	}
	task.Invocation.WorkspaceIsolation.ContextSHA256 = hash
	task.ContributionSpecs[0].ContextSHA256 = hash
	task.AcceptedContributions = []domain.RunnerContributionReceipt{{ID: "accepted-extraction", TaskID: task.ID, Attempt: 1, RepositoryID: "extraction-repo", Dir: "repo", BaseSHA: sha, ResultSHA: sha, ContextSHA256: hash, SHA256: strings.Repeat("b", 64), Size: 1, AcceptedAt: now}}
	task.Outputs.ContextEvidence = &domain.RunnerContextEvidence{Version: 1, TaskID: task.ID, Attempt: 1, IntelligenceSHA256: run.Intelligence.SHA256, ContextSHA256: hash, Files: []domain.RunnerMaterializedDigest{}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	// PutRun correctly refuses to mint node-task authority. Seed the completed
	// fixture's projection as the existing queue/acceptance transaction would.
	if err := s.DB.Model(&runNodeRow{}).Where("run_id = ? AND node_id = ?", run.ID, "extract").Update("payload", jsonBytes(run.Nodes["extract"])).Error; err != nil {
		t.Fatal(err)
	}
	saveExtractionTaskFixture(t, s, task)
	return domain.KnowledgeExtractionRequest{RunID: run.ID, NodeID: "extract", ArtifactID: artifact.ID, ExpectedArtifactSHA256: artifact.SHA256, RepositoryID: "extraction-repo", ExpectedCommit: sha}, run, task
}
func saveExtractionTaskFixture(t *testing.T, s *Store, task domain.RunnerTask) {
	t.Helper()
	if err := s.DB.Where("id = ?", task.ID).Delete(&runnerTaskRow{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&runnerTaskRow{ID: task.ID, Provider: task.Provider, RunnerID: task.RunnerID, Status: task.Status, Attempt: task.Attempt, MaxAttempts: task.MaxAttempts, Payload: runnerTaskPayloadBytes(task), Created: &task.CreatedAt, Updated: &task.CreatedAt}).Error; err != nil {
		t.Fatal(err)
	}
}
func extractionCounts(t *testing.T, s *Store) (int64, int64, int64) {
	t.Helper()
	var a, b, c int64
	s.DB.Model(&knowledgeExtractionRow{}).Count(&a)
	s.DB.Model(&governedMemoryRow{}).Count(&b)
	s.DB.Model(&skillCandidateRow{}).Count(&c)
	return a, b, c
}
func TestKnowledgeExtractionAtomicQuarantineReplayAndProvenance(t *testing.T) {
	s := intelligenceStore(t)
	in, _, _ := extractionFixture(t, s)
	v, replay, err := s.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true})
	if err != nil || replay || len(v.Items) != 2 {
		t.Fatalf("create replay=%v err=%v", replay, err)
	}
	if v.Source.ObservedModel != "observed-model" || v.Source.ConfiguredModel != "configured-model" || v.Source.Usage.CostStatus != "unknown" || v.Source.TaskAttempt != 1 || v.Source.BudgetReceipt.SessionID != "actual-session" {
		t.Fatal("source observations lost")
	}
	m, err := s.GovernedMemory("team-a", v.Items[0].CandidateID)
	if err != nil || m.State != domain.MemoryCandidate || m.Source.ExtractionID != v.ID {
		t.Fatalf("memory quarantine: %v", err)
	}
	skill, err := s.SkillCandidate("team-a", v.Items[1].CandidateID)
	if err != nil || skill.State != domain.SkillCandidateQuarantined || skill.Package.Source.ExtractionID != v.ID || !strings.HasPrefix(skill.Package.License, "UNREVIEWED:") {
		t.Fatalf("skill quarantine: %v", err)
	}
	if _, ok := s.Skill("extracted-skill"); ok {
		t.Fatal("ingest promoted skill")
	}
	hits, err := s.RecallGovernedMemory("team-a", "cumulative", 5)
	if err != nil || len(hits) != 0 {
		t.Fatal("candidate leaked into recall")
	}
	if _, err := s.DecideGovernedMemory("team-a", m.ID, 1, domain.MemoryRejected, "owner", "reviewed and rejected"); err != nil {
		t.Fatal(err)
	}
	second, replay, err := s.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true})
	if err != nil || !replay || second.ID != v.ID || second.CreatedBy != "local" {
		t.Fatalf("replay: %v", err)
	}
	m, _ = s.GovernedMemory("team-a", m.ID)
	if m.State != domain.MemoryRejected || m.Revision != 2 {
		t.Fatal("replay overwrote decision")
	}
	if a, b, c := extractionCounts(t, s); a != 1 || b != 1 || c != 1 {
		t.Fatalf("duplicates: %d/%d/%d", a, b, c)
	}
	if _, err := s.KnowledgeExtraction("team-b", v.ID); !errors.Is(err, ErrIntelligenceUnavailable) {
		t.Fatal("cross-project receipt exposed")
	}
	manual := domain.GovernedMemory{Title: "Forged", Content: "x", Source: m.Source}
	if _, err := s.CreateMemoryCandidate("team-a", manual, "member"); err == nil {
		t.Fatal("manual memory forged receipt")
	}
	if _, err := s.CreateSkillCandidate("team-a", skill.Package, "member"); err == nil {
		t.Fatal("manual skill forged receipt")
	}
}
func TestKnowledgeExtractionRejectsInvalidSourceWithoutPartialWrites(t *testing.T) {
	for _, name := range []string{"foreign", "artifact-hash", "attempt", "snapshot-contract-name", "snapshot-contract-path", "snapshot-contract-media", "snapshot-contract-required", "task-output-declaration", "task-output", "latest-task", "mock", "receipt-commit", "budget-invocation", "restricted-frozen", "restricted-current", "origin-frozen", "cancelled", "deadline", "budget-limit", "upstream", "revoked-release", "unknown-json"} {
		t.Run(name, func(t *testing.T) {
			s := intelligenceStore(t)
			in, run, task := extractionFixture(t, s)
			switch name {
			case "foreign":
				run.ExecutionContext.ProjectID = "team-b"
			case "artifact-hash":
				run.Artifacts[0].Content += " "
			case "attempt":
				run.Nodes["extract"].Attempt = 2
			case "snapshot-contract-name":
				run.WorkflowSnapshot.Nodes[0].ArtifactOutputs[0].Name = "other"
			case "snapshot-contract-path":
				run.WorkflowSnapshot.Nodes[0].ArtifactOutputs[0].Path = ".agentflow/artifacts/other.json"
			case "snapshot-contract-media":
				run.WorkflowSnapshot.Nodes[0].ArtifactOutputs[0].MediaType = "text/plain"
			case "snapshot-contract-required":
				run.WorkflowSnapshot.Nodes[0].ArtifactOutputs[0].Required = false
			case "task-output-declaration":
				task.Invocation.OutputFiles = []string{".agentflow/artifacts/other.json"}
			case "task-output":
				task.Outputs.Files[run.Artifacts[0].Path] = "{}"
			case "latest-task":
				task.ID = "superseding-task"
				task.CreatedAt = task.CreatedAt.Add(time.Second)
			case "mock":
				task.Provider = "mock"
			case "receipt-commit":
				task.AcceptedContributions[0].ResultSHA = strings.Repeat("2", 40)
			case "budget-invocation":
				task.Invocation.InvocationID = "other"
			case "restricted-frozen":
				run.Intelligence.Configuration.Agents[0].Distribution = &domain.DistributionPolicy{Copy: "deny", Edit: "allow", Export: "allow"}
				run.Intelligence.SHA256, _ = run.Intelligence.Digest()
			case "restricted-current":
				a, _ := s.Agent("extraction-author")
				a.Distribution = &domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "deny"}
				if _, err := s.SetDistribution("agent", a.ID, a.SourceRevision, *a.Distribution, "restrict before extraction", DistributionActor{Local: true}); err != nil {
					t.Fatal(err)
				}
			case "origin-frozen":
				run.Intelligence.Configuration.Agents[0].DistributionOrigins = []domain.DistributionOrigin{{Kind: "agent", ID: "source", SourceRevision: "r1", SHA256: strings.Repeat("a", 64), Policy: domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "allow"}}}
				run.Intelligence.SHA256, _ = run.Intelligence.Digest()
			case "cancelled":
				run.CancelRequested = true
			case "deadline":
				v := run.Nodes["extract"].StartedAt.Add(time.Second)
				task.ExecutionDeadline = &v
			case "budget-limit":
				run.ExecutionContext.ExecutionBudget.MaxTotalTokens = 262145
			case "upstream":
				run.WorkflowSnapshot.Edges = []domain.WorkflowEdge{{Source: "earlier", Target: "extract"}}
			case "revoked-release":
				if _, err := s.RevokeTeamRelease("team-a", run.Intelligence.ReleaseID, 1, "owner", "revoke before extraction"); err != nil {
					t.Fatal(err)
				}
			case "unknown-json":
				raw := strings.Replace(run.Artifacts[0].Content, `"schemaVersion":1`, `"schemaVersion":1,"approved":true`, 1)
				sum := sha256.Sum256([]byte(raw))
				run.Artifacts[0].Content = raw
				run.Artifacts[0].Size = int64(len(raw))
				run.Artifacts[0].SHA256 = fmt.Sprintf("%x", sum)
				in.ExpectedArtifactSHA256 = run.Artifacts[0].SHA256
				task.Outputs.Files[run.Artifacts[0].Path] = raw
			}
			// Test fixture updates deliberately bypass public execution mutators to
			// exercise corrupted/stale durable inputs at the read/commit boundary.
			if err := s.DB.Model(&runRow{}).Where("id = ?", run.ID).Updates(runRowValues(run)).Error; err != nil {
				t.Fatal(err)
			}
			for _, n := range run.Nodes {
				if err := s.DB.Model(&runNodeRow{}).Where("run_id = ? AND node_id = ?", run.ID, n.NodeID).Updates(map[string]any{"attempt": n.Attempt, "status": n.Status, "payload": jsonBytes(n)}).Error; err != nil {
					t.Fatal(err)
				}
			}
			a := run.Artifacts[0]
			if err := s.DB.Model(&runArtifactRow{}).Where("id = ?", a.ID).Updates(map[string]any{"content": a.Content, "size_bytes": a.Size, "sha256": a.SHA256}).Error; err != nil {
				t.Fatal(err)
			}
			saveExtractionTaskFixture(t, s, task)
			if _, _, err := s.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true}); err == nil {
				t.Fatal("invalid source accepted")
			}
			if a, b, c := extractionCounts(t, s); a != 0 || b != 0 || c != 0 {
				t.Fatalf("partial writes: %d/%d/%d", a, b, c)
			}
		})
	}
}
func TestKnowledgeExtractionRollsBackEarlierCandidateOnInsertFailure(t *testing.T) {
	s := intelligenceStore(t)
	in, _, _ := extractionFixture(t, s)
	injected := errors.New("injected candidate persistence failure")
	if err := s.DB.Callback().Create().Before("gorm:create").Register("extraction_test_fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "skill_candidates" {
			tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true})
	if !errors.Is(err, injected) {
		t.Fatalf("failure=%v", err)
	}
	_ = s.DB.Callback().Create().Remove("extraction_test_fail")
	if a, b, c := extractionCounts(t, s); a != 0 || b != 0 || c != 0 {
		t.Fatalf("failed batch partially persisted: %d/%d/%d", a, b, c)
	}
	if _, _, err := s.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true}); err != nil {
		t.Fatalf("rollback prevented valid retry: %v", err)
	}
}

func TestKnowledgeExtractionRejectsUnknownOrRestrictedInvocationClosure(t *testing.T) {
	s := intelligenceStore(t)
	_, run, task := extractionFixture(t, s)
	for _, kind := range []string{"rule", "skill", "mcp", "agent-change"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := json.Marshal(task)
			var changed domain.RunnerTask
			_ = json.Unmarshal(raw, &changed)
			switch kind {
			case "rule":
				changed.Invocation.Assets.Rules = append(changed.Invocation.Assets.Rules, domain.RuleDefinition{ID: "unfrozen", Content: "source unknown"})
			case "skill":
				changed.Invocation.Assets.Skills = append(changed.Invocation.Assets.Skills, domain.SkillDefinition{ID: "unfrozen"})
			case "mcp":
				changed.Invocation.Assets.MCPServers = append(changed.Invocation.Assets.MCPServers, domain.MCPServerDefinition{ID: "unfrozen"})
			case "agent-change":
				changed.Invocation.Agent.Prompt += "changed"
			}
			err := s.DB.Transaction(func(tx *gorm.DB) error { return s.CatalogRepo.validateExtractionTeam(tx, run, changed) })
			if !errors.Is(err, ErrExtractionSource) {
				t.Fatalf("unknown input closure accepted: %v", err)
			}
		})
	}
	for _, state := range []domain.DistributionState{
		{Distribution: &domain.DistributionPolicy{Copy: "deny", Edit: "allow", Export: "allow"}},
		{Distribution: &domain.DistributionPolicy{Copy: "allow", Edit: "deny", Export: "allow"}},
		{Distribution: &domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "deny"}},
		{DistributionOrigins: []domain.DistributionOrigin{{Kind: "skill", ID: "original", SourceRevision: "r1", SHA256: strings.Repeat("a", 64), Policy: domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "allow"}}}},
	} {
		p := domain.TeamReleasePayload{Assets: domain.AgentAssetBundle{Skills: []domain.SkillDefinition{{ID: "restricted", DistributionState: state}}}}
		if err := unrestrictedExtractionPayload(p); !errors.Is(err, ErrExtractionRestricted) {
			t.Fatal("known restriction/origin cleared")
		}
	}
}

func TestKnowledgeExtractionCancelledRequestHasNoWrites(t *testing.T) {
	s := intelligenceStore(t)
	in, _, _ := extractionFixture(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.CreateKnowledgeExtraction(ctx, "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation obscured: %v", err)
	}
	if a, b, c := extractionCounts(t, s); a != 0 || b != 0 || c != 0 {
		t.Fatal("cancelled request wrote candidates")
	}
}
