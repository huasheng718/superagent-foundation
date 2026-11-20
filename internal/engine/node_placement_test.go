package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

func placementEngineFixture(t *testing.T) (*Engine, *repo.Store, domain.Run, domain.Runner) {
	t.Helper()
	e, store := controlledTestEngine(t)
	expires := time.Now().UTC().Add(time.Hour)
	runner := domain.Runner{
		ID: "placed-mac", OwnerUserID: 71, OS: "darwin", Arch: "arm64",
		RunnerVersion: domain.RunnerVersionNodePlacement, Status: domain.RunnerStatusOnline,
		SessionTokenHash: strings.Repeat("a", 64), SessionExpiresAt: &expires,
		Runtimes: []domain.RunnerRuntime{{Provider: "terminal", BinaryPath: "(builtin)", Status: domain.RuntimeStatusOnline, Capabilities: []string{"terminal"}}},
	}
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: "placed-run", WorkflowSnapshot: domain.WorkflowDefinition{Nodes: []domain.WorkflowNode{{ID: "writer", Type: "script"}, {ID: "approve", Type: "approval"}}},
		ExecutionContext: domain.TelemetryContext{
			ProjectID: "placement-project", ResponsibleUserID: 71,
			DeliveryPolicy: domain.DeliveryPolicyControlledPR, RunnerWorkspaceLayout: domain.RunnerWorkspaceLayoutAttempt,
			ExecutionMode: domain.ExecutionModeRunner, ExecutionRunnerID: "original-default",
			NodePlacements: map[string]domain.NodePlacementRequest{"writer": {RunnerID: runner.ID, OS: "darwin", Arch: "arm64", RequiredCapabilities: []string{"terminal"}}},
		},
		Nodes: map[string]*domain.NodeRun{"writer": {NodeID: "writer", Attempt: 1}},
	}
	return e, store, run, runner
}

func TestNodePlacementRequestPreflightFreezesAnIndependentCompleteMap(t *testing.T) {
	e, _, run, _ := placementEngineFixture(t)
	original := run.ExecutionContext.NodePlacements
	if err := e.freezeNodePlacementRequests(&run); err != nil {
		t.Fatal(err)
	}
	request := original["writer"]
	request.RequiredCapabilities[0] = "changed-after-freeze"
	original["writer"] = domain.NodePlacementRequest{RunnerID: "another-runner"}
	if run.ExecutionContext.NodePlacements["writer"].RunnerID != "placed-mac" || run.ExecutionContext.NodePlacements["writer"].RequiredCapabilities[0] != "terminal" {
		t.Fatal("caller mutation changed frozen requests")
	}
	if run.ExecutionContext.ExecutionRunnerID != "original-default" || run.Nodes["writer"].ExecutionPlacement != nil {
		t.Fatal("preflight changed Run default or pretended to freeze an effective attempt")
	}
}

func TestNodePlacementEffectiveAttemptFreezePersistsOnceAndKeepsRunDefault(t *testing.T) {
	e, store, run, runner := placementEngineFixture(t)
	if err := e.freezeNodePlacementRequests(&run); err != nil {
		t.Fatal(err)
	}
	node := run.Nodes["writer"]
	if err := e.freezeNodeExecutionPlacement(&run, run.WorkflowSnapshot.Nodes[0], node); err != nil {
		t.Fatal(err)
	}
	first := *node.ExecutionPlacement
	if run.ExecutionContext.ExecutionRunnerID != "original-default" || first.RunnerID != runner.ID {
		t.Fatalf("effective freeze rebound Run default: context=%+v placement=%+v", run.ExecutionContext, first)
	}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	stored, ok := store.RunState(run.ID)
	if !ok || stored.Nodes["writer"].ExecutionPlacement == nil {
		t.Fatal("effective placement was not persisted")
	}
	if err := e.freezeNodeExecutionPlacement(&stored, run.WorkflowSnapshot.Nodes[0], stored.Nodes["writer"]); err != nil {
		t.Fatalf("same attempt was not idempotent: %v", err)
	}
	if !reflect.DeepEqual(&first, stored.Nodes["writer"].ExecutionPlacement) {
		t.Fatal("same attempt changed its frozen placement")
	}
}

