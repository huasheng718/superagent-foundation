package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func runSessionCallback(s *Store, operation string, task domain.RunnerTask, runner domain.Runner, session string, now time.Time) error {
	switch operation {
	case "start":
		return s.StartRunnerTaskForSession(task.ID, runner.ID, session, task.Attempt, now)
	case "renew":
		_, err := s.RenewRunnerTaskLeaseForSession(task.ID, runner.ID, session, task.Attempt, 2*time.Minute, now)
		return err
	case "complete":
		return s.CompleteRunnerTaskForSession(task.ID, runner.ID, session, task.Attempt, domain.RunnerTaskSuccess, nil, "completed", "", nil, nil, now)
	case "events":
		_, err := s.AppendRunnerTaskEventsForSession(task.ID, runner.ID, session, task.Attempt, []domain.RunnerTaskEvent{{Seq: 1, Type: "output", Data: "private event"}}, now)
		return err
	}
	panic("unknown callback")
}

func TestRunnerSessionCallbacksFenceCurrentAuthority(t *testing.T) {
	for _, operation := range []string{"start", "renew", "complete", "events"} {
		t.Run(operation, func(t *testing.T) {
			for _, mode := range []string{"valid", "empty-session", "wrong-session", "expired-session", "registry-auth-drift", "run-cancelled", "run-paused", "engine-expired", "node-superseded", "task-lease-expired", "old-task-attempt"} {
				t.Run(mode, func(t *testing.T) {
					s, runner, run, task, now := newSessionPlacedClaimFixture(t)
					claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
					if err != nil || !found {
						t.Fatalf("claim found=%v err=%v", found, err)
					}
					task = claimed
					session := runner.SessionTokenHash
					want := ErrRunnerTaskLeaseLost
					switch mode {
					case "empty-session":
						session, want = "", ErrRunnerPlacementSessionInvalid
					case "wrong-session":
						session, want = strings.Repeat("2", 64), ErrRunnerPlacementSessionInvalid
					case "expired-session":
						expires := now.Add(-time.Second)
						runner.SessionExpiresAt, want = &expires, ErrRunnerPlacementSessionInvalid
						if err := s.PutRunner(runner); err != nil {
							t.Fatal(err)
						}
					case "registry-auth-drift":
						want = domain.ErrRunnerPlacementChanged
						if err := s.SetRunnerRuntimeAuthMode(runner.ID, "codex", domain.RuntimeAuthPersonal, now); err != nil {
							t.Fatal(err)
						}
					case "run-cancelled":
						if _, err := s.RequestRunCancel(run.ID); err != nil {
							t.Fatal(err)
						}
					case "run-paused":
						if _, err := s.RequestRunPause(run.ID); err != nil {
							t.Fatal(err)
						}
					case "engine-expired":
						if err := s.DB.Model(&runRow{}).Where("id = ?", run.ID).Update("lease_until", now.Add(-time.Second)).Error; err != nil {
							t.Fatal(err)
						}
					case "node-superseded":
						run.Nodes[task.NodeID].Attempt++
						run.Nodes[task.NodeID].ExecutionPlacement = nil
						if err := s.PutRun(run); err != nil {
							t.Fatal(err)
						}
					case "task-lease-expired":
						now = claimed.LeaseUntil.Add(time.Second)
					case "old-task-attempt":
						task.Attempt++
					}
					before, _ := s.RunnerTask(task.ID)
					err = runSessionCallback(s, operation, task, runner, session, now)
					if mode == "valid" {
						if err != nil {
							t.Fatal(err)
						}
						stored, _ := s.RunnerTask(task.ID)
						switch operation {
						case "start":
							if stored.Status != domain.RunnerTaskRunning || stored.StartedAt == nil {
								t.Fatal("start did not persist")
							}
						case "renew":
							if !stored.LeaseUntil.After(*before.LeaseUntil) {
								t.Fatal("lease did not extend")
							}
						case "complete":
							if stored.Status != domain.RunnerTaskSuccess || stored.CompletedAt == nil || stored.LeaseUntil != nil {
								t.Fatal("completion did not persist")
							}
						case "events":
							if len(s.RunnerTaskEventsAfter(task.ID, task.Attempt, 0)) != 1 {
								t.Fatal("event did not persist")
							}
						}
						return
					}
					if !errors.Is(err, want) {
						t.Fatalf("expected %v, got %v", want, err)
					}
					after, _ := s.RunnerTask(task.ID)
					if !reflect.DeepEqual(before, after) || len(s.RunnerTaskEventsAfter(task.ID, claimed.Attempt, 0)) != 0 {
						t.Fatal("rejected callback changed task or events")
					}
				})
			}
		})
	}
}

