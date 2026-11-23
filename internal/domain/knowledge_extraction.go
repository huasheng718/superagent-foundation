package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const KnowledgeExtractionMaxBytes = 256 << 10

var ErrKnowledgeExtractionInvalid = errors.New("invalid knowledge extraction")

type KnowledgeExtractionRequest struct {
	RunID                  string `json:"runId"`
	NodeID                 string `json:"nodeId"`
	ArtifactID             string `json:"artifactId"`
	ExpectedArtifactSHA256 string `json:"expectedArtifactSHA256"`
	RepositoryID           string `json:"repositoryId"`
	ExpectedCommit         string `json:"expectedCommit"`
}
type KnowledgeSuggestion struct {
	Key           string            `json:"key"`
	Kind          string            `json:"kind"`
	Title         string            `json:"title,omitempty"`
	Content       string            `json:"content,omitempty"`
	SkillID       string            `json:"skillId,omitempty"`
	Description   string            `json:"description,omitempty"`
	Files         map[string]string `json:"files,omitempty"`
	NoticePaths   []string          `json:"noticePaths,omitempty"`
	EvidencePaths []string          `json:"evidencePaths"`
	LicenseClaim  string            `json:"licenseClaim"`
}
type KnowledgeSuggestionArtifact struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Suggestions   []KnowledgeSuggestion `json:"suggestions"`
}

// All provenance except explicitly labelled claims is derived from durable
// control-plane records. No prompt, credentials or provider raw body is kept.
type KnowledgeExtractionSource struct {
	ResponsibleUserID         int64                     `json:"responsibleUserId"`
	AcceptedContribution      RunnerContributionReceipt `json:"acceptedContribution"`
	InvocationID              string                    `json:"invocationId"`
	BudgetPolicy              ExecutionBudgetPolicy     `json:"budgetPolicy"`
	BudgetReceipt             ExecutionBudgetReceipt    `json:"budgetReceipt"`
	RunID                     string                    `json:"runId"`
	Generation                int                       `json:"generation"`
	RunRevision               int                       `json:"runRevision"`
	NodeID                    string                    `json:"nodeId"`
	NodeAttempt               int                       `json:"nodeAttempt"`
	TaskID                    string                    `json:"taskId"`
	TaskAttempt               int                       `json:"taskAttempt"`
	RunnerID                  string                    `json:"runnerId"`
	RuntimeVersion            string                    `json:"runtimeVersion,omitempty"`
	ArtifactID                string                    `json:"artifactId"`
	ArtifactSHA256            string                    `json:"artifactSHA256"`
	ArtifactPath              string                    `json:"artifactPath"`
	ArtifactSize              int64                     `json:"artifactSize"`
	RepositoryID              string                    `json:"repositoryId"`
	Repository                string                    `json:"repository"`
	Commit                    string                    `json:"commit"`
	ContributionID            string                    `json:"contributionId"`
	ContributionContextSHA256 string                    `json:"contributionContextSHA256"`
	AgentID                   string                    `json:"agentId"`
	ReleaseID                 string                    `json:"releaseId"`
	ReleaseSHA256             string                    `json:"releaseSHA256"`
	IntelligenceSHA256        string                    `json:"intelligenceSHA256"`
	Provider                  string                    `json:"provider"`
	ConfiguredModel           string                    `json:"configuredModel,omitempty"`
	ObservedModel             string                    `json:"observedModel,omitempty"`
	ModelObservation          string                    `json:"modelObservation"`
	SessionID                 string                    `json:"sessionId"`
	Usage                     TokenUsage                `json:"usage"`
	BudgetPolicySHA256        string                    `json:"budgetPolicySHA256"`
	StartedAt                 time.Time                 `json:"startedAt"`
	FinishedAt                time.Time                 `json:"finishedAt"`
	Deadline                  time.Time                 `json:"deadline"`
}
type KnowledgeExtractionItem struct {
	Key             string   `json:"key"`
	Kind            string   `json:"kind"`
	CandidateID     string   `json:"candidateId"`
	CandidateSHA256 string   `json:"candidateSHA256"`
	LicenseClaim    string   `json:"licenseClaim"`
	EvidencePaths   []string `json:"evidencePaths"`
	EvidenceStatus  string   `json:"evidenceStatus"`
}
type KnowledgeExtractionReceipt struct {
	CreatedByUserID int64                     `json:"createdByUserId,omitempty"`
	ID              string                    `json:"id"`
	ProjectID       string                    `json:"projectId"`
	SchemaVersion   int                       `json:"schemaVersion"`
	Source          KnowledgeExtractionSource `json:"source"`
	Items           []KnowledgeExtractionItem `json:"items"`
	Policy          string                    `json:"policy"`
	CreatedBy       string                    `json:"createdBy"`
	CreatedAt       time.Time                 `json:"createdAt"`
	SHA256          string                    `json:"sha256"`
}

func (v KnowledgeExtractionReceipt) Digest() (string, error) {
	v.SHA256 = ""
	return IntelligenceDigest(v)
}

