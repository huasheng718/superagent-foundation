package repo

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ProjectRunLimit caps concurrently admitted top-level SOP runs per project on
// platform (cloud) execution. Personal machines are counted per machine
// against their optional user-configured Runner.RunLimit instead.
const ProjectRunLimit = 10

// Slot accounting is partitioned into buckets: "project:<id>" for platform
// execution (the empty id is the shared bucket of unassigned runs) and
// "runner:<id>" for a personal machine. Each bucket has its own lock row, so
// different projects and machines never wait on each other.
//
// Lock rule: a transaction that takes bucket locks takes the complete sorted
// set FIRST, before any Run row lock. Only transactions that claim a slot
// (insert/restart/promote) or change a machine limit take them. Writes that
// keep or release a slot never do: a release that races admission can only
// make it over-count and queue a little longer, never exceed the limit.
type runAdmissionLockRow struct {
	ID string `gorm:"column:id;primaryKey;size:128"`
}

func (runAdmissionLockRow) TableName() string { return "run_admission_locks" }

// lockAdmissionGate locks one bucket. The row is runtime lock bookkeeping
// created on first use, not seed data.
func lockAdmissionGate(d *deps, tx *gorm.DB, key string) error {
	row := runAdmissionLockRow{ID: key}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return err
	}
	return d.locking(tx.Where("id = ?", row.ID)).Take(&row).Error
}

// admissionTxOptions makes plain reads inside a bucket-locked transaction see
// every commit up to the lock (MySQL defaults to REPEATABLE READ snapshots).
// sqlite serializes writers on its single test connection already.
func (d *deps) admissionTxOptions() []*sql.TxOptions {
	if d.isMySQL() {
		return []*sql.TxOptions{{Isolation: sql.LevelReadCommitted}}
	}
	return nil
}

// admissionLockRequired aborts a write attempt that turned out to claim a
// slot without holding its complete bucket lock set; writeRun retries with
// those locks in lexical order.
type admissionLockRequired struct{ keys []string }

func (e *admissionLockRequired) Error() string {
	return "admission locks required for " + strings.Join(e.keys, ",")
}

func admissionBucketKey(project, runnerID string, exempt bool) string {
	if exempt {
		return "runner:" + strings.TrimSpace(runnerID)
	}
	return "project:" + strings.TrimSpace(project)
}

// runAdmissionBucketKey is the persisted bucket of a run whose project and
// exemption are already frozen on it.
func runAdmissionBucketKey(run domain.Run) string {
	if len(run.ExecutionContext.NodePlacements) > 0 {
		// Keep a scalar marker for old readers without allowing it to double
		// count the additive membership rows.
		return "placed:" + strings.TrimSpace(run.ID)
	}
	return admissionBucketKey(run.ExecutionContext.ProjectID, run.ExecutionContext.ExecutionRunnerID, run.ConcurrencyExempt)
}

type runnerAdmissionInfo struct {
	personal bool
	limit    int
}

// admissionScope caches per-transaction lookups so legacy rows without
// persisted quota metadata do not issue one runner/issue query each.
type admissionScope struct {
	tx      *gorm.DB
	runners map[string]runnerAdmissionInfo
	issues  map[string]string
}

func newAdmissionScope(tx *gorm.DB) *admissionScope {
	return &admissionScope{tx: tx, runners: map[string]runnerAdmissionInfo{}, issues: map[string]string{}}
}

