package repo

import (
	"agentflow/internal/domain"
	"errors"
	mysqldriver "github.com/go-sql-driver/mysql"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestMySQLSkillCandidateApprovalAcrossStores(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_CANDIDATE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("disposable candidate MySQL DSN not supplied")
	}
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil || os.Getenv("AGENTFLOW_CANDIDATE_MYSQL_DISPOSABLE") != "1" || cfg == nil || !strings.HasPrefix(cfg.DBName, "candidate_acceptance_") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("requires isolated loopback candidate_acceptance_ schema; credentials omitted")
	}
	a, err := Open(dsn)
	if err != nil {
		t.Fatalf("open disposable candidate MySQL failed (%T); credentials omitted", err)
	}
	defer a.Close()
	b, err := Open(dsn)
	if err != nil {
		t.Fatalf("open second disposable candidate MySQL failed (%T); credentials omitted", err)
	}
	defer b.Close()
	if err := a.PutProject(domain.Project{ID: "team-a", Name: "Disposable candidate project", OwnerID: 1}); err != nil {
		t.Fatal(err)
	}
	input := candidatePackageFixture()
	v, err := a.CreateSkillCandidate("team-a", input, "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := b.Skill(input.Skill.ID); exists {
		t.Fatal("unreviewed candidate executable in independent Store")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range []*Store{a, b} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			<-start
			_, err := s.DecideSkillCandidate("team-a", v.ID, 1, "approved", "owner", "reviewed complete disposable package")
			results <- err
		}(store)
	}
	close(start)
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrIntelligenceConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected independent-store decision error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("expected one approval and one stale conflict; wins=%d conflicts=%d", wins, conflicts)
	}
	retained, err := b.SkillCandidate("team-a", v.ID)
	if err != nil || retained.State != "approved" || retained.SHA256 != v.SHA256 {
		t.Fatal("approved audit identity not durable")
	}
	active, exists := b.Skill(input.Skill.ID)
	if !exists || active.Source == nil || active.Source.ReviewedCandidate.CandidateID != v.ID {
		t.Fatal("active skill not atomically promoted")
	}
	for _, ref := range input.Skill.FileRefs {
		raw, ok := b.SkillFileContent(input.Skill.ID, ref.Path)
		if !ok || string(raw) != string(input.Binary[ref.Path]) {
			t.Fatal("binary source not retained across Stores")
		}
	}
	second, err := b.CreateSkillCandidate("team-a", input, "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.DecideSkillCandidate("team-a", second.ID, 1, "approved", "owner", "collision must not overwrite"); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatalf("active collision=%v", err)
	}
	still, _ := a.SkillCandidate("team-a", second.ID)
	if still.State != domain.SkillCandidateQuarantined || still.Revision != 1 {
		t.Fatal("collision partially promoted or decided candidate")
	}
	t.Log("two independent MySQL Stores: quarantine invisible, one approval/one stale conflict, complete binary retention, create-only collision rollback")
}
