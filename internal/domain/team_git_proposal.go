package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

const TeamGitProposalPolicy = "teamai-configuration-proposal-v1"

type TeamGitProposalFile struct {
	Path   string         `json:"path"`
	Before TeamImportFile `json:"before"`
	After  TeamImportFile `json:"after"`
}

type TeamGitProposalValidation struct {
	Policy                string   `json:"policy"`
	Checks                []string `json:"checks"`
	RunGatesVerified      bool     `json:"runGatesVerified"`
	RemoteTargetAtomicCAS bool     `json:"remoteTargetAtomicCAS"`
}

// TeamGitProposal retains controller-prepared bytes, separately from formal
// releases and Runner execution. Its publication ends at an unmerged draft PR.
type TeamGitProposal struct {
	ID                        string                    `json:"id"`
	ProjectID                 string                    `json:"projectId"`
	RepositoryID              string                    `json:"repositoryId"`
	RepositoryURL             string                    `json:"repositoryUrl"`
	ProviderProject           string                    `json:"providerProject"`
	ReleaseID                 string                    `json:"releaseId"`
	ReleaseSHA256             string                    `json:"releaseSha256"`
	ReleaseRevision           int                       `json:"releaseRevision"`
	TargetRef                 string                    `json:"targetRef"`
	BaseCommit                string                    `json:"baseCommit"`
	ResultSHA                 string                    `json:"resultSHA"`
	ResultTreeSHA             string                    `json:"resultTreeSHA"`
	SHA256                    string                    `json:"sha256"`
	Patch                     string                    `json:"patch"`
	PatchSHA256               string                    `json:"patchSha256"`
	ProposedSHA256            string                    `json:"proposedSha256"`
	ResultConfigurationSHA256 string                    `json:"resultConfigurationSha256"`
	BundleSHA256              string                    `json:"bundleSha256"`
	BundleSize                int64                     `json:"bundleSize"`
	Files                     []TeamGitProposalFile     `json:"files"`
	Validation                TeamGitProposalValidation `json:"validation"`
	State                     string                    `json:"state"`
	Revision                  int                       `json:"revision"`
	CreatedBy                 string                    `json:"createdBy"`
	CreatedAt                 time.Time                 `json:"createdAt"`
	UpdatedAt                 time.Time                 `json:"updatedAt"`
	Decisions                 []IntelligenceDecision    `json:"decisions"`
	Publication               *ContributionPublication  `json:"publication,omitempty"`
}

func TeamGitProposalDigest(v TeamGitProposal) (string, error) {
	v.SHA256 = ""
	v.State = ""
	v.Revision = 0
	v.UpdatedAt = time.Time{}
	v.Decisions = nil
	v.Publication = nil
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func TeamGitProposalID(v TeamGitProposal) string {
	input := struct{ Kind, Project, Repository, Release, ReleaseSHA, Base, Ref, Patch, Proposed, Actor string }{TeamGitProposalPolicy, v.ProjectID, v.RepositoryID, v.ReleaseID, v.ReleaseSHA256, v.BaseCommit, v.TargetRef, v.PatchSHA256, v.ProposedSHA256, v.CreatedBy}
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// TeamGitWritebackPolicy evaluates the frozen formal closure. Visibility and
// execution permission alone never grant derivative edit/export permission.
func TeamGitWritebackPolicy(payload TeamReleasePayload) error {
	check := func(v any) error {
		if err := CheckDistribution(v, "edit"); err != nil {
			return err
		}
		return CheckDistribution(v, "export")
	}
	for _, v := range payload.Agents {
		if err := check(v); err != nil {
			return err
		}
	}
	for _, v := range payload.Assets.Rules {
		if err := check(v); err != nil {
			return err
		}
	}
	for _, v := range payload.Assets.Skills {
		if err := check(v); err != nil {
			return err
		}
	}
	for _, v := range payload.Assets.MCPServers {
		if err := check(v); err != nil {
			return err
		}
	}
	return nil
}
