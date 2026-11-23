package repo

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

var ErrSourcePrecondition = errors.New("source editing requires the observed source revision")
var ErrSourceConflict = errors.New("source changed; refresh and compare before submitting again")
var ErrSkillSourceReadOnly = errors.New("market skill files are read-only; use the reviewed market update flow")

func firstSourceRevision(values []string) string {
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

func (r *CatalogRepo) CloneAgentForEvaluation(sourceID, targetID, projectID, expected string) (domain.AgentDefinition, error) {
	var saved domain.AgentDefinition
	err := r.editSource("agents", sourceID, expected, func(catalog *CatalogRepo, raw []byte, _ string, exists bool) error {
		if !exists {
			return ErrSourceConflict
		}
		var agent domain.AgentDefinition
		if err := json.Unmarshal(raw, &agent); err != nil {
			return err
		}
		state, err := domain.CopyDistribution(agent)
		if err != nil {
			return err
		}
		agent.DistributionState = state
		agent.ID = targetID
		agent.Name += "（评测副本）"
		agent.ProjectID = projectID
		agent.Shared = false
		agent.Version = 1
		agent.SourceRevision = ""
		agent.UpdatedAt = time.Time{}
		saved, err = catalog.SaveAgent(agent)
		if errors.Is(err, ErrSourcePrecondition) {
			return ErrSourceConflict
		}
		return err
	})
	return saved, err
}

// Legacy revisions are derived on read, never backfilled. MCP callers pass
// the public masked representation so no credential-derived digest is exposed.
func sourceRevision(table string, value any) string {
	raw, _ := json.Marshal(value)
	var revision struct {
		Revision string `json:"sourceRevision"`
	}
	_ = json.Unmarshal(raw, &revision)
	if revision.Revision != "" {
		return revision.Revision
	}
	sum := sha256.Sum256(append([]byte(table+":"), raw...))
	return "legacy-" + hex.EncodeToString(sum[:])
}

func storedSourceRevision(table string, raw []byte) (string, error) {
	switch table {
	case "agents":
		var value domain.AgentDefinition
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
		return sourceRevision(table, value), nil
	case "rules":
		var value domain.RuleDefinition
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
		return sourceRevision(table, value), nil
	case "skills":
		var value domain.SkillDefinition
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
		return sourceRevision(table, value), nil
	case "mcp_servers":
		var value domain.MCPServerDefinition
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
		return sourceRevision(table, domain.PublicMCPServer(value)), nil
	}
	return "", fmt.Errorf("unsupported catalog source")
}

// editSource serializes every parent and file edit on the existing source row.
// Empty expected revision is create-only, including internal callers.
func (r *CatalogRepo) editSource(table, id, expected string, edit func(*CatalogRepo, []byte, string, bool) error) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var row payloadRow
		query := tx.Table(table)
		// Missing-row gap locks can deadlock concurrent create-only inserts.
		// Uniqueness is enforced by INSERT; existing conditional edits lock rows.
		if expected != "" {
			query = r.locking(query)
		}
		err := query.Where("id = ?", id).Take(&row).Error
		exists := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if exists {
			if expected == "" {
				return ErrSourcePrecondition
			}
			current, err := storedSourceRevision(table, row.Payload)
			if err != nil {
				return err
			}
			if expected != current {
				return ErrSourceConflict
			}
		} else if expected != "" {
			return ErrSourceConflict
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		catalog := *r
		catalog.deps = &deps{DB: tx}
		return edit(&catalog, row.Payload, hex.EncodeToString(nonce[:]), exists)
	})
}

func writeSource(db *gorm.DB, table string, values map[string]any, exists bool) error {
	if err := guardDistributionWrite(db, table, values, exists); err != nil {
		return err
	}
	if exists {
		return db.Table(table).Where("id = ?", values["id"]).Updates(values).Error
	}
	err := db.Table(table).Create(values).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrSourceConflict
	}
	return err
}

func (r *CatalogRepo) SaveAgent(v domain.AgentDefinition) (domain.AgentDefinition, error) {
	var saved domain.AgentDefinition
	err := r.saveAgent(v, &saved)
	return saved, err
}

// Dependency sharing is an explicit human HTTP operation. Ordinary internal
// Agent writes never grant source authority as an incidental storage effect.
func (r *CatalogRepo) SaveAgentWithSharing(v domain.AgentDefinition) (domain.AgentDefinition, error) {
	var saved domain.AgentDefinition
	err := r.saveAgent(v, &saved, true)
	return saved, err
}
func (r *CatalogRepo) PutAgent(v domain.AgentDefinition) error { return r.saveAgent(v, nil) }
func (r *CatalogRepo) SaveRule(v domain.RuleDefinition) (domain.RuleDefinition, error) {
	var saved domain.RuleDefinition
	err := r.saveRule(v, &saved)
	return saved, err
}
func (r *CatalogRepo) PutRule(v domain.RuleDefinition) error { return r.saveRule(v, nil) }
func (r *CatalogRepo) SaveSkillMetadata(v domain.SkillDefinition) (domain.SkillDefinition, error) {
	var saved domain.SkillDefinition
	err := r.saveSkill(v, &saved, true)
	return saved, err
}
func (r *CatalogRepo) PutSkill(v domain.SkillDefinition) error { return r.saveSkill(v, nil, false) }
func (r *CatalogRepo) SaveMCPServer(v domain.MCPServerDefinition) (domain.MCPServerDefinition, error) {
	var saved domain.MCPServerDefinition
	err := r.saveMCPServer(v, &saved)
	return saved, err
}
func (r *CatalogRepo) PutMCPServer(v domain.MCPServerDefinition) error {
	return r.saveMCPServer(v, nil)
}

