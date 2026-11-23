package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func TestRunnerCodexSparseSessionBudgetAssociation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		frames     []string
		maxTokens  int64
		wantTokens int64
		wantStop   string
	}{
		{
			name: "thread established in this invocation",
			frames: []string{
				`{"type":"thread.started","thread_id":"current-thread"}`,
				`{"type":"turn.started"}`,
				`{"type":"turn.completed","usage":{"input_tokens":73055,"cached_input_tokens":62336,"output_tokens":940}}`,
			},
			maxTokens: 131072, wantTokens: 73995,
		},
		{
			name:      "unknown thread cannot inherit previous Node session",
			frames:    []string{`{"type":"turn.completed","usage":{"input_tokens":73055,"output_tokens":940}}`},
			maxTokens: 131072, wantStop: "budget_usage_unknown",
		},
		{
			name: "explicit changed thread still refused",
			frames: []string{
				`{"type":"thread.started","thread_id":"current-thread"}`,
				`{"type":"turn.completed","usage":{"input_tokens":10}}`,
				`{"type":"turn.completed","thread_id":"different-thread","usage":{"input_tokens":20}}`,
			},
			maxTokens: 131072, wantTokens: 10, wantStop: "budget_session_changed",
		},
		{
			name: "sparse usage enforces token limit",
			frames: []string{
				`{"type":"thread.started","thread_id":"current-thread"}`,
				`{"type":"turn.completed","usage":{"input_tokens":73055,"output_tokens":940}}`,
			},
			maxTokens: 73995, wantTokens: 73995, wantStop: "token_budget_exhausted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s := newRecoveryEngine(t)
			policy := &domain.ExecutionBudgetPolicy{Mode: "metered", MaxInvocations: 1, MaxTotalTokens: tc.maxTokens}
			state, err := domain.NewExecutionBudgetState(policy)
			if err != nil {
				t.Fatal(err)
			}
			run := domain.Run{ID: "runner-budget", Status: "running", ExecutionContext: domain.TelemetryContext{ExecutionBudget: policy}, ExecutionBudget: state, Nodes: map[string]*domain.NodeRun{
				"work": {NodeID: "work", Status: "running", Attempt: 1, SessionID: "previous-node-thread"},
			}}
			if err := s.PutRun(run); err != nil {
				t.Fatal(err)
			}
			if err := e.beginBudgetInvocation(run.ID, "work", 1, "work-1-1", "codex"); err != nil {
				t.Fatal(err)
			}
			task := domain.RunnerTask{Provider: "codex", Invocation: &domain.RunnerInvocation{InvocationID: "work-1-1", Executor: domain.ExecutorDefinition{Kind: "codex", Protocol: "jsonl"}}}
			var events []domain.RunnerTaskEvent
			observer := runtime.NewProtocolObserver(task.Invocation.Executor, func(event domain.TelemetryEvent) {
				raw, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, domain.RunnerTaskEvent{Seq: int64(len(events) + 1), Type: "observation", Data: string(raw), Ts: time.Now().UTC()})
			})
			for _, frame := range tc.frames {
				observer.Handle(domain.TelemetryEvent{Name: "process.stdout", Message: frame})
			}
			sink := func(event domain.TelemetryEvent) { e.observe(run.ID, "work", event) }
			after := bridgeRunnerEventsToSink(task, events, 0, sink)
			// Durable polling replay must not count the same final usage twice.
			bridgeRunnerEventsToSink(task, events, after, sink)
			err = e.finishBudgetInvocation(run.ID, "work", 1, "work-1-1")
			if tc.wantStop == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantStop != "" && !errors.Is(err, domain.ErrExecutionBudgetStopped) {
				t.Fatalf("unsafe budget completion: %v", err)
			}
			got, _ := s.RunState(run.ID)
			if got.ExecutionBudget.ConsumedTokens != tc.wantTokens || got.ExecutionBudget.StopReason != tc.wantStop {
				t.Fatalf("budget mismatch: %+v", got.ExecutionBudget)
			}
			if tc.wantStop == "" {
				receipt := got.ExecutionBudget.Receipts["work-1-1"]
				if !receipt.UsageKnown || !receipt.Finished || receipt.SessionID != "current-thread" || len(got.ExecutionBudget.Active) != 0 || len(got.ExecutionBudget.Cursors) != 1 || got.Nodes["work"].Usage.TotalTokens != tc.wantTokens {
					t.Fatalf("missing exact invocation usage: %+v node=%+v", got.ExecutionBudget, got.Nodes["work"])
				}
			}
		})
	}
}
