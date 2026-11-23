package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	RunnerVersionNodePlacement    = "0.11.0"
	NodeExecutionPlacementVersion = 1
	NodePlacementAccessPersonal   = "personal"
	NodePlacementAccessCloudPool  = "cloud_pool"
	NodeTaskRoleAuthor            = "author"
	NodeTaskRoleVerification      = "verification"
)

var (
	ErrRunnerPlacementInvalid     = errors.New("invalid runner placement")
	ErrRunnerPlacementDenied      = errors.New("runner placement denied")
	ErrRunnerPlacementUnavailable = errors.New("runner placement unavailable")
	ErrRunnerPlacementChanged     = errors.New("runner placement changed")
)

type NodePlacementRequest struct {
	RunnerID             string   `json:"runnerId"`
	OS                   string   `json:"os,omitempty"`
	Arch                 string   `json:"arch,omitempty"`
	RequiredCapabilities []string `json:"requiredCapabilities,omitempty"`
}

// NodeExecutionPlacement contains identity and selected capability inventory,
// never credentials or volatile heartbeat/session-expiry timestamps. Structural
// validation does not prove authorization or current registry state.
type NodeExecutionPlacement struct {
	Version            int                   `json:"version"`
	NodeAttempt        int                   `json:"nodeAttempt"`
	RunnerID           string                `json:"runnerId"`
	RegistrationSHA256 string                `json:"registrationSHA256"`
	ResponsibleUserID  int64                 `json:"responsibleUserId"`
	ProjectID          string                `json:"projectId"`
	Access             string                `json:"access"`
	PoolLabel          string                `json:"poolLabel,omitempty"`
	OS                 string                `json:"os"`
	Arch               string                `json:"arch"`
	RunnerVersion      string                `json:"runnerVersion"`
	Runtimes           []NodeRuntimeSnapshot `json:"runtimes"`
	CapabilitySHA256   string                `json:"capabilitySHA256"`
}

type NodeRuntimeSnapshot struct {
	Provider     string   `json:"provider"`
	Version      string   `json:"version"`
	BinaryPath   string   `json:"binaryPath"`
	Capabilities []string `json:"capabilities"`
	AuthMode     string   `json:"authMode"`
}

type RunnerContributionInputSource struct {
	RunID         string `json:"runId"`
	NodeID        string `json:"nodeId"`
	TaskID        string `json:"taskId"`
	ReceiptID     string `json:"receiptId"`
	NodeAttempt   int    `json:"nodeAttempt"`
	TaskAttempt   int    `json:"taskAttempt"`
	ContextSHA256 string `json:"contextSHA256"`
	BundleSHA256  string `json:"bundleSHA256"`
	ResultSHA     string `json:"resultSHA"`
}

type RunnerContributionInput struct {
	RepositoryID string                          `json:"repositoryId"`
	BaseSHA      string                          `json:"baseSHA"`
	HeadSHA      string                          `json:"headSHA"`
	BundleSHA256 string                          `json:"bundleSHA256"`
	BundleSize   int64                           `json:"bundleSize"`
	Sources      []RunnerContributionInputSource `json:"sources"`
}

type NodeTaskExecution struct {
	Role                 string `json:"role"`
	NodeAttempt          int    `json:"nodeAttempt"`
	Sequence             int    `json:"sequence"`
	PreviousAuthorTaskID string `json:"previousAuthorTaskId,omitempty"`
}

type RunnerMaintenanceKey struct {
	RunID              string `json:"runId"`
	Action             string `json:"action"`
	RunnerID           string `json:"runnerId"`
	RegistrationSHA256 string `json:"registrationSHA256,omitempty"`
}

type RunnerWorkspaceParticipant struct {
	RunnerID           string                 `json:"runnerId"`
	RegistrationSHA256 string                 `json:"registrationSHA256"`
	Placement          NodeExecutionPlacement `json:"placement"`
}

const (
	RunnerWorkspaceCleanupPending = "pending"
	RunnerWorkspaceCleanupSuccess = "success"
	RunnerWorkspaceCleanupFailure = "failure"
)

