package gitwork

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func githubObjectGit(t *testing.T, dir string, input []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func githubRawObjectSHA(kind string, raw []byte) string {
	header := []byte(kind + " " + strconv.Itoa(len(raw)) + "\x00")
	sum := sha1.Sum(append(header, raw...))
	return hex.EncodeToString(sum[:])
}

func requireGitHubObjectSHA(t *testing.T, dir, kind, expected string, raw []byte) {
	t.Helper()
	if actual := githubRawObjectSHA(kind, raw); actual != expected {
		t.Fatalf("serialized %s SHA=%s want=%s raw=%q", kind, actual, expected, raw)
	}
	stored := githubObjectGit(t, dir, nil, "cat-file", kind, expected)
	if !bytes.Equal(stored, raw) {
		t.Fatalf("serialized %s differs from Git object %s", kind, expected)
	}
}

// Write raw commits independently of the serializer so header ordering,
// distinct identities, UTC offsets and message bytes are observable fixtures.
func githubFixtureCommit(t *testing.T, dir, tree string, parents []string, extraHeader, message string) string {
	t.Helper()
	var raw strings.Builder
	fmt.Fprintf(&raw, "tree %s\n", tree)
	for _, parent := range parents {
		fmt.Fprintf(&raw, "parent %s\n", parent)
	}
	raw.WriteString("author Author 名 <author@example.test> 1731655200 +0530\n")
	raw.WriteString("committer Committer 人 <committer@example.test> 1731662400 -0700\n")
	if extraHeader != "" {
		raw.WriteString(extraHeader)
		if !strings.HasSuffix(extraHeader, "\n") {
			raw.WriteByte('\n')
		}
	}
	raw.WriteByte('\n')
	raw.WriteString(message)
	return strings.TrimSpace(string(githubObjectGit(t, dir, []byte(raw.String()), "hash-object", "-t", "commit", "-w", "--stdin")))
}

func githubCheckoutCommit(t *testing.T, dir, sha string) {
	t.Helper()
	githubObjectGit(t, dir, nil, "checkout", "-q", "--detach", sha)
}

// A real two-parent history with binary blobs, Unicode names, executable and
// symlink modes exercises Git tree sorting and exact byte preservation.
func githubObjectsFixture(t *testing.T) (string, string, string) {
	t.Helper()
	dir, base := contributionFixture(t)
	files := map[string][]byte{"a.txt": []byte("first\n"), "dir/binary.dat": {0, 255, 1, 128, 10, 0}, "dir/子.txt": []byte("中文\n"), "dir.c": []byte("sort before directory\n"), "run.sh": []byte("#!/bin/sh\nprintf ok\n")}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir/子.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	githubObjectGit(t, dir, nil, "add", ".")
	tree := strings.TrimSpace(string(githubObjectGit(t, dir, nil, "write-tree")))
	left := githubFixtureCommit(t, dir, tree, []string{base}, "", "left message\n\nbody α\n")
	right := githubFixtureCommit(t, dir, tree, []string{base}, "", "right message without trailing newline")
	merge := githubFixtureCommit(t, dir, tree, []string{left, right}, "", "Merge both writers\n\nexact UTF-8 中文\n")
	githubCheckoutCommit(t, dir, merge)
	return dir, base, merge
}

func TestGitHubContributionObjectsRebuildExactRawSHAs(t *testing.T) {
	dir, base, result := githubObjectsFixture(t)
	objects, err := GitHubContributionObjects(context.Background(), dir, base, result)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	modes := map[string]bool{}
	commits := 0
	for _, object := range objects {
		if _, ok := seen[object.SHA]; ok {
			t.Fatalf("duplicate object %s", object.SHA)
		}
		requireGitHubObjectSHA(t, dir, object.Type, object.SHA, object.Content)
		switch object.Type {
		case "blob":
			if object.Commit != nil || len(object.Entries) != 0 {
				t.Fatal("blob contains another object type")
			}
		case "tree":
			var raw bytes.Buffer
			for _, entry := range object.Entries {
				if seen[entry.SHA] != entry.Type {
					t.Fatalf("tree %s precedes child %s", object.SHA, entry.SHA)
				}
				modes[entry.Mode] = true
				mode := entry.Mode
				if mode == "040000" {
					mode = "40000"
				}
				fmt.Fprintf(&raw, "%s %s\x00", mode, entry.Path)
				sha, err := hex.DecodeString(entry.SHA)
				if err != nil {
					t.Fatal(err)
				}
				raw.Write(sha)
			}
			requireGitHubObjectSHA(t, dir, "tree", object.SHA, raw.Bytes())
		case "commit":
			commits++
			commit := object.Commit
			if commit == nil || seen[commit.Tree] != "tree" {
				t.Fatal("commit precedes tree")
			}
			var raw strings.Builder
			fmt.Fprintf(&raw, "tree %s\n", commit.Tree)
			for _, parent := range commit.Parents {
				if parent != base && seen[parent] != "commit" {
					t.Fatalf("commit precedes parent %s", parent)
				}
				fmt.Fprintf(&raw, "parent %s\n", parent)
			}
			for _, identity := range []struct {
				kind  string
				value GitHubCommitIdentity
			}{{"author", commit.Author}, {"committer", commit.Committer}} {
				date, err := time.Parse(time.RFC3339, identity.value.Date)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&raw, "%s %s <%s> %d %s\n", identity.kind, identity.value.Name, identity.value.Email, date.Unix(), date.Format("-0700"))
			}
			raw.WriteByte('\n')
			raw.WriteString(commit.Message)
			requireGitHubObjectSHA(t, dir, "commit", object.SHA, []byte(raw.String()))
		default:
			t.Fatalf("unexpected object type %q", object.Type)
		}
		seen[object.SHA] = object.Type
	}
	if commits != 3 || seen[result] != "commit" {
		t.Fatalf("commits=%d result=%q", commits, seen[result])
	}
	for _, mode := range []string{"100644", "100755", "120000", "040000"} {
		if !modes[mode] {
			t.Fatalf("missing tree mode %s", mode)
		}
	}
	nochange, err := GitHubContributionObjects(context.Background(), dir, result, result)
	if err != nil || len(nochange) != 0 {
		t.Fatalf("no-change objects=%d err=%v", len(nochange), err)
	}
}

