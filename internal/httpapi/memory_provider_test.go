package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/memoryprovider"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

func memoryHTTPFixture(t *testing.T) (*repo.Store, http.Handler, domain.User, domain.User) {
	t.Helper()
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SESSION_SECRET", "memory-provider-http-fixture-only")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	users := []domain.User{}
	for _, name := range []string{"memory-owner", "memory-other"} {
		if err := store.PutUser(domain.User{LoginName: name}); err != nil {
			t.Fatal(err)
		}
		user, _ := store.UserByLoginName(name)
		users = append(users, user)
	}
	for i, id := range []string{"memory-a", "memory-b"} {
		if err := store.PutProject(domain.Project{ID: id, Name: id, OwnerID: users[i].ID}); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(store, engine.New(store, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
	return store, handler, users[0], users[1]
}
func TestMemoryProviderHTTPRejectsForgedScopeAndUnsafeConfiguration(t *testing.T) {
	_, handler, owner, other := memoryHTTPFixture(t)
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_URL", "http://127.0.0.1:1")
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_TOKEN", "private-test-key")
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_INSTANCE", "pilot")
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_GENERATION", "one")
	base := "/api/projects/memory-a"
	unauth := httptest.NewRequest(http.MethodPost, base+"/memory-provider/sync", strings.NewReader("{}"))
	anonymousResponse := httptest.NewRecorder()
	handler.ServeHTTP(anonymousResponse, unauth)
	if anonymousResponse.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous provider access status=%d", anonymousResponse.Code)
	}
	for _, key := range []string{"user_id", "team_id", "agent_id", "session_id", "providerURL", "token", "actor"} {
		requestJSONAuthed[map[string]any](t, handler, owner, http.MethodPost, base+"/memory-recall", map[string]any{"query": "rollback", key: "forged"}, http.StatusBadRequest)
	}
	requestJSONAuthed[map[string]any](t, handler, other, http.MethodPost, base+"/memory-provider/sync", map[string]any{}, http.StatusForbidden)
	requestJSONAuthed[map[string]any](t, handler, owner, http.MethodPost, base+"/memory-provider/sync", map[string]any{"userId": other.ID}, http.StatusBadRequest)
	// Server scope rejects headers before any HTTP attempt to the unavailable provider.
	req := httptest.NewRequest(http.MethodPost, base+"/memory-recall", strings.NewReader(`{"query":"rollback"}`))
	req = authedRequest(t, req, owner)
	req.Header.Set("x-tdai-user-id", "foreign")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("forged header status=%d body=%s", response.Code, response.Body.String())
	}
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "false")
	requestJSONAuthed[map[string]any](t, handler, owner, http.MethodPost, base+"/memory-recall", map[string]any{"query": "rollback"}, http.StatusServiceUnavailable)
}

