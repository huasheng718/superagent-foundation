package memoryprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

type fakeProvider struct {
	mu           sync.Mutex
	rows         []record
	adds         int
	uncertain    bool
	late         []record
	forged       bool
	corrupt      bool
	unhealthy    bool
	deleteFails  bool
	beforeSearch func()
}

func (p *fakeProvider) handler(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer server-only-secret" {
		w.WriteHeader(401)
		return
	}
	if r.URL.Path == "/health" {
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "0.1.0", "stores": map[string]bool{"vectorStore": true, "ftsAvailable": !p.unhealthy, "embeddingService": false}})
		return
	}
	var body map[string]json.RawMessage
	json.NewDecoder(r.Body).Decode(&body)
	str := func(k string) string { var v string; json.Unmarshal(body[k], &v); return v }
	match := func(row record) bool {
		return row.TeamID == str("team_id") && row.UserID == str("user_id") && row.AgentID == str("agent_id") && (str("session_id") == "" || row.SessionID == str("session_id"))
	}
	var data any
	switch r.URL.Path {
	case "/v3/conversation/add":
		p.adds++
		var messages []struct {
			Content string `json:"content"`
		}
		json.Unmarshal(body["messages"], &messages)
		ids := []string{}
		versions := []string{}
		rows := []record{}
		for i, message := range messages {
			id := "msg-" + strconv.Itoa(p.adds) + "-" + strconv.Itoa(i)
			ids = append(ids, id)
			versions = append(versions, "v1")
			rows = append(rows, record{ID: id, Role: "assistant", Content: message.Content, TeamID: str("team_id"), UserID: str("user_id"), AgentID: str("agent_id"), SessionID: str("session_id"), Score: 1})
		}
		if p.uncertain {
			p.late = rows
			w.WriteHeader(500)
			return
		}
		p.rows = append(p.rows, rows...)
		data = map[string]any{"accepted_ids": ids, "accepted_versions": versions, "total_count": len(ids)}
	case "/v3/conversation/query":
		rows := []record{}
		for _, row := range p.rows {
			if match(row) {
				rows = append(rows, row)
			}
		}
		offset := 0
		json.Unmarshal(body["offset"], &offset)
		total := len(rows)
		if offset > total {
			offset = total
		}
		rows = rows[offset:]
		if len(rows) > 100 {
			rows = rows[:100]
		}
		data = map[string]any{"messages": rows, "total": total}
	case "/v3/conversation/search":
		if p.beforeSearch != nil {
			p.beforeSearch()
			p.beforeSearch = nil
		}
		rows := []record{}
		for _, row := range p.rows {
			if match(row) {
				if p.corrupt {
					row.Content = "forged content"
				}
				rows = append(rows, row)
			}
		}
		if p.forged {
			rows = append(rows, record{ID: "foreign-id", Role: "assistant", Content: "private foreign text", Score: 5})
		}
		data = map[string]any{"messages": rows}
	case "/v3/conversation/delete":
		if p.deleteFails {
			w.WriteHeader(500)
			return
		}
		var sessions []string
		json.Unmarshal(body["session_ids"], &sessions)
		kept := []record{}
		count := 0
		for _, row := range p.rows {
			remove := false
			for _, session := range sessions {
				if match(row) && row.SessionID == session {
					remove = true
				}
			}
			if remove {
				count++
			} else {
				kept = append(kept, row)
			}
		}
		p.rows = kept
		data = map[string]int{"deleted_count": count}
	default:
		w.WriteHeader(404)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"code": 0, "request_id": "request-test", "data": data})
}
func serviceFixture(t *testing.T) (Service, *repo.Store, *fakeProvider, domain.MemoryProviderScope, domain.GovernedMemory) {
	t.Helper()
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.PutProject(domain.Project{ID: "project", Name: "Project", OwnerID: 1}); err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateMemoryCandidate("project", domain.GovernedMemory{Title: "Release", Content: "Reviewed deployment rollback alpha", Source: domain.IntelligenceSource{Path: "docs/rollback.md"}}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	m, err = store.DecideGovernedMemory("project", m.ID, m.Revision, domain.MemoryApproved, "owner", "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	client := &Client{baseURL: server.URL, token: "server-only-secret", serviceID: "default", Binding: domain.MemoryProviderBinding{InstanceID: "owned-pilot", Generation: "generation-1", SourceCommit: SourceCommit, PackageVersion: "1.0.2-beta.1", HealthVersion: "0.1.0", IndexSchema: "sqlite-fts-v2"}, http: server.Client()}
	return Service{Client: client, Memory: store.MemoryRepo}, store, fake, domain.MemoryProviderScope{ProjectID: "project", UserID: 1, AgentID: "project-ui"}, m
}
func TestMirrorUncertainAddNeverRepeatsAfterRestartAndRecoversLate(t *testing.T) {
	service, store, fake, scope, m := serviceFixture(t)
	ctx := context.Background()
	fake.uncertain = true
	first, err := service.Sync(ctx, scope, 10)
	if err != nil || first.Complete || first.Pending != 1 || fake.adds != 1 {
		t.Fatalf("first=%+v adds=%d err=%v", first, fake.adds, err)
	}
	// A new service instance uses only durable native SQL intent, no process cache.
	service = Service{Client: service.Client, Memory: store.MemoryRepo}
	second, err := service.Sync(ctx, scope, 10)
	if err != nil || second.Complete || fake.adds != 1 {
		t.Fatalf("empty discovery appended: %+v %v", second, err)
	}
	fake.mu.Lock()
	fake.rows = append(fake.rows, fake.late...)
	fake.late = nil
	fake.mu.Unlock()
	recovered, err := service.Sync(ctx, scope, 10)
	if err != nil || !recovered.Complete || fake.adds != 1 {
		t.Fatalf("recovery=%+v err=%v", recovered, err)
	}
	items, receipt, err := service.Recall(ctx, scope, "rollback", 5)
	if err != nil || len(items) != 1 || items[0].ID != m.ID || receipt.SHA256 == "" {
		t.Fatalf("recall=%+v receipt=%+v err=%v", items, receipt, err)
	}
	if _, err := store.MemoryRecallReceipt(scope.ProjectID, receipt.ID, 2, true); err == nil {
		t.Fatal("other user read receipt")
	}
	if err := receipt.ValidatePins(items); err != nil {
		t.Fatal(err)
	}
	other := scope
	other.UserID = 2
	if _, _, err := service.Recall(ctx, other, "rollback", 5); err != ErrSyncRequired {
		t.Fatalf("other scope: %v", err)
	}
}
func TestProviderIntegrityAndMissingIndexFailClosed(t *testing.T) {
	service, _, fake, scope, _ := serviceFixture(t)
	ctx := context.Background()
	if out, err := service.Sync(ctx, scope, 10); err != nil || !out.Complete {
		t.Fatalf("sync %+v %v", out, err)
	}
	fake.forged = true
	if _, _, err := service.Recall(ctx, scope, "rollback", 5); err != ErrIntegrity {
		t.Fatalf("foreign id: %v", err)
	}
	fake.forged = false
	fake.corrupt = true
	if _, _, err := service.Recall(ctx, scope, "rollback", 5); err != ErrIntegrity {
		t.Fatalf("corrupt text: %v", err)
	}
	fake.corrupt = false
	fake.rows = nil
	if _, _, err := service.Recall(ctx, scope, "rollback", 5); err != ErrSyncRequired {
		t.Fatalf("lost data directory: %v", err)
	}
	fake.unhealthy = true
	if _, _, err := service.Recall(ctx, scope, "rollback", 5); err != ErrUnavailable {
		t.Fatalf("missing FTS: %v", err)
	}
}
func TestRevocationDuringSearchAndFailedDeletionCannotAuthorize(t *testing.T) {
	service, store, fake, scope, m := serviceFixture(t)
	ctx := context.Background()
	if _, err := service.Sync(ctx, scope, 10); err != nil {
		t.Fatal(err)
	}
	fake.beforeSearch = func() {
		if _, err := store.DecideGovernedMemory("project", m.ID, m.Revision, domain.MemoryRevoked, "owner", "revoked during search"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := service.Recall(ctx, scope, "rollback", 5); err != ErrSyncRequired {
		t.Fatalf("revocation: %v", err)
	}
	fake.deleteFails = true
	out, err := service.Sync(ctx, scope, 10)
	if err != nil || out.Failed != 1 || out.Complete {
		t.Fatalf("delete failure %+v %v", out, err)
	}
	items, _, err := service.Recall(ctx, scope, "rollback", 5)
	if err != nil || len(items) != 0 {
		t.Fatalf("revoked returned: %+v %v", items, err)
	}
	fake.deleteFails = false
	out, err = service.Sync(ctx, scope, 10)
	if err != nil || !out.Complete || len(fake.rows) != 0 {
		t.Fatalf("cleanup %+v %v", out, err)
	}
}
func TestUTF8ChunkingIsExactAndBounded(t *testing.T) {
	text := ""
	for i := 0; i < 4000; i++ {
		text += "知识"
	}
	text = text[:16000] // intentionally invalid UTF8 is rejected.
	if _, err := chunks(domain.GovernedMemory{Title: "中文", Content: text}); err == nil {
		t.Fatal("invalid UTF8 accepted")
	}
	valid := ""
	for i := 0; i < 2000; i++ {
		valid += "知识"
	}
	parts, err := chunks(domain.GovernedMemory{Title: "中文", Content: valid})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, p := range parts {
		if len(p) > 7000 {
			t.Fatal("chunk overflow")
		}
		joined += p
	}
	if joined != "中文\n"+valid {
		t.Fatal("chunk bytes changed")
	}
}

func TestRevokedUnknownAddRetainsCleanupUntilGenerationRetirement(t *testing.T) {
	service, store, fake, scope, m := serviceFixture(t)
	ctx := context.Background()
	fake.uncertain = true
	if _, err := service.Sync(ctx, scope, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecideGovernedMemory(scope.ProjectID, m.ID, m.Revision, domain.MemoryRevoked, "owner", "revoke uncertain add"); err != nil {
		t.Fatal(err)
	}
	out, err := service.Sync(ctx, scope, 10)
	if err != nil || out.Complete || out.CleanupPending != 1 {
		t.Fatalf("empty delete falsely final: %+v %v", out, err)
	}
	fake.mu.Lock()
	fake.rows = append(fake.rows, fake.late...)
	fake.mu.Unlock()
	out, err = service.Sync(ctx, scope, 10)
	if err != nil || out.Complete || out.CleanupPending != 1 || len(fake.rows) != 0 || fake.adds != 1 {
		t.Fatalf("late add cleanup %+v adds=%d %v", out, fake.adds, err)
	}
}
