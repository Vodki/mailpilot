//go:build !unix

package claudecli

import (
	"os/exec"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) { cmd.WaitDelay = 2 * time.Second }
