package teamconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// ResolveRegisteredBranch observes only the registered heads ref. No checkout,
// upstream hooks, remote HEAD, arbitrary ref or client-supplied URL is used.
func ResolveRegisteredBranch(ctx context.Context, repository domain.Repository) (string, string, error) {
	ref := "refs/heads/" + repository.DefaultBranch
	if !domain.ValidTeamImportRef(ref) {
		return "", "", fmt.Errorf("registered defaultBranch must be a valid heads branch")
	}
	if err := ValidateURL(repository.CloneURL, os.Getenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT"), os.Getenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE") == "1"); err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "agentflow-team-ref-")
	if err != nil {
		return "", "", ErrSource
	}
	defer os.RemoveAll(dir)
	if _, err = runGit(ctx, dir, 128, "check-ref-format", ref); err != nil {
		if ctx.Err() != nil {
			return "", "", ErrSource
		}
		return "", "", fmt.Errorf("registered defaultBranch must be a valid heads branch")
	}
	out, err := runGit(ctx, dir, 4096, "ls-remote", "--refs", "--exit-code", "--", repository.CloneURL, ref)
	if err != nil {
		var failure gitCommandError
		if errors.As(err, &failure) && failure.exitCode == 2 {
			return "", "", ErrRefConflict
		}
		return "", "", ErrSource
	}
	commit, err := parseRegisteredRef(out, ref)
	return ref, commit, err
}

func parseRegisteredRef(out []byte, ref string) (string, error) {
	// Exactly one record, not a suffix/pattern match or peeled tag.
	line := strings.TrimSuffix(string(out), "\n")
	sha, name, ok := strings.Cut(line, "\t")
	if !ok || name != ref || !domain.ValidGitCommitSHA(sha) {
		return "", ErrSource
	}
	return sha, nil
}

type FileChange struct {
	Path   string                 `json:"path"`
	Kind   string                 `json:"kind"`
	Before *domain.TeamImportFile `json:"before"`
	After  *domain.TeamImportFile `json:"after"`
}

// Changes is a bounded source manifest comparison; full payload review remains
// mandatory, even if a source commit changes only unselected repository files.
func Changes(before, after []domain.TeamImportFile) []FileChange {
	old, next := map[string]domain.TeamImportFile{}, map[string]domain.TeamImportFile{}
	for _, f := range before {
		old[f.Path] = f
	}
	for _, f := range after {
		next[f.Path] = f
	}
	paths := map[string]bool{}
	for p := range old {
		paths[p] = true
	}
	for p := range next {
		paths[p] = true
	}
	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	changes := []FileChange{}
	for _, p := range ordered {
		a, aOK := old[p]
		b, bOK := next[p]
		if aOK && bOK && a == b {
			continue
		}
		change := FileChange{Path: p, Kind: "changed"}
		if aOK {
			change.Before = &a
		} else {
			change.Kind = "added"
		}
		if bOK {
			change.After = &b
		} else {
			change.Kind = "removed"
		}
		changes = append(changes, change)
	}
	return changes
}
