//go:build !windows

package beadsclient

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr detaches the child process on Unix so it survives
// when the parent loom process exits.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}
