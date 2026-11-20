package tracker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentflow/internal/gitwork"
)

func contributionObjectsCheckout(t *testing.T) (string, string, string, []gitwork.GitHubContributionObject) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_DATE=2026-11-15T01:02:03+00:00", "GIT_COMMITTER_DATE=2026-11-15T01:02:03+00:00")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("local fixture git %v: %v %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Publication Test")
	git("config", "user.email", "publication@example.invalid")
	git("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "base")
	base := git("rev-parse", "HEAD")
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "binary.bin"), []byte{0, 1, 2, 255, '\n'}, 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "add binary contribution")
	result := git("rev-parse", "HEAD")
	objects, err := gitwork.GitHubContributionObjects(context.Background(), dir, base, result)
	if err != nil {
		t.Fatal(err)
	}
	return dir, base, result, objects
}

func objectPublicationInput(object gitwork.GitHubContributionObject) any {
	switch object.Type {
	case "blob":
		return map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(object.Content)}
	case "tree":
		entries := make([]map[string]string, 0, len(object.Entries))
		for _, e := range object.Entries {
			entries = append(entries, map[string]string{"path": e.Path, "mode": e.Mode, "type": e.Type, "sha": e.SHA})
		}
		return map[string]any{"tree": entries}
	default:
		c := object.Commit
		return map[string]any{"message": c.Message, "tree": c.Tree, "parents": c.Parents,
			"author":    map[string]string{"name": c.Author.Name, "email": c.Author.Email, "date": c.Author.Date},
			"committer": map[string]string{"name": c.Committer.Name, "email": c.Committer.Email, "date": c.Committer.Date}}
	}
}

