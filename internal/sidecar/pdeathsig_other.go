//go:build !linux

package sidecar

import "os/exec"

// applySysProcAttr 非 Linux 平台没有 Pdeathsig:
// 主进程异常退出时可能遗留 sing-box 子进程,由系统重启兜底
func applySysProcAttr(cmd *exec.Cmd) {}
