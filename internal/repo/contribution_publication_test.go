package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func publicationFixture(t *testing.T, s *Store, id string) domain.ContributionPublication {
	t.Helper()
	raw := []byte("publication-source")
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if err := s.PutRunnerContributionBlob(digest, raw); err != nil {
		t.Fatal(err)
	}
	integration := domain.ContributionIntegration{ID: id, ManifestSHA256: id, RunID: "publication-run", RepositoryID: "publication-repo", BaseSHA: strings.Repeat("1", 40), ResultSHA: strings.Repeat("2", 40), ContextSHA256: strings.Repeat("c", 64), BundleSHA256: digest, BundleSize: int64(len(raw)), State: "prepared", Inputs: []domain.RunnerContributionReceipt{{ID: "source-receipt", SHA256: digest, Size: int64(len(raw))}}}
	if _, err := s.PrepareContributionIntegration(integration); err != nil {
		t.Fatal(err)
	}
	value, err := s.PrepareContributionPublication(id, "main", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func publicationGate(value domain.ContributionPublication, kind, status string) domain.ContributionGateEvidence {
	return domain.ContributionGateEvidence{ID: kind + "-evidence", Kind: kind, IntegrationID: value.ID, ResultSHA: value.ResultSHA, ContextSHA256: value.ContextSHA256, ManifestSHA256: value.ManifestSHA256, Status: status, EvidenceRef: "controller-evidence/" + kind, EvidenceSHA256: strings.Repeat("e", 64), ReviewerID: "independent-review-session"}
}

func publicationReady(t *testing.T, s *Store, id string) domain.ContributionPublication {
	t.Helper()
	value := publicationFixture(t, s, id)
	for _, kind := range []string{"fast", "slow", "review"} {
		var err error
		value, err = s.RecordContributionGate(id, publicationGate(value, kind, "passed"), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
	}
	return value
}

func TestContributionPublicationRequiresIndependentExactSHAGates(t *testing.T) {
	s := newTestRunner(t)
	value := publicationFixture(t, s, strings.Repeat("a", 64))
	now := time.Now().UTC()
	if _, err := s.ClaimContributionPublication(value.ID, "publisher", time.Minute, now); !errors.Is(err, ErrContributionPublicationNotReady) {
		t.Fatalf("ungated claim: %v", err)
	}
	if _, err := s.PrepareContributionPublication(value.ID, "other-target", now); !errors.Is(err, ErrContributionPublicationConflict) {
		t.Fatalf("target changed: %v", err)
	}
	for _, field := range []string{"sha", "context", "manifest", "integration", "evidence", "reviewer"} {
		gate := publicationGate(value, "review", "passed")
		switch field {
		case "sha":
			gate.ResultSHA = strings.Repeat("9", 40)
		case "context":
			gate.ContextSHA256 = strings.Repeat("9", 64)
		case "manifest":
			gate.ManifestSHA256 = strings.Repeat("9", 64)
		case "integration":
			gate.IntegrationID = strings.Repeat("9", 64)
		case "evidence":
			gate.EvidenceSHA256 = "invalid"
		case "reviewer":
			gate.ReviewerID = ""
		}
		if _, err := s.RecordContributionGate(value.ID, gate, now); !errors.Is(err, ErrContributionPublicationInvalid) {
			t.Fatalf("%s accepted: %v", field, err)
		}
	}
	fast := publicationGate(value, "fast", "passed")
	fast.GateTaskID = "independent-fast-task"
	first, err := s.RecordContributionGate(value.ID, fast, now)
	if err != nil {
		t.Fatal(err)
	}
	fast.CreatedAt = now.Add(-time.Hour)
	replay, err := s.RecordContributionGate(value.ID, fast, now.Add(time.Second))
	if err != nil || len(replay.Gates) != 1 || !replay.Gates[0].CreatedAt.Equal(first.Gates[0].CreatedAt) {
		t.Fatalf("replay changed evidence: %+v %v", replay, err)
	}
	slow := publicationGate(value, "slow", "passed")
	slow.GateTaskID = fast.GateTaskID
	if _, err := s.RecordContributionGate(value.ID, slow, now); !errors.Is(err, ErrContributionPublicationConflict) {
		t.Fatalf("same task across gate kinds: %v", err)
	}
	slow.GateTaskID = "independent-slow-task"
	slow.ID = fast.ID
	if _, err := s.RecordContributionGate(value.ID, slow, now); !errors.Is(err, ErrContributionPublicationConflict) {
		t.Fatalf("same ID across gate kinds: %v", err)
	}
	for _, kind := range []string{"slow", "review"} {
		value, err = s.RecordContributionGate(value.ID, publicationGate(value, kind, "passed"), now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if value.State != "ready" || len(value.Gates) != 3 || value.Branch != "superagent/integration/"+value.ID {
		t.Fatalf("wrong readiness: %+v", value)
	}
	if value, err = s.PrepareContributionPublication(value.ID, "main", now); err != nil || value.State != "ready" || len(value.Gates) != 3 {
		t.Fatalf("prepare replay reset gates: %+v %v", value, err)
	}
	// A new immutable integration cannot borrow prior hash/context evidence.
	other := publicationFixture(t, s, strings.Repeat("d", 64))
	if _, err := s.RecordContributionGate(other.ID, fast, now); !errors.Is(err, ErrContributionPublicationInvalid) {
		t.Fatalf("cross integration evidence accepted: %v", err)
	}
}

func TestContributionPublicationFailureCannotBecomeReady(t *testing.T) {
	for _, status := range []string{"failed", "rejected"} {
		t.Run(status, func(t *testing.T) {
			s := newTestRunner(t)
			value := publicationFixture(t, s, strings.Repeat("a", 64))
			for i, kind := range []string{"fast", "slow", "review"} {
				gateStatus := "passed"
				if i == 0 {
					gateStatus = status
				}
				var err error
				value, err = s.RecordContributionGate(value.ID, publicationGate(value, kind, gateStatus), time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
			}
			if value.State != "rejected" || len(value.Gates) != 3 {
				t.Fatalf("failure became ready: %+v", value)
			}
			if _, err := s.RecordContributionGate(value.ID, publicationGate(value, "fast", "passed"), time.Now().UTC()); !errors.Is(err, ErrContributionPublicationConflict) {
				t.Fatalf("overwrote failure: %v", err)
			}
			if _, err := s.ClaimContributionPublication(value.ID, "publisher", time.Minute, time.Now().UTC()); !errors.Is(err, ErrContributionPublicationNotReady) {
				t.Fatalf("claimed rejection: %v", err)
			}
		})
	}
}

func TestContributionPublicationLeaseAndTerminalReceipt(t *testing.T) {
	s := newTestRunner(t)
	value := publicationReady(t, s, strings.Repeat("a", 64))
	now := time.Now().UTC()
	first, err := s.ClaimContributionPublication(value.ID, "same-publisher", time.Second, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimContributionPublication(value.ID, "other-publisher", time.Minute, now); !errors.Is(err, ErrContributionPublicationLeaseLost) {
		t.Fatalf("double claim: %v", err)
	}
	if _, err := s.CompleteContributionPublication(value.ID, "same-publisher", first.Attempt, "https://github.com/example/repo/pull/7", 7, *first.LeaseUntil); !errors.Is(err, ErrContributionPublicationLeaseLost) {
		t.Fatalf("expired publish completed: %v", err)
	}
	second, err := s.ClaimContributionPublication(value.ID, "same-publisher", time.Minute, *first.LeaseUntil)
	if err != nil || second.Attempt != first.Attempt+1 {
		t.Fatalf("reclaim: %+v %v", second, err)
	}
	if _, err := s.CompleteContributionPublication(value.ID, "same-publisher", first.Attempt, "https://github.com/example/repo/pull/7", 7, *first.LeaseUntil); !errors.Is(err, ErrContributionPublicationLeaseLost) {
		t.Fatalf("old same-owner callback accepted: %v", err)
	}
	ready, err := s.CompleteContributionPublication(value.ID, "same-publisher", second.Attempt, "https://github.com/example/repo/pull/7", 7, *first.LeaseUntil)
	if err != nil || ready.State != "pr_ready" || ready.LeaseUntil != nil || ready.PRNumber != 7 {
		t.Fatalf("receipt: %+v %v", ready, err)
	}
	if _, err := s.CompleteContributionPublication(value.ID, "same-publisher", second.Attempt, ready.PRURL, ready.PRNumber, now.Add(time.Hour)); err != nil {
		t.Fatalf("receipt replay: %v", err)
	}
	if _, err := s.CompleteContributionPublication(value.ID, "same-publisher", second.Attempt, "https://github.com/example/repo/pull/8", 8, now); !errors.Is(err, ErrContributionPublicationConflict) {
		t.Fatalf("receipt overwritten: %v", err)
	}
	if _, err := s.ClaimContributionPublication(value.ID, "publisher", time.Minute, now.Add(time.Hour)); !errors.Is(err, ErrContributionPublicationNotReady) {
		t.Fatalf("terminal publication reclaimed: %v", err)
	}
	if _, err := s.CompleteContributionPublication(value.ID, "same-publisher", second.Attempt, "https://token@github.com/example/repo/pull/7", 7, now); !errors.Is(err, ErrContributionPublicationInvalid) {
		t.Fatalf("credential URL persisted: %v", err)
	}
}
