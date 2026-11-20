package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/memoryprovider"
	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
)

func memoryProviderError(c *gin.Context, err error) {
	status := http.StatusServiceUnavailable
	code := "memory_provider_unavailable"
	switch {
	case errors.Is(err, memoryprovider.ErrSyncRequired):
		code = "memory_provider_sync_required"
	case errors.Is(err, memoryprovider.ErrIntegrity):
		status = http.StatusBadGateway
		code = "memory_provider_integrity"
	case errors.Is(err, memoryprovider.ErrBudget), errors.Is(err, repo.ErrMemoryIndexCapacity):
		status = http.StatusUnprocessableEntity
		code = "memory_provider_capacity"
	}
	c.JSON(status, gin.H{"error": code, "code": code})
}
func (h *intelligenceHandlers) memoryProviderScope(c *gin.Context) (domain.MemoryProviderScope, bool) {
	if !authRequired() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "private memory provider requires AGENTFLOW_AUTH_REQUIRED=true"})
		return domain.MemoryProviderScope{}, false
	}
	for name := range c.Request.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-tdai-") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "caller memory provider scope headers are forbidden"})
			return domain.MemoryProviderScope{}, false
		}
	}
	user, ok := h.requestUser(c.Request, true)
	if !ok || user == nil || user.ID <= 0 || user.IsGuest {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated human user required"})
		return domain.MemoryProviderScope{}, false
	}
	// Project authorization is always performed by the endpoint before this scope.
	return domain.MemoryProviderScope{ProjectID: c.Param("id"), UserID: user.ID, AgentID: "project-ui"}, true
}
func (h *intelligenceHandlers) syncMemoryProvider(c *gin.Context) {
	if _, ok := h.authorize(c, false); !ok {
		return
	}
	var input struct {
		Limit int `json:"limit"`
	}
	if !decodeGovernedInput(c, &input) {
		return
	}
	scope, ok := h.memoryProviderScope(c)
	if !ok {
		return
	}
	client, err := memoryprovider.FromEnv()
	if err != nil || client == nil {
		memoryProviderError(c, memoryprovider.ErrUnavailable)
		return
	}
	out, err := (memoryprovider.Service{Client: client, Memory: h.repo.MemoryRepo}).Sync(c.Request.Context(), scope, input.Limit)
	if err != nil {
		memoryProviderError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}
