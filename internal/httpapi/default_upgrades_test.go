package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

// Adopt through the real HTTP clone path, then compare the original platform
// definition, new default, original mapped project copy, and current team copy.
func TestGenericDefaultUpgradePreservesOverridesAndOriginalBaseline(t *testing.T) {
	h, s := newGenericTemplateServer(t)
	template := repo.GenericTemplate{Workflow: domain.WorkflowDefinition{ID: "upgrade-template", Name: "Upgrade template", SandboxProfileID: "local", Nodes: []domain.WorkflowNode{{ID: "work", Type: "agent", AgentID: "upgrade-agent"}}}, Agents: []domain.AgentDefinition{{ID: "upgrade-agent", Name: "Default agent", Prompt: "original prompt", Rules: []string{"upgrade-rule"}, Skills: []string{"upgrade-skill"}}}, Rules: []domain.RuleDefinition{{ID: "upgrade-rule", Name: "Default rule", Content: "original rule"}}, Skills: []domain.SkillDefinition{{ID: "upgrade-skill", Name: "Default skill", Content: "original skill"}}}
	if err := s.PutGenericTemplate(template); err != nil {
		t.Fatal(err)
	}
	// A sibling owns the plain IDs: adoption must remap all references, and the
	// diff must not mistake those mandatory binding changes for team overrides.
	if err := s.PutAgent(domain.AgentDefinition{ID: "upgrade-agent", Name: "Sibling", ProjectID: "beta"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRule(domain.RuleDefinition{ID: "upgrade-rule", Name: "Sibling", ProjectID: "beta"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSkill(domain.SkillDefinition{ID: "upgrade-skill", Name: "Sibling", ProjectID: "beta"}); err != nil {
		t.Fatal(err)
	}
	adopt := func(id string) {
		requestJSON[domain.WorkflowDefinition](t, h, http.MethodPost, "/api/workflows/upgrade-template/clone", map[string]any{"id": id, "name": id, "projectId": "alpha"}, http.StatusCreated)
	}
	adopt("upgrade-alpha-first")
	path := "/api/projects/alpha/default-upgrades"
	initial := requestEmptyJSON[[]domain.DefaultUpgradeDiff](t, h, http.MethodGet, path, http.StatusOK)
	if len(initial) != 3 {
		t.Fatalf("initial resources: %+v", initial)
	}
	baselineIDs := map[string]string{}
	for _, diff := range initial {
		if !diff.Tracked || !diff.DefaultAvailable || len(diff.OverridePaths) != 0 || len(diff.DefaultChangedPaths) != 0 {
			t.Fatalf("remapping incorrectly counted as override: %+v", diff)
		}
		baselineIDs[diff.Kind] = diff.BaselineRevisionID
	}
	agent, _ := s.Agent("upgrade-agent-alpha")
	agent.Prompt = "team prompt"
	if err := s.PutAgent(agent); err != nil {
		t.Fatal(err)
	}
	rule, _ := s.Rule("upgrade-rule-alpha")
	rule.Content = "team rule"
	if err := s.PutRule(rule); err != nil {
		t.Fatal(err)
	}
	skill, _ := s.Skill("upgrade-skill-alpha")
	skill.Content = "team skill"
	if err := s.PutSkill(skill); err != nil {
		t.Fatal(err)
	}
	template.Agents[0].Prompt = "new default prompt"
	template.Rules[0].Content = "new default rule"
	template.Skills[0].Content = "new default skill"
	if err := s.PutGenericTemplate(template); err != nil {
		t.Fatal(err)
	}
	compare := func() {
		diffs := requestEmptyJSON[[]domain.DefaultUpgradeDiff](t, h, http.MethodGet, path, http.StatusOK)
		if len(diffs) != 3 {
			t.Fatalf("diff count: %d", len(diffs))
		}
		for _, diff := range diffs {
			field := "/content"
			original := "original " + diff.Kind
			team := "team " + diff.Kind
			latest := "new default " + diff.Kind
			if diff.Kind == "agent" {
				field = "/prompt"
				original = "original prompt"
				team = "team prompt"
				latest = "new default prompt"
			}
			if diff.BaselineRevisionID != baselineIDs[diff.Kind] || !slices.Contains(diff.DefaultChangedPaths, field) || !slices.Contains(diff.OverridePaths, field) || !slices.Contains(diff.ConflictingPaths, field) || !strings.Contains(string(diff.BaseDefault), original) || !strings.Contains(string(diff.CurrentDefault), latest) || !strings.Contains(string(diff.TeamOverride), team) {
				t.Fatalf("lost three-way content/conflict: %+v", diff)
			}
		}
	}
	compare()
	adopt("upgrade-alpha-second")
	compare()
	// Existing unknown origins stay unknown; no read/startup/re-adoption backfill.
	if err := s.PutRule(domain.RuleDefinition{ID: "untracked", Name: "Old local rule", ProjectID: "alpha", Content: "keep"}); err != nil {
		t.Fatal(err)
	}
	diffs, err := s.DefaultUpgrades("alpha")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diff := range diffs {
		if diff.EntityID == "untracked" {
			found = true
			if diff.Tracked || diff.BaselineRevisionID != "" {
				t.Fatal("historical lineage fabricated")
			}
		}
	}
	if !found {
		t.Fatal("unknown asset missing")
	}
	// The immutable base occupies existing revision storage, not a new registry;
	// malformed evidence must fail closed rather than appear untracked.
	if err := s.DB.Table("definition_revisions").Where("id = ?", baselineIDs["agent"]).Update("payload", json.RawMessage(`{"id":"corrupt"}`)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.DefaultUpgrades("alpha"); err == nil {
		t.Fatal("corrupt baseline hidden")
	}
}