// RunnerWorkspaceCleanupParticipant is the durable aggregate state for one
// frozen Runner registration that contributed work to a placed Run. The
// registration is part of the identity: a later daemon registration must not
// claim cleanup queued for the previous one.
type RunnerWorkspaceCleanupParticipant struct {
	RunID              string    `json:"runId"`
	Action             string    `json:"action"`
	RunnerID           string    `json:"runnerId"`
	RegistrationSHA256 string    `json:"registrationSHA256"`
	Status             string    `json:"status"`
	TaskID             string    `json:"taskId,omitempty"`
	Error              string    `json:"error,omitempty"`
	UpdatedAt          time.Time `json:"updatedAt"`
	CompletedAt        time.Time `json:"completedAt,omitempty"`
}

// RunnerRegistrationSHA256 purpose-separates the durable registry identity
// from its secret session-token hash. Do not substitute a token or raw hash in
// a placement snapshot. Ordinary heartbeat/expiry extensions do not enter it.
func RunnerRegistrationSHA256(sessionTokenHash string) (string, error) {
	if !ValidWorkspaceContextSHA256(sessionTokenHash) {
		return "", placementInvalid("registration requires a lowercase SHA-256 session hash")
	}
	return placementHash("superagent.runner-registration.v1\x00", []byte(sessionTokenHash)), nil
}

var placementIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
var placementSemver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// RunnerVersionSupportsNodePlacement accepts strict SemVer, including build
// metadata, rather than the legacy permissive numeric-prefix version parser.
// A prerelease of the minimum release has not yet reached the feature gate.
func RunnerVersionSupportsNodePlacement(version string) bool {
	return runnerVersionSupports(version, []string{"0", "11", "0"})
}

func runnerVersionSupports(version string, minimumParts []string) bool {
	matches := placementSemver.FindStringSubmatch(version)
	if matches == nil {
		return false
	}
	if matches[4] != "" {
		for _, id := range strings.Split(matches[4], ".") {
			if len(id) > 1 && id[0] == '0' && strings.Trim(id, "0123456789") == "" {
				return false
			}
		}
	}
	for i, minimum := range minimumParts {
		part := matches[i+1]
		if len(part) != len(minimum) {
			return len(part) > len(minimum)
		}
		if part != minimum {
			return part > minimum
		}
	}
	return matches[4] == ""
}

// CanonicalNodePlacementRequest validates structure only. The Engine owns
// complete workflow-map validation and live machine/project authorization.
func CanonicalNodePlacementRequest(request NodePlacementRequest) (NodePlacementRequest, error) {
	if !placementText(request.RunnerID) || (request.OS != "" && !placementIdentifier.MatchString(request.OS)) || (request.Arch != "" && !placementIdentifier.MatchString(request.Arch)) {
		return NodePlacementRequest{}, placementInvalid("invalid request target or platform")
	}
	caps, err := canonicalPlacementCapabilities(request.RequiredCapabilities)
	if err != nil {
		return NodePlacementRequest{}, err
	}
	request.RequiredCapabilities = caps
	return request, nil
}

