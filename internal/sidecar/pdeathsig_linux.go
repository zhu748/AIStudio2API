//go:build linux

package sidecar

import (
	"os/exec"
	"syscall"
)

// applySysProcAttr 让子进程随主进程退出自动终止(Pdeathsig)
func applySysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
