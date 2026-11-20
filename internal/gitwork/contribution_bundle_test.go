package gitwork

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func contributionFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "worker")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.invalid"}} {
		if _, err := C(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "code.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "code.txt"}, {"commit", "-qm", "baseline"}} {
		if _, err := C(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	base, ok := RevParse(ctx, repo, "HEAD")
	if !ok {
		t.Fatal("missing base")
	}
	return repo, base
}

func TestContributionBundleIndependentReconstruction(t *testing.T) {
	repo, base := contributionFixture(t)
	ctx := context.Background()
	root := filepath.Dir(repo)
	marker := filepath.Join(root, "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"code.txt": "changed\n", "new.txt": "new\n", ".env": "private-test-value\n", ".agentflow/private": "internal\n"} {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Even an executor-prestaged credential must not get into the snapshot.
	if _, err := C(ctx, repo, "add", "-f", ".env"); err != nil {
		t.Fatal(err)
	}
	bundle, err := ExportContribution(ctx, repo, filepath.Join(root, "result.bundle"), base, "task result", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.BaseSHA != base || bundle.ResultSHA == base || bundle.Size <= 0 {
		t.Fatalf("bad manifest: %+v", bundle)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("commit hook was run")
	}
	// The original checkout can disappear completely; objects alone suffice.
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(root, "controller")
	if err := VerifyContribution(ctx, bundle.Path, clone, bundle, 8<<20); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"code.txt": "changed\n", "new.txt": "new\n"} {
		got, err := os.ReadFile(filepath.Join(clone, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	for _, name := range []string{".env", ".agentflow/private", ".git/hooks/pre-commit", ".git/objects/info/alternates"} {
		if _, err := os.Stat(filepath.Join(clone, name)); !os.IsNotExist(err) {
			t.Fatalf("worker private path imported: %s", name)
		}
	}
	head, _ := RevParse(ctx, clone, "HEAD")
	if head != bundle.ResultSHA {
		t.Fatal("result SHA changed")
	}
}

func TestImportContributionBundlePinsCleanTaskCheckout(t *testing.T) {
	repo, base := contributionFixture(t)
	ctx := context.Background()
	root := filepath.Dir(repo)
	if err := os.WriteFile(filepath.Join(repo, "code.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bundle, err := ExportContribution(ctx, repo, filepath.Join(root, "input.bundle"), base, "input", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "consumer")
	if _, err := C(ctx, root, "clone", "-q", repo, target); err != nil {
		t.Fatal(err)
	}
	if _, err := C(ctx, target, "checkout", "-q", "-B", "agentflow/test-input", base); err != nil {
		t.Fatal(err)
	}
	if err := ImportContributionBundle(ctx, bundle.Path, target, bundle, 8<<20); err != nil {
		t.Fatal(err)
	}
	head, ok := RevParse(ctx, target, "HEAD")
	if !ok || head != bundle.ResultSHA {
		t.Fatalf("head=%q ok=%v want=%q", head, ok, bundle.ResultSHA)
	}
	branch, err := C(ctx, target, "branch", "--show-current")
	if err != nil || strings.TrimSpace(branch) != "agentflow/test-input" {
		t.Fatalf("branch=%q err=%v", branch, err)
	}
	content, err := os.ReadFile(filepath.Join(target, "code.txt"))
	if err != nil || string(content) != "changed\n" {
		t.Fatalf("imported content=%q err=%v", content, err)
	}
}

func TestContributionBundleRejectsInvalidObjectsAndMetadata(t *testing.T) {
	repo, base := contributionFixture(t)
	ctx := context.Background()
	root := filepath.Dir(repo)
	if err := os.WriteFile(filepath.Join(repo, "code.txt"), []byte(strings.Repeat("content\n", 100)), 0600); err != nil {
		t.Fatal(err)
	}
	good, err := ExportContribution(ctx, repo, filepath.Join(root, "good.bundle"), base, "task", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ContributionBundle)
		limit  int64
	}{
		{"hash", func(b *ContributionBundle) { b.SHA256 = strings.Repeat("0", 64) }, 1 << 20},
		{"size", func(b *ContributionBundle) { b.Size++ }, 1 << 20},
		{"sha", func(b *ContributionBundle) { b.ResultSHA = base }, 1 << 20},
		{"base", func(b *ContributionBundle) { b.BaseSHA = strings.Repeat("1", 40) }, 1 << 20},
		{"expansion", func(b *ContributionBundle) {}, 1},
		{"expansion-cap", func(b *ContributionBundle) {}, domain.RunnerContributionMaxExpandedBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := good
			tc.mutate(&b)
			dest := filepath.Join(root, tc.name)
			if err := VerifyContribution(ctx, b.Path, dest, b, tc.limit); err == nil {
				t.Fatal("invalid bundle accepted")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("failed reconstruction left checkout")
			}
		})
	}
	// A thin bundle requiring a worker-only object cache must fail before import.
	thin := filepath.Join(root, "thin.bundle")
	if _, err := C(ctx, repo, "bundle", "create", thin, "HEAD", "^"+base); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(thin)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	b := good
	b.Path = thin
	b.SHA256 = hex.EncodeToString(hash[:])
	b.Size = int64(len(raw))
	if err := VerifyContribution(ctx, thin, filepath.Join(root, "thin-clone"), b, 1<<20); err == nil {
		t.Fatal("prerequisite bundle accepted")
	}
}

func TestContributionExportNoChangeShallowAndExcludedCommit(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		repo, base := contributionFixture(t)
		b, err := ExportContribution(context.Background(), repo, filepath.Join(filepath.Dir(repo), "same.bundle"), base, "", 1<<20)
		if err != nil || b.ResultSHA != base {
			t.Fatalf("unchanged snapshot: %+v %v", b, err)
		}
	})
	t.Run("shallow", func(t *testing.T) {
		repo, base := contributionFixture(t)
		if err := os.WriteFile(filepath.Join(repo, ".git", "shallow"), []byte(base+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ExportContribution(context.Background(), repo, filepath.Join(filepath.Dir(repo), "bad.bundle"), base, "", 1<<20); err == nil {
			t.Fatal("shallow export accepted")
		}
	})
	t.Run("excluded-commit", func(t *testing.T) {
		repo, base := contributionFixture(t)
		ctx := context.Background()
		if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("test-secret"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := C(ctx, repo, "add", "-f", ".env"); err != nil {
			t.Fatal(err)
		}
		if _, err := C(ctx, repo, "commit", "-qm", "bad"); err != nil {
			t.Fatal(err)
		}
		if _, err := ExportContribution(ctx, repo, filepath.Join(filepath.Dir(repo), "bad.bundle"), base, "", 1<<20); err == nil {
			t.Fatal("already committed secret accepted")
		}
	})
	t.Run("compressed-limit", func(t *testing.T) {
		repo, base := contributionFixture(t)
		if _, err := ExportContribution(context.Background(), repo, filepath.Join(filepath.Dir(repo), "tiny.bundle"), base, "", 1); err == nil {
			t.Fatal("byte limit ignored")
		}
	})
}
