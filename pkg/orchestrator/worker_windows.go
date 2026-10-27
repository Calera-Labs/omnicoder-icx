// SPDX-License-Identifier: Apache-2.0
//go:build windows

package orchestrator

import (
	"fmt"
	"os/exec"
	"strings"
)

func killSubprocessTree(pid int) {
	if pid <= 0 {
		return
	}
	killCmd := exec.Command("taskkill.exe", "/F", "/T", "/PID", fmt.Sprintf("%d", pid))
	_ = killCmd.Run()
}

func isProcessRunningOS(pid int) bool {
	if pid <= 0 {
		return false
	}
	cmd := exec.Command("tasklist.exe", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), fmt.Sprintf("\"%d\"", pid))
}
