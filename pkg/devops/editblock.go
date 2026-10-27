// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// editblock.go implements 4-tier fuzzy search/replace matching (exact, trimmed whitespace,
// indentation-relaxed, blank-line relaxed), transactional in-memory staging,
// and atomic disk rollback on failure with CAS Merkle attestation.

package devops

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
)

// EditBlock represents a single search/replace modification on a specific file.
type EditBlock struct {
	FilePath    string `json:"file_path"`
	SearchText  string `json:"search_text"`
	ReplaceText string `json:"replace_text"`
}

// EditBlockRequest encapsulates parameters for parsing and applying edit blocks.
type EditBlockRequest struct {
	RepoPath       string      `json:"repo_path"`
	EditBlocksText string      `json:"edit_blocks_text,omitempty"`
	Blocks         []EditBlock `json:"blocks,omitempty"`
	TargetFile     string      `json:"target_file,omitempty"`
	Message        string      `json:"message,omitempty"`
	Author         string      `json:"author,omitempty"`
	DryRun         bool        `json:"dry_run,omitempty"`
	FuzzyMatch     bool        `json:"fuzzy_match,omitempty"`
}

// EditBlockResult reports the outcome of an edit-block transaction with CAS provenance.
type EditBlockResult struct {
	Status        string      `json:"status"` // "APPLIED", "REJECTED"
	PatchID       string      `json:"patch_id"`
	CASHash       string      `json:"cas_hash"`
	MerkleSeal    string      `json:"merkle_seal"`
	FilesModified []string    `json:"files_modified"`
	BlocksApplied int         `json:"blocks_applied"`
	LinesAdded    int         `json:"lines_added"`
	LinesRemoved  int         `json:"lines_removed"`
	DiffSummary   string      `json:"diff_summary,omitempty"`
	ErrorMessage  string      `json:"error_message,omitempty"`
	Applied       bool        `json:"applied"`
	Blocks        []EditBlock `json:"blocks,omitempty"`
}

var (
	searchDividerRegex  = regexp.MustCompile(`^<{5,9}(?:\s*(?:SEARCH|HEAD|[\w\-./\\]+))?\s*$`)
	replaceDividerRegex = regexp.MustCompile(`^={5,9}\s*$`)
	endDividerRegex     = regexp.MustCompile(`^>{5,9}(?:\s*(?:REPLACE|[\w\-./\\]+))?\s*$`)
	filePathHeaderRegex = regexp.MustCompile(`^(?:[#*` + "`" + `\s]*)([\w\-./\\]+\.[a-zA-Z0-9_\-]+)(?:[:` + "`" + `\s]*)$`)
)

// ParseAiderEditBlocks parses standard Aider multi-hunk edit block format:
// <<<<<<< SEARCH
// ...
// =======
// ...
// >>>>>>> REPLACE
func ParseAiderEditBlocks(text string, defaultFile string) ([]EditBlock, error) {
	text = canonical.NormalizeLF(text)
	scanner := bufio.NewScanner(strings.NewReader(text))
	var blocks []EditBlock

	inSearch := false
	inReplace := false
	var currentFile string = defaultFile
	var searchLines []string
	var replaceLines []string
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		rawLine := scanner.Text()
		trimmed := strings.TrimSpace(rawLine)

		if !inSearch && !inReplace {
			// Check if this line designates a target file
			if matches := filePathHeaderRegex.FindStringSubmatch(trimmed); len(matches) > 1 {
				currentFile = filepath.ToSlash(matches[1])
				continue
			}
		}

		if searchDividerRegex.MatchString(trimmed) {
			if inSearch || inReplace {
				return nil, fmt.Errorf("PARSE_ERROR: unexpected <<<<<<< SEARCH at line %d while block is open", lineNum)
			}
			inSearch = true
			searchLines = nil
			continue
		}

		if replaceDividerRegex.MatchString(trimmed) {
			if !inSearch {
				return nil, fmt.Errorf("PARSE_ERROR: unexpected ======= at line %d without matching SEARCH divider", lineNum)
			}
			inSearch = false
			inReplace = true
			replaceLines = nil
			continue
		}

		if endDividerRegex.MatchString(trimmed) {
			if !inReplace {
				return nil, fmt.Errorf("PARSE_ERROR: unexpected >>>>>>> REPLACE at line %d without matching ======= divider", lineNum)
			}
			inReplace = false

			target := currentFile
			if target == "" {
				target = defaultFile
			}
			if target == "" {
				return nil, fmt.Errorf("PARSE_ERROR: missing file path for edit block terminating at line %d", lineNum)
			}

			blocks = append(blocks, EditBlock{
				FilePath:    filepath.ToSlash(target),
				SearchText:  strings.Join(searchLines, "\n"),
				ReplaceText: strings.Join(replaceLines, "\n"),
			})
			searchLines = nil
			replaceLines = nil
			continue
		}

		if inSearch {
			searchLines = append(searchLines, rawLine)
		} else if inReplace {
			replaceLines = append(replaceLines, rawLine)
		}
	}

	if inSearch || inReplace {
		return nil, fmt.Errorf("PARSE_ERROR: unclosed edit block at end of input")
	}

	if len(blocks) == 0 {
		return nil, fmt.Errorf("NO_BLOCKS_PARSED: no valid <<<<<<< SEARCH ... ======= ... >>>>>>> REPLACE blocks found")
	}

	return blocks, nil
}

// StripLineNumberPrefixes strips line number and focus marker prefixes (e.g. " 109: ", "> 113: ", "109 | ", "> ")
// from lines in search or replace blocks that an LLM may have copied from prompt context.
func StripLineNumberPrefixes(text string) string {
	if !strings.Contains(text, ":") && !strings.Contains(text, "|") && !strings.Contains(text, ">") {
		return text
	}
	lines := strings.Split(text, "\n")
	linePrefixRegex := regexp.MustCompile(`^(?:[\s>]*)\d+[:|]\s?(.*)$`)
	markerOnlyRegex := regexp.MustCompile(`^>\s+(.*)$`)

	hasAnyPrefix := false
	for _, l := range lines {
		if linePrefixRegex.MatchString(l) || markerOnlyRegex.MatchString(l) {
			hasAnyPrefix = true
			break
		}
	}
	if !hasAnyPrefix {
		return text
	}

	var stripped []string
	for _, l := range lines {
		if m := linePrefixRegex.FindStringSubmatch(l); len(m) > 1 {
			stripped = append(stripped, m[1])
		} else if m := markerOnlyRegex.FindStringSubmatch(l); len(m) > 1 {
			stripped = append(stripped, m[1])
		} else {
			stripped = append(stripped, l)
		}
	}
	return strings.Join(stripped, "\n")
}

