package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

const TeamGitImportFormat = "tencent-teamai-basic-v1@2660cfce1c2ff9c8c00c737f6c24ad25f76ba17f"

type TeamImportMCP struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type TeamImportBindings struct {
	Rules      []string `json:"rules,omitempty"`
	Skills     []string `json:"skills,omitempty"`
	MCPServers []string `json:"mcpServers,omitempty"`
}

type TeamImportSelection struct {
	Agents     []string                      `json:"agents"`
	Rules      []string                      `json:"rules"`
	Skills     []string                      `json:"skills"`
	MCPServers []TeamImportMCP               `json:"mcpServers"`
	Bindings   map[string]TeamImportBindings `json:"bindings,omitempty"`
}

type TeamImportFile struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	BlobSHA string `json:"blobSha"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// TeamGitImport records the source objects actually read, not machine-local
// TeamAI delivery state or permission to read an arbitrary repository.
type TeamGitImport struct {
	Format       string              `json:"format"`
	RepositoryID string              `json:"repositoryId"`
	Commit       string              `json:"commit"`
	Ref          string              `json:"ref,omitempty"`
	Root         string              `json:"root"`
	Selection    TeamImportSelection `json:"selection"`
	Files        []TeamImportFile    `json:"files"`
}

// TeamImportUpdateFrom pins the reviewed release and registered heads ref.
// It is an approval precondition, never a dynamic execution source.
type TeamImportUpdateFrom struct {
	ReleaseID     string `json:"releaseId"`
	ReleaseSHA256 string `json:"releaseSha256"`
	Ref           string `json:"ref"`
}

// EqualTeamImportSelection compares canonical selection wire values. Empty
// optional bindings have the same meaning after a persisted JSON round trip.
func EqualTeamImportSelection(a, b TeamImportSelection) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func ValidTeamImportRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return false
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if branch == "" || len(branch) > 255 || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "refs/") || strings.HasSuffix(branch, ".") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") || strings.ContainsAny(branch, " ~^:?*[\\") {
		return false
	}
	for _, c := range branch {
		if c < 32 || c == 127 {
			return false
		}
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func ValidTeamImportPath(v string) bool {
	return v != "" && v != "." && v != ".." && len(v) <= 512 && path.Clean(v) == v && !strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "../") && !strings.ContainsAny(v, "\\\x00\r\n\t")
}

func ValidateTeamGitImport(v *TeamGitImport, source IntelligenceSource) error {
	if v == nil {
		return nil
	}
	if (v.Ref != "" && !ValidTeamImportRef(v.Ref)) || v.Format != TeamGitImportFormat || v.RepositoryID == "" || !ValidGitCommitSHA(v.Commit) || v.Commit != source.Commit || v.Root != source.Path || (v.Root != "." && !ValidTeamImportPath(v.Root)) || len(v.Files) == 0 || len(v.Files) > 1024 {
		return fmt.Errorf("invalid Git import provenance")
	}
	previous := ""
	var total int64
	for _, f := range v.Files {
		if !ValidTeamImportPath(f.Path) || f.Path <= previous || (f.Mode != "100644" && f.Mode != "100755") || !ValidGitCommitSHA(f.BlobSHA) || len(f.SHA256) != 64 || f.Size < 0 || f.Size > 1<<20 {
			return fmt.Errorf("invalid Git import source file manifest")
		}
		for _, c := range f.SHA256 {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return fmt.Errorf("invalid Git import source digest")
			}
		}
		previous = f.Path
		total += f.Size
	}
	if total > 6<<20 {
		return fmt.Errorf("Git import source exceeds snapshot budget")
	}
	return nil
}
