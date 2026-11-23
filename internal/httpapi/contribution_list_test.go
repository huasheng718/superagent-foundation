package httpapi

import (
	"agentflow/internal/domain"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestContributionListProjectsReceiptsWithoutExecutionSecrets(t *testing.T) {
	f := newContributionAPIFixture(t, false)
	integration := f.prepare(t)
	response := requestJSON[struct {
		Tasks []struct {
			TaskID        string                             `json:"taskId"`
			RunID         string                             `json:"runId"`
			Contributions []domain.RunnerContributionReceipt `json:"contributions"`
		} `json:"tasks"`
		Integrations []domain.ContributionIntegration `json:"integrations"`
	}](t, f.h, http.MethodGet, "/api/runs/controller/contributions", nil, http.StatusOK)
	if len(response.Tasks) != 1 || response.Tasks[0].TaskID != f.task.ID || response.Tasks[0].RunID != f.source.ID || len(response.Tasks[0].Contributions) != 1 {
		t.Fatalf("receipt projection leaked project or lost source: %+v", response.Tasks)
	}
	if len(response.Integrations) != 1 || response.Integrations[0].ID != integration.ID {
		t.Fatal("lost controller integration")
	}
	raw, _ := json.Marshal(response)
	for _, key := range []string{"invocation", "gitToken", "apiKey", "sessionToken"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Fatalf("execution secret-bearing field %s exposed", key)
		}
	}
	foreign := domain.Run{ID: "foreign-controller", Status: "succeeded", ExecutionContext: domain.TelemetryContext{ProjectID: "other-project"}}
	if err := f.s.PutRun(foreign); err != nil {
		t.Fatal(err)
	}
	isolated := requestJSON[struct {
		Tasks []any `json:"tasks"`
	}](t, f.h, http.MethodGet, "/api/runs/foreign-controller/contributions", nil, http.StatusOK)
	if len(isolated.Tasks) != 0 {
		t.Fatal("foreign project receipts exposed")
	}
	requestJSON[map[string]any](t, f.h, http.MethodGet, "/api/runs/missing/contributions", nil, http.StatusNotFound)
	requestJSON[struct {
		Integrations []domain.ContributionIntegration `json:"integrations"`
	}](t, f.h, http.MethodGet, "/api/runs/source/contributions", nil, http.StatusOK)
}
func TestContributionListRequiresControllerAdministrator(t *testing.T) {
	f := newPublicationHTTPFixture(t)
	path := "/api/runs/controller/contributions"
	requestJSON[map[string]any](t, f.h, http.MethodGet, path, nil, http.StatusUnauthorized)
	requestJSONAuthed[map[string]any](t, f.h, f.member, http.MethodGet, path, nil, http.StatusForbidden)
	requestJSONAuthed[map[string]any](t, f.h, f.admin, http.MethodGet, path, nil, http.StatusOK)
	response := requestWithHeaders(t, f.h, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer " + f.runnerSession})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("runner read control-plane receipts: %d", response.Code)
	}
}