// ReplaceFuzzy executes a 4-tier fuzzy match and replace:
// Tier 1: Exact string match
// Tier 2: Right-whitespace trimmed line-by-line comparison
// Tier 3: Indentation-relaxed comparison with candidate indentation preservation
// Tier 4: Blank-line relaxed comparison
func ReplaceFuzzy(content, searchText, replaceText string, allowFuzzy bool) (string, int, int, error) {
	// Normalize CRLF to LF for consistent comparison
	contentNorm := strings.ReplaceAll(content, "\r\n", "\n")
	searchNorm := StripLineNumberPrefixes(strings.ReplaceAll(searchText, "\r\n", "\n"))
	replaceNorm := StripLineNumberPrefixes(strings.ReplaceAll(replaceText, "\r\n", "\n"))

	isElisionLine := func(s string) bool {
		t := strings.TrimSpace(s)
		return strings.HasPrefix(t, "...") || strings.HasPrefix(t, "# ...") || strings.HasPrefix(t, "// ...") ||
			strings.Contains(t, "docstring elided") || strings.Contains(t, "lines elided")
	}

	// If both searchNorm and replaceNorm share an unchanged prefix or suffix containing an elision marker
	// (e.g. "... (docstring elided)"), strip the unchanged elided prefix/suffix before matching.
	if strings.Contains(searchNorm, "...") || strings.Contains(searchNorm, "elided") {
		sTmp := strings.Split(searchNorm, "\n")
		rTmp := strings.Split(replaceNorm, "\n")
		pre := 0
		for pre < len(sTmp) && pre < len(rTmp) && strings.TrimSpace(sTmp[pre]) == strings.TrimSpace(rTmp[pre]) {
			pre++
		}
		cutPre := 0
		for k := 0; k < pre; k++ {
			if isElisionLine(sTmp[k]) {
				cutPre = k + 1
				if cutPre < pre && (strings.Contains(sTmp[cutPre], `"""`) || strings.Contains(sTmp[cutPre], `'''`)) {
					cutPre++
				}
			}
		}
		if cutPre > 0 && cutPre < len(sTmp) && cutPre < len(rTmp) {
			sTmp = sTmp[cutPre:]
			rTmp = rTmp[cutPre:]
		}
		post := 0
		for post < len(sTmp) && post < len(rTmp) &&
			strings.TrimSpace(sTmp[len(sTmp)-1-post]) == strings.TrimSpace(rTmp[len(rTmp)-1-post]) {
			post++
		}
		cutPost := 0
		for k := 0; k < post; k++ {
			if isElisionLine(sTmp[len(sTmp)-1-k]) {
				cutPost = k + 1
			}
		}
		if cutPost > 0 && cutPost < len(sTmp) && cutPost < len(rTmp) {
			sTmp = sTmp[:len(sTmp)-cutPost]
			rTmp = rTmp[:len(rTmp)-cutPost]
		}
		searchNorm = strings.Join(sTmp, "\n")
		replaceNorm = strings.Join(rTmp, "\n")
	}

	// Calculate line counts
	searchLineCount := len(strings.Split(searchNorm, "\n"))
	replaceLineCount := len(strings.Split(replaceNorm, "\n"))
	if searchNorm == "" {
		searchLineCount = 0
	}
	if replaceNorm == "" {
		replaceLineCount = 0
	}

	if searchNorm == "" {
		return replaceNorm + contentNorm, 0, replaceLineCount, nil
	}
	if strings.TrimSpace(searchNorm) == "" {
		return "", 0, 0, fmt.Errorf("EMPTY_SEARCH_MISMATCH: cannot match whitespace-only search block in non-empty content")
	}

	// Pre-Tier 1: Unclosed Docstring Method-Prefix Expansion
	// If searchNorm starts with `def <name>(` and was truncated inside a triple-quoted docstring
	// (odd count of `"""` or `'''`) while replaceNorm starts with the same `def <name>(` header,
	// closes the docstring (even count >= 2), and provides a replacement method body, expand
	// searchNorm in contentNorm to cover the full original `def` method body.
	for _, q := range []string{`"""`, `'''`} {
		if strings.Count(searchNorm, q)%2 == 1 && strings.Count(replaceNorm, q) >= 2 && strings.Count(replaceNorm, q)%2 == 0 {
			sInit := strings.Split(searchNorm, "\n")
			rInit := strings.Split(replaceNorm, "\n")
			if len(sInit) >= 2 && len(rInit) > len(sInit) &&
				strings.TrimSpace(sInit[0]) == strings.TrimSpace(rInit[0]) &&
				strings.HasPrefix(strings.TrimSpace(sInit[0]), "def ") &&
				strings.Count(contentNorm, searchNorm) == 1 {
				cAll := strings.Split(contentNorm, "\n")
				sFirstTrim := strings.TrimSpace(sInit[0])
				defLineIdx := -1
				for li, cl := range cAll {
					if strings.TrimSpace(cl) == sFirstTrim && li+1 < len(cAll) && strings.TrimSpace(cAll[li+1]) == strings.TrimSpace(sInit[1]) {
						defLineIdx = li
						break
					}
				}
				if defLineIdx >= 0 {
					defIndent := getMinLeadingIndent([]string{cAll[defLineIdx]})
					endDefIdx := defLineIdx + len(sInit)
					qCount := 0
					for k := defLineIdx; k < len(cAll); k++ {
						qCount += strings.Count(cAll[k], q)
						if k >= defLineIdx+len(sInit) && qCount >= 2 && qCount%2 == 0 {
							tk := strings.TrimSpace(cAll[k])
							if tk != "" && !strings.HasPrefix(tk, "#") && getMinLeadingIndent([]string{cAll[k]}) <= defIndent {
								break
							}
							endDefIdx = k + 1
						} else if k >= defLineIdx+len(sInit) {
							endDefIdx = k + 1
						}
					}
					for endDefIdx > defLineIdx+len(sInit) && strings.TrimSpace(cAll[endDefIdx-1]) == "" {
						endDefIdx--
					}
					if endDefIdx > defLineIdx+len(sInit) {
						searchNorm = strings.Join(cAll[defLineIdx:endDefIdx], "\n")
						searchLineCount = endDefIdx - defLineIdx
					}
				}
			}
		} else if strings.Count(searchNorm, q) == 1 && strings.Count(replaceNorm, q) == 1 {
			sInit := strings.Split(searchNorm, "\n")
			rInit := strings.Split(replaceNorm, "\n")
			s0Trim := strings.TrimSpace(sInit[0])
			if len(sInit) >= 2 && len(rInit) >= 2 && strings.TrimSpace(rInit[0]) == s0Trim &&
				strings.Contains(sInit[1], q) && strings.Contains(rInit[1], q) &&
				(strings.HasPrefix(s0Trim, "def ") || strings.HasPrefix(s0Trim, "class ")) {
				cAll := strings.Split(contentNorm, "\n")
				findDocClose := func(startIdx int) int {
					qCount := 0
					for k := startIdx; k < len(cAll) && k <= startIdx+350; k++ {
						qCount += strings.Count(cAll[k], q)
						if qCount >= 2 && qCount%2 == 0 {
							return k
						}
					}
					return -1
				}
				matchesPostDoc := func(closeIdx int) bool {
					if closeIdx < 0 {
						return false
					}
					if len(sInit) >= 3 && strings.TrimSpace(sInit[2]) != "" {
						return closeIdx+1 < len(cAll) && strings.TrimSpace(cAll[closeIdx+1]) == strings.TrimSpace(sInit[2])
					}
					return true
				}
				defLineIdx := -1
				docCloseIdx := -1
				for li, cl := range cAll {
					if strings.TrimSpace(cl) == s0Trim && li+1 < len(cAll) && strings.Contains(cAll[li+1], q) {
						dc := findDocClose(li)
						if matchesPostDoc(dc) {
							defLineIdx = li
							docCloseIdx = dc
							break
						}
					}
				}
				if defLineIdx == -1 {
					kw := "def "
					if strings.HasPrefix(s0Trim, "class ") {
						kw = "class "
					}
					symName := strings.TrimSpace(strings.Split(strings.Split(strings.TrimPrefix(s0Trim, kw), "(")[0], ":")[0])
					if len(symName) >= 2 {
						var matches [][2]int
						for li, cl := range cAll {
							t := strings.TrimSpace(cl)
							if (strings.HasPrefix(t, kw+symName+"(") || strings.HasPrefix(t, kw+symName+":") || t == kw+symName) &&
								li+1 < len(cAll) && strings.Contains(cAll[li+1], q) {
								dc := findDocClose(li)
								if matchesPostDoc(dc) {
									matches = append(matches, [2]int{li, dc})
								}
							}
						}
						if len(matches) == 1 {
							defLineIdx = matches[0][0]
							docCloseIdx = matches[0][1]
						}
					}
				}
				if defLineIdx >= 0 && docCloseIdx > defLineIdx {
					docSpan := strings.Join(cAll[defLineIdx:docCloseIdx+1], "\n")
					if len(sInit) > 2 {
						searchNorm = docSpan + "\n" + strings.Join(sInit[2:], "\n")
					} else {
						searchNorm = docSpan
					}
					if len(rInit) > 2 {
						replaceNorm = docSpan + "\n" + strings.Join(rInit[2:], "\n")
					} else {
						replaceNorm = docSpan
					}
					searchLineCount = len(strings.Split(searchNorm, "\n"))
					replaceLineCount = len(strings.Split(replaceNorm, "\n"))
				}
			}
		}
	}

	// Pre-Tier 1b: Unclosed Parenthesized Expression Expansion
	// If searchNorm ends inside an unclosed parenthesized expression (more `(` than `)`)
	// while replaceNorm has balanced parentheses, expand searchNorm in contentNorm to include
	// the remaining lines of the unclosed parenthesized expression.
	if strings.Count(searchNorm, "(") > strings.Count(searchNorm, ")") &&
		strings.Count(replaceNorm, "(") == strings.Count(replaceNorm, ")") &&
		strings.Count(contentNorm, searchNorm) == 1 {
		idx := strings.Index(contentNorm, searchNorm)
		if idx != -1 {
			endPos := idx + len(searchNorm)
			pDepth := strings.Count(searchNorm, "(") - strings.Count(searchNorm, ")")
			steps := 0
			for endPos < len(contentNorm) && pDepth > 0 && steps < 15 {
				nlIdx := strings.IndexByte(contentNorm[endPos:], '\n')
				var lineStr string
				if nlIdx == -1 {
					lineStr = contentNorm[endPos:]
					endPos = len(contentNorm)
				} else {
					lineStr = contentNorm[endPos : endPos+nlIdx+1]
					endPos += nlIdx + 1
				}
				pDepth += strings.Count(lineStr, "(") - strings.Count(lineStr, ")")
				steps++
			}
			if pDepth == 0 {
				searchNorm = strings.TrimRight(contentNorm[idx:endPos], "\n")
				searchLineCount = len(strings.Split(searchNorm, "\n"))
			}
		}
	}

	// Tier 1: Exact Substring Match
	if idx := strings.Index(contentNorm, searchNorm); idx != -1 {
		if cnt := strings.Count(contentNorm, searchNorm); cnt >= 2 && cnt <= 3 && searchLineCount >= 2 && len(strings.TrimSpace(searchNorm)) >= 35 {
			result := strings.ReplaceAll(contentNorm, searchNorm, replaceNorm)
			return result, replaceLineCount * cnt, searchLineCount * cnt, nil
		}
		result := contentNorm[:idx] + replaceNorm + contentNorm[idx+len(searchNorm):]
		return result, replaceLineCount, searchLineCount, nil
	}

	if !allowFuzzy {
		return "", 0, 0, fmt.Errorf("EXACT_SEARCH_MISMATCH: exact search block not found in content")
	}

	contentLines := strings.Split(contentNorm, "\n")
	searchLines := strings.Split(searchNorm, "\n")
	replaceLines := strings.Split(replaceNorm, "\n")
	searchLen := len(searchLines)

	// Pre-Tier 2: Line-0 Stripped Indentation Recovery
	// When the LLM omits leading spaces on line 0 of search_text/replace_text (right after the opening JSON quote)
	// while preserving the target file's actual indentation on lines 1..N, restore line 0's leading spaces
	// so Tier 2 matches directly without Tier 3/4 double-indenting lines 1..N.
	if searchLen >= 2 && getIndentationLevel(searchLines[0]) == 0 && getIndentationLevel(searchLines[1]) >= 4 && searchLen <= len(contentLines) {
		for i := 0; i <= len(contentLines)-searchLen; i++ {
			if getIndentationLevel(contentLines[i]) > 0 && getIndentationLevel(contentLines[i+1]) == getIndentationLevel(searchLines[1]) {
				allMatch := true
				for j := 0; j < searchLen; j++ {
					if strings.TrimSpace(contentLines[i+j]) != strings.TrimSpace(searchLines[j]) {
						allMatch = false
						break
					}
				}
				if allMatch {
					l0Cols := getIndentationLevel(contentLines[i])
					l0Prefix := contentLines[i][:leadingWhitespaceBytesForCols(contentLines[i], l0Cols)]
					searchLines[0] = l0Prefix + searchLines[0]
					if len(replaceLines) >= 2 && getIndentationLevel(replaceLines[0]) == 0 && getIndentationLevel(replaceLines[1]) >= l0Cols {
						replaceLines[0] = l0Prefix + replaceLines[0]
					}
					break
				}
			}
		}
	}

	// Tier 2: Trimmed Trailing Whitespace
	if searchLen <= len(contentLines) {
		for i := 0; i <= len(contentLines)-searchLen; i++ {
			matched := true
			for j := 0; j < searchLen; j++ {
				cLine := strings.TrimRight(contentLines[i+j], " \t\r")
				sLine := strings.TrimRight(searchLines[j], " \t\r")
				if cLine != sLine {
					matched = false
					break
				}
			}
			if matched {
				var newLines []string
				newLines = append(newLines, contentLines[:i]...)
				newLines = append(newLines, replaceLines...)
				newLines = append(newLines, contentLines[i+searchLen:]...)
				return strings.Join(newLines, "\n"), replaceLineCount, searchLen, nil
			}
		}
	}

	// Tier 3: Indentation-Relaxed Matching
	minSearchIndent := getMinLeadingIndent(searchLines)
	var strippedSearch []string
	for _, l := range searchLines {
		if strings.TrimSpace(l) == "" {
			strippedSearch = append(strippedSearch, "")
		} else {
			strippedSearch = append(strippedSearch, stripIndentCols(l, minSearchIndent))
		}
	}

	if searchLen <= len(contentLines) {
		for i := 0; i <= len(contentLines)-searchLen; i++ {
			candIndent := getCandidateLeadingIndent(contentLines[i : i+searchLen])
			matched := true
			for j := 0; j < searchLen; j++ {
				cLine := contentLines[i+j]
				sLine := strippedSearch[j]

				if strings.TrimSpace(sLine) == "" {
					if strings.TrimSpace(cLine) != "" {
						matched = false
						break
					}
					continue
				}

				cStripped := stripIndentCols(cLine, candIndent)

				if !linesMatchIndentationRelaxed(cStripped, sLine) {
					matched = false
					break
				}
			}

			if matched {
				// Determine candidate indentation prefix (preserving tabs or spaces)
				candPrefix := ""
				for _, cl := range contentLines[i : i+searchLen] {
					if strings.TrimSpace(cl) != "" {
						byteOffset := leadingWhitespaceBytesForCols(cl, candIndent)
						candPrefix = cl[:byteOffset]
						break
					}
				}
				if candPrefix == "" && candIndent > 0 {
					candPrefix = strings.Repeat(" ", candIndent)
				}

				var reindentedReplace []string
				minReplaceIndent := getMinLeadingIndent(replaceLines)
				for _, rl := range replaceLines {
					if strings.TrimSpace(rl) == "" {
						reindentedReplace = append(reindentedReplace, "")
					} else {
						rStripped := stripIndentCols(rl, minReplaceIndent)
						reindentedReplace = append(reindentedReplace, formatReindentedLine(candPrefix, rStripped))
					}
				}

				var newLines []string
				newLines = append(newLines, contentLines[:i]...)
				newLines = append(newLines, reindentedReplace...)
				newLines = append(newLines, contentLines[i+searchLen:]...)
				return strings.Join(newLines, "\n"), len(reindentedReplace), searchLen, nil
			}
		}
	}

	// Tier 4: Non-Significant Blank Lines Matching
	type indexedLine struct {
		line  string
		index int
	}
	var nonBlankSearch []indexedLine
	for idx, l := range searchLines {
		if strings.TrimSpace(l) != "" {
			nonBlankSearch = append(nonBlankSearch, indexedLine{line: strings.TrimRight(l, " \t\r"), index: idx})
		}
	}

	var nonBlankContent []indexedLine
	for idx, l := range contentLines {
		if strings.TrimSpace(l) != "" {
			nonBlankContent = append(nonBlankContent, indexedLine{line: strings.TrimRight(l, " \t\r"), index: idx})
		}
	}

	nbSearchLen := len(nonBlankSearch)
	if nbSearchLen > 0 && nbSearchLen <= len(nonBlankContent) {
		for i := 0; i <= len(nonBlankContent)-nbSearchLen; i++ {
			matched := true
			for j := 0; j < nbSearchLen; j++ {
				cText := strings.TrimSpace(nonBlankContent[i+j].line)
				sText := strings.TrimSpace(nonBlankSearch[j].line)
				if cText != sText {
					matched = false
					break
				}
			}

			if matched {
				// Span in content lines from first matched line to last matched line
				cStart := nonBlankContent[i].index
				cEnd := nonBlankContent[i+nbSearchLen-1].index + 1
				removedCount := cEnd - cStart

				candSlice := contentLines[cStart:cEnd]
				candIndent := getCandidateLeadingIndent(candSlice)
				finalReplace := replaceLines
				if candIndent != minSearchIndent {
					candPrefix := ""
					for _, cl := range candSlice {
						if strings.TrimSpace(cl) != "" {
							byteOffset := leadingWhitespaceBytesForCols(cl, candIndent)
							candPrefix = cl[:byteOffset]
							break
						}
					}
					if candPrefix == "" && candIndent > 0 {
						candPrefix = strings.Repeat(" ", candIndent)
					}
					var reindented []string
					minRepIndent := getMinLeadingIndent(replaceLines)
					for _, rl := range replaceLines {
						if strings.TrimSpace(rl) == "" {
							reindented = append(reindented, "")
						} else {
							rStripped := stripIndentCols(rl, minRepIndent)
							reindented = append(reindented, formatReindentedLine(candPrefix, rStripped))
						}
					}
					finalReplace = reindented
				}

				var newLines []string
				newLines = append(newLines, contentLines[:cStart]...)
				newLines = append(newLines, finalReplace...)
				newLines = append(newLines, contentLines[cEnd:]...)
				return strings.Join(newLines, "\n"), len(finalReplace), removedCount, nil
			}
		}
	}

	// Tier 4.5: Unchanged-Context Prefix/Suffix Trimming
	// When the LLM includes 1-3 unchanged context lines at the top or bottom of both Search and Replace
	// that had a minor quote/comment/hallucination mismatch, stripping those identical no-op boundary lines
	// leaves the exact diff + valid inner anchors.
	{
		commonPre := 0
		for commonPre < len(searchLines) && commonPre < len(replaceLines) &&
			strings.TrimSpace(searchLines[commonPre]) == strings.TrimSpace(replaceLines[commonPre]) {
			commonPre++
		}
		commonPost := 0
		for commonPost < len(searchLines)-commonPre && commonPost < len(replaceLines)-commonPre &&
			strings.TrimSpace(searchLines[len(searchLines)-1-commonPost]) == strings.TrimSpace(replaceLines[len(replaceLines)-1-commonPost]) {
			commonPost++
		}
		if commonPre > 0 || commonPost > 0 {
			for trimPre := 0; trimPre <= commonPre && trimPre <= 3; trimPre++ {
				for trimPost := 0; trimPost <= commonPost && trimPost <= 3; trimPost++ {
					if trimPre == 0 && trimPost == 0 {
						continue
					}
					sSub := searchLines[trimPre : len(searchLines)-trimPost]
					rSub := replaceLines[trimPre : len(replaceLines)-trimPost]
					if len(sSub) == 0 || len(rSub) == 0 {
						continue
					}
					var nbSub []indexedLine
					for idx, l := range sSub {
						if strings.TrimSpace(l) != "" {
							nbSub = append(nbSub, indexedLine{line: strings.TrimRight(l, " \t\r"), index: idx})
						}
					}
					if len(nbSub) == 0 || (len(nbSub) == 1 && len(strings.TrimSpace(nbSub[0].line)) < 25) {
						continue
					}
					if len(nbSub) > len(nonBlankContent) {
						continue
					}
					matchCount := 0
					matchIdx := -1
					for i := 0; i <= len(nonBlankContent)-len(nbSub); i++ {
						ok := true
						for j := 0; j < len(nbSub); j++ {
							if strings.TrimSpace(nonBlankContent[i+j].line) != strings.TrimSpace(nbSub[j].line) {
								ok = false
								break
							}
						}
						if ok {
							matchCount++
							matchIdx = i
						}
					}
					if matchCount == 1 && matchIdx >= 0 {
						cStart := nonBlankContent[matchIdx].index
						cEnd := nonBlankContent[matchIdx+len(nbSub)-1].index + 1
						removedCount := cEnd - cStart
						candSlice := contentLines[cStart:cEnd]
						candIndent := getCandidateLeadingIndent(candSlice)
						candPrefix := ""
						for _, cl := range candSlice {
							if strings.TrimSpace(cl) != "" {
								byteOffset := leadingWhitespaceBytesForCols(cl, candIndent)
								candPrefix = cl[:byteOffset]
								break
							}
						}
						if candPrefix == "" && candIndent > 0 {
							candPrefix = strings.Repeat(" ", candIndent)
						}
						var reindented []string
						minRepIndent := getMinLeadingIndent(rSub)
						for _, rl := range rSub {
							if strings.TrimSpace(rl) == "" {
								reindented = append(reindented, "")
							} else {
								rStripped := stripIndentCols(rl, minRepIndent)
								reindented = append(reindented, formatReindentedLine(candPrefix, rStripped))
							}
						}
						var newLines []string
						newLines = append(newLines, contentLines[:cStart]...)
						newLines = append(newLines, reindented...)
						newLines = append(newLines, contentLines[cEnd:]...)
						return strings.Join(newLines, "\n"), len(reindented), removedCount, nil
					}
				}
			}
		}
	}

	// Tier 5: Comment/Ellipsis-Relaxed & Unique Anchor-Span Matching
	// Handles cases where the LLM omitted a comment line or used '...' inside search_text
	// while providing a complete, non-ellipsis replace_text anchored by unique boundary lines.
	hasReplaceEllipsis := false
	for _, rl := range replaceLines {
		if isElisionLine(rl) {
			hasReplaceEllipsis = true
			break
		}
	}
	if !hasReplaceEllipsis && nbSearchLen >= 3 {
		isIgnorableSearchLine := func(s string) bool {
			t := strings.TrimSpace(s)
			return t == "" || t == "..." || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//")
		}
		var codeSearch []indexedLine
		for _, item := range nonBlankSearch {
			if !isIgnorableSearchLine(item.line) {
				codeSearch = append(codeSearch, item)
			}
		}
		var codeContent []indexedLine
		for _, item := range nonBlankContent {
			if !isIgnorableSearchLine(item.line) {
				codeContent = append(codeContent, item)
			}
		}
		csLen := len(codeSearch)
		if csLen >= 2 && csLen <= len(codeContent) {
			matchIdx := -1
			matchCount := 0
			for i := 0; i <= len(codeContent)-csLen; i++ {
				matched := true
				for j := 0; j < csLen; j++ {
					if strings.TrimSpace(codeContent[i+j].line) != strings.TrimSpace(codeSearch[j].line) {
						matched = false
						break
					}
				}
				if matched {
					cStart := codeContent[i].index
					cEnd := codeContent[i+csLen-1].index + 1
					if cEnd-cStart <= searchLen+12 {
						matchIdx = i
						matchCount++
					}
				}
			}
			if matchCount == 1 && matchIdx >= 0 {
				cStart := codeContent[matchIdx].index
				cEnd := codeContent[matchIdx+csLen-1].index + 1
				removedCount := cEnd - cStart
				candSlice := contentLines[cStart:cEnd]
				candIndent := getCandidateLeadingIndent(candSlice)
				candPrefix := ""
				for _, cl := range candSlice {
					if strings.TrimSpace(cl) != "" {
						byteOffset := leadingWhitespaceBytesForCols(cl, candIndent)
						candPrefix = cl[:byteOffset]
						break
					}
				}
				if candPrefix == "" && candIndent > 0 {
					candPrefix = strings.Repeat(" ", candIndent)
				}
				var reindented []string
				minRepIndent := getMinLeadingIndent(replaceLines)
				for _, rl := range replaceLines {
					if strings.TrimSpace(rl) == "" {
						reindented = append(reindented, "")
					} else {
						rStripped := stripIndentCols(rl, minRepIndent)
						reindented = append(reindented, formatReindentedLine(candPrefix, rStripped))
					}
				}
				var newLines []string
				newLines = append(newLines, contentLines[:cStart]...)
				newLines = append(newLines, reindented...)
				newLines = append(newLines, contentLines[cEnd:]...)
				return strings.Join(newLines, "\n"), len(reindented), removedCount, nil
			}
		}
	}

	// Tier 6: Docstring-Bridge & Class/Def Header Matching
	// Handles edits anchored on a class/def header where a multi-line docstring was elided in the prompt
	// or where the LLM guessed a base class and anchored on `class Foo(...):\n    """`.
	if !hasReplaceEllipsis && nbSearchLen >= 1 {
		firstS := strings.TrimSpace(nonBlankSearch[0].line)
		if strings.HasPrefix(firstS, "class ") || strings.HasPrefix(firstS, "def ") {
			isCls := strings.HasPrefix(firstS, "class ")
			kw := "def "
			if isCls {
				kw = "class "
			}
			symName := strings.TrimSpace(strings.TrimPrefix(firstS, kw))
			symName = strings.Split(strings.Split(symName, "(")[0], ":")[0]
			symName = strings.TrimSpace(symName)
			if len(symName) >= 2 {
				var declMatches []int
				for idx, cl := range contentLines {
					t := strings.TrimSpace(cl)
					if strings.HasPrefix(t, kw+symName+"(") || strings.HasPrefix(t, kw+symName+":") || t == kw+symName {
						declMatches = append(declMatches, idx)
					}
				}
				if len(declMatches) == 1 {
					declIdx := declMatches[0]
					docStart := -1
					docEnd := -1
					for k := declIdx + 1; k < len(contentLines) && k <= declIdx+5; k++ {
						tk := strings.TrimSpace(contentLines[k])
						if tk == "" || strings.HasPrefix(tk, "#") {
							continue
						}
						var q string
						if strings.HasPrefix(tk, `"""`) || strings.HasPrefix(tk, `r"""`) {
							q = `"""`
						} else if strings.HasPrefix(tk, `'''`) || strings.HasPrefix(tk, `r'''`) {
							q = `'''`
						}
						if q != "" && strings.Count(tk, q) == 1 {
							docStart = k
							for m := k + 1; m < len(contentLines) && m <= k+350; m++ {
								if strings.Contains(contentLines[m], q) {
									docEnd = m
									break
								}
							}
						}
						break
					}

					if docStart > declIdx && docEnd > docStart {
						s1Trim := ""
						if nbSearchLen >= 2 {
							s1Trim = strings.TrimSpace(nonBlankSearch[1].line)
						}
						isS1Doc := strings.HasPrefix(s1Trim, `"""`) || strings.HasPrefix(s1Trim, `'''`) || strings.HasPrefix(s1Trim, `r"""`) || strings.HasPrefix(s1Trim, `r'''`)
						if nbSearchLen == 2 && isS1Doc && len(replaceLines) > 2 {
							repDocLine := -1
							for rIdx, rl := range replaceLines {
								rt := strings.TrimSpace(rl)
								if rIdx > 0 && (strings.HasPrefix(rt, `"""`) || strings.HasPrefix(rt, `'''`) || strings.HasPrefix(rt, `r"""`)) {
									repDocLine = rIdx
									break
								}
							}
							if repDocLine > 0 {
								var addedLines []string
								if repDocLine > 1 {
									addedLines = append(addedLines, replaceLines[1:repDocLine]...)
								}
								if repDocLine+1 < len(replaceLines) {
									addedLines = append(addedLines, replaceLines[repDocLine+1:]...)
								}
								if len(addedLines) > 0 {
									var newLines []string
									newLines = append(newLines, contentLines[:docEnd+1]...)
									newLines = append(newLines, addedLines...)
									newLines = append(newLines, contentLines[docEnd+1:]...)
									return strings.Join(newLines, "\n"), len(addedLines), 0, nil
								}
							}
						}

						tailStartIdx := 1
						if nbSearchLen >= 2 && isS1Doc {
							tailStartIdx = 2
						}
						if tailStartIdx < nbSearchLen {
							tailSearch := nonBlankSearch[tailStartIdx:]
							var postDocNB []indexedLine
							for k := docEnd + 1; k < len(contentLines) && len(postDocNB) < len(tailSearch); k++ {
								if strings.TrimSpace(contentLines[k]) != "" {
									postDocNB = append(postDocNB, indexedLine{line: contentLines[k], index: k})
								}
							}
							if len(postDocNB) == len(tailSearch) {
								tailMatched := true
								for tIdx := 0; tIdx < len(tailSearch); tIdx++ {
									if strings.TrimSpace(postDocNB[tIdx].line) != strings.TrimSpace(tailSearch[tIdx].line) {
										tailMatched = false
										break
									}
								}
								if tailMatched {
									cEnd := postDocNB[len(postDocNB)-1].index + 1
									repTailStart := 0
									for rIdx, rl := range replaceLines {
										rt := strings.TrimSpace(rl)
										if strings.HasPrefix(rt, kw) {
											repTailStart = rIdx + 1
											for r2 := repTailStart; r2 < len(replaceLines); r2++ {
												rt2 := strings.TrimSpace(replaceLines[r2])
												if rt2 == "" || strings.HasPrefix(rt2, "#") {
													continue
												}
												if rt2 == `"""` || rt2 == `'''` {
													repTailStart = r2 + 1
												}
												break
											}
											break
										}
									}
									addedOrReplaced := replaceLines[repTailStart:]
									lastIdx := postDocNB[len(postDocNB)-1].index
									lastTrim := strings.TrimSpace(contentLines[lastIdx])
									if strings.HasPrefix(lastTrim, "def ") && strings.HasSuffix(lastTrim, ":") {
										defIndent := len(contentLines[lastIdx]) - len(strings.TrimLeft(contentLines[lastIdx], " \t"))
										defBodyEnd := cEnd
										for bIdx := lastIdx + 1; bIdx < len(contentLines); bIdx++ {
											bt := strings.TrimSpace(contentLines[bIdx])
											if bt == "" || strings.HasPrefix(bt, "#") {
												defBodyEnd = bIdx + 1
												continue
											}
											bIndent := len(contentLines[bIdx]) - len(strings.TrimLeft(contentLines[bIdx], " \t"))
											if bIndent <= defIndent {
												break
											}
											defBodyEnd = bIdx + 1
										}
										lastRepTrim := ""
										for rIdx := len(addedOrReplaced) - 1; rIdx >= 0; rIdx-- {
											if strings.TrimSpace(addedOrReplaced[rIdx]) != "" {
												lastRepTrim = strings.TrimSpace(addedOrReplaced[rIdx])
												break
											}
										}
										if lastRepTrim != lastTrim && defBodyEnd > cEnd {
											defPosInRep := -1
											nextDefInRep := -1
											for rIdx, rl := range addedOrReplaced {
												rt := strings.TrimSpace(rl)
												if rt == lastTrim {
													defPosInRep = rIdx
												} else if defPosInRep >= 0 && rt != "" && !strings.HasPrefix(rt, "#") {
													if strings.HasPrefix(rt, "def ") && (len(rl)-len(strings.TrimLeft(rl, " \t"))) <= defIndent {
														nextDefInRep = rIdx
													}
													break
												}
											}
											if defPosInRep >= 0 && nextDefInRep > defPosInRep {
												var fixedRep []string
												fixedRep = append(fixedRep, addedOrReplaced[:defPosInRep+1]...)
												fixedRep = append(fixedRep, contentLines[lastIdx+1:defBodyEnd]...)
												fixedRep = append(fixedRep, addedOrReplaced[nextDefInRep:]...)
												addedOrReplaced = fixedRep
												cEnd = defBodyEnd
											} else if defPosInRep >= 0 {
												cEnd = defBodyEnd
											}
										}
									}
									var newLines []string
									newLines = append(newLines, contentLines[:docEnd+1]...)
									newLines = append(newLines, addedOrReplaced...)
									newLines = append(newLines, contentLines[cEnd:]...)
									return strings.Join(newLines, "\n"), len(addedOrReplaced), cEnd - (docEnd + 1), nil
								}
							}
						}
					}
				}
			}
		}
	}

	return "", 0, 0, fmt.Errorf("FUZZY_SEARCH_MISMATCH: all 4 tiers of fuzzy search failed to match target block")
}

