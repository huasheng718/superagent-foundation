package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"agentflow/internal/domain"
)

type githubPublicationPull struct {
	Number    int             `json:"number"`
	URL       string          `json:"html_url"`
	State     string          `json:"state"`
	Merged    *bool           `json:"merged"`
	MergedAt  json.RawMessage `json:"merged_at"`
	AutoMerge json.RawMessage `json:"auto_merge"`
	Draft     *bool           `json:"draft"`
	Head      struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

func validPublicationBranch(branch string) bool {
	if branch == "" || len(branch) > 255 || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") || strings.Contains(branch, "//") || strings.ContainsAny(branch, "~^:?*[\\") || branch == "@" {
		return false
	}
	for _, part := range strings.Split(branch, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return !strings.ContainsFunc(branch, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func (g *githubClient) verifyPublicationBranch(ctx context.Context, branch, sha string) error {
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := g.requestContext(ctx, "GET", "/git/ref/heads/"+url.PathEscape(branch), nil, &ref); err != nil {
		return err
	}
	if ref.Ref != "refs/heads/"+branch || ref.Object.Type != "commit" || ref.Object.SHA != sha {
		return fmt.Errorf("GitHub publication branch does not match the fixed result SHA")
	}
	return nil
}

// findPublicationPull checks all PR states and all targets for this stable
// branch. A closed/merged/wrong-target history must never cause a new create.
func (g *githubClient) findPublicationPull(ctx context.Context, branch string) (githubPublicationPull, bool, error) {
	var found githubPublicationPull
	count := 0
	for page := 1; page <= 20; page++ {
		q := url.Values{"state": {"all"}, "head": {strings.Split(g.path, "/")[0] + ":" + branch}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		var list []githubPublicationPull
		if err := g.requestContext(ctx, "GET", "/pulls?"+q.Encode(), nil, &list); err != nil {
			return found, false, err
		}
		if list == nil {
			return found, false, fmt.Errorf("invalid GitHub stable branch PR list")
		}
		for _, pull := range list {
			if pull.Head.Ref == "" {
				return found, false, fmt.Errorf("incomplete GitHub stable branch PR identity")
			}
			if pull.Head.Ref != branch {
				continue
			}
			if pull.Head.Repo.FullName != g.path || pull.Number < 1 {
				return found, false, fmt.Errorf("GitHub stable branch PR identity mismatch")
			}
			count++
			if count > 1 {
				return found, false, fmt.Errorf("GitHub stable branch has multiple PR records")
			}
			found = pull
		}
		if len(list) < 100 {
			return found, count == 1, nil
		}
	}
	return found, false, fmt.Errorf("GitHub stable branch PR search exceeded its page limit")
}

func (g *githubClient) publicationReceipt(ctx context.Context, number int, branch, target, sha string) (domain.ContributionPRReceipt, error) {
	var pull githubPublicationPull
	if number < 1 {
		return domain.ContributionPRReceipt{}, fmt.Errorf("invalid GitHub PR identity")
	}
	if err := g.requestContext(ctx, "GET", "/pulls/"+strconv.Itoa(number), nil, &pull); err != nil {
		return domain.ContributionPRReceipt{}, err
	}
	canonicalURL := "https://github.com/" + g.path + "/pull/" + strconv.Itoa(number)
	if pull.Number != number || pull.URL != canonicalURL || pull.State != "open" || pull.Merged == nil || *pull.Merged || !bytes.Equal(bytes.TrimSpace(pull.MergedAt), []byte("null")) || !bytes.Equal(bytes.TrimSpace(pull.AutoMerge), []byte("null")) || pull.Head.Ref != branch || pull.Head.SHA != sha || pull.Head.Repo.FullName != g.path || pull.Base.Ref != target || pull.Base.Repo.FullName != g.path {
		return domain.ContributionPRReceipt{}, fmt.Errorf("GitHub PR is not an open, unmerged fixed-SHA human-review delivery")
	}
	if err := g.verifyPublicationBranch(ctx, branch, sha); err != nil {
		return domain.ContributionPRReceipt{}, err
	}
	return domain.ContributionPRReceipt{URL: canonicalURL, Number: number, HeadSHA: sha, Draft: pull.Draft != nil && *pull.Draft}, nil
}

// EnsureOpenBranchPRAtSHA is create-only. It never updates a branch/PR, enables
// auto-merge, or calls a merge endpoint. Caller owns the durable outbox and
// existing repository integration lock; provider discovery reconciles uncertain
// POST outcomes using the same stable branch.
func (s *Service) EnsureOpenBranchPRAtSHA(ctx context.Context, project, branch, target, resultSHA, title, body string) (domain.ContributionPRReceipt, error) {
	return s.openBranchPRAtSHA(ctx, project, branch, target, resultSHA, title, body, true)
}

// FindOpenBranchPRAtSHA reconciles an uncertain durable create phase. A missing
// or temporarily invisible PR never authorizes a second POST or replacement.
func (s *Service) FindOpenBranchPRAtSHA(ctx context.Context, project, branch, target, resultSHA, title, body string) (domain.ContributionPRReceipt, error) {
	return s.openBranchPRAtSHA(ctx, project, branch, target, resultSHA, title, body, false)
}

func (s *Service) openBranchPRAtSHA(ctx context.Context, project, branch, target, resultSHA, title, body string, allowCreate bool) (domain.ContributionPRReceipt, error) {
	if err := ctx.Err(); err != nil {
		return domain.ContributionPRReceipt{}, err
	}
	if !strings.HasPrefix(project, "github:") || !validPublicationBranch(branch) || !validPublicationBranch(target) || branch == target || !domain.ValidWorkspaceBaseSHA(resultSHA) {
		return domain.ContributionPRReceipt{}, fmt.Errorf("invalid GitHub fixed-SHA publication request")
	}
	g, err := s.githubPublicationClient(ctx, project)
	if err != nil {
		return domain.ContributionPRReceipt{}, fmt.Errorf("invalid GitHub publication repository")
	}
	if err := g.verifyPublicationBranch(ctx, branch, resultSHA); err != nil {
		return domain.ContributionPRReceipt{}, err
	}
	pull, found, err := g.findPublicationPull(ctx, branch)
	if err != nil {
		return domain.ContributionPRReceipt{}, err
	}
	if !found {
		if !allowCreate {
			return domain.ContributionPRReceipt{}, fmt.Errorf("GitHub stable branch PR is not visible; uncertain creation remains reconcile-only")
		}
		if strings.TrimSpace(title) == "" {
			title = "SuperAgent: " + branch + " → " + target
		}
		var created githubPublicationPull
		createErr := g.requestContext(ctx, "POST", "/pulls", map[string]any{"head": branch, "base": target, "title": title, "body": body, "draft": true}, &created)
		if createErr == nil && created.Number > 0 {
			pull = created
		} else {
			if ctx.Err() != nil {
				return domain.ContributionPRReceipt{}, ctx.Err()
			}
			// Even a malformed success body or network/HTTP error can follow a
			// successful provider commit. Reconcile once; never issue a second POST.
			var lookupErr error
			pull, found, lookupErr = g.findPublicationPull(ctx, branch)
			if lookupErr != nil {
				return domain.ContributionPRReceipt{}, lookupErr
			}
			if !found {
				return domain.ContributionPRReceipt{}, fmt.Errorf("GitHub PR creation could not be confirmed")
			}
		}
	}
	return g.publicationReceipt(ctx, pull.Number, branch, target, resultSHA)
}
