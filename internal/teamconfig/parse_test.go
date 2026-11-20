package teamconfig

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func importFixture() (domain.Repository, domain.TeamImportSelection, map[string]File) {
	repository := domain.Repository{ID: "config", ProjectID: "project", CloneURL: "https://example.test/config.git"}
	selection := domain.TeamImportSelection{
		Agents: []string{"agents/reviewer.yaml"}, Rules: []string{"rules/style.md"}, Skills: []string{"skills/check"},
		MCPServers: []domain.TeamImportMCP{{Path: "mcp/mcp.yaml", Name: "format"}},
		Bindings:   map[string]domain.TeamImportBindings{"agents/reviewer.yaml": {Rules: []string{"rules/style.md"}, Skills: []string{"skills/check"}, MCPServers: []string{"mcp/mcp.yaml#format"}}},
	}
	files := map[string]File{}
	for p, content := range map[string]string{
		"teamai.yaml":                      "team: Example\nrepo: https://example.test/config.git\n",
		"agents/reviewer.yaml":             "name: reviewer\ndescription: Review changes\ninstructions: Check the reviewed diff.\n",
		"rules/style.md":                   "# Style\nKeep changes simple.\n",
		"skills/check/SKILL.md":            "---\nname: check\ndescription: Check files\nlicense: MIT\n---\nRead scripts/check.sh, then decide.\n",
		"skills/check/scripts/check.sh":    "#!/bin/sh\necho never-run-by-import\n",
		"skills/check/references/guide.md": "Exact nested guide.\n",
		"skills/check/assets/blob.bin":     "\x00\xff\x01",
		"mcp/mcp.yaml":                     "servers:\n  - name: format\n    description: Format text\n    transport: stdio\n    command: fixture-not-executed\n    args: [--stdio]\n    timeout: 30000\n",
		"LICENSE":                          "MIT fixture notice\n",
	} {
		mode := "100644"
		if strings.HasSuffix(p, ".sh") {
			mode = "100755"
		}
		files[p] = importFile(p, mode, []byte(content))
	}
	return repository, selection, files
}

func importFile(p, mode string, raw []byte) File {
	sha := sha256.Sum256(raw)
	blob := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(raw))), raw...))
	return File{TeamImportFile: domain.TeamImportFile{Path: p, Mode: mode, BlobSHA: hex.EncodeToString(blob[:]), SHA256: hex.EncodeToString(sha[:]), Size: int64(len(raw))}, Content: raw}
}

func TestImportParsesSelectedResourcesAndPreservesPackageBytes(t *testing.T) {
	r, s, files := importFixture()
	preview, err := Parse("project", r, strings.Repeat("a", 40), ".", s, files)
	if err != nil {
		t.Fatal(err)
	}
	p := preview.Payload
	if len(p.Agents) != 1 || len(p.Assets.Rules) != 1 || len(p.Assets.Skills) != 1 || len(p.Assets.MCPServers) != 1 || len(p.GitImport.Files) != len(files) {
		t.Fatal("selected resources or provenance omitted")
	}
	a := p.Agents[0]
	if a.Prompt != "Check the reviewed diff." || strings.Join(a.Rules, ",") != "style" || strings.Join(a.Skills, ",") != "check" || strings.Join(a.MCPServers, ",") != "format" || a.ProjectID != "project" || a.Shared || a.SourceRevision != "" {
		t.Fatal("Agent mapping, explicit bindings or scope changed")
	}
	skill := p.Assets.Skills[0]
	if skill.Files["SKILL.md"] != string(files["skills/check/SKILL.md"].Content) || skill.Files["references/guide.md"] != "Exact nested guide.\n" || len(skill.FileRefs) != 2 || len(p.Assets.SkillFiles["check"]) != 2 {
		t.Fatal("skill source/resources were flattened or lost")
	}
	for _, ref := range skill.FileRefs {
		source := files["skills/check/"+ref.Path]
		if !bytes.Equal(p.Assets.SkillFiles["check"][ref.Path], source.Content) || ref.SHA256 != source.SHA256 || ref.Path == "scripts/check.sh" && ref.Mode != 0o755 {
			t.Fatal("skill binary/executable bytes or mode changed")
		}
	}
	if p.Assets.MCPServers[0].TimeoutSec != 30 || preview.Format != domain.TeamGitImportFormat || len(preview.NotImported) == 0 || !p.Agents[0].UpdatedAt.IsZero() {
		t.Fatal("timeout units, compatibility notice or deterministic timestamps changed")
	}
	again, err := Parse("project", r, strings.Repeat("a", 40), ".", s, files)
	if err != nil || preview.SHA256 != again.SHA256 {
		t.Fatal("identical exact source does not produce a stable digest")
	}
	// Binding inference from roles/filenames is deliberately absent.
	s.Bindings = nil
	without, err := Parse("project", r, strings.Repeat("a", 40), ".", s, files)
	if err != nil || len(without.Payload.Agents[0].Skills) != 0 || without.SHA256 == preview.SHA256 {
		t.Fatal("unapproved implicit binding or digest collision")
	}
}

