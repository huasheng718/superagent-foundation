package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func contributionTask(t *testing.T, s *Store, id, runner string, count int, now time.Time) domain.RunnerTask {
	t.Helper()
	task := domain.RunnerTask{ID: id, RunnerID: runner, Provider: "contribution-" + id, RunID: "run-" + id, NodeID: "implement", MaxAttempts: 2, Invocation: &domain.RunnerInvocation{}}
	for i := 0; i < count; i++ {
		spec := domain.RunnerContributionSpec{RepositoryID: fmt.Sprintf("repo-%d", i), Dir: fmt.Sprintf("repo-%d", i), BaseSHA: strings.Repeat("1", 40), ContextSHA256: strings.Repeat("a", 64), MaxBytes: domain.RunnerContributionMaxBytes}
		task.ContributionSpecs = append(task.ContributionSpecs, spec)
		task.Invocation.Repositories = append(task.Invocation.Repositories, domain.RunnerRepository{RepositoryID: spec.RepositoryID, Dir: spec.Dir, BaseSHA: spec.BaseSHA})
	}
	if err := s.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := s.ClaimRunnerTask(runner, []string{task.Provider}, now, time.Minute)
	if err != nil || !found || claimed.ID != id {
		t.Fatalf("claim=%+v found=%v err=%v", claimed, found, err)
	}
	return claimed
}

func contributionBytes(t *testing.T, s *Store, task domain.RunnerTask, index int, content string) domain.RunnerContributionReceipt {
	t.Helper()
	spec := task.ContributionSpecs[index]
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	if err := s.PutRunnerContributionBlob(hash, []byte(content)); err != nil {
		t.Fatal(err)
	}
	return domain.RunnerContributionReceipt{TaskID: task.ID, Attempt: task.Attempt, RepositoryID: spec.RepositoryID, Dir: spec.Dir, BaseSHA: spec.BaseSHA, ResultSHA: strings.Repeat("2", 40), ContextSHA256: spec.ContextSHA256, SHA256: hash, Size: int64(len(content))}
}

func TestRunnerContributionStagesThenAtomicallyAcceptsOnSuccess(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	task := contributionTask(t, s, "contribution-success", "runner", 2, now)
	var ids []string
	for i := range task.ContributionSpecs {
		spec, err := s.ValidateRunnerContributionUpload(task.ID, "runner", task.Attempt, task.ContributionSpecs[i].RepositoryID, now)
		if err != nil || spec.ContextSHA256 != task.ContributionSpecs[i].ContextSHA256 {
			t.Fatalf("preflight=%+v err=%v", spec, err)
		}
		receipt := contributionBytes(t, s, task, i, fmt.Sprintf("bundle-data-%d", i))
		receipt.ID, receipt.StorageKey, receipt.AcceptedAt = "client-selected", "client/path", now.Add(-time.Hour)
		staged, err := s.StageRunnerContribution(receipt, "runner", now)
		if err != nil || staged.ID == "" || staged.ID == receipt.ID || staged.StorageKey != staged.SHA256 || !staged.AcceptedAt.IsZero() {
			t.Fatalf("staged=%+v err=%v", staged, err)
		}
		replayed, err := s.StageRunnerContribution(receipt, "runner", now.Add(time.Second))
		if err != nil || replayed.ID != staged.ID || !replayed.CreatedAt.Equal(staged.CreatedAt) {
			t.Fatalf("replayed=%+v err=%v", replayed, err)
		}
		ids = append(ids, staged.ID)
	}
	before, _ := s.RunnerTask(task.ID)
	if len(before.PendingContributions) != 2 || len(before.AcceptedContributions) != 0 || before.Status != domain.RunnerTaskLeased {
		t.Fatalf("upload published early: %+v", before)
	}
	if err := s.CompleteRunnerTask(task.ID, "runner", task.Attempt, domain.RunnerTaskSuccess, nil, "ok", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: ids[:1]}, now.Add(time.Second)); !errors.Is(err, ErrRunnerContributionInvalid) {
		t.Fatalf("partial completion=%v", err)
	}
	partial, _ := s.RunnerTask(task.ID)
	if partial.Status != domain.RunnerTaskLeased || len(partial.AcceptedContributions) != 0 {
		t.Fatal("failed acceptance partially committed")
	}
	if err := s.CompleteRunnerTask(task.ID, "runner", task.Attempt, domain.RunnerTaskSuccess, nil, "ok", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: ids}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	accepted, _ := s.RunnerTask(task.ID)
	if accepted.Status != domain.RunnerTaskSuccess || len(accepted.AcceptedContributions) != 2 || accepted.AcceptedContributions[0].AcceptedAt.IsZero() || accepted.AcceptedContributions[0].StorageKey != accepted.AcceptedContributions[0].SHA256 {
		t.Fatalf("accepted=%+v", accepted)
	}
	if _, err := s.StageRunnerContribution(contributionBytes(t, s, task, 0, "bundle-data-0"), "runner", now.Add(3*time.Second)); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatalf("terminal upload=%v", err)
	}
}

