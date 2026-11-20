package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// RunnerContributionContextSHA identifies the frozen execution input, before
// claim/lease fields exist. This P0 assignment manifest also includes the
// compact approved-context provenance binding when present. It remains distinct
// from the TeamRelease/Memory content version. Credential values and the hash itself
// do not enter the digest. Task claim attempts are bound separately by the
// lease; placed Node attempts are frozen in the version-2 manifest. Hashing is
// not proof that a placement or source receipt is currently authorized.
func RunnerContributionContextSHA(task RunnerTask) (string, error) {
	public := PublicRunnerTask(task)
	if public.Invocation != nil && public.Invocation.WorkspaceIsolation != nil {
		isolation := *public.Invocation.WorkspaceIsolation
		isolation.ContextSHA256 = ""
		public.Invocation.WorkspaceIsolation = &isolation
	}
	version := 1
	var runnerID string
	if public.ExecutionPlacement != nil || public.Execution != nil || public.MaintenanceKey != nil || (public.Invocation != nil && len(public.Invocation.ContributionInputs) > 0) {
		version = 2
		runnerID = public.RunnerID
		if public.ExecutionPlacement != nil {
			canonical, err := CanonicalNodeExecutionPlacement(*public.ExecutionPlacement)
			if err != nil {
				return "", err
			}
			public.ExecutionPlacement = &canonical
		}
	}
	manifest := struct {
		Version            int                     `json:"version"`
		TaskID             string                  `json:"taskId"`
		RunID              string                  `json:"runId"`
		NodeID             string                  `json:"nodeId"`
		DeliveryPolicy     string                  `json:"deliveryPolicy"`
		Provider           string                  `json:"provider"`
		Prompt             string                  `json:"prompt"`
		Args               []string                `json:"args"`
		Command            string                  `json:"command"`
		Invocation         *RunnerInvocation       `json:"invocation"`
		Sandbox            *RunnerSandboxSpec      `json:"sandbox"`
		ExecutionDeadline  *time.Time              `json:"executionDeadline,omitempty"`
		RunnerID           string                  `json:"runnerId,omitempty"`
		ExecutionPlacement *NodeExecutionPlacement `json:"executionPlacement,omitempty"`
		Execution          *NodeTaskExecution      `json:"execution,omitempty"`
		MaintenanceKey     *RunnerMaintenanceKey   `json:"maintenanceKey,omitempty"`
	}{version, public.ID, public.RunID, public.NodeID, public.DeliveryPolicy, public.Provider, public.Prompt, public.Args, public.Command, public.Invocation, public.Sandbox, public.ExecutionDeadline, runnerID, public.ExecutionPlacement, public.Execution, public.MaintenanceKey}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
