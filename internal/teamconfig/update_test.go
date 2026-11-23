package teamconfig

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestTeamUpdateResolvesExactRegisteredHeadsAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "source")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, dir, "init", "--template=", ".")
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, dir, "add", "fixture")
	fixtureGit(t, dir, "commit", "-m", "original")
	old := fixtureGit(t, dir, "rev-parse", "HEAD")
	fixtureGit(t, dir, "branch", "topic/config", old)
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, dir, "commit", "-am", "new HEAD and same-name tag")
	next := fixtureGit(t, dir, "rev-parse", "HEAD")
	fixtureGit(t, dir, "tag", "topic/config", next)
	t.Setenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE", "1")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT", root)
	repository := domain.Repository{CloneURL: (&url.URL{Scheme: "file", Path: dir}).String(), DefaultBranch: "topic/config"}
	ref, commit, err := ResolveRegisteredBranch(context.Background(), repository)
	if err != nil || ref != "refs/heads/topic/config" || commit != old {
		t.Fatalf("HEAD/tag substituted for exact registered heads: %q %q %v", ref, commit, err)
	}
	fixtureGit(t, dir, "update-ref", ref, next)
	_, commit, err = ResolveRegisteredBranch(context.Background(), repository)
	if err != nil || commit != next {
		t.Fatalf("branch update not observed: %v", err)
	}
	fixtureGit(t, dir, "update-ref", "-d", ref)
	if _, _, err = ResolveRegisteredBranch(context.Background(), repository); !errors.Is(err, ErrRefConflict) {
		t.Fatalf("missing branch fell back to tag/HEAD: %v", err)
	}
	for _, branch := range []string{"", "refs/heads/main", "-main", "@{-1}", "main\nprivate", "main*", "../main", "a//b", "main.lock"} {
		invalid := repository
		invalid.DefaultBranch = branch
		if _, _, err = ResolveRegisteredBranch(context.Background(), invalid); err == nil {
			t.Fatalf("invalid branch accepted: %q", branch)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = ResolveRegisteredBranch(ctx, repository); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatal("canceled branch read leaked source or succeeded")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err = ResolveRegisteredBranch(context.Background(), repository); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatal("unavailable source leaked local path or succeeded")
	}
}

func TestTeamUpdateStrictRefRecords(t *testing.T) {
	ref := "refs/heads/main"
	sha := strings.Repeat("a", 40)
	for _, out := range []string{"", sha + "\tHEAD\n", sha + "\trefs/tags/main\n", sha + "\trefs/heads/prefix/refs/heads/main\n", sha + "\t" + ref + "\n" + sha + "\t" + ref + "\n", strings.ToUpper(sha) + "\t" + ref + "\n", sha[:12] + "\t" + ref + "\n", sha + " " + ref + "\n", sha + "\t" + ref + "\r\n"} {
		if _, err := parseRegisteredRef([]byte(out), ref); !errors.Is(err, ErrSource) {
			t.Fatalf("ambiguous record accepted: %q", out)
		}
	}
	if got, err := parseRegisteredRef([]byte(sha+"\t"+ref+"\n"), ref); err != nil || got != sha {
		t.Fatal("valid exact record rejected")
	}
}

func TestTeamUpdateRefDigestAndSourceChanges(t *testing.T) {
	repository, selection, files := importFixture()
	old, err := Parse("project", repository, strings.Repeat("a", 40), ".", selection, files)
	if err != nil {
		t.Fatal(err)
	}
	same, err := Parse("project", repository, strings.Repeat("a", 40), ".", selection, files, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(old.Payload.GitImport)
	if old.SHA256 != same.SHA256 || strings.Contains(string(raw), `"ref"`) {
		t.Fatal("optional ref changed historical import digest/wire")
	}
	withRef, err := Parse("project", repository, strings.Repeat("a", 40), ".", selection, files, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if withRef.SHA256 == old.SHA256 || withRef.Payload.GitImport.Ref != "refs/heads/main" {
		t.Fatal("reviewed ref is absent from immutable identity")
	}
	before := []domain.TeamImportFile{{Path: "b", Mode: "100644", Size: 1}, {Path: "a", Size: 1}}
	after := []domain.TeamImportFile{{Path: "b", Mode: "100755", Size: 1}, {Path: "c", Size: 1}}
	changes := Changes(before, after)
	if len(changes) != 3 || changes[0].Path != "a" || changes[0].Kind != "removed" || changes[0].After != nil || changes[1].Kind != "changed" || changes[1].Before.Mode != "100644" || changes[1].After.Mode != "100755" || changes[2].Kind != "added" || changes[2].Before != nil {
		t.Fatalf("wrong source comparison: %#v", changes)
	}
}