func TestRunnerSessionCanceledCallbackConvergesAfterRunCancel(t *testing.T) {
	s, runner, run, task, now := newSessionPlacedClaimFixture(t)
	claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if _, err := s.RequestRunCancel(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelRunnerTask(task.ID, now); err != nil {
		t.Fatal(err)
	}
	canceledRun, _ := s.RunState(run.ID)
	canceledRun.Status = "cancelled"
	canceledRun.Nodes[task.NodeID].Status = "cancelled"
	if err := s.PutRun(canceledRun); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteRunnerTaskForSession(claimed.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, domain.RunnerTaskCanceled, nil, "runner stopped", "", nil, nil, now); err != nil {
		t.Fatalf("canceled callback should converge before lease expiry: %v", err)
	}
	stored, ok := s.RunnerTask(task.ID)
	if !ok || stored.Status != domain.RunnerTaskCanceled || stored.LeaseOwner != "" || stored.LeaseUntil != nil || stored.CompletedAt == nil {
		t.Fatalf("canceled callback did not settle task: %+v", stored)
	}
}

func TestRunnerLegacyCallbacksCannotBypassPlacedAuthority(t *testing.T) {
	s, runner, _, _, now := newSessionPlacedClaimFixture(t)
	task, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	before, _ := s.RunnerTask(task.ID)
	operations := []func() error{
		func() error { return s.StartRunnerTask(task.ID, runner.ID, task.Attempt, now) },
		func() error {
			_, err := s.RenewRunnerTaskLease(task.ID, runner.ID, task.Attempt, time.Minute, now)
			return err
		},
		func() error {
			return s.CompleteRunnerTask(task.ID, runner.ID, task.Attempt, domain.RunnerTaskSuccess, nil, "", "", nil, nil, now)
		},
		func() error {
			_, err := s.AppendRunnerTaskEvents(task.ID, runner.ID, task.Attempt, []domain.RunnerTaskEvent{{Seq: 1}}, now)
			return err
		},
	}
	for i, operation := range operations {
		if err := operation(); !errors.Is(err, ErrRunnerTaskLeaseLost) {
			t.Fatalf("legacy callback %d: %v", i, err)
		}
	}
	after, _ := s.RunnerTask(task.ID)
	if !reflect.DeepEqual(before, after) || len(s.RunnerTaskEventsAfter(task.ID, task.Attempt, 0)) != 0 {
		t.Fatal("legacy callback changed placed task")
	}
}

func TestRunnerSessionCallbackRechecksPostLockClock(t *testing.T) {
	for _, mode := range []string{"session", "engine", "task"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, run, _, now := newSessionPlacedClaimFixture(t)
			task, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
			if err != nil || !found {
				t.Fatalf("claim found=%v err=%v", found, err)
			}
			advanced := task.LeaseUntil.Add(time.Second)
			want := ErrRunnerTaskLeaseLost
			if mode == "session" {
				advanced, want = runner.SessionExpiresAt.Add(time.Second), ErrRunnerPlacementSessionInvalid
			} else if mode == "engine" {
				advanced = run.LeaseUntil.Add(time.Second)
			}
			s.RunnersRepo.DB = s.RunnersRepo.DB.Session(&gorm.Session{NowFunc: func() time.Time { return advanced }})
			if err := s.StartRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, task.Attempt, now); !errors.Is(err, want) {
				t.Fatalf("expired post-lock authority: %v", err)
			}
			stored, _ := s.RunnerTask(task.ID)
			if stored.Status != domain.RunnerTaskLeased || stored.StartedAt != nil {
				t.Fatal("expired authority started task")
			}
		})
	}
}

