package domain

import "encoding/json"

// TemplateDefaultBaseline is retained in definition_revisions at explicit
// adoption. Source and adopted shapes differ because project IDs/bindings are
// remapped. Neither historical shape is inferred for preexisting clones.
type TemplateDefaultBaseline struct {
	ProjectID  string          `json:"projectId"`
	TemplateID string          `json:"templateId"`
	SourceID   string          `json:"sourceId"`
	Source     json.RawMessage `json:"source"`
	Adopted    json.RawMessage `json:"adopted"`
}

type DefaultUpgradeDiff struct {
	Kind                string          `json:"kind"`
	EntityID            string          `json:"entityId"`
	Tracked             bool            `json:"tracked"`
	BaselineRevisionID  string          `json:"baselineRevisionId,omitempty"`
	TemplateID          string          `json:"templateId,omitempty"`
	SourceID            string          `json:"sourceId,omitempty"`
	DefaultAvailable    bool            `json:"defaultAvailable"`
	BaseSHA256          string          `json:"baseSHA256,omitempty"`
	DefaultSHA256       string          `json:"defaultSHA256,omitempty"`
	TeamSHA256          string          `json:"teamSHA256,omitempty"`
	DefaultChangedPaths []string        `json:"defaultChangedPaths"`
	OverridePaths       []string        `json:"overridePaths"`
	ConflictingPaths    []string        `json:"conflictingPaths"`
	BaseDefault         json.RawMessage `json:"baseDefault,omitempty"`
	CurrentDefault      json.RawMessage `json:"currentDefault,omitempty"`
	AdoptedBase         json.RawMessage `json:"adoptedBase,omitempty"`
	TeamOverride        json.RawMessage `json:"teamOverride,omitempty"`
}
