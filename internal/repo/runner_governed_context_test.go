package repo

import (
	"agentflow/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestGovernedTaskClaimRejectsLegacyRunnerAndRequiresSession(t *testing.T) {
	s, runner, _, now := newRegistryPlacementFixture(t, false)
	sha := strings.Repeat("1", 64)
	if err := s.PutProject(domain.Project{ID: "project", OwnerID: 42}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAgent(domain.AgentDefinition{ID: "context-agent", ProjectID: "project", Name: "Context Agent"}); err != nil {
		t.Fatal(err)
	}
	release, err := s.CreateTeamRelease("project", domain.TeamReleaseRequest{Name: "Context", AgentIDs: []string{"context-agent"}}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	task := domain.RunnerTask{ID: "governed-claim", RunID: "run", NodeID: "node", RunnerID: runner.ID, Provider: "terminal", Command: "true", Invocation: &domain.RunnerInvocation{RunID: "run", NodeID: "node", GovernedContext: &domain.RunnerGovernedContext{Version: 1, ProjectID: "project", ReleaseID: release.ID, ReleaseSHA256: release.SHA256, IntelligenceSHA256: sha}}}
	if err := s.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ClaimRunnerTask(runner.ID, []string{"terminal"}, now, time.Minute); err != nil || found {
		t.Fatalf("legacy unauthenticated claim leaked governed task: found=%v err=%v", found, err)
	}
	if _, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"terminal"}, now, time.Minute); err == nil || found {
		t.Fatalf("old runner admitted: found=%v err=%v", found, err)
	}
	runner.RunnerVersion = domain.RunnerVersionGovernedContext
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"terminal"}, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("supported authenticated claim: found=%v err=%v", found, err)
	}
	if err := s.CompleteRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, domain.RunnerTaskSuccess, nil, "", "", nil, nil, now); err == nil {
		t.Fatal("success without consumed context proof accepted")
	}
	digest, _ := domain.RunnerContributionContextSHA(claimed)
	outputs := &domain.RunnerTaskOutputs{ContextEvidence: &domain.RunnerContextEvidence{Version: 1, TaskID: task.ID, Attempt: claimed.Attempt + 1, IntelligenceSHA256: sha, ContextSHA256: digest}}
	if err := s.CompleteRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, domain.RunnerTaskSuccess, nil, "", "", nil, outputs, now); err == nil {
		t.Fatal("wrong attempt context proof accepted")
	}
	outputs.ContextEvidence.Attempt = claimed.Attempt
	if err := s.CompleteRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, domain.RunnerTaskSuccess, nil, "", "", nil, outputs, now); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.RunnerTask(task.ID)
	if stored.Outputs == nil || stored.Outputs.ContextEvidence == nil || stored.Outputs.ContextEvidence.ContextSHA256 != digest {
		t.Fatal("consumption evidence not durable")
	}
}

func TestGovernedQueuedClaimRechecksRevocationAndExpiry(t *testing.T) {
	for _, which := range []string{"release-revoked", "memory-revoked", "memory-expired"} {
		t.Run(which, func(t *testing.T) {
			s, runner, _, now := newRegistryPlacementFixture(t, false)
			runner.RunnerVersion = domain.RunnerVersionGovernedContext
			if err := s.PutRunner(runner); err != nil {
				t.Fatal(err)
			}
			if err := s.PutProject(domain.Project{ID: "project", OwnerID: 42}); err != nil {
				t.Fatal(err)
			}
			if err := s.PutAgent(domain.AgentDefinition{ID: "context-agent", ProjectID: "project", Name: "Context Agent"}); err != nil {
				t.Fatal(err)
			}
			release, err := s.CreateTeamRelease("project", domain.TeamReleaseRequest{Name: "Context", AgentIDs: []string{"context-agent"}}, "owner")
			if err != nil {
				t.Fatal(err)
			}
			expires := time.Now().UTC().Add(time.Hour)
			memory, err := s.CreateMemoryCandidate("project", domain.GovernedMemory{Title: "Reviewed", Content: "approved evidence", Source: domain.IntelligenceSource{Path: "docs/evidence.md"}, ExpiresAt: &expires}, "owner")
			if err != nil {
				t.Fatal(err)
			}
			memory, err = s.DecideGovernedMemory("project", memory.ID, memory.Revision, domain.MemoryApproved, "reviewer", "checked")
			if err != nil {
				t.Fatal(err)
			}
			binding := &domain.RunnerGovernedContext{Version: 1, ProjectID: "project", ReleaseID: release.ID, ReleaseSHA256: release.SHA256, IntelligenceSHA256: strings.Repeat("1", 64), Memories: []domain.RunnerGovernedMemory{{ID: memory.ID, Revision: memory.Revision, SHA256: memory.SHA256}}}
			task := domain.RunnerTask{ID: "revoked-task", RunID: "run", NodeID: "node", RunnerID: runner.ID, Provider: "terminal", Command: "true", Invocation: &domain.RunnerInvocation{RunID: "run", NodeID: "node", GovernedContext: binding}}
			if err := s.PutRunnerTask(task); err != nil {
				t.Fatal(err)
			}
			if which == "release-revoked" {
				_, err = s.RevokeTeamRelease("project", release.ID, release.Revision, "reviewer", "revoked before claim")
			}
			if which == "memory-revoked" {
				_, err = s.DecideGovernedMemory("project", memory.ID, memory.Revision, domain.MemoryRevoked, "reviewer", "outdated")
			}
			if which == "memory-expired" {
				// Preserve payload/content digest while advancing the authoritative claim
				// clock through the row's legitimate future expiry.
				now = expires.Add(time.Minute)
				sessionExpires := now.Add(time.Hour)
				runner.SessionExpiresAt = &sessionExpires
				if err := s.PutRunner(runner); err != nil {
					t.Fatal(err)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"terminal"}, now, time.Minute); err == nil || found {
				t.Fatalf("unavailable governance claimed: found=%v err=%v", found, err)
			}
			stored, _ := s.RunnerTask(task.ID)
			if stored.Status != domain.RunnerTaskQueued || stored.Attempt != 0 {
				t.Fatal("rejected claim mutated assignment")
			}
		})
	}
}
