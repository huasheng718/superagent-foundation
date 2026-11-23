package repo

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestCatalogSourceLegacyReadCASAndRecreation(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	legacy := domain.RuleDefinition{ID: "cas-legacy", Name: "Legacy", ProjectID: "platform", Content: "before"}
	if err := s.DB.Table("rules").Create(map[string]any{"id": legacy.ID, "name": legacy.Name, "payload": mustJSON(legacy)}).Error; err != nil {
		t.Fatal(err)
	}
	base, ok := s.Rule(legacy.ID)
	if !ok || !strings.HasPrefix(base.SourceRevision, "legacy-") {
		t.Fatal(base)
	}
	var raw []byte
	s.DB.Table("rules").Select("payload").Where("id = ?", legacy.ID).Row().Scan(&raw)
	if strings.Contains(string(raw), "sourceRevision") {
		t.Fatal("read backfilled legacy row")
	}
	if err := s.PutRule(legacy); !errors.Is(err, ErrSourcePrecondition) {
		t.Fatalf("missing base = %v", err)
	}
	winner := base
	winner.Content = "winner"
	saved, err := s.SaveRule(winner)
	if err != nil {
		t.Fatal(err)
	}
	if saved.SourceRevision == base.SourceRevision || strings.HasPrefix(saved.SourceRevision, "legacy-") {
		t.Fatal("revision not rotated")
	}
	loser := base
	loser.Content = "loser"
	if err := s.PutRule(loser); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("stale save = %v", err)
	}
	current, _ := s.Rule(base.ID)
	if current.Content != "winner" {
		t.Fatal(current)
	}
	if err := s.DeleteRule(base.ID, base.SourceRevision); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("stale delete = %v", err)
	}
	if err := s.DeleteRule(saved.ID, saved.SourceRevision); err != nil {
		t.Fatal(err)
	}
	legacy.Content = "recreated"
	recreated, err := s.SaveRule(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if recreated.SourceRevision == saved.SourceRevision {
		t.Fatal("reused deleted revision")
	}
	if err := s.PutRule(saved); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("ABA save = %v", err)
	}
}

func TestCatalogSourceFileManifestAndMetadataShareOneBase(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutSkill(domain.SkillDefinition{ID: "cas-skill", Name: "Skill", ProjectID: "platform", Content: "original"}); err != nil {
		t.Fatal(err)
	}
	base, _ := s.Skill("cas-skill")
	files := []SkillFileInput{{Path: "nested/data.bin", Content: []byte{0, 255, 1}, Mode: 0755}}
	uploaded, err := s.SaveSkillFiles(base.ID, files, base.SourceRevision)
	if err != nil {
		t.Fatal(err)
	}
	base.Name = "stale metadata"
	if _, err := s.SaveSkillMetadata(base); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("stale metadata = %v", err)
	}
	uploaded.Name = "new metadata"
	uploaded.Content = "forged"
	uploaded.Source = &domain.SkillSource{Provider: "forged"}
	uploaded.FileRefs = nil
	metadata, err := s.SaveSkillMetadata(uploaded)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Content != "original" || metadata.Source != nil || len(metadata.FileRefs) != 1 || metadata.FileRefs[0].Mode != 0755 {
		t.Fatal("metadata changed closure", metadata)
	}
	if _, err := s.RemoveSkillFile(metadata.ID, "nested/data.bin", uploaded.SourceRevision); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("stale file delete = %v", err)
	}
	current, _ := s.Skill(base.ID)
	bytes, ok := s.SkillFileContent(base.ID, "nested/data.bin")
	if !ok || !reflect.DeepEqual(bytes, files[0].Content) || current.SourceRevision != metadata.SourceRevision {
		t.Fatal("stale mutation changed accepted closure")
	}
	sourced := domain.SkillDefinition{ID: "cas-source", Name: "Source", ProjectID: "platform", Source: &domain.SkillSource{Provider: domain.SkillSourceSkillHub, Slug: "fixture", Version: "1"}}
	installed, err := s.SaveSkillSnapshot(sourced, files, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSkillFiles(installed.ID, files, installed.SourceRevision); !errors.Is(err, ErrSkillSourceReadOnly) {
		t.Fatalf("sourced upload = %v", err)
	}
	if _, err := s.RemoveSkillFile(installed.ID, files[0].Path, installed.SourceRevision); !errors.Is(err, ErrSkillSourceReadOnly) {
		t.Fatalf("sourced delete = %v", err)
	}
}

