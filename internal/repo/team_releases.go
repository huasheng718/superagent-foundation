package repo

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

type teamReleaseRow struct {
	ID        string    `gorm:"column:id;primaryKey;size:64"`
	ProjectID string    `gorm:"column:project_id;uniqueIndex:uq_team_release_version,priority:1;index:idx_team_release_project;size:64;not null"`
	Version   int       `gorm:"column:version;uniqueIndex:uq_team_release_version,priority:2;not null"`
	Revision  int       `gorm:"column:revision;not null"`
	State     string    `gorm:"column:state;size:32;not null"`
	Payload   []byte    `gorm:"column:payload;type:json;not null"`
	Created   time.Time `gorm:"column:created_at;not null"`
}

func (teamReleaseRow) TableName() string { return "team_releases" }

var ErrIntelligenceConflict = errors.New("intelligence revision or lifecycle conflict")
var ErrIntelligenceUnavailable = errors.New("intelligence resource missing, revoked or outside project")

func intelligenceID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

func (r *CatalogRepo) CreateTeamRelease(projectID string, input domain.TeamReleaseRequest, actor string) (domain.TeamRelease, error) {
	var result domain.TeamRelease
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(input.Name) == "" || len(input.Name) > 200 || strings.TrimSpace(actor) == "" || input.ExpectedBaseVersion < 0 {
		return result, fmt.Errorf("project, bounded name and authenticated actor are required")
	}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var project projectRow
		if err := r.locking(tx).Where("id = ?", projectID).Take(&project).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		scoped := &CatalogRepo{deps: &deps{DB: tx}}
		payload, err := scoped.captureTeamRelease(projectID, input)
		if err != nil {
			return err
		}
		digest, err := domain.IntelligenceDigest(payload)
		if err != nil {
			return err
		}
		var version int
		if err := tx.Model(&teamReleaseRow{}).Select("COALESCE(MAX(version),0)").Where("project_id = ?", projectID).Scan(&version).Error; err != nil {
			return err
		}
		if version != input.ExpectedBaseVersion {
			return ErrIntelligenceConflict
		}
		id, err := intelligenceID("team-")
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		result = domain.TeamRelease{ID: id, ProjectID: projectID, Name: strings.TrimSpace(input.Name), Version: version + 1, Revision: 1, State: domain.TeamReleasePublished, SHA256: digest, Payload: payload, CreatedBy: actor, CreatedAt: now, Decisions: []domain.IntelligenceDecision{{Decision: domain.TeamReleasePublished, Actor: actor, At: now}}}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return tx.Create(&teamReleaseRow{ID: id, ProjectID: projectID, Version: result.Version, Revision: 1, State: result.State, Payload: raw, Created: now}).Error
	})
	return result, err
}