func (r *CatalogRepo) FillAgentMCPBindings(id, projectID string, bindings []string) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var row agentRow
		if err := r.locking(tx).Where("id = ?", id).Take(&row).Error; err != nil {
			return err
		}
		catalog := *r
		catalog.deps = &deps{DB: tx}
		agent, ok := catalog.Agent(id)
		if !ok {
			return ErrSourceConflict
		}
		if agent.ProjectID != projectID || len(agent.MCPServers) > 0 {
			return nil
		}
		agent.MCPServers = append([]string(nil), bindings...)
		return catalog.PutAgent(agent)
	})
}

func (r *CatalogRepo) CreateAgentClone(agent domain.AgentDefinition, mcps []domain.MCPServerDefinition, sources ...domain.AgentDefinition) (domain.AgentDefinition, error) {
	var saved domain.AgentDefinition
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		catalog := *r
		catalog.deps = &deps{DB: tx}
		if len(sources) > 0 {
			source := sources[0]
			locked, err := catalog.lockDistributionSource("agent", source.ID)
			if err != nil {
				return err
			}
			actual := locked.(domain.AgentDefinition)
			if source.SourceRevision != actual.SourceRevision {
				return ErrSourceConflict
			}
			state, err := domain.CopyDistribution(actual)
			if err != nil {
				return err
			}
			agent.DistributionState = state
		}
		// Every MCP that is actually duplicated carries its precise immediate
		// source. Independent target bindings are references and are not copied.
		for i := range mcps {
			if len(mcps[i].DistributionOrigins) == 0 {
				continue
			}
			origin := mcps[i].DistributionOrigins[len(mcps[i].DistributionOrigins)-1]
			if origin.Kind != "mcp" {
				return domain.ErrDistributionInvalid
			}
			locked, err := catalog.lockDistributionSource("mcp", origin.ID)
			if err != nil {
				return err
			}
			actual := locked.(domain.MCPServerDefinition)
			if actual.SourceRevision != origin.SourceRevision {
				return ErrSourceConflict
			}
			state, err := domain.CopyDistribution(actual)
			if err != nil {
				return err
			}
			mcps[i].DistributionState = state
		}
		agent.SourceRevision = ""
		var err error
		saved, err = catalog.SaveAgent(agent)
		if errors.Is(err, ErrSourcePrecondition) {
			return ErrSourceConflict
		}
		if err != nil {
			return err
		}
		for _, server := range mcps {
			server.SourceRevision = ""
			if err := catalog.PutMCPServer(server); err != nil {
				if errors.Is(err, ErrSourcePrecondition) {
					return ErrSourceConflict
				}
				return err
			}
		}
		return nil
	})
	return saved, err
}

// Import is create-only and atomic across the entire batch. Existing catalog
// entries must be reviewed through their conditional editor, never overwritten.
func (r *CatalogRepo) CreateMCPServers(values []domain.MCPServerDefinition) ([]domain.MCPServerDefinition, error) {
	var saved []domain.MCPServerDefinition
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		catalog := *r
		catalog.deps = &deps{DB: tx}
		for _, value := range values {
			value.SourceRevision = ""
			value.FileRefs = nil
			item, err := catalog.SaveMCPServer(value)
			if err != nil {
				if errors.Is(err, ErrSourcePrecondition) {
					return ErrSourceConflict
				}
				return err
			}
			saved = append(saved, item)
		}
		return nil
	})
	return saved, err
}

