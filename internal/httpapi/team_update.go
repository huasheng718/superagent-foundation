package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
	"agentflow/internal/teamconfig"
	"github.com/gin-gonic/gin"
)

type teamUpdatePreview struct {
	Status              string                  `json:"status"`
	BaseReleaseID       string                  `json:"baseReleaseId"`
	BaseReleaseSHA256   string                  `json:"baseReleaseSha256"`
	ExpectedBaseVersion int                     `json:"expectedBaseVersion"`
	Ref                 string                  `json:"ref"`
	Commit              string                  `json:"commit"`
	Preview             *teamconfig.Preview     `json:"preview"`
	Changes             []teamconfig.FileChange `json:"changes"`
}

func teamUpdateError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, teamconfig.ErrRefConflict), errors.Is(err, repo.ErrTeamImportRefConflict):
		c.JSON(409, gin.H{"code": "team_import_ref_conflict", "error": "registered source branch changed or disappeared; check and review again"})
	case errors.Is(err, teamconfig.ErrSource), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		c.JSON(502, gin.H{"code": "team_import_source", "error": "could not read the registered source branch"})
	default:
		c.JSON(400, gin.H{"code": "team_import_input", "error": err.Error()})
	}
}

// loadTeamUpdateSource derives source selection from an immutable reviewed
// release. Ref is optional only for the first check, never during publication.
func (h *intelligenceHandlers) loadTeamUpdateSource(c *gin.Context, from domain.TeamImportUpdateFrom) (domain.TeamRelease, domain.Repository, bool) {
	var repository domain.Repository
	source, err := h.repo.TeamRelease(c.Param("id"), from.ReleaseID)
	if err != nil {
		intelligenceError(c, err)
		return source, repository, false
	}
	if source.State != domain.TeamReleasePublished || source.SHA256 != from.ReleaseSHA256 {
		intelligenceError(c, repo.ErrIntelligenceConflict)
		return source, repository, false
	}
	if source.Payload.GitImport == nil {
		c.JSON(400, gin.H{"code": "team_import_input", "error": "published Git import source required"})
		return source, repository, false
	}
	var ok bool
	repository, ok = h.repo.Repository(source.Payload.GitImport.RepositoryID)
	if !ok || repository.ProjectID != c.Param("id") {
		c.JSON(404, gin.H{"error": "registered project repository not found"})
		return source, repository, false
	}
	ref := "refs/heads/" + repository.DefaultBranch
	if repository.CloneURL != source.Payload.Source.Repository || from.Ref != "" && from.Ref != ref {
		teamUpdateError(c, teamconfig.ErrRefConflict)
		return source, repository, false
	}
	if !domain.ValidTeamImportRef(ref) {
		c.JSON(400, gin.H{"code": "team_import_input", "error": "registered defaultBranch must be a valid heads branch"})
		return source, repository, false
	}
	return source, repository, true
}

func (h *intelligenceHandlers) resolveTeamUpdate(c *gin.Context, repository domain.Repository) (string, string, bool) {
	project, ok := h.repo.Project(c.Param("id"))
	if !ok {
		c.JSON(404, gin.H{"error": "project not found"})
		return "", "", false
	}
	ctx := c.Request.Context()
	if project.OwnerID > 0 {
		ctx = gitwork.WithToken(ctx, h.repo.GitCredential(ctx, project.OwnerID))
	}
	ref, commit, err := teamconfig.ResolveRegisteredBranch(ctx, repository)
	if err != nil {
		teamUpdateError(c, err)
		return "", "", false
	}
	return ref, commit, true
}

func (h *intelligenceHandlers) verifyTeamUpdateRef(c *gin.Context, from domain.TeamImportUpdateFrom, commit string) bool {
	_, repository, ok := h.loadTeamUpdateSource(c, from)
	if !ok {
		return false
	}
	ref, current, ok := h.resolveTeamUpdate(c, repository)
	if !ok {
		return false
	}
	if ref != from.Ref || current != commit {
		teamUpdateError(c, teamconfig.ErrRefConflict)
		return false
	}
	return true
}

