package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func providerRepoFixture(t *testing.T) (*Store, domain.GovernedMemory, domain.MemoryProviderMirror) {
	t.Helper()
	s := intelligenceStore(t)
	m, err := s.CreateMemoryCandidate("team-a", domain.GovernedMemory{Title: "Reviewed", Content: "native authority", Source: domain.IntelligenceSource{Path: "docs/source.md"}}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	m, err = s.DecideGovernedMemory("team-a", m.ID, m.Revision, domain.MemoryApproved, "owner", "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	scope := domain.MemoryProviderScope{ProjectID: "team-a", UserID: 1, AgentID: "project-ui"}
	binding := domain.MemoryProviderBinding{InstanceID: "local", Generation: "one"}
	id := domain.MemoryMirrorID(scope, binding, m.ID, m.Revision, m.SHA256)
	return s, m, domain.MemoryProviderMirror{ID: id, Scope: scope, Binding: binding, MemoryID: m.ID, Revision: m.Revision, SHA256: m.SHA256, SessionID: "lfm-" + id, State: "pending", Chunks: []domain.MemoryProviderChunk{{SHA256: "approved-chunk-hash", Size: 16}}}
}
func TestMemoryMirrorDurableReservationCASAndRevocation(t *testing.T) {
	s, m, intent := providerRepoFixture(t)
	first, created, err := s.ReserveMemoryMirror(intent)
	if err != nil || !created || first.IntentAt.IsZero() {
		t.Fatalf("reserve %+v %v %v", first, created, err)
	}
	again, created, err := s.ReserveMemoryMirror(intent)
	if err != nil || created || !again.IntentAt.Equal(first.IntentAt) {
		t.Fatal("existing intent granted a second append")
	}
	// Another Store models a second instance using the same SQL database.
	other := newStore(s.DB)
	if _, created, err := other.ReserveMemoryMirror(intent); err != nil || created {
		t.Fatalf("second instance created=%v err=%v", created, err)
	}
	pins := []domain.MemoryProviderChunk{{SHA256: "approved-chunk-hash", Size: 16, RecordID: "msg-owned", Version: "v1"}}
	if err := s.CompleteMemoryMirror(intent.ID, pins); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideGovernedMemory("team-a", m.ID, m.Revision, domain.MemoryRevoked, "owner", "revoked"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteMemoryMirror(intent.ID, pins); !errors.Is(err, ErrIntelligenceUnavailable) {
		t.Fatalf("late completion after revoke: %v", err)
	}
}
func TestMemoryReceiptSelectionTTLAndFrozenRead(t *testing.T) {
	s, m, _ := providerRepoFixture(t)
	receipt, err := s.SaveMemoryRecallReceipt(domain.MemoryRecallReceipt{Scope: domain.MemoryProviderScope{ProjectID: "team-a", UserID: 1, AgentID: "project-ui"}, Hits: []domain.MemoryRecallHit{{MemoryID: m.ID, Revision: m.Revision, SHA256: m.SHA256, RecordIDs: []string{"msg-owned"}, RecordVersions: []string{"v1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	receipt.ExpiresAt = time.Now().Add(-time.Minute)
	receipt.SHA256, _ = receipt.Digest()
	raw, _ := json.Marshal(receipt)
	if err := s.DB.Model(&memoryRecallReceiptRow{}).Where("id = ?", receipt.ID).Update("payload", raw).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.MemoryRecallReceipt("team-a", receipt.ID, 1, true); !errors.Is(err, ErrIntelligenceUnavailable) {
		t.Fatalf("expired selection: %v", err)
	}
	if _, err := s.MemoryRecallReceipt("team-a", receipt.ID, 1, false); err != nil {
		t.Fatalf("frozen replay selected latest: %v", err)
	}
	if _, err := s.MemoryRecallReceipt("team-a", receipt.ID, 2, false); err == nil {
		t.Fatal("wrong user")
	}
	if _, err := s.MemoryRecallReceipt("team-b", receipt.ID, 1, false); err == nil {
		t.Fatal("wrong project")
	}
	receipt.QuerySHA256 = "tampered"
	raw, _ = json.Marshal(receipt)
	s.DB.Model(&memoryRecallReceiptRow{}).Where("id = ?", receipt.ID).Update("payload", raw)
	if _, err := s.MemoryRecallReceipt("team-a", receipt.ID, 1, false); !errors.Is(err, ErrIntelligenceConflict) {
		t.Fatalf("tamper: %v", err)
	}
}
func TestProviderEligibleSetRejectsOverflowAndCandidate(t *testing.T) {
	s, m, _ := providerRepoFixture(t)
	raw, _ := json.Marshal(m)
	rows := make([]governedMemoryRow, 501)
	for i := range rows {
		rows[i] = governedMemoryRow{ID: fmt.Sprintf("overflow-%03d", i), ProjectID: "team-b", State: domain.MemoryApproved, Revision: 2, Payload: raw, Created: time.Now()}
	}
	if err := s.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.EligibleProviderMemories("team-b"); !errors.Is(err, ErrMemoryIndexCapacity) {
		t.Fatalf("silently truncated approved set: %v", err)
	}
	candidate, err := s.CreateMemoryCandidate("team-a", domain.GovernedMemory{Title: "Candidate", Content: "not approved", Source: domain.IntelligenceSource{Path: "docs/source.md"}}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := s.EligibleProviderMemories("team-a")
	if err != nil || len(eligible) != 1 || eligible[0].ID == candidate.ID {
		t.Fatalf("candidate indexed: %+v %v", eligible, err)
	}
}

func TestMemoryReceiptClaimRequiresExplicitRunnerVersion(t *testing.T) {
	store, runner, _, now := newRegistryPlacementFixture(t, false)
	runner.RunnerVersion = domain.RunnerVersionGovernedContext
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if err := store.PutProject(domain.Project{ID: "project", OwnerID: 42}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRule(domain.RuleDefinition{ID: "receipt-rule", Name: "Rule", ProjectID: "project", Content: "reviewed"}); err != nil {
		t.Fatal(err)
	}
	release, err := store.CreateTeamRelease("project", domain.TeamReleaseRequest{Name: "Receipt", RuleIDs: []string{"receipt-rule"}}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := store.SaveMemoryRecallReceipt(domain.MemoryRecallReceipt{Scope: domain.MemoryProviderScope{ProjectID: "project", UserID: 42, AgentID: "project-ui"}, Hits: []domain.MemoryRecallHit{}})
	if err != nil {
		t.Fatal(err)
	}
	task := domain.RunnerTask{ID: "receipt-claim", RunID: "run", NodeID: "node", RunnerID: runner.ID, Provider: "terminal", Command: "true", Invocation: &domain.RunnerInvocation{RunID: "run", NodeID: "node", GovernedContext: &domain.RunnerGovernedContext{Version: 1, ProjectID: "project", ReleaseID: release.ID, ReleaseSHA256: release.SHA256, IntelligenceSHA256: strings.Repeat("1", 64), MemoryRecallReceipt: &receipt}}}
	if err := store.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"terminal"}, now, time.Minute); err == nil || found || !strings.Contains(err.Error(), "0.13.0") {
		t.Fatalf("old receipt runner claim found=%v err=%v", found, err)
	}
	stored, _ := store.RunnerTask(task.ID)
	if stored.Attempt != 0 || stored.Status != domain.RunnerTaskQueued {
		t.Fatal("version rejection mutated assignment")
	}
	runner.RunnerVersion = domain.RunnerVersionMemoryRecallReceipt
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ClaimRunnerTaskForSession(runner.ID, runner.SessionTokenHash, []string{"terminal"}, now, time.Minute); err != nil || !found {
		t.Fatalf("new receipt runner claim found=%v err=%v", found, err)
	}
}
