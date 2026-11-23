package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

func TestIntelligenceGovernanceRequiresProjectHumanAuthority(t *testing.T) {
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SESSION_SECRET", "governed-memory-fixture-secret")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	users := map[string]domain.User{}
	for _, name := range []string{"gov-owner", "gov-member", "gov-outsider"} {
		if err := s.PutUser(domain.User{LoginName: name}); err != nil {
			t.Fatal(err)
		}
		users[name], _ = s.UserByLoginName(name)
	}
	owner, member, outsider := users["gov-owner"], users["gov-member"], users["gov-outsider"]
	for _, p := range []domain.Project{{ID: "governance-a", Name: "A", OwnerID: owner.ID, Members: []domain.ProjectMember{{UserID: member.ID, Role: "member"}}}, {ID: "governance-b", Name: "B", OwnerID: outsider.ID}} {
		if err := s.PutProject(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutRule(domain.RuleDefinition{ID: "gov-rule", Name: "Rule", ProjectID: "governance-a", Content: "exact reviewed rule"}); err != nil {
		t.Fatal(err)
	}
	h := New(s, engine.New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
	base := "/api/projects/governance-a"
	for _, path := range []string{"/team-releases", "/memories", "/default-upgrades"} {
		requestJSONAuthed[map[string]any](t, h, outsider, http.MethodGet, base+path, nil, http.StatusForbidden)
	}
	requestJSONAuthed[[]domain.DefaultUpgradeDiff](t, h, member, http.MethodGet, base+"/default-upgrades", nil, http.StatusOK)
	releaseInput := domain.TeamReleaseRequest{Name: "Release 1", RuleIDs: []string{"gov-rule"}}
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base+"/team-releases", releaseInput, http.StatusForbidden)
	release := requestJSONAuthed[domain.TeamRelease](t, h, owner, http.MethodPost, base+"/team-releases", releaseInput, http.StatusCreated)
	list := requestJSONAuthed[[]map[string]any](t, h, member, http.MethodGet, base+"/team-releases", nil, http.StatusOK)
	if len(list) != 1 || list[0]["id"] != release.ID || list[0]["payload"] != nil {
		t.Fatal("release list not scoped bounded metadata")
	}
	conflict := requestJSONAuthed[map[string]any](t, h, owner, http.MethodPost, base+"/team-releases", releaseInput, http.StatusConflict)
	if conflict["code"] != "intelligence_conflict" {
		t.Fatal("publish conflict lost stable refresh code")
	}
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base+"/team-releases/"+release.ID+"/restore", map[string]any{"name": "restore", "reason": "reviewed", "expectedBaseVersion": 1}, http.StatusForbidden)
	conflict = requestJSONAuthed[map[string]any](t, h, owner, http.MethodPost, base+"/team-releases/"+release.ID+"/restore", map[string]any{"name": "restore", "reason": "reviewed", "expectedBaseVersion": 0}, http.StatusConflict)
	if conflict["code"] != "intelligence_conflict" {
		t.Fatal("restore conflict lost stable refresh code")
	}
	restored := requestJSONAuthed[domain.TeamRelease](t, h, owner, http.MethodPost, base+"/team-releases/"+release.ID+"/restore", map[string]any{"name": "restore", "reason": "reviewed", "expectedBaseVersion": 1}, http.StatusCreated)
	if restored.Version != 2 || restored.ID == release.ID || restored.SHA256 != release.SHA256 {
		t.Fatal("HTTP restore did not publish exact new history")
	}
	// Content route checks the same project, not only possession of a release ID.
	requestJSONAuthed[map[string]any](t, h, outsider, http.MethodGet, "/api/projects/governance-b/team-releases/"+release.ID, nil, http.StatusNotFound)
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base+"/memories", map[string]any{"title": "Money", "content": "integer currency units", "source": map[string]string{"path": "docs/money.md"}, "state": "approved", "revision": 99, "createdBy": "forged-admin"}, http.StatusBadRequest)
	memory := requestJSONAuthed[domain.GovernedMemory](t, h, member, http.MethodPost, base+"/memories", map[string]any{"title": "Money", "content": "integer currency units", "source": map[string]string{"path": "docs/money.md"}}, http.StatusCreated)
	if memory.State != domain.MemoryCandidate || memory.Revision != 1 || memory.CreatedBy != "gov-member" {
		t.Fatal("caller forged memory governance fields")
	}
	recalled := requestJSONAuthed[[]domain.GovernedMemory](t, h, member, http.MethodPost, base+"/memory-recall", map[string]any{"query": "currency"}, http.StatusOK)
	if len(recalled) != 0 {
		t.Fatal("unreviewed candidate recalled")
	}
	decision := map[string]any{"decision": "approved", "expectedRevision": 1, "reason": "checked source"}
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base+"/memories/"+memory.ID+"/decision", decision, http.StatusForbidden)
	approved := requestJSONAuthed[domain.GovernedMemory](t, h, owner, http.MethodPost, base+"/memories/"+memory.ID+"/decision", decision, http.StatusOK)
	requestJSONAuthed[map[string]any](t, h, owner, http.MethodPost, base+"/memories/"+memory.ID+"/decision", decision, http.StatusConflict)
	snapshot := requestJSONAuthed[domain.ExecutionIntelligenceContext](t, h, member, http.MethodPost, base+"/context/resolve", map[string]any{"releaseId": release.ID, "memoryIds": []string{memory.ID}}, http.StatusOK)
	if snapshot.ProjectID != "governance-a" || len(snapshot.Memories) != 1 || snapshot.Memories[0].SHA256 != approved.SHA256 {
		t.Fatal("context did not pin approved scoped memory")
	}
	// Scope comes from the authenticated project route, never a supplied scope
	// or identity field, even when all other fields form a valid request.
	for _, field := range []string{"projectId", "teamId", "userId", "agentId", "sessionId"} {
		for _, input := range []struct {
			path string
			body map[string]any
		}{
			{"/memories", map[string]any{"title": "Forged", "content": "must not be saved"}},
			{"/memory-recall", map[string]any{"query": "currency"}},
			{"/memories/" + memory.ID + "/decision", map[string]any{"decision": "revoked", "expectedRevision": 2, "reason": "must not apply"}},
			{"/context/resolve", map[string]any{"releaseId": release.ID, "memoryIds": []string{memory.ID}}},
		} {
			input.body[field] = "governance-b"
			requestJSONAuthed[map[string]any](t, h, owner, http.MethodPost, base+input.path, input.body, http.StatusBadRequest)
		}
	}
	requestJSONAuthed[map[string]any](t, h, owner, http.MethodPost, base+"/memories", map[string]any{"title": "Forged nested scope", "content": "must not be saved", "source": map[string]any{"path": "docs/money.md", "projectId": "governance-b"}}, http.StatusBadRequest)
	memories := requestJSONAuthed[[]domain.GovernedMemory](t, h, owner, http.MethodGet, base+"/memories", nil, http.StatusOK)
	if len(memories) != 1 || memories[0].ID != approved.ID || memories[0].State != domain.MemoryApproved || memories[0].Revision != approved.Revision {
		t.Fatal("rejected scope claims mutated governed memory")
	}
	foreignRun := domain.Run{ID: "foreign-memory-source", WorkflowID: "foreign-workflow", Status: "succeeded", CreatedAt: time.Now().UTC(), ExecutionContext: domain.TelemetryContext{ProjectID: "governance-b"}, Nodes: map[string]*domain.NodeRun{}}
	if err := s.PutRun(foreignRun); err != nil {
		t.Fatal(err)
	}
	requestJSONAuthed[map[string]any](t, h, owner, http.MethodPost, base+"/memories", map[string]any{"title": "Foreign source", "content": "must not enter this project", "source": map[string]string{"runId": foreignRun.ID}}, http.StatusNotFound)
	memories = requestJSONAuthed[[]domain.GovernedMemory](t, h, owner, http.MethodGet, base+"/memories", nil, http.StatusOK)
	if len(memories) != 1 || memories[0].ID != approved.ID {
		t.Fatal("foreign source reference created memory")
	}
	if err := s.PutIssue(domain.Issue{ID: "governed-issue", ProjectID: "governance-a", Title: "Governed issue"}); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: "governed-read", WorkflowID: "governed-workflow", Status: "succeeded", CreatedAt: time.Now().UTC(),
		WorkflowSnapshot: domain.WorkflowDefinition{ID: "governed-workflow", Name: "Governed workflow", ProjectID: "governance-a"},
		ExecutionContext: domain.TelemetryContext{ProjectID: "governance-a", IssueID: "governed-issue", TeamContext: &domain.TeamContextSelection{ReleaseID: release.ID, MemoryIDs: []string{approved.ID}}}, Intelligence: &snapshot, Nodes: map[string]*domain.NodeRun{}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/stream", "/telemetry", "/logs", "/workspace", "/workspace/tree", "/workspace/file?path=.agentflow/config/agent-system.md", "/artifacts/private", "/preview", "/browser/stream"} {
		requestJSONAuthed[map[string]any](t, h, outsider, http.MethodGet, "/api/runs/"+run.ID+suffix, nil, http.StatusForbidden)
	}
	for _, actor := range []domain.User{owner, member} {
		detail := requestJSONAuthed[domain.Run](t, h, actor, http.MethodGet, "/api/runs/"+run.ID, nil, http.StatusOK)
		if detail.Intelligence == nil || len(detail.Intelligence.Memories) != 1 || detail.Intelligence.Memories[0].Content != approved.Content {
			t.Fatal("authorized project reader lost governed detail")
		}
	}
	// Summary projections are shared with unscoped list and issue progress
	// endpoints. They must never carry the normalized memory/resource snapshot.
	for _, path := range []string{"/api/runs", "/api/issues/governed-issue/runs", "/api/issues/governed-issue/requirement", "/api/bootstrap"} {
		body := requestJSONAuthed[any](t, h, outsider, http.MethodGet, path, nil, http.StatusOK)
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), `"intelligence"`) || strings.Contains(string(raw), approved.Content) {
			t.Fatalf("%s leaked governed snapshot: %s", path, raw)
		}
	}
	projected, _ := json.Marshal(publicRun(run, true))
	if strings.Contains(string(projected), `"intelligence"`) || strings.Contains(string(projected), approved.Content) {
		t.Fatal("direct summary projection leaked normalized intelligence")
	}
	_, runnerSession := registerTestRunner(t, h)
	response := requestWithHeaders(t, h, http.MethodGet, "/api/runs/"+run.ID, nil, map[string]string{"Authorization": "Bearer " + runnerSession})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("non-human Runner token read governed detail: %d", response.Code)
	}
	requestJSONAuthed[domain.GovernedMemory](t, h, owner, http.MethodPost, base+"/memories/"+memory.ID+"/decision", map[string]any{"decision": "revoked", "expectedRevision": 2, "reason": "source outdated"}, http.StatusOK)
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base+"/context/resolve", map[string]any{"releaseId": release.ID, "memoryIds": []string{memory.ID}}, http.StatusNotFound)
	requestJSONAuthed[domain.TeamRelease](t, h, owner, http.MethodPost, base+"/team-releases/"+release.ID+"/revoke", map[string]any{"expectedRevision": 1, "reason": "rollback"}, http.StatusOK)
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base+"/context/resolve", map[string]any{"releaseId": release.ID}, http.StatusNotFound)
}
