// SPDX-License-Identifier: Apache-2.0
//go:build !windows

package vm

import (
	"os/exec"
	"syscall"
)

// ConfigureSubprocessGroup isolates a subprocess into its own POSIX process group
// so that KillProcessTree can terminate the entire descendant tree (including
// grandchildren spawned by sh -c / bash -c) without leaving orphan processes
// holding stdout/stderr pipes open.
// Note: Do not set cmd.Cancel here because callers using exec.Command (rather than
// exec.CommandContext) manage timeouts via explicit select + KillProcessTree(cmd),
// and Go's os/exec rejects cmd.Start() when Cancel != nil on a non-Context Cmd.
func ConfigureSubprocessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillProcessTree terminates both the process group (-pid) and the leader process.
func KillProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	_ = cmd.Process.Kill()
}
