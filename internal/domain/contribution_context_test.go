package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContributionContextBindsAssignmentWithoutCredentialsOrLease(t *testing.T) {
	task := RunnerTask{ID: "task", RunID: "run", NodeID: "node", Provider: "codex", DeliveryPolicy: DeliveryPolicyControlledPR, Invocation: &RunnerInvocation{RunID: "run", NodeID: "node", WorkspaceIsolation: &RunnerWorkspaceIsolation{Version: 1}, CodexAPIKey: "first", Repositories: []RunnerRepository{{RepositoryID: "repo", BaseSHA: "1111111111111111111111111111111111111111", GitToken: "first"}}, Inputs: []RunnerInputFile{{Path: "source.txt", Content: "frozen input"}}}}
	first, err := RunnerContributionContextSHA(task)
	if err != nil {
		t.Fatal(err)
	}
	task.Invocation.WorkspaceIsolation.ContextSHA256 = first
	task.Invocation.CodexAPIKey = "rotated"
	task.Invocation.Repositories[0].GitToken = "rotated"
	task.Attempt = 2
	task.LeaseOwner = "runner"
	task.Status = RunnerTaskRunning
	same, err := RunnerContributionContextSHA(task)
	if err != nil || first != same {
		t.Fatal("credential/lease/hash changed immutable context")
	}
	task.Invocation.Inputs[0].Content = "changed input"
	different, err := RunnerContributionContextSHA(task)
	if err != nil || different == first {
		t.Fatal("input change did not invalidate context")
	}
	task.Invocation.Inputs[0].Content = "frozen input"
	task.Invocation.Repositories[0].BaseSHA = "2222222222222222222222222222222222222222"
	different, err = RunnerContributionContextSHA(task)
	if err != nil || different == first {
		t.Fatal("base change did not invalidate context")
	}
	if task.Invocation.WorkspaceIsolation.ContextSHA256 != first || task.Invocation.CodexAPIKey != "rotated" || task.Invocation.Repositories[0].GitToken != "rotated" {
		t.Fatal("digest mutated original assignment")
	}
}

func legacyNodePlacementContextFixture() RunnerTask {
	return RunnerTask{ID: "legacy-task", RunID: "run", NodeID: "node", Provider: "terminal", Command: "go test ./...", DeliveryPolicy: DeliveryPolicyControlledPR, Invocation: &RunnerInvocation{RunID: "run", NodeID: "node", WorkspaceIsolation: &RunnerWorkspaceIsolation{Version: 1}, Repositories: []RunnerRepository{{RepositoryID: "repo", BaseSHA: "1111111111111111111111111111111111111111", GitToken: "secret"}}}}
}

func TestContributionContextLegacyGoldenBeforePlacement(t *testing.T) {
	// Captured by the unmodified version-1 implementation at 41f26a5 before
	// adding any placement fields; do not regenerate from this implementation.
	const golden = "00ece63073b4b4d0b2f4fe53a4c32938b450d17ffa396f374b19212d0b27540d"
	task := legacyNodePlacementContextFixture()
	for _, empty := range []bool{false, true} {
		if empty {
			task.Invocation.ContributionInputs = []RunnerContributionInput{}
		}
		// Legacy manifests deliberately did not bind the RunnerID.
		task.RunnerID = "legacy-bound-runner"
		sha, err := RunnerContributionContextSHA(task)
		if err != nil || sha != golden {
			t.Fatalf("version-1 digest changed: %q %v", sha, err)
		}
	}
	for _, value := range []any{TelemetryContext{}, NodeRun{}, RunnerTask{}, RunnerInvocation{}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"nodePlacements", "executionPlacement", "executionSequence", "executionTaskId", "contributionInputs", "maintenanceKey", `"execution"`} {
			if strings.Contains(string(raw), field) {
				t.Fatalf("empty additive field changed old JSON: %s", raw)
			}
		}
	}
}

func placedContextFixture(t *testing.T) RunnerTask {
	t.Helper()
	task := legacyNodePlacementContextFixture()
	p := placementFixture(t)
	task.RunnerID = p.RunnerID
	task.ExecutionPlacement = &p
	task.Execution = &NodeTaskExecution{Role: NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: 1}
	task.Invocation.ContributionInputs = []RunnerContributionInput{{RepositoryID: "repo", BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40), BundleSHA256: strings.Repeat("3", 64), BundleSize: 123, Sources: []RunnerContributionInputSource{{RunID: "run", NodeID: "previous", TaskID: "source", ReceiptID: "receipt", NodeAttempt: 1, TaskAttempt: 1, ContextSHA256: strings.Repeat("4", 64), BundleSHA256: strings.Repeat("5", 64), ResultSHA: strings.Repeat("2", 40)}}}}
	return task
}