func sortedUniqueAdmissionKeys(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// buckets returns every limited bucket frozen by a Run. A placed Run can
// reserve several personal Runner limits and the shared project limit when a
// shared Runner participates. Unlimited personal Runners do not need a lock.
func (s *admissionScope) buckets(run domain.Run) ([]string, error) {
	if len(run.ExecutionContext.NodePlacements) == 0 {
		key := admissionBucketKey(run.ExecutionContext.ProjectID, run.ExecutionContext.ExecutionRunnerID, run.ConcurrencyExempt)
		limit, err := s.limit(key)
		if err != nil || limit <= 0 {
			return nil, err
		}
		return []string{key}, nil
	}
	keys := []string{}
	for _, placement := range run.ExecutionContext.NodePlacements {
		info, err := s.runner(placement.RunnerID)
		if err != nil {
			return nil, err
		}
		if info.personal {
			if info.limit > 0 {
				keys = append(keys, admissionBucketKey("", placement.RunnerID, true))
			}
			continue
		}
		keys = append(keys, admissionBucketKey(run.ExecutionContext.ProjectID, "", false))
	}
	return sortedUniqueAdmissionKeys(keys), nil
}

func (s *admissionScope) membership(runID string) ([]string, error) {
	var rows []runAdmissionMembershipRow
	if err := s.tx.Where("run_id = ?", runID).Order("bucket ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.Bucket)
	}
	return sortedUniqueAdmissionKeys(keys), nil
}

func (s *admissionScope) rowBuckets(row runRow) ([]string, error) {
	if membership, err := s.membership(row.ID); err != nil {
		return nil, err
	} else if len(membership) > 0 {
		return membership, nil
	}
	run := runFromRow(row)
	if len(run.ExecutionContext.NodePlacements) > 0 {
		return s.buckets(run)
	}
	if row.QuotaBucket != nil && *row.QuotaBucket != "" && !strings.HasPrefix(*row.QuotaBucket, "placed:") {
		return []string{*row.QuotaBucket}, nil
	}
	key, err := s.rowBucketKey(row)
	if err != nil || key == "" {
		return nil, err
	}
	return []string{key}, nil
}

func (s *admissionScope) runner(id string) (runnerAdmissionInfo, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return runnerAdmissionInfo{}, nil
	}
	if info, ok := s.runners[id]; ok {
		return info, nil
	}
	var row runnerRow
	info := runnerAdmissionInfo{}
	if err := s.tx.Select("id", "owner_user_id", "run_limit").Where("id = ?", id).Take(&row).Error; err == nil {
		info.personal = row.OwnerUserID != nil && *row.OwnerUserID > 0
		if row.RunLimit != nil && *row.RunLimit > 0 {
			info.limit = *row.RunLimit
		}
	} else if err != gorm.ErrRecordNotFound {
		return runnerAdmissionInfo{}, err
	}
	s.runners[id] = info
	return info, nil
}

func (s *admissionScope) project(run domain.Run) (string, error) {
	if id := strings.TrimSpace(run.ExecutionContext.ProjectID); id != "" {
		return id, nil
	}
	issueID := strings.TrimSpace(run.ExecutionContext.IssueID)
	if issueID == "" {
		return "", nil
	}
	if id, ok := s.issues[issueID]; ok {
		return id, nil
	}
	var row issueRow
	project := ""
	if err := s.tx.Select("id", "project_id").Where("id = ?", issueID).Take(&row).Error; err == nil {
		project = row.ProjectID
	} else if err != gorm.ErrRecordNotFound {
		return "", err
	}
	s.issues[issueID] = project
	return project, nil
}

func (s *admissionScope) exempt(run domain.Run) (bool, error) {
	info, err := s.runner(run.ExecutionContext.ExecutionRunnerID)
	return info.personal, err
}

// limit returns the bucket's slot count; 0 means unlimited.
func (s *admissionScope) limit(key string) (int, error) {
	if id, ok := strings.CutPrefix(key, "runner:"); ok {
		info, err := s.runner(id)
		return info.limit, err
	}
	return ProjectRunLimit, nil
}

// rowBucketKey reads the persisted bucket, falling back to execution_context,
// the linked issue, and current runner ownership for legacy rows that the
// 010 backfill has not populated (written by the previous binary).
func (s *admissionScope) rowBucketKey(row runRow) (string, error) {
	if row.QuotaBucket != nil && *row.QuotaBucket != "" {
		return *row.QuotaBucket, nil
	}
	run := runFromRow(row)
	project := ""
	if row.QuotaProjectID != nil {
		project = *row.QuotaProjectID
	} else {
		var err error
		if project, err = s.project(run); err != nil {
			return "", err
		}
	}
	exempt := row.QuotaExempt != nil && *row.QuotaExempt
	if row.QuotaExempt == nil {
		var err error
		if exempt, err = s.exempt(run); err != nil {
			return "", err
		}
	}
	return admissionBucketKey(project, run.ExecutionContext.ExecutionRunnerID, exempt), nil
}

