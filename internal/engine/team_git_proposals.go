package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
	"agentflow/internal/teamconfig"
)

var ErrTeamGitSourceStale = errors.New("registered TeamAI target no longer equals the reviewed source SHA")

func (e *Engine) teamGitContext(ctx context.Context, projectID string) (context.Context, error) {
	p, ok := e.repo.Project(projectID)
	if !ok {
		return ctx, repo.ErrIntelligenceUnavailable
	}
	if p.OwnerID > 0 {
		ctx = gitwork.WithToken(ctx, e.repo.GitCredential(ctx, p.OwnerID))
	}
	return ctx, nil
}

func (e *Engine) ObserveTeamGitProposalTarget(ctx context.Context, v domain.TeamGitProposal) error {
	if err := e.repo.ValidateTeamGitProposalSource(v); err != nil {
		return err
	}
	r, ok := e.repo.Repository(v.RepositoryID)
	if !ok {
		return repo.ErrIntelligenceUnavailable
	}
	ctx, err := e.teamGitContext(ctx, v.ProjectID)
	if err != nil {
		return err
	}
	ref, sha, err := teamconfig.ResolveRegisteredBranch(ctx, r)
	if err != nil {
		return err
	}
	if ref != v.TargetRef || sha != v.BaseCommit {
		return ErrTeamGitSourceStale
	}
	return nil
}

func (e *Engine) PrepareTeamGitProposal(ctx context.Context, projectID, releaseID, releaseSHA, base, patchSHA, actor string, changes []teamconfig.Replacement) (domain.TeamGitProposal, error) {
	var v domain.TeamGitProposal
	if actor == "" {
		return v, repo.ErrTeamGitProposalInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	release, err := e.repo.TeamRelease(projectID, releaseID)
	if err != nil {
		return v, err
	}
	if release.State != domain.TeamReleasePublished || release.SHA256 != releaseSHA || release.Payload.GitImport == nil || release.Payload.GitImport.Commit != base {
		return v, repo.ErrTeamGitProposalConflict
	}
	r, ok := e.repo.Repository(release.Payload.GitImport.RepositoryID)
	if !ok || r.ProjectID != projectID || r.Provider != "github" || !strings.HasPrefix(r.GongfengProjectID, "github:") {
		return v, repo.ErrTeamGitProposalInvalid
	}
	info, valid := domain.ParseRepositoryURL("https://github.com/" + strings.TrimPrefix(r.GongfengProjectID, "github:") + ".git")
	if !valid || info.Project != r.GongfengProjectID {
		return v, repo.ErrTeamGitProposalInvalid
	}
	if r.CloneURL != info.CloneURL && r.CloneURL != info.WebURL {
		// Local transport exists only behind the existing operator fixture-root
		// opt-in. A read mirror cannot stand in for the actual hosted target.
		if !strings.HasPrefix(r.CloneURL, "file://") || teamconfig.ValidateURL(r.CloneURL, os.Getenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT"), os.Getenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE") == "1") != nil {
			return v, repo.ErrTeamGitProposalInvalid
		}
	}
	v = domain.TeamGitProposal{ProjectID: projectID, RepositoryID: r.ID, RepositoryURL: r.CloneURL, ProviderProject: r.GongfengProjectID, ReleaseID: release.ID, ReleaseSHA256: release.SHA256, ReleaseRevision: release.Revision, TargetRef: "refs/heads/" + r.DefaultBranch, BaseCommit: base, CreatedBy: actor}
	ctx, err = e.teamGitContext(ctx, projectID)
	if err != nil {
		return v, err
	}
	if err = e.ObserveTeamGitProposalTarget(ctx, v); err != nil {
		return v, err
	}
	preview, err := teamconfig.Export(ctx, r, release, changes)
	if err != nil {
		return v, err
	}
	if patchSHA != preview.PatchSHA256 {
		return v, repo.ErrTeamGitProposalConflict
	}
	v.PatchSHA256 = patchSHA
	v.ProposedSHA256 = preview.Proposed.SHA256
	v.ID = domain.TeamGitProposalID(v)
	if existing, err := e.repo.TeamGitProposal(projectID, v.ID); err == nil {
		return existing, nil
	} else if !errors.Is(err, repo.ErrIntelligenceUnavailable) {
		return v, err
	}
	v.CreatedAt = time.Now().UTC()
	v.UpdatedAt = v.CreatedAt
	prepared, err := teamconfig.PrepareProposal(ctx, r, release, changes, v.CreatedAt)
	if err != nil {
		return v, err
	}
	if prepared.Export.PatchSHA256 != patchSHA || prepared.Export.Proposed.SHA256 != v.ProposedSHA256 {
		return v, repo.ErrTeamGitProposalConflict
	}
	v.ResultSHA = prepared.ResultSHA
	v.ResultTreeSHA = prepared.ResultTreeSHA
	v.ResultConfigurationSHA256 = prepared.ResultConfigurationSHA256
	v.BundleSHA256 = prepared.BundleSHA256
	v.BundleSize = int64(len(prepared.Bundle))
	v.Files = prepared.Files
	v.Patch = prepared.Export.Patch
	v.Validation = domain.TeamGitProposalValidation{Policy: domain.TeamGitProposalPolicy, Checks: []string{"exact-active-formal-source", "frozen-edit-export-policy", "supported-configuration-parse", "license-notice-retention", "raw-resource-byte-and-mode-identity", "complete-tree-selected-path-only", "retained-git-bundle-verification"}}
	v.State = "pending_review"
	v.Revision = 1
	v.Decisions = []domain.IntelligenceDecision{}
	v.SHA256, err = domain.TeamGitProposalDigest(v)
	if err != nil {
		return v, err
	}
	if err = e.ObserveTeamGitProposalTarget(ctx, v); err != nil {
		return v, err
	}
	return e.repo.CreateTeamGitProposal(v, prepared.Bundle)
}