func TestGitHubContributionObjectsRejectUnsupportedCommitBytes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"gpg-signature", func(raw []byte) []byte {
			return bytes.Replace(raw, []byte("\n\n"), []byte("\ngpgsig -----BEGIN PGP SIGNATURE-----\n fake-signature\n -----END PGP SIGNATURE-----\n\n"), 1)
		}},
		{"unknown-header", func(raw []byte) []byte { return bytes.Replace(raw, []byte("\n\n"), []byte("\nx-extra value\n\n"), 1) }},
		{"encoding-header", func(raw []byte) []byte {
			return bytes.Replace(raw, []byte("\n\n"), []byte("\nencoding ISO-8859-1\n\n"), 1)
		}},
		{"leading-zero-timestamp", func(raw []byte) []byte {
			return bytes.Replace(raw, []byte("1731655200 +0530"), []byte("01731655200 +0530"), 1)
		}},
		{"negative-zero-zone", func(raw []byte) []byte { return bytes.Replace(raw, []byte("+0530"), []byte("-0000"), 1) }},
		{"identity-header-order", func(raw []byte) []byte {
			lines := bytes.Split(raw, []byte{'\n'})
			lines[2], lines[3] = lines[3], lines[2]
			return bytes.Join(lines, []byte{'\n'})
		}},
		{"non-utf8-message", func(raw []byte) []byte { return append(raw, 255) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base := contributionFixture(t)
			tree := strings.TrimSpace(string(githubObjectGit(t, dir, nil, "rev-parse", "HEAD^{tree}")))
			commit := githubFixtureCommit(t, dir, tree, []string{base}, "", "message\n")
			raw := githubObjectGit(t, dir, nil, "cat-file", "commit", commit)
			raw = tc.change(raw)
			sha := strings.TrimSpace(string(githubObjectGit(t, dir, raw, "hash-object", "--literally", "-t", "commit", "-w", "--stdin")))
			githubCheckoutCommit(t, dir, sha)
			if _, err := githubCommitObject(raw); err == nil {
				t.Fatal("unreproducible commit accepted by parser")
			}
			if _, err := GitHubContributionObjects(context.Background(), dir, base, sha); err == nil {
				t.Fatal("unreproducible commit exported")
			}
		})
	}
}

