package repo

import (
	"bytes"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// CLI Runner connection store (cli-connect.md). Task state is always
// durable: a runner disconnect only affects delivery, never the task itself.

var (
	ErrRunnerTaskNotClaimable = errors.New("runner task is not claimable")
	ErrRunnerTaskLeaseLost    = errors.New("runner task lease ownership changed")
	ErrRunnerNotFound         = errors.New("runner not found")
)

type runnerTaskPayload struct {
	RegistrationSHA256    string                             `json:"registrationSHA256,omitempty"`
	ExecutionPlacement    *domain.NodeExecutionPlacement     `json:"executionPlacement,omitempty"`
	Execution             *domain.NodeTaskExecution          `json:"execution,omitempty"`
	MaintenanceKey        *domain.RunnerMaintenanceKey       `json:"maintenanceKey,omitempty"`
	ExecutionDeadline     *runnerTaskTime                    `json:"executionDeadline,omitempty"`
	ContributionSpecs     []domain.RunnerContributionSpec    `json:"contributionSpecs,omitempty"`
	PendingContributions  []domain.RunnerContributionReceipt `json:"pendingContributions,omitempty"`
	AcceptedContributions []domain.RunnerContributionReceipt `json:"acceptedContributions,omitempty"`
	DeliveryPolicy        string                             `json:"deliveryPolicy,omitempty"`
	Prompt                string                             `json:"prompt"`
	Workspace             string                             `json:"workspace,omitempty"`
	Args                  []string                           `json:"args,omitempty"`
	ExitCode              *int                               `json:"exitCode,omitempty"`
	Result                string                             `json:"result,omitempty"`
	Error                 string                             `json:"error,omitempty"`
	StartedAt             *runnerTaskTime                    `json:"startedAt,omitempty"`
	CompletedAt           *runnerTaskTime                    `json:"completedAt,omitempty"`
	RunID                 string                             `json:"runId,omitempty"`
	NodeID                string                             `json:"nodeId,omitempty"`
	Invocation            *domain.RunnerInvocation           `json:"invocation,omitempty"`
	Sandbox               *domain.RunnerSandboxSpec          `json:"sandbox,omitempty"`
	Command               string                             `json:"command,omitempty"`
	Action                string                             `json:"action,omitempty"`
	PreviewArchive        *domain.RunnerPreviewArchiveSpec   `json:"previewArchive,omitempty"`
	WorkBranchMessage     string                             `json:"workBranchMessage,omitempty"`
	IntegrationLabel      string                             `json:"integrationLabel,omitempty"`
	// Artifacts holds artifact content uploaded by the runner at completion
	// (name → file content) so the engine can write them into the run
	// workspace and reuse the existing verification/handoff pipeline.
	Artifacts map[string]string         `json:"artifacts,omitempty"`
	Outputs   *domain.RunnerTaskOutputs `json:"outputs,omitempty"`
}

// runnerTaskTime decodes timestamps written by both write paths: Go struct
// marshaling (RFC3339) and the legacy MySQL JSON_SET DATETIME format
// ("2006-01-02 15:04:05.999999"). An unparseable value must never discard
// the whole payload, so parse failures resolve to the zero time.
type runnerTaskTime struct{ time.Time }

func (t *runnerTaskTime) UnmarshalJSON(raw []byte) error {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999", "2006-01-02T15:04:05.999999"} {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed.UTC()
			return nil
		}
	}
	return nil
}

func (t runnerTaskTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.Time.UTC().Format(time.RFC3339Nano))
}

func newRunnerTaskTime(v *time.Time) *runnerTaskTime {
	if v == nil {
		return nil
	}
	return &runnerTaskTime{Time: *v}
}

// toTime collapses a payload timestamp back into a plain pointer, returning
// nil for zero times so JSON output omits them.
func (t *runnerTaskTime) toTime() *time.Time {
	if t == nil || t.Time.IsZero() {
		return nil
	}
	value := t.Time
	return &value
}

func runnerTaskFromRow(row runnerTaskRow) domain.RunnerTask {
	t := domain.RunnerTask{
		ID: row.ID, RunnerID: row.RunnerID, Provider: row.Provider, Status: row.Status,
		LeaseOwner: row.LeaseOwner, DesiredState: row.DesiredState,
		Attempt: row.Attempt, MaxAttempts: row.MaxAttempts,
		CreatedAt: time.Time{}, UpdatedAt: time.Time{},
	}
	if row.LeaseUntil != nil {
		until := *row.LeaseUntil
		t.LeaseUntil = &until
	}
	if row.Created != nil {
		t.CreatedAt = *row.Created
	}
	if row.Updated != nil {
		t.UpdatedAt = *row.Updated
	}
	var p runnerTaskPayload
	if json.Unmarshal(row.Payload, &p) == nil {
		t.ExecutionPlacement = p.ExecutionPlacement
		t.Execution = p.Execution
		t.MaintenanceKey = p.MaintenanceKey
		t.ExecutionDeadline = p.ExecutionDeadline.toTime()
		t.ContributionSpecs = p.ContributionSpecs
		t.PendingContributions = p.PendingContributions
		t.AcceptedContributions = p.AcceptedContributions
		for i := range t.PendingContributions {
			t.PendingContributions[i].StorageKey = t.PendingContributions[i].SHA256
		}
		for i := range t.AcceptedContributions {
			t.AcceptedContributions[i].StorageKey = t.AcceptedContributions[i].SHA256
		}
		t.DeliveryPolicy = p.DeliveryPolicy
		t.Prompt = p.Prompt
		t.Workspace = p.Workspace
		t.Args = p.Args
		t.ExitCode = p.ExitCode
		t.Result = p.Result
		t.Error = p.Error
		t.StartedAt = p.StartedAt.toTime()
		t.CompletedAt = p.CompletedAt.toTime()
		t.RunID = p.RunID
		t.NodeID = p.NodeID
		t.Invocation = p.Invocation
		t.Sandbox = p.Sandbox
		t.Command = p.Command
		t.Action = p.Action
		t.PreviewArchive = p.PreviewArchive
		t.WorkBranchMessage = p.WorkBranchMessage
		t.IntegrationLabel = p.IntegrationLabel
		t.Artifacts = p.Artifacts
		t.Outputs = p.Outputs
	}
	return t
}

func runnerTaskPayloadBytes(v domain.RunnerTask) []byte {
	return mustJSON(runnerTaskPayload{
		ExecutionPlacement: v.ExecutionPlacement, Execution: v.Execution, MaintenanceKey: v.MaintenanceKey,
		ExecutionDeadline: newRunnerTaskTime(v.ExecutionDeadline),
		ContributionSpecs: v.ContributionSpecs, PendingContributions: v.PendingContributions, AcceptedContributions: v.AcceptedContributions,
		DeliveryPolicy: v.DeliveryPolicy,
		Prompt:         v.Prompt, Workspace: v.Workspace, Args: v.Args, ExitCode: v.ExitCode,
		Result: v.Result, Error: v.Error, StartedAt: newRunnerTaskTime(v.StartedAt),
		CompletedAt: newRunnerTaskTime(v.CompletedAt), RunID: v.RunID, NodeID: v.NodeID,
		Invocation: v.Invocation, Sandbox: v.Sandbox, Command: v.Command, Action: v.Action,
		PreviewArchive: v.PreviewArchive, WorkBranchMessage: v.WorkBranchMessage, Artifacts: v.Artifacts,
		Outputs: v.Outputs, IntegrationLabel: v.IntegrationLabel,
	})
}

func runnerTaskRowValues(v domain.RunnerTask) map[string]any {
	return map[string]any{
		"id": v.ID, "runner_id": v.RunnerID, "provider": v.Provider, "status": v.Status,
		"lease_owner": v.LeaseOwner, "lease_until": v.LeaseUntil, "desired_state": v.DesiredState,
		"attempt": v.Attempt, "max_attempts": v.MaxAttempts, "payload": runnerTaskPayloadBytes(v),
		"created_at": timePtr(v.CreatedAt), "updated_at": timePtr(v.UpdatedAt),
	}
}

func takeRunnerTask(db *gorm.DB, id string) (runnerTaskRow, bool) {
	var row runnerTaskRow
	if err := db.Where("id = ?", id).Take(&row).Error; err != nil {
		return runnerTaskRow{}, false
	}
	return row, true
}

// ---------- Runner registry ----------