func (r *CatalogRepo) captureTeamRelease(projectID string, in domain.TeamReleaseRequest) (domain.TeamReleasePayload, error) {
	out := domain.TeamReleasePayload{Source: in.Source, Agents: []domain.AgentDefinition{}, Assets: domain.AgentAssetBundle{Rules: []domain.RuleDefinition{}, Skills: []domain.SkillDefinition{}, MCPServers: []domain.MCPServerDefinition{}}}
	agentIDs, err := domain.CanonicalIntelligenceIDs(in.AgentIDs, 64)
	if err != nil {
		return out, err
	}
	ruleSet := map[string]bool{}
	skillSet := map[string]bool{}
	mcpSet := map[string]bool{}
	for _, id := range in.RuleIDs {
		ruleSet[id] = true
	}
	for _, id := range in.SkillIDs {
		skillSet[id] = true
	}
	for _, id := range in.MCPServerIDs {
		mcpSet[id] = true
	}
	for _, id := range agentIDs {
		var a domain.AgentDefinition
		if err := r.lockTeamDistributionSource("agent", id, &a); err != nil {
			return out, err
		}
		if a.ID != id || a.ProjectID != projectID && !a.Shared {
			return out, ErrIntelligenceUnavailable
		}
		if a.ProjectID != projectID {
			if a.DistributionState, err = domain.CopyDistribution(a); err != nil {
				return out, err
			}
		}
		out.Agents = append(out.Agents, a)
		for _, ref := range a.Rules {
			ruleSet[ref] = true
		}
		for _, ref := range a.Skills {
			skillSet[ref] = true
		}
		for _, ref := range a.MCPServers {
			mcpSet[ref] = true
		}
	}
	ids := func(m map[string]bool, max int) ([]string, error) {
		out := []string{}
		for id := range m {
			out = append(out, id)
		}
		return domain.CanonicalIntelligenceIDs(out, max)
	}
	rules, err := ids(ruleSet, 128)
	if err != nil {
		return out, err
	}
	for _, id := range rules {
		var v domain.RuleDefinition
		if err := r.lockTeamDistributionSource("rule", id, &v); err != nil {
			return out, err
		}
		if v.ID != id || v.ProjectID != projectID && !v.Shared {
			return out, ErrIntelligenceUnavailable
		}
		if v.ProjectID != projectID {
			if v.DistributionState, err = domain.CopyDistribution(v); err != nil {
				return out, err
			}
		}
		out.Assets.Rules = append(out.Assets.Rules, v)
	}
	skills, err := ids(skillSet, 128)
	if err != nil {
		return out, err
	}
	for _, id := range skills {
		var v domain.SkillDefinition
		if err := r.lockTeamDistributionSource("skill", id, &v); err != nil {
			return out, err
		}
		if v.ID != id || v.ProjectID != projectID && !v.Shared {
			return out, ErrIntelligenceUnavailable
		}
		if v.ProjectID != projectID {
			if v.DistributionState, err = domain.CopyDistribution(v); err != nil {
				return out, err
			}
		}
		out.Assets.Skills = append(out.Assets.Skills, v)
		if len(v.FileRefs) > 0 {
			if out.Assets.SkillFiles == nil {
				out.Assets.SkillFiles = map[string]map[string][]byte{}
			}
			var rows []skillFileRow
			if err := r.locking(r.DB).Where("skill_id = ?", id).Find(&rows).Error; err != nil {
				return out, err
			}
			files := map[string][]byte{}
			for _, row := range rows {
				files[row.Path] = row.Content
			}
			out.Assets.SkillFiles[id] = files
		}
	}
	mcps, err := ids(mcpSet, 64)
	if err != nil {
		return out, err
	}
	for _, id := range mcps {
		var v domain.MCPServerDefinition
		if err := r.lockTeamDistributionSource("mcp", id, &v); err != nil {
			return out, err
		}
		if v.ID != id || v.ProjectID != projectID {
			return out, ErrIntelligenceUnavailable
		}
		out.Assets.MCPServers = append(out.Assets.MCPServers, v)
		if len(v.FileRefs) > 0 {
			if out.Assets.MCPFiles == nil {
				out.Assets.MCPFiles = map[string]map[string][]byte{}
			}
			var rows []mcpFileRow
			if err := r.locking(r.DB).Where("mcp_id = ?", id).Find(&rows).Error; err != nil {
				return out, err
			}
			files := map[string][]byte{}
			for _, row := range rows {
				files[row.Path] = row.Content
			}
			out.Assets.MCPFiles[id] = files
		}
	}
	return out, domain.ValidateTeamReleasePayload(out, projectID)
}

// Capture is called inside the project publication transaction. File writers
// lock the same source parent, so the policy and complete bytes stay together.
func (r *CatalogRepo) lockTeamDistributionSource(kind, id string, value any) error {
	table, err := distributionTable(kind)
	if err != nil {
		return err
	}
	var row payloadRow
	if err := r.locking(r.DB.Table(table)).Where("id = ?", id).Take(&row).Error; err != nil {
		return ErrIntelligenceUnavailable
	}
	// A locking read observes the latest row under MySQL REPEATABLE READ. Do
	// not discard it and issue a consistent read from an earlier MVCC snapshot.
	if err := json.Unmarshal(row.Payload, value); err != nil {
		return err
	}
	token, err := storedSourceRevision(table, row.Payload)
	if err != nil {
		return err
	}
	switch v := value.(type) {
	case *domain.AgentDefinition:
		v.SourceRevision = token
	case *domain.RuleDefinition:
		v.SourceRevision = token
	case *domain.SkillDefinition:
		v.SourceRevision = token
	case *domain.MCPServerDefinition:
		v.SourceRevision = token
	}
	return nil
}

