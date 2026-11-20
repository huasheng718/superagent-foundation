package repo

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"agentflow/internal/domain"
	mysqldriver "github.com/go-sql-driver/mysql"
)

func memoryProviderMySQLDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AGENTFLOW_MEMORY_PROVIDER_MYSQL_DSN")
	if dsn == "" {
		t.Skip("owned local provider MySQL schema not selected")
	}
	config, err := mysqldriver.ParseDSN(dsn)
	if err != nil || config == nil || os.Getenv("AGENTFLOW_MEMORY_PROVIDER_MYSQL_DISPOSABLE") != "1" || !strings.HasPrefix(config.DBName, "provider_acceptance_") || config.Net != "tcp" || !strings.HasPrefix(config.Addr, "127.0.0.1:") {
		t.Fatal("explicit owned loopback provider_acceptance schema required; DSN omitted")
	}
	return dsn
}
func TestMySQLMemoryProviderRestartHelper(t *testing.T) {
	if os.Getenv("AGENTFLOW_MEMORY_PROVIDER_HELPER") != "1" {
		t.Skip("fresh-process helper not selected")
	}
	store := budgetMySQLStore(t, memoryProviderMySQLDSN(t))
	defer store.Close()
	scope := domain.MemoryProviderScope{ProjectID: "memory-provider-cas", UserID: 1, AgentID: "project-ui"}
	binding := domain.MemoryProviderBinding{InstanceID: "local-provider", Generation: "one"}
	mirrors, err := store.MemoryProviderMirrors(scope, binding)
	if err != nil || len(mirrors) != 1 {
		t.Fatalf("fresh process lost durable intent: %v", err)
	}
	mirror := mirrors[0]
	if mirror.State != os.Getenv("AGENTFLOW_MEMORY_PROVIDER_HELPER_STATE") {
		t.Fatalf("unexpected durable state %s", mirror.State)
	}
	if mirror.State == "pending" {
		if _, created, err := store.ReserveMemoryMirror(mirror); err != nil || created {
			t.Fatalf("restart granted second append: created=%v err=%v", created, err)
		}
	}
	if id := os.Getenv("AGENTFLOW_MEMORY_PROVIDER_HELPER_RECEIPT"); id != "" {
		if _, err := store.MemoryRecallReceipt(scope.ProjectID, id, scope.UserID, true); err != nil {
			t.Fatal("fresh process lost immutable receipt")
		}
	}
}
func TestMySQLMemoryProviderIntentAcrossStoresAndFreshProcess(t *testing.T) {
	dsn := memoryProviderMySQLDSN(t)
	a, b := budgetMySQLStore(t, dsn), budgetMySQLStore(t, dsn)
	defer a.Close()
	defer b.Close()
	if !a.DB.Migrator().HasTable(&memoryProviderMirrorRow{}) || !a.DB.Migrator().HasTable(&memoryRecallReceiptRow{}) {
		t.Fatal("manual038 missing tables")
	}
	if err := a.PutProject(domain.Project{ID: "memory-provider-cas", Name: "Owned provider CAS", OwnerID: 1}); err != nil {
		t.Fatal(err)
	}
	memory, err := a.CreateMemoryCandidate("memory-provider-cas", domain.GovernedMemory{Title: "Reviewed", Content: "approved native authority", Source: domain.IntelligenceSource{Path: "docs/source.md"}}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	memory, err = a.DecideGovernedMemory(memory.ProjectID, memory.ID, memory.Revision, domain.MemoryApproved, "owner", "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	scope := domain.MemoryProviderScope{ProjectID: memory.ProjectID, UserID: 1, AgentID: "project-ui"}
	binding := domain.MemoryProviderBinding{InstanceID: "local-provider", Generation: "one"}
	id := domain.MemoryMirrorID(scope, binding, memory.ID, memory.Revision, memory.SHA256)
	intent := domain.MemoryProviderMirror{ID: id, Scope: scope, Binding: binding, MemoryID: memory.ID, Revision: memory.Revision, SHA256: memory.SHA256, SessionID: "lfm-" + id, State: "pending", Chunks: []domain.MemoryProviderChunk{{SHA256: "fixed-chunk", Size: 8}}}
	type outcome struct {
		created bool
		err     error
	}
	gate := make(chan struct{})
	results := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, store := range []*Store{a, b} {
		wait.Add(1)
		go func(store *Store) {
			defer wait.Done()
			<-gate
			_, created, err := store.ReserveMemoryMirror(intent)
			results <- outcome{created, err}
		}(store)
	}
	close(gate)
	wait.Wait()
	close(results)
	creators := 0
	for out := range results {
		if out.err != nil {
			t.Fatalf("concurrent reserve (%T)", out.err)
		}
		if out.created {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("add permissions=%d", creators)
	}
	child := func(state, receipt string) {
		t.Helper()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestMySQLMemoryProviderRestartHelper$", "-test.v")
		cmd.Env = append(os.Environ(), "AGENTFLOW_MEMORY_PROVIDER_HELPER=1", "AGENTFLOW_MEMORY_PROVIDER_HELPER_STATE="+state, "AGENTFLOW_MEMORY_PROVIDER_HELPER_RECEIPT="+receipt)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fresh process failed: %s", output)
		}
		t.Log(strings.TrimSpace(string(output)))
	}
	child("pending", "")
	if err := b.CompleteMemoryMirror(id, []domain.MemoryProviderChunk{{SHA256: "fixed-chunk", Size: 8, RecordID: "actual-provider-id", Version: "v1"}}); err != nil {
		t.Fatal(err)
	}
	receipt, err := a.SaveMemoryRecallReceipt(domain.MemoryRecallReceipt{Scope: scope, Binding: binding, Hits: []domain.MemoryRecallHit{{MemoryID: memory.ID, Revision: memory.Revision, SHA256: memory.SHA256, RecordIDs: []string{"actual-provider-id"}, RecordVersions: []string{"v1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	child("ready", receipt.ID)
	if _, err := a.DecideGovernedMemory(memory.ProjectID, memory.ID, memory.Revision, domain.MemoryRevoked, "owner", "race completion with revoke"); err != nil {
		t.Fatal(err)
	}
	if err := b.CompleteMemoryMirror(id, []domain.MemoryProviderChunk{{SHA256: "fixed-chunk", Size: 8, RecordID: "actual-provider-id", Version: "v1"}}); !errors.Is(err, ErrIntelligenceUnavailable) {
		t.Fatal("stale completion authorized revoked memory")
	}
	t.Log("manual038 tables, two independent pools one durable add permission, two fresh-process reads, exact receipt and revoked completion denial")
}
