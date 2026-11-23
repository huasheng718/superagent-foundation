//go:build !darwin && !linux

package runtime

import "os/exec"

func runNativeCommand(cmd *exec.Cmd, sink EventSink) (string, error) {
	return runCommand(cmd, sink)
}
