package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

// ResolveIntelligenceContext builds a fresh authorized snapshot from explicit
// IDs. It never imports caller-supplied release or memory content.
func (e *Engine) ResolveIntelligenceContext(projectID string, selection domain.TeamContextSelection, pins map[string]string, responsibleUsers ...int64) (*domain.ExecutionIntelligenceContext, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(selection.ReleaseID) == "" {
		return nil, fmt.Errorf("project and releaseId required")
	}
	release, err := e.repo.TeamRelease(projectID, selection.ReleaseID)
	if err != nil {
		return nil, err
	}
	if release.State != domain.TeamReleasePublished {
		return nil, repo.ErrIntelligenceUnavailable
	}
	if err := domain.ValidateTeamReleasePayload(release.Payload, projectID); err != nil {
		return nil, err
	}
	ids, err := domain.CanonicalIntelligenceIDs(selection.MemoryIDs, domain.MemoryMaxRecallItems)
	if err != nil {
		return nil, err
	}
	ctx := &domain.ExecutionIntelligenceContext{Version: 1, ProjectID: projectID, ReleaseID: release.ID, ReleaseSHA256: release.SHA256, Configuration: release.Payload, Memories: []domain.GovernedMemory{}, RepositoryBaseSHAs: map[string]string{}}
	bytes := 0
	for _, id := range ids {
		m, err := e.repo.GovernedMemory(projectID, id)
		if err != nil {
			return nil, err
		}
		if m.State != domain.MemoryApproved || (m.ExpiresAt != nil && !m.ExpiresAt.After(time.Now().UTC())) {
			return nil, repo.ErrIntelligenceUnavailable
		}
		bytes += len(m.Content)
		if bytes > domain.MemoryMaxRecallBytes {
			return nil, fmt.Errorf("selected memory exceeds recall budget")
		}
		ctx.Memories = append(ctx.Memories, m)
	}
	if selection.MemoryRecallReceiptID != "" {
		if len(responsibleUsers) != 1 || responsibleUsers[0] <= 0 {
			return nil, repo.ErrIntelligenceUnavailable
		}
		receipt, err := e.repo.MemoryRecallReceipt(projectID, selection.MemoryRecallReceiptID, responsibleUsers[0], true)
		if err != nil {
			return nil, err
		}
		if err = receipt.ValidatePins(ctx.Memories); err != nil {
			return nil, err
		}
		ctx.MemoryRecallReceipt = &receipt
	}
	for id, sha := range pins {
		if strings.TrimSpace(id) == "" || !domain.ValidGitCommitSHA(sha) {
			return nil, fmt.Errorf("context requires exact repository SHA pins")
		}
		repository, ok := e.repo.Repository(id)
		if !ok || repository.ProjectID != projectID {
			return nil, repo.ErrIntelligenceUnavailable
		}
		ctx.RepositoryBaseSHAs[id] = sha
	}
	ctx.SHA256, err = ctx.Digest()
	if err != nil {
		return nil, err
	}
	return ctx, nil
}

// FreezeRunIntelligence is called once before the first Run persistence. Retry
// must retain the resulting Run.Intelligence instead of resolving latest assets.
func (e *Engine) FreezeRunIntelligence(run *domain.Run) error {
	if run.ExecutionContext.TeamContext == nil {
		run.Intelligence = nil
		return nil
	}
	ctx, err := e.ResolveIntelligenceContext(run.ExecutionContext.ProjectID, *run.ExecutionContext.TeamContext, run.ExecutionContext.WorkspaceBaseSHAs, run.ExecutionContext.ResponsibleUserID)
	if err != nil {
		return err
	}
	if len(run.RepositoryContexts()) > 0 {
		if len(ctx.RepositoryBaseSHAs) != len(run.RepositoryContexts()) {
			return fmt.Errorf("governed context requires exact pins for all assigned repositories")
		}
		for _, r := range run.RepositoryContexts() {
			if _, ok := ctx.RepositoryBaseSHAs[r.RepositoryID]; !ok {
				return fmt.Errorf("governed context repository pin missing")
			}
		}
	}
	// Every executable Agent must come from the approved release; no live catalog
	// fallback is allowed for a missing role or dynamically generated preview.
	for _, node := range run.WorkflowSnapshot.Nodes {
		if node.Type == "agent" {
			found := false
			for _, a := range ctx.Configuration.Agents {
				if a.ID == node.AgentID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("Agent %q absent from selected team release", node.AgentID)
			}
		}
		if node.Type == "sop" {
			return fmt.Errorf("governed context child SOP inheritance is not supported")
		}
	}
	run.Intelligence = ctx
	return e.ValidateRunIntelligence(*run)
}

