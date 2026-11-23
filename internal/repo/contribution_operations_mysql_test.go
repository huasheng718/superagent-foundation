package repo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	mysqldriver "github.com/go-sql-driver/mysql"
)

// The wrapper runs each phase in a fresh process, with a dump/restore between
// seed and verify. Collection is explicitly quiesced, never a live GC daemon.
func TestMySQLContributionBlobBackupRestoreAndQuiescedGC(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_OPERATIONS_MYSQL_DSN")
	if dsn == "" {
		t.Skip("disposable operations MySQL DSN not supplied")
	}
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil || cfg == nil || os.Getenv("AGENTFLOW_OPERATIONS_MYSQL_DISPOSABLE") != "1" || !strings.HasPrefix(cfg.DBName, "operations_acceptance_") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("explicit loopback disposable operations schema required; DSN omitted")
	}
	s := budgetMySQLStore(t, dsn)
	defer s.Close()
	content := func(kind string) []byte {
		if kind == "accepted" {
			return bytes.Repeat([]byte("0123456789abcdef"), 1<<20) // exact 16 MiB cap
		}
		return []byte("operations-normal-fixture-" + kind)
	}
	digest := func(kind string) string {
		sum := sha256.Sum256(content(kind))
		return hex.EncodeToString(sum[:])
	}
	check := func(kind string, present bool) {
		t.Helper()
		raw, size, found := s.RunnerContributionBlob(digest(kind))
		if found != present || (present && (size != int64(len(content(kind))) || !bytes.Equal(raw, content(kind)))) {
			t.Fatalf("blob %s presence/size/bytes mismatch", kind)
		}
	}
	phase := os.Getenv("AGENTFLOW_OPERATIONS_MYSQL_PHASE")
	if phase == "seed" {
		var packet int64
		if err := s.DB.Raw("SELECT @@session.max_allowed_packet").Scan(&packet).Error; err != nil {
			t.Fatal(err)
		}
		if packet <= domain.RunnerContributionMaxBytes {
			t.Fatal("disposable MySQL max_allowed_packet must exceed the 16 MiB application cap")
		}
		t.Logf("session max_allowed_packet=%d; exact-cap blob=%d", packet, domain.RunnerContributionMaxBytes)
		now := time.Now().UTC()
		for _, kind := range []string{"accepted", "staged", "input", "integration", "orphan", "fresh"} {
			if err := s.PutRunnerContributionBlob(digest(kind), content(kind)); err != nil {
				t.Fatal(err)
			}
			if kind != "fresh" {
				if err := s.DB.Model(&runnerContributionBlobRow{}).Where("sha256 = ?", digest(kind)).Update("created_at", now.Add(-48*time.Hour)).Error; err != nil {
					t.Fatal(err)
				}
			}
			check(kind, true)
		}
		for _, kind := range []string{"accepted", "staged"} {
			task := contributionTask(t, s, "operations-"+kind, "operations-runner", 1, now)
			spec := task.ContributionSpecs[0]
			receipt := domain.RunnerContributionReceipt{TaskID: task.ID, Attempt: task.Attempt, RepositoryID: spec.RepositoryID, Dir: spec.Dir, BaseSHA: spec.BaseSHA, ResultSHA: strings.Repeat("2", 40), ContextSHA256: spec.ContextSHA256, SHA256: digest(kind), Size: int64(len(content(kind)))}
			staged, err := s.StageRunnerContribution(receipt, "operations-runner", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if kind == "accepted" {
				if err := s.CompleteRunnerTask(task.ID, "operations-runner", task.Attempt, domain.RunnerTaskSuccess, nil, "normal fixture complete", "", nil, &domain.RunnerTaskOutputs{ContributionReceipts: []string{staged.ID}}, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
		}
		// Storage fixture only: the placed-execution queue is separately tested.
		// Here we verify backup and retention of an already durable input payload.
		input := domain.RunnerTask{ID: "operations-input", Provider: "terminal", RunID: "operations-input-run", MaxAttempts: 1, Invocation: &domain.RunnerInvocation{ContributionInputs: []domain.RunnerContributionInput{{RepositoryID: "repo", BundleSHA256: digest("input"), BundleSize: int64(len(content("input")))}}}}
		if err := s.DB.Create(&runnerTaskRow{ID: input.ID, Provider: input.Provider, Status: domain.RunnerTaskQueued, MaxAttempts: 1, Payload: runnerTaskPayloadBytes(input), Created: &now, Updated: &now}).Error; err != nil {
			t.Fatal(err)
		}
		id := strings.Repeat("f", 64)
		if _, err := s.PrepareContributionIntegration(domain.ContributionIntegration{ID: id, ManifestSHA256: id, RunID: "operations-controller", RepositoryID: "repo", BaseSHA: strings.Repeat("1", 40), ResultSHA: strings.Repeat("2", 40), ContextSHA256: strings.Repeat("a", 64), BundleSHA256: digest("integration"), BundleSize: int64(len(content("integration"))), Inputs: []domain.RunnerContributionReceipt{{SHA256: digest("accepted"), Size: int64(len(content("accepted")))}}, State: "prepared"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PrepareContributionPublication(id, "main", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		t.Log("seeded exact-cap accepted, staged, frozen input, prepared integration, old orphan and fresh unreferenced bytes")
		return
	}
	if phase != "restored" && phase != "collected" {
		t.Fatal("explicit seed/restored/collected phase required")
	}
	for _, kind := range []string{"accepted", "staged", "input", "integration", "fresh"} {
		check(kind, true)
	}
	accepted, ok := s.RunnerTask("operations-accepted")
	if !ok || accepted.Status != domain.RunnerTaskSuccess || len(accepted.AcceptedContributions) != 1 || accepted.AcceptedContributions[0].SHA256 != digest("accepted") {
		t.Fatal("accepted task/receipt history not restored")
	}
	staged, ok := s.RunnerTask("operations-staged")
	if !ok || len(staged.PendingContributions) != 1 || len(staged.AcceptedContributions) != 0 {
		t.Fatal("staged task history not restored")
	}
	if integration, found := s.ContributionIntegration(strings.Repeat("f", 64)); !found || integration.BundleSHA256 != digest("integration") || integration.Inputs[0].SHA256 != digest("accepted") {
		t.Fatal("prepared integration history not restored")
	}
	if publication, found := s.ContributionPublication(strings.Repeat("f", 64)); !found || publication.State != "gating" || publication.ResultSHA != strings.Repeat("2", 40) {
		t.Fatal("gating publication history not restored")
	}
	if phase == "collected" {
		check("orphan", false)
		t.Log("fresh process retained accepted/staged/input/integration/fresh bytes and audited receipts after collection")
		return
	}
	check("orphan", true)
	runnerRefs, err := s.RunnerContributionBlobReferences()
	if err != nil {
		t.Fatal(err)
	}
	deliveryRefs, err := s.DeliveryContributionBlobReferences()
	if err != nil {
		t.Fatal(err)
	}
	refs := append(runnerRefs, deliveryRefs...)
	keep := map[string]bool{}
	for _, ref := range refs {
		keep[ref] = true
	}
	for _, kind := range []string{"accepted", "staged", "input", "integration"} {
		if !keep[digest(kind)] {
			t.Fatalf("missing durable reference for %s", kind)
		}
	}
	deleted, err := s.DeleteUnreferencedContributionBlobs(refs, time.Now().UTC().Add(-24*time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("quiesced collection deleted=%d err=%v", deleted, err)
	}
	check("orphan", false)
	t.Log("restored exact bytes and histories; quiesced 24h-cutoff GC removed only the old orphan (test cutoff, not production policy)")
}
