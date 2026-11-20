package repo

import (
	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func (r *CatalogRepo) validateExtractionTeam(tx *gorm.DB, run domain.Run, task domain.RunnerTask) error {
	ctx := run.Intelligence
	hash, err := ctx.Digest()
	if err != nil || hash != ctx.SHA256 || ctx.Version != 1 || ctx.ProjectID != run.ExecutionContext.ProjectID || ctx.ReleaseID != run.ExecutionContext.TeamContext.ReleaseID {
		return ErrExtractionSource
	}
	var row teamReleaseRow
	if err := r.locking(tx).Where("project_id = ? AND id = ?", ctx.ProjectID, ctx.ReleaseID).Take(&row).Error; err != nil {
		return ErrIntelligenceUnavailable
	}
	release, err := decodeTeamRelease(row)
	if err != nil {
		return err
	}
	payloadHash, err := domain.IntelligenceDigest(ctx.Configuration)
	if err != nil || release.State != domain.TeamReleasePublished || release.SHA256 != ctx.ReleaseSHA256 || payloadHash != release.SHA256 {
		return ErrIntelligenceUnavailable
	}
	if err := unrestrictedExtractionPayload(ctx.Configuration); err != nil {
		return err
	}
	if len(ctx.RepositoryBaseSHAs) != len(run.ExecutionContext.WorkspaceBaseSHAs) {
		return ErrExtractionSource
	}
	for id, sha := range ctx.RepositoryBaseSHAs {
		if sha != run.ExecutionContext.WorkspaceBaseSHAs[id] {
			return ErrExtractionSource
		}
	}
	found := false
	actualAgentHash, _ := domain.IntelligenceDigest(task.Invocation.Agent)
	for _, a := range ctx.Configuration.Agents {
		if a.ID == task.Invocation.Agent.ID {
			expectedHash, err := domain.IntelligenceDigest(a)
			found = err == nil && expectedHash == actualAgentHash
		}
	}
	if !found {
		return ErrExtractionSource
	}
	// A structurally valid governance binding does not itself prove that every
	// concrete invocation resource belongs to that frozen input closure.
	knownRules, knownSkills, knownMCP := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range ctx.Configuration.Assets.Rules {
		knownRules[v.ID] = true
	}
	for _, v := range ctx.Configuration.Assets.Skills {
		knownSkills[v.ID] = true
	}
	for _, v := range ctx.Configuration.Assets.MCPServers {
		knownMCP[v.ID] = true
	}
	for _, v := range ctx.Memories {
		knownRules["governed-"+v.ID] = true
	}
	for _, v := range task.Invocation.Assets.Rules {
		if !knownRules[v.ID] {
			return extractionInvalid("invocation rule is outside the frozen input closure")
		}
	}
	for _, v := range task.Invocation.Assets.Skills {
		if !knownSkills[v.ID] {
			return extractionInvalid("invocation skill is outside the frozen input closure")
		}
	}
	for _, v := range task.Invocation.Assets.MCPServers {
		if !knownMCP[v.ID] {
			return extractionInvalid("invocation MCP is outside the frozen input closure")
		}
	}
	for id := range task.Invocation.Assets.SkillFiles {
		if !knownSkills[id] {
			return ErrExtractionSource
		}
	}
	for id := range task.Invocation.Assets.MCPFiles {
		if !knownMCP[id] {
			return ErrExtractionSource
		}
	}

	// Current policy is also authoritative: a post-execution restriction cannot
	// be bypassed with an earlier frozen allowed snapshot under MySQL RR.
	scoped := &CatalogRepo{deps: &deps{DB: tx}}
	for _, a := range ctx.Configuration.Agents {
		var v domain.AgentDefinition
		if err := scoped.lockTeamDistributionSource("agent", a.ID, &v); err != nil {
			return err
		}
		if err := unrestrictedExtractionResource(v); err != nil {
			return err
		}
	}
	for _, a := range ctx.Configuration.Assets.Rules {
		var v domain.RuleDefinition
		if err := scoped.lockTeamDistributionSource("rule", a.ID, &v); err != nil {
			return err
		}
		if err := unrestrictedExtractionResource(v); err != nil {
			return err
		}
	}
	for _, a := range ctx.Configuration.Assets.Skills {
		var v domain.SkillDefinition
		if err := scoped.lockTeamDistributionSource("skill", a.ID, &v); err != nil {
			return err
		}
		if err := unrestrictedExtractionResource(v); err != nil {
			return err
		}
	}
	for _, a := range ctx.Configuration.Assets.MCPServers {
		var v domain.MCPServerDefinition
		if err := scoped.lockTeamDistributionSource("mcp", a.ID, &v); err != nil {
			return err
		}
		if err := unrestrictedExtractionResource(v); err != nil {
			return err
		}
	}
	return nil
}