func (e *Engine) ValidateTeamGitProposal(v domain.TeamGitProposal, revision int, sha string) error {
	current, err := e.repo.TeamGitProposal(v.ProjectID, v.ID)
	if err != nil {
		return err
	}
	if current.State != "approved" || current.Revision != revision || current.SHA256 != sha || current.ID != domain.TeamGitProposalID(current) || current.Validation.Policy != domain.TeamGitProposalPolicy {
		return repo.ErrTeamGitProposalConflict
	}
	if len(current.Decisions) != 1 || current.Decisions[0].Decision != "approved" || current.Decisions[0].Actor == "" {
		return repo.ErrTeamGitProposalConflict
	}
	p, ok := e.repo.ContributionPublication(v.ID)
	if !ok || p.SourceKind != "teamai_proposal" || p.ProjectID != v.ProjectID || p.SourceSHA256 != v.SHA256 || p.RunID != "" || p.RepositoryID != v.RepositoryID || p.ManifestSHA256 != v.SHA256 || p.ResultSHA != v.ResultSHA || p.Branch != "superagent/integration/"+v.ID || p.TargetBranch != strings.TrimPrefix(v.TargetRef, "refs/heads/") || len(p.Gates) != 0 {
		return repo.ErrTeamGitProposalConflict
	}
	return e.repo.ValidateTeamGitProposalSource(current)
}

