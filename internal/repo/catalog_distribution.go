package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

var ErrDistributionOwner = errors.New("only the source owner or platform administrator may change distribution policy")

type DistributionActor struct {
	UserID        int64
	LoginName     string
	PlatformAdmin bool
	Local         bool
}
type DistributionDecision struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	SourceRevision string `json:"sourceRevision"`
	domain.DistributionState
	EffectiveDistribution domain.DistributionPolicy `json:"effectiveDistribution"`
}

func distributionTable(kind string) (string, error) {
	switch kind {
	case "agent":
		return "agents", nil
	case "rule":
		return "rules", nil
	case "skill":
		return "skills", nil
	case "mcp":
		return "mcp_servers", nil
	}
	return "", domain.ErrDistributionInvalid
}

// guardDistributionWrite runs inside the already locked parent-source write
// transaction. Old clients may omit governance metadata; they cannot erase it.
// Only the dedicated owner decision path below may change the local declaration.
func guardDistributionWrite(db *gorm.DB, table string, values map[string]any, exists bool) error {
	if table != "agents" && table != "rules" && table != "skills" && table != "mcp_servers" {
		return nil
	}
	raw, ok := values["payload"].([]byte)
	if !ok {
		return domain.ErrDistributionInvalid
	}
	var incoming domain.DistributionState
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return err
	}
	if err := incoming.Validate(); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var row payloadRow
	if err := (&deps{DB: db}).locking(db.Table(table)).Where("id = ?", values["id"]).Take(&row).Error; err != nil {
		return err
	}
	var old domain.DistributionState
	if err := json.Unmarshal(row.Payload, &old); err != nil {
		return err
	}
	if incoming.Distribution != nil && !reflect.DeepEqual(incoming.Distribution, old.Distribution) {
		return fmt.Errorf("%w: use the owner distribution endpoint", domain.ErrDistributionDenied)
	}
	if incoming.DistributionOrigins != nil && !reflect.DeepEqual(incoming.DistributionOrigins, old.DistributionOrigins) {
		return fmt.Errorf("%w: inherited origins are server-owned", domain.ErrDistributionDenied)
	}
	if err := old.Check("edit"); err != nil {
		// Explicit unsharing remains possible to withdraw a resource. This never
		// permits any source/metadata change or a new share grant.
		var a, b map[string]json.RawMessage
		_ = json.Unmarshal(row.Payload, &a)
		_ = json.Unmarshal(raw, &b)
		wasShared := string(a["shared"]) == "true"
		nowShared := string(b["shared"]) == "true"
		for _, m := range []map[string]json.RawMessage{a, b} {
			for _, key := range []string{"sourceRevision", "updatedAt", "shared", "distribution", "distributionOrigins"} {
				delete(m, key)
			}
		}
		if !wasShared || nowShared || !reflect.DeepEqual(a, b) {
			return err
		}
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	// Preserve old state exactly, including nil/omitempty legacy representation.
	if old.Distribution != nil {
		body["distribution"] = mustJSON(old.Distribution)
	} else {
		delete(body, "distribution")
	}
	if len(old.DistributionOrigins) > 0 {
		body["distributionOrigins"] = mustJSON(old.DistributionOrigins)
	} else {
		delete(body, "distributionOrigins")
	}
	values["payload"] = mustJSON(body)
	return nil
}

// SetDistribution locks the owning project before the source row so an owner
// transfer cannot race a policy decision. Origins/floors never come from input.
func (r *CatalogRepo) SetDistribution(kind, id, expected string, policy domain.DistributionPolicy, reason string, actor DistributionActor) (DistributionDecision, error) {
	out := DistributionDecision{}
	table, err := distributionTable(kind)
	if err != nil {
		return out, err
	}
	if expected == "" {
		return out, ErrSourcePrecondition
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 1000 {
		return out, fmt.Errorf("%w: reason must contain 1-1000 bytes", domain.ErrDistributionInvalid)
	}
	if err := (domain.DistributionState{Distribution: &policy}).Validate(); err != nil {
		return out, err
	}
	var initial struct {
		ProjectID   string `json:"projectId"`
		Scope       string `json:"scope"`
		OwnerUserID int64  `json:"ownerUserId"`
	}
	var source payloadRow
	if err := r.DB.Table(table).Where("id = ?", id).Take(&source).Error; err != nil {
		return out, err
	}
	if err := json.Unmarshal(source.Payload, &initial); err != nil {
		return out, err
	}
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		if initial.ProjectID != "" {
			var project projectRow
			if err := r.locking(tx).Where("id = ?", initial.ProjectID).Take(&project).Error; err != nil {
				return err
			}
			if !actor.Local && !actor.PlatformAdmin && (project.OwnerUserID == nil || *project.OwnerUserID != actor.UserID || actor.UserID <= 0) {
				return ErrDistributionOwner
			}
		} else if !actor.Local && !actor.PlatformAdmin && (kind != "mcp" || initial.Scope != "personal" || actor.UserID <= 0 || initial.OwnerUserID != actor.UserID) {
			return ErrDistributionOwner
		}
		scoped := *r
		scoped.deps = &deps{DB: tx}
		return scoped.editSource(table, id, expected, func(catalog *CatalogRepo, raw []byte, token string, exists bool) error {
			if !exists {
				return ErrSourceConflict
			}
			var current struct {
				ProjectID   string `json:"projectId"`
				Scope       string `json:"scope"`
				OwnerUserID int64  `json:"ownerUserId"`
				Name        string `json:"name"`
				domain.DistributionState
			}
			if err := json.Unmarshal(raw, &current); err != nil {
				return err
			}
			if current.ProjectID != initial.ProjectID || current.Scope != initial.Scope || current.OwnerUserID != initial.OwnerUserID {
				return ErrSourceConflict
			}
			if err := current.DistributionState.Validate(); err != nil {
				return err
			}
			// Even a target owner cannot relax an inherited origin's declaration.
			floor := domain.DistributionState{DistributionOrigins: current.DistributionOrigins}.Effective()
			if (floor.Copy == "deny" && policy.Copy != "deny") || (floor.Edit == "deny" && policy.Edit != "deny") || (floor.Export == "deny" && policy.Export != "deny") {
				return domain.ErrDistributionDenied
			}
			now := time.Now().UTC()
			if err := catalog.snapshotRevision("distribution-"+kind, id, current.Name, 0, 0, map[string]any{"kind": kind, "id": id, "actorUserId": actor.UserID, "actor": actor.LoginName, "local": actor.Local, "reason": reason, "before": current.DistributionState, "after": policy, "sourceRevision": expected, "createdAt": now}); err != nil {
				return err
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				return err
			}
			body["distribution"] = mustJSON(policy)
			body["sourceRevision"] = mustJSON(token)
			body["updatedAt"] = mustJSON(now)
			if err := tx.Table(table).Where("id = ?", id).Updates(map[string]any{"payload": mustJSON(body), "updated_at": timePtr(now)}).Error; err != nil {
				return err
			}
			current.Distribution = &policy
			out = DistributionDecision{Kind: kind, ID: id, SourceRevision: token, DistributionState: current.DistributionState, EffectiveDistribution: current.DistributionState.Effective()}
			return nil
		})
	})
	return out, err
}

