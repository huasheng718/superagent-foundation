package httpapi

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Drive the public stdio transcript without a provider account. These tests
// protect the chat boundary: read-only turns, streamed blocks, tool identity,
// usage and resumable sessions must survive translation together.
func codexProtocolFixture(t *testing.T) (assistantEngineInvocationOpts, assistantCLIInvocation, *json.Decoder) {
	t.Helper()
	root := t.TempDir()
	config := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(config, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	opts := assistantEngineInvocationOpts{Bin: "codex", Root: root, Config: config, Prompt: "answer", Model: "test-model", Effort: "high", SessionID: "chat-1", SystemPrompt: "use the approved gateway"}
	inv, err := prepareAssistantCodexInvocation(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inv.finish(false) })
	return opts, inv, json.NewDecoder(inv.command.Stdin)
}

func readCodexRequest(t *testing.T, decoder *json.Decoder, method string) map[string]any {
	t.Helper()
	var message map[string]any
	if err := decoder.Decode(&message); err != nil {
		t.Fatal(err)
	}
	if message["method"] != method {
		t.Fatalf("want %s, received %v", method, message)
	}
	return message
}

func TestAssistantCodexProtocolTurnAndResume(t *testing.T) {
	opts, inv, decoder := codexProtocolFixture(t)
	readCodexRequest(t, decoder, "initialize")
	inv.translate(`{"id":1,"result":{}}`)
	readCodexRequest(t, decoder, "initialized")
	start := readCodexRequest(t, decoder, "thread/start")["params"].(map[string]any)
	if start["sandbox"] != "read-only" || start["approvalPolicy"] != "never" || start["developerInstructions"] != opts.SystemPrompt || start["model"] != opts.Model {
		t.Fatalf("unsafe or incomplete thread: %v", start)
	}
	init := inv.translate(`{"id":2,"result":{"thread":{"id":"thread-123"}}}`)
	if !strings.Contains(strings.Join(init, ""), `"session_id":"thread-123"`) {
		t.Fatal(init)
	}
	turn := readCodexRequest(t, decoder, "turn/start")["params"].(map[string]any)
	if turn["threadId"] != "thread-123" || turn["input"].([]any)[0].(map[string]any)["text"] != opts.Prompt {
		t.Fatal(turn)
	}
	inv.translate(`{"id":3,"result":{}}`)
	for _, line := range []string{"", "diagnostic", "{bad json", `{"method":"unknown","params":{}}`, `{"method":"item/started","params":0}`} {
		if got := inv.translate(line); len(got) != 0 {
			t.Fatalf("unexpected protocol output: %v", got)
		}
	}
	var output []string
	for _, line := range []string{
		`{"method":"item/reasoning/summaryTextDelta","params":{"itemId":"r","delta":"consider"}}`,
		`{"method":"item/reasoning/textDelta","params":{"itemId":"r","delta":"evidence"}}`,
		`{"method":"item/completed","params":{"item":{"id":"r","type":"reasoning","content":["evidence"]}}}`,
		`{"method":"item/agentMessage/delta","params":{"itemId":"a","delta":"hello"}}`,
		`{"method":"item/agentMessage/delta","params":{"itemId":"a","delta":" world"}}`,
		`{"method":"item/completed","params":{"item":{"id":"a","type":"agentMessage","text":"hello world"}}}`,
		`{"method":"item/started","params":{"item":{"id":"tool-1","type":"mcpToolCall","server":"platform","tool":"read","arguments":{"id":"approved"}}}}`,
		`{"method":"item/completed","params":{"item":{"id":"tool-1","type":"mcpToolCall","server":"platform","tool":"read","status":"completed"}}}`,
		`{"method":"thread/tokenUsage/updated","params":{"tokenUsage":{"last":{"inputTokens":11,"cachedInputTokens":4,"outputTokens":7}}}}`,
		`{"method":"turn/completed","params":{"turn":{"status":"completed"}}}`,
	} {
		output = append(output, inv.translate(line)...)
	}
	joined := strings.Join(output, "\n")
	for _, fragment := range []string{`"thinking_delta"`, `"text_delta"`, `"content_block_stop"`, `"name":"mcp__platform__read"`, `"tool_use_id":"tool-1"`, `"result":"hello world"`, `"input_tokens":11`, `"cached_input_tokens":4`, `"output_tokens":7`, `"is_error":false`} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("missing %s in %s", fragment, joined)
		}
	}
	if strings.Count(joined, `"type":"tool_use"`) != 1 {
		t.Fatal("started/completed duplicated the tool call")
	}
	if _, err := io.ReadAll(inv.command.Stdin); err != nil {
		t.Fatal(err)
	}
	inv.finish(true)
	if readCodexThread(opts.Root, opts.SessionID) != "thread-123" {
		t.Fatal("successful native session was not retained")
	}
	opts.Resume = true
	resumed, err := prepareAssistantCodexInvocation(opts)
	if err != nil || resumed.resumeMissing {
		t.Fatalf("resume: %v, missing=%v", err, resumed.resumeMissing)
	}
	defer resumed.finish(false)
	decoder = json.NewDecoder(resumed.command.Stdin)
	readCodexRequest(t, decoder, "initialize")
	resumed.translate(`{"id":1,"result":{}}`)
	readCodexRequest(t, decoder, "initialized")
	if readCodexRequest(t, decoder, "thread/resume")["params"].(map[string]any)["threadId"] != "thread-123" {
		t.Fatal("resume lost native identity")
	}
	failed := resumed.translate(`{"id":2,"error":{"message":"gone"}}`)
	if !strings.Contains(strings.Join(failed, ""), "No conversation found with session ID: thread-123") {
		t.Fatal(failed)
	}
	opts.SessionID = "missing"
	missing, err := prepareAssistantCodexInvocation(opts)
	if err != nil || !missing.resumeMissing {
		t.Fatal("missing history must not start an unrelated turn")
	}
}

