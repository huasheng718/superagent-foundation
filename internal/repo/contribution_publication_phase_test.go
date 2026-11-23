package repo

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestContributionPRCreateMarkerIsDurableAttemptFencedAndSticky(t *testing.T) {
	s := newTestRunner(t)
	value := publicationReady(t, s, strings.Repeat("a", 64))
	now := time.Now().UTC()
	first, err := s.ClaimContributionPublication(value.ID, "first", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkContributionPRCreateAttempted(value.ID, "wrong-owner", first.Attempt, now); !errors.Is(err, ErrContributionPublicationLeaseLost) {
		t.Fatalf("wrong owner marked: %v", err)
	}
	if _, err := s.MarkContributionPRCreateAttempted(value.ID, "first", first.Attempt+1, now); !errors.Is(err, ErrContributionPublicationLeaseLost) {
		t.Fatalf("wrong attempt marked: %v", err)
	}
	marked, err := s.MarkContributionPRCreateAttempted(value.ID, "first", first.Attempt, now)
	if err != nil || !marked.PRCreateAttempted || marked.PhaseVersion != 1 {
		t.Fatalf("mark=%+v err=%v", marked, err)
	}
	if _, err := s.MarkContributionPRCreateAttempted(value.ID, "first", first.Attempt, now); !errors.Is(err, ErrContributionPRCreateUncertain) {
		t.Fatalf("second create authority: %v", err)
	}
	second, err := s.ClaimContributionPublication(value.ID, "second", time.Minute, *first.LeaseUntil)
	if err != nil || !second.PRCreateAttempted {
		t.Fatalf("reclaim lost phase: %+v %v", second, err)
	}
	if _, err := s.MarkContributionPRCreateAttempted(value.ID, "first", first.Attempt, *first.LeaseUntil); !errors.Is(err, ErrContributionPublicationLeaseLost) {
		t.Fatalf("stale owner marked: %v", err)
	}
	if _, err := s.MarkContributionPRCreateAttempted(value.ID, "second", second.Attempt, *first.LeaseUntil); !errors.Is(err, ErrContributionPRCreateUncertain) {
		t.Fatalf("new owner recreated: %v", err)
	}
	if _, err := s.MarkContributionPRCreateAttempted(value.ID, "second", second.Attempt, *second.LeaseUntil); !errors.Is(err, ErrContributionPublicationLeaseLost) {
		t.Fatalf("expired mark accepted: %v", err)
	}
	stored, _ := s.ContributionPublication(value.ID)
	if !stored.PRCreateAttempted {
		t.Fatal("phase not durable")
	}
}
func TestContributionPRCreateMarkerLegacyCompatibilityIsConservative(t *testing.T) {
	for _, state := range []string{"ready", "publishing"} {
		t.Run(state, func(t *testing.T) {
			s := newTestRunner(t)
			value := publicationReady(t, s, strings.Repeat("b", 64))
			value.PhaseVersion = 0
			value.PRCreateAttempted = false
			value.State = state
			value.Attempt = 1
			payload, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DB.Model(&contributionPublicationRow{}).Where("id = ?", value.ID).Updates(map[string]any{"state": state, "attempt": 1, "payload": string(payload)}).Error; err != nil {
				t.Fatal(err)
			}
			claimed, err := s.ClaimContributionPublication(value.ID, "upgrade", time.Minute, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if claimed.PhaseVersion != 1 || claimed.PRCreateAttempted != (state == "publishing") {
				t.Fatalf("legacy phase upgraded unsafely: %+v", claimed)
			}
		})
	}
}

// The protocol extends the existing JSON payload; no automatic data migration
// or table is needed, and terminal historical receipts remain readable.
func TestContributionPRCreateMarkerPreservesTerminalHistoricalReceipt(t *testing.T) {
	s := newTestRunner(t)
	value := publicationReady(t, s, strings.Repeat("c", 64))
	now := time.Now().UTC()
	claimed, err := s.ClaimContributionPublication(value.ID, "owner", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := s.CompleteContributionPublication(value.ID, "owner", claimed.Attempt, "https://github.com/example/repo/pull/9", 9, now)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State != "pr_ready" {
		t.Fatal("historical completion changed")
	}
}
