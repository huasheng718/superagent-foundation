package httpapi

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

// adoptGenericWorkflow clones a built-in generic template into a project: the
// workflow becomes an editable draft and every role it references is cloned
// into the same project with remapped ids, so the copy is immediately runnable
// instead of failing on "agent not found".
func (h *catalogHandlers) adoptGenericWorkflow(template domain.WorkflowDefinition, id, name, owner, projectID string) (domain.WorkflowDefinition, error) {
	var out domain.WorkflowDefinition
	err := h.repo.WithGenericTemplateCopy(template.ID, projectID, func(store *repo.Store) error {
		scopedDeps := *h.deps
		scopedDeps.repo = store
		scoped := *h
		scoped.deps = &scopedDeps
		var err error
		out, err = scoped.adoptGenericWorkflowLocked(template, id, name, owner, projectID)
		return err
	})
	if err == nil {
		h.configureSync(h.repo.SyncConfig())
	}
	return out, err
}
func (h *catalogHandlers) adoptGenericWorkflowLocked(template domain.WorkflowDefinition, id, name, owner, projectID string) (domain.WorkflowDefinition, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return domain.WorkflowDefinition{}, errors.New("通用模板必须复制到具体项目后才能使用")
	}
	if _, found := h.repo.Project(projectID); !found {
		return domain.WorkflowDefinition{}, errors.New("目标项目不存在，请刷新后重新选择")
	}
	if err := ensureDefaultProjectMCPs(h.repo, projectID); err != nil {
		return domain.WorkflowDefinition{}, fmt.Errorf("安装默认 MCP: %w", err)
	}
	sandboxID := h.genericTemplateSandbox(template.SandboxProfileID, projectID)
	if sandboxID == "" {
		return domain.WorkflowDefinition{}, errors.New("当前项目没有可用沙箱，请先在「沙箱管理」创建或共享一个沙箱")
	}
	agents, err := h.cloneGenericTemplateAgents(template.ID, projectID)
	if err != nil {
		return domain.WorkflowDefinition{}, err
	}
	adopted := template
	// Copy the slices so remapping node bindings can never leak back into the
	// shared built-in catalog.
	adopted.Nodes = append([]domain.WorkflowNode(nil), template.Nodes...)
	adopted.Edges = append([]domain.WorkflowEdge(nil), template.Edges...)
	adopted.Tags = append(append([]string(nil), template.Tags...), "generic-source:"+template.ID)
	adopted.Requirements = append([]domain.WorkflowRequirement(nil), template.Requirements...)
	adopted.ID, adopted.Name, adopted.Owner = id, name, owner
	adopted.ProjectID = projectID
	adopted.Shared = false
	adopted.Status = "draft"
	adopted.Version = 1
	adopted.Revision = 0
	adopted.Generic = false
	if strings.TrimSpace(adopted.SOPLane) == "" {
		adopted.SOPLane = domain.InferSOPLane(template)
	}
	adopted.CreatedAt, adopted.UpdatedAt = time.Time{}, time.Time{}
	adopted.SandboxProfileID = sandboxID
	for index := range adopted.Nodes {
		if target, ok := agents[adopted.Nodes[index].AgentID]; ok {
			adopted.Nodes[index].AgentID = target
		}
	}
	if err := h.repo.PutWorkflow(adopted); err != nil {
		// The outer template-copy transaction rolls the whole adoption back.
		return domain.WorkflowDefinition{}, err
	}
	stored, _ := h.repo.Workflow(id)
	return stored, nil
}

