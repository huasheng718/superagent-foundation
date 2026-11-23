package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/teamconfig"
	"agentflow/internal/tracker"
	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestTeamUpdateReviewedRealHTTPFlow(t *testing.T) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	teamUpdateHTTPFlow(t, store, store)
}

func TestMySQLTeamUpdateReviewedRealHTTPFlow(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEAM_IMPORT_MYSQL_DSN")
	if dsn == "" {
		t.Skip("disposable TeamAI Git update MySQL DSN not supplied")
	}
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil || cfg == nil || os.Getenv("AGENTFLOW_TEAM_IMPORT_MYSQL_DISPOSABLE") != "1" || !strings.HasPrefix(cfg.DBName, "team_import_acceptance_") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("explicit loopback disposable team_import_acceptance_ schema required; DSN omitted")
	}
	a, err := repo.Open(dsn)
	if err != nil {
		t.Fatal("disposable MySQL open failed; DSN omitted")
	}
	t.Cleanup(a.Close)
	b, err := repo.Open(dsn)
	if err != nil {
		t.Fatal("independent MySQL open failed; DSN omitted")
	}
	t.Cleanup(b.Close)
	teamUpdateHTTPFlow(t, a, b)
}

func teamUpdateHTTPFlow(t *testing.T, store, reader *repo.Store) {
	t.Helper()
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SESSION_SECRET", "team-update-test-only")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	root, dir, original := teamImportFixture(t)
	teamImportGit(t, dir, "branch", "-M", "main")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE", "1")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT", root)
	for _, login := range []string{"update-owner", "update-member"} {
		if err := store.PutUser(domain.User{LoginName: login}); err != nil {
			t.Fatal(err)
		}
	}
	owner, _ := store.UserByLoginName("update-owner")
	member, _ := store.UserByLoginName("update-member")
	project := domain.Project{ID: "update-project", Name: "Update fixture", OwnerID: owner.ID, Members: []domain.ProjectMember{{UserID: member.ID, Role: "member"}}}
	if err := store.PutProject(project); err != nil {
		t.Fatal(err)
	}
	registered := domain.Repository{ID: "update-source", ProjectID: project.ID, CloneURL: (&url.URL{Scheme: "file", Path: dir}).String(), DefaultBranch: "main"}
	if err := store.PutRepository(registered); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, engine.New(store, runtime.NewManager(t.TempDir())), tracker.NewService(), "", ""))
	t.Cleanup(server.Close)
	base := "/api/projects/" + project.ID
	input := teamImportInput{RepositoryID: registered.ID, Commit: original, Root: ".", Selection: domain.TeamImportSelection{Rules: []string{"rules/style.md"}}}
	preview := teamImportSocketJSON[teamconfig.Preview](t, server, owner, http.MethodPost, base+"/team-imports/preview", input, 200)
	first := teamImportSocketJSON[domain.TeamRelease](t, server, owner, http.MethodPost, base+"/team-imports/publish", teamImportPublishInput{teamImportInput: input, Name: "Original", ExpectedPreviewSHA256: preview.SHA256, Approve: true}, 201)
	catalogBefore, _ := json.Marshal([]any{reader.Agents(), reader.Rules(), reader.Skills(), reader.MCPServers()})
	updatePath := base + "/team-releases/" + first.ID + "/git-update-preview"
	checkInput := map[string]any{"expectedReleaseSha256": first.SHA256}
	teamImportSocketJSON[map[string]any](t, server, domain.User{}, http.MethodPost, updatePath, checkInput, 401)
	teamImportSocketJSON[map[string]any](t, server, member, http.MethodPost, updatePath, checkInput, 403)
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, updatePath, map[string]any{"expectedReleaseSha256": first.SHA256, "ref": "refs/heads/main"}, 400)
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, updatePath, map[string]any{"expectedReleaseSha256": strings.Repeat("0", 64)}, 409)
	unchanged := teamImportSocketJSON[teamUpdatePreview](t, server, owner, http.MethodPost, updatePath, checkInput, 200)
	if unchanged.Status != "unchanged" || unchanged.Preview != nil || len(unchanged.Changes) != 0 || unchanged.Ref != "refs/heads/main" || unchanged.Commit != original {
		t.Fatalf("incorrect unchanged response: %#v", unchanged)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules/style.md"), []byte("Reviewed update A.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	teamImportGit(t, dir, "commit", "-am", "source A")
	commitA := teamImportGit(t, dir, "rev-parse", "HEAD")
	check := teamImportSocketJSON[teamUpdatePreview](t, server, owner, http.MethodPost, updatePath, checkInput, 200)
	if check.Status != "updated" || check.Commit != commitA || check.ExpectedBaseVersion != 1 || check.Preview == nil || check.Preview.Payload.GitImport.Ref != "refs/heads/main" || len(check.Changes) != 1 || check.Changes[0].Kind != "changed" || check.Changes[0].Path != "rules/style.md" {
		t.Fatalf("invalid frozen update preview: %#v", check)
	}
	from := &domain.TeamImportUpdateFrom{ReleaseID: check.BaseReleaseID, ReleaseSHA256: check.BaseReleaseSHA256, Ref: check.Ref}
	manifest := check.Preview.Payload.GitImport
	publish := teamImportPublishInput{teamImportInput: teamImportInput{RepositoryID: manifest.RepositoryID, Commit: check.Commit, Root: manifest.Root, Selection: manifest.Selection}, Name: "Reviewed A", ExpectedBaseVersion: check.ExpectedBaseVersion, ExpectedPreviewSHA256: check.Preview.SHA256, Approve: true, UpdateFrom: from}
	publishPath := base + "/team-imports/publish"
	teamImportSocketJSON[map[string]any](t, server, member, http.MethodPost, publishPath, publish, 403)
	for _, mutate := range []func(*teamImportPublishInput){
		func(v *teamImportPublishInput) { v.Approve = false },
		func(v *teamImportPublishInput) { v.ExpectedPreviewSHA256 = strings.Repeat("0", 64) },
		func(v *teamImportPublishInput) { v.ExpectedBaseVersion = 0 },
		func(v *teamImportPublishInput) { v.UpdateFrom = nil },
		func(v *teamImportPublishInput) { v.Root = ".teamai" },
	} {
		bad := publish
		mutate(&bad)
		status := 409
		if !bad.Approve {
			status = 400
		}
		teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, publishPath, bad, status)
	}
	wrongRef := *from
	wrongRef.Ref = "refs/heads/other"
	bad := publish
	bad.UpdateFrom = &wrongRef
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, publishPath, bad, 409)
	if err := os.WriteFile(filepath.Join(dir, "rules/style.md"), []byte("Unreviewed B.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	teamImportGit(t, dir, "commit", "-am", "source B")
	commitB := teamImportGit(t, dir, "rev-parse", "HEAD")
	conflict := teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, publishPath, publish, 409)
	if conflict["code"] != "team_import_ref_conflict" {
		t.Fatal("ref drift lacks explicit conflict code")
	}
	teamImportGit(t, dir, "update-ref", "refs/heads/main", commitA)
	changed := registered
	changed.DefaultBranch = "another"
	if err := store.PutRepository(changed); err != nil {
		t.Fatal(err)
	}
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, publishPath, publish, 409)
	changed = registered
	changed.CloneURL = "https://example.invalid/different.git"
	if err := store.PutRepository(changed); err != nil {
		t.Fatal(err)
	}
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, publishPath, publish, 409)
	if err := store.PutRepository(registered); err != nil {
		t.Fatal(err)
	}
	list, _ := reader.TeamReleases(project.ID)
	if len(list) != 1 {
		t.Fatal("preview/rejected approval wrote release")
	}
	for _, operation := range []string{"preview", "publish"} {
		for _, mutation := range []string{"move", "delete"} {
			t.Run(operation+"-during-source-read-"+mutation, func(t *testing.T) {
				teamImportGit(t, dir, "update-ref", "refs/heads/main", commitA)
				count := teamUpdateGitMutationOnSecondObservation(t, dir, "refs/heads/main", commitB, mutation)
				path, body := publishPath, any(publish)
				if operation == "preview" {
					path, body = updatePath, checkInput
				}
				response := teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, path, body, 409)
				if response["code"] != "team_import_ref_conflict" {
					t.Fatal("concurrent source change not classified")
				}
				if _, err := os.Stat(count); err != nil {
					t.Fatal("real Git observation wrapper was not invoked")
				}
			})
		}
	}
	teamImportGit(t, dir, "update-ref", "refs/heads/main", commitA)
	accepted := teamImportSocketJSON[domain.TeamRelease](t, server, owner, http.MethodPost, publishPath, publish, 201)
	if accepted.Version != 2 || accepted.SHA256 != check.Preview.SHA256 || accepted.Payload.GitImport.Commit != commitA || accepted.Payload.GitImport.Ref != check.Ref || accepted.Payload.Assets.Rules[0].Content != "Reviewed update A.\n" {
		t.Fatal("published a different source than reviewed")
	}
	originalReload, err := reader.TeamRelease(project.ID, first.ID)
	if err != nil || originalReload.SHA256 != first.SHA256 || originalReload.Payload.GitImport.Ref != "" || originalReload.Payload.Assets.Rules[0].Content != "Reviewed original rule.\n" {
		t.Fatal("historical release changed")
	}
	exported := teamImportSocketJSON[teamconfig.ExportPreview](t, server, owner, http.MethodPost, base+"/team-releases/"+accepted.ID+"/git-export-preview", map[string]any{"expectedReleaseSha256": accepted.SHA256, "changes": []teamconfig.Replacement{{Path: "rules/style.md", Content: []byte("Proposal only.\n")}}}, 200)
	if exported.BaseCommit != commitA || exported.Proposed.Payload.GitImport.Ref != check.Ref {
		t.Fatal("ref provenance broke fixed commit export")
	}
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, publishPath, publish, 409)
	if _, err := store.RevokeTeamRelease(project.ID, first.ID, 1, "update-owner", "negative test"); err != nil {
		t.Fatal(err)
	}
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, publishPath, publish, 409)
	teamImportSocketJSON[map[string]any](t, server, owner, http.MethodPost, updatePath, checkInput, 409)
	catalogAfter, _ := json.Marshal([]any{reader.Agents(), reader.Rules(), reader.Skills(), reader.MCPServers()})
	if string(catalogAfter) != string(catalogBefore) {
		t.Fatal("Git update mutated live catalog")
	}
	list, _ = reader.TeamReleases(project.ID)
	if len(list) != 2 {
		t.Fatal("rejected repeated approval wrote duplicate")
	}
}

