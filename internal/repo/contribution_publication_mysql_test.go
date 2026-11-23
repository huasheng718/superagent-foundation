package repo

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestMySQLContributionPublicationFencesAndRestart(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_MYSQL_DSN is not set")
	}
	first, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { first.Close() }()
	second, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	id := fmt.Sprintf("%064x", time.Now().UnixNano())
	value := publicationFixture(t, first, id)
	t.Run("concurrent-gates", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 3)
		for _, kind := range []string{"fast", "slow", "review"} {
			wg.Add(1)
			go func(kind string) {
				defer wg.Done()
				_, err := second.RecordContributionGate(id, publicationGate(value, kind, "passed"), time.Now().UTC())
				errs <- err
			}(kind)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		ready, _ := first.ContributionPublication(id)
		if ready.State != "ready" || len(ready.Gates) != 3 {
			t.Fatalf("lost concurrent gates: %+v", ready)
		}
	})
	var winner domain.ContributionPublication
	t.Run("one-cross-instance-publisher", func(t *testing.T) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		type outcome struct {
			value domain.ContributionPublication
			err   error
		}
		results := make(chan outcome, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				store := first
				if i%2 == 1 {
					store = second
				}
				claimed, err := store.ClaimContributionPublication(id, fmt.Sprintf("publisher-%d", i), time.Minute, time.Now().UTC())
				results <- outcome{claimed, err}
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)
		wins := 0
		for result := range results {
			if result.err == nil {
				winner = result.value
				wins++
			} else if !errors.Is(result.err, ErrContributionPublicationLeaseLost) {
				t.Fatal(result.err)
			}
		}
		if wins != 1 || winner.Attempt != 1 {
			t.Fatalf("publisher wins=%d attempt=%d", wins, winner.Attempt)
		}
	})
	t.Run("callback-lock-wait-expiry", func(t *testing.T) {
		until := time.Now().UTC().Add(100 * time.Millisecond)
		if err := first.DB.Model(&contributionPublicationRow{}).Where("id = ?", id).Update("lease_until", until).Error; err != nil {
			t.Fatal(err)
		}
		tx := first.DB.Begin()
		defer tx.Rollback()
		var row contributionPublicationRow
		if err := first.locking(tx).Where("id = ?", id).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		requested := time.Now().UTC()
		done := make(chan error, 1)
		go func() {
			_, err := second.CompleteContributionPublication(id, winner.LeaseOwner, winner.Attempt, "https://github.com/example/repo/pull/17", 17, requested)
			done <- err
		}()
		time.Sleep(180 * time.Millisecond)
		if err := tx.Commit().Error; err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrContributionPublicationLeaseLost) {
				t.Fatalf("expired callback accepted: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("publisher callback lock waiter stalled")
		}
		current, _ := first.ContributionPublication(id)
		if current.State != "publishing" || current.PRURL != "" {
			t.Fatalf("expired PR result persisted: %+v", current)
		}
	})
	// A delayed callback from the same owner still cannot cross attempt fencing.
	reclaimed, err := second.ClaimContributionPublication(id, winner.LeaseOwner, time.Minute, time.Now().UTC())
	if err != nil || reclaimed.Attempt != 2 {
		t.Fatalf("reclaim=%+v err=%v", reclaimed, err)
	}
	if _, err := first.CompleteContributionPublication(id, winner.LeaseOwner, 1, "https://github.com/example/repo/pull/17", 17, time.Now().UTC()); !errors.Is(err, ErrContributionPublicationLeaseLost) {
		t.Fatalf("stale owner attempt persisted: %v", err)
	}
	if _, err := second.CompleteContributionPublication(id, winner.LeaseOwner, 2, "https://github.com/example/repo/pull/17", 17, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	first.Close()
	first, err = Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	stored, found := first.ContributionPublication(id)
	if !found || stored.State != "pr_ready" || stored.PRNumber != 17 || len(stored.Gates) != 3 || stored.ResultSHA != value.ResultSHA || stored.ManifestSHA256 != id || stored.LeaseUntil != nil || stored.Branch != value.Branch {
		t.Fatalf("restart lost PR-ready fixed-SHA proof: %+v", stored)
	}
}
