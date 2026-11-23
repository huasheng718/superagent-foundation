package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agentflow/internal/domain"
)

// RecordContributionGateFromRun consumes existing Engine/Runner evidence. It
// never accepts a caller-provided status, SHA, review verdict or report hash.
// Verification uses ordinary isolated SOP nodes, not a second execution loop.
func (e *Engine) RecordContributionGateFromRun(integrationID, gateRunID, nodeID, kind, artifactID string) (domain.ContributionPublication, error) {
	value, ok := e.repo.ContributionIntegration(integrationID)
	if !ok {
		return domain.ContributionPublication{}, errors.New("integration not found")
	}
	if err := e.ValidateContributionIntegration(value); err != nil {
		return domain.ContributionPublication{}, err
	}
	repository, ok := e.repo.Repository(value.RepositoryID)
	if !ok || repository.DefaultBranch == "" {
		return domain.ContributionPublication{}, errors.New("publication default target unavailable")
	}
	gate, err := e.contributionGateFromRun(value, gateRunID, nodeID, kind, artifactID)
	if err != nil {
		return domain.ContributionPublication{}, err
	}
	publication, err := e.repo.PrepareContributionPublication(value.ID, repository.DefaultBranch, time.Now().UTC())
	if err != nil {
		return domain.ContributionPublication{}, err
	}
	for _, previous := range publication.Gates {
		if previous.Kind != gate.Kind && previous.GateTaskID == gate.GateTaskID {
			return domain.ContributionPublication{}, errors.New("contribution gates require distinct execution tasks")
		}
	}
	return e.repo.RecordContributionGate(value.ID, gate, time.Now().UTC())
}

