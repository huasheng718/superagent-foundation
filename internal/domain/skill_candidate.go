package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const SkillCandidateQuarantined = "quarantined"

type ReviewedSkillProvenance struct {
	CandidateID   string             `json:"candidateId"`
	PackageSHA256 string             `json:"packageSHA256"`
	Source        IntelligenceSource `json:"source"`
	Publisher     string             `json:"publisher"`
	License       string             `json:"license"`
	NoticePaths   []string           `json:"noticePaths"`
}

// A regular-file byte package, never a filesystem/archive extraction request.
// Binary uses JSON base64 byte values; its manifest is mandatory and verified.
type SkillCandidatePackage struct {
	Skill        SkillDefinition    `json:"skill"`
	Binary       map[string][]byte  `json:"binary,omitempty"`
	Source       IntelligenceSource `json:"source"`
	Publisher    string             `json:"publisher"`
	License      string             `json:"license"`
	NoticePaths  []string           `json:"noticePaths"`
	SupersedesID string             `json:"supersedesId,omitempty"`
}
type SkillCandidate struct {
	Resources       []SkillFileRef         `json:"resources,omitempty"`
	ID              string                 `json:"id"`
	ProjectID       string                 `json:"projectId"`
	State           string                 `json:"state"`
	Revision        int                    `json:"revision"`
	SHA256          string                 `json:"sha256"`
	Package         SkillCandidatePackage  `json:"package"`
	CreatedBy       string                 `json:"createdBy"`
	CreatedAt       time.Time              `json:"createdAt"`
	Decisions       []IntelligenceDecision `json:"decisions"`
	PromotedSkillID string                 `json:"promotedSkillId,omitempty"`
}

func SkillCandidateEntry(v SkillCandidatePackage) []byte {
	raw := []byte(v.Skill.Content)
	if text, ok := v.Skill.Files["SKILL.md"]; ok {
		raw = []byte(text)
	}
	if data, ok := v.Binary["SKILL.md"]; ok {
		raw = data
	}
	return raw
}
func ValidateSkillCandidatePackage(v SkillCandidatePackage, projectID string) error {
	s := v.Skill
	if len(s.ID) > 48 || s.ID == "" || RuntimeResourceName(s.ID) != s.ID || s.ID[0] == '-' || strings.HasSuffix(s.ID, "-") {
		return fmt.Errorf("candidate requires a canonical skill id of at most 48 lowercase letters, digits or hyphens")
	}
	if s.ProjectID != projectID || s.Shared || s.SourceRevision != "" || s.Source != nil || !s.UpdatedAt.IsZero() {
		return fmt.Errorf("candidate skill scope and source must be server-owned")
	}
	if strings.TrimSpace(v.Publisher) == "" || len(v.Publisher) > 200 || !utf8.ValidString(v.Publisher) || strings.TrimSpace(v.License) == "" || len(v.License) > 2000 || !utf8.ValidString(v.License) {
		return fmt.Errorf("bounded publisher and license records required")
	}
	if err := ValidateIntelligenceSource(v.Source); err != nil {
		return err
	}
	if v.Source.RunID == "" && v.Source.Repository == "" && v.Source.Path == "" {
		return fmt.Errorf("candidate source provenance required")
	}
	if v.Source.ArtifactID != "" && v.Source.RunID == "" {
		return fmt.Errorf("candidate artifact source requires Run")
	}
	if err := ValidateTeamReleasePayload(TeamReleasePayload{Assets: AgentAssetBundle{Skills: []SkillDefinition{s}, SkillFiles: map[string]map[string][]byte{s.ID: v.Binary}}}, projectID); err != nil {
		return err
	}
	if len(v.NoticePaths) == 0 || len(v.NoticePaths) > 16 {
		return fmt.Errorf("one or more license/NOTICE resource paths required")
	}
	seen := map[string]bool{}
	for _, p := range v.NoticePaths {
		if seen[p] {
			return fmt.Errorf("duplicate license/NOTICE path")
		}
		seen[p] = true
		if _, ok := s.Files[p]; !ok {
			if _, ok := v.Binary[p]; !ok {
				return fmt.Errorf("license/NOTICE resource absent")
			}
		}
	}
	raw := string(SkillCandidateEntry(v))
	if s.Content != "" && s.Content != raw {
		return fmt.Errorf("skill Content and selected SKILL.md disagree")
	}
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return fmt.Errorf("candidate requires SKILL.md with frontmatter")
	}
	header, _, ok := strings.Cut(normalized[4:], "\n---\n")
	if !ok {
		return fmt.Errorf("candidate SKILL.md frontmatter is not closed")
	}
	name, description := "", ""
	for _, line := range strings.Split(header, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if key == "name" {
			if name != "" {
				return fmt.Errorf("duplicate SKILL.md name")
			}
			name = value
		}
		if key == "description" {
			if description != "" {
				return fmt.Errorf("duplicate SKILL.md description")
			}
			description = value
		}
	}
	if name != s.ID || description == "" {
		return fmt.Errorf("SKILL.md name must equal candidate skill id and description is required")
	}
	_, err := IntelligenceDigest(v)
	return err
}

// Derived inspection inventory; the package SHA remains the immutable identity.
func SkillCandidateResources(v SkillCandidatePackage) []SkillFileRef {
	entries := map[string][]byte{}
	modes := map[string]uint32{}
	for path, text := range v.Skill.Files {
		entries[path] = []byte(text)
		modes[path] = 0o644
	}
	for _, ref := range v.Skill.FileRefs {
		entries[ref.Path] = v.Binary[ref.Path]
		modes[ref.Path] = NormalizeSkillFileMode(ref.Mode)
	}
	if _, ok := entries["SKILL.md"]; !ok {
		entries["SKILL.md"] = []byte(v.Skill.Content)
		modes["SKILL.md"] = 0o644
	}
	out := []SkillFileRef{}
	for path, data := range entries {
		sum := sha256.Sum256(data)
		out = append(out, SkillFileRef{Path: path, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Mode: modes[path]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
