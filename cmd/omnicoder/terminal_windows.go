//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func isTerminalFile(f *os.File) bool {
	mod := syscall.NewLazyDLL("kernel32.dll")
	proc := mod.NewProc("GetConsoleMode")
	var mode uint32
	r1, _, _ := proc.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return r1 != 0
}
