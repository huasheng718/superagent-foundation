package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

func publicationGitHubMetadata(task *domain.RunnerTask, run *domain.Run) {
	for i := range run.Repositories {
		run.Repositories[i].CloneURL = "https://github.com/example/repo.git"
		run.Repositories[i].Provider = "github"
		run.Repositories[i].GongfengProjectID = "github:example/repo"
		run.Repositories[i].DefaultBranch = "main"
	}
	if task != nil {
		for i := range task.Invocation.Repositories {
			task.Invocation.Repositories[i].CloneURL = "https://github.com/example/repo.git"
		}
	}
}

// Source objects remain real local Git bundles. Only the frozen provider
// identity is public GitHub; a PATH guard below forbids all remote Git IO.
func newContributionPublishFixture(t *testing.T) (*contributionGateFixture, domain.ContributionPublication) {
	t.Helper()
	base := newAcceptedContributionFixture(t)
	publicationGitHubMetadata(nil, &base.controller)
	if err := base.s.PutRun(base.controller); err != nil {
		t.Fatal(err)
	}
	repository, _ := base.s.Repository("repo")
	repository.CloneURL, repository.Provider, repository.GongfengProjectID, repository.DefaultBranch = "https://github.com/example/repo.git", "github", "github:example/repo", "main"
	if err := base.s.PutRepository(repository); err != nil {
		t.Fatal(err)
	}
	if err := base.s.PutUser(domain.User{LoginName: "publication-owner", GitToken: "test-only-controller-token"}); err != nil {
		t.Fatal(err)
	}
	user, _ := base.s.UserByLoginName("publication-owner")
	project, _ := base.s.Project("project")
	project.OwnerID = user.ID
	if err := base.s.PutProject(project); err != nil {
		t.Fatal(err)
	}
	writer := base.accept(t, "publish-author", "feature.py", "feature = True\n", func(task *domain.RunnerTask, run *domain.Run) {
		publicationGitHubMetadata(task, run)
		task.Invocation.Agent.ID = "author-agent"
	})
	integration, err := base.e.IntegrateRunnerContributions(context.Background(), base.controller.ID, "repo", []string{writer.ID})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := base.s.RunnerContributionBlob(integration.BundleSHA256)
	bundle, checkout := filepath.Join(base.root, "publish-gate.bundle"), filepath.Join(base.root, "publish-gate-source")
	if err := os.WriteFile(bundle, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := gitwork.VerifyContribution(context.Background(), bundle, checkout, gitwork.ContributionBundle{BaseSHA: integration.BaseSHA, ResultSHA: integration.ResultSHA, SHA256: integration.BundleSHA256, Size: integration.BundleSize}, 64<<20); err != nil {
		t.Fatal(err)
	}
	gateSource := *base
	gateSource.source, gateSource.base = checkout, integration.ResultSHA
	f := &contributionGateFixture{acceptedContributionFixture: base, integration: integration, gateSource: &gateSource}
	var publication domain.ContributionPublication
	for _, kind := range []string{"fast", "slow", "review"} {
		publication = f.record(t, f.gate(t, "publish-"+kind, kind, false, publicationGitHubMetadata), kind)
	}
	if err := f.e.validateContributionPublication(f.integration, publication); err != nil {
		t.Fatalf("valid publish fixture: %v", err)
	}
	return f, publication
}

func forbidPublicationRemoteGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "remote-command")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nfor arg in \"$@\"; do\ncase \"$arg\" in\nhttps://github.com/*) printf attempted > " + quote(marker) + "; exit 97;;\nesac\ndone\nexec " + quote(git) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func TestContributionPublishTerminalReplayNeverCreatesPR(t *testing.T) {
	f, publication := newContributionPublishFixture(t)
	marker := forbidPublicationRemoteGit(t)
	claimed, err := f.s.ClaimContributionPublication(publication.ID, "fixture-publisher", time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ready, err := f.s.CompleteContributionPublication(publication.ID, claimed.LeaseOwner, claimed.Attempt, "https://github.com/example/repo/pull/17", 17, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.e.CreateContributionPR = func(context.Context, string, string, string, string, string, string) (domain.ContributionPRReceipt, error) {
		calls++
		t.Error("terminal receipt replay acquired PR-create authority")
		return domain.ContributionPRReceipt{}, nil
	}
	for i := 0; i < 2; i++ {
		got, err := f.e.PublishContribution(context.Background(), publication.ID)
		if err != nil || got.State != "pr_ready" || got.PRURL != ready.PRURL || got.PRNumber != 17 || got.Attempt != claimed.Attempt {
			t.Fatalf("terminal replay=%+v err=%v", got, err)
		}
	}
	if calls != 0 {
		t.Fatal("duplicate PR adapter invoked")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("terminal replay attempted Git remote IO: %v", err)
	}
}

func TestContributionPublishRejectsTamperedOrCancelledSourcesBeforeIO(t *testing.T) {
	f, original := newContributionPublishFixture(t)
	marker := forbidPublicationRemoteGit(t)
	calls := 0
	f.e.CreateContributionPR = func(context.Context, string, string, string, string, string, string) (domain.ContributionPRReceipt, error) {
		calls++
		return domain.ContributionPRReceipt{}, nil
	}
	for _, mutate := range []struct {
		name   string
		change func(*domain.ContributionPublication)
	}{
		{"result-sha", func(p *domain.ContributionPublication) { p.ResultSHA = strings.Repeat("0", 40) }},
		{"context", func(p *domain.ContributionPublication) { p.ContextSHA256 = strings.Repeat("0", 64) }},
		{"missing-gate", func(p *domain.ContributionPublication) { p.Gates = p.Gates[:2] }},
		{"failed-gate", func(p *domain.ContributionPublication) { p.Gates[0].Status = "failed" }},
		{"forged-evidence", func(p *domain.ContributionPublication) { p.Gates[0].EvidenceSHA256 = strings.Repeat("0", 64) }},
		{"branch-redirect", func(p *domain.ContributionPublication) { p.Branch = "main" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			value := original
			value.Gates = append([]domain.ContributionGateEvidence(nil), original.Gates...)
			mutate.change(&value)
			raw, _ := json.Marshal(value)
			if err := f.s.DB.Table("contribution_publications").Where("id = ?", value.ID).Update("payload", string(raw)).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := f.e.PublishContribution(context.Background(), value.ID); err == nil {
				t.Fatal("tampered publication accepted")
			}
		})
	}
	raw, _ := json.Marshal(original)
	if err := f.s.DB.Table("contribution_publications").Where("id = ?", original.ID).Update("payload", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.controller.ID, "run-publish-author", "run-publish-review"} {
		run, _ := f.s.RunState(id)
		originalStatus := run.Status
		run.Status = "running"
		if err := f.s.PutRun(run); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.RequestRunCancel(id); err != nil {
			t.Fatal(err)
		}
		run, _ = f.s.RunState(id)
		run.Status = originalStatus
		if err := f.s.PutRun(run); err != nil {
			t.Fatal(err)
		}
		if run, ok := f.s.RunState(id); !ok || !run.CancelRequested {
			t.Fatalf("cancellation fixture not persisted for %s", id)
		}
		if _, err := f.e.PublishContribution(context.Background(), original.ID); err == nil {
			t.Fatalf("canceled %s accepted", id)
		}
		if err := f.s.ClearRunControl(id); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("invalid source called PR adapter")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("invalid source attempted remote Git: %v", err)
	}
}

func TestContributionPublishMissingAuthorityAndMetadataRedirectFailClosed(t *testing.T) {
	f, publication := newContributionPublishFixture(t)
	marker := forbidPublicationRemoteGit(t)
	if _, err := f.e.PublishContribution(context.Background(), publication.ID); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured adapter: %v", err)
	}
	calls := 0
	f.e.CreateContributionPR = func(context.Context, string, string, string, string, string, string) (domain.ContributionPRReceipt, error) {
		calls++
		return domain.ContributionPRReceipt{}, nil
	}
	project, _ := f.s.Project("project")
	ownerID := project.OwnerID
	if err := f.s.DB.Table("projects").Where("id = ?", project.ID).Update("owner_user_id", nil).Error; err != nil {
		t.Fatal(err)
	}
	if project, ok := f.s.Project("project"); !ok || project.OwnerID != 0 {
		t.Fatal("missing-owner fixture not persisted")
	}
	if _, err := f.e.PublishContribution(context.Background(), publication.ID); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("no project owner: %v", err)
	}
	if err := f.s.DB.Table("projects").Where("id = ?", project.ID).Update("owner_user_id", ownerID).Error; err != nil {
		t.Fatal(err)
	}
	repository, _ := f.s.Repository("repo")
	for _, change := range []func(*domain.Repository){
		func(r *domain.Repository) { r.GongfengProjectID = "github:attacker/repo" },
		func(r *domain.Repository) { r.CloneURL = "https://github.com/attacker/repo.git" },
		func(r *domain.Repository) { r.DefaultBranch = "other" },
	} {
		changed := repository
		change(&changed)
		if err := f.s.PutRepository(changed); err != nil {
			t.Fatal(err)
		}
		if _, err := f.e.PublishContribution(context.Background(), publication.ID); err == nil {
			t.Fatal("metadata redirected publication")
		}
		if err := f.s.PutRepository(repository); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.e.PublishContribution(ctx, publication.ID); err == nil {
		t.Fatal("canceled caller acquired publication authority")
	}
	if calls != 0 {
		t.Fatal("missing authority called PR adapter")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("metadata or authority failure attempted Git remote IO: %v", err)
	}
}
