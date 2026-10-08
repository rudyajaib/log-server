//go:build unix

package oslog

import (
	"os/exec"
	"syscall"
)

// setProcessAttrs puts the stream in its own process group so that terminating
// it also tears down grandchildren such as the `log` process spawned inside a
// simulator.
func setProcessAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcess asks the whole process group to exit.
func terminateProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid > 0 {
		return syscall.Kill(-pgid, syscall.SIGTERM)
	}
	return cmd.Process.Signal(syscall.SIGTERM)
}