// lockDistributionSource is used only within an existing transaction. It does
// not grant visibility; the HTTP caller must independently authorize inspection.
func (r *CatalogRepo) lockDistributionSource(kind, id string) (any, error) {
	table, err := distributionTable(kind)
	if err != nil {
		return nil, err
	}
	var row payloadRow
	if err := r.locking(r.DB.Table(table)).Where("id = ?", id).Take(&row).Error; err != nil {
		return nil, err
	}
	switch kind {
	case "agent":
		var v domain.AgentDefinition
		if err := json.Unmarshal(row.Payload, &v); err != nil {
			return nil, err
		}
		v.SourceRevision = sourceRevision(table, v)
		return v, nil
	case "rule":
		var v domain.RuleDefinition
		if err := json.Unmarshal(row.Payload, &v); err != nil {
			return nil, err
		}
		v.SourceRevision = sourceRevision(table, v)
		return v, nil
	case "skill":
		var v domain.SkillDefinition
		if err := json.Unmarshal(row.Payload, &v); err != nil {
			return nil, err
		}
		v.SourceRevision = sourceRevision(table, v)
		return v, nil
	case "mcp":
		var v domain.MCPServerDefinition
		if err := json.Unmarshal(row.Payload, &v); err != nil {
			return nil, err
		}
		v.SourceRevision = sourceRevision(table, domain.PublicMCPServer(v))
		return v, nil
	}
	return nil, domain.ErrDistributionInvalid
}

// WithGenericTemplateCopy makes the existing multi-resource adoption atomic
// while locking the real template container shared by its conditional writers.
func (r *CatalogRepo) WithGenericTemplateCopy(workflowID, projectID string, copy func(*Store) error) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var row genericTemplateRow
		if err := r.locking(tx).Where("id = ?", workflowID).Take(&row).Error; err != nil {
			return err
		}
		store := newStore(tx)
		template, ok := store.GenericTemplateByWorkflow(workflowID)
		if !ok {
			return ErrSourceConflict
		}
		// Reuse an independently existing project definition; only new definitions
		// are copies. Preflight every actual source before ancillary writes.
		for _, source := range template.Agents {
			a, found := store.Agent(source.ID)
			b, other := store.Agent(source.ID + "-" + projectID)
			if (found && a.ProjectID == projectID) || (other && b.ProjectID == projectID) {
				continue
			}
			if err := domain.CheckDistribution(source, "copy"); err != nil {
				return err
			}
		}
		for _, source := range template.Rules {
			a, found := store.Rule(source.ID)
			b, other := store.Rule(source.ID + "-" + projectID)
			if (found && a.ProjectID == projectID) || (other && b.ProjectID == projectID) {
				continue
			}
			if err := domain.CheckDistribution(source, "copy"); err != nil {
				return err
			}
		}
		for _, source := range template.Skills {
			a, found := store.Skill(source.ID)
			b, other := store.Skill(source.ID + "-" + projectID)
			if (found && a.ProjectID == projectID) || (other && b.ProjectID == projectID) {
				continue
			}
			if err := domain.CheckDistribution(source, "copy"); err != nil {
				return err
			}
		}
		return copy(store)
	})
}

func ordinaryDistributionState(old, incoming domain.DistributionState) (domain.DistributionState, error) {
	if err := incoming.Validate(); err != nil {
		return domain.DistributionState{}, err
	}
	if incoming.Distribution != nil && !reflect.DeepEqual(old.Distribution, incoming.Distribution) {
		return domain.DistributionState{}, domain.ErrDistributionDenied
	}
	if incoming.DistributionOrigins != nil && !reflect.DeepEqual(old.DistributionOrigins, incoming.DistributionOrigins) {
		return domain.DistributionState{}, domain.ErrDistributionDenied
	}
	if err := old.Check("edit"); err != nil {
		return domain.DistributionState{}, err
	}
	return old, nil
}
