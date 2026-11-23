package teamconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

type PreparedProposal struct {
	Export                                                            ExportPreview
	ResultSHA, ResultTreeSHA, ResultConfigurationSHA256, BundleSHA256 string
	Bundle                                                            []byte
	Files                                                             []domain.TeamGitProposalFile
}

// PrepareProposal replaces selected entries in the complete source tree. It
// executes no source hooks/tools and never writes a source or remote ref.
func PrepareProposal(ctx context.Context, repository domain.Repository, release domain.TeamRelease, changes []Replacement, at time.Time) (PreparedProposal, error) {
	var out PreparedProposal
	preview, err := Export(ctx, repository, release, changes)
	if err != nil {
		return out, err
	}
	m := release.Payload.GitImport
	files, err := Read(ctx, repository, m.Commit, m.Root, m.Selection)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "agentflow-team-proposal-")
	if err != nil {
		return out, ErrSource
	}
	defer os.RemoveAll(dir)
	run := func(args ...string) (string, error) {
		raw, err := runGit(ctx, dir, 1<<20, args...)
		return strings.TrimSpace(string(raw)), err
	}
	if _, err = run("init", "--bare", "--template=", "."); err != nil {
		return out, err
	}
	// A complete bounded bundle is retained, rather than depending on future
	// repository access or lying about shallow/partial contribution provenance.
	if _, err = run("fetch", "--no-tags", "--no-recurse-submodules", "--", repository.CloneURL, m.Commit); err != nil {
		return out, err
	}
	if _, err = run("read-tree", m.Commit); err != nil {
		return out, err
	}
	updated := make(map[string]File, len(files))
	for p, f := range files {
		updated[p] = f
	}
	expected := map[string]domain.TeamImportFile{}
	for _, f := range preview.Proposed.Payload.GitImport.Files {
		expected[f.Path] = f
	}
	paths := []string{}
	for _, change := range changes {
		before, after := files[change.Path], expected[change.Path]
		if before.Mode == after.Mode && bytes.Equal(before.Content, change.Content) {
			continue
		}
		target := change.Path
		if m.Root != "." {
			target = path.Join(m.Root, target)
		}
		blob, err := runGitInput(ctx, dir, 128, change.Content, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return out, err
		}
		if strings.TrimSpace(string(blob)) != after.BlobSHA {
			return out, fmt.Errorf("proposal resource identity changed")
		}
		if _, err = run("update-index", "--add", "--cacheinfo", after.Mode, after.BlobSHA, target); err != nil {
			return out, err
		}
		updated[change.Path] = File{TeamImportFile: after, Content: change.Content}
		out.Files = append(out.Files, domain.TeamGitProposalFile{Path: change.Path, Before: before.TeamImportFile, After: after})
		paths = append(paths, target)
	}
	tree, err := run("write-tree")
	if err != nil {
		return out, err
	}
	if !domain.ValidGitCommitSHA(tree) || at.Unix() < 0 {
		return out, fmt.Errorf("invalid prepared tree or timestamp")
	}
	commit := fmt.Sprintf("tree %s\nparent %s\nauthor SuperAgent <superagent@example.invalid> %d +0000\ncommitter SuperAgent <superagent@example.invalid> %d +0000\n\nReviewed TeamAI configuration proposal\n", tree, m.Commit, at.Unix(), at.Unix())
	raw, err := runGitInput(ctx, dir, 128, []byte(commit), "hash-object", "-w", "-t", "commit", "--stdin")
	if err != nil {
		return out, err
	}
	result := strings.TrimSpace(string(raw))
	if !domain.ValidGitCommitSHA(result) {
		return out, ErrSource
	}
	diff, err := runGit(ctx, dir, 1<<20, "diff-tree", "--no-commit-id", "-r", "--name-only", "-z", m.Commit, result)
	if err != nil {
		return out, err
	}
	got := strings.Split(strings.TrimSuffix(string(diff), "\x00"), "\x00")
	sort.Strings(got)
	sort.Strings(paths)
	if len(got) != len(paths) {
		return out, fmt.Errorf("proposal changes unexpected paths")
	}
	for i := range paths {
		if got[i] != paths[i] {
			return out, fmt.Errorf("proposal changes unexpected path")
		}
	}
	parsed, err := Parse(release.ProjectID, repository, result, m.Root, m.Selection, updated, m.Ref)
	if err != nil {
		return out, err
	}
	if err = preserveReleaseDistribution(release.Payload, &parsed); err != nil {
		return out, err
	}
	if _, err = run("update-ref", domain.RunnerContributionRef, result); err != nil {
		return out, err
	}
	bundlePath := filepath.Join(dir, "proposal.bundle")
	if _, err = run("bundle", "create", "--version=2", bundlePath, domain.RunnerContributionRef); err != nil {
		return out, err
	}
	info, err := os.Stat(bundlePath)
	if err != nil || info.Size() <= 0 || info.Size() > domain.RunnerContributionMaxBytes {
		return out, fmt.Errorf("complete proposal exceeds retained bundle limit")
	}
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		return out, ErrSource
	}
	digest := sha256.Sum256(bundle)
	metadata := gitwork.ContributionBundle{BaseSHA: m.Commit, ResultSHA: result, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(bundle))}
	// Reuse actual byte/object/bounds verification. Unsupported complete trees
	// fail before publication; source attributes never authorize dropped bytes.
	if err := gitwork.VerifyContribution(ctx, bundlePath, filepath.Join(dir, "verified"), metadata, domain.RunnerContributionMaxExpandedBytes); err != nil {
		return out, fmt.Errorf("prepared proposal cannot be verified: %w", err)
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	out.Export = preview
	out.ResultSHA = result
	out.ResultTreeSHA = tree
	out.ResultConfigurationSHA256 = parsed.SHA256
	out.Bundle = bundle
	out.BundleSHA256 = metadata.SHA256
	return out, nil
}
