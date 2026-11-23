package repo

import "agentflow/internal/domain"

// Known application provenance cannot be cleared by submitting a new candidate
// ID. Unattributed manual text is not automatically recognized as a copy.
func candidateDistribution(input domain.SkillCandidatePackage, previous *domain.SkillCandidate, source *domain.Run) (domain.DistributionState, error) {
	out := domain.DistributionState{}
	inherit := func(value any) error {
		if err := domain.CheckDistribution(value, "edit"); err != nil {
			return err
		}
		state, err := domain.CopyDistribution(value)
		if err != nil {
			return err
		}
		out, err = domain.PreserveDistribution(out, state)
		return err
	}
	if previous != nil {
		if err := inherit(previous.Package.Skill); err != nil {
			return out, err
		}
	}
	if source == nil || source.Intelligence == nil {
		return out, nil
	}
	context := source.Intelligence
	digest, err := context.Digest()
	if err != nil || digest != context.SHA256 || context.ProjectID != input.Skill.ProjectID {
		return out, ErrIntelligenceConflict
	}
	// A generated package cannot prove which restricted input contributed to its
	// bytes, so an explicit Run source conservatively inherits all restrictions.
	inheritRestricted := func(value any) error {
		p := domain.DistributionOf(value).Effective()
		if p.Copy == "allow" && p.Edit == "allow" && p.Export == "allow" {
			return nil
		}
		return inherit(value)
	}
	for _, value := range context.Configuration.Agents {
		if err := inheritRestricted(value); err != nil {
			return out, err
		}
	}
	for _, value := range context.Configuration.Assets.Rules {
		if err := inheritRestricted(value); err != nil {
			return out, err
		}
	}
	for _, value := range context.Configuration.Assets.Skills {
		if err := inheritRestricted(value); err != nil {
			return out, err
		}
	}
	for _, value := range context.Configuration.Assets.MCPServers {
		if err := inheritRestricted(value); err != nil {
			return out, err
		}
	}
	return out, nil
}
