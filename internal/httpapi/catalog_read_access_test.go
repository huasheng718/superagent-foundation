package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentflow/internal/auth"
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
)

func catalogReadFixture(t *testing.T) (http.Handler, *repo.Store, map[string]domain.User) {
	t.Helper()
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SESSION_SECRET", "source-read-fixture-secret")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	users := map[string]domain.User{}
	for _, name := range []string{"owner", "member", "viewer", "outsider", "admin"} {
		login := "source-" + name
		if err := s.PutUser(domain.User{LoginName: login}); err != nil {
			t.Fatal(err)
		}
		users[name], _ = s.UserByLoginName(login)
	}
	if _, err := s.GrantPlatformAdmin(users["admin"].LoginName); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProject(domain.Project{ID: "source-a", Name: "Source A", OwnerID: users["owner"].ID, Members: []domain.ProjectMember{{UserID: users["member"].ID, Role: "member"}, {UserID: users["viewer"].ID, Role: "viewer"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProject(domain.Project{ID: "source-b", Name: "Source B", OwnerID: users["outsider"].ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SeedTestGenericTemplates(s); err != nil {
		t.Fatal(err)
	}
	h := New(s, engine.New(s, runtime.NewManager(t.TempDir())), nil, "", "")
	return h, s, users
}
func catalogSourceResponse(t *testing.T, h http.Handler, user domain.User, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+req.Host)
	token, err := auth.NewClientFromEnv().SignUser(&auth.User{ID: user.ID, LoginName: user.LoginName, AuthTime: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
func TestCatalogSourceReadAndDownloadRequireProjectMembership(t *testing.T) {
	h, s, users := catalogReadFixture(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	agent := domain.AgentDefinition{ID: "private-agent", Name: "Private Agent", ProjectID: "source-a", Prompt: "source-private-prompt"}
	must(s.PutAgent(agent))
	agent, _ = s.Agent(agent.ID)
	agent.Prompt += " changed"
	must(s.PutAgent(agent))
	must(s.PutRule(domain.RuleDefinition{ID: "private-rule", Name: "Private Rule", ProjectID: "source-a", Content: "source-private-rule"}))
	must(s.PutSkill(domain.SkillDefinition{ID: "private-skill", Name: "Private Skill", ProjectID: "source-a", Content: "source-private-skill", Files: map[string]string{"references/info.md": "source-private-text"}}))
	observedSkill, _ := s.Skill("private-skill")
	must(s.PutSkillFiles("private-skill", []repo.SkillFileInput{{Path: "assets/data.bin", Content: []byte("source-private-binary"), Mode: 0644}}, observedSkill.SourceRevision))
	must(s.PutMCPServer(domain.MCPServerDefinition{ID: "private-mcp", Name: "Private MCP", ProjectID: "source-a", Scope: "project", Transport: "stdio", Command: "sh", Description: "source-private-mcp"}))
	observedMCP, _ := s.MCPServer("private-mcp")
	must(s.PutMCPFiles("private-mcp", []repo.SkillFileInput{{Path: "scripts/main.sh", Content: []byte("source-private-mcp-script"), Mode: 0755}}, observedMCP.SourceRevision))
	must(s.PutDockerfile(domain.DockerfileDefinition{ID: "private-dockerfile", Name: "Private Dockerfile", ProjectID: "source-a", Content: "FROM source-private-image"}))
	must(s.PutEnvironment(domain.EnvironmentDefinition{ID: "private-environment", Name: "Private Environment", ProjectID: "source-a", InstallScript: "echo source-private-environment"}))
	must(s.PutSandbox(domain.SandboxProfile{ID: "private-sandbox", Name: "Private Sandbox", ProjectID: "source-a", Driver: "local", Environment: map[string]string{"VALUE": "source-private-sandbox"}}))
	workflow := domain.WorkflowDefinition{ID: "private-workflow", Name: "Private Workflow", ProjectID: "source-a", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{{ID: "script", Name: "Script", Type: "script", ScriptLanguage: "shell", Script: "echo source-private-script"}}}
	must(s.PutWorkflow(workflow))
	workflow, _ = s.Workflow(workflow.ID)
	workflow.Description = "changed"
	must(s.PutWorkflow(workflow))
	agentRevision := s.Revisions("agent", agent.ID)[0]
	workflowRevision := s.Revisions("workflow", workflow.ID)[0]
	listPaths := []string{"/api/agents", "/api/rules", "/api/skills", "/api/mcp-servers", "/api/dockerfiles", "/api/environments", "/api/sandboxes", "/api/workflows", "/api/bootstrap"}
	for _, path := range listPaths {
		for _, suffix := range []string{"", "?projectId=source-a"} {
			rec := catalogSourceResponse(t, h, users["outsider"], http.MethodGet, path+suffix, nil)
			if rec.Code != 200 || strings.Contains(rec.Body.String(), "source-private-") || strings.Contains(rec.Body.String(), `"private-`) {
				t.Fatalf("private list/summary leak %s: %d %s", path+suffix, rec.Code, rec.Body.String())
			}
		}
	}
	detailPaths := []string{"/api/agents/private-agent", "/api/agents/private-agent/revisions", "/api/agents/private-agent/revisions/" + agentRevision.ID, "/api/workflows/private-workflow", "/api/workflows/private-workflow/revisions", "/api/workflows/private-workflow/revisions/" + workflowRevision.ID, "/api/skills/private-skill/files/assets/data.bin", "/api/mcp-servers/private-mcp/files/scripts/main.sh"}
	for _, path := range detailPaths {
		rec := catalogSourceResponse(t, h, users["outsider"], http.MethodGet, path, nil)
		if rec.Code != 403 || strings.Contains(rec.Body.String(), "source-private-") {
			t.Fatalf("private source downloaded: %s %d %s", path, rec.Code, rec.Body.String())
		}
		for _, actor := range []string{"owner", "member", "viewer", "admin"} {
			rec := catalogSourceResponse(t, h, users[actor], http.MethodGet, path, nil)
			if rec.Code != 200 {
				t.Fatalf("authorized %s read %s: %d %s", actor, path, rec.Code, rec.Body.String())
			}
		}
	}
	for _, path := range []string{"/api/agents/private-agent/clone", "/api/workflows/private-workflow/clone"} {
		rec := catalogSourceResponse(t, h, users["outsider"], http.MethodPost, path, map[string]any{"id": "stolen-copy", "name": "Copy", "projectId": "source-b"})
		if rec.Code != 403 {
			t.Fatalf("unreadable source cloned: %s %d", path, rec.Code)
		}
	}
	requestJSONAuthed[map[string]any](t, h, users["outsider"], http.MethodPost, "/api/projects/source-b/team-releases", domain.TeamReleaseRequest{Name: "private source denied", SkillIDs: []string{"private-skill"}}, http.StatusNotFound)
	// A readable current shared role must not expose its prior private revisions.
	agent, _ = s.Agent(agent.ID)
	agent.Shared = true
	must(s.PutAgent(agent))
	visible := requestJSONAuthed[[]domain.DefinitionRevision](t, h, users["outsider"], http.MethodGet, "/api/agents/private-agent/revisions", nil, http.StatusOK)
	if len(visible) != 0 {
		t.Fatal("sharing current agent retroactively exposed private history")
	}
	requestJSONAuthed[map[string]any](t, h, users["outsider"], http.MethodGet, "/api/agents/private-agent/revisions/"+agentRevision.ID, nil, http.StatusNotFound)
	// Restore must bind the revision to the exact route owner before mutation.
	own := domain.AgentDefinition{ID: "own-agent", Name: "Own", ProjectID: "source-b", Prompt: "keep own"}
	must(s.PutAgent(own))
	own, _ = s.Agent(own.ID)
	sourceJSON(t, sourceEditRequest(t, h, users["outsider"], http.MethodPost, "/api/agents/own-agent/revisions/"+agentRevision.ID+"/restore", map[string]any{}, map[string]string{"If-Match": "\"" + own.SourceRevision + "\""}), http.StatusNotFound)
	unchanged, _ := s.Agent(agent.ID)
	if !unchanged.Shared {
		t.Fatal("foreign revision restore modified protected source")
	}
	// Explicitly shared assets and public generic templates remain readable.
	must(s.PutRule(domain.RuleDefinition{ID: "shared-rule", Name: "Shared", ProjectID: "source-a", Shared: true, Content: "shared-policy"}))
	rules := requestJSONAuthed[[]domain.RuleDefinition](t, h, users["outsider"], http.MethodGet, "/api/rules", nil, http.StatusOK)
	sharedFound := false
	for _, rule := range rules {
		if rule.ID == "shared-rule" {
			sharedFound = true
		}
		if rule.ID == "private-rule" {
			t.Fatal("private source appeared beside shared resource")
		}
	}
	if !sharedFound {
		t.Fatal("explicit shared source disappeared")
	}
	requestJSONAuthed[domain.WorkflowDefinition](t, h, users["outsider"], http.MethodGet, "/api/workflows/"+repo.GenericBackendDeliveryWorkflowID, nil, http.StatusOK)
}

func TestFormalReleaseDoesNotGrantSourceWriteOrDropSkillSource(t *testing.T) {
	h, s, users := catalogReadFixture(t)
	skill := domain.SkillDefinition{ID: "source-locked-skill", Name: "Source Locked", ProjectID: "source-a", Shared: true, Content: "original source body", Source: &domain.SkillSource{Provider: "skillhub", Namespace: "upstream", Slug: "reviewed", Version: "1.2.3"}}
	if err := s.PutSkillSnapshot(skill, []repo.SkillFileInput{{Path: "SKILL.md", Content: []byte("---\nname: source-locked-skill\n---\nUpstream reviewed body\n"), Mode: 0644}}, true); err != nil {
		t.Fatal(err)
	}
	release := requestJSONAuthed[domain.TeamRelease](t, h, users["outsider"], http.MethodPost, "/api/projects/source-b/team-releases", domain.TeamReleaseRequest{Name: "Shared reviewed version", SkillIDs: []string{skill.ID}}, http.StatusCreated)
	if release.Payload.Assets.Skills[0].Source == nil || release.Payload.Assets.Skills[0].Source.Version != "1.2.3" {
		t.Fatal("publication stripped upstream provenance")
	}
	rec := catalogSourceResponse(t, h, users["outsider"], http.MethodGet, "/api/skills/"+skill.ID+"/files/SKILL.md", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Upstream reviewed body") {
		t.Fatal("explicit sharing lost readable source")
	}
	// Retaining an authorized historical release does not keep a live-source
	// sharing grant open after its owner withdraws that grant.
	privateSource, _ := s.Skill(skill.ID)
	privateSource.Shared = false
	if err := s.PutSkill(privateSource); err != nil {
		t.Fatal(err)
	}
	requestJSONAuthed[map[string]any](t, h, users["outsider"], http.MethodGet, "/api/skills/"+skill.ID+"/files/SKILL.md", nil, http.StatusForbidden)
	retained := requestJSONAuthed[domain.TeamRelease](t, h, users["outsider"], http.MethodGet, "/api/projects/source-b/team-releases/"+release.ID, nil, http.StatusOK)
	if retained.SHA256 != release.SHA256 {
		t.Fatal("source sharing change mutated retained release")
	}
	observedPrivate, _ := s.Skill(skill.ID)
	sourceJSON(t, sourceEditRequest(t, h, users["outsider"], http.MethodPut, "/api/skills", map[string]any{"id": skill.ID, "name": "Hijacked", "projectId": "source-a", "content": "hijacked", "source": nil}, map[string]string{"If-Match": "\"" + observedPrivate.SourceRevision + "\""}), http.StatusForbidden)
	sourceJSON(t, sourceEditRequest(t, h, users["owner"], http.MethodDelete, "/api/skills/"+skill.ID+"/files/SKILL.md", nil, map[string]string{"If-Match": "\"" + observedPrivate.SourceRevision + "\""}), http.StatusConflict)
	sourceJSON(t, sourceEditRequest(t, h, users["owner"], http.MethodPost, "/api/skills/"+skill.ID+"/files", nil, map[string]string{"If-Match": "\"" + observedPrivate.SourceRevision + "\""}), http.StatusConflict)
	sourceJSON(t, sourceEditRequest(t, h, users["owner"], http.MethodPut, "/api/skills", map[string]any{"id": skill.ID, "name": "Rename only", "projectId": "source-a", "shared": true, "content": "hijacked", "files": map[string]string{"SKILL.md": "hijacked"}, "source": nil}, map[string]string{"If-Match": "\"" + observedPrivate.SourceRevision + "\""}), http.StatusOK)
	stored, _ := s.Skill(skill.ID)
	if stored.Content != skill.Content || stored.Source == nil || stored.Source.Version != "1.2.3" || len(stored.FileRefs) != 1 {
		t.Fatal("metadata edit removed source/file ownership")
	}
}

func TestSharedAgentCopyDoesNotPublishPrivateClosureOrTransferCredentials(t *testing.T) {
	h, s, users := catalogReadFixture(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutRule(domain.RuleDefinition{ID: "private-closure-rule", Name: "Private", ProjectID: "source-a", Content: "private closure"}))
	role := domain.AgentDefinition{ID: "shared-closure-agent", Name: "Shared role", ProjectID: "source-a", Shared: true, Rules: []string{"private-closure-rule"}}
	must(s.PutAgent(role))
	clone := func(id string, want int) domain.AgentDefinition {
		return requestJSONAuthed[domain.AgentDefinition](t, h, users["outsider"], http.MethodPost, "/api/agents/"+role.ID+"/clone", map[string]any{"id": id, "name": id, "projectId": "source-b"}, want)
	}
	clone("denied-rule-copy", http.StatusForbidden)
	unchanged, _ := s.Rule("private-closure-rule")
	if unchanged.Shared {
		t.Fatal("failed clone published private closure")
	}
	if _, found := s.Agent("denied-rule-copy"); found {
		t.Fatal("failed clone created an active role")
	}
	source := domain.MCPServerDefinition{ID: "copy-source-mcp", Name: "Private connection", ProjectID: "source-a", Scope: "project", Transport: "http", URL: "https://example.test/mcp", Environment: map[string]string{"API_KEY": "fixture-source-value"}, MarketID: "independent-market"}
	must(s.PutMCPServer(source))
	role, _ = s.Agent(role.ID)
	role.Rules = nil
	role.MCPServers = []string{source.ID}
	must(s.PutAgent(role))
	before := len(s.MCPServers())
	clone("denied-mcp-copy", http.StatusForbidden)
	if len(s.MCPServers()) != before {
		t.Fatal("denied closure clone created MCP state")
	}
	personal := domain.MCPServerDefinition{ID: "copy-personal-mcp", Name: "Personal connection", Scope: "personal", OwnerUserID: users["owner"].ID, Transport: "http", URL: "https://example.test/private"}
	must(s.PutMCPServer(personal))
	role, _ = s.Agent(role.ID)
	role.MCPServers = []string{personal.ID}
	must(s.PutAgent(role))
	clone("denied-personal-copy", http.StatusForbidden)
	// Read access is still not permission to transfer a connection secret into
	// another execution scope. A source viewer cannot promote that binding.
	project, _ := s.Project("source-a")
	project.Members = append(project.Members, domain.ProjectMember{UserID: users["outsider"].ID, Role: "viewer"})
	must(s.PutProject(project))
	role, _ = s.Agent(role.ID)
	role.MCPServers = []string{source.ID}
	must(s.PutAgent(role))
	clone("denied-credential-copy", http.StatusConflict)
	if _, found := s.Agent("denied-credential-copy"); found {
		t.Fatal("credential conflict created an active role")
	}
	// Independently configured target market bindings are reusable without
	// reading or copying the source credentials, even after source access ends.
	project.Members = project.Members[:len(project.Members)-1]
	must(s.PutProject(project))
	target := source
	target.ID = "target-independent-mcp"
	target.ProjectID = "source-b"
	target.Environment = map[string]string{"API_KEY": "fixture-target-value"}
	must(s.PutMCPServer(target))
	copied := clone("allowed-independent-binding", http.StatusCreated)
	if len(copied.MCPServers) != 1 || copied.MCPServers[0] != target.ID {
		t.Fatal("copy did not use independently authorized target binding")
	}
	retained, _ := s.MCPServer(target.ID)
	if retained.Environment["API_KEY"] != "fixture-target-value" {
		t.Fatal("source credential replaced target binding")
	}
}
