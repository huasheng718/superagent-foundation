package repo

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// ErrRunnerPlacementSessionInvalid is distinct from task-attempt conflicts so
// task-plane handlers can return 401 without inspecting provider/error text.
var ErrRunnerPlacementSessionInvalid = errors.New("invalid runner placement session")

// ValidateRunnerPlacement checks a frozen snapshot against the authoritative
// registry. The Engine separately owns project pool policy and Run/Node access.
// This standalone predispatch check is not atomic task-claim authorization.
func (r *RunnersRepo) ValidateRunnerPlacement(snapshot domain.NodeExecutionPlacement, now time.Time) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		return r.validateRunnerPlacementTx(tx, snapshot, "", now)
	})
}

// validateRunnerPlacementTx locks only the registry row in its caller's
// transaction. Task callers must first lock the owning Run and involved tasks,
// then pass the authenticated session hash; they also own lineage/lease checks.
// Empty sessionHash is reserved for standalone predispatch validation.
func (r *RunnersRepo) validateRunnerPlacementTx(tx *gorm.DB, snapshot domain.NodeExecutionPlacement, sessionHash string, now time.Time) error {
	if !domain.ValidWorkspaceContextSHA256(snapshot.CapabilitySHA256) {
		return fmt.Errorf("%w: frozen capability digest is required", domain.ErrRunnerPlacementInvalid)
	}
	frozen, err := domain.CanonicalNodeExecutionPlacement(snapshot)
	if err != nil {
		return err
	}
	var row runnerRow
	if err := r.locking(tx).Where("id = ?", frozen.RunnerID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("%w: registered runner is missing", domain.ErrRunnerPlacementUnavailable)
		}
		return err
	}
	current := runnerFromRow(row)
	checkedAt := leaseCheckTime(tx, now)
	if !domain.ValidWorkspaceContextSHA256(current.SessionTokenHash) || current.SessionExpiresAt == nil || !current.SessionExpiresAt.After(checkedAt) {
		return ErrRunnerPlacementSessionInvalid
	}
	if sessionHash != "" && (!domain.ValidWorkspaceContextSHA256(sessionHash) || subtle.ConstantTimeCompare([]byte(current.SessionTokenHash), []byte(sessionHash)) != 1) {
		return ErrRunnerPlacementSessionInvalid
	}
	registration, err := domain.RunnerRegistrationSHA256(current.SessionTokenHash)
	if err != nil {
		return ErrRunnerPlacementSessionInvalid
	}
	if registration != frozen.RegistrationSHA256 {
		return fmt.Errorf("%w: registered runner identity differs", domain.ErrRunnerPlacementChanged)
	}
	if current.Status != domain.RunnerStatusOnline {
		return fmt.Errorf("%w: registered runner is offline", domain.ErrRunnerPlacementUnavailable)
	}
	switch frozen.Access {
	case domain.NodePlacementAccessPersonal:
		if current.OwnerUserID <= 0 || current.OwnerUserID != frozen.ResponsibleUserID {
			return fmt.Errorf("%w: registered runner ownership differs", domain.ErrRunnerPlacementChanged)
		}
	case domain.NodePlacementAccessCloudPool:
		if current.OwnerUserID != 0 || !runnerPlacementHasPoolLabel(current.Labels, frozen.PoolLabel) {
			return fmt.Errorf("%w: registered runner pool identity differs", domain.ErrRunnerPlacementChanged)
		}
	}

	// Derive the actual selected inventory, retaining the frozen authorization
	// context. Volatile heartbeat/expiry timestamps and unselected runtimes do
	// not invalidate an otherwise unchanged placement.
	actual := frozen
	actual.OS, actual.Arch, actual.RunnerVersion = current.OS, current.Arch, current.RunnerVersion
	actual.RegistrationSHA256, actual.CapabilitySHA256 = registration, ""
	actual.Runtimes = make([]domain.NodeRuntimeSnapshot, 0, len(frozen.Runtimes))
	for _, selected := range frozen.Runtimes {
		var found *domain.RunnerRuntime
		for i := range current.Runtimes {
			if !strings.EqualFold(current.Runtimes[i].Provider, selected.Provider) {
				continue
			}
			if found != nil {
				return fmt.Errorf("%w: duplicate registered runtime", domain.ErrRunnerPlacementChanged)
			}
			found = &current.Runtimes[i]
		}
		if found == nil {
			return fmt.Errorf("%w: selected runtime is missing", domain.ErrRunnerPlacementChanged)
		}
		if found.Status != domain.RuntimeStatusOnline {
			return fmt.Errorf("%w: selected runtime is not online", domain.ErrRunnerPlacementUnavailable)
		}
		if found.AuthMode != "" && found.AuthMode != domain.RuntimeAuthLocal && found.AuthMode != domain.RuntimeAuthPersonal {
			return fmt.Errorf("%w: registered runtime auth mode is invalid", domain.ErrRunnerPlacementChanged)
		}
		caps := make([]string, 0, len(found.Capabilities))
		seen := make(map[string]bool, len(found.Capabilities))
		for _, capability := range found.Capabilities {
			if !seen[capability] {
				caps = append(caps, capability)
				seen[capability] = true
			}
		}
		actual.Runtimes = append(actual.Runtimes, domain.NodeRuntimeSnapshot{
			Provider: selected.Provider, Version: found.Version,
			BinaryPath: found.BinaryPath, Capabilities: caps,
			AuthMode: current.RuntimeAuthMode(selected.Provider),
		})
	}
	digest, err := domain.NodeExecutionPlacementCapabilitySHA256(actual)
	if err != nil || digest != frozen.CapabilitySHA256 {
		return fmt.Errorf("%w: registered runner capabilities differ", domain.ErrRunnerPlacementChanged)
	}
	return nil
}

func runnerPlacementHasPoolLabel(labels []string, required string) bool {
	for _, label := range labels {
		if strings.TrimSpace(label) == required {
			return true
		}
	}
	return false
}
