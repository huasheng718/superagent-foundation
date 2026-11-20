package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/auth"
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/teamconfig"
	"agentflow/internal/tracker"
	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestTeamImportReviewedRealHTTPFlow(t *testing.T) {
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	teamImportHTTPFlow(t, s, s)
}

func TestMySQLTeamImportReviewedRealHTTPFlow(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEAM_IMPORT_MYSQL_DSN")
	if dsn == "" {
		t.Skip("disposable TeamAI Git import MySQL DSN not supplied")
	}
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil || cfg == nil || os.Getenv("AGENTFLOW_TEAM_IMPORT_MYSQL_DISPOSABLE") != "1" || !strings.HasPrefix(cfg.DBName, "team_import_acceptance_") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("explicit loopback disposable team_import_acceptance_ schema required; DSN omitted")
	}
	a, err := repo.Open(dsn)
	if err != nil {
		t.Fatal("disposable MySQL store open failed; DSN omitted")
	}
	t.Cleanup(a.Close)
	b, err := repo.Open(dsn)
	if err != nil {
		t.Fatal("independent disposable MySQL store open failed; DSN omitted")
	}
	t.Cleanup(b.Close)
	teamImportHTTPFlow(t, a, b)
}

func teamImportFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "registered-source")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for p, content := range map[string]string{
		"teamai.yaml":                   "team: Fixture\nrepo: https://example.test/fixture.git\n",
		"agents/reviewer.yaml":          "name: reviewer\ndescription: Review selected changes\ninstructions: Check exact source.\n",
		"rules/style.md":                "Reviewed original rule.\n",
		"skills/check/SKILL.md":         "---\nname: check\ndescription: Check selected files\n---\nRead scripts/check.sh.\n",
		"skills/check/scripts/check.sh": "#!/bin/sh\nexit 99\n",
		"skills/check/assets/blob.bin":  "\x00\xff\x01",
		"mcp/mcp.yaml":                  "servers:\n- name: format\n  transport: stdio\n  command: fixture-never-invoked\n  timeout: 30000\n",
		"LICENSE":                       "MIT synthetic fixture\n",
		"unselected/keep.txt":           "Unselected source remains unchanged.\n",
	} {
		local := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(local), 0700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0600)
		if strings.HasSuffix(p, ".sh") {
			mode = 0700
		}
		if err := os.WriteFile(local, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	teamImportGit(t, dir, "init", "--template=", ".")
	teamImportGit(t, dir, "add", "--all")
	teamImportGit(t, dir, "commit", "-m", "Synthetic supported TeamAI source")
	return root, dir, teamImportGit(t, dir, "rev-parse", "HEAD")
}

func teamImportGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "user.name=TeamAI fixture", "-c", "user.email=teamai@example.test", "-c", "core.hooksPath=/dev/null"}, args...)...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("local fixture Git failed: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func teamImportSocketJSON[T any](t *testing.T, server *httptest.Server, user domain.User, method, path string, input any, status int) T {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return teamImportSocketRaw[T](t, server, user, method, path, raw, status)
}

func teamImportSocketRaw[T any](t *testing.T, server *httptest.Server, user domain.User, method, path string, raw []byte, status int) T {
	t.Helper()
	request, err := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", server.URL)
	if user.ID > 0 {
		token, err := auth.NewClientFromEnv().SignUser(&auth.User{ID: user.ID, LoginName: user.LoginName, AuthTime: time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: authCookieName, Value: token})
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 10<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("real HTTP %s %s: want %d got %d: %s", method, path, status, response.StatusCode, content)
	}
	if status == 200 && strings.Contains(path, "/team-imports/preview") && response.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatal("source preview lacks private no-store header")
	}
	var out T
	if err := json.Unmarshal(content, &out); err != nil {
		t.Fatalf("real HTTP invalid JSON: %v", err)
	}
	return out
}

