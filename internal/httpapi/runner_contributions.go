package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"

	"github.com/gin-gonic/gin"
)

var contributionCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// runnerContributionContent stages immutable bytes, never publication or task
// success. The durable receipt is fenced a second time after reconstruction.
func (h *runnersHandlers) runnerContributionContent(c *gin.Context) {
	runner, ok := h.runnerSessionAuth(c)
	if !ok {
		return
	}
	attempt, err := strconv.Atoi(c.Query("attempt"))
	if err != nil || attempt <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "positive attempt required"})
		return
	}
	spec, err := h.repo.ValidateRunnerContributionUploadForSession(c.Param("id"), runner.ID, runner.SessionTokenHash, attempt, c.Param("repositoryId"), time.Now().UTC())
	if err != nil {
		writeContributionError(c, err)
		return
	}
	baseSHA, resultSHA := c.GetHeader("X-Contribution-Base-SHA"), c.GetHeader("X-Contribution-Result-SHA")
	if c.GetHeader("Content-Type") != "application/x-git-bundle" || !contributionCommitSHA.MatchString(baseSHA) || !contributionCommitSHA.MatchString(resultSHA) || baseSHA != spec.BaseSHA || c.GetHeader("X-Contribution-Context-SHA256") != spec.ContextSHA256 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid contribution metadata"})
		return
	}
	limit := spec.MaxBytes
	if limit <= 0 || limit > domain.RunnerContributionMaxBytes {
		limit = domain.RunnerContributionMaxBytes
	}
	if c.Request.ContentLength > limit {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "contribution byte limit exceeded"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Minute)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = c.Request.Body.Close() })
	defer stopClose()
	dir, err := os.MkdirTemp("", "agentflow-contribution-upload-")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create contribution buffer"})
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "source.bundle")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create contribution buffer"})
		return
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(c.Request.Body, limit+1))
	closeErr := file.Close()
	if size > limit {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "contribution byte limit exceeded"})
		return
	}
	if copyErr != nil || closeErr != nil || ctx.Err() != nil || size == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "incomplete contribution body"})
		return
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	expected := gitwork.ContributionBundle{Path: path, BaseSHA: baseSHA, ResultSHA: resultSHA, SHA256: digest, Size: size}
	if err := gitwork.VerifyContribution(ctx, path, filepath.Join(dir, "verified"), expected, 64<<20); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid standalone contribution bundle"})
		return
	}
	content, err := os.ReadFile(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read contribution buffer"})
		return
	}
	if err := h.repo.PutRunnerContributionBlob(digest, content); err != nil {
		writeContributionError(c, err)
		return
	}
	receipt, err := h.repo.StageRunnerContributionForSession(domain.RunnerContributionReceipt{
		TaskID: c.Param("id"), Attempt: attempt, RepositoryID: spec.RepositoryID,
		Dir: spec.Dir, BaseSHA: baseSHA, ResultSHA: resultSHA, ContextSHA256: spec.ContextSHA256,
		SHA256: digest, Size: size,
	}, runner.ID, runner.SessionTokenHash, time.Now().UTC())
	if err != nil {
		writeContributionError(c, err)
		return
	}
	c.JSON(http.StatusOK, receipt)
}

// runnerContributionInput serves only the currently leased task's frozen
// contribution input. The Repo method rechecks session, placement, attempt,
// lineage and blob hash under one transaction before bytes are returned.
func (h *runnersHandlers) runnerContributionInput(c *gin.Context) {
	runner, ok := h.runnerSessionAuth(c)
	if !ok {
		return
	}
	attempt, err := strconv.Atoi(c.Query("attempt"))
	if err != nil || attempt <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "positive attempt required"})
		return
	}
	input, content, found, err := h.repo.RunnerContributionInputForSession(c.Param("id"), runner.ID, runner.SessionTokenHash, attempt, c.Param("repositoryId"), time.Now().UTC())
	if errors.Is(err, repo.ErrRunnerPlacementSessionInvalid) {
		writeRunnerTaskAuthorizationError(c, err)
		return
	}
	if !found && err == nil {
		c.Status(http.StatusNotFound)
		return
	}
	if errors.Is(err, repo.ErrRunnerTaskLeaseLost) {
		c.JSON(http.StatusConflict, gin.H{"error": "task lease lost"})
		return
	}
	if errors.Is(err, repo.ErrRunnerContributionInvalid) {
		c.JSON(http.StatusConflict, gin.H{"error": "frozen contribution input unavailable"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read contribution input"})
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("Content-Length", strconv.FormatInt(int64(len(content)), 10))
	c.Header("X-Contribution-SHA256", input.BundleSHA256)
	c.Header("X-Contribution-Base-SHA", input.BaseSHA)
	c.Header("X-Contribution-Result-SHA", input.HeadSHA)
	c.Data(http.StatusOK, "application/x-git-bundle", content)
}

func writeContributionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, repo.ErrRunnerPlacementSessionInvalid), errors.Is(err, domain.ErrRunnerPlacementInvalid), errors.Is(err, domain.ErrRunnerPlacementDenied), errors.Is(err, domain.ErrRunnerPlacementChanged), errors.Is(err, domain.ErrRunnerPlacementUnavailable):
		writeRunnerTaskAuthorizationError(c, err)
	case errors.Is(err, repo.ErrRunnerTaskLeaseLost):
		c.JSON(http.StatusConflict, gin.H{"error": "task lease lost"})
	case errors.Is(err, repo.ErrRunnerContributionConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "conflicting contribution"})
	case errors.Is(err, repo.ErrRunnerContributionTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "contribution byte limit exceeded"})
	case errors.Is(err, repo.ErrRunnerContributionInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid contribution"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist contribution"})
	}
}