func (h *intelligenceHandlers) previewTeamUpdate(c *gin.Context) {
	if _, ok := h.authorize(c, true); !ok {
		return
	}
	var input struct {
		ExpectedReleaseSHA256 string `json:"expectedReleaseSha256"`
	}
	if !decodeTeamImport(c, &input) {
		return
	}
	if len(input.ExpectedReleaseSHA256) != 64 {
		c.JSON(400, gin.H{"code": "team_import_input", "error": "reviewed source release digest required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	from := domain.TeamImportUpdateFrom{ReleaseID: c.Param("releaseId"), ReleaseSHA256: input.ExpectedReleaseSHA256}
	source, repository, ok := h.loadTeamUpdateSource(c, from)
	if !ok {
		return
	}
	releases, err := h.repo.TeamReleases(c.Param("id"))
	if err != nil {
		intelligenceError(c, err)
		return
	}
	baseVersion := 0
	for _, release := range releases {
		if release.Version > baseVersion {
			baseVersion = release.Version
		}
	}
	ref, commit, ok := h.resolveTeamUpdate(c, repository)
	if !ok {
		return
	}
	from.Ref = ref
	result := teamUpdatePreview{Status: "unchanged", BaseReleaseID: source.ID, BaseReleaseSHA256: source.SHA256, ExpectedBaseVersion: baseVersion, Ref: ref, Commit: commit, Changes: []teamconfig.FileChange{}}
	if commit != source.Payload.GitImport.Commit {
		m := source.Payload.GitImport
		preview, ok := h.readTeamImport(c, teamImportInput{RepositoryID: m.RepositoryID, Commit: commit, Root: m.Root, Selection: m.Selection}, ref)
		if !ok {
			return
		}
		if preview.Payload.Source.Repository != source.Payload.Source.Repository {
			teamUpdateError(c, teamconfig.ErrRefConflict)
			return
		}
		if err := retainTeamUpdateDistribution(source.Payload, &preview); err != nil {
			intelligenceError(c, err)
			return
		}
		result.Status = "updated"
		result.Preview = &preview
		result.Changes = teamconfig.Changes(m.Files, preview.Payload.GitImport.Files)
	}
	if !h.verifyTeamUpdateRef(c, from, commit) {
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, result)
}

func (h *intelligenceHandlers) readTeamUpdateForPublish(c *gin.Context, input teamImportPublishInput) (teamconfig.Preview, bool) {
	var empty teamconfig.Preview
	from := *input.UpdateFrom
	if from.ReleaseID == "" || len(from.ReleaseSHA256) != 64 || !domain.ValidTeamImportRef(from.Ref) {
		c.JSON(400, gin.H{"code": "team_import_input", "error": "reviewed update source release and exact heads ref required"})
		return empty, false
	}
	source, repository, ok := h.loadTeamUpdateSource(c, from)
	if !ok {
		return empty, false
	}
	m := source.Payload.GitImport
	if input.RepositoryID != m.RepositoryID || input.Root != m.Root || !domain.EqualTeamImportSelection(input.Selection, m.Selection) || input.Commit == m.Commit {
		intelligenceError(c, repo.ErrIntelligenceConflict)
		return empty, false
	}
	ref, commit, ok := h.resolveTeamUpdate(c, repository)
	if !ok {
		return empty, false
	}
	if ref != from.Ref || commit != input.Commit {
		teamUpdateError(c, teamconfig.ErrRefConflict)
		return empty, false
	}
	preview, ok := h.readTeamImport(c, input.teamImportInput, ref)
	if !ok {
		return empty, false
	}
	if err := retainTeamUpdateDistribution(source.Payload, &preview); err != nil {
		intelligenceError(c, err)
		return empty, false
	}
	if !h.verifyTeamUpdateRef(c, from, input.Commit) {
		return empty, false
	}
	return preview, true
}

func retainTeamUpdateDistribution(base domain.TeamReleasePayload, preview *teamconfig.Preview) error {
	if err := domain.PreserveTeamDistribution(base, &preview.Payload); err != nil {
		return err
	}
	var err error
	preview.SHA256, err = domain.IntelligenceDigest(preview.Payload)
	return err
}
