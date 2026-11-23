//go:build darwin || linux

package runtime

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This models a cooperative CLI managing a silent tool in a distinct group.
// TERM would terminate the CLI before it can clean that tool; INT is its
// cancellation protocol. It is not a hostile setsid containment test.
func TestNativeProcessInterruptHelper(t *testing.T) {
	dir := os.Getenv("AGENTFLOW_NATIVE_INTERRUPT_HELPER_DIR")
	if dir == "" {
		return
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	if err := os.WriteFile(filepath.Join(dir, "leader.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := exec.Command("/bin/sh", "-c", `trap '' TERM INT HUP
echo $$ > "$1/child.pid"
while :; do echo tick >> "$1/ticks"; /bin/sleep 0.05; done`, "tool", dir)
	tool.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := tool.Start(); err != nil {
		t.Fatal(err)
	}
	<-interrupt
	if err := syscall.Kill(-tool.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = tool.Wait()
	if err := os.WriteFile(filepath.Join(dir, "cooperative-cleanup"), []byte("interrupted and stopped independent tool group"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestNativeProcessInterruptLetsCLICleanIndependentToolGroup(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeProcessInterruptHelper$")
	cmd.Env = append(os.Environ(), "AGENTFLOW_NATIVE_INTERRUPT_HELPER_DIR="+dir)
	done := make(chan error, 1)
	go func() { _, err := runNativeCommand(cmd, nil); done <- err }()
	leader, child := nativeFixturePIDs(t, dir)
	t.Cleanup(func() { _ = syscall.Kill(-child, syscall.SIGKILL) })
	group, err := exec.Command("/bin/ps", "-o", "pgid=", "-p", strconv.Itoa(child)).Output()
	if err != nil || strings.TrimSpace(string(group)) != strconv.Itoa(child) || child == leader {
		t.Fatalf("tool is not in an independent group: %q err=%v", group, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cooperative CLI cancellation did not return")
	}
	if _, err := os.Stat(filepath.Join(dir, "cooperative-cleanup")); err != nil {
		t.Fatal("CLI interrupt handler did not clean its tool")
	}
	assertNativeStopped(t, leader, child)
}
