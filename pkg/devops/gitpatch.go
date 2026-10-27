// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// gitpatch.go implements unified diff parsing, atomic in-memory patch application,
// pre-patch SHA-256 tree validation, and CAS Merkle seal attestation via pkg/canonical.

package devops

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
)

// Hunk represents a single unified diff hunk (@@ -o,l +n,l @@).
type Hunk struct {
	OldStart int      `json:"old_start"`
	OldCount int      `json:"old_count"`
	NewStart int      `json:"new_start"`
	NewCount int      `json:"new_count"`
	Heading  string   `json:"heading,omitempty"`
	Lines    []string `json:"lines"`
}

// ParsedFileDiff represents the parsed diff for a single file.
type ParsedFileDiff struct {
	OldPath      string `json:"old_path"`
	NewPath      string `json:"new_path"`
	Hunks        []Hunk `json:"hunks"`
	LinesAdded   int    `json:"lines_added"`
	LinesRemoved int    `json:"lines_removed"`
}

// GitPatchRequest encapsulates atomic patch parameters.
type GitPatchRequest struct {
	RepoPath   string `json:"repo_path"`
	DiffHunk   string `json:"diff_hunk"`
	TargetFile string `json:"target_file,omitempty"`
	Message    string `json:"message,omitempty"`
	Author     string `json:"author,omitempty"`
	DryRun     bool   `json:"dry_run,omitempty"`
}

// GitPatchResult reports the outcome of applying or verifying an atomic patch.
type GitPatchResult struct {
	Status          string           `json:"status"` // "APPLIED", "VERIFIED", "REJECTED"
	PatchID         string           `json:"patch_id"`
	CASHash         string           `json:"cas_hash"`
	TargetFile      string           `json:"target_file,omitempty"`
	LinesAdded      int              `json:"lines_added"`
	LinesRemoved    int              `json:"lines_removed"`
	HunksCount      int              `json:"hunks_count"`
	MerkleSeal      string           `json:"merkle_seal"`
	OriginalTreeSHA string           `json:"original_tree_sha,omitempty"`
	NewTreeSHA      string           `json:"new_tree_sha,omitempty"`
	Applied         bool             `json:"applied"`
	Diffs           []ParsedFileDiff `json:"diffs,omitempty"`
	ErrorMessage    string           `json:"error_message,omitempty"`
}

var hunkHeaderRegex = regexp.MustCompile(`^@@\s+-(\d+)(?:,(\d+))?\s+\+(\d+)(?:,(\d+))?\s+@@(.*)$`)