func TestGitHubContributionObjectsRejectUnsafeCheckout(t *testing.T) {
	for _, kind := range []string{"dirty-tracked", "dirty-untracked", "wrong-head", "nonancestor-base", "invalid-sha", "symlink-gitdir", "gitfile"} {
		t.Run(kind, func(t *testing.T) {
			dir, base, result := githubObjectsFixture(t)
			switch kind {
			case "dirty-tracked":
				if err := os.WriteFile(filepath.Join(dir, "code.txt"), []byte("dirty\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "dirty-untracked":
				if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("dirty\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-head":
				result = base
			case "nonancestor-base":
				tree := strings.TrimSpace(string(githubObjectGit(t, dir, nil, "rev-parse", "HEAD^{tree}")))
				base = githubFixtureCommit(t, dir, tree, nil, "", "unrelated root\n")
			case "invalid-sha":
				base = "main"
			case "symlink-gitdir", "gitfile":
				gitdir := filepath.Join(dir, ".git")
				actual := filepath.Join(filepath.Dir(dir), "actual-gitdir")
				if err := os.Rename(gitdir, actual); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink-gitdir" {
					if err := os.Symlink(actual, gitdir); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(gitdir, []byte("gitdir: "+actual+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := GitHubContributionObjects(context.Background(), dir, base, result); err == nil {
				t.Fatal("unsafe checkout exported")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir, base, result := githubObjectsFixture(t)
	if _, err := GitHubContributionObjects(ctx, dir, base, result); err == nil {
		t.Fatal("cancelled serialization exported objects")
	}
}

func TestGitHubContributionObjectsRejectSubmodule(t *testing.T) {
	dir, base := contributionFixture(t)
	githubObjectGit(t, dir, nil, "update-index", "--add", "--cacheinfo", "160000,"+base+",submodule")
	if err := os.Mkdir(filepath.Join(dir, "submodule"), 0700); err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(string(githubObjectGit(t, dir, nil, "write-tree")))
	result := githubFixtureCommit(t, dir, tree, []string{base}, "", "submodule commit\n")
	githubCheckoutCommit(t, dir, result)
	if dirty := githubObjectGit(t, dir, nil, "status", "--porcelain"); len(dirty) != 0 {
		t.Fatalf("submodule fixture unexpectedly dirty: %s", dirty)
	}
	if _, err := GitHubContributionObjects(context.Background(), dir, base, result); err == nil || !strings.Contains(err.Error(), "tree mode") {
		t.Fatalf("submodule: %v", err)
	}
}

func TestGitHubContributionObjectsEnforceObjectByteBudget(t *testing.T) {
	dir, base := contributionFixture(t)
	path := filepath.Join(dir, "too-large.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((64 << 20) + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	githubObjectGit(t, dir, nil, "add", "too-large.bin")
	tree := strings.TrimSpace(string(githubObjectGit(t, dir, nil, "write-tree")))
	result := githubFixtureCommit(t, dir, tree, []string{base}, "", "oversized blob\n")
	githubCheckoutCommit(t, dir, result)
	if _, err := GitHubContributionObjects(context.Background(), dir, base, result); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("oversized object: %v", err)
	}
}
