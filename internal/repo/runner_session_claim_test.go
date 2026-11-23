package repo

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestRunnerSessionClaimRechecksIdentity(t *testing.T) {
	for _, mode := range []string{"valid", "wrong-session", "empty-session", "expired", "offline", "missing-runner", "empty-queue-rotated-session"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, _, now := newRegistryPlacementFixture(t, false)
			task := domain.RunnerTask{ID: "legacy-session-task", RunnerID: runner.ID, Provider: "codex", Prompt: "private invocation", Invocation: &domain.RunnerInvocation{CodexAPIKey: "test-only-private-key"}}
			if mode != "empty-queue-rotated-session" {
				if err := s.PutRunnerTask(task); err != nil {
					t.Fatal(err)
				}
			}
			session := runner.SessionTokenHash
			switch mode {
			case "wrong-session":
				session = strings.Repeat("2", 64)
			case "empty-session":
				session = ""
			case "expired":
				now = runner.SessionExpiresAt.Add(time.Second)
			case "offline":
				runner.Status = domain.RunnerStatusOffline
			case "empty-queue-rotated-session":
				runner.SessionTokenHash = strings.Repeat("2", 64)
			}
			if mode == "offline" || mode == "empty-queue-rotated-session" {
				if err := s.PutRunner(runner); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-runner" {
				if err := s.DB.Where("id = ?", runner.ID).Delete(&runnerRow{}).Error; err != nil {
					t.Fatal(err)
				}
			}
			got, claimed, err := s.ClaimRunnerTaskForSession(runner.ID, session, []string{"codex"}, now, time.Minute)
			if mode == "valid" {
				if err != nil || !claimed || got.ID != task.ID || got.Attempt != 1 || got.LeaseOwner != runner.ID || got.Invocation == nil || got.Invocation.CodexAPIKey != "test-only-private-key" {
					t.Fatalf("valid authenticated claim: task=%+v claimed=%v error=%v", got, claimed, err)
				}
				return
			}
			want := ErrRunnerPlacementSessionInvalid
			if mode == "offline" {
				want = domain.ErrRunnerPlacementUnavailable
			}
			if !errors.Is(err, want) || claimed || got.ID != "" || got.Invocation != nil {
				t.Fatalf("invalid principal got task authority: task=%+v claimed=%v error=%v", got, claimed, err)
			}
			if mode != "empty-queue-rotated-session" {
				stored, _ := s.RunnerTask(task.ID)
				if stored.Status != domain.RunnerTaskQueued || stored.Attempt != 0 || stored.LeaseOwner != "" {
					t.Fatal("failed session mutated task lease")
				}
			}
		})
	}
}

func TestRunnerSessionClaimPreflightIsAuthenticatedSignalOnly(t *testing.T) {
	s, runner, _, now := newRegistryPlacementFixture(t, false)
	task := domain.RunnerTask{ID: "preflight-task", RunnerID: runner.ID, Provider: "codex"}
	if err := s.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	available, err := s.RunnerTaskAvailableForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now)
	if err != nil || !available {
		t.Fatalf("valid preflight=%v error=%v", available, err)
	}
	stored, _ := s.RunnerTask(task.ID)
	if stored.Status != domain.RunnerTaskQueued || stored.Attempt != 0 {
		t.Fatal("preflight leased task")
	}
	oldSession := runner.SessionTokenHash
	runner.SessionTokenHash = strings.Repeat("2", 64)
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if available, err := s.RunnerTaskAvailableForSession(runner.ID, oldSession, []string{"codex"}, now); available || !errors.Is(err, ErrRunnerPlacementSessionInvalid) {
		t.Fatalf("rotated preflight principal retained signal: available=%v error=%v", available, err)
	}
	if available, err := s.RunnerTaskAvailableForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, *runner.SessionExpiresAt); available || !errors.Is(err, ErrRunnerPlacementSessionInvalid) {
		t.Fatalf("expired preflight retained signal: available=%v error=%v", available, err)
	}
}

