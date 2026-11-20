package repo

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestMySQLRunnerSessionCallbackWaitExpiry(t *testing.T) {
	for _, mode := range []string{"session", "engine-lease"} {
		t.Run(mode, func(t *testing.T) {
			s, other, runner, run, task := newMySQLSessionClaimFixture(t)
			claimed, found, err := s.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"codex"}, time.Now().UTC(), time.Minute)
			if err != nil || !found {
				t.Fatalf("claim found=%v err=%v", found, err)
			}
			task = claimed
			expires := time.Now().UTC().Add(600 * time.Millisecond)
			if mode == "engine-lease" {
				if err := s.DB.Model(&runRow{}).Where("id = ?", run.ID).Update("lease_until", expires).Error; err != nil {
					t.Fatal(err)
				}
			}
			blocker := other.DB.Begin()
			if blocker.Error != nil {
				t.Fatal(blocker.Error)
			}
			defer blocker.Rollback()
			var row runnerRow
			if err := other.locking(blocker).Where("id = ?", runner.ID).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if mode == "session" {
				if err := blocker.Model(&runnerRow{}).Where("id = ?", runner.ID).Update("session_expires_at", expires).Error; err != nil {
					t.Fatal(err)
				}
			}
			waiting := make(chan struct{})
			var once sync.Once
			callback := "session-callback-wait-" + mode
			if err := s.DB.Callback().Query().Before("gorm:query").Register(callback, func(db *gorm.DB) {
				if db.Statement.Table == "runners" {
					once.Do(func() { close(waiting) })
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer s.DB.Callback().Query().Remove(callback)
			result := make(chan error, 1)
			captured := time.Now().UTC()
			go func() { result <- runSessionCallback(s, "renew", task, runner, runner.SessionTokenHash, captured) }()
			select {
			case <-waiting:
			case early := <-result:
				t.Fatalf("callback did not wait for registry lock: %v", early)
			case <-time.After(5 * time.Second):
				t.Fatal("callback did not reach registry lock")
			}
			if remaining := time.Until(expires.Add(150 * time.Millisecond)); remaining > 0 {
				time.Sleep(remaining)
			}
			if err := blocker.Commit().Error; err != nil {
				t.Fatal(err)
			}
			want := ErrRunnerPlacementSessionInvalid
			if mode == "engine-lease" {
				want = ErrRunnerTaskLeaseLost
			}
			select {
			case err := <-result:
				if !errors.Is(err, want) {
					t.Fatalf("expired callback authorized: got=%v want=%v", err, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("callback did not finish after lock release")
			}
			stored, _ := s.RunnerTask(task.ID)
			if !reflect.DeepEqual(task, stored) {
				t.Fatal("expired callback changed task or lease")
			}
		})
	}
}