// CanonicalNodeExecutionPlacement sorts selected runtime inventory and
// capabilities without mutating the caller. Duplicate inventory entries are
// rejected; registry inventory deduplication must happen before freezing.
// An empty capability digest is derived, while a supplied digest must match.
func CanonicalNodeExecutionPlacement(p NodeExecutionPlacement) (NodeExecutionPlacement, error) {
	if p.Version != NodeExecutionPlacementVersion || p.NodeAttempt < 1 || p.ResponsibleUserID < 1 || !placementText(p.RunnerID) || !placementText(p.ProjectID) || !ValidWorkspaceContextSHA256(p.RegistrationSHA256) {
		return NodeExecutionPlacement{}, placementInvalid("invalid snapshot identity or version")
	}
	if !placementIdentifier.MatchString(p.OS) || !placementIdentifier.MatchString(p.Arch) || !RunnerVersionSupportsNodePlacement(p.RunnerVersion) {
		return NodeExecutionPlacement{}, placementInvalid("invalid platform or unsupported runner version")
	}
	switch p.Access {
	case NodePlacementAccessPersonal:
		if p.PoolLabel != "" {
			return NodeExecutionPlacement{}, placementInvalid("personal placement cannot carry pool authority")
		}
	case NodePlacementAccessCloudPool:
		if !placementText(p.PoolLabel) {
			return NodeExecutionPlacement{}, placementInvalid("shared placement requires a pool label")
		}
	default:
		return NodeExecutionPlacement{}, placementInvalid("unsupported access mode")
	}
	if len(p.Runtimes) == 0 {
		return NodeExecutionPlacement{}, placementInvalid("snapshot requires selected runtimes")
	}
	p = cloneNodeExecutionPlacement(p)
	providers := make(map[string]bool, len(p.Runtimes))
	for i, runtime := range p.Runtimes {
		if !placementIdentifier.MatchString(runtime.Provider) || providers[runtime.Provider] || !placementText(runtime.BinaryPath) || (runtime.Version == "" && runtime.Provider != "terminal") || (runtime.Version != "" && !placementText(runtime.Version)) {
			return NodeExecutionPlacement{}, placementInvalid("invalid or duplicate selected runtime")
		}
		providers[runtime.Provider] = true
		if runtime.AuthMode != RuntimeAuthLocal && runtime.AuthMode != RuntimeAuthPersonal {
			return NodeExecutionPlacement{}, placementInvalid("snapshot requires effective runtime auth mode")
		}
		configured := Runner{Runtimes: []RunnerRuntime{{Provider: runtime.Provider, AuthMode: runtime.AuthMode}}}
		if configured.RuntimeAuthMode(runtime.Provider) != runtime.AuthMode {
			return NodeExecutionPlacement{}, placementInvalid("runtime auth mode differs from effective provider policy")
		}
		caps, err := canonicalPlacementCapabilities(runtime.Capabilities)
		if err != nil {
			return NodeExecutionPlacement{}, err
		}
		p.Runtimes[i].Capabilities = caps
	}
	sort.Slice(p.Runtimes, func(i, j int) bool { return p.Runtimes[i].Provider < p.Runtimes[j].Provider })
	digest, err := nodePlacementCapabilitySHA256(p)
	if err != nil {
		return NodeExecutionPlacement{}, err
	}
	if p.CapabilitySHA256 != "" && p.CapabilitySHA256 != digest {
		return NodeExecutionPlacement{}, placementInvalid("capability digest differs from canonical snapshot")
	}
	p.CapabilitySHA256 = digest
	return p, nil
}

func NodeExecutionPlacementCapabilitySHA256(p NodeExecutionPlacement) (string, error) {
	canonical, err := CanonicalNodeExecutionPlacement(p)
	return canonical.CapabilitySHA256, err
}

func nodePlacementCapabilitySHA256(p NodeExecutionPlacement) (string, error) {
	// Explicit ordered manifest: do not hash NodeAttempt, the digest itself,
	// heartbeat times or credentials when identifying capability inventory.
	manifest := struct {
		RunnerID           string                `json:"runnerId"`
		RegistrationSHA256 string                `json:"registrationSHA256"`
		ResponsibleUserID  int64                 `json:"responsibleUserId"`
		ProjectID          string                `json:"projectId"`
		Access             string                `json:"access"`
		PoolLabel          string                `json:"poolLabel,omitempty"`
		OS                 string                `json:"os"`
		Arch               string                `json:"arch"`
		RunnerVersion      string                `json:"runnerVersion"`
		Runtimes           []NodeRuntimeSnapshot `json:"runtimes"`
	}{p.RunnerID, p.RegistrationSHA256, p.ResponsibleUserID, p.ProjectID, p.Access, p.PoolLabel, p.OS, p.Arch, p.RunnerVersion, p.Runtimes}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return placementHash("superagent.node-placement-capabilities.v1\x00", raw), nil
}

func canonicalPlacementCapabilities(values []string) ([]string, error) {
	canonical := append([]string{}, values...)
	sort.Strings(canonical)
	for i, value := range canonical {
		if !placementText(value) || (i > 0 && canonical[i-1] == value) {
			return nil, placementInvalid("empty, malformed or duplicate capability")
		}
	}
	return canonical, nil
}

func placementText(value string) bool {
	return value != "" && utf8.ValidString(value) && value == strings.TrimSpace(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func placementInvalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrRunnerPlacementInvalid, reason)
}

func placementHash(purpose string, raw []byte) string {
	h := sha256.New()
	h.Write([]byte(purpose))
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

func cloneNodeExecutionPlacement(p NodeExecutionPlacement) NodeExecutionPlacement {
	if p.Runtimes != nil {
		p.Runtimes = append([]NodeRuntimeSnapshot{}, p.Runtimes...)
		for i := range p.Runtimes {
			if p.Runtimes[i].Capabilities != nil {
				p.Runtimes[i].Capabilities = append([]string{}, p.Runtimes[i].Capabilities...)
			}
		}
	}
	return p
}
