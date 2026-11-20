package repo

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func intelligenceStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, p := range []string{"team-a", "team-b"} {
		if err := s.PutProject(domain.Project{ID: p, Name: p, OwnerID: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func TestTeamReleaseFreezesResourcesAndMonotonicVersions(t *testing.T) {
	s := intelligenceStore(t)
	if err := s.PutRule(domain.RuleDefinition{ID: "team-rule", Name: "Rule", ProjectID: "team-a", Content: "original rule"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSkill(domain.SkillDefinition{ID: "team-skill", Name: "Skill", ProjectID: "team-a", Content: "# Original", Files: map[string]string{"docs/info.txt": "old text"}}); err != nil {
		t.Fatal(err)
	}
	skill, _ := s.Skill("team-skill")
	skill, err := s.SaveSkillFiles("team-skill", []SkillFileInput{{Path: "assets/data.bin", Content: []byte{0, 1, 255}, Mode: 0644}}, skill.SourceRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutAgent(domain.AgentDefinition{ID: "team-author", Name: "Author", ProjectID: "team-a", Rules: []string{"team-rule"}, Skills: []string{"team-skill"}}); err != nil {
		t.Fatal(err)
	}
	input := domain.TeamReleaseRequest{Name: "Release", AgentIDs: []string{"team-author"}}
	first, err := s.CreateTeamRelease("team-a", input, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.State != domain.TeamReleasePublished || first.Revision != 1 {
		t.Fatalf("release %+v", first)
	}
	rule, _ := s.Rule("team-rule")
	rule.Content = "mutated"
	if err := s.PutRule(rule); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSkillFiles("team-skill", []SkillFileInput{{Path: "assets/data.bin", Content: []byte("new bytes")}}, skill.SourceRevision); err != nil {
		t.Fatal(err)
	}
	input.ExpectedBaseVersion = first.Version
	second, err := s.CreateTeamRelease("team-a", input, "owner")
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.TeamRelease("team-a", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != 2 || first.SHA256 == second.SHA256 || old.Payload.Assets.Rules[0].Content != "original rule" || string(old.Payload.Assets.SkillFiles["team-skill"]["assets/data.bin"]) != string([]byte{0, 1, 255}) {
		t.Fatal("published configuration followed latest resources")
	}
	if _, err := s.TeamRelease("team-b", first.ID); !errors.Is(err, ErrIntelligenceUnavailable) {
		t.Fatalf("cross-project release accessible: %v", err)
	}
	if _, err := s.CreateTeamRelease("team-b", input, "owner"); !errors.Is(err, ErrIntelligenceUnavailable) {
		t.Fatalf("foreign Agent imported: %v", err)
	}
	if _, err := s.RevokeTeamRelease("team-a", first.ID, 99, "owner", "rollback"); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatalf("stale revoke: %v", err)
	}
	revoked, err := s.RevokeTeamRelease("team-a", first.ID, 1, "owner", "rollback")
	if err != nil {
		t.Fatal(err)
	}
	if revoked.State != domain.TeamReleaseRevoked || revoked.SHA256 != first.SHA256 || revoked.Revision != 2 || len(revoked.Decisions) != 2 {
		t.Fatal("revoke mutated immutable content or erased audit")
	}

	restored, err := s.RestoreTeamRelease("team-a", first.ID, "Rollback copy", "owner", "restore known-good configuration", second.Version)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Version != 3 || restored.ID == first.ID || restored.State != domain.TeamReleasePublished || restored.SHA256 != first.SHA256 {
		t.Fatal("restore rewrote history instead of publishing a new exact version")
	}
	stillRevoked, err := s.TeamRelease("team-a", first.ID)
	if err != nil || stillRevoked.State != domain.TeamReleaseRevoked {
		t.Fatal("restore reactivated revoked historical row")
	}
	if _, err := s.RestoreTeamRelease("team-b", first.ID, "forged", "owner", "foreign", 0); !errors.Is(err, ErrIntelligenceUnavailable) {
		t.Fatal("foreign historical version restored")
	}
	var row teamReleaseRow
	if err := s.DB.Where("id = ?", second.ID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	var corrupt domain.TeamRelease
	_ = json.Unmarshal(row.Payload, &corrupt)
	corrupt.Payload.Agents[0].Prompt = "tampered"
	row.Payload, _ = json.Marshal(corrupt)
	if err := s.DB.Model(&teamReleaseRow{}).Where("id = ?", second.ID).Update("payload", row.Payload).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamRelease("team-a", second.ID); err == nil {
		t.Fatal("digest corruption accepted")
	}
}
func TestTeamReleaseRejectsCredentialBearingMCP(t *testing.T) {
	s := intelligenceStore(t)
	for name, mcp := range map[string]domain.MCPServerDefinition{
		"environment": {Environment: map[string]string{"API_KEY": "private-value"}}, "headers": {Headers: map[string]string{"Authorization": "Bearer private-value"}}, "query": {URL: "https://example.test/mcp?key=private-value"},
	} {
		t.Run(name, func(t *testing.T) {
			mcp.ID = "release-mcp"
			mcp.Name = "MCP"
			mcp.ProjectID = "team-a"
			mcp.Scope = "project"
			mcp.Transport = "http"
			if mcp.URL == "" {
				mcp.URL = "https://example.test/mcp"
			}
			if observed, found := s.MCPServer(mcp.ID); found {
				mcp.SourceRevision = observed.SourceRevision
			}
			if err := s.PutMCPServer(mcp); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateTeamRelease("team-a", domain.TeamReleaseRequest{Name: "Secret", MCPServerIDs: []string{mcp.ID}}, "owner"); err == nil {
				t.Fatal("credential-bearing MCP copied into release")
			}
		})
	}
}
func TestGovernedMemoryPublicationScopeAndRevocation(t *testing.T) {
	s := intelligenceStore(t)
	candidate, err := s.CreateMemoryCandidate("team-a", domain.GovernedMemory{Title: "Billing", Content: "Use exact integer money units.", State: domain.MemoryApproved, Source: domain.IntelligenceSource{Path: "docs/billing.md"}}, "author")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != domain.MemoryCandidate || candidate.Revision != 1 {
		t.Fatal("caller self-approved memory")
	}
	found, err := s.RecallGovernedMemory("team-a", "money", 10)
	if err != nil || len(found) != 0 {
		t.Fatal("candidate eligible for recall")
	}
	if _, err := s.DecideGovernedMemory("team-b", candidate.ID, 1, domain.MemoryApproved, "owner", "verified"); !errors.Is(err, ErrIntelligenceUnavailable) {
		t.Fatal("foreign memory decision allowed")
	}
	approved, err := s.DecideGovernedMemory("team-a", candidate.ID, 1, domain.MemoryApproved, "owner", "verified")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideGovernedMemory("team-a", candidate.ID, 1, domain.MemoryRevoked, "owner", "stale"); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatal("stale decision accepted")
	}
	found, err = s.RecallGovernedMemory("team-a", "money", 10)
	if err != nil || len(found) != 1 || found[0].SHA256 != approved.SHA256 {
		t.Fatalf("approved recall %v %+v", err, found)
	}
	foreign, err := s.RecallGovernedMemory("team-b", "money", 10)
	if err != nil || len(foreign) != 0 {
		t.Fatal("cross-project memory recall")
	}
	if _, err := s.DecideGovernedMemory("team-a", candidate.ID, 2, domain.MemoryRevoked, "owner", "source stale"); err != nil {
		t.Fatal(err)
	}
	found, err = s.RecallGovernedMemory("team-a", "money", 10)
	if err != nil || len(found) != 0 {
		t.Fatal("revoked memory remains in recall")
	}
	if _, err := s.DecideGovernedMemory("team-a", candidate.ID, 3, domain.MemoryApproved, "owner", "revive"); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatal("revoked memory silently republished")
	}
	if _, err := s.CreateMemoryCandidate("team-a", domain.GovernedMemory{Title: "oversized", Content: strings.Repeat("x", domain.MemoryMaxContentBytes+1), Source: domain.IntelligenceSource{Path: "x"}}, "author"); err == nil {
		t.Fatal("unbounded candidate accepted")
	}
	past := time.Now().Add(-time.Hour)
	if _, err := s.CreateMemoryCandidate("team-a", domain.GovernedMemory{Title: "expired", Content: "x", Source: domain.IntelligenceSource{Path: "x"}, ExpiresAt: &past}, "author"); err == nil {
		t.Fatal("expired source accepted")
	}
	if _, err := s.RecallGovernedMemory("team-a", "money", 11); err == nil {
		t.Fatal("unbounded recall accepted")
	}
}

func TestTeamReleaseRequiresLoadedBaseAndRetainsNoticeBytes(t *testing.T) {
	s := intelligenceStore(t)
	entry := "---\nname: portable-skill\ndescription: Checked instructions\n---\n\nUse nested resources.\n"
	skill := domain.SkillDefinition{ID: "portable-skill", ProjectID: "team-a", Name: "Portable", Content: "fallback", Files: map[string]string{"SKILL.md": entry, "LICENSE": "MIT\nCopyright 2026 Upstream\n", "NOTICE": "Origin: reviewed upstream commit\n", "references/api.md": "# API\n"}}
	if err := s.PutSkill(skill); err != nil {
		t.Fatal(err)
	}
	skill, _ = s.Skill(skill.ID)
	script := []byte("#!/bin/sh\nprintf approved\\n\n")
	skill, err := s.SaveSkillFiles(skill.ID, []SkillFileInput{{Path: "scripts/check.sh", Content: script, Mode: 0755}, {Path: "assets/data.bin", Content: []byte{0, 255, 1}, Mode: 0644}}, skill.SourceRevision)
	if err != nil {
		t.Fatal(err)
	}
	req := domain.TeamReleaseRequest{Name: "portable v1", SkillIDs: []string{skill.ID}, Source: domain.IntelligenceSource{Repository: "https://example.test/upstream.git", Commit: strings.Repeat("a", 40), Path: "skills/portable"}}
	release, err := s.CreateTeamRelease("team-a", req, "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateTeamRelease("team-a", req, "human"); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatalf("stale publish accepted: %v", err)
	}
	if _, err = s.RestoreTeamRelease("team-a", release.ID, "restore", "human", "reviewed", 0); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatalf("stale restore accepted: %v", err)
	}
	skill.Files["SKILL.md"] = "mutated"
	skill.Files["NOTICE"] = "lost provenance"
	if err = s.PutSkill(skill); err != nil {
		t.Fatal(err)
	}
	restored, err := s.RestoreTeamRelease("team-a", release.ID, "restore", "human", "reviewed", 1)
	if err != nil {
		t.Fatal(err)
	}
	frozen := restored.Payload.Assets.Skills[0]
	if frozen.Files["SKILL.md"] != entry || frozen.Files["NOTICE"] != "Origin: reviewed upstream commit\n" || frozen.Files["LICENSE"] != "MIT\nCopyright 2026 Upstream\n" || string(restored.Payload.Assets.SkillFiles[skill.ID]["scripts/check.sh"]) != string(script) || restored.SHA256 != release.SHA256 || restored.Payload.Source != req.Source {
		t.Fatal("source, license, notice or full package bytes changed during restore")
	}
	list, err := s.TeamReleases("team-a")
	if err != nil || len(list) != 2 {
		t.Fatal("stale publication/restore created history")
	}
}

func TestTeamReleaseRejectsPersistedInvalidUTF8Entrypoint(t *testing.T) {
	s := intelligenceStore(t)
	if err := s.PutSkill(domain.SkillDefinition{ID: "invalid-entry", Name: "Invalid entry", ProjectID: "team-a", Content: "fallback"}); err != nil {
		t.Fatal(err)
	}
	skill, _ := s.Skill("invalid-entry")
	if err := s.PutSkillFiles("invalid-entry", []SkillFileInput{{Path: "SKILL.md", Content: []byte{0xff, 0xfe}, Mode: 0644}}, skill.SourceRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeamRelease("team-a", domain.TeamReleaseRequest{Name: "must fail", SkillIDs: []string{"invalid-entry"}}, "human"); err == nil {
		t.Fatal("persisted raw invalid entrypoint published")
	}
	list, err := s.TeamReleases("team-a")
	if err != nil || len(list) != 0 {
		t.Fatal("invalid package created release history")
	}
}
