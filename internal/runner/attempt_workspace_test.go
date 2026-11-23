package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

func isolatedFixtureTask(id string, attempt int) domain.RunnerTask {
	task := domain.RunnerTask{ID: id, RunID: "isolated-run", NodeID: id, Attempt: attempt, Provider: "terminal", Invocation: &domain.RunnerInvocation{RunID: "isolated-run", NodeID: id, WorkspaceIsolation: &domain.RunnerWorkspaceIsolation{Version: 1}}, Sandbox: &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{ID: "local", Driver: "local"}}}
	task.Invocation.WorkspaceIsolation.ContextSHA256, _ = domain.RunnerContributionContextSHA(task)
	return task
}

func TestAttemptWorkspaceRejectsSymlinksAndWrongOwner(t *testing.T) {
	task := isolatedFixtureTask("a", 1)
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "runs")); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareIsolatedTaskWorkspace(root, task); err == nil {
		t.Fatal("symlinked workspace accepted")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("symlink target was modified")
	}
	root = t.TempDir()
	desc, err := PrepareIsolatedTaskWorkspace(root, task)
	if err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(root, filepath.FromSlash(desc.WorkspaceRel), isolatedWorkspaceOwner)
	if err := os.Chmod(owner, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(owner, []byte("another attempt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareIsolatedTaskWorkspace(root, task); err == nil {
		t.Fatal("wrong owner marker accepted")
	}
}

func TestAttemptWorkspaceConcurrentPublicationAndFrozenContext(t *testing.T) {
	root := t.TempDir()
	results := make(chan error, 24)
	for i := 0; i < cap(results); i++ {
		go func(i int) {
			_, err := PrepareIsolatedTaskWorkspace(root, isolatedFixtureTask(fmt.Sprintf("parallel-%d", i), 1))
			results <- err
		}(i)
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if err := verifyIsolatedRunCleanup(root, "isolated-run"); err != nil {
		t.Fatal(err)
	}
	task := isolatedFixtureTask("mutated", 1)
	task.Command = "a command outside the frozen assignment"
	if _, err := PrepareIsolatedTaskWorkspace(root, task); err == nil {
		t.Fatal("mutated frozen invocation accepted")
	}
	marker := filepath.Join(root, filepath.FromSlash(domain.IsolatedRunWorkspaceRel("isolated-run")), isolatedRunOwner)
	if err := os.Chmod(marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(`{"runId":"another-run"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyIsolatedRunCleanup(root, "isolated-run"); err == nil {
		t.Fatal("cleanup accepted a different run owner")
	}
}

func TestAttemptWorkspaceClonesPinnedBaseAndKeepsPeerAndRetrySeparate(t *testing.T) {
	ctx := context.Background()
	remote := bareRemoteWithBaseline(t)
	base, ok := gitwork.RevParse(ctx, remote, "refs/heads/master")
	if !ok {
		t.Fatal("fixture base missing")
	}
	source := filepath.Join(filepath.Dir(remote), "source")
	if err := os.WriteFile(filepath.Join(source, "newer.txt"), []byte("floating ref advanced"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitT(t, source, "add", "newer.txt")
	runGitT(t, source, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "newer than pin")
	runGitT(t, source, "push", remote, "master")
	for _, disableReuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "reuse", true: "fresh"}[disableReuse], func(t *testing.T) {
			d := &Daemon{cfg: Config{DefaultWorkspaceRoot: t.TempDir(), DisableRunSandboxReuse: disableReuse}}
			t.Cleanup(func() { d.closeAllRunSandboxes(ctx) })
			a, b := isolatedFixtureTask("a", 1), isolatedFixtureTask("b", 1)
			for _, task := range []*domain.RunnerTask{&a, &b} {
				task.Invocation.Repositories = []domain.RunnerRepository{{RepositoryID: "repo", Dir: "repo", CloneURL: remote, Ref: "master", BaseSHA: base}}
				task.Invocation.WorkspaceIsolation.ContextSHA256, _ = domain.RunnerContributionContextSHA(*task)
				if err := d.checkoutTaskRepositories(ctx, *task); err != nil {
					t.Fatal(err)
				}
			}
			aRepo, bRepo := filepath.Join(d.taskWorkspace(a), "repo"), filepath.Join(d.taskWorkspace(b), "repo")
			if err := os.WriteFile(filepath.Join(aRepo, "private-a.txt"), []byte("only A"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(bRepo, "private-a.txt")); !os.IsNotExist(err) {
				t.Fatal("B observes A private checkout file")
			}
			aBranch, _ := gitwork.C(ctx, aRepo, "branch", "--show-current")
			bBranch, _ := gitwork.C(ctx, bRepo, "branch", "--show-current")
			if aBranch == bBranch {
				t.Fatal("parallel attempts share a branch")
			}
			for _, repo := range []string{aRepo, bRepo} {
				head, ok := gitwork.RevParse(ctx, repo, "HEAD")
				if !ok || head != base {
					t.Fatal("base SHA not pinned")
				}
				baseline, err := os.ReadFile(filepath.Join(repo, ".git", "agentflow-baseline"))
				if err != nil || string(baseline) != base {
					t.Fatal("baseline moved to floating acquisition ref")
				}
				if _, err := os.Stat(filepath.Join(repo, "newer.txt")); !os.IsNotExist(err) {
					t.Fatal("floating ref files appeared in pinned checkout")
				}
			}
			first, release, err := d.acquireTaskSandbox(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			release()
			second, release, err := d.acquireTaskSandbox(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if first == second || first.Workspace() == second.Workspace() {
				t.Fatal("peer tasks share sandbox or writable mount")
			}
			a.Attempt = 2
			if err := d.checkoutTaskRepositories(ctx, a); err != nil {
				t.Fatal(err)
			}
			if d.taskWorkspace(a) == filepath.Dir(aRepo) {
				t.Fatal("retry reused old attempt directory")
			}
			if _, err := os.Stat(filepath.Join(d.taskWorkspace(a), "repo", "private-a.txt")); !os.IsNotExist(err) {
				t.Fatal("retry inherited stale dirty file")
			}
			if _, err := os.Stat(filepath.Join(aRepo, "private-a.txt")); err != nil {
				t.Fatal("retry erased old attempt evidence")
			}
			if _, err := os.Stat(filepath.Join(bRepo, "README.md")); err != nil {
				t.Fatal("retry damaged peer")
			}
		})
	}
	bad := isolatedFixtureTask("bad", 1)
	bad.Invocation.Repositories = []domain.RunnerRepository{{RepositoryID: "repo", Dir: "repo", CloneURL: remote, Ref: "master", BaseSHA: strings.Repeat("0", 40)}}
	bad.Invocation.WorkspaceIsolation.ContextSHA256, _ = domain.RunnerContributionContextSHA(bad)
	d := &Daemon{cfg: Config{DefaultWorkspaceRoot: t.TempDir()}}
	if err := d.checkoutTaskRepositories(ctx, bad); err == nil {
		t.Fatal("unavailable pinned commit silently replaced by branch HEAD")
	}
}
