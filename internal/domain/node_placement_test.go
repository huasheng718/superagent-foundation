package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func placementFixture(t *testing.T) NodeExecutionPlacement {
	t.Helper()
	registration, err := RunnerRegistrationSHA256(strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	return NodeExecutionPlacement{Version: 1, NodeAttempt: 1, RunnerID: "mac", RegistrationSHA256: registration, ResponsibleUserID: 7, ProjectID: "project", Access: NodePlacementAccessPersonal, OS: "darwin", Arch: "arm64", RunnerVersion: RunnerVersionNodePlacement, Runtimes: []NodeRuntimeSnapshot{
		{Provider: "terminal", BinaryPath: "/bin/sh", AuthMode: RuntimeAuthLocal},
		{Provider: "codex", Version: "1.2.3", BinaryPath: "/usr/local/bin/codex", AuthMode: RuntimeAuthLocal, Capabilities: []string{"tools", "json"}},
	}}
}

func TestRunnerRegistrationIdentityPurposeAndHeartbeat(t *testing.T) {
	runner := Runner{SessionTokenHash: strings.Repeat("1", 64)}
	first, err := RunnerRegistrationSHA256(runner.SessionTokenHash)
	if err != nil || first != "bafe3ce6268170e875f5e35ff7a981995e7faf4c0f5b5073638a0b6e35d764a0" {
		t.Fatalf("registration golden: %q %v", first, err)
	}
	now := time.Now()
	expiry := now.Add(time.Hour)
	runner.LastSeenAt = &now
	runner.SessionExpiresAt = &expiry
	runner.UpdatedAt = now
	same, err := RunnerRegistrationSHA256(runner.SessionTokenHash)
	if err != nil || same != first {
		t.Fatal("heartbeat or expiry extension changed registration identity")
	}
	runner.SessionTokenHash = strings.Repeat("2", 64)
	changed, err := RunnerRegistrationSHA256(runner.SessionTokenHash)
	if err != nil || changed == first {
		t.Fatal("registry replacement did not change identity")
	}
	for _, bad := range []string{"", strings.Repeat("A", 64), strings.Repeat("1", 63), strings.Repeat("g", 64), " " + strings.Repeat("1", 64), "raw-session-token"} {
		if _, err := RunnerRegistrationSHA256(bad); !errors.Is(err, ErrRunnerPlacementInvalid) {
			t.Errorf("accepted malformed registration hash %q", bad)
		}
	}
	p := placementFixture(t)
	raw, err := json.Marshal(p)
	if err != nil || strings.Contains(string(raw), strings.Repeat("1", 64)) || strings.Contains(string(raw), "sessionTokenHash") {
		t.Fatal("snapshot exposed raw session identity")
	}
}

func TestNodePlacementCanonicalInventoryAndDigest(t *testing.T) {
	p := placementFixture(t)
	original := cloneNodeExecutionPlacement(p)
	canonical, err := CanonicalNodeExecutionPlacement(p)
	if err != nil {
		t.Fatal(err)
	}
	// Independently computed from the ordered JSON manifest with Python's
	// SHA256 and an actual NUL purpose separator.
	if canonical.CapabilitySHA256 != "36d7a0ec67d47c62f8d9169cbd9a3a49ab5c62bd937d6753084913655e09ec65" {
		t.Fatalf("capability manifest golden changed: %s", canonical.CapabilitySHA256)
	}
	if !reflect.DeepEqual(original, p) {
		t.Fatal("canonicalization mutated input inventory")
	}
	if canonical.Runtimes[0].Provider != "codex" || canonical.Runtimes[0].Capabilities[0] != "json" || canonical.Runtimes[1].Capabilities == nil {
		t.Fatal("canonical order or empty capabilities incorrect")
	}
	permuted := placementFixture(t)
	permuted.Runtimes[0], permuted.Runtimes[1] = permuted.Runtimes[1], permuted.Runtimes[0]
	permuted.Runtimes[0].Capabilities = []string{"json", "tools"}
	permuted.Runtimes[1].Capabilities = []string{}
	reordered, err := CanonicalNodeExecutionPlacement(permuted)
	if err != nil || !reflect.DeepEqual(canonical, reordered) {
		t.Fatalf("inventory order changed canonical snapshot: %v", err)
	}
	twice, err := CanonicalNodeExecutionPlacement(canonical)
	if err != nil || !reflect.DeepEqual(twice, canonical) {
		t.Fatal("canonicalization is not idempotent")
	}
	changedAttempt := canonical
	changedAttempt.NodeAttempt++
	digest, err := NodeExecutionPlacementCapabilitySHA256(changedAttempt)
	if err != nil || digest != canonical.CapabilitySHA256 {
		t.Fatal("node attempt incorrectly changed capability inventory identity")
	}
	// Returned inventory must not retain aliases to the input.
	canonical.Runtimes[0].Capabilities[0] = "changed"
	if p.Runtimes[1].Capabilities[1] != "json" {
		t.Fatal("canonical inventory aliases source")
	}
	for name, mutate := range map[string]func(*NodeExecutionPlacement){
		"target":         func(p *NodeExecutionPlacement) { p.RunnerID = "linux" },
		"registration":   func(p *NodeExecutionPlacement) { p.RegistrationSHA256 = strings.Repeat("a", 64) },
		"owner":          func(p *NodeExecutionPlacement) { p.ResponsibleUserID++ },
		"project":        func(p *NodeExecutionPlacement) { p.ProjectID = "other" },
		"pool":           func(p *NodeExecutionPlacement) { p.Access = NodePlacementAccessCloudPool; p.PoolLabel = "approved" },
		"os":             func(p *NodeExecutionPlacement) { p.OS = "linux" },
		"arch":           func(p *NodeExecutionPlacement) { p.Arch = "amd64" },
		"version":        func(p *NodeExecutionPlacement) { p.RunnerVersion = "0.11.1" },
		"auth":           func(p *NodeExecutionPlacement) { p.Runtimes[1].AuthMode = RuntimeAuthPersonal },
		"path":           func(p *NodeExecutionPlacement) { p.Runtimes[1].BinaryPath = "/opt/bin/codex" },
		"runtimeVersion": func(p *NodeExecutionPlacement) { p.Runtimes[1].Version = "1.2.4" },
		"capability": func(p *NodeExecutionPlacement) {
			p.Runtimes[1].Capabilities = append(p.Runtimes[1].Capabilities, "extra")
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := placementFixture(t)
			mutate(&changed)
			got, err := NodeExecutionPlacementCapabilitySHA256(changed)
			if err != nil || got == reordered.CapabilitySHA256 {
				t.Fatalf("identity mutation not bound: %v", err)
			}
		})
	}
}

