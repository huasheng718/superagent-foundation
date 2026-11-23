package repo

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"agentflow/internal/domain"
	mysqldriver "github.com/go-sql-driver/mysql"
)

// Two independent GORM pools exercise the production project-row lock, rather
// than OpenTest's single SQLite connection. The wrapper supplies a new schema.
func TestMySQLTeamReleaseCompareAndSwapAcrossStores(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_RELEASE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("disposable release MySQL DSN not supplied")
	}
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil || cfg == nil || os.Getenv("AGENTFLOW_RELEASE_MYSQL_DISPOSABLE") != "1" || !strings.HasPrefix(cfg.DBName, "release_acceptance_") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("explicit loopback disposable release_acceptance_ schema required; DSN omitted")
	}
	a, b := budgetMySQLStore(t, dsn), budgetMySQLStore(t, dsn)
	defer a.Close()
	defer b.Close()
	if err := a.PutProject(domain.Project{ID: "release-cas", Name: "Release CAS"}); err != nil {
		t.Fatal(err)
	}
	if err := a.PutRule(domain.RuleDefinition{ID: "release-cas-rule", ProjectID: "release-cas", Name: "Rule", Content: "reviewed original"}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		release domain.TeamRelease
		err     error
	}
	race := func(action func(*Store) (domain.TeamRelease, error)) domain.TeamRelease {
		t.Helper()
		gate := make(chan struct{})
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for _, s := range []*Store{a, b} {
			wg.Add(1)
			go func(s *Store) { defer wg.Done(); <-gate; r, e := action(s); results <- result{r, e} }(s)
		}
		close(gate)
		wg.Wait()
		close(results)
		wins, conflicts := 0, 0
		var winner domain.TeamRelease
		for r := range results {
			if r.err == nil {
				wins++
				winner = r.release
			} else if errors.Is(r.err, ErrIntelligenceConflict) {
				conflicts++
			} else {
				t.Fatal(r.err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("same-base race: success=%d conflict=%d", wins, conflicts)
		}
		return winner
	}
	first := race(func(s *Store) (domain.TeamRelease, error) {
		return s.CreateTeamRelease("release-cas", domain.TeamReleaseRequest{Name: "v1", RuleIDs: []string{"release-cas-rule"}}, "human")
	})
	if first.Version != 1 {
		t.Fatalf("first version %d", first.Version)
	}
	if _, err := a.RevokeTeamRelease("release-cas", first.ID, 1, "human", "known-good history restore test"); err != nil {
		t.Fatal(err)
	}
	second := race(func(s *Store) (domain.TeamRelease, error) {
		return s.RestoreTeamRelease("release-cas", first.ID, "restore copy", "human", "reviewed rollback", 1)
	})
	if second.Version != 2 || second.ID == first.ID || second.SHA256 != first.SHA256 {
		t.Fatal("restore mutated content/history")
	}
	old, err := b.TeamRelease("release-cas", first.ID)
	if err != nil || old.State != domain.TeamReleaseRevoked || old.Revision != 2 {
		t.Fatal("historical revocation lost")
	}
	list, err := a.TeamReleases("release-cas")
	if err != nil || len(list) != 2 {
		t.Fatalf("race inserted extra releases: n=%d err=%v", len(list), err)
	}
	t.Log("two independent MySQL pools: publish and restore each admitted one winner, rejected one stale base; revoked history retained")
}
