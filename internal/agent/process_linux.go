//go:build linux

package agent

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// A killed agent must not leave an untracked GPU process serving indefinitely.
func configureChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	// Petals spawns handlers, runtime pools and a libp2p daemon. Cancel the
	// process group, not just its Python parent, including on lease expiry.
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
	}
	// Bound inherited stdout/stderr pipes if a descendant detached itself.
	cmd.WaitDelay = 5 * time.Second
}

// Context cancellation may no longer invoke cmd.Cancel after Wait returns.
// A Python parent that crashes can still leave living members of its group.
func cleanupChild(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