func (e *Engine) ValidateRunIntelligence(run domain.Run) error {
	ctx := run.Intelligence
	if run.ExecutionContext.TeamContext == nil {
		if ctx != nil {
			return fmt.Errorf("unexpected intelligence snapshot")
		}
		return nil
	}
	if ctx == nil || ctx.Version != 1 || ctx.ProjectID != run.ExecutionContext.ProjectID || ctx.ReleaseID != run.ExecutionContext.TeamContext.ReleaseID {
		return fmt.Errorf("governed execution context missing or mismatched")
	}
	digest, err := ctx.Digest()
	if err != nil || digest != ctx.SHA256 {
		return fmt.Errorf("governed context digest mismatch")
	}
	release, err := e.repo.TeamRelease(ctx.ProjectID, ctx.ReleaseID)
	if err != nil {
		return err
	}
	if release.State != domain.TeamReleasePublished || release.SHA256 != ctx.ReleaseSHA256 {
		return repo.ErrIntelligenceUnavailable
	}
	payloadDigest, err := domain.IntelligenceDigest(ctx.Configuration)
	if err != nil || payloadDigest != ctx.ReleaseSHA256 {
		return fmt.Errorf("governed configuration digest mismatch")
	}
	ids, err := domain.CanonicalIntelligenceIDs(run.ExecutionContext.TeamContext.MemoryIDs, domain.MemoryMaxRecallItems)
	if err != nil || len(ids) != len(ctx.Memories) {
		return fmt.Errorf("governed memory selection mismatch")
	}
	for i, pinned := range ctx.Memories {
		if ids[i] != pinned.ID {
			return fmt.Errorf("governed memory selection mismatch")
		}
		current, err := e.repo.GovernedMemory(ctx.ProjectID, pinned.ID)
		if err != nil {
			return err
		}
		if current.State != domain.MemoryApproved || current.Revision != pinned.Revision || current.SHA256 != pinned.SHA256 || (current.ExpiresAt != nil && !current.ExpiresAt.After(time.Now().UTC())) {
			return repo.ErrIntelligenceUnavailable
		}
		hash, err := domain.MemoryContentDigest(pinned)
		if err != nil || hash != current.SHA256 {
			return fmt.Errorf("governed memory content changed")
		}
	}
	selectedReceipt := run.ExecutionContext.TeamContext.MemoryRecallReceiptID
	if selectedReceipt == "" {
		if ctx.MemoryRecallReceipt != nil {
			return fmt.Errorf("unexpected memory recall receipt")
		}
	} else {
		pinned := ctx.MemoryRecallReceipt
		if pinned == nil || pinned.ID != selectedReceipt || pinned.Scope.ProjectID != ctx.ProjectID || pinned.Scope.UserID != run.ExecutionContext.ResponsibleUserID {
			return fmt.Errorf("memory recall receipt scope mismatch")
		}
		receipt, err := e.repo.MemoryRecallReceipt(ctx.ProjectID, selectedReceipt, run.ExecutionContext.ResponsibleUserID, false)
		if err != nil {
			return err
		}
		digest, err := pinned.Digest()
		if err != nil || digest != pinned.SHA256 || digest != receipt.SHA256 {
			return fmt.Errorf("memory recall receipt digest mismatch")
		}
		if err := pinned.ValidatePins(ctx.Memories); err != nil {
			return err
		}
	}
	if len(ctx.RepositoryBaseSHAs) != len(run.ExecutionContext.WorkspaceBaseSHAs) {
		return fmt.Errorf("governed repository pins changed")
	}
	for id, sha := range ctx.RepositoryBaseSHAs {
		if run.ExecutionContext.WorkspaceBaseSHAs[id] != sha {
			return fmt.Errorf("governed repository pins changed")
		}
	}
	return nil
}

