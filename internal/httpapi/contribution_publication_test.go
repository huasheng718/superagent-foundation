package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

type publicationHTTPFixture struct {
	contributionAPIFixture
	integration   domain.ContributionIntegration
	admin, member domain.User
	runnerSession string
	remoteCalls   *int
}

func newPublicationHTTPFixture(t *testing.T) publicationHTTPFixture {
	t.Helper()
	f := newContributionAPIFixture(t, false)
	eng := engine.New(f.s, runtime.NewManager(t.TempDir()))
	eng.ConfigureExecution("publication-http-test", "test", time.Minute, false)
	calls := 0
	eng.CreateContributionPR = func(context.Context, string, string, string, string, string, string) (domain.ContributionPRReceipt, error) {
		calls++
		t.Error("unready or unauthorized HTTP request reached the remote PR callback")
		return domain.ContributionPRReceipt{}, nil
	}
	f.h = New(f.s, eng, tracker.NewService(), "", "")
	repository, _ := f.s.Repository("repo")
	repository.DefaultBranch = "main"
	if err := f.s.PutRepository(repository); err != nil {
		t.Fatal(err)
	}
	integration := f.prepare(t)
	_, runnerSession := registerTestRunner(t, f.h)
	admin := domain.User{ID: 601, LoginName: "publication-admin"}
	member := domain.User{ID: 602, LoginName: "publication-member"}
	for _, user := range []domain.User{admin, member} {
		if err := f.s.PutUser(user); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AGENTFLOW_PLATFORM_ADMINS", admin.LoginName)
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	return publicationHTTPFixture{contributionAPIFixture: f, integration: integration, admin: admin, member: member, runnerSession: runnerSession, remoteCalls: &calls}
}

func publicationHTTPEndpoints(runID, integrationID string) []struct {
	method, path string
	body         any
} {
	base := "/api/runs/" + runID + "/contributions/" + integrationID
	return []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, base + "/gates", map[string]any{"gateRunId": "source", "nodeId": "implement", "kind": "fast", "artifactId": "missing-report", "status": "passed", "resultSHA": strings.Repeat("f", 40), "evidenceSHA256": strings.Repeat("f", 64)}},
		{http.MethodPost, base + "/publish", map[string]any{}},
		{http.MethodGet, base + "/publication", nil},
	}
}

func TestContributionPublicationHTTPRequiresAdminWithoutRunnerAuthority(t *testing.T) {
	f := newPublicationHTTPFixture(t)
	for _, endpoint := range publicationHTTPEndpoints(f.controller.ID, f.integration.ID) {
		t.Run(endpoint.method+" "+endpoint.path, func(t *testing.T) {
			requestJSON[map[string]any](t, f.h, endpoint.method, endpoint.path, endpoint.body, http.StatusUnauthorized)
			requestJSONAuthed[map[string]any](t, f.h, f.member, endpoint.method, endpoint.path, endpoint.body, http.StatusForbidden)
			response := requestWithHeaders(t, f.h, endpoint.method, endpoint.path, endpoint.body, map[string]string{"Authorization": "Bearer " + f.runnerSession})
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("Runner acquired controller authority: %d %s", response.Code, response.Body.String())
			}
		})
	}
	if *f.remoteCalls != 0 {
		t.Fatal("denied caller reached remote callback")
	}
	if _, ok := f.s.ContributionPublication(f.integration.ID); ok {
		t.Fatal("denied caller created publication state")
	}
}

func TestContributionPublicationHTTPOwnershipAndMissingGates(t *testing.T) {
	f := newPublicationHTTPFixture(t)
	for _, pair := range [][2]string{{"wrong-run", f.integration.ID}, {f.source.ID, f.integration.ID}, {f.controller.ID, strings.Repeat("0", 64)}} {
		for _, endpoint := range publicationHTTPEndpoints(pair[0], pair[1]) {
			requestJSONAuthed[map[string]any](t, f.h, f.admin, endpoint.method, endpoint.path, endpoint.body, http.StatusNotFound)
		}
	}
	base := "/api/runs/" + f.controller.ID + "/contributions/" + f.integration.ID
	requestJSONAuthed[map[string]any](t, f.h, f.admin, http.MethodGet, base+"/publication", nil, http.StatusNotFound)
	for _, endpoint := range publicationHTTPEndpoints(f.controller.ID, f.integration.ID) {
		if endpoint.method == http.MethodPost {
			requestJSONAuthed[map[string]any](t, f.h, f.admin, endpoint.method, endpoint.path, endpoint.body, http.StatusConflict)
		}
	}
	if _, ok := f.s.ContributionPublication(f.integration.ID); ok {
		t.Fatal("forged report created publication state")
	}
	// An existing empty outbox is observable by the controller but grants no
	// remote authority. The handler does not treat its metadata as passed gates.
	if _, err := f.s.PrepareContributionPublication(f.integration.ID, "main", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	publication := requestJSONAuthed[domain.ContributionPublication](t, f.h, f.admin, http.MethodGet, base+"/publication", nil, http.StatusOK)
	if publication.State != "gating" || len(publication.Gates) != 0 {
		t.Fatalf("unready publication=%+v", publication)
	}
	requestJSONAuthed[map[string]any](t, f.h, f.admin, http.MethodPost, base+"/publish", map[string]any{}, http.StatusConflict)
	if *f.remoteCalls != 0 {
		t.Fatal("missing gates reached remote callback")
	}
	stored, ok := f.s.ContributionPublication(f.integration.ID)
	if !ok || stored.State != "gating" || stored.Attempt != 0 || stored.PRURL != "" {
		t.Fatalf("rejected publication acquired authority: %+v", stored)
	}
}