// cloneGenericTemplateAgents copies the template roles into the project and
// returns the source-id -> project-id mapping used to rewrite node bindings.
// A role that already exists in the project is reused, so adopting the
// template twice never overwrites local edits.
func (h *catalogHandlers) cloneGenericTemplateAgents(workflowID, projectID string) (map[string]string, error) {
	template, ok := h.repo.GenericTemplateByWorkflow(workflowID)
	if !ok {
		return nil, fmt.Errorf("generic template %q not found", workflowID)
	}
	now := time.Now().UTC()
	// LoopForge rules and skills ship with the template: clone them into the
	// project first so the role bindings can be remapped onto real assets.
	ruleRemap, err := h.cloneGenericTemplateRules(template.Rules, projectID, workflowID, now)
	if err != nil {
		return nil, err
	}
	skillRemap, err := h.cloneGenericTemplateSkills(template.Skills, projectID, workflowID, now)
	if err != nil {
		return nil, err
	}
	remap := make(map[string]string, len(template.Agents))
	for _, source := range template.Agents {
		if existing, found := h.repo.Agent(source.ID); found && existing.ProjectID == projectID {
			h.upgradeGenericTemplateAgent(&existing, source, projectID, now)
			remap[source.ID] = existing.ID
			continue
		}
		// Projects that did not get the plain template id (a sibling project
		// owns it) hold a deterministic <id>-<project> clone instead; reuse it
		// so re-adopting the template never duplicates the roles.
		if existing, found := h.repo.Agent(source.ID + "-" + projectID); found && existing.ProjectID == projectID {
			h.upgradeGenericTemplateAgent(&existing, source, projectID, now)
			remap[source.ID] = existing.ID
			continue
		}
		agent := source
		state, err := domain.CopyDistribution(source)
		if err != nil {
			return nil, err
		}
		agent.DistributionState = state
		agent.SourceRevision = ""
		agent.ID = h.availableGenericAgentID(source.ID, projectID)
		agent.ProjectID = projectID
		agent.Shared = false
		agent.Version = 1
		agent.UpdatedAt = now
		agent.MCPServers = h.resolveGenericTemplateMCP(agent.MCPServers, projectID)
		agent.Rules = remapAssetIDs(agent.Rules, ruleRemap)
		agent.Skills = remapAssetIDs(agent.Skills, skillRemap)
		if err := h.repo.PutAgent(agent); err != nil {
			return nil, err
		}
		stored, _ := h.repo.Agent(agent.ID)
		if err := h.repo.RecordTemplateDefaultBase("agent", projectID, workflowID, source.ID, agent.ID, source, stored); err != nil {
			return nil, err
		}
		remap[source.ID] = agent.ID
	}
	return remap, nil
}

// upgradeGenericTemplateAgent refreshes an existing clone on re-adoption. The
// only thing it ever touches is the MCP binding, and only when the role has
// none: a clone adopted before the project installed the template MCP stays
// unbound, and re-adopting is the documented way to pick the install up. A
// role with any binding may hold deliberate local edits and is left alone.
func (h *catalogHandlers) upgradeGenericTemplateAgent(existing *domain.AgentDefinition, source domain.AgentDefinition, projectID string, now time.Time) {
	if len(existing.MCPServers) > 0 || len(source.MCPServers) == 0 {
		return
	}
	resolved := h.resolveGenericTemplateMCP(source.MCPServers, projectID)
	if len(resolved) == 0 {
		return
	}
	_ = h.repo.FillAgentMCPBindings(existing.ID, projectID, resolved)
}

func (h *catalogHandlers) cloneGenericTemplateRules(sources []domain.RuleDefinition, projectID, templateID string, now time.Time) (map[string]string, error) {
	remap := make(map[string]string, len(sources))
	for _, source := range sources {
		if existing, found := h.repo.Rule(source.ID); found && existing.ProjectID == projectID {
			remap[source.ID] = existing.ID
			continue
		}
		if existing, found := h.repo.Rule(source.ID + "-" + projectID); found && existing.ProjectID == projectID {
			remap[source.ID] = existing.ID
			continue
		}
		rule := source
		state, err := domain.CopyDistribution(source)
		if err != nil {
			return nil, err
		}
		rule.DistributionState = state
		rule.SourceRevision = ""
		rule.ID = h.availableGenericRuleID(source.ID, projectID)
		rule.ProjectID = projectID
		rule.Shared = false
		rule.UpdatedAt = now
		if err := h.repo.PutRule(rule); err != nil {
			return nil, err
		}
		if templateID != "" {
			if err := h.repo.RecordTemplateDefaultBase("rule", projectID, templateID, source.ID, rule.ID, source, rule); err != nil {
				return nil, err
			}
		}
		remap[source.ID] = rule.ID
	}
	return remap, nil
}

func (h *catalogHandlers) cloneGenericTemplateSkills(sources []domain.SkillDefinition, projectID, templateID string, now time.Time) (map[string]string, error) {
	remap := make(map[string]string, len(sources))
	for _, source := range sources {
		if existing, found := h.repo.Skill(source.ID); found && existing.ProjectID == projectID {
			remap[source.ID] = existing.ID
			continue
		}
		if existing, found := h.repo.Skill(source.ID + "-" + projectID); found && existing.ProjectID == projectID {
			remap[source.ID] = existing.ID
			continue
		}
		skill := source
		state, err := domain.CopyDistribution(source)
		if err != nil {
			return nil, err
		}
		skill.DistributionState = state
		skill.SourceRevision = ""
		skill.ID = h.availableGenericSkillID(source.ID, projectID)
		skill.ProjectID = projectID
		skill.Shared = false
		skill.UpdatedAt = now
		if err := h.repo.PutSkill(skill); err != nil {
			return nil, err
		}
		if templateID != "" {
			if err := h.repo.RecordTemplateDefaultBase("skill", projectID, templateID, source.ID, skill.ID, source, skill); err != nil {
				return nil, err
			}
		}
		remap[source.ID] = skill.ID
	}
	return remap, nil
}