func TestRunnerSessionVerificationCompletionRequiresFrozenHead(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "modified-code"}[changed], func(t *testing.T) {
			s, _, author, runner, now := newNodeQueueFixture(t, true)
			if err := s.QueueNodeExecutionTask(author); err != nil {
				t.Fatal(err)
			}
			_, input := completeNodeQueueFixture(t, s, author, runner, now)
			verify := domain.PublicRunnerTask(author)
			verify.ID, verify.Provider = "callback-verifier", "terminal"
			verify.Execution = &domain.NodeTaskExecution{Role: domain.NodeTaskRoleVerification, NodeAttempt: 1, Sequence: 1, PreviousAuthorTaskID: author.ID}
			verify.Invocation.ContributionInputs = []domain.RunnerContributionInput{input}
			freezeNodeQueueTask(t, &verify)
			if err := s.QueueNodeExecutionTask(verify); err != nil {
				t.Fatal(err)
			}
			claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"terminal"}, now, time.Minute)
			if err != nil || !found {
				t.Fatalf("verifier claim found=%v err=%v", found, err)
			}
			raw := []byte("verification durable receipt bytes")
			hash := sha256.Sum256(raw)
			digest := hex.EncodeToString(hash[:])
			if err := s.PutRunnerContributionBlob(digest, raw); err != nil {
				t.Fatal(err)
			}
			head := input.HeadSHA
			if changed {
				head = strings.Repeat("3", 40)
			}
			spec := verify.ContributionSpecs[0]
			receipt, err := s.StageRunnerContributionForSession(domain.RunnerContributionReceipt{TaskID: verify.ID, Attempt: claimed.Attempt, RepositoryID: spec.RepositoryID, Dir: spec.Dir, BaseSHA: spec.BaseSHA, ResultSHA: head, ContextSHA256: spec.ContextSHA256, SHA256: digest, Size: int64(len(raw))}, runner.ID, runner.SessionTokenHash, now)
			if err != nil {
				t.Fatal(err)
			}
			err = s.CompleteRunnerTaskForSession(verify.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, domain.RunnerTaskSuccess, nil, "exit 0", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{receipt.ID}}, now)
			stored, _ := s.RunnerTask(verify.ID)
			if changed {
				if !errors.Is(err, ErrRunnerContributionInvalid) || stored.Status != domain.RunnerTaskLeased || len(stored.AcceptedContributions) != 0 {
					t.Fatalf("changed code verified: status=%s accepted=%d err=%v", stored.Status, len(stored.AcceptedContributions), err)
				}
			} else if err != nil || stored.Status != domain.RunnerTaskSuccess || len(stored.AcceptedContributions) != 1 {
				t.Fatalf("unchanged verification failed: status=%s err=%v", stored.Status, err)
			}
		})
	}
}

func TestRunnerLegacyCallbacksRejectMalformedPayload(t *testing.T) {
	s, runner, _, now := newRegistryPlacementFixture(t, false)
	if err := s.PutRunnerTask(domain.RunnerTask{ID: "malformed-callback", RunnerID: runner.ID, Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	task, found, err := s.ClaimRunnerTask(runner.ID, []string{"codex"}, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("legacy claim found=%v err=%v", found, err)
	}
	if err := s.DB.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Update("payload", []byte(`{"executionPlacement":"invalid-snapshot"}`)).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.StartRunnerTask(task.ID, runner.ID, task.Attempt, now); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatalf("malformed persisted authority mutated: %v", err)
	}
	var row runnerTaskRow
	if err := s.DB.Where("id = ?", task.ID).Take(&row).Error; err != nil || row.Status != domain.RunnerTaskLeased {
		t.Fatalf("malformed task state changed: status=%s error=%v", row.Status, err)
	}
}
