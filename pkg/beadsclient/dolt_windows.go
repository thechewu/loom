package beadsclient

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr detaches the child process on Windows so it survives
// when the parent loom process exits.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}
