package repo

import (
	"errors"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestSkillCandidateKnownRunDistributionAndSupersession(t *testing.T) {
	for _, operation := range []string{"copy", "edit", "export"} {
		t.Run(operation, func(t *testing.T) {
			s := intelligenceStore(t)
			policy := &domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "allow"}
			if operation == "copy" {
				policy.Copy = "deny"
			}
			if operation == "edit" {
				policy.Edit = "deny"
			}
			if operation == "export" {
				policy.Export = "deny"
			}
			context := &domain.ExecutionIntelligenceContext{Version: 1, ProjectID: "team-a", Configuration: domain.TeamReleasePayload{Assets: domain.AgentAssetBundle{Rules: []domain.RuleDefinition{{ID: "known-input", ProjectID: "team-a", Content: "retained source", DistributionState: domain.DistributionState{Distribution: policy}}}}}}
			context.SHA256, _ = context.Digest()
			run := domain.Run{ID: "actual-run", Intelligence: context}
			run.ExecutionContext.ProjectID = "team-a"
			pkg := candidatePackageFixture()
			pkg.Source.RunID = run.ID
			if _, err := s.CreateSkillCandidate("team-a", pkg, "member"); !errors.Is(err, ErrIntelligenceUnavailable) {
				t.Fatal("unresolved Run body granted known-source authority")
			}
			candidate, err := s.CreateSkillCandidateFromRun("team-a", pkg, "member", run)
			if operation != "export" {
				if !errors.Is(err, domain.ErrDistributionDenied) {
					t.Fatalf("restricted %s candidate: %v", operation, err)
				}
				values, _ := s.SkillCandidates("team-a")
				if len(values) != 0 {
					t.Fatal("denied source produced candidate")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(candidate.Package.Skill.DistributionOrigins) != 1 || candidate.Package.Skill.DistributionState.Effective().Export != "deny" {
				t.Fatal("candidate discarded Run input floor")
			}
			resubmit := candidatePackageFixture()
			resubmit.SupersedesID = candidate.ID
			resubmit.Skill.ID = "successor-skill"
			resubmit.Skill.Content = strings.ReplaceAll(resubmit.Skill.Content, "candidate-skill", "successor-skill")
			resubmit.Skill.Files["SKILL.md"] = resubmit.Skill.Content
			next, err := s.CreateSkillCandidate("team-a", resubmit, "member")
			if err != nil {
				t.Fatal(err)
			}
			if next.Package.Skill.DistributionState.Effective().Export != "deny" {
				t.Fatal("new candidate ID cleared inherited floor")
			}
			if _, err := s.DecideSkillCandidate("team-a", next.ID, 1, "approved", "owner", "reviewed derived source and notices"); err != nil {
				t.Fatal(err)
			}
			promoted, _ := s.Skill(resubmit.Skill.ID)
			if promoted.DistributionState.Effective().Export != "deny" {
				t.Fatal("promotion lost source floor")
			}
			forged := candidatePackageFixture()
			forged.Skill.Distribution = &domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "allow"}
			if _, err := s.CreateSkillCandidate("team-a", forged, "member"); !errors.Is(err, domain.ErrDistributionInvalid) {
				t.Fatalf("caller policy accepted: %v", err)
			}
		})
	}
}
