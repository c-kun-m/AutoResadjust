//go:build !linux

package agent

import "os/exec"

func configureChild(cmd *exec.Cmd) {}
