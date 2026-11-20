package tracker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

type githubObjectRef struct {
	Ref    string `json:"ref"`
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

func (g *githubClient) contributionRef(ctx context.Context, branch string) (string, bool, error) {
	var ref githubObjectRef
	err := g.requestContext(ctx, http.MethodGet, "/git/ref/heads/"+url.PathEscape(branch), nil, &ref)
	var status *githubHTTPStatusError
	if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if ref.Ref != "refs/heads/"+branch || ref.Object.Type != "commit" || !domain.ValidWorkspaceBaseSHA(ref.Object.SHA) {
		return "", false, fmt.Errorf("invalid GitHub contribution ref identity")
	}
	return ref.Object.SHA, true, nil
}

// PublishContributionObjects transports verified immutable objects over GitHub's
// Git Data API when Git smart HTTP is unavailable. It only creates the stable
// branch; it never changes an existing ref or enables merge authority.
func (s *Service) PublishContributionObjects(ctx context.Context, project, checkout, branch, target, baseSHA, resultSHA string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !strings.HasPrefix(project, "github:") || !strings.HasPrefix(branch, "superagent/integration/") || branch == "superagent/integration/" || !validPublicationBranch(branch) || !validPublicationBranch(target) || branch == target || !domain.ValidWorkspaceBaseSHA(baseSHA) || !domain.ValidWorkspaceBaseSHA(resultSHA) {
		return fmt.Errorf("invalid GitHub contribution object publication request")
	}
	g, err := s.githubPublicationClient(ctx, project)
	if err != nil {
		return fmt.Errorf("invalid GitHub publication repository")
	}
	targetSHA, exists, err := g.contributionRef(ctx, target)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("GitHub publication target does not exist")
	}
	var comparison struct {
		Status    string `json:"status"`
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := g.requestContext(ctx, http.MethodGet, "/compare/"+baseSHA+"..."+targetSHA, nil, &comparison); err != nil {
		return err
	}
	if (comparison.Status != "ahead" && comparison.Status != "identical") || comparison.MergeBase.SHA != baseSHA {
		return fmt.Errorf("GitHub target does not descend from the frozen contribution base")
	}
	current, exists, err := g.contributionRef(ctx, branch)
	if err != nil {
		return err
	}
	if exists {
		if current != resultSHA {
			return fmt.Errorf("GitHub immutable contribution branch already exists at another SHA")
		}
		return nil
	}
	objects, err := gitwork.GitHubContributionObjects(ctx, checkout, baseSHA, resultSHA)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("could not serialize verified GitHub contribution objects")
	}
	resultPresent := false
	for _, object := range objects {
		if !domain.ValidWorkspaceBaseSHA(object.SHA) || (object.Type != "blob" && object.Type != "tree" && object.Type != "commit") || (object.Type == "commit" && object.Commit == nil) {
			return fmt.Errorf("invalid GitHub contribution object graph")
		}
		if object.SHA == resultSHA && object.Type == "commit" {
			resultPresent = true
		}
	}
	if !resultPresent {
		return fmt.Errorf("GitHub contribution result commit is absent")
	}
	for _, object := range objects {
		var input any
		switch object.Type {
		case "blob":
			input = map[string]any{"content": base64.StdEncoding.EncodeToString(object.Content), "encoding": "base64"}
		case "tree":
			entries := make([]map[string]string, 0, len(object.Entries))
			for _, entry := range object.Entries {
				entries = append(entries, map[string]string{"path": entry.Path, "mode": entry.Mode, "type": entry.Type, "sha": entry.SHA})
			}
			input = map[string]any{"tree": entries}
		case "commit":
			commit := object.Commit
			input = map[string]any{"message": commit.Message, "tree": commit.Tree, "parents": commit.Parents,
				"author":    map[string]string{"name": commit.Author.Name, "email": commit.Author.Email, "date": commit.Author.Date},
				"committer": map[string]string{"name": commit.Committer.Name, "email": commit.Committer.Email, "date": commit.Committer.Date}}
		}
		var created struct {
			SHA string `json:"sha"`
		}
		if err := g.requestContext(ctx, http.MethodPost, "/git/"+object.Type+"s", input, &created); err != nil {
			return err
		}
		if created.SHA != object.SHA {
			return fmt.Errorf("GitHub contribution object SHA mismatch")
		}
	}
	var created githubObjectRef
	_ = g.requestContext(ctx, http.MethodPost, "/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": resultSHA}, &created)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Creation can commit before an HTTP failure or malformed success response.
	// One GET reconciles every outcome; no second POST and no branch update.
	current, exists, lookupErr := g.contributionRef(ctx, branch)
	if lookupErr != nil {
		return lookupErr
	}
	if !exists || current != resultSHA {
		return fmt.Errorf("GitHub immutable contribution branch creation could not be confirmed")
	}
	return nil
}
