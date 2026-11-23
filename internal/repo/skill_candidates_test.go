package repo

import (
	"agentflow/internal/domain"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"gorm.io/gorm"
	"strings"
	"sync"
	"testing"
)

func candidatePackageFixture() domain.SkillCandidatePackage {
	raw := "---\nname: candidate-skill\ndescription: Read reviewed resources.\n---\n\n# Reviewed skill\n"
	data := []byte{0, 255, 128, 1}
	executable := []byte("#!/bin/sh\necho reviewed\n")
	refs := []domain.SkillFileRef{}
	binary := map[string][]byte{"assets/nested/data.bin": data, "scripts/check.sh": executable}
	for path, b := range binary {
		sum := sha256.Sum256(b)
		mode := uint32(0o644)
		if strings.HasPrefix(path, "scripts/") {
			mode = 0o755
		}
		refs = append(refs, domain.SkillFileRef{Path: path, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:]), Mode: mode})
	}
	return domain.SkillCandidatePackage{Skill: domain.SkillDefinition{ID: "candidate-skill", Name: "Reviewed candidate", ProjectID: "team-a", Content: raw, Files: map[string]string{"SKILL.md": raw, "LICENSE": "Original MIT license text\n", "NOTICE": "Original attribution\n", "nested/reference.md": "reviewed nested bytes\n"}, FileRefs: refs}, Binary: binary, Publisher: "Original publisher", License: "MIT", NoticePaths: []string{"LICENSE", "NOTICE"}, Source: domain.IntelligenceSource{Repository: "https://example.com/team/source", Commit: strings.Repeat("1", 40), Path: "skills/candidate-skill"}}
}
func TestSkillCandidateQuarantineHumanPromotionPreservesCompletePackage(t *testing.T) {
	s := intelligenceStore(t)
	input := candidatePackageFixture()
	candidate, err := s.CreateSkillCandidate("team-a", input, "member")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != domain.SkillCandidateQuarantined || candidate.CreatedBy != "member" {
		t.Fatal("submission bypassed quarantine")
	}
	if _, found := s.Skill(input.Skill.ID); found {
		t.Fatal("quarantined candidate entered active catalog")
	}
	if _, err := s.CreateTeamRelease("team-a", domain.TeamReleaseRequest{Name: "Unreviewed", SkillIDs: []string{input.Skill.ID}}, "owner"); err == nil {
		t.Fatal("candidate published through release")
	}
	approved, err := s.DecideSkillCandidate("team-a", candidate.ID, 1, "approved", "owner", "reviewed original instructions, binary and license")
	if err != nil {
		t.Fatal(err)
	}
	if approved.State != "approved" || approved.Revision != 2 || approved.SHA256 != candidate.SHA256 {
		t.Fatal("approval changed retained source")
	}
	stored, err := s.SkillCandidate("team-a", candidate.ID)
	if err != nil || stored.Package.Skill.Files["SKILL.md"] != input.Skill.Content {
		t.Fatal("approval mutated retained package")
	}
	skill, found := s.Skill(input.Skill.ID)
	if !found || skill.ProjectID != "team-a" || skill.Shared || skill.Source == nil || skill.Source.ReviewedCandidate.License != "MIT" || skill.Source.ReviewedCandidate.CandidateID != candidate.ID {
		t.Fatal("source/license/scope lost on promotion")
	}
	if skill.Files["LICENSE"] != input.Skill.Files["LICENSE"] || skill.Files["NOTICE"] != input.Skill.Files["NOTICE"] || skill.Files["nested/reference.md"] != input.Skill.Files["nested/reference.md"] {
		t.Fatal("text resources lost")
	}
	for _, ref := range skill.FileRefs {
		data, ok := s.SkillFileContent(skill.ID, ref.Path)
		var original domain.SkillFileRef
		for _, source := range input.Skill.FileRefs {
			if source.Path == ref.Path {
				original = source
			}
		}
		if !ok || string(data) != string(input.Binary[ref.Path]) || ref.Mode != original.Mode || ref.Size != original.Size || ref.SHA256 != original.SHA256 {
			t.Fatal("binary bytes/mode changed")
		}
	}
	if _, err := s.DecideSkillCandidate("team-a", candidate.ID, 1, "approved", "owner", "stale"); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatalf("stale decision=%v", err)
	}
	if _, err := s.DecideSkillCandidate("team-a", candidate.ID, 2, "withdrawn", "owner", "erase approval"); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatal("approved history rewritten")
	}
	releases, _ := s.TeamReleases("team-a")
	if len(releases) != 0 {
		t.Fatal("candidate approval auto-published team release")
	}
	release, err := s.CreateTeamRelease("team-a", domain.TeamReleaseRequest{Name: "Explicit release", SkillIDs: []string{skill.ID}}, "owner")
	if err != nil || release.Payload.Assets.Skills[0].Source.ReviewedCandidate.PackageSHA256 != candidate.SHA256 {
		t.Fatalf("formal release provenance lost: %v", err)
	}
}
func TestSkillCandidateDecisionCollisionsConcurrencyAndHistory(t *testing.T) {
	s := intelligenceStore(t)
	first, err := s.CreateSkillCandidate("team-a", candidatePackageFixture(), "member")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			independent := &CatalogRepo{deps: &deps{DB: s.DB.Session(&gorm.Session{NewDB: true})}}
			_, err := independent.DecideSkillCandidate("team-a", first.ID, 1, "approved", "owner", "checked")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrIntelligenceConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("decisions wins=%d conflicts=%d", wins, conflicts)
	}
	second, err := s.CreateSkillCandidate("team-a", candidatePackageFixture(), "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideSkillCandidate("team-a", second.ID, 1, "approved", "owner", "collision"); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatalf("collision=%v", err)
	}
	unchanged, _ := s.SkillCandidate("team-a", second.ID)
	if unchanged.State != domain.SkillCandidateQuarantined || unchanged.Revision != 1 {
		t.Fatal("failed promotion partially decided")
	}
	for _, decision := range []string{"rejected", "withdrawn"} {
		p := candidatePackageFixture()
		p.Skill.ID = "candidate-" + decision
		p.Skill.Content = strings.ReplaceAll(p.Skill.Content, "candidate-skill", p.Skill.ID)
		p.Skill.Files["SKILL.md"] = p.Skill.Content
		p.SupersedesID = second.ID
		v, err := s.CreateSkillCandidate("team-a", p, "member")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DecideSkillCandidate("team-a", v.ID, 1, decision, "owner", "not approved"); err != nil {
			t.Fatal(err)
		}
		if _, found := s.Skill(p.Skill.ID); found {
			t.Fatal("nonapproval created active skill")
		}
	}
	if _, err := s.SkillCandidate("team-b", first.ID); !errors.Is(err, ErrIntelligenceUnavailable) {
		t.Fatal("crossproject candidate readable")
	}
}
func TestSkillCandidateInvalidResourcesNeverEnterActiveState(t *testing.T) {
	for name, mutate := range map[string]func(*domain.SkillCandidatePackage){
		"traversal":      func(p *domain.SkillCandidatePackage) { p.Skill.Files["../escape"] = "bad" },
		"case":           func(p *domain.SkillCandidatePackage) { p.Skill.Files["license"] = "collision" },
		"file-directory": func(p *domain.SkillCandidatePackage) { p.Skill.Files["nested"] = "collision" },
		"digest":         func(p *domain.SkillCandidatePackage) { p.Skill.FileRefs[0].SHA256 = strings.Repeat("0", 64) },
		"symlink-mode":   func(p *domain.SkillCandidatePackage) { p.Skill.FileRefs[0].Mode = 0o120777 },
		"utf8":           func(p *domain.SkillCandidatePackage) { p.Skill.Files["bad.txt"] = string([]byte{255}) },
		"oversize": func(p *domain.SkillCandidatePackage) {
			p.Skill.Files["large"] = strings.Repeat("x", int(domain.SkillFileMaxBytes)+1)
		},
		"missing-notice":  func(p *domain.SkillCandidatePackage) { p.NoticePaths = []string{"absent-NOTICE"} },
		"unlisted-binary": func(p *domain.SkillCandidatePackage) { p.Binary["unlisted"] = []byte("bad") },
		"identity":        func(p *domain.SkillCandidatePackage) { p.Skill.ID = "Candidate_Skill" },
		"source-revision": func(p *domain.SkillCandidatePackage) { p.Skill.SourceRevision = "forged-observed-source" },
		"wrong-frontmatter": func(p *domain.SkillCandidatePackage) {
			p.Skill.Content = strings.ReplaceAll(p.Skill.Content, "name: candidate-skill", "name: other")
			p.Skill.Files["SKILL.md"] = p.Skill.Content
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := intelligenceStore(t)
			p := candidatePackageFixture()
			mutate(&p)
			if _, err := s.CreateSkillCandidate("team-a", p, "member"); err == nil {
				t.Fatal("malformed package accepted")
			}
			if len(s.Skills()) != 0 {
				for _, s := range s.Skills() {
					if s.ID == p.Skill.ID {
						t.Fatal("malformed package created skill")
					}
				}
			}
			rows, _ := s.SkillCandidates("team-a")
			if len(rows) != 0 {
				t.Fatal("invalid package persisted")
			}
		})
	}
}

