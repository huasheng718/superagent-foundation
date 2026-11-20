package domain

import "time"

const (
	RunnerContributionMaxBytes         int64 = 16 << 20
	RunnerContributionMaxExpandedBytes int64 = 64 << 20
	RunnerContributionMaxObjects             = 100000
	RunnerContributionRef                    = "refs/heads/agentflow-contribution"
)

// RunnerContributionSpec is frozen by the Engine before dispatch. It grants
// bundle export/upload only, never remote Git or hosting write authority.
type RunnerContributionSpec struct {
	RepositoryID  string `json:"repositoryId"`
	Dir           string `json:"dir"`
	BaseSHA       string `json:"baseSHA"`
	ContextSHA256 string `json:"contextSHA256"`
	MaxBytes      int64  `json:"maxBytes"`
}

// RunnerContributionReceipt is generated and persisted by the control plane.
// Pending receipt metadata alone is not accepted execution output. Only the
// successful task-completion transaction can populate AcceptedAt.
type RunnerContributionReceipt struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"taskId"`
	Attempt       int       `json:"attempt"`
	RepositoryID  string    `json:"repositoryId"`
	Dir           string    `json:"dir"`
	BaseSHA       string    `json:"baseSHA"`
	ResultSHA     string    `json:"resultSHA"`
	ContextSHA256 string    `json:"contextSHA256"`
	SHA256        string    `json:"sha256"`
	Size          int64     `json:"size"`
	StorageKey    string    `json:"-"`
	CreatedAt     time.Time `json:"createdAt"`
	AcceptedAt    time.Time `json:"acceptedAt,omitempty"`
}
