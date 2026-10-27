// SPDX-License-Identifier: Apache-2.0
//go:build windows

package ui

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

var (
	modKernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode                = modKernel32.NewProc("GetConsoleMode")
	procSetConsoleMode                = modKernel32.NewProc("SetConsoleMode")
	procGetNumberOfConsoleInputEvents = modKernel32.NewProc("GetNumberOfConsoleInputEvents")
)

const (
	enableVirtualTerminalProcessing = 0x0004
	enableVirtualTerminalInput      = 0x0200
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
	var mode uint32
	r1, _, _ := procGetConsoleMode.Call(uintptr(f.Fd()), uintptr(unsafe.Pointer(&mode)))
	return r1 != 0
}

func hasPendingConsoleEvents() bool {
	var count uint32
	r1, _, _ := procGetNumberOfConsoleInputEvents.Call(uintptr(os.Stdin.Fd()), uintptr(unsafe.Pointer(&count)))
	return r1 != 0 && count > 0
}

// SetupTerminalModes configures Windows console handles for virtual terminal input and output.
func SetupTerminalModes(in io.Reader, out io.Writer) func() {
	var cleanups []func()

	if inF, ok := in.(*os.File); ok && isTerminalFile(inF) {
		var oldInMode uint32
		r, _, _ := procGetConsoleMode.Call(uintptr(inF.Fd()), uintptr(unsafe.Pointer(&oldInMode)))
		if r != 0 {
			newMode := oldInMode | enableVirtualTerminalInput
			procSetConsoleMode.Call(uintptr(inF.Fd()), uintptr(newMode))
			cleanups = append(cleanups, func() {
				procSetConsoleMode.Call(uintptr(inF.Fd()), uintptr(oldInMode))
			})
		}
	}

	if outF, ok := out.(*os.File); ok && isTerminalFile(outF) {
		var oldOutMode uint32
		r, _, _ := procGetConsoleMode.Call(uintptr(outF.Fd()), uintptr(unsafe.Pointer(&oldOutMode)))
		if r != 0 {
			newMode := oldOutMode | enableVirtualTerminalProcessing
			procSetConsoleMode.Call(uintptr(outF.Fd()), uintptr(newMode))
			cleanups = append(cleanups, func() {
				procSetConsoleMode.Call(uintptr(outF.Fd()), uintptr(oldOutMode))
			})
		}
	}

	return func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}
