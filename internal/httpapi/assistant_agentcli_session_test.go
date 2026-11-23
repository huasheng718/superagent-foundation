package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/runtime"
)

func TestAssistantAgentCLISessionLifecycle(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(config, []byte(`{"mcpServers":{"gateway":{"url":"http://127.0.0.1:1/mcp","tools":{"defer_loading":true}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{runtime.AgentCLIPi, runtime.AgentCLIKimi, runtime.AgentCLICursor} {
		t.Run(kind, func(t *testing.T) {
			opts := assistantEngineInvocationOpts{Root: root, Config: config, Bin: "test-cli", SessionID: "chat", SystemPrompt: "approved tools only", Prompt: "hello"}
			inv, err := prepareAssistantAgentCLIInvocation(kind, opts)
			if err != nil || inv.resumeMissing {
				t.Fatalf("prepare: %v", err)
			}
			if len(inv.command.CredentialBindings) != 1 || inv.command.CredentialBindings[0] != runtime.BindAgentCLI {
				t.Fatal("invocation lost closed credential binding")
			}
			// The common translated event boundary retains native identity and
			// converts failed results into errors visible to the chat pipeline.
			native := ""
			line := assistantAgentCLILine(kind, `{"type":"system","session_id":"native-`+kind+`"}`, &native)
			if native != "native-"+kind || !strings.Contains(line, native) {
				t.Fatal("native session lost")
			}
			failed := assistantAgentCLILine(kind, `{"type":"result","is_error":true,"result":"invalid api key"}`, &native)
			if !strings.Contains(failed, `"type":"error"`) || !strings.Contains(failed, "invalid api key") {
				t.Fatal(failed)
			}
			if !strings.Contains(assistantAgentCLILine(kind, `{"type":"result","is_error":true}`, &native), `"type":"error"`) {
				t.Fatal("empty failed result disappeared")
			}
			opts.Resume = true
			missing, err := prepareAssistantAgentCLIInvocation(kind, opts)
			if err != nil || !missing.resumeMissing {
				t.Fatal("unrecorded session must not silently restart")
			}
			if err := writeAgentCLISession(root, kind, "chat", native); err != nil {
				t.Fatal(err)
			}
			resumed, err := prepareAssistantAgentCLIInvocation(kind, opts)
			if err != nil || resumed.resumeMissing || resumed.sessionRef != native {
				t.Fatalf("resume: %v ref=%s", err, resumed.sessionRef)
			}
			if readAgentCLISession(root, kind, "another-chat") != "" {
				t.Fatal("session crossed chat boundary")
			}
		})
	}
	// A real translated Pi transcript only persists a native session on a
	// successful finish; a failed process cannot replace resumable history.
	opts := assistantEngineInvocationOpts{Root: root, Config: config, Bin: "pi", SessionID: "pi-transcript"}
	for _, success := range []bool{false, true} {
		inv, err := prepareAssistantAgentCLIInvocation(runtime.AgentCLIPi, opts)
		if err != nil {
			t.Fatal(err)
		}
		out := inv.translate(`{"type":"session","id":"native-transcript"}`)
		if len(out) != 1 || !strings.Contains(out[0], `"session_id":"native-transcript"`) {
			t.Fatal(out)
		}
		inv.finish(success)
		if got := readAgentCLISession(root, runtime.AgentCLIPi, opts.SessionID); (got != "") != success {
			t.Fatalf("finish(%v) retained %q", success, got)
		}
	}
	for _, invalid := range []string{"", "--execute", "has space", "a/../../b", "line\nbreak"} {
		if agentCLISessionValid(invalid) {
			t.Fatalf("unsafe native session accepted: %q", invalid)
		}
	}
	data, err := os.ReadFile(agentCLISessionsPath(root))
	var sessions map[string]string
	if err != nil || json.Unmarshal(data, &sessions) != nil || len(sessions) != 4 {
		t.Fatalf("provider session mappings were overwritten: %v %s", err, data)
	}
	info, err := os.Stat(agentCLISessionsPath(root))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("session map must be private")
	}
}