func TestNodePlacementEffectiveAttemptRejectsStaleFrozenSnapshotAndSiblingRebind(t *testing.T) {
	e, _, run, _ := placementEngineFixture(t)
	if err := e.freezeNodePlacementRequests(&run); err != nil {
		t.Fatal(err)
	}
	node := run.WorkflowSnapshot.Nodes[0]
	placement, err := e.resolveNodeExecutionPlacement(run, node, 1, "terminal")
	if err != nil {
		t.Fatal(err)
	}
	run.Nodes["writer"].ExecutionPlacement = &placement
	run.Nodes["writer"].Attempt = 2
	if err := e.freezeNodeExecutionPlacement(&run, node, run.Nodes["writer"]); !errors.Is(err, domain.ErrRunnerPlacementChanged) {
		t.Fatalf("stale frozen attempt accepted: %v", err)
	}
	if run.ExecutionContext.ExecutionRunnerID != "original-default" {
		t.Fatal("stale placement changed global Run target")
	}
}

func TestNodePlacementRequestPreflightRejectsIncompleteOrUnsupportedWorkflows(t *testing.T) {
	cases := []struct {
		name string
		edit func(*domain.Run)
	}{
		{"missing executable", func(r *domain.Run) {
			r.WorkflowSnapshot.Nodes = append(r.WorkflowSnapshot.Nodes, domain.WorkflowNode{ID: "unplaced", Type: "script"})
		}},
		{"unknown key", func(r *domain.Run) {
			r.ExecutionContext.NodePlacements["outside"] = r.ExecutionContext.NodePlacements["writer"]
		}},
		{"approval target", func(r *domain.Run) {
			r.ExecutionContext.NodePlacements["approve"] = r.ExecutionContext.NodePlacements["writer"]
		}},
		{"duplicate node", func(r *domain.Run) {
			r.WorkflowSnapshot.Nodes = append(r.WorkflowSnapshot.Nodes, r.WorkflowSnapshot.Nodes[0])
		}},
		{"child SOP", func(r *domain.Run) {
			r.WorkflowSnapshot.Nodes = append(r.WorkflowSnapshot.Nodes, domain.WorkflowNode{ID: "child", Type: "sop"})
		}},
		{"interactive workspace", func(r *domain.Run) { r.Kind = domain.RunKindWorkspace }},
		{"parent run", func(r *domain.Run) { r.ExecutionContext.ParentRunID = "parent" }},
		{"shared checkout", func(r *domain.Run) { r.ExecutionContext.RunnerWorkspaceLayout = domain.RunnerWorkspaceLayoutShared }},
		{"legacy delivery", func(r *domain.Run) { r.ExecutionContext.DeliveryPolicy = domain.DeliveryPolicyLegacy }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, run, _ := placementEngineFixture(t)
			tc.edit(&run)
			if err := e.freezeNodePlacementRequests(&run); !errors.Is(err, domain.ErrRunnerPlacementInvalid) {
				t.Fatalf("expected invalid placement, got %v", err)
			}
		})
	}
}

