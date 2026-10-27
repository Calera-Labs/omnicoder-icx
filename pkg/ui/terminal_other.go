// SPDX-License-Identifier: Apache-2.0
//go:build !windows

package ui

import (
	"io"
	"os"
)

func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	if (fi.Mode() & os.ModeCharDevice) == 0 {
		return false
	}
	if nullFi, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, nullFi) {
		return false
	}
	return true
}

func hasPendingConsoleEvents() bool {
	return false
}

// SetupTerminalModes is a no-op fallback on non-Windows platforms.
func SetupTerminalModes(in io.Reader, out io.Writer) func() {
	return func() {}
}
