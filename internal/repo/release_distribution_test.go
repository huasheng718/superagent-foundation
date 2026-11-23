package repo

import (
	"errors"
	"testing"

	"agentflow/internal/domain"
)

func TestTeamReleaseDistributionScopeAndImmutableFloor(t *testing.T) {
	s := intelligenceStore(t)
	if err := s.PutRule(domain.RuleDefinition{ID: "restricted", Name: "Restricted", ProjectID: "team-a", Shared: true, Content: "inspectable policy"}); err != nil {
		t.Fatal(err)
	}
	rule, _ := s.Rule("restricted")
	if _, err := s.SetDistribution("rule", rule.ID, rule.SourceRevision, domain.DistributionPolicy{Copy: "deny", Edit: "deny", Export: "deny"}, "test restriction", DistributionActor{Local: true}); err != nil {
		t.Fatal(err)
	}
	input := domain.TeamReleaseRequest{Name: "Frozen", RuleIDs: []string{rule.ID}}
	if _, err := s.CreateTeamRelease("team-b", input, "owner"); !errors.Is(err, domain.ErrDistributionDenied) {
		t.Fatalf("foreign copy: %v", err)
	}
	if values, _ := s.TeamReleases("team-b"); len(values) != 0 {
		t.Fatal("denied closure partially published")
	}
	// Freezing one's own configuration is an execution/versioning operation.
	if _, err := s.CreateTeamRelease("team-a", input, "owner"); err != nil {
		t.Fatal(err)
	}
	rule, _ = s.Rule(rule.ID)
	if _, err := s.SetDistribution("rule", rule.ID, rule.SourceRevision, domain.DistributionPolicy{Copy: "allow", Edit: "deny", Export: "deny"}, "allow managed copy only", DistributionActor{Local: true}); err != nil {
		t.Fatal(err)
	}
	copy, err := s.CreateTeamRelease("team-b", input, "owner")
	if err != nil {
		t.Fatal(err)
	}
	frozen := copy.Payload.Assets.Rules[0]
	if len(frozen.DistributionOrigins) != 1 || frozen.DistributionOrigins[0].ProjectID != "team-a" || frozen.DistributionState.Effective().Export != "deny" {
		t.Fatal("foreign snapshot lost floor")
	}
	rule, _ = s.Rule(rule.ID)
	if _, err := s.SetDistribution("rule", rule.ID, rule.SourceRevision, domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "allow"}, "later source policy", DistributionActor{Local: true}); err != nil {
		t.Fatal(err)
	}
	restored, err := s.RestoreTeamRelease("team-b", copy.ID, "Restored", "owner", "retain reviewed policy", 1)
	if err != nil || restored.SHA256 != copy.SHA256 || restored.Payload.Assets.Rules[0].DistributionState.Effective().Export != "deny" {
		t.Fatalf("immutable policy changed during restore: %v", err)
	}
}