func decodeTeamRelease(row teamReleaseRow) (domain.TeamRelease, error) {
	var out domain.TeamRelease
	if err := json.Unmarshal(row.Payload, &out); err != nil {
		return out, err
	}
	if out.ID != row.ID || out.ProjectID != row.ProjectID || out.Revision != row.Revision || out.Version != row.Version || out.State != row.State {
		return out, ErrIntelligenceConflict
	}
	digest, err := domain.IntelligenceDigest(out.Payload)
	if err != nil || digest != out.SHA256 {
		return out, fmt.Errorf("team release digest mismatch")
	}
	return out, nil
}
func (r *CatalogRepo) TeamRelease(projectID, id string) (domain.TeamRelease, error) {
	var row teamReleaseRow
	if err := r.DB.Where("project_id = ? AND id = ?", projectID, id).Take(&row).Error; err != nil {
		return domain.TeamRelease{}, ErrIntelligenceUnavailable
	}
	return decodeTeamRelease(row)
}
func (r *CatalogRepo) TeamReleases(projectID string) ([]domain.TeamRelease, error) {
	var rows []teamReleaseRow
	if err := r.DB.Where("project_id = ?", projectID).Order("version DESC").Limit(20).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []domain.TeamRelease{}
	for _, row := range rows {
		v, err := decodeTeamRelease(row)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (r *CatalogRepo) RevokeTeamRelease(projectID, id string, revision int, actor, reason string) (domain.TeamRelease, error) {
	var out domain.TeamRelease
	if revision <= 0 || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(reason) > 2000 {
		return out, fmt.Errorf("expectedRevision, authenticated actor and bounded reason required")
	}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var row teamReleaseRow
		if err := r.locking(tx).Where("project_id = ? AND id = ?", projectID, id).Take(&row).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		v, err := decodeTeamRelease(row)
		if err != nil {
			return err
		}
		if v.Revision != revision || v.State != domain.TeamReleasePublished {
			return ErrIntelligenceConflict
		}
		v.State = domain.TeamReleaseRevoked
		v.Revision++
		v.Decisions = append(v.Decisions, domain.IntelligenceDecision{Decision: domain.TeamReleaseRevoked, Actor: actor, Reason: reason, At: time.Now().UTC()})
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		res := tx.Model(&teamReleaseRow{}).Where("id = ? AND revision = ?", id, revision).Updates(map[string]any{"state": v.State, "revision": v.Revision, "payload": raw})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrIntelligenceConflict
		}
		out = v
		return nil
	})
	return out, err
}

// RestoreTeamRelease publishes a new version from an immutable historical
// payload. It never rewrites history or silently reactivates a revoked row.
func (r *CatalogRepo) RestoreTeamRelease(projectID, sourceID, name, actor, reason string, expectedBaseVersion int) (domain.TeamRelease, error) {
	var result domain.TeamRelease
	if strings.TrimSpace(name) == "" || len(name) > 200 || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(reason) > 2000 || expectedBaseVersion < 0 {
		return result, fmt.Errorf("bounded name, authenticated actor and reason required")
	}
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var project projectRow
		if err := r.locking(tx).Where("id = ?", projectID).Take(&project).Error; err != nil {
			return ErrIntelligenceUnavailable
		}
		scoped := &CatalogRepo{deps: &deps{DB: tx}}
		archived, err := scoped.TeamRelease(projectID, sourceID)
		if err != nil {
			return err
		}
		if err := domain.ValidateTeamReleasePayload(archived.Payload, projectID); err != nil {
			return err
		}
		var version int
		if err := tx.Model(&teamReleaseRow{}).Select("COALESCE(MAX(version),0)").Where("project_id = ?", projectID).Scan(&version).Error; err != nil {
			return err
		}
		if version != expectedBaseVersion {
			return ErrIntelligenceConflict
		}
		id, err := intelligenceID("team-")
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		result = domain.TeamRelease{ID: id, ProjectID: projectID, Name: strings.TrimSpace(name), Version: version + 1, Revision: 1, State: domain.TeamReleasePublished, SHA256: archived.SHA256, Payload: archived.Payload, CreatedBy: actor, CreatedAt: now, Decisions: []domain.IntelligenceDecision{{Decision: domain.TeamReleasePublished, Actor: actor, Reason: "restore " + sourceID + ": " + reason, At: now}}}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return tx.Create(&teamReleaseRow{ID: id, ProjectID: projectID, Version: result.Version, Revision: 1, State: result.State, Payload: raw, Created: now}).Error
	})
	return result, err
}
