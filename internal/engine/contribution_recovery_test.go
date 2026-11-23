package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

// The provider callbacks model acknowledged external effects whose response or
// local receipt is lost. Real local Git verification, durable publication state
// and the existing Engine authorization checks still execute on every attempt.
func TestContributionPublicationRecoversLostEffectReceipts(t *testing.T) {
	for _, lost := range []string{"branch-response", "pr-response"} {
		t.Run(lost, func(t *testing.T) {
			f, p := newContributionPublishFixture(t)
			var branch, created bool
			branchCreates, prCreates, objectCalls, prCalls, discoveryCalls := 0, 0, 0, 0, 0
			attach := func(e *Engine) {
				e.PublishContributionObjects = func(ctx context.Context, project, checkout, name, target, base, result string) error {
					objectCalls++
					if name != p.Branch || result != p.ResultSHA || target != p.TargetBranch {
						t.Fatal("recovery changed frozen publication identity")
					}
					if !branch {
						branch = true
						branchCreates++
					}
					if lost == "branch-response" && objectCalls == 1 {
						return errors.New("simulated lost branch acknowledgement")
					}
					return nil
				}
				e.CreateContributionPR = func(ctx context.Context, project, name, target, result, title, body string) (domain.ContributionPRReceipt, error) {
					prCalls++
					if !branch || name != p.Branch || result != p.ResultSHA {
						t.Fatal("PR did not use stable exact-SHA branch")
					}
					if !created {
						created = true
						prCreates++
					}
					if lost == "pr-response" && prCalls == 1 {
						return domain.ContributionPRReceipt{}, errors.New("simulated lost PR acknowledgement")
					}
					return domain.ContributionPRReceipt{URL: "https://github.com/example/repo/pull/17", Number: 17, HeadSHA: result}, nil
				}
				e.DiscoverContributionPR = func(ctx context.Context, project, name, target, result, title, body string) (domain.ContributionPRReceipt, error) {
					discoveryCalls++
					if !created {
						return domain.ContributionPRReceipt{}, errors.New("external PR outcome not visible; do not create")
					}
					if name != p.Branch || result != p.ResultSHA {
						t.Fatal("discovery changed fixed branch")
					}
					return domain.ContributionPRReceipt{URL: "https://github.com/example/repo/pull/17", Number: 17, HeadSHA: result}, nil
				}
			}
			attach(f.e)
			if _, err := f.e.PublishContribution(context.Background(), p.ID); err == nil {
				t.Fatal("uncertain effect must not invent a durable receipt")
			}
			pending, ok := f.s.ContributionPublication(p.ID)
			if !ok || pending.State != "publishing" || pending.Attempt != 1 || pending.PRURL != "" {
				t.Fatalf("uncertain state=%+v", pending)
			}
			if pending.PRCreateAttempted != (lost == "pr-response") {
				t.Fatalf("durable PR phase does not match attempted effect: %+v", pending)
			}
			// A different Engine must not race an outstanding publisher lease.
			next := New(f.s, runtime.NewManager(t.TempDir()))
			attach(next)
			previousCalls := objectCalls + prCalls + discoveryCalls
			if _, err := next.PublishContribution(context.Background(), p.ID); err == nil {
				t.Fatal("live publisher lease was bypassed")
			}
			if previousCalls != objectCalls+prCalls+discoveryCalls {
				t.Fatal("lease loser produced external effects")
			}
			if err := f.s.DB.Table("contribution_publications").Where("id = ?", p.ID).Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
				t.Fatal(err)
			}
			ready, err := next.PublishContribution(context.Background(), p.ID)
			if err != nil || ready.State != "pr_ready" || ready.Attempt != 2 || ready.PRNumber != 17 {
				t.Fatalf("recovery=%+v err=%v", ready, err)
			}
			if branchCreates != 1 || prCreates != 1 {
				t.Fatalf("duplicate effects branch=%d PR=%d", branchCreates, prCreates)
			}
			if prCalls != 1 || (lost == "pr-response" && discoveryCalls != 1) {
				t.Fatalf("recovery retried create adapter: create=%d discover=%d", prCalls, discoveryCalls)
			}
			previousCalls = objectCalls + prCalls + discoveryCalls
			if _, err := f.e.PublishContribution(context.Background(), p.ID); err != nil {
				t.Fatal(err)
			}
			if previousCalls != objectCalls+prCalls+discoveryCalls {
				t.Fatal("terminal replay called provider")
			}
		})
	}
}

func TestContributionPublicationRechecksAuthorityBetweenExternalEffects(t *testing.T) {
	for _, changed := range []string{"cancel-after-branch", "expiry-after-branch", "cancel-after-pr"} {
		t.Run(changed, func(t *testing.T) {
			f, p := newContributionPublishFixture(t)
			objectCalls, prCalls := 0, 0
			change := func() {
				if changed == "expiry-after-branch" {
					if err := f.s.DB.Table("contribution_publications").Where("id = ?", p.ID).Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
						t.Fatal(err)
					}
				} else {
					run, _ := f.s.RunState(f.controller.ID)
					originalStatus := run.Status
					run.Status = "running"
					if err := f.s.PutRun(run); err != nil {
						t.Fatal(err)
					}
					if _, err := f.s.RequestRunCancel(run.ID); err != nil {
						t.Fatal(err)
					}
					run, _ = f.s.RunState(run.ID)
					run.Status = originalStatus
					if err := f.s.PutRun(run); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.e.PublishContributionObjects = func(context.Context, string, string, string, string, string, string) error {
				objectCalls++
				if changed != "cancel-after-pr" {
					change()
				}
				return nil
			}
			f.e.CreateContributionPR = func(ctx context.Context, project, name, target, result, title, body string) (domain.ContributionPRReceipt, error) {
				prCalls++
				change()
				return domain.ContributionPRReceipt{URL: "https://github.com/example/repo/pull/17", Number: 17, HeadSHA: result}, nil
			}
			if _, err := f.e.PublishContribution(context.Background(), p.ID); err == nil {
				t.Fatal("revoked/expired publisher accepted completion")
			}
			expectedPR := 0
			if changed == "cancel-after-pr" {
				expectedPR = 1
			}
			if objectCalls != 1 || prCalls != expectedPR {
				t.Fatalf("effects objects=%d PR=%d", objectCalls, prCalls)
			}
			current, _ := f.s.ContributionPublication(p.ID)
			if current.State != "publishing" || current.PRURL != "" {
				t.Fatalf("uncertain external effect falsely settled=%+v", current)
			}
			if changed != "expiry-after-branch" {
				next := New(f.s, runtime.NewManager(t.TempDir()))
				next.PublishContributionObjects = f.e.PublishContributionObjects
				next.CreateContributionPR = f.e.CreateContributionPR
				if _, err := next.PublishContribution(context.Background(), p.ID); err == nil {
					t.Fatal("restarted Engine resumed cancelled publication")
				}
				if objectCalls != 1 || prCalls != expectedPR {
					t.Fatal("cancelled publication replay produced external effects")
				}
			}
		})
	}
}
