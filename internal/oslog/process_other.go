//go:build !unix

package oslog

import "os/exec"

// setProcessAttrs is a no-op on platforms without POSIX process groups.
func setProcessAttrs(cmd *exec.Cmd) {}

// terminateProcess signals the direct child only.
func terminateProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