func TestRunnerContributionRejectsChangedContextReplayAndIdentity(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	task := contributionTask(t, s, "contribution-invalid", "runner", 1, now)
	receipt := contributionBytes(t, s, task, 0, "bundle")
	for _, mutate := range []func(*domain.RunnerContributionReceipt){
		func(r *domain.RunnerContributionReceipt) { r.ContextSHA256 = strings.Repeat("b", 64) },
		func(r *domain.RunnerContributionReceipt) { r.BaseSHA = strings.Repeat("3", 40) },
		func(r *domain.RunnerContributionReceipt) { r.Dir = "../escape" },
		func(r *domain.RunnerContributionReceipt) { r.ResultSHA = "HEAD" },
		func(r *domain.RunnerContributionReceipt) { r.SHA256 = strings.Repeat("f", 64) },
		func(r *domain.RunnerContributionReceipt) { r.Size++ },
	} {
		invalid := receipt
		mutate(&invalid)
		if _, err := s.StageRunnerContribution(invalid, "runner", now); err == nil {
			t.Fatalf("invalid receipt accepted: %+v", invalid)
		}
	}
	if _, err := s.StageRunnerContribution(receipt, "other", now); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatalf("wrong runner=%v", err)
	}
	staged, err := s.StageRunnerContribution(receipt, "runner", now)
	if err != nil {
		t.Fatal(err)
	}
	conflict := contributionBytes(t, s, task, 0, "other-bundle")
	if _, err := s.StageRunnerContribution(conflict, "runner", now); !errors.Is(err, ErrRunnerContributionConflict) {
		t.Fatalf("conflicting replay=%v", err)
	}
	if err := s.CompleteRunnerTask(task.ID, "runner", task.Attempt, domain.RunnerTaskFailed, nil, "", "failure", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{staged.ID}}, now); err != nil {
		t.Fatal(err)
	}
	failed, _ := s.RunnerTask(task.ID)
	if len(failed.AcceptedContributions) != 0 {
		t.Fatal("failed execution accepted contribution")
	}
}