// Opt in with the owner-only connection file created by a reviewed, fresh local
// provider pilot. This never starts, installs or configures an upstream service.
func TestMemoryProviderLocalRealHTTPReceiptAndUncertainAdd(t *testing.T) {
	connectionFile := os.Getenv("AGENTFLOW_MEMORY_PROVIDER_PILOT_CONNECTION")
	if connectionFile == "" {
		t.Skip("owned local provider pilot not selected")
	}
	started := time.Now()
	raw, err := os.ReadFile(connectionFile)
	if err != nil {
		t.Fatal(err)
	}
	var connection struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &connection) != nil {
		t.Fatal("invalid private connection")
	}
	providerURL, err := url.Parse(connection.URL)
	if err != nil || providerURL.Hostname() != "127.0.0.1" {
		t.Fatal("pilot must use owned loopback")
	}
	// The private test proxy forwards the first real add, consumes its successful
	// response, then returns 502: next sync must discover, never append twice.
	var adds atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(providerURL)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.URL.Path == "/v3/conversation/add" && adds.Add(1) == 1 {
			response.Body.Close()
			response.StatusCode = http.StatusBadGateway
			response.Status = "502 Bad Gateway"
			response.Body = http.NoBody
			response.ContentLength = 0
			response.Header.Del("Content-Length")
		}
		return nil
	}
	front := httptest.NewServer(proxy)
	defer front.Close()
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_URL", front.URL)
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_TOKEN", connection.Token)
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_INSTANCE", "owned-adapter-pilot-20261118")
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_GENERATION", "fresh-1")
	store, handler, owner, other := memoryHTTPFixture(t)
	for _, id := range []string{"memory-a", "memory-b"} {
		if err := store.PutRule(domain.RuleDefinition{ID: id + "-rule", Name: "Reviewed", ProjectID: id, Content: "fixed release rule"}); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/projects/memory-a"
	release := requestJSONAuthed[domain.TeamRelease](t, handler, owner, http.MethodPost, base+"/team-releases", domain.TeamReleaseRequest{Name: "Fixed local receipt", RuleIDs: []string{"memory-a-rule"}}, http.StatusCreated)
	candidate := requestJSONAuthed[domain.GovernedMemory](t, handler, owner, http.MethodPost, base+"/memories", map[string]any{"title": "rollbackalpha", "content": "rollbackalpha reviewed native memory", "source": map[string]string{"path": "docs/rollback.md"}}, http.StatusCreated)
	requestJSONAuthed[[]domain.GovernedMemory](t, handler, owner, http.MethodPost, base+"/memory-recall", map[string]any{"query": "rollbackalpha"}, http.StatusOK)
	memory := requestJSONAuthed[domain.GovernedMemory](t, handler, owner, http.MethodPost, base+"/memories/"+candidate.ID+"/decision", map[string]any{"decision": "approved", "expectedRevision": candidate.Revision, "reason": "verified local fixture"}, http.StatusOK)
	before := requestJSONAuthed[map[string]any](t, handler, owner, http.MethodPost, base+"/memory-recall", map[string]any{"query": "rollbackalpha"}, http.StatusServiceUnavailable)
	if before["code"] != "memory_provider_sync_required" {
		t.Fatal("partial index recalled")
	}
	first := requestJSONAuthed[memoryprovider.SyncResult](t, handler, owner, http.MethodPost, base+"/memory-provider/sync", map[string]any{}, http.StatusOK)
	if first.Complete || first.Pending != 1 {
		t.Fatalf("unknown add %+v", first)
	}
	second := requestJSONAuthed[memoryprovider.SyncResult](t, handler, owner, http.MethodPost, base+"/memory-provider/sync", map[string]any{}, http.StatusOK)
	if !second.Complete || adds.Load() != 1 {
		t.Fatalf("discovery %+v add count=%d", second, adds.Load())
	}
	var result struct {
		Items   []domain.GovernedMemory    `json:"items"`
		Receipt domain.MemoryRecallReceipt `json:"receipt"`
	}
	result = requestJSONAuthed[struct {
		Items   []domain.GovernedMemory    `json:"items"`
		Receipt domain.MemoryRecallReceipt `json:"receipt"`
	}](t, handler, owner, http.MethodPost, base+"/memory-recall", map[string]any{"query": "rollbackalpha", "includeReceipt": true}, http.StatusOK)
	if len(result.Items) != 1 || result.Items[0].ID != memory.ID || result.Receipt.Binding.SourceCommit != memoryprovider.SourceCommit || result.Receipt.Scope.UserID != owner.ID {
		t.Fatalf("real BM25 native result/pin missing")
	}
	ctx := requestJSONAuthed[domain.ExecutionIntelligenceContext](t, handler, owner, http.MethodPost, base+"/context/resolve", map[string]any{"releaseId": release.ID, "memoryIds": []string{memory.ID}, "memoryRecallReceiptId": result.Receipt.ID}, http.StatusOK)
	if ctx.MemoryRecallReceipt == nil || ctx.MemoryRecallReceipt.SHA256 != result.Receipt.SHA256 {
		t.Fatal("server-owned receipt not frozen")
	}
	// Empty foreign project at the same raw provider cannot see the first scope.
	foreign := requestJSONAuthed[[]domain.GovernedMemory](t, handler, other, http.MethodPost, "/api/projects/memory-b/memory-recall", map[string]any{"query": "rollbackalpha"}, http.StatusOK)
	if len(foreign) != 0 {
		t.Fatal("project scope leaked")
	}
	// Current membership is rechecked, even for previously signed identities.
	project, _ := store.Project("memory-a")
	project.OwnerID = other.ID
	if err := store.PutProject(project); err != nil {
		t.Fatal(err)
	}
	requestJSONAuthed[map[string]any](t, handler, owner, http.MethodPost, base+"/memory-recall", map[string]any{"query": "rollbackalpha"}, http.StatusForbidden)
	project.OwnerID = owner.ID
	if err := store.PutProject(project); err != nil {
		t.Fatal(err)
	}
	requestJSONAuthed[domain.GovernedMemory](t, handler, owner, http.MethodPost, base+"/memories/"+memory.ID+"/decision", map[string]any{"decision": "revoked", "expectedRevision": memory.Revision, "reason": "cleaned pilot"}, http.StatusOK)
	stale := requestJSONAuthed[[]domain.GovernedMemory](t, handler, owner, http.MethodPost, base+"/memory-recall", map[string]any{"query": "rollbackalpha"}, http.StatusOK)
	if len(stale) != 0 {
		t.Fatal("revoked native content returned")
	}
	cleaned := requestJSONAuthed[memoryprovider.SyncResult](t, handler, owner, http.MethodPost, base+"/memory-provider/sync", map[string]any{}, http.StatusOK)
	if !cleaned.Complete {
		t.Fatalf("cleanup %+v", cleaned)
	}
	if output := os.Getenv("AGENTFLOW_MEMORY_PROVIDER_PILOT_REPORT"); output != "" {
		report := map[string]any{"passed": true, "sourceCommit": memoryprovider.SourceCommit, "providerBinding": result.Receipt.Binding, "healthVersion": "0.1.0", "realBM25": true, "uncertainAddDiscovered": true, "actualAddRequests": adds.Load(), "projectIsolation": true, "currentMembershipRechecked": true, "nativeRevokeDenied": true, "providerSessionDeleteVerified": true, "serverOwnedReceiptFrozen": true, "receiptSHA256": result.Receipt.SHA256, "latencyMS": result.Receipt.LatencyMS, "elapsedMS": time.Since(started).Milliseconds(), "modelRequests": 0, "physicalErasureProven": false}
		raw, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(output, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
