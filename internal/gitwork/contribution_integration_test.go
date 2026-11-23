package gitwork

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestContributionIntegrationIndependentWritersAndConflict(t *testing.T) {
	repo, base := contributionFixture(t)
	ctx := context.Background()
	root := filepath.Dir(repo)
	branches := []ContributionBundle{}
	for i, name := range []string{"left", "right"} {
		copy := filepath.Join(root, name)
		if _, err := C(ctx, root, "clone", "-q", "--no-hardlinks", repo, copy); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(copy, name+".py"), []byte(name+" = True\n"), 0600); err != nil {
			t.Fatal(err)
		}
		b, err := ExportContribution(ctx, copy, filepath.Join(root, name+".bundle"), base, "writer", 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		branches = append(branches, b)
		if i == 0 && b.ResultSHA == base {
			t.Fatal("writer did not commit")
		}
		if err := os.RemoveAll(copy); err != nil {
			t.Fatal(err)
		}
	}
	merged, err := IntegrateContributionBundles(ctx, branches, filepath.Join(root, "merged"), filepath.Join(root, "merged.bundle"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"left.py", "right.py"} {
		if _, err := os.Stat(filepath.Join(root, "merged", name)); err != nil {
			t.Fatal(err)
		}
	}
	again, err := IntegrateContributionBundles(ctx, branches, filepath.Join(root, "again"), filepath.Join(root, "again.bundle"), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if merged.ResultSHA != again.ResultSHA {
		t.Fatal("same accepted ordered inputs changed integration SHA")
	}
	conflicts := []ContributionBundle{}
	for _, name := range []string{"conflict-a", "conflict-b"} {
		copy := filepath.Join(root, name)
		if _, err := C(ctx, root, "clone", "-q", "--no-hardlinks", repo, copy); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(copy, "code.txt"), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		b, err := ExportContribution(ctx, copy, filepath.Join(root, name+".bundle"), base, "writer", 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		conflicts = append(conflicts, b)
	}
	if _, err := IntegrateContributionBundles(ctx, conflicts, filepath.Join(root, "conflict"), filepath.Join(root, "conflict.bundle"), 8<<20); err == nil {
		t.Fatal("conflicting writers silently integrated")
	}
	if _, err := os.Stat(filepath.Join(root, "conflict")); !os.IsNotExist(err) {
		t.Fatal("failed integration left checkout")
	}
	wrong := branches
	wrong[1].BaseSHA = "1111111111111111111111111111111111111111"
	if _, err := IntegrateContributionBundles(ctx, wrong, filepath.Join(root, "wrong"), filepath.Join(root, "wrong.bundle"), 8<<20); err == nil {
		t.Fatal("different base accepted")
	}
}
