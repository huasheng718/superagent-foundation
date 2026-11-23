package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Governed requests select content and pins, never an identity or scope. Reject
// unknown fields instead of silently accepting a caller's apparent scope claim.
func decodeGovernedInput(c *gin.Context, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid governed request: " + err.Error()})
		return false
	}
	if len(raw) == 0 || raw[0] != '{' {
		c.JSON(http.StatusBadRequest, gin.H{"error": "governed request must be a JSON object"})
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "governed request must contain exactly one JSON object"})
		return false
	}
	fields := json.NewDecoder(bytes.NewReader(raw))
	fields.DisallowUnknownFields()
	if err := fields.Decode(value); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid governed request: " + err.Error()})
		return false
	}
	return true
}
