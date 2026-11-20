package gitwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProgressTreeSHA computes the product tree with a private index. It neither
// commits nor changes HEAD/the user's index. Control reports and credentials
// are excluded using the same policy as accepted contributions.
func ProgressTreeSHA(ctx context.Context, dir string) (string, error) {
	if err := validateContributionRepository(dir); err != nil {
		return "", err
	}
	home, err := os.MkdirTemp("", "agentflow-progress-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(home)
	index := filepath.Join(home, "index")
	if _, err = contributionRun(ctx, dir, home, index, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	files, err := contributionRun(ctx, dir, home, index, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	var eligible, excluded []string
	for _, p := range strings.Split(files, "\x00") {
		if p != "" && !Excluded(p) {
			eligible = append(eligible, p)
		} else if p != "" {
			excluded = append(excluded, p)
		}
		if len(eligible)+len(excluded) > 100_000 {
			return "", errors.New("progress tree exceeds file-count limit")
		}
	}
	if len(excluded) > 0 {
		cmd := contributionGit(ctx, dir, home, index, "update-index", "--force-remove", "-z", "--stdin")
		cmd.Stdin = strings.NewReader(strings.Join(excluded, "\x00") + "\x00")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("exclude control paths from progress: %w: %.512s", err, out)
		}
	}
	if len(eligible) > 0 {
		cmd := contributionGit(ctx, dir, home, index, "--literal-pathspecs", "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul")
		cmd.Stdin = strings.NewReader(strings.Join(eligible, "\x00") + "\x00")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("stage progress tree: %w: %.512s", err, out)
		}
	}
	return contributionRun(ctx, dir, home, index, "write-tree")
}

// VerifiedProgressTreeSHA reads a fresh controller-verified bundle checkout,
// so a reported commit ID or patch filename inventory cannot fake progress.
func VerifiedProgressTreeSHA(ctx context.Context, dir string) (string, error) {
	home, err := os.MkdirTemp("", "agentflow-progress-read-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(home)
	return contributionRun(ctx, dir, home, "", "rev-parse", "HEAD^{tree}")
}
