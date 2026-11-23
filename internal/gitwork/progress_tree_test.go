package gitwork

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProgressTreeTracksContentAndIgnoresControlNoise(t *testing.T) {
	dir, head := contributionFixture(t)
	ctx := context.Background()
	indexPath := filepath.Join(dir, ".git", "index")
	before, _ := os.ReadFile(indexPath)
	tree, err := ProgressTreeSHA(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".agentflow/report.json", ".env"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("changed control only"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	same, err := ProgressTreeSHA(ctx, dir)
	if err != nil || same != tree {
		t.Fatal("control noise changed fingerprint", err, tree, same)
	}
	if _, err := C(ctx, dir, "commit", "--allow-empty", "-qm", "new claim without code"); err != nil {
		t.Fatal(err)
	}
	same, err = ProgressTreeSHA(ctx, dir)
	if err != nil || same != tree {
		t.Fatal("empty commit faked progress", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code.txt"), []byte("different contents, same filename\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := ProgressTreeSHA(ctx, dir)
	if err != nil || changed == tree {
		t.Fatal("content change ignored", err)
	}
	if err := os.Remove(filepath.Join(dir, "code.txt")); err != nil {
		t.Fatal(err)
	}
	deleted, err := ProgressTreeSHA(ctx, dir)
	if err != nil || deleted == changed || deleted == tree {
		t.Fatal("deletion ignored", err)
	}
	after, _ := os.ReadFile(indexPath)
	// Empty commits do not modify index content; all progress snapshots use private indexes.
	if string(before) != string(after) {
		t.Fatal("user index was mutated")
	}
	now, _ := RevParse(ctx, dir, "HEAD")
	if now == head {
		t.Fatal("fixture empty commit did not run")
	}
	again, _ := RevParse(ctx, dir, "HEAD")
	if again != now {
		t.Fatal("progress snapshot changed HEAD")
	}
}

func TestProgressTreeReadsHeadIndependentlyOfUserStaging(t *testing.T) {
	dir, _ := contributionFixture(t)
	ctx := context.Background()
	tree, err := ProgressTreeSHA(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := C(ctx, dir, "rm", "code.txt"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := ProgressTreeSHA(ctx, dir)
	if err != nil || deleted == tree {
		t.Fatal("staged deletion ignored", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil || string(before) != string(after) {
		t.Fatal("staged deletion was changed", err)
	}
}
