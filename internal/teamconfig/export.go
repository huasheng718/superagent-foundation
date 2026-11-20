package teamconfig

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"agentflow/internal/domain"
)

type Replacement struct {
	Path       string `json:"path"`
	Content    []byte `json:"content"`
	Executable bool   `json:"executable"`
}

type ExportPreview struct {
	BaseCommit    string  `json:"baseCommit"`
	ReleaseSHA256 string  `json:"releaseSha256"`
	Patch         string  `json:"patch"`
	PatchSHA256   string  `json:"patchSha256"`
	Proposed      Preview `json:"proposed"`
}

// Export rereads exact approved source objects and produces an unapproved patch.
// It never writes the source repository, a branch, a release or a remote.
func Export(ctx context.Context, repository domain.Repository, release domain.TeamRelease, changes []Replacement) (ExportPreview, error) {
	var out ExportPreview
	if err := domain.TeamGitWritebackPolicy(release.Payload); err != nil {
		return out, err
	}
	m := release.Payload.GitImport
	if release.State != domain.TeamReleasePublished || m == nil || repository.ID != m.RepositoryID || repository.ProjectID != release.ProjectID || repository.CloneURL != release.Payload.Source.Repository || len(changes) == 0 || len(changes) > 1024 {
		return out, fmt.Errorf("active imported release and matching registered source required")
	}
	files, err := Read(ctx, repository, m.Commit, m.Root, m.Selection)
	if err != nil {
		return out, err
	}
	baseline, err := Parse(release.ProjectID, repository, m.Commit, m.Root, m.Selection, files, m.Ref)
	if err != nil {
		return out, err
	}
	if err = preserveReleaseDistribution(release.Payload, &baseline); err != nil {
		return out, err
	}
	if baseline.SHA256 != release.SHA256 {
		return out, fmt.Errorf("approved source manifest or payload differs from Git objects")
	}
	updated := make(map[string]File, len(files))
	for p, file := range files {
		updated[p] = file
	}
	seen := map[string]bool{}
	changed := false
	for _, replacement := range changes {
		original, exists := files[replacement.Path]
		if !exists || !domain.ValidTeamImportPath(replacement.Path) || seen[replacement.Path] || len(replacement.Content) > 1<<20 || replacement.Path == "teamai.yaml" || !selectedResourceFile(replacement.Path, m.Selection) {
			return out, fmt.Errorf("replacement must name one existing selected resource file within size limits")
		}
		seen[replacement.Path] = true
		mode := "100644"
		if replacement.Executable {
			mode = "100755"
		}
		changed = changed || mode != original.Mode || !bytes.Equal(replacement.Content, original.Content)
		digest := sha256.Sum256(replacement.Content)
		// Git's blob identity includes its type and byte length.
		blob := sha1.New()
		fmt.Fprintf(blob, "blob %d\x00", len(replacement.Content))
		blob.Write(replacement.Content)
		updated[replacement.Path] = File{TeamImportFile: domain.TeamImportFile{Path: replacement.Path, Mode: mode, BlobSHA: hex.EncodeToString(blob.Sum(nil)), SHA256: hex.EncodeToString(digest[:]), Size: int64(len(replacement.Content))}, Content: replacement.Content}
	}
	if !changed {
		return out, fmt.Errorf("replacement batch contains no changes")
	}
	proposed, err := Parse(release.ProjectID, repository, m.Commit, m.Root, m.Selection, updated, m.Ref)
	if err != nil {
		return out, err
	}
	if err = preserveReleaseDistribution(release.Payload, &proposed); err != nil {
		return out, err
	}
	patch, err := exportPatch(ctx, m.Root, files, updated)
	if err != nil {
		return out, err
	}
	digest := sha256.Sum256(patch)
	return ExportPreview{BaseCommit: m.Commit, ReleaseSHA256: release.SHA256, Patch: string(patch), PatchSHA256: hex.EncodeToString(digest[:]), Proposed: proposed}, nil
}

// Git owns resource bytes; the frozen native closure owns distribution policy.
func preserveReleaseDistribution(base domain.TeamReleasePayload, preview *Preview) error {
	if err := domain.PreserveTeamDistribution(base, &preview.Payload); err != nil {
		return err
	}
	var err error
	preview.SHA256, err = domain.IntelligenceDigest(preview.Payload)
	return err
}

func selectedResourceFile(p string, s domain.TeamImportSelection) bool {
	// selectedFile includes top-level configuration/notices automatically; those
	// are not resource edits and must remain untouched by this narrow exporter.
	if p == "LICENSE" || p == "NOTICE" || p == "LICENSE.md" || p == "NOTICE.md" {
		return false
	}
	return selectedFile(p, s)
}

func exportPatch(ctx context.Context, root string, before, after map[string]File) ([]byte, error) {
	dir, err := os.MkdirTemp("", "agentflow-team-export-")
	if err != nil {
		return nil, ErrSource
	}
	defer os.RemoveAll(dir)
	if _, err = runGit(ctx, dir, 16<<10, "init", "--bare", "--template=", "."); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(before))
	for p := range before {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	// Build raw object trees: worktree staging obeys selected .gitignore and
	// .gitattributes and can silently omit resources or normalize their bytes.
	tree := func(files map[string]File) (string, error) {
		for _, p := range paths {
			file, target := files[p], p
			if root != "." {
				target = path.Join(root, p)
			}
			for _, component := range strings.Split(target, "/") {
				if strings.EqualFold(component, ".git") {
					return "", fmt.Errorf("Git metadata cannot be exported as resource content")
				}
			}
			object, err := runGitInput(ctx, dir, 128, file.Content, "hash-object", "-w", "--no-filters", "--stdin")
			if err != nil {
				return "", err
			}
			sha := strings.TrimSpace(string(object))
			if !domain.ValidGitCommitSHA(sha) {
				return "", ErrSource
			}
			if _, err := runGit(ctx, dir, 16<<10, "update-index", "--add", "--cacheinfo", file.Mode, sha, target); err != nil {
				return "", err
			}
		}
		object, err := runGit(ctx, dir, 128, "write-tree")
		return strings.TrimSpace(string(object)), err
	}
	oldTree, err := tree(before)
	if err != nil {
		return nil, err
	}
	newTree, err := tree(after)
	if err != nil {
		return nil, err
	}
	diff := func() ([]byte, error) {
		return runGit(ctx, dir, 16<<20, "diff-tree", "-r", "-p", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", oldTree, newTree, "--")
	}
	patch, err := diff()
	if err != nil || utf8.Valid(patch) {
		return patch, err
	}
	// Git can classify non-UTF8 blobs without NULs as text. JSON strings would
	// replace those bytes; binary patches preserve them through API transport.
	if err := os.MkdirAll(filepath.Join(dir, "info"), 0700); err != nil {
		return nil, ErrSource
	}
	if err := os.WriteFile(filepath.Join(dir, "info", "attributes"), []byte("* -diff\n"), 0600); err != nil {
		return nil, ErrSource
	}
	return diff()
}
