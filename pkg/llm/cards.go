// SPDX-License-Identifier: Apache-2.0
// Package llm provides universal OpenAI-compatible decision gate cards, prompt formatters,
// token budget clamping (<300 tokens), and resilient JSON extraction for OmniCoder-ICX.

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

// Token budget and ratio constants.
const (
	MaxCardTokenBudget = 300
	TokenCharRatio     = 3.5 // Conservative character-to-token ratio
)

// Typed errors for decision card failures.
var (
	ErrTokenBudgetExceeded = errors.New("llm: prompt exceeds 300 token budget")
	ErrInvalidJSONOutput   = errors.New("llm: failed to extract valid JSON from LLM output")
	ErrInvalidDecision     = errors.New("llm: invalid decision payload structure")
)

// ============================================================================
// Token Estimation & Clamping Helpers
// ============================================================================

// EstimateTokens returns a conservative deterministic token estimate for text.
// Uses hybrid heuristic: max(chars/3.5, words*1.25) to avoid underestimating
// whitespace-dense or code-dense text.
func EstimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	charEst := int(float64(len(s)) / TokenCharRatio)
	words := len(strings.Fields(s))
	wordEst := int(float64(words) * 1.25)

	tokens := charEst
	if wordEst > tokens {
		tokens = wordEst
	}
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// ValidateTokenBudget verifies that the combined prompt parts stay strictly below maxTokens.
func ValidateTokenBudget(systemPrompt, userPrompt string, maxTokens int) error {
	total := EstimateTokens(systemPrompt + " " + userPrompt)
	if total >= maxTokens {
		return fmt.Errorf("%w: estimated %d tokens >= budget %d (chars: sys=%d, user=%d)",
			ErrTokenBudgetExceeded, total, maxTokens, len(systemPrompt), len(userPrompt))
	}
	return nil
}

// clampTextToBudget truncates variableText so that EstimateTokens(baseText + " " + variableText) < maxTokens.
func clampTextToBudget(baseText, variableText string, maxTokens int) string {
	baseTokens := EstimateTokens(baseText)
	budgetRemaining := maxTokens - baseTokens - 6 // 6 token safety margin
	if budgetRemaining <= 0 {
		return ""
	}
	if EstimateTokens(baseText+" "+variableText) < maxTokens-6 {
		return variableText
	}

	// If variableText contains newlines (e.g. source code snippets or diffs),
	// clamp line-by-line to strictly preserve indentation and newline structure.
	if strings.Contains(variableText, "\n") {
		// First pass: try folding unreferenced AST function bodies if not already folded
		if !strings.Contains(variableText, "lines folded]") && len(variableText) > 200 {
			folded := FoldUnreferencedAST(variableText, -1, nil, "")
			if folded != variableText {
				if EstimateTokens(baseText+" "+folded) < maxTokens-6 {
					return folded
				}
				variableText = folded
			}
		}

		lines := strings.Split(variableText, "\n")

		// If a line is marked with ">" (focus / error line), window outward around it
		focusIdx := -1
		for idx, l := range lines {
			trimmed := strings.TrimSpace(l)
			if strings.HasPrefix(trimmed, ">") || strings.Contains(trimmed, "> ") {
				focusIdx = idx
				break
			}
		}

		if focusIdx != -1 {
			// Find the natural boundary of the enclosing block downwards (closing brace or next def/func)
			maxDownIdx := len(lines) - 1
			for idx := focusIdx + 1; idx < len(lines); idx++ {
				trimmed := strings.TrimSpace(lines[idx])
				if trimmed == "}" || (len(lines[idx]) > 0 && lines[idx][0] != ' ' && lines[idx][0] != '\t' && (strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "func "))) {
					maxDownIdx = idx
					break
				}
			}

			startIdx := focusIdx
			endIdx := focusIdx
			// 1. Expand down to the end of the current function/block first
			for endIdx < maxDownIdx {
				candidate := strings.Join(lines[startIdx:endIdx+2], "\n")
				if EstimateTokens(baseText+" "+candidate) >= maxTokens-6 {
					break
				}
				endIdx++
			}

			// 2. Expand up to include enclosing function signature and preceding type/class definitions
			for startIdx > 0 {
				candidate := strings.Join(lines[startIdx-1:endIdx+1], "\n")
				if EstimateTokens(baseText+" "+candidate) >= maxTokens-6 {
					break
				}
				startIdx--
			}

			// 3. If budget remains, expand downward further into subsequent context
			for endIdx+1 < len(lines) {
				candidate := strings.Join(lines[startIdx:endIdx+2], "\n")
				if EstimateTokens(baseText+" "+candidate) >= maxTokens-6 {
					break
				}
				endIdx++
			}

			return strings.Join(lines[startIdx:endIdx+1], "\n")
		}

		var kept []string
		for _, l := range lines {
			candidate := strings.Join(append(kept, l), "\n")
			if EstimateTokens(baseText+" "+candidate) >= maxTokens-6 {
				break
			}
			kept = append(kept, l)
		}
		if len(kept) > 0 {
			return strings.Join(kept, "\n")
		}
	}
	maxChars := int(float64(budgetRemaining) * TokenCharRatio)
	maxWords := int(float64(budgetRemaining) / 1.25)

	if len(variableText) > maxChars {
		if maxChars > 25 {
			variableText = variableText[:maxChars-20] + "\n[clamped...]"
		} else {
			variableText = variableText[:maxChars]
		}
	}

	words := strings.Fields(variableText)
	if len(words) > maxWords && maxWords > 0 {
		if maxWords > 4 {
			variableText = strings.Join(words[:maxWords-3], " ") + " [clamped...]"
		} else {
			variableText = strings.Join(words[:maxWords], " ")
		}
	}

	// Strictly guarantee that EstimateTokens(baseText + " " + variableText) < maxTokens
	for EstimateTokens(baseText+" "+variableText) >= maxTokens && len(variableText) > 5 {
		cut := len(variableText) * 3 / 4
		variableText = strings.TrimSpace(variableText[:cut])
		if len(variableText) > 10 {
			variableText += "..."
		}
	}

	return variableText
}

// extractFocusedSnippet locates the most relevant window in snippet based on keywords in instruction.
func extractFocusedSnippet(snippet, instruction string) string {
	if len(snippet) < 600 {
		return snippet
	}
	words := strings.Fields(instruction)
	bestIdx := -1
	for _, w := range words {
		wClean := strings.Trim(w, "`'\",.:;()[]{}")
		if len(wClean) > 3 && !strings.Contains("func type return package struct import", strings.ToLower(wClean)) {
			if idx := strings.Index(snippet, wClean); idx != -1 {
				bestIdx = idx
				break
			}
		}
	}
	if bestIdx > 300 {
		start := bestIdx - 150
		if start < 0 {
			start = 0
		}
		if nl := strings.LastIndex(snippet[:start], "\n"); nl != -1 {
			start = nl + 1
		}
		return "// ...\n" + snippet[start:]
	}
	return snippet
}

// ============================================================================
// AST Semantic Skeleton & Folding Engine (<300 Tokens)
// ============================================================================

// FoldUnreferencedAST folds unreferenced function and method bodies into "... [N lines folded] ...",
// preserving class signatures, enclosing blocks, and the focused method/lines.
// If focusLine < 0, it auto-detects focus lines marked with ">" or lines referencing focusSymbols.
func FoldUnreferencedAST(source string, focusLine int, focusSymbols []string, ext string) string {
	if len(source) == 0 {
		return source
	}

	ext = strings.ToLower(ext)
	var focusLines []int
	if focusLine > 0 {
		focusLines = append(focusLines, focusLine)
	}

	lines := strings.Split(source, "\n")
	for idx, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, ">") || strings.Contains(trimmed, "> ") {
			focusLines = append(focusLines, idx+1)
		}
	}

	if ext == ".go" || (ext == "" && strings.Contains(source, "package ")) {
		return foldGoAST(source, focusLines, focusSymbols)
	}
	if ext == ".py" || (ext == "" && (strings.Contains(source, "def ") || strings.Contains(source, "import "))) {
		return foldPythonAST(source, focusLines, focusSymbols)
	}
	return foldBraceAST(source, focusLines, focusSymbols)
}

// FoldASTSkeleton extracts the semantic skeleton of source with unreferenced functions folded,
// ensuring the output fits tightly within a given token budget.
func FoldASTSkeleton(source string, instruction string, ext string) string {
	var focusSyms []string
	for _, w := range strings.Fields(instruction) {
		wClean := strings.Trim(w, "`'\",.:;()[]{}")
		if len(wClean) > 3 {
			focusSyms = append(focusSyms, wClean)
		}
	}
	return FoldUnreferencedAST(source, -1, focusSyms, ext)
}