func TestContributionContextBindsPlacementExecutionAndCodeInputs(t *testing.T) {
	base, err := RunnerContributionContextSHA(placedContextFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RunnerTask){
		"runner":            func(task *RunnerTask) { task.RunnerID = "other"; task.ExecutionPlacement.RunnerID = "other" },
		"auth":              func(task *RunnerTask) { task.ExecutionPlacement.Runtimes[1].AuthMode = RuntimeAuthPersonal },
		"nodeAttempt":       func(task *RunnerTask) { task.ExecutionPlacement.NodeAttempt++; task.Execution.NodeAttempt++ },
		"role":              func(task *RunnerTask) { task.Execution.Role = NodeTaskRoleVerification },
		"sequence":          func(task *RunnerTask) { task.Execution.Sequence++ },
		"previousAuthor":    func(task *RunnerTask) { task.Execution.PreviousAuthorTaskID = "previous-author" },
		"inputHead":         func(task *RunnerTask) { task.Invocation.ContributionInputs[0].HeadSHA = strings.Repeat("6", 40) },
		"inputBase":         func(task *RunnerTask) { task.Invocation.ContributionInputs[0].BaseSHA = strings.Repeat("6", 40) },
		"inputBundle":       func(task *RunnerTask) { task.Invocation.ContributionInputs[0].BundleSHA256 = strings.Repeat("6", 64) },
		"inputSize":         func(task *RunnerTask) { task.Invocation.ContributionInputs[0].BundleSize++ },
		"inputRepository":   func(task *RunnerTask) { task.Invocation.ContributionInputs[0].RepositoryID = "other" },
		"sourceReceipt":     func(task *RunnerTask) { task.Invocation.ContributionInputs[0].Sources[0].ReceiptID = "other" },
		"sourceTask":        func(task *RunnerTask) { task.Invocation.ContributionInputs[0].Sources[0].TaskID = "other" },
		"sourceTaskAttempt": func(task *RunnerTask) { task.Invocation.ContributionInputs[0].Sources[0].TaskAttempt++ },
		"sourceNodeAttempt": func(task *RunnerTask) { task.Invocation.ContributionInputs[0].Sources[0].NodeAttempt++ },
		"sourceContext": func(task *RunnerTask) {
			task.Invocation.ContributionInputs[0].Sources[0].ContextSHA256 = strings.Repeat("6", 64)
		},
		"sourceBundle": func(task *RunnerTask) {
			task.Invocation.ContributionInputs[0].Sources[0].BundleSHA256 = strings.Repeat("6", 64)
		},
		"sourceResult": func(task *RunnerTask) {
			task.Invocation.ContributionInputs[0].Sources[0].ResultSHA = strings.Repeat("6", 40)
		},
	} {
		t.Run(name, func(t *testing.T) {
			task := placedContextFixture(t)
			mutate(&task)
			sha, err := RunnerContributionContextSHA(task)
			if err != nil || sha == base {
				t.Fatalf("metadata mutation did not change version-2 digest: %v", err)
			}
		})
	}
	task := placedContextFixture(t)
	task.Invocation.CodexAPIKey = "rotated"
	task.Invocation.Repositories[0].GitToken = "rotated"
	task.Invocation.WorkspaceIsolation.ContextSHA256 = base
	task.Attempt = 3
	task.LeaseOwner = "other"
	sha, err := RunnerContributionContextSHA(task)
	if err != nil || sha != base {
		t.Fatal("credentials, task lease or self digest changed placed assignment digest")
	}
	// Canonical placement order has no effect on the assignment hash.
	task = placedContextFixture(t)
	task.ExecutionPlacement.Runtimes[0], task.ExecutionPlacement.Runtimes[1] = task.ExecutionPlacement.Runtimes[1], task.ExecutionPlacement.Runtimes[0]
	task.ExecutionPlacement.Runtimes[0].Capabilities = []string{"json", "tools"}
	sha, err = RunnerContributionContextSHA(task)
	if err != nil || sha != base {
		t.Fatal("canonical inventory permutation changed assignment identity")
	}
	if task.ExecutionPlacement.CapabilitySHA256 != "" || task.ExecutionPlacement.Runtimes[0].Provider != "codex" {
		t.Fatal("digest mutated source placement")
	}
}

func TestContributionContextNewMetadataSelectsVersionTwo(t *testing.T) {
	legacy := legacyNodePlacementContextFixture()
	old, err := RunnerContributionContextSHA(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RunnerTask){
		"placementOnly": func(task *RunnerTask) { p := placementFixture(t); task.ExecutionPlacement = &p },
		"executionOnly": func(task *RunnerTask) {
			task.Execution = &NodeTaskExecution{Role: NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: 1}
		},
		"inputOnly": func(task *RunnerTask) {
			task.Invocation.ContributionInputs = placedContextFixture(t).Invocation.ContributionInputs
		},
		"maintenanceOnly": func(task *RunnerTask) {
			task.MaintenanceKey = &RunnerMaintenanceKey{RunID: "run", Action: RunnerTaskActionCleanupRun, RunnerID: "runner", RegistrationSHA256: strings.Repeat("a", 64)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			task := legacyNodePlacementContextFixture()
			mutate(&task)
			first, err := RunnerContributionContextSHA(task)
			if err != nil || first == old {
				t.Fatalf("new metadata reused version-1 identity: %v", err)
			}
			task.RunnerID = "other"
			second, err := RunnerContributionContextSHA(task)
			if err != nil || second == first {
				t.Fatal("version-2 identity failed to bind target")
			}
		})
	}
}
