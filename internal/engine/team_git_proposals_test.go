package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/teamconfig"
)

type teamProposalFixture struct {
	e        *Engine
	s        *repo.Store
	source   string
	release  domain.TeamRelease
	proposal domain.TeamGitProposal
	git      func(...string) string
}

func newTeamProposalFixture(t *testing.T) teamProposalFixture {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = source
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v %.512s", args[0], err, raw)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "--template=", "-b", "main")
	for p, raw := range map[string][]byte{".teamai/teamai.yaml": []byte("team: fixture\nrepo: fixture\nmode: self\n"), ".teamai/LICENSE": []byte("Original fixture license\n"), ".teamai/rules/review.md": []byte("Original rule\n"), ".teamai/skills/read/SKILL.md": []byte("---\nname: read\ndescription: Read fixture\n---\nRead nested resources.\n"), ".teamai/skills/read/.gitignore": []byte("nested/data.bin\n"), ".teamai/skills/read/nested/data.bin": {0, 255, 128, 7}, ".teamai/skills/read/nested/raw.dat": {255, '\n'}, "src/keep.bin": {0, 255, 4}} {
		p = filepath.Join(source, p)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "--all", "--force")
	git("commit", "-qm", "fixture source")
	base := git("rev-parse", "HEAD")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_ALLOW_FILE", "1")
	t.Setenv("AGENTFLOW_TEAM_IMPORT_FILE_ROOT", root)
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err = s.PutUser(domain.User{LoginName: "team-proposal-owner", GitToken: "fixture-controller-only"}); err != nil {
		t.Fatal(err)
	}
	owner, _ := s.UserByLoginName("team-proposal-owner")
	if err = s.PutProject(domain.Project{ID: "team-proposal", Name: "Fixture", OwnerID: owner.ID}); err != nil {
		t.Fatal(err)
	}
	r := domain.Repository{ID: "team-source", ProjectID: "team-proposal", Provider: "github", Name: "fixture", GongfengProjectID: "github:example/repo", CloneURL: "file://" + source, DefaultBranch: "main"}
	if err = s.PutRepository(r); err != nil {
		t.Fatal(err)
	}
	selection := domain.TeamImportSelection{Rules: []string{"rules/review.md"}, Skills: []string{"skills/read"}}
	files, err := teamconfig.Read(context.Background(), r, base, ".teamai", selection)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := teamconfig.Parse(r.ProjectID, r, base, ".teamai", selection, files)
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.CreateImportedTeamRelease(r.ProjectID, "Fixture source", "owner", 0, preview.Payload)
	if err != nil {
		t.Fatal(err)
	}
	changes := []teamconfig.Replacement{{Path: "rules/review.md", Content: []byte("Reviewed new rule\n")}, {Path: "skills/read/nested/data.bin", Content: []byte{0, 255, 128, 42}, Executable: true}, {Path: "skills/read/nested/raw.dat", Content: []byte{254, '\n'}}}
	export, err := teamconfig.Export(context.Background(), r, release, changes)
	if err != nil {
		t.Fatal(err)
	}
	e := New(s, runtime.NewManager(filepath.Join(root, "workspaces")))
	proposal, err := e.PrepareTeamGitProposal(context.Background(), r.ProjectID, release.ID, release.SHA256, base, export.PatchSHA256, "owner", changes)
	if err != nil {
		t.Fatal(err)
	}
	return teamProposalFixture{e: e, s: s, source: source, release: release, proposal: proposal, git: git}
}
func (f teamProposalFixture) approve(t *testing.T) domain.TeamGitProposal {
	t.Helper()
	v, err := f.s.DecideTeamGitProposal(f.proposal.ProjectID, f.proposal.ID, f.proposal.SHA256, "owner", "approved", "Exact bytes and limited configuration policy reviewed", 1)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTeamGitProposalRetainsCompleteRawGitTreeAndDecisionCAS(t *testing.T) {
	f := newTeamProposalFixture(t)
	p := f.proposal
	if p.State != "pending_review" || p.Revision != 1 || p.Publication != nil || len(p.Files) != 3 || p.Validation.RunGatesVerified || p.Validation.RemoteTargetAtomicCAS {
		t.Fatalf("invalid proposal state %+v", p)
	}
	if p.ProposedSHA256 == p.ResultConfigurationSHA256 {
		t.Fatal("new source commit provenance was not distinguished")
	}
	raw, err := f.s.TeamGitProposalBundle(p.ProjectID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bundle := filepath.Join(dir, "proposal.bundle")
	if err = os.WriteFile(bundle, raw, 0600); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(dir, "checkout")
	if err = gitwork.VerifyContribution(context.Background(), bundle, checkout, gitwork.ContributionBundle{BaseSHA: p.BaseCommit, ResultSHA: p.ResultSHA, SHA256: p.BundleSHA256, Size: p.BundleSize}, domain.RunnerContributionMaxExpandedBytes); err != nil {
		t.Fatal(err)
	}
	kept, _ := os.ReadFile(filepath.Join(checkout, "src/keep.bin"))
	if string(kept) != string([]byte{0, 255, 4}) {
		t.Fatal("unselected tree changed")
	}
	binary, _ := os.ReadFile(filepath.Join(checkout, ".teamai/skills/read/nested/data.bin"))
	mode, _ := os.Stat(filepath.Join(checkout, ".teamai/skills/read/nested/data.bin"))
	if string(binary) != string([]byte{0, 255, 128, 42}) || mode.Mode()&0111 == 0 {
		t.Fatal("ignored tracked binary bytes/mode lost")
	}
	invalid, _ := os.ReadFile(filepath.Join(checkout, ".teamai/skills/read/nested/raw.dat"))
	if string(invalid) != string([]byte{254, '\n'}) {
		t.Fatal("nonUTF8 bytes lost")
	}
	if f.git("rev-parse", "HEAD") != p.BaseCommit || f.git("status", "--porcelain") != "" {
		t.Fatal("source was modified")
	}
	if _, err = f.s.DecideTeamGitProposal(p.ProjectID, p.ID, p.SHA256, "owner", "approved", "review", 2); !errors.Is(err, repo.ErrTeamGitProposalConflict) {
		t.Fatalf("stale decision %v", err)
	}
	approved := f.approve(t)
	if approved.Revision != 2 || approved.Publication == nil || approved.Publication.SourceKind != "teamai_proposal" || approved.Publication.RunID != "" || len(approved.Publication.Gates) != 0 {
		t.Fatal("proposal forged a Run gate")
	}
	if _, err = f.s.DecideTeamGitProposal(p.ProjectID, p.ID, p.SHA256, "owner", "approved", "retry", 1); !errors.Is(err, repo.ErrTeamGitProposalConflict) {
		t.Fatal("approval replay acquired authority")
	}
	if _, err = f.s.TeamGitProposal("foreign-project", p.ID); !errors.Is(err, repo.ErrIntelligenceUnavailable) {
		t.Fatal("foreign proposal read")
	}
}

func TestTeamGitProposalPublicationRecoversWithoutAnotherCreate(t *testing.T) {
	for _, mode := range []string{"success", "lost-ack", "marker-before-send", "stale-target", "cancel-during-post", "not-draft"} {
		t.Run(mode, func(t *testing.T) {
			f := newTeamProposalFixture(t)
			p := f.approve(t)
			creates, discovers, branches := 0, 0, 0
			attach := func(e *Engine) {
				e.PublishContributionObjects = func(ctx context.Context, project, checkout, branch, target, base, result string) error {
					branches++
					if gitwork.TokenFrom(ctx) != "fixture-controller-only" {
						t.Fatal("explicit project controller credential was not bound")
					}
					if branch != "superagent/integration/"+p.ID || result != p.ResultSHA || base != p.BaseCommit {
						t.Fatal("mutable publication identity")
					}
					return nil
				}
				e.CreateContributionPR = func(context.Context, string, string, string, string, string, string) (domain.ContributionPRReceipt, error) {
					creates++
					if mode == "cancel-during-post" {
						_, err := f.s.DecideTeamGitProposal(p.ProjectID, p.ID, p.SHA256, "owner", "cancelled", "stop", 2)
						if err != nil {
							t.Fatal(err)
						}
					}
					if mode == "lost-ack" {
						return domain.ContributionPRReceipt{}, errors.New("lost provider acknowledgement")
					}
					return domain.ContributionPRReceipt{URL: "https://github.com/example/repo/pull/17", Number: 17, HeadSHA: p.ResultSHA, Draft: mode != "not-draft"}, nil
				}
				e.DiscoverContributionPR = func(context.Context, string, string, string, string, string, string) (domain.ContributionPRReceipt, error) {
					discovers++
					if mode == "marker-before-send" {
						return domain.ContributionPRReceipt{}, errors.New("not visible; never recreate")
					}
					return domain.ContributionPRReceipt{URL: "https://github.com/example/repo/pull/17", Number: 17, HeadSHA: p.ResultSHA, Draft: true}, nil
				}
			}
			attach(f.e)
			if mode == "marker-before-send" {
				claim, err := f.s.ClaimContributionPublication(p.ID, "dead-process", time.Minute, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.s.MarkContributionPRCreateAttempted(p.ID, claim.LeaseOwner, claim.Attempt, time.Now()); err != nil {
					t.Fatal(err)
				}
				f.s.DB.Table("contribution_publications").Where("id = ?", p.ID).Update("lease_until", time.Now().Add(-time.Minute))
			}
			if mode == "stale-target" {
				f.git("commit", "--allow-empty", "-qm", "target advanced")
			}
			result, err := f.e.PublishTeamGitProposal(context.Background(), p.ProjectID, p.ID, p.SHA256, p.BaseCommit, 2, false)
			if mode == "success" {
				if err != nil || result.State != "pr_ready" {
					t.Fatalf("publish %v %+v", err, result)
				}
				if _, err = f.e.PublishTeamGitProposal(context.Background(), p.ProjectID, p.ID, p.SHA256, p.BaseCommit, 2, false); err != nil {
					t.Fatal(err)
				}
				if creates != 1 || branches != 1 || discovers != 0 {
					t.Fatal("terminal replay performed IO")
				}
				return
			}
			if err == nil {
				t.Fatal("unsafe/uncertain outcome accepted")
			}
			if mode == "stale-target" {
				if creates != 0 || branches != 0 || !errors.Is(err, ErrTeamGitSourceStale) {
					t.Fatal("stale target had side effect")
				}
				return
			}
			f.s.DB.Table("contribution_publications").Where("id = ?", p.ID).Update("lease_until", time.Now().Add(-time.Minute))
			next := New(f.s, runtime.NewManager(t.TempDir()))
			attach(next)
			result, err = next.PublishTeamGitProposal(context.Background(), p.ProjectID, p.ID, p.SHA256, p.BaseCommit, 2, true)
			if mode == "lost-ack" || mode == "not-draft" {
				if err != nil || result.State != "pr_ready" || creates != 1 || discovers != 1 {
					t.Fatalf("recovery %+v %v create%d discover%d", result, err, creates, discovers)
				}
			} else if err == nil {
				t.Fatal("cancelled/invisible publication recovered unsafely")
			}
			if mode == "marker-before-send" && creates != 0 {
				t.Fatal("empty lookup authorized another POST")
			}
			if mode == "cancel-during-post" && discovers != 0 {
				t.Fatal("cancellation allowed future publication IO")
			}
		})
	}
}

func TestTeamGitProposalRevocationIdentityAndArtifactGuards(t *testing.T) {
	for _, mode := range []string{"revoked", "url", "provider", "provider-project", "default-ref", "bundle", "payload"} {
		t.Run(mode, func(t *testing.T) {
			f := newTeamProposalFixture(t)
			p := f.approve(t)
			switch mode {
			case "revoked":
				if _, err := f.s.RevokeTeamRelease(p.ProjectID, p.ReleaseID, p.ReleaseRevision, "owner", "withdraw source authority"); err != nil {
					t.Fatal(err)
				}
			case "url", "provider", "provider-project", "default-ref":
				r, _ := f.s.Repository(p.RepositoryID)
				switch mode {
				case "url":
					r.CloneURL = "https://github.com/other/repo.git"
				case "provider":
					r.Provider = "gitlab"
				case "provider-project":
					r.GongfengProjectID = "github:other/repo"
				case "default-ref":
					r.DefaultBranch = "other"
				}
				if err := f.s.PutRepository(r); err != nil {
					t.Fatal(err)
				}
			case "bundle":
				if err := f.s.DB.Table("team_git_proposals").Where("id = ?", p.ID).Update("bundle", []byte("corrupt retained bytes")).Error; err != nil {
					t.Fatal(err)
				}
			case "payload":
				p.Patch += "unreviewed change"
				raw, _ := json.Marshal(p)
				if err := f.s.DB.Table("team_git_proposals").Where("id = ?", p.ID).Update("payload", string(raw)).Error; err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			f.e.PublishContributionObjects = func(context.Context, string, string, string, string, string, string) error { calls++; return nil }
			pr := func(context.Context, string, string, string, string, string, string) (domain.ContributionPRReceipt, error) {
				calls++
				return domain.ContributionPRReceipt{}, nil
			}
			f.e.CreateContributionPR, f.e.DiscoverContributionPR = pr, pr
			if _, err := f.e.PublishTeamGitProposal(context.Background(), p.ProjectID, p.ID, p.SHA256, p.BaseCommit, 2, false); err == nil || calls != 0 {
				t.Fatalf("invalidated proposal had provider effect: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestTeamGitProposalReceiptCompletionRechecksLocalAuthority(t *testing.T) {
	for _, mode := range []string{"approved", "cancelled", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			f := newTeamProposalFixture(t)
			p := f.approve(t)
			claim, err := f.s.ClaimContributionPublication(p.ID, "fixture-publisher", time.Minute, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.MarkContributionPRCreateAttempted(p.ID, claim.LeaseOwner, claim.Attempt, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if mode == "cancelled" {
				_, err = f.s.DecideTeamGitProposal(p.ProjectID, p.ID, p.SHA256, "owner", "cancelled", "stop publication", p.Revision)
			} else if mode == "revoked" {
				_, err = f.s.RevokeTeamRelease(p.ProjectID, p.ReleaseID, p.ReleaseRevision, "owner", "withdraw source")
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.s.CompleteTeamGitProposalPublication(p.ProjectID, p.ID, p.SHA256, p.Revision, claim.LeaseOwner, claim.Attempt, "https://github.com/example/repo/pull/17", 17, time.Now().UTC())
			if (err == nil) != (mode == "approved") {
				t.Fatalf("completion after %s: %v", mode, err)
			}
			publication, _ := f.s.ContributionPublication(p.ID)
			if mode != "approved" && (publication.State != "publishing" || !publication.PRCreateAttempted || publication.PRURL != "") {
				t.Fatal("invalidated authority changed retained marker/receipt")
			}
		})
	}
}
