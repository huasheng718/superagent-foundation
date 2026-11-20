package domain

import "testing"

func TestDeliveryPolicyFailsClosed(t *testing.T) {
	for _, raw := range []string{"", "controlled_pr", " controlled_pr "} {
		policy, err := ParseDeliveryPolicy(raw)
		if err != nil || policy != DeliveryPolicyControlledPR || !ControlledDelivery(raw) {
			t.Fatalf("policy=%q err=%v", policy, err)
		}
	}
	if policy, err := ParseDeliveryPolicy("legacy"); err != nil || policy != DeliveryPolicyLegacy || ControlledDelivery(policy) {
		t.Fatalf("legacy=%q err=%v", policy, err)
	}
	if _, err := ParseDeliveryPolicy("unknown"); err == nil || !ControlledDelivery("unknown") {
		t.Fatal("invalid policy granted legacy authority")
	}
}

func TestRestrictRunnerDeliveryCopiesAndStripsAuthority(t *testing.T) {
	original := RunnerTask{Invocation: &RunnerInvocation{
		CodexAPIKey:    "model-key",
		Repository:     &RunnerRepository{GitToken: "legacy-token", CloneURL: "https://user:password@example.com/repo.git"},
		Repositories:   []RunnerRepository{{GitToken: "array-token"}},
		Assets:         AgentAssetBundle{Skills: []SkillDefinition{{ID: "skill"}}, MCPServers: []MCPServerDefinition{{ID: "privileged"}}, MCPFiles: map[string]map[string][]byte{"mcp": {"server.js": []byte("code")}}},
		MCPCallServers: []MCPServerDefinition{{ID: "call"}},
	}, Sandbox: &RunnerSandboxSpec{Profile: SandboxProfile{Environment: map[string]string{"GH_TOKEN": "secret", "MODEL_URL": "model"}}, Environment: &EnvironmentDefinition{Variables: map[string]string{"GIT_TOKEN": "token"}}}}
	if err := ValidateRunnerDelivery(original); err == nil {
		t.Fatal("credentialed controlled task accepted")
	}
	clean := original
	RestrictRunnerDelivery(&clean)
	if err := ValidateRunnerDelivery(clean); err != nil {
		t.Fatal(err)
	}
	if clean.Invocation.CodexAPIKey != "model-key" || len(clean.Invocation.Assets.Skills) != 1 || clean.Sandbox.Profile.Environment["MODEL_URL"] != "model" || clean.Invocation.Repository.CloneURL != "https://example.com/repo.git" {
		t.Fatalf("execution capability lost or URL credential retained: %+v", clean)
	}
	if original.Invocation.Repository.GitToken != "legacy-token" || original.Invocation.Repositories[0].GitToken != "array-token" || len(original.Invocation.Assets.MCPFiles) != 1 || original.Sandbox.Profile.Environment["GH_TOKEN"] != "secret" || original.Sandbox.Environment.Variables["GIT_TOKEN"] != "token" {
		t.Fatal("sanitizer mutated source task")
	}
	original.DeliveryPolicy = DeliveryPolicyLegacy
	RestrictRunnerDelivery(&original)
	if err := ValidateRunnerDelivery(original); err != nil || original.Invocation.Repository.GitToken != "legacy-token" {
		t.Fatal("explicit legacy compatibility changed")
	}
}

func TestValidateRunnerDeliveryRejectsEveryPublicationSurface(t *testing.T) {
	tasks := []RunnerTask{
		{Action: RunnerTaskActionPushWorkBranch}, {Action: RunnerTaskActionIntegrate}, {Action: RunnerTaskActionAlignBaseline},
		{Invocation: &RunnerInvocation{Repository: &RunnerRepository{PushIntegrate: true}}},
		{Invocation: &RunnerInvocation{Repositories: []RunnerRepository{{SharedPushTarget: "main"}}}},
		{Invocation: &RunnerInvocation{Repositories: []RunnerRepository{{CloneURL: "https://token@example.com/repo.git"}}}},
		{Sandbox: &RunnerSandboxSpec{Profile: SandboxProfile{Environment: map[string]string{"GIT_CONFIG_VALUE_0": "credential"}}}},
		{DeliveryPolicy: "unknown"},
	}
	for i, task := range tasks {
		if err := ValidateRunnerDelivery(task); err == nil {
			t.Fatalf("case %d accepted", i)
		}
		if task.DeliveryPolicy == "" {
			task.DeliveryPolicy = DeliveryPolicyLegacy
			if err := ValidateRunnerDelivery(task); err != nil {
				t.Fatalf("case %d explicit legacy: %v", i, err)
			}
		}
	}
	for _, action := range []string{"", RunnerTaskActionPullWorkBranch, RunnerTaskActionCleanupRun, RunnerTaskActionArchivePreview} {
		if err := ValidateRunnerDelivery(RunnerTask{Action: action}); err != nil {
			t.Fatalf("read/local action %q: %v", action, err)
		}
	}
}