func (r *RunnersRepo) PutRunner(v domain.Runner) error {
	if strings.TrimSpace(v.ID) == "" {
		return fmt.Errorf("runner id is required")
	}
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	if v.Status == "" {
		v.Status = domain.RunnerStatusOffline
	}
	if v.MaxConcurrency < 1 {
		v.MaxConcurrency = 1
	}
	return upsert(r.DB, "runners", map[string]any{
		"id": v.ID, "name": v.Name, "status": v.Status, "os": v.OS, "arch": v.Arch,
		"runner_version": v.RunnerVersion, "labels": jsonBytes(v.Labels), "max_concurrency": v.MaxConcurrency,
		"owner_user_id": nullableInt64(v.OwnerUserID), "session_token_hash": v.SessionTokenHash,
		"session_expires_at": v.SessionExpiresAt, "runtimes": jsonBytes(v.Runtimes),
		"last_seen_at": v.LastSeenAt, "created_at": timePtr(v.CreatedAt), "updated_at": timePtr(v.UpdatedAt),
	})
}

// MaxRunnerRunLimit bounds the user-configurable per-machine run limit.
const MaxRunnerRunLimit = 100

// SetRunnerRunLimit is the only writer of run_limit; PutRunner (register and
// heartbeat) deliberately omits the column so a reconnect never resets it.
// It takes only this machine's admission bucket so a limit change cannot
// interleave with an admission decision on the same machine.
func (r *RunnersRepo) SetRunnerRunLimit(id string, limit int) error {
	if limit < 0 || limit > MaxRunnerRunLimit {
		return fmt.Errorf("最多同时运行条数需在 0-%d 之间（0 表示不限制）", MaxRunnerRunLimit)
	}
	var value any
	if limit > 0 {
		value = limit
	}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockAdmissionGate(r.deps, tx, admissionBucketKey("", id, true)); err != nil {
			return err
		}
		res := tx.Model(&runnerRow{}).Where("id = ?", id).Updates(map[string]any{"run_limit": value, "updated_at": time.Now().UTC()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrRunnerNotFound
		}
		return nil
	})
}

func (r *RunnersRepo) Runner(id string) (domain.Runner, bool) {
	return r.runnerRow(id)
}

func (r *RunnersRepo) runnerRow(id string) (domain.Runner, bool) {
	var row runnerRow
	if err := r.DB.Where("id = ?", id).Take(&row).Error; err != nil {
		return domain.Runner{}, false
	}
	return runnerFromRow(row), true
}

func runnerFromRow(row runnerRow) domain.Runner {
	v := domain.Runner{
		ID: row.ID, Name: row.Name, Status: row.Status, OS: row.OS, Arch: row.Arch,
		RunnerVersion: row.RunnerVersion, MaxConcurrency: row.MaxConcurrency,
		SessionTokenHash: row.SessionTokenHash,
	}
	if row.OwnerUserID != nil {
		v.OwnerUserID = *row.OwnerUserID
	}
	if row.RunLimit != nil && *row.RunLimit > 0 {
		v.RunLimit = *row.RunLimit
	}
	if len(row.Labels) > 0 {
		_ = json.Unmarshal(row.Labels, &v.Labels)
	}
	if len(row.Runtimes) > 0 {
		_ = json.Unmarshal(row.Runtimes, &v.Runtimes)
	}
	if row.SessionExpiresAt != nil {
		t := *row.SessionExpiresAt
		v.SessionExpiresAt = &t
	}
	if row.LastSeenAt != nil {
		t := *row.LastSeenAt
		v.LastSeenAt = &t
	}
	if row.Created != nil {
		v.CreatedAt = *row.Created
	}
	if row.Updated != nil {
		v.UpdatedAt = *row.Updated
	}
	return v
}

func (r *RunnersRepo) Runners() []domain.Runner {
	var rows []runnerRow
	if err := r.DB.Order("created_at").Find(&rows).Error; err != nil {
		return []domain.Runner{}
	}
	out := make([]domain.Runner, 0, len(rows))
	for _, row := range rows {
		out = append(out, runnerFromRow(row))
	}
	return out
}

// RunnerSessionByHash resolves a session token hash to its runner. Expired
// sessions are rejected so a leaked old session token stops working.
func (r *RunnersRepo) RunnerSessionByHash(hash string) (domain.Runner, bool) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return domain.Runner{}, false
	}
	var row runnerRow
	if err := r.DB.Where("session_token_hash = ?", hash).Take(&row).Error; err != nil {
		return domain.Runner{}, false
	}
	if row.SessionExpiresAt != nil && !row.SessionExpiresAt.After(time.Now().UTC()) {
		return domain.Runner{}, false
	}
	return runnerFromRow(row), true
}

// RunnerHeartbeat records liveness and the latest runtime inventory. The
// server-side disabled flag survives: a CLI disabled from the 执行器 page
// stays disabled even though the daemon keeps reporting it online.
func (r *RunnersRepo) RunnerHeartbeat(id string, runtimes []domain.RunnerRuntime, version string, now, sessionExpiresAt time.Time) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var row runnerRow
		if err := r.locking(tx).Where("id = ?", id).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRunnerNotFound
			}
			return err
		}
		current := runnerFromRow(row)
		disabled := map[string]bool{}
		authModes := map[string]string{}
		for _, rt := range current.Runtimes {
			provider := strings.ToLower(strings.TrimSpace(rt.Provider))
			if rt.Status == domain.RuntimeStatusDisabled {
				disabled[provider] = true
			}
			if rt.AuthMode == domain.RuntimeAuthLocal || rt.AuthMode == domain.RuntimeAuthPersonal {
				authModes[provider] = rt.AuthMode
			}
		}
		// Never mutate the caller's inventory slice while merging platform state.
		runtimes = append([]domain.RunnerRuntime(nil), runtimes...)
		for i := range runtimes {
			provider := strings.ToLower(strings.TrimSpace(runtimes[i].Provider))
			if disabled[provider] {
				runtimes[i].Status = domain.RuntimeStatusDisabled
			}
			if domain.IsAgentCLIKind(runtimes[i].Provider) {
				runtimes[i].AuthMode = domain.RuntimeAuthPersonal
			} else if mode := authModes[provider]; mode != "" {
				runtimes[i].AuthMode = mode
			} else if strings.EqualFold(runtimes[i].Provider, "codebuddy") {
				runtimes[i].AuthMode = domain.RuntimeAuthPersonal
			} else {
				runtimes[i].AuthMode = domain.RuntimeAuthLocal
			}
		}
		raw, err := json.Marshal(runtimes)
		if err != nil {
			return err
		}
		updates := map[string]any{
			"status": domain.RunnerStatusOnline, "last_seen_at": now, "runtimes": string(raw),
			"session_expires_at": sessionExpiresAt, "updated_at": now,
		}
		if version = strings.TrimSpace(version); version != "" {
			updates["runner_version"] = version
		}
		res := tx.Model(&runnerRow{}).Where("id = ?", id).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrRunnerNotFound
		}
		return nil
	})
}

// SetRunnerRuntimeAuthMode selects local CLI login or the responsible user's
// saved provider credential for one runtime. The daemon inventory heartbeat
// cannot overwrite this server-managed preference.
func (r *RunnersRepo) SetRunnerRuntimeAuthMode(runnerID, provider, authMode string, now time.Time) error {
	if authMode != domain.RuntimeAuthLocal && authMode != domain.RuntimeAuthPersonal {
		return fmt.Errorf("invalid runtime auth mode %q", authMode)
	}
	if strings.EqualFold(provider, "codebuddy") && authMode != domain.RuntimeAuthPersonal {
		return fmt.Errorf("CodeBuddy 仅支持个人凭证")
	}
	if domain.IsAgentCLIKind(provider) && authMode != domain.RuntimeAuthPersonal {
		return fmt.Errorf("%s 仅支持个人凭证", provider)
	}
	return r.updateRunnerRuntimes(runnerID, func(runtimes []domain.RunnerRuntime) (bool, error) {
		found := false
		for i := range runtimes {
			if strings.EqualFold(runtimes[i].Provider, provider) {
				runtimes[i].AuthMode = authMode
				found = true
			}
		}
		if !found {
			return false, fmt.Errorf("runtime %q not found on runner %s", provider, runnerID)
		}
		return true, nil
	}, now)
}

// SetRunnerRuntimeStatus enables or disables one discovered CLI on a runner.
// Disabled CLIs stop receiving tasks of that provider.
func (r *RunnersRepo) SetRunnerRuntimeStatus(runnerID, provider, status string, now time.Time) error {
	if status != domain.RuntimeStatusOnline && status != domain.RuntimeStatusDisabled {
		return fmt.Errorf("invalid runtime status %q", status)
	}
	return r.updateRunnerRuntimes(runnerID, func(runtimes []domain.RunnerRuntime) (bool, error) {
		found := false
		for i := range runtimes {
			if strings.EqualFold(runtimes[i].Provider, provider) {
				runtimes[i].Status = status
				found = true
			}
		}
		if !found {
			return false, fmt.Errorf("runtime %q not found on runner %s", provider, runnerID)
		}
		return true, nil
	}, now)
}

