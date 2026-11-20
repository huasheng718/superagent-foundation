package teamconfig

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agentflow/internal/domain"
)

func TestExportPatchPreservesUnselectedTreeAndReimportsExactChanges(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", args[0], err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(p string, b []byte) {
		t.Helper()
		p = filepath.Join(source, p)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(source, "init", "--template=", ".")
	git(source, "config", "user.name", "Local Fixture")
	git(source, "config", "user.email", "fixture@example.invalid")
	write(".teamai/teamai.yaml", []byte("team: example\nrepo: fixture\nmode: self\n"))
	write(".teamai/LICENSE", []byte("Retained original license\n"))
	write(".teamai/rules/review.md", []byte("Original rule\n"))
	write(".teamai/skills/read/SKILL.md", []byte("---\nname: read\ndescription: Read fixture\n---\nRead nested data.\n"))
	write(".teamai/skills/read/nested/data.bin", []byte{0, 255, 128, 7})
	write(".teamai/skills/read/nested/raw.dat", []byte{255, '\n'})
	write(".teamai/skills/read/NOTICE", []byte("Fixture notice\n"))
	write(".teamai/skills/read/.gitignore", []byte("nested/data.bin\n"))
	write("src/keep.bin", []byte{255, 0, 1, 2})
	write(".teamai/agents/unselected.yaml", []byte("tools: [unsupported-for-import]\n"))
	git(source, "add", "--all", "--force")
	git(source, "commit", "-qm", "fixture baseline")
	commit := git(source, "rev-parse", "HEAD")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE", "1")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT", root)
	repository := domain.Repository{ID: "fixture", ProjectID: "project", CloneURL: "file://" + source}
	selection := domain.TeamImportSelection{Rules: []string{"rules/review.md"}, Skills: []string{"skills/read"}}
	files, err := Read(context.Background(), repository, commit, ".teamai", selection)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := Parse("project", repository, commit, ".teamai", selection, files)
	if err != nil {
		t.Fatal(err)
	}
	refPreview, err := Parse("project", repository, commit, ".teamai", selection, files, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	refRelease := domain.TeamRelease{ID: "ref-approved", ProjectID: "project", State: domain.TeamReleasePublished, SHA256: refPreview.SHA256, Payload: refPreview.Payload}
	refExport, err := Export(context.Background(), repository, refRelease, []Replacement{{Path: "rules/review.md", Content: []byte("Ref update\n")}})
	if err != nil || refExport.Proposed.Payload.GitImport.Ref != "refs/heads/main" || refExport.ReleaseSHA256 != refRelease.SHA256 {
		t.Fatalf("ref-bound source could not export: %v", err)
	}
	var governed domain.TeamRelease
	governedRaw, _ := json.Marshal(refRelease)
	if err := json.Unmarshal(governedRaw, &governed); err != nil {
		t.Fatal(err)
	}
	policy := domain.DistributionPolicy{Copy: "deny", Edit: "allow", Export: "allow"}
	governed.Payload.Assets.Rules[0].Distribution = &policy
	governed.SHA256, _ = domain.IntelligenceDigest(governed.Payload)
	governedChanges := []Replacement{{Path: "rules/review.md", Content: []byte("Governed update\n")}}
	governedExport, err := Export(context.Background(), repository, governed, governedChanges)
	if err != nil || governedExport.Proposed.Payload.Assets.Rules[0].Distribution == nil || governedExport.Proposed.Payload.Assets.Rules[0].Distribution.Copy != "deny" {
		t.Fatalf("native frozen policy was dropped or treated as a Git mismatch: %v", err)
	}
	if _, err := PrepareProposal(context.Background(), repository, governed, governedChanges, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"edit", "export"} {
		denied := policy
		if operation == "edit" {
			denied.Edit = "deny"
		} else {
			denied.Export = "deny"
		}
		governed.Payload.Assets.Rules[0].Distribution = &denied
		governed.SHA256, _ = domain.IntelligenceDigest(governed.Payload)
		if _, err := Export(context.Background(), repository, governed, governedChanges); !errors.Is(err, domain.ErrDistributionDenied) {
			t.Fatalf("frozen %s denial was bypassed: %v", operation, err)
		}
	}
	release := domain.TeamRelease{ID: "approved", ProjectID: "project", State: domain.TeamReleasePublished, SHA256: preview.SHA256, Payload: preview.Payload}
	changes := []Replacement{{Path: "rules/review.md", Content: []byte("Updated rule\n")}, {Path: "skills/read/nested/data.bin", Content: []byte{0, 255, 128, 42, 19}, Executable: true}, {Path: "skills/read/nested/raw.dat", Content: []byte{254, '\n'}}}
	result, err := Export(context.Background(), repository, release, changes)
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseCommit != commit || result.ReleaseSHA256 != release.SHA256 || !strings.Contains(result.Patch, "GIT binary patch") || !strings.Contains(result.Patch, "a/.teamai/rules/review.md") || !strings.Contains(result.Patch, "new mode 100755") {
		t.Fatal("missing exact-base binary/path/mode patch evidence")
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var transported ExportPreview
	if err := json.Unmarshal(wire, &transported); err != nil || transported.Patch != result.Patch {
		t.Fatal("JSON transport changed patch bytes", err)
	}
	copyDir := filepath.Join(root, "apply")
	git(root, "clone", "--no-local", "--", repository.CloneURL, copyDir)
	patch := filepath.Join(root, "proposal.patch")
	if err := os.WriteFile(patch, []byte(transported.Patch), 0600); err != nil {
		t.Fatal(err)
	}
	git(copyDir, "apply", "--check", "--index", patch)
	git(copyDir, "apply", "--index", patch)
	if got := git(copyDir, "diff", "--cached", "--name-only"); got != ".teamai/rules/review.md\n.teamai/skills/read/nested/data.bin\n.teamai/skills/read/nested/raw.dat" {
		t.Fatalf("unexpected tree edits: %s", got)
	}
	kept, _ := os.ReadFile(filepath.Join(copyDir, "src/keep.bin"))
	if !bytes.Equal(kept, []byte{255, 0, 1, 2}) {
		t.Fatal("unselected binary changed")
	}
	if git(copyDir, "hash-object", ".teamai/agents/unselected.yaml") != git(source, "hash-object", ".teamai/agents/unselected.yaml") {
		t.Fatal("unselected unsupported configuration changed")
	}
	git(copyDir, "-c", "user.name=Local Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "apply proposal locally")
	newCommit := git(copyDir, "rev-parse", "HEAD")
	repository.CloneURL = "file://" + copyDir
	reimport, err := Read(context.Background(), repository, newCommit, ".teamai", selection)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse("project", repository, newCommit, ".teamai", selection, reimport)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Payload.Assets.Rules[0].Content != "Updated rule\n" || !bytes.Equal(reimport["skills/read/nested/data.bin"].Content, changes[1].Content) || reimport["skills/read/nested/data.bin"].Mode != "100755" {
		t.Fatal("roundtrip lost changed bytes/mode")
	}
	if !bytes.Equal(reimport["skills/read/nested/raw.dat"].Content, changes[2].Content) {
		t.Fatal("JSON patch/application/reimport lost non-UTF8 bytes")
	}
	if git(source, "rev-parse", "HEAD") != commit || git(source, "status", "--porcelain") != "" {
		t.Fatal("export mutated original source")
	}
	repository.CloneURL = release.Payload.Source.Repository
	for _, bad := range [][]Replacement{
		nil,
		{{Path: "rules/review.md", Content: []byte("Original rule\n")}},
		{{Path: "src/keep.bin", Content: []byte("bad")}}, {{Path: "../escape", Content: []byte("bad")}}, {{Path: "teamai.yaml", Content: []byte("bad")}},
		{{Path: "LICENSE", Content: []byte("new license")}},
		{{Path: "rules/review.md", Content: []byte("---\npaths: '**'\n---\nscoped")}},
		{{Path: "rules/review.md", Content: []byte("one")}, {Path: "rules/review.md", Content: []byte("two")}},
	} {
		if _, err := Export(context.Background(), repository, release, bad); err == nil {
			t.Fatal("invalid export accepted")
		}
	}
	tampered := release
	tampered.SHA256 = strings.Repeat("0", 64)
	if _, err := Export(context.Background(), repository, tampered, changes); err == nil {
		t.Fatal("stale source digest accepted")
	}
	revoked := release
	revoked.State = domain.TeamReleaseRevoked
	if _, err := Export(context.Background(), repository, revoked, changes); err == nil {
		t.Fatal("revoked source release exported")
	}
}

func TestExportPatchPreservesRawBytesDespiteSourceAttributes(t *testing.T) {
	attributes := File{TeamImportFile: domain.TeamImportFile{Mode: "100644"}, Content: []byte("*.txt text eol=lf\n")}
	before := map[string]File{
		"skills/read/.gitattributes": attributes,
		"skills/read/raw.txt":        {TeamImportFile: domain.TeamImportFile{Mode: "100644"}, Content: []byte("old\r\n")},
	}
	after := map[string]File{
		"skills/read/.gitattributes": attributes,
		"skills/read/raw.txt":        {TeamImportFile: domain.TeamImportFile{Mode: "100644"}, Content: []byte("new\r\n")},
	}
	patch, err := exportPatch(context.Background(), ".", before, after)
	if err != nil {
		t.Fatal(err)
	}
	blob := func(b []byte) string {
		return fmt.Sprintf("%x", sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(b))), b...)))
	}
	if !strings.Contains(string(patch), "index "+blob(before["skills/read/raw.txt"].Content)+".."+blob(after["skills/read/raw.txt"].Content)) {
		t.Fatalf("Git attributes changed selected byte identities: %s", patch)
	}
}

func TestExportPatchCanRoundtripNonUTF8WithoutNULThroughJSON(t *testing.T) {
	before := map[string]File{"skills/read/raw.bin": {TeamImportFile: domain.TeamImportFile{Mode: "100644"}, Content: []byte{0xff, '\n'}}}
	after := map[string]File{"skills/read/raw.bin": {TeamImportFile: domain.TeamImportFile{Mode: "100644"}, Content: []byte{0xfe, '\n'}}}
	patch, err := exportPatch(context.Background(), ".", before, after)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(patch) {
		t.Fatal("non-UTF8 text diff would lose patch bytes in JSON string encoding")
	}
}
