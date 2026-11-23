package memoryprovider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

type Service struct {
	Client *Client
	Memory *repo.MemoryRepo
}
type SyncResult struct {
	CleanupPending int  `json:"cleanupPending"`
	Eligible       int  `json:"eligible"`
	Ready          int  `json:"ready"`
	Pending        int  `json:"pending"`
	Failed         int  `json:"failed"`
	Complete       bool `json:"complete"`
}

func errorClass(err error) string {
	switch {
	case errors.Is(err, ErrIntegrity):
		return "provider_integrity"
	case errors.Is(err, ErrBudget), errors.Is(err, repo.ErrMemoryIndexCapacity):
		return "provider_capacity"
	case errors.Is(err, ErrSyncRequired):
		return "sync_pending"
	default:
		return "provider_unavailable"
	}
}
func eligibleDigest(memories []domain.GovernedMemory) string {
	pins := []struct {
		ID       string
		Revision int
		SHA256   string
	}{}
	for _, m := range memories {
		pins = append(pins, struct {
			ID       string
			Revision int
			SHA256   string
		}{m.ID, m.Revision, m.SHA256})
	}
	digest, _ := domain.IntelligenceDigest(pins)
	return digest
}
func (s Service) Sync(ctx context.Context, scope domain.MemoryProviderScope, limit int) (SyncResult, error) {
	out := SyncResult{}
	if !validScope(scope) || s.Client == nil || s.Memory == nil {
		return out, ErrUnavailable
	}
	if limit == 0 {
		limit = 10
	}
	if limit < 1 || limit > 10 {
		return out, ErrBudget
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.Client.Health(ctx); err != nil {
		return out, err
	}
	eligible, err := s.Memory.EligibleProviderMemories(scope.ProjectID)
	if err != nil {
		return out, err
	}
	out.Eligible = len(eligible)
	mirrors, err := s.Memory.MemoryProviderMirrors(scope, s.Client.Binding)
	if err != nil {
		return out, err
	}
	wanted := map[string]bool{}
	known := map[string]domain.MemoryProviderMirror{}
	for _, m := range eligible {
		wanted[domain.MemoryMirrorID(scope, s.Client.Binding, m.ID, m.Revision, m.SHA256)] = true
	}
	operations := 0
	size := 0
	for _, mirror := range mirrors {
		known[mirror.ID] = mirror
		if wanted[mirror.ID] || mirror.State == "deleted" || operations >= limit {
			continue
		}
		operations++
		if mirror.State != "delete_pending" {
			if err := s.Memory.SetMemoryMirrorState(mirror.ID, mirror.State, "delete_pending", func() string {
				if mirror.State == "pending" {
					return "add_result_unknown"
				}
				return ""
			}()); err != nil {
				out.Failed++
				continue
			}
			if mirror.State == "pending" {
				mirror.LastError = "add_result_unknown"
			}
			mirror.State = "delete_pending"
		}
		if err := s.Client.remove(ctx, mirror); err != nil {
			out.Failed++
			continue
		}
		// A pending add may still arrive after an empty delete verification. Keep
		// its cleanup intent durable until the owned provider generation is retired.
		if mirror.LastError == "add_result_unknown" {
			continue
		}
		if err := s.Memory.SetMemoryMirrorState(mirror.ID, "delete_pending", "deleted", ""); err != nil {
			out.Failed++
		}
	}
	for _, m := range eligible {
		id := domain.MemoryMirrorID(scope, s.Client.Binding, m.ID, m.Revision, m.SHA256)
		mirror, found := known[id]
		if found && mirror.State == "ready" {
			continue
		}
		if operations >= limit || ctx.Err() != nil {
			continue
		}
		texts, err := chunks(m)
		if err != nil {
			return out, err
		}
		for _, t := range texts {
			size += len(t)
		}
		if size > 256<<10 {
			break
		}
		operations++
		mirror, created, err := s.Memory.ReserveMemoryMirror(domain.MemoryProviderMirror{ID: id, Scope: scope, Binding: s.Client.Binding, MemoryID: m.ID, Revision: m.Revision, SHA256: m.SHA256, SessionID: "lfm-" + id, State: "pending", Chunks: chunkPins(texts)})
		if err != nil {
			out.Failed++
			continue
		}
		var accepted []domain.MemoryProviderChunk
		if created {
			accepted, err = s.Client.add(ctx, scope, mirror.SessionID, texts)
			if err != nil {
				_ = s.Memory.SetMemoryMirrorState(id, "pending", "pending", errorClass(err))
				out.Failed++
				continue
			}
		}
		// Existing pending intents only discover: empty or partial results can never
		// authorize another append, including after process restart.
		records, err := s.Client.query(ctx, scope, mirror.SessionID)
		if err == nil {
			var pins []domain.MemoryProviderChunk
			pins, err = verifyRecords(scope, mirror.SessionID, texts, records)
			if err == nil && created {
				ids := map[string]bool{}
				for _, p := range accepted {
					ids[p.RecordID] = true
				}
				for _, p := range pins {
					if !ids[p.RecordID] {
						err = ErrIntegrity
					}
				}
			}
			if err == nil {
				err = s.Memory.CompleteMemoryMirror(id, pins)
			}
		}
		if err != nil {
			_ = s.Memory.SetMemoryMirrorState(id, "pending", "pending", errorClass(err))
			out.Failed++
		}
	}
	current, err := s.Memory.EligibleProviderMemories(scope.ProjectID)
	if err != nil {
		return out, err
	}
	mirrors, err = s.Memory.MemoryProviderMirrors(scope, s.Client.Binding)
	if err != nil {
		return out, err
	}
	ready := map[string]bool{}
	for _, mirror := range mirrors {
		if mirror.State == "delete_pending" {
			out.CleanupPending++
		}
		if mirror.State == "ready" {
			ready[mirror.ID] = true
		}
	}
	out.Eligible = len(current)
	for _, m := range current {
		if ready[domain.MemoryMirrorID(scope, s.Client.Binding, m.ID, m.Revision, m.SHA256)] {
			out.Ready++
		} else {
			out.Pending++
		}
	}
	out.Complete = out.Pending == 0 && out.Failed == 0 && out.CleanupPending == 0
	return out, nil
}

// queryScope verifies actual index coverage; a surviving SQL ledger alone does
// not prove that the configured provider still holds the owned data directory.
func (c *Client) queryScope(ctx context.Context, scope domain.MemoryProviderScope) ([]record, []string, error) {
	out := []record{}
	requestIDs := []string{}
	expected := -1
	for offset := 0; offset <= 2000; offset += 100 {
		body := scopeBody(scope)
		body["limit"] = 100
		body["offset"] = offset
		var page struct {
			Messages []record `json:"messages"`
			Total    *int     `json:"total"`
		}
		requestID, err := c.request(ctx, "/v3/conversation/query", body, &page)
		if err != nil {
			return nil, nil, err
		}
		requestIDs = append(requestIDs, requestID)
		if page.Total == nil || page.Messages == nil {
			return nil, nil, ErrIntegrity
		}
		if *page.Total < 0 || *page.Total > 2000 || len(page.Messages) > 100 || expected >= 0 && *page.Total != expected {
			return nil, nil, ErrIntegrity
		}
		expected = *page.Total
		out = append(out, page.Messages...)
		if len(out) == expected {
			return out, requestIDs, nil
		}
		if len(page.Messages) != 100 || len(out) > expected {
			return nil, nil, ErrIntegrity
		}
	}
	return nil, nil, ErrBudget
}

func (s Service) Recall(ctx context.Context, scope domain.MemoryProviderScope, query string, limit int) ([]domain.GovernedMemory, domain.MemoryRecallReceipt, error) {
	receipt := domain.MemoryRecallReceipt{}
	items := []domain.GovernedMemory{}
	if !validScope(scope) || s.Client == nil || s.Memory == nil {
		return nil, receipt, ErrUnavailable
	}
	if strings.TrimSpace(query) == "" || len(query) > 1000 {
		return nil, receipt, ErrBudget
	}
	if limit == 0 {
		limit = 5
	}
	if limit < 1 || limit > 10 {
		return nil, receipt, ErrBudget
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.Client.Health(ctx); err != nil {
		return nil, receipt, err
	}
	memories, err := s.Memory.EligibleProviderMemories(scope.ProjectID)
	if err != nil {
		return nil, receipt, err
	}
	eligibleHash := eligibleDigest(memories)
	mirrors, err := s.Memory.MemoryProviderMirrors(scope, s.Client.Binding)
	if err != nil {
		return nil, receipt, err
	}
	byID := map[string]domain.MemoryProviderMirror{}
	for _, m := range mirrors {
		byID[m.ID] = m
	}
	type mapping struct {
		mirror domain.MemoryProviderMirror
		chunk  domain.MemoryProviderChunk
	}
	records := map[string]mapping{}
	required := map[string]bool{}
	for _, m := range memories {
		mirror, ok := byID[domain.MemoryMirrorID(scope, s.Client.Binding, m.ID, m.Revision, m.SHA256)]
		if !ok || mirror.State != "ready" {
			return nil, receipt, ErrSyncRequired
		}
		for _, chunk := range mirror.Chunks {
			required[chunk.RecordID] = true
		}
	}
	for _, mirror := range mirrors {
		for _, chunk := range mirror.Chunks {
			if chunk.RecordID == "" {
				continue
			}
			if _, ok := records[chunk.RecordID]; ok {
				return nil, receipt, ErrIntegrity
			}
			records[chunk.RecordID] = mapping{mirror, chunk}
		}
	}
	coverage, coverageRequestIDs, err := s.Client.queryScope(ctx, scope)
	if err != nil {
		return nil, receipt, err
	}
	scopeFields := scopeBody(scope)
	seen := map[string]bool{}
	for _, r := range coverage {
		m, ok := records[r.ID]
		if !ok || seen[r.ID] || r.TeamID != scopeFields["team_id"] || r.UserID != scopeFields["user_id"] || r.AgentID != scopeFields["agent_id"] || r.SessionID != m.mirror.SessionID || r.Role != "assistant" || len(r.Content) != m.chunk.Size || textDigest(r.Content) != m.chunk.SHA256 {
			return nil, receipt, ErrIntegrity
		}
		seen[r.ID] = true
	}
	for id := range required {
		if !seen[id] {
			return nil, receipt, ErrSyncRequired
		}
	}
	hits, requestID, err := s.Client.search(ctx, scope, query)
	if err != nil {
		return nil, receipt, err
	}
	ranked := map[string]domain.MemoryRecallHit{}
	seen = map[string]bool{}
	for _, hit := range hits {
		mapped, ok := records[hit.ID]
		if !ok || seen[hit.ID] || hit.Role != "assistant" || math.IsNaN(hit.Score) || math.IsInf(hit.Score, 0) || len(hit.Content) != mapped.chunk.Size || textDigest(hit.Content) != mapped.chunk.SHA256 {
			return nil, receipt, ErrIntegrity
		}
		seen[hit.ID] = true
		mirror := mapped.mirror
		if _, err := s.Memory.MemoryProviderPin(scope.ProjectID, mirror.MemoryID, mirror.Revision, mirror.SHA256); err != nil {
			if errors.Is(err, repo.ErrIntelligenceUnavailable) {
				continue
			}
			return nil, receipt, err
		}
		if mirror.State != "ready" {
			return nil, receipt, ErrSyncRequired
		}
		pin, ok := ranked[mirror.MemoryID]
		if !ok {
			pin = domain.MemoryRecallHit{MemoryID: mirror.MemoryID, Revision: mirror.Revision, SHA256: mirror.SHA256, Score: hit.Score, RecordIDs: []string{}, RecordVersions: []string{}}
		}
		if hit.Score > pin.Score {
			pin.Score = hit.Score
		}
		pin.RecordIDs = append(pin.RecordIDs, hit.ID)
		pin.RecordVersions = append(pin.RecordVersions, mapped.chunk.Version)
		ranked[mirror.MemoryID] = pin
	}
	ordered := []domain.MemoryRecallHit{}
	for _, pin := range ranked {
		ordered = append(ordered, pin)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Score == ordered[j].Score {
			return ordered[i].MemoryID < ordered[j].MemoryID
		}
		return ordered[i].Score > ordered[j].Score
	})
	receipt.Hits = []domain.MemoryRecallHit{}
	bytes := 0
	for _, pin := range ordered {
		m, err := s.Memory.MemoryProviderPin(scope.ProjectID, pin.MemoryID, pin.Revision, pin.SHA256)
		if err != nil {
			return nil, receipt, err
		}
		raw, _ := json.Marshal(m)
		if bytes+len(raw) > domain.MemoryMaxRecallBytes {
			continue
		}
		items = append(items, m)
		receipt.Hits = append(receipt.Hits, pin)
		bytes += len(raw)
		if len(items) == limit {
			break
		}
	}
	// An approval-set change during HTTP invalidates the complete coverage claim.
	current, err := s.Memory.EligibleProviderMemories(scope.ProjectID)
	if err != nil {
		return nil, receipt, err
	}
	if eligibleDigest(current) != eligibleHash {
		return nil, receipt, ErrSyncRequired
	}
	receipt.Scope = scope
	receipt.Binding = s.Client.Binding
	receipt.QuerySHA256 = textDigest(query)
	receipt.EligibleSHA256 = eligibleHash
	receipt.RequestIDs = append(coverageRequestIDs, requestID)
	receipt.Provider = "tencentdb-l0-bm25"
	receipt.Limit = limit
	receipt.ObservedFTSAvailable = true
	receipt.ObservedEmbeddingService = false
	receipt.LatencyMS = time.Since(started).Milliseconds()
	receipt, err = s.Memory.SaveMemoryRecallReceipt(receipt)
	if err != nil {
		return nil, receipt, err
	}
	for _, m := range items {
		if _, err := s.Memory.MemoryProviderPin(scope.ProjectID, m.ID, m.Revision, m.SHA256); err != nil {
			return nil, receipt, err
		}
	}
	return items, receipt, nil
}
