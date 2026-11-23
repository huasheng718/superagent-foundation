package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
)

type extractionHandlers struct {
	*deps
	authorizeProject func(*gin.Context, string, bool) (string, bool)
}

func extractionError(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "extraction_unavailable", "knowledge extraction storage unavailable"
	switch {
	case errors.Is(err, repo.ErrExtractionForbidden):
		status, code, message = http.StatusForbidden, "extraction_membership_changed", err.Error()
	case errors.Is(err, repo.ErrExtractionRestricted):
		status, code, message = http.StatusForbidden, "extraction_restricted_inputs", err.Error()
	case errors.Is(err, repo.ErrIntelligenceUnavailable):
		status, code, message = http.StatusNotFound, "extraction_unavailable", "source or extraction is unavailable in this project"
	case errors.Is(err, repo.ErrExtractionSource), errors.Is(err, repo.ErrIntelligenceConflict), errors.Is(err, domain.ErrExecutionBudgetStopped), errors.Is(err, domain.ErrExecutionBudgetUnsupported):
		status, code, message = http.StatusConflict, "extraction_source_conflict", err.Error()
	case errors.Is(err, domain.ErrKnowledgeExtractionInvalid):
		status, code, message = http.StatusBadRequest, "extraction_invalid", "invalid bounded extraction request or artifact"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = http.StatusGatewayTimeout, "extraction_timeout", "knowledge extraction timed out; retry the same fixed source"
	case errors.Is(err, context.Canceled):
		status, code, message = http.StatusRequestTimeout, "extraction_cancelled", "knowledge extraction cancelled"
	}
	c.JSON(status, gin.H{"error": message, "code": code})
}
func (h *extractionHandlers) create(c *gin.Context) {
	actor, ok := h.authorizeProject(c, c.Param("id"), false)
	if !ok {
		return
	}
	c.Header("Cache-Control", "private, no-store")
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	if err != nil {
		extractionError(c, domain.ErrKnowledgeExtractionInvalid)
		return
	}
	input, err := domain.ParseKnowledgeExtractionRequest(raw)
	if err != nil {
		extractionError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	principal := repo.KnowledgeExtractionActor{LoginName: actor, Local: !authRequired()}
	if authRequired() {
		user, valid := h.requestUser(c.Request, true)
		if !valid || user == nil {
			extractionError(c, repo.ErrExtractionForbidden)
			return
		}
		principal.UserID = user.ID
	}
	receipt, replayed, err := h.repo.CreateKnowledgeExtraction(ctx, c.Param("id"), input, principal)
	if err != nil {
		extractionError(c, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{"receipt": receipt, "replayed": replayed})
}
func (h *extractionHandlers) detail(c *gin.Context) {
	if _, ok := h.authorizeProject(c, c.Param("id"), false); !ok {
		return
	}
	c.Header("Cache-Control", "private, no-store")
	receipt, err := h.repo.KnowledgeExtraction(c.Param("id"), c.Param("extractionId"))
	if err != nil {
		extractionError(c, err)
		return
	}
	c.JSON(http.StatusOK, receipt)
}
