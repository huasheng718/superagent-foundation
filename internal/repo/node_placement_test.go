package repo

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func newRegistryPlacementFixture(t *testing.T, shared bool) (*Store, domain.Runner, domain.NodeExecutionPlacement, time.Time) {
	t.Helper()
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	now := time.Now().UTC().Truncate(time.Millisecond)
	expires := now.Add(time.Hour)
	runner := domain.Runner{
		ID: "placement-registry", Name: "Placement test", OS: "linux", Arch: "arm64",
		RunnerVersion: domain.RunnerVersionNodePlacement, Status: domain.RunnerStatusOnline,
		OwnerUserID: 42, SessionTokenHash: strings.Repeat("1", 64), SessionExpiresAt: &expires, LastSeenAt: &now,
		Runtimes: []domain.RunnerRuntime{
			{Provider: "codex", BinaryPath: "/opt/bin/codex", Version: "0.57.0", Capabilities: []string{"sandbox", "non_interactive"}, Status: domain.RuntimeStatusOnline, AuthMode: domain.RuntimeAuthLocal},
			{Provider: "terminal", BinaryPath: "(builtin)", Capabilities: []string{"terminal"}, Status: domain.RuntimeStatusOnline, AuthMode: domain.RuntimeAuthLocal},
		},
	}
	access, label := domain.NodePlacementAccessPersonal, ""
	if shared {
		runner.OwnerUserID = 0
		runner.Labels = []string{"os=linux", "pool=placement"}
		access, label = domain.NodePlacementAccessCloudPool, "pool=placement"
	}
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	registration, err := domain.RunnerRegistrationSHA256(runner.SessionTokenHash)
	if err != nil {
		t.Fatal(err)
	}
	placement := domain.NodeExecutionPlacement{
		Version: domain.NodeExecutionPlacementVersion, NodeAttempt: 1,
		RunnerID: runner.ID, RegistrationSHA256: registration, ResponsibleUserID: 42,
		ProjectID: "project", Access: access, PoolLabel: label,
		OS: runner.OS, Arch: runner.Arch, RunnerVersion: runner.RunnerVersion,
		Runtimes: []domain.NodeRuntimeSnapshot{
			{Provider: "codex", BinaryPath: "/opt/bin/codex", Version: "0.57.0", Capabilities: []string{"non_interactive", "sandbox"}, AuthMode: domain.RuntimeAuthLocal},
			{Provider: "terminal", BinaryPath: "(builtin)", Capabilities: []string{"terminal"}, AuthMode: domain.RuntimeAuthLocal},
		},
	}
	placement, err = domain.CanonicalNodeExecutionPlacement(placement)
	if err != nil {
		t.Fatal(err)
	}
	return s, runner, placement, now
}

func TestValidateRunnerPlacementUnchangedRegistryAndHeartbeat(t *testing.T) {
	s, runner, frozen, now := newRegistryPlacementFixture(t, false)
	if err := s.ValidateRunnerPlacement(frozen, now); err != nil {
		t.Fatal(err)
	}
	// A repeated inventory report may reorder/deduplicate capabilities and
	// extend expiry without replacing the registered machine.
	reported := append([]domain.RunnerRuntime(nil), runner.Runtimes...)
	reported[0].Capabilities = []string{"sandbox", "non_interactive", "sandbox"}
	reported[0].AuthMode = domain.RuntimeAuthPersonal // heartbeat cannot replace platform auth preference
	if err := s.RunnerHeartbeat(runner.ID, reported, runner.RunnerVersion, now.Add(time.Minute), now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRunnerPlacement(frozen, now.Add(time.Minute)); err != nil {
		t.Fatalf("unchanged heartbeat invalidated frozen registration: %v", err)
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		return s.validateRunnerPlacementTx(tx, frozen, runner.SessionTokenHash, now.Add(time.Minute))
	}); err != nil {
		t.Fatalf("authenticated registry check: %v", err)
	}
	// Additional unselected runtime inventory is not part of this task's
	// capability requirements and cannot invalidate the selected inventory.
	current, _ := s.Runner(runner.ID)
	current.Runtimes = append(current.Runtimes, domain.RunnerRuntime{Provider: "claude-code", BinaryPath: "/other", Version: "2", Status: domain.RuntimeStatusDisabled})
	if err := s.PutRunner(current); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRunnerPlacement(frozen, now); err != nil {
		t.Fatalf("unselected runtime invalidated placement: %v", err)
	}
}

