package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

func TestDistributionHTTPGovernanceIndependentOfInspection(t *testing.T) {
	h, s, users := catalogReadFixture(t)
	owner := users["owner"]
	for _, kind := range []string{"agents", "rules", "skills", "mcp-servers"} {
		t.Run(kind, func(t *testing.T) {
			value := map[string]any{"id": "policy-" + kind, "name": "Original", "projectId": "source-a", "description": "visible inspection"}
			if kind == "mcp-servers" {
				value["scope"] = "project"
				value["transport"] = "http"
				value["url"] = "https://example.invalid"
			}
			first := sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", "/api/"+kind, value, nil), 200)
			endpoint := "/api/" + kind + "/policy-" + kind + "/distribution"
			body := map[string]any{"policy": map[string]string{"copy": "deny", "edit": "deny", "export": "deny"}, "reason": "reviewed operation restrictions"}
			headers := map[string]string{"If-Match": `"` + first["sourceRevision"].(string) + `"`}
			for _, name := range []string{"member", "viewer", "outsider"} {
				sourceJSON(t, sourceEditRequest(t, h, users[name], "PUT", endpoint, body, headers), 403)
			}
			sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", endpoint, body, nil), 428)
			decision := sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", endpoint, body, headers), 200)
			sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", endpoint, body, headers), 409)
			// Source inspection remains a read operation; policy is not source secrecy.
			read := catalogSourceResponse(t, h, users["viewer"], "GET", "/api/"+kind, nil)
			if read.Code != 200 || !strings.Contains(read.Body.String(), "visible inspection") {
				t.Fatalf("inspection blocked: %d %s", read.Code, read.Body.String())
			}
			first["sourceRevision"] = decision["sourceRevision"]
			first["name"] = "mutated"
			sourceJSON(t, sourceEditRequest(t, h, owner, "PUT", "/api/"+kind, first, map[string]string{"If-Match": `"` + decision["sourceRevision"].(string) + `"`}), 403)
			if kind == "agents" {
				sourceJSON(t, sourceEditRequest(t, h, owner, "POST", "/api/agents/policy-agents/clone", map[string]string{"id": "policy-agent-copy", "name": "Copy", "projectId": "source-a"}, nil), 403)
				if _, ok := s.Agent("policy-agent-copy"); ok {
					t.Fatal("partial copy")
				}
			}
			// Only explicit owner/admin decisions reopen source editing.
			body["policy"] = map[string]string{"copy": "allow", "edit": "allow", "export": "allow"}
			sourceJSON(t, sourceEditRequest(t, h, users["admin"], "PUT", endpoint, body, map[string]string{"If-Match": `"` + decision["sourceRevision"].(string) + `"`}), 200)
		})
	}
}
func TestDistributionHTTPRawExportAndCreateForgery(t *testing.T) {
	h, s, users := catalogReadFixture(t)
	skill, err := s.SaveSkillMetadata(domain.SkillDefinition{ID: "policy-raw", Name: "Raw", ProjectID: "source-a", Content: "source visible"})
	if err != nil {
		t.Fatal(err)
	}
	skill, err = s.SaveSkillFiles(skill.ID, []repo.SkillFileInput{{Path: "data.bin", Content: []byte("binary-visible-only-through-export")}}, skill.SourceRevision)
	if err != nil {
		t.Fatal(err)
	}
	p := domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "deny"}
	if _, err := s.SetDistribution("skill", skill.ID, skill.SourceRevision, p, "deny raw export", repo.DistributionActor{Local: true}); err != nil {
		t.Fatal(err)
	}
	raw := catalogSourceResponse(t, h, users["owner"], http.MethodGet, "/api/skills/policy-raw/files/data.bin", nil)
	if raw.Code != 403 || strings.Contains(raw.Body.String(), "binary-visible") {
		t.Fatal(raw.Code, raw.Body.String())
	}
	forged := domain.SkillDefinition{ID: "policy-forged", Name: "Forged", ProjectID: "source-a", DistributionState: domain.DistributionState{Distribution: &p}}
	sourceJSON(t, sourceEditRequest(t, h, users["owner"], "PUT", "/api/skills", forged, nil), 403)
	if _, ok := s.Skill(forged.ID); ok {
		t.Fatal("forged create became active")
	}
	// Capture a valid derivation; a fresh target body cannot wash inherited floors.
	known, _ := s.Skill(skill.ID)
	state, err := domain.CopyDistribution(known)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := s.SaveSkillMetadata(domain.SkillDefinition{ID: "policy-copy", Name: "Copy", ProjectID: "source-a", DistributionState: state})
	if err != nil {
		t.Fatal(err)
	}
	copied.DistributionState = domain.DistributionState{}
	copied.Description = "ordinary edit"
	saved, err := s.SaveSkillMetadata(copied)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DistributionState.Effective().Export != "deny" {
		t.Fatal("origins stripped")
	}
	saved.DistributionOrigins = nil
	saved.Distribution = &domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "allow"}
	if err := s.PutSkill(saved); !errors.Is(err, domain.ErrDistributionDenied) {
		t.Fatal(err)
	}
}

func TestDistributionGenericDenyRollsBackBeforeAncillaryWrites(t *testing.T) {
	h, s := newGenericTemplateServer(t)
	template, ok := s.GenericTemplateByWorkflow(repo.GenericBackendDeliveryWorkflowID)
	if !ok || len(template.Skills) == 0 {
		t.Fatal("missing template")
	}
	template.Skills[len(template.Skills)-1].Distribution = &domain.DistributionPolicy{Copy: "deny", Edit: "allow", Export: "allow"}
	if err := s.PutGenericTemplate(template); err != nil {
		t.Fatal(err)
	}
	before := len(s.Agents()) + len(s.Rules()) + len(s.Skills()) + len(s.MCPServers())
	rec := catalogSourceResponse(t, h, domain.User{}, "POST", "/api/workflows/"+repo.GenericBackendDeliveryWorkflowID+"/clone", map[string]string{"id": "distribution-denied-workflow", "name": "Denied", "projectId": "alpha"})
	if rec.Code != 403 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	after := len(s.Agents()) + len(s.Rules()) + len(s.Skills()) + len(s.MCPServers())
	if before != after {
		t.Fatalf("denied template left partial/ancillary assets: %d -> %d", before, after)
	}
	if _, ok := s.Workflow("distribution-denied-workflow"); ok {
		t.Fatal("partial workflow")
	}
}
