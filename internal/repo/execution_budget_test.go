package repo

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/domain"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestExecutionBudgetPersistenceAndStaleWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	open := func() *Store {
		db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		s := newStore(db)
		if err := db.AutoMigrate(allModels()...); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	p := &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 3, MaxTotalTokens: 100}
	state, _ := domain.NewExecutionBudgetState(p)
	run := domain.Run{ID: "budget", Status: "failed", CreatedAt: time.Now().UTC(), ExecutionContext: domain.TelemetryContext{ExecutionBudget: p}, ExecutionBudget: state, Nodes: map[string]*domain.NodeRun{}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	a, _ := s.RunState(run.ID)
	stale, _ := s.RunState(run.ID)
	if err := a.ExecutionBudget.Begin(p, "invocation", "n", "codex", 1); err != nil {
		t.Fatal(err)
	}
	if err := a.ExecutionBudget.Observe(p, "n", "session", domain.TokenUsage{TotalTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if err := a.ExecutionBudget.Finish(p, "invocation"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRun(a); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRun(stale); !errors.Is(err, ErrRunDecisionStale) {
		t.Fatalf("stale usage write accepted: %v", err)
	}
	s.Close()
	s = open()
	defer s.Close()
	got, ok := s.RunState(run.ID)
	if !ok || got.ExecutionBudget.ConsumedTokens != 20 || got.ExecutionBudget.Invocations != 1 || !got.ExecutionBudget.Receipts["invocation"].Finished {
		t.Fatalf("ledger did not survive new connection: %+v", got.ExecutionBudget)
	}
	original := got.ExecutionContext.ExecutionBudget.MaxTotalTokens
	got.ExecutionContext.ExecutionBudget.MaxTotalTokens++
	if err := s.PutRun(got); err == nil {
		t.Fatal("budget policy increased after start")
	}
	got.ExecutionContext.ExecutionBudget.MaxTotalTokens = original
	got.ExecutionBudget.ConsumedTokens = 0
	if err := s.PutRun(got); err == nil {
		t.Fatal("spend reset accepted")
	}
	got, _ = s.RunState(run.ID)
	delete(got.ExecutionBudget.Receipts, "invocation")
	if err := s.PutRun(got); err == nil {
		t.Fatal("receipt deletion accepted")
	}
	got, _ = s.RunState(run.ID)
	got.ExecutionBudget.StopReason = "no_progress"
	if err := s.PutRun(got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.RunState(run.ID)
	got.ExecutionBudget.StopReason = ""
	if err := s.PutRun(got); err == nil {
		t.Fatal("sticky stop erased")
	}
	plain := domain.Run{ID: "plain", Status: "failed", Nodes: map[string]*domain.NodeRun{}}
	if err := s.PutRun(plain); err != nil {
		t.Fatal(err)
	}
	plain, _ = s.RunState("plain")
	plain.ExecutionContext.ExecutionBudget = p
	plain.ExecutionBudget = state
	if err := s.PutRun(plain); err == nil {
		t.Fatal("budget added after creation")
	}
}
