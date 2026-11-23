package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func governedTaskFixture(t *testing.T) RunnerTask {
	t.Helper()
	sha := strings.Repeat("1", 64)
	return RunnerTask{ID: "task", RunID: "run", NodeID: "node", Attempt: 1, Provider: "codex", Invocation: &RunnerInvocation{RunID: "run", NodeID: "node", GovernedContext: &RunnerGovernedContext{Version: 1, IntelligenceSHA256: sha, ProjectID: "project", ReleaseID: "release", ReleaseSHA256: sha, Memories: []RunnerGovernedMemory{{ID: "memory", Revision: 2, SHA256: sha}}, RepositoryBaseSHAs: map[string]string{"repo": strings.Repeat("2", 40)}}, Repositories: []RunnerRepository{{RepositoryID: "repo", BaseSHA: strings.Repeat("2", 40)}}}}
}
func TestGovernedContextManifestBindingAndNegativeCases(t *testing.T) {
	task := governedTaskFixture(t)
	if err := ValidateRunnerGovernedContext(task); err != nil {
		t.Fatal(err)
	}
	first, _ := RunnerContributionContextSHA(task)
	raw, _ := json.Marshal(task)
	cases := map[string]func(*RunnerTask){
		"missing-run":   func(v *RunnerTask) { v.RunID = "" },
		"missing-node":  func(v *RunnerTask) { v.Invocation.NodeID = "" },
		"bad-version":   func(v *RunnerTask) { v.Invocation.GovernedContext.Version = 2 },
		"bad-sha":       func(v *RunnerTask) { v.Invocation.GovernedContext.IntelligenceSHA256 = "invalid" },
		"bad-release":   func(v *RunnerTask) { v.Invocation.GovernedContext.ReleaseID = "" },
		"bad-memory":    func(v *RunnerTask) { v.Invocation.GovernedContext.Memories[0].Revision = 0 },
		"bad-pin":       func(v *RunnerTask) { v.Invocation.Repositories[0].BaseSHA = strings.Repeat("3", 40) },
		"missing-pin":   func(v *RunnerTask) { delete(v.Invocation.GovernedContext.RepositoryBaseSHAs, "repo") },
		"cleanup":       func(v *RunnerTask) { v.Action = RunnerTaskActionCleanupRun },
		"oversize-body": func(v *RunnerTask) { v.Invocation.Agent.Prompt = strings.Repeat("x", IntelligenceMaxSnapshotBytes) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			var v RunnerTask
			_ = json.Unmarshal(raw, &v)
			change(&v)
			if ValidateRunnerGovernedContext(v) == nil {
				t.Fatal("bad governed binding admitted")
			}
		})
	}
	task.Invocation.GovernedContext.IntelligenceSHA256 = strings.Repeat("3", 64)
	changed, _ := RunnerContributionContextSHA(task)
	if first == changed {
		t.Fatal("approved context identity omitted from existing task manifest")
	}
	task.Invocation.GovernedContext = nil
	if ValidateRunnerGovernedContext(task) != nil {
		t.Fatal("legacy task compatibility lost")
	}
}
func TestGovernedContextMinimumRunnerAndReceiptLinkage(t *testing.T) {
	for _, v := range []string{"0.11.0", "0.12.0-rc.1", "0.12", "v0.12.0", "0.12.0junk"} {
		if RunnerVersionSupportsGovernedContext(v) {
			t.Fatalf("old/malformed version accepted %s", v)
		}
	}
	for _, v := range []string{"0.12.0", "0.12.0+build.2", "0.12.1", "1.0.0"} {
		if !RunnerVersionSupportsGovernedContext(v) {
			t.Fatalf("version rejected %s", v)
		}
	}
	task := governedTaskFixture(t)
	sha, _ := RunnerContributionContextSHA(task)
	evidence := &RunnerTaskOutputs{ContextEvidence: &RunnerContextEvidence{Version: 1, TaskID: task.ID, Attempt: task.Attempt, IntelligenceSHA256: task.Invocation.GovernedContext.IntelligenceSHA256, ContextSHA256: sha}}
	if err := ValidateRunnerContextEvidence(task, evidence); err != nil {
		t.Fatal(err)
	}
	if ValidateRunnerContextEvidence(task, nil) == nil {
		t.Fatal("missing consumption evidence accepted")
	}
	evidence.ContextEvidence.Attempt++
	if ValidateRunnerContextEvidence(task, evidence) == nil {
		t.Fatal("stale evidence accepted")
	}
}

func TestGovernedContextEvidenceRequiresAssignedInventory(t *testing.T) {
	task := governedTaskFixture(t)
	task.Invocation.Assets.Rules = []RuleDefinition{{ID: "approved-memory", Content: "reviewed content"}}
	sha, _ := RunnerContributionContextSHA(task)
	evidence := &RunnerTaskOutputs{ContextEvidence: &RunnerContextEvidence{Version: 1, TaskID: task.ID, Attempt: task.Attempt, IntelligenceSHA256: task.Invocation.GovernedContext.IntelligenceSHA256, ContextSHA256: sha}}
	if ValidateRunnerContextEvidence(task, evidence) == nil {
		t.Fatal("missing assigned rule evidence accepted")
	}
	evidence.ContextEvidence.Files = []RunnerMaterializedDigest{{Kind: "rule", ResourceID: "approved-memory", Path: ".agentflow/rules/approved-memory.md", SourceSHA256: strings.Repeat("5", 64), SHA256: strings.Repeat("5", 64), Size: 16, Mode: 0o600}}
	if ValidateRunnerContextEvidence(task, evidence) == nil {
		t.Fatal("forged source digest accepted")
	}
}
