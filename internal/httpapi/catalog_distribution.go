package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Source distribution decisions are owner governance, independent of normal
// project writer permission or permission to inspect/execute the source.
func (d deps) decideDistribution(c *gin.Context, kind string) {
	expected, ok := sourceEditPrecondition(c, true)
	if !ok {
		return
	}
	var req struct {
		Policy domain.DistributionPolicy `json:"policy"`
		Reason string                    `json:"reason"`
	}
	if !decode(c, &req) {
		return
	}
	actor := repo.DistributionActor{Local: !authRequired()}
	if !actor.Local {
		user, authenticated := d.requestUser(c.Request, true)
		if !authenticated || user == nil {
			c.JSON(403, gin.H{"error": "human source owner required"})
			return
		}
		actor.UserID = user.ID
		actor.LoginName = user.LoginName
		actor.PlatformAdmin = d.isPlatformAdmin(c.Request)
	}
	out, err := d.repo.SetDistribution(kind, strings.TrimSpace(c.Param("id")), expected, req.Policy, req.Reason, actor)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(404, gin.H{"error": "source not found"})
		return
	}
	if err != nil {
		respond(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("ETag", `"`+out.SourceRevision+`"`)
	c.JSON(http.StatusOK, out)
}
func (h *catalogHandlers) distribution(c *gin.Context) {
	kind := ""
	switch c.FullPath() {
	case "/api/agents/:id/distribution":
		kind = "agent"
	case "/api/rules/:id/distribution":
		kind = "rule"
	case "/api/skills/:id/distribution":
		kind = "skill"
	}
	h.decideDistribution(c, kind)
}

// Initial human definitions cannot manufacture server-owned derivation chains
// or bypass owner policy decisions through a normal metadata PUT.
func ordinaryDistributionCreate(c *gin.Context, state domain.DistributionState, exists bool) bool {
	if !exists && (state.Distribution != nil || len(state.DistributionOrigins) > 0) {
		sourceEditError(c, domain.ErrDistributionDenied)
		return false
	}
	return true
}
