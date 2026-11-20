package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func gcBlob(t *testing.T, s *Store, content string, created time.Time) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	if err := s.PutRunnerContributionBlob(digest, []byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&runnerContributionBlobRow{}).Where("sha256 = ?", digest).Update("created_at", created).Error; err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestContributionBlobReferencesProtectRunnerAndDeliveryInputs(t *testing.T) {
	s := newTestRunner(t)
	cutoff := time.Now().UTC().Add(-time.Hour)
	orphan := gcBlob(t, s, "orphan", cutoff.Add(-time.Minute))
	runnerRef := gcBlob(t, s, "runner-reference", cutoff.Add(-time.Minute))
	runnerReceiptRef := gcBlob(t, s, "runner-receipt-reference", cutoff.Add(-time.Minute))
	deliveryRef := gcBlob(t, s, "delivery-reference", cutoff.Add(-time.Minute))

	task := domain.RunnerTask{ID: "gc-reference-task", Provider: "terminal", Invocation: &domain.RunnerInvocation{
		ContributionInputs: []domain.RunnerContributionInput{{RepositoryID: "repo", BundleSHA256: runnerRef, BundleSize: int64(len("runner-reference"))}},
	}, PendingContributions: []domain.RunnerContributionReceipt{{SHA256: runnerReceiptRef, Size: int64(len("runner-receipt-reference"))}}}
	now := time.Now().UTC()
	if err := s.DB.Create(&runnerTaskRow{
		ID: task.ID, Provider: task.Provider, Status: domain.RunnerTaskQueued,
		MaxAttempts: 1, Payload: runnerTaskPayloadBytes(task), Created: &now, Updated: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	integrationID := strings.Repeat("a", 64)
	if _, err := s.PrepareContributionIntegration(domain.ContributionIntegration{
		ID: integrationID, ManifestSHA256: integrationID, RunID: "gc-run", RepositoryID: "repo",
		BaseSHA: strings.Repeat("1", 40), ResultSHA: strings.Repeat("2", 40), ContextSHA256: strings.Repeat("c", 64),
		BundleSHA256: deliveryRef, BundleSize: int64(len("delivery-reference")),
		Inputs: []domain.RunnerContributionReceipt{{SHA256: deliveryRef, Size: int64(len("delivery-reference"))}}, State: "prepared",
	}); err != nil {
		t.Fatal(err)
	}
	runnerRefs, err := s.RunnerContributionBlobReferences()
	if err != nil {
		t.Fatal(err)
	}
	deliveryRefs, err := s.DeliveryContributionBlobReferences()
	if err != nil {
		t.Fatal(err)
	}
	references := append(append([]string{}, runnerRefs...), deliveryRefs...)
	sort.Strings(references)
	runnerSet := map[string]bool{}
	for _, ref := range runnerRefs {
		runnerSet[ref] = true
	}
	if len(runnerRefs) != 2 || !runnerSet[runnerRef] || !runnerSet[runnerReceiptRef] || len(deliveryRefs) != 1 || deliveryRefs[0] != deliveryRef {
		t.Fatalf("references runner=%v delivery=%v", runnerRefs, deliveryRefs)
	}
	deleted, err := s.DeleteUnreferencedContributionBlobs(references, cutoff)
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if _, _, ok := s.RunnerContributionBlob(orphan); ok {
		t.Fatal("orphan blob was retained")
	}
	for _, digest := range []string{runnerRef, runnerReceiptRef, deliveryRef} {
		if _, _, ok := s.RunnerContributionBlob(digest); !ok {
			t.Fatalf("referenced blob %s was deleted", digest)
		}
	}
}

func TestDeleteUnreferencedContributionBlobsRequiresCutoff(t *testing.T) {
	s := newTestRunner(t)
	if _, err := s.DeleteUnreferencedContributionBlobs(nil, time.Time{}); err == nil {
		t.Fatal("missing cutoff accepted")
	}
}

func TestPrepareContributionIntegrationRequiresDurableBlobMetadata(t *testing.T) {
	s := newTestRunner(t)
	integration := domain.ContributionIntegration{
		ID:             strings.Repeat("a", 64),
		ManifestSHA256: strings.Repeat("a", 64),
		RunID:          "gc-run",
		RepositoryID:   "repo",
		BaseSHA:        strings.Repeat("1", 40),
		ResultSHA:      strings.Repeat("2", 40),
		ContextSHA256:  strings.Repeat("c", 64),
		BundleSHA256:   strings.Repeat("b", 64),
		BundleSize:     7,
		Inputs:         []domain.RunnerContributionReceipt{{SHA256: strings.Repeat("b", 64), Size: 7}},
		State:          "prepared",
	}
	if _, err := s.PrepareContributionIntegration(integration); err == nil {
		t.Fatal("missing durable bundle accepted")
	}
	raw := []byte("durable")
	sum := sha256.Sum256(raw)
	integration.BundleSHA256 = hex.EncodeToString(sum[:])
	integration.Inputs[0].SHA256 = integration.BundleSHA256
	if err := s.PutRunnerContributionBlob(integration.BundleSHA256, raw); err != nil {
		t.Fatal(err)
	}
	integration.BundleSize++
	integration.Inputs[0].Size = integration.BundleSize
	if _, err := s.PrepareContributionIntegration(integration); err == nil {
		t.Fatal("size-mismatched durable bundle accepted")
	}
}

func TestContributionBlobReferencesFailClosedOnMalformedRunnerTaskPayload(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	if err := s.DB.Create(&runnerTaskRow{
		ID: "gc-malformed-task", Provider: "terminal", Status: domain.RunnerTaskQueued,
		MaxAttempts: 1, Payload: []byte("{"), Created: &now, Updated: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunnerContributionBlobReferences(); err == nil {
		t.Fatal("malformed runner payload was treated as an empty reference set")
	}
}
