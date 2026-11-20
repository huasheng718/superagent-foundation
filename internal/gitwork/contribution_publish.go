package gitwork

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"strings"
)

// PublishContributionBranch publishes an already reconstructed, verified SHA
// using controller-only authority. It can create the deterministic branch or
// observe the identical SHA; it never overwrites an existing different head.
func PublishContributionBranch(ctx context.Context, checkout, remote, branch, sha, baseSHA, target, token string) error {
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, ".git") || strings.Count(strings.Trim(u.Path, "/"), "/") != 1 || token == "" {
		return errors.New("publication requires an explicit GitHub HTTPS repository and controller credential")
	}
	return publishContributionBranch(ctx, checkout, remote, branch, sha, baseSHA, target, token)
}

func publishContributionBranch(ctx context.Context, checkout, remote, branch, sha, baseSHA, target, token string) (publishErr error) {
	defer func() {
		if ctx.Err() != nil {
			publishErr = ctx.Err()
		}
	}()
	const prefix = "superagent/integration/"
	if !fullCommitSHA(sha) || !fullCommitSHA(baseSHA) || !strings.HasPrefix(branch, prefix) || len(strings.TrimPrefix(branch, prefix)) != 64 || target == "" || strings.HasPrefix(target, "-") {
		return errors.New("invalid immutable publication branch or SHA")
	}
	for _, r := range strings.TrimPrefix(branch, prefix) {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return errors.New("invalid publication identity")
		}
	}
	home, err := os.MkdirTemp("", "agentflow-publication-home-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	run := func(args ...string) (string, error) {
		cmd := contributionGit(ctx, checkout, home, "", append([]string{"-c", "protocol.https.allow=always", "-c", "http.version=HTTP/1.1"}, args...)...)
		if token != "" {
			// A scoped in-memory header keeps the credential out of URLs, argv,
			// repository configuration, hooks and error output.
			cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
		}
		raw, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", errors.New("immutable Git publication command failed")
		}
		return strings.TrimSpace(string(raw)), nil
	}
	head, err := run("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || head != sha {
		return errors.New("publication checkout SHA mismatch")
	}
	if dirty, err := run("status", "--porcelain", "--untracked-files=all"); err != nil || dirty != "" {
		return errors.New("publication checkout is not clean")
	}
	if _, err := run("check-ref-format", "refs/heads/"+target); err != nil {
		return errors.New("invalid publication target branch")
	}
	// Read-only mirrors are valid sources, but metadata alone cannot authorize
	// publishing an unrelated history into the configured hosting repository.
	// Read the actual target history before creating any remote branch.
	if _, err := run("fetch", "--no-tags", remote, "refs/heads/"+target); err != nil {
		return err
	}
	if _, err := run("merge-base", "--is-ancestor", baseSHA, "FETCH_HEAD"); err != nil {
		return errors.New("contribution base does not belong to the remote target history")
	}
	if _, err := run("merge-base", "--is-ancestor", baseSHA, sha); err != nil {
		return errors.New("publication result does not descend from its frozen base")
	}
	ref := "refs/heads/" + branch
	readHead := func() (string, error) {
		out, err := run("ls-remote", "--heads", remote, ref)
		if err != nil || out == "" {
			return "", err
		}
		parts := strings.Fields(out)
		if len(parts) != 2 || parts[1] != ref || !fullCommitSHA(parts[0]) {
			return "", errors.New("invalid publication remote response")
		}
		return parts[0], nil
	}
	existing, err := readHead()
	if err != nil {
		return err
	}
	if existing != "" {
		if existing != sha {
			return errors.New("publication branch already has a different SHA")
		}
		return nil
	}
	// The empty expected value is a create-only compare-and-swap. Even a
	// concurrent human update must not be overwritten (including fast-forward).
	_, pushErr := run("push", "--porcelain", "--force-with-lease="+ref+":", remote, sha+":"+ref)
	existing, err = readHead()
	if err != nil {
		return err
	}
	if existing == sha {
		return nil
	} // Reconcile an ambiguous push response.
	if pushErr != nil {
		return pushErr
	}
	return errors.New("published branch did not retain the validated SHA")
}
