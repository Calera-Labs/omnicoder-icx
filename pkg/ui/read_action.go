// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	readActionMaxLines = 400
	readActionMaxBytes = 24 * 1024
	// readActionMaxTurns caps consecutive READ-only feedback turns per user request.
	readActionMaxTurns = 16
)

// resolveReadPath maps a model-supplied path to an existing file inside the workspace.
// It tries the path relative to the workspace root first, then relative to the active project.
func resolveReadPath(ws, activeProject, raw string) (string, error) {
	p := strings.Trim(strings.TrimSpace(raw), "\"'`")
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	p = filepath.FromSlash(p)
	wsAbs, err := filepath.Abs(ws)
	if err != nil {
		return "", err
	}

	var candidates []string
	if filepath.IsAbs(p) {
		candidates = append(candidates, p)
	} else {
		candidates = append(candidates, filepath.Join(wsAbs, p))
		if activeProject != "" && activeProject != "." {
			candidates = append(candidates, filepath.Join(wsAbs, filepath.FromSlash(activeProject), p))
		}
	}

	for _, c := range candidates {
		c = filepath.Clean(c)
		rel, relErr := filepath.Rel(wsAbs, c)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue // never read outside the workspace
		}
		if info, statErr := os.Stat(c); statErr == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("file not found inside workspace: %s", raw)
}

// parseReadRange parses "start-end", "start:end" or "start" into 1-based inclusive bounds.
func parseReadRange(spec string) (int, int) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 1, readActionMaxLines
	}
	spec = strings.NewReplacer(":", "-", "..", "-", ",", "-").Replace(spec)
	parts := strings.SplitN(spec, "-", 2)
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || start < 1 {
		start = 1
	}
	end := start + readActionMaxLines - 1
	if len(parts) == 2 {
		if e, eErr := strconv.Atoi(strings.TrimSpace(parts[1])); eErr == nil && e >= start {
			end = e
		}
	}
	if end-start+1 > readActionMaxLines {
		end = start + readActionMaxLines - 1
	}
	return start, end
}

// readFileForArchitect returns a line-numbered excerpt of a workspace file suitable for
// embedding directly in the architect's next prompt, so the model really sees the content.
func readFileForArchitect(ws, activeProject string, action *ActionIntent) string {
	raw := action.Path()
	full, err := resolveReadPath(ws, activeProject, raw)
	if err != nil {
		return fmt.Sprintf("--- READ %s ---\n[error] %v\n", raw, err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return fmt.Sprintf("--- READ %s ---\n[error] %v\n", raw, err)
	}
	if strings.IndexByte(string(data[:min(len(data), 4096)]), 0) >= 0 {
		return fmt.Sprintf("--- READ %s ---\n[error] binary file, not displayed\n", raw)
	}

	spec := action.Attributes["lines"]
	if spec == "" {
		spec = action.Attributes["range"]
	}
	start, end := parseReadRange(spec)

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	total := len(lines)
	if start > total {
		return fmt.Sprintf("--- READ %s (%d lines) ---\n[error] start line %d is beyond end of file\n", raw, total, start)
	}
	if end > total {
		end = total
	}

	var sb strings.Builder
	rel, _ := filepath.Rel(ws, full)
	fmt.Fprintf(&sb, "--- READ %s (lines %d-%d of %d) ---\n", filepath.ToSlash(rel), start, end, total)
	for i := start; i <= end; i++ {
		line := fmt.Sprintf("%d: %s\n", i, lines[i-1])
		if sb.Len()+len(line) > readActionMaxBytes {
			fmt.Fprintf(&sb, "[truncated at %d bytes; request lines=\"%d-%d\" to continue]\n", readActionMaxBytes, i, i+readActionMaxLines-1)
			return sb.String()
		}
		sb.WriteString(line)
	}
	if end < total {
		fmt.Fprintf(&sb, "[%d more lines; request lines=\"%d-%d\" to continue]\n", total-end, end+1, end+readActionMaxLines)
	}
	return sb.String()
}

const readIngestMaxBytes = 512 * 1024

// ingestReadFileIntoICX crystallizes a file the architect just read into the ICX memory space so
// pre-existing project code becomes recallable (not just files OmniCoder wrote). CrystallizeFile is
// content-hash idempotent, so re-reading an unchanged file costs nothing.
func ingestReadFileIntoICX(ctx context.Context, session *Session, ws string, action *ActionIntent) {
	if session == nil || action == nil {
		return
	}
	bs := session.BuildSession()
	if bs == nil {
		return
	}
	full, err := resolveReadPath(ws, session.ActiveProject(), action.Path())
	if err != nil {
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.Size() == 0 || info.Size() > readIngestMaxBytes {
		return
	}
	data, err := os.ReadFile(full)
	if err != nil || bytes.IndexByte(data[:min(len(data), 4096)], 0) >= 0 {
		return
	}
	rel, err := filepath.Rel(ws, full)
	if err != nil {
		return
	}
	_, _ = bs.CrystallizeFile(ctx, filepath.ToSlash(rel), string(data), "")
}
