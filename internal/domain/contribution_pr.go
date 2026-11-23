package domain

// ContributionPRReceipt is a verified open, unmerged PR for an immutable head.
// It contains no merge authorization or business completion signal.
type ContributionPRReceipt struct {
	URL     string `json:"url"`
	Number  int    `json:"number"`
	HeadSHA string `json:"headSHA"`
	Draft   bool   `json:"draft,omitempty"`
}