func foldPythonAST(source string, focusLines []int, focusSymbols []string) string {
	lines := strings.Split(source, "\n")
	if len(lines) < 6 {
		return source
	}

	type pyFuncBlock struct {
		name      string
		startIdx  int // 0-indexed def line
		sigEndIdx int // line with ':'
		endIdx    int // last line of body
		indent    int
		inFocus   bool
	}

	var blocks []pyFuncBlock
	reDef := regexp.MustCompile(`^(\s*)(?:async\s+)?def\s+([a-zA-Z0-9_]+)\s*\(`)

	isLineInFocus := func(line1Idx int) bool {
		for _, fl := range focusLines {
			if fl == line1Idx {
				return true
			}
		}
		if line1Idx-1 >= 0 && line1Idx-1 < len(lines) {
			trimmed := strings.TrimSpace(lines[line1Idx-1])
			if strings.HasPrefix(trimmed, ">") || strings.Contains(trimmed, "> ") {
				return true
			}
		}
		return false
	}

	isSymbolInFocus := func(name string) bool {
		for _, s := range focusSymbols {
			if strings.EqualFold(s, name) || strings.Contains(strings.ToLower(name), strings.ToLower(s)) {
				return true
			}
		}
		return false
	}

	// 1. Discover all function blocks
	for i := 0; i < len(lines); i++ {
		m := reDef.FindStringSubmatch(lines[i])
		if len(m) > 2 {
			indent := len(m[1])
			name := m[2]
			startIdx := i
			sigEndIdx := i

			// Find signature end (line containing ':')
			for sigEndIdx < len(lines) {
				if strings.Contains(lines[sigEndIdx], ":") {
					break
				}
				sigEndIdx++
			}
			if sigEndIdx >= len(lines) {
				sigEndIdx = startIdx
			}

			// Find function body end (lines with indent > def indent, or comments/blank lines)
			endIdx := sigEndIdx
			for j := sigEndIdx + 1; j < len(lines); j++ {
				line := lines[j]
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				curIndent := 0
				for _, r := range line {
					if r == ' ' {
						curIndent++
					} else if r == '\t' {
						curIndent += 4
					} else {
						break
					}
				}
				if curIndent <= indent {
					break
				}
				endIdx = j
			}

			// Check if function is in focus
			inFocus := isSymbolInFocus(name)
			if !inFocus {
				for k := startIdx; k <= endIdx; k++ {
					if isLineInFocus(k + 1) {
						inFocus = true
						break
					}
				}
			}

			blocks = append(blocks, pyFuncBlock{
				name:      name,
				startIdx:  startIdx,
				sigEndIdx: sigEndIdx,
				endIdx:    endIdx,
				indent:    indent,
				inFocus:   inFocus,
			})
			i = endIdx
		}
	}

	if len(blocks) == 0 {
		return source
	}

	anyFocus := false
	for _, b := range blocks {
		if b.inFocus {
			anyFocus = true
			break
		}
	}
	if !anyFocus {
		blocks[len(blocks)-1].inFocus = true
	}

	// 2. Reassemble with unreferenced functions folded
	var result []string
	lineIdx := 0

	for blockIdx := 0; blockIdx < len(blocks); blockIdx++ {
		b := blocks[blockIdx]

		// Add lines before this block (class definitions, comments, imports)
		for lineIdx < b.startIdx {
			result = append(result, lines[lineIdx])
			lineIdx++
		}

		if b.inFocus {
			// Keep all lines of focused function
			for lineIdx <= b.endIdx {
				result = append(result, lines[lineIdx])
				lineIdx++
			}
			continue
		}

		// Check if we have consecutive unreferenced blocks at the same indentation (a cluster)
		clusterEndBlock := blockIdx
		for clusterEndBlock+1 < len(blocks) && !blocks[clusterEndBlock+1].inFocus && blocks[clusterEndBlock+1].indent == b.indent {
			clusterEndBlock++
		}

		if clusterEndBlock > blockIdx {
			// Multiple consecutive unreferenced functions: collapse cluster into single fold marker
			clusterLast := blocks[clusterEndBlock]
			totalLinesFolded := (clusterLast.endIdx - b.startIdx) + 1
			indentStr := strings.Repeat(" ", b.indent)
			result = append(result, fmt.Sprintf("%s... [%d lines folded] ...", indentStr, totalLinesFolded))
			lineIdx = clusterLast.endIdx + 1
			blockIdx = clusterEndBlock
			continue
		}

		// Single unreferenced function: fold its body
		bodyLinesCount := b.endIdx - b.sigEndIdx
		if bodyLinesCount >= 2 {
			// Keep signature
			for lineIdx <= b.sigEndIdx {
				result = append(result, lines[lineIdx])
				lineIdx++
			}
			indentStr := strings.Repeat(" ", b.indent+4)
			result = append(result, fmt.Sprintf("%s... [%d lines folded] ...", indentStr, bodyLinesCount))
			lineIdx = b.endIdx + 1
		} else {
			// Keep very short function as is
			for lineIdx <= b.endIdx {
				result = append(result, lines[lineIdx])
				lineIdx++
			}
		}
	}

	// Add any trailing lines
	for lineIdx < len(lines) {
		result = append(result, lines[lineIdx])
		lineIdx++
	}

	return strings.Join(result, "\n")
}

func foldGoAST(source string, focusLines []int, focusSymbols []string) string {
	fset := token.NewFileSet()
	hasPkg := strings.Contains(source, "package ")
	srcToParse := source
	lineOffset := 0
	if !hasPkg {
		srcToParse = "package main\n" + source
		lineOffset = 1
	}

	file, err := parser.ParseFile(fset, "code.go", srcToParse, parser.ParseComments)
	lines := strings.Split(source, "\n")
	if err != nil || file == nil || len(lines) < 6 {
		return foldBraceAST(source, focusLines, focusSymbols)
	}

	isLineInFocus := func(line1Idx int) bool {
		for _, fl := range focusLines {
			if fl == line1Idx {
				return true
			}
		}
		if line1Idx-1 >= 0 && line1Idx-1 < len(lines) {
			trimmed := strings.TrimSpace(lines[line1Idx-1])
			if strings.HasPrefix(trimmed, ">") || strings.Contains(trimmed, "> ") {
				return true
			}
		}
		return false
	}

	isSymbolInFocus := func(name string) bool {
		for _, s := range focusSymbols {
			if strings.EqualFold(s, name) || strings.Contains(strings.ToLower(name), strings.ToLower(s)) {
				return true
			}
		}
		return false
	}

	type goFuncBlock struct {
		name      string
		startLine int
		bodyStart int
		bodyEnd   int
		inFocus   bool
	}

	var blocks []goFuncBlock
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}

		startL := fset.Position(fn.Pos()).Line - lineOffset
		bStart := fset.Position(fn.Body.Lbrace).Line - lineOffset
		bEnd := fset.Position(fn.Body.Rbrace).Line - lineOffset

		inFocus := isSymbolInFocus(fn.Name.Name)
		if !inFocus {
			for l := startL; l <= bEnd; l++ {
				if isLineInFocus(l) {
					inFocus = true
					break
				}
			}
		}

		blocks = append(blocks, goFuncBlock{
			name:      fn.Name.Name,
			startLine: startL,
			bodyStart: bStart,
			bodyEnd:   bEnd,
			inFocus:   inFocus,
		})
	}

	if len(blocks) == 0 {
		return source
	}

	anyFocus := false
	for _, b := range blocks {
		if b.inFocus {
			anyFocus = true
			break
		}
	}
	if !anyFocus {
		blocks[len(blocks)-1].inFocus = true
	}

	var result []string
	curLine := 1

	for _, b := range blocks {
		for curLine < b.startLine && curLine <= len(lines) {
			result = append(result, lines[curLine-1])
			curLine++
		}

		if b.inFocus {
			for curLine <= b.bodyEnd && curLine <= len(lines) {
				result = append(result, lines[curLine-1])
				curLine++
			}
			continue
		}

		bodyFoldCount := b.bodyEnd - b.bodyStart - 1
		if bodyFoldCount >= 2 {
			for curLine <= b.bodyStart && curLine <= len(lines) {
				result = append(result, lines[curLine-1])
				curLine++
			}
			result = append(result, fmt.Sprintf("\t... [%d lines folded] ...", bodyFoldCount))
			curLine = b.bodyEnd
			if curLine <= len(lines) {
				result = append(result, lines[curLine-1])
				curLine++
			}
		} else {
			for curLine <= b.bodyEnd && curLine <= len(lines) {
				result = append(result, lines[curLine-1])
				curLine++
			}
		}
	}

	for curLine <= len(lines) {
		result = append(result, lines[curLine-1])
		curLine++
	}

	return strings.Join(result, "\n")
}

