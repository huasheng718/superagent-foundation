package memoryprovider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientRejectsUnsafeConfigurationAndRedirects(t *testing.T) {
	for _, raw := range []string{"http://remote.example", "https://user:secret@example.com", "https://example.com?key=secret", "https://example.com/other", "file:///tmp/provider"} {
		t.Setenv("AGENTFLOW_MEMORY_PROVIDER_URL", raw)
		if _, err := FromEnv(); err == nil {
			t.Fatalf("unsafe URL accepted")
		}
	}
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_URL", "")
	if client, err := FromEnv(); err != nil || client != nil {
		t.Fatal("native mode changed")
	}
	hits := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(200) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer redirect.Close()
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_URL", redirect.URL)
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_TOKEN", "server-only")
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_INSTANCE", "pilot")
	t.Setenv("AGENTFLOW_MEMORY_PROVIDER_GENERATION", "new")
	client, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err != ErrUnavailable || hits != 0 {
		t.Fatal("redirect forwarded server credential")
	}
}
func TestClientEnvelopeResponseAndDeadlineBudgets(t *testing.T) {
	response := `{"request_id":"missing-success","data":{}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if response == "slow" {
			time.Sleep(60 * time.Millisecond)
		}
		w.Write([]byte(response))
	}))
	defer server.Close()
	client := &Client{baseURL: server.URL, http: server.Client()}
	var output map[string]any
	if _, err := client.request(context.Background(), "/test", map[string]string{}, &output); err != ErrIntegrity {
		t.Fatalf("missing code %v", err)
	}
	response = `{"code":9,"request_id":"rejected","data":{}}`
	if _, err := client.request(context.Background(), "/test", map[string]string{}, &output); err != ErrIntegrity {
		t.Fatal("nonzero envelope accepted")
	}
	response = strings.Repeat("x", (1<<20)+1)
	if _, err := client.request(context.Background(), "/test", map[string]string{}, &output); err != ErrBudget {
		t.Fatal("response overflow")
	}
	if _, err := client.request(context.Background(), "/test", map[string]string{"oversized": strings.Repeat("x", 256<<10)}, &output); err != ErrBudget {
		t.Fatal("request overflow")
	}
	response = "slow"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := client.request(ctx, "/test", map[string]string{}, &output); err != ErrUnavailable {
		t.Fatalf("deadline %v", err)
	}
}
