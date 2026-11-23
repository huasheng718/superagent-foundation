package repo

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestMySQLQueueNodeExecutionDifferentRuns(t *testing.T) {
	s, other, _, templateRun, templateTask := newMySQLSessionClaimFixture(t)
	const count = 16
	runs := make([]domain.Run, count)
	tasks := make([]domain.RunnerTask, count)
	ids := make([]string, count)
	suffix := time.Now().UnixNano()
	for i := range tasks {
		raw, _ := json.Marshal(templateRun)
		if err := json.Unmarshal(raw, &runs[i]); err != nil {
			t.Fatal(err)
		}
		runs[i].ID, runs[i].Revision = fmt.Sprintf("mysql-node-disjoint-%d-%02d", suffix, i), 0
		runs[i].ExecutionContext.RunID = runs[i].ID
		raw, _ = json.Marshal(templateTask)
		if err := json.Unmarshal(raw, &tasks[i]); err != nil {
			t.Fatal(err)
		}
		ids[i] = fmt.Sprintf("mysql-node-disjoint-task-%d-%02d", suffix, i)
		tasks[i].ID, tasks[i].RunID, tasks[i].Invocation.RunID = ids[i], runs[i].ID, runs[i].ID
		tasks[i].CreatedAt, tasks[i].UpdatedAt = time.Time{}, time.Time{}
		freezeNodeQueueTask(t, &tasks[i])
		if err := s.PutRun(runs[i]); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = s.DB.Where("id IN ?", ids).Delete(&runnerTaskRow{}).Error
		for _, run := range runs {
			_ = s.DB.Where("run_id = ?", run.ID).Delete(&runNodeRow{}).Error
			_ = s.DB.Where("id = ?", run.ID).Delete(&runRow{}).Error
		}
	})
	start := make(chan struct{})
	results := make(chan error, count)
	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			store := s
			if i%2 != 0 {
				store = other
			}
			results <- store.QueueNodeExecutionTask(tasks[i])
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
	for i, run := range runs {
		stored, ok := s.RunState(run.ID)
		if !ok || stored.Nodes[tasks[i].NodeID].ExecutionTaskID != tasks[i].ID || stored.Nodes[tasks[i].NodeID].ExecutionSequence != 1 {
			t.Fatalf("disjoint queue lost author %d", i)
		}
		if _, ok := s.RunnerTask(tasks[i].ID); !ok {
			t.Fatalf("disjoint queue lost task %d", i)
		}
	}
}
