//go:build containment_experiment && darwin

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentflow/internal/domain"
)

// TestNativeProcessSetsidEscapeNegativeEvidence is an opt-in experiment, not
// a passing security claim. It records the current native boundary: a child
// that calls os.setsid leaves the supervised process group and survives the
// normal cancellation/watchdog cleanup. The child is killed by the test after
// observation so the experiment cannot leak a process.
func TestNativeProcessSetsidEscapeNegativeEvidence(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", `
/usr/bin/python3 - "$1" <<'PY' &
import os, sys, time
os.setsid()
with open(os.path.join(sys.argv[1], "escaped.pid"), "w") as f:
    f.write(str(os.getpid()))
while True:
    time.sleep(0.05)
PY
wait
`, "setsid-experiment", dir)
	done := make(chan error, 1)
	go func() {
		_, err := runNativeCommand(command, nil)
		done <- err
	}()

	var escaped int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(filepath.Join(dir, "escaped.pid"))
		escaped, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		if escaped > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if escaped <= 0 {
		t.Fatal("setsid child did not publish its pid")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervised command did not stop")
	}
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(escaped, 0); err != nil {
		t.Fatalf("setsid child did not survive group cleanup: pid=%d err=%v", escaped, err)
	}
	_ = syscall.Kill(escaped, syscall.SIGKILL)
	for i := 0; i < 50; i++ {
		if errors.Is(syscall.Kill(escaped, 0), syscall.ESRCH) {
			t.Logf("setsid child escaped native group and was then cleaned: pid=%d", escaped)
			t.Skip("negative evidence: native process supervision does not contain setsid escapes")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("escaped child %d remained after cleanup", escaped))
}

func TestSeatbeltSiblingReadNegativeEvidence(t *testing.T) {
	workspace := t.TempDir()
	sibling := filepath.Join(t.TempDir(), "sibling-secret.txt")
	if err := os.WriteFile(sibling, []byte("sibling-visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	sandbox, err := createSeatbelt("sibling-read", workspace, domainSandboxProfileForExperiment(), SandboxRequirements{})
	if err != nil {
		t.Skipf("seatbelt unavailable: %v", err)
	}
	output, err := sandbox.Exec(context.Background(), CommandSpec{Argv: []string{"/bin/cat", sibling}}, nil)
	if err != nil {
		t.Fatalf("sibling read unexpectedly failed before observation: %v", err)
	}
	if strings.TrimSpace(output) != "sibling-visible" {
		t.Fatalf("unexpected sibling content: %q", output)
	}
	t.Logf("native seatbelt workspace could read an absolute sibling path: %s", sibling)
	t.Skip("negative evidence: same-user native sandbox does not deny sibling reads")
}

func domainSandboxProfileForExperiment() domain.SandboxProfile {
	return domain.SandboxProfile{Driver: "seatbelt"}
}
