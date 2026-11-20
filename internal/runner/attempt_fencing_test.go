package runner

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestClientRejectsUnfencedClaimResponses(t *testing.T) {
	for _, tc := range []struct {
		name, version, body string
		status              int
	}{
		{"old server lease", "", `{"task":{"id":"old","attempt":1}}`, http.StatusOK},
		{"old server idle", "", "", http.StatusNoContent},
		{"missing generation", "2", `{"task":{"id":"old"}}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Runner-Task-Protocol") != "2" {
					t.Error("claim handshake missing")
				}
				w.Header().Set("X-Runner-Task-Protocol", tc.version)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			task, found, err := NewClient(server.URL).Claim(context.Background(), ClaimRequest{})
			if err == nil || task != nil || found {
				t.Fatalf("unsafe claim task=%+v found=%v err=%v", task, found, err)
			}
		})
	}
}

func TestRunnerCarriesClaimedAttemptOnEveryRequest(t *testing.T) {
	const attempt = 7
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = true
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("attempt") != "7" {
				t.Error("control lost attempt")
			}
		} else {
			var body struct {
				Attempt int `json:"attempt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Attempt != attempt {
				t.Errorf("%s attempt=%d err=%v", r.URL.Path, body.Attempt, err)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c := NewClient(server.URL)
	ctx := context.Background()
	if err := c.StartTask(ctx, "same", attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RenewLease(ctx, "same", attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TaskControl(ctx, "same", attempt); err != nil {
		t.Fatal(err)
	}
	batcher := newEventBatcher(domain.RunnerTask{ID: "same", Attempt: attempt}, c)
	batcher.add("stdout", "old output")
	batcher.flush(ctx)
	if err := c.Complete(ctx, "same", attempt, CompleteRequest{Attempt: 99, Status: domain.RunnerTaskSuccess}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 5 {
		t.Fatalf("requests=%v", seen)
	}
}

func TestAttemptControlsAndManagedStateDoNotAlias(t *testing.T) {
	d := &Daemon{controls: map[taskAttemptKey]chan string{}, pendingControls: map[taskAttemptKey]string{}, managedTasks: map[taskAttemptKey]*managedTask{}}
	old := d.registerTaskControl("same", 1)
	current := d.registerTaskControl("same", 2)
	d.signalTaskControl("same", 1, domain.RunnerTaskDesiredCancel)
	select {
	case <-old:
	default:
		t.Fatal("old signal missing")
	}
	select {
	case <-current:
		t.Fatal("old cancellation reached new execution")
	default:
	}
	state := &managedTask{}
	d.managedTasks[taskAttemptKey{"same", 2}] = state
	d.unregisterTaskControl("same", 1)
	d.releaseManagedTask("same", 1)
	if d.taskControl("same", 2) == nil || d.managedTaskState("same", 2) != state {
		t.Fatal("old cleanup deleted new state")
	}
}

func TestWebSocketCancellationConfirmsEachAttempt(t *testing.T) {
	confirmed := make(chan int, 2)
	peers := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/control") {
			attempt, _ := strconv.Atoi(r.URL.Query().Get("attempt"))
			confirmed <- attempt
			if attempt == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"task lease lost"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(RenewResponse{}) // New execution has no cancellation.
			return
		}
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			peers <- peer
		}
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	peer := <-peers
	defer peer.Close()
	d := &Daemon{client: NewClient(server.URL), controls: map[taskAttemptKey]chan string{}, pendingControls: map[taskAttemptKey]string{}}
	old := d.registerTaskControl("same", 1)
	current := d.registerTaskControl("same", 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.readControls(ctx, conn) }()
	if err := peer.WriteJSON(controlMessage{Type: "task.control", TaskID: "same", DesiredState: domain.RunnerTaskDesiredCancel}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-confirmed:
		case <-time.After(time.Second):
			t.Fatal("missing fenced control confirmation")
		}
	}
	select {
	case <-old:
	case <-time.After(time.Second):
		t.Fatal("lost old attempt did not stop")
	}
	select {
	case <-current:
		t.Fatal("stale WebSocket cancellation reached current attempt")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("control did not stop")
	}
}
