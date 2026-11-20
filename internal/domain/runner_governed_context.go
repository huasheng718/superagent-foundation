package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// A compact provenance binding within the existing assignment manifest. It
// carries no instruction bodies or credentials and is not another run state.
const RunnerVersionGovernedContext = "0.12.0"

// Receipt metadata is part of the assignment digest. Older decoders would drop
// this field, so receipt-bearing assignments require this explicit capability.
const RunnerVersionMemoryRecallReceipt = "0.13.0"

func RunnerVersionSupportsMemoryRecallReceipt(version string) bool {
	return runnerVersionSupports(version, []string{"0", "13", "0"})
}

type RunnerGovernedMemory struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	SHA256   string `json:"sha256"`
}
type RunnerGovernedContext struct {
	MemoryRecallReceipt *MemoryRecallReceipt   `json:"memoryRecallReceipt,omitempty"`
	Version             int                    `json:"version"`
	IntelligenceSHA256  string                 `json:"intelligenceSHA256"`
	ProjectID           string                 `json:"projectId"`
	ReleaseID           string                 `json:"releaseId"`
	ReleaseSHA256       string                 `json:"releaseSHA256"`
	Memories            []RunnerGovernedMemory `json:"memories"`
	RepositoryBaseSHAs  map[string]string      `json:"repositoryBaseSHAs"`
	BudgetPolicySHA256  string                 `json:"budgetPolicySHA256,omitempty"`
}

