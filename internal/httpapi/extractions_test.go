package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKnowledgeExtractionHTTPAuthenticatedFixedSourceAndReceiptBoundary(t *testing.T) {
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SESSION_SECRET", "extraction-http-fixture-only")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	users := map[string]domain.User{}
	for _, name := range []string{"extract-owner", "extract-member", "extract-outsider"} {
		if err := s.PutUser(domain.User{LoginName: name}); err != nil {
			t.Fatal(err)
		}
		users[name], _ = s.UserByLoginName(name)
	}
	owner, member, outsider := users["extract-owner"], users["extract-member"], users["extract-outsider"]
	for _, p := range []domain.Project{{ID: "extract-a", OwnerID: owner.ID, Members: []domain.ProjectMember{{UserID: member.ID}}}, {ID: "extract-b", OwnerID: outsider.ID}} {
		if err := s.PutProject(p); err != nil {
			t.Fatal(err)
		}
	}
	h := New(s, engine.New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
	base := "/api/projects/extract-a/knowledge-extractions"
	in := domain.KnowledgeExtractionRequest{RunID: "not-in-project", NodeID: "extract", ArtifactID: "fixed-artifact", RepositoryID: "fixed-repo", ExpectedCommit: strings.Repeat("1", 40), ExpectedArtifactSHA256: strings.Repeat("a", 64)}
	requestJSON[map[string]any](t, h, http.MethodPost, base, in, http.StatusUnauthorized)
	requestJSONAuthed[map[string]any](t, h, outsider, http.MethodPost, base, in, http.StatusForbidden)
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base, in, http.StatusNotFound)
	raw, _ := json.Marshal(in)
	var forged map[string]any
	_ = json.Unmarshal(raw, &forged)
	for _, key := range []string{"actor", "projectId", "model", "content", "state", "extractionId", "local", "userId"} {
		forged[key] = "forged"
		response := requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base, forged, http.StatusBadRequest)
		if response["code"] != "extraction_invalid" {
			t.Fatal("invalid request lacks stable code")
		}
		delete(forged, key)
	}
	// Retained receipt reads do not depend on still-existing Run/workspace data.
	source := domain.KnowledgeExtractionSource{RunID: "historical-run", NodeID: "extract", TaskID: "fixed-task", ArtifactID: "fixed-artifact", ArtifactSHA256: strings.Repeat("a", 64), RepositoryID: "r", Commit: strings.Repeat("1", 40)}
	receipt := domain.KnowledgeExtractionReceipt{ProjectID: "extract-a", Source: source, SchemaVersion: 1, Policy: "v1-unrestricted-inputs-only", CreatedBy: member.LoginName, CreatedAt: time.Now().UTC(), Items: []domain.KnowledgeExtractionItem{}}
	receipt.ID = domain.KnowledgeExtractionID(receipt.ProjectID, source)
	receipt.SHA256, err = receipt.Digest()
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(receipt)
	if err := s.DB.Table("knowledge_extractions").Create(map[string]any{"id": receipt.ID, "project_id": receipt.ProjectID, "payload": payload, "created_at": receipt.CreatedAt}).Error; err != nil {
		t.Fatal(err)
	}
	requestJSON[map[string]any](t, h, http.MethodGet, base+"/"+receipt.ID, nil, http.StatusUnauthorized)
	requestJSONAuthed[map[string]any](t, h, outsider, http.MethodGet, base+"/"+receipt.ID, nil, http.StatusForbidden)
	requestJSONAuthed[map[string]any](t, h, outsider, http.MethodGet, "/api/projects/extract-b/knowledge-extractions/"+receipt.ID, nil, http.StatusNotFound)
	got := requestJSONAuthed[domain.KnowledgeExtractionReceipt](t, h, member, http.MethodGet, base+"/"+receipt.ID, nil, http.StatusOK)
	if got.SHA256 != receipt.SHA256 {
		t.Fatal("retained provenance changed")
	}
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, "/api/projects/extract-a/memories", map[string]any{"title": "Forged", "content": "x", "source": map[string]any{"path": "x", "extractionId": receipt.ID}}, http.StatusBadRequest)
}
