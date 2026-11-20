package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	TeamReleasePublished         = "published"
	TeamReleaseRevoked           = "revoked"
	MemoryCandidate              = "candidate"
	MemoryApproved               = "approved"
	MemoryRejected               = "rejected"
	MemoryRevoked                = "revoked"
	IntelligenceMaxSnapshotBytes = 8 << 20
	MemoryMaxContentBytes        = 16 << 10
	MemoryMaxRecallBytes         = 64 << 10
	MemoryMaxRecallItems         = 10
)

// IntelligenceSource is provenance, never an execution authorization. A Run
// reference is checked against the project when a memory is submitted.
type IntelligenceSource struct {
	ExtractionID string `json:"extractionId,omitempty"`
	RunID        string `json:"runId,omitempty"`
	ArtifactID   string `json:"artifactId,omitempty"`
	Repository   string `json:"repository,omitempty"`
	Commit       string `json:"commit,omitempty"`
	Path         string `json:"path,omitempty"`
}

type TeamReleaseRequest struct {
	ExpectedBaseVersion int                `json:"expectedBaseVersion"`
	Name                string             `json:"name"`
	AgentIDs            []string           `json:"agentIds"`
	RuleIDs             []string           `json:"ruleIds,omitempty"`
	SkillIDs            []string           `json:"skillIds,omitempty"`
	MCPServerIDs        []string           `json:"mcpServerIds,omitempty"`
	Source              IntelligenceSource `json:"source"`
}

// TeamReleasePayload is immutable approved formal configuration. Credential
// bindings are deliberately not part of this versioned, portable resource.
type TeamReleasePayload struct {
	Agents    []AgentDefinition  `json:"agents"`
	Assets    AgentAssetBundle   `json:"assets"`
	Source    IntelligenceSource `json:"source"`
	GitImport *TeamGitImport     `json:"gitImport,omitempty"`
}

