package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

func TestTaskContributionIsAcceptedOnlyWithSuccessfulComplete(t *testing.T) {
	for _, mode := range []string{"terminal", "builtin-agent"} {
		t.Run(mode, func(t *testing.T) {
			server, store, _ := newDaemonTestServer(t)
			workspace := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", workspace}, args...)...)
				cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("fixture git: %v %s", err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-q")
			git("config", "user.name", "Fixture")
			git("config", "user.email", "fixture@example.invalid")
			if err := os.WriteFile(filepath.Join(workspace, "code.txt"), []byte("before\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", "code.txt")
			git("commit", "-qm", "base")
			base := git("rev-parse", "HEAD")
			d, err := New(Config{ServerURL: server.URL, BootstrapToken: "boot-secret", StatePath: filepath.Join(t.TempDir(), "state.json"), DefaultWorkspaceRoot: workspace})
			if err != nil {
				t.Fatal(err)
			}
			registered, err := d.client.Register(context.Background(), "boot-secret", RegisterRequest{Name: "contribution-runner", MaxConcurrency: 1, Runtimes: []domain.RunnerRuntime{{Provider: "mock", Status: "online"}}})
			if err != nil {
				t.Fatal(err)
			}
			d.client.SetSession(registered.SessionToken)
			spec := domain.RunnerContributionSpec{RepositoryID: "repo", BaseSHA: base, ContextSHA256: strings.Repeat("a", 64), MaxBytes: domain.RunnerContributionMaxBytes}
			task := domain.RunnerTask{ID: "contribution-" + mode, RunID: "run-" + mode, NodeID: "fix", Workspace: workspace, Provider: "mock", Status: domain.RunnerTaskQueued, MaxAttempts: 1, ContributionSpecs: []domain.RunnerContributionSpec{spec}, Invocation: &domain.RunnerInvocation{Executor: domain.ExecutorDefinition{Kind: "mock"}, Agent: domain.AgentDefinition{ID: "fixture-agent"}, Repositories: []domain.RunnerRepository{{RepositoryID: "repo", BaseSHA: base}}}}
			task.Invocation.InvocationID = "inv-" + mode
			task.Invocation.RunID = task.RunID
			task.Invocation.NodeID = task.NodeID
			if mode == "terminal" {
				task.Command = "printf 'after\\n' > code.txt"
			} else if err := os.WriteFile(filepath.Join(workspace, "code.txt"), []byte("after\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := store.PutRunnerTask(task); err != nil {
				t.Fatal(err)
			}
			claimed, ok, err := store.ClaimRunnerTask(registered.RunnerID, []string{"mock"}, time.Now().UTC(), time.Minute)
			if err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			atomic.AddInt32(&d.running, 1)
			d.executeTask(context.Background(), claimed)
			stored, _ := store.RunnerTask(task.ID)
			if stored.Status != domain.RunnerTaskSuccess || len(stored.AcceptedContributions) != 1 || stored.Outputs == nil || len(stored.Outputs.ContributionReceipts) != 1 {
				t.Fatalf("task failed to export/upload before completion: status=%s error=%s accepted=%d", stored.Status, stored.Error, len(stored.AcceptedContributions))
			}
			receipt := stored.AcceptedContributions[0]
			if receipt.BaseSHA != base || receipt.ResultSHA == base || receipt.AcceptedAt.IsZero() || receipt.ID != stored.Outputs.ContributionReceipts[0] {
				t.Fatal("accepted contribution identity is wrong")
			}
			blob, size, ok := store.RunnerContributionBlob(receipt.SHA256)
			if !ok || size != receipt.Size {
				t.Fatal("accepted receipt has no durable bytes")
			}
			path := filepath.Join(t.TempDir(), "accepted.bundle")
			if err := os.WriteFile(path, blob, 0o600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "reconstructed")
			if err := gitwork.VerifyContribution(context.Background(), path, destination, gitwork.ContributionBundle{BaseSHA: base, ResultSHA: receipt.ResultSHA, SHA256: receipt.SHA256, Size: receipt.Size}, 64<<20); err != nil {
				t.Fatal(err)
			}
			text, err := os.ReadFile(filepath.Join(destination, "code.txt"))
			if err != nil || string(text) != "after\n" {
				t.Fatalf("reconstructed code=%q err=%v", text, err)
			}
		})
	}
}

func TestClientContributionRejectsLeaseLossAndWrongReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.bundle")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := domain.RunnerContributionSpec{RepositoryID: "repo", BaseSHA: strings.Repeat("a", 40), ContextSHA256: strings.Repeat("b", 64), MaxBytes: domain.RunnerContributionMaxBytes}
	bundle := gitwork.ContributionBundle{Path: path, BaseSHA: spec.BaseSHA, ResultSHA: strings.Repeat("c", 40), SHA256: strings.Repeat("d", 64), Size: 7}
	for _, leaseLost := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut || r.URL.Query().Get("attempt") != "7" || r.Header.Get("Authorization") != "Bearer fixture-session" || r.Header.Get("X-Contribution-Context-SHA256") != spec.ContextSHA256 {
				t.Error("upload identity metadata lost")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != "fixture" {
				t.Error("bundle body lost")
			}
			if leaseLost {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"task lease lost"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(domain.RunnerContributionReceipt{ID: "receipt", TaskID: "same", Attempt: 6, RepositoryID: spec.RepositoryID, BaseSHA: spec.BaseSHA, ContextSHA256: spec.ContextSHA256, ResultSHA: bundle.ResultSHA, SHA256: bundle.SHA256, Size: bundle.Size})
		}))
		client := NewClient(server.URL)
		client.SetSession("fixture-session")
		_, err := client.UploadContribution(context.Background(), "same", 7, spec, bundle)
		server.Close()
		if err == nil || (leaseLost && !errors.Is(err, ErrTaskLeaseLost)) {
			t.Fatalf("leaseLost=%v err=%v", leaseLost, err)
		}
	}
}