func teamImportHTTPFlow(t *testing.T, a, b *repo.Store) {
	t.Helper()
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SESSION_SECRET", "team-import-local-test-only-session")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	root, dir, commit := teamImportFixture(t)
	t.Setenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE", "1")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT", root)
	for _, name := range []string{"import-owner", "import-member"} {
		if err := a.PutUser(domain.User{LoginName: name}); err != nil {
			t.Fatal(err)
		}
	}
	owner, _ := a.UserByLoginName("import-owner")
	member, _ := a.UserByLoginName("import-member")
	project := domain.Project{ID: "import-project", Name: "TeamAI disposable fixture", OwnerID: owner.ID, Members: []domain.ProjectMember{{UserID: member.ID, Role: "member"}}}
	if err := a.PutProject(project); err != nil {
		t.Fatal(err)
	}
	registered := domain.Repository{ID: "import-source", ProjectID: project.ID, CloneURL: (&url.URL{Scheme: "file", Path: dir}).String()}
	if err := a.PutRepository(registered); err != nil {
		t.Fatal(err)
	}
	if err := a.PutRepository(domain.Repository{ID: "import-http-source", ProjectID: project.ID, CloneURL: "http://example.test/not-read.git"}); err != nil {
		t.Fatal(err)
	}
	if err := a.PutRule(domain.RuleDefinition{ID: "style", Name: "Editable source", ProjectID: project.ID, Content: "Live catalog content must survive import."}); err != nil {
		t.Fatal(err)
	}
	catalogBefore, _ := json.Marshal([]any{a.Agents(), a.Rules(), a.Skills(), a.MCPServers()})
	handler := New(a, engine.New(a, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	base := "/api/projects/" + project.ID
	input := teamImportInput{RepositoryID: registered.ID, Commit: commit, Root: ".", Selection: domain.TeamImportSelection{
		Agents: []string{"agents/reviewer.yaml"}, Rules: []string{"rules/style.md"}, Skills: []string{"skills/check"}, MCPServers: []domain.TeamImportMCP{{Path: "mcp/mcp.yaml", Name: "format"}},
		Bindings: map[string]domain.TeamImportBindings{"agents/reviewer.yaml": {Rules: []string{"rules/style.md"}, Skills: []string{"skills/check"}, MCPServers: []string{"mcp/mcp.yaml#format"}}},
	}}
	previewPath := base + "/team-imports/preview"
	teamImportSocketJSON[map[string]any](t, server, domain.User{}, http.MethodPost, previewPath, input, http.StatusUnauthorized)
	teamImportSocketJSON[map[string]any](t, server, member, http.MethodPost, previewPath, input, http.StatusForbidden)
	for _, raw := range []string{`{"repositoryId":"import-source","url":"file:///private"}`, `{} {}`, `{"selection":{},"actor":"forged"}`} {
		teamImportSocketRaw[map[string]any](t, server, owner, http.MethodPost, previewPath, []byte(raw), http.StatusBadRequest)
	}
	bad := input
	bad.Commit = commit[:12]
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, previewPath, bad, http.StatusBadRequest)
	bad = input
	bad.RepositoryID = "not-registered"
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, previewPath, bad, http.StatusNotFound)
	bad.RepositoryID = "import-http-source"
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, previewPath, bad, http.StatusBadRequest)
	preview := teamImportSocketJSON[teamconfig.Preview](t, server, owner, http.MethodPost, previewPath, input, http.StatusOK)
	if preview.Payload.GitImport == nil || preview.Payload.Source.Commit != commit || preview.Payload.GitImport.RepositoryID != registered.ID || len(preview.Payload.GitImport.Files) != 8 || preview.Payload.Assets.Rules[0].Content != "Reviewed original rule.\n" || preview.Payload.Agents[0].SourceRevision != "" {
		t.Fatal("preview does not contain exact registered scoped source")
	}
	list, err := b.TeamReleases(project.ID)
	if err != nil || len(list) != 0 {
		t.Fatal("preview unexpectedly created a release")
	}
	publish := teamImportPublishInput{teamImportInput: input, Name: "Explicit reviewed import", ExpectedBaseVersion: 0, ExpectedPreviewSHA256: preview.SHA256}
	publishPath := base + "/team-imports/publish"
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, publishPath, publish, http.StatusBadRequest)
	publish.Approve = true
	publish.ExpectedPreviewSHA256 = strings.Repeat("0", 64)
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, publishPath, publish, http.StatusConflict)
	publish.ExpectedPreviewSHA256 = preview.SHA256
	release := teamImportSocketJSON[domain.TeamRelease](t, server, owner, http.MethodPost, publishPath, publish, http.StatusCreated)
	if release.Version != 1 || release.State != domain.TeamReleasePublished || release.SHA256 != preview.SHA256 || release.CreatedBy != owner.LoginName {
		t.Fatal("publication did not pin reviewed payload, human identity or project version")
	}
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, publishPath, publish, http.StatusConflict)
	// Root's exporter is preview-only: same governance boundary, no DB writes.
	exportPath := base + "/team-releases/" + release.ID + "/git-export-preview"
	exportInput := map[string]any{"expectedReleaseSha256": release.SHA256, "changes": []teamconfig.Replacement{{Path: "rules/style.md", Content: []byte("Reviewed proposed rule.\n")}}}
	teamImportSocketJSON[map[string]any](t, server, domain.User{}, http.MethodPost, exportPath, exportInput, http.StatusUnauthorized)
	teamImportSocketJSON[map[string]any](t, server, member, http.MethodPost, exportPath, exportInput, http.StatusForbidden)
	exportInput["expectedReleaseSha256"] = strings.Repeat("0", 64)
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, exportPath, exportInput, http.StatusConflict)
	exportInput["expectedReleaseSha256"] = release.SHA256
	exported := teamImportSocketJSON[teamconfig.ExportPreview](t, server, owner, http.MethodPost, exportPath, exportInput, http.StatusOK)
	if exported.ReleaseSHA256 != release.SHA256 || !strings.Contains(exported.Patch, "+Reviewed proposed rule.") || exported.Proposed.Payload.Assets.Rules[0].Content != "Reviewed proposed rule.\n" {
		t.Fatal("export preview did not return the proposed scoped patch")
	}
	// A new source commit cannot mutate an approved historical release.
	if err := os.WriteFile(filepath.Join(dir, "rules/style.md"), []byte("Future source rule.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	teamImportGit(t, dir, "add", "--all")
	teamImportGit(t, dir, "commit", "-m", "Future source revision")
	nextCommit := teamImportGit(t, dir, "rev-parse", "HEAD")
	next := input
	next.Commit = nextCommit
	newPreview := teamImportSocketJSON[teamconfig.Preview](t, server, owner, http.MethodPost, previewPath, next, http.StatusOK)
	if newPreview.SHA256 == release.SHA256 || newPreview.Payload.Assets.Rules[0].Content != "Future source rule.\n" {
		t.Fatal("new source revision was silently treated as the approved source")
	}
	old, err := b.TeamRelease(project.ID, release.ID)
	if err != nil || old.SHA256 != release.SHA256 || old.Payload.Source.Commit != commit || old.Payload.Assets.Rules[0].Content != "Reviewed original rule.\n" {
		t.Fatal("independent store read lost immutable imported source")
	}
	readback := teamImportSocketJSON[domain.TeamRelease](t, server, owner, http.MethodGet, base+"/team-releases/"+release.ID, nil, http.StatusOK)
	if readback.SHA256 != old.SHA256 {
		t.Fatal("HTTP release readback changed imported history")
	}
	list, err = b.TeamReleases(project.ID)
	if err != nil || len(list) != 1 {
		t.Fatal("preview/export/stale publications created extra releases")
	}
	catalogAfter, _ := json.Marshal([]any{b.Agents(), b.Rules(), b.Skills(), b.MCPServers()})
	if !bytes.Equal(catalogBefore, catalogAfter) {
		t.Fatal("controlled import or export mutated live catalog definitions")
	}
	if _, err := a.RevokeTeamRelease(project.ID, release.ID, release.Revision, owner.LoginName, "independent export lifecycle check"); err != nil {
		t.Fatal(err)
	}
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, exportPath, exportInput, http.StatusUnprocessableEntity)
	t.Log("real HTTP socket + exact temporary Git: human authorization, bounded input, preview zero writes, digest approval, stale-base conflict, immutable readback and export-preview checked; no live Catalog writes")
}
