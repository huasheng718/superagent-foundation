package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
)

const publicationTestSHA = "1111111111111111111111111111111111111111"
const publicationTestBranch = "superagent/integration/immutable-test"

type publicationGitHubFixture struct {
	mu            sync.Mutex
	mode          string
	exists        bool
	posts         int
	reads         int
	hiddenReads   int
	requests      []string
	draft         bool
	expectedToken string
}

func (f *publicationGitHubFixture) pull() map[string]any {
	headSHA, base, headRepo, state := publicationTestSHA, "main", "example/repo", "open"
	merged := false
	var mergedAt, autoMerge any
	switch f.mode {
	case "pull-sha":
		headSHA = strings.Repeat("2", 40)
	case "wrong-base":
		base = "other"
	case "wrong-repo":
		headRepo = "attacker/repo"
	case "closed":
		state = "closed"
	case "merged":
		state, merged, mergedAt = "closed", true, "2026-11-15T00:00:00Z"
	case "auto-merge":
		autoMerge = map[string]any{"merge_method": "squash"}
	}
	value := map[string]any{"number": 17, "html_url": "https://github.com/example/repo/pull/17", "state": state, "merged": merged, "merged_at": mergedAt, "auto_merge": autoMerge, "draft": f.draft,
		"head": map[string]any{"ref": publicationTestBranch, "sha": headSHA, "repo": map[string]any{"full_name": headRepo}},
		"base": map[string]any{"ref": base, "repo": map[string]any{"full_name": "example/repo"}}}
	if f.mode == "missing-auto-merge" {
		delete(value, "auto_merge")
	}
	if f.mode == "wrong-url" {
		value["html_url"] = "https://attacker.invalid/pull/17"
	}
	return value
}