func TestValidateRunnerPlacementRejectsRegistryDrift(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.Runner)
		want   error
	}{
		{"registration-replaced", func(r *domain.Runner) { r.SessionTokenHash = strings.Repeat("2", 64) }, domain.ErrRunnerPlacementChanged},
		{"owner-replaced", func(r *domain.Runner) { r.OwnerUserID = 43 }, domain.ErrRunnerPlacementChanged},
		{"owner-cleared", func(r *domain.Runner) { r.OwnerUserID = 0 }, domain.ErrRunnerPlacementChanged},
		{"auth-mode", func(r *domain.Runner) { r.Runtimes[0].AuthMode = domain.RuntimeAuthPersonal }, domain.ErrRunnerPlacementChanged},
		{"auth-mode-invalid", func(r *domain.Runner) { r.Runtimes[0].AuthMode = "unknown" }, domain.ErrRunnerPlacementChanged},
		{"runtime-disabled", func(r *domain.Runner) { r.Runtimes[0].Status = domain.RuntimeStatusDisabled }, domain.ErrRunnerPlacementUnavailable},
		{"runtime-offline", func(r *domain.Runner) { r.Runtimes[0].Status = domain.RuntimeStatusOffline }, domain.ErrRunnerPlacementUnavailable},
		{"terminal-offline", func(r *domain.Runner) { r.Runtimes[1].Status = domain.RuntimeStatusOffline }, domain.ErrRunnerPlacementUnavailable},
		{"capability-missing", func(r *domain.Runner) { r.Runtimes[0].Capabilities = []string{"sandbox"} }, domain.ErrRunnerPlacementChanged},
		{"capability-added", func(r *domain.Runner) { r.Runtimes[0].Capabilities = append(r.Runtimes[0].Capabilities, "new") }, domain.ErrRunnerPlacementChanged},
		{"capability-malformed", func(r *domain.Runner) { r.Runtimes[0].Capabilities = []string{""} }, domain.ErrRunnerPlacementChanged},
		{"binary-path", func(r *domain.Runner) { r.Runtimes[0].BinaryPath = "/different/codex" }, domain.ErrRunnerPlacementChanged},
		{"runtime-version", func(r *domain.Runner) { r.Runtimes[0].Version = "0.58.0" }, domain.ErrRunnerPlacementChanged},
		{"runner-version", func(r *domain.Runner) { r.RunnerVersion = "0.12.0" }, domain.ErrRunnerPlacementChanged},
		{"runner-too-old", func(r *domain.Runner) { r.RunnerVersion = "0.10.0" }, domain.ErrRunnerPlacementChanged},
		{"operating-system", func(r *domain.Runner) { r.OS = "darwin" }, domain.ErrRunnerPlacementChanged},
		{"architecture", func(r *domain.Runner) { r.Arch = "amd64" }, domain.ErrRunnerPlacementChanged},
		{"runtime-missing", func(r *domain.Runner) { r.Runtimes = r.Runtimes[1:] }, domain.ErrRunnerPlacementChanged},
		{"runtime-duplicate", func(r *domain.Runner) { r.Runtimes = append(r.Runtimes, r.Runtimes[0]) }, domain.ErrRunnerPlacementChanged},
		{"runner-offline", func(r *domain.Runner) { r.Status = domain.RunnerStatusOffline }, domain.ErrRunnerPlacementUnavailable},
		{"missing-session-expiry", func(r *domain.Runner) { r.SessionExpiresAt = nil }, ErrRunnerPlacementSessionInvalid},
		{"malformed-session-hash", func(r *domain.Runner) { r.SessionTokenHash = "not-a-session-hash" }, ErrRunnerPlacementSessionInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, runner, frozen, now := newRegistryPlacementFixture(t, false)
			tc.mutate(&runner)
			if err := s.PutRunner(runner); err != nil {
				t.Fatal(err)
			}
			err := s.ValidateRunnerPlacement(frozen, now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), runner.SessionTokenHash) || strings.Contains(err.Error(), frozen.RegistrationSHA256) {
				t.Fatal("registry authorization error leaked credential or identity digest")
			}
		})
	}
}

