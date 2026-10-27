// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// markdown.go implements atomic Markdown document surgery primitives (checklist toggling,
// section replacement) so AI agents don't have to read 600-line files in slices or calculate offsets.

package devops

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ChecklistToggle defines a target checklist item to update.
type ChecklistToggle struct {
	Pattern string `json:"pattern"`        // Substring or regex to match within the checklist line
	Checked bool   `json:"checked"`        // true for [x], false for [ ]
	Note    string `json:"note,omitempty"` // Optional evidence or timestamp to append
}

// ChecklistResult reports the outcome of a checklist toggle operation.
type ChecklistResult struct {
	Path         string   `json:"path"`
	MatchedCount int      `json:"matched_count"`
	UpdatedItems []string `json:"updated_items"`
	Status       string   `json:"status"`
}

// SectionResult reports the outcome of a section update operation.
type SectionResult struct {
	Path         string `json:"path"`
	Heading      string `json:"heading"`
	BytesWritten int    `json:"bytes_written"`
	Status       string `json:"status"`
}

var checklistRegex = regexp.MustCompile(`^(\s*[-*]\s+\[)([ xX])(\]\s+)(.*)$`)
var headingRegex = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)

// ToggleChecklist atomically toggles markdown checklist items matching specified patterns.
func ToggleChecklist(workspaceDir, relPath string, items []ChecklistToggle) (*ChecklistResult, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("at least one checklist toggle item is required")
	}

	fullPath := filepath.Join(workspaceDir, filepath.FromSlash(relPath))
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	rawContent := string(data)
	isCRLF := strings.Contains(rawContent, "\r\n")
	normalized := strings.ReplaceAll(rawContent, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	res := &ChecklistResult{
		Path:   relPath,
		Status: "UNCHANGED",
	}

	// Prepare compiled patterns
	type compiledToggle struct {
		re      *regexp.Regexp
		sub     string
		checked bool
		note    string
	}
	var compiled []compiledToggle
	for _, it := range items {
		clean := strings.TrimSpace(it.Pattern)
		if clean == "" {
			continue
		}
		ct := compiledToggle{
			sub:     strings.ToLower(clean),
			checked: it.Checked,
			note:    strings.TrimSpace(it.Note),
		}
		if r, err := regexp.Compile("(?i)" + regexp.QuoteMeta(clean)); err == nil {
			ct.re = r
		}
		compiled = append(compiled, ct)
	}

	modified := false
	for i, line := range lines {
		m := checklistRegex.FindStringSubmatch(line)
		if len(m) != 5 {
			continue
		}
		prefix := m[1]
		checkChar := m[2]
		suffix := m[3]
		text := m[4]

		lowerText := strings.ToLower(text)
		for _, ct := range compiled {
			matched := false
			if ct.re != nil && ct.re.MatchString(text) {
				matched = true
			} else if strings.Contains(lowerText, ct.sub) {
				matched = true
			}

			if matched {
				targetChar := " "
				if ct.checked {
					targetChar = "x"
				}
				newText := text
				if ct.note != "" && !strings.Contains(text, ct.note) {
					newText = fmt.Sprintf("%s (%s)", strings.TrimSpace(text), ct.note)
				}

				if checkChar != targetChar || newText != text {
					lines[i] = prefix + targetChar + suffix + newText
					res.MatchedCount++
					res.UpdatedItems = append(res.UpdatedItems, strings.TrimSpace(lines[i]))
					modified = true
				}
				break
			}
		}
	}

	if modified {
		sep := "\n"
		if isCRLF {
			sep = "\r\n"
		}
		newPayload := strings.Join(lines, sep)
		if err := atomicWriteFile(fullPath, []byte(newPayload)); err != nil {
			return nil, err
		}
		res.Status = "UPDATED"
	}

	return res, nil
}

// UpdateSection replaces an entire Markdown section under a given heading without disturbing
// the rest of the document.
func UpdateSection(workspaceDir, relPath, heading, newContent string) (*SectionResult, error) {
	if strings.TrimSpace(heading) == "" {
		return nil, fmt.Errorf("heading is required")
	}

	fullPath := filepath.Join(workspaceDir, filepath.FromSlash(relPath))
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	rawContent := string(data)
	isCRLF := strings.Contains(rawContent, "\r\n")
	normalized := strings.ReplaceAll(rawContent, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	cleanHeading := strings.ToLower(strings.TrimSpace(strings.TrimLeft(heading, "# ")))
	targetHeadingIndex := -1
	targetHeadingLevel := 0

	for i, line := range lines {
		m := headingRegex.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) == 3 {
			level := len(m[1])
			title := strings.ToLower(strings.TrimSpace(m[2]))
			if strings.Contains(title, cleanHeading) || strings.Contains(cleanHeading, title) {
				targetHeadingIndex = i
				targetHeadingLevel = level
				break
			}
		}
	}

	if targetHeadingIndex == -1 {
		return nil, fmt.Errorf("heading %q not found in document %s", heading, relPath)
	}

	// Find the end of this section (next heading with level <= targetHeadingLevel)
	sectionEndIndex := len(lines)
	for i := targetHeadingIndex + 1; i < len(lines); i++ {
		m := headingRegex.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if len(m) == 3 {
			level := len(m[1])
			if level <= targetHeadingLevel {
				sectionEndIndex = i
				break
			}
		}
	}

	// Reconstruct content: lines[:targetHeadingIndex+1] + newContentLines + lines[sectionEndIndex:]
	newLines := strings.Split(strings.ReplaceAll(newContent, "\r\n", "\n"), "\n")
	var resultLines []string
	resultLines = append(resultLines, lines[:targetHeadingIndex+1]...)
	resultLines = append(resultLines, newLines...)
	if sectionEndIndex < len(lines) {
		resultLines = append(resultLines, lines[sectionEndIndex:]...)
	}

	sep := "\n"
	if isCRLF {
		sep = "\r\n"
	}
	outputData := []byte(strings.Join(resultLines, sep))
	if err := atomicWriteFile(fullPath, outputData); err != nil {
		return nil, err
	}

	return &SectionResult{
		Path:         relPath,
		Heading:      heading,
		BytesWritten: len(outputData),
		Status:       "UPDATED",
	}, nil
}

func atomicWriteFile(targetPath string, data []byte) error {
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(dir, ".tmp-md-*")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	// On Windows, target must be removed or renamed safely
	_ = os.Remove(targetPath)
	return os.Rename(tmpName, targetPath)
}
