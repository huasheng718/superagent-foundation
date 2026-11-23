package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func ValidateTeamDistribution(payload TeamReleasePayload) error {
	for _, v := range payload.Agents {
		if err := v.DistributionState.Validate(); err != nil {
			return err
		}
	}
	for _, v := range payload.Assets.Rules {
		if err := v.DistributionState.Validate(); err != nil {
			return err
		}
	}
	for _, v := range payload.Assets.Skills {
		if err := v.DistributionState.Validate(); err != nil {
			return err
		}
	}
	for _, v := range payload.Assets.MCPServers {
		if err := v.DistributionState.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// PreserveTeamDistribution keeps native governance attached to stable resource
// identities when new Git bytes are reviewed. Git is not policy authority.
func PreserveTeamDistribution(base TeamReleasePayload, next *TeamReleasePayload) error {
	states := map[string]DistributionState{}
	definitions := map[string]any{}
	for _, v := range base.Agents {
		states["agent:"+v.ID] = v.DistributionState
		definitions["agent:"+v.ID] = v
	}
	for _, v := range base.Assets.Rules {
		states["rule:"+v.ID] = v.DistributionState
		definitions["rule:"+v.ID] = v
	}
	for _, v := range base.Assets.Skills {
		states["skill:"+v.ID] = v.DistributionState
		definitions["skill:"+v.ID] = v
	}
	for _, v := range base.Assets.MCPServers {
		states["mcp:"+v.ID] = v.DistributionState
		definitions["mcp:"+v.ID] = v
	}
	apply := func(key string, incoming any) (DistributionState, error) {
		previous, ok := states[key]
		if !ok {
			return DistributionState{}, fmt.Errorf("updated resource absent from reviewed source: %s", key)
		}
		if err := previous.Check("edit"); err != nil && !sameTeamDefinition(definitions[key], incoming) {
			return DistributionState{}, err
		}
		delete(states, key)
		return PreserveDistribution(previous, DistributionOf(incoming))
	}
	var err error
	for i := range next.Agents {
		v := &next.Agents[i]
		if v.DistributionState, err = apply("agent:"+v.ID, *v); err != nil {
			return err
		}
	}
	for i := range next.Assets.Rules {
		v := &next.Assets.Rules[i]
		if v.DistributionState, err = apply("rule:"+v.ID, *v); err != nil {
			return err
		}
	}
	for i := range next.Assets.Skills {
		v := &next.Assets.Skills[i]
		if v.DistributionState, err = apply("skill:"+v.ID, *v); err != nil {
			return err
		}
	}
	for i := range next.Assets.MCPServers {
		v := &next.Assets.MCPServers[i]
		if v.DistributionState, err = apply("mcp:"+v.ID, *v); err != nil {
			return err
		}
	}
	if len(states) != 0 {
		return fmt.Errorf("updated selection omits reviewed resources")
	}
	return nil
}

// Native edit policy covers the definition's content and configuration, not
// fresh transport revisions/timestamps or the separately retained governance.
func sameTeamDefinition(a, b any) bool {
	comparable := func(v any) []byte {
		raw, _ := json.Marshal(v)
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		for _, key := range []string{"sourceRevision", "updatedAt", "distribution", "distributionOrigins"} {
			delete(body, key)
		}
		raw, _ = json.Marshal(body)
		return raw
	}
	return bytes.Equal(comparable(a), comparable(b))
}
