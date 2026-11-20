//go:build darwin || linux

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// Native CLIs use SIGINT to cancel their own managed tool sessions, some of
// which legitimately run in another process group. Give that cooperative path
// a bounded window before os/exec kills the direct process. The independent
// watchdog still handles an exiting leader or owner death without Go cleanup.
const nativeCancellationWaitDelay = 3 * time.Second

// The watchdog is started before exec replaces the launcher with the target.
// It is independent of the Go owner's cleanup paths, so even SIGKILL of that
// owner ends the target group. Its own group membership reserves the PGID until
// cleanup. This covers descendants that stay in the group, not setsid escapes.
// Absolute utility paths keep the watchdog independent of the task's PATH.
const nativeProcessLauncher = `
owner=$1
shift
/bin/sh -c '
leader=$1
owner=$2
cancelled=0
trap "" INT
trap "cancelled=1" TERM HUP
while [ "$cancelled" -eq 0 ]; do
    set -- $(/bin/ps -o ppid= -p "$leader" 2>/dev/null)
    [ "$#" -eq 1 ] && [ "$1" = "$owner" ] || break
    /bin/sleep 0.1
done
trap "" TERM INT HUP
/bin/sleep 0.2
/bin/kill -KILL -- "-$leader"
' agentflow-process-watchdog "$$" "$owner" </dev/null >/dev/null 2>&1 &
exec "$@"
`

func runNativeCommand(cmd *exec.Cmd, sink EventSink) (string, error) {
	displayArgs := append([]string(nil), cmd.Args...)
	args := []string{"/bin/sh", "-c", nativeProcessLauncher, "agentflow-process-supervisor", strconv.Itoa(os.Getpid()), cmd.Path}
	if len(cmd.Args) > 1 {
		args = append(args, cmd.Args[1:]...)
	}
	cmd.Path, cmd.Args = "/bin/sh", args
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	// os/exec permits Cancel only on commands created with CommandContext.
	if cmd.Cancel != nil {
		cmd.WaitDelay = nativeCancellationWaitDelay
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return os.ErrProcessDone
			}
			// SIGTERM can make an Agent CLI exit before it cancels a tool
			// session in another group. SIGINT invokes its interrupt handler;
			// WaitDelay and the watchdog bound a CLI that ignores it.
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
	}
	return runCommandWithDisplayArgs(cmd, sink, displayArgs)
}
