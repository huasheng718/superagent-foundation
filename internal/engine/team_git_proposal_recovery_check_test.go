package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestTeamProposalLegacyPublishingClaimRemainsDiscoverOnly(t *testing.T) {
	fixture := newTeamProposalFixture(t)
	proposal := fixture.approve(t)
	old, err := fixture.s.ClaimContributionPublication(proposal.ID, "old-publisher", time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// Shared outbox upgrades old in-flight records conservatively during claim.
	// A pre-claim snapshot is not proof that this invocation owns a first POST.
	old.PhaseVersion = 0
	old.PRCreateAttempted = false
	payload, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.s.DB.Table("contribution_publications").Where("id = ?", proposal.ID).Updates(map[string]any{"payload": string(payload), "lease_until": time.Now().Add(-time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	creates, discovers, objects := 0, 0, 0
	fixture.e.PublishContributionObjects = func(context.Context, string, string, string, string, string, string) error { objects++; return nil }
	fixture.e.CreateContributionPR = func(context.Context, string, string, string, string, string, string) (domain.ContributionPRReceipt, error) {
		creates++
		return domain.ContributionPRReceipt{URL: "https://github.com/example/repo/pull/19", Number: 19, HeadSHA: proposal.ResultSHA, Draft: true}, nil
	}
	fixture.e.DiscoverContributionPR = func(context.Context, string, string, string, string, string, string) (domain.ContributionPRReceipt, error) {
		discovers++
		return domain.ContributionPRReceipt{URL: "https://github.com/example/repo/pull/19", Number: 19, HeadSHA: proposal.ResultSHA, Draft: true}, nil
	}
	publication, err := fixture.e.PublishTeamGitProposal(context.Background(), proposal.ProjectID, proposal.ID, proposal.SHA256, proposal.BaseCommit, proposal.Revision, false)
	if err != nil || publication.State != "pr_ready" || creates != 0 || discovers != 1 || objects != 0 {
		t.Fatalf("upgraded recovery made another create: state=%s err=%v create=%d discover=%d objects=%d", publication.State, err, creates, discovers, objects)
	}
}
