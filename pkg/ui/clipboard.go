// SPDX-License-Identifier: Apache-2.0

package ui

// GetClipboardText reads textual content from the operating system clipboard across platforms.
func GetClipboardText() (string, error) {
	return getClipboardOS()
}