func TestCatalogSourceAgentStaleSharingAndRestoreAreAtomic(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutRule(domain.RuleDefinition{ID: "cas-share-rule", Name: "Rule", ProjectID: "platform", Content: "original"}); err != nil {
		t.Fatal(err)
	}
	agent, err := s.SaveAgent(domain.AgentDefinition{ID: "cas-agent", Name: "Agent", ProjectID: "platform", Version: 1, Prompt: "original", Rules: []string{"cas-share-rule"}})
	if err != nil {
		t.Fatal(err)
	}
	old := agent
	agent.Prompt = "winner"
	agent.Version = 2
	agent, err = s.SaveAgent(agent)
	if err != nil {
		t.Fatal(err)
	}
	before := s.Revisions("agent", agent.ID)
	if len(before) != 1 {
		t.Fatal(before)
	}
	old.Shared = true
	if _, err := s.SaveAgent(old); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("stale share = %v", err)
	}
	rule, _ := s.Rule("cas-share-rule")
	if rule.Shared || len(s.Revisions("agent", agent.ID)) != len(before) {
		t.Fatal("failed edit left sharing/history side effects")
	}
	agent.Shared = true
	agent, err = s.SaveAgentWithSharing(agent)
	if err != nil {
		t.Fatal(err)
	}
	sharedRule, _ := s.Rule(rule.ID)
	if !sharedRule.Shared || sharedRule.Content != rule.Content || sharedRule.SourceRevision == rule.SourceRevision {
		t.Fatal("sharing lost source content or token")
	}
	restored, err := s.RestoreAgentRevision(before[0].ID, agent.SourceRevision)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Prompt != "original" || restored.SourceRevision == old.SourceRevision || restored.SourceRevision == agent.SourceRevision || restored.Version <= agent.Version {
		t.Fatal(restored)
	}
	if _, err := s.RestoreAgentRevision(before[0].ID, agent.SourceRevision); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("stale restore = %v", err)
	}
	var historical domain.AgentDefinition
	if err := json.Unmarshal(before[0].Payload, &historical); err != nil || historical.Prompt != "original" {
		t.Fatal("historical bytes changed")
	}
}

func TestCatalogSourceMCPMaskedSecretsAndImportRollback(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	value, err := s.SaveMCPServer(domain.MCPServerDefinition{ID: "cas-mcp", Name: "MCP", ProjectID: "platform", Scope: "project", Transport: "http", URL: "https://example.invalid", Headers: map[string]string{"Authorization": "Bearer private-fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	public := domain.PublicMCPServer(value)
	if public.SourceRevision != value.SourceRevision {
		t.Fatal("public masking lost revision")
	}
	public.Name = "new name"
	saved, err := s.SaveMCPServer(public)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Headers["Authorization"] != value.Headers["Authorization"] {
		t.Fatal("masked update changed secret")
	}
	if _, err := s.SaveMCPServer(public); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("stale secret update = %v", err)
	}
	_, err = s.CreateMCPServers([]domain.MCPServerDefinition{{ID: "cas-batch-fresh", Name: "Fresh", Transport: "http", URL: "https://example.invalid"}, {ID: saved.ID, Name: "collision", Transport: "http", URL: "https://example.invalid"}})
	if !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("import = %v", err)
	}
	if _, ok := s.MCPServer("cas-batch-fresh"); ok {
		t.Fatal("failed import committed earlier item")
	}
}

func TestCatalogSourceGenericContainerKeepsBothReviewedHalves(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wf := domain.WorkflowDefinition{ID: "cas-generic", Name: "Before", Version: 1, Revision: 1, Generic: true}
	if err := s.PutGenericTemplate(GenericTemplate{Workflow: wf, Agents: []domain.AgentDefinition{{ID: "cas-bundled", Name: "Bundled", Version: 1, Prompt: "before"}}}); err != nil {
		t.Fatal(err)
	}
	base, _ := s.GenericTemplateByWorkflow(wf.ID)
	edited := base.Agents[0]
	edited.Prompt = "reviewed agent"
	saved, err := s.UpdateGenericAgent(wf.ID, edited)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateGenericAgent(wf.ID, edited); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("stale bundled edit = %v", err)
	}
	stale := base.Workflow
	stale.Name = "reviewed workflow"
	if _, err := s.UpdateGenericWorkflow(stale); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GenericTemplateByWorkflow(wf.ID)
	if after.Workflow.Name != stale.Name || after.Agents[0].Prompt != saved.Prompt || after.Agents[0].SourceRevision != saved.SourceRevision {
		t.Fatal("workflow update erased accepted agent", after)
	}
	if _, err := s.PublishGenericWorkflow(wf.ID); err != nil {
		t.Fatal(err)
	}
	after, _ = s.GenericTemplateByWorkflow(wf.ID)
	if after.Agents[0].SourceRevision != saved.SourceRevision || after.Agents[0].Prompt != saved.Prompt {
		t.Fatal("publish erased accepted agent")
	}
}

func TestCatalogSourceCloneAncillaryCollisionRollsBackAgent(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mcp, err := s.SaveMCPServer(domain.MCPServerDefinition{ID: "cas-clone-mcp", Name: "Independent", ProjectID: "platform", Scope: "project", Transport: "http", URL: "https://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	clone := domain.AgentDefinition{ID: "cas-clone", Name: "Clone", ProjectID: "platform", MCPServers: []string{mcp.ID}}
	proposed := mcp
	proposed.Name = "must not overwrite"
	if _, err := s.CreateAgentClone(clone, []domain.MCPServerDefinition{proposed}); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("ancillary collision = %v", err)
	}
	if _, ok := s.Agent(clone.ID); ok {
		t.Fatal("ancillary collision committed agent")
	}
	retained, _ := s.MCPServer(mcp.ID)
	if retained.Name != mcp.Name || retained.SourceRevision != mcp.SourceRevision {
		t.Fatal("collision changed existing binding")
	}
	accepted, err := s.CreateAgentClone(clone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgentClone(clone, []domain.MCPServerDefinition{{ID: "cas-clone-unused", Name: "Unused", Transport: "http", URL: "https://example.invalid"}}); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("target collision = %v", err)
	}
	if _, ok := s.MCPServer("cas-clone-unused"); ok {
		t.Fatal("target collision committed ancillary row")
	}
	retainedAgent, _ := s.Agent(clone.ID)
	if retainedAgent.SourceRevision != accepted.SourceRevision {
		t.Fatal("target collision overwrote accepted agent")
	}
}
