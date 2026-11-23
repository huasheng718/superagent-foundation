package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

// freezeNodePlacementRequests validates the complete opt-in request before a
// Run is admitted. Resolving a candidate here is preflight, not authorization
// for a future task: the effective node attempt and transactional claim must
// independently freeze/check that candidate when the dispatch path is wired.
func (e *Engine) freezeNodePlacementRequests(run *domain.Run) error {
	if len(run.ExecutionContext.NodePlacements) == 0 {
		return nil
	}
	if err := validateNodePlacementMode(*run); err != nil {
		return err
	}
	requests := make(map[string]domain.NodePlacementRequest, len(run.ExecutionContext.NodePlacements))
	for id, request := range run.ExecutionContext.NodePlacements {
		canonical, err := domain.CanonicalNodePlacementRequest(request)
		if err != nil {
			return fmt.Errorf("node %q placement: %w", id, err)
		}
		requests[id] = canonical
	}
	seen := make(map[string]bool, len(run.WorkflowSnapshot.Nodes))
	for _, node := range run.WorkflowSnapshot.Nodes {
		if strings.TrimSpace(node.ID) == "" || seen[node.ID] {
			return fmt.Errorf("%w: placement requires unique workflow node IDs", domain.ErrRunnerPlacementInvalid)
		}
		seen[node.ID] = true
		_, configured := requests[node.ID]
		if node.Type == "approval" {
			if configured {
				return fmt.Errorf("%w: approval node %q has no execution target", domain.ErrRunnerPlacementInvalid, node.ID)
			}
			continue
		}
		if node.Type != "script" && node.Type != "command" && node.Type != "agent" {
			return fmt.Errorf("%w: node %q does not support isolated placement", domain.ErrRunnerPlacementInvalid, node.ID)
		}
		if !configured {
			return fmt.Errorf("%w: missing executable node %q", domain.ErrRunnerPlacementInvalid, node.ID)
		}
		provider, err := e.nodePlacementProvider(*run, node)
		if err != nil {
			return err
		}
		if _, err := e.resolveNodeExecutionPlacement(*run, node, 1, provider); err != nil {
			return err
		}
	}
	for id := range requests {
		if !seen[id] {
			return fmt.Errorf("%w: unknown workflow node %q", domain.ErrRunnerPlacementInvalid, id)
		}
	}
	// Only publish the independent canonical map after every check succeeds.
	run.ExecutionContext.NodePlacements = requests
	return nil
}

func validateNodePlacementMode(run domain.Run) error {
	context := run.ExecutionContext
	if !domain.ControlledDelivery(context.DeliveryPolicy) || context.RunnerWorkspaceLayout != domain.RunnerWorkspaceLayoutAttempt || run.IsWorkspace() || context.ParentRunID != "" {
		return fmt.Errorf("%w: node placement requires a top-level controlled isolated Run", domain.ErrRunnerPlacementInvalid)
	}
	if context.ResponsibleUserID <= 0 || strings.TrimSpace(context.ProjectID) == "" {
		return fmt.Errorf("%w: node placement requires a project and responsible user", domain.ErrRunnerPlacementInvalid)
	}
	return nil
}

func (e *Engine) nodePlacementProvider(run domain.Run, node domain.WorkflowNode) (string, error) {
	if node.Type == "script" || node.Type == "command" {
		return "terminal", nil
	}
	if node.Type != "agent" {
		return "", fmt.Errorf("%w: node %q is not executable", domain.ErrRunnerPlacementInvalid, node.ID)
	}
	agent, ok := workflowAgent(e.repo, run.WorkflowSnapshot, node.AgentID)
	if !ok {
		return "", fmt.Errorf("%w: node %q Agent is unavailable", domain.ErrRunnerPlacementInvalid, node.ID)
	}
	executor, err := resolveNodeExecutor(e.repo, node, agent)
	if err != nil {
		return "", fmt.Errorf("%w: node %q executor is unavailable", domain.ErrRunnerPlacementInvalid, node.ID)
	}
	return executor.Kind, nil
}

