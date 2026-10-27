// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// resolveSecurePath validates that reqPath resolves within workspaceDir and cannot escape.
func resolveSecurePath(workspaceDir, reqPath string) (string, error) {
	if workspaceDir == "" {
		return "", errors.New("workspace directory cannot be empty")
	}
	if reqPath == "" {
		return "", errors.New("requested path cannot be empty")
	}

	// 1. Poison Null Byte Check & Traversal Sequence Check
	if strings.ContainsRune(reqPath, 0) {
		return "", errors.New("path contains null byte")
	}
	if strings.Contains(reqPath, "..") {
		return "", errors.New("path contains traversal sequence '..'")
	}

	// 2. Reject UNC network paths
	if strings.HasPrefix(reqPath, `\\`) || strings.HasPrefix(reqPath, `//`) {
		return "", errors.New("UNC network paths not allowed")
	}

	// 3. Reject Windows Alternate Data Streams (":")
	vol := filepath.VolumeName(reqPath)
	body := reqPath[len(vol):]
	if strings.Contains(body, ":") {
		return "", errors.New("alternate data streams not allowed")
	}

	// 3b. Reject Windows Reserved Device Names
	cleanReq := filepath.Clean(reqPath)
	parts := strings.FieldsFunc(cleanReq, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	for _, part := range parts {
		upperPart := strings.ToUpper(part)
		partExt := filepath.Ext(upperPart)
		partNoExt := strings.TrimSuffix(upperPart, partExt)
		if windowsReservedNames[upperPart] || windowsReservedNames[partNoExt] {
			return "", fmt.Errorf("Windows reserved device name '%s' not allowed", part)
		}
	}

	// 4. Multi-pass URL-Encoded Traversal Detection
	curr := reqPath
	for i := 0; i < 5; i++ {
		if !strings.Contains(curr, "%") {
			break
		}
		unescaped, err := url.PathUnescape(curr)
		if err != nil {
			return "", fmt.Errorf("malformed URL-encoded path (%s): %v", reqPath, err)
		}
		if unescaped == curr {
			break
		}
		curr = unescaped
		if strings.ContainsRune(curr, 0) || strings.Contains(curr, "..") {
			return "", fmt.Errorf("URL-encoded traversal detected (%s)", reqPath)
		}
	}

	// 5. Canonicalize workspace root
	absWorkDir, err := filepath.Abs(filepath.Clean(workspaceDir))
	if err != nil {
		return "", fmt.Errorf("invalid workspace dir: %v", err)
	}
	realWorkDir, err := filepath.EvalSymlinks(absWorkDir)
	if err != nil {
		realWorkDir = absWorkDir
	}

	// 6. Lexical candidate construction
	nativeReq := filepath.FromSlash(reqPath)
	var candidate string
	if filepath.IsAbs(nativeReq) || isExplicitRootPath(nativeReq) {
		candidate = filepath.Clean(nativeReq)
		if isPathDescendantOrEqual(candidate, absWorkDir) {
			if rel, errRel := filepath.Rel(absWorkDir, candidate); errRel == nil {
				candidate = filepath.Clean(filepath.Join(realWorkDir, rel))
			}
		}
	} else {
		candidate = filepath.Clean(filepath.Join(realWorkDir, nativeReq))
	}

	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}

	// 7. Lexical boundary verification
	if !isPathDescendantOrEqual(absCandidate, realWorkDir) {
		return "", fmt.Errorf("path '%s' escapes workspace boundary '%s'", reqPath, realWorkDir)
	}

	// 8. Physical symlink verification
	realTarget, err := filepath.EvalSymlinks(absCandidate)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
			return absCandidate, nil
		}
		return "", fmt.Errorf("broken symlink or inaccessible: %v", err)
	}

	if !isPathDescendantOrEqual(realTarget, realWorkDir) {
		return "", fmt.Errorf("symlink target '%s' escapes workspace boundary '%s'", realTarget, realWorkDir)
	}

	return absCandidate, nil
}

func isExplicitRootPath(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return true
	}
	if len(p) >= 2 && p[1] == ':' {
		return true
	}
	return false
}

func isPathDescendantOrEqual(child, parent string) bool {
	cleanChild := filepath.Clean(child)
	cleanParent := filepath.Clean(parent)

	if cleanChild == cleanParent {
		return true
	}

	if runtime.GOOS == "windows" {
		targetLower := strings.ToLower(cleanChild)
		baseLower := strings.ToLower(cleanParent)
		rel, err := filepath.Rel(baseLower, targetLower)
		if err != nil {
			return false
		}
		if rel == "." {
			return true
		}
		return !strings.HasPrefix(rel, "..") && !strings.HasPrefix(rel, string(filepath.Separator)) && !filepath.IsAbs(rel)
	}

	rel, err := filepath.Rel(cleanParent, cleanChild)
	if err != nil {
		return false
	}

	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