// RunAgent and RunAssets are the normal SOP execution seam, not a separate
// runtime. A selected release replaces live catalog reads for role content.
func (e *Engine) RunAgent(run domain.Run, wf domain.WorkflowDefinition, id string) (domain.AgentDefinition, bool) {
	if run.Intelligence == nil {
		if run.ExecutionContext.TeamContext != nil {
			return domain.AgentDefinition{}, false
		}
		return workflowAgent(e.repo, wf, id)
	}
	if e.ValidateRunIntelligence(run) != nil {
		return domain.AgentDefinition{}, false
	}
	for _, a := range run.Intelligence.Configuration.Agents {
		if a.ID == id {
			return a, true
		}
	}
	return domain.AgentDefinition{}, false
}
func (e *Engine) RunAssets(run domain.Run, agent domain.AgentDefinition) (domain.AgentAssetBundle, error) {
	if run.Intelligence == nil {
		if run.ExecutionContext.TeamContext != nil {
			return domain.AgentAssetBundle{}, fmt.Errorf("selected governed context snapshot is missing")
		}
		return e.resolveAssets(agent, run.ExecutionContext.ProjectID, run.WorkflowSnapshot.ID, run.ExecutionContext.ResponsibleUserID), nil
	}
	if err := e.ValidateRunIntelligence(run); err != nil {
		return domain.AgentAssetBundle{}, err
	}
	all := run.Intelligence.Configuration.Assets
	out := domain.AgentAssetBundle{Rules: []domain.RuleDefinition{}, Skills: []domain.SkillDefinition{}, MCPServers: []domain.MCPServerDefinition{}}
	has := func(ids []string, id string) bool {
		for _, v := range ids {
			if v == id {
				return true
			}
		}
		return false
	}
	for _, r := range all.Rules {
		if has(agent.Rules, r.ID) || (r.Global && ruleTargetsWorkflow(r, run.WorkflowSnapshot.ID)) {
			out.Rules = append(out.Rules, r)
		}
	}
	for _, s := range all.Skills {
		if has(agent.Skills, s.ID) {
			out.Skills = append(out.Skills, s)
			if files := all.SkillFiles[s.ID]; len(files) > 0 {
				if out.SkillFiles == nil {
					out.SkillFiles = map[string]map[string][]byte{}
				}
				out.SkillFiles[s.ID] = files
			}
		}
	}
	for _, m := range all.MCPServers {
		if has(agent.MCPServers, m.ID) {
			out.MCPServers = append(out.MCPServers, m)
			if files := all.MCPFiles[m.ID]; len(files) > 0 {
				if out.MCPFiles == nil {
					out.MCPFiles = map[string]map[string][]byte{}
				}
				out.MCPFiles[m.ID] = files
			}
		}
	}
	for _, m := range run.Intelligence.Memories {
		out.Rules = append(out.Rules, domain.RuleDefinition{ID: "governed-" + m.ID, Name: m.Title, ProjectID: m.ProjectID, Priority: 50, Content: fmt.Sprintf("# Reviewed team memory\nSource: %s\nSHA256: %s\n\n%s", m.Source.Path, m.SHA256, m.Content)})
	}
	// Return an independent copy: downstream materialization may sanitize or
	// augment assets without mutating the immutable persisted context.
	raw, err := json.Marshal(out)
	if err != nil {
		return domain.AgentAssetBundle{}, err
	}
	var copy domain.AgentAssetBundle
	err = json.Unmarshal(raw, &copy)
	return copy, err
}

// Scripts have no Agent resource list. A governed script receives precisely
// the MCP set published in its selected project release, never personal or
// subsequently added catalog MCPs. Callers must still apply delivery filtering.
func (e *Engine) RunScriptAssets(run domain.Run) (domain.AgentAssetBundle, error) {
	if run.ExecutionContext.TeamContext == nil && run.Intelligence == nil {
		return e.resolveScriptMCPAssets(run.ExecutionContext.ProjectID, run.ExecutionContext.ResponsibleUserID), nil
	}
	if err := e.ValidateRunIntelligence(run); err != nil {
		return domain.AgentAssetBundle{}, err
	}
	assets := run.Intelligence.Configuration.Assets
	out := domain.AgentAssetBundle{MCPServers: assets.MCPServers, MCPFiles: assets.MCPFiles}
	raw, err := json.Marshal(out)
	if err != nil {
		return domain.AgentAssetBundle{}, err
	}
	var copy domain.AgentAssetBundle
	err = json.Unmarshal(raw, &copy)
	return copy, err
}
