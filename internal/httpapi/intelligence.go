package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/memoryprovider"
	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
)

// intelligenceHandlers owns formal configuration, governed memory decisions
// and authorized context previews. Runner sessions are never governance actors.
type intelligenceHandlers struct{ *deps }

func (h *intelligenceHandlers) defaultUpgrades(c *gin.Context) {
	if _, ok := h.authorize(c, false); !ok {
		return
	}
	values, err := h.repo.DefaultUpgrades(c.Param("id"))
	if err != nil {
		intelligenceError(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, values)
}

func (h *intelligenceHandlers) authorize(c *gin.Context, manage bool) (string, bool) {
	return h.authorizeProject(c, strings.TrimSpace(c.Param("id")), manage)
}

func (h *intelligenceHandlers) authorizeProject(c *gin.Context, projectID string, manage bool) (string, bool) {
	p, ok := h.repo.Project(projectID)
	if !ok || projectID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return "", false
	}
	if !authRequired() {
		return "local", true
	}
	user, ok := h.requestUser(c.Request, true)
	if !ok || user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated human user required"})
		return "", false
	}
	admin := h.isPlatformAdmin(c.Request)
	allowed := admin || p.OwnerID == user.ID
	if !manage && !allowed {
		for _, member := range p.Members {
			if member.UserID == user.ID {
				allowed = true
				break
			}
		}
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "project membership required; only owner or administrator may publish or revoke"})
		return "", false
	}
	return user.LoginName, true
}