// ParseUnifiedDiff parses unified diff text into structured file diffs and hunks.
func ParseUnifiedDiff(diffText string) ([]ParsedFileDiff, error) {
	diffText = canonical.NormalizeLF(diffText)
	if strings.TrimSpace(diffText) == "" {
		return nil, fmt.Errorf("EMPTY_DIFF: unified diff text is empty")
	}

	scanner := bufio.NewScanner(strings.NewReader(diffText))
	var diffs []ParsedFileDiff
	var curDiff *ParsedFileDiff
	var curHunk *Hunk

	flushHunk := func() {
		if curDiff != nil && curHunk != nil {
			curDiff.Hunks = append(curDiff.Hunks, *curHunk)
			curHunk = nil
		}
	}

	flushDiff := func() {
		flushHunk()
		if curDiff != nil {
			diffs = append(diffs, *curDiff)
			curDiff = nil
		}
	}

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "diff --git ") {
			flushDiff()
			parts := strings.Fields(line)
			oldP, newP := "", ""
			if len(parts) >= 4 {
				oldP = strings.TrimPrefix(parts[2], "a/")
				newP = strings.TrimPrefix(parts[3], "b/")
			}
			curDiff = &ParsedFileDiff{
				OldPath: oldP,
				NewPath: newP,
			}
			continue
		}

		if strings.HasPrefix(line, "--- ") {
			p := strings.TrimPrefix(line, "--- ")
			p = strings.TrimPrefix(p, "a/")
			p = strings.Fields(p)[0]
			if curDiff == nil {
				curDiff = &ParsedFileDiff{OldPath: p}
			} else if curDiff.OldPath == "" {
				curDiff.OldPath = p
			}
			continue
		}

		if strings.HasPrefix(line, "+++ ") {
			p := strings.TrimPrefix(line, "+++ ")
			p = strings.TrimPrefix(p, "b/")
			p = strings.Fields(p)[0]
			if curDiff == nil {
				curDiff = &ParsedFileDiff{NewPath: p}
			} else {
				curDiff.NewPath = p
			}
			continue
		}

		if strings.HasPrefix(line, "@@ ") {
			flushHunk()
			if curDiff == nil {
				curDiff = &ParsedFileDiff{}
			}

			matches := hunkHeaderRegex.FindStringSubmatch(line)
			if len(matches) < 4 {
				return nil, fmt.Errorf("MALFORMED_HUNK_HEADER: '%s'", line)
			}

			oldStart, _ := strconv.Atoi(matches[1])
			oldCount := 1
			if matches[2] != "" {
				oldCount, _ = strconv.Atoi(matches[2])
			}

			newStart, _ := strconv.Atoi(matches[3])
			newCount := 1
			if matches[4] != "" {
				newCount, _ = strconv.Atoi(matches[4])
			}

			heading := ""
			if len(matches) > 5 {
				heading = strings.TrimSpace(matches[5])
			}

			curHunk = &Hunk{
				OldStart: oldStart,
				OldCount: oldCount,
				NewStart: newStart,
				NewCount: newCount,
				Heading:  heading,
				Lines:    []string{},
			}
			continue
		}

		if curHunk != nil {
			if len(line) == 0 {
				line = " "
			}
			prefix := line[0]
			switch prefix {
			case '+':
				curDiff.LinesAdded++
				curHunk.Lines = append(curHunk.Lines, line)
			case '-':
				curDiff.LinesRemoved++
				curHunk.Lines = append(curHunk.Lines, line)
			case ' ':
				curHunk.Lines = append(curHunk.Lines, line)
			case '\\':
				// "\ No newline at end of file" marker
				curHunk.Lines = append(curHunk.Lines, line)
			default:
				// Treat leading unrecognized lines as context
				curHunk.Lines = append(curHunk.Lines, " "+line)
			}
		}
	}

	flushDiff()

	if len(diffs) == 0 {
		return nil, fmt.Errorf("NO_DIFFS_FOUND: could not extract valid diff hunks")
	}

	return diffs, scanner.Err()
}