// Delegate every operation to real Git, but move/delete the fixture ref at the
// second ls-remote observation to prove read/approve races fail closed.
func teamUpdateGitMutationOnSecondObservation(t *testing.T, source, ref, target, mutation string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	count := filepath.Join(bin, "observed")
	script := `#!/bin/sh
for arg in "$@"; do
 if [ "$arg" = ls-remote ]; then
  if [ -f "$TEAM_UPDATE_TEST_COUNT" ]; then
   if [ "$TEAM_UPDATE_TEST_MUTATION" = delete ]; then
    "$TEAM_UPDATE_TEST_REAL_GIT" -C "$TEAM_UPDATE_TEST_SOURCE" update-ref -d "$TEAM_UPDATE_TEST_REF" || exit 97
   else
    "$TEAM_UPDATE_TEST_REAL_GIT" -C "$TEAM_UPDATE_TEST_SOURCE" update-ref "$TEAM_UPDATE_TEST_REF" "$TEAM_UPDATE_TEST_TARGET" || exit 97
   fi
  else
   : > "$TEAM_UPDATE_TEST_COUNT"
  fi
 fi
done
exec "$TEAM_UPDATE_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"TEAM_UPDATE_TEST_REAL_GIT": realGit, "TEAM_UPDATE_TEST_SOURCE": source, "TEAM_UPDATE_TEST_REF": ref, "TEAM_UPDATE_TEST_TARGET": target, "TEAM_UPDATE_TEST_MUTATION": mutation, "TEAM_UPDATE_TEST_COUNT": count} {
		t.Setenv(key, value)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return count
}
