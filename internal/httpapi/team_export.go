package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
	"agentflow/internal/teamconfig"
	"github.com/gin-gonic/gin"
)

func (h *intelligenceHandlers) previewTeamExport(c *gin.Context) {
	if _, ok := h.authorize(c, true); !ok {
		return
	}
	var input struct {
		ExpectedReleaseSHA256 string                   `json:"expectedReleaseSha256"`
		Changes               []teamconfig.Replacement `json:"changes"`
	}
	d := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 9<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil || d.Decode(new(any)) != io.EOF || len(input.ExpectedReleaseSHA256) != 64 {
		c.JSON(400, gin.H{"error": "one bounded export request with release digest and replacements required"})
		return
	}
	release, err := h.repo.TeamRelease(c.Param("id"), c.Param("releaseId"))
	if err != nil {
		intelligenceError(c, err)
		return
	}
	if input.ExpectedReleaseSHA256 != release.SHA256 {
		intelligenceError(c, repo.ErrIntelligenceConflict)
		return
	}
	m := release.Payload.GitImport
	if m == nil {
		c.JSON(400, gin.H{"error": "release has no verified TeamAI Git source"})
		return
	}
	repository, ok := h.repo.Repository(m.RepositoryID)
	if !ok || repository.ProjectID != c.Param("id") {
		c.JSON(404, gin.H{"error": "registered project repository not found"})
		return
	}
	project, ok := h.repo.Project(c.Param("id"))
	if !ok {
		c.JSON(404, gin.H{"error": "project not found"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	if project.OwnerID > 0 {
		ctx = gitwork.WithToken(ctx, h.repo.GitCredential(ctx, project.OwnerID))
	}
	preview, err := teamconfig.Export(ctx, repository, release, input.Changes)
	if err != nil {
		if errors.Is(err, domain.ErrDistributionDenied) {
			c.JSON(403, gin.H{"code": "distribution_denied", "error": err.Error()})
			return
		}
		if errors.Is(err, teamconfig.ErrSource) || errors.Is(err, context.DeadlineExceeded) {
			c.JSON(502, gin.H{"error": "could not read exact registered source for export"})
			return
		}
		c.JSON(422, gin.H{"error": err.Error()})
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(200, preview)
}