func (r *RunnersRepo) updateRunnerRuntimes(runnerID string, mutate func([]domain.RunnerRuntime) (bool, error), now time.Time) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var row runnerRow
		if err := r.locking(tx).Where("id = ?", runnerID).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRunnerNotFound
			}
			return err
		}
		current := runnerFromRow(row)
		changed, err := mutate(current.Runtimes)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		raw, err := json.Marshal(current.Runtimes)
		if err != nil {
			return err
		}
		return tx.Model(&runnerRow{}).Where("id = ?", runnerID).Updates(map[string]any{
			"runtimes": string(raw), "updated_at": now,
		}).Error
	})
}

// MarkOfflineRunners flags runners whose heartbeat is older than threshold.
// Callers decide their own thresholds; the store only persists the state.
func (r *RunnersRepo) MarkOfflineRunners(threshold time.Duration, now time.Time) {
	if r.DB == nil {
		return
	}
	cutoff := now.Add(-threshold)
	_ = r.DB.Model(&runnerRow{}).
		Where("status <> ? AND last_seen_at IS NOT NULL AND last_seen_at < ?", domain.RunnerStatusOffline, cutoff).
		Updates(map[string]any{"status": domain.RunnerStatusOffline, "updated_at": now})
}

// MarkRunnerOffline records an intentional daemon disconnect while retaining
// the runner identity, session, and user preference for the next restart.
func (r *RunnersRepo) MarkRunnerOffline(id string, now time.Time) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrRunnerNotFound
	}
	res := r.DB.Model(&runnerRow{}).Where("id = ?", id).Updates(map[string]any{
		"status": domain.RunnerStatusOffline, "updated_at": now,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrRunnerNotFound
	}
	return nil
}

// ---------- Runner tasks ----------

// PutRunnerTask creates a task or edits an unclaimed queued definition.
// Existing execution identities must be mutated through their lease methods.
func (r *RunnersRepo) PutRunnerTask(v domain.RunnerTask) error {
	if runnerTaskHasNodeAuthority(v) {
		return fmt.Errorf("%w: placed tasks require the owned queue path", domain.ErrRunnerPlacementInvalid)
	}
	v, err := prepareRunnerTask(v)
	if err != nil {
		return err
	}
	return r.DB.Transaction(func(tx *gorm.DB) error { return r.putPreparedRunnerTask(tx, v) }, r.admissionTxOptions()...)
}

func runnerTaskHasNodeAuthority(task domain.RunnerTask) bool {
	return task.ExecutionPlacement != nil || task.Execution != nil || task.MaintenanceKey != nil || (task.Invocation != nil && len(task.Invocation.ContributionInputs) > 0)
}

func prepareRunnerTask(v domain.RunnerTask) (domain.RunnerTask, error) {
	if err := domain.ValidateRunnerGovernedContext(v); err != nil {
		return v, err
	}
	if v.ExecutionDeadline != nil && v.ExecutionDeadline.IsZero() {
		return v, fmt.Errorf("executionDeadline must be a nonzero absolute timestamp")
	}
	if len(v.PendingContributions) != 0 || len(v.AcceptedContributions) != 0 {
		return v, invalidContribution("receipt metadata is owned by staged upload and successful completion")
	}
	if strings.TrimSpace(v.ID) == "" {
		return v, fmt.Errorf("runner task id is required")
	}
	if strings.TrimSpace(v.Provider) == "" {
		return v, fmt.Errorf("runner task provider is required")
	}
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	if v.Status == "" {
		v.Status = domain.RunnerTaskQueued
	}
	if v.MaxAttempts < 1 {
		v.MaxAttempts = 1
	}
	v.UpdatedAt = now
	return v, nil
}

func (r *RunnersRepo) putPreparedRunnerTask(tx *gorm.DB, v domain.RunnerTask) error {
	registration := ""
	if v.RunnerID != "" {
		var runner runnerRow
		if err := tx.Where("id = ?", v.RunnerID).Take(&runner).Error; err == nil && runner.SessionTokenHash != "" {
			registration, _ = domain.RunnerRegistrationSHA256(runner.SessionTokenHash)
		}
	}
	inserted, err := insertIgnore(tx, "runner_tasks", runnerTaskRowValuesWithRegistration(v, registration))
	if err != nil || inserted {
		return err
	}
	// Insert first: a missing-ID FOR UPDATE under MySQL REPEATABLE READ can
	// deadlock unrelated creators through gap locks. On collision, re-read the
	// actual winning row under lock; an earlier absence check cannot protect a
	// concurrently inserted placed task from a legacy pointer-stripping edit.
	var current runnerTaskRow
	if err := r.locking(tx).Where("id = ?", v.ID).Take(&current).Error; err != nil {
		return err
	}
	var payload runnerTaskPayload
	if json.Unmarshal(current.Payload, &payload) != nil || runnerTaskHasNodeAuthority(runnerTaskFromRow(current)) {
		return fmt.Errorf("%w: legacy queue cannot replace placed task authority", domain.ErrRunnerPlacementInvalid)
	}
	// A task ID is a durable execution identity. Only unclaimed queued
	// definitions can be edited; retries use ClaimRunnerTask, while new
	// logical work must use a fresh ID rather than resetting its generation.
	if v.Attempt != 0 || v.Status != domain.RunnerTaskQueued {
		return ErrRunnerTaskLeaseLost
	}
	res := tx.Model(&runnerTaskRow{}).Where("id = ? AND attempt = 0 AND status = ?", v.ID, domain.RunnerTaskQueued).Updates(runnerTaskRowValuesWithRegistration(v, payload.RegistrationSHA256))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrRunnerTaskLeaseLost
	}
	return nil
}

func runnerTaskRowValuesWithRegistration(v domain.RunnerTask, registration string) map[string]any {
	values := runnerTaskRowValues(v)
	if registration != "" {
		payload := runnerTaskPayload{
			RegistrationSHA256: registration, ExecutionPlacement: v.ExecutionPlacement, Execution: v.Execution, MaintenanceKey: v.MaintenanceKey,
			ExecutionDeadline: newRunnerTaskTime(v.ExecutionDeadline), ContributionSpecs: v.ContributionSpecs, PendingContributions: v.PendingContributions, AcceptedContributions: v.AcceptedContributions,
			DeliveryPolicy: v.DeliveryPolicy, Prompt: v.Prompt, Workspace: v.Workspace, Args: v.Args, ExitCode: v.ExitCode, Result: v.Result, Error: v.Error,
			StartedAt: newRunnerTaskTime(v.StartedAt), CompletedAt: newRunnerTaskTime(v.CompletedAt), RunID: v.RunID, NodeID: v.NodeID, Invocation: v.Invocation, Sandbox: v.Sandbox,
			Command: v.Command, Action: v.Action, PreviewArchive: v.PreviewArchive, WorkBranchMessage: v.WorkBranchMessage, IntegrationLabel: v.IntegrationLabel, Artifacts: v.Artifacts, Outputs: v.Outputs,
		}
		values["payload"] = mustJSON(payload)
	}
	return values
}

func runnerTaskRowValuesPreservingRegistration(tx *gorm.DB, v domain.RunnerTask) map[string]any {
	var row runnerTaskRow
	if err := tx.Select("payload").Where("id = ?", v.ID).Take(&row).Error; err == nil {
		var payload runnerTaskPayload
		if json.Unmarshal(row.Payload, &payload) == nil && payload.RegistrationSHA256 != "" {
			return runnerTaskRowValuesWithRegistration(v, payload.RegistrationSHA256)
		}
	}
	return runnerTaskRowValues(v)
}