// ApplyPatchToText applies a unified diff hunk to an in-memory string.
func ApplyPatchToText(originalText string, diffHunk string) (string, int, int, error) {
	originalText = canonical.NormalizeLF(originalText)
	diffHunk = canonical.NormalizeLF(diffHunk)
	diffs, err := ParseUnifiedDiff(diffHunk)
	if err != nil {
		return "", 0, 0, err
	}
	if len(diffs) == 0 || len(diffs[0].Hunks) == 0 {
		return "", 0, 0, fmt.Errorf("NO_HUNKS: diff contains no applicable hunks")
	}

	var origLines []string
	if originalText != "" {
		origLines = strings.Split(originalText, "\n")
	}

	fd := diffs[0]

	// Sort hunks by OldStart in ascending order to prevent out-of-order desynchronization
	hunks := make([]Hunk, len(fd.Hunks))
	copy(hunks, fd.Hunks)
	sort.SliceStable(hunks, func(i, j int) bool {
		return hunks[i].OldStart < hunks[j].OldStart
	})

	var resultLines []string
	origIdx := 0

	for _, h := range hunks {
		var targetLine int
		if len(origLines) == 0 {
			// New file creation or empty original text
			if h.OldStart < 0 || h.OldStart > 1 || h.OldCount > 0 {
				return "", 0, 0, fmt.Errorf("HUNK_OUT_OF_BOUNDS: hunk target line %d exceeds empty file length", h.OldStart)
			}
			targetLine = 0
		} else if h.OldCount == 0 {
			// Pure insertion after line h.OldStart
			if h.OldStart < 0 || h.OldStart > len(origLines) {
				return "", 0, 0, fmt.Errorf("HUNK_OUT_OF_BOUNDS: hunk target line %d exceeds file length %d", h.OldStart, len(origLines))
			}
			targetLine = h.OldStart
		} else {
			// Standard modification/deletion of existing lines
			if h.OldStart < 1 || h.OldStart > len(origLines) {
				return "", 0, 0, fmt.Errorf("HUNK_OUT_OF_BOUNDS: hunk target line %d exceeds file length %d", h.OldStart, len(origLines))
			}
			targetLine = h.OldStart - 1
		}

		if targetLine < origIdx {
			return "", 0, 0, fmt.Errorf("HUNK_OVERLAP: hunk target line %d is before current line %d", h.OldStart, origIdx+1)
		}

		// Copy untouched lines before hunk
		for origIdx < targetLine && origIdx < len(origLines) {
			resultLines = append(resultLines, origLines[origIdx])
			origIdx++
		}

		for _, l := range h.Lines {
			if strings.HasPrefix(l, "\\") {
				continue
			}
			if len(l) == 0 {
				continue
			}
			marker := l[0]
			content := ""
			if len(l) > 1 {
				content = strings.TrimRight(l[1:], "\r")
			}

			switch marker {
			case ' ':
				// Context line verification: must match origLines[origIdx]
				if origIdx >= len(origLines) {
					return "", 0, 0, fmt.Errorf("PATCH_CONTEXT_MISMATCH: expected context '%s' at line %d, but reached EOF", content, origIdx+1)
				}
				if strings.TrimRight(origLines[origIdx], " \t\r") != strings.TrimRight(content, " \t\r") {
					return "", 0, 0, fmt.Errorf("PATCH_CONTEXT_MISMATCH: hunk context line '%s' did not match file content '%s' at line %d", content, origLines[origIdx], origIdx+1)
				}
				resultLines = append(resultLines, origLines[origIdx])
				origIdx++
			case '-':
				// Deletion line verification: must match origLines[origIdx]
				if origIdx >= len(origLines) {
					return "", 0, 0, fmt.Errorf("PATCH_DELETION_MISMATCH: expected to delete '%s' at line %d, but reached EOF", content, origIdx+1)
				}
				if strings.TrimRight(origLines[origIdx], " \t\r") != strings.TrimRight(content, " \t\r") {
					return "", 0, 0, fmt.Errorf("PATCH_DELETION_MISMATCH: expected to delete '%s', found '%s' at line %d", content, origLines[origIdx], origIdx+1)
				}
				origIdx++
			case '+':
				// Addition line
				resultLines = append(resultLines, content)
			}
		}
	}

	// Copy remaining untouched lines after last hunk
	for origIdx < len(origLines) {
		resultLines = append(resultLines, origLines[origIdx])
		origIdx++
	}

	return strings.Join(resultLines, "\n"), fd.LinesAdded, fd.LinesRemoved, nil
}

