package teamconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"agentflow/internal/domain"
	"github.com/goccy/go-yaml"
)

type Diagnostic struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (d Diagnostic) Error() string       { return d.Path + ": " + d.Message }
func diagnostic(p, message string) error { return Diagnostic{Path: p, Message: message} }

type Preview struct {
	Format      string                    `json:"format"`
	SHA256      string                    `json:"sha256"`
	Payload     domain.TeamReleasePayload `json:"payload"`
	NotImported []string                  `json:"notImported"`
}

func ValidateSelection(s domain.TeamImportSelection) error {
	if len(s.Agents)+len(s.Rules)+len(s.Skills)+len(s.MCPServers) == 0 || len(s.Agents) > 64 || len(s.Rules) > 128 || len(s.Skills) > 128 || len(s.MCPServers) > 64 || len(s.Bindings) > 64 {
		return fmt.Errorf("an explicit bounded nonempty selection is required")
	}
	seen := map[string]bool{}
	check := func(p, prefix string) error {
		if !domain.ValidTeamImportPath(p) || !strings.HasPrefix(p, prefix+"/") || seen[p] {
			return fmt.Errorf("invalid or duplicate selected %s path", prefix)
		}
		seen[p] = true
		return nil
	}
	for _, item := range []struct {
		paths  []string
		prefix string
	}{{s.Agents, "agents"}, {s.Rules, "rules"}, {s.Skills, "skills"}} {
		for _, p := range item.paths {
			if err := check(p, item.prefix); err != nil {
				return err
			}
		}
	}
	for _, m := range s.MCPServers {
		if !domain.ValidTeamImportPath(m.Path) || !strings.HasPrefix(m.Path, "mcp/") || m.Name == "" || strings.ContainsAny(m.Name, "#/\\\x00") || seen[m.Path+"#"+m.Name] {
			return fmt.Errorf("invalid or duplicate selected MCP identity")
		}
		seen[m.Path+"#"+m.Name] = true
	}
	for p, b := range s.Bindings {
		if !contains(s.Agents, p) {
			return fmt.Errorf("binding Agent must be explicitly selected")
		}
		if len(b.Rules) > 128 || len(b.Skills) > 128 || len(b.MCPServers) > 64 {
			return fmt.Errorf("binding exceeds resource selection budget")
		}
	}
	return nil
}

func contains(values []string, v string) bool {
	for _, item := range values {
		if item == v {
			return true
		}
	}
	return false
}

func strictYAML(p string, raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return diagnostic(p, "source YAML is not UTF-8")
	}
	if err := yaml.UnmarshalWithOptions(raw, out, yaml.Strict()); err != nil {
		// Parser errors can quote secret literals; return only the source path.
		return diagnostic(p, "malformed YAML or unsupported field; use the documented basic-source schema")
	}
	return nil
}

func canonicalSelection(s domain.TeamImportSelection) domain.TeamImportSelection {
	s.Agents = append([]string{}, s.Agents...)
	sort.Strings(s.Agents)
	s.Rules = append([]string{}, s.Rules...)
	sort.Strings(s.Rules)
	s.Skills = append([]string{}, s.Skills...)
	sort.Strings(s.Skills)
	s.MCPServers = append([]domain.TeamImportMCP{}, s.MCPServers...)
	sort.Slice(s.MCPServers, func(i, j int) bool {
		return s.MCPServers[i].Path+"#"+s.MCPServers[i].Name < s.MCPServers[j].Path+"#"+s.MCPServers[j].Name
	})
	bindings := map[string]domain.TeamImportBindings{}
	for p, b := range s.Bindings {
		b.Rules = append([]string{}, b.Rules...)
		sort.Strings(b.Rules)
		b.Skills = append([]string{}, b.Skills...)
		sort.Strings(b.Skills)
		b.MCPServers = append([]string{}, b.MCPServers...)
		sort.Strings(b.MCPServers)
		bindings[p] = b
	}
	s.Bindings = bindings
	return s
}