func TestRunnerSessionCallbacksAuthorizePlacedCleanupMaintenance(t *testing.T) {
	s, runner, placement, now := newRegistryPlacementFixture(t, false)
	if err := s.PutRun(domain.Run{ID: "cleanup-run", Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	task, created, err := s.QueueRunnerMaintenanceTask(domain.RunnerTask{
		ID: "cleanup-callbacks", RunID: "cleanup-run", RunnerID: runner.ID,
		Provider: "terminal", Action: domain.RunnerTaskActionCleanupRun,
		Status: domain.RunnerTaskQueued, MaxAttempts: 1,
		MaintenanceKey: &domain.RunnerMaintenanceKey{RunID: "cleanup-run", Action: domain.RunnerTaskActionCleanupRun, RunnerID: runner.ID, RegistrationSHA256: placement.RegistrationSHA256},
	})
	if err != nil || !created {
		t.Fatalf("queue maintenance: task=%+v created=%v err=%v", task, created, err)
	}
	claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"terminal"}, now, time.Minute)
	if err != nil || !found || claimed.ID != task.ID {
		t.Fatalf("claim maintenance: task=%+v found=%v err=%v", claimed, found, err)
	}
	if err := s.StartRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, now); err != nil {
		t.Fatalf("start maintenance: %v", err)
	}
	if _, err := s.RenewRunnerTaskLeaseForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, time.Minute, now); err != nil {
		t.Fatalf("renew maintenance: %v", err)
	}
	if err := s.CompleteRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, domain.RunnerTaskSuccess, nil, "cleaned", "", nil, nil, now); err != nil {
		t.Fatalf("complete maintenance: %v", err)
	}
}

func TestRunnerLegacyClaimCannotDowngradePlacedAuthority(t *testing.T) {
	s, runner, placement, now := newRegistryPlacementFixture(t, false)
	placed := domain.RunnerTask{
		ID: "a-placed-task", RunnerID: runner.ID, Provider: "codex", Status: domain.RunnerTaskQueued,
		ExecutionPlacement: &placement, Execution: &domain.NodeTaskExecution{Role: domain.NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: 1},
		MaintenanceKey: &domain.RunnerMaintenanceKey{RunID: "run", RunnerID: runner.ID, RegistrationSHA256: placement.RegistrationSHA256},
		CreatedAt:      now.Add(-time.Minute),
	}
	// Deliberately seed stored metadata directly: the public legacy Put path
	// must reject both initial bypass and later pointer-stripping downgrades.
	placed, err := prepareRunnerTask(placed)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&runnerTaskRow{ID: placed.ID, RunnerID: placed.RunnerID, Provider: placed.Provider, Status: placed.Status, Payload: runnerTaskPayloadBytes(placed), Created: timePtr(placed.CreatedAt), MaxAttempts: 1}).Error; err != nil {
		t.Fatal(err)
	}
	stored, _ := s.RunnerTask(placed.ID)
	if stored.ExecutionPlacement == nil || stored.Execution == nil || stored.MaintenanceKey == nil {
		t.Fatal("new task authority was lost by payload persistence")
	}
	if err := s.PutRunnerTask(placed); !errors.Is(err, domain.ErrRunnerPlacementInvalid) {
		t.Fatalf("legacy Put accepted placed authority: %v", err)
	}
	downgraded := domain.RunnerTask{ID: placed.ID, Provider: "codex", RunnerID: runner.ID, Prompt: "authority stripped"}
	if err := s.PutRunnerTask(downgraded); !errors.Is(err, domain.ErrRunnerPlacementInvalid) {
		t.Fatalf("legacy Put stripped existing authority: %v", err)
	}
	if _, claimed, err := s.ClaimRunnerTask(runner.ID, []string{"codex"}, now, time.Minute); err != nil || claimed {
		t.Fatalf("legacy claim acquired placement: claimed=%v error=%v", claimed, err)
	}
	legacy := domain.RunnerTask{ID: "z-legacy-task", RunnerID: runner.ID, Provider: "codex"}
	if err := s.PutRunnerTask(legacy); err != nil {
		t.Fatal(err)
	}
	got, claimed, err := s.ClaimRunnerTask(runner.ID, []string{"codex"}, now, time.Minute)
	if err != nil || !claimed || got.ID != legacy.ID {
		t.Fatalf("older placed task starved legacy work: task=%+v claimed=%v error=%v", got, claimed, err)
	}
	stored, _ = s.RunnerTask(placed.ID)
	if stored.Status != domain.RunnerTaskQueued || stored.Attempt != 0 || stored.ExecutionPlacement == nil {
		t.Fatal("legacy path mutated placed identity")
	}
}