func TestNodePlacementRejectsMalformedSnapshots(t *testing.T) {
	for name, mutate := range map[string]func(*NodeExecutionPlacement){
		"snapshotVersion":  func(p *NodeExecutionPlacement) { p.Version++ },
		"attempt":          func(p *NodeExecutionPlacement) { p.NodeAttempt = 0 },
		"owner":            func(p *NodeExecutionPlacement) { p.ResponsibleUserID = 0 },
		"target":           func(p *NodeExecutionPlacement) { p.RunnerID = "" },
		"project":          func(p *NodeExecutionPlacement) { p.ProjectID = " project" },
		"registration":     func(p *NodeExecutionPlacement) { p.RegistrationSHA256 = strings.Repeat("A", 64) },
		"os":               func(p *NodeExecutionPlacement) { p.OS = "Darwin" },
		"arch":             func(p *NodeExecutionPlacement) { p.Arch = "arm 64" },
		"oldRunner":        func(p *NodeExecutionPlacement) { p.RunnerVersion = "0.10.99" },
		"personalPool":     func(p *NodeExecutionPlacement) { p.PoolLabel = "pool" },
		"missingPool":      func(p *NodeExecutionPlacement) { p.Access = NodePlacementAccessCloudPool },
		"access":           func(p *NodeExecutionPlacement) { p.Access = "unknown" },
		"emptyRuntime":     func(p *NodeExecutionPlacement) { p.Runtimes = nil },
		"duplicateRuntime": func(p *NodeExecutionPlacement) { p.Runtimes = append(p.Runtimes, p.Runtimes[0]) },
		"providerCase":     func(p *NodeExecutionPlacement) { p.Runtimes[1].Provider = "Codex" },
		"binary":           func(p *NodeExecutionPlacement) { p.Runtimes[0].BinaryPath = "" },
		"missingVersion":   func(p *NodeExecutionPlacement) { p.Runtimes[1].Version = "" },
		"auth":             func(p *NodeExecutionPlacement) { p.Runtimes[1].AuthMode = "" },
		"effectiveAuth":    func(p *NodeExecutionPlacement) { p.Runtimes[1].Provider = "pi" },
		"duplicateCap":     func(p *NodeExecutionPlacement) { p.Runtimes[1].Capabilities = []string{"json", "json"} },
		"blankCap":         func(p *NodeExecutionPlacement) { p.Runtimes[1].Capabilities = []string{""} },
		"controlCap":       func(p *NodeExecutionPlacement) { p.Runtimes[1].Capabilities = []string{"json\nsecret"} },
		"invalidUTF8Cap":   func(p *NodeExecutionPlacement) { p.Runtimes[1].Capabilities = []string{string([]byte{0xff})} },
		"wrongDigest":      func(p *NodeExecutionPlacement) { p.CapabilitySHA256 = strings.Repeat("a", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			p := placementFixture(t)
			mutate(&p)
			if _, err := CanonicalNodeExecutionPlacement(p); !errors.Is(err, ErrRunnerPlacementInvalid) {
				t.Fatalf("malformed snapshot accepted: %v", err)
			}
		})
	}
}