// occupied counts admitted top-level runs in one bucket through
// idx_runs_quota_bucket. Rows with a NULL bucket are legacy rows not yet
// backfilled; they are resolved in Go so they still count.
//
// Conversation workspaces and retired template-free groups do not execute SOPs
// and never reserve SOP capacity. Nested SOPs share their top-level SOP slot.
func (s *admissionScope) occupied(key, exclude string) (int, error) {
	var rows []runRow
	q := s.tx.Select("id", "kind", "workflow_id", "status", "quota_bucket", "quota_project_id", "quota_parent_run_id", "quota_exempt", "admission_state", "execution_context").
		Where("status NOT IN ? AND (admission_state IS NULL OR admission_state <> ?)", []string{"succeeded", "queued"}, "unstarted")
	if exclude != "" {
		q = q.Where("id <> ?", exclude)
	}
	if err := q.Find(&rows).Error; err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		parent := ""
		if row.QuotaParentRunID != nil {
			parent = *row.QuotaParentRunID
		} else {
			parent = runFromRow(row).ExecutionContext.ParentRunID
		}
		if strings.TrimSpace(parent) != "" {
			continue
		}
		if row.Kind == domain.RunKindWorkspace || row.WorkflowID == domain.FreeCollaborationWorkflowID {
			continue
		}
		buckets, err := s.rowBuckets(row)
		if err != nil {
			return 0, err
		}
		for _, bucket := range buckets {
			if bucket == key {
				n++
				break
			}
		}
	}
	return n, nil
}

// hasQueued reports whether a Run already waits in the bucket; a new request
// must not overtake it. Membership rows are resolved for placed candidates.
func (s *admissionScope) hasQueued(key, exclude string) (bool, error) {
	var rows []runRow
	if err := s.tx.Select("id", "kind", "workflow_id", "status", "quota_bucket", "quota_project_id", "quota_parent_run_id", "quota_exempt", "admission_state", "execution_context").Where("status = ? AND id <> ?", "queued", exclude).Find(&rows).Error; err != nil {
		return false, err
	}
	for _, row := range rows {
		buckets, err := s.rowBuckets(row)
		if err != nil {
			return false, err
		}
		for _, bucket := range buckets {
			if bucket == key {
				return true, nil
			}
		}
	}
	return false, nil
}

func queuedMessage(key string) string {
	if strings.HasPrefix(key, "runner:") {
		return "等待执行机器可用运行名额"
	}
	return "等待项目可用运行名额"
}

// prepareRunAdmission freezes the Run's quota identity and decides whether a
// running write claims a slot. lockedKeys is the complete bucket lock set held
// by the caller's transaction (nil for the first optimistic attempt).
func (r *RunsRepo) prepareRunAdmission(tx *gorm.DB, run *domain.Run, current runRow, exists bool, lockedKeys []string) error {
	s := newAdmissionScope(tx)
	if exists {
		cur := runFromRow(current)
		run.AdmissionState = current.AdmissionState
		if current.QueueEntered != nil {
			run.QueueEnteredAt = *current.QueueEntered
		}
		run.ExecutionContext.ParentRunID = cur.ExecutionContext.ParentRunID
		if current.QuotaProjectID != nil {
			run.ExecutionContext.ProjectID = *current.QuotaProjectID
		} else {
			project, err := s.project(cur)
			if err != nil {
				return err
			}
			run.ExecutionContext.ProjectID = project
		}
		if current.QuotaExempt != nil {
			run.ConcurrencyExempt = *current.QuotaExempt
		} else {
			exempt, err := s.exempt(cur)
			if err != nil {
				return err
			}
			run.ConcurrencyExempt = exempt
		}
		// A running write based on an earlier snapshot must not bypass a queue.
		if current.Status == "queued" && run.Status == "running" {
			return fmt.Errorf("run is queued")
		}
	} else {
		project, err := s.project(*run)
		if err != nil {
			return err
		}
		run.ExecutionContext.ProjectID = project
		exempt, err := s.exempt(*run)
		if err != nil {
			return err
		}
		run.ConcurrencyExempt = exempt
	}
	if run.Status == "succeeded" {
		run.AdmissionState = "released"
		return nil
	}
	if run.Status != "running" {
		if exists && current.Status == "queued" {
			if run.Status != "cancelled" && run.Status != "queued" {
				return fmt.Errorf("run is queued")
			}
			if run.Status == "cancelled" {
				run.AdmissionState = "unstarted"
			}
		}
		return nil
	}
	if exists && current.Status != "succeeded" && current.AdmissionState != "unstarted" {
		run.AdmissionState = "held"
		return r.checkParentAdmission(tx, *run)
	}
	run.AdmissionState = "held"
	if strings.TrimSpace(run.ExecutionContext.ParentRunID) != "" {
		return r.checkParentAdmission(tx, *run)
	}
	keys, err := s.buckets(*run)
	if err != nil {
		return err
	}
	locked := make(map[string]bool, len(lockedKeys))
	for _, key := range lockedKeys {
		locked[key] = true
	}
	for _, key := range keys {
		if !locked[key] {
			return &admissionLockRequired{keys: keys}
		}
	}
	queued := false
	for _, key := range keys {
		limit, err := s.limit(key)
		if err != nil {
			return err
		}
		if limit <= 0 {
			continue
		}
		occupied, err := s.occupied(key, run.ID)
		if err != nil {
			return err
		}
		waiting, err := s.hasQueued(key, run.ID)
		if err != nil {
			return err
		}
		if occupied >= limit || waiting {
			queued = true
		}
	}
	if queued {
		run.Status, run.AdmissionState = "queued", "queued"
		if run.QueueEnteredAt.IsZero() {
			run.QueueEnteredAt = time.Now().UTC()
		}
		run.OwnerInstanceID, run.LeaseUntil = "", time.Time{}
		run.CancelRequested, run.PauseRequested = false, false
		for i := range run.Events {
			if run.Events[i].Type == "run.started" && !exists {
				run.Events[i].Type = "run.queued"
				messageKey := "project:"
				if len(keys) > 0 {
					messageKey = keys[0]
				}
				run.Events[i].Message = queuedMessage(messageKey)
			}
		}
	}
	return nil
}

