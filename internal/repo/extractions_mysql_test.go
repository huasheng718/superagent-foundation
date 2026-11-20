package repo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

func extractionMySQLDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AGENTFLOW_EXTRACTION_MYSQL_DSN")
	if dsn == "" {
		t.Skip("owned extraction MySQL schema not selected")
	}
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil || cfg == nil || os.Getenv("AGENTFLOW_EXTRACTION_MYSQL_DISPOSABLE") != "1" || !strings.HasPrefix(cfg.DBName, "extraction_acceptance_") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("requires owned loopback extraction_acceptance_ schema; DSN omitted")
	}
	return dsn
}
func TestMySQLKnowledgeExtractionRestartHelper(t *testing.T) {
	if os.Getenv("AGENTFLOW_EXTRACTION_HELPER") != "1" {
		t.Skip("fresh process helper not selected")
	}
	s := budgetMySQLStore(t, extractionMySQLDSN(t))
	defer s.Close()
	var in domain.KnowledgeExtractionRequest
	if json.Unmarshal([]byte(os.Getenv("AGENTFLOW_EXTRACTION_INPUT")), &in) != nil {
		t.Fatal("invalid fixed input")
	}
	v, replayed, err := s.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true})
	if err != nil || !replayed || v.ID != os.Getenv("AGENTFLOW_EXTRACTION_ID") {
		t.Fatalf("restart replay=%v err=%v", replayed, err)
	}
	if a, b, c := extractionCounts(t, s); a != 1 || b != 1 || c != 1 {
		t.Fatal("fresh process duplicated candidates")
	}
	t.Log("fresh-process source replay retained exact two quarantined candidates")
}
func TestMySQLKnowledgeExtractionAtomicReplayAndCurrentReads(t *testing.T) {
	dsn := extractionMySQLDSN(t)
	a, b := budgetMySQLStore(t, dsn), budgetMySQLStore(t, dsn)
	defer a.Close()
	defer b.Close()
	if !a.DB.Migrator().HasTable(&knowledgeExtractionRow{}) {
		t.Fatal("manual039 missing extraction table")
	}
	in, _, _ := extractionFixture(t, a)
	type result struct {
		receipt  domain.KnowledgeExtractionReceipt
		replayed bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, s := range []*Store{a, b} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			<-start
			v, replayed, err := s.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true})
			results <- result{v, replayed, err}
		}(s)
	}
	close(start)
	wg.Wait()
	close(results)
	creators := 0
	id := ""
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent ingestion: %v", result.err)
		}
		if !result.replayed {
			creators++
		}
		if id != "" && id != result.receipt.ID {
			t.Fatal("source generated two receipts")
		}
		id = result.receipt.ID
	}
	if creators != 1 {
		t.Fatalf("source creators=%d", creators)
	}
	if x, y, z := extractionCounts(t, a); x != 1 || y != 1 || z != 1 {
		t.Fatal("concurrent ingestion duplicated candidates")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	child := exec.Command(exe, "-test.run=^TestMySQLKnowledgeExtractionRestartHelper$", "-test.v")
	child.Env = append(os.Environ(), "AGENTFLOW_EXTRACTION_HELPER=1", "AGENTFLOW_EXTRACTION_ID="+id, "AGENTFLOW_EXTRACTION_INPUT="+string(raw))
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh process: %s", output)
	}
	t.Log(strings.TrimSpace(string(output)))
	// Explicitly establish an old REPEATABLE READ snapshot, then commit a deny
	// from the other pool. The production locked read must consume latest policy.
	tx := a.DB.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	var old agentRow
	if err := tx.Where("id = ?", "extraction-author").Take(&old).Error; err != nil {
		t.Fatal(err)
	}
	current, _ := b.Agent("extraction-author")
	if _, err := b.SetDistribution("agent", current.ID, current.SourceRevision, domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "deny"}, "current-read denial", DistributionActor{Local: true}); err != nil {
		t.Fatal(err)
	}
	scoped := *a.ExtractionsRepo
	scoped.deps = &deps{DB: tx}
	if _, _, err := scoped.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true}); !errors.Is(err, ErrExtractionRestricted) {
		t.Fatalf("old snapshot bypassed current deny: %v", err)
	}
	_ = tx.Rollback()
	// Restore via owning CAS before testing membership/source lock-wait races.
	current, _ = b.Agent("extraction-author")
	if _, err := b.SetDistribution("agent", current.ID, current.SourceRevision, domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "allow"}, "restore owned fixture", DistributionActor{Local: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.PutUser(domain.User{LoginName: "extraction-member"}); err != nil {
		t.Fatal(err)
	}
	member, _ := a.UserByLoginName("extraction-member")
	project, _ := a.Project("team-a")
	project.OwnerID = member.ID + 1000 // actor is a member, not the fixture owner
	project.Members = []domain.ProjectMember{{UserID: member.ID}}
	if err := a.PutProject(project); err != nil {
		t.Fatal(err)
	}
	principal := KnowledgeExtractionActor{UserID: member.ID, LoginName: member.LoginName}
	blocker := b.DB.Begin()
	var row projectRow
	if err := b.locking(blocker).Where("id = ?", "team-a").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := a.CreateKnowledgeExtraction(context.Background(), "team-a", in, principal)
		done <- err
	}()
	select {
	case err := <-done:
		_ = blocker.Rollback()
		t.Fatalf("import did not wait for project lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := blocker.Where("project_id = ? AND user_id = ?", "team-a", member.ID).Delete(&projectMemberRow{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrExtractionForbidden) {
		t.Fatalf("removed member retained import authority: %v", err)
	}
	// Source cancellation committed while importer waits for the Run row must
	// prevent even an existing-source replay from gaining new write authority.
	sourceBlocker := b.DB.Begin()
	var sourceRow runRow
	if err := b.locking(sourceBlocker).Where("id = ?", in.RunID).Take(&sourceRow).Error; err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _, err := a.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true})
		done <- err
	}()
	select {
	case err := <-done:
		_ = sourceBlocker.Rollback()
		t.Fatalf("import did not wait for source lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := sourceBlocker.Model(&runRow{}).Where("id = ?", in.RunID).Update("cancel_requested", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := sourceBlocker.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrExtractionSource) {
		t.Fatalf("cancelled source retained import authority: %v", err)
	}
	if x, y, z := extractionCounts(t, a); x != 1 || y != 1 || z != 1 {
		t.Fatal("denied races altered candidate/receipt rows")
	}
	t.Log("manual039 table; two pools one atomic receipt/two candidates; fresh-process replay; old RR snapshot current-policy denial; member removal and source-cancel lock-wait denial")
}

