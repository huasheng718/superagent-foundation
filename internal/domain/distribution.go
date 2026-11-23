package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// DistributionState governs platform-managed operations, not source visibility,
// execution authority, legal entitlement, or copying bytes outside the platform.
// Omitting it preserves legacy JSON and immutable snapshot digests.
type DistributionState struct {
	Distribution        *DistributionPolicy  `json:"distribution,omitempty"`
	DistributionOrigins []DistributionOrigin `json:"distributionOrigins,omitempty"`
}
type DistributionPolicy struct {
	Copy   string `json:"copy"`
	Edit   string `json:"edit"`
	Export string `json:"export"`
}
type DistributionOrigin struct {
	Kind           string             `json:"kind"`
	ID             string             `json:"id"`
	ProjectID      string             `json:"projectId,omitempty"`
	SourceRevision string             `json:"sourceRevision"`
	SHA256         string             `json:"sha256"`
	Policy         DistributionPolicy `json:"policy"`
}

var ErrDistributionDenied = errors.New("distribution policy denies this platform operation")
var ErrDistributionInvalid = errors.New("invalid distribution policy")

func (s DistributionState) Validate() error {
	valid := func(p DistributionPolicy) bool {
		return (p.Copy == "allow" || p.Copy == "deny") && (p.Edit == "allow" || p.Edit == "deny") && (p.Export == "allow" || p.Export == "deny")
	}
	if s.Distribution != nil && !valid(*s.Distribution) {
		return ErrDistributionInvalid
	}
	if len(s.DistributionOrigins) > 64 {
		return fmt.Errorf("%w: too many origins", ErrDistributionInvalid)
	}
	for _, o := range s.DistributionOrigins {
		if !valid(o.Policy) || o.ID == "" || o.SourceRevision == "" || len(o.SHA256) != 64 || (o.Kind != "agent" && o.Kind != "rule" && o.Kind != "skill" && o.Kind != "mcp") {
			return ErrDistributionInvalid
		}
	}
	return nil
}
func (s DistributionState) Effective() DistributionPolicy {
	p := DistributionPolicy{Copy: "allow", Edit: "allow", Export: "allow"}
	apply := func(q DistributionPolicy) {
		if q.Copy != "allow" {
			p.Copy = "deny"
		}
		if q.Edit != "allow" {
			p.Edit = "deny"
		}
		if q.Export != "allow" {
			p.Export = "deny"
		}
	}
	if s.Distribution != nil {
		apply(*s.Distribution)
	}
	for _, o := range s.DistributionOrigins {
		apply(o.Policy)
	}
	return p
}
func (s DistributionState) Check(operation string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	p := s.Effective()
	v := ""
	switch operation {
	case "copy":
		v = p.Copy
	case "edit":
		v = p.Edit
	case "export":
		v = p.Export
	default:
		return ErrDistributionInvalid
	}
	if v != "allow" {
		return fmt.Errorf("%w: %s", ErrDistributionDenied, operation)
	}
	return nil
}
func DistributionOf(value any) DistributionState {
	switch v := value.(type) {
	case AgentDefinition:
		return v.DistributionState
	case RuleDefinition:
		return v.DistributionState
	case SkillDefinition:
		return v.DistributionState
	case MCPServerDefinition:
		return v.DistributionState
	}
	return DistributionState{}
}
func CheckDistribution(value any, operation string) error {
	switch v := value.(type) {
	case AgentDefinition, RuleDefinition, SkillDefinition, MCPServerDefinition:
		return DistributionOf(value).Check(operation)
	case *AgentDefinition:
		if v != nil {
			return v.DistributionState.Check(operation)
		}
	case *RuleDefinition:
		if v != nil {
			return v.DistributionState.Check(operation)
		}
	case *SkillDefinition:
		if v != nil {
			return v.DistributionState.Check(operation)
		}
	case *MCPServerDefinition:
		if v != nil {
			return v.DistributionState.Check(operation)
		}
	case DistributionState:
		return v.Check(operation)
	}
	return ErrDistributionInvalid
}

// CopyDistribution records the actual observed native source, including all
// previous floors. Callers must also check read/target-write authority and lock
// or CAS the source when persisting a managed copy.
func CopyDistribution(value any) (DistributionState, error) {
	state := DistributionOf(value)
	if err := state.Check("copy"); err != nil {
		return DistributionState{}, err
	}
	var kind, id, project, revision string
	switch v := value.(type) {
	case AgentDefinition:
		kind, id, project, revision = "agent", v.ID, v.ProjectID, v.SourceRevision
	case RuleDefinition:
		kind, id, project, revision = "rule", v.ID, v.ProjectID, v.SourceRevision
	case SkillDefinition:
		kind, id, project, revision = "skill", v.ID, v.ProjectID, v.SourceRevision
	case MCPServerDefinition:
		kind, id, project, revision = "mcp", v.ID, v.ProjectID, v.SourceRevision
	default:
		return DistributionState{}, ErrDistributionInvalid
	}
	if mcp, ok := value.(MCPServerDefinition); ok {
		value = PublicMCPServer(mcp)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return DistributionState{}, err
	}
	sum := sha256.Sum256(raw)
	if revision == "" {
		revision = "snapshot-" + hex.EncodeToString(sum[:])
	}
	out := DistributionState{DistributionOrigins: append([]DistributionOrigin(nil), state.DistributionOrigins...)}
	if state.Distribution != nil {
		policy := *state.Distribution
		out.Distribution = &policy
	}
	origin := DistributionOrigin{Kind: kind, ID: id, ProjectID: project, SourceRevision: revision, SHA256: hex.EncodeToString(sum[:]), Policy: state.Effective()}
	found := false
	for _, o := range out.DistributionOrigins {
		if o == origin {
			found = true
			break
		}
	}
	if !found {
		out.DistributionOrigins = append(out.DistributionOrigins, origin)
	}
	return out, out.Validate()
}

// PreserveDistribution retains every known inherited floor across restores,
// Git refreshes and other application-managed replacements. Target metadata is
// never authority to remove an origin. The local declaration stays unchanged.
func PreserveDistribution(base DistributionState, incoming DistributionState) (DistributionState, error) {
	out := DistributionState{DistributionOrigins: append([]DistributionOrigin(nil), base.DistributionOrigins...)}
	if base.Distribution != nil {
		policy := *base.Distribution
		out.Distribution = &policy
	}
	for _, o := range incoming.DistributionOrigins {
		found := false
		for _, old := range out.DistributionOrigins {
			if old == o {
				found = true
				break
			}
		}
		if !found {
			out.DistributionOrigins = append(out.DistributionOrigins, o)
		}
	}
	return out, out.Validate()
}