func TestSkillCandidateRuntimeIdentityCollisionPreservesExistingSkill(t *testing.T) {
	s := intelligenceStore(t)
	active := domain.SkillDefinition{ID: "CANDIDATE-SKILL", Name: "Existing skill", ProjectID: "team-a", Files: map[string]string{"LICENSE": "existing license"}}
	if err := s.PutSkillSnapshot(active, []SkillFileInput{{Path: "SKILL.md", Content: []byte("existing instructions"), Mode: 0o644}}, true); err != nil {
		t.Fatal(err)
	}
	v, err := s.CreateSkillCandidate("team-a", candidatePackageFixture(), "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideSkillCandidate("team-a", v.ID, 1, "approved", "owner", "reviewed"); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatalf("runtime identity collision=%v", err)
	}
	retained, err := s.SkillCandidate("team-a", v.ID)
	if err != nil || retained.Revision != 1 || retained.State != domain.SkillCandidateQuarantined {
		t.Fatal("collision changed quarantine lifecycle")
	}
	bytes, ok := s.SkillFileContent(active.ID, "SKILL.md")
	if !ok || string(bytes) != "existing instructions" {
		t.Fatal("collision overwrote active Skill bytes")
	}
}
func TestSkillCandidateTextOnlyPromotionKeepsQuarantineBytes(t *testing.T) {
	s := intelligenceStore(t)
	p := candidatePackageFixture()
	p.Skill.FileRefs = nil
	p.Binary = nil
	v, err := s.CreateSkillCandidate("team-a", p, "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideSkillCandidate("team-a", v.ID, 1, "approved", "owner", "checked text-only package"); err != nil {
		t.Fatal(err)
	}
	retained, err := s.SkillCandidate("team-a", v.ID)
	if err != nil || retained.SHA256 != v.SHA256 || retained.Package.Skill.Files["SKILL.md"] != p.Skill.Content {
		t.Fatal("text-only promotion mutated quarantine package")
	}
	b, ok := s.SkillFileContent(p.Skill.ID, "SKILL.md")
	if !ok || string(b) != p.Skill.Content {
		t.Fatal("text-only SKILL.md changed")
	}
}