// This key excludes the submitting actor/time and includes the effective source
// execution, so retries by another member cannot create additional candidates.
func KnowledgeExtractionID(project string, s KnowledgeExtractionSource) string {
	raw, _ := json.Marshal([]any{1, project, s.RunID, s.Generation, s.NodeID, s.NodeAttempt, s.TaskID, s.TaskAttempt, s.ArtifactID, s.ArtifactSHA256, s.RepositoryID, s.Commit})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (v KnowledgeExtractionRequest) Validate() error {
	for _, id := range []string{v.RunID, v.NodeID, v.ArtifactID, v.RepositoryID} {
		if strings.TrimSpace(id) == "" || len(id) > 128 {
			return ErrKnowledgeExtractionInvalid
		}
	}
	if !ValidWorkspaceContextSHA256(v.ExpectedArtifactSHA256) || !ValidGitCommitSHA(v.ExpectedCommit) {
		return ErrKnowledgeExtractionInvalid
	}
	return nil
}
func ParseKnowledgeExtractionRequest(raw []byte) (KnowledgeExtractionRequest, error) {
	var v KnowledgeExtractionRequest
	if len(raw) > 8192 {
		return v, ErrKnowledgeExtractionInvalid
	}
	if err := strictKnowledgeJSON(raw, &v); err != nil {
		return v, err
	}
	return v, v.Validate()
}
func ParseKnowledgeSuggestions(raw []byte, project string) (KnowledgeSuggestionArtifact, error) {
	var v KnowledgeSuggestionArtifact
	if len(raw) == 0 || len(raw) > KnowledgeExtractionMaxBytes {
		return v, ErrKnowledgeExtractionInvalid
	}
	if err := strictKnowledgeJSON(raw, &v); err != nil {
		return v, err
	}
	if v.SchemaVersion != 1 || len(v.Suggestions) < 1 || len(v.Suggestions) > 8 {
		return v, ErrKnowledgeExtractionInvalid
	}
	keys := map[string]bool{}
	for _, s := range v.Suggestions {
		if !knowledgeKey(s.Key) || keys[s.Key] || strings.TrimSpace(s.LicenseClaim) == "" || len(s.LicenseClaim) > 1988 || len(s.EvidencePaths) > 16 {
			return v, ErrKnowledgeExtractionInvalid
		}
		keys[s.Key] = true
		for _, p := range s.EvidencePaths {
			if !knowledgePath(p) {
				return v, ErrKnowledgeExtractionInvalid
			}
		}
		switch s.Kind {
		case "memory":
			if strings.TrimSpace(s.Title) == "" || len(s.Title) > 200 || strings.TrimSpace(s.Content) == "" || len(s.Content) > MemoryMaxContentBytes || s.SkillID != "" || s.Description != "" || len(s.Files) > 0 || len(s.NoticePaths) > 0 {
				return v, ErrKnowledgeExtractionInvalid
			}
		case "skill":
			if s.Title != "" || s.Content != "" || strings.TrimSpace(s.Description) == "" || len(s.Description) > 2000 || len(s.Files) < 1 || len(s.Files) > 8 {
				return v, ErrKnowledgeExtractionInvalid
			}
			size := 0
			for p, b := range s.Files {
				if !knowledgePath(p) {
					return v, ErrKnowledgeExtractionInvalid
				}
				size += len(b)
			}
			if size > 64<<10 {
				return v, ErrKnowledgeExtractionInvalid
			}
			if err := ValidateSkillCandidatePackage(s.SkillPackage(project, IntelligenceSource{Path: "knowledge-candidates.json"}, "generated"), project); err != nil {
				return v, fmt.Errorf("%w: skill package failed validation", ErrKnowledgeExtractionInvalid)
			}
		default:
			return v, ErrKnowledgeExtractionInvalid
		}
	}
	return v, nil
}
func (s KnowledgeSuggestion) SkillPackage(project string, source IntelligenceSource, publisher string) SkillCandidatePackage {
	return SkillCandidatePackage{Skill: SkillDefinition{ID: s.SkillID, Name: s.SkillID, ProjectID: project, Description: s.Description, Files: s.Files}, Source: source, Publisher: publisher, License: "UNREVIEWED: " + s.LicenseClaim, NoticePaths: s.NoticePaths}
}
func knowledgeKey(v string) bool {
	if len(v) < 1 || len(v) > 64 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func knowledgePath(v string) bool {
	return v != "" && len(v) <= 512 && v != "." && v != ".." && !strings.HasPrefix(v, "../") && !path.IsAbs(v) && path.Clean(v) == v && !strings.ContainsAny(v, "\\\x00")
}

// DisallowUnknownFields alone accepts duplicate object keys. A bounded token
// walk first rejects ambiguity at every depth, then decodes the typed shape.
func strictKnowledgeJSON(raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return ErrKnowledgeExtractionInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 16 {
			return ErrKnowledgeExtractionInvalid
		}
		t, err := d.Token()
		if err != nil {
			return ErrKnowledgeExtractionInvalid
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		if delim == '{' {
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return ErrKnowledgeExtractionInvalid
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return ErrKnowledgeExtractionInvalid
				}
				seen[key] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		} else if delim == '[' {
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		} else {
			return ErrKnowledgeExtractionInvalid
		}
		_, err = d.Token()
		if err != nil {
			return ErrKnowledgeExtractionInvalid
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrKnowledgeExtractionInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrKnowledgeExtractionInvalid
	}
	return nil
}
