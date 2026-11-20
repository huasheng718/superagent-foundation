package repo

import (
	"reflect"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestControlledDeliveryReconcileCannotCompleteSuccessfulIssue(t *testing.T) {
	for _, tc := range []struct{ name, deployment, runPolicy string }{
		{"missing policies", "", ""},
		{"controlled run under legacy deployment", "legacy", "controlled_pr"},
		{"missing run under legacy deployment", "legacy", ""},
		{"legacy run under controlled deployment", "controlled_pr", "legacy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := OpenTest()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			if err := s.DeliveryRepo.ConfigureDeliveryPolicy(tc.deployment); err != nil {
				t.Fatal(err)
			}
			issue := domain.Issue{ID: "policy-issue", ProjectID: "p", Status: "todo", AutomationState: "eligible", LatestTaskID: "policy-task"}
			if err := s.PutIssue(issue); err != nil {
				t.Fatal(err)
			}
			if err := s.PutTask(domain.Task{ID: issue.LatestTaskID, IssueID: issue.ID, RunID: "policy-run", Status: "completed"}); err != nil {
				t.Fatal(err)
			}
			if err := s.PutRun(domain.Run{ID: "policy-run", Status: "succeeded", ExecutionContext: domain.TelemetryContext{IssueID: issue.ID, DeliveryPolicy: tc.runPolicy}, Artifacts: []domain.Artifact{{Name: "mr-body", Content: "https://git.example.com/org/repo/-/merge_requests/1"}}}); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Issue(issue.ID)
			if err := s.ReconcileCompletedDeliveries(); err != nil {
				t.Fatal(err)
			}
			if err := s.ReconcileUnlinkedDeliveryMRs(); err != nil {
				t.Fatal(err)
			}
			after, _ := s.Issue(issue.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("reconcile completed business issue: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestControlledUnlinkedBackfillAndOpenedCallbackDoNotCompleteIssue(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	issue := domain.Issue{ID: "pending-policy-issue", ProjectID: "p", Status: "todo", LatestTaskID: "policy-task"}
	if err := s.PutIssue(issue); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTask(domain.Task{ID: issue.LatestTaskID, IssueID: issue.ID, RunID: "policy-run", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	const mrURL = "https://git.example.com/org/repo/-/merge_requests/1"
	if err := s.PutRun(domain.Run{ID: "policy-run", Status: "succeeded", ExecutionContext: domain.TelemetryContext{IssueID: issue.ID, DeliveryPolicy: domain.DeliveryPolicyControlledPR}, Artifacts: []domain.Artifact{{Name: "mr-body", Content: mrURL}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureMRDeliveryState("org/repo", 1, mrURL, issue.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMRDeliveryState("org/repo", 1, "opened", "main", time.Time{}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Issue(issue.ID)
	if after.Status == "done" || after.DeliveryLocked() || after.AutomationState == "ignored" {
		t.Fatalf("opened callback completed issue: %+v", after)
	}
	// The legacy link-recovery pass applies only to previously locked issues;
	// controlled Run artifacts must not become recovered delivery evidence.
	after.Status, after.DeliveryState = "done", "merged"
	after.SetDeliveryMRs(nil)
	if err := s.PutIssue(after); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileUnlinkedDeliveryMRs(); err != nil {
		t.Fatal(err)
	}
	backfilled, _ := s.Issue(issue.ID)
	if len(backfilled.DeliveryMRList()) != 0 {
		t.Fatalf("controlled artifact trusted by legacy backfill: %+v", backfilled)
	}
}
