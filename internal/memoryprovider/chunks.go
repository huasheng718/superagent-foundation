package memoryprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"agentflow/internal/domain"
)

func textDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
func chunkPins(texts []string) []domain.MemoryProviderChunk {
	out := make([]domain.MemoryProviderChunk, len(texts))
	for i, text := range texts {
		out[i] = domain.MemoryProviderChunk{SHA256: textDigest(text), Size: len(text)}
	}
	return out
}
func chunks(memory domain.GovernedMemory) ([]string, error) {
	text := memory.Title + "\n" + memory.Content
	if !utf8.ValidString(text) || len(memory.Content) > domain.MemoryMaxContentBytes || len(memory.Title) > 200 {
		return nil, ErrBudget
	}
	out := []string{}
	for len(text) > 0 {
		end := len(text)
		if end > 7000 {
			end = 7000
			for !utf8.RuneStart(text[end]) {
				end--
			}
		}
		out = append(out, text[:end])
		text = text[end:]
	}
	if len(out) == 0 || len(out) > 4 {
		return nil, ErrBudget
	}
	return out, nil
}
func validScope(scope domain.MemoryProviderScope) bool {
	return strings.TrimSpace(scope.ProjectID) != "" && scope.UserID > 0 && strings.TrimSpace(scope.AgentID) != "" && len(scope.AgentID) <= 200
}
func verifyRecords(scope domain.MemoryProviderScope, session string, texts []string, records []record) ([]domain.MemoryProviderChunk, error) {
	if len(texts) != len(records) {
		return nil, ErrSyncRequired
	}
	expected := scopeBody(scope)
	remaining := map[string]int{}
	for _, text := range texts {
		remaining[text]++
	}
	pins := make([]domain.MemoryProviderChunk, 0, len(records))
	seen := map[string]bool{}
	for _, r := range records {
		if r.ID == "" || len(r.ID) > 128 || seen[r.ID] || r.Role != "assistant" || r.TeamID != expected["team_id"] || r.UserID != expected["user_id"] || r.AgentID != expected["agent_id"] || r.SessionID != session || remaining[r.Content] == 0 {
			return nil, ErrIntegrity
		}
		seen[r.ID] = true
		remaining[r.Content]--
		pins = append(pins, domain.MemoryProviderChunk{SHA256: textDigest(r.Content), Size: len(r.Content), RecordID: r.ID, Version: "v1"})
	}
	return pins, nil
}
