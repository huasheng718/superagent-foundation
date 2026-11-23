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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

// DownloadContributionInput fetches one task-scoped immutable input bundle.
// The expected metadata comes from the claimed task payload; response headers
// and bytes must match it exactly before a temporary file is returned.
func (c *Client) DownloadContributionInput(ctx context.Context, taskID string, attempt int, repositoryID string, expected domain.RunnerContributionInput) (string, error) {
	if taskID == "" || attempt <= 0 || repositoryID == "" || expected.RepositoryID != repositoryID || expected.BundleSize <= 0 || expected.BundleSize > domain.RunnerContributionMaxBytes || expected.BundleSHA256 == "" || expected.BaseSHA == "" || expected.HeadSHA == "" {
		return "", errors.New("invalid contribution input identity or size")
	}
	path := fmt.Sprintf("/runners/tasks/%s/contribution-inputs/%s?attempt=%d", url.PathEscape(taskID), url.PathEscape(repositoryID), attempt)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	if session := c.sessionToken(); session != "" {
		req.Header.Set("Authorization", "Bearer "+session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", ErrUnauthorized
	}
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, expected.BundleSize+1))
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode == http.StatusConflict {
		var detail struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &detail) == nil && detail.Error == ErrTaskLeaseLost.Error() {
			return "", ErrTaskLeaseLost
		}
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("contribution input not found")
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("contribution input rejected (%d)", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/x-git-bundle" || resp.Header.Get("X-Contribution-SHA256") != expected.BundleSHA256 || resp.Header.Get("X-Contribution-Base-SHA") != expected.BaseSHA || resp.Header.Get("X-Contribution-Result-SHA") != expected.HeadSHA || int64(len(payload)) != expected.BundleSize || (resp.ContentLength >= 0 && resp.ContentLength != expected.BundleSize) {
		return "", errors.New("contribution input metadata mismatch")
	}
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != expected.BundleSHA256 {
		return "", errors.New("contribution input hash mismatch")
	}
	file, err := os.CreateTemp("", "agentflow-contribution-input-*.bundle")
	if err != nil {
		return "", err
	}
	name := file.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(payload)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// UploadContribution transfers only the standalone Git bundle. It cannot
// choose a controller path or make a staged receipt accepted task output.
func (c *Client) UploadContribution(ctx context.Context, taskID string, attempt int, spec domain.RunnerContributionSpec, bundle gitwork.ContributionBundle) (domain.RunnerContributionReceipt, error) {
	var receipt domain.RunnerContributionReceipt
	if attempt <= 0 || taskID == "" || spec.RepositoryID == "" || bundle.Size <= 0 || bundle.Size > domain.RunnerContributionMaxBytes || (spec.MaxBytes > 0 && bundle.Size > spec.MaxBytes) {
		return receipt, errors.New("invalid contribution upload identity or size")
	}
	file, err := os.Open(bundle.Path)
	if err != nil {
		return receipt, err
	}
	defer file.Close()
	path := fmt.Sprintf("/runners/tasks/%s/contributions/%s/content?attempt=%d", url.PathEscape(taskID), url.PathEscape(spec.RepositoryID), attempt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path, io.LimitReader(file, bundle.Size))
	if err != nil {
		return receipt, err
	}
	req.ContentLength = bundle.Size
	req.Header.Set("Content-Type", "application/x-git-bundle")
	req.Header.Set("Authorization", "Bearer "+c.sessionToken())
	req.Header.Set("X-Contribution-Base-SHA", spec.BaseSHA)
	req.Header.Set("X-Contribution-Result-SHA", bundle.ResultSHA)
	req.Header.Set("X-Contribution-Context-SHA256", spec.ContextSHA256)
	resp, err := c.http.Do(req)
	if err != nil {
		return receipt, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return receipt, ErrUnauthorized
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return receipt, err
	}
	if resp.StatusCode != http.StatusOK {
		var detail struct {
			Error string `json:"error"`
		}
		if resp.StatusCode == http.StatusConflict && json.Unmarshal(payload, &detail) == nil && detail.Error == ErrTaskLeaseLost.Error() {
			return receipt, ErrTaskLeaseLost
		}
		return receipt, fmt.Errorf("contribution upload rejected (%d)", resp.StatusCode)
	}
	if err := json.Unmarshal(payload, &receipt); err != nil {
		return receipt, fmt.Errorf("decode contribution receipt: %w", err)
	}
	if receipt.ID == "" || receipt.TaskID != taskID || receipt.Attempt != attempt || receipt.RepositoryID != spec.RepositoryID || receipt.Dir != spec.Dir || receipt.BaseSHA != spec.BaseSHA || receipt.ResultSHA != bundle.ResultSHA || receipt.ContextSHA256 != spec.ContextSHA256 || receipt.SHA256 != bundle.SHA256 || receipt.Size != bundle.Size || !receipt.AcceptedAt.IsZero() {
		return domain.RunnerContributionReceipt{}, errors.New("contribution receipt identity mismatch")
	}
	return receipt, nil
}

func (d *Daemon) exportTaskContributions(ctx context.Context, task domain.RunnerTask, outputs *domain.RunnerTaskOutputs) (*domain.RunnerTaskOutputs, error) {
	if len(task.ContributionSpecs) == 0 {
		return outputs, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// Keep bundle files outside the checkout so export cannot commit its own
	// output or expose private temporary data to a later workspace upload.
	dir, err := os.MkdirTemp("", "agentflow-runner-contribution-")
	if err != nil {
		return outputs, err
	}
	defer os.RemoveAll(dir)
	if outputs == nil {
		outputs = &domain.RunnerTaskOutputs{}
	}
	outputs.ContributionReceipts = nil
	for i, spec := range task.ContributionSpecs {
		repository, err := contributionRepositoryPath(d.taskWorkspace(task), spec.Dir)
		if err != nil {
			return outputs, err
		}
		limit := spec.MaxBytes
		if limit <= 0 || limit > domain.RunnerContributionMaxBytes {
			limit = domain.RunnerContributionMaxBytes
		}
		bundle, err := gitwork.ExportContribution(ctx, repository, filepath.Join(dir, fmt.Sprintf("%d.bundle", i)), spec.BaseSHA, fmt.Sprintf("AgentFlow contribution %s attempt %d", task.ID, task.Attempt), limit)
		if err != nil {
			return outputs, fmt.Errorf("export repository %s: %w", spec.RepositoryID, err)
		}
		receipt, err := d.client.UploadContribution(ctx, task.ID, task.Attempt, spec, bundle)
		if err != nil {
			return outputs, fmt.Errorf("upload repository %s: %w", spec.RepositoryID, err)
		}
		outputs.ContributionReceipts = append(outputs.ContributionReceipts, receipt.ID)
	}
	return outputs, nil
}

func contributionRepositoryPath(workspace, dir string) (string, error) {
	if dir != "" && dir != "." && !domain.SafeWorkspaceRelPath(dir) {
		return "", errors.New("unsafe contribution repository directory")
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("contribution repository escapes workspace")
	}
	return target, nil
}
