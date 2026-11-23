package gitwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IntegrateContributionBundles imports independent accepted snapshots into a
// fresh controller checkout and deterministically merges their commits. The
// caller authenticates acceptance/repository/context before invoking it. No
// remotes, uploaded Git config or Worker filesystem paths are consulted.
func IntegrateContributionBundles(ctx context.Context, bundles []ContributionBundle, destination, bundlePath string, maxExpanded int64) (ContributionBundle, error) {
	var result ContributionBundle
	if len(bundles) == 0 || len(bundles) > 32 {
		return result, errors.New("integration requires 1..32 accepted contributions")
	}
	base := bundles[0].BaseSHA
	for _, b := range bundles {
		if b.BaseSHA != base {
			return result, errors.New("contribution integration baselines differ")
		}
	}
	if err := VerifyContribution(ctx, bundles[0].Path, destination, bundles[0], maxExpanded); err != nil {
		return result, err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(destination)
		}
	}()
	home, err := os.MkdirTemp("", "agentflow-integration-home-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(home)
	run := func(args ...string) (string, error) {
		cmd := contributionGit(ctx, destination, home, "", args...)
		// A retry of the same frozen ordered inputs produces the same merge SHA.
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("integrate contribution %s: %w: %.2048s", args[0], err, output)
		}
		return strings.TrimSpace(string(output)), nil
	}
	current := bundles[0].ResultSHA
	for i, b := range bundles[1:] {
		verified := filepath.Join(home, fmt.Sprintf("input-%d", i+1))
		if err := VerifyContribution(ctx, b.Path, verified, b, maxExpanded); err != nil {
			return result, err
		}
		if _, err := run("bundle", "unbundle", b.Path); err != nil {
			return result, err
		}
		if b.ResultSHA == current {
			continue
		}
		if _, err := run("merge-base", "--is-ancestor", b.ResultSHA, current); err == nil {
			continue
		}
		if _, err := run("merge-base", "--is-ancestor", current, b.ResultSHA); err == nil {
			current = b.ResultSHA
			continue
		}
		merged, err := run("merge-tree", "--write-tree", current, b.ResultSHA)
		if err != nil {
			return result, fmt.Errorf("contribution integration conflict: %w", err)
		}
		tree := strings.Split(merged, "\n")[0]
		if !fullCommitSHA(tree) {
			return result, errors.New("integration did not produce an immutable tree")
		}
		current, err = run("commit-tree", tree, "-p", current, "-p", b.ResultSHA, "-m", "SuperAgent accepted contribution integration")
		if err != nil {
			return result, err
		}
	}
	if _, err := run("checkout", "-q", "--detach", current); err != nil {
		return result, err
	}
	result, err = ExportContribution(ctx, destination, bundlePath, base, "SuperAgent accepted contribution integration", 16<<20)
	if err != nil {
		return result, err
	}
	// Reconstructing the aggregate once more checks its combined object budget,
	// not just the budget of each input.
	if err = VerifyContribution(ctx, bundlePath, filepath.Join(home, "aggregate-check"), result, maxExpanded); err != nil {
		os.Remove(bundlePath)
		return ContributionBundle{}, err
	}
	success = true
	return result, nil
}