func TestRunnerSessionClaimRejectsPartialOrMalformedAuthority(t *testing.T) {
	for _, mode := range []string{"execution-only", "input-only", "malformed-placement"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, _, now := newRegistryPlacementFixture(t, false)
			task := domain.RunnerTask{ID: "partial-task", RunnerID: runner.ID, Provider: "codex", Status: domain.RunnerTaskQueued}
			if mode == "execution-only" {
				task.Execution = &domain.NodeTaskExecution{Role: domain.NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: 1}
			}
			if mode == "input-only" {
				task.Invocation = &domain.RunnerInvocation{ContributionInputs: []domain.RunnerContributionInput{{RepositoryID: "repo"}}}
			}
			raw := runnerTaskPayloadBytes(task)
			if mode == "malformed-placement" {
				var value map[string]any
				_ = json.Unmarshal(raw, &value)
				value["executionPlacement"] = "not-a-snapshot"
				raw, _ = json.Marshal(value)
			}
			if err := s.DB.Create(&runnerTaskRow{ID: task.ID, RunnerID: runner.ID, Provider: "codex", Status: domain.RunnerTaskQueued, Payload: raw, MaxAttempts: 1}).Error; err != nil {
				t.Fatal(err)
			}
			if _, claimed, err := s.ClaimRunnerTask(runner.ID, []string{"codex"}, now, time.Minute); err != nil || claimed {
				t.Fatalf("legacy claim acquired partial authority: claimed=%v error=%v", claimed, err)
			}
			got, claimed, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
			if !errors.Is(err, domain.ErrRunnerPlacementInvalid) || claimed || got.ID != "" {
				t.Fatalf("partial authority claimed: task=%+v claimed=%v error=%v", got, claimed, err)
			}
		})
	}
}

