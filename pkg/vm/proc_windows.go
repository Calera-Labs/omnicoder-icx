// SPDX-License-Identifier: Apache-2.0
//go:build windows

package vm

import (
	"fmt"
	"os/exec"
)

// ConfigureSubprocessGroup is a no-op on Windows because KillProcessTree
// terminates the entire descendant tree via taskkill /T /F /PID when a timeout occurs.
// Do not set cmd.Cancel here because callers using exec.Command (rather than
// exec.CommandContext) manage timeouts via explicit select + KillProcessTree(cmd).
func ConfigureSubprocessGroup(cmd *exec.Cmd) {
	_ = cmd
}

// KillProcessTree terminates a process and all of its descendant processes on Windows.
func KillProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid > 0 {
		killCmd := exec.Command("taskkill.exe", "/T", "/F", "/PID", fmt.Sprintf("%d", pid))
		_ = killCmd.Run()
	}
	_ = cmd.Process.Kill()
}