// Measured before invoking a model/shell, not reconstructed from its output.
// Source and effective digests differ when SKILL.md frontmatter is normalized.
type RunnerMaterializedDigest struct {
	Kind         string `json:"kind"`
	ResourceID   string `json:"resourceId"`
	Path         string `json:"path"`
	SourceSHA256 string `json:"sourceSHA256"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	Mode         uint32 `json:"mode"`
}
type RunnerContextEvidence struct {
	Version            int                        `json:"version"`
	TaskID             string                     `json:"taskId"`
	Attempt            int                        `json:"attempt"`
	IntelligenceSHA256 string                     `json:"intelligenceSHA256"`
	ContextSHA256      string                     `json:"contextSHA256"`
	Files              []RunnerMaterializedDigest `json:"files"`
}

func RunnerVersionSupportsGovernedContext(version string) bool {
	return runnerVersionSupports(version, []string{"0", "12", "0"})
}
func GovernedContextForRun(run Run) (*RunnerGovernedContext, error) {
	ctx := run.Intelligence
	if ctx == nil {
		return nil, fmt.Errorf("governed context snapshot missing")
	}
	sha, err := ctx.Digest()
	if err != nil || sha != ctx.SHA256 {
		return nil, fmt.Errorf("governed context digest mismatch")
	}
	binding := &RunnerGovernedContext{Version: 1, IntelligenceSHA256: ctx.SHA256, ProjectID: ctx.ProjectID, ReleaseID: ctx.ReleaseID, ReleaseSHA256: ctx.ReleaseSHA256, Memories: []RunnerGovernedMemory{}, RepositoryBaseSHAs: map[string]string{}}
	for id, sha := range ctx.RepositoryBaseSHAs {
		binding.RepositoryBaseSHAs[id] = sha
	}
	for _, m := range ctx.Memories {
		binding.Memories = append(binding.Memories, RunnerGovernedMemory{m.ID, m.Revision, m.SHA256})
	}
	if ctx.MemoryRecallReceipt != nil {
		receipt := ctx.MemoryRecallReceipt
		digest, err := receipt.Digest()
		if err != nil || digest != receipt.SHA256 || receipt.Scope.ProjectID != ctx.ProjectID || receipt.Scope.UserID != run.ExecutionContext.ResponsibleUserID {
			return nil, fmt.Errorf("governed memory recall receipt mismatch")
		}
		if err := receipt.ValidatePins(ctx.Memories); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			return nil, err
		}
		binding.MemoryRecallReceipt = &MemoryRecallReceipt{}
		if err := json.Unmarshal(raw, binding.MemoryRecallReceipt); err != nil {
			return nil, err
		}
	}
	if run.ExecutionContext.ExecutionBudget != nil {
		binding.BudgetPolicySHA256 = ExecutionBudgetPolicySHA(run.ExecutionContext.ExecutionBudget)
	}
	return binding, nil
}
func ValidateRunnerGovernedContext(task RunnerTask) error {
	inv := task.Invocation
	if inv == nil || inv.GovernedContext == nil {
		return nil
	}
	b := inv.GovernedContext
	if task.Action != "" || task.ID == "" || task.RunID == "" || task.NodeID == "" || inv.RunID != task.RunID || inv.NodeID != task.NodeID || b.Version != 1 || !placementText(b.ProjectID) || !placementText(b.ReleaseID) || !ValidWorkspaceContextSHA256(b.IntelligenceSHA256) || !ValidWorkspaceContextSHA256(b.ReleaseSHA256) || (b.BudgetPolicySHA256 != "" && !ValidWorkspaceContextSHA256(b.BudgetPolicySHA256)) {
		return fmt.Errorf("invalid governed assignment identity, version or digest")
	}
	if len(b.Memories) > MemoryMaxRecallItems || len(b.RepositoryBaseSHAs) > 128 {
		return fmt.Errorf("governed binding exceeds resource limit")
	}
	seen := map[string]bool{}
	for _, m := range b.Memories {
		if !placementText(m.ID) || seen[m.ID] || m.Revision < 1 || !ValidWorkspaceContextSHA256(m.SHA256) {
			return fmt.Errorf("invalid governed memory binding")
		}
		seen[m.ID] = true
	}
	if receipt := b.MemoryRecallReceipt; receipt != nil {
		digest, err := receipt.Digest()
		if err != nil || digest != receipt.SHA256 || receipt.ID == "" || receipt.Scope.ProjectID != b.ProjectID || receipt.Scope.UserID <= 0 || receipt.Scope.AgentID == "" {
			return fmt.Errorf("invalid governed memory recall receipt")
		}
		memories := make([]GovernedMemory, 0, len(b.Memories))
		for _, m := range b.Memories {
			memories = append(memories, GovernedMemory{ID: m.ID, Revision: m.Revision, SHA256: m.SHA256})
		}
		if err := receipt.ValidatePins(memories); err != nil {
			return err
		}
	}
	repositories := inv.Repositories
	if len(repositories) == 0 && inv.Repository != nil {
		repositories = []RunnerRepository{*inv.Repository}
	}
	seen = map[string]bool{}
	for _, r := range repositories {
		if r.RepositoryID == "" || seen[r.RepositoryID] || !ValidGitCommitSHA(r.BaseSHA) || b.RepositoryBaseSHAs[r.RepositoryID] != r.BaseSHA {
			return fmt.Errorf("governed repository identity or pin mismatch")
		}
		seen[r.RepositoryID] = true
	}
	if len(seen) != len(b.RepositoryBaseSHAs) {
		return fmt.Errorf("governed repository allowlist mismatch")
	}
	// Bound the serialized envelope too, including oversized identity strings.
	if _, err := IntelligenceDigest(struct {
		Agent  AgentDefinition
		Assets AgentAssetBundle
	}{inv.Agent, inv.Assets}); err != nil {
		return err
	}
	if _, err := IntelligenceDigest(b); err != nil {
		return err
	}
	return nil
}

// Run-aware admission prevents replacing/removing governance provenance while
// recomputing an otherwise structurally valid P0 assignment hash.
func ValidateRunnerGovernedContextForRun(run Run, task RunnerTask) error {
	if task.Action != "" {
		return ValidateRunnerGovernedContext(task)
	}
	if run.Intelligence == nil {
		if task.Invocation != nil && task.Invocation.GovernedContext != nil {
			return fmt.Errorf("unexpected governed assignment")
		}
		return nil
	}
	if err := ValidateRunnerGovernedContext(task); err != nil {
		return err
	}
	if task.Invocation == nil || task.Invocation.GovernedContext == nil {
		return fmt.Errorf("governed assignment binding missing")
	}
	expected, err := GovernedContextForRun(run)
	if err != nil {
		return err
	}
	expectedSHA, err := IntelligenceDigest(expected)
	if err != nil {
		return err
	}
	actualSHA, err := IntelligenceDigest(task.Invocation.GovernedContext)
	if err != nil || expectedSHA != actualSHA {
		return fmt.Errorf("governed assignment differs from frozen Run")
	}
	return nil
}

// Evidence is accepted under the existing task-attempt lease fence. This
// validates reported measurements' linkage, not remote-host trust/attestation.
func ValidateRunnerContextEvidence(task RunnerTask, outputs *RunnerTaskOutputs) error {
	if task.Invocation == nil || task.Invocation.GovernedContext == nil {
		return nil
	}
	if outputs == nil || outputs.ContextEvidence == nil {
		return fmt.Errorf("governed completion requires context evidence")
	}
	v := outputs.ContextEvidence
	sha, err := RunnerContributionContextSHA(task)
	if err != nil {
		return err
	}
	if v.Version != 1 || v.TaskID != task.ID || v.Attempt != task.Attempt || v.IntelligenceSHA256 != task.Invocation.GovernedContext.IntelligenceSHA256 || v.ContextSHA256 != sha {
		return fmt.Errorf("governed evidence identity or context mismatch")
	}
	if task.Invocation.WorkspaceIsolation != nil && task.Invocation.WorkspaceIsolation.ContextSHA256 != sha {
		return fmt.Errorf("governed evidence assignment mismatch")
	}
	if len(v.Files) > 4096 {
		return fmt.Errorf("governed evidence too large")
	}
	type expectedFile struct {
		kind, id, source string
		normalized       bool
		mode             uint32
	}
	expected := map[string]expectedFile{}
	add := func(kind, id, path string, data []byte, normalized bool, mode uint32) {
		sum := sha256.Sum256(data)
		expected[path] = expectedFile{kind, id, hex.EncodeToString(sum[:]), normalized, mode}
	}
	for _, rule := range task.Invocation.Assets.Rules {
		add("rule", rule.ID, ".agentflow/rules/"+RuntimeResourceName(rule.ID)+".md", []byte(rule.Content), false, 0o600)
	}
	for _, skill := range task.Invocation.Assets.Skills {
		base := ".agentflow/skills/" + RuntimeResourceName(skill.ID) + "/"
		source := []byte(skill.Content)
		if text, ok := skill.Files["SKILL.md"]; ok {
			source = []byte(text)
		}
		if data, ok := task.Invocation.Assets.SkillFiles[skill.ID]["SKILL.md"]; ok {
			source = data
		}
		add("skill", skill.ID, base+"SKILL.md", source, true, 0o644)
		for name, text := range skill.Files {
			if name != "SKILL.md" {
				add("skill", skill.ID, base+name, []byte(text), false, 0o644)
			}
		}
		for _, ref := range skill.FileRefs {
			if ref.Path != "SKILL.md" {
				data, ok := task.Invocation.Assets.SkillFiles[skill.ID][ref.Path]
				if !ok {
					return fmt.Errorf("governed source binary unavailable")
				}
				add("skill", skill.ID, base+ref.Path, data, false, uint32(NormalizeSkillFileMode(ref.Mode)))
			}
		}
	}
	if len(expected) != len(v.Files) {
		return fmt.Errorf("governed evidence resource inventory mismatch")
	}
	seen := map[string]bool{}
	for _, f := range v.Files {
		if (f.Kind != "rule" && f.Kind != "skill") || !placementText(f.ResourceID) || !SafeWorkspaceRelPath(f.Path) || seen[f.Path] || !ValidWorkspaceContextSHA256(f.SourceSHA256) || !ValidWorkspaceContextSHA256(f.SHA256) || f.Size < 0 || f.Size > IntelligenceMaxSnapshotBytes || (f.Mode != 0o600 && f.Mode != 0o644 && f.Mode != 0o755) {
			return fmt.Errorf("invalid governed materialization evidence")
		}
		entry, ok := expected[f.Path]
		if !ok || entry.kind != f.Kind || entry.id != f.ResourceID || entry.source != f.SourceSHA256 || entry.mode != f.Mode || (!entry.normalized && f.SHA256 != entry.source) {
			return fmt.Errorf("governed evidence differs from assigned resource")
		}
		seen[f.Path] = true
	}
	return nil
}