func (r *CatalogRepo) editAgentSource(v domain.AgentDefinition, saved *domain.AgentDefinition, shareOwned bool) error {
	return r.editSource("agents", v.ID, v.SourceRevision, func(catalog *CatalogRepo, raw []byte, token string, exists bool) error {
		if exists {
			var old domain.AgentDefinition
			if err := json.Unmarshal(raw, &old); err != nil {
				return err
			}
			v.ProjectID = domain.KeepCatalogProjectID(v.ProjectID, old.ProjectID)
			if old.ProjectID != "" && old.ProjectID != v.ProjectID {
				return ErrSourceConflict
			}
			if err := catalog.snapshotRevision("agent", old.ID, old.Name, old.Version, 0, old); err != nil {
				return err
			}
		}
		v.SourceRevision = token
		v.UpdatedAt = time.Now().UTC()
		v.Rules = normalizeRefs(v.Rules, "rules/")
		v.Skills = normalizeRefs(v.Skills, "skills/")
		if v.Shared && shareOwned {
			// Serialize dependency flags with their actual latest content. The
			// Agent and all owned dependency changes commit or roll back together.
			rules := append([]string(nil), v.Rules...)
			skills := append([]string(nil), v.Skills...)
			sort.Strings(rules)
			sort.Strings(skills)
			for _, id := range rules {
				item, ok := catalog.Rule(id)
				if !ok || item.Shared || (item.ProjectID != "" && item.ProjectID != v.ProjectID) {
					continue
				}
				item.Shared = true
				if err := catalog.PutRule(item); err != nil {
					return err
				}
			}
			for _, id := range skills {
				item, ok := catalog.Skill(id)
				if !ok || item.Shared || (item.ProjectID != "" && item.ProjectID != v.ProjectID) {
					continue
				}
				item.Shared = true
				if err := catalog.PutSkill(item); err != nil {
					return err
				}
			}
		}
		if err := writeSource(catalog.DB, "agents", map[string]any{"id": v.ID, "name": v.Name, "payload": mustJSON(v), "updated_at": timePtr(v.UpdatedAt)}, exists); err != nil {
			return err
		}
		if saved != nil {
			*saved = v
			var row payloadRow
			if err := catalog.DB.Table("agents").Where("id = ?", v.ID).Take(&row).Error; err != nil {
				return err
			}
			if err := json.Unmarshal(row.Payload, saved); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *CatalogRepo) saveRule(v domain.RuleDefinition, saved *domain.RuleDefinition) error {
	return r.editSource("rules", v.ID, v.SourceRevision, func(catalog *CatalogRepo, raw []byte, token string, exists bool) error {
		if exists {
			var old domain.RuleDefinition
			if err := json.Unmarshal(raw, &old); err != nil {
				return err
			}
			if old.ProjectID != "" && v.ProjectID != old.ProjectID {
				return ErrSourceConflict
			}
		}
		v.SourceRevision = token
		v.UpdatedAt = time.Now().UTC()
		if err := writeSource(catalog.DB, "rules", map[string]any{"id": v.ID, "name": v.Name, "payload": mustJSON(v), "updated_at": timePtr(v.UpdatedAt)}, exists); err != nil {
			return err
		}
		if saved != nil {
			*saved = v
			var row payloadRow
			if err := catalog.DB.Table("rules").Where("id = ?", v.ID).Take(&row).Error; err != nil {
				return err
			}
			if err := json.Unmarshal(row.Payload, saved); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *CatalogRepo) saveSkill(v domain.SkillDefinition, saved *domain.SkillDefinition, metadata bool) error {
	return r.editSource("skills", v.ID, v.SourceRevision, func(catalog *CatalogRepo, raw []byte, token string, exists bool) error {
		if exists {
			var old domain.SkillDefinition
			if err := json.Unmarshal(raw, &old); err != nil {
				return err
			}
			if old.ProjectID != "" && v.ProjectID != old.ProjectID {
				return ErrSourceConflict
			}
			v.FileRefs = old.FileRefs
			if metadata || old.Source != nil {
				v.Content = old.Content
				v.Files = old.Files
				v.Source = old.Source
			}
		} else if metadata {
			v.FileRefs = nil
			v.Files = nil
			v.Source = nil
		}
		v.SourceRevision = token
		v.UpdatedAt = time.Now().UTC()
		if err := writeSource(catalog.DB, "skills", map[string]any{"id": v.ID, "name": v.Name, "payload": mustJSON(v), "updated_at": timePtr(v.UpdatedAt)}, exists); err != nil {
			return err
		}
		if saved != nil {
			*saved = v
			var row payloadRow
			if err := catalog.DB.Table("skills").Where("id = ?", v.ID).Take(&row).Error; err != nil {
				return err
			}
			if err := json.Unmarshal(row.Payload, saved); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *CatalogRepo) saveMCPServer(v domain.MCPServerDefinition, saved *domain.MCPServerDefinition) error {
	return r.editSource("mcp_servers", v.ID, v.SourceRevision, func(catalog *CatalogRepo, raw []byte, token string, exists bool) error {
		if exists {
			var old domain.MCPServerDefinition
			if err := json.Unmarshal(raw, &old); err != nil {
				return err
			}
			domain.RestoreMaskedMCPSecrets(&v, old)
			v.FileRefs = old.FileRefs
			if old.ProjectID != "" && v.ProjectID != old.ProjectID {
				return ErrSourceConflict
			}
			v.Scope, v.ProjectID, v.OwnerUserID = old.Scope, old.ProjectID, old.OwnerUserID
		}
		if err := domain.ValidateMCPServer(v); err != nil {
			return err
		}
		v.SourceRevision = token
		v.UpdatedAt = time.Now().UTC()
		if err := writeSource(catalog.DB, "mcp_servers", map[string]any{"id": v.ID, "name": v.Name, "payload": mustJSON(v), "updated_at": timePtr(v.UpdatedAt)}, exists); err != nil {
			return err
		}
		if saved != nil {
			*saved = v
			var row payloadRow
			if err := catalog.DB.Table("mcp_servers").Where("id = ?", v.ID).Take(&row).Error; err != nil {
				return err
			}
			if err := json.Unmarshal(row.Payload, saved); err != nil {
				return err
			}
		}
		return nil
	})
}
