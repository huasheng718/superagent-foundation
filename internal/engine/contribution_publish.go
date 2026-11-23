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
)

// PublishContribution creates one reviewable PR over controller-verified
// immutable code. No merge callback, Runner token or Issue transition is used.
func (e *Engine) PublishContribution(ctx context.Context, integrationID string) (domain.ContributionPublication, error) {
	var empty domain.ContributionPublication
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	value, ok := e.repo.ContributionIntegration(integrationID)
	if !ok {
		return empty, errors.New("integration not found")
	}
	unlock, err := e.repo.LockIntegration(ctx, "contribution:"+value.RepositoryID, 30*time.Second)
	if err != nil {
		return empty, err
	}
	defer unlock()
	publication, ok := e.repo.ContributionPublication(integrationID)
	if !ok {
		return empty, errors.New("publication gates not recorded")
	}
	if err := e.validateContributionPublication(value, publication); err != nil {
		return empty, err
	}
	repository, ok := e.repo.Repository(value.RepositoryID)
	controller, found := e.repo.RunState(value.RunID)
	if !ok || !found {
		return empty, errors.New("publication source unavailable")
	}
	frozen, err := contributionFrozenRepository(controller, value.RepositoryID)
	if err != nil {
		return empty, err
	}
	project := repository.GongfengProjectID
	if repository.Provider != "github" || frozen.Provider != "github" || frozen.GongfengProjectID != project || frozen.CloneURL != repository.CloneURL || frozen.DefaultBranch != publication.TargetBranch || !strings.HasPrefix(project, "github:") {
		return empty, errors.New("publication repository identity or default target differs from frozen source")
	}
	remote := "https://github.com/" + strings.TrimPrefix(project, "github:") + ".git"
	info, valid := domain.ParseRepositoryURL(remote)
	if !valid || info.Project != project {
		return empty, errors.New("invalid publication GitHub repository")
	}
	if e.CreateContributionPR == nil && e.DiscoverContributionPR == nil {
		return empty, errors.New("create-only PR adapter is not configured")
	}
	projectRecord, ok := e.repo.Project(repository.ProjectID)
	if !ok || projectRecord.OwnerID <= 0 {
		return empty, errors.New("publication requires a project owner")
	}
	owner, ok := e.repo.User(projectRecord.OwnerID)
	if !ok || owner.GitToken == "" {
		return empty, errors.New("controller hosting credential is unavailable")
	}
	title := "SuperAgent validation: " + value.ResultSHA[:12]
	body := fmt.Sprintf("Controller-verified contribution integration.\n\nResult SHA: `%s`\nContext SHA256: `%s`\nManifest SHA256: `%s`\n\nFast, Slow and independent review evidence are bound to this exact SHA. This draft awaits human review and merge; execution success does not complete a business work item.\n", value.ResultSHA, value.ContextSHA256, value.ManifestSHA256)
	for _, gate := range publication.Gates {
		body += fmt.Sprintf("\n- %s: `%s` (evidence SHA256 `%s`)\n", gate.Kind, gate.EvidenceRef, gate.EvidenceSHA256)
	}
	// A completed receipt is historical. Replays return that same receipt and
	// never rediscover/create a replacement PR after human closure or merging.
	// Live hosting state can be read separately without granting create authority.
	if publication.State == "pr_ready" {
		return publication, nil
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	claimed, err := e.repo.ClaimContributionPublication(value.ID, e.instanceID, 2*time.Minute, time.Now().UTC())
	if err != nil {
		return empty, err
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, ok := e.repo.ContributionPublication(value.ID)
		if !ok || current.State != "publishing" || current.LeaseOwner != claimed.LeaseOwner || current.Attempt != claimed.Attempt || current.LeaseUntil == nil || !current.LeaseUntil.After(time.Now().UTC()) {
			return errors.New("publication ownership lost")
		}
		return e.validateContributionPublication(value, current)
	}
	dir, err := os.MkdirTemp("", "agentflow-controlled-publication-")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(dir)
	raw, size, ok := e.repo.RunnerContributionBlob(value.BundleSHA256)
	if !ok || size != value.BundleSize {
		return empty, errors.New("publication bytes unavailable")
	}
	path := filepath.Join(dir, "aggregate.bundle")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return empty, err
	}
	checkout := filepath.Join(dir, "checkout")
	if err := gitwork.VerifyContribution(ctx, path, checkout, gitwork.ContributionBundle{BaseSHA: value.BaseSHA, ResultSHA: value.ResultSHA, SHA256: value.BundleSHA256, Size: value.BundleSize}, 64<<20); err != nil {
		return empty, err
	}
	if err := check(); err != nil {
		return empty, err
	}
	var publishErr error
	if e.PublishContributionObjects != nil {
		publishErr = e.PublishContributionObjects(ctx, project, checkout, claimed.Branch, claimed.TargetBranch, value.BaseSHA, value.ResultSHA)
	} else {
		publishErr = gitwork.PublishContributionBranch(ctx, checkout, remote, claimed.Branch, value.ResultSHA, value.BaseSHA, claimed.TargetBranch, owner.GitToken)
	}
	if publishErr != nil {
		return empty, publishErr
	}
	if err := check(); err != nil {
		return empty, err
	}
	prAdapter := e.DiscoverContributionPR
	if !claimed.PRCreateAttempted {
		if e.CreateContributionPR == nil {
			return empty, errors.New("create-only PR adapter is not configured")
		}
		// The durable phase is the authorization linearization point for one
		// automatic create attempt. Crashing after it but before POST sacrifices
		// availability: reclaim may discover, never authorize another creation.
		claimed, err = e.repo.MarkContributionPRCreateAttempted(value.ID, claimed.LeaseOwner, claimed.Attempt, time.Now().UTC())
		if err != nil {
			return empty, err
		}
		if err := check(); err != nil {
			return empty, err
		}
		prAdapter = e.CreateContributionPR
	}
	if prAdapter == nil {
		return empty, errors.New("uncertain PR creation requires a discover-only adapter; automatic create retry is disabled")
	}
	receipt, err := prAdapter(ctx, project, claimed.Branch, claimed.TargetBranch, value.ResultSHA, title, body)
	if err != nil {
		return empty, err
	}
	if receipt.HeadSHA != value.ResultSHA {
		return empty, errors.New("PR head differs from gated SHA")
	}
	if err := check(); err != nil {
		return empty, err
	}
	return e.repo.CompleteContributionPublication(value.ID, claimed.LeaseOwner, claimed.Attempt, receipt.URL, receipt.Number, time.Now().UTC())
}