// Keep the callback/transaction failure path independent of the successful
// concurrent fixture: failure of item2 must undo the already-inserted item1.
func TestMySQLKnowledgeExtractionRollback(t *testing.T) {
	s := budgetMySQLStore(t, extractionMySQLDSN(t))
	defer s.Close()
	// This test is selected separately by the wrapper after the main durability
	// test; remove only its owned fixture rows, not any external/global table.
	for _, model := range []any{&knowledgeExtractionRow{}, &governedMemoryRow{}, &skillCandidateRow{}} {
		if err := s.DB.Where("project_id = ?", "team-a").Delete(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	in, _, _ := extractionFixture(t, s)
	injected := errors.New("injected owned extraction write failure")
	if err := s.DB.Callback().Create().Before("gorm:create").Register("extraction_mysql_fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "skill_candidates" {
			tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.CreateKnowledgeExtraction(context.Background(), "team-a", in, KnowledgeExtractionActor{LoginName: "local", Local: true})
	_ = s.DB.Callback().Create().Remove("extraction_mysql_fail")
	if !errors.Is(err, injected) {
		t.Fatalf("rollback fault did not reach item2: %v", err)
	}
	if a, b, c := extractionCounts(t, s); a != 0 || b != 0 || c != 0 {
		t.Fatal("MySQL batch partially committed")
	}
}
