package repo

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestRunnerSessionClaimRetiresObsoleteHeadAndContinues(t *testing.T) {
	s, runner, run, stale, now := newSessionPlacedClaimFixture(t)
	// The frozen registration is permanently obsolete after a rebind. Keep the
	// Run/Node otherwise active so this is not an Engine lease recovery case.
	runner.SessionTokenHash = strings.Repeat("2", 64)
	runner.SessionExpiresAt = ptrTime(now.Add(time.Hour))
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	legacy := domain.RunnerTask{ID: "healthy-after-obsolete", RunnerID: runner.ID, Provider: "codex", Status: domain.RunnerTaskQueued}
	if err := s.PutRunnerTask(legacy); err != nil {
		t.Fatal(err)
	}
	// The new session authenticates, retires only the old frozen head, and
	// proceeds to the healthy legacy task.
	claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
	if err != nil || !found || claimed.ID != legacy.ID {
		t.Fatalf("healthy task was starved: claimed=%+v found=%v err=%v", claimed, found, err)
	}
	retired, ok := s.RunnerTask(stale.ID)
	if !ok || retired.Status != domain.RunnerTaskFailed || retired.Error != "frozen runner placement is obsolete" {
		t.Fatalf("obsolete head was not retired: %+v", retired)
	}
	if stored, ok := s.RunState(run.ID); !ok || stored.Nodes[stale.NodeID].ExecutionTaskID != stale.ID {
		t.Fatal("retirement mutated Run author state")
	}
}

func TestRunnerTaskHistoryBindsFrozenRegistration(t *testing.T) {
	s, runner, _, task, now := newSessionPlacedClaimFixture(t)
	claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if err := s.StartRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendRunnerTaskEventsForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, []domain.RunnerTaskEvent{{Seq: 1, Data: "private history"}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, domain.RunnerTaskSuccess, nil, "done", "", nil, nil, now); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.RunnerTaskForSessionHistory(task.ID, runner.ID, runner.SessionTokenHash, now); err != nil || !ok || got.ID != task.ID {
		t.Fatalf("same registration history denied: task=%+v ok=%v err=%v", got, ok, err)
	}
	events, ok, err := s.RunnerTaskEventsForSession(task.ID, runner.ID, runner.SessionTokenHash, now)
	if err != nil || !ok || len(events) != 1 || events[0].Data != "private history" {
		t.Fatalf("same registration events denied: events=%+v ok=%v err=%v", events, ok, err)
	}
	oldSession := runner.SessionTokenHash
	runner.SessionTokenHash = strings.Repeat("2", 64)
	runner.SessionExpiresAt = ptrTime(now.Add(time.Hour))
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.RunnerTaskForSessionHistory(task.ID, runner.ID, oldSession, now); !errors.Is(err, ErrRunnerPlacementSessionInvalid) || ok {
		t.Fatalf("old session history leaked: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.RunnerTaskForSessionHistory(task.ID, runner.ID, runner.SessionTokenHash, now); err != nil || ok {
		t.Fatalf("new registration read old history: ok=%v err=%v", ok, err)
	}
}

func TestRunnerTaskHistoryRemainsReadableWhenRunnerOffline(t *testing.T) {
	s, runner, _, task, now := newSessionPlacedClaimFixture(t)
	claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, now, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if err := s.StartRunnerTaskForSession(task.ID, runner.ID, runner.SessionTokenHash, claimed.Attempt, now); err != nil {
		t.Fatal(err)
	}
	runner.Status = domain.RunnerStatusOffline
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.RunnerTaskForSessionHistory(task.ID, runner.ID, runner.SessionTokenHash, now)
	if err != nil || !ok || got.ID != task.ID {
		t.Fatalf("offline runner lost same-registration history: task=%+v ok=%v err=%v", got, ok, err)
	}
}

func ptrTime(v time.Time) *time.Time { return &v }
