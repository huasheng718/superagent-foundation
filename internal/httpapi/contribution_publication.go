package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *runsHandlers) contributionPublicationAllowed(c *gin.Context) bool {
	if authRequired() && !h.isPlatformAdmin(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "controller publication requires platform admin"})
		return false
	}
	value, ok := h.repo.ContributionIntegration(c.Param("integrationId"))
	if !ok || value.RunID != c.Param("id") {
		c.JSON(http.StatusNotFound, gin.H{"error": "integration not found"})
		return false
	}
	return true
}

func (h *runsHandlers) recordContributionGate(c *gin.Context) {
	if !h.contributionPublicationAllowed(c) {
		return
	}
	var input struct {
		GateRunID  string `json:"gateRunId"`
		NodeID     string `json:"nodeId"`
		Kind       string `json:"kind"`
		ArtifactID string `json:"artifactId"`
	}
	if !decode(c, &input) {
		return
	}
	value, err := h.engine.RecordContributionGateFromRun(c.Param("integrationId"), input.GateRunID, input.NodeID, input.Kind, input.ArtifactID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *runsHandlers) publishContribution(c *gin.Context) {
	if !h.contributionPublicationAllowed(c) {
		return
	}
	value, err := h.engine.PublishContribution(c.Request.Context(), c.Param("integrationId"))
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *runsHandlers) contributionPublication(c *gin.Context) {
	if !h.contributionPublicationAllowed(c) {
		return
	}
	value, ok := h.repo.ContributionPublication(c.Param("integrationId"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "publication not found"})
		return
	}
	c.JSON(http.StatusOK, value)
}