// PublishTeamGitProposal uses the existing durable publisher and adapters. Its
// narrow configuration validation policy never pretends to be Run three-gates.
func (e *Engine) PublishTeamGitProposal(ctx context.Context, projectID, id, sha, base string, revision int, reconcileOnly bool) (domain.ContributionPublication, error) {
	var empty domain.ContributionPublication
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	v, err := e.repo.TeamGitProposal(projectID, id)
	if err != nil {
		return empty, err
	}
	if v.BaseCommit != base {
		return empty, repo.ErrTeamGitProposalConflict
	}
	unlock, err := e.repo.LockIntegration(ctx, "contribution:"+v.RepositoryID, 30*time.Second)
	if err != nil {
		return empty, err
	}
	defer unlock()
	if err = e.ValidateTeamGitProposal(v, revision, sha); err != nil {
		return empty, err
	}
	p, ok := e.repo.ContributionPublication(id)
	if !ok {
		return empty, repo.ErrTeamGitProposalInvalid
	}
	if p.State == "pr_ready" {
		return p, nil
	}
	if reconcileOnly && !p.PRCreateAttempted {
		return empty, repo.ErrTeamGitProposalConflict
	}
	if e.CreateContributionPR == nil || e.DiscoverContributionPR == nil || e.PublishContributionObjects == nil {
		return empty, fmt.Errorf("controller publication adapters are unavailable")
	}
	project, ok := e.repo.Project(projectID)
	if !ok || project.OwnerID <= 0 {
		return empty, fmt.Errorf("controller hosting credential is unavailable")
	}
	owner, ok := e.repo.User(project.OwnerID)
	if !ok || owner.GitToken == "" {
		return empty, fmt.Errorf("controller hosting credential is unavailable")
	}
	ctx = gitwork.WithToken(ctx, owner.GitToken)
	if err = e.ObserveTeamGitProposalTarget(ctx, v); err != nil {
		return empty, err
	}
	claimed, err := e.repo.ClaimContributionPublication(id, e.instanceID, 2*time.Minute, time.Now().UTC())
	if err != nil {
		return empty, err
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, ok := e.repo.ContributionPublication(id)
		if !ok || current.State != "publishing" || current.LeaseOwner != claimed.LeaseOwner || current.Attempt != claimed.Attempt || current.LeaseUntil == nil || !current.LeaseUntil.After(time.Now().UTC()) {
			return repo.ErrContributionPublicationLeaseLost
		}
		return e.ValidateTeamGitProposal(v, revision, sha)
	}
	createAuthorized := false
	if !claimed.PRCreateAttempted {
		bundle, err := e.repo.TeamGitProposalBundle(projectID, id)
		if err != nil {
			return empty, err
		}
		dir, err := os.MkdirTemp("", "agentflow-team-publication-")
		if err != nil {
			return empty, err
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "proposal.bundle")
		if err = os.WriteFile(path, bundle, 0600); err != nil {
			return empty, err
		}
		checkout := filepath.Join(dir, "checkout")
		metadata := gitwork.ContributionBundle{BaseSHA: v.BaseCommit, ResultSHA: v.ResultSHA, SHA256: v.BundleSHA256, Size: v.BundleSize}
		if err = gitwork.VerifyContribution(ctx, path, checkout, metadata, domain.RunnerContributionMaxExpandedBytes); err != nil {
			return empty, err
		}
		if err = check(); err != nil {
			return empty, err
		}
		if err = e.ObserveTeamGitProposalTarget(ctx, v); err != nil {
			return empty, err
		}
		if err = e.PublishContributionObjects(ctx, v.ProviderProject, checkout, claimed.Branch, claimed.TargetBranch, v.BaseCommit, v.ResultSHA); err != nil {
			return empty, err
		}
		if err = check(); err != nil {
			return empty, err
		}
		if err = e.ObserveTeamGitProposalTarget(ctx, v); err != nil {
			return empty, err
		}
		claimed, err = e.repo.MarkContributionPRCreateAttempted(id, claimed.LeaseOwner, claimed.Attempt, time.Now().UTC())
		if err != nil {
			return empty, err
		}
		createAuthorized = true
		if err = check(); err != nil {
			return empty, err
		}
	}
	adapter := e.DiscoverContributionPR
	if createAuthorized && !reconcileOnly {
		adapter = e.CreateContributionPR
	}
	body := fmt.Sprintf("Human-approved TeamAI configuration proposal.\n\nProposal: `%s`\nSource release: `%s`\nBase: `%s`\nResult: `%s`\nPolicy: `%s`\n\nThis is configuration proposal validation, not Fast/Slow/independent Run-gate certification. Requires human hosted review and merge. No automatic formal-release publication.\n", v.SHA256, v.ReleaseSHA256, v.BaseCommit, v.ResultSHA, v.Validation.Policy)
	if err = check(); err != nil {
		return empty, err
	}
	receipt, err := adapter(ctx, v.ProviderProject, claimed.Branch, claimed.TargetBranch, v.ResultSHA, "TeamAI configuration proposal "+v.ResultSHA[:12], body)
	if err != nil {
		return empty, err
	}
	if receipt.HeadSHA != v.ResultSHA || !receipt.Draft {
		return empty, repo.ErrTeamGitProposalConflict
	}
	if err = check(); err != nil {
		return empty, err
	}
	if err = e.ObserveTeamGitProposalTarget(ctx, v); err != nil {
		return empty, err
	}
	return e.repo.CompleteTeamGitProposalPublication(projectID, id, sha, revision, claimed.LeaseOwner, claimed.Attempt, receipt.URL, receipt.Number, time.Now().UTC())
}
