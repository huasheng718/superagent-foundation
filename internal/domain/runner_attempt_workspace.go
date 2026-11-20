package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
)

const (
	RunnerWorkspaceLayoutAttempt    = "attempt"
	RunnerWorkspaceIsolationVersion = 1
	RunnerVersionWorkspaceIsolation = "0.10.0"
)

type RunnerWorkspaceIsolation struct {
	Version       int    `json:"version"`
	ContextSHA256 string `json:"contextSHA256"`
}

// RunnerWorkspaceDescriptor is derived only from a frozen assignment and its
// authoritative claimed attempt. It is also the identity future result exports
// must retain; a physical path alone is not an ownership token.
type RunnerWorkspaceDescriptor struct {
	Version        int    `json:"version"`
	RunID          string `json:"runId"`
	NodeID         string `json:"nodeId"`
	TaskID         string `json:"taskId"`
	Attempt        int    `json:"attempt"`
	IdentitySHA256 string `json:"identitySHA256"`
	ContextSHA256  string `json:"contextSHA256"`
	WorkspaceRel   string `json:"workspaceRel"`
	Branch         string `json:"branch"`
}

func (r Runner) SupportsWorkspaceIsolation() bool {
	return versionAtLeast(r.RunnerVersion, RunnerVersionWorkspaceIsolation)
}

func ValidWorkspaceBaseSHA(value string) bool {
	// This initial implementation deliberately supports SHA-1 repositories
	// only. Do not accept abbreviations, symbolic refs or unsupported SHA-256.
	if len(value) != 40 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ValidWorkspaceContextSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func workspaceIdentityHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func IsolatedRunWorkspaceRel(runID string) string {
	return path.Join("runs", workspaceIdentityHash(runID))
}

func IsolatedWorkspaceDescriptor(task RunnerTask) (RunnerWorkspaceDescriptor, error) {
	inv := task.Invocation
	if inv == nil || inv.WorkspaceIsolation == nil || inv.WorkspaceIsolation.Version != RunnerWorkspaceIsolationVersion {
		return RunnerWorkspaceDescriptor{}, fmt.Errorf("unsupported or missing workspace isolation version")
	}
	if !ValidWorkspaceContextSHA256(inv.WorkspaceIsolation.ContextSHA256) {
		return RunnerWorkspaceDescriptor{}, fmt.Errorf("isolated workspace requires a frozen SHA-256 context")
	}
	if task.Action != "" || strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.RunID) == "" || strings.TrimSpace(task.NodeID) == "" || task.Attempt <= 0 || inv.RunID != task.RunID || inv.NodeID != task.NodeID {
		return RunnerWorkspaceDescriptor{}, fmt.Errorf("isolated workspace requires matching run/node/task and a positive claimed attempt")
	}
	d := RunnerWorkspaceDescriptor{Version: inv.WorkspaceIsolation.Version, RunID: task.RunID, NodeID: task.NodeID, TaskID: task.ID, Attempt: task.Attempt, ContextSHA256: inv.WorkspaceIsolation.ContextSHA256}
	raw, _ := json.Marshal(d)
	d.IdentitySHA256 = workspaceIdentityHash(string(raw))
	d.WorkspaceRel = path.Join(IsolatedRunWorkspaceRel(task.RunID), "nodes", workspaceIdentityHash(task.NodeID), "tasks", workspaceIdentityHash(task.ID), "attempts", strconv.Itoa(task.Attempt), "workspace")
	d.Branch = "agentflow/attempt/" + d.IdentitySHA256
	return d, nil
}