// persistRunAdmissionMemberships freezes the complete bucket set alongside
// the Run row. Existing membership is immutable; a changed placed request is
// rejected before any header update can be committed.
func (r *RunsRepo) persistRunAdmissionMemberships(tx *gorm.DB, run domain.Run, current runRow, exists bool) error {
	if len(run.ExecutionContext.NodePlacements) == 0 {
		return nil
	}
	s := newAdmissionScope(tx)
	want, err := s.buckets(run)
	if err != nil {
		return err
	}
	if exists {
		got, err := s.membership(run.ID)
		if err != nil {
			return err
		}
		if len(got) > 0 && !sameAdmissionKeys(got, want) {
			return fmt.Errorf("placed Run admission membership changed")
		}
	}
	for _, bucket := range want {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&runAdmissionMembershipRow{RunID: run.ID, Bucket: bucket}).Error; err != nil {
			return err
		}
	}
	return nil
}

func sameAdmissionKeys(left, right []string) bool {
	left = sortedUniqueAdmissionKeys(left)
	right = sortedUniqueAdmissionKeys(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (r *RunsRepo) checkParentAdmission(tx *gorm.DB, run domain.Run) error {
	parentID := strings.TrimSpace(run.ExecutionContext.ParentRunID)
	seen := map[string]bool{run.ID: true}
	for parentID != "" {
		if seen[parentID] {
			return fmt.Errorf("cyclic parent run")
		}
		seen[parentID] = true
		var parent runRow
		if err := tx.Where("id = ?", parentID).Take(&parent).Error; err != nil {
			return fmt.Errorf("parent run unavailable: %w", err)
		}
		if parent.Status == "succeeded" || parent.Status == "queued" || parent.AdmissionState == "unstarted" {
			return fmt.Errorf("请从顶层 SOP 重新运行，父流程尚未获得运行名额")
		}
		parentID = strings.TrimSpace(runFromRow(parent).ExecutionContext.ParentRunID)
	}
	return nil
}

// PromoteQueuedRuns admits queued Runs in deterministic queue order. Each
// candidate locks its complete sorted bucket set and is promoted only when it
// is oldest in every participating bucket and every bucket has capacity. A
// blocked candidate in one bucket does not prevent a disjoint candidate from
// progressing.
func (r *RunsRepo) PromoteQueuedRuns() ([]string, error) {
	var queued []runRow
	if err := r.DB.Select("id", "queue_entered_at", "created_at").Where("status = ?", "queued").Find(&queued).Error; err != nil {
		return nil, err
	}
	sort.Slice(queued, func(i, j int) bool {
		qi, qj := queued[i].QueueEntered, queued[j].QueueEntered
		if qi == nil && qj != nil {
			return true
		}
		if qi != nil && qj == nil {
			return false
		}
		if qi != nil && qj != nil && !qi.Equal(*qj) {
			return qi.Before(*qj)
		}
		ci, cj := queued[i].Created, queued[j].Created
		if ci == nil && cj != nil {
			return true
		}
		if ci != nil && cj == nil {
			return false
		}
		if ci != nil && cj != nil && !ci.Equal(*cj) {
			return ci.Before(*cj)
		}
		return queued[i].ID < queued[j].ID
	})
	var promoted []string
	var firstErr error
	for _, row := range queued {
		ok, err := r.promoteCandidate(row.ID)
		if ok {
			promoted = append(promoted, row.ID)
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("promote %s: %w", row.ID, err)
		}
	}
	return promoted, firstErr
}

func (r *RunsRepo) promoteCandidate(id string) (bool, error) {
	promoted := false
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		s := newAdmissionScope(tx)
		// Read the candidate without owning it, derive the complete lock set,
		// then acquire bucket locks before taking the Run row lock.
		var candidate runRow
		if err := tx.Where("id = ?", id).Take(&candidate).Error; err != nil {
			return err
		}
		if candidate.Status != "queued" {
			return nil
		}
		keys, err := s.rowBuckets(candidate)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := lockAdmissionGate(r.deps, tx, key); err != nil {
				return err
			}
		}
		row, err := r.takeRunForUpdate(tx, id)
		if err != nil {
			return err
		}
		if row.Status != "queued" {
			return nil
		}
		currentKeys, err := s.rowBuckets(row)
		if err != nil {
			return err
		}
		if !sameAdmissionKeys(keys, currentKeys) {
			return nil
		}
		// The Run row was locked after the bucket gates, so all checks below
		// observe a single frozen candidate and cannot partially reserve it.
		for _, key := range keys {
			limit, err := s.limit(key)
			if err != nil {
				return err
			}
			if limit <= 0 {
				continue
			}
			occupied, err := s.occupied(key, id)
			if err != nil {
				return err
			}
			older, err := s.olderQueuedSharesBucket(tx, id, row, key)
			if err != nil {
				return err
			}
			if occupied >= limit || older {
				return nil
			}
		}
		now := time.Now().UTC()
		res := tx.Model(&runRow{}).Where("id = ? AND status = ?", id, "queued").Updates(map[string]any{
			"status": "running", "admission_state": "held", "revision": gorm.Expr("revision + 1"), "updated_at": now,
			"cancel_requested": false, "pause_requested": false, "owner_instance_id": "", "lease_until": nil,
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		var seq int64
		if err := tx.Model(&runEventRow{}).Select("COALESCE(MAX(seq), -1) + 1").Where("run_id = ?", id).Scan(&seq).Error; err != nil {
			return err
		}
		ev := domain.RunEvent{ID: fmt.Sprintf("adm-%d-%d", now.UnixNano(), seq), RunID: id, Type: "run.started", Message: "获得运行名额，开始执行", Timestamp: now}
		if err := tx.Create(&runEventRow{RunID: id, Seq: seq, EventID: ev.ID, Type: ev.Type, Message: ev.Message, Payload: jsonBytes(ev), Ts: &now}).Error; err != nil {
			return err
		}
		promoted = true
		return nil
	}, r.admissionTxOptions()...)
	return promoted, err
}

func (s *admissionScope) olderQueuedSharesBucket(tx *gorm.DB, candidateID string, candidate runRow, key string) (bool, error) {
	var rows []runRow
	if err := tx.Select("id", "kind", "workflow_id", "status", "quota_bucket", "quota_project_id", "quota_parent_run_id", "quota_exempt", "admission_state", "execution_context", "queue_entered_at", "created_at").Where("status = ?", "queued").Find(&rows).Error; err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.ID == candidateID || !queuedBefore(row, candidate) {
			continue
		}
		buckets, err := s.rowBuckets(row)
		if err != nil {
			return false, err
		}
		for _, bucket := range buckets {
			if bucket == key {
				return true, nil
			}
		}
	}
	return false, nil
}

