package domain

// ContributionIntegration is a controller-created fixed-SHA snapshot. Prepared
// is not PR-ready or business acceptance. Worker payloads cannot create it.
type ContributionIntegration struct {
	ID             string                      `json:"id"`
	RunID          string                      `json:"runId"`
	RepositoryID   string                      `json:"repositoryId"`
	BaseSHA        string                      `json:"baseSHA"`
	ResultSHA      string                      `json:"resultSHA"`
	ContextSHA256  string                      `json:"contextSHA256"`
	ManifestSHA256 string                      `json:"manifestSHA256"`
	BundleSHA256   string                      `json:"bundleSHA256"`
	BundleSize     int64                       `json:"bundleSize"`
	Inputs         []RunnerContributionReceipt `json:"inputs"`
	State          string                      `json:"state"`
}
