package engine

import (
	"context"
	"errors"
	"fmt"

	"agentflow/internal/domain"
)

var ErrControlledDelivery = errors.New("controlled_pr delivery requires controller publication and human merge; runner push and automatic integration are disabled")

func (e *Engine) ConfigureDeliveryPolicy(raw string) error {
	policy, err := domain.ParseDeliveryPolicy(raw)
	if err != nil {
		return err
	}
	if e.repo != nil {
		if err := e.repo.DeliveryRepo.ConfigureDeliveryPolicy(policy); err != nil {
			return err
		}
	}
	e.mu.Lock()
	e.deliveryPolicy = policy
	e.mu.Unlock()
	return nil
}

func (e *Engine) DeliveryPolicy() string {
	e.mu.Lock()
	policy := e.deliveryPolicy
	e.mu.Unlock()
	if domain.ControlledDelivery(policy) {
		return domain.DeliveryPolicyControlledPR
	}
	return domain.DeliveryPolicyLegacy
}

func (e *Engine) controlledRun(run domain.Run) bool {
	return domain.ControlledDelivery(e.DeliveryPolicy()) || domain.ControlledDelivery(run.ExecutionContext.DeliveryPolicy)
}

func (e *Engine) applyRunnerTaskPolicy(run domain.Run, task *domain.RunnerTask) error {
	if node := run.Nodes[task.NodeID]; node != nil && task.Action == "" {
		task.ExecutionDeadline = node.ExecutionDeadline
	}
	task.DeliveryPolicy = domain.DeliveryPolicyLegacy
	if e.controlledRun(run) {
		task.DeliveryPolicy = domain.DeliveryPolicyControlledPR
	}
	domain.RestrictRunnerDelivery(task)
	if err := domain.ValidateRunnerDelivery(*task); err != nil {
		return err
	}
	// Sandbox and gate payload preparation also use this policy sanitizer
	// before an assignment exists. Only a named durable task can freeze its
	// execution context and require a destination Runner's capability.
	if task.ID == "" {
		return nil
	}
	if task.Action == "" && (run.Intelligence != nil || run.ExecutionContext.TeamContext != nil) {
		if err := e.ValidateRunIntelligence(run); err != nil {
			return err
		}
		registered, ok := e.repo.Runner(task.RunnerID)
		if !ok || !domain.RunnerVersionSupportsGovernedContext(registered.RunnerVersion) {
			return fmt.Errorf("governed execution requires Runner %s or newer", domain.RunnerVersionGovernedContext)
		}
		if run.Intelligence.MemoryRecallReceipt != nil && !domain.RunnerVersionSupportsMemoryRecallReceipt(registered.RunnerVersion) {
			return fmt.Errorf("memory recall receipt requires Runner %s or newer", domain.RunnerVersionMemoryRecallReceipt)
		}
		if task.Invocation == nil {
			return fmt.Errorf("governed execution requires an invocation")
		}
		binding, err := domain.GovernedContextForRun(run)
		if err != nil {
			return err
		}
		task.Invocation.GovernedContext = binding
	}
	if err := e.bindWorkspaceIsolation(run, task); err != nil {
		return err
	}
	return domain.ValidateRunnerGovernedContext(*task)
}

func (e *Engine) deliveryAssets(run domain.Run, assets domain.AgentAssetBundle) domain.AgentAssetBundle {
	if e.controlledRun(run) {
		return domain.RestrictDeliveryAssets(assets)
	}
	return assets
}

// deliveryContext covers legacy checkout helpers that may initialize missing
// remote branches. Controlled execution may fetch/clone, but never seed them.
type controlledDeliveryContextKey struct{}

func (e *Engine) deliveryContext(ctx context.Context, run domain.Run) context.Context {
	if e.controlledRun(run) {
		return context.WithValue(ctx, controlledDeliveryContextKey{}, true)
	}
	return ctx
}

func controlledDeliveryContext(ctx context.Context) bool {
	controlled, _ := ctx.Value(controlledDeliveryContextKey{}).(bool)
	return controlled
}

// Reject embedded URL credentials in controller-side checkout metadata too.
// Runner payloads are separately copied and sanitized before dispatch.
func (e *Engine) validateDeliveryRepositories(run domain.Run) error {
	if !e.controlledRun(run) {
		return nil
	}
	task := domain.RunnerTask{Invocation: &domain.RunnerInvocation{}}
	for _, repository := range run.RepositoryContexts() {
		task.Invocation.Repositories = append(task.Invocation.Repositories, domain.RunnerRepository{CloneURL: repository.CloneURL})
	}
	return domain.ValidateRunnerDelivery(task)
}
