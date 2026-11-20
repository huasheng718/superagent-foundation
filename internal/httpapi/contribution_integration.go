package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"agentflow/internal/domain"

	"github.com/gin-gonic/gin"
)

// listContributions deliberately projects receipt metadata rather than Runner
// invocations, which may contain execution credentials.
func (h *runsHandlers) listContributions(c *gin.Context) {
	if authRequired() && !h.isPlatformAdmin(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "controller integration requires platform admin"})
		return
	}
	runID := c.Param("id")
	controller, ok := h.repo.Run(runID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	type taskSummary struct {
		TaskID        string                             `json:"taskId"`
		RunID         string                             `json:"runId"`
		NodeID        string                             `json:"nodeId"`
		Attempt       int                                `json:"attempt"`
		Status        string                             `json:"status"`
		Contributions []domain.RunnerContributionReceipt `json:"contributions"`
	}
	tasks := make([]taskSummary, 0)
	projectID := domain.RunProjectID(controller, "")
	for _, task := range h.repo.RunnerTasks() {
		source, exists := h.repo.RunState(task.RunID)
		if !exists || (task.RunID != runID && (projectID == "" || domain.RunProjectID(source, "") != projectID || source.Status != "succeeded")) {
			continue
		}
		if len(task.AcceptedContributions) == 0 {
			continue
		}
		tasks = append(tasks, taskSummary{task.ID, task.RunID, task.NodeID, task.Attempt, task.Status, task.AcceptedContributions})
	}
	integrations, err := h.repo.ContributionIntegrationsForRun(runID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot read controller integrations"})
		return
	}
	publications := make([]domain.ContributionPublication, 0)
	for _, value := range integrations {
		if publication, ok := h.repo.ContributionPublication(value.ID); ok {
			publications = append(publications, publication)
		}
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, gin.H{"tasks": tasks, "integrations": integrations, "publications": publications})
}

func (h *runsHandlers) integrateContributions(c *gin.Context) {
	if authRequired() && !h.isPlatformAdmin(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "controller integration requires platform admin"})
		return
	}
	var input struct {
		RepositoryID string   `json:"repositoryId"`
		TaskIDs      []string `json:"taskIds"`
	}
	if !decode(c, &input) {
		return
	}
	result, err := h.engine.IntegrateRunnerContributions(c.Request.Context(), c.Param("id"), input.RepositoryID, input.TaskIDs)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *runsHandlers) integrationContent(c *gin.Context) {
	if authRequired() && !h.isPlatformAdmin(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "controller integration requires platform admin"})
		return
	}
	value, ok := h.repo.ContributionIntegration(c.Param("integrationId"))
	if !ok || value.RunID != c.Param("id") {
		c.JSON(http.StatusNotFound, gin.H{"error": "integration not found"})
		return
	}
	if err := h.engine.ValidateContributionIntegration(value); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	raw, size, ok := h.repo.RunnerContributionBlob(value.BundleSHA256)
	sum := sha256.Sum256(raw)
	if !ok || size != value.BundleSize || hex.EncodeToString(sum[:]) != value.BundleSHA256 {
		c.JSON(http.StatusConflict, gin.H{"error": "integration bytes unavailable"})
		return
	}
	c.Header("X-Contribution-Result-SHA", value.ResultSHA)
	c.Header("X-Contribution-SHA256", value.BundleSHA256)
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "application/x-git-bundle", raw)
}
