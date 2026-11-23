package httpapi

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentflow/internal/auth"
	"agentflow/internal/domain"
)

func sourceEditRequest(t *testing.T, h http.Handler, user domain.User, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	var err error
	if value, ok := body.([]byte); ok {
		raw = value
	} else {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+req.Host)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	token, err := auth.NewClientFromEnv().SignUser(&auth.User{ID: user.ID, LoginName: user.LoginName, AuthTime: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
func sourceJSON(t *testing.T, rec *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d want %d: %s", rec.Code, status, rec.Body.String())
	}
	var value map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
	}
	return value
}

func TestCatalogSourceEditRequiresObservedBaseAcrossNativeDefinitions(t *testing.T) {
	h, _, users := catalogReadFixture(t)
	owner := users["owner"]
	for _, kind := range []string{"agents", "rules", "skills", "mcp-servers"} {
		t.Run(kind, func(t *testing.T) {
			path := "/api/" + kind
			value := map[string]any{"id": "edit-" + kind, "name": "Original", "projectId": "source-a"}
			if kind == "mcp-servers" {
				value["scope"] = "project"
				value["transport"] = "http"
				value["url"] = "https://example.invalid"
			}
			first := sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", path, value, map[string]string{"If-None-Match": "*"}), 200)
			base := first["sourceRevision"].(string)
			first["name"] = "Winner"
			missing := sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", path, first, nil), 428)
			if missing["code"] != "source_precondition_required" {
				t.Fatal(missing)
			}
			sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", path, first, map[string]string{"If-Match": "*"}), 400)
			sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", path, first, map[string]string{"If-None-Match": "*"}), 409)
			winner := sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", path, first, map[string]string{"If-Match": "\"" + base + "\""}), 200)
			if winner["sourceRevision"] == base || winner["name"] != "Winner" {
				t.Fatal(winner)
			}
			first["name"] = "Loser"
			stale := sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", path, first, map[string]string{"If-Match": "\"" + base + "\""}), 409)
			if stale["code"] != "source_edit_conflict" {
				t.Fatal(stale)
			}
			// Source CAS is independent of whether authentication is enabled.
			t.Setenv("AGENTFLOW_AUTH_REQUIRED", "false")
			sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", path, first, nil), 428)
		})
	}
}

func TestCatalogSourceFilesRejectMissingStaleAndSourcedMutations(t *testing.T) {
	h, s, users := catalogReadFixture(t)
	owner := users["owner"]
	skill := sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", "/api/skills", map[string]any{"id": "edit-files", "name": "Files", "projectId": "source-a"}, nil), 200)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("# Immutable input\n"))
	writer.Close()
	path := "/api/skills/edit-files/files"
	headers := map[string]string{"Content-Type": writer.FormDataContentType()}
	sourceJSON(t, sourceEditRequest(t, h, owner, "POST", path, body.Bytes(), headers), 428)
	headers["If-Match"] = "\"" + skill["sourceRevision"].(string) + "\""
	uploaded := sourceJSON(t, sourceEditRequest(t, h, owner, "POST", path, body.Bytes(), headers), 200)
	sourceJSON(t, sourceEditRequest(t, h, owner, "POST", path, body.Bytes(), headers), 409)
	sourceJSON(t, sourceEditRequest(t, h, owner, "DELETE", path+"/SKILL.md", nil, nil), 428)
	if current, _ := s.Skill("edit-files"); current.SourceRevision != uploaded["sourceRevision"] || len(current.FileRefs) != 1 {
		t.Fatal("failed mutation changed manifest")
	}
	sourceJSON(t, sourceEditRequest(t, h, users["outsider"], "POST", path, body.Bytes(), headers), 403)
	sourceJSON(t, sourceEditRequest(t, h, users["viewer"], "POST", path, body.Bytes(), headers), 403)
}

func TestCatalogSourceAgentRestoreHasCurrentBaseAndRetainsHistoricalIdentity(t *testing.T) {
	h, s, users := catalogReadFixture(t)
	owner := users["owner"]
	first := sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", "/api/agents", map[string]any{"id": "edit-restore", "name": "Agent", "prompt": "original", "version": 1, "projectId": "source-a"}, nil), 200)
	first["prompt"] = "changed"
	sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", "/api/agents", first, map[string]string{"If-Match": "\"" + first["sourceRevision"].(string) + "\""}), 200)
	current, _ := s.Agent("edit-restore")
	revisions := s.Revisions("agent", current.ID)
	if len(revisions) != 1 {
		t.Fatal(revisions)
	}
	path := "/api/agents/" + current.ID + "/revisions/" + revisions[0].ID + "/restore"
	sourceJSON(t, sourceEditRequest(t, h, owner, "POST", path, map[string]any{}, nil), 428)
	value := sourceJSON(t, sourceEditRequest(t, h, owner, "POST", path, map[string]any{}, map[string]string{"If-Match": "\"" + current.SourceRevision + "\""}), 200)
	if value["prompt"] != "original" || value["sourceRevision"] == first["sourceRevision"] {
		t.Fatal(value)
	}
	sourceJSON(t, sourceEditRequest(t, h, owner, "POST", path, map[string]any{}, map[string]string{"If-Match": "\"" + current.SourceRevision + "\""}), 409)
}