type IntelligenceDecision struct {
	Decision string    `json:"decision"`
	Actor    string    `json:"actor"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}

type TeamRelease struct {
	ID        string                 `json:"id"`
	ProjectID string                 `json:"projectId"`
	Name      string                 `json:"name"`
	Version   int                    `json:"version"`
	Revision  int                    `json:"revision"`
	State     string                 `json:"state"`
	SHA256    string                 `json:"sha256"`
	Payload   TeamReleasePayload     `json:"payload"`
	CreatedBy string                 `json:"createdBy"`
	CreatedAt time.Time              `json:"createdAt"`
	Decisions []IntelligenceDecision `json:"decisions"`
}

type GovernedMemory struct {
	ID        string                 `json:"id"`
	ProjectID string                 `json:"projectId"`
	Title     string                 `json:"title"`
	Content   string                 `json:"content"`
	Source    IntelligenceSource     `json:"source"`
	SHA256    string                 `json:"sha256"`
	State     string                 `json:"state"`
	Revision  int                    `json:"revision"`
	CreatedBy string                 `json:"createdBy"`
	CreatedAt time.Time              `json:"createdAt"`
	ExpiresAt *time.Time             `json:"expiresAt,omitempty"`
	Decisions []IntelligenceDecision `json:"decisions"`
}

type TeamContextSelection struct {
	MemoryRecallReceiptID string   `json:"memoryRecallReceiptId,omitempty"`
	ReleaseID             string   `json:"releaseId"`
	MemoryIDs             []string `json:"memoryIds,omitempty"`
}

// ExecutionIntelligenceContext is server-built and frozen into a Run; callers
// select IDs only. Hash checks prove content identity, not current authority.
type ExecutionIntelligenceContext struct {
	MemoryRecallReceipt *MemoryRecallReceipt `json:"memoryRecallReceipt,omitempty"`
	Version             int                  `json:"version"`
	ProjectID           string               `json:"projectId"`
	ReleaseID           string               `json:"releaseId"`
	ReleaseSHA256       string               `json:"releaseSha256"`
	Configuration       TeamReleasePayload   `json:"configuration"`
	Memories            []GovernedMemory     `json:"memories"`
	RepositoryBaseSHAs  map[string]string    `json:"repositoryBaseSHAs"`
	SHA256              string               `json:"sha256"`
}

func IntelligenceDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(raw) > IntelligenceMaxSnapshotBytes {
		return "", fmt.Errorf("intelligence snapshot exceeds %d bytes", IntelligenceMaxSnapshotBytes)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func (v ExecutionIntelligenceContext) Digest() (string, error) {
	v.SHA256 = ""
	return IntelligenceDigest(v)
}
func MemoryContentDigest(v GovernedMemory) (string, error) {
	return IntelligenceDigest(struct {
		Title     string             `json:"title"`
		Content   string             `json:"content"`
		Source    IntelligenceSource `json:"source"`
		ExpiresAt *time.Time         `json:"expiresAt,omitempty"`
	}{v.Title, v.Content, v.Source, v.ExpiresAt})
}
func ValidateIntelligenceSource(v IntelligenceSource) error {
	if v.ExtractionID != "" && !ValidWorkspaceContextSHA256(v.ExtractionID) {
		return fmt.Errorf("invalid extraction receipt id")
	}
	if v.Commit != "" && !ValidGitCommitSHA(v.Commit) {
		return fmt.Errorf("source commit requires a full lowercase SHA")
	}
	if v.Repository != "" {
		u, err := url.Parse(v.Repository)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("source repository must not contain credentials or query parameters")
		}
	}
	if len(v.Repository)+len(v.Path)+len(v.RunID)+len(v.ArtifactID) > 4096 {
		return fmt.Errorf("source provenance is too large")
	}
	return nil
}
func ValidGitCommitSHA(v string) bool {
	if len(v) != 40 {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ValidateTeamReleasePayload checks complete immutable resources and rejects
// credential-bearing MCP configuration rather than copying tokens into versions.
func ValidateTeamReleasePayload(v TeamReleasePayload, projectID string) error {
	if err := ValidateTeamDistribution(v); err != nil {
		return err
	}
	if strings.TrimSpace(projectID) == "" {
		return fmt.Errorf("project is required")
	}
	if err := ValidateIntelligenceSource(v.Source); err != nil {
		return err
	}
	if err := ValidateTeamGitImport(v.GitImport, v.Source); err != nil {
		return err
	}
	if len(v.Agents) > 64 || len(v.Assets.Rules) > 128 || len(v.Assets.Skills) > 128 || len(v.Assets.MCPServers) > 64 {
		return fmt.Errorf("too many release resources")
	}
	seen := map[string]bool{}
	materialized := map[string]bool{}
	scope := func(id, p string, shared bool) error {
		if strings.HasSuffix(id, ":") || seen[id] {
			return fmt.Errorf("missing or duplicate release resource")
		}
		seen[id] = true
		kind, rawID, _ := strings.Cut(id, ":")
		name := RuntimeResourceName(rawID)
		if name == "" || materialized[kind+":"+name] {
			return fmt.Errorf("resource identities collide after runtime materialization")
		}
		materialized[kind+":"+name] = true
		if p != projectID && !shared {
			return fmt.Errorf("resource outside project scope")
		}
		return nil
	}
	for _, a := range v.Agents {
		if err := scope("agent:"+a.ID, a.ProjectID, a.Shared); err != nil {
			return err
		}
		if a.PlatformMCP || a.KnowledgeBase {
			return fmt.Errorf("release Agent must disable dynamic platform/knowledge MCP; use pinned governed memory")
		}
	}
	for _, r := range v.Assets.Rules {
		if err := scope("rule:"+r.ID, r.ProjectID, r.Shared); err != nil {
			return err
		}
	}
	for _, s := range v.Assets.Skills {
		if err := scope("skill:"+s.ID, s.ProjectID, s.Shared); err != nil {
			return err
		}
		if err := ValidateManagedFiles(s.Files); err != nil {
			return err
		}
		if err := ValidateReleaseSkill(s, v.Assets.SkillFiles[s.ID]); err != nil {
			return err
		}
		if err := ValidateSkillFileRefs(s.FileRefs); err != nil {
			return err
		}
		for _, ref := range s.FileRefs {
			b, ok := v.Assets.SkillFiles[s.ID][ref.Path]
			if !ok || int64(len(b)) != ref.Size {
				return fmt.Errorf("release skill binary missing or size mismatch")
			}
			sum := sha256.Sum256(b)
			if ref.SHA256 == "" || hex.EncodeToString(sum[:]) != ref.SHA256 {
				return fmt.Errorf("release skill binary digest mismatch")
			}
			if _, exists := s.Files[ref.Path]; exists {
				return fmt.Errorf("duplicate text/binary skill path")
			}
		}
	}
	for _, m := range v.Assets.MCPServers {
		if err := scope("mcp:"+m.ID, m.ProjectID, false); err != nil {
			return err
		}
		if m.Scope != "project" || m.OwnerUserID != 0 || m.AuthType != "" || len(m.Headers) > 0 || len(m.Environment) > 0 {
			return fmt.Errorf("release MCP must be project-scoped without credential bindings; configure credentials outside formal releases")
		}
		u, err := url.Parse(m.URL)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("release MCP URL must not carry credentials")
		}
		if err := ValidateMCPServer(m); err != nil {
			return err
		}
		for _, ref := range m.FileRefs {
			b, ok := v.Assets.MCPFiles[m.ID][ref.Path]
			if !ok || int64(len(b)) != ref.Size {
				return fmt.Errorf("release MCP binary missing or size mismatch")
			}
			sum := sha256.Sum256(b)
			if ref.SHA256 == "" || hex.EncodeToString(sum[:]) != ref.SHA256 {
				return fmt.Errorf("release MCP binary digest mismatch")
			}
		}
	}

	for _, a := range v.Agents {
		for _, id := range a.Rules {
			if !seen["rule:"+id] {
				return fmt.Errorf("Agent rule absent from release")
			}
		}
		for _, id := range a.Skills {
			if !seen["skill:"+id] {
				return fmt.Errorf("Agent skill absent from release")
			}
		}
		for _, id := range a.MCPServers {
			if !seen["mcp:"+id] {
				return fmt.Errorf("Agent MCP absent from release")
			}
		}
	}
	for id, files := range v.Assets.SkillFiles {
		if !seen["skill:"+id] {
			return fmt.Errorf("unlisted skill binary resource")
		}
		for path := range files {
			found := false
			for _, skill := range v.Assets.Skills {
				if skill.ID == id {
					for _, ref := range skill.FileRefs {
						if ref.Path == path {
							found = true
						}
					}
				}
			}
			if !found {
				return fmt.Errorf("unlisted skill binary path")
			}
		}
	}
	for id, files := range v.Assets.MCPFiles {
		if !seen["mcp:"+id] {
			return fmt.Errorf("unlisted MCP binary resource")
		}
		for path := range files {
			found := false
			for _, mcp := range v.Assets.MCPServers {
				if mcp.ID == id {
					for _, ref := range mcp.FileRefs {
						if ref.Path == path {
							found = true
						}
					}
				}
			}
			if !found {
				return fmt.Errorf("unlisted MCP binary path")
			}
		}
	}
	_, err := IntelligenceDigest(v)
	return err
}

func CanonicalIntelligenceIDs(ids []string, maxItems int) ([]string, error) {
	out := append([]string{}, ids...)
	sort.Strings(out)
	if len(out) > maxItems {
		return nil, fmt.Errorf("too many selected resources")
	}
	for i, id := range out {
		if strings.TrimSpace(id) == "" || len(id) > 128 || (i > 0 && id == out[i-1]) {
			return nil, fmt.Errorf("missing or duplicate resource id")
		}
	}
	return out, nil
}
