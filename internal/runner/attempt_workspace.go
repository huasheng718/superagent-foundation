package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
)

const isolatedWorkspaceOwner = ".agentflow-workspace-owner.json"
const isolatedRunOwner = ".agentflow-run-owner.json"

func isolatedTask(task domain.RunnerTask) bool {
	return task.Invocation != nil && task.Invocation.WorkspaceIsolation != nil
}

func isolationRepositories(inv *domain.RunnerInvocation) []domain.RunnerRepository {
	if len(inv.Repositories) > 0 {
		return inv.Repositories
	}
	if inv.Repository != nil {
		return []domain.RunnerRepository{*inv.Repository}
	}
	return nil
}

// PrepareIsolatedTaskWorkspace verifies the immutable claimed identity and
// creates only its owned directory. Result exporters can call it again to
// verify the same owner marker before reading objects. It never copies work
// from a previous attempt or follows a symlink below the configured root.
func PrepareIsolatedTaskWorkspace(workspaceRoot string, task domain.RunnerTask) (domain.RunnerWorkspaceDescriptor, error) {
	desc, err := domain.IsolatedWorkspaceDescriptor(task)
	if err != nil {
		return desc, err
	}
	contextSHA, err := domain.RunnerContributionContextSHA(task)
	if err != nil || contextSHA != desc.ContextSHA256 {
		return desc, fmt.Errorf("isolated task does not match its frozen contextSHA256")
	}
	seen := map[string]bool{}
	for _, repository := range isolationRepositories(task.Invocation) {
		if !domain.ValidWorkspaceBaseSHA(repository.BaseSHA) {
			return desc, fmt.Errorf("isolated repository %q requires a full SHA-1 baseSHA", repository.RepositoryID)
		}
		dir := safeRunnerRepoDir(repository.Dir)
		if dir == "" || filepath.ToSlash(dir) != repository.Dir || strings.Contains(repository.Dir, "/") || seen[dir] {
			return desc, fmt.Errorf("isolated repository directory must be unique and first-level: %q", repository.Dir)
		}
		seen[dir] = true
	}
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		return desc, err
	}
	runRoot, err := openIsolatedDirectory(workspaceRoot, domain.IsolatedRunWorkspaceRel(task.RunID))
	if err != nil {
		return desc, err
	}
	wantRun, _ := json.Marshal(struct {
		RunID string `json:"runId"`
	}{task.RunID})
	err = writeOrVerifyWorkspaceOwner(runRoot, isolatedRunOwner, wantRun)
	runRoot.Close()
	if err != nil {
		return desc, err
	}
	root, err := openIsolatedDirectory(workspaceRoot, desc.WorkspaceRel)
	if err != nil {
		return desc, err
	}
	defer root.Close()
	want, _ := json.Marshal(desc)
	return desc, writeOrVerifyWorkspaceOwner(root, isolatedWorkspaceOwner, want)
}

func writeOrVerifyWorkspaceOwner(root *os.Root, name string, want []byte) error {
	if _, err := root.Lstat(name); os.IsNotExist(err) {
		// Publish a complete marker atomically: parallel nodes may initialize
		// the same run root, and must never observe another writer's empty file.
		temp := ".agentflow-owner-" + rand.Text()
		file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
		if err != nil {
			return err
		}
		defer root.Remove(temp)
		_, writeErr := file.Write(want)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := root.Link(temp, name); err != nil && !os.IsExist(err) {
			return fmt.Errorf("publish workspace owner: %w", err)
		}
	} else if err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("workspace owner must be a regular file")
	}
	got, err := root.ReadFile(name)
	if err != nil || !bytes.Equal(got, want) {
		return fmt.Errorf("workspace owner does not match claimed attempt")
	}
	return nil
}

// Root confines all relative traversal even if paths change while opening.
// Lstat + opened-directory identity checks additionally reject in-root aliases.
func openIsolatedDirectory(workspaceRoot, rel string) (*os.Root, error) {
	return openIsolatedDirectoryMode(workspaceRoot, rel, true)
}

