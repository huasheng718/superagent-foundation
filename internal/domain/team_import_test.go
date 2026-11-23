package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestTeamImportLegacyReleaseWireDigestUnchanged(t *testing.T) {
	// Historical wire data predates gitImport and sourceRevision. Hash checks
	// must survive decoding/re-encoding through the new payload definitions.
	const oldJSON = `{"agents":[{"id":"reviewer","name":"Reviewer","version":1,"projectId":"team","description":"Review","prompt":"Read the diff.","rules":["style"],"skills":["check"],"mcpServers":["format"],"loop":{"enabled":false,"maxIterations":0,"exitCondition":"","onExhausted":""},"verification":null,"updatedAt":"0001-01-01T00:00:00Z"}],"assets":{"rules":[{"id":"style","name":"Style","projectId":"team","description":"Style rules","content":"Simple.","priority":0,"updatedAt":"0001-01-01T00:00:00Z"}],"skills":[{"id":"check","name":"Check","projectId":"team","description":"Check files","content":"Reviewed instructions.","updatedAt":"0001-01-01T00:00:00Z"}],"mcpServers":[{"id":"format","name":"Format","projectId":"team","scope":"project","description":"Format","transport":"stdio","command":"fixture-not-executed","timeoutSec":30,"updatedAt":"0001-01-01T00:00:00Z"}]},"source":{"path":"docs/approved.md"}}`
	var payload TeamReleasePayload
	if err := json.Unmarshal([]byte(oldJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTeamReleasePayload(payload, "team"); err != nil {
		t.Fatal(err)
	}
	current, err := json.Marshal(payload)
	if err != nil || !bytes.Equal(current, []byte(oldJSON)) {
		t.Fatalf("legacy payload wire shape changed: %s (%v)", current, err)
	}
	sum := sha256.Sum256([]byte(oldJSON))
	digest, err := IntelligenceDigest(payload)
	if err != nil || digest != hex.EncodeToString(sum[:]) || payload.GitImport != nil {
		t.Fatal("historical payload digest changed")
	}
	payload.Agents[0].SourceRevision = "reviewed-source-revision"
	changed, err := IntelligenceDigest(payload)
	if err != nil || changed == digest {
		t.Fatal("explicit source revision omitted from new release identity")
	}
}

func TestTeamImportManifestAndPathValidation(t *testing.T) {
	for _, p := range []string{"..", "../file", "a/../file", "/file", "a\\b", "a\nfile", "a//b", ""} {
		if ValidTeamImportPath(p) {
			t.Fatalf("invalid import path accepted: %q", p)
		}
	}
	source := IntelligenceSource{Repository: "https://example.test/source.git", Commit: strings.Repeat("a", 40), Path: "."}
	manifest := func() *TeamGitImport {
		return &TeamGitImport{Format: TeamGitImportFormat, RepositoryID: "registered", Commit: source.Commit, Root: ".", Files: []TeamImportFile{{Path: "teamai.yaml", Mode: "100644", BlobSHA: strings.Repeat("b", 40), SHA256: strings.Repeat("c", 64), Size: 10}}}
	}
	if err := ValidateTeamGitImport(manifest(), source); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*TeamGitImport){
		func(m *TeamGitImport) { m.Commit = strings.Repeat("d", 40) },
		func(m *TeamGitImport) { m.Files[0].Mode = "120000" },
		func(m *TeamGitImport) { m.Files[0].SHA256 = strings.Repeat("z", 64) },
		func(m *TeamGitImport) { m.Files[0].Size = (1 << 20) + 1 },
		func(m *TeamGitImport) { m.Files = append(m.Files, m.Files[0]) },
	} {
		m := manifest()
		change(m)
		if err := ValidateTeamGitImport(m, source); err == nil {
			t.Fatal("inconsistent source manifest accepted")
		}
	}
}