func TestCatalogSourceEvaluationRequiresReviewedTargetAndRejectsMixedBatch(t *testing.T) {
	h, s, users := catalogReadFixture(t)
	agent, err := s.SaveAgent(domain.AgentDefinition{ID: "cas-eval-agent", Name: "Agent", ProjectID: "source-a", Prompt: "original"})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: "cas-eval-run", Status: "succeeded", ExecutionContext: domain.TelemetryContext{ProjectID: "source-a"}, Nodes: map[string]*domain.NodeRun{}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	eval := domain.RunEvalRecord{ID: "cas-eval-record", RunID: run.ID, Status: domain.RunEvalSucceeded, Result: domain.RunEvalResult{Recommendations: []domain.RunEvalRecommendation{
		{ID: "agent-one", Target: "agent", TargetID: agent.ID, Action: "append_instructions", Text: "reviewed instruction", Status: domain.RunEvalRecPending},
		{ID: "agent-two", Target: "agent", TargetID: agent.ID, Action: "append_instructions", Text: "second instruction", Status: domain.RunEvalRecPending},
	}}}
	if err := s.PutRunEval(eval); err != nil {
		t.Fatal(err)
	}
	path := "/api/runs/" + run.ID + "/eval/apply"
	request := func(ids []string, bases map[string]string, status int) {
		t.Helper()
		sourceJSON(t, sourceEditRequest(t, h, users["owner"], "POST", path, map[string]any{"evalId": eval.ID, "recommendationIds": ids, "expectedAgentRevisions": bases}, nil), status)
	}
	request([]string{"agent-one"}, nil, 428)
	request([]string{"agent-one"}, map[string]string{agent.ID: "stale-review"}, 409)
	request([]string{"agent-one", "agent-two"}, map[string]string{agent.ID: agent.SourceRevision}, 400)
	unchanged, _ := s.Agent(agent.ID)
	if unchanged.Prompt != agent.Prompt || unchanged.SourceRevision != agent.SourceRevision || len(s.Revisions("agent", agent.ID)) != 0 {
		t.Fatal("rejected review mutated target")
	}
	request([]string{"agent-one"}, map[string]string{agent.ID: agent.SourceRevision}, 200)
	saved, _ := s.Agent(agent.ID)
	if saved.Prompt == agent.Prompt || saved.SourceRevision == agent.SourceRevision {
		t.Fatal("reviewed edit missing")
	}
	request([]string{"agent-two"}, map[string]string{agent.ID: agent.SourceRevision}, 409)
	retained, _ := s.Agent(agent.ID)
	if retained.SourceRevision != saved.SourceRevision || retained.Prompt != saved.Prompt {
		t.Fatal("stale second apply changed target")
	}
}

func TestCatalogSourceCloneCannotBindDeterministicForeignCollision(t *testing.T) {
	_, s, _ := catalogReadFixture(t)
	source := domain.MCPServerDefinition{ID: "cas-helper-source", Name: "Source", Scope: "project", ProjectID: "source-a", Transport: "http", URL: "https://example.invalid"}
	if err := s.PutMCPServer(source); err != nil {
		t.Fatal(err)
	}
	handler := &catalogHandlers{deps: &deps{repo: s}}
	ids, planned, err := handler.cloneAgentMCPServers([]string{source.ID}, "source-b", 1)
	if err != nil || len(planned) != 1 {
		t.Fatal("plan", err)
	}
	collision := planned[0]
	collision.ProjectID = "source-a"
	collision.Headers = map[string]string{"Authorization": "private-fixture"}
	if err := s.PutMCPServer(collision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := handler.cloneAgentMCPServers([]string{source.ID}, "source-b", 1); err == nil {
		t.Fatal("deterministic foreign credential binding reused")
	}
	retained, _ := s.MCPServer(ids[0])
	if retained.ProjectID != collision.ProjectID || retained.Headers["Authorization"] != "private-fixture" {
		t.Fatal("collision changed source")
	}
}
