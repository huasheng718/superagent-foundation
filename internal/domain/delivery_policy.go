package domain

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	DeliveryPolicyControlledPR = "controlled_pr"
	DeliveryPolicyLegacy       = "legacy"
)

func ParseDeliveryPolicy(raw string) (string, error) {
	switch policy := strings.TrimSpace(raw); policy {
	case "", DeliveryPolicyControlledPR:
		return DeliveryPolicyControlledPR, nil
	case DeliveryPolicyLegacy:
		return policy, nil
	default:
		return "", fmt.Errorf("unknown delivery policy %q", policy)
	}
}

// Missing or invalid persisted policies never grant legacy publication rights.
func ControlledDelivery(policy string) bool {
	return strings.TrimSpace(policy) != DeliveryPolicyLegacy
}

// RestrictDeliveryAssets preserves model/skill capabilities but removes all
// MCP capabilities until a separately verified read-only policy is available.
func RestrictDeliveryAssets(assets AgentAssetBundle) AgentAssetBundle {
	assets.MCPServers = nil
	assets.MCPFiles = nil
	return assets
}

func hostingCredential(key string) bool {
	switch strings.ToUpper(strings.TrimSpace(key)) {
	case "AGENTFLOW_GIT_TOKEN", "GIT_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "GONGFENG_TOKEN", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT":
		return true
	}
	return strings.HasPrefix(strings.ToUpper(key), "GIT_CONFIG_KEY_") || strings.HasPrefix(strings.ToUpper(key), "GIT_CONFIG_VALUE_")
}

// RestrictDeliveryEnvironment only classifies known hosting credential keys;
// arbitrary secrets in scripts or ambient host state are a separate boundary.
func RestrictDeliveryEnvironment(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		if !hostingCredential(key) {
			out[key] = value
		}
	}
	return out
}

func restrictRunnerRepository(repository RunnerRepository) RunnerRepository {
	repository.GitToken = ""
	if parsed, err := url.Parse(repository.CloneURL); err == nil && parsed.User != nil {
		parsed.User = nil
		repository.CloneURL = parsed.String()
	}
	return repository
}

// RestrictRunnerDelivery copies nested mutable values before sanitizing. It
// deliberately leaves prohibited actions/targets intact so validation rejects
// them rather than silently changing their meaning.
func RestrictRunnerDelivery(task *RunnerTask) {
	if task == nil || !ControlledDelivery(task.DeliveryPolicy) {
		return
	}
	if task.Invocation != nil {
		invocation := *task.Invocation
		invocation.Assets = RestrictDeliveryAssets(invocation.Assets)
		invocation.MCPCallServers = nil
		if invocation.Repository != nil {
			repository := restrictRunnerRepository(*invocation.Repository)
			invocation.Repository = &repository
		}
		invocation.Repositories = append([]RunnerRepository(nil), invocation.Repositories...)
		for i := range invocation.Repositories {
			invocation.Repositories[i] = restrictRunnerRepository(invocation.Repositories[i])
		}
		task.Invocation = &invocation
	}
	if task.Sandbox != nil {
		sandbox := *task.Sandbox
		sandbox.Profile.Environment = RestrictDeliveryEnvironment(sandbox.Profile.Environment)
		if sandbox.Environment != nil {
			environment := *sandbox.Environment
			environment.Variables = RestrictDeliveryEnvironment(environment.Variables)
			sandbox.Environment = &environment
		}
		task.Sandbox = &sandbox
	}
}

func ValidateRunnerDelivery(task RunnerTask) error {
	policy, err := ParseDeliveryPolicy(task.DeliveryPolicy)
	if err != nil {
		return err
	}
	if !ControlledDelivery(policy) {
		return nil
	}
	deny := func(reason string) error { return fmt.Errorf("controlled_pr delivery: %s", reason) }
	switch task.Action {
	case RunnerTaskActionPushWorkBranch, RunnerTaskActionIntegrate, RunnerTaskActionAlignBaseline:
		return deny("runner publication action is forbidden")
	}
	if invocation := task.Invocation; invocation != nil {
		if len(invocation.Assets.MCPServers) != 0 || len(invocation.Assets.MCPFiles) != 0 || len(invocation.MCPCallServers) != 0 {
			return deny("runner MCP capabilities are forbidden")
		}
		repositories := append([]RunnerRepository(nil), invocation.Repositories...)
		if invocation.Repository != nil {
			repositories = append(repositories, *invocation.Repository)
		}
		for _, repository := range repositories {
			if repository.GitToken != "" || repository.PushIntegrate || strings.TrimSpace(repository.SharedPushTarget) != "" {
				return deny("runner repository publication authority is forbidden")
			}
			if parsed, err := url.Parse(repository.CloneURL); err == nil && parsed.User != nil {
				return deny("repository URL credentials are forbidden")
			}
		}
	}
	if task.Sandbox != nil {
		values := []map[string]string{task.Sandbox.Profile.Environment}
		if task.Sandbox.Environment != nil {
			values = append(values, task.Sandbox.Environment.Variables)
		}
		for _, environment := range values {
			for key := range environment {
				if hostingCredential(key) {
					return deny("runner hosting credential environment is forbidden")
				}
			}
		}
	}
	return nil
}
