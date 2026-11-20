package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

// This opt-in compatibility check uses an existing image and its own container.
// A storage-driver refusal is retained as fail-closed evidence, not startup success.
func TestDockerHardenedLocalCompatibility(t *testing.T) {
	if os.Getenv("AGENTFLOW_TEST_DOCKER_HARDENED") != "1" {
		t.Skip("requires explicit local Docker acceptance opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id := fmt.Sprintf("p1-normal-%d", time.Now().UnixNano())
	name := dockerContainerName(id)
	report := map[string]any{"container": name, "normalStartup": false}
	command := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := command("container", "inspect", name); err == nil || !dockerObjectGone(out, err) {
		t.Fatalf("fresh container name unavailable: %s: %v", out, err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		out, err := exec.CommandContext(cleanupCtx, "docker", "container", "inspect", name).CombinedOutput()
		if err == nil {
			removed, removeErr := exec.CommandContext(cleanupCtx, "docker", "rm", "-f", name).CombinedOutput()
			report["cleanupOutput"] = strings.TrimSpace(string(removed))
			if removeErr != nil {
				t.Errorf("remove own container: %v: %s", removeErr, removed)
			}
			out, err = exec.CommandContext(cleanupCtx, "docker", "container", "inspect", name).CombinedOutput()
		}
		report["cleanupVerified"] = dockerObjectGone(string(out), err)
		report["cleanupInspect"] = strings.TrimSpace(string(out))
		if report["cleanupVerified"] != true {
			t.Error("own container absence not verified")
		}
		data, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		t.Log(string(data))
		if path := os.Getenv("AGENTFLOW_TEST_DOCKER_REPORT"); path != "" {
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Error(err)
			}
		}
	}()
	for _, key := range []string{dockerMemoryLimitEnv, dockerCPULimitEnv, dockerPIDsLimitEnv, dockerStorageLimitEnv, dockerTmpfsLimitEnv, "AGENTFLOW_RUNTIME_CLI_DIR"} {
		t.Setenv(key, "")
	}
	t.Setenv(dockerHardenedEnv, "1")
	t.Setenv("AGENTFLOW_DOCKER_NETWORK", "none")
	t.Setenv("AGENTFLOW_ISOLATED_IMAGE", "alpine:3.20")
	if !dockerImageExists(ctx, "alpine:3.20") {
		t.Fatal("preinstalled alpine:3.20 required; this test does not pull/build")
	}
	limits, err := dockerResourceLimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	report["declaredResourceArgs"] = appendDockerResourceLimitArgs(nil, limits)
	report["declaredTmpfs"] = "/tmp:rw,noexec,nosuid,size=" + limits.tmpfs
	workspace := t.TempDir()
	defer os.RemoveAll(dockerIsolationMaskRoot(workspace))
	sandbox, err := createDocker(ctx, id, workspace, domain.SandboxProfile{Driver: "docker", ReadOnlyRootFS: true}, SandboxRequirements{}, domain.DockerfileDefinition{Content: IsolatedDockerfile})
	if err != nil {
		t.Fatal(err)
	}
	err = sandbox.(*dockerSandbox).ensureStarted(ctx)
	if err != nil {
		report["startupError"] = err.Error()
		if strings.Contains(err.Error(), "docker create:") && strings.Contains(err.Error(), "--storage-opt is supported only for overlay over xfs with 'pquota' mount option") {
			out, inspectErr := command("container", "inspect", name)
			if sandbox.(*dockerSandbox).started || !dockerObjectGone(out, inspectErr) {
				t.Fatalf("storage refusal left a sandbox: %s: %v", out, inspectErr)
			}
			report["result"] = "storage-driver-rejected-fail-closed"
			return
		}
		report["result"] = "normal-startup-blocked"
		t.Fatal(err)
	}
	report["normalStartup"] = true
	out, err := command("inspect", name)
	if err != nil {
		t.Fatalf("inspect own container: %s: %v", out, err)
	}
	var inspected []struct {
		HostConfig struct {
			Memory, NanoCPUs int64
			PidsLimit        int64
			StorageOpt       map[string]string
			Tmpfs            map[string]string
			ReadonlyRootfs   bool
			NetworkMode      string
		}
	}
	if err := json.Unmarshal([]byte(out), &inspected); err != nil || len(inspected) != 1 {
		t.Fatalf("inspect decode: %v", err)
	}
	host := inspected[0].HostConfig
	report["hostConfig"] = host
	if host.Memory != 4<<30 || host.NanoCPUs != 2_000_000_000 || host.PidsLimit != 512 || host.StorageOpt["size"] != "8g" || host.Tmpfs["/tmp"] != "rw,noexec,nosuid,size=1g" || !host.ReadonlyRootfs || host.NetworkMode != "none" {
		t.Fatalf("resource contract mismatch: %+v", host)
	}
	out, err = command("exec", name, "sh", "-lc", "cd /workspace && echo normal-command-ok && pwd")
	report["normalCommandOutput"] = out
	if err != nil || !strings.Contains(out, "normal-command-ok\n/workspace") {
		t.Fatalf("normal command: %s: %v", out, err)
	}
	report["result"] = "normal-startup-resource-inspect-passed"
}
