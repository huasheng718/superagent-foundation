package repo

import (
	"agentflow/internal/domain"
	"errors"
	"testing"
)

func TestDistributionOwnerCASAndEditFloor(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutProject(domain.Project{ID: "distribution-project", Name: "Project", OwnerID: 1001}); err != nil {
		t.Fatal(err)
	}
	rule, err := s.SaveRule(domain.RuleDefinition{ID: "distribution-rule", Name: "Rule", ProjectID: "distribution-project", Content: "before"})
	if err != nil {
		t.Fatal(err)
	}
	policy := domain.DistributionPolicy{Copy: "allow", Edit: "deny", Export: "deny"}
	if _, err := s.SetDistribution("rule", rule.ID, rule.SourceRevision, policy, "review", DistributionActor{UserID: 1002}); !errors.Is(err, ErrDistributionOwner) {
		t.Fatal(err)
	}
	decision, err := s.SetDistribution("rule", rule.ID, rule.SourceRevision, policy, "review", DistributionActor{UserID: 1001})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SourceRevision == rule.SourceRevision || decision.EffectiveDistribution.Export != "deny" {
		t.Fatal(decision)
	}
	if _, err := s.SetDistribution("rule", rule.ID, rule.SourceRevision, policy, "stale", DistributionActor{UserID: 1001}); !errors.Is(err, ErrSourceConflict) {
		t.Fatal(err)
	}
	current, _ := s.Rule(rule.ID)
	current.Content = "changed"
	if err := s.PutRule(current); !errors.Is(err, domain.ErrDistributionDenied) {
		t.Fatal(err)
	}
	allow := domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "allow"}
	decision, err = s.SetDistribution("rule", rule.ID, decision.SourceRevision, allow, "reopen", DistributionActor{UserID: 1001})
	if err != nil {
		t.Fatal(err)
	}
	current, _ = s.Rule(rule.ID)
	current.DistributionState = domain.DistributionState{}
	current.Content = "allowed"
	saved, err := s.SaveRule(current)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Distribution == nil || saved.Distribution.Edit != "allow" {
		t.Fatal("old client erased policy")
	}
	// The receiving owner cannot relax an inherited floor even after changing ID.
	original := domain.RuleDefinition{ID: "source", Name: "Source", DistributionState: domain.DistributionState{Distribution: &policy}}
	floor, err := domain.CopyDistribution(original)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := s.SaveRule(domain.RuleDefinition{ID: "distribution-copy", Name: "Copy", ProjectID: "distribution-project", DistributionState: floor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDistribution("rule", copy.ID, copy.SourceRevision, allow, "wash", DistributionActor{UserID: 1001}); !errors.Is(err, domain.ErrDistributionDenied) {
		t.Fatal(err)
	}
	copy.Distribution = nil
	copy.DistributionOrigins = nil
	if err := s.PutRule(copy); !errors.Is(err, domain.ErrDistributionDenied) {
		t.Fatal(err)
	}
	if err := s.DeleteRule(copy.ID, copy.SourceRevision); err != nil {
		t.Fatal("lifecycle delete must remain possible", err)
	}
}
func TestDistributionSkillFilesAndManagedAgentCopy(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	skill, err := s.SaveSkillMetadata(domain.SkillDefinition{ID: "distribution-skill", Name: "Skill", ProjectID: "platform"})
	if err != nil {
		t.Fatal(err)
	}
	policy := domain.DistributionPolicy{Copy: "deny", Edit: "deny", Export: "deny"}
	d, err := s.SetDistribution("skill", skill.ID, skill.SourceRevision, policy, "lock files", DistributionActor{Local: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSkillFiles(skill.ID, []SkillFileInput{{Path: "a", Content: []byte("x")}}, d.SourceRevision); !errors.Is(err, domain.ErrDistributionDenied) {
		t.Fatal(err)
	}
	agent, err := s.SaveAgent(domain.AgentDefinition{ID: "distribution-agent", Name: "Agent", ProjectID: "platform"})
	if err != nil {
		t.Fatal(err)
	}
	d, err = s.SetDistribution("agent", agent.ID, agent.SourceRevision, policy, "deny copy", DistributionActor{Local: true})
	if err != nil {
		t.Fatal(err)
	}
	source, _ := s.Agent(agent.ID)
	target := source
	target.ID = "distribution-target"
	if _, err := s.CreateAgentClone(target, nil, source); !errors.Is(err, domain.ErrDistributionDenied) {
		t.Fatal(err)
	}
	if _, ok := s.Agent(target.ID); ok {
		t.Fatal("partial copy")
	}
	if _, err := s.CloneAgentForEvaluation(agent.ID, "distribution-eval", "platform", d.SourceRevision); !errors.Is(err, domain.ErrDistributionDenied) {
		t.Fatal(err)
	}
}

func TestDistributionDeniedEditStillAllowsUnshare(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rule, err := s.SaveRule(domain.RuleDefinition{ID: "distribution-unshare", Name: "Rule", ProjectID: "platform", Shared: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SetDistribution("rule", rule.ID, rule.SourceRevision, domain.DistributionPolicy{Copy: "deny", Edit: "deny", Export: "deny"}, "withdraw later", DistributionActor{Local: true})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := s.Rule(rule.ID)
	current.Shared = false
	current.DistributionState = domain.DistributionState{}
	saved, err := s.SaveRule(current)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Shared || saved.Distribution == nil || saved.Distribution.Edit != "deny" {
		t.Fatal("unshare lost policy", saved)
	}
	saved.Shared = true
	if err := s.PutRule(saved); !errors.Is(err, domain.ErrDistributionDenied) {
		t.Fatal("reshare must not bypass edit", err)
	}
}