func TestPublishContributionObjectsCreateOnlyAndReconcile(t *testing.T) {
	checkout, base, result, objects := contributionObjectsCheckout(t)
	for _, mode := range []string{"normal", "bound-credential", "existing-same", "existing-other", "unrelated-base", "missing-target", "forbidden-ref", "blob-sha", "tree-sha", "commit-sha", "uncertain-created", "conflict-created", "malformed-created", "uncertain-absent", "create-wrong-sha", "object-error"} {
		t.Run(mode, func(t *testing.T) {
			objectPosts, refPosts := 0, 0
			created := mode == "existing-same" || mode == "existing-other"
			write := func(w http.ResponseWriter, value any) {
				t.Helper()
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Error(err)
				}
			}
			ref := func(w http.ResponseWriter, branch, sha string) {
				write(w, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}})
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "bound-credential" && r.Header.Get("Authorization") != "Bearer explicit-project-controller" {
					t.Error("explicit object publisher credential not used")
				}
				path := strings.TrimPrefix(r.URL.Path, "/repos/example/repo")
				switch {
				case r.Method == "GET" && path == "/git/ref/heads/main":
					if mode == "missing-target" {
						w.WriteHeader(404)
						return
					}
					ref(w, "main", base)
				case r.Method == "GET" && path == "/compare/"+base+"..."+base:
					mergeBase, status := base, "identical"
					if mode == "unrelated-base" {
						mergeBase, status = strings.Repeat("0", 40), "diverged"
					}
					write(w, map[string]any{"status": status, "merge_base_commit": map[string]string{"sha": mergeBase}})
				case r.Method == "GET" && path == "/git/ref/heads/"+publicationTestBranch:
					if mode == "forbidden-ref" {
						w.WriteHeader(403)
						_, _ = w.Write([]byte("secret-provider-body"))
						return
					}
					if !created {
						w.WriteHeader(404)
						return
					}
					sha := result
					if mode == "existing-other" || mode == "create-wrong-sha" {
						sha = base
					}
					ref(w, publicationTestBranch, sha)
				case r.Method == "POST" && (path == "/git/blobs" || path == "/git/trees" || path == "/git/commits"):
					if objectPosts >= len(objects) {
						t.Error("extra object POST")
						w.WriteHeader(500)
						return
					}
					object := objects[objectPosts]
					objectPosts++
					if path != "/git/"+object.Type+"s" {
						t.Errorf("object dependency order changed: %s expected %s", path, object.Type)
					}
					var got, want any
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
					}
					raw, _ := json.Marshal(objectPublicationInput(object))
					_ = json.Unmarshal(raw, &want)
					if !reflect.DeepEqual(got, want) {
						t.Errorf("%s input does not preserve the immutable object", object.Type)
					}
					if mode == "object-error" {
						w.WriteHeader(500)
						_, _ = w.Write([]byte("secret-provider-body"))
						return
					}
					sha := object.SHA
					if mode == object.Type+"-sha" {
						sha = strings.Repeat("0", 40)
					}
					write(w, map[string]string{"sha": sha})
				case r.Method == "POST" && path == "/git/refs":
					refPosts++
					var input map[string]string
					_ = json.NewDecoder(r.Body).Decode(&input)
					if input["ref"] != "refs/heads/"+publicationTestBranch || input["sha"] != result || len(input) != 2 {
						t.Error("incorrect create-only ref payload")
					}
					created = mode != "uncertain-absent"
					switch mode {
					case "uncertain-created", "uncertain-absent":
						w.WriteHeader(500)
					case "conflict-created":
						w.WriteHeader(422)
					case "malformed-created":
						_, _ = w.Write([]byte("invalid-json"))
					default:
						ref(w, publicationTestBranch, result)
					}
				default:
					t.Errorf("unexpected/unsafe provider operation %s %s", r.Method, path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			s := &Service{githubAPIBase: server.URL}
			ctx := context.Background()
			if mode == "bound-credential" {
				ctx = gitwork.WithToken(ctx, "explicit-project-controller")
			}
			err := s.PublishContributionObjects(ctx, "github:example/repo", checkout, publicationTestBranch, "main", base, result)
			success := mode == "normal" || mode == "bound-credential" || mode == "existing-same" || mode == "uncertain-created" || mode == "conflict-created" || mode == "malformed-created"
			if (err == nil) != success {
				t.Fatalf("success=%t error=%v", success, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-provider-body") {
				t.Fatal("provider body leaked")
			}
			if mode == "existing-same" || mode == "existing-other" || mode == "unrelated-base" || mode == "missing-target" || mode == "forbidden-ref" {
				if objectPosts != 0 || refPosts != 0 {
					t.Fatal("preflight failure/replay wrote provider objects or refs")
				}
			} else if strings.HasSuffix(mode, "-sha") && mode != "create-wrong-sha" || mode == "object-error" {
				if refPosts != 0 {
					t.Fatal("object failure acquired ref-write authority")
				}
			} else if refPosts != 1 {
				t.Fatalf("create ref POSTs=%d; expected one", refPosts)
			}
		})
	}
}

func TestPublishContributionObjectsValidationAndCancellation(t *testing.T) {
	s := &Service{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.PublishContributionObjects(ctx, "github:example/repo", "", publicationTestBranch, "main", publicationTestSHA, publicationTestSHA); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	for _, branch := range []string{"main", "feature/new", "superagent/integration/", "superagent/integration/../main"} {
		if err := s.PublishContributionObjects(context.Background(), "github:example/repo", "", branch, "main", publicationTestSHA, publicationTestSHA); err == nil {
			t.Fatalf("invalid branch %q accepted", branch)
		}
	}
	if err := s.PublishContributionObjects(context.Background(), "gitlab:example/repo", "", publicationTestBranch, "main", publicationTestSHA, publicationTestSHA); err == nil {
		t.Fatal("non-GitHub provider accepted")
	}
}

func TestPublishContributionObjectsCancelsInFlightBeforeWrites(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" {
			t.Error("canceled preflight attempted mutation")
		}
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	s := &Service{githubAPIBase: server.URL}
	err := s.PublishContributionObjects(ctx, "github:example/repo", "", publicationTestBranch, "main", publicationTestSHA, publicationTestSHA)
	if !errors.Is(err, context.Canceled) || requests != 1 {
		t.Fatalf("requests=%d error=%v", requests, err)
	}
}
