package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func templateBaselineID(kind, projectID, entityID string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + projectID + "\x00" + entityID))
	return "default-base-" + hex.EncodeToString(sum[:])
}

// RecordTemplateDefaultBase is called only after explicit new-asset adoption,
// never from startup, reads, or re-adoption of an untracked historical clone.
// A deterministic existing DefinitionRevision ID makes the initial base
// immutable across independently connected adopters.
func (r *CatalogRepo) RecordTemplateDefaultBase(kind, projectID, templateID, sourceID, entityID string, source, adopted any) error {
	if kind != "agent" && kind != "rule" && kind != "skill" {
		return fmt.Errorf("unsupported template resource")
	}
	if projectID == "" || templateID == "" || sourceID == "" || entityID == "" {
		return fmt.Errorf("template provenance identity required")
	}
	base, err := json.Marshal(source)
	if err != nil {
		return err
	}
	team, err := json.Marshal(adopted)
	if err != nil {
		return err
	}
	if len(base)+len(team) > domain.IntelligenceMaxSnapshotBytes {
		return fmt.Errorf("template baseline too large")
	}
	payload, err := json.Marshal(domain.TemplateDefaultBaseline{ProjectID: projectID, TemplateID: templateID, SourceID: sourceID, Source: base, Adopted: team})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	revision := domain.DefinitionRevision{ID: templateBaselineID(kind, projectID, entityID), Kind: kind + "-default", EntityID: entityID, Version: 1, Revision: 1, Name: sourceID, Summary: "Original default at explicit project adoption", Payload: payload, CreatedAt: now}
	raw, err := json.Marshal(revision)
	if err != nil {
		return err
	}
	return r.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&definitionRevisionRow{ID: revision.ID, Kind: revision.Kind, EntityID: entityID, Version: 1, Revision: 1, Name: sourceID, Payload: raw, Created: &now}).Error
}

func defaultComparable(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	// Catalog timestamps and identity remapping do not represent policy edits.
	for _, key := range []string{"id", "projectId", "shared", "updatedAt", "createdAt", "version", "revision", "sourceRevision"} {
		delete(object, key)
	}
	return json.Marshal(object)
}

func changedDefaultPaths(before, after json.RawMessage) ([]string, error) {
	var a, b any
	if err := json.Unmarshal(before, &a); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(after, &b); err != nil {
		return nil, err
	}
	out := []string{}
	var walk func(string, any, any)
	walk = func(path string, left, right any) {
		if reflect.DeepEqual(left, right) {
			return
		}
		lm, lok := left.(map[string]any)
		rm, rok := right.(map[string]any)
		if !lok || !rok {
			out = append(out, path)
			return
		}
		keys := map[string]bool{}
		for k := range lm {
			keys[k] = true
		}
		for k := range rm {
			keys[k] = true
		}
		for k := range keys {
			walk(path+"/"+strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"), lm[k], rm[k])
		}
	}
	walk("", a, b)
	sort.Strings(out)
	return out, nil
}

// DefaultUpgrades is a bounded read-only three-way comparison. The original
// mapped baseline remains fixed; new platform defaults never replace team
// overrides. Existing unknown lineage is reported rather than backfilled.
func (r *CatalogRepo) DefaultUpgrades(projectID string) ([]domain.DefaultUpgradeDiff, error) {
	type asset struct {
		kind, id string
		value    any
	}
	assets := []asset{}
	for _, a := range r.Agents() {
		if a.ProjectID == projectID {
			assets = append(assets, asset{"agent", a.ID, a})
		}
	}
	for _, a := range r.Rules() {
		if a.ProjectID == projectID {
			assets = append(assets, asset{"rule", a.ID, a})
		}
	}
	for _, a := range r.Skills() {
		if a.ProjectID == projectID {
			assets = append(assets, asset{"skill", a.ID, a})
		}
	}
	if len(assets) > 500 {
		return nil, fmt.Errorf("default comparison exceeds 500 project resources")
	}
	out := []domain.DefaultUpgradeDiff{}
	for _, a := range assets {
		diff := domain.DefaultUpgradeDiff{Kind: a.kind, EntityID: a.id, DefaultChangedPaths: []string{}, OverridePaths: []string{}, ConflictingPaths: []string{}}
		var row definitionRevisionRow
		err := r.DB.Where("id = ?", templateBaselineID(a.kind, projectID, a.id)).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			out = append(out, diff)
			continue
		}
		if err != nil {
			return nil, err
		}
		var revision domain.DefinitionRevision
		if json.Unmarshal(row.Payload, &revision) != nil || revision.ID != row.ID {
			return nil, ErrIntelligenceConflict
		}
		var baseline domain.TemplateDefaultBaseline
		if revision.Kind != a.kind+"-default" || revision.EntityID != a.id || json.Unmarshal(revision.Payload, &baseline) != nil || baseline.ProjectID != projectID {
			return nil, ErrIntelligenceConflict
		}
		diff.Tracked = true
		diff.BaselineRevisionID = revision.ID
		diff.TemplateID = baseline.TemplateID
		diff.SourceID = baseline.SourceID
		diff.BaseDefault, err = defaultComparable(baseline.Source)
		if err != nil {
			return nil, err
		}
		diff.AdoptedBase, err = defaultComparable(baseline.Adopted)
		if err != nil {
			return nil, err
		}
		diff.TeamOverride, err = defaultComparable(a.value)
		if err != nil {
			return nil, err
		}
		diff.BaseSHA256, _ = domain.IntelligenceDigest(diff.BaseDefault)
		diff.TeamSHA256, _ = domain.IntelligenceDigest(diff.TeamOverride)
		diff.OverridePaths, err = changedDefaultPaths(diff.AdoptedBase, diff.TeamOverride)
		if err != nil {
			return nil, err
		}
		if template, found := r.GenericTemplateByWorkflow(baseline.TemplateID); found {
			var latest any
			switch a.kind {
			case "agent":
				for _, item := range template.Agents {
					if item.ID == baseline.SourceID {
						latest = item
					}
				}
			case "rule":
				for _, item := range template.Rules {
					if item.ID == baseline.SourceID {
						latest = item
					}
				}
			case "skill":
				for _, item := range template.Skills {
					if item.ID == baseline.SourceID {
						latest = item
					}
				}
			}
			if latest != nil {
				diff.DefaultAvailable = true
				diff.CurrentDefault, err = defaultComparable(latest)
				if err != nil {
					return nil, err
				}
				diff.DefaultSHA256, _ = domain.IntelligenceDigest(diff.CurrentDefault)
				diff.DefaultChangedPaths, err = changedDefaultPaths(diff.BaseDefault, diff.CurrentDefault)
				if err != nil {
					return nil, err
				}
			}
		}
		for _, dp := range diff.DefaultChangedPaths {
			for _, op := range diff.OverridePaths {
				if dp == op || strings.HasPrefix(dp, op+"/") || strings.HasPrefix(op, dp+"/") {
					diff.ConflictingPaths = append(diff.ConflictingPaths, dp)
					break
				}
			}
		}
		out = append(out, diff)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind+":"+out[i].EntityID < out[j].Kind+":"+out[j].EntityID })
	if _, err := domain.IntelligenceDigest(out); err != nil {
		return nil, err
	}
	return out, nil
}