func foldBraceAST(source string, focusLines []int, focusSymbols []string) string {
	lines := strings.Split(source, "\n")
	if len(lines) < 6 {
		return source
	}

	reFunc := regexp.MustCompile(`^(\s*)(?:export\s+)?(?:async\s+)?(?:pub(?:\([^)]+\))?\s+)?(?:function|fn|def|func)?\s*(?:\([^)]+\)\s+)?([a-zA-Z0-9_]+)\s*\([^)]*\)`)

	isLineInFocus := func(idx int) bool {
		for _, fl := range focusLines {
			if fl == idx+1 {
				return true
			}
		}
		trimmed := strings.TrimSpace(lines[idx])
		if strings.HasPrefix(trimmed, ">") || strings.Contains(trimmed, "> ") {
			return true
		}
		return false
	}

	isSymbolInFocus := func(name string) bool {
		for _, s := range focusSymbols {
			if strings.EqualFold(s, name) || strings.Contains(strings.ToLower(name), strings.ToLower(s)) {
				return true
			}
		}
		return false
	}

	type braceBlock struct {
		name       string
		startIdx   int
		openBrace  int
		closeBrace int
		indent     int
		inFocus    bool
	}

	var blocks []braceBlock
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		// Skip control flow statements
		if strings.HasPrefix(trimmed, "if ") || strings.HasPrefix(trimmed, "if(") ||
			strings.HasPrefix(trimmed, "for ") || strings.HasPrefix(trimmed, "for(") ||
			strings.HasPrefix(trimmed, "while ") || strings.HasPrefix(trimmed, "switch ") ||
			strings.HasPrefix(trimmed, "catch ") {
			continue
		}

		m := reFunc.FindStringSubmatch(lines[i])
		if len(m) > 2 {
			name := m[2]
			if name == "if" || name == "for" || name == "while" || name == "switch" || name == "catch" {
				continue
			}
			indent := len(m[1])
			startIdx := i

			openBrace := -1
			for j := i; j < len(lines) && j <= i+5; j++ {
				if strings.Contains(lines[j], "{") {
					openBrace = j
					break
				}
			}
			if openBrace == -1 {
				continue
			}

			depth := 0
			closeBrace := -1
			for j := openBrace; j < len(lines); j++ {
				for _, ch := range lines[j] {
					if ch == '{' {
						depth++
					} else if ch == '}' {
						depth--
						if depth == 0 {
							closeBrace = j
							break
						}
					}
				}
				if depth == 0 && closeBrace != -1 {
					break
				}
			}

			if closeBrace == -1 {
				continue
			}

			inFocus := isSymbolInFocus(name)
			if !inFocus {
				for k := startIdx; k <= closeBrace; k++ {
					if isLineInFocus(k) {
						inFocus = true
						break
					}
				}
			}

			blocks = append(blocks, braceBlock{
				name:       name,
				startIdx:   startIdx,
				openBrace:  openBrace,
				closeBrace: closeBrace,
				indent:     indent,
				inFocus:    inFocus,
			})
			i = closeBrace
		}
	}

	if len(blocks) == 0 {
		return source
	}

	anyFocus := false
	for _, b := range blocks {
		if b.inFocus {
			anyFocus = true
			break
		}
	}
	if !anyFocus {
		blocks[len(blocks)-1].inFocus = true
	}

	var result []string
	curIdx := 0

	for _, b := range blocks {
		for curIdx < b.startIdx {
			result = append(result, lines[curIdx])
			curIdx++
		}

		if b.inFocus {
			for curIdx <= b.closeBrace {
				result = append(result, lines[curIdx])
				curIdx++
			}
			continue
		}

		foldedCount := b.closeBrace - b.openBrace - 1
		if foldedCount >= 2 {
			for curIdx <= b.openBrace {
				result = append(result, lines[curIdx])
				curIdx++
			}
			indentStr := strings.Repeat(" ", b.indent+4)
			if strings.HasPrefix(lines[b.startIdx], "\t") {
				indentStr = "\t"
			}
			result = append(result, fmt.Sprintf("%s... [%d lines folded] ...", indentStr, foldedCount))
			curIdx = b.closeBrace
			result = append(result, lines[curIdx])
			curIdx++
		} else {
			for curIdx <= b.closeBrace {
				result = append(result, lines[curIdx])
				curIdx++
			}
		}
	}

	for curIdx < len(lines) {
		result = append(result, lines[curIdx])
		curIdx++
	}

	return strings.Join(result, "\n")
}

// ============================================================================
// Resilient JSON & Markdown Extraction
// ============================================================================

// StripMarkdownFences removes markdown code fences (```json ... ``` or ``` ... ```) and trims whitespace.
func StripMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	// Strip DeepSeek/R1 <think>...</think> blocks if present
	if strings.HasPrefix(s, "<think>") {
		if endIdx := strings.Index(s, "</think>"); endIdx != -1 {
			s = strings.TrimSpace(s[endIdx+8:])
		}
	}

	// Extract ```json ... ``` code fence anywhere in text
	if idx := strings.Index(s, "```json"); idx != -1 {
		body := s[idx+7:]
		if endIdx := strings.Index(body, "```"); endIdx != -1 {
			return strings.TrimSpace(body[:endIdx])
		}
		return strings.TrimSpace(body)
	}

	// Extract generic ``` ... ``` code fence anywhere in text
	if idx := strings.Index(s, "```"); idx != -1 {
		body := s[idx+3:]
		if newline := strings.Index(body, "\n"); newline != -1 {
			body = body[newline+1:]
		}
		if endIdx := strings.Index(body, "```"); endIdx != -1 {
			return strings.TrimSpace(body[:endIdx])
		}
		return strings.TrimSpace(body)
	}

	return strings.TrimSpace(s)
}

// FindOutermostJSON locates the outermost balanced JSON object { ... } in s.
// Properly tracks string literal bounds and escape characters.
func FindOutermostJSON(s string) string {
	s = StripMarkdownFences(s)
	start := strings.Index(s, "{")
	if start == -1 {
		return ""
	}

	depth := 0
	inString := false
	escaped := false

	for i := start; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}

	// Fallback for truncated outputs: return from start to last '}'
	last := strings.LastIndex(s, "}")
	if last > start {
		return s[start : last+1]
	}
	return ""
}

var trailingCommaRegex = regexp.MustCompile(`,(\s*[}\]])`)

