package httpapi

import (
	"context"
	"net/http"
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
)

func TestTeamGitProposalHTTPRequiresReviewedOwnerApprovalAndAdminPublish(t *testing.T) {
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SESSION_SECRET", "team-proposal-http-fixture")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "rules"), 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = source
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v %.512s", args[0], err, raw)
		}
		return strings.TrimSpace(string(raw))
	}
	for p, content := range map[string]string{"teamai.yaml": "team: fixture\nrepo: fixture\n", "LICENSE": "Original fixture license\n", "rules/review.md": "Original rule\n", "unselected.txt": "Keep this file\n"} {
		if err := os.WriteFile(filepath.Join(source, p), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "--template=", "-b", "main")
	git("add", ".")
	git("commit", "-qm", "source")
	base := git("rev-parse", "HEAD")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE", "1")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT", root)
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	users := map[string]domain.User{}
	for _, login := range []string{"proposal-owner", "proposal-member", "proposal-outsider", "proposal-admin"} {
		if err = s.PutUser(domain.User{LoginName: login, GitToken: "local-fake-controller-token"}); err != nil {
			t.Fatal(err)
		}
		users[login], _ = s.UserByLoginName(login)
	}
	owner, member, outsider, admin := users["proposal-owner"], users["proposal-member"], users["proposal-outsider"], users["proposal-admin"]
	if _, err = s.GrantPlatformAdmin(admin.LoginName); err != nil {
		t.Fatal(err)
	}
	if err = s.PutProject(domain.Project{ID: "proposal-http", Name: "Fixture", OwnerID: owner.ID, Members: []domain.ProjectMember{{UserID: member.ID, Role: "member"}}}); err != nil {
		t.Fatal(err)
	}
	r := domain.Repository{ID: "proposal-http-source", ProjectID: "proposal-http", Provider: "github", Name: "fixture", GongfengProjectID: "github:example/repo", CloneURL: "file://" + source, DefaultBranch: "main"}
	if err = s.PutRepository(r); err != nil {
		t.Fatal(err)
	}
	selection := domain.TeamImportSelection{Rules: []string{"rules/review.md"}}
	files, err := teamconfig.Read(context.Background(), r, base, ".", selection)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := teamconfig.Parse(r.ProjectID, r, base, ".", selection, files)
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.CreateImportedTeamRelease(r.ProjectID, "Source", owner.LoginName, 0, preview.Payload)
	if err != nil {
		t.Fatal(err)
	}
	changes := []teamconfig.Replacement{{Path: "rules/review.md", Content: []byte("Human reviewed replacement\n")}}
	export, err := teamconfig.Export(context.Background(), r, release, changes)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(s, runtime.NewManager(filepath.Join(root, "workspaces")))
	h := New(s, e, nil, "", "")
	endpoint := "/api/projects/" + r.ProjectID + "/team-git-proposals"
	input := map[string]any{"releaseId": release.ID, "expectedReleaseSha256": release.SHA256, "expectedBaseCommit": base, "expectedPatchSha256": export.PatchSHA256, "changes": changes}
	for _, u := range []domain.User{member, outsider} {
		requestJSONAuthed[map[string]any](t, h, u, "POST", endpoint, input, 403)
		requestJSONAuthed[map[string]any](t, h, u, "GET", endpoint, nil, 403)
	}
	input["actor"] = "forged"
	requestJSONAuthed[map[string]any](t, h, owner, "POST", endpoint, input, 400)
	delete(input, "actor")
	input["expectedPatchSha256"] = strings.Repeat("0", 64)
	requestJSONAuthed[map[string]any](t, h, owner, "POST", endpoint, input, 409)
	input["expectedPatchSha256"] = export.PatchSHA256
	p := requestJSONAuthed[domain.TeamGitProposal](t, h, owner, "POST", endpoint, input, 201)
	if p.State != "pending_review" || p.CreatedBy != owner.LoginName || p.Publication != nil || p.Revision != 1 {
		t.Fatal("prepare granted publication authority")
	}
	list := requestJSONAuthed[[]domain.TeamGitProposal](t, h, owner, "GET", endpoint, nil, 200)
	if len(list) != 1 || list[0].Patch != "" {
		t.Fatal("list exposed bulk content")
	}
	path := endpoint + "/" + p.ID
	decision := map[string]any{"expectedProposalSha256": p.SHA256, "decision": "approved", "reason": "Local synthetic fixture reviewed; no real hosted write"}
	requestJSONAuthed[map[string]any](t, h, owner, "POST", path+"/decision", decision, 428)
	decision["expectedRevision"] = 1
	requestJSONAuthed[map[string]any](t, h, member, "POST", path+"/decision", decision, 403)
	p = requestJSONAuthed[domain.TeamGitProposal](t, h, owner, "POST", path+"/decision", decision, 200)
	requestJSONAuthed[map[string]any](t, h, owner, "POST", path+"/decision", decision, 409)
	if p.Revision != 2 || p.State != "approved" || p.Publication == nil || p.Publication.SourceKind != "teamai_proposal" {
		t.Fatal("approval/outbox binding missing")
	}
	publish := map[string]any{"expectedProposalSha256": p.SHA256, "expectedRevision": 2, "expectedBaseCommit": base}
	requestJSONAuthed[map[string]any](t, h, owner, "POST", path+"/publish", publish, 403)
	posts, objects := 0, 0
	e.PublishContributionObjects = func(context.Context, string, string, string, string, string, string) error { objects++; return nil }
	e.CreateContributionPR = func(context.Context, string, string, string, string, string, string) (domain.ContributionPRReceipt, error) {
		posts++
		return domain.ContributionPRReceipt{URL: "https://github.com/example/repo/pull/31", Number: 31, HeadSHA: p.ResultSHA, Draft: true}, nil
	}
	e.DiscoverContributionPR = e.CreateContributionPR // Only terminal replay is used in this success fixture.
	publication := requestJSONAuthed[domain.ContributionPublication](t, h, admin, "POST", path+"/publish", publish, 200)
	if publication.State != "pr_ready" || posts != 1 || objects != 1 {
		t.Fatal("draft publication did not complete once")
	}
	requestJSONAuthed[domain.ContributionPublication](t, h, admin, "POST", path+"/publish", publish, 200)
	if posts != 1 || objects != 1 {
		t.Fatal("terminal publication replay performed provider IO")
	}
	if git("rev-parse", "HEAD") != base || git("for-each-ref", "--format=%(refname)") != "refs/heads/main" {
		t.Fatal("source/target local refs changed")
	}
	requestJSONAuthed[map[string]any](t, h, outsider, "GET", path, nil, http.StatusForbidden)
}
