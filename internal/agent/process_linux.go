//go:build linux

package agent

import (
	"os/exec"
	"syscall"
)

// A killed agent must not leave an untracked GPU process serving indefinitely.
func configureChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