func TestImportRejectsUnsupportedOrIncompleteSourceWithoutLeakingYAML(t *testing.T) {
	for _, tc := range []struct{ name, p, raw string }{
		{"agent-tool-policy", "agents/reviewer.yaml", "name: reviewer\ndescription: Review\ninstructions: Review.\ntools: [private-secret-marker]\n"},
		{"malformed-secret", "agents/reviewer.yaml", "name: [private-secret-marker\n"},
		{"agent-name", "agents/reviewer.yaml", "name: different\ndescription: Review\ninstructions: Review.\n"},
		{"rule-scope", "rules/style.md", "---\npaths: [src/*]\n---\nScoped rule."},
		{"skill-policy", "skills/check/SKILL.md", "---\nname: check\ndescription: Check\nallowed-tools: private-secret-marker\n---\nBody."},
		{"skill-closing-delimiter", "skills/check/SKILL.md", "---\nname: check\ndescription: Check\n---invalid\nBody."},
		{"mcp-env", "mcp/mcp.yaml", "servers:\n- name: format\n  transport: stdio\n  command: fixture\n  env:\n    TOKEN: private-secret-marker\n"},
		{"mcp-fractional-timeout", "mcp/mcp.yaml", "servers:\n- name: format\n  transport: stdio\n  command: fixture\n  timeout: 1500\n"},
		{"mcp-sse", "mcp/mcp.yaml", "servers:\n- name: format\n  transport: sse\n  url: https://example.test/events\n"},
		{"mcp-variable", "mcp/mcp.yaml", "servers:\n- name: format\n  transport: stdio\n  command: fixture\n  args: ['${TOKEN}']\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, files := importFixture()
			files[tc.p] = importFile(tc.p, "100644", []byte(tc.raw))
			preview, err := Parse("project", r, strings.Repeat("a", 40), ".", s, files)
			if err == nil || strings.Contains(err.Error(), "private-secret-marker") || !strings.Contains(err.Error(), tc.p) || preview.SHA256 != "" {
				t.Fatalf("unsupported source yielded partial payload or unsafe diagnostic: %v", err)
			}
		})
	}
	for _, missing := range []string{"teamai.yaml", "rules/style.md", "skills/check/SKILL.md", "mcp/mcp.yaml", "agents/reviewer.yaml"} {
		r, s, files := importFixture()
		delete(files, missing)
		if _, err := Parse("project", r, strings.Repeat("a", 40), ".", s, files); err == nil {
			t.Fatalf("missing selected source accepted: %s", missing)
		}
	}
}

func TestImportSelectionAndFlatSkill(t *testing.T) {
	r, s, files := importFixture()
	s.Skills = []string{"skills/flat.md"}
	s.Bindings = nil
	files["skills/flat.md"] = importFile("skills/flat.md", "100644", []byte("---\r\nname: flat\r\ndescription: Flat package\r\n---\r\nBody.\r\n"))
	preview, err := Parse("project", r, strings.Repeat("a", 40), ".", s, files)
	if err != nil || len(preview.Payload.Assets.Skills) != 1 || preview.Payload.Assets.Skills[0].Files["SKILL.md"] != string(files["skills/flat.md"].Content) {
		t.Fatalf("flat skill CRLF bytes not preserved: %v", err)
	}
	for _, mutate := range []func(*domain.TeamImportSelection){
		func(s *domain.TeamImportSelection) { s.Rules = append(s.Rules, s.Rules[0]) },
		func(s *domain.TeamImportSelection) { s.Skills = []string{"../skills/check"} },
		func(s *domain.TeamImportSelection) {
			s.Bindings = map[string]domain.TeamImportBindings{"agents/other.yaml": {}}
		},
		func(s *domain.TeamImportSelection) {
			s.Bindings = map[string]domain.TeamImportBindings{"agents/reviewer.yaml": {Skills: make([]string, 129)}}
		},
	} {
		_, selection, _ := importFixture()
		mutate(&selection)
		if err := ValidateSelection(selection); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	_, wrongBinding, original := importFixture()
	wrongBinding.Bindings["agents/reviewer.yaml"] = domain.TeamImportBindings{Skills: []string{"rules/style.md"}}
	if _, err := Parse("project", r, strings.Repeat("a", 40), ".", wrongBinding, original); err == nil {
		t.Fatal("wrong-kind binding accepted")
	}
}