func getMinLeadingIndent(lines []string) int {
	minIndent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		indent := 0
		for _, ch := range l {
			if ch == ' ' {
				indent++
			} else if ch == '\t' {
				indent += 4
			} else {
				break
			}
		}
		if minIndent == -1 || indent < minIndent {
			minIndent = indent
		}
	}
	if minIndent == -1 {
		return 0
	}
	return minIndent
}

func getCandidateLeadingIndent(lines []string) int {
	return getMinLeadingIndent(lines)
}

// leadingWhitespaceCols returns visual column width of leading whitespace (tab = 4 spaces).
func leadingWhitespaceCols(s string) int {
	cols := 0
	for _, ch := range s {
		if ch == ' ' {
			cols++
		} else if ch == '\t' {
			cols += 4
		} else {
			break
		}
	}
	return cols
}

// leadingWhitespaceBytesForCols returns the byte offset in line where visual column reaches or exceeds targetCols,
// stopping at the end of leading whitespace.
func leadingWhitespaceBytesForCols(line string, targetCols int) int {
	if targetCols <= 0 {
		return 0
	}
	cols := 0
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if ch == ' ' {
			cols++
		} else if ch == '\t' {
			cols += 4
		} else {
			return i
		}
		if cols >= targetCols {
			return i + 1
		}
	}
	return len(line)
}