func TestRunnerRegistryWritersMergeConcurrentPolicy(t *testing.T) {
	s, runner, _, now := newRegistryPlacementFixture(t, false)
	for round := 0; round < 8; round++ {
		if err := s.PutRunner(runner); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errorsOut := make(chan error, 3)
		var wg sync.WaitGroup
		for _, operation := range []func() error{
			func() error { return s.SetRunnerRuntimeAuthMode(runner.ID, "codex", domain.RuntimeAuthPersonal, now) },
			func() error { return s.SetRunnerRuntimeStatus(runner.ID, "codex", domain.RuntimeStatusDisabled, now) },
			func() error {
				return s.RunnerHeartbeat(runner.ID, runner.Runtimes, runner.RunnerVersion, now, now.Add(time.Hour))
			},
		} {
			wg.Add(1)
			go func(operation func() error) { defer wg.Done(); <-start; errorsOut <- operation() }(operation)
		}
		close(start)
		wg.Wait()
		close(errorsOut)
		for err := range errorsOut {
			if err != nil {
				t.Fatal(err)
			}
		}
		stored, _ := s.Runner(runner.ID)
		if stored.Runtimes[0].Status != domain.RuntimeStatusDisabled || stored.RuntimeAuthMode("codex") != domain.RuntimeAuthPersonal {
			t.Fatalf("round %d lost concurrent platform policy: %+v", round, stored.Runtimes)
		}
		// Provider spelling in a report must not bypass the existing disabled
		// preference; merging must also leave the caller's report untouched.
		reported := []domain.RunnerRuntime{{Provider: "CODEX", BinaryPath: "/opt/bin/codex", Version: "0.57.0", Capabilities: []string{"sandbox", "non_interactive"}, Status: domain.RuntimeStatusOnline, AuthMode: domain.RuntimeAuthLocal}}
		if err := s.RunnerHeartbeat(runner.ID, reported, runner.RunnerVersion, now, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if reported[0].Status != domain.RuntimeStatusOnline || reported[0].AuthMode != domain.RuntimeAuthLocal {
			t.Fatal("heartbeat mutated caller inventory")
		}
		stored, _ = s.Runner(runner.ID)
		if stored.Runtimes[0].Status != domain.RuntimeStatusDisabled || stored.RuntimeAuthMode("codex") != domain.RuntimeAuthPersonal {
			t.Fatalf("case-changed heartbeat reset policy: %+v", stored.Runtimes)
		}
	}
}

func newSessionPlacedClaimFixture(t *testing.T) (*Store, domain.Runner, domain.Run, domain.RunnerTask, time.Time) {
	t.Helper()
	s, runner, placement, now := newRegistryPlacementFixture(t, false)
	run := domain.Run{
		ID: "session-placed-run", WorkflowID: "session-placed-workflow", Status: "running",
		OwnerInstanceID: "session-test-engine", LeaseUntil: now.Add(3 * time.Hour),
		WorkflowSnapshot: domain.WorkflowDefinition{ID: "session-placed-workflow", Nodes: []domain.WorkflowNode{{ID: "author", Type: "agent", AgentID: "developer"}}},
		ExecutionContext: domain.TelemetryContext{
			DeliveryPolicy: domain.DeliveryPolicyControlledPR, ProjectID: "project", ResponsibleUserID: 42,
			ExecutionMode: domain.ExecutionModeRunner, ExecutionRunnerID: runner.ID,
			RunnerWorkspaceLayout: domain.RunnerWorkspaceLayoutAttempt,
			NodePlacements:        map[string]domain.NodePlacementRequest{"author": {RunnerID: runner.ID}},
		},
		Nodes: map[string]*domain.NodeRun{"author": {NodeID: "author", Status: "running", Attempt: 1, ExecutionPlacement: &placement}},
	}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	run, _ = s.RunState(run.ID)
	task := domain.RunnerTask{
		ID: "session-placed-author", RunID: run.ID, NodeID: "author", RunnerID: runner.ID,
		Provider: "codex", Prompt: "private placed invocation", DeliveryPolicy: domain.DeliveryPolicyControlledPR,
		ExecutionPlacement: &placement,
		Execution:          &domain.NodeTaskExecution{Role: domain.NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: 1},
		Invocation: &domain.RunnerInvocation{
			RunID: run.ID, NodeID: "author", CredentialSource: domain.RuntimeAuthLocal,
			Executor:           domain.ExecutorDefinition{Kind: "codex"},
			WorkspaceIsolation: &domain.RunnerWorkspaceIsolation{Version: domain.RunnerWorkspaceIsolationVersion},
		},
	}
	contextSHA, err := domain.RunnerContributionContextSHA(task)
	if err != nil {
		t.Fatal(err)
	}
	task.Invocation.WorkspaceIsolation.ContextSHA256 = contextSHA
	if err := s.QueueNodeExecutionTask(task); err != nil {
		t.Fatal(err)
	}
	run, _ = s.RunState(run.ID)
	task, _ = s.RunnerTask(task.ID)
	return s, runner, run, task, now
}

func TestRunnerSessionClaimPlacedRunAndRegistryAuthority(t *testing.T) {
	for _, mode := range []string{"valid", "run-cancelled", "run-paused", "run-lease-expired", "node-superseded", "registry-replaced", "registry-auth-changed", "runtime-disabled", "session-expired", "task-moved-to-other-run"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, run, task, now := newSessionPlacedClaimFixture(t)
			want := ErrRunnerTaskLeaseLost
			session := runner.SessionTokenHash
			switch mode {
			case "run-cancelled":
				if _, err := s.RequestRunCancel(run.ID); err != nil {
					t.Fatal(err)
				}
			case "run-paused":
				if _, err := s.RequestRunPause(run.ID); err != nil {
					t.Fatal(err)
				}
			case "run-lease-expired":
				now = run.LeaseUntil.Add(time.Second)
			case "node-superseded":
				node := run.Nodes[task.NodeID]
				node.Attempt++
				placement := *node.ExecutionPlacement
				placement.NodeAttempt++
				node.ExecutionPlacement = &placement
				if err := s.PutRun(run); err != nil {
					t.Fatal(err)
				}
			case "registry-replaced":
				runner.SessionTokenHash = strings.Repeat("2", 64)
				if err := s.PutRunner(runner); err != nil {
					t.Fatal(err)
				}
				session = runner.SessionTokenHash
				want = domain.ErrRunnerPlacementChanged
			case "registry-auth-changed":
				if err := s.SetRunnerRuntimeAuthMode(runner.ID, "codex", domain.RuntimeAuthPersonal, now); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrRunnerPlacementChanged
			case "runtime-disabled":
				if err := s.SetRunnerRuntimeStatus(runner.ID, "codex", domain.RuntimeStatusDisabled, now); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrRunnerPlacementUnavailable
			case "session-expired":
				now = runner.SessionExpiresAt.Add(time.Second)
				want = ErrRunnerPlacementSessionInvalid
			case "task-moved-to-other-run":
				var payload map[string]any
				_ = json.Unmarshal(runnerTaskPayloadBytes(task), &payload)
				payload["runId"] = "different-run"
				raw, _ := json.Marshal(payload)
				if err := s.DB.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Update("payload", raw).Error; err != nil {
					t.Fatal(err)
				}
			}
			got, claimed, err := s.ClaimRunnerTaskForSession(runner.ID, session, []string{"codex"}, now, time.Minute)
			if mode == "valid" {
				if err != nil || !claimed || got.ID != task.ID || got.Attempt != 1 || got.ExecutionPlacement == nil || got.Execution == nil || got.Invocation == nil || got.Prompt != task.Prompt {
					t.Fatalf("valid placed claim: task=%+v claimed=%v error=%v", got, claimed, err)
				}
				return
			}
			if !errors.Is(err, want) || claimed || got.ID != "" || got.Invocation != nil {
				t.Fatalf("stale placed claim disclosed invocation: task=%+v claimed=%v error=%v", got, claimed, err)
			}
			stored, _ := s.RunnerTask(task.ID)
			if stored.Status != domain.RunnerTaskQueued || stored.Attempt != 0 || stored.LeaseOwner != "" {
				t.Fatal("rejected placed claim changed task lease")
			}
		})
	}
}

func TestRunnerSessionClaimRejectsPlacedMaintenance(t *testing.T) {
	s, runner, _, task, now := newSessionPlacedClaimFixture(t)
	var payload map[string]any
	_ = json.Unmarshal(runnerTaskPayloadBytes(task), &payload)
	payload["action"] = domain.RunnerTaskActionCleanupRun
	raw, _ := json.Marshal(payload)
	if err := s.DB.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Update("payload", raw).Error; err != nil {
		t.Fatal(err)
	}
	if got, claimed, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute); err != nil || claimed || got.ID != "" {
		t.Fatalf("execution claim acquired terminal maintenance: task=%+v claimed=%v error=%v", got, claimed, err)
	}
}