// QueueRunnerMaintenanceTask serializes maintenance creation on the durable
// Run row. Concurrent API/worker processes reuse one active task per action;
// after it ends, new work receives its caller-supplied fresh ID and retry budget.
func (r *RunnersRepo) QueueRunnerMaintenanceTask(v domain.RunnerTask) (domain.RunnerTask, bool, error) {
	v, err := prepareRunnerTask(v)
	if err != nil {
		return domain.RunnerTask{}, false, err
	}
	if strings.TrimSpace(v.RunID) == "" || (v.Action != domain.RunnerTaskActionCleanupRun && v.Action != domain.RunnerTaskActionArchivePreview) || v.Status != domain.RunnerTaskQueued || v.Attempt != 0 {
		return domain.RunnerTask{}, false, fmt.Errorf("maintenance requires a run, supported action and new queued task")
	}
	var task domain.RunnerTask
	created := false
	var txOptions []*sql.TxOptions
	if r.isMySQL() {
		// Run-row locks serialize the same logical queue. READ COMMITTED
		// prevents unrelated Runs' JSON-filtered empty lookups from taking
		// overlapping next-key gaps and deadlocking on their first inserts.
		txOptions = append(txOptions, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	}
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		var run runRow
		if err := r.locking(tx).Select("id").Where("id = ?", v.RunID).Take(&run).Error; err != nil {
			return err
		}
		var active []runnerTaskRow
		// Re-read current tasks after waiting for the Run lock, and keep any
		// reused active task stable until this transaction finishes.
		if err := r.locking(r.runnerTasksForRunAction(tx, v.RunID, v.Action)).Where("status IN ?", []string{domain.RunnerTaskQueued, domain.RunnerTaskLeased, domain.RunnerTaskRunning}).Order("created_at DESC").Find(&active).Error; err != nil {
			return err
		}
		for _, row := range active {
			candidate := runnerTaskFromRow(row)
			// Legacy maintenance remains one active task per Run/action. Placed
			// maintenance is one active task per frozen registration identity.
			if v.MaintenanceKey == nil || sameRunnerMaintenanceKey(candidate.MaintenanceKey, v.MaintenanceKey) {
				task = candidate
				return nil
			}
		}
		// Unlike PutRunnerTask this path must never update a reused identity.
		inserted, err := insertIgnore(tx, "runner_tasks", runnerTaskRowValues(v))
		if err != nil {
			return err
		}
		if !inserted {
			return ErrRunnerTaskLeaseLost
		}
		task, created = v, true
		return nil
	}, txOptions...)
	if err != nil {
		return domain.RunnerTask{}, false, err
	}
	return task, created, nil
}

func sameRunnerMaintenanceKey(a, b *domain.RunnerMaintenanceKey) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.RunID == b.RunID && a.Action == b.Action && a.RunnerID == b.RunnerID && a.RegistrationSHA256 == b.RegistrationSHA256
}

func (r *RunnersRepo) runnerTasksForRunAction(tx *gorm.DB, runID, action string) *gorm.DB {
	return tx.Model(&runnerTaskRow{}).Where(r.sqlJSONText("$.runId")+" = ? AND "+r.sqlJSONText("$.action")+" = ?", "$.runId", runID, "$.action", action)
}

// RunnerTasksForRun returns the complete durable task history for a Run. It
// intentionally has no list limit: placed cleanup must account for failed,
// cancelled and older attempt tasks even after the Run row is deleted.
func (r *RunnersRepo) RunnerTasksForRun(runID string) []domain.RunnerTask {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	var rows []runnerTaskRow
	if err := r.DB.Model(&runnerTaskRow{}).Where(r.sqlJSONText("$.runId")+" = ?", "$.runId", runID).Order("created_at, id").Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]domain.RunnerTask, 0, len(rows))
	for _, row := range rows {
		out = append(out, runnerTaskFromRow(row))
	}
	return out
}

// LatestRunnerTaskForRunAction includes terminal history and legacy task IDs.
func (r *RunnersRepo) LatestRunnerTaskForRunAction(runID, action string) (domain.RunnerTask, bool) {
	var row runnerTaskRow
	if err := r.runnerTasksForRunAction(r.DB, runID, action).
		Order("CASE WHEN status IN ('queued', 'leased', 'running') THEN 0 ELSE 1 END").
		Order("created_at DESC").Order("id DESC").Take(&row).Error; err != nil {
		return domain.RunnerTask{}, false
	}
	return runnerTaskFromRow(row), true
}

func (r *RunnersRepo) RunnerTask(id string) (domain.RunnerTask, bool) {
	row, ok := takeRunnerTask(r.DB, id)
	if !ok {
		return domain.RunnerTask{}, false
	}
	return runnerTaskFromRow(row), true
}

// RunnerTaskControl reads only the lease and state polled by active runners.
// Prompt, invocation, results and artifact bodies are not needed by this API.
func (r *RunnersRepo) RunnerTaskControl(id string) (domain.RunnerTask, bool) {
	var row struct {
		ID                string
		DesiredState      string
		Status            string
		LeaseOwner        string
		Attempt           int
		LeaseUntil        *time.Time
		ExecutionDeadline *string
	}
	selectSQL := "id, desired_state, status, lease_owner, attempt, lease_until, " + r.sqlJSONText("$.executionDeadline") + " AS execution_deadline"
	if err := r.DB.Model(&runnerTaskRow{}).Select(selectSQL, "$.executionDeadline").Where("id = ?", id).Take(&row).Error; err != nil {
		return domain.RunnerTask{}, false
	}
	var deadline *time.Time
	if row.ExecutionDeadline != nil {
		parsed, _ := time.Parse(time.RFC3339Nano, *row.ExecutionDeadline)
		deadline = &parsed
	}
	return domain.RunnerTask{ID: row.ID, DesiredState: row.DesiredState, Status: row.Status, LeaseOwner: row.LeaseOwner, Attempt: row.Attempt, LeaseUntil: row.LeaseUntil, ExecutionDeadline: deadline}, true
}

func (r *RunnersRepo) RunnerTasks() []domain.RunnerTask {
	var rows []runnerTaskRow
	if err := r.DB.Order("created_at DESC").Limit(200).Find(&rows).Error; err != nil {
		return []domain.RunnerTask{}
	}
	out := make([]domain.RunnerTask, 0, len(rows))
	for _, row := range rows {
		out = append(out, runnerTaskFromRow(row))
	}
	return out
}

// RunnerTaskAvailable checks for eligible queued work without taking a lease.
func (r *RunnersRepo) RunnerTaskAvailable(runnerID string, providers []string) (bool, error) {
	return runnerTaskAvailable(r.DB, runnerID, providers)
}

// RunnerTaskAvailableForSession is a signal-only preflight. It rechecks the
// current principal but neither leases a task nor authorizes placed execution;
// the subsequent claim must still acquire Run/task/Runner locks and revalidate.
func (r *RunnersRepo) RunnerTaskAvailableForSession(runnerID, sessionHash string, providers []string, now time.Time) (bool, error) {
	if !domain.ValidWorkspaceContextSHA256(sessionHash) {
		return false, ErrRunnerPlacementSessionInvalid
	}
	available := false
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		if err := r.validateRunnerClaimSessionTx(tx, runnerID, sessionHash, now); err != nil {
			return err
		}
		var err error
		available, err = runnerTaskAvailable(tx, runnerID, providers)
		return err
	})
	return available, err
}

func runnerTaskAvailable(db *gorm.DB, runnerID string, providers []string) (bool, error) {
	if strings.TrimSpace(runnerID) == "" || len(providers) == 0 {
		return false, nil
	}
	allowed := make([]string, 0, len(providers))
	for _, provider := range providers {
		if provider = strings.TrimSpace(provider); provider != "" {
			allowed = append(allowed, provider)
		}
	}
	if len(allowed) == 0 {
		return false, nil
	}
	var rows []runnerTaskRow
	err := db.Select("id").Where("status = ? AND (runner_id = '' OR runner_id = ?) AND provider IN ?", domain.RunnerTaskQueued, runnerID, allowed).Limit(1).Find(&rows).Error
	return len(rows) > 0, err
}

// ClaimRunnerTask is the legacy internal/test claim entry. It must never lease
// tasks carrying placed execution/input/maintenance authority without a session.
func (r *RunnersRepo) ClaimRunnerTask(runnerID string, providers []string, now time.Time, leaseTTL time.Duration) (domain.RunnerTask, bool, error) {
	return r.claimRunnerTask(runnerID, "", providers, now, leaseTTL, false)
}

// ClaimRunnerTaskForSession rechecks the authenticated session after acquiring
// the relevant locks. HTTP supplies the server-authenticated hash, never JSON.
func (r *RunnersRepo) ClaimRunnerTaskForSession(runnerID, sessionHash string, providers []string, now time.Time, leaseTTL time.Duration) (domain.RunnerTask, bool, error) {
	if !domain.ValidWorkspaceContextSHA256(sessionHash) {
		return domain.RunnerTask{}, false, ErrRunnerPlacementSessionInvalid
	}
	if leaseTTL <= 0 {
		return domain.RunnerTask{}, false, fmt.Errorf("runner task lease TTL must be positive")
	}
	return r.claimRunnerTask(runnerID, sessionHash, providers, now, leaseTTL, true)
}

