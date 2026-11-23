// Package teamconfig reads a bounded, explicitly selected TeamAI source subset.
// It never runs the upstream CLI, checks out files or changes editable catalog rows.
package teamconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

var ErrRefConflict = errors.New("registered source ref changed or disappeared")

type gitCommandError struct{ exitCode int }

func (e gitCommandError) Error() string { return ErrSource.Error() }
func (e gitCommandError) Unwrap() error { return ErrSource }

var ErrSource = errors.New("registered Git source could not be read at the requested commit")

type File struct {
	domain.TeamImportFile
	Content []byte
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("Git output exceeds import budget")
	}
	return b.Buffer.Write(p)
}

func runGit(ctx context.Context, dir string, limit int, args ...string) ([]byte, error) {
	return runGitInput(ctx, dir, limit, nil, args...)
}

func runGitInput(ctx context.Context, dir string, limit int, input []byte, args ...string) ([]byte, error) {
	token := gitwork.TokenFrom(ctx)
	base := append(gitwork.AuthConfigArgs(token), "-c", "core.hooksPath=/dev/null", "-c", "http.followRedirects=false")
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = gitwork.Env(token)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	for key, value := range map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "GIT_TEMPLATE_DIR": "", "GIT_DIR": "", "GIT_WORK_TREE": "", "GIT_INDEX_FILE": "", "GIT_OBJECT_DIRECTORY": "", "GIT_ALTERNATE_OBJECT_DIRECTORIES": "", "GIT_CEILING_DIRECTORIES": filepath.Dir(dir)} {
		cmd.Env = gitwork.WithEnvValue(cmd.Env, key, value)
	}
	// Empty repository-location variables have special meanings to Git; omit them.
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		prefix := key + "="
		filtered := cmd.Env[:0]
		for _, entry := range cmd.Env {
			if !strings.HasPrefix(entry, prefix) {
				filtered = append(filtered, entry)
			}
		}
		cmd.Env = filtered
	}
	out := &limitedBuffer{limit: limit}
	cmd.Stdout = out
	cmd.Stderr = &limitedBuffer{limit: 16 << 10}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, gitCommandError{exitCode: exit.ExitCode()}
		}
		return nil, ErrSource // Raw Git errors may include source paths or credentials.
	}
	return out.Bytes(), nil
}

// Read fetches precisely one registered source commit into a disposable bare
// repository, then reads only the selected regular blobs and config/notices.
func Read(ctx context.Context, repository domain.Repository, commit, root string, selection domain.TeamImportSelection) (map[string]File, error) {
	if !domain.ValidGitCommitSHA(commit) || (root != "." && !domain.ValidTeamImportPath(root)) {
		return nil, fmt.Errorf("full commit and clean relative TeamAI root are required")
	}
	if err := ValidateSelection(selection); err != nil {
		return nil, err
	}
	if err := ValidateURL(repository.CloneURL, os.Getenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT"), os.Getenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE") == "1"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "agentflow-team-import-")
	if err != nil {
		return nil, ErrSource
	}
	defer os.RemoveAll(dir)
	if _, err := runGit(ctx, dir, 16<<10, "init", "--bare", "--template=", "."); err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, dir, 16<<10, "fetch", "--depth=1", "--no-tags", "--no-recurse-submodules", "--", repository.CloneURL, commit); err != nil {
		return nil, err
	}
	resolved, err := runGit(ctx, dir, 128, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(resolved)) != commit {
		return nil, ErrSource
	}
	args := []string{"ls-tree", "-r", "-l", "-z", commit, "--"}
	if root != "." {
		args = append(args, root+"/")
	}
	listing, err := runGit(ctx, dir, 1<<20, args...)
	if err != nil {
		return nil, err
	}
	files := map[string]File{}
	var total int64
	for _, entry := range bytes.Split(listing, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		header, p, ok := strings.Cut(string(entry), "\t")
		if root != "." {
			p = strings.TrimPrefix(p, root+"/")
		}
		if !ok || !selectedFile(p, selection) {
			continue
		}
		parts := strings.Fields(header)
		if len(parts) != 4 || parts[1] != "blob" || (parts[0] != "100644" && parts[0] != "100755") || !domain.ValidTeamImportPath(p) {
			return nil, diagnostic(p, "selected source must contain regular files only")
		}
		size, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || size < 0 || size > 1<<20 || len(files) >= 1024 || total+size > 6<<20 {
			return nil, diagnostic(p, "selected source exceeds import size/count budget")
		}
		content, err := runGit(ctx, dir, 1<<20, "cat-file", "blob", parts[2])
		if err != nil || int64(len(content)) != size {
			return nil, ErrSource
		}
		digest := sha256.Sum256(content)
		files[p] = File{TeamImportFile: domain.TeamImportFile{Path: p, Mode: parts[0], BlobSHA: parts[2], SHA256: hex.EncodeToString(digest[:]), Size: size}, Content: content}
		total += size
	}
	return files, nil
}

func selectedFile(p string, s domain.TeamImportSelection) bool {
	if p == "teamai.yaml" || p == "LICENSE" || p == "NOTICE" || p == "LICENSE.md" || p == "NOTICE.md" {
		return true
	}
	for _, v := range append(append([]string{}, s.Agents...), s.Rules...) {
		if p == v {
			return true
		}
	}
	for _, v := range s.Skills {
		if p == v || strings.HasPrefix(p, v+"/") {
			return true
		}
	}
	for _, v := range s.MCPServers {
		if p == v.Path {
			return true
		}
	}
	return false
}
