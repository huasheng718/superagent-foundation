package httpapi

import (
	"errors"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/teamconfig"
)

func TestTeamUpdateRetainsDistributionInReviewedDigest(t *testing.T) {
	base := domain.TeamReleasePayload{Assets: domain.AgentAssetBundle{Rules: []domain.RuleDefinition{{ID: "style", DistributionState: domain.DistributionState{Distribution: &domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "deny"}}}}}}
	next := teamconfig.Preview{Payload: domain.TeamReleasePayload{Assets: domain.AgentAssetBundle{Rules: []domain.RuleDefinition{{ID: "style", Content: "new Git bytes"}}}}}
	unrestricted, _ := domain.IntelligenceDigest(next.Payload)
	if err := retainTeamUpdateDistribution(base, &next); err != nil {
		t.Fatal(err)
	}
	if next.SHA256 == unrestricted || next.Payload.Assets.Rules[0].Distribution.Export != "deny" {
		t.Fatal("review digest lost inherited restriction")
	}
	digest, _ := domain.IntelligenceDigest(next.Payload)
	if next.SHA256 != digest {
		t.Fatal("preview identity does not cover policy")
	}
	base.Assets.Rules[0].Distribution.Edit = "deny"
	blocked := teamconfig.Preview{Payload: domain.TeamReleasePayload{Assets: domain.AgentAssetBundle{Rules: []domain.RuleDefinition{{ID: "style", Content: "new Git bytes"}}}}}
	if err := retainTeamUpdateDistribution(base, &blocked); !errors.Is(err, domain.ErrDistributionDenied) {
		t.Fatalf("Git content update bypassed edit deny: %v", err)
	}
	missing := teamconfig.Preview{}
	if err := retainTeamUpdateDistribution(base, &missing); err == nil {
		t.Fatal("resource drop silently discarded policy")
	}
}