func (r *RunnersRepo) claimRunnerTask(runnerID, sessionHash string, providers []string, now time.Time, leaseTTL time.Duration, authenticated bool) (domain.RunnerTask, bool, error) {
	if !authenticated {
		return r.claimRunnerTaskOnce(runnerID, sessionHash, providers, now, leaseTTL, authenticated)
	}
	// A queued placed task can become permanently obsolete while it remains
	// the oldest candidate. Retire only after authenticating the current
	// session, then continue to later work; transient Run/Engine conflicts
	// remain visible and are never retired here.
	for i := 0; i < 64; i++ {
		task, found, err := r.claimRunnerTaskOnce(runnerID, sessionHash, providers, now, leaseTTL, authenticated)
		if err == nil || !errors.Is(err, domain.ErrRunnerPlacementChanged) && !errors.Is(err, ErrRunnerTaskLeaseLost) {
			return task, found, err
		}
		retired, retireErr := r.retireObsoletePlacedTask(runnerID, sessionHash, providers, now)
		if retireErr != nil {
			// The original Run/Engine conflict is authoritative when the
			// supplied deterministic clock is already beyond session expiry;
			// do not turn a legacy lease failure into a retirement-side 401.
			if errors.Is(err, ErrRunnerTaskLeaseLost) && errors.Is(retireErr, ErrRunnerPlacementSessionInvalid) {
				return domain.RunnerTask{}, false, err
			}
			return domain.RunnerTask{}, false, retireErr
		}
		if !retired {
			return domain.RunnerTask{}, false, err
		}
	}
	return domain.RunnerTask{}, false, nil
}

func (r *RunnersRepo) claimRunnerTaskOnce(runnerID, sessionHash string, providers []string, now time.Time, leaseTTL time.Duration, authenticated bool) (domain.RunnerTask, bool, error) {
	if strings.TrimSpace(runnerID) == "" {
		return domain.RunnerTask{}, false, fmt.Errorf("runner id is required")
	}
	if len(providers) == 0 {
		return domain.RunnerTask{}, false, nil
	}
	allowed := make([]string, 0, len(providers))
	for _, p := range providers {
		if p = strings.TrimSpace(p); p != "" {
			allowed = append(allowed, p)
		}
	}
	if len(allowed) == 0 {
		return domain.RunnerTask{}, false, nil
	}
	// Peek without a task lock: placed work must acquire its Run before tasks.
	query := r.DB.Where("status = ? AND (runner_id = '' OR runner_id = ?) AND provider IN ?", domain.RunnerTaskQueued, runnerID, allowed)
	if !authenticated {
		for _, path := range []string{"$.executionPlacement", "$.execution", "$.maintenanceKey", "$.invocation.contributionInputs", "$.invocation.governedContext"} {
			query = query.Where(r.sqlJSONText(path)+" IS NULL", path)
		}
	} else {
		// Node execution and cleanup maintenance are both authenticated, but
		// maintenance is authorized by its frozen registration key below.
		query = query.Where("("+r.sqlJSONText("$.executionPlacement")+" IS NULL OR "+r.sqlJSONText("$.action")+" IS NULL OR "+r.sqlJSONText("$.action")+" = '')", "$.executionPlacement", "$.action", "$.action")
	}
	var candidates []runnerTaskRow
	if err := query.Order("created_at, id").Limit(1).Find(&candidates).Error; err != nil {
		return domain.RunnerTask{}, false, err
	}
	if len(candidates) == 0 {
		if authenticated {
			// Even an empty long-poll must notice rotation/expiry rather than
			// continuing to hold a stale authenticated principal indefinitely.
			err := r.DB.Transaction(func(tx *gorm.DB) error {
				return r.validateRunnerClaimSessionTx(tx, runnerID, sessionHash, now)
			})
			return domain.RunnerTask{}, false, err
		}
		return domain.RunnerTask{}, false, nil
	}
	peek := candidates[0]
	var peekPayload runnerTaskPayload
	if err := json.Unmarshal(peek.Payload, &peekPayload); err != nil {
		return domain.RunnerTask{}, false, fmt.Errorf("%w: malformed queued task payload", domain.ErrRunnerPlacementInvalid)
	}
	peekTask := runnerTaskFromRow(peek)
	var claimed domain.RunnerTask
	var found bool
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var run domain.Run
		var lockedTasks map[string]domain.RunnerTask
		if peekTask.ExecutionPlacement != nil {
			if !authenticated || r.runs == nil || peekTask.Action != "" {
				return ErrRunnerTaskNotClaimable
			}
			var err error
			run, err = r.runs.lockRunnerExecutionRunTx(tx, peekTask.RunID, now)
			if err != nil {
				return err
			}
			lockedTasks, err = r.lockNodeExecutionTasksTx(tx, peekTask)
			if err != nil {
				return err
			}
		}
		var row runnerTaskRow
		if err := r.locking(tx).Where("id = ?", peek.ID).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		// A concurrent queue edit/reclaim must not turn the peek into a new
		// assignment with a different lock set or leak a different invocation.
		if row.Status != domain.RunnerTaskQueued || row.RunnerID != peek.RunnerID || row.Provider != peek.Provider || row.Attempt != peek.Attempt || !bytes.Equal(row.Payload, peek.Payload) {
			return nil
		}
		taskID := row.ID
		queued := runnerTaskFromRow(row)
		if queued.Invocation != nil && queued.Invocation.GovernedContext != nil {
			if !authenticated {
				return ErrRunnerTaskNotClaimable
			}
			if err := domain.ValidateRunnerGovernedContext(queued); err != nil {
				return err
			}
			if r.catalog == nil || r.memory == nil {
				return ErrIntelligenceUnavailable
			}
			if err := r.catalog.validateRunnerReleaseTx(tx, *queued.Invocation.GovernedContext); err != nil {
				return err
			}
			if err := r.memory.validateRunnerMemoriesTx(tx, *queued.Invocation.GovernedContext, now); err != nil {
				return err
			}
			var registration runnerRow
			if err := r.locking(tx).Where("id = ?", runnerID).Take(&registration).Error; err != nil {
				return err
			}
			if !domain.RunnerVersionSupportsGovernedContext(runnerFromRow(registration).RunnerVersion) {
				return fmt.Errorf("governed task requires Runner %s or newer", domain.RunnerVersionGovernedContext)
			}
			if queued.Invocation.GovernedContext.MemoryRecallReceipt != nil && !domain.RunnerVersionSupportsMemoryRecallReceipt(runnerFromRow(registration).RunnerVersion) {
				return fmt.Errorf("memory recall receipt requires Runner %s or newer", domain.RunnerVersionMemoryRecallReceipt)
			}
		}
		if queued.ExecutionPlacement != nil {
			if queued.ExecutionPlacement.RunnerID != runnerID || queued.RunnerID != runnerID {
				return ErrRunnerTaskNotClaimable
			}
			if err := r.runs.validateRunnerExecutionState(run, queued); err != nil {
				return err
			}
			if err := r.validateNodeContributionInputsTx(tx, run, queued, lockedTasks); err != nil {
				return err
			}
			if err := r.validateRunnerPlacementTx(tx, *queued.ExecutionPlacement, sessionHash, now); err != nil {
				return err
			}
		} else if queued.MaintenanceKey != nil {
			if !authenticated || queued.Action == "" || queued.MaintenanceKey.RunID != queued.RunID || queued.MaintenanceKey.Action != queued.Action || queued.MaintenanceKey.RunnerID != runnerID || queued.MaintenanceKey.RegistrationSHA256 == "" {
				return ErrRunnerTaskNotClaimable
			}
			if err := r.validateRunnerClaimSessionTx(tx, runnerID, sessionHash, now); err != nil {
				return err
			}
			registration, regErr := domain.RunnerRegistrationSHA256(sessionHash)
			if regErr != nil || queued.MaintenanceKey.RegistrationSHA256 != registration {
				return ErrRunnerTaskNotClaimable
			}
			if queued.Action != domain.RunnerTaskActionCleanupRun {
				return ErrRunnerTaskNotClaimable
			}
		} else if queued.Execution != nil || (queued.Invocation != nil && len(queued.Invocation.ContributionInputs) > 0) {
			return fmt.Errorf("%w: task is missing its execution placement", domain.ErrRunnerPlacementInvalid)
		} else if authenticated {
			if err := r.validateRunnerClaimSessionTx(tx, runnerID, sessionHash, now); err != nil {
				return err
			}
		}
		checkedAt := leaseCheckTime(tx, now)
		if queued.ExecutionPlacement != nil {
			if err := r.runs.validateRunnerExecutionRunAt(run, checkedAt); err != nil {
				return err
			}
		}
		if domain.ExecutionDeadlineExpired(queued.ExecutionDeadline, checkedAt) {
			queued.Status, queued.Error = domain.RunnerTaskFailed, domain.ErrExecutionDeadlineExceeded.Error()
			completed := checkedAt
			queued.CompletedAt, queued.UpdatedAt = &completed, completed
			scrubRunnerTaskCredentials(&queued)
			return tx.Model(&runnerTaskRow{}).Where("id = ?", taskID).Updates(runnerTaskRowValuesPreservingRegistration(tx, queued)).Error
		}
		leaseAt := checkedAt
		if !authenticated {
			// Internal legacy callers retain their supplied timeline, including
			// deterministic recovery tests. Session claims start after locks.
			leaseAt = now
		}
		leaseUntil := leaseAt.Add(leaseTTL)
		if queued.ExecutionDeadline != nil && queued.ExecutionDeadline.Before(leaseUntil) {
			leaseUntil = *queued.ExecutionDeadline
		}
		updates := map[string]any{
			"status": domain.RunnerTaskLeased, "lease_owner": runnerID,
			"lease_until": leaseUntil, "attempt": row.Attempt + 1,
			"desired_state": "", "updated_at": leaseAt,
		}
		if authenticated {
			registration, regErr := domain.RunnerRegistrationSHA256(sessionHash)
			if regErr != nil {
				return regErr
			}
			updates["payload"] = runnerTaskRowValuesWithRegistration(queued, registration)["payload"]
		}
		res := tx.Model(&runnerTaskRow{}).
			Where("id = ? AND status = ?", taskID, domain.RunnerTaskQueued).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		row, ok := takeRunnerTask(tx, taskID)
		if !ok {
			return fmt.Errorf("claimed task %s disappeared", taskID)
		}
		claimed = runnerTaskFromRow(row)
		found = true
		return nil
	})
	if err != nil {
		return domain.RunnerTask{}, false, err
	}
	return claimed, found, nil
}

