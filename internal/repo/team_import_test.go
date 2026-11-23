package repo

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/teamconfig"
)

func teamUpdateRepoPayload(t *testing.T, repository domain.Repository, commit, ref string) domain.TeamReleasePayload {
	t.Helper()
	file := func(path, content string) teamconfig.File {
		return teamconfig.File{TeamImportFile: domain.TeamImportFile{Path: path, Mode: "100644", BlobSHA: strings.Repeat("b", 40), SHA256: strings.Repeat("c", 64), Size: int64(len(content))}, Content: []byte(content)}
	}
	files := map[string]teamconfig.File{"teamai.yaml": file("teamai.yaml", "team: Fixture\nrepo: fixture\n"), "rules/check.md": file("rules/check.md", "Reviewed source "+commit)}
	preview, err := teamconfig.Parse(repository.ProjectID, repository, commit, ".", domain.TeamImportSelection{Rules: []string{"rules/check.md"}}, files, ref)
	if err != nil {
		t.Fatal(err)
	}
	return preview.Payload
}

func TestTeamImportUpdateTransactionGuards(t *testing.T) {
	for _, scenario := range []string{"accepted", "branch-changed", "url-changed", "source-revoked", "source-digest", "selection-changed", "base-stale", "update-required", "distribution-stripped", "distribution-retained", "distribution-edit-denied"} {
		t.Run(scenario, func(t *testing.T) {
			store := intelligenceStore(t)
			repository := domain.Repository{ID: "repo-update", ProjectID: "team-a", CloneURL: "https://example.test/source.git", DefaultBranch: "main"}
			if err := store.PutRepository(repository); err != nil {
				t.Fatal(err)
			}
			original := teamUpdateRepoPayload(t, repository, strings.Repeat("a", 40), "")
			if strings.HasPrefix(scenario, "distribution-") {
				original.Assets.Rules[0].Distribution = &domain.DistributionPolicy{Copy: "allow", Edit: "allow", Export: "deny"}
				if scenario == "distribution-edit-denied" {
					original.Assets.Rules[0].Distribution.Edit = "deny"
				}
			}
			first, err := store.CreateImportedTeamRelease("team-a", "old", "owner", 0, original)
			if err != nil {
				t.Fatal(err)
			}
			payload := teamUpdateRepoPayload(t, repository, strings.Repeat("d", 40), "refs/heads/main")
			from := domain.TeamImportUpdateFrom{ReleaseID: first.ID, ReleaseSHA256: first.SHA256, Ref: "refs/heads/main"}
			updates := []domain.TeamImportUpdateFrom{from}
			base := 1
			expected := ErrIntelligenceConflict
			switch scenario {
			case "distribution-edit-denied":
				expected = domain.ErrDistributionDenied
			case "branch-changed":
				repository.DefaultBranch = "new"
				expected = ErrTeamImportRefConflict
			case "url-changed":
				repository.CloneURL = "https://example.test/changed.git"
				expected = ErrTeamImportRefConflict
			case "source-revoked":
				if _, err := store.RevokeTeamRelease("team-a", first.ID, 1, "owner", "review revoked"); err != nil {
					t.Fatal(err)
				}
			case "source-digest":
				updates[0].ReleaseSHA256 = strings.Repeat("0", 64)
			case "selection-changed":
				payload.GitImport.Selection.Bindings = map[string]domain.TeamImportBindings{"unreviewed": {}}
			case "base-stale":
				base = 0
			case "update-required":
				updates = nil
			case "distribution-retained":
				if err := domain.PreserveTeamDistribution(first.Payload, &payload); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.PutRepository(repository); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal([]any{store.Agents(), store.Rules(), store.Skills(), store.MCPServers()})
			release, err := store.CreateImportedTeamRelease("team-a", "update", "owner", base, payload, updates...)
			if scenario == "accepted" || scenario == "distribution-retained" {
				if err != nil || release.Version != 2 || release.Payload.GitImport.Ref != from.Ref {
					t.Fatalf("accepted fixed update: %v", err)
				}
			} else if scenario == "update-required" || scenario == "selection-changed" {
				if err == nil {
					t.Fatal("invalid update source accepted")
				}
			} else if !errors.Is(err, expected) {
				t.Fatalf("guard %s: want %v got %v", scenario, expected, err)
			}
			releases, _ := store.TeamReleases("team-a")
			count := 1
			if scenario == "accepted" || scenario == "distribution-retained" {
				count = 2
			}
			if len(releases) != count {
				t.Fatal("rejected update wrote release")
			}
			after, _ := json.Marshal([]any{store.Agents(), store.Rules(), store.Skills(), store.MCPServers()})
			if string(before) != string(after) {
				t.Fatal("update wrote editable catalog")
			}
		})
	}
}
