package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
)

var sourceMatchPattern = regexp.MustCompile(`^"([a-f0-9]{32}|legacy-[a-f0-9]{64})"$`)

// Existing edits require the exact observed base, even with authentication off.
// The body cannot supply or override the HTTP precondition.
func sourceEditPrecondition(c *gin.Context, exists bool) (string, bool) {
	match, none := strings.TrimSpace(c.GetHeader("If-Match")), strings.TrimSpace(c.GetHeader("If-None-Match"))
	if none != "" {
		if match != "" || none != "*" {
			c.JSON(400, gin.H{"error": "use one strong source edit precondition"})
			return "", false
		}
		if exists {
			sourceEditError(c, repo.ErrSourceConflict)
			return "", false
		}
		return "", true
	}
	if match == "" {
		if exists {
			sourceEditError(c, repo.ErrSourcePrecondition)
			return "", false
		}
		return "", true
	}
	parts := sourceMatchPattern.FindStringSubmatch(match)
	if len(parts) != 2 {
		c.JSON(400, gin.H{"error": "If-Match must contain one quoted sourceRevision"})
		return "", false
	}
	return parts[1], true
}

func sourceEditError(c *gin.Context, err error) bool {
	if errors.Is(err, domain.ErrDistributionDenied) || errors.Is(err, repo.ErrDistributionOwner) {
		c.JSON(403, gin.H{"code": "distribution_denied", "error": err.Error()})
		return true
	}
	if errors.Is(err, domain.ErrDistributionInvalid) {
		c.JSON(400, gin.H{"code": "distribution_invalid", "error": err.Error()})
		return true
	}
	if errors.Is(err, repo.ErrSourcePrecondition) {
		c.JSON(http.StatusPreconditionRequired, gin.H{"code": "source_precondition_required", "error": "当前客户端未携带编辑基线，请刷新或升级客户端后重试"})
		return true
	}
	if errors.Is(err, repo.ErrSourceConflict) {
		c.JSON(http.StatusConflict, gin.H{"code": "source_edit_conflict", "error": "内容已被其他操作修改，本次更改未保存。请刷新并比较后重新提交"})
		return true
	}
	if errors.Is(err, repo.ErrSkillSourceReadOnly) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return true
	}
	return false
}