func (e *Engine) resolveNodeExecutionPlacement(run domain.Run, node domain.WorkflowNode, nodeAttempt int, provider string) (domain.NodeExecutionPlacement, error) {
	var result domain.NodeExecutionPlacement
	if err := validateNodePlacementMode(run); err != nil {
		return result, err
	}
	request, ok := run.ExecutionContext.NodePlacements[node.ID]
	if !ok {
		return result, fmt.Errorf("%w: node %q has no placement request", domain.ErrRunnerPlacementInvalid, node.ID)
	}
	request, err := domain.CanonicalNodePlacementRequest(request)
	if err != nil {
		return result, err
	}
	resolvedProvider, err := e.nodePlacementProvider(run, node)
	if err != nil || resolvedProvider != provider || nodeAttempt <= 0 {
		return result, fmt.Errorf("%w: node provider or attempt does not match", domain.ErrRunnerPlacementInvalid)
	}
	runner, ok := e.repo.Runner(request.RunnerID)
	if !ok || runner.Status != domain.RunnerStatusOnline || !domain.RunnerVersionSupportsNodePlacement(runner.RunnerVersion) {
		return result, fmt.Errorf("%w: selected Runner is missing, offline or lacks node placement", domain.ErrRunnerPlacementUnavailable)
	}
	if (request.OS != "" && request.OS != runner.OS) || (request.Arch != "" && request.Arch != runner.Arch) {
		return result, fmt.Errorf("%w: selected Runner platform differs from request", domain.ErrRunnerPlacementUnavailable)
	}
	result = domain.NodeExecutionPlacement{
		Version: domain.NodeExecutionPlacementVersion, NodeAttempt: nodeAttempt,
		RunnerID: runner.ID, ResponsibleUserID: run.ExecutionContext.ResponsibleUserID,
		ProjectID: run.ExecutionContext.ProjectID, OS: runner.OS, Arch: runner.Arch,
		RunnerVersion: runner.RunnerVersion,
	}
	if runner.OwnerUserID > 0 {
		if runner.OwnerUserID != result.ResponsibleUserID {
			return domain.NodeExecutionPlacement{}, fmt.Errorf("%w: selected personal Runner belongs to another user", domain.ErrRunnerPlacementDenied)
		}
		result.Access = domain.NodePlacementAccessPersonal
	} else {
		e.mu.Lock()
		pool := e.cloudPool
		e.mu.Unlock()
		if pool.Label == "" || !cloudPoolServes(pool.Projects, result.ProjectID) || !poolRunnerEligible(runner, pool.Label) {
			return domain.NodeExecutionPlacement{}, fmt.Errorf("%w: selected shared Runner is outside the project pool", domain.ErrRunnerPlacementDenied)
		}
		result.Access, result.PoolLabel = domain.NodePlacementAccessCloudPool, pool.Label
	}
	result.RegistrationSHA256, err = domain.RunnerRegistrationSHA256(runner.SessionTokenHash)
	if err != nil {
		return domain.NodeExecutionPlacement{}, fmt.Errorf("%w: selected Runner has no valid registration identity", domain.ErrRunnerPlacementUnavailable)
	}
	providers := []string{provider}
	if provider != "terminal" {
		providers = append(providers, "terminal")
	}
	for _, needed := range providers {
		matches := 0
		for _, inventory := range runner.Runtimes {
			if inventory.Provider != needed {
				continue
			}
			matches++
			if inventory.Status != domain.RuntimeStatusOnline {
				return domain.NodeExecutionPlacement{}, fmt.Errorf("%w: selected runtime is offline or disabled", domain.ErrRunnerPlacementUnavailable)
			}
			if needed == provider {
				for _, capability := range request.RequiredCapabilities {
					if !containsPlacementCapability(inventory.Capabilities, capability) {
						return domain.NodeExecutionPlacement{}, fmt.Errorf("%w: selected runtime lacks a required capability", domain.ErrRunnerPlacementUnavailable)
					}
				}
			}
			capabilities := make([]string, 0, len(inventory.Capabilities))
			seen := make(map[string]bool, len(inventory.Capabilities))
			for _, capability := range inventory.Capabilities {
				if !seen[capability] {
					capabilities = append(capabilities, capability)
					seen[capability] = true
				}
			}
			result.Runtimes = append(result.Runtimes, domain.NodeRuntimeSnapshot{
				Provider: inventory.Provider, Version: inventory.Version, BinaryPath: inventory.BinaryPath,
				Capabilities: capabilities, AuthMode: runner.RuntimeAuthMode(needed),
			})
		}
		if matches != 1 {
			return domain.NodeExecutionPlacement{}, fmt.Errorf("%w: selected runtime inventory is missing or ambiguous", domain.ErrRunnerPlacementUnavailable)
		}
	}
	result, err = domain.CanonicalNodeExecutionPlacement(result)
	if err != nil {
		return domain.NodeExecutionPlacement{}, fmt.Errorf("%w: selected Runner inventory is invalid", domain.ErrRunnerPlacementUnavailable)
	}
	if err := e.repo.ValidateRunnerPlacement(result, time.Now().UTC()); err != nil {
		if errors.Is(err, repo.ErrRunnerPlacementSessionInvalid) {
			return domain.NodeExecutionPlacement{}, fmt.Errorf("%w: selected Runner session is not active", domain.ErrRunnerPlacementUnavailable)
		}
		return domain.NodeExecutionPlacement{}, err
	}
	return result, nil
}

