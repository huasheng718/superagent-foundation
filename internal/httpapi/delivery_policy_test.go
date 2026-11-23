package httpapi

import (
	"net/http"
	"testing"

	"agentflow/internal/domain"
)

func TestManualRunnerTaskCannotDowngradeDeliveryPolicy(t *testing.T) {
	h := newRunnerTestServer(t)
	task := requestJSON[domain.RunnerTask](t, h, http.MethodPost, "/api/runners/tasks", map[string]any{
		"provider": "mock", "prompt": "execute only", "deliveryPolicy": domain.DeliveryPolicyLegacy,
	}, http.StatusCreated)
	if task.DeliveryPolicy != domain.DeliveryPolicyControlledPR {
		t.Fatalf("client downgraded server policy: %q", task.DeliveryPolicy)
	}
}
