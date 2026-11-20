package domain

import (
	"fmt"
	"time"
)

// Provider state is a disposable search index. GovernedMemory remains the
// authority for content, approval, revision, expiry and execution eligibility.
type MemoryProviderBinding struct {
	InstanceID     string `json:"instanceId"`
	Generation     string `json:"generation"`
	SourceCommit   string `json:"sourceCommit"`
	PackageVersion string `json:"packageVersion"`
	HealthVersion  string `json:"healthVersion"`
	IndexSchema    string `json:"indexSchema"`
}

type MemoryProviderScope struct {
	ProjectID string `json:"projectId"`
	UserID    int64  `json:"userId"`
	AgentID   string `json:"agentId"`
}

type MemoryProviderChunk struct {
	SHA256   string `json:"sha256"`
	Size     int    `json:"size"`
	RecordID string `json:"recordId,omitempty"`
	Version  string `json:"version,omitempty"`
}

type MemoryProviderMirror struct {
	ID        string                `json:"id"`
	Scope     MemoryProviderScope   `json:"scope"`
	Binding   MemoryProviderBinding `json:"binding"`
	MemoryID  string                `json:"memoryId"`
	Revision  int                   `json:"revision"`
	SHA256    string                `json:"sha256"`
	SessionID string                `json:"sessionId"`
	State     string                `json:"state"`
	Chunks    []MemoryProviderChunk `json:"chunks"`
	IntentAt  time.Time             `json:"intentAt"`
	LastError string                `json:"lastError,omitempty"`
}

type MemoryRecallHit struct {
	MemoryID       string   `json:"memoryId"`
	Revision       int      `json:"revision"`
	SHA256         string   `json:"sha256"`
	RecordIDs      []string `json:"recordIds"`
	RecordVersions []string `json:"recordVersions"`
	Score          float64  `json:"score"`
}

type MemoryRecallReceipt struct {
	Provider                 string                `json:"provider"`
	Limit                    int                   `json:"limit"`
	ObservedFTSAvailable     bool                  `json:"observedFtsAvailable"`
	ObservedEmbeddingService bool                  `json:"observedEmbeddingService"`
	ID                       string                `json:"id"`
	Scope                    MemoryProviderScope   `json:"scope"`
	Binding                  MemoryProviderBinding `json:"binding"`
	QuerySHA256              string                `json:"querySha256"`
	EligibleSHA256           string                `json:"eligibleSha256"`
	Hits                     []MemoryRecallHit     `json:"hits"`
	RequestIDs               []string              `json:"requestIds"`
	LatencyMS                int64                 `json:"latencyMs"`
	CreatedAt                time.Time             `json:"createdAt"`
	ExpiresAt                time.Time             `json:"expiresAt"`
	SHA256                   string                `json:"sha256"`
}

func (r MemoryRecallReceipt) Digest() (string, error) {
	r.SHA256 = ""
	return IntelligenceDigest(r)
}

func MemoryMirrorID(scope MemoryProviderScope, binding MemoryProviderBinding, id string, revision int, sha string) string {
	digest, _ := IntelligenceDigest(struct {
		Scope    MemoryProviderScope   `json:"scope"`
		Binding  MemoryProviderBinding `json:"binding"`
		ID       string                `json:"id"`
		Revision int                   `json:"revision"`
		SHA256   string                `json:"sha256"`
	}{scope, binding, id, revision, sha})
	return digest
}

// Receipt pins must exactly cover the selected native memories; the provider
// contributes provenance and rank, never an alternative memory body.
func (r MemoryRecallReceipt) ValidatePins(memories []GovernedMemory) error {
	if len(r.Hits) != len(memories) {
		return fmt.Errorf("memory recall receipt selection mismatch")
	}
	byID := map[string]MemoryRecallHit{}
	for _, hit := range r.Hits {
		if _, found := byID[hit.MemoryID]; found || hit.MemoryID == "" || len(hit.RecordIDs) == 0 || len(hit.RecordIDs) != len(hit.RecordVersions) {
			return fmt.Errorf("memory recall receipt pins invalid")
		}
		byID[hit.MemoryID] = hit
	}
	for _, m := range memories {
		hit, ok := byID[m.ID]
		if !ok || hit.Revision != m.Revision || hit.SHA256 != m.SHA256 {
			return fmt.Errorf("memory recall receipt selection mismatch")
		}
		delete(byID, m.ID)
	}
	if len(byID) != 0 {
		return fmt.Errorf("memory recall receipt selection mismatch")
	}
	return nil
}