func (r *RunnersRepo) validateRunnerClaimSessionTx(tx *gorm.DB, runnerID, sessionHash string, now time.Time) error {
	var row runnerRow
	if err := r.locking(tx).Where("id = ?", runnerID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrRunnerPlacementSessionInvalid
		}
		return err
	}
	if !domain.ValidWorkspaceContextSHA256(sessionHash) || row.SessionExpiresAt == nil || !row.SessionExpiresAt.After(leaseCheckTime(tx, now)) || subtle.ConstantTimeCompare([]byte(row.SessionTokenHash), []byte(sessionHash)) != 1 {
		return ErrRunnerPlacementSessionInvalid
	}
	if row.Status != domain.RunnerStatusOnline {
		return fmt.Errorf("%w: registered runner is offline", domain.ErrRunnerPlacementUnavailable)
	}
	return nil
}

// StartRunnerTask moves a leased task into running.
func (r *RunnersRepo) StartRunnerTask(id, runnerID string, attempt int, now time.Time) error {
	return r.startRunnerTask(id, runnerID, "", attempt, now)
}

func (r *RunnersRepo) StartRunnerTaskForSession(id, runnerID, sessionHash string, attempt int, now time.Time) error {
	if !domain.ValidWorkspaceContextSHA256(sessionHash) {
		return ErrRunnerPlacementSessionInvalid
	}
	return r.startRunnerTask(id, runnerID, sessionHash, attempt, now)
}

func (r *RunnersRepo) startRunnerTask(id, runnerID, sessionHash string, attempt int, now time.Time) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		row, err := r.takeRunnerTaskForMutation(tx, id, runnerID, sessionHash, attempt, now)
		if err != nil {
			return err
		}
		if sessionHash != "" {
			now = leaseCheckTime(tx, now)
		}
		task := runnerTaskFromRow(row)
		task.Status = domain.RunnerTaskRunning
		if task.StartedAt == nil {
			started := now
			task.StartedAt = &started
		}
		task.UpdatedAt = now
		values := runnerTaskRowValuesPreservingRegistration(tx, task)
		values["status"], values["updated_at"] = task.Status, now
		return tx.Model(&runnerTaskRow{}).Where("id = ?", id).Updates(values).Error
	})
}

// leaseCheckTime rechecks time after a row-lock wait. Gorm's per-DB clock is
// injectable in tests; the request timestamp remains a lower bound so a
// deterministic future expiry test never moves authority backwards.
func leaseCheckTime(tx *gorm.DB, requested time.Time) time.Time {
	actual := tx.NowFunc().UTC()
	if actual.After(requested) {
		return actual
	}
	return requested
}

// takeRunnerTaskForLease locks the exact live attempt. A matching Runner ID
// alone is insufficient when that Runner has reclaimed the same task.
func (r *RunnersRepo) takeRunnerTaskForLease(tx *gorm.DB, id, runnerID string, attempt int, now time.Time) (runnerTaskRow, bool) {
	if attempt < 1 || strings.TrimSpace(id) == "" || strings.TrimSpace(runnerID) == "" {
		return runnerTaskRow{}, false
	}
	var row runnerTaskRow
	if err := r.locking(tx).
		Where("id = ? AND lease_owner = ? AND attempt = ? AND status IN ? AND lease_until > ?", id, runnerID, attempt, []string{domain.RunnerTaskLeased, domain.RunnerTaskRunning}, now).
		Take(&row).Error; err != nil {
		return runnerTaskRow{}, false
	}
	if row.LeaseUntil == nil || !row.LeaseUntil.After(leaseCheckTime(tx, now)) {
		return runnerTaskRow{}, false
	}
	var payload runnerTaskPayload
	if json.Unmarshal(row.Payload, &payload) != nil || runnerTaskHasNodeAuthority(runnerTaskFromRow(row)) {
		return runnerTaskRow{}, false
	}
	if domain.ExecutionDeadlineExpired(runnerTaskFromRow(row).ExecutionDeadline, leaseCheckTime(tx, now)) {
		return runnerTaskRow{}, false
	}
	return row, true
}

// Session mutation callers use the ordered Run/task/registry authorization
// path. Legacy entry points may mutate only tasks without placed authority.
func (r *RunnersRepo) takeRunnerTaskForMutation(tx *gorm.DB, id, runnerID, sessionHash string, attempt int, now time.Time) (runnerTaskRow, error) {
	return r.takeRunnerTaskForMutationMode(tx, id, runnerID, sessionHash, attempt, now, false)
}

func (r *RunnersRepo) takeRunnerTaskForMutationMode(tx *gorm.DB, id, runnerID, sessionHash string, attempt int, now time.Time, allowCanceled bool) (runnerTaskRow, error) {
	if sessionHash != "" {
		return r.takeRunnerTaskForSessionMode(tx, id, runnerID, sessionHash, attempt, now, allowCanceled)
	}
	row, ok := r.takeRunnerTaskForLease(tx, id, runnerID, attempt, now)
	if !ok {
		return runnerTaskRow{}, ErrRunnerTaskLeaseLost
	}
	return row, nil
}

// RenewRunnerTaskLease extends the lease and returns the desired state so a
// cancel can travel on the same response without extra polling.
func (r *RunnersRepo) RenewRunnerTaskLease(id, runnerID string, attempt int, ttl time.Duration, now time.Time) (string, error) {
	return r.renewRunnerTaskLease(id, runnerID, "", attempt, ttl, now)
}

func (r *RunnersRepo) RenewRunnerTaskLeaseForSession(id, runnerID, sessionHash string, attempt int, ttl time.Duration, now time.Time) (string, error) {
	if !domain.ValidWorkspaceContextSHA256(sessionHash) {
		return "", ErrRunnerPlacementSessionInvalid
	}
	if ttl <= 0 {
		return "", fmt.Errorf("runner task lease ttl must be positive")
	}
	return r.renewRunnerTaskLease(id, runnerID, sessionHash, attempt, ttl, now)
}