func TestAssistantCodexProtocolFailuresAndToolResults(t *testing.T) {
	for _, stage := range []string{"initialize", "thread", "turn", "notification"} {
		t.Run(stage, func(t *testing.T) {
			opts, inv, decoder := codexProtocolFixture(t)
			readCodexRequest(t, decoder, "initialize")
			if stage != "initialize" {
				inv.translate(`{"id":1,"result":{}}`)
				readCodexRequest(t, decoder, "initialized")
				readCodexRequest(t, decoder, "thread/start")
			}
			if stage == "turn" || stage == "notification" {
				inv.translate(`{"id":2,"result":{"thread":{"id":"failed-thread"}}}`)
				readCodexRequest(t, decoder, "turn/start")
			}
			line := `{"id":1,"error":{"message":"denied"}}`
			switch stage {
			case "thread":
				line = `{"id":2,"error":{"message":"denied"}}`
			case "turn":
				line = `{"id":3,"error":{"message":"denied"}}`
			case "notification":
				inv.translate(`{"method":"error","params":{"error":{"message":"transient"},"willRetry":true}}`)
				inv.translate(`{"method":"error","params":{"error":{"message":"denied"},"willRetry":false}}`)
				line = `{"method":"turn/completed","params":{"turn":{"status":"failed"}}}`
			}
			out := strings.Join(inv.translate(line), "")
			if !strings.Contains(out, `"type":"error"`) || !strings.Contains(out, "denied") {
				t.Fatal(out)
			}
			inv.finish(false)
			if readCodexThread(opts.Root, opts.SessionID) != "" {
				t.Fatal("failed session persisted")
			}
		})
	}
	_, inv, decoder := codexProtocolFixture(t)
	readCodexRequest(t, decoder, "initialize")
	inv.translate(`{"id":99,"method":"item/commandExecution/requestApproval","params":{}}`)
	var denied map[string]any
	if err := decoder.Decode(&denied); err != nil || denied["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("approval must be rejected: %v %v", denied, err)
	}
	for _, item := range []string{
		`{"id":"shell","type":"commandExecution","command":"false","exitCode":1}`,
		`{"id":"web","type":"webSearch","query":"query","status":"declined"}`,
		`{"id":"mcp","type":"mcpToolCall","server":"platform","tool":"read","error":{"message":"denied"}}`,
	} {
		out := strings.Join(inv.translate(`{"method":"item/completed","params":{"item":`+item+`}}`), "")
		if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"type":"tool_result"`) || !strings.Contains(out, `"is_error":true`) {
			t.Fatalf("failed tool lost its result: %s", out)
		}
	}
}
