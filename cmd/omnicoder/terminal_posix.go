//go:build !windows

package main

import "os"

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