func TestValidateRunnerPlacementSharedPoolIdentity(t *testing.T) {
	for _, mode := range []string{"valid", "label-removed", "personal-owner"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, frozen, now := newRegistryPlacementFixture(t, true)
			switch mode {
			case "label-removed":
				runner.Labels = []string{"pool=other"}
			case "personal-owner":
				runner.OwnerUserID = 42
			}
			if err := s.PutRunner(runner); err != nil {
				t.Fatal(err)
			}
			err := s.ValidateRunnerPlacement(frozen, now)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, domain.ErrRunnerPlacementChanged) {
				t.Fatalf("pool identity drift: %v", err)
			}
		})
	}
}

func TestValidateRunnerPlacementSessionAndPostLockTime(t *testing.T) {
	s, runner, frozen, now := newRegistryPlacementFixture(t, false)
	for _, supplied := range []string{strings.Repeat("2", 64), "invalid-session-hash"} {
		err := s.DB.Transaction(func(tx *gorm.DB) error {
			return s.validateRunnerPlacementTx(tx, frozen, supplied, now)
		})
		if !errors.Is(err, ErrRunnerPlacementSessionInvalid) {
			t.Fatalf("wrong session: %v", err)
		}
	}
	// SQL clock is already beyond expiry even though the request's captured
	// time is before it. This proves validation uses the post-lock clock.
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		tx = tx.Session(&gorm.Session{NowFunc: func() time.Time { return now.Add(2 * time.Hour) }})
		return s.validateRunnerPlacementTx(tx, frozen, runner.SessionTokenHash, now)
	})
	if !errors.Is(err, ErrRunnerPlacementSessionInvalid) {
		t.Fatalf("post-lock expiry: %v", err)
	}
	// Expiry is exclusive, including a future caller time greater than the DB
	// clock; choosing the earlier clock would wrongly authorize this request.
	if err := s.ValidateRunnerPlacement(frozen, *runner.SessionExpiresAt); !errors.Is(err, ErrRunnerPlacementSessionInvalid) {
		t.Fatalf("exact-boundary session expiry: %v", err)
	}
}

func TestValidateRunnerPlacementRegistrationRotationAndEffectiveAuth(t *testing.T) {
	s, runner, frozen, now := newRegistryPlacementFixture(t, false)
	// An omitted Codex preference still means the existing local-login default.
	runner.Runtimes[0].AuthMode = ""
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRunnerPlacement(frozen, now); err != nil {
		t.Fatalf("effective local auth default changed: %v", err)
	}
	oldSession := runner.SessionTokenHash
	runner.SessionTokenHash = strings.Repeat("2", 64)
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		session string
		want    error
	}{
		{oldSession, ErrRunnerPlacementSessionInvalid},
		{runner.SessionTokenHash, domain.ErrRunnerPlacementChanged},
	} {
		err := s.DB.Transaction(func(tx *gorm.DB) error {
			return s.validateRunnerPlacementTx(tx, frozen, tc.session, now)
		})
		if !errors.Is(err, tc.want) {
			t.Fatalf("rotated registration: expected %v, got %v", tc.want, err)
		}
	}
}

func TestValidateRunnerPlacementRejectsInvalidFrozenSnapshot(t *testing.T) {
	for _, mode := range []string{"digest-missing", "digest-forged", "duplicate-provider", "unknown-version"} {
		t.Run(mode, func(t *testing.T) {
			s, _, frozen, now := newRegistryPlacementFixture(t, false)
			switch mode {
			case "digest-missing":
				frozen.CapabilitySHA256 = ""
			case "digest-forged":
				frozen.CapabilitySHA256 = strings.Repeat("0", 64)
			case "duplicate-provider":
				frozen.Runtimes = append(frozen.Runtimes, frozen.Runtimes[0])
			case "unknown-version":
				frozen.Version++
			}
			if err := s.ValidateRunnerPlacement(frozen, now); !errors.Is(err, domain.ErrRunnerPlacementInvalid) {
				t.Fatalf("invalid frozen snapshot: %v", err)
			}
		})
	}
	// A valid frozen snapshot whose registry row vanished is unavailable, not
	// an empty/legacy inventory that could accidentally pass provider checks.
	s, runner, frozen, now := newRegistryPlacementFixture(t, false)
	if err := s.DB.Where("id = ?", runner.ID).Delete(&runnerRow{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRunnerPlacement(frozen, now); !errors.Is(err, domain.ErrRunnerPlacementUnavailable) {
		t.Fatalf("missing registry: %v", err)
	}
}