// Governed Runs contain reviewed memory and exact configuration bytes; legacy
// globally readable Run routes must not bypass their project's human access.
// Runner task/claim/event transports have separate routes and authorization.
func (h *intelligenceHandlers) governedRunReadGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if !authRequired() || (path != "/api/runs/:id" && !strings.HasPrefix(path, "/api/runs/:id/")) {
			c.Next()
			return
		}
		run, ok := h.repo.RunState(c.Param("id"))
		if !ok || (run.ExecutionContext.TeamContext == nil && run.Intelligence == nil) {
			c.Next()
			return
		}
		projectID := strings.TrimSpace(run.ExecutionContext.ProjectID)
		if projectID == "" || (run.Intelligence != nil && run.Intelligence.ProjectID != projectID) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "governed Run project scope is unavailable"})
			return
		}
		if _, ok := h.authorizeProject(c, projectID, false); !ok {
			c.Abort()
			return
		}
		c.Header("Cache-Control", "private, no-store")
		c.Next()
	}
}
func intelligenceError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, domain.ErrDistributionDenied) {
		status = http.StatusForbidden
	} else if errors.Is(err, repo.ErrIntelligenceUnavailable) {
		status = http.StatusNotFound
	} else if errors.Is(err, repo.ErrIntelligenceConflict) {
		status = http.StatusConflict
	}
	body := gin.H{"error": err.Error()}
	if status == http.StatusConflict {
		body["code"] = "intelligence_conflict"
	}
	c.JSON(status, body)
}
func (h *intelligenceHandlers) teamReleases(c *gin.Context) {
	actor, ok := h.authorize(c, c.Request.Method == http.MethodPost)
	if !ok {
		return
	}
	projectID := c.Param("id")
	if c.Request.Method == http.MethodGet {
		values, err := h.repo.TeamReleases(projectID)
		if err != nil {
			intelligenceError(c, err)
			return
		}
		// Lists contain bounded metadata; exact resource bytes are available from
		// the scoped release detail endpoint, never from a global catalog listing.
		out := []gin.H{}
		for _, v := range values {
			out = append(out, gin.H{"id": v.ID, "projectId": v.ProjectID, "name": v.Name, "version": v.Version, "revision": v.Revision, "state": v.State, "sha256": v.SHA256, "createdBy": v.CreatedBy, "createdAt": v.CreatedAt, "decisions": v.Decisions, "agentCount": len(v.Payload.Agents), "ruleCount": len(v.Payload.Assets.Rules), "skillCount": len(v.Payload.Assets.Skills), "mcpCount": len(v.Payload.Assets.MCPServers)})
		}
		c.JSON(http.StatusOK, out)
		return
	}
	var input domain.TeamReleaseRequest
	if !decode(c, &input) {
		return
	}
	value, err := h.repo.CreateTeamRelease(projectID, input, actor)
	if err != nil {
		intelligenceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, value)
}
func (h *intelligenceHandlers) teamRelease(c *gin.Context) {
	if _, ok := h.authorize(c, false); !ok {
		return
	}
	v, err := h.repo.TeamRelease(c.Param("id"), c.Param("releaseId"))
	if err != nil {
		intelligenceError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func (h *intelligenceHandlers) revokeTeamRelease(c *gin.Context) {
	actor, ok := h.authorize(c, true)
	if !ok {
		return
	}
	var input struct {
		ExpectedRevision int    `json:"expectedRevision"`
		Reason           string `json:"reason"`
	}
	if !decode(c, &input) {
		return
	}
	v, err := h.repo.RevokeTeamRelease(c.Param("id"), c.Param("releaseId"), input.ExpectedRevision, actor, input.Reason)
	if err != nil {
		intelligenceError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func (h *intelligenceHandlers) memories(c *gin.Context) {
	actor, ok := h.authorize(c, false)
	if !ok {
		return
	}
	projectID := c.Param("id")
	if c.Request.Method == http.MethodGet {
		out, err := h.repo.GovernedMemories(projectID)
		if err != nil {
			intelligenceError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
		return
	}
	// Decode only candidate content; callers cannot submit state, revision,
	// approval identity or a forged digest through the candidate endpoint.
	var input struct {
		Title     string                    `json:"title"`
		Content   string                    `json:"content"`
		Source    domain.IntelligenceSource `json:"source"`
		ExpiresAt *time.Time                `json:"expiresAt,omitempty"`
	}
	if !decodeGovernedInput(c, &input) {
		return
	}
	candidate := domain.GovernedMemory{Title: input.Title, Content: input.Content, Source: input.Source, ExpiresAt: input.ExpiresAt}
	v, err := h.repo.CreateMemoryCandidate(projectID, candidate, actor)
	if err != nil {
		intelligenceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, v)
}
func (h *intelligenceHandlers) decideMemory(c *gin.Context) {
	actor, ok := h.authorize(c, true)
	if !ok {
		return
	}
	var input struct {
		Decision         string `json:"decision"`
		ExpectedRevision int    `json:"expectedRevision"`
		Reason           string `json:"reason"`
	}
	if !decodeGovernedInput(c, &input) {
		return
	}
	v, err := h.repo.DecideGovernedMemory(c.Param("id"), c.Param("memoryId"), input.ExpectedRevision, input.Decision, actor, input.Reason)
	if err != nil {
		intelligenceError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func (h *intelligenceHandlers) recallMemory(c *gin.Context) {
	if _, ok := h.authorize(c, false); !ok {
		return
	}
	var input struct {
		Query          string `json:"query"`
		Limit          int    `json:"limit"`
		IncludeReceipt bool   `json:"includeReceipt"`
	}
	if !decodeGovernedInput(c, &input) {
		return
	}
	client, err := memoryprovider.FromEnv()
	if err != nil {
		memoryProviderError(c, err)
		return
	}
	if client != nil {
		scope, ok := h.memoryProviderScope(c)
		if !ok {
			return
		}
		values, receipt, err := (memoryprovider.Service{Client: client, Memory: h.repo.MemoryRepo}).Recall(c.Request.Context(), scope, input.Query, input.Limit)
		if err != nil {
			memoryProviderError(c, err)
			return
		}
		if input.IncludeReceipt {
			c.JSON(http.StatusOK, gin.H{"items": values, "receipt": receipt})
		} else {
			c.JSON(http.StatusOK, values)
		}
		return
	}
	if input.IncludeReceipt {
		c.JSON(http.StatusBadRequest, gin.H{"error": "memory provider receipts require the configured private provider"})
		return
	}
	v, err := h.repo.RecallGovernedMemory(c.Param("id"), input.Query, input.Limit)
	if err != nil {
		intelligenceError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func (h *intelligenceHandlers) resolveContext(c *gin.Context) {
	if _, ok := h.authorize(c, false); !ok {
		return
	}
	var input struct {
		ReleaseID             string            `json:"releaseId"`
		MemoryRecallReceiptID string            `json:"memoryRecallReceiptId"`
		MemoryIDs             []string          `json:"memoryIds"`
		RepositoryBaseSHAs    map[string]string `json:"repositoryBaseSHAs"`
	}
	if !decodeGovernedInput(c, &input) {
		return
	}
	var userID int64
	if user, ok := h.requestUser(c.Request, true); ok && user != nil {
		userID = user.ID
	}
	v, err := h.engine.ResolveIntelligenceContext(c.Param("id"), domain.TeamContextSelection{ReleaseID: input.ReleaseID, MemoryIDs: input.MemoryIDs, MemoryRecallReceiptID: input.MemoryRecallReceiptID}, input.RepositoryBaseSHAs, userID)
	if err != nil {
		intelligenceError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (h *intelligenceHandlers) restoreTeamRelease(c *gin.Context) {
	actor, ok := h.authorize(c, true)
	if !ok {
		return
	}
	var input struct {
		ExpectedBaseVersion int    `json:"expectedBaseVersion"`
		Name                string `json:"name"`
		Reason              string `json:"reason"`
	}
	if !decode(c, &input) {
		return
	}
	v, err := h.repo.RestoreTeamRelease(c.Param("id"), c.Param("releaseId"), input.Name, actor, input.Reason, input.ExpectedBaseVersion)
	if err != nil {
		intelligenceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, v)
}
