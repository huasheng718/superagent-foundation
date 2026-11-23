package domain

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func budgetFixture(t *testing.T, policy ExecutionBudgetPolicy) (*ExecutionBudgetPolicy, *ExecutionBudgetState) {
	t.Helper()
	s, err := NewExecutionBudgetState(&policy)
	if err != nil {
		t.Fatal(err)
	}
	return &policy, s
}
func TestExecutionBudgetPolicyAndStrictAdmission(t *testing.T) {
	cases := []ExecutionBudgetPolicy{{Mode: "metered", MaxInvocations: 2}, {Mode: "metered", MaxInvocations: 0, MaxTotalTokens: 10}, {Mode: "metered", MaxInvocations: 2, MaxCostMicrosUSD: 1}, {Mode: "metered", MaxInvocations: 2, MaxCostMicrosUSD: 1, Currency: "CNY", PriceVersion: "v1"}, {Mode: "metered", MaxInvocations: 2, MaxNoProgress: 1}}
	for _, p := range cases {
		if err := ValidateExecutionBudgetPolicy(&p); err == nil {
			t.Fatalf("accepted invalid %+v", p)
		}
	}
	p, s := budgetFixture(t, ExecutionBudgetPolicy{Mode: "strict", MaxInvocations: 2, MaxTotalTokens: 100})
	if err := s.Begin(p, "i", "n", "codex", 1); !errors.Is(err, ErrExecutionBudgetUnsupported) {
		t.Fatal(err)
	}
	if s.Invocations != 0 || len(s.Active) != 0 {
		t.Fatal("unsupported strict invocation reserved allowance")
	}
	s.Receipts = nil
	if err := s.Begin(p, "i", "n", "codex", 1); !errors.Is(err, ErrExecutionBudgetStopped) || s.StopReason != "budget_state_invalid" {
		t.Fatalf("corrupt durable state: %v %+v", err, s)
	}
}
func TestExecutionBudgetUsageIsMonotonicAndDurablePerSession(t *testing.T) {
	p, s := budgetFixture(t, ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 3, MaxTotalTokens: 30})
	if err := s.Begin(p, "i1", "n", "codex", 1); err != nil {
		t.Fatal(err)
	}
	for _, u := range []TokenUsage{{InputTokens: 5, OutputTokens: 2}, {InputTokens: 5, OutputTokens: 2}, {InputTokens: 3, OutputTokens: 1}, {InputTokens: 7, OutputTokens: 3}} {
		if err := s.Observe(p, "n", "session", u); err != nil {
			t.Fatal(err)
		}
	}
	if s.ConsumedTokens != 10 {
		t.Fatalf("usage %d", s.ConsumedTokens)
	}
	if err := s.Finish(p, "i1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Begin(p, "i2", "n", "codex", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Observe(p, "n", "session", TokenUsage{InputTokens: 9, OutputTokens: 4}); err != nil {
		t.Fatal(err)
	}
	if s.ConsumedTokens != 13 {
		t.Fatalf("resumed cumulative session counted twice: %d", s.ConsumedTokens)
	}
	if err := s.Observe(p, "n", "different", TokenUsage{InputTokens: 1}); !errors.Is(err, ErrExecutionBudgetStopped) || s.StopReason != "budget_session_changed" {
		t.Fatalf("session drift: %v", err)
	}
	if s.ConsumedTokens != 13 {
		t.Fatal("unknown session spent ledger allowance")
	}
}
func TestExecutionBudgetCapsUnknownAndOverflowFailClosed(t *testing.T) {
	t.Run("token equality and sticky stop", func(t *testing.T) {
		p, s := budgetFixture(t, ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 2, MaxTotalTokens: 10})
		_ = s.Begin(p, "i", "n", "codex", 1)
		if err := s.Observe(p, "n", "s", TokenUsage{TotalTokens: 10}); !errors.Is(err, ErrExecutionBudgetStopped) || s.StopReason != "token_budget_exhausted" {
			t.Fatal(err, s)
		}
		if err := s.Begin(p, "i2", "n2", "codex", 1); !errors.Is(err, ErrExecutionBudgetStopped) || s.Invocations != 1 {
			t.Fatal(err, s)
		}
	})
	t.Run("micro rounding and reported cost", func(t *testing.T) {
		p, s := budgetFixture(t, ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 2, MaxCostMicrosUSD: 2, Currency: "USD", PriceVersion: "test-v1"})
		_ = s.Begin(p, "i", "n", "codex", 1)
		if err := s.Observe(p, "n", "s", TokenUsage{CostUSD: 0.0000001, CostStatus: "reported"}); err != nil || s.ConsumedCostMicrosUSD != 1 {
			t.Fatal(err, s)
		}
		if err := s.Observe(p, "n", "s", TokenUsage{CostUSD: 0.0000011, CostStatus: "reported"}); !errors.Is(err, ErrExecutionBudgetStopped) || s.ConsumedCostMicrosUSD != 2 {
			t.Fatal(err, s)
		}
	})
	for _, kind := range []string{"missing", "estimated", "nan", "overflow", "highwater-overflow"} {
		t.Run(kind, func(t *testing.T) {
			p, s := budgetFixture(t, ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 2, MaxTotalTokens: math.MaxInt64, MaxCostMicrosUSD: 100, Currency: "USD", PriceVersion: "test-v1"})
			_ = s.Begin(p, "i", "n", "codex", 1)
			var err error
			switch kind {
			case "missing":
				err = s.Finish(p, "i")
			case "estimated":
				err = s.Observe(p, "n", "s", TokenUsage{CostStatus: "estimated"})
			case "nan":
				err = s.Observe(p, "n", "s", TokenUsage{CostStatus: "reported", CostUSD: math.NaN()})
			case "overflow":
				err = s.Observe(p, "n", "s", TokenUsage{InputTokens: math.MaxInt64, OutputTokens: 1, CostStatus: "reported"})
			case "highwater-overflow":
				s.Cursors["n\x00s"] = ExecutionBudgetCursor{InputTokens: math.MaxInt64 - 1}
				s.ConsumedTokens = math.MaxInt64 - 1
				err = s.Observe(p, "n", "s", TokenUsage{OutputTokens: 2, CostStatus: "reported"})
			}
			if !errors.Is(err, ErrExecutionBudgetStopped) {
				t.Fatalf("did not fail closed: %v %+v", err, s)
			}
		})
	}
}
func TestExecutionBudgetInvocationAndProgressCannotBeReset(t *testing.T) {
	p, s := budgetFixture(t, ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 3, MaxNoProgress: 2})
	_ = s.Begin(p, "i", "n", "mock", 1)
	if err := s.Begin(p, "i", "n", "mock", 1); !errors.Is(err, ErrExecutionBudgetStopped) || s.StopReason != "budget_invocation_uncertain" {
		t.Fatal(err, s)
	}
	p, s = budgetFixture(t, ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 3, MaxNoProgress: 2})
	if err := s.RecordProgress(p, "n", strings.Repeat("a", 64), true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProgress(p, "n", strings.Repeat("b", 64), true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProgress(p, "n", strings.Repeat("b", 64), true); !errors.Is(err, ErrExecutionBudgetStopped) || s.StopReason != "no_progress" {
		t.Fatal(err, s)
	}
	if err := s.RecordProgress(p, "n", strings.Repeat("c", 64), false); !errors.Is(err, ErrExecutionBudgetStopped) {
		t.Fatal("success reset sticky stop", err)
	}
	p, s = budgetFixture(t, ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 2, MaxNoProgress: 2})
	_ = s.Begin(p, "i", "n", "mock", 1)
	run := Run{ExecutionContext: TelemetryContext{ExecutionBudget: p}, ExecutionBudget: s}
	if err := ValidateRunExecutionBudget(run); !errors.Is(err, ErrExecutionBudgetStopped) {
		t.Fatal("unfinished receipt accepted", err)
	}
	_ = s.Finish(p, "i")
	_ = s.Begin(p, "i2", "n", "mock", 1)
	_ = s.Finish(p, "i2")
	if err := s.Begin(p, "i3", "n", "mock", 1); !errors.Is(err, ErrExecutionBudgetStopped) || s.StopReason != "invocation_budget_exhausted" {
		t.Fatal(err, s)
	}
}

func TestExecutionBudgetTotalOnlyAndExpandedUsageNeverDoubleCount(t *testing.T) {
	p, s := budgetFixture(t, ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 2, MaxTotalTokens: 1000})
	_ = s.Begin(p, "i", "n", "codex", 1)
	for _, u := range []TokenUsage{{TotalTokens: 100}, {InputTokens: 80, OutputTokens: 20, TotalTokens: 100}, {InputTokens: 80, OutputTokens: 20, TotalTokens: 150}} {
		if err := s.Observe(p, "n", "s", u); err != nil {
			t.Fatal(err)
		}
	}
	if s.ConsumedTokens != 150 {
		t.Fatalf("provider total undercounted or total-only snapshot counted twice: %d", s.ConsumedTokens)
	}
}
