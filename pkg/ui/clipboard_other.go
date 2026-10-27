// SPDX-License-Identifier: Apache-2.0
//go:build !windows

package ui

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

func getClipboardOS() (string, error) {
	if runtime.GOOS == "darwin" {
		return getClipboardDarwin()
	}
	return getClipboardLinux()
}

func getClipboardDarwin() (string, error) {
	cmd := exec.Command("pbpaste")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ui: pbpaste error: %w", err)
	}
	return strings.ReplaceAll(string(out), "\r\n", "\n"), nil
}

func getClipboardLinux() (string, error) {
	if _, err := exec.LookPath("wl-paste"); err == nil {
		cmd := exec.Command("wl-paste", "--no-newline")
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			return strings.ReplaceAll(string(out), "\r\n", "\n"), nil
		}
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		cmd := exec.Command("xclip", "-selection", "clipboard", "-o")
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			return strings.ReplaceAll(string(out), "\r\n", "\n"), nil
		}
	}
	if _, err := exec.LookPath("xsel"); err == nil {
		cmd := exec.Command("xsel", "--clipboard", "--output")
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			return strings.ReplaceAll(string(out), "\r\n", "\n"), nil
		}
	}
	return "", errors.New("ui: clipboard tool not available (wl-paste, xclip, or xsel required on Linux)")
}
