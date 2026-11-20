// Package memoryprovider is a narrow, private TencentDB L0/BM25 transport.
// It does not extract knowledge, publish memories or decide execution authority.
package memoryprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/domain"
)

var (
	ErrUnavailable  = errors.New("memory provider unavailable")
	ErrIntegrity    = errors.New("memory provider response does not match the approved mirror")
	ErrSyncRequired = errors.New("approved memory index synchronization required")
	ErrBudget       = errors.New("memory provider request or index budget exceeded")
)

const SourceCommit = "42d4cde50c5a83182fced8c584f9ddb32a620615"

type Client struct {
	baseURL   string
	token     string
	serviceID string
	Binding   domain.MemoryProviderBinding
	http      *http.Client
}

func FromEnv() (*Client, error) {
	raw := strings.TrimSpace(os.Getenv("AGENTFLOW_MEMORY_PROVIDER_URL"))
	if raw == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" || parsed.Hostname() == "" {
		return nil, ErrUnavailable
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" && parsed.Hostname() != "::1") {
		return nil, ErrUnavailable
	}
	token := strings.TrimSpace(os.Getenv("AGENTFLOW_MEMORY_PROVIDER_TOKEN"))
	instance := strings.TrimSpace(os.Getenv("AGENTFLOW_MEMORY_PROVIDER_INSTANCE"))
	generation := strings.TrimSpace(os.Getenv("AGENTFLOW_MEMORY_PROVIDER_GENERATION"))
	if token == "" || len(token) > 4096 || instance == "" || len(instance) > 128 || generation == "" || len(generation) > 128 {
		return nil, ErrUnavailable
	}
	service := strings.TrimSpace(os.Getenv("AGENTFLOW_MEMORY_PROVIDER_SERVICE"))
	if service == "" {
		service = "default"
	}
	if len(service) > 128 {
		return nil, ErrUnavailable
	}
	return &Client{baseURL: strings.TrimRight(raw, "/"), token: token, serviceID: service, Binding: domain.MemoryProviderBinding{InstanceID: instance, Generation: generation, SourceCommit: SourceCommit, PackageVersion: "1.0.2-beta.1", HealthVersion: "0.1.0", IndexSchema: "sqlite-fts-v2"}, http: &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) request(ctx context.Context, path string, body any, out any) (string, error) {
	var raw []byte
	var err error
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
		raw, err = json.Marshal(body)
		if err != nil {
			return "", ErrIntegrity
		}
		if len(raw) > 256<<10 {
			return "", ErrBudget
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return "", ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("x-tdai-service-id", c.serviceID)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return "", ErrUnavailable
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return "", ErrUnavailable
	}
	if len(data) > 1<<20 {
		return "", ErrBudget
	}
	if response.StatusCode != 200 {
		return "", ErrUnavailable
	}
	if body == nil {
		if json.Unmarshal(data, out) != nil {
			return "", ErrIntegrity
		}
		return "", nil
	}
	var envelope struct {
		Code      int             `json:"code"`
		RequestID string          `json:"request_id"`
		Data      json.RawMessage `json:"data"`
	}
	// Require an explicit success code; absent code must not default to success.
	var marker map[string]json.RawMessage
	if json.Unmarshal(data, &marker) != nil || string(marker["code"]) != "0" || json.Unmarshal(data, &envelope) != nil || envelope.Code != 0 || envelope.RequestID == "" || len(envelope.RequestID) > 128 || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return "", ErrIntegrity
	}
	if json.Unmarshal(envelope.Data, out) != nil {
		return "", ErrIntegrity
	}
	return envelope.RequestID, nil
}

func (c *Client) Health(ctx context.Context) error {
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Stores  struct {
			VectorStore bool `json:"vectorStore"`
			Embedding   bool `json:"embeddingService"`
			FTS         bool `json:"ftsAvailable"`
		} `json:"stores"`
	}
	_, err := c.request(ctx, "/health", nil, &health)
	if err != nil {
		return err
	}
	if health.Status != "ok" || health.Version != c.Binding.HealthVersion || !health.Stores.VectorStore || !health.Stores.FTS || health.Stores.Embedding {
		return ErrUnavailable
	}
	return nil
}