func TestClientDownloadContributionInputPinsResponseMetadata(t *testing.T) {
	raw := []byte("immutable bundle bytes")
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	base := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/runners/tasks/task/contribution-inputs/repo" || r.URL.Query().Get("attempt") != "3" || r.Header.Get("Authorization") != "Bearer fixture-session" {
			t.Errorf("request identity lost: %s %s auth=%q", r.Method, r.URL.String(), r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/x-git-bundle")
		w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
		w.Header().Set("X-Contribution-SHA256", digest)
		w.Header().Set("X-Contribution-Base-SHA", base)
		w.Header().Set("X-Contribution-Result-SHA", head)
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	client := NewClient(server.URL)
	client.SetSession("fixture-session")
	input := domain.RunnerContributionInput{RepositoryID: "repo", BaseSHA: base, HeadSHA: head, BundleSHA256: digest, BundleSize: int64(len(raw))}
	path, err := client.DownloadContributionInput(context.Background(), "task", 3, "repo", input)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("downloaded=%q err=%v", got, err)
	}
}

func TestContributionRepositoryRejectsSymlinkEscape(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"../outside", outside, "escape"} {
		if _, err := contributionRepositoryPath(workspace, dir); err == nil {
			t.Fatalf("unsafe directory accepted: %s", dir)
		}
	}
}

func TestContributionUploadRemainsUnderTaskLease(t *testing.T) {
	started := make(chan struct{})
	uploadCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lease/renew") {
			select {
			case <-started:
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"task lease lost"}`))
			default:
				_ = json.NewEncoder(w).Encode(RenewResponse{LeaseUntil: time.Now().Add(time.Minute)})
			}
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(uploadCanceled)
		case <-time.After(5 * time.Second):
			t.Error("lease loss did not cancel contribution upload")
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "fixture.bundle")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewClient(server.URL)
	d := &Daemon{client: client}
	until := time.Now().Add(time.Minute)
	parent, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx, stop := d.keepTaskLease(parent, domain.RunnerTask{ID: "task", Attempt: 2, LeaseUntil: &until}, 10*time.Millisecond)
	defer stop()
	_, err := client.UploadContribution(ctx, "task", 2, domain.RunnerContributionSpec{RepositoryID: "repo", MaxBytes: domain.RunnerContributionMaxBytes}, gitwork.ContributionBundle{Path: path, Size: 7})
	if !errors.Is(err, context.Canceled) || parent.Err() != nil {
		t.Fatalf("upload must be canceled by lease fence, not parent timeout: %v", err)
	}
	select {
	case <-uploadCanceled:
	case <-time.After(time.Second):
		t.Fatal("server did not observe upload connection cancellation")
	}
}