// escapeControlCharsInJSON escapes raw unescaped control characters (tabs, newlines, CRs)
// located inside JSON string literals so that standard JSON unmarshaling succeeds.
func escapeControlCharsInJSON(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				if c == '\n' {
					b.WriteByte('n')
					continue
				}
				if c == '\r' {
					b.WriteByte('r')
					continue
				}
				if c == '\t' {
					b.WriteByte('t')
					continue
				}
				b.WriteByte(c)
				continue
			}
			if c == '\\' {
				escaped = true
				b.WriteByte(c)
				continue
			}
			if c == '"' {
				inString = false
				b.WriteByte(c)
				continue
			}
			if c == '\n' {
				b.WriteString(`\n`)
				continue
			}
			if c == '\r' {
				b.WriteString(`\r`)
				continue
			}
			if c == '\t' {
				b.WriteString(`\t`)
				continue
			}
			if c < 0x20 {
				b.WriteString(fmt.Sprintf(`\u%04x`, c))
				continue
			}
			b.WriteByte(c)
		} else {
			if c == '"' {
				inString = true
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}

// SanitizeJSON strips illegal trailing commas and escapes unescaped control characters in string literals.
func SanitizeJSON(s string) string {
	cleaned := trailingCommaRegex.ReplaceAllString(s, "$1")
	return escapeControlCharsInJSON(cleaned)
}


// ExtractJSON parses JSON from raw LLM output into target, applying fence stripping and sanitization.
func ExtractJSON(raw string, target any) error {
	// 1. Check all markdown json fences in reverse (later blocks are often self-corrections)
	fenceRegex := regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")
	matches := fenceRegex.FindAllStringSubmatch(raw, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		m := strings.TrimSpace(matches[i][1])
		if err := json.Unmarshal([]byte(m), target); err == nil {
			return nil
		}
		if err := json.Unmarshal([]byte(SanitizeJSON(m)), target); err == nil {
			return nil
		}
	}

	candidate := FindOutermostJSON(raw)
	if candidate == "" {
		candidate = strings.TrimSpace(raw)
	}

	var lastErr error
	// 2. Direct unmarshal attempt
	if err := json.Unmarshal([]byte(candidate), target); err == nil {
		return nil
	} else {
		lastErr = err
	}

	// 3. Trailing comma sanitized unmarshal attempt
	sanitized := SanitizeJSON(candidate)
	if err := json.Unmarshal([]byte(sanitized), target); err == nil {
		return nil
	} else {
		lastErr = err
	}

	// 4. Raw fence-stripped fallback
	strippedRaw := StripMarkdownFences(raw)
	if err := json.Unmarshal([]byte(strippedRaw), target); err == nil {
		return nil
	} else {
		lastErr = err
	}

	// 5. Raw fence-stripped sanitized fallback
	sanitizedStripped := SanitizeJSON(strippedRaw)
	if err := json.Unmarshal([]byte(sanitizedStripped), target); err == nil {
		return nil
	} else {
		lastErr = err
	}

	snippet := raw
	if len(snippet) > 100 {
		snippet = snippet[:100] + "..."
	}
	return fmt.Errorf("%w (unmarshal err: %v): cannot parse %q", ErrInvalidJSONOutput, lastErr, snippet)
}

// ============================================================================
// Card 1: GateIntent
// ============================================================================

// OperationType defines high-level actions selected by GateIntent.
type OperationType string

const (
	OpModify   OperationType = "MODIFY"
	OpCreate   OperationType = "CREATE"
	OpRefactor OperationType = "REFACTOR"
	OpExplain  OperationType = "EXPLAIN"
)

// GateIntentCard packages user request and repository outline into a bounded micro-card.
type GateIntentCard struct {
	UserPrompt       string   `json:"user_prompt"`
	RepoMapSummary   string   `json:"repo_map_summary"`
	FocusFiles       []string `json:"focus_files,omitempty"`
	TrajectoryRibbon string   `json:"trajectory_ribbon,omitempty"`
}

// IntentDecision captures the high-level intent, affected files, and rationale.
type IntentDecision struct {
	Operation   OperationType `json:"operation"`
	TargetFiles []string      `json:"target_files"`
	Rationale   string        `json:"rationale"`
	Confidence  float64       `json:"confidence,omitempty"`
}

// FormatPrompt constructs system and user prompts bounded strictly below 300 tokens.
func (c *GateIntentCard) FormatPrompt() (systemPrompt, userPrompt string, err error) {
	systemPrompt = "You are OmniCoder GateIntent. Given prompt and repo map, decide operation (MODIFY, CREATE, REFACTOR, EXPLAIN) and target files. Output strictly JSON: {\"operation\":\"...\",\"target_files\":[\"...\"],\"rationale\":\"...\"}. Subfolder Policy: All application code must be placed in projects/<project-name>/ (e.g. projects/my-app/...). Never place project code in root pkg/ or cmd/."

	var sb strings.Builder
	sb.WriteString("User Task: ")

	sysTokens := EstimateTokens(systemPrompt)
	availableTokens := MaxCardTokenBudget - sysTokens - 15
	if availableTokens < 60 {
		availableTokens = 60
	}

	promptBudget := availableTokens / 2
	repoBudget := availableTokens - promptBudget

	clampedPrompt := clampTextToBudget("User Task: ", c.UserPrompt, sysTokens+promptBudget)
	sb.WriteString(clampedPrompt)

	if len(c.FocusFiles) > 0 {
		sb.WriteString("\nFocus: " + strings.Join(c.FocusFiles, ", "))
	}

	if c.TrajectoryRibbon != "" {
		sb.WriteString("\nTrajectory: " + c.TrajectoryRibbon)
	}

	if c.RepoMapSummary != "" {
		sb.WriteString("\nRepo Outline:\n")
		clampedRepo := clampTextToBudget(systemPrompt+" "+sb.String(), c.RepoMapSummary, sysTokens+promptBudget+repoBudget)
		sb.WriteString(clampedRepo)
	}

	userPrompt = sb.String()
	if err := ValidateTokenBudget(systemPrompt, userPrompt, MaxCardTokenBudget); err != nil {
		userPrompt = clampTextToBudget(systemPrompt+" User Task: ", c.UserPrompt, MaxCardTokenBudget-10)
	}
	return systemPrompt, userPrompt, nil
}

// ============================================================================
// Card 2: GateEdit
// ============================================================================

// EditBlockData represents a search-and-replace text block pair.
type EditBlockData struct {
	FilePath    string `json:"file_path,omitempty"`
	SearchText  string `json:"search_text"`
	ReplaceText string `json:"replace_text"`
}

// GateEditCard packages targeted file context and AST neighborhood.
type GateEditCard struct {
	FilePath         string   `json:"file_path"`
	Instruction      string   `json:"instruction"`
	ContextSnippet   string   `json:"context_snippet"`
	ASTNeighborhood  []string `json:"ast_neighborhood,omitempty"`
	ASTRibbon        string   `json:"ast_ribbon,omitempty"`
	TrajectoryRibbon string   `json:"trajectory_ribbon,omitempty"`
	UnifiedDiffMode  bool     `json:"unified_diff_mode,omitempty"`
}

// EditDecision contains synthesized edits adhering to ICX invariants.
type EditDecision struct {
	FilePath    string          `json:"file_path"`
	EditBlocks  []EditBlockData `json:"edit_blocks,omitempty"`
	UnifiedDiff string          `json:"unified_diff,omitempty"`
	Explanation string          `json:"explanation,omitempty"`
}

// SanitizeRibbonRoot sanitizes and bounds rootName so the base <nav:ribbon: root=... hops=5 | ... /> tag never exceeds safe token limits.
func SanitizeRibbonRoot(raw string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 40
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "root"
	}
	raw = strings.TrimPrefix(raw, "file_")
	if raw == "" {
		return "root"
	}
	if len(raw) <= maxChars {
		return raw
	}
	clean := filepath.ToSlash(raw)
	base := filepath.Base(raw)
	if base != "." && base != "/" && len(base) <= maxChars-4 {
		return ".../" + base
	}
	if len(clean) > maxChars {
		if maxChars <= 3 {
			return clean[len(clean)-maxChars:]
		}
		return "..." + clean[len(clean)-(maxChars-3):]
	}
	return clean
}

// FormatASTRibbonFromStrings compresses a slice of AST neighborhood strings into a dense <nav:ribbon> representation strictly < maxTokens.
func FormatASTRibbonFromStrings(rootName string, items []string, maxTokens int) string {
	if maxTokens <= 0 {
		maxTokens = 150
	}
	root := SanitizeRibbonRoot(rootName, 40)
	basePrefix := fmt.Sprintf("<nav:ribbon: root=%q hops=5 | ", root)
	if len(items) == 0 {
		return basePrefix + "isolated />"
	}

	var formattedItems []string
	seen := make(map[string]bool)
	for _, it := range items {
		itClean := strings.TrimSpace(it)
		if itClean == "" || seen[itClean] {
			continue
		}
		seen[itClean] = true
		formattedItems = append(formattedItems, itClean)
	}

	if len(formattedItems) == 0 {
		return basePrefix + "isolated />"
	}

	var kept []string
	for i, it := range formattedItems {
		candidate := basePrefix + strings.Join(append(kept, it), " > ") + " />"
		if EstimateTokens(candidate) >= maxTokens {
			moreCount := len(formattedItems) - i
			if moreCount > 0 {
				kept = append(kept, fmt.Sprintf("[+%d_more]", moreCount))
			}
			break
		}
		kept = append(kept, it)
	}

	if len(kept) == 0 && len(formattedItems) > 0 {
		kept = append(kept, formattedItems[0])
	}

	res := basePrefix + strings.Join(kept, " > ") + " />"
	for EstimateTokens(res) >= maxTokens && len(kept) > 1 {
		kept = kept[:len(kept)-1]
		res = basePrefix + strings.Join(kept, " > ") + " [clamped] />"
	}
	if EstimateTokens(res) >= maxTokens {
		res = fmt.Sprintf("<nav:ribbon: root=%q hops=5 | [clamped] />", root)
	}
	for EstimateTokens(res) >= maxTokens {
		if len(root) > 8 {
			root = root[:len(root)/2] + "..."
			res = fmt.Sprintf("<nav:ribbon: root=%q hops=5 | [clamped] />", root)
		} else {
			res = "<nav:ribbon: [clamped] />"
			break
		}
	}
	return res
}