// ApplyPatch validates and applies an atomic patch against a repository/workspace.
func ApplyPatch(req GitPatchRequest) (*GitPatchResult, error) {
	diffs, err := ParseUnifiedDiff(req.DiffHunk)
	if err != nil {
		return &GitPatchResult{
			Status:       "REJECTED",
			ErrorMessage: err.Error(),
		}, err
	}

	casBytes := canonical.SHA256Bytes([]byte(req.DiffHunk + "|" + req.Message + "|" + req.Author))
	casHash := casBytes
	patchID := fmt.Sprintf("patch_%s", casHash[:12])

	totAdded := 0
	totRemoved := 0
	totHunks := 0
	targetFile := req.TargetFile

	for _, d := range diffs {
		totAdded += d.LinesAdded
		totRemoved += d.LinesRemoved
		totHunks += len(d.Hunks)
		if targetFile == "" {
			if d.NewPath != "" && d.NewPath != "/dev/null" {
				targetFile = d.NewPath
			} else if d.OldPath != "" && d.OldPath != "/dev/null" {
				targetFile = d.OldPath
			}
		}
	}

	repoPath := req.RepoPath
	if repoPath == "" {
		repoPath = "."
	}

	var fullTargetPath string
	if targetFile != "" {
		if filepath.IsAbs(targetFile) {
			fullTargetPath = targetFile
		} else {
			fullTargetPath = filepath.Join(repoPath, targetFile)
		}
	}

	origTreeSHA := ""
	newTreeSHA := ""
	var originalContent string

	if fullTargetPath != "" {
		if b, err := os.ReadFile(fullTargetPath); err == nil {
			originalContent = canonical.NormalizeLF(string(b))
			origTreeSHA = canonical.SHA256Bytes([]byte(originalContent))
		}
	}

	patchedText, _, _, applyErr := ApplyPatchToText(originalContent, req.DiffHunk)
	if applyErr != nil {
		return &GitPatchResult{
			Status:       "REJECTED",
			PatchID:      patchID,
			CASHash:      casHash,
			TargetFile:   targetFile,
			ErrorMessage: applyErr.Error(),
		}, applyErr
	}

	newTreeSHA = canonical.SHA256Bytes([]byte(patchedText))

	// RFC 8785 Canonical Merkle Seal Attestation via pkg/canonical
	merkleSeal := canonical.SHA256Of(map[string]any{
		"patch_id":          patchID,
		"cas_hash":          casHash,
		"target_file":       filepath.ToSlash(targetFile),
		"original_tree_sha": origTreeSHA,
		"new_tree_sha":      newTreeSHA,
		"lines_added":       totAdded,
		"lines_removed":     totRemoved,
	})

	applied := false
	if !req.DryRun && fullTargetPath != "" {
		if err := os.MkdirAll(filepath.Dir(fullTargetPath), 0755); err == nil {
			if err := os.WriteFile(fullTargetPath, []byte(patchedText), 0644); err == nil {
				applied = true
			} else {
				return &GitPatchResult{
					Status:       "REJECTED",
					ErrorMessage: fmt.Sprintf("WRITE_FAILED: %v", err),
				}, err
			}
		}
	}

	status := "VERIFIED"
	if applied {
		status = "APPLIED"
	}

	return &GitPatchResult{
		Status:          status,
		PatchID:         patchID,
		CASHash:         casHash,
		TargetFile:      targetFile,
		LinesAdded:      totAdded,
		LinesRemoved:    totRemoved,
		HunksCount:      totHunks,
		MerkleSeal:      merkleSeal,
		OriginalTreeSHA: origTreeSHA,
		NewTreeSHA:      newTreeSHA,
		Applied:         applied,
		Diffs:           diffs,
	}, nil
}

// GenerateUnifiedDiff generates a standard unified diff string between oldText and newText.
func GenerateUnifiedDiff(oldPath, newPath, oldText, newText string) string {
	if oldText == newText {
		return ""
	}

	var oldLines, newLines []string
	if oldText != "" {
		oldLines = strings.Split(oldText, "\n")
	}
	if newText != "" {
		newLines = strings.Split(newText, "\n")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- a/%s\n", oldPath))
	sb.WriteString(fmt.Sprintf("+++ b/%s\n", newPath))
	sb.WriteString(fmt.Sprintf("@@ -1,%d +1,%d @@\n", len(oldLines), len(newLines)))

	for _, l := range oldLines {
		sb.WriteString("-" + l + "\n")
	}
	for _, l := range newLines {
		sb.WriteString("+" + l + "\n")
	}

	return sb.String()
}