// stripIndentCols removes up to targetCols visual columns of leading whitespace from line.
func stripIndentCols(line string, targetCols int) string {
	if targetCols <= 0 {
		return line
	}
	offset := leadingWhitespaceBytesForCols(line, targetCols)
	return line[offset:]
}

// linesMatchIndentationRelaxed compares two lines after base indentation has been stripped,
// normalizing tab vs space equivalence for relative indentation.
func linesMatchIndentationRelaxed(cStripped, sLine string) bool {
	cTrimmed := strings.TrimRight(cStripped, " \t\r")
	sTrimmed := strings.TrimRight(sLine, " \t\r")
	if cTrimmed == sTrimmed {
		return true
	}
	if leadingWhitespaceCols(cTrimmed) == leadingWhitespaceCols(sTrimmed) &&
		strings.TrimLeft(cTrimmed, " \t") == strings.TrimLeft(sTrimmed, " \t") {
		return true
	}
	return false
}

// formatReindentedLine applies candidate base indentation prefix to a replacement line,
// preserving the candidate's tab/space indentation style for relative indentation.
func formatReindentedLine(candPrefix string, rStripped string) string {
	if strings.TrimSpace(rStripped) == "" {
		return ""
	}
	content := strings.TrimLeft(rStripped, " \t")
	relCols := leadingWhitespaceCols(rStripped)
	if relCols == 0 {
		return candPrefix + content
	}

	candUsesTabs := strings.Contains(candPrefix, "\t")
	var relIndent string
	if candUsesTabs {
		relIndent = strings.Repeat("\t", relCols/4) + strings.Repeat(" ", relCols%4)
	} else {
		relIndent = strings.Repeat(" ", relCols)
	}

	return candPrefix + relIndent + content
}