func containsPlacementCapability(capabilities []string, expected string) bool {
	for _, capability := range capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}

// nodeExecutionRun projects a target into a local value only. It must never be
// persisted as the Run's default; sibling nodes retain their own snapshots.
func (e *Engine) nodeExecutionRun(run domain.Run, nodeID string) (domain.Run, error) {
	node := run.Nodes[nodeID]
	if len(run.ExecutionContext.NodePlacements) == 0 {
		if node != nil && node.ExecutionPlacement != nil {
			return run, fmt.Errorf("%w: node snapshot lacks a frozen Run request", domain.ErrRunnerPlacementInvalid)
		}
		return run, nil
	}
	if err := validateNodePlacementMode(run); err != nil {
		return run, err
	}
	request, ok := run.ExecutionContext.NodePlacements[nodeID]
	if !ok || node == nil || node.ExecutionPlacement == nil || node.Attempt <= 0 || node.ExecutionPlacement.NodeAttempt != node.Attempt {
		return run, fmt.Errorf("%w: effective node placement is missing or stale", domain.ErrRunnerPlacementInvalid)
	}
	request, err := domain.CanonicalNodePlacementRequest(request)
	if err != nil {
		return run, err
	}
	if !domain.ValidWorkspaceContextSHA256(node.ExecutionPlacement.CapabilitySHA256) {
		return run, fmt.Errorf("%w: effective node placement lacks its frozen digest", domain.ErrRunnerPlacementInvalid)
	}
	placement, err := domain.CanonicalNodeExecutionPlacement(*node.ExecutionPlacement)
	if err != nil {
		return run, err
	}
	if request.RunnerID != placement.RunnerID || placement.ResponsibleUserID != run.ExecutionContext.ResponsibleUserID || placement.ProjectID != run.ExecutionContext.ProjectID || (request.OS != "" && request.OS != placement.OS) || (request.Arch != "" && request.Arch != placement.Arch) {
		return run, fmt.Errorf("%w: node placement differs from its frozen Run request", domain.ErrRunnerPlacementChanged)
	}
	var definition *domain.WorkflowNode
	for i := range run.WorkflowSnapshot.Nodes {
		if run.WorkflowSnapshot.Nodes[i].ID == nodeID {
			definition = &run.WorkflowSnapshot.Nodes[i]
			break
		}
	}
	if definition == nil {
		return run, fmt.Errorf("%w: placed node is outside the workflow snapshot", domain.ErrRunnerPlacementInvalid)
	}
	provider, err := e.nodePlacementProvider(run, *definition)
	if err != nil {
		return run, err
	}
	primaryFound, terminalFound := false, false
	for _, runtime := range placement.Runtimes {
		terminalFound = terminalFound || runtime.Provider == "terminal"
		if runtime.Provider != provider {
			continue
		}
		primaryFound = true
		for _, capability := range request.RequiredCapabilities {
			if !containsPlacementCapability(runtime.Capabilities, capability) {
				return run, fmt.Errorf("%w: node snapshot lacks a requested capability", domain.ErrRunnerPlacementChanged)
			}
		}
	}
	if !primaryFound || !terminalFound {
		return run, fmt.Errorf("%w: node snapshot lacks its executor or terminal runtime", domain.ErrRunnerPlacementChanged)
	}
	run.ExecutionContext.ExecutionMode = domain.ExecutionModeRunner
	run.ExecutionContext.ExecutionRunnerID = placement.RunnerID
	run.ExecutionContext.ExecutionPool = ""
	if placement.Access == domain.NodePlacementAccessCloudPool {
		run.ExecutionContext.ExecutionPool = domain.ExecutionModeCloud
	}
	return run, nil
}
