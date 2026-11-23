package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
	"agentflow/internal/teamconfig"
	"github.com/gin-gonic/gin"
)

type teamImportInput struct {
	RepositoryID string                     `json:"repositoryId"`
	Commit       string                     `json:"commit"`
	Root         string                     `json:"root"`
	Selection    domain.TeamImportSelection `json:"selection"`
}

type teamImportPublishInput struct {
	teamImportInput
	Name                  string                       `json:"name"`
	ExpectedBaseVersion   int                          `json:"expectedBaseVersion"`
	ExpectedPreviewSHA256 string                       `json:"expectedPreviewSha256"`
	Approve               bool                         `json:"approve"`
	UpdateFrom            *domain.TeamImportUpdateFrom `json:"updateFrom,omitempty"`
}

func decodeTeamImport(c *gin.Context, out any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10)
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		c.JSON(400, gin.H{"code": "team_import_input", "error": "invalid import request or unknown field"})
		return false
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		c.JSON(400, gin.H{"code": "team_import_input", "error": "one JSON object required"})
		return false
	}
	return true
}

func (h *intelligenceHandlers) previewTeamImport(c *gin.Context) {
	if _, ok := h.authorize(c, true); !ok {
		return
	}
	var input teamImportInput
	if !decodeTeamImport(c, &input) {
		return
	}
	preview, ok := h.readTeamImport(c, input)
	if !ok {
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(200, preview)
}

func (h *intelligenceHandlers) publishTeamImport(c *gin.Context) {
	actor, ok := h.authorize(c, true)
	if !ok {
		return
	}
	var input teamImportPublishInput
	if !decodeTeamImport(c, &input) {
		return
	}
	if !input.Approve || strings.TrimSpace(input.Name) == "" || len(input.Name) > 200 || input.ExpectedBaseVersion < 0 || len(input.ExpectedPreviewSHA256) != 64 {
		c.JSON(400, gin.H{"code": "team_import_input", "error": "explicit approval, bounded name, expected base and preview digest required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	var preview teamconfig.Preview
	var updates []domain.TeamImportUpdateFrom
	if input.UpdateFrom != nil {
		preview, ok = h.readTeamUpdateForPublish(c, input)
		updates = []domain.TeamImportUpdateFrom{*input.UpdateFrom}
	} else {
		preview, ok = h.readTeamImport(c, input.teamImportInput)
	}
	if !ok {
		return
	}
	if preview.SHA256 != input.ExpectedPreviewSHA256 {
		intelligenceError(c, repo.ErrIntelligenceConflict)
		return
	}
	value, err := h.repo.CreateImportedTeamRelease(c.Param("id"), input.Name, actor, input.ExpectedBaseVersion, preview.Payload, updates...)
	if err != nil {
		if errors.Is(err, repo.ErrTeamImportRefConflict) {
			teamUpdateError(c, teamconfig.ErrRefConflict)
			return
		}
		intelligenceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, value)
}

func (h *intelligenceHandlers) readTeamImport(c *gin.Context, input teamImportInput, refs ...string) (teamconfig.Preview, bool) {
	var empty teamconfig.Preview
	if !domain.ValidGitCommitSHA(input.Commit) || input.RepositoryID == "" || (input.Root != "." && !domain.ValidTeamImportPath(input.Root)) {
		c.JSON(400, gin.H{"code": "team_import_input", "error": "registered repositoryId, full commit and clean relative root required"})
		return empty, false
	}
	if err := teamconfig.ValidateSelection(input.Selection); err != nil {
		c.JSON(400, gin.H{"code": "team_import_input", "error": err.Error()})
		return empty, false
	}
	repository, ok := h.repo.Repository(input.RepositoryID)
	if !ok || repository.ProjectID != c.Param("id") {
		c.JSON(404, gin.H{"error": "registered project repository not found"})
		return empty, false
	}
	project, ok := h.repo.Project(c.Param("id"))
	if !ok {
		c.JSON(404, gin.H{"error": "project not found"})
		return empty, false
	}
	ctx := c.Request.Context()
	if project.OwnerID > 0 {
		ctx = gitwork.WithToken(ctx, h.repo.GitCredential(ctx, project.OwnerID))
	}
	files, err := teamconfig.Read(ctx, repository, input.Commit, input.Root, input.Selection)
	if err != nil {
		var diag teamconfig.Diagnostic
		switch {
		case errors.As(err, &diag):
			c.JSON(422, gin.H{"code": "team_import_unsupported", "error": diag.Error(), "diagnostics": []teamconfig.Diagnostic{diag}})
		case errors.Is(err, teamconfig.ErrSource), errors.Is(err, context.DeadlineExceeded):
			c.JSON(502, gin.H{"code": "team_import_source", "error": "could not read the registered source at the requested commit"})
		default:
			c.JSON(400, gin.H{"code": "team_import_input", "error": err.Error()})
		}
		return empty, false
	}
	preview, err := teamconfig.Parse(project.ID, repository, input.Commit, input.Root, input.Selection, files, refs...)
	if err != nil {
		c.JSON(422, gin.H{"code": "team_import_unsupported", "error": err.Error()})
		return empty, false
	}
	return preview, true
}
