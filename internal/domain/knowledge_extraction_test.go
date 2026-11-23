package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func extractionJSONFixture() string {
	return `{"schemaVersion":1,"suggestions":[{"key":"m","kind":"memory","title":"Title","content":"Reviewed later","evidencePaths":["src/main.go"],"licenseClaim":"unreviewed"},{"key":"s","kind":"skill","skillId":"extracted-skill","description":"Review changes","files":{"SKILL.md":"---\nname: extracted-skill\ndescription: Review changes\n---\nReview changes.\n","NOTICE.md":"Generated; human review required."},"noticePaths":["NOTICE.md"],"evidencePaths":[],"licenseClaim":"unreviewed"}]}`
}
func TestKnowledgeExtractionBoundedStrictArtifact(t *testing.T) {
	raw := extractionJSONFixture()
	v, err := ParseKnowledgeSuggestions([]byte(raw), "p")
	if err != nil || len(v.Suggestions) != 2 {
		t.Fatalf("valid artifact: %v", err)
	}
	for name, input := range map[string]string{
		"duplicate-root":       strings.Replace(raw, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1),
		"duplicate-file":       strings.Replace(raw, `"NOTICE.md":"Generated; human review required."`, `"NOTICE.md":"Generated; human review required.","NOTICE.md":"overwritten"`, 1),
		"unknown":              strings.Replace(raw, `"schemaVersion":1`, `"schemaVersion":1,"actor":"owner"`, 1),
		"forged-model":         strings.Replace(raw, `"key":"m"`, `"key":"m","model":"trusted"`, 1),
		"trailing":             raw + `{}`,
		"absolute":             strings.Replace(raw, "src/main.go", "/src/main.go", 1),
		"escape":               strings.Replace(raw, "src/main.go", "../main.go", 1),
		"duplicate-item":       strings.Replace(raw, `"key":"s"`, `"key":"m"`, 1),
		"unclosed-frontmatter": strings.Replace(raw, `name: extracted-skill`, `name: other`, 1),
		"oversize":             strings.Repeat(" ", KnowledgeExtractionMaxBytes) + raw,
		"invalid-utf8":         string([]byte{255}),
		"mixed-fields":         strings.Replace(raw, `"key":"m"`, `"key":"m","skillId":"forged"`, 1),
		"file-path":            strings.Replace(raw, `"NOTICE.md":"Generated; human review required."`, `"../NOTICE.md":"Generated; human review required."`, 1),
		"binary":               strings.Replace(raw, `"files":{`, `"binary":{},"files":{`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKnowledgeSuggestions([]byte(input), "p"); err == nil {
				t.Fatal("invalid artifact accepted")
			}
		})
	}
	var shape KnowledgeSuggestionArtifact
	_ = json.Unmarshal([]byte(raw), &shape)
	shape.Suggestions[0].Content = strings.Repeat("x", MemoryMaxContentBytes+1)
	b, _ := json.Marshal(shape)
	if _, err := ParseKnowledgeSuggestions(b, "p"); err == nil {
		t.Fatal("oversized memory accepted")
	}
}
func TestKnowledgeExtractionRequestAndLegacySourceDigest(t *testing.T) {
	request := KnowledgeExtractionRequest{RunID: "r", NodeID: "n", ArtifactID: "a", RepositoryID: "repo", ExpectedArtifactSHA256: strings.Repeat("a", 64), ExpectedCommit: strings.Repeat("1", 40)}
	raw, _ := json.Marshal(request)
	if _, err := ParseKnowledgeExtractionRequest(raw); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{`,"content":"forged"}`, `,"runId":"other"}`, `,"expectedArtifactSHA256":"bad"}`} {
		input := strings.TrimSuffix(string(raw), "}") + extra
		if _, err := ParseKnowledgeExtractionRequest([]byte(input)); err == nil {
			t.Fatal("forged or ambiguous request accepted")
		}
	}
	old := GovernedMemory{Title: "Title", Content: "Content", Source: IntelligenceSource{RunID: "r", ArtifactID: "a", Repository: "https://example.com/r", Commit: strings.Repeat("1", 40), Path: "p"}}
	// Frozen legacy projection, independent of the newly extended source struct.
	expected, err := IntelligenceDigest(json.RawMessage(`{"title":"Title","content":"Content","source":{"runId":"r","artifactId":"a","repository":"https://example.com/r","commit":"1111111111111111111111111111111111111111","path":"p"}}`))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := MemoryContentDigest(old)
	if before != expected {
		t.Fatal("empty new source field changed historical digest")
	}
	old.Source.ExtractionID = strings.Repeat("e", 64)
	after, _ := MemoryContentDigest(old)
	if before == after {
		t.Fatal("extraction association omitted from content identity")
	}
	if err := ValidateIntelligenceSource(old.Source); err != nil {
		t.Fatal(err)
	}
}
