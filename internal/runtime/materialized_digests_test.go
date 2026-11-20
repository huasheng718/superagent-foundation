package runtime

import (
	"agentflow/internal/domain"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestMeasureMaterializedResourcesSeparatesSourceFromGeneratedAndChecksBytes(t *testing.T) {
	workspace := t.TempDir()
	binary := []byte{0, 255, 3, 4}
	assets := domain.AgentAssetBundle{Rules: []domain.RuleDefinition{{ID: "pinned-rule", Content: "original rule", Priority: 0}, {ID: "governed-memory", Content: "reviewed memory content", Priority: 50}}, Skills: []domain.SkillDefinition{{ID: "sample", Description: "Sample", Files: map[string]string{"SKILL.md": "# Original content", "nested/text.txt": "nested input"}, FileRefs: []domain.SkillFileRef{{Path: "nested/data.bin", Mode: 0o644, Size: int64(len(binary))}, {Path: "scripts/run.sh", Mode: 0o755, Size: 9}}}}, SkillFiles: map[string]map[string][]byte{"sample": {"nested/data.bin": binary, "scripts/run.sh": []byte("#!/bin/sh")}}}
	agent := domain.AgentDefinition{ID: "author", Name: "Author"}
	task := domain.RunnerTask{ID: "task", RunID: "run", NodeID: "node", Invocation: &domain.RunnerInvocation{Agent: agent, Assets: assets}}
	beforeSHA, _ := domain.RunnerContributionContextSHA(task)
	if _, err := materialize(workspace, "invocation", domain.ExecutorDefinition{Kind: "codex"}, agent, assets); err != nil {
		t.Fatal(err)
	}
	afterSHA, _ := domain.RunnerContributionContextSHA(task)
	if beforeSHA != afterSHA {
		t.Fatal("rendering priority mutated immutable assignment digest")
	}
	records, err := MeasureMaterializedResources(workspace, agent, assets)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 6 {
		t.Fatalf("resources=%d", len(records))
	}
	found := false
	for _, r := range records {
		if r.Path == ".agentflow/skills/sample/SKILL.md" {
			found = true
			sum := sha256.Sum256([]byte("# Original content"))
			if r.SourceSHA256 != hex.EncodeToString(sum[:]) || r.SourceSHA256 == r.SHA256 {
				t.Fatal("source/generated digest conflated")
			}
		}
	}
	if !found {
		t.Fatal("missing canonical SKILL.md")
	}
	target := filepath.Join(workspace, ".agentflow", "skills", "sample", "nested", "data.bin")
	if err := os.WriteFile(target, []byte{0, 255, 3, 5}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MeasureMaterializedResources(workspace, agent, assets); err == nil {
		t.Fatal("changed materialized bytes accepted")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := MeasureMaterializedResources(workspace, agent, assets); err == nil {
		t.Fatal("missing materialized bytes accepted")
	}
}
