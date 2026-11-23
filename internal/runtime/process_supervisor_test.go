//go:build darwin || linux

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

// Both processes deliberately ignore TERM. The child is silent and writes a
// local heartbeat, so pipe closure / SIGPIPE cannot masquerade as cleanup.
const nativeProcessFixture = `
trap '' TERM INT HUP
echo $$ > "$1/leader.pid"
/bin/sh -c '
trap "" TERM INT HUP
echo $$ > "$1/child.pid"
while :; do
    echo tick >> "$1/ticks"
    /bin/sleep 0.05
done
' child "$1" </dev/null >/dev/null 2>&1 &
if [ "$2" = exit ]; then
    while [ ! -s "$1/child.pid" ]; do /bin/sleep 0.01; done
    exit 0
fi
wait
`

func TestNativeProcessPreservesOutputAndExitStatus(t *testing.T) {
	var events []domain.TelemetryEvent
	output, err := runNativeCommand(exec.Command("/bin/sh", "-c", "echo native-stdout; echo native-stderr >&2; exit 7"), func(event domain.TelemetryEvent) {
		events = append(events, event)
	})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("exit status = %v; want 7", err)
	}
	if !strings.Contains(output, "native-stdout") || !strings.Contains(output, "native-stderr") {
		t.Fatalf("output lost: %q", output)
	}
	if len(events) == 0 || events[0].Message != "/bin/sh -c echo native-stdout; echo native-stderr >&2; exit 7" {
		t.Fatalf("original argv telemetry lost: %+v", events)
	}
}

func TestNativeProcessCancellationStopsSilentDescendant(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := runNativeCommand(exec.CommandContext(ctx, "/bin/sh", "-c", nativeProcessFixture, "fixture", dir, "wait"), nil)
		done <- err
	}()
	leader, child := nativeFixturePIDs(t, dir)
	assertNativeGroup(t, leader, child)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled command returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled command did not return")
	}
	assertNativeStopped(t, leader, child)
}

func TestNativeProcessNormalExitStopsSilentDescendant(t *testing.T) {
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := runNativeCommand(exec.Command("/bin/sh", "-c", nativeProcessFixture, "fixture", dir, "exit"), nil)
		done <- err
	}()
	leader, child := nativeFixturePIDs(t, dir)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("normal command exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("normal command did not return")
	}
	assertNativeStopped(t, leader, child)
}

func TestNativeProcessOwnerHelper(t *testing.T) {
	dir := os.Getenv("AGENTFLOW_NATIVE_PROCESS_TEST_DIR")
	if dir == "" {
		return
	}
	_, err := runNativeCommand(exec.Command("/bin/sh", "-c", nativeProcessFixture, "fixture", dir, "wait"), nil)
	t.Fatalf("helper target exited before owner was killed: %v", err)
}

func TestNativeProcessOwnerSIGKILLStopsSilentDescendant(t *testing.T) {
	dir := t.TempDir()
	owner := exec.Command(os.Args[0], "-test.run=^TestNativeProcessOwnerHelper$")
	owner.Env = append(os.Environ(), "AGENTFLOW_NATIVE_PROCESS_TEST_DIR="+dir)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ownerDone := false
	t.Cleanup(func() {
		if !ownerDone {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	})
	leader, child := nativeFixturePIDs(t, dir)
	assertNativeGroup(t, leader, child)
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	ownerDone = true
	assertNativeStopped(t, leader, child)
}

func nativeFixturePIDs(t *testing.T, dir string) (int, int) {
	t.Helper()
	var leader, child int
	t.Cleanup(func() {
		if leader > 0 {
			_ = syscall.Kill(-leader, syscall.SIGKILL)
		}
		if child > 0 {
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		leaderData, _ := os.ReadFile(filepath.Join(dir, "leader.pid"))
		childData, _ := os.ReadFile(filepath.Join(dir, "child.pid"))
		leader, _ = strconv.Atoi(strings.TrimSpace(string(leaderData)))
		child, _ = strconv.Atoi(strings.TrimSpace(string(childData)))
		if leader > 0 && child > 0 {
			return leader, child
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture PIDs missing: leader=%d child=%d", leader, child)
	return 0, 0
}

func assertNativeGroup(t *testing.T, leader, child int) {
	t.Helper()
	for _, pid := range []int{leader, child} {
		out, err := exec.Command("/bin/ps", "-o", "pgid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || strings.TrimSpace(string(out)) != strconv.Itoa(leader) {
			t.Fatalf("pid %d group = %q err=%v; want %d", pid, out, err, leader)
		}
	}
}

func assertNativeStopped(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var active []int
	for time.Now().Before(deadline) {
		active = active[:0]
		for _, pid := range pids {
			if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				continue
			}
			out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
			if err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
				continue
			}
			active = append(active, pid)
		}
		if len(active) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("native processes still executing: %v", active))
}
