package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"agentflow/internal/domain"
	"github.com/gin-gonic/gin"
)

// catalogReadPredicate preserves public platform presets and explicit sharing,
// while project-owned source bytes require actual human membership. Permission
// to execute a formal release is never consulted to authorize its source.
func (d deps) catalogReadPredicate(c *gin.Context) func(string, bool) bool {
	if !authRequired() {
		return func(string, bool) bool { return true }
	}
	c.Header("Cache-Control", "private, no-store")
	if d.isPlatformAdmin(c.Request) {
		return func(string, bool) bool { return true }
	}
	projects := map[string]bool{}
	if user, ok := d.requestUser(c.Request, true); ok && user != nil {
		for _, p := range d.repo.Projects() {
			if p.OwnerID == user.ID {
				projects[p.ID] = true
				continue
			}
			for _, m := range p.Members {
				if m.UserID == user.ID {
					projects[p.ID] = true
					break
				}
			}
		}
	}
	return func(projectID string, shared bool) bool {
		return shared || strings.TrimSpace(projectID) == "" || projects[projectID]
	}
}

func (d deps) requireCatalogRead(c *gin.Context, projectID string, shared bool) bool {
	if d.catalogReadPredicate(c)(projectID, shared) {
		return true
	}
	c.JSON(http.StatusForbidden, gin.H{"error": "project membership required to read or copy source assets"})
	return false
}

// Filtering lists (including bootstrap) uses the same predicate as detail and
// download routes. Do not serialize a private body and hide it only in the UI.
func filterCatalogRead[T any](items []T, allowed func(string, bool) bool, scope func(T) (string, bool)) []T {
	out := make([]T, 0, len(items))
	for _, item := range items {
		project, shared := scope(item)
		if allowed(project, shared) {
			out = append(out, item)
		}
	}
	return out
}
func agentReadScope(v domain.AgentDefinition) (string, bool) { return v.ProjectID, v.Shared }
func ruleReadScope(v domain.RuleDefinition) (string, bool)   { return v.ProjectID, v.Shared }
func skillReadScope(v domain.SkillDefinition) (string, bool) { return v.ProjectID, v.Shared }
func workflowReadScope(v domain.WorkflowDefinition) (string, bool) {
	return v.ProjectID, v.Shared || v.Generic
}
func dockerfileReadScope(v domain.DockerfileDefinition) (string, bool) { return v.ProjectID, v.Shared }
func environmentReadScope(v domain.EnvironmentDefinition) (string, bool) {
	return v.ProjectID, v.Shared
}
func sandboxReadScope(v domain.SandboxProfile) (string, bool) { return v.ProjectID, v.Shared }

func (d deps) readableMCPServers(c *gin.Context, items []domain.MCPServerDefinition) []domain.MCPServerDefinition {
	userID := int64(0)
	if user, ok := d.requestUser(c.Request, true); ok && user != nil {
		userID = user.ID
	}
	items = visibleMCPServersForUser(items, userID)
	return filterCatalogRead(items, d.catalogReadPredicate(c), func(v domain.MCPServerDefinition) (string, bool) {
		if v.Scope == "personal" {
			return "", false
		}
		return v.ProjectID, false
	})
}

// A newly shared definition does not retroactively publish formerly private
// revision bodies. Each historical source carries its own original scope.
func catalogRevisionVisible(rev domain.DefinitionRevision, kind, id string, allowed func(string, bool) bool) bool {
	if rev.Kind != kind || rev.EntityID != id {
		return false
	}
	switch kind {
	case "agent":
		var item domain.AgentDefinition
		if json.Unmarshal(rev.Payload, &item) != nil || item.ID != id {
			return false
		}
		return allowed(item.ProjectID, item.Shared)
	case "workflow":
		var item domain.WorkflowDefinition
		if json.Unmarshal(rev.Payload, &item) != nil || item.ID != id {
			return false
		}
		return allowed(item.ProjectID, item.Shared || item.Generic)
	}
	return false
}
func (d deps) readableCatalogRevisions(c *gin.Context, kind, id string) []domain.DefinitionRevision {
	allowed := d.catalogReadPredicate(c)
	out := []domain.DefinitionRevision{}
	for _, rev := range d.repo.Revisions(kind, id) {
		if catalogRevisionVisible(rev, kind, id, allowed) {
			out = append(out, rev)
		}
	}
	return out
}

// A shared role is not a blanket source-copy grant for every referenced asset.
// Preflight the whole closure before clone helpers can create or share rows.
func (h *catalogHandlers) allowAgentCloneSources(c *gin.Context, agent domain.AgentDefinition, targetProject string) bool {
	if !authRequired() {
		return true
	}
	allowed := h.catalogReadPredicate(c)
	deny := func() bool {
		c.JSON(http.StatusForbidden, gin.H{"error": "source asset permission required to copy this role"})
		return false
	}
	for _, id := range agent.Rules {
		if item, ok := h.repo.Rule(id); ok && !allowed(item.ProjectID, item.Shared) {
			return deny()
		}
	}
	for _, id := range agent.Skills {
		if item, ok := h.repo.Skill(id); ok && !allowed(item.ProjectID, item.Shared) {
			return deny()
		}
	}
	userID := int64(0)
	if user, ok := h.requestUser(c.Request, true); ok && user != nil {
		userID = user.ID
	}
	for _, id := range agent.MCPServers {
		source, ok := h.repo.MCPServer(id)
		if !ok || source.Scope == "" {
			continue
		}
		if source.Scope == "personal" {
			if userID <= 0 || source.OwnerUserID != userID {
				return deny()
			}
			continue
		}
		if source.Scope != "project" {
			continue
		}
		if source.ProjectID == targetProject {
			continue
		}
		targetBound := false
		for _, item := range h.repo.MCPServers() {
			if source.MarketID != "" && item.Scope == "project" && item.ProjectID == targetProject && item.MarketID == source.MarketID {
				targetBound = true
				break
			}
		}
		// Reuse an independently installed target market binding, without reading
		// or copying the source connection's credentials or file tree.
		if targetBound {
			continue
		}
		if !allowed(source.ProjectID, false) {
			return deny()
		}
		urlValue, urlErr := url.Parse(source.URL)
		if len(source.Environment) > 0 || len(source.Headers) > 0 || source.AuthType != "" || source.OwnerUserID != 0 || urlErr != nil || urlValue.User != nil || urlValue.RawQuery != "" || urlValue.Fragment != "" {
			c.JSON(http.StatusConflict, gin.H{"error": "credential-bound MCP cannot be copied across scopes; configure an independent target binding"})
			return false
		}
	}
	return true
}
