//go:build unix

package claudecli

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup puts the child in its own process group so a timeout kills
// it and anything it spawned: no orphan survives a cancelled call.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}