// FormatPrompt constructs system and user prompts bounded strictly below 300 tokens.
func (c *GateEditCard) FormatPrompt() (systemPrompt, userPrompt string, err error) {
	systemPrompt = "You are OmniCoder GateEdit. Synthesize minimal search/replace edit blocks to fulfill instruction. Maintain existing signatures. Output strictly JSON: {\"file_path\":\"...\",\"edit_blocks\":[{\"search_text\":\"...\",\"replace_text\":\"...\"}],\"explanation\":\"...\"}"

	const maxTotalBudget = MaxCardTokenBudget // 300
	const targetBudget = maxTotalBudget - 5    // 295 token ceiling for safety margin

	sysTokens := EstimateTokens(systemPrompt)
	availUserTokens := targetBudget - sysTokens // typically ~225-228 tokens

	// 1. Mandatory File header
	filePath := c.FilePath
	if filePath == "" {
		filePath = "unknown"
	}
	if len(filePath) > 60 {
		filePath = SanitizeRibbonRoot(filePath, 60)
	}
	fileHeader := fmt.Sprintf("File: %s\n", filePath)
	fileTokens := EstimateTokens(fileHeader)

	remaining := availUserTokens - fileTokens
	if remaining < 40 {
		remaining = 40
	}

	hasCode := strings.TrimSpace(c.ContextSnippet) != ""

	// 2. Resolve AST context: prioritize ASTRibbon, or format ASTNeighborhood as ribbon
	astText := ""
	maxASTRibbonTokens := 150
	if hasCode {
		maxASTRibbonTokens = 40 // Bounded AST to prevent code snippet starvation
	}
	if c.ASTRibbon != "" {
		astText = c.ASTRibbon
	} else if len(c.ASTNeighborhood) > 0 {
		if len(c.ASTNeighborhood) == 1 && strings.HasPrefix(c.ASTNeighborhood[0], "<nav:ribbon") {
			astText = c.ASTNeighborhood[0]
		} else {
			astText = FormatASTRibbonFromStrings(c.FilePath, c.ASTNeighborhood, maxASTRibbonTokens)
		}
	}

	maxASTLimit := 155
	if hasCode {
		maxASTLimit = 40
	}
	if astText != "" && EstimateTokens(astText) > maxASTLimit {
		astText = clampTextToBudget("AST: ", astText, maxASTLimit)
	}

	// 3. Dynamic budgeting for Instruction
	rawInst := c.Instruction
	instTokens := EstimateTokens("Instruction: " + rawInst + "\n")
	maxInstructionTokens := 140
	if hasCode {
		maxInstructionTokens = 65 // Reserve generous budget for code snippet
	}
	instructionText := rawInst
	if instTokens > maxInstructionTokens {
		instructionText = clampTextToBudget("Instruction: ", rawInst, maxInstructionTokens)
		if !strings.HasSuffix(instructionText, "[clamped...]") && !strings.HasSuffix(instructionText, "...") {
			instructionText += " [clamped...]"
		}
	}
	instHeader := fmt.Sprintf("Instruction: %s\n", instructionText)
	instTokens = EstimateTokens(instHeader)

	remaining = availUserTokens - fileTokens - instTokens

	// 4. Fit AST ribbon into remaining budget
	astHeader := ""
	if astText != "" {
		astHeader = "AST: " + astText + "\n"
		astTokens := EstimateTokens(astHeader)
		budgetForAST := remaining - 15
		if hasCode && budgetForAST > 40 {
			budgetForAST = 40
		}
		if astTokens > budgetForAST && remaining > 30 {
			maxAST := budgetForAST
			if maxAST < 25 {
				maxAST = 25
			}
			clampedAST := clampTextToBudget("AST: ", astText, maxAST)
			if clampedAST != "" {
				astHeader = "AST: " + clampedAST + "\n"
			} else {
				astHeader = "AST: <nav:ribbon: [clamped] />\n"
			}
		} else if remaining <= 30 {
			astHeader = "AST: <nav:ribbon: [clamped] />\n"
		}
		remaining -= EstimateTokens(astHeader)
	}

	// 5. Fit Code snippet into remaining budget with guaranteed allocation
	codeHeader := ""
	if hasCode {
		codePrefix := "Code:\n"
		var focusSyms []string
		for _, w := range strings.Fields(c.Instruction) {
			wClean := strings.Trim(w, "`'\",.:;()[]{}")
			if len(wClean) > 3 {
				focusSyms = append(focusSyms, wClean)
			}
		}
		for _, s := range c.ASTNeighborhood {
			focusSyms = append(focusSyms, s)
		}

		foldedSnippet := FoldUnreferencedAST(c.ContextSnippet, -1, focusSyms, filepath.Ext(c.FilePath))
		focusedSnippet := extractFocusedSnippet(foldedSnippet, c.Instruction)
		if remaining > 10 {
			clampedSnippet := clampTextToBudget(systemPrompt+" "+fileHeader+instHeader+astHeader+codePrefix, focusedSnippet, targetBudget)
			codeHeader = codePrefix + clampedSnippet
		} else {
			codeHeader = codePrefix + "[omitted for budget]\n"
		}
	}

	// 6. Assemble userPrompt
	var sb strings.Builder
	sb.WriteString(fileHeader)
	sb.WriteString(instHeader)
	if c.TrajectoryRibbon != "" {
		trajText := c.TrajectoryRibbon
		if EstimateTokens(trajText) > 40 {
			trajText = clampTextToBudget("Trajectory: ", trajText, 40)
		}
		sb.WriteString("Trajectory: " + trajText + "\n")
	}
	if astHeader != "" {
		sb.WriteString(astHeader)
	}
	if codeHeader != "" {
		sb.WriteString(codeHeader)
	}
	userPrompt = sb.String()

	// 7. Strict Invariant Guarantee: Total tokens < MaxCardTokenBudget (300)
	// Progressively trim from secondary components (Trajectory first, then AST ribbon, then instruction tail,
	// preserving the code snippet so search/replace anchors are not lost).
	// "File: " and "Instruction: " headers are NEVER stripped.
	for EstimateTokens(systemPrompt+" "+userPrompt) >= MaxCardTokenBudget {
		if trajIdx := strings.Index(userPrompt, "Trajectory: "); trajIdx != -1 {
			trajEnd := strings.Index(userPrompt[trajIdx:], "\n")
			if trajEnd != -1 {
				userPrompt = userPrompt[:trajIdx] + userPrompt[trajIdx+trajEnd+1:]
			} else {
				userPrompt = userPrompt[:trajIdx]
			}
			continue
		}
		if astIdx := strings.Index(userPrompt, "AST: "); astIdx != -1 {
			astEnd := strings.Index(userPrompt[astIdx:], "\n")
			if astEnd != -1 {
				userPrompt = userPrompt[:astIdx] + userPrompt[astIdx+astEnd+1:]
			} else {
				userPrompt = userPrompt[:astIdx]
			}
			continue
		}
		if instIdx := strings.Index(userPrompt, "Instruction: "); instIdx != -1 {
			instEnd := strings.Index(userPrompt[instIdx:], "\n")
			var instContent string
			if instEnd != -1 {
				instContent = userPrompt[instIdx+13 : instIdx+instEnd]
			} else {
				instContent = userPrompt[instIdx+13:]
			}
			if len(instContent) > 35 {
				cutLen := len(instContent) * 3 / 4
				if instEnd != -1 {
					userPrompt = userPrompt[:instIdx+13] + instContent[:cutLen] + "... [clamped]" + userPrompt[instIdx+instEnd:]
				} else {
					userPrompt = userPrompt[:instIdx+13] + instContent[:cutLen] + "... [clamped]"
				}
				continue
			}
		}
		if codeIdx := strings.Index(userPrompt, "Code:\n"); codeIdx != -1 {
			codePart := userPrompt[codeIdx+6:]
			if len(codePart) > 30 {
				userPrompt = userPrompt[:codeIdx+6+len(codePart)*3/4] + "...\n"
				continue
			}
			userPrompt = strings.TrimSpace(userPrompt[:codeIdx]) + "\n"
			continue
		}
		break
	}

	return systemPrompt, userPrompt, nil
}

// ============================================================================
// Card 3: GateHeal
// ============================================================================

// GateHealCard packages compiler error diagnostics and the 5-line context window.
type GateHealCard struct {
	FilePath         string `json:"file_path"`
	ErrorLine        int    `json:"error_line"`
	ErrorMessage     string `json:"error_message"`
	Snippet5Lines    string `json:"snippet_5_lines"` // 5-line window: > 42: offending code
	AltSnippet5Lines string `json:"alt_snippet_5_lines,omitempty"`
	Command          string `json:"command"`
	ErrorCode        string `json:"error_code,omitempty"`
	IssueContext     string `json:"issue_context,omitempty"`
	PriorTurnPatch   string `json:"prior_turn_patch,omitempty"`
	PriorTurnOutcome string `json:"prior_turn_outcome,omitempty"`
	TokenBudget      int    `json:"token_budget,omitempty"`
}

// HealDecision contains surgical repair diffs and root cause diagnostics.
type HealDecision struct {
	FilePath      string          `json:"file_path"`
	EditBlocks    []EditBlockData `json:"edit_blocks,omitempty"`
	RepairSummary string          `json:"repair_summary"`
	RootCause     string          `json:"root_cause,omitempty"`
}

