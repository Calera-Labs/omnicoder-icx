// SPDX-License-Identifier: Apache-2.0
//go:build windows

package ui

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	modUser32                  = syscall.NewLazyDLL("user32.dll")
	procOpenClipboard          = modUser32.NewProc("OpenClipboard")
	procCloseClipboard         = modUser32.NewProc("CloseClipboard")
	procGetClipboardData       = modUser32.NewProc("GetClipboardData")
	procIsClipboardFormatAvail = modUser32.NewProc("IsClipboardFormatAvailable")

	procGlobalLock    = modKernel32.NewProc("GlobalLock")
	procGlobalUnlock  = modKernel32.NewProc("GlobalUnlock")
	procLstrlenW      = modKernel32.NewProc("lstrlenW")
	procLstrlenA      = modKernel32.NewProc("lstrlenA")
	procRtlMoveMemory = modKernel32.NewProc("RtlMoveMemory")
)

const (
	cfText        = 1
	cfUnicodeText = 13
)

func getClipboardOS() (string, error) {
	// Strict 64-bit Win32 handles: HWND = uintptr(0)
	r, _, err := procOpenClipboard.Call(uintptr(0))
	if r == 0 {
		return "", fmt.Errorf("ui: cannot open clipboard: %w", err)
	}
	defer procCloseClipboard.Call()

	avail, _, _ := procIsClipboardFormatAvail.Call(uintptr(cfUnicodeText))
	if avail != 0 {
		hMem, _, err := procGetClipboardData.Call(uintptr(cfUnicodeText))
		if hMem != 0 {
			pData, _, err := procGlobalLock.Call(hMem)
			if pData != 0 {
				defer procGlobalUnlock.Call(hMem)

				length, _, _ := procLstrlenW.Call(pData)
				if length == 0 {
					return "", nil
				}
				buf := make([]uint16, length)
				procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), pData, length*2)
				return strings.ReplaceAll(string(utf16.Decode(buf)), "\r\n", "\n"), nil
			}
			return "", fmt.Errorf("ui: failed to lock unicode clipboard data: %w", err)
		}
		return "", fmt.Errorf("ui: failed to get unicode clipboard handle: %w", err)
	}

	// Fallback to ANSI text format (CF_TEXT)
	availANSI, _, _ := procIsClipboardFormatAvail.Call(uintptr(cfText))
	if availANSI != 0 {
		hMem, _, err := procGetClipboardData.Call(uintptr(cfText))
		if hMem != 0 {
			pData, _, err := procGlobalLock.Call(hMem)
			if pData != 0 {
				defer procGlobalUnlock.Call(hMem)

				length, _, _ := procLstrlenA.Call(pData)
				if length == 0 {
					return "", nil
				}
				buf := make([]byte, length)
				procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), pData, length)
				return strings.ReplaceAll(string(buf), "\r\n", "\n"), nil
			}
			return "", fmt.Errorf("ui: failed to lock ANSI clipboard data: %w", err)
		}
		return "", fmt.Errorf("ui: failed to get ANSI clipboard handle: %w", err)
	}

	return "", errors.New("ui: no text format available in clipboard")
}
