package gitwork

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestContributionPublishCreateOnlyAndReplay(t *testing.T) {
	worker, base := contributionFixture(t)
	ctx := context.Background()
	root := filepath.Dir(worker)
	remote := filepath.Join(root, "target.git")
	if _, err := C(ctx, root, "init", "--bare", "-q", remote); err != nil {
		t.Fatal(err)
	}
	branch := "superagent/integration/" + strings.Repeat("a", 64)
	if _, err := C(ctx, worker, "push", remote, base+":refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	bundle, err := ExportContribution(ctx, worker, filepath.Join(root, "unchanged.bundle"), base, "unchanged", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(root, "verified")
	if err := VerifyContribution(ctx, bundle.Path, checkout, bundle, 1<<20); err != nil {
		t.Fatal(err)
	}
	// A push from two independent publishers converges at the same fixed SHA.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- publishContributionBranch(ctx, checkout, remote, branch, base, base, "main", "")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got, err := C(ctx, root, "--git-dir="+remote, "rev-parse", "refs/heads/"+branch); err != nil || got != base {
		t.Fatalf("head %s %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(worker, "code.txt"), []byte("human change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := C(ctx, worker, "commit", "-am", "human"); err != nil {
		t.Fatal(err)
	}
	human, _ := RevParse(ctx, worker, "HEAD")
	if _, err := C(ctx, worker, "push", remote, "HEAD:refs/heads/"+branch); err != nil {
		t.Fatal(err)
	}
	if err := publishContributionBranch(ctx, checkout, remote, branch, base, base, "main", ""); err == nil {
		t.Fatal("overwrote different head")
	}
	if got, err := C(ctx, root, "--git-dir="+remote, "rev-parse", "refs/heads/"+branch); err != nil || got != human {
		t.Fatalf("human head changed %s %v", got, err)
	}
	if got, err := C(ctx, root, "--git-dir="+remote, "rev-parse", "refs/heads/main"); err != nil || got != base {
		t.Fatalf("default changed %s %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "untrusted"), []byte("dirty"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishContributionBranch(ctx, checkout, remote, "superagent/integration/"+strings.Repeat("b", 64), base, base, "main", ""); err == nil {
		t.Fatal("dirty checkout published")
	}
}

func TestContributionPublishRejectsNonGitHubAuthority(t *testing.T) {
	for _, remote := range []string{"file:///tmp/target.git", "https://github.com.evil/example/repo.git", "https://user:secret@github.com/example/repo.git", "http://github.com/example/repo.git", "https://github.com/example/repo.git?x=y", "https://github.com/example/repo.git#x"} {
		if err := PublishContributionBranch(context.Background(), "", remote, "superagent/integration/"+strings.Repeat("a", 64), strings.Repeat("1", 40), strings.Repeat("1", 40), "main", "controller-token"); err == nil {
			t.Fatalf("accepted %s", remote)
		}
	}
}

func TestContributionPublishRejectsUnrelatedTargetHistory(t *testing.T) {
	worker, base := contributionFixture(t)
	ctx := context.Background()
	root := filepath.Dir(worker)
	remote := filepath.Join(root, "target.git")
	for _, args := range [][]string{{"init", "--bare", "-q", remote}, {"-C", worker, "push", remote, base + ":refs/heads/main"}, {"-C", worker, "checkout", "--orphan", "unrelated"}, {"-C", worker, "rm", "-rf", "."}} {
		if _, err := C(ctx, root, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worker, "unrelated.txt"), []byte("another repository history\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := C(ctx, worker, "add", "unrelated.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := C(ctx, worker, "commit", "-qm", "unrelated history"); err != nil {
		t.Fatal(err)
	}
	unrelated, _ := RevParse(ctx, worker, "HEAD")
	branch := "superagent/integration/" + strings.Repeat("d", 64)
	if err := publishContributionBranch(ctx, worker, remote, branch, unrelated, unrelated, "main", ""); err == nil || !strings.Contains(err.Error(), "target history") {
		t.Fatalf("unrelated base allowed: %v", err)
	}
	if got, err := C(ctx, root, "--git-dir="+remote, "for-each-ref", "--format=%(refname)", "refs/heads/"+branch); err != nil || got != "" {
		t.Fatalf("remote mutated: %s %v", got, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := publishContributionBranch(canceled, worker, remote, branch, unrelated, unrelated, "main", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("context lost: %v", err)
	}
}