// FormatPrompt constructs system and user prompts bounded strictly below the token budget.
func (c *GateHealCard) FormatPrompt() (systemPrompt, userPrompt string, err error) {
	budget := MaxCardTokenBudget
	if c.TokenBudget > 0 {
		budget = c.TokenBudget
	}

	if budget <= 300 {
		systemPrompt = "You are OmniCoder GateHeal. Output minimal search/replace JSON: {\"file_path\":\"...\",\"edit_blocks\":[{\"search_text\":\"...\",\"replace_text\":\"...\"}],\"repair_summary\":\"...\"}"
	} else {
		systemPrompt = "You are OmniCoder GateHeal. Analyze the test failure, error diagnostic, and context. Synthesize surgical, minimal search/replace edit blocks to fix the bug. CRITICAL RULES:\n1. 'search_text' must be MINIMAL: include ONLY the specific 3 to 10 consecutive lines being modified, plus 1 line of unchanged context. NEVER reproduce the whole function or class.\n2. 'replace_text' must contain the complete, working production code replacement. NEVER output placeholder comments, 'pass', or ellipses.\n3. Output strictly valid JSON: {\"root_cause\":\"1-sentence root cause analysis\",\"file_path\":\"...\",\"edit_blocks\":[{\"file_path\":\"...\",\"search_text\":\"...\",\"replace_text\":\"...\"}],\"repair_summary\":\"...\"}."
	}

	var sb strings.Builder
	cleanCmd := strings.TrimSpace(c.Command)
	if strings.HasPrefix(cleanCmd, "python -m pytest ") {
		cleanCmd = "pytest " + cleanCmd[len("python -m pytest "):]
	}
	if len(cleanCmd) > 50 {
		cleanCmd = cleanCmd[:47] + "..."
	}

	cleanErr := strings.TrimSpace(c.ErrorMessage)
	if resIdx := strings.Index(cleanErr, "<Result "); resIdx != -1 {
		cleanErr = strings.TrimSuffix(cleanErr[resIdx+len("<Result "):], ">.exit_code")
	} else if idx := strings.Index(cleanErr, " (test failure: "); idx != -1 {
		mainMsg := strings.TrimSpace(cleanErr[:idx])
		hint := cleanErr[idx+len(" (test failure: "):]
		if endIdx := strings.LastIndex(hint, ")"); endIdx != -1 {
			hint = hint[:endIdx]
		}
		if hint != "" {
			cleanErr = hint
		} else if mainMsg != "" {
			cleanErr = mainMsg
		}
	} else if idx := strings.Index(cleanErr, "AssertionError: "); idx != -1 {
		cleanErr = cleanErr[idx+len("AssertionError: "):]
	}

	maxErrLen := 160
	maxIssueLen := 50
	if budget > 300 {
		maxErrLen = 500
		maxIssueLen = 1500
		if budget >= 4096 {
			maxIssueLen = 3500
		}
	}

	if len(cleanErr) > maxErrLen {
		cleanErr = cleanErr[:maxErrLen-3] + "..."
	}
	sb.WriteString(fmt.Sprintf("Cmd: %s\nError in `%s:%d`: %s\n", cleanCmd, c.FilePath, c.ErrorLine, cleanErr))
	if c.IssueContext != "" {
		cleanIssue := strings.TrimSpace(c.IssueContext)
		if len(cleanIssue) > maxIssueLen {
			cleanIssue = cleanIssue[:maxIssueLen-3] + "..."
		}
		sb.WriteString(fmt.Sprintf("Issue: %s\n", cleanIssue))
	}

	if c.PriorTurnPatch != "" {
		cleanPatch := strings.TrimSpace(c.PriorTurnPatch)
		if len(cleanPatch) > 1200 {
			cleanPatch = cleanPatch[:1197] + "..."
		}
		sb.WriteString(fmt.Sprintf("Prior Attempt Patch:\n```diff\n%s\n```\n", cleanPatch))
		if c.PriorTurnOutcome != "" {
			cleanOutcome := strings.TrimSpace(c.PriorTurnOutcome)
			if len(cleanOutcome) > 400 {
				cleanOutcome = cleanOutcome[:397] + "..."
			}
			if strings.Contains(cleanOutcome, "rolled back") {
				sb.WriteString(fmt.Sprintf("Prior Attempt Outcome: %s\nIMPORTANT: The prior attempt patch above was REJECTED and rolled back because it did not fix the failure. DO NOT repeat the exact same patch! Try a different approach or fix the missing logic/method.\n", cleanOutcome))
			} else {
				sb.WriteString(fmt.Sprintf("Prior Attempt Outcome: %s\nGoal: Refine the patch to resolve this new error without reverting the valid fix from the prior turn.\n", cleanOutcome))
			}
		}
	}

	base := systemPrompt + " " + sb.String()
	clampedSnippet := clampTextToBudget(base+"Context:\n", c.Snippet5Lines, budget)
	sb.WriteString("Context:\n" + clampedSnippet)

	userPrompt = sb.String()
	if err := ValidateTokenBudget(systemPrompt, userPrompt, budget); err != nil {
		userPrompt = clampTextToBudget(systemPrompt+" Cmd: "+cleanCmd+"\nErr: "+cleanErr+"\n", c.Snippet5Lines, budget)
	}
	return systemPrompt, userPrompt, nil
}

// ============================================================================
// Card 4: GateFinalize
// ============================================================================

// GateFinalizeCard packages test outputs and task summary for final commit approval.
type GateFinalizeCard struct {
	TaskDescription   string   `json:"task_description"`
	FilesModified     []string `json:"files_modified"`
	TestCommand       string   `json:"test_command"`
	TestExitCode      int      `json:"test_exit_code"`
	TestOutputSummary string   `json:"test_output_summary"`
}