// Parse maps only explicitly selected supported objects, preserving original
// skill bytes and file provenance. It returns no partially executable payload.
func Parse(projectID string, repository domain.Repository, commit, root string, selection domain.TeamImportSelection, files map[string]File, refs ...string) (Preview, error) {
	var out Preview
	if err := ValidateSelection(selection); err != nil {
		return out, err
	}
	selection = canonicalSelection(selection)
	config, ok := files["teamai.yaml"]
	if !ok {
		return out, diagnostic("teamai.yaml", "required source config is missing")
	}
	var team map[string]any
	if err := strictYAML("teamai.yaml", config.Content, &team); err != nil {
		return out, err
	}
	for _, key := range []string{"team", "repo"} {
		if v, ok := team[key].(string); !ok || strings.TrimSpace(v) == "" {
			return out, diagnostic("teamai.yaml", "team and repo must be nonempty strings")
		}
	}
	if mode, ok := team["mode"]; ok && mode != "self" {
		return out, diagnostic("teamai.yaml", "unsupported source mode")
	}
	if root == ".teamai" && team["mode"] != "self" {
		return out, diagnostic("teamai.yaml", "self root requires mode: self")
	}
	if len(refs) > 1 || len(refs) == 1 && refs[0] != "" && !domain.ValidTeamImportRef(refs[0]) {
		return out, fmt.Errorf("one valid registered heads ref required")
	}
	manifest := &domain.TeamGitImport{Format: domain.TeamGitImportFormat, RepositoryID: repository.ID, Commit: commit, Root: root, Selection: selection}
	for _, file := range files {
		manifest.Files = append(manifest.Files, file.TeamImportFile)
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	if len(refs) == 1 {
		manifest.Ref = refs[0]
	}
	payload := domain.TeamReleasePayload{Agents: []domain.AgentDefinition{}, Assets: domain.AgentAssetBundle{Rules: []domain.RuleDefinition{}, Skills: []domain.SkillDefinition{}, MCPServers: []domain.MCPServerDefinition{}}, Source: domain.IntelligenceSource{Repository: repository.CloneURL, Commit: commit, Path: root}, GitImport: manifest}
	ids := map[string]string{}
	for _, p := range selection.Rules {
		file, ok := files[p]
		if !ok || path.Ext(p) != ".md" || !utf8.Valid(file.Content) {
			return out, diagnostic(p, "selected plain Markdown rule is missing or invalid")
		}
		if bytes.HasPrefix(bytes.TrimPrefix(file.Content, []byte{0xef, 0xbb, 0xbf}), []byte("---")) {
			return out, diagnostic(p, "rule frontmatter/scoped rules require a separate compatibility mapping")
		}
		id := strings.TrimSuffix(strings.TrimPrefix(p, "rules/"), ".md")
		ids[p] = id
		payload.Assets.Rules = append(payload.Assets.Rules, domain.RuleDefinition{ID: id, Name: id, ProjectID: projectID, Content: string(file.Content)})
	}
	for _, p := range selection.Skills {
		skill, binary, err := parseSkill(projectID, p, files)
		if err != nil {
			return out, err
		}
		ids[p] = skill.ID
		payload.Assets.Skills = append(payload.Assets.Skills, skill)
		if len(binary) > 0 {
			if payload.Assets.SkillFiles == nil {
				payload.Assets.SkillFiles = map[string]map[string][]byte{}
			}
			payload.Assets.SkillFiles[skill.ID] = binary
		}
	}
	for _, selected := range selection.MCPServers {
		file, ok := files[selected.Path]
		if !ok || path.Base(selected.Path) != "mcp.yaml" {
			return out, diagnostic(selected.Path, "selected MCP list is missing; MCP folders are unsupported")
		}
		var list struct {
			Servers []struct {
				Name        string   `yaml:"name"`
				Description string   `yaml:"description"`
				Transport   string   `yaml:"transport"`
				Command     string   `yaml:"command"`
				Args        []string `yaml:"args"`
				URL         string   `yaml:"url"`
				Timeout     int      `yaml:"timeout"`
			} `yaml:"servers"`
		}
		if err := strictYAML(selected.Path, file.Content, &list); err != nil {
			return out, err
		}
		found := false
		names := map[string]bool{}
		for _, m := range list.Servers {
			if m.Name == "" || names[m.Name] {
				return out, diagnostic(selected.Path, "missing or duplicate MCP name")
			}
			names[m.Name] = true
			if m.Name != selected.Name {
				continue
			}
			if m.Transport != "stdio" && m.Transport != "http" || m.Timeout < 0 || m.Timeout > 600000 || m.Timeout%1000 != 0 {
				return out, diagnostic(selected.Path, "unsupported MCP transport or nonintegral/out-of-range timeout")
			}
			if strings.Contains(m.URL+m.Command+strings.Join(m.Args, ""), "${") {
				return out, diagnostic(selected.Path, "MCP variable/credential binding is unsupported")
			}
			if m.Transport == "stdio" && (m.Command == "" || m.URL != "") || m.Transport == "http" && (m.URL == "" || m.Command != "" || len(m.Args) > 0) {
				return out, diagnostic(selected.Path, "MCP fields do not match transport")
			}
			ids[selected.Path+"#"+m.Name] = m.Name
			found = true
			payload.Assets.MCPServers = append(payload.Assets.MCPServers, domain.MCPServerDefinition{ID: m.Name, Name: m.Name, Description: m.Description, ProjectID: projectID, Scope: "project", Transport: m.Transport, Command: m.Command, Args: m.Args, URL: m.URL, TimeoutSec: m.Timeout / 1000})
		}
		if !found {
			return out, diagnostic(selected.Path, "selected MCP server does not exist")
		}
	}
	for _, p := range selection.Agents {
		file, ok := files[p]
		if !ok || path.Ext(p) != ".yaml" {
			return out, diagnostic(p, "selected canonical Agent YAML is missing")
		}
		var a struct {
			Name         string `yaml:"name"`
			Description  string `yaml:"description"`
			Instructions string `yaml:"instructions"`
		}
		if err := strictYAML(p, file.Content, &a); err != nil {
			return out, err
		}
		if a.Name != strings.TrimSuffix(path.Base(p), ".yaml") || strings.TrimSpace(a.Description) == "" || strings.TrimSpace(a.Instructions) == "" {
			return out, diagnostic(p, "Agent requires filename-matching name, description and instructions")
		}
		agent := domain.AgentDefinition{ID: a.Name, Name: a.Name, Description: a.Description, Prompt: a.Instructions, ProjectID: projectID, Rules: []string{}, Skills: []string{}, MCPServers: []string{}}
		binding := selection.Bindings[p]
		for _, pair := range []struct {
			refs     []string
			selected []string
			target   *[]string
		}{{binding.Rules, selection.Rules, &agent.Rules}, {binding.Skills, selection.Skills, &agent.Skills}, {binding.MCPServers, nil, &agent.MCPServers}} {
			seen := map[string]bool{}
			for _, ref := range pair.refs {
				id, ok := ids[ref]
				if !ok || seen[ref] || pair.selected != nil && !contains(pair.selected, ref) || pair.selected == nil && !strings.Contains(ref, "#") {
					return out, diagnostic(p, "binding refers to an unselected, duplicate or wrong-kind resource")
				}
				seen[ref] = true
				*pair.target = append(*pair.target, id)
			}
		}
		payload.Agents = append(payload.Agents, agent)
	}
	if err := domain.ValidateTeamReleasePayload(payload, projectID); err != nil {
		return out, diagnostic("selection", err.Error())
	}
	digest, err := domain.IntelligenceDigest(payload)
	if err != nil {
		return out, diagnostic("selection", err.Error())
	}
	out = Preview{Format: domain.TeamGitImportFormat, SHA256: digest, Payload: payload, NotImported: []string{"Unselected source resources, role/member distribution, tool delivery settings, environment, hooks, models, workflows and automatic updates are not imported or executed."}}
	return out, nil
}

func parseSkill(projectID, p string, files map[string]File) (domain.SkillDefinition, map[string][]byte, error) {
	flat := strings.HasSuffix(p, ".md")
	entry := p + "/SKILL.md"
	name := path.Base(p)
	if flat {
		entry = p
		name = strings.TrimSuffix(name, ".md")
	}
	skill := domain.SkillDefinition{ID: name, Name: name, ProjectID: projectID, Files: map[string]string{}}
	file, ok := files[entry]
	if !ok || !utf8.Valid(file.Content) {
		return skill, nil, diagnostic(p, "skill requires a UTF-8 SKILL.md or selected flat skill file")
	}
	parts := bytes.SplitN(file.Content, []byte("\n"), 2)
	if len(parts) != 2 || strings.TrimSpace(string(parts[0])) != "---" {
		return skill, nil, diagnostic(entry, "skill requires name/description frontmatter")
	}
	end := -1
	offset := 0
	for _, line := range bytes.Split(parts[1], []byte("\n")) {
		if bytes.Equal(bytes.TrimSuffix(line, []byte("\r")), []byte("---")) {
			end = offset
			break
		}
		offset += len(line) + 1
	}
	if end < 0 {
		return skill, nil, diagnostic(entry, "skill frontmatter is not terminated")
	}
	var header struct {
		Name          string         `yaml:"name"`
		Description   string         `yaml:"description"`
		License       string         `yaml:"license"`
		Compatibility string         `yaml:"compatibility"`
		Metadata      map[string]any `yaml:"metadata"`
	}
	if err := strictYAML(entry, parts[1][:end], &header); err != nil {
		return skill, nil, err
	}
	if header.Name != name || strings.TrimSpace(header.Description) == "" {
		return skill, nil, diagnostic(entry, "skill requires matching name and nonempty description")
	}
	skill.Description = header.Description
	binary := map[string][]byte{}
	paths := []string{}
	for f := range files {
		if f == entry || !flat && strings.HasPrefix(f, p+"/") {
			paths = append(paths, f)
		}
	}
	sort.Strings(paths)
	for _, f := range paths {
		rel := strings.TrimPrefix(f, p+"/")
		if flat {
			rel = "SKILL.md"
		}
		v := files[f]
		mode := uint32(0o644)
		if v.Mode == "100755" {
			mode = 0o755
		}
		if utf8.Valid(v.Content) && mode == 0o644 {
			skill.Files[rel] = string(v.Content)
		} else {
			digest := sha256.Sum256(v.Content)
			skill.FileRefs = append(skill.FileRefs, domain.SkillFileRef{Path: rel, Size: int64(len(v.Content)), SHA256: hex.EncodeToString(digest[:]), Mode: mode})
			binary[rel] = append([]byte{}, v.Content...)
		}
	}
	return skill, binary, nil
}
