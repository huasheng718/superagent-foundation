package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/teamconfig"
	"github.com/gin-gonic/gin"
)

type teamDeliveryHandlers struct {
	*deps
	authorizeProject func(*gin.Context, string, bool) (string, bool)
}

func teamProposalError(c *gin.Context, err error) {
	status, code := 422, "team_git_proposal_invalid"
	switch {
	case errors.Is(err, domain.ErrDistributionDenied):
		status, code = 403, "distribution_denied"
	case errors.Is(err, repo.ErrIntelligenceUnavailable):
		status, code = 404, "team_git_proposal_unavailable"
	case errors.Is(err, repo.ErrTeamGitProposalConflict), errors.Is(err, repo.ErrIntelligenceConflict), errors.Is(err, repo.ErrContributionPublicationLeaseLost), errors.Is(err, repo.ErrContributionPRCreateUncertain):
		status, code = 409, "team_git_proposal_conflict"
	case errors.Is(err, engine.ErrTeamGitSourceStale), errors.Is(err, teamconfig.ErrRefConflict):
		status, code = 409, "teamai_source_stale"
	case errors.Is(err, teamconfig.ErrSource):
		status, code = 502, "team_git_source_unavailable"
	}
	c.JSON(status, gin.H{"code": code, "error": err.Error()})
}

func decodeTeamProposal(c *gin.Context, out any, max int64) bool {
	d := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, max))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		c.JSON(400, gin.H{"code": "team_git_proposal_input", "error": "one bounded JSON object with documented fields required"})
		return false
	}
	return true
}

func (h *teamDeliveryHandlers) list(c *gin.Context) {
	if _, ok := h.authorizeProject(c, c.Param("id"), true); !ok {
		return
	}
	values, err := h.repo.TeamGitProposals(c.Param("id"))
	if err != nil {
		teamProposalError(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(200, values)
}
func (h *teamDeliveryHandlers) detail(c *gin.Context) {
	if _, ok := h.authorizeProject(c, c.Param("id"), true); !ok {
		return
	}
	v, err := h.repo.TeamGitProposal(c.Param("id"), c.Param("proposalId"))
	if err != nil {
		teamProposalError(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(200, v)
}
func (h *teamDeliveryHandlers) prepare(c *gin.Context) {
	actor, ok := h.authorizeProject(c, c.Param("id"), true)
	if !ok {
		return
	}
	var input struct {
		ReleaseID  string                   `json:"releaseId"`
		ReleaseSHA string                   `json:"expectedReleaseSha256"`
		Base       string                   `json:"expectedBaseCommit"`
		PatchSHA   string                   `json:"expectedPatchSha256"`
		Changes    []teamconfig.Replacement `json:"changes"`
	}
	if !decodeTeamProposal(c, &input, 9<<20) {
		return
	}
	if len(input.ReleaseSHA) != 64 || len(input.PatchSHA) != 64 || !domain.ValidGitCommitSHA(input.Base) {
		c.JSON(400, gin.H{"error": "reviewed release, full base commit and patch digest required"})
		return
	}
	v, err := h.engine.PrepareTeamGitProposal(c.Request.Context(), c.Param("id"), input.ReleaseID, input.ReleaseSHA, input.Base, input.PatchSHA, actor, input.Changes)
	if err != nil {
		teamProposalError(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(201, v)
}

type teamProposalDecisionInput struct {
	Revision int    `json:"expectedRevision"`
	SHA      string `json:"expectedProposalSha256"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func (h *teamDeliveryHandlers) decision(c *gin.Context) { h.decide(c, false) }
func (h *teamDeliveryHandlers) cancel(c *gin.Context)   { h.decide(c, true) }
func (h *teamDeliveryHandlers) decide(c *gin.Context, cancel bool) {
	actor, ok := h.authorizeProject(c, c.Param("id"), true)
	if !ok {
		return
	}
	var input teamProposalDecisionInput
	if !decodeTeamProposal(c, &input, 16<<10) {
		return
	}
	if input.Revision <= 0 {
		c.JSON(428, gin.H{"code": "team_git_proposal_precondition", "error": "reviewed revision required"})
		return
	}
	if cancel {
		if input.Decision != "" {
			c.JSON(400, gin.H{"error": "cancel action chooses its own decision"})
			return
		}
		input.Decision = "cancelled"
	}
	if input.Decision != "approved" && input.Decision != "rejected" && !(cancel && input.Decision == "cancelled") {
		c.JSON(400, gin.H{"error": "approved or rejected decision required"})
		return
	}
	v, err := h.repo.TeamGitProposal(c.Param("id"), c.Param("proposalId"))
	if err != nil {
		teamProposalError(c, err)
		return
	}
	if cancel && input.SHA == "" {
		input.SHA = v.SHA256
	}
	if !cancel {
		if err = h.engine.ObserveTeamGitProposalTarget(c.Request.Context(), v); err != nil {
			teamProposalError(c, err)
			return
		}
	}
	v, err = h.repo.DecideTeamGitProposal(c.Param("id"), v.ID, input.SHA, actor, input.Decision, input.Reason, input.Revision)
	if err != nil {
		teamProposalError(c, err)
		return
	}
	c.JSON(200, v)
}
func (h *teamDeliveryHandlers) publish(c *gin.Context)   { h.publishOrReconcile(c, false) }
func (h *teamDeliveryHandlers) reconcile(c *gin.Context) { h.publishOrReconcile(c, true) }
func (h *teamDeliveryHandlers) publishOrReconcile(c *gin.Context, reconcile bool) {
	if _, ok := h.authorizeProject(c, c.Param("id"), true); !ok {
		return
	}
	if authRequired() && !h.isPlatformAdmin(c.Request) {
		c.JSON(403, gin.H{"error": "controller publication requires platform admin"})
		return
	}
	var input struct {
		Revision int    `json:"expectedRevision"`
		SHA      string `json:"expectedProposalSha256"`
		Base     string `json:"expectedBaseCommit"`
	}
	if !decodeTeamProposal(c, &input, 16<<10) {
		return
	}
	if input.Revision <= 0 {
		c.JSON(428, gin.H{"error": "reviewed revision required"})
		return
	}
	v, err := h.engine.PublishTeamGitProposal(c.Request.Context(), c.Param("id"), c.Param("proposalId"), input.SHA, input.Base, input.Revision, reconcile)
	if err != nil {
		teamProposalError(c, err)
		return
	}
	c.JSON(200, v)
}