func TestNodePlacementStrictRunnerVersion(t *testing.T) {
	for _, version := range []string{"0.11.0", "0.11.0+build.001", "0.11.1", "0.12.0-rc.1", "1.0.0", "999999999999999999999.0.0"} {
		if !RunnerVersionSupportsNodePlacement(version) {
			t.Errorf("rejected supported SemVer %q", version)
		}
	}
	for _, version := range []string{"", "0.10.99", "0.11.0-rc.1", "0.11.0-0", "v0.11.0", "0.11", "0.11.0.1", "00.11.0", "0.011.0", "0.11.00", "0.12.0-01", "0.12.0-rc.01", "0.12.0+", "0.12.0-", "0.12.0+build..1", " 0.11.0", "0.11.0\n", "0.11.0garbage"} {
		if RunnerVersionSupportsNodePlacement(version) {
			t.Errorf("accepted unsupported or malformed SemVer %q", version)
		}
	}
}

func TestNodePlacementRequestCanonical(t *testing.T) {
	request := NodePlacementRequest{RunnerID: "runner", RequiredCapabilities: []string{"tools", "json"}}
	got, err := CanonicalNodePlacementRequest(request)
	if err != nil || got.RequiredCapabilities[0] != "json" || request.RequiredCapabilities[0] != "tools" {
		t.Fatal("request canonicalization incorrect or mutates input")
	}
	for _, request := range []NodePlacementRequest{{}, {RunnerID: "x", OS: "LINUX"}, {RunnerID: "x", Arch: "arm 64"}, {RunnerID: "x", RequiredCapabilities: []string{"json", "json"}}, {RunnerID: "x", RequiredCapabilities: []string{" "}}} {
		if _, err := CanonicalNodePlacementRequest(request); !errors.Is(err, ErrRunnerPlacementInvalid) {
			t.Fatalf("malformed request accepted: %+v %v", request, err)
		}
	}
}

func TestPublicRunnerTaskCopiesNodeIdentityAndInputLineage(t *testing.T) {
	p := placementFixture(t)
	task := RunnerTask{ExecutionPlacement: &p, Execution: &NodeTaskExecution{Role: NodeTaskRoleAuthor, NodeAttempt: 1, Sequence: 1}, MaintenanceKey: &RunnerMaintenanceKey{RunID: "run"}, Invocation: &RunnerInvocation{ContributionInputs: []RunnerContributionInput{{RepositoryID: "repo", Sources: []RunnerContributionInputSource{{TaskID: "source"}}}}}}
	public := PublicRunnerTask(task)
	public.ExecutionPlacement.Runtimes[1].Capabilities[0] = "changed"
	public.Execution.Role = NodeTaskRoleVerification
	public.MaintenanceKey.RunID = "changed"
	public.Invocation.ContributionInputs[0].RepositoryID = "changed"
	public.Invocation.ContributionInputs[0].Sources[0].TaskID = "changed"
	if task.ExecutionPlacement.Runtimes[1].Capabilities[0] != "tools" || task.Execution.Role != NodeTaskRoleAuthor || task.MaintenanceKey.RunID != "run" || task.Invocation.ContributionInputs[0].RepositoryID != "repo" || task.Invocation.ContributionInputs[0].Sources[0].TaskID != "source" {
		t.Fatal("public task retains mutable aliases to new metadata")
	}
	// Pointer copying must also occur for maintenance tasks without invocation.
	task.Invocation = nil
	public = PublicRunnerTask(task)
	public.ExecutionPlacement.RunnerID = "changed"
	public.Execution.Sequence = 2
	public.MaintenanceKey.RunID = "other"
	if task.ExecutionPlacement.RunnerID != "mac" || task.Execution.Sequence != 1 || task.MaintenanceKey.RunID != "run" {
		t.Fatal("nil-invocation public task aliases metadata")
	}
}
