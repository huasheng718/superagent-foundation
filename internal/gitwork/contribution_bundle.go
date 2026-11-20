package gitwork

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// ContributionBundle describes immutable bytes, not an accepted task result.
// Acceptance remains owned by the task's lease-fenced completion transaction.
type ContributionBundle struct {
	Path      string
	BaseSHA   string
	ResultSHA string
	SHA256    string
	Size      int64
}

func fullCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 20 && value == strings.ToLower(value)
}

// contributionGit has no host HOME/config, credentials or transport authority.
// Callers own a private HOME for the duration; no uploaded Git config is used
// during reconstruction. Explicit config also disables hooks and signing.
func contributionGit(ctx context.Context, dir, home, index string, args ...string) *exec.Cmd {
	config := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "core.autocrlf=false"}
	cmd := exec.CommandContext(ctx, "git", append(config, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_AUTHOR_NAME=SuperAgent", "GIT_AUTHOR_EMAIL=superagent@example.invalid", "GIT_COMMITTER_NAME=SuperAgent", "GIT_COMMITTER_EMAIL=superagent@example.invalid", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir)}
	if index != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
	}
	return cmd
}

func contributionRun(ctx context.Context, dir, home, index string, args ...string) (string, error) {
	cmd := contributionGit(ctx, dir, home, index, args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("contribution git %s: %w: %.2048s", args[0], err, output.String())
	}
	return strings.TrimSpace(output.String()), nil
}

func validateContributionRepository(dir string) error {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("contribution requires an independent clone with a real .git directory")
	}
	for _, rel := range []string{"shallow", "objects/info/alternates", "info/grafts"} {
		if _, err := os.Lstat(filepath.Join(dir, ".git", filepath.FromSlash(rel))); !os.IsNotExist(err) {
			return fmt.Errorf("contribution repository has unsupported %s", rel)
		}
	}
	for _, rel := range []string{"objects", "refs"} {
		if info, err := os.Lstat(filepath.Join(dir, ".git", rel)); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("contribution .git/%s must be a real directory", rel)
		}
	}
	return nil
}

// ExportContribution finalizes eligible working changes using a fresh index,
// commits locally without hooks, and streams a prerequisite-free bundle. No
// change is valid: ResultSHA then equals HEAD. Shallow/alternate clones fail.
func ExportContribution(ctx context.Context, repositoryDir, bundlePath, baseSHA, message string, maxBytes int64) (ContributionBundle, error) {
	var result ContributionBundle
	if !fullCommitSHA(baseSHA) || maxBytes <= 0 || maxBytes > domain.RunnerContributionMaxBytes {
		return result, errors.New("invalid contribution baseline or size limit")
	}
	if err := validateContributionRepository(repositoryDir); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	home, err := os.MkdirTemp("", "agentflow-contribution-home-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(home)
	run := func(args ...string) (string, error) { return contributionRun(ctx, repositoryDir, home, "", args...) }
	head, err := run("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return result, err
	}
	if _, err = run("merge-base", "--is-ancestor", baseSHA, head); err != nil {
		return result, fmt.Errorf("contribution HEAD does not descend from assigned base: %w", err)
	}
	// Reject excluded paths already committed by the executor as well as keeping
	// control files/credentials out of the final staging operation.
	changed, err := run("diff", "--name-only", "-z", baseSHA, head)
	if err != nil {
		return result, err
	}
	for _, path := range strings.Split(changed, "\x00") {
		if path != "" && Excluded(path) {
			return result, fmt.Errorf("contribution commit changes excluded path %q", path)
		}
	}
	index := filepath.Join(home, "index")
	if _, err = contributionRun(ctx, repositoryDir, home, index, "read-tree", head); err != nil {
		return result, err
	}
	files, err := run("ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return result, err
	}
	eligible := []string{}
	for _, path := range strings.Split(files, "\x00") {
		if path != "" && !Excluded(path) {
			eligible = append(eligible, path)
		}
	}
	if len(eligible) > 0 {
		add := contributionGit(ctx, repositoryDir, home, index, "--literal-pathspecs", "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul")
		add.Stdin = strings.NewReader(strings.Join(eligible, "\x00") + "\x00")
		if output, err := add.CombinedOutput(); err != nil {
			return result, fmt.Errorf("stage contribution: %w: %.2048s", err, output)
		}
	}
	tree, err := contributionRun(ctx, repositoryDir, home, index, "write-tree")
	if err != nil {
		return result, err
	}
	previousTree, err := run("rev-parse", head+"^{tree}")
	if err != nil {
		return result, err
	}
	resultSHA := head
	if tree != previousTree {
		if strings.TrimSpace(message) == "" {
			message = "SuperAgent task contribution"
		}
		resultSHA, err = run("commit-tree", tree, "-p", head, "-m", message)
		if err != nil {
			return result, err
		}
		if _, err = run("update-ref", "HEAD", resultSHA, head); err != nil {
			return result, err
		}
	}
	if _, err = run("update-ref", domain.RunnerContributionRef, resultSHA); err != nil {
		return result, err
	}
	file, err := os.OpenFile(bundlePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return result, err
	}
	success := false
	defer func() {
		file.Close()
		if !success {
			os.Remove(bundlePath)
		}
	}()
	hash := sha256.New()
	writer := &contributionLimitWriter{dst: io.MultiWriter(file, hash), left: maxBytes}
	cmd := contributionGit(ctx, repositoryDir, home, "", "bundle", "create", "-", domain.RunnerContributionRef)
	var stderr bytes.Buffer
	cmd.Stdout = writer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		return result, fmt.Errorf("export contribution bundle: %w: %.2048s", err, stderr.String())
	}
	if err = file.Sync(); err != nil {
		return result, err
	}
	if err = file.Close(); err != nil {
		return result, err
	}
	result = ContributionBundle{Path: bundlePath, BaseSHA: baseSHA, ResultSHA: resultSHA, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: maxBytes - writer.left}
	success = true
	return result, nil
}

