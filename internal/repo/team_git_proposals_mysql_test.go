package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	mysqldriver "github.com/go-sql-driver/mysql"
)

// This exercises persistence authority over two real MySQL connection pools;
// real Git bytes and provider effects are covered by the Engine/tracker suites.
func TestMySQLTeamGitProposalApprovalLeaseAndRestart(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEAM_PROPOSAL_MYSQL_DSN")
	if dsn == "" {
		t.Skip("disposable TeamAI proposal MySQL DSN not supplied")
	}
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil || cfg == nil || os.Getenv("AGENTFLOW_TEAM_PROPOSAL_MYSQL_DISPOSABLE") != "1" || !strings.HasPrefix(cfg.DBName, "team_proposal_acceptance_") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("explicit loopback disposable team_proposal_acceptance_ schema required; DSN omitted")
	}
	a, b := budgetMySQLStore(t, dsn), budgetMySQLStore(t, dsn)
	t.Cleanup(func() { a.Close(); b.Close() })
	if err := a.PutProject(domain.Project{ID: "proposal-project", Name: "Proposal fixture"}); err != nil {
		t.Fatal(err)
	}
	r := domain.Repository{ID: "proposal-repo", ProjectID: "proposal-project", Name: "Fixture", Provider: "github", GongfengProjectID: "github:example/repo", CloneURL: "https://github.com/example/repo.git", DefaultBranch: "main"}
	if err := a.PutRepository(r); err != nil {
		t.Fatal(err)
	}
	release, err := a.CreateImportedTeamRelease(r.ProjectID, "Reviewed source", "owner", 0, teamUpdateRepoPayload(t, r, strings.Repeat("a", 40), ""))
	if err != nil {
		t.Fatal(err)
	}
	bundle := []byte("synthetic persistence-only fixture; not a verified Git bundle")
	sum := sha256.Sum256(bundle)
	p := domain.TeamGitProposal{ProjectID: r.ProjectID, RepositoryID: r.ID, RepositoryURL: r.CloneURL, ProviderProject: r.GongfengProjectID, ReleaseID: release.ID, ReleaseSHA256: release.SHA256, ReleaseRevision: release.Revision, TargetRef: "refs/heads/main", BaseCommit: release.Payload.GitImport.Commit, ResultSHA: strings.Repeat("d", 40), BundleSHA256: hex.EncodeToString(sum[:]), BundleSize: int64(len(bundle)), PatchSHA256: strings.Repeat("b", 64), ProposedSHA256: strings.Repeat("c", 64), Validation: domain.TeamGitProposalValidation{Policy: domain.TeamGitProposalPolicy}, State: "pending_review", Revision: 1, CreatedBy: "owner", CreatedAt: time.Now().UTC()}
	p.UpdatedAt = p.CreatedAt
	p.ID = domain.TeamGitProposalID(p)
	p.SHA256, _ = domain.TeamGitProposalDigest(p)
	if p, err = a.CreateTeamGitProposal(p, bundle); err != nil {
		t.Fatal(err)
	}

	gate := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []*Store{a, b} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			<-gate
			_, err := s.DecideTeamGitProposal(p.ProjectID, p.ID, p.SHA256, "owner", "approved", "reviewed exact fixture", 1)
			results <- err
		}(s)
	}
	close(gate)
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrTeamGitProposalConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("approval winners=%d conflicts=%d", wins, conflicts)
	}
	var count int64
	if err := a.DB.Table("contribution_publications").Where("id = ? AND run_id IS NULL", p.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("typed non-Run outbox count=%d err=%v", count, err)
	}

	claims := make(chan domain.ContributionPublication, 2)
	results = make(chan error, 2)
	gate = make(chan struct{})
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func(s *Store, owner string) {
			defer wg.Done()
			<-gate
			v, err := s.ClaimContributionPublication(p.ID, owner, time.Minute, time.Now().UTC())
			claims <- v
			results <- err
		}(s, []string{"publisher-a", "publisher-b"}[i])
	}
	close(gate)
	wg.Wait()
	close(results)
	close(claims)
	wins, conflicts = 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrContributionPublicationLeaseLost) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("lease winners=%d conflicts=%d", wins, conflicts)
	}
	var claim domain.ContributionPublication
	for v := range claims {
		if v.Attempt == 1 {
			claim = v
		}
	}
	if _, err := a.MarkContributionPRCreateAttempted(p.ID, claim.LeaseOwner, claim.Attempt, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	unlock, err := a.LockIntegration(context.Background(), "contribution:"+r.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.LockIntegration(context.Background(), "contribution:"+r.ID, time.Second)
	if err == nil {
		second()
		unlock()
		t.Fatal("same integration lock admitted second controller")
	}
	unlock()
	a.Close()
	b.Close()
	c := budgetMySQLStore(t, dsn)
	defer c.Close()
	saved, err := c.TeamGitProposal(p.ProjectID, p.ID)
	if err != nil || saved.State != "approved" || saved.Revision != 2 || saved.Publication == nil || !saved.Publication.PRCreateAttempted || saved.Publication.RunID != "" {
		t.Fatalf("restart lost approved typed authority: %v", err)
	}
	if _, err = c.MarkContributionPRCreateAttempted(p.ID, claim.LeaseOwner, claim.Attempt, time.Now().UTC()); !errors.Is(err, ErrContributionPRCreateUncertain) {
		t.Fatalf("restart reset create marker: %v", err)
	}
	if _, err = c.CompleteTeamGitProposalPublication(p.ProjectID, p.ID, p.SHA256, 2, claim.LeaseOwner, claim.Attempt, "https://github.com/example/repo/pull/17", 17, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	t.Log("two independent MySQL pools: one approval/outbox, one publisher lease, same integration lock; reopening retained typed NULL Run ID and sticky create marker")
}