// FinalizeDecision captures task completion confirmation and structured Git commit metadata.
type FinalizeDecision struct {
	Approved       bool     `json:"approved"`
	CommitMessage  string   `json:"commit_message"`
	Summary        string   `json:"summary"`
	FilesCommitted []string `json:"files_committed,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}

// FormatPrompt constructs system and user prompts bounded strictly below 300 tokens.
func (c *GateFinalizeCard) FormatPrompt() (systemPrompt, userPrompt string, err error) {
	systemPrompt = "You are OmniCoder GateFinalize. Verify tests pass. If exit_code=0, approve and produce a Conventional Commit message. Output strictly JSON: {\"approved\":true,\"commit_message\":\"type(scope): msg\",\"summary\":\"...\"}"

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Task: %s\nFiles: %s\nTest: `%s` (Exit: %d)\n",
		c.TaskDescription, strings.Join(c.FilesModified, ", "), c.TestCommand, c.TestExitCode))

	base := systemPrompt + " " + sb.String()
	clampedSummary := clampTextToBudget(base+"Output:\n", c.TestOutputSummary, MaxCardTokenBudget-10)
	sb.WriteString("Output:\n" + clampedSummary)

	userPrompt = sb.String()
	if err := ValidateTokenBudget(systemPrompt, userPrompt, MaxCardTokenBudget); err != nil {
		userPrompt = clampTextToBudget(systemPrompt+" Task: "+c.TaskDescription+"\nExit: "+strconv.Itoa(c.TestExitCode)+"\n", c.TestOutputSummary, MaxCardTokenBudget-10)
	}
	return systemPrompt, userPrompt, nil
}

// ============================================================================
// Card 5: GateReproducer
// ============================================================================

// GateReproducerCard packages bug descriptions and repository context into a bounded
// micro-card strictly under 300 tokens for synthesizing standalone reproducer scripts.
type GateReproducerCard struct {
	IssueText        string   `json:"issue_text"`
	RepoSummary      string   `json:"repo_summary,omitempty"`
	CausalFiles      []string `json:"causal_files,omitempty"`
	Language         string   `json:"language,omitempty"`
	TracebackSnippet string   `json:"traceback_snippet,omitempty"`
	TokenBudget      int      `json:"token_budget,omitempty"`
}

// ReproducerDecision captures the synthesized standalone reproduction script.
type ReproducerDecision struct {
	ScriptName    string `json:"script_name"`
	ScriptContent string `json:"script_content"`
	Explanation   string `json:"explanation,omitempty"`
	Language      string `json:"language,omitempty"`
}

// FormatPrompt constructs system and user prompts bounded strictly below 300 tokens (or c.TokenBudget when set).
func (c *GateReproducerCard) FormatPrompt() (systemPrompt, userPrompt string, err error) {
	budget := MaxCardTokenBudget
	if c.TokenBudget > 0 {
		budget = c.TokenBudget
	}
	systemPrompt = "You are OmniCoder GateReproducer. Synthesize a minimal, standalone reproducer script (e.g. reproduce_issue.py). Protocol: exit 1 if bug present, exit 0 if resolved. Must execute <2.5s without network. Output strictly JSON: {\"script_name\":\"reproduce_issue.py\",\"script_content\":\"...\",\"explanation\":\"...\"}"

	var sb strings.Builder
	cleanLang := strings.TrimSpace(c.Language)
	if cleanLang == "" {
		cleanLang = "python"
	}
	sb.WriteString(fmt.Sprintf("Lang: %s\n", cleanLang))

	if len(c.CausalFiles) > 0 {
		sb.WriteString("Files: " + strings.Join(c.CausalFiles, ", ") + "\n")
	}

	if c.RepoSummary != "" && budget > 300 {
		cleanRepo := strings.TrimSpace(c.RepoSummary)
		if len(cleanRepo) > 1200 {
			cleanRepo = cleanRepo[:1197] + "..."
		}
		sb.WriteString("Context:\n" + cleanRepo + "\n")
	}

	if c.TracebackSnippet != "" {
		cleanTrace := strings.TrimSpace(c.TracebackSnippet)
		maxTrace := 80
		if budget > 300 {
			maxTrace = 600
		}
		if len(cleanTrace) > maxTrace {
			cleanTrace = cleanTrace[:maxTrace-3] + "..."
		}
		sb.WriteString("Traceback: " + cleanTrace + "\n")
	}

	base := systemPrompt + " " + sb.String()
	clampedIssue := clampTextToBudget(base+"Issue: ", c.IssueText, budget-10)
	sb.WriteString("Issue: " + clampedIssue)

	userPrompt = sb.String()
	if err := ValidateTokenBudget(systemPrompt, userPrompt, budget); err != nil {
		userPrompt = clampTextToBudget(systemPrompt+" Issue: ", c.IssueText, budget-10)
	}
	return systemPrompt, userPrompt, nil
}

// ============================================================================
// Unified DecisionGate Interface & Aliases
// ============================================================================

// Type aliases for DecisionGate decision payloads.
type GateIntentDecision = IntentDecision
type GateEditDecision = EditDecision
type GateHealDecision = HealDecision
type GateFinalizeDecision = FinalizeDecision
type GateReproducerDecision = ReproducerDecision

// DecisionGate defines the micro-decision evaluation contracts.
type DecisionGate interface {
	DecideIntent(ctx context.Context, card GateIntentCard) (*IntentDecision, error)
	DecideEdit(ctx context.Context, card GateEditCard) (*EditDecision, error)
	DecideHeal(ctx context.Context, card GateHealCard) (*HealDecision, error)
	DecideHealSpeculative(ctx context.Context, card GateHealCard, k int) ([]*HealDecision, error)
	DecideFinalize(ctx context.Context, card GateFinalizeCard) (*FinalizeDecision, error)
	DecideReproducer(ctx context.Context, card GateReproducerCard) (*ReproducerDecision, error)

	// Convenience wrappers matching DecisionGate interface signatures
	Intent(ctx context.Context, card GateIntentCard) (*GateIntentDecision, error)
	Edit(ctx context.Context, card GateEditCard) (*GateEditDecision, error)
	Heal(ctx context.Context, card GateHealCard) (*GateHealDecision, error)
	Finalize(ctx context.Context, card GateFinalizeCard) (*GateFinalizeDecision, error)
	Reproducer(ctx context.Context, card GateReproducerCard) (*GateReproducerDecision, error)
}

// ============================================================================
// Production Implementation: LLMDecisionGate
// ============================================================================

// LLMDecisionGate implements DecisionGate backed by an LLMClient.
type LLMDecisionGate struct {
	client LLMClient
	model  string
}

// Ensure LLMDecisionGate implements DecisionGate.
var _ DecisionGate = (*LLMDecisionGate)(nil)

// NewDecisionGate constructs an LLMDecisionGate with client and target model.
func NewDecisionGate(client LLMClient, model string) *LLMDecisionGate {
	if model == "" && client != nil {
		model = client.Config().Model
	}
	if model == "" {
		model = DefaultModel
	}
	return &LLMDecisionGate{
		client: client,
		model:  model,
	}
}

// DecideIntent evaluates repository state and prompt to determine action and target files.
func (g *LLMDecisionGate) DecideIntent(ctx context.Context, card GateIntentCard) (*IntentDecision, error) {
	if g.client == nil {
		return nil, errors.New("llm: decision gate client is nil")
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		return nil, err
	}

	temp := 0.1
	var respFmt *ResponseFormat
	if IsGeminiModel(g.model) {
		respFmt = &ResponseFormat{Type: "json_object"}
	}
	req := &ChatCompletionRequest{
		Model: g.model,
		Messages: []ChatMessage{
			{Role: "system", Content: sysPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature:     &temp,
		ResponseFormat:  respFmt,
	}

	resp, err := g.client.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("llm: GateIntent completion error: %w", err)
	}

	var decision IntentDecision
	if err := ExtractJSON(resp.Content(), &decision); err != nil {
		return nil, fmt.Errorf("llm: GateIntent json extraction error: %w", err)
	}

	// Normalization & Validation
	if decision.Operation == "" {
		if len(decision.TargetFiles) > 0 {
			decision.Operation = OpModify
		} else {
			decision.Operation = OpExplain
		}
	}
	for i := range decision.TargetFiles {
		decision.TargetFiles[i] = filepath.ToSlash(decision.TargetFiles[i])
	}

	return &decision, nil
}

// DecideEdit evaluates file snippet and instruction to generate search/replace blocks.
func (g *LLMDecisionGate) DecideEdit(ctx context.Context, card GateEditCard) (*EditDecision, error) {
	if g.client == nil {
		return nil, errors.New("llm: decision gate client is nil")
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		return nil, err
	}

	temp := 0.2
	var respFmt *ResponseFormat
	if IsGeminiModel(g.model) {
		respFmt = &ResponseFormat{Type: "json_object"}
	}
	req := &ChatCompletionRequest{
		Model: g.model,
		Messages: []ChatMessage{
			{Role: "system", Content: sysPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature:     &temp,
		MaxTokens:       1536,
		ResponseFormat:  respFmt,
	}

	resp, err := g.client.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("llm: GateEdit completion error: %w", err)
	}

	var decision EditDecision
	if err := ExtractJSON(resp.Content(), &decision); err != nil {
		// Resilient fallback: parse raw Aider search/replace blocks from content
		blocks, parseErr := devops.ParseAiderEditBlocks(resp.Content(), card.FilePath)
		if parseErr == nil && len(blocks) > 0 {
			decision.FilePath = card.FilePath
			for _, b := range blocks {
				decision.EditBlocks = append(decision.EditBlocks, EditBlockData{
					SearchText:  b.SearchText,
					ReplaceText: b.ReplaceText,
				})
			}
			decision.Explanation = "parsed from raw edit blocks"
		} else if diffs, diffErr := devops.ParseUnifiedDiff(resp.Content()); diffErr == nil && len(diffs) > 0 {
			decision.FilePath = card.FilePath
			decision.UnifiedDiff = resp.Content()
			for _, d := range diffs {
				for _, h := range d.Hunks {
					var searchLines, replaceLines []string
					for _, l := range h.Lines {
						if len(l) == 0 {
							continue
						}
						switch l[0] {
						case ' ':
							searchLines = append(searchLines, l[1:])
							replaceLines = append(replaceLines, l[1:])
						case '-':
							searchLines = append(searchLines, l[1:])
						case '+':
							replaceLines = append(replaceLines, l[1:])
						}
					}
					decision.EditBlocks = append(decision.EditBlocks, EditBlockData{
						SearchText:  strings.Join(searchLines, "\n"),
						ReplaceText: strings.Join(replaceLines, "\n"),
					})
				}
			}
			decision.Explanation = "parsed from raw unified diff"
		} else {
			return nil, fmt.Errorf("llm: GateEdit json extraction error: %w", err)
		}
	}

	cleanCardFP := filepath.ToSlash(card.FilePath)
	cleanDecFP := filepath.ToSlash(decision.FilePath)
	if cleanDecFP == "" || cleanDecFP != cleanCardFP {
		if cleanCardFP != "" && (cleanDecFP == "" || filepath.Base(cleanDecFP) == filepath.Base(cleanCardFP) || strings.HasSuffix(cleanCardFP, "/"+cleanDecFP) || !strings.Contains(cleanDecFP, "/")) {
			decision.FilePath = cleanCardFP
		} else if cleanCardFP != "" {
			decision.FilePath = cleanCardFP
		}
	}
	decision.FilePath = filepath.ToSlash(decision.FilePath)

	return &decision, nil
}

// DecideHeal evaluates compiler error diagnostics and synthesizes surgical repairs.
func (g *LLMDecisionGate) DecideHeal(ctx context.Context, card GateHealCard) (*HealDecision, error) {
	if g.client == nil {
		return nil, errors.New("llm: decision gate client is nil")
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(os.Stderr, "  [GateHeal UserPrompt Sent to LLM]:\n%s\n", userPrompt)

	temp := 0.1
	var respFmt *ResponseFormat
	if IsGeminiModel(g.model) {
		respFmt = &ResponseFormat{Type: "json_object"}
	}
	promptTokens := EstimateTokens(sysPrompt + " " + userPrompt)
	if codeEst := int(float64(len(sysPrompt)+len(userPrompt)) / 2.60); codeEst > promptTokens {
		promptTokens = codeEst
	}
	maxTokens := 1024
	if 8192-promptTokens-128 < maxTokens {
		maxTokens = 8192 - promptTokens - 128
		if maxTokens < 384 {
			maxTokens = 384
		}
	}

	req := &ChatCompletionRequest{
		Model: g.model,
		Messages: []ChatMessage{
			{Role: "system", Content: sysPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature:     &temp,
		MaxTokens:       maxTokens,
		ResponseFormat:  respFmt,
	}

	resp, err := g.client.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("llm: GateHeal completion error: %w", err)
	}

	fmt.Fprintf(os.Stderr, "  [GateHeal Response]:\n%s\n", resp.Content())

	return parseHealDecisionContent(resp.Content(), card.FilePath)
}

// DecideHealSpeculative generates up to k candidate repair solutions in parallel using continuous batching.
func (g *LLMDecisionGate) DecideHealSpeculative(ctx context.Context, card GateHealCard, k int) ([]*HealDecision, error) {
	if k <= 1 {
		dec, err := g.DecideHeal(ctx, card)
		if err != nil {
			return nil, err
		}
		return []*HealDecision{dec}, nil
	}

	if g.client == nil {
		return nil, errors.New("llm: decision gate client is nil")
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(os.Stderr, "  [GateHealSpeculative UserPrompt Sent to LLM (k=%d)]:\n%s\n", k, userPrompt)

	temp := 0.6
	topP := 0.95
	var respFmt *ResponseFormat
	if IsGeminiModel(g.model) {
		respFmt = &ResponseFormat{Type: "json_object"}
	}
	promptTokens := EstimateTokens(sysPrompt + " " + userPrompt)
	if codeEst := int(float64(len(sysPrompt)+len(userPrompt)) / 2.60); codeEst > promptTokens {
		promptTokens = codeEst
	}
	maxTokens := 1024
	if 8192-promptTokens-128 < maxTokens {
		maxTokens = 8192 - promptTokens - 128
		if maxTokens < 384 {
			maxTokens = 384
		}
	}

	kPrimary := k
	hasAlt := strings.TrimSpace(card.AltSnippet5Lines) != "" && k >= 2
	if hasAlt {
		kPrimary = k - 1
	}

	req := &ChatCompletionRequest{
		Model: g.model,
		Messages: []ChatMessage{
			{Role: "system", Content: sysPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature:    &temp,
		TopP:           &topP,
		MaxTokens:      maxTokens,
		ResponseFormat: respFmt,
		N:              kPrimary,
	}

	var resp *ChatCompletionResponse
	var altResp *ChatCompletionResponse
	if hasAlt {
		altCard := card
		altCard.Snippet5Lines = card.AltSnippet5Lines
		altCard.AltSnippet5Lines = ""
		altSys, altUser, altFmtErr := altCard.FormatPrompt()
		if altFmtErr == nil {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				resp, err = g.client.Complete(ctx, req)
			}()
			go func() {
				defer wg.Done()
				altTemp := 0.35
				altReq := &ChatCompletionRequest{
					Model: g.model,
					Messages: []ChatMessage{
						{Role: "system", Content: altSys},
						{Role: "user", Content: altUser},
					},
					Temperature:    &altTemp,
					TopP:           &topP,
					MaxTokens:      maxTokens,
					ResponseFormat: respFmt,
					N:              1,
				}
				altResp, _ = g.client.Complete(ctx, altReq)
			}()
			wg.Wait()
		} else {
			req.N = k
			resp, err = g.client.Complete(ctx, req)
		}
	} else {
		resp, err = g.client.Complete(ctx, req)
	}
	if err != nil && altResp == nil {
		return nil, fmt.Errorf("llm: GateHealSpeculative completion error: %w", err)
	}

	var allChoices []Choice
	if resp != nil {
		allChoices = append(allChoices, resp.Choices...)
	}
	if altResp != nil {
		allChoices = append(allChoices, altResp.Choices...)
	}

	var decisions []*HealDecision
	for idx, choice := range allChoices {
		content := choice.Message.Content
		if strings.TrimSpace(content) == "" {
			content = choice.Message.ReasoningContent
		}
		dec, parseErr := parseHealDecisionContent(content, card.FilePath)
		if parseErr == nil && dec != nil {
			profileName := fmt.Sprintf("Candidate %d", idx+1)
			switch idx {
			case 0:
				profileName = "Candidate Alpha (Minimal)"
			case 1:
				profileName = "Candidate Beta (Structural)"
			case 2:
				profileName = "Candidate Gamma (Alternative)"
			}
			dec.RepairSummary = fmt.Sprintf("[%s] %s", profileName, dec.RepairSummary)
			decisions = append(decisions, dec)
		}
	}

	if len(decisions) == 0 {
		return nil, fmt.Errorf("llm: GateHealSpeculative failed to parse any candidate completions (%d returned)", len(allChoices))
	}

	return decisions, nil
}

// parseHealDecisionContent parses LLM completion text into a HealDecision via JSON, Aider blocks, or diffs.
func parseHealDecisionContent(content string, defaultFilePath string) (*HealDecision, error) {
	var decision HealDecision
	if err := ExtractJSON(content, &decision); err != nil {
		// Resilient fallback: parse raw Aider blocks
		blocks, parseErr := devops.ParseAiderEditBlocks(content, defaultFilePath)
		if parseErr == nil && len(blocks) > 0 {
			decision.FilePath = defaultFilePath
			for _, b := range blocks {
				decision.EditBlocks = append(decision.EditBlocks, EditBlockData{
					SearchText:  b.SearchText,
					ReplaceText: b.ReplaceText,
				})
			}
			decision.RepairSummary = "repaired via raw search/replace block"
		} else if diffs, diffErr := devops.ParseUnifiedDiff(content); diffErr == nil && len(diffs) > 0 {
			decision.FilePath = defaultFilePath
			for _, d := range diffs {
				for _, h := range d.Hunks {
					var searchLines, replaceLines []string
					for _, l := range h.Lines {
						if len(l) == 0 {
							continue
						}
						switch l[0] {
						case ' ':
							searchLines = append(searchLines, l[1:])
							replaceLines = append(replaceLines, l[1:])
						case '-':
							searchLines = append(searchLines, l[1:])
						case '+':
							replaceLines = append(replaceLines, l[1:])
						}
					}
					decision.EditBlocks = append(decision.EditBlocks, EditBlockData{
						SearchText:  strings.Join(searchLines, "\n"),
						ReplaceText: strings.Join(replaceLines, "\n"),
					})
				}
			}
			decision.RepairSummary = "repaired via raw unified diff block"
		} else {
			return nil, fmt.Errorf("llm: GateHeal json extraction error: %w\n[Raw LLM Output]:\n%s", err, content)
		}
	}

	cleanCardFP := filepath.ToSlash(defaultFilePath)
	cleanDecFP := filepath.ToSlash(decision.FilePath)
	if cleanDecFP == "" {
		decision.FilePath = cleanCardFP
	} else {
		decision.FilePath = cleanDecFP
	}
	decision.FilePath = filepath.ToSlash(decision.FilePath)

	for i := range decision.EditBlocks {
		if decision.EditBlocks[i].FilePath == "" {
			decision.EditBlocks[i].FilePath = decision.FilePath
		} else {
			decision.EditBlocks[i].FilePath = filepath.ToSlash(decision.EditBlocks[i].FilePath)
		}
	}

	return &decision, nil
}

// DecideFinalize evaluates test verification outcomes and approves commit creation.
func (g *LLMDecisionGate) DecideFinalize(ctx context.Context, card GateFinalizeCard) (*FinalizeDecision, error) {
	if g.client == nil {
		return nil, errors.New("llm: decision gate client is nil")
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		return nil, err
	}

	temp := 0.1
	var respFmt *ResponseFormat
	if IsGeminiModel(g.model) {
		respFmt = &ResponseFormat{Type: "json_object"}
	}
	req := &ChatCompletionRequest{
		Model: g.model,
		Messages: []ChatMessage{
			{Role: "system", Content: sysPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature:     &temp,
		ResponseFormat:  respFmt,
	}

	resp, err := g.client.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("llm: GateFinalize completion error: %w", err)
	}

	var decision FinalizeDecision
	if err := ExtractJSON(resp.Content(), &decision); err != nil {
		contentLower := strings.ToLower(resp.Content())
		if strings.Contains(contentLower, "\"approved\": true") || strings.Contains(contentLower, "approved: true") {
			decision.Approved = true
			decision.CommitMessage = "feat: " + card.TaskDescription
		} else if strings.Contains(contentLower, "\"approved\": false") || strings.Contains(contentLower, "approved: false") {
			decision.Approved = false
			decision.Reason = "rejected by GateFinalize evaluation"
		} else if card.TestExitCode == 0 {
			decision.Approved = true
			decision.CommitMessage = "feat: " + card.TaskDescription
		} else {
			decision.Approved = false
			decision.Reason = fmt.Sprintf("test command `%s` failed with exit code %d", card.TestCommand, card.TestExitCode)
		}
	}

	// Invariant safety clamp: Failing tests MUST NEVER be approved
	if card.TestExitCode != 0 {
		decision.Approved = false
		if decision.Reason == "" {
			decision.Reason = fmt.Sprintf("test command `%s` failed with exit code %d", card.TestCommand, card.TestExitCode)
		}
	}

	// Conventional commit fallback if approved
	if decision.Approved && decision.CommitMessage == "" {
		decision.CommitMessage = "feat: " + card.TaskDescription
	}

	return &decision, nil
}

// Wrapper aliases matching DecisionGate interface signatures
func (g *LLMDecisionGate) Intent(ctx context.Context, card GateIntentCard) (*GateIntentDecision, error) {
	return g.DecideIntent(ctx, card)
}

func (g *LLMDecisionGate) Edit(ctx context.Context, card GateEditCard) (*GateEditDecision, error) {
	return g.DecideEdit(ctx, card)
}

func (g *LLMDecisionGate) Heal(ctx context.Context, card GateHealCard) (*GateHealDecision, error) {
	return g.DecideHeal(ctx, card)
}

func (g *LLMDecisionGate) Finalize(ctx context.Context, card GateFinalizeCard) (*GateFinalizeDecision, error) {
	return g.DecideFinalize(ctx, card)
}

// DecideReproducer evaluates issue description and synthesizes a standalone reproduction script.
func (g *LLMDecisionGate) DecideReproducer(ctx context.Context, card GateReproducerCard) (*ReproducerDecision, error) {
	if g.client == nil {
		return nil, errors.New("llm: decision gate client is nil")
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		return nil, err
	}

	temp := 0.1
	var respFmt *ResponseFormat
	if IsGeminiModel(g.model) {
		respFmt = &ResponseFormat{Type: "json_object"}
	}
	req := &ChatCompletionRequest{
		Model: g.model,
		Messages: []ChatMessage{
			{Role: "system", Content: sysPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature:     &temp,
		ResponseFormat:  respFmt,
	}

	resp, err := g.client.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("llm: GateReproducer completion error: %w", err)
	}

	var decision ReproducerDecision
	if err := ExtractJSON(resp.Content(), &decision); err != nil {
		// Resilient fallback: extract raw code block from content
		fenced := StripMarkdownFences(resp.Content())
		if fenced != "" {
			decision.ScriptName = "reproduce_issue.py"
			decision.ScriptContent = fenced
			decision.Explanation = "extracted from raw markdown fence"
		} else {
			return nil, fmt.Errorf("llm: GateReproducer json extraction error: %w", err)
		}
	}

	if decision.ScriptName == "" {
		decision.ScriptName = "reproduce_issue.py"
	}
	return &decision, nil
}

func (g *LLMDecisionGate) Reproducer(ctx context.Context, card GateReproducerCard) (*GateReproducerDecision, error) {
	return g.DecideReproducer(ctx, card)
}
