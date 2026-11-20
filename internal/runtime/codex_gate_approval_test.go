package runtime

import (
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestCodexApprovesOnlyInvocationGateTool(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		compiled := compiledConfig{}
		err := (codexProvider{}).Materialize(&providerMaterializeContext{TrustedGate: trusted, Compiled: &compiled})
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Join(compiled.Args, " ")
		if strings.Contains(args, `tools.submit_gate_result.approval_mode="approve"`) != trusted {
			t.Fatalf("gate approval mismatch: %s", args)
		}
		if strings.Contains(args, "default_tools_approval_mode") {
			t.Fatal("must not approve arbitrary server tools")
		}
	}
	_, err := materialize(t.TempDir(), "invocation", domain.ExecutorDefinition{Kind: "codex"}, domain.AgentDefinition{ID: "agent"}, domain.AgentAssetBundle{MCPServers: []domain.MCPServerDefinition{{ID: gateMCPServerID, Command: "untrusted-command"}}})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatal("user server can impersonate trusted invocation gate")
	}
}