func (e *Engine) contributionGateFromRun(value domain.ContributionIntegration, gateRunID, nodeID, kind, artifactID string) (domain.ContributionGateEvidence, error) {
	var evidence domain.ContributionGateEvidence
	if kind != "fast" && kind != "slow" && kind != "review" {
		return evidence, errors.New("unsupported contribution gate kind")
	}
	controller, ok := e.repo.RunState(value.RunID)
	if !ok {
		return evidence, errors.New("controller unavailable")
	}
	run, ok := e.repo.RunState(gateRunID)
	if !ok || run.ID == value.RunID || run.Status != "succeeded" || run.CancelRequested || !e.controlledRun(run) || run.ExecutionContext.ProjectID != controller.ExecutionContext.ProjectID || run.ExecutionContext.RunnerWorkspaceLayout != domain.RunnerWorkspaceLayoutAttempt || run.ExecutionContext.WorkspaceBaseSHAs[value.RepositoryID] != value.ResultSHA {
		return evidence, errors.New("gate requires an independent successful isolated run pinned to the exact integration SHA")
	}
	// This deliberately reuses the accepted contribution provenance checks.
	// For a validation Run the base must equal its result: tests/review may
	// create excluded reports, but changing product code invalidates the gate.
	task, err := e.repo.LatestRunnerExecutionTaskForNode(run.ID, nodeID)
	if err != nil {
		return evidence, err
	}
	inputs, err := e.acceptedIntegrationInputs(run, value.RepositoryID, []string{task.ID})
	if err != nil || len(inputs) != 1 || inputs[0].ResultSHA != value.ResultSHA || inputs[0].BaseSHA != value.ResultSHA {
		return evidence, errors.New("gate task did not accept an unchanged exact-SHA checkout")
	}
	if len(task.ContributionSpecs) != 1 {
		return evidence, errors.New("gate must validate exactly one repository")
	}
	node := run.Nodes[nodeID]
	var definition domain.WorkflowNode
	for _, n := range run.WorkflowSnapshot.Nodes {
		if n.ID == nodeID {
			definition = n
		}
	}
	if node == nil || definition.ID == "" || node.Status != "succeeded" || node.Attempt <= 0 {
		return evidence, errors.New("gate node outcome unavailable")
	}
	if (kind == "review" && definition.Type != "agent") || (kind != "review" && definition.Type != "script") {
		return evidence, errors.New("gate kind does not match frozen workflow node")
	}
	if kind != "review" && (task.Provider != "terminal" || strings.TrimSpace(task.Command) == "") {
		return evidence, errors.New("test gate requires an accepted terminal script execution")
	}
	artifact, ok := e.repo.Artifact(run.ID, artifactID)
	sum := sha256.Sum256([]byte(artifact.Content))
	if !ok || artifact.NodeID != nodeID || artifact.Attempt != node.Attempt || artifact.Content == "" || artifact.Size != int64(len(artifact.Content)) || artifact.SHA256 != hex.EncodeToString(sum[:]) {
		return evidence, errors.New("gate report artifact is missing, corrupt or from another attempt")
	}
	reviewer := ""
	if kind == "review" {
		if definition.AgentID == "" || task.Invocation.Agent.ID != definition.AgentID || task.Provider == "terminal" || task.Provider == "mock" || node.SessionID == "" || node.GateResult == nil || node.GateResult.Decision != "pass" {
			return evidence, errors.New("independent model review did not return an authenticated passing gate result")
		}
		for _, finding := range node.GateResult.Findings {
			severity := strings.ToLower(strings.TrimSpace(finding.Severity))
			if severity == "critical" || severity == "high" {
				return evidence, errors.New("review has blocking findings")
			}
		}
		for _, input := range value.Inputs {
			sourceTask, ok := e.repo.RunnerTask(input.TaskID)
			if !ok || sourceTask.Invocation == nil || sourceTask.Invocation.Agent.ID == definition.AgentID || sourceTask.RunID == run.ID {
				return evidence, errors.New("review is not independent of contributing execution")
			}
		}
		reviewer = definition.AgentID
	}
	proof := struct {
		Integration domain.ContributionIntegration `json:"integration"`
		Kind        string                         `json:"kind"`
		Task        domain.RunnerTask              `json:"task"`
		Node        *domain.NodeRun                `json:"node"`
		Definition  domain.WorkflowNode            `json:"definition"`
		Artifact    domain.Artifact                `json:"artifact"`
	}{value, kind, domain.PublicRunnerTask(task), node, definition, artifact}
	raw, err := json.Marshal(proof)
	if err != nil {
		return evidence, err
	}
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])
	evidence = domain.ContributionGateEvidence{ID: hash, Kind: kind, IntegrationID: value.ID, ResultSHA: value.ResultSHA, ContextSHA256: value.ContextSHA256, ManifestSHA256: value.ManifestSHA256, Status: "passed", EvidenceRef: "run:" + run.ID + "/node:" + nodeID + "/artifact:" + artifact.ID, EvidenceSHA256: hash, ReviewerID: reviewer, GateRunID: run.ID, GateNodeID: nodeID, GateTaskID: task.ID, GateTaskAttempt: task.Attempt, ArtifactID: artifact.ID}
	return evidence, nil
}

func (e *Engine) validateContributionPublication(value domain.ContributionIntegration, publication domain.ContributionPublication) error {
	if err := e.ValidateContributionIntegration(value); err != nil {
		return err
	}
	repository, ok := e.repo.Repository(value.RepositoryID)
	if !ok || publication.ID != value.ID || publication.RunID != value.RunID || publication.RepositoryID != value.RepositoryID || publication.Branch != "superagent/integration/"+value.ID || publication.TargetBranch != repository.DefaultBranch || publication.ResultSHA != value.ResultSHA || publication.ContextSHA256 != value.ContextSHA256 || publication.ManifestSHA256 != value.ManifestSHA256 || len(publication.Gates) != 3 {
		return errors.New("publication no longer matches the catalog target or immutable integration")
	}
	seen := map[string]bool{}
	tasks := map[string]bool{}
	for _, gate := range publication.Gates {
		current, err := e.contributionGateFromRun(value, gate.GateRunID, gate.GateNodeID, gate.Kind, gate.ArtifactID)
		if err != nil {
			return err
		}
		// CreatedAt belongs to the publication transaction, while all remaining
		// fields must exactly equal freshly derived controller evidence.
		current.CreatedAt = gate.CreatedAt
		if seen[gate.Kind] || tasks[current.GateTaskID] || current != gate {
			return errors.New("publication gate evidence changed or was superseded")
		}
		seen[gate.Kind] = true
		tasks[current.GateTaskID] = true
	}
	if !seen["fast"] || !seen["slow"] || !seen["review"] {
		return errors.New("publication gates incomplete")
	}
	return nil
}
