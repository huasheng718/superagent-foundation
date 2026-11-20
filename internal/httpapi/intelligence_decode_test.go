package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestDecodeGovernedInputRejectsAmbiguousOrUnboundedRequests(t *testing.T) {
	for _, body := range []string{
		`null`, `[]`, `{"query":"ok"} {}`, `{"query":"ok"} trailing`,
		`{"query":"ok","userId":"other"}`, `{"query":`,
		`{"query":"` + strings.Repeat("x", 1<<20) + `"}`,
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		var input struct {
			Query string `json:"query"`
		}
		if decodeGovernedInput(c, &input) || w.Code != http.StatusBadRequest {
			t.Fatalf("invalid request accepted (bytes=%d): status=%d", len(body), w.Code)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(" \n{\"query\":\"currency\"}\n"))
	var input struct {
		Query string `json:"query"`
	}
	if !decodeGovernedInput(c, &input) || input.Query != "currency" {
		t.Fatal("valid governed request rejected")
	}
}