// remapAssetIDs rewrites template asset references onto the cloned project ids,
// keeping order and platform-wide ids (which resolve globally) untouched.
func remapAssetIDs(ids []string, remap map[string]string) []string {
	if len(ids) == 0 || len(remap) == 0 {
		return ids
	}
	out := make([]string, len(ids))
	for index, id := range ids {
		if mapped, ok := remap[id]; ok {
			out[index] = mapped
			continue
		}
		out[index] = id
	}
	return out
}

// genericAssetIDCandidates lists the deterministic ids tried for a cloned
// template asset, in order: the template id itself, then the id suffixed with
// the owning project. The suffix keeps the clone predictable and reusable, so
// re-adopting the template in the same project never piles up duplicates.
func genericAssetIDCandidates(sourceID, projectID string) []string {
	return []string{sourceID, sourceID + "-" + projectID}
}

func (h *catalogHandlers) availableGenericRuleID(sourceID, projectID string) string {
	return h.pickAvailableID(genericAssetIDCandidates(sourceID, projectID), func(id string) bool {
		_, taken := h.repo.Rule(id)
		return taken
	})
}

func (h *catalogHandlers) availableGenericSkillID(sourceID, projectID string) string {
	return h.pickAvailableID(genericAssetIDCandidates(sourceID, projectID), func(id string) bool {
		_, taken := h.repo.Skill(id)
		return taken
	})
}

func (h *catalogHandlers) availableGenericAgentID(sourceID, projectID string) string {
	return h.pickAvailableID(genericAssetIDCandidates(sourceID, projectID), func(id string) bool {
		_, taken := h.repo.Agent(id)
		return taken
	})
}

// pickAvailableID returns the first candidate that is not taken, falling back
// to random suffixes when every deterministic candidate is owned by something
// else.
func (h *catalogHandlers) pickAvailableID(candidates []string, taken func(string) bool) string {
	for _, candidate := range candidates {
		if !taken(candidate) {
			return candidate
		}
	}
	sourceID := candidates[0]
	for attempt := 0; attempt < 20; attempt++ {
		candidate := fmt.Sprintf("%s-%s", sourceID, randomID()[:6])
		if !taken(candidate) {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%d", sourceID, time.Now().UnixNano())
}

// resolveGenericTemplateMCP maps a template MCP reference onto an asset that is
// actually usable by the project. The template declares market aliases (for
// example "gongfeng"), while a run can only bind a project-scoped install of
// that market item. Adopt installs the platform defaults first, so the common
// Gongfeng/TAPD aliases resolve without a market click. Other missing installs
// are still dropped so the node can degrade.
func (h *catalogHandlers) resolveGenericTemplateMCP(ids []string, projectID string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, found := h.repo.MCPServer(id); found {
			out = append(out, id)
			continue
		}
		item, found := marketItem(id)
		if !found {
			for _, server := range h.repo.MCPServers() {
				if server.ProjectID == projectID && server.Scope == "project" && (strings.EqualFold(server.Name, id) || strings.HasPrefix(strings.ToLower(server.Name), strings.ToLower(id)+" ")) {
					out = append(out, server.ID)
					break
				}
			}
			continue
		}
		for _, server := range h.repo.MCPServers() {
			if server.Scope != "project" || server.ProjectID != projectID || server.MarketID == "" {
				continue
			}
			if sameMarketItem(server.MarketID, item.ID) {
				out = append(out, server.ID)
				break
			}
		}
	}
	return out
}

// genericTemplateSandbox prefers the template sandbox and falls back to a
// sandbox the project can actually run, so a deployment without the built-in
// "local" profile still adopts the template.
func (h *catalogHandlers) genericTemplateSandbox(preferred, projectID string) string {
	if profile, found := h.repo.Sandbox(preferred); found && (profile.ProjectID == projectID || profile.ProjectID == "" || profile.Shared) {
		return profile.ID
	}
	for _, profile := range h.repo.Sandboxes() {
		if profile.ProjectID == projectID {
			return profile.ID
		}
	}
	for _, profile := range h.repo.Sandboxes() {
		if profile.Shared || profile.ProjectID == "" {
			return profile.ID
		}
	}
	return ""
}