func openIsolatedDirectoryMode(workspaceRoot, rel string, create bool) (*os.Root, error) {
	if !domain.SafeWorkspaceRelPath(rel) {
		return nil, fmt.Errorf("unsafe isolated workspace path %q", rel)
	}
	current, err := os.OpenRoot(workspaceRoot)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(rel, "/") {
		if create {
			if err := current.Mkdir(part, 0o700); err != nil && !os.IsExist(err) {
				current.Close()
				return nil, err
			}
		}
		info, err := current.Lstat(part)
		if err != nil {
			current.Close()
			return nil, err
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			current.Close()
			return nil, fmt.Errorf("isolated workspace component is not a real directory: %s", part)
		}
		next, err := current.OpenRoot(part)
		if err == nil {
			opened, statErr := next.Stat(".")
			if statErr != nil || !os.SameFile(info, opened) {
				next.Close()
				err = fmt.Errorf("isolated workspace directory changed while opening")
			}
		}
		current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func verifyIsolatedRunCleanup(root, runID string) error {
	runRoot, err := openIsolatedDirectoryMode(root, domain.IsolatedRunWorkspaceRel(runID), false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer runRoot.Close()
	info, err := runRoot.Lstat(isolatedRunOwner)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("isolated run cleanup requires a valid owner marker")
	}
	want, _ := json.Marshal(struct {
		RunID string `json:"runId"`
	}{runID})
	got, err := runRoot.ReadFile(isolatedRunOwner)
	if err != nil || !bytes.Equal(got, want) {
		return fmt.Errorf("isolated run cleanup owner does not match run")
	}
	return nil
}

func (d *Daemon) prepareTaskWorkspace(task domain.RunnerTask) error {
	if !isolatedTask(task) {
		return nil
	}
	_, err := PrepareIsolatedTaskWorkspace(d.cfg.DefaultWorkspaceRoot, task)
	return err
}

func (d *Daemon) checkoutTaskRepositories(ctx context.Context, task domain.RunnerTask) error {
	workspace := d.taskWorkspace(task)
	driver := runnerTaskSandboxDriver(task)
	if !isolatedTask(task) {
		return checkoutInvocationRepositories(ctx, workspace, task.Invocation, driver)
	}
	desc, err := PrepareIsolatedTaskWorkspace(d.cfg.DefaultWorkspaceRoot, task)
	if err != nil {
		return err
	}
	for _, repository := range isolationRepositories(task.Invocation) {
		root, err := openIsolatedDirectory(workspace, repository.Dir)
		if err != nil {
			return err
		}
		root.Close()
		target := filepath.Join(workspace, repository.Dir)
		if err := checkoutIsolatedRepository(ctx, target, repository, desc); err != nil {
			return err
		}
		if err := d.importTaskContributionInput(ctx, task, repository, target); err != nil {
			return err
		}
		if err := configureRepositoryGitIdentity(ctx, target, task.Invocation.GitUserName, task.Invocation.GitUserEmail); err != nil {
			return err
		}
		if err := runtime.ProtectProjectAgentGitIndex(ctx, target, driver); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) importTaskContributionInput(ctx context.Context, task domain.RunnerTask, repository domain.RunnerRepository, target string) error {
	if task.Invocation == nil || len(task.Invocation.ContributionInputs) == 0 {
		return nil
	}
	var input *domain.RunnerContributionInput
	for i := range task.Invocation.ContributionInputs {
		candidate := &task.Invocation.ContributionInputs[i]
		if candidate.RepositoryID != repository.RepositoryID {
			continue
		}
		if input != nil {
			return fmt.Errorf("repository %s has duplicate frozen contribution inputs", repository.RepositoryID)
		}
		input = candidate
	}
	if input == nil {
		return nil
	}
	if input.BaseSHA != repository.BaseSHA {
		return fmt.Errorf("repository %s input base differs from checkout baseline", repository.RepositoryID)
	}
	bundlePath, err := d.client.DownloadContributionInput(ctx, task.ID, task.Attempt, repository.RepositoryID, *input)
	if err != nil {
		return fmt.Errorf("download contribution input %s: %w", repository.RepositoryID, err)
	}
	defer os.Remove(bundlePath)
	bundle := gitwork.ContributionBundle{Path: bundlePath, BaseSHA: input.BaseSHA, ResultSHA: input.HeadSHA, SHA256: input.BundleSHA256, Size: input.BundleSize}
	if err := gitwork.ImportContributionBundle(ctx, bundlePath, target, bundle, 64<<20); err != nil {
		return fmt.Errorf("import contribution input %s: %w", repository.RepositoryID, err)
	}
	return nil
}

func checkoutIsolatedRepository(ctx context.Context, target string, repository domain.RunnerRepository, desc domain.RunnerWorkspaceDescriptor) error {
	if err := gitwork.ValidateRemoteURL(repository.CloneURL, true); err != nil {
		return err
	}
	repoCtx := gitwork.WithToken(ctx, repository.GitToken)
	root, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()
	const stamp = ".git/agentflow-isolated-checkout.json"
	want, _ := json.Marshal(struct {
		Identity string `json:"identitySHA256"`
		BaseSHA  string `json:"baseSHA"`
		Branch   string `json:"branch"`
	}{desc.IdentitySHA256, repository.BaseSHA, desc.Branch})
	if info, err := root.Lstat(".git"); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("isolated repository .git is not a real directory")
		}
		info, err := root.Lstat(stamp)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("isolated checkout has no valid ownership stamp; preserving it")
		}
		got, err := root.ReadFile(stamp)
		if err != nil || !bytes.Equal(got, want) {
			return fmt.Errorf("isolated checkout does not match assignment")
		}
		branch, err := gitwork.C(repoCtx, target, "branch", "--show-current")
		if err != nil || strings.TrimSpace(branch) != desc.Branch {
			return fmt.Errorf("isolated checkout branch differs from assigned branch")
		}
		baseline, err := root.ReadFile(".git/agentflow-baseline")
		if err != nil || string(baseline) != repository.BaseSHA {
			return fmt.Errorf("isolated checkout baseline differs from pinned baseSHA")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		return fmt.Errorf("isolated repository must start empty; preserving existing files")
	}
	args := []string{"clone", "--no-hardlinks", "--no-checkout"}
	if ref := strings.TrimSpace(repository.Ref); ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, "--", repository.CloneURL, ".")
	if _, err := gitwork.C(repoCtx, target, args...); err != nil {
		return fmt.Errorf("clone isolated repository: %w", err)
	}
	if _, ok := gitwork.RevParse(repoCtx, target, repository.BaseSHA+"^{commit}"); !ok {
		if _, err := gitwork.C(repoCtx, target, "fetch", "--no-tags", "origin", repository.BaseSHA); err != nil {
			return fmt.Errorf("fetch pinned baseSHA: %w", err)
		}
	}
	if _, err := gitwork.C(repoCtx, target, "checkout", "--quiet", "-b", desc.Branch, repository.BaseSHA); err != nil {
		return fmt.Errorf("checkout pinned baseSHA: %w", err)
	}
	head, ok := gitwork.RevParse(repoCtx, target, "HEAD")
	if !ok || head != repository.BaseSHA {
		return fmt.Errorf("isolated checkout HEAD does not match pinned baseSHA")
	}
	// RecordBaseline's legacy fallback prefers origin/master over HEAD. This
	// assignment instead pins an explicit commit that may be older than Ref.
	if err := root.WriteFile(".git/agentflow-baseline", []byte(repository.BaseSHA), 0o600); err != nil {
		return err
	}
	return root.WriteFile(stamp, want, 0o400)
}