func (r *RunnersRepo) renewRunnerTaskLease(id, runnerID, sessionHash string, attempt int, ttl time.Duration, now time.Time) (string, error) {
	var desired string
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		row, err := r.takeRunnerTaskForMutation(tx, id, runnerID, sessionHash, attempt, now)
		if err != nil {
			return err
		}
		if sessionHash != "" {
			now = leaseCheckTime(tx, now)
		}
		until := now.Add(ttl)
		if deadline := runnerTaskFromRow(row).ExecutionDeadline; deadline != nil && deadline.Before(until) {
			until = *deadline
		}
		if err := tx.Model(&runnerTaskRow{}).Where("id = ?", id).Updates(map[string]any{
			"lease_until": until, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		desired = row.DesiredState
		return nil
	})
	return desired, err
}

// CompleteRunnerTask finalizes a task. Only the lease owner may complete it.
// artifacts (legacy, by contract name) and outputs carry what the runner
// reported about its workspace; either may be nil.
func (r *RunnersRepo) CompleteRunnerTask(id, runnerID string, attempt int, status string, exitCode *int, result, errMsg string, artifacts map[string]string, outputs *domain.RunnerTaskOutputs, now time.Time) error {
	return r.completeRunnerTask(id, runnerID, "", attempt, status, exitCode, result, errMsg, artifacts, outputs, now)
}

func (r *RunnersRepo) CompleteRunnerTaskForSession(id, runnerID, sessionHash string, attempt int, status string, exitCode *int, result, errMsg string, artifacts map[string]string, outputs *domain.RunnerTaskOutputs, now time.Time) error {
	if !domain.ValidWorkspaceContextSHA256(sessionHash) {
		return ErrRunnerPlacementSessionInvalid
	}
	return r.completeRunnerTask(id, runnerID, sessionHash, attempt, status, exitCode, result, errMsg, artifacts, outputs, now)
}

func (r *RunnersRepo) completeRunnerTask(id, runnerID, sessionHash string, attempt int, status string, exitCode *int, result, errMsg string, artifacts map[string]string, outputs *domain.RunnerTaskOutputs, now time.Time) error {
	if status != domain.RunnerTaskSuccess && status != domain.RunnerTaskFailed && status != domain.RunnerTaskCanceled {
		return fmt.Errorf("invalid final task status %q", status)
	}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		row, err := r.takeRunnerTaskForMutationMode(tx, id, runnerID, sessionHash, attempt, now, status == domain.RunnerTaskCanceled)
		if err != nil {
			return err
		}
		if sessionHash != "" {
			now = leaseCheckTime(tx, now)
		}
		if row.DesiredState == domain.RunnerTaskDesiredCancel && status != domain.RunnerTaskCanceled {
			return ErrRunnerTaskLeaseLost
		}
		task := runnerTaskFromRow(row)
		if status == domain.RunnerTaskSuccess {
			if err := domain.ValidateRunnerContextEvidence(task, outputs); err != nil {
				return err
			}
			if err := acceptRunnerContributions(tx, &task, outputs, now); err != nil {
				return err
			}
			if err := validateRunnerVerificationCompletion(task); err != nil {
				return err
			}
		} else {
			task.AcceptedContributions = nil
		}
		task.Status = status
		task.ExitCode = exitCode
		task.Result = result
		task.Error = errMsg
		if artifacts != nil {
			task.Artifacts = artifacts
		}
		if outputs != nil {
			task.Outputs = outputs
		}
		scrubRunnerTaskCredentials(&task)
		completed := now
		task.CompletedAt = &completed
		task.LeaseOwner = ""
		task.LeaseUntil = nil
		task.UpdatedAt = now
		return tx.Model(&runnerTaskRow{}).Where("id = ?", id).Updates(runnerTaskRowValuesPreservingRegistration(tx, task)).Error
	})
}

// Verification must attest the imported frozen code. A successful command
// cannot turn a changed checkout into successful verification of its input.
func validateRunnerVerificationCompletion(task domain.RunnerTask) error {
	if task.Execution == nil || task.Execution.Role != domain.NodeTaskRoleVerification {
		return nil
	}
	if task.Invocation == nil {
		return invalidContribution("verification invocation is missing")
	}
	repositories := task.Invocation.Repositories
	if len(repositories) == 0 && task.Invocation.Repository != nil {
		repositories = []domain.RunnerRepository{*task.Invocation.Repository}
	}
	inputs := make(map[string]domain.RunnerContributionInput, len(task.Invocation.ContributionInputs))
	for _, input := range task.Invocation.ContributionInputs {
		if _, duplicate := inputs[input.RepositoryID]; duplicate || !domain.ValidWorkspaceBaseSHA(input.HeadSHA) {
			return invalidContribution("verification requires exactly one valid input per repository")
		}
		inputs[input.RepositoryID] = input
	}
	if len(inputs) != len(repositories) || len(task.AcceptedContributions) != len(repositories) {
		return invalidContribution("verification requires complete frozen inputs and accepted receipts")
	}
	seen := make(map[string]bool, len(repositories))
	for _, repository := range repositories {
		input, ok := inputs[repository.RepositoryID]
		if !ok || seen[repository.RepositoryID] {
			return invalidContribution("verification repository input is missing or duplicate")
		}
		seen[repository.RepositoryID] = true
		matched := 0
		for _, receipt := range task.AcceptedContributions {
			if receipt.RepositoryID == repository.RepositoryID {
				matched++
				if receipt.ResultSHA != input.HeadSHA {
					return invalidContribution("verification changed the frozen input commit")
				}
			}
		}
		if matched != 1 {
			return invalidContribution("verification requires one accepted receipt per input")
		}
	}
	return nil
}

func scrubRunnerTaskCredentials(task *domain.RunnerTask) {
	if task == nil || task.Invocation == nil {
		return
	}
	task.Invocation.CodeBuddyAPIKey = ""
	task.Invocation.CodeBuddyInternetEnvironment = ""
	task.Invocation.CodexAPIKey = ""
	task.Invocation.CodexBaseURL = ""
	task.Invocation.ClaudeCodeAPIKey = ""
	task.Invocation.ClaudeCodeBaseURL = ""
	task.Invocation.AgentCLIAPIKey = ""
	task.Invocation.AgentCLIBaseURL = ""
	if task.Invocation.Repository != nil {
		task.Invocation.Repository.GitToken = ""
	}
	// repositories[*].gitToken must be scrubbed too: unlike the single legacy
	// repository field the array entries would otherwise keep per-checkout
	// tokens in the durable payload after the task reaches a terminal state.
	for i := range task.Invocation.Repositories {
		task.Invocation.Repositories[i].GitToken = ""
	}
}

// CancelRunnerTask asks for cancellation. Queued tasks are canceled outright;
// live tasks are flagged and the flag reaches the runner on lease renewal.
func (r *RunnersRepo) CancelRunnerTask(id string, now time.Time) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&runnerTaskRow{}).
			Where("id = ? AND status = ?", id, domain.RunnerTaskQueued).
			Updates(map[string]any{"status": domain.RunnerTaskCanceled, "lease_owner": "", "lease_until": nil, "desired_state": "", "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			// Scrub credentials in the payload of the canceled task.
			if row, ok := takeRunnerTask(tx, id); ok {
				task := runnerTaskFromRow(row)
				scrubRunnerTaskCredentials(&task)
				completed := now
				task.CompletedAt = &completed
				return tx.Model(&runnerTaskRow{}).Where("id = ?", id).Updates(map[string]any{
					"payload": runnerTaskPayloadBytes(task), "updated_at": now,
				}).Error
			}
			return nil
		}
		return tx.Model(&runnerTaskRow{}).
			Where("id = ? AND status IN ?", id, []string{domain.RunnerTaskLeased, domain.RunnerTaskRunning}).
			Updates(map[string]any{"desired_state": domain.RunnerTaskDesiredCancel, "updated_at": now}).Error
	})
}

