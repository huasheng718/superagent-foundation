package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func httpCandidateFixture() map[string]any {
	return map[string]any{"skill": map[string]any{"id": "http-reviewed-skill", "name": "HTTP reviewed skill", "files": map[string]string{"SKILL.md": "---\nname: http-reviewed-skill\ndescription: Reviewed instructions.\n---\n\n# Reviewed\n", "LICENSE": "Original license\n", "nested/context.txt": "retained nested context\n"}}, "source": map[string]string{"repository": "https://example.com/team/source", "commit": strings.Repeat("1", 40), "path": "skills/http-reviewed-skill"}, "publisher": "Original author", "license": "MIT", "noticePaths": []string{"LICENSE"}, "state": "approved", "revision": 99, "createdBy": "forged-owner"}
}
func TestSkillCandidateHTTPProjectHumanReviewAndQuarantineBoundary(t *testing.T) {
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SESSION_SECRET", "candidate-http-fixture-secret")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	users := map[string]domain.User{}
	for _, name := range []string{"candidate-owner", "candidate-member", "candidate-outsider"} {
		if err := s.PutUser(domain.User{LoginName: name}); err != nil {
			t.Fatal(err)
		}
		users[name], _ = s.UserByLoginName(name)
	}
	owner, member, outsider := users["candidate-owner"], users["candidate-member"], users["candidate-outsider"]
	for _, p := range []domain.Project{{ID: "candidate-a", OwnerID: owner.ID, Members: []domain.ProjectMember{{UserID: member.ID, Role: "member"}}}, {ID: "candidate-b", OwnerID: outsider.ID}} {
		if err := s.PutProject(p); err != nil {
			t.Fatal(err)
		}
	}
	h := New(s, engine.New(s, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
	base := "/api/projects/candidate-a/skill-candidates"
	requestJSON[map[string]any](t, h, http.MethodGet, base, nil, http.StatusUnauthorized)
	requestJSONAuthed[map[string]any](t, h, outsider, http.MethodGet, base, nil, http.StatusForbidden)
	requestJSONAuthed[map[string]any](t, h, outsider, http.MethodPost, base, httpCandidateFixture(), http.StatusForbidden)
	candidate := requestJSONAuthed[domain.SkillCandidate](t, h, member, http.MethodPost, base, httpCandidateFixture(), http.StatusCreated)
	if candidate.State != domain.SkillCandidateQuarantined || candidate.Revision != 1 || candidate.CreatedBy != member.LoginName || candidate.Package.Skill.ProjectID != "candidate-a" {
		t.Fatal("body forged governance authority")
	}
	if _, ok := s.Skill("http-reviewed-skill"); ok {
		t.Fatal("member submission became executable")
	}
	metadata := requestJSONAuthed[[]map[string]any](t, h, member, http.MethodGet, base, nil, http.StatusOK)
	if len(metadata) != 1 || metadata[0]["package"] != nil {
		t.Fatal("list exposed full unbounded resource bytes")
	}
	requestJSONAuthed[map[string]any](t, h, outsider, http.MethodGet, base+"/"+candidate.ID, nil, http.StatusForbidden)
	requestJSONAuthed[map[string]any](t, h, outsider, http.MethodGet, "/api/projects/candidate-b/skill-candidates/"+candidate.ID, nil, http.StatusNotFound)
	detail := requestJSONAuthed[domain.SkillCandidate](t, h, member, http.MethodGet, base+"/"+candidate.ID, nil, http.StatusOK)
	if detail.Package.Skill.Files["nested/context.txt"] != "retained nested context\n" || len(detail.Resources) != 3 {
		t.Fatal("review detail omitted bytes or resource digests")
	}
	decision := map[string]any{"decision": "approved", "expectedRevision": 1, "reason": "human reviewed instructions, source, license and resources"}
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base+"/"+candidate.ID+"/decision", decision, http.StatusForbidden)
	approved := requestJSONAuthed[domain.SkillCandidate](t, h, owner, http.MethodPost, base+"/"+candidate.ID+"/decision", decision, http.StatusOK)
	if approved.State != "approved" || approved.PromotedSkillID != "http-reviewed-skill" {
		t.Fatal("explicit approval did not create skill")
	}
	requestJSONAuthed[map[string]any](t, h, owner, http.MethodPost, base+"/"+candidate.ID+"/decision", decision, http.StatusConflict)
	active, _ := s.Skill("http-reviewed-skill")
	if active.Source == nil || active.Source.ReviewedCandidate.CandidateID != candidate.ID {
		t.Fatal("active skill lost license/source audit")
	}
	releases, _ := s.TeamReleases("candidate-a")
	if len(releases) != 0 {
		t.Fatal("approval implicitly published TeamAI")
	}
	reject := requestJSONAuthed[domain.SkillCandidate](t, h, member, http.MethodPost, base, httpCandidateFixture(), http.StatusCreated)
	requestJSONAuthed[domain.SkillCandidate](t, h, owner, http.MethodPost, base+"/"+reject.ID+"/decision", map[string]any{"decision": "rejected", "expectedRevision": 1, "reason": "do not overwrite existing skill"}, http.StatusOK)
	requestJSONAuthed[map[string]any](t, h, owner, http.MethodPost, base+"/"+reject.ID+"/decision", decision, http.StatusConflict)
	if err := s.PutRun(domain.Run{ID: "foreign-source", ExecutionContext: domain.TelemetryContext{ProjectID: "candidate-b"}}); err != nil {
		t.Fatal(err)
	}
	forged := httpCandidateFixture()
	forged["source"] = map[string]any{"runId": "foreign-source", "path": "artifact.md"}
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base, forged, http.StatusNotFound)
	malformed := httpCandidateFixture()
	malformed["noticePaths"] = []string{"missing-NOTICE"}
	requestJSONAuthed[map[string]any](t, h, member, http.MethodPost, base, malformed, http.StatusBadRequest)
}