func queuedBefore(left, right runRow) bool {
	if left.QueueEntered != nil && right.QueueEntered == nil {
		return true
	}
	if left.QueueEntered == nil && right.QueueEntered != nil {
		return false
	}
	if left.QueueEntered != nil && right.QueueEntered != nil && !left.QueueEntered.Equal(*right.QueueEntered) {
		return left.QueueEntered.Before(*right.QueueEntered)
	}
	if left.Created != nil && right.Created == nil {
		return true
	}
	if left.Created == nil && right.Created != nil {
		return false
	}
	if left.Created != nil && right.Created != nil && !left.Created.Equal(*right.Created) {
		return left.Created.Before(*right.Created)
	}
	return left.ID < right.ID
}

// CancelQueuedRun settles a queued run as cancelled-before-start. It takes
// only the run row: promotion re-checks the status on that same row.
func (r *RunsRepo) CancelQueuedRun(id string) (bool, error) {
	cancelled := false
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		row, err := r.takeRunForUpdate(tx, id)
		if err != nil {
			return err
		}
		if row.Status != "queued" {
			return nil
		}
		cancelled = true
		return tx.Model(&runRow{}).Where("id = ?", id).Updates(map[string]any{"status": "cancelled", "admission_state": "unstarted", "revision": gorm.Expr("revision + 1"), "updated_at": time.Now().UTC()}).Error
	})
	return cancelled, err
}
