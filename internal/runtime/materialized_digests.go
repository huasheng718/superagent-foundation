package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"agentflow/internal/domain"
)

// MeasureMaterializedResources verifies actual files before process launch.
// MCP configs are deliberately excluded: binding may add private credentials.
// Their immutable source is already covered by the assignment manifest.
func MeasureMaterializedResources(workspace string, agent domain.AgentDefinition, assets domain.AgentAssetBundle) ([]domain.RunnerMaterializedDigest, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	records := []domain.RunnerMaterializedDigest{}
	measure := func(kind, id, path string, source, expected []byte, mode uint32) error {
		info, err := root.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("materialized %s resource missing or not regular", kind)
		}
		if info.Size() != int64(len(expected)) || uint32(info.Mode().Perm()) != mode {
			return fmt.Errorf("materialized %s resource size or mode mismatch", kind)
		}
		actual, err := root.ReadFile(path)
		if err != nil || !bytes.Equal(actual, expected) {
			return fmt.Errorf("materialized %s resource digest mismatch", kind)
		}
		sourceSHA, actualSHA := sha256.Sum256(source), sha256.Sum256(actual)
		records = append(records, domain.RunnerMaterializedDigest{Kind: kind, ResourceID: id, Path: filepath.ToSlash(path), SourceSHA256: hex.EncodeToString(sourceSHA[:]), SHA256: hex.EncodeToString(actualSHA[:]), Size: int64(len(actual)), Mode: mode})
		return nil
	}
	for _, rule := range assets.Rules {
		data := []byte(rule.Content)
		if err := measure("rule", rule.ID, filepath.Join(".agentflow", "rules", safeID(rule.ID)+".md"), data, data, 0o600); err != nil {
			return nil, err
		}
	}
	for _, skill := range assets.Skills {
		binary := assets.SkillFiles[skill.ID]
		item, err := compileSelectedSkill(filepath.Join(".agentflow", safeID(agent.ID)), skill, binary)
		if err != nil {
			return nil, err
		}
		raw := []byte(skill.Content)
		if text, ok := skill.Files["SKILL.md"]; ok {
			raw = []byte(text)
		}
		if data, ok := binary["SKILL.md"]; ok {
			raw = data
		}
		base := filepath.Join(".agentflow", "skills", item.id)
		if err := measure("skill", skill.ID, filepath.Join(base, "SKILL.md"), raw, item.body, 0o644); err != nil {
			return nil, err
		}
		files := map[string][]byte{}
		modes := map[string]uint32{}
		for name, text := range skill.Files {
			if name != "SKILL.md" {
				files[name] = []byte(text)
				modes[name] = 0o644
			}
		}
		for _, ref := range skill.FileRefs {
			if ref.Path == "SKILL.md" {
				continue
			}
			data, ok := binary[ref.Path]
			if !ok {
				return nil, fmt.Errorf("materialized skill binary source unavailable")
			}
			files[ref.Path] = data
			modes[ref.Path] = uint32(domain.NormalizeSkillFileMode(ref.Mode))
		}
		for name, data := range files {
			if !domain.SafeWorkspaceRelPath(name) {
				return nil, fmt.Errorf("unsafe materialized skill path")
			}
			if err := measure("skill", skill.ID, filepath.Join(base, name), data, data, modes[name]); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	return records, nil
}
