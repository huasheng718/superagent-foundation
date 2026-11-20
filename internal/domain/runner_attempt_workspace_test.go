package domain

import (
	"strings"
	"testing"
)

func TestIsolatedWorkspaceIdentityRejectsAmbiguityAndStaleAttempts(t *testing.T) {
	task := RunnerTask{ID: "task/a", RunID: "run", NodeID: "node", Attempt: 1, Invocation: &RunnerInvocation{RunID: "run", NodeID: "node", WorkspaceIsolation: &RunnerWorkspaceIsolation{Version: 1, ContextSHA256: strings.Repeat("c", 64)}}}
	first, err := IsolatedWorkspaceDescriptor(task)
	if err != nil {
		t.Fatal(err)
	}
	task.ID = "task?a"
	second, err := IsolatedWorkspaceDescriptor(task)
	if err != nil || first.WorkspaceRel == second.WorkspaceRel || first.Branch == second.Branch {
		t.Fatal("lossy sanitization aliases distinct task identities")
	}
	task.Attempt++
	third, err := IsolatedWorkspaceDescriptor(task)
	if err != nil || second.WorkspaceRel == third.WorkspaceRel || second.Branch == third.Branch {
		t.Fatal("reclaimed attempt aliases the old workspace")
	}
	task.Attempt = 0
	if _, err := IsolatedWorkspaceDescriptor(task); err == nil {
		t.Fatal("unclaimed attempt accepted")
	}
	task.Attempt = 1
	task.Invocation.NodeID = "spoofed"
	if _, err := IsolatedWorkspaceDescriptor(task); err == nil {
		t.Fatal("mismatched frozen identity accepted")
	}
}

func TestWorkspaceIsolationRejectsUnsupportedBaseFormatsAndOldRunners(t *testing.T) {
	if !ValidWorkspaceBaseSHA(strings.Repeat("a", 40)) {
		t.Fatal("full SHA-1 rejected")
	}
	for _, value := range []string{"main", "abc123", strings.Repeat("a", 64), strings.Repeat("A", 40), strings.Repeat("z", 40)} {
		if ValidWorkspaceBaseSHA(value) {
			t.Fatalf("unsupported base %q accepted", value)
		}
	}
	if (Runner{RunnerVersion: "0.9.2"}).SupportsWorkspaceIsolation() || !(Runner{RunnerVersion: RunnerVersionWorkspaceIsolation}).SupportsWorkspaceIsolation() {
		t.Fatal("workspace capability gate is not enforced")
	}
}