// ApplyEditBlocks applies search/replace edit blocks with transactional staging,
// atomic disk rollback, and CAS Merkle seal attestation.
func ApplyEditBlocks(req EditBlockRequest) (*EditBlockResult, error) {
	if req.RepoPath == "" {
		req.RepoPath = "."
	}

	var blocks []EditBlock
	if len(req.Blocks) > 0 {
		blocks = req.Blocks
	} else if req.EditBlocksText != "" {
		parsed, err := ParseAiderEditBlocks(req.EditBlocksText, req.TargetFile)
		if err != nil {
			return nil, err
		}
		blocks = parsed
	} else {
		return nil, fmt.Errorf("NO_BLOCKS: neither EditBlocksText nor Blocks provided")
	}

	// 1. Group blocks by file path preserving deterministic encounter order
	fileMap := make(map[string][]EditBlock)
	var orderedFiles []string
	for _, b := range blocks {
		fp := filepath.ToSlash(b.FilePath)
		if req.TargetFile != "" && (fp == "" || fp == ".") {
			fp = filepath.ToSlash(req.TargetFile)
		}
		if _, exists := fileMap[fp]; !exists {
			orderedFiles = append(orderedFiles, fp)
		}
		fileMap[fp] = append(fileMap[fp], b)
	}

	// 2. In-memory staging: read snapshots and simulate replacements
	originalContents := make(map[string]string)
	modifiedContents := make(map[string]string)
	totalAdded := 0
	totalRemoved := 0
	blocksApplied := 0

	resolvePath := func(p string) string {
		clean := filepath.FromSlash(p)
		if filepath.IsAbs(clean) {
			return clean
		}
		return filepath.Join(req.RepoPath, clean)
	}

	for _, relPath := range orderedFiles {
		fullPath := resolvePath(relPath)
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				raw = []byte("")
			} else {
				return nil, fmt.Errorf("FILE_READ_ERROR: could not read %s: %w", relPath, err)
			}
		}
		rawStr := string(raw)
		originalContents[relPath] = rawStr
		content := canonical.NormalizeLF(rawStr)

		currentText := content
		fileBlocks := fileMap[relPath]

		for blockIdx, b := range fileBlocks {
			b.SearchText = canonical.NormalizeLF(b.SearchText)
			b.ReplaceText = canonical.NormalizeLF(b.ReplaceText)

			// If currentText is empty, direct replacement initializes the file
			if len(strings.TrimSpace(currentText)) == 0 {
				currentText = b.ReplaceText
				replaceLines := len(strings.Split(b.ReplaceText, "\n"))
				if b.ReplaceText == "" {
					replaceLines = 0
				}
				totalAdded += replaceLines
				blocksApplied++
				continue
			}

			newText, added, removed, matchErr := ReplaceFuzzy(currentText, b.SearchText, b.ReplaceText, req.FuzzyMatch)
			if matchErr != nil {
				// Resilient check 1: Is ReplaceText already in currentText?
				trimReplace := strings.TrimSpace(strings.ReplaceAll(b.ReplaceText, "\r\n", "\n"))
				trimCurrent := strings.ReplaceAll(currentText, "\r\n", "\n")
				if trimReplace != "" && strings.Contains(trimCurrent, trimReplace) {
					blocksApplied++
					continue
				}

				// Resilient check 2: If the file was originally empty in this patch transaction,
				// and multiple blocks were supplied, append the block to avoid failing new file synthesis
				if len(strings.TrimSpace(originalContents[relPath])) == 0 {
					currentText = currentText + "\n" + b.ReplaceText
					replaceLines := len(strings.Split(strings.ReplaceAll(b.ReplaceText, "\r\n", "\n"), "\n"))
					totalAdded += replaceLines
					blocksApplied++
					continue
				}

				// Resilient check 3: First and last line anchor matching
				searchNorm := strings.ReplaceAll(b.SearchText, "\r\n", "\n")
				var sLines []string
				for _, sl := range strings.Split(searchNorm, "\n") {
					if strings.TrimSpace(sl) != "" {
						sLines = append(sLines, strings.TrimSpace(sl))
					}
				}
				if len(sLines) >= 2 {
					firstL := sLines[0]
					lastL := sLines[len(sLines)-1]
					curNorm := strings.ReplaceAll(currentText, "\r\n", "\n")
					cLines := strings.Split(curNorm, "\n")
					startIdx := -1
					endIdx := -1
					for i, cl := range cLines {
						if strings.TrimSpace(cl) == firstL {
							startIdx = i
							for j := i + 1; j < len(cLines); j++ {
								if strings.TrimSpace(cLines[j]) == lastL {
									endIdx = j + 1
									break
								}
							}
							if endIdx != -1 {
								break
							}
						}
					}
					if startIdx != -1 && endIdx != -1 {
						var newL []string
						newL = append(newL, cLines[:startIdx]...)
						newL = append(newL, strings.Split(strings.ReplaceAll(b.ReplaceText, "\r\n", "\n"), "\n")...)
						newL = append(newL, cLines[endIdx:]...)
						currentText = strings.Join(newL, "\n")
						replaceLines := len(strings.Split(strings.ReplaceAll(b.ReplaceText, "\r\n", "\n"), "\n"))
						totalAdded += replaceLines
						totalRemoved += (endIdx - startIdx)
						blocksApplied++
						continue
					}
				}

				// Resilient check 4: Multi-block fallback recovery on existing files ONLY for documentation
				ext := strings.ToLower(filepath.Ext(relPath))
				if (ext == ".md" || ext == ".txt") && req.FuzzyMatch && (len(fileBlocks) > 1 || blockIdx > 0) {
					trimmedRep := strings.TrimSpace(b.ReplaceText)
					if trimmedRep != "" && !strings.Contains(currentText, trimmedRep) {
						currentText = currentText + "\n\n" + b.ReplaceText
						replaceLines := len(strings.Split(strings.ReplaceAll(b.ReplaceText, "\r\n", "\n"), "\n"))
						totalAdded += replaceLines
						blocksApplied++
						continue
					} else if trimmedRep != "" && strings.Contains(currentText, trimmedRep) {
						blocksApplied++
						continue
					}
				}

				return &EditBlockResult{
					Status:       "REJECTED",
					PatchID:      fmt.Sprintf("patch-fail-%d", time.Now().UnixNano()),
					ErrorMessage: fmt.Sprintf("BLOCK_MATCH_FAILED on file %s (block %d): %v", relPath, blockIdx+1, matchErr),
					Applied:      false,
				}, fmt.Errorf("BLOCK_MATCH_FAILED on %s (block %d): %w", relPath, blockIdx+1, matchErr)
			}
			currentText = newText
			totalAdded += added
			totalRemoved += removed
			blocksApplied++
		}
		if strings.Contains(originalContents[relPath], "\r\n") && !strings.Contains(currentText, "\r\n") {
			currentText = strings.ReplaceAll(currentText, "\n", "\r\n")
		}
		modifiedContents[relPath] = currentText
	}

	// 3. Compute CAS Hash and Canonical Merkle Seal
	h := sha256.New()
	for _, relPath := range orderedFiles {
		h.Write([]byte(relPath))
		h.Write([]byte(modifiedContents[relPath]))
	}
	casHash := hex.EncodeToString(h.Sum(nil))
	patchID := fmt.Sprintf("aider-cas-%s", casHash[:12])

	merkleSeal := canonical.SHA256Of(map[string]any{
		"patch_id":       patchID,
		"cas_hash":       casHash,
		"files_modified": orderedFiles,
		"blocks_applied": blocksApplied,
		"lines_added":    totalAdded,
		"lines_removed":  totalRemoved,
	})

	// 4. If DryRun, do not touch disk
	if req.DryRun {
		return &EditBlockResult{
			Status:        "APPLIED",
			PatchID:       patchID,
			CASHash:       casHash,
			MerkleSeal:    merkleSeal,
			FilesModified: orderedFiles,
			BlocksApplied: blocksApplied,
			LinesAdded:    totalAdded,
			LinesRemoved:  totalRemoved,
			Applied:       false,
			Blocks:        blocks,
		}, nil
	}

	// 5. Atomic disk commit with rollback on failure
	var writtenFiles []string
	var newlyCreatedFiles []string
	for _, relPath := range orderedFiles {
		fullPath := resolvePath(relPath)
		if _, statErr := os.Stat(fullPath); os.IsNotExist(statErr) {
			newlyCreatedFiles = append(newlyCreatedFiles, relPath)
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			// Roll back all previously written files
			for _, written := range writtenFiles {
				wPath := resolvePath(written)
				if isItemInList(newlyCreatedFiles, written) {
					_ = os.Remove(wPath)
				} else {
					orig := originalContents[written]
					_ = os.WriteFile(wPath, []byte(orig), 0644)
				}
			}
			return nil, fmt.Errorf("MKDIR_ERROR: failed creating directory for %s: %w", relPath, err)
		}
		newContent := modifiedContents[relPath]
		if err := os.WriteFile(fullPath, []byte(newContent), 0644); err != nil {
			// Roll back all previously written files in this transaction
			for _, written := range writtenFiles {
				wPath := resolvePath(written)
				if isItemInList(newlyCreatedFiles, written) {
					_ = os.Remove(wPath)
				} else {
					orig := originalContents[written]
					_ = os.WriteFile(wPath, []byte(orig), 0644)
				}
			}
			return nil, fmt.Errorf("WRITE_ROLLBACK_ERROR: failed writing %s: %w", relPath, err)
		}
		writtenFiles = append(writtenFiles, relPath)
	}

	return &EditBlockResult{
		Status:        "APPLIED",
		PatchID:       patchID,
		CASHash:       casHash,
		MerkleSeal:    merkleSeal,
		FilesModified: orderedFiles,
		BlocksApplied: blocksApplied,
		LinesAdded:    totalAdded,
		LinesRemoved:  totalRemoved,
		Applied:       true,
		Blocks:        blocks,
	}, nil
}

func isItemInList(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}
