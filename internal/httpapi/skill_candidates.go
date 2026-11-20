package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
)

// Candidate reads, quarantine submissions and human decisions have their own
// handler; governance policy is explicitly injected from the existing owner.
type skillCandidateHandlers struct {
	*deps
	authorizeProject func(*gin.Context, string, bool) (string, bool)
}

func skillCandidateError(c *gin.Context, err error) {
	if errors.Is(err, repo.ErrSkillExists) {
		c.JSON(http.StatusConflict, gin.H{"error": "active skill identity already exists", "code": "intelligence_conflict"})
		return
	}
	intelligenceError(c, err)
}
func (h *skillCandidateHandlers) candidates(c *gin.Context) {
	actor, ok := h.authorizeProject(c, c.Param("id"), false)
	if !ok {
		return
	}
	c.Header("Cache-Control", "private, no-store")
	if c.Request.Method == http.MethodGet {
		values, err := h.repo.SkillCandidates(c.Param("id"))
		if err != nil {
			skillCandidateError(c, err)
			return
		}
		out := []gin.H{}
		for _, v := range values {
			out = append(out, gin.H{"id": v.ID, "projectId": v.ProjectID, "state": v.State, "revision": v.Revision, "sha256": v.SHA256, "skillId": v.Package.Skill.ID, "name": v.Package.Skill.Name, "publisher": v.Package.Publisher, "license": v.Package.License, "source": v.Package.Source, "createdBy": v.CreatedBy, "createdAt": v.CreatedAt, "promotedSkillId": v.PromotedSkillID, "decisions": v.Decisions})
		}
		c.JSON(http.StatusOK, out)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, domain.IntelligenceMaxSnapshotBytes+(1<<20))
	var input domain.SkillCandidatePackage
	if !decode(c, &input) {
		return
	}
	// Provenance is evidence, not an authorization grant. A supplied Run/artifact
	// must still belong to the same project before its reference can be retained.
	var sourceRun *domain.Run
	if input.Source.RunID != "" {
		run, found := h.repo.RunState(input.Source.RunID)
		if !found || run.ExecutionContext.ProjectID != c.Param("id") {
			c.JSON(http.StatusNotFound, gin.H{"error": "source Run is unavailable in this project"})
			return
		}
		sourceRun = &run
		if input.Source.ArtifactID != "" {
			if _, found := h.repo.Artifact(run.ID, input.Source.ArtifactID); !found {
				c.JSON(http.StatusNotFound, gin.H{"error": "source artifact unavailable"})
				return
			}
		}
	}
	var value domain.SkillCandidate
	var err error
	if sourceRun != nil {
		value, err = h.repo.CreateSkillCandidateFromRun(c.Param("id"), input, actor, *sourceRun)
	} else {
		value, err = h.repo.CreateSkillCandidate(c.Param("id"), input, actor)
	}
	if err != nil {
		skillCandidateError(c, err)
		return
	}
	c.JSON(http.StatusCreated, value)
}
func (h *skillCandidateHandlers) candidate(c *gin.Context) {
	if _, ok := h.authorizeProject(c, c.Param("id"), false); !ok {
		return
	}
	c.Header("Cache-Control", "private, no-store")
	value, err := h.repo.SkillCandidate(c.Param("id"), c.Param("candidateId"))
	if err != nil {
		skillCandidateError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *skillCandidateHandlers) decide(c *gin.Context) {
	actor, ok := h.authorizeProject(c, c.Param("id"), true)
	if !ok {
		return
	}
	var input struct {
		ExpectedRevision int    `json:"expectedRevision"`
		Decision         string `json:"decision"`
		Reason           string `json:"reason"`
	}
	if !decode(c, &input) {
		return
	}
	value, err := h.repo.DecideSkillCandidate(c.Param("id"), c.Param("candidateId"), input.ExpectedRevision, input.Decision, actor, input.Reason)
	if err != nil {
		skillCandidateError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
