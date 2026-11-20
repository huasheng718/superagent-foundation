package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateCodexDetachesHostDesktopSession(t *testing.T) {
	for _, key := range []string{"CODEX_APP_TOOLS_PIPE_PATH", "CODEX_APP_FUTURE_CAPABILITY", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_TOOL_CALL_ID", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "CODEX_PERMISSION_PROFILE", "CODEX_TASK_WORKSPACE_VERIFYING_IDENTITY"} {
		t.Setenv(key, "host-session-capability")
	}
	workspace := t.TempDir()
	sb := &localSandbox{workspace: workspace}
	bound, err := bindCommand(sb, workspace, CommandSpec{
		Argv: []string{"codex", "exec", "test"}, Provider: "codex", PrivateHome: true,
		CredentialBindings: []CredentialBinding{BindCodex},
		environment:        map[string]string{"CODEX_APP_COMMAND_CAPABILITY": "command-overlay"},
	}, RunCredentials{Codex: ProviderCredential{APIKey: "task-key"}})
	if err != nil {
		t.Fatal(err)
	}
	bound.Argv = []string{"sh", "-c", `
for name in CODEX_APP_TOOLS_PIPE_PATH CODEX_APP_FUTURE_CAPABILITY CODEX_APP_COMMAND_CAPABILITY CODEX_THREAD_ID CODEX_SESSION_ID CODEX_TOOL_CALL_ID CODEX_INTERNAL_ORIGINATOR_OVERRIDE CODEX_PERMISSION_PROFILE CODEX_TASK_WORKSPACE_VERIFYING_IDENTITY; do
  eval 'value=${'"$name"'-}'
  test -z "$value" || exit 51
done
test "$CODEX_API_KEY" = task-key || exit 52
test "$CODEX_HOME" = "$HOME/.codex" || exit 53
printf detached
`}
	out, err := sb.Exec(context.Background(), bound, nil)
	if err != nil || strings.TrimSpace(out) != "detached" {
		t.Fatalf("private Codex process isolation failed: %v, %q", err, out)
	}
	if bound.environment["CODEX_HOME"] != filepath.Join(workspace, ".agentflow", "private-home", "codex", ".codex") {
		t.Fatal("private Codex home changed")
	}
}