func scopeBody(scope domain.MemoryProviderScope) map[string]any {
	digest, _ := domain.IntelligenceDigest(struct {
		Project string
		Agent   string
	}{scope.ProjectID, scope.AgentID})
	project, _ := domain.IntelligenceDigest(scope.ProjectID)
	return map[string]any{"team_id": "lf-project-" + project, "user_id": "lf-user-" + strconv.FormatInt(scope.UserID, 10), "agent_id": "lf-agent-" + digest}
}

type record struct {
	ID        string  `json:"id"`
	Role      string  `json:"role"`
	Content   string  `json:"content"`
	Score     float64 `json:"score"`
	TeamID    string  `json:"team_id"`
	UserID    string  `json:"user_id"`
	AgentID   string  `json:"agent_id"`
	SessionID string  `json:"session_id"`
}

func (c *Client) add(ctx context.Context, scope domain.MemoryProviderScope, session string, texts []string) ([]domain.MemoryProviderChunk, error) {
	body := scopeBody(scope)
	body["session_id"] = session
	messages := []map[string]string{}
	for _, text := range texts {
		messages = append(messages, map[string]string{"role": "assistant", "content": text})
	}
	body["messages"] = messages
	var result struct {
		IDs      []string `json:"accepted_ids"`
		Versions []string `json:"accepted_versions"`
		Count    int      `json:"total_count"`
	}
	_, err := c.request(ctx, "/v3/conversation/add", body, &result)
	if err != nil {
		return nil, err
	}
	if len(result.IDs) != len(texts) || len(result.Versions) != len(texts) || result.Count != len(texts) {
		return nil, ErrIntegrity
	}
	chunks := chunkPins(texts)
	seen := map[string]bool{}
	for i, id := range result.IDs {
		if id == "" || len(id) > 128 || seen[id] || result.Versions[i] != "v1" {
			return nil, ErrIntegrity
		}
		seen[id] = true
		chunks[i].RecordID = id
		chunks[i].Version = "v1"
	}
	return chunks, nil
}

func (c *Client) query(ctx context.Context, scope domain.MemoryProviderScope, session string) ([]record, error) {
	body := scopeBody(scope)
	body["session_id"] = session
	body["limit"] = 100
	body["offset"] = 0
	var result struct {
		Messages []record `json:"messages"`
		Total    *int     `json:"total"`
	}
	_, err := c.request(ctx, "/v3/conversation/query", body, &result)
	if err != nil {
		return nil, err
	}
	// One native record has at most four chunks. A larger/partial page is never
	// considered a complete approved mirror and must not trigger another append.
	if result.Total == nil || result.Messages == nil || *result.Total != len(result.Messages) || *result.Total > 4 || *result.Total < 0 {
		return nil, ErrIntegrity
	}
	return result.Messages, nil
}

func (c *Client) search(ctx context.Context, scope domain.MemoryProviderScope, query string) ([]record, string, error) {
	body := scopeBody(scope)
	body["query"] = query
	body["limit"] = 100
	var result struct {
		Messages []record `json:"messages"`
	}
	id, err := c.request(ctx, "/v3/conversation/search", body, &result)
	if err != nil {
		return nil, "", err
	}
	if result.Messages == nil {
		return nil, "", ErrIntegrity
	}
	if len(result.Messages) > 100 {
		return nil, "", ErrBudget
	}
	return result.Messages, id, nil
}

func (c *Client) remove(ctx context.Context, mirror domain.MemoryProviderMirror) error {
	body := scopeBody(mirror.Scope)
	body["session_ids"] = []string{mirror.SessionID}
	var result struct {
		Count int `json:"deleted_count"`
	}
	_, err := c.request(ctx, "/v3/conversation/delete", body, &result)
	if err != nil {
		return err
	}
	remaining, err := c.query(ctx, mirror.Scope, mirror.SessionID)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return ErrIntegrity
	}
	return nil
}
