package runtime

import (
	"testing"

	"agentflow/internal/domain"
)

func TestCodexProtocolBindsSparseFramesToInvocationThread(t *testing.T) {
	var events []domain.TelemetryEvent
	observer := NewProtocolObserver(domain.ExecutorDefinition{Kind: "codex", Protocol: "jsonl"}, func(event domain.TelemetryEvent) {
		events = append(events, event)
	})
	for _, line := range []string{
		`{"type":"thread.started","thread_id":"thread-first"}`,
		`{"type":"turn.started"}`,
		`{"type":"turn.completed","usage":{"input_tokens":73055,"cached_input_tokens":62336,"output_tokens":940}}`,
		`{"type":"turn.started","threadId":"thread-explicit"}`,
		`{"type":"turn.completed","usage":{"input_tokens":2,"output_tokens":1}}`,
	} {
		observer.Handle(domain.TelemetryEvent{Name: "process.stdout", Message: line})
	}
	if len(events) != 7 {
		t.Fatalf("normalized events=%d, want 7", len(events))
	}
	for i, event := range events {
		want := "thread-first"
		if i >= 4 {
			want = "thread-explicit"
		}
		if event.SessionID != want || event.Context.SessionID != want {
			t.Fatalf("event %d lost per-invocation thread: %+v", i, event)
		}
	}
	if events[2].Usage == nil || events[2].Usage.InputTokens != 73055 || events[2].Usage.OutputTokens != 940 || events[2].Usage.CacheReadTokens != 62336 {
		t.Fatalf("sparse usage changed: %+v", events[2])
	}

	// A new invocation must not borrow the previous observer's identity.
	var isolated []domain.TelemetryEvent
	next := NewProtocolObserver(domain.ExecutorDefinition{Kind: "codex", Protocol: "jsonl"}, func(event domain.TelemetryEvent) {
		isolated = append(isolated, event)
	})
	next.Handle(domain.TelemetryEvent{Name: "process.stdout", Message: `{"type":"turn.completed","usage":{"input_tokens":2,"output_tokens":1}}`})
	if len(isolated) != 2 || isolated[0].SessionID != "" || isolated[0].Context.SessionID != "" {
		t.Fatalf("unknown thread borrowed another invocation: %+v", isolated)
	}
}