// ExpireRunnerTasks recovers tasks whose lease was never renewed (runner
// slept, crashed, or lost network). Retryable tasks go back to QUEUED;
// exhausted tasks fail. Returns the recovered tasks for observability.
func (r *RunnersRepo) ExpireRunnerTasks(now time.Time) ([]domain.RunnerTask, error) {
	var recovered []domain.RunnerTask
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var rows []runnerTaskRow
		if err := r.locking(tx).
			Where("(status IN ? AND lease_until IS NOT NULL AND lease_until <= ?) OR (status IN ? AND "+r.sqlJSONText("$.executionDeadline")+" IS NOT NULL)", []string{domain.RunnerTaskLeased, domain.RunnerTaskRunning}, now, []string{domain.RunnerTaskQueued, domain.RunnerTaskLeased, domain.RunnerTaskRunning}, "$.executionDeadline").
			Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			task := runnerTaskFromRow(row)
			deadlineExpired := domain.ExecutionDeadlineExpired(task.ExecutionDeadline, leaseCheckTime(tx, now))
			if !deadlineExpired && (row.LeaseUntil == nil || row.LeaseUntil.After(now)) {
				continue
			}
			if deadlineExpired {
				task.Status, task.Error = domain.RunnerTaskFailed, domain.ErrExecutionDeadlineExceeded.Error()
				completed := now
				task.CompletedAt = &completed
				scrubRunnerTaskCredentials(&task)
			} else if task.DesiredState == domain.RunnerTaskDesiredCancel {
				task.Status = domain.RunnerTaskCanceled
				completed := now
				task.CompletedAt = &completed
				scrubRunnerTaskCredentials(&task)
			} else if task.Attempt < task.MaxAttempts {
				task.Status = domain.RunnerTaskQueued
			} else {
				task.Status = domain.RunnerTaskFailed
				task.Error = "lease expired"
				completed := now
				task.CompletedAt = &completed
				scrubRunnerTaskCredentials(&task)
			}
			task.LeaseOwner = ""
			task.LeaseUntil = nil
			task.DesiredState = ""
			task.UpdatedAt = now
			if err := tx.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Updates(runnerTaskRowValuesPreservingRegistration(tx, task)).Error; err != nil {
				return err
			}
			recovered = append(recovered, task)
		}
		return nil
	})
	return recovered, err
}

// AppendRunnerTaskEvents stores output under a locked, live lease. The
// (task_id, attempt, seq) key makes retries idempotent within one attempt.
func (r *RunnersRepo) AppendRunnerTaskEvents(taskID, runnerID string, attempt int, events []domain.RunnerTaskEvent, now time.Time) (int, error) {
	return r.appendRunnerTaskEvents(taskID, runnerID, "", attempt, events, now)
}

func (r *RunnersRepo) AppendRunnerTaskEventsForSession(taskID, runnerID, sessionHash string, attempt int, events []domain.RunnerTaskEvent, now time.Time) (int, error) {
	if !domain.ValidWorkspaceContextSHA256(sessionHash) {
		return 0, ErrRunnerPlacementSessionInvalid
	}
	return r.appendRunnerTaskEvents(taskID, runnerID, sessionHash, attempt, events, now)
}

func (r *RunnersRepo) appendRunnerTaskEvents(taskID, runnerID, sessionHash string, attempt int, events []domain.RunnerTaskEvent, now time.Time) (int, error) {
	accepted := 0
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		if _, err := r.takeRunnerTaskForMutation(tx, taskID, runnerID, sessionHash, attempt, now); err != nil {
			return err
		}
		for _, ev := range events {
			if ev.Seq < 1 {
				return fmt.Errorf("runner task event sequence must be positive")
			}
			if ev.Attempt != 0 && ev.Attempt != attempt {
				return fmt.Errorf("runner task event attempt does not match lease")
			}
		}
		for _, ev := range events {
			inserted, err := insertIgnore(tx, "runner_task_events", map[string]any{
				"task_id": taskID, "attempt": attempt, "seq": ev.Seq, "type": ev.Type, "data": ev.Data, "ts": timePtr(ev.Ts),
			})
			if err != nil {
				return err
			}
			if inserted {
				accepted++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return accepted, err
}

// RunnerTaskEvents is the management history reader, including legacy attempt
// zero records whose generation is unknown. Engine playback must use the
// attempt-scoped RunnerTaskEventsAfter reader instead.
func (r *RunnersRepo) RunnerTaskEvents(taskID string) []domain.RunnerTaskEvent {
	return runnerTaskEventsFromQuery(r.DB.Where("task_id = ?", taskID).Order("attempt").Order("seq"))
}

// RunnerTaskEventsAfter returns only events newer than the last sequence the
// worker has bridged. This keeps live observability polling proportional to
// new output instead of repeatedly loading an entire CLI transcript.
func (r *RunnersRepo) RunnerTaskEventsAfter(taskID string, attempt int, after int64) []domain.RunnerTaskEvent {
	if attempt < 1 {
		return []domain.RunnerTaskEvent{}
	}
	return runnerTaskEventsFromQuery(r.DB.Where("task_id = ? AND attempt = ? AND seq > ?", taskID, attempt, after).Order("seq"))
}

func runnerTaskEventsFromQuery(query *gorm.DB) []domain.RunnerTaskEvent {
	var rows []runnerTaskEventRow
	if err := query.Find(&rows).Error; err != nil {
		return []domain.RunnerTaskEvent{}
	}
	out := make([]domain.RunnerTaskEvent, 0, len(rows))
	for _, row := range rows {
		ev := domain.RunnerTaskEvent{Attempt: row.Attempt, Seq: row.Seq, Type: row.Type, Data: row.Data}
		if row.Ts != nil {
			ev.Ts = *row.Ts
		}
		out = append(out, ev)
	}
	return out
}

// DeregisterRunner removes a runner and everything that points at it: the
// machine record, default-execution-machine selections that target it, and
// its queued tasks (running tasks recover through lease expiry). Called when
// the daemon uninstalls or stops gracefully.
func (r *RunnersRepo) DeregisterRunner(id string, now time.Time) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("runner id is required")
	}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var rows []runnerTaskRow
		if err := tx.Where("runner_id = ? AND status = ?", id, domain.RunnerTaskQueued).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			task := runnerTaskFromRow(row)
			task.Status = domain.RunnerTaskCanceled
			task.LeaseOwner = ""
			task.LeaseUntil = nil
			scrubRunnerTaskCredentials(&task)
			completed := now
			task.CompletedAt = &completed
			task.UpdatedAt = now
			if err := tx.Model(&runnerTaskRow{}).Where("id = ?", task.ID).Updates(runnerTaskRowValuesPreservingRegistration(tx, task)).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("default_runner_id = ?", id).Delete(&userRunnerPrefRow{}).Error; err != nil {
			return err
		}
		res := tx.Where("id = ?", id).Delete(&runnerRow{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrRunnerNotFound
		}
		return nil
	})
}

// ---------- default execution machine (执行机器) ----------

// DefaultRunner returns the user's default execution machine, if any. An
// empty result means the cloud mode (control-plane sandbox + platform API
// keys).
func (r *RunnersRepo) DefaultRunner(userID int64) string {
	if userID <= 0 {
		return ""
	}
	var row userRunnerPrefRow
	if err := r.DB.Where("user_id = ?", userID).Take(&row).Error; err != nil {
		return ""
	}
	return strings.TrimSpace(row.DefaultRunnerID)
}

// SetDefaultRunner selects (or clears, with an empty id) the user's default
// execution machine.
func (r *RunnersRepo) SetDefaultRunner(userID int64, runnerID string, now time.Time) error {
	if userID <= 0 {
		return fmt.Errorf("user id is required")
	}
	runnerID = strings.TrimSpace(runnerID)
	if runnerID != "" {
		if _, ok := r.Runner(runnerID); !ok {
			return ErrRunnerNotFound
		}
	}
	if runnerID == "" {
		return r.DB.Where("user_id = ?", userID).Delete(&userRunnerPrefRow{}).Error
	}
	return upsert(r.DB, "user_runner_prefs", map[string]any{
		"user_id": userID, "default_runner_id": runnerID, "updated_at": timePtr(now),
	})
}

// ---------- helpers ----------

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// sortRunnerTasksByCreated is used by tests that need deterministic order.
func sortRunnerTasksByCreated(tasks []domain.RunnerTask) {
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.Before(tasks[j].CreatedAt) })
}

// ActiveRunnerTasksForRun lists the queued/leased/running tasks of a Run.
func (r *RunnersRepo) ActiveRunnerTasksForRun(runID string) ([]domain.RunnerTask, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, nil
	}
	var rows []runnerTaskRow
	if err := r.DB.Where("status IN ?", []string{domain.RunnerTaskQueued, domain.RunnerTaskLeased, domain.RunnerTaskRunning}).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.RunnerTask, 0, len(rows))
	for _, row := range rows {
		task := runnerTaskFromRow(row)
		if task.RunID == runID {
			out = append(out, task)
		}
	}
	return out, nil
}

// CancelRunnerTasksForRun interrupts all active remote work for a Run. Queued
// tasks become terminal immediately; leased/running tasks receive the durable
// desired_state=cancel signal consumed by the runner control channel.
func (r *RunnersRepo) CancelRunnerTasksForRun(runID string, now time.Time) ([]string, error) {
	tasks, err := r.ActiveRunnerTasksForRun(runID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if err := r.CancelRunnerTask(task.ID, now); err != nil {
			return ids, err
		}
		ids = append(ids, task.ID)
	}
	return ids, nil
}
