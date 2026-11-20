package repo

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	mysqldriver "github.com/go-sql-driver/mysql"
)

// This suite deliberately uses a dedicated DSN and schema name rather than the
// ordinary application/test datasource. The wrapper creates and removes it.
func budgetMySQLDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AGENTFLOW_BUDGET_MYSQL_DSN")
	if dsn == "" {
		t.Skip("disposable budget MySQL DSN not supplied")
	}
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil || os.Getenv("AGENTFLOW_BUDGET_MYSQL_DISPOSABLE") != "1" || cfg == nil || !strings.HasPrefix(cfg.DBName, "budget_acceptance_") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("requires explicitly marked loopback disposable budget_acceptance_ schema; DSN omitted")
	}
	return dsn
}
func budgetMySQLStore(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open disposable MySQL failed (%T); credentials omitted", err)
	}
	if s.DB.Dialector.Name() != "mysql" {
		s.Close()
		t.Fatal("acceptance requires actual MySQL")
	}
	return s
}
func budgetMySQLRun(t *testing.T, id string) domain.Run {
	t.Helper()
	policy := &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 4, MaxTotalTokens: 1000, MaxCostMicrosUSD: 250000, Currency: "USD", PriceVersion: "disposable-acceptance-v1", MaxNoProgress: 2}
	state, err := domain.NewExecutionBudgetState(policy)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Run{ID: id, Status: "running", CreatedAt: time.Now().UTC(), ExecutionContext: domain.TelemetryContext{ExecutionBudget: policy}, ExecutionBudget: state, Nodes: map[string]*domain.NodeRun{"a": {NodeID: "a", Attempt: 1, Status: "running"}, "b": {NodeID: "b", Attempt: 1, Status: "running"}}}
}
func budgetMySQLChild(t *testing.T, runID, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestMySQLExecutionBudgetRestartHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "AGENTFLOW_BUDGET_HELPER_RUN_ID="+runID, "AGENTFLOW_BUDGET_HELPER_MODE="+mode)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh test process failed: %v\n%s", err, out)
	}
	t.Logf("fresh process %s passed", mode)
}
func TestMySQLExecutionBudgetCrossStoreAndProcessRestart(t *testing.T) {
	dsn := budgetMySQLDSN(t)
	a, b := budgetMySQLStore(t, dsn), budgetMySQLStore(t, dsn)
	defer a.Close()
	defer b.Close()
	id := fmt.Sprintf("budget-mysql-%d", time.Now().UnixNano())
	initial := budgetMySQLRun(t, id)
	if err := a.PutRun(initial); err != nil {
		t.Fatal(err)
	}
	snapshots := make([]domain.Run, 2)
	for i, s := range []*Store{a, b} {
		var ok bool
		snapshots[i], ok = s.RunState(id)
		if !ok {
			t.Fatal("shared initial Run missing")
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	type outcome struct {
		node string
		err  error
	}
	results := make(chan outcome, 2)
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			node := []string{"a", "b"}[i]
			r := snapshots[i]
			err := r.ExecutionBudget.Begin(r.ExecutionContext.ExecutionBudget, "reservation-"+node, node, "acceptance-provider", 1)
			if err != nil {
				results <- outcome{node, err}
				return
			}
			<-start
			results <- outcome{node, s.PutRun(r)}
		}(i, s)
	}
	close(start)
	wg.Wait()
	close(results)
	winner := ""
	wins, stales := 0, 0
	for result := range results {
		if result.err == nil {
			wins++
			winner = result.node
		} else if errors.Is(result.err, ErrRunDecisionStale) {
			stales++
		} else {
			t.Fatal(result.err)
		}
	}
	if wins != 1 || stales != 1 {
		t.Fatalf("cross-store reservation race: wins=%d stale=%d", wins, stales)
	}
	current, ok := b.RunState(id)
	if !ok || current.ExecutionBudget.Invocations != 1 || len(current.ExecutionBudget.Receipts) != 1 {
		t.Fatal("lost/duplicate durable invocation")
	}
	stale, _ := a.RunState(id)
	policy := current.ExecutionContext.ExecutionBudget
	if err := current.ExecutionBudget.Observe(policy, winner, "resumed-session", domain.TokenUsage{TotalTokens: 100, CostUSD: .05, CostStatus: "reported"}); err != nil {
		t.Fatal(err)
	}
	if err := current.ExecutionBudget.Finish(policy, "reservation-"+winner); err != nil {
		t.Fatal(err)
	}
	if err := b.PutRun(current); err != nil {
		t.Fatal(err)
	}
	if err := a.PutRun(stale); !errors.Is(err, ErrRunDecisionStale) {
		t.Fatalf("stale usage writer committed: %v", err)
	}
	current, _ = a.RunState(id)
	current.ExecutionBudget.ConsumedTokens = 0
	if err := a.PutRun(current); err == nil {
		t.Fatal("fresh-revision spend rollback committed")
	}
	current, _ = b.RunState(id)
	if err := current.ExecutionBudget.Begin(policy, "resume-2", winner, "acceptance-provider", 1); err != nil {
		t.Fatal(err)
	}
	for _, usage := range []domain.TokenUsage{{InputTokens: 120, OutputTokens: 30, TotalTokens: 150, CostUSD: .08, CostStatus: "reported"}, {TotalTokens: 100, CostUSD: .05, CostStatus: "reported"}, {InputTokens: 120, OutputTokens: 30, TotalTokens: 150, CostUSD: .08, CostStatus: "reported"}} {
		if err := current.ExecutionBudget.Observe(policy, winner, "resumed-session", usage); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.ExecutionBudget.Finish(policy, "resume-2"); err != nil {
		t.Fatal(err)
	}
	if current.ExecutionBudget.ConsumedTokens != 150 || current.ExecutionBudget.ConsumedCostMicrosUSD != 80000 {
		t.Fatal("resumed session refunded/double counted usage")
	}
	if err := b.PutRun(current); err != nil {
		t.Fatal(err)
	}
	// No Store/connection pool from this test survives into the child process.
	a.Close()
	b.Close()
	budgetMySQLChild(t, id, "resume-cap")
	reopened := budgetMySQLStore(t, dsn)
	defer reopened.Close()
	final, ok := reopened.RunState(id)
	if !ok || final.ExecutionBudget.ConsumedTokens != 1000 || final.ExecutionBudget.Invocations != 3 || final.ExecutionBudget.StopReason != "token_budget_exhausted" || len(final.ExecutionBudget.Active) != 0 {
		t.Fatalf("process restart lost accounting: %+v", final.ExecutionBudget)
	}
	if err := domain.ValidateRunExecutionBudget(final); !errors.Is(err, domain.ErrExecutionBudgetStopped) {
		t.Fatal("exhausted Run validated for publication", err)
	}
	final.ExecutionBudget.StopReason = ""
	if err := reopened.PutRun(final); err == nil {
		t.Fatal("stop was cleared after restart")
	}
	// An interrupted invocation retains its known spend, and cannot be replayed
	// or replaced by a new invocation after an independent process reopens it.
	uncertain := budgetMySQLRun(t, id+"-uncertain")
	if err := uncertain.ExecutionBudget.Begin(uncertain.ExecutionContext.ExecutionBudget, "unfinished", "a", "acceptance-provider", 1); err != nil {
		t.Fatal(err)
	}
	if err := uncertain.ExecutionBudget.Observe(uncertain.ExecutionContext.ExecutionBudget, "a", "crash-session", domain.TokenUsage{TotalTokens: 20, CostUSD: .01, CostStatus: "reported"}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.PutRun(uncertain); err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	budgetMySQLChild(t, uncertain.ID, "uncertain")
	check := budgetMySQLStore(t, dsn)
	defer check.Close()
	stored, _ := check.RunState(uncertain.ID)
	if stored.ExecutionBudget.ConsumedTokens != 20 || stored.ExecutionBudget.Invocations != 1 || stored.ExecutionBudget.StopReason != "budget_invocation_uncertain" {
		t.Fatal("uncertain invocation lost durable spend or received allowance")
	}
	t.Log("MySQL: two Store pools, one race winner, stale/rollback denial, fresh process cumulative cap, sticky stop and uncertain recovery verified")
}

// Executed as an actual separate OS process by the acceptance above. A normal
// test invocation skips this helper; it never reads application credentials.
func TestMySQLExecutionBudgetRestartHelper(t *testing.T) {
	runID := os.Getenv("AGENTFLOW_BUDGET_HELPER_RUN_ID")
	if runID == "" {
		t.Skip("fresh-process helper only")
	}
	s := budgetMySQLStore(t, budgetMySQLDSN(t))
	defer s.Close()
	run, ok := s.RunState(runID)
	if !ok {
		t.Fatal("durable Run missing")
	}
	policy := run.ExecutionContext.ExecutionBudget
	switch os.Getenv("AGENTFLOW_BUDGET_HELPER_MODE") {
	case "resume-cap":
		if run.ExecutionBudget.ConsumedTokens != 150 || run.ExecutionBudget.ConsumedCostMicrosUSD != 80000 || run.ExecutionBudget.Invocations != 2 {
			t.Fatal("restart counters changed")
		}
		node := ""
		for _, receipt := range run.ExecutionBudget.Receipts {
			node = receipt.NodeID
		}
		if err := run.ExecutionBudget.Begin(policy, "resume-after-process-restart", node, "acceptance-provider", 1); err != nil {
			t.Fatal(err)
		}
		if err := run.ExecutionBudget.Observe(policy, node, "resumed-session", domain.TokenUsage{InputTokens: 120, OutputTokens: 30, TotalTokens: 150, CostUSD: .08, CostStatus: "reported"}); err != nil {
			t.Fatal(err)
		}
		if run.ExecutionBudget.ConsumedTokens != 150 {
			t.Fatal("restart replay counted twice")
		}
		if err := run.ExecutionBudget.Observe(policy, node, "resumed-session", domain.TokenUsage{InputTokens: 800, OutputTokens: 200, TotalTokens: 1000, CostUSD: .1, CostStatus: "reported"}); !errors.Is(err, domain.ErrExecutionBudgetStopped) {
			t.Fatal("crossing cap did not stop", err)
		}
		_ = run.ExecutionBudget.Finish(policy, "resume-after-process-restart")
		if err := s.PutRun(run); err != nil {
			t.Fatal(err)
		}
		if err := run.ExecutionBudget.Begin(policy, "post-limit", node, "acceptance-provider", 1); !errors.Is(err, domain.ErrExecutionBudgetStopped) {
			t.Fatal("post-limit invocation was accepted", err)
		}
	case "uncertain":
		if err := domain.ValidateRunExecutionBudget(run); !errors.Is(err, domain.ErrExecutionBudgetStopped) {
			t.Fatal("unfinished restart receipt validated", err)
		}
		if err := run.ExecutionBudget.Begin(policy, "replacement", "a", "acceptance-provider", 1); !errors.Is(err, domain.ErrExecutionBudgetStopped) || run.ExecutionBudget.StopReason != "budget_invocation_uncertain" {
			t.Fatal("restart replay granted allowance", err)
		}
		if err := s.PutRun(run); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown helper mode")
	}
}
