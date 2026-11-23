package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBindSandboxPrivateHomeDetachesHostCodexSession(t *testing.T) {
	// A host-only variable must also be explicitly cleared: omitting it from
	// this overlay would let process creation inherit the desktop capability.
	t.Setenv("CODEX_APP_HOST_ONLY_IPC", "host-desktop-socket")
	t.Setenv("CODEX_THREAD_ID", "host-thread")
	for _, driver := range []string{"local", "docker"} {
		t.Run(driver, func(t *testing.T) {
			workspace := t.TempDir()
			env, err := bindSandboxPrivateHome(workspace, driver, "codex", map[string]string{
				"CODEX_API_KEY":                           "fixture-personal-key",
				"CODEX_APP_SERVER_SOCKET":                 "explicit-desktop-socket",
				"CODEX_INTERNAL_ORIGINATOR_OVERRIDE":      "desktop-originator",
				"CODEX_PERMISSION_PROFILE":                "host-profile",
				"CODEX_SESSION_ID":                        "host-session",
				"CODEX_TOOL_CALL_ID":                      "host-tool-call",
				"CODEX_TASK_WORKSPACE_VERIFYING_IDENTITY": "host-workspace-identity",
				"CODEX_HOME":                              "/host-private-config",
				"PATH":                                    "/fixture/runtime/bin",
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{
				"CODEX_APP_HOST_ONLY_IPC", "CODEX_APP_SERVER_SOCKET", "CODEX_THREAD_ID",
				"CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "CODEX_PERMISSION_PROFILE",
				"CODEX_SESSION_ID", "CODEX_TOOL_CALL_ID", "CODEX_TASK_WORKSPACE_VERIFYING_IDENTITY",
			} {
				if value, exists := env[key]; !exists || value != "" {
					t.Errorf("host capability %s must be explicitly empty", key)
				}
			}
			if env["CODEX_API_KEY"] != "fixture-personal-key" || env["PATH"] != "/fixture/runtime/bin" {
				t.Fatal("personal credential or runtime path lost")
			}
			root := workspace
			if driver == "docker" {
				root = "/workspace"
			}
			wantHome := filepath.Join(root, ".agentflow", "private-home", "codex")
			if env["HOME"] != filepath.ToSlash(wantHome) || env["CODEX_HOME"] != filepath.ToSlash(filepath.Join(wantHome, ".codex")) || env["TMPDIR"] != filepath.ToSlash(filepath.Join(root, ".agentflow", "tmp", "codex")) {
				t.Fatal("private home/config/temp does not point into the task workspace")
			}
			for _, rel := range []string{".agentflow/private-home/codex", ".agentflow/private-home/codex/.codex", ".agentflow/tmp/codex"} {
				info, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(rel)))
				if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
					t.Fatalf("private directory %s is missing or accessible by other users", rel)
				}
			}
		})
	}
}