type contributionLimitWriter struct {
	dst  io.Writer
	left int64
}

func (w *contributionLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, errors.New("contribution exceeds byte limit")
	}
	n, err := w.dst.Write(p)
	w.left -= int64(n)
	return n, err
}

// VerifyContribution reconstructs only Git objects in a fresh checkout. It
// never loads Worker config/hooks/remotes or starts submodules. maxExpanded
// limits the sum of reachable object sizes before any checkout; callers also
// enforce a process/container resource boundary for hostile pack inflation.
func VerifyContribution(ctx context.Context, bundlePath, destination string, expected ContributionBundle, maxExpanded int64) error {
	if !fullCommitSHA(expected.BaseSHA) || !fullCommitSHA(expected.ResultSHA) || len(expected.SHA256) != 64 || expected.Size <= 0 || expected.Size > domain.RunnerContributionMaxBytes || maxExpanded <= 0 || maxExpanded > domain.RunnerContributionMaxExpandedBytes {
		return errors.New("invalid contribution verification metadata")
	}
	file, err := os.Open(bundlePath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expected.Size {
		return errors.New("contribution bundle size or file type mismatch")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(file, expected.Size+1)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return errors.New("contribution bundle hash mismatch")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReader(io.LimitReader(file, 4096))
	line, err := reader.ReadString('\n')
	if err != nil || line != "# v2 git bundle\n" {
		return errors.New("only complete SHA1 version2 bundles are supported")
	}
	line, err = reader.ReadString('\n')
	if err != nil || line != expected.ResultSHA+" "+domain.RunnerContributionRef+"\n" {
		return errors.New("contribution bundle has prerequisites, unexpected refs or result SHA")
	}
	line, err = reader.ReadString('\n')
	if err != nil || line != "\n" {
		return errors.New("contribution bundle must advertise exactly one ref")
	}
	if err = os.Mkdir(destination, 0o700); err != nil {
		return fmt.Errorf("fresh contribution directory: %w", err)
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(destination)
		}
	}()
	home, err := os.MkdirTemp("", "agentflow-import-home-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	run := func(args ...string) (string, error) { return contributionRun(ctx, destination, home, "", args...) }
	if _, err = run("init", "-q", "--template=", "--object-format=sha1"); err != nil {
		return err
	}
	if _, err = run("bundle", "verify", bundlePath); err != nil {
		return err
	}
	if _, err = run("bundle", "unbundle", bundlePath); err != nil {
		return err
	}
	if _, err = run("update-ref", domain.RunnerContributionRef, expected.ResultSHA); err != nil {
		return err
	}
	if _, err = run("fsck", "--strict", "--no-reflogs"); err != nil {
		return err
	}
	if _, err = run("merge-base", "--is-ancestor", expected.BaseSHA, expected.ResultSHA); err != nil {
		return errors.New("contribution base is unavailable or not an ancestor")
	}
	// Size enumeration is streamed so an object-count bomb cannot fill a Go
	// output buffer. Git's own pack inflation still needs an OS resource limit.
	cmd := contributionGit(ctx, destination, home, "", "cat-file", "--batch-all-objects", "--batch-check=%(objectsize)")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	var expanded int64
	var objects int
	for scanner.Scan() {
		size, parseErr := strconv.ParseInt(scanner.Text(), 10, 64)
		objects++
		if parseErr != nil || size < 0 || size > maxExpanded-expanded || objects > domain.RunnerContributionMaxObjects {
			cmd.Process.Kill()
			cmd.Wait()
			return errors.New("contribution expanded object limit exceeded")
		}
		expanded += size
	}
	if err = scanner.Err(); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	if err = cmd.Wait(); err != nil {
		return err
	}
	changed, err := run("diff", "--name-only", "-z", expected.BaseSHA, expected.ResultSHA)
	if err != nil {
		return err
	}
	for _, path := range strings.Split(changed, "\x00") {
		if path != "" && Excluded(path) {
			return fmt.Errorf("contribution modifies excluded path %q", path)
		}
	}
	if _, err = run("checkout", "-q", "--detach", expected.ResultSHA); err != nil {
		return err
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil || head != expected.ResultSHA {
		return errors.New("reconstructed contribution HEAD mismatch")
	}
	success = true
	return nil
}

// ImportContributionBundle verifies an accepted bundle in an isolated fresh
// checkout, then imports its objects into a clean task checkout that is still
// exactly at the frozen base. The checkout's existing branch name is retained
// so a retry sees the same ownership marker and branch contract.
func ImportContributionBundle(ctx context.Context, bundlePath, destination string, expected ContributionBundle, maxExpanded int64) error {
	if err := validateContributionRepository(destination); err != nil {
		return err
	}
	verified, err := os.MkdirTemp("", "agentflow-input-verify-")
	if err != nil {
		return err
	}
	if err := os.Remove(verified); err != nil {
		return err
	}
	defer os.RemoveAll(verified)
	if err := VerifyContribution(ctx, bundlePath, verified, expected, maxExpanded); err != nil {
		return err
	}
	home, err := os.MkdirTemp("", "agentflow-input-import-home-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	run := func(args ...string) (string, error) { return contributionRun(ctx, destination, home, "", args...) }
	status, err := run("status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching")
	if err != nil || status != "" {
		if err != nil {
			return err
		}
		return errors.New("input checkout is not clean")
	}
	base, err := run("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || base != expected.BaseSHA {
		return errors.New("input checkout is not at the frozen base")
	}
	branch, err := run("branch", "--show-current")
	if err != nil || branch == "" {
		return errors.New("input checkout must remain on its assigned branch")
	}
	if _, err := run("check-ref-format", "--branch", branch); err != nil {
		return errors.New("input checkout branch is invalid")
	}
	if _, err := run("bundle", "unbundle", bundlePath); err != nil {
		return err
	}
	if _, err := run("update-ref", domain.RunnerContributionRef, expected.ResultSHA); err != nil {
		return err
	}
	if _, err := run("checkout", "-q", "--force", "-B", branch, expected.ResultSHA); err != nil {
		return err
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil || head != expected.ResultSHA {
		return errors.New("imported contribution HEAD mismatch")
	}
	return nil
}