func TestNodePlacementResolveRejectsForeignOfflineAndMismatchedTargets(t *testing.T) {
	cases := []struct {
		name string
		edit func(*domain.Runner, *domain.Run)
		want error
	}{
		{"foreign personal runner", func(r *domain.Runner, _ *domain.Run) { r.OwnerUserID = 72 }, domain.ErrRunnerPlacementDenied},
		{"personal label cannot join pool", func(r *domain.Runner, _ *domain.Run) { r.OwnerUserID = 72; r.Labels = []string{"shared"} }, domain.ErrRunnerPlacementDenied},
		{"runner offline", func(r *domain.Runner, _ *domain.Run) { r.Status = domain.RunnerStatusOffline }, domain.ErrRunnerPlacementUnavailable},
		{"runtime offline", func(r *domain.Runner, _ *domain.Run) { r.Runtimes[0].Status = domain.RuntimeStatusOffline }, domain.ErrRunnerPlacementUnavailable},
		{"runtime disabled", func(r *domain.Runner, _ *domain.Run) { r.Runtimes[0].Status = domain.RuntimeStatusDisabled }, domain.ErrRunnerPlacementUnavailable},
		{"old daemon", func(r *domain.Runner, _ *domain.Run) { r.RunnerVersion = "0.10.0" }, domain.ErrRunnerPlacementUnavailable},
		{"missing capability", func(r *domain.Runner, _ *domain.Run) { r.Runtimes[0].Capabilities = nil }, domain.ErrRunnerPlacementUnavailable},
		{"wrong platform", func(r *domain.Runner, _ *domain.Run) { r.OS = "linux" }, domain.ErrRunnerPlacementUnavailable},
		{"ambiguous provider", func(r *domain.Runner, _ *domain.Run) { r.Runtimes = append(r.Runtimes, r.Runtimes[0]) }, domain.ErrRunnerPlacementUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, store, run, runner := placementEngineFixture(t)
			e.ConfigureCloudRunnerPool(CloudRunnerPool{Label: "shared", Projects: []string{run.ExecutionContext.ProjectID}})
			tc.edit(&runner, &run)
			if err := store.PutRunner(runner); err != nil {
				t.Fatal(err)
			}
			if _, err := e.resolveNodeExecutionPlacement(run, run.WorkflowSnapshot.Nodes[0], 1, "terminal"); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestNodePlacementSharedTargetUsesExistingProjectPoolPolicy(t *testing.T) {
	e, store, run, runner := placementEngineFixture(t)
	runner.OwnerUserID, runner.Labels = 0, []string{"shared"}
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	e.ConfigureCloudRunnerPool(CloudRunnerPool{Label: "shared", Projects: []string{"different-project"}})
	if _, err := e.resolveNodeExecutionPlacement(run, run.WorkflowSnapshot.Nodes[0], 1, "terminal"); !errors.Is(err, domain.ErrRunnerPlacementDenied) {
		t.Fatalf("unserved project acquired shared target: %v", err)
	}
	e.ConfigureCloudRunnerPool(CloudRunnerPool{Label: "shared", Projects: []string{run.ExecutionContext.ProjectID}})
	placement, err := e.resolveNodeExecutionPlacement(run, run.WorkflowSnapshot.Nodes[0], 1, "terminal")
	if err != nil {
		t.Fatal(err)
	}
	if placement.Access != domain.NodePlacementAccessCloudPool || placement.PoolLabel != "shared" {
		t.Fatal("shared placement lost frozen pool identity")
	}
}

func TestNodePlacementReportedCapabilitiesDedupeButRejectMalformedInventory(t *testing.T) {
	e, store, run, runner := placementEngineFixture(t)
	runner.Runtimes[0].Capabilities = []string{"terminal", "terminal"}
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	placement, err := e.resolveNodeExecutionPlacement(run, run.WorkflowSnapshot.Nodes[0], 1, "terminal")
	if err != nil || len(placement.Runtimes) != 1 || !reflect.DeepEqual(placement.Runtimes[0].Capabilities, []string{"terminal"}) {
		t.Fatalf("reported duplicates were not normalized: %+v, %v", placement, err)
	}
	runner.Runtimes[0].Capabilities = []string{"terminal", ""}
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.resolveNodeExecutionPlacement(run, run.WorkflowSnapshot.Nodes[0], 1, "terminal"); !errors.Is(err, domain.ErrRunnerPlacementUnavailable) {
		t.Fatalf("malformed capability inventory accepted: %v", err)
	}
}

func TestNodePlacementExpiredRunnerSessionIsUnavailableAtPreflight(t *testing.T) {
	e, store, run, runner := placementEngineFixture(t)
	expired := time.Now().UTC().Add(-time.Second)
	runner.SessionExpiresAt = &expired
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.resolveNodeExecutionPlacement(run, run.WorkflowSnapshot.Nodes[0], 1, "terminal"); !errors.Is(err, domain.ErrRunnerPlacementUnavailable) || errors.Is(err, repo.ErrRunnerPlacementSessionInvalid) {
		t.Fatalf("Runner expiry should not become the user's authentication error: %v", err)
	}
}

func TestNodePlacementProjectionRefusesChangedAgentExecutorAndCapabilities(t *testing.T) {
	e, store, run, runner := placementEngineFixture(t)
	agent := domain.AgentDefinition{ID: "placement-agent", Name: "Placement Agent", ExecutorID: "codex"}
	if err := store.PutAgent(agent); err != nil {
		t.Fatal(err)
	}
	run.WorkflowSnapshot.Nodes[0].Type, run.WorkflowSnapshot.Nodes[0].AgentID = "agent", agent.ID
	run.ExecutionContext.NodePlacements["writer"] = domain.NodePlacementRequest{RunnerID: runner.ID, RequiredCapabilities: []string{"non_interactive"}}
	runner.Runtimes = append(runner.Runtimes, domain.RunnerRuntime{Provider: "codex", BinaryPath: "/test/codex", Version: "test-version", Status: domain.RuntimeStatusOnline, Capabilities: []string{"non_interactive"}, AuthMode: domain.RuntimeAuthLocal})
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	placement, err := e.resolveNodeExecutionPlacement(run, run.WorkflowSnapshot.Nodes[0], 1, "codex")
	if err != nil {
		t.Fatal(err)
	}
	run.Nodes["writer"].ExecutionPlacement = &placement
	if _, err := e.nodeExecutionRun(run, "writer"); err != nil {
		t.Fatal(err)
	}
	agent, _ = store.Agent(agent.ID)
	agent.ExecutorID = "pi"
	agent, err = store.SaveAgent(agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.nodeExecutionRun(run, "writer"); !errors.Is(err, domain.ErrRunnerPlacementChanged) {
		t.Fatalf("changed Agent executor reused old runtime placement: %v", err)
	}
	agent.ExecutorID = "codex"
	if err := store.PutAgent(agent); err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext.NodePlacements["writer"] = domain.NodePlacementRequest{RunnerID: runner.ID, RequiredCapabilities: []string{"new_missing_capability"}}
	if _, err := e.nodeExecutionRun(run, "writer"); !errors.Is(err, domain.ErrRunnerPlacementChanged) {
		t.Fatalf("new request capabilities were ignored: %v", err)
	}
}

func TestNodePlacementSiblingProjectionsKeepSeparateTargets(t *testing.T) {
	e, store, run, mac := placementEngineFixture(t)
	linux := mac
	linux.ID, linux.OS, linux.SessionTokenHash = "placed-linux", "linux", strings.Repeat("b", 64)
	if err := store.PutRunner(linux); err != nil {
		t.Fatal(err)
	}
	run.WorkflowSnapshot.Nodes = append(run.WorkflowSnapshot.Nodes, domain.WorkflowNode{ID: "linux-writer", Type: "script"})
	run.ExecutionContext.NodePlacements["linux-writer"] = domain.NodePlacementRequest{RunnerID: linux.ID, OS: "linux"}
	run.Nodes["linux-writer"] = &domain.NodeRun{NodeID: "linux-writer", Attempt: 1}
	for _, node := range run.WorkflowSnapshot.Nodes {
		if node.Type == "approval" {
			continue
		}
		placement, err := e.resolveNodeExecutionPlacement(run, node, 1, "terminal")
		if err != nil {
			t.Fatal(err)
		}
		run.Nodes[node.ID].ExecutionPlacement = &placement
	}
	macRun, macErr := e.nodeExecutionRun(run, "writer")
	linuxRun, linuxErr := e.nodeExecutionRun(run, "linux-writer")
	if macErr != nil || linuxErr != nil || macRun.ExecutionContext.ExecutionRunnerID != mac.ID || linuxRun.ExecutionContext.ExecutionRunnerID != linux.ID || run.ExecutionContext.ExecutionRunnerID != "original-default" {
		t.Fatalf("sibling targets leaked through Run default: %v, %v", macErr, linuxErr)
	}
}

func TestNodeExecutionProjectionPreservesDefaultAndRejectsStaleAttempts(t *testing.T) {
	e, _, run, _ := placementEngineFixture(t)
	placement, err := e.resolveNodeExecutionPlacement(run, run.WorkflowSnapshot.Nodes[0], 1, "terminal")
	if err != nil {
		t.Fatal(err)
	}
	run.Nodes["writer"].ExecutionPlacement = &placement
	before := run.ExecutionContext
	projected, err := e.nodeExecutionRun(run, "writer")
	if err != nil {
		t.Fatal(err)
	}
	if projected.ExecutionContext.ExecutionRunnerID != placement.RunnerID || !reflect.DeepEqual(run.ExecutionContext, before) {
		t.Fatal("node projection did not preserve original Run target")
	}
	placement.CapabilitySHA256 = ""
	if _, err := e.nodeExecutionRun(run, "writer"); !errors.Is(err, domain.ErrRunnerPlacementInvalid) {
		t.Fatalf("missing persisted digest was silently regenerated: %v", err)
	}
	placement, err = domain.CanonicalNodeExecutionPlacement(placement)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext.NodePlacements["writer"] = domain.NodePlacementRequest{RunnerID: placement.RunnerID, RequiredCapabilities: []string{"terminal", "terminal"}}
	if _, err := e.nodeExecutionRun(run, "writer"); !errors.Is(err, domain.ErrRunnerPlacementInvalid) {
		t.Fatalf("malformed request reused a valid snapshot: %v", err)
	}
	run.ExecutionContext.NodePlacements["writer"] = domain.NodePlacementRequest{RunnerID: placement.RunnerID}
	run.Nodes["writer"].Attempt++
	if _, err := e.nodeExecutionRun(run, "writer"); !errors.Is(err, domain.ErrRunnerPlacementInvalid) {
		t.Fatalf("stale placement reused across new node attempt: %v", err)
	}
	legacy := domain.Run{ExecutionContext: domain.TelemetryContext{ExecutionRunnerID: "legacy-default"}}
	projected, err = e.nodeExecutionRun(legacy, "old-node")
	if err != nil || !reflect.DeepEqual(projected, legacy) {
		t.Fatalf("legacy projection changed: %v", err)
	}
}

// Human approval stays on the control plane even when every executable node
// uses a frozen remote target. It must never attempt Runner placement.
func TestNodePlacementApprovalWaitsWithoutAnExecutionTarget(t *testing.T) {
	e, store, run, _ := placementEngineFixture(t)
	if err := e.freezeNodePlacementRequests(&run); err != nil {
		t.Fatal(err)
	}
	e.ConfigureExecution("approval-placement-test", "test-host", time.Minute, true)
	run.OwnerInstanceID = "approval-placement-test"
	run.Generation = 1
	run.LeaseUntil = time.Now().UTC().Add(time.Minute)
	run.Status = "running"
	run.CreatedAt = time.Now().UTC()
	run.Nodes["writer"].Status = "succeeded"
	run.Nodes["approve"] = &domain.NodeRun{NodeID: "approve", Status: "pending"}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	e.executeNode(context.Background(), run.ID, run.WorkflowSnapshot, run.WorkflowSnapshot.Nodes[1])
	got, _ := store.RunState(run.ID)
	approval := got.Nodes["approve"]
	if approval.Status != "waiting_approval" || approval.Error != "" || approval.ExecutionPlacement != nil || approval.Attempt != 1 {
		t.Fatalf("placed workflow approval failed or acquired a Runner: %+v", approval)
	}
	if len(store.RunnerTasksForRun(run.ID)) != 0 {
		t.Fatal("approval dispatched execution to a Runner")
	}
}