func TestRunnerContributionExpiryReclaimAndCancellationNeverAccept(t *testing.T) {
	for _, cancellation := range []bool{false, true} {
		t.Run(fmt.Sprint(cancellation), func(t *testing.T) {
			s := newTestRunner(t)
			now := time.Now().UTC()
			task := contributionTask(t, s, "contribution-fence", "runner", 1, now)
			receipt := contributionBytes(t, s, task, 0, "bundle")
			if _, err := s.ValidateRunnerContributionUpload(task.ID, "runner", task.Attempt, receipt.RepositoryID, now); err != nil {
				t.Fatal(err)
			}
			if cancellation {
				if err := s.CancelRunnerTask(task.ID, now); err != nil {
					t.Fatal(err)
				}
				if _, err := s.StageRunnerContribution(receipt, "runner", now); !errors.Is(err, ErrRunnerTaskLeaseLost) {
					t.Fatalf("canceled stage=%v", err)
				}
				if err := s.CompleteRunnerTask(task.ID, "runner", task.Attempt, domain.RunnerTaskCanceled, nil, "", "", nil, nil, now); err != nil {
					t.Fatal(err)
				}
			} else {
				expiredAt := *task.LeaseUntil
				if _, err := s.StageRunnerContribution(receipt, "runner", expiredAt); !errors.Is(err, ErrRunnerTaskLeaseLost) {
					t.Fatalf("expiry-boundary stage=%v", err)
				}
				if _, err := s.ExpireRunnerTasks(expiredAt); err != nil {
					t.Fatal(err)
				}
				second, found, err := s.ClaimRunnerTask("runner", []string{task.Provider}, expiredAt, time.Minute)
				if err != nil || !found || second.Attempt != 2 {
					t.Fatalf("second=%+v err=%v", second, err)
				}
				if _, err := s.StageRunnerContribution(receipt, "runner", expiredAt); !errors.Is(err, ErrRunnerTaskLeaseLost) {
					t.Fatalf("old attempt stage=%v", err)
				}
				current := contributionBytes(t, s, second, 0, "current-bundle")
				staged, err := s.StageRunnerContribution(current, "runner", expiredAt)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.CompleteRunnerTask(task.ID, "runner", 1, domain.RunnerTaskSuccess, nil, "", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{staged.ID}}, expiredAt); !errors.Is(err, ErrRunnerTaskLeaseLost) {
					t.Fatalf("old complete=%v", err)
				}
				if err := s.CompleteRunnerTask(task.ID, "runner", 2, domain.RunnerTaskSuccess, nil, "", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{staged.ID}}, expiredAt); err != nil {
					t.Fatal(err)
				}
			}
			stored, _ := s.RunnerTask(task.ID)
			for _, accepted := range stored.AcceptedContributions {
				if accepted.Attempt != 2 || cancellation {
					t.Fatalf("stale/canceled acceptance: %+v", accepted)
				}
			}
		})
	}
}

func TestRunnerContributionBlobLimitAndImmutableBytes(t *testing.T) {
	s := newTestRunner(t)
	content := []byte("bundle")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	if err := s.PutRunnerContributionBlob(hash, content); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunnerContributionBlob(hash, []byte("changed")); !errors.Is(err, ErrRunnerContributionInvalid) {
		t.Fatalf("hash overwrite=%v", err)
	}
	stored, size, found := s.RunnerContributionBlob(hash)
	if !found || string(stored) != string(content) || size != int64(len(content)) {
		t.Fatal("immutable bytes changed")
	}
	if err := s.PutRunnerContributionBlob(hash, make([]byte, domain.RunnerContributionMaxBytes+1)); !errors.Is(err, ErrRunnerContributionTooLarge) {
		t.Fatalf("oversize=%v", err)
	}
	if err := s.PutRunnerTask(domain.RunnerTask{ID: "forged-accepted", Provider: "terminal", AcceptedContributions: []domain.RunnerContributionReceipt{{ID: "forged"}}}); !errors.Is(err, ErrRunnerContributionInvalid) {
		t.Fatalf("forged acceptance=%v", err)
	}
}

func TestRunnerLeaseFenceHonorsInjectedDatabaseClock(t *testing.T) {
	s := newTestRunner(t)
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.DB.NowFunc = func() time.Time { return now }
	task := contributionTask(t, s, "injected-lease-clock", "runner", 0, now)
	if err := s.StartRunnerTask(task.ID, "runner", task.Attempt, now); err != nil {
		t.Fatal(err)
	}
	now = *task.LeaseUntil
	if _, err := s.RenewRunnerTaskLease(task.ID, "runner", task.Attempt, time.Minute, now.Add(-time.Minute)); !errors.Is(err, ErrRunnerTaskLeaseLost) {
		t.Fatalf("injected clock expiry=%v", err)
	}
}
