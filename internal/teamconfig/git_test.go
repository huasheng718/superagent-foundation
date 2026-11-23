package teamconfig

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "user.name=Import fixture", "-c", "user.email=import@example.test", "-c", "core.hooksPath=/dev/null"}, args...)...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("local fixture git %s: %v: %s", args[0], err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestImportReadExactCommittedGitObjectsAndModes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "registered")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r, selection, source := importFixture()
	fixtureGit(t, dir, "init", "--template=", ".")
	for p, file := range source {
		local := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(local), 0700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0600)
		if file.Mode == "100755" {
			mode = 0700
		}
		if err := os.WriteFile(local, file.Content, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "unselected-private.txt"), []byte("not selected"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, dir, "add", "--all")
	fixtureGit(t, dir, "commit", "-m", "Synthetic TeamAI source")
	commit := fixtureGit(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "rules/style.md"), []byte("uncommitted future change"), 0600); err != nil {
		t.Fatal(err)
	}
	r.CloneURL = (&url.URL{Scheme: "file", Path: dir}).String()
	t.Setenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE", "1")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT", root)
	files, err := Read(context.Background(), r, commit, ".", selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(source) {
		t.Fatal("unselected file leaked or selected resource omitted")
	}
	for p, expected := range source {
		got, ok := files[p]
		if !ok || got.TeamImportFile != expected.TeamImportFile || !bytes.Equal(got.Content, expected.Content) {
			t.Fatalf("committed blob/hash/mode not preserved: %s", p)
		}
	}
	if _, err := Parse("project", r, commit, ".", selection, files); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(context.Background(), r, strings.Repeat("a", 40), ".", selection); !errors.Is(err, ErrSource) {
		t.Fatalf("missing exact commit should give sanitized source error: %v", err)
	}
	t.Log("real temporary Git source: selected committed blobs, SHA-1/SHA-256, executable mode and binary bytes preserved; dirty worktree ignored")
}

func TestImportReadSelfRootAndOrdinaryMissingEntry(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "self")
	if err := os.MkdirAll(filepath.Join(dir, ".teamai/rules"), 0700); err != nil {
		t.Fatal(err)
	}
	for p, content := range map[string]string{".teamai/teamai.yaml": "team: Self\nrepo: https://example.test/self.git\nmode: self\n", ".teamai/rules/simple.md": "Rule."} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixtureGit(t, dir, "init", "--template=", ".")
	fixtureGit(t, dir, "add", "--all")
	fixtureGit(t, dir, "commit", "-m", "Self mode fixture")
	commit := fixtureGit(t, dir, "rev-parse", "HEAD")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE", "1")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT", root)
	r := domain.Repository{ID: "self", ProjectID: "project", CloneURL: (&url.URL{Scheme: "file", Path: dir}).String()}
	selection := domain.TeamImportSelection{Rules: []string{"rules/simple.md"}}
	files, err := Read(context.Background(), r, commit, ".teamai", selection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse("project", r, commit, ".teamai", selection, files); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(context.Background(), r, commit, "..", selection); err == nil {
		t.Fatal("parent root accepted")
	}
}

func TestImportSourceURLContract(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "registered")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	local := (&url.URL{Scheme: "file", Path: child}).String()
	if err := ValidateURL(local, root, true); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://example.test/source.git", "ssh://example.test/source.git", "https://token@example.test/source.git", "https://example.test/source.git?token=secret", "https://example.test/source.git#main", child} {
		if err := ValidateURL(raw, root, true); err == nil {
			t.Fatalf("unsupported registered URL accepted: %s", raw)
		}
	}
	if err := ValidateURL(local, root, false); err == nil {
		t.Fatal("local fixture accepted without deployment opt-in")
	}
	if err := ValidateURL((&url.URL{Scheme: "file", Path: root}).String(), root, true); err == nil {
		t.Fatal("fixture root itself accepted as registered child")
	}
}
