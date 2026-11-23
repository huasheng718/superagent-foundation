package httpapi

import "github.com/gin-gonic/gin"

func (h *mcpHandlers) distribution(c *gin.Context) { h.decideDistribution(c, "mcp") }
