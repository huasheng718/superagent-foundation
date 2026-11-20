package repo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func extractionRunWithMemory(run domain.Run, memory domain.GovernedMemory) domain.Run {
	selection := *run.ExecutionContext.TeamContext
	selection.MemoryIDs = []string{memory.ID}
	selection.MemoryRecallReceiptID = ""
	run.ExecutionContext.TeamContext = &selection
	intelligence := *run.Intelligence
	intelligence.Memories = []domain.GovernedMemory{memory}
	intelligence.MemoryRecallReceipt = nil
	run.Intelligence = &intelligence
	return run
}

func TestKnowledgeExtractionManualRunMemoryCannotProveUnrestrictedSource(t *testing.T) {
	s := intelligenceStore(t)
	in, run, _ := extractionFixture(t, s)
	agent, _ := s.Agent("extraction-author")
	if _, err := s.SetDistribution("agent", agent.ID, agent.SourceRevision, domain.DistributionPolicy{Copy: "deny", Edit: "allow", Export: "allow"}, "restricted source Run fixture", DistributionActor{Local: true}); err != nil {
		t.Fatal(err)
	}
	memory, err := s.CreateMemoryCandidate("team-a", domain.GovernedMemory{Title: "Manually registered Run claim", Content: "Must not wash known Run restrictions", Source: domain.IntelligenceSource{RunID: in.RunID, ArtifactID: in.ArtifactID, Repository: run.Repository.CloneURL, Commit: in.ExpectedCommit, Path: run.Artifacts[0].Path}}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	memory, err = s.DecideGovernedMemory("team-a", memory.ID, 1, domain.MemoryApproved, "owner", "human review is not source-policy proof")
	if err != nil {
		t.Fatal(err)
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		return s.MemoryRepo.validateExtractionMemories(tx, extractionRunWithMemory(run, memory))
	})
	if !errors.Is(err, ErrExtractionRestricted) {
		t.Fatalf("manual Run memory gained extraction authority: %v", err)
	}
}

func TestKnowledgeExtractionIndependentHumanMemoryRemainsEligible(t *testing.T) {
	s := intelligenceStore(t)
	_, run, _ := extractionFixture(t, s)
	memory, err := s.CreateMemoryCandidate("team-a", domain.GovernedMemory{Title: "Independent declaration", Content: "A reviewed human-authored observation", Source: domain.IntelligenceSource{Path: "manual/observation.md"}}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	memory, err = s.DecideGovernedMemory("team-a", memory.ID, 1, domain.MemoryApproved, "owner", "reviewed independent declaration")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		return s.MemoryRepo.validateExtractionMemories(tx, extractionRunWithMemory(run, memory))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeExtractionTrustedMemoryReceiptAndTampering(t *testing.T) {
	for _, scenario := range []string{"valid", "digest", "policy", "source-path", "wrong-kind", "wrong-candidate", "wrong-sha", "duplicate-item", "missing", "foreign-project"} {
		t.Run(scenario, func(t *testing.T) {
			s := intelligenceStore(t)
			in, run, _ := extractionFixture(t, s)
			receipt, _, err := s.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true})
			if err != nil {
				t.Fatal(err)
			}
			memory, err := s.DecideGovernedMemory("team-a", receipt.Items[0].CandidateID, 1, domain.MemoryApproved, "owner", "reviewed fixed extraction candidate")
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "digest":
				receipt.Source.Provider = "tampered-provider"
			case "policy":
				receipt.Policy = "unbounded-unreviewed"
			case "source-path":
				receipt.Source.ArtifactPath = "other/knowledge-candidates.json"
			case "wrong-kind":
				receipt.Items[0].Kind = "skill"
			case "wrong-candidate":
				receipt.Items[0].CandidateID = "unrelated-memory"
			case "wrong-sha":
				receipt.Items[0].CandidateSHA256 = strings.Repeat("f", 64)
			case "duplicate-item":
				receipt.Items = append(receipt.Items, receipt.Items[0])
			case "missing":
				if err := s.DB.Where("id = ?", receipt.ID).Delete(&knowledgeExtractionRow{}).Error; err != nil {
					t.Fatal(err)
				}
			case "foreign-project":
				if err := s.DB.Model(&knowledgeExtractionRow{}).Where("id = ?", receipt.ID).Update("project_id", "other-project").Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "valid" && scenario != "missing" && scenario != "foreign-project" {
				if scenario != "digest" {
					receipt.SHA256, err = receipt.Digest()
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := s.DB.Model(&knowledgeExtractionRow{}).Where("id = ?", receipt.ID).Update("payload", jsonBytes(receipt)).Error; err != nil {
					t.Fatal(err)
				}
			}
			err = s.DB.Transaction(func(tx *gorm.DB) error {
				return s.MemoryRepo.validateExtractionMemories(tx, extractionRunWithMemory(run, memory))
			})
			if scenario == "valid" && err != nil {
				t.Fatalf("trusted approved candidate rejected: %v", err)
			}
			if scenario != "valid" && err == nil {
				t.Fatal("untrusted source receipt accepted")
			}
		})
	}
}