func TestPublicationReceiptPreservesProviderDraftReadback(t *testing.T) {
	for _, draft := range []bool{true, false} {
		t.Run(fmt.Sprint(draft), func(t *testing.T) {
			fixture := &publicationGitHubFixture{exists: true, draft: draft}
			service := publicationGitHubService(t, fixture)
			receipt, err := service.FindOpenBranchPRAtSHA(context.Background(), "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "review", "")
			if err != nil || receipt.Draft != draft || fixture.posts != 0 {
				t.Fatalf("draft readback=%+v err=%v posts=%d", receipt, err, fixture.posts)
			}
		})
	}
}

func TestGitHubPublicationUsesExplicitControllerCredential(t *testing.T) {
	fixture := &publicationGitHubFixture{exists: true, draft: true, expectedToken: "explicit-project-controller"}
	service := publicationGitHubService(t, fixture)
	ctx := gitwork.WithToken(context.Background(), fixture.expectedToken)
	if _, err := service.FindOpenBranchPRAtSHA(ctx, "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "review", ""); err != nil {
		t.Fatal(err)
	}
}

func publicationGitHubService(t *testing.T, fixture *publicationGitHubFixture) *Service {
	t.Helper()
	st, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.PutUser(domain.User{LoginName: "publication-owner", GitToken: "private-project-token"}); err != nil {
		t.Fatal(err)
	}
	user, _ := st.UserByLoginName("publication-owner")
	if err := st.PutProject(domain.Project{ID: "publication-project", Name: "Publication", OwnerID: user.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRepository(domain.Repository{ID: "publication-repository", ProjectID: "publication-project", Provider: "github", Name: "repo", GongfengProjectID: "github:example/repo", DefaultBranch: "main", WebURL: "https://github.com/example/repo"}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		fixture.requests = append(fixture.requests, r.Method+" "+r.URL.Path)
		expectedToken := fixture.expectedToken
		if expectedToken == "" {
			expectedToken = "private-project-token"
		}
		if r.Header.Get("Authorization") != "Bearer "+expectedToken {
			t.Error("project owner token was not used")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/example/repo/git/ref/heads/"):
			fixture.reads++
			sha := publicationTestSHA
			if fixture.mode == "branch-sha" || fixture.mode == "branch-moved" && fixture.reads > 1 {
				sha = strings.Repeat("2", 40)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/" + publicationTestBranch, "object": map[string]any{"type": "commit", "sha": sha}})
		case r.Method == "GET" && r.URL.Path == "/repos/example/repo/pulls":
			if r.URL.Query().Get("state") != "all" || r.URL.Query().Get("head") != "example:"+publicationTestBranch || r.URL.Query().Has("base") {
				t.Error("PR discovery must cover all stable branch states/targets")
			}
			list := []map[string]any{}
			if fixture.mode == "paged-closed" && r.URL.Query().Get("page") == "1" {
				for i := 0; i < 100; i++ {
					list = append(list, map[string]any{"head": map[string]any{"ref": fmt.Sprintf("unrelated-%d", i)}})
				}
			} else if fixture.exists && !(fixture.mode == "post-hidden" && fixture.hiddenReads > 0) {
				pull := fixture.pull()
				if fixture.mode == "paged-closed" {
					pull["state"] = "closed"
				}
				list = append(list, pull)
				if fixture.mode == "ambiguous" {
					list = append(list, pull)
				}
			}
			if fixture.mode == "post-hidden" && fixture.exists && fixture.hiddenReads > 0 {
				fixture.hiddenReads--
			}
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == "POST" && r.URL.Path == "/repos/example/repo/pulls":
			fixture.posts++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			fixture.draft, _ = body["draft"].(bool)
			if body["head"] != publicationTestBranch || body["base"] != "main" || !fixture.draft {
				t.Error("PR creation must be draft at the exact branch/target")
			}
			if fixture.mode == "post-unconfirmed" {
				w.WriteHeader(500)
				_, _ = w.Write([]byte("private-project-token secret response"))
				return
			}
			fixture.exists = true
			if fixture.mode == "post-uncertain" || fixture.mode == "post-hidden" {
				w.WriteHeader(500)
				_, _ = w.Write([]byte("private-project-token secret response"))
				return
			}
			if fixture.mode == "post-malformed" {
				_, _ = w.Write([]byte("private-project-token malformed JSON"))
				return
			}
			if fixture.mode == "post-empty" {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_ = json.NewEncoder(w).Encode(fixture.pull())
		case r.Method == "GET" && r.URL.Path == "/repos/example/repo/pulls/17":
			pull := fixture.pull()
			if fixture.mode == "paged-closed" {
				pull["state"] = "closed"
			}
			_ = json.NewEncoder(w).Encode(pull)
		default:
			t.Errorf("forbidden publication side effect: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	service := NewService()
	service.ConfigureWithStore(domain.SyncConfig{}, st)
	service.githubAPIBase = server.URL
	return service
}

func TestGitHubPublicationCreatesOnlyDraftAndReconcilesSamePR(t *testing.T) {
	for _, mode := range []string{"normal", "post-uncertain", "post-malformed", "post-empty"} {
		t.Run(mode, func(t *testing.T) {
			fixture := &publicationGitHubFixture{mode: mode}
			service := publicationGitHubService(t, fixture)
			for i := 0; i < 2; i++ {
				receipt, err := service.EnsureOpenBranchPRAtSHA(context.Background(), "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "Review changes", "Await human acceptance")
				if err != nil || receipt.Number != 17 || receipt.HeadSHA != publicationTestSHA || receipt.URL != "https://github.com/example/repo/pull/17" {
					t.Fatalf("receipt=%+v err=%v", receipt, err)
				}
			}
			if fixture.posts != 1 || !fixture.draft {
				t.Fatalf("duplicate or nondraft PR create: %d %v", fixture.posts, fixture.draft)
			}
			for _, request := range fixture.requests {
				if strings.HasPrefix(request, "PUT ") || strings.HasPrefix(request, "PATCH ") || strings.HasPrefix(request, "DELETE ") || strings.Contains(request, "/merge") {
					t.Fatalf("merge/update capability used: %s", request)
				}
			}
		})
	}
}

func TestGitHubPublicationRejectsUnsafeExistingPRAndChangedSHA(t *testing.T) {
	for _, mode := range []string{"branch-sha", "branch-moved", "pull-sha", "wrong-base", "wrong-repo", "closed", "merged", "auto-merge", "missing-auto-merge", "wrong-url", "ambiguous", "paged-closed"} {
		t.Run(mode, func(t *testing.T) {
			fixture := &publicationGitHubFixture{mode: mode, exists: true}
			service := publicationGitHubService(t, fixture)
			if _, err := service.EnsureOpenBranchPRAtSHA(context.Background(), "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "Review", ""); err == nil {
				t.Fatal("unsafe PR was accepted")
			}
			if fixture.posts != 0 {
				t.Fatalf("unsafe existing branch triggered %d PR creates", fixture.posts)
			}
		})
	}
}

func TestGitHubPublicationErrorsAreRedactedAndContextIsPreserved(t *testing.T) {
	fixture := &publicationGitHubFixture{mode: "post-unconfirmed"}
	service := publicationGitHubService(t, fixture)
	_, err := service.EnsureOpenBranchPRAtSHA(context.Background(), "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "Review", "")
	if err == nil || strings.Contains(err.Error(), "private-project-token") || strings.Contains(err.Error(), "secret response") || fixture.posts != 1 {
		t.Fatalf("unconfirmed create leaked/retried: %v posts=%d", err, fixture.posts)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	service.githubAPIBase = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = service.EnsureOpenBranchPRAtSHA(ctx, "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "Review", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context deadline hidden: %v", err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	_, err = service.EnsureOpenBranchPRAtSHA(canceled, "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "Review", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context cancellation hidden: %v", err)
	}
	for _, project := range []string{"gongfeng:example/repo", "example/repo", "github:../repo"} {
		if _, err := service.EnsureOpenBranchPRAtSHA(context.Background(), project, publicationTestBranch, "main", publicationTestSHA, "Review", ""); err == nil {
			t.Fatalf("unsupported project accepted: %s", project)
		}
	}
}

// The first create reaches the provider, but both its response and discovery
// are uncertain. A later replay must rediscover that same stable-branch PR.
func TestGitHubPublicationReplaysAfterDelayedVisibilityWithoutReplacement(t *testing.T) {
	fixture := &publicationGitHubFixture{mode: "post-hidden", hiddenReads: 3}
	service := publicationGitHubService(t, fixture)
	if _, err := service.EnsureOpenBranchPRAtSHA(context.Background(), "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "Review", ""); err == nil {
		t.Fatal("hidden create outcome must remain uncertain")
	}
	if fixture.posts != 1 || !fixture.exists {
		t.Fatalf("first outcome posts=%d exists=%v", fixture.posts, fixture.exists)
	}
	for i := 0; i < 2; i++ {
		if _, err := service.FindOpenBranchPRAtSHA(context.Background(), "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "Review", ""); err == nil {
			t.Fatal("hidden PR should remain uncertain")
		}
		if fixture.posts != 1 {
			t.Fatal("discovery retried POST")
		}
	}
	receipt, err := service.FindOpenBranchPRAtSHA(context.Background(), "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "Review", "")
	if err != nil || receipt.Number != 17 || receipt.HeadSHA != publicationTestSHA || fixture.posts != 1 {
		t.Fatalf("replay=%+v err=%v posts=%d", receipt, err, fixture.posts)
	}
	for _, state := range []string{"closed", "merged"} {
		fixture.mode = state
		if _, err := service.FindOpenBranchPRAtSHA(context.Background(), "github:example/repo", publicationTestBranch, "main", publicationTestSHA, "Review", ""); err == nil {
			t.Fatalf("human %s PR must not be replaced", state)
		}
		if fixture.posts != 1 {
			t.Fatalf("human %s state created replacement", state)
		}
	}
}
