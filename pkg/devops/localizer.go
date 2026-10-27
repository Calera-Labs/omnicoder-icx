// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// localizer.go implements the Spectrum Fault Localizer & AST Slicer:
// - BM25 token indexer over repository symbol names
// - AST backward-slicing module connecting assertion points to source definitions
// - Multi-file candidate ranker with test-file exclusion rules
// - Weighted scoring function: Score = w1*BM25 + w2*TracebackDepth + w3*ASTDep + w4*GitChurn

package devops

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

var (
	// ErrNoDiagnostics is returned when neither issue description nor traceback is provided.
	ErrNoDiagnostics = errors.New("localizer: issue description and traceback cannot both be empty")
)

// LocalizationResult encapsulates the identified primary fault location,
// causal symbol, line range, confidence score, and supporting multi-file candidates.
type LocalizationResult struct {
	PrimaryFile     string   `json:"primary_file"`
	CausalSymbol    string   `json:"causal_symbol"`
	LineRange       [2]int   `json:"line_range"`
	Confidence      float64  `json:"confidence"`
	SupportingFiles []string `json:"supporting_files"`
	MirrorTwins     []string `json:"mirror_twins,omitempty"`
}

// TracebackFrame represents a parsed stack trace frame.
type TracebackFrame struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
	Code     string `json:"code,omitempty"`
	IsTest   bool   `json:"is_test"`
	IsStdlib bool   `json:"is_stdlib"`
}

// CandidateScore holds the multi-factor scoring breakdown for a candidate file and symbol.
type CandidateScore struct {
	File            string   `json:"file"`
	Symbol          string   `json:"symbol"`
	LineRange       [2]int   `json:"line_range"`
	BM25Score       float64  `json:"bm25_score"`
	TracebackScore  float64  `json:"traceback_score"`
	ASTScore        float64  `json:"ast_score"`
	GitChurnScore   float64  `json:"git_churn_score"`
	TotalScore      float64  `json:"total_score"`
	MatchedKeywords []string `json:"matched_keywords,omitempty"`
}

// ============================================================================
// BM25 Token Indexer Over Repository Symbol Names
// ============================================================================

// BM25SymbolIndex indexes repository symbols using the BM25 probabilistic model.
type BM25SymbolIndex struct {
	docs     []bm25Doc
	avgdl    float64
	docCount int
	df       map[string]int // term -> document frequency
	k1       float64
	b        float64
}

type bm25Doc struct {
	symbol   SymbolTag
	file     string
	tokens   []string
	termFreq map[string]int
	length   int
}

// BM25SymbolMatch represents a scored match from BM25SymbolIndex.
type BM25SymbolMatch struct {
	Symbol SymbolTag `json:"symbol"`
	Score  float64   `json:"score"`
	Tokens []string  `json:"tokens"`
}

// NewBM25SymbolIndexer constructs a new BM25 index over a collection of symbols.
func NewBM25SymbolIndexer(symbols []SymbolTag) *BM25SymbolIndex {
	idx := &BM25SymbolIndex{
		df: make(map[string]int),
		k1: 1.2,
		b:  0.75,
	}
	idx.Index(symbols)
	return idx
}

// TokenizeIdentifier splits camelCase, snake_case, and kebab-case identifiers into lowercase terms.
func TokenizeIdentifier(ident string) []string {
	if ident == "" {
		return nil
	}

	var rawTokens []string
	var current strings.Builder
	var currentCompound strings.Builder
	var compoundParts int

	flushCompound := func() {
		if compoundParts >= 2 && currentCompound.Len() >= 5 {
			rawTokens = append(rawTokens, currentCompound.String())
		}
		currentCompound.Reset()
		compoundParts = 0
	}

	runes := []rune(ident)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if unicode.IsSpace(r) || r == '-' || r == '.' || r == '/' || r == '\\' || r == ':' || r == '(' || r == ')' || r == '[' || r == ']' || r == '{' || r == '}' || r == '"' || r == '\'' || r == '`' || r == ',' || r == ';' || r == '!' || r == '?' || r == '=' || r == '<' || r == '>' || r == '+' || r == '*' {
			if current.Len() > 0 {
				rawTokens = append(rawTokens, current.String())
				compoundParts++
				current.Reset()
			}
			flushCompound()
			continue
		}
		if r == '_' {
			if current.Len() > 0 {
				part := current.String()
				startRuneIdx := i - len([]rune(part))
				if startRuneIdx >= 2 && runes[startRuneIdx-1] == '_' && runes[startRuneIdx-2] == '_' && i+1 < len(runes) && runes[i+1] == '_' {
					rawTokens = append(rawTokens, "__"+part+"__")
				}
				rawTokens = append(rawTokens, part)
				if strings.ToLower(part) != "test" && strings.ToLower(part) != "tests" {
					currentCompound.WriteString(part)
					compoundParts++
				}
				current.Reset()
			}
			continue
		}

		// CamelCase boundary detection
		if unicode.IsUpper(r) {
			if current.Len() > 0 {
				prevIsUpper := unicode.IsUpper(runes[i-1])
				nextIsLower := (i+1 < len(runes)) && unicode.IsLower(runes[i+1])
				if !prevIsUpper || nextIsLower {
					rawTokens = append(rawTokens, current.String())
					compoundParts++
					current.Reset()
				}
			}
		}
		current.WriteRune(r)
		currentCompound.WriteRune(r)
	}
	if current.Len() > 0 {
		part := current.String()
		rawTokens = append(rawTokens, part)
		if strings.ToLower(part) != "test" && strings.ToLower(part) != "tests" {
			compoundParts++
		}
	}
	flushCompound()

	var result []string
	seenTok := make(map[string]bool)

	addResult := func(tok string) {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if len(tok) < 2 || len(tok) > 40 || searchStopwords[tok] || seenTok[tok] {
			return
		}
		seenTok[tok] = true
		result = append(result, tok)

		// Morphological variants (e.g. registration <-> register, methods <-> method)
		if strings.HasSuffix(tok, "tion") && len(tok) > 5 {
			root := tok[:len(tok)-4]
			for _, variant := range []string{root, root + "er", root + "e"} {
				if len(variant) >= 2 && !seenTok[variant] && !searchStopwords[variant] {
					seenTok[variant] = true
					result = append(result, variant)
				}
			}
		} else if strings.HasSuffix(tok, "ing") && len(tok) > 4 {
			root := tok[:len(tok)-3]
			if len(root) >= 2 && !seenTok[root] && !searchStopwords[root] {
				seenTok[root] = true
				result = append(result, root)
			}
		} else if strings.HasSuffix(tok, "ed") && len(tok) > 3 {
			root := tok[:len(tok)-2]
			if len(root) >= 2 && !seenTok[root] && !searchStopwords[root] {
				seenTok[root] = true
				result = append(result, root)
			}
		} else if strings.HasSuffix(tok, "s") && len(tok) > 3 && !strings.HasSuffix(tok, "ss") {
			root := tok[:len(tok)-1]
			if len(root) >= 2 && !seenTok[root] && !searchStopwords[root] {
				seenTok[root] = true
				result = append(result, root)
			}
		}
		if strings.HasPrefix(tok, "auto") && len(tok) >= 7 {
			stem := tok[4:]
			for _, variant := range []string{stem, stem + "documenter"} {
				if len(variant) >= 3 && !seenTok[variant] && !searchStopwords[variant] {
					seenTok[variant] = true
					result = append(result, variant)
				}
			}
		}
	}

	for _, tok := range rawTokens {
		addResult(tok)
	}
	return result
}

// Index populates the BM25 index from a slice of SymbolTags.
func (idx *BM25SymbolIndex) Index(symbols []SymbolTag) {
	idx.docs = make([]bm25Doc, 0, len(symbols))
	idx.df = make(map[string]int)
	totalLen := 0

	for _, s := range symbols {
		toks := TokenizeIdentifier(s.Name)
		cleanSym := strings.ReplaceAll(strings.ToLower(s.Name), "_", "")
		if len(cleanSym) >= 4 {
			toks = append(toks, cleanSym)
		}
		// Include file path tokens, parent class tokens, and signature tokens for richer matching
		fileToks := TokenizeIdentifier(s.File)
		parentToks := TokenizeIdentifier(s.ParentClass)
		sigToks := TokenizeIdentifier(s.Signature)

		allToks := append(toks, fileToks...)
		allToks = append(allToks, parentToks...)
		allToks = append(allToks, sigToks...)

		tf := make(map[string]int)
		seenInDoc := make(map[string]bool)

		for _, t := range allToks {
			tf[t]++
			if !seenInDoc[t] {
				seenInDoc[t] = true
				idx.df[t]++
			}
		}

		docLen := len(allToks)
		totalLen += docLen

		idx.docs = append(idx.docs, bm25Doc{
			symbol:   s,
			file:     s.File,
			tokens:   allToks,
			termFreq: tf,
			length:   docLen,
		})
	}

	idx.docCount = len(idx.docs)
	if idx.docCount > 0 {
		idx.avgdl = float64(totalLen) / float64(idx.docCount)
	} else {
		idx.avgdl = 1.0
	}
}

// Score computes the BM25 score for query terms against all indexed symbols.
func (idx *BM25SymbolIndex) Score(query string, topN int) []BM25SymbolMatch {
	if idx.docCount == 0 || query == "" {
		return nil
	}

	queryTokens := TokenizeIdentifier(query)
	if len(queryTokens) == 0 {
		return nil
	}

	N := float64(idx.docCount)
	var matches []BM25SymbolMatch

	for _, doc := range idx.docs {
		score := 0.0
		var matched []string
		symClean := strings.ReplaceAll(strings.ToLower(doc.symbol.Name), "_", "")

		for _, q := range queryTokens {
			tf := float64(doc.termFreq[q])
			if tf == 0 {
				continue
			}
			matched = append(matched, q)
			dfVal := float64(idx.df[q])
			// Standard Lucene/BM25 IDF: ln((N - n + 0.5)/(n + 0.5) + 1.0)
			idf := math.Log(((N - dfVal + 0.5) / (dfVal + 0.5)) + 1.0)
			if idf < 0.1 {
				idf = 0.1
			}

			docLen := float64(doc.length)
			denom := tf + idx.k1*(1.0-idx.b+idx.b*(docLen/idx.avgdl))
			termScore := idf * (tf * (idx.k1 + 1.0)) / denom
			if len(q) >= 5 && (q == symClean || q == strings.ToLower(doc.symbol.Name)) {
				termScore *= 3.0
			}
			score += termScore
		}

		if score > 0 {
			score *= ComputeSubtokenSynergyBoost(queryTokens, doc.symbol)
			matches = append(matches, BM25SymbolMatch{
				Symbol: doc.symbol,
				Score:  score,
				Tokens: matched,
			})
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Score > matches[j].Score
	})

	if topN > 0 && len(matches) > topN {
		matches = matches[:topN]
	}
	return matches
}

// ScoreFile aggregates the BM25 scores for all symbols belonging to a specific file.
func (idx *BM25SymbolIndex) ScoreFile(query string, file string) float64 {
	matches := idx.Score(query, 0)
	total := 0.0
	for _, m := range matches {
		if filepath.ToSlash(m.Symbol.File) == filepath.ToSlash(file) {
			total += m.Score
		}
	}
	return total
}

// ============================================================================
// Traceback Parsing & Standard Library / Test Pruning
// ============================================================================

var (
	pyFrameRegex   = regexp.MustCompile(`(?m)^\s*File\s+"([^"]+)",\s*line\s+(\d+)(?:,\s*in\s+([a-zA-Z0-9_<>]+))?`)
	pyShortRegex   = regexp.MustCompile(`(?m)^((?:[a-zA-Z]:)?[^:\r\n]+?\.py):(\d+):(?:\s*in\s+([a-zA-Z0-9_<>]+))?`)
	goFrameRegex   = regexp.MustCompile(`(?m)^\s*((?:[a-zA-Z]:)?[^:\r\n]+?\.go):(\d+)(?:\s+\+0x[0-9a-fA-F]+)?$`)
	rsFrameRegex   = regexp.MustCompile(`(?m)^\s*(?:-->\s*)?((?:[a-zA-Z]:)?[^:\r\n]+?\.rs):(\d+):(\d+)`)
	tsFrameRegex   = regexp.MustCompile(`(?m)^\s*(?:at\s+.*?\s+\()?((?:[a-zA-Z]:)?[^:\r\n]+?\.[jt]sx?):(\d+):(\d+)`)
)

// ParseTraceback extracts all execution frames in order from a raw traceback string.
func ParseTraceback(traceback string) []TracebackFrame {
	var frames []TracebackFrame
	if strings.TrimSpace(traceback) == "" {
		return frames
	}

	lines := strings.Split(traceback, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// 1. Python standard traceback: File "path/to/file.py", line 123, in func
		if m := pyFrameRegex.FindStringSubmatch(line); len(m) > 2 {
			fPath := m[1]
			lNum, _ := strconv.Atoi(m[2])
			fnName := ""
			if len(m) > 3 {
				fnName = m[3]
			}
			codeSnippet := ""
			if i+1 < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "File ") {
				codeSnippet = strings.TrimSpace(lines[i+1])
			}
			frames = append(frames, TracebackFrame{
				File:     filepath.ToSlash(fPath),
				Line:     lNum,
				Function: fnName,
				Code:     codeSnippet,
				IsTest:   isTestPath(fPath),
				IsStdlib: isStdlibOrVendorPath(fPath),
			})
			continue
		}

		// 2. Pytest compact traceback: path/to/file.py:123: in func
		if m := pyShortRegex.FindStringSubmatch(line); len(m) > 2 {
			fPath := m[1]
			lNum, _ := strconv.Atoi(m[2])
			fnName := ""
			if len(m) > 3 {
				fnName = m[3]
			}
			frames = append(frames, TracebackFrame{
				File:     filepath.ToSlash(fPath),
				Line:     lNum,
				Function: fnName,
				IsTest:   isTestPath(fPath),
				IsStdlib: isStdlibOrVendorPath(fPath),
			})
			continue
		}

		// 3. Go traceback: path/to/file.go:123 +0x42
		if m := goFrameRegex.FindStringSubmatch(line); len(m) > 2 {
			fPath := m[1]
			lNum, _ := strconv.Atoi(m[2])
			frames = append(frames, TracebackFrame{
				File:     filepath.ToSlash(fPath),
				Line:     lNum,
				IsTest:   isTestPath(fPath),
				IsStdlib: isStdlibOrVendorPath(fPath),
			})
			continue
		}

		// 4. Rust / TS tracebacks
		if m := rsFrameRegex.FindStringSubmatch(line); len(m) > 2 {
			fPath := m[1]
			lNum, _ := strconv.Atoi(m[2])
			frames = append(frames, TracebackFrame{
				File:     filepath.ToSlash(fPath),
				Line:     lNum,
				IsTest:   isTestPath(fPath),
				IsStdlib: isStdlibOrVendorPath(fPath),
			})
			continue
		}
		if m := tsFrameRegex.FindStringSubmatch(line); len(m) > 2 {
			fPath := m[1]
			lNum, _ := strconv.Atoi(m[2])
			frames = append(frames, TracebackFrame{
				File:     filepath.ToSlash(fPath),
				Line:     lNum,
				IsTest:   isTestPath(fPath),
				IsStdlib: isStdlibOrVendorPath(fPath),
			})
			continue
		}
	}

	return frames
}

// isTestPath returns true if the path points to a test file or test directory.
func isTestPath(p string) bool {
	clean := filepath.ToSlash(strings.ToLower(p))
	base := filepath.Base(clean)

	// Strict exclusion of markdown and doc files from candidate mutation
	if strings.HasSuffix(clean, ".md") || strings.HasSuffix(clean, ".rst") || strings.HasSuffix(clean, ".txt") {
		return true
	}
	if base == "problem.md" || base == "readme.md" || base == "changelog.md" || strings.HasPrefix(base, "license") || base == "conftest.py" ||
		base == "generic_parsetab.py" || base == "generic_lextab.py" || base == "cds_parsetab.py" || base == "cds_lextab.py" ||
		base == "pytester.py" || base == "pytester_assertions.py" {
		return true
	}

	// Path segments check (handles leading, interior, and trailing directories e.g. tests/..., foo/tests/...)
	segments := strings.Split(clean, "/")
	for _, seg := range segments {
		if seg == "tests" || seg == "test" || seg == "testing" || seg == "__tests__" ||
			seg == "docs" || seg == "doc" || seg == "examples" || seg == "benchmarks" || seg == "spec" ||
			seg == "tutorials" || seg == "tutorial" || seg == "galleries" || seg == "gallery" || seg == "sample_data" {
			return true
		}
	}

	if strings.Contains(clean, "/docs/") || strings.Contains(clean, "/doc/") ||
		strings.Contains(clean, "/examples/") || strings.Contains(clean, "/benchmarks/") ||
		strings.Contains(clean, "/tests/") || strings.Contains(clean, "/test/") ||
		strings.Contains(clean, "/testing/") || strings.Contains(clean, "/spec/") ||
		strings.Contains(clean, "/__tests__/") {
		return true
	}
	if strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
		strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".spec.ts") || base == "test.py" || base == "tests.py" {
		return true
	}
	return false
}

// isStdlibOrVendorPath returns true if the path resides in standard libraries or third-party packages.
func isStdlibOrVendorPath(p string) bool {
	clean := filepath.ToSlash(strings.ToLower(p))
	stdlibMarkers := []string{
		"/usr/lib", "/usr/local/lib", "site-packages", "dist-packages",
		"lib/python", "<frozen", "\\python", "goroot", "/go/src/runtime",
		"node_modules", "vendor/", "/lib/x86_64-linux-gnu", "appdata/local/programs/python",
	}
	for _, marker := range stdlibMarkers {
		if strings.Contains(clean, marker) {
			return true
		}
	}
	// Check known python stdlib modules
	base := filepath.Base(clean)
	switch base {
	case "encoder.py", "decoder.py", "contextlib.py", "importlib.py", "typing.py",
		"inspect.py", "traceback.py", "unittest.py", "runner.py", "core.py":
		if strings.Contains(clean, "json/") || strings.Contains(clean, "click/") || strings.Contains(clean, "unittest/") {
			return true
		}
	}
	return false
}

// isTestAuthoringIssue checks if the user explicitly requested authoring or fixing tests.
func isTestAuthoringIssue(issueDesc string) bool {
	lower := strings.ToLower(issueDesc)
	triggers := []string{
		"write test", "add test", "create test", "author test",
		"new test case", "update test file", "broken test case",
	}
	for _, trig := range triggers {
		if strings.Contains(lower, trig) {
			return true
		}
	}
	return false
}

// ============================================================================
// AST Backward-Slicing Module
// ============================================================================

// ASTSliceResult captures backward-sliced source definitions and dependency links.
type ASTSliceResult struct {
	SourceFile   string      `json:"source_file"`
	Symbol       string      `json:"symbol"`
	LineRange    [2]int      `json:"line_range"`
	CallingFrame string      `json:"calling_frame,omitempty"`
	Dependencies []SymbolTag `json:"dependencies,omitempty"`
}

// SliceASTBackward traces backwards from assertion points and caller frames
// to identify the underlying source method and class definitions.
func SliceASTBackward(repoDir string, frames []TracebackFrame, allSymbols []SymbolTag, issueDesc string) (*ASTSliceResult, error) {
	if len(frames) == 0 && len(allSymbols) == 0 {
		return nil, errors.New("ast: cannot slice backward without frames or symbols")
	}

	// 1. Separate frames into repository frames, test frames, and stdlib frames
	var repoNonTestFrames []TracebackFrame
	var testFrames []TracebackFrame

	for _, f := range frames {
		if f.IsStdlib {
			continue
		}
		if f.IsTest {
			testFrames = append(testFrames, f)
		} else {
			repoNonTestFrames = append(repoNonTestFrames, f)
		}
	}

	// 2. If we have a repository non-test frame in the traceback, that is the most direct causal location!
	if len(repoNonTestFrames) > 0 {
		// Innermost repo frame (closest to failure point)
		innermost := repoNonTestFrames[len(repoNonTestFrames)-1]
		relPath := resolveRelPath(repoDir, innermost.File)
		sym, lineRange := findEnclosingSymbol(repoDir, relPath, innermost.Line, innermost.Function, allSymbols)

		return &ASTSliceResult{
			SourceFile:   relPath,
			Symbol:       sym,
			LineRange:    lineRange,
			CallingFrame: fmt.Sprintf("%s:%d in %s", innermost.File, innermost.Line, innermost.Function),
		}, nil
	}

	// Count non-test file frequency per symbol name to avoid matching ubiquitous methods (e.g. description, clean, get)
	symbolFileCount := make(map[string]int)
	seenSymFile := make(map[string]bool)
	for _, s := range allSymbols {
		if isTestPath(s.File) {
			continue
		}
		k := strings.ToLower(s.Name) + "@" + s.File
		if !seenSymFile[k] {
			seenSymFile[k] = true
			symbolFileCount[strings.ToLower(s.Name)]++
		}
	}

	// 3. Assertion point is in a test frame. Trace backward from the test assertion call to source definitions.
	if len(testFrames) > 0 {
		testFrame := testFrames[len(testFrames)-1]
		testRelPath := resolveRelPath(repoDir, testFrame.File)

		var priorityTokens []string
		for _, tf := range testFrames {
			if tf.Function != "" {
				strippedFn := strings.TrimPrefix(strings.ToLower(tf.Function), "test_")
				if len(strippedFn) >= 4 && !searchStopwords[strippedFn] {
					priorityTokens = append(priorityTokens, strippedFn)
				}
			}
		}

		var testContextLines []string
		for _, tf := range testFrames {
			if tf.Code != "" {
				testContextLines = append(testContextLines, tf.Code)
			}
			tfRel := resolveRelPath(repoDir, tf.File)
			if data, err := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(tfRel))); err == nil {
				fLines := strings.Split(string(data), "\n")
				if tf.Line > 0 && tf.Line <= len(fLines) {
					priorityTokens = append(priorityTokens, ExtractCandidateTokens(fLines[tf.Line-1], nil, "")...)
				}
				fnRange := FindEnclosingSymbolRange(repoDir, tfRel, tf.Line, tf.Function)
				startL := tf.Line - 15
				if fnRange[0] > 0 && startL < fnRange[0]-1 {
					startL = fnRange[0] - 1
				}
				if startL < 0 {
					startL = 0
				}
				endL := tf.Line + 5
				if fnRange[1] > 0 && endL > fnRange[1] {
					endL = fnRange[1]
				}
				if endL > len(fLines) {
					endL = len(fLines)
				}
				for idx := startL; idx < endL; idx++ {
					testContextLines = append(testContextLines, fLines[idx])
				}
			}
		}

		// Extract referenced tokens/identifiers from test frame code or test function
		candidateTokens := append(priorityTokens, ExtractCandidateTokens(testFrame.Code, testContextLines, "")...)
		for _, tf := range testFrames {
			candidateTokens = append(candidateTokens, TokenizeIdentifier(tf.Function)...)
		}

		// Search symbols for definitions matching the test assertion references
		for _, tok := range candidateTokens {
			if len(tok) < 4 || searchStopwords[strings.ToLower(tok)] {
				continue
			}
			for _, s := range allSymbols {
				if isTestPath(s.File) {
					continue
				}
				sLower := strings.ToLower(s.Name)
				sClean := strings.ReplaceAll(sLower, "_", "")
				tLower := strings.ToLower(tok)
				if sLower == tLower || (len(tLower) >= 6 && sClean == tLower) {
					if symbolFileCount[sLower] <= 3 {
						lineRange := findEnclosingSymbolRange(repoDir, s.File, s.Line, s.Name)
						return &ASTSliceResult{
							SourceFile:   s.File,
							Symbol:       s.Name,
							LineRange:    lineRange,
							CallingFrame: fmt.Sprintf("test assertion in %s:%d", testRelPath, testFrame.Line),
						}, nil
					}
				}
			}
		}
	}

	// 4. Fallback: match specific (low document-frequency) tokens from issue description against repository symbols
	issueTokens := TokenizeIdentifier(issueDesc)
	for _, tok := range issueTokens {
		if len(tok) < 5 || searchStopwords[strings.ToLower(tok)] {
			continue
		}
		for _, s := range allSymbols {
			if isTestPath(s.File) {
				continue
			}
			sLower := strings.ToLower(s.Name)
			sClean := strings.ReplaceAll(sLower, "_", "")
			tLower := strings.ToLower(tok)
			if (sLower == tLower || (len(tLower) >= 6 && sClean == tLower)) && symbolFileCount[sLower] <= 2 {
				lineRange := findEnclosingSymbolRange(repoDir, s.File, s.Line, s.Name)
				return &ASTSliceResult{
					SourceFile: s.File,
					Symbol:     s.Name,
					LineRange:  lineRange,
				}, nil
			}
		}
	}

	return nil, errors.New("ast: backward slicing found no candidate source definitions")
}

// resolveRelPath normalizes a file path relative to repoDir.
func resolveRelPath(repoDir, path string) string {
	cleanPath := filepath.ToSlash(path)
	cleanRepo := filepath.ToSlash(repoDir)

	if rel, err := filepath.Rel(cleanRepo, cleanPath); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}

	// 1. Check if suffix segments exist on disk within repoDir (handles Docker /workspace/... or absolute container/site-packages paths)
	parts := strings.Split(cleanPath, "/")
	for i := 1; i < len(parts); i++ {
		subRel := strings.Join(parts[i:], "/")
		if fi, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(subRel))); err == nil && !fi.IsDir() {
			return subRel
		}
		if fi, err := os.Stat(filepath.Join(repoDir, "src", filepath.FromSlash(subRel))); err == nil && !fi.IsDir() {
			return "src/" + subRel
		}
	}

	// 2. Search for common workspace folder components
	idx := strings.Index(cleanPath, cleanRepo)
	if idx != -1 {
		return filepath.ToSlash(cleanPath[idx+len(cleanRepo):])
	}

	if !filepath.IsAbs(path) {
		return strings.TrimPrefix(cleanPath, "./")
	}

	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return filepath.Base(path)
}

// findEnclosingSymbol locates the function, method, or class enclosing a given line number.
func findEnclosingSymbol(repoDir, file string, line int, fallbackFunc string, symbols []SymbolTag) (string, [2]int) {
	// If fallbackFunc is a specific function from traceback frame, prefer matching symbol
	if fallbackFunc != "" && fallbackFunc != "<module>" {
		for _, s := range symbols {
			if filepath.ToSlash(s.File) == filepath.ToSlash(file) && s.Name == fallbackFunc {
				return s.Name, findEnclosingSymbolRange(repoDir, file, s.Line, s.Name)
			}
		}
	}

	// Otherwise, find symbol with definition at or closest preceding line
	var bestSymbol string
	bestRange := [2]int{line, line}
	closestDist := 1000000

	for _, s := range symbols {
		if filepath.ToSlash(s.File) != filepath.ToSlash(file) {
			continue
		}
		if s.Line <= line && (line-s.Line) < closestDist {
			closestDist = line - s.Line
			bestSymbol = s.Name
			bestRange = findEnclosingSymbolRange(repoDir, file, s.Line, s.Name)
		}
	}

	if bestSymbol != "" {
		return bestSymbol, bestRange
	}

	if fallbackFunc != "" && fallbackFunc != "<module>" {
		return fallbackFunc, findEnclosingSymbolRange(repoDir, file, line, fallbackFunc)
	}

	return filepath.Base(file), [2]int{line, line}
}

func findEnclosingSymbolRange(repoDir, file string, startLine int, symName string) [2]int {
	return FindEnclosingSymbolRange(repoDir, file, startLine, symName)
}

// FindEnclosingSymbolRange determines the [startLine, endLine] boundaries of a symbol in a source file.
func FindEnclosingSymbolRange(repoDir, file string, startLine int, symName string) [2]int {
	targetPath := filepath.Join(repoDir, filepath.FromSlash(file))
	data, err := os.ReadFile(targetPath)
	if err != nil {
		if startLine > 0 {
			return [2]int{startLine, startLine + 10}
		}
		return [2]int{1, 50}
	}

	ext := strings.ToLower(filepath.Ext(file))
	if ext == ".go" {
		fset := token.NewFileSet()
		node, parseErr := parser.ParseFile(fset, file, data, 0)
		if parseErr == nil && node != nil {
			for _, decl := range node.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					s := fset.Position(fn.Pos()).Line
					e := fset.Position(fn.End()).Line
					if (startLine > 0 && s <= startLine && startLine <= e) || fn.Name.Name == symName {
						return [2]int{s, e}
					}
				}
				if gen, ok := decl.(*ast.GenDecl); ok {
					s := fset.Position(gen.Pos()).Line
					e := fset.Position(gen.End()).Line
					if startLine > 0 && s <= startLine && startLine <= e {
						return [2]int{s, e}
					}
				}
			}
		}
	}

	// Polyglot indentation-based block finder (Python, TS, Rust)
	lines := strings.Split(string(data), "\n")
	if startLine <= 0 || startLine > len(lines) {
		startLine = 1
	}

	// Find the definition line at or above startLine
	defLineIdx := startLine - 1
	funcDefIdx := defLineIdx

	if symName != "" {
		found := false
		// Scan forward up to 10 lines from startLine-1 (e.g. startLine points to @staticmethod or docstring)
		forwardLimit := startLine + 10
		if forwardLimit > len(lines) {
			forwardLimit = len(lines)
		}
		for i := startLine - 1; i < forwardLimit; i++ {
			if strings.Contains(lines[i], symName) && (strings.Contains(lines[i], "def ") || strings.Contains(lines[i], "class ") || strings.Contains(lines[i], "func ") || strings.Contains(lines[i], "fn ")) {
				funcDefIdx = i
				defLineIdx = i
				// Include preceding decorators
				for d := i - 1; d >= 0 && d >= i-8; d-- {
					if strings.HasPrefix(strings.TrimSpace(lines[d]), "@") {
						defLineIdx = d
					} else {
						break
					}
				}
				found = true
				break
			}
		}
		if !found {
			// Scan backward up to 120 lines from startLine-1
			for i := startLine - 1; i >= 0 && i >= startLine-120; i-- {
				if strings.Contains(lines[i], symName) && (strings.Contains(lines[i], "def ") || strings.Contains(lines[i], "class ") || strings.Contains(lines[i], "func ") || strings.Contains(lines[i], "fn ")) {
					funcDefIdx = i
					defLineIdx = i
					// Include preceding decorators
					for d := i - 1; d >= 0 && d >= i-8; d-- {
						if strings.HasPrefix(strings.TrimSpace(lines[d]), "@") {
							defLineIdx = d
						} else {
							break
						}
					}
					break
				}
			}
		}
	} else {
		// When symName is empty (e.g. internal usage site line), search upward for nearest enclosing def/class
		currIndent := getIndentationLevel(lines[startLine-1])
		for i := startLine - 1; i >= 0 && i >= startLine-260; i-- {
			trimmed := strings.TrimSpace(lines[i])
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "class ") || strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(trimmed, "fn ") {
				indent := getIndentationLevel(lines[i])
				if indent < currIndent || (indent == currIndent && (strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "class "))) {
					funcDefIdx = i
					defLineIdx = i
					// Include preceding decorators
					for d := i - 1; d >= 0 && d >= i-8; d-- {
						if strings.HasPrefix(strings.TrimSpace(lines[d]), "@") {
							defLineIdx = d
						} else {
							break
						}
					}
					break
				}
			}
		}
	}

	baseIndent := getIndentationLevel(lines[funcDefIdx])
	endLineIdx := funcDefIdx
	sigParenDepth := strings.Count(lines[funcDefIdx], "(") - strings.Count(lines[funcDefIdx], ")")

	inTripleQuote := ""
	for i := funcDefIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if inTripleQuote != "" {
			if strings.Contains(trimmed, inTripleQuote) {
				inTripleQuote = ""
			}
			endLineIdx = i
			continue
		}
		for _, q := range []string{`"""`, `'''`} {
			if c := strings.Count(trimmed, q); c%2 == 1 {
				inTripleQuote = q
				break
			}
		}
		if inTripleQuote != "" || trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			endLineIdx = i
			continue
		}
		if sigParenDepth > 0 || strings.HasPrefix(trimmed, ")") {
			sigParenDepth += strings.Count(trimmed, "(") - strings.Count(trimmed, ")")
			if sigParenDepth < 0 {
				sigParenDepth = 0
			}
			endLineIdx = i
			continue
		}
		indent := getIndentationLevel(lines[i])
		if indent <= baseIndent {
			break
		}
		endLineIdx = i
	}

	return [2]int{defLineIdx + 1, endLineIdx + 1}
}

func getIndentationLevel(s string) int {
	indent := 0
	for _, r := range s {
		if r == ' ' {
			indent++
		} else if r == '\t' {
			indent += 4
		} else {
			break
		}
	}
	return indent
}

// ============================================================================
// Multi-File Candidate Ranker
// ============================================================================

// ============================================================================
// Exception / Warning Stub De-referencing & Intra-File Re-anchoring
// ============================================================================

// isExceptionOrWarningStub returns true if symName or the lines in lineRange represent an
// exception, error, or warning definition or a stub class (<= 4 lines).
func isExceptionOrWarningStub(repoDir, file string, symName string, lineRange [2]int) bool {
	lowerName := strings.ToLower(symName)
	isExceptName := strings.HasSuffix(lowerName, "warning") ||
		strings.HasSuffix(lowerName, "error") ||
		strings.HasSuffix(lowerName, "exception") ||
		strings.HasSuffix(lowerName, "fault")

	isShortStub := (lineRange[1] - lineRange[0]) <= 4

	if isExceptName {
		return true
	}

	targetPath := filepath.Join(repoDir, filepath.FromSlash(file))
	data, err := os.ReadFile(targetPath)
	if err != nil {
		return isExceptName || isShortStub
	}

	lines := strings.Split(string(data), "\n")
	startIdx := lineRange[0] - 1
	if startIdx >= 0 && startIdx < len(lines) {
		defLine := strings.ToLower(lines[startIdx])
		if strings.Contains(defLine, "warning") || strings.Contains(defLine, "error") || strings.Contains(defLine, "exception") {
			return true
		}
		if isShortStub && (strings.Contains(defLine, "class ") || strings.Contains(defLine, "def ")) {
			for i := startIdx + 1; i <= lineRange[1]-1 && i < len(lines); i++ {
				trimmed := strings.TrimSpace(lines[i])
				if trimmed == "pass" || trimmed == "..." {
					return true
				}
			}
		}
	}

	return false
}

// FindSymbolUsageSiteInFile searches file for occurrences of symName where it is used/raised,
// and returns the enclosing function/method definition line, name, and range.
func FindSymbolUsageSiteInFile(repoDir, file string, symName string, issueDesc string) (int, string, [2]int) {
	targetPath := filepath.Join(repoDir, filepath.FromSlash(file))
	data, err := os.ReadFile(targetPath)
	if err != nil || len(symName) < 3 {
		return 0, "", [2]int{0, 0}
	}

	lines := strings.Split(string(data), "\n")
	issueTokens := TokenizeIdentifier(issueDesc)
	issueSet := make(map[string]bool)
	for _, tok := range issueTokens {
		issueSet[tok] = true
	}

	type usageCand struct {
		defLine   int
		funcName  string
		lineRange [2]int
		score     float64
	}

	var candidates []usageCand
	seenFuncs := make(map[string]bool)

	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(l, symName) {
			if strings.HasPrefix(trimmed, "class "+symName) || strings.HasPrefix(trimmed, "def "+symName) {
				continue
			}

			rng := FindEnclosingSymbolRange(repoDir, file, i+1, "")
			defLine := rng[0]
			funcName := ""
			if defLine > 0 && defLine <= len(lines) {
				for cur := defLine - 1; cur < len(lines) && cur < defLine+5; cur++ {
					defText := strings.TrimSpace(lines[cur])
					if strings.HasPrefix(defText, "def ") {
						parts := strings.Fields(defText)
						if len(parts) >= 2 {
							funcName = strings.Split(parts[1], "(")[0]
							break
						}
					} else if strings.HasPrefix(defText, "class ") {
						parts := strings.Fields(defText)
						if len(parts) >= 2 {
							funcName = strings.Split(strings.Split(parts[1], "(")[0], ":")[0]
							break
						}
					}
				}
			}

			if funcName == "" || seenFuncs[funcName] {
				continue
			}
			seenFuncs[funcName] = true

			score := 1.0
			if strings.Contains(l, "warn") || strings.Contains(l, "raise") {
				score += 2.0
			}
			fToks := TokenizeIdentifier(funcName)
			for _, ft := range fToks {
				if issueSet[ft] {
					score += 1.5
				}
			}

			candidates = append(candidates, usageCand{
				defLine:   defLine,
				funcName:  funcName,
				lineRange: rng,
				score:     score,
			})
		}
	}

	if len(candidates) == 0 {
		return 0, "", [2]int{0, 0}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	top := candidates[0]
	return top.defLine, top.funcName, top.lineRange
}

// FindBestImplementationSymbolInFile searches file for substantive classes/methods that
// best match the issue description tokens, avoiding stub classes.
func FindBestImplementationSymbolInFile(repoDir, file string, issueDesc string) (int, string, [2]int) {
	if top := FindTopImplementationSymbolsInFile(repoDir, file, issueDesc, 1); len(top) > 0 {
		return top[0].DefLine, top[0].Name, top[0].LineRange
	}
	targetPath := filepath.Join(repoDir, filepath.FromSlash(file))
	data, err := os.ReadFile(targetPath)
	if err != nil {
		return 0, "", [2]int{0, 0}
	}

	lines := strings.Split(string(data), "\n")
	issueTokens := TokenizeIdentifier(issueDesc)
	if len(issueTokens) == 0 {
		return 0, "", [2]int{0, 0}
	}
	issueSet := make(map[string]bool)
	for _, tok := range issueTokens {
		issueSet[tok] = true
	}

	type implCand struct {
		defLine   int
		name      string
		lineRange [2]int
		score     float64
	}

	var candidates []implCand

	issueLower := strings.ToLower(issueDesc)

	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		isDef := strings.HasPrefix(trimmed, "def ")
		isClass := strings.HasPrefix(trimmed, "class ")
		if !isDef && !isClass {
			continue
		}

		name := ""
		parts := strings.Fields(trimmed)
		if len(parts) >= 2 {
			name = strings.Split(strings.Split(parts[1], "(")[0], ":")[0]
		}
		if name == "" {
			continue
		}

		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, "warning") || strings.HasSuffix(lower, "error") || strings.HasSuffix(lower, "exception") {
			continue
		}

		rng := FindEnclosingSymbolRange(repoDir, file, i+1, name)
		length := rng[1] - rng[0] + 1
		if length < 4 {
			continue
		}

		score := 0.0
		isExactCaseClass := isClass && name != lower && len(name) >= 3 && strings.Contains(issueDesc, name)
		strippedName := strings.Trim(strings.TrimPrefix(strings.TrimPrefix(lower, "_print_"), "_eval_"), "_")
		if isExactCaseClass {
			score += 22.0
		} else if len(lower) >= 4 && !searchStopwords[lower] && strings.Contains(issueLower, lower) {
			score += 15.0
			if isDef {
				score += 8.0
			} else if isClass && length <= 60 {
				score += 5.0
			}
		} else if len(strippedName) >= 3 && !searchStopwords[strippedName] && (issueSet[strippedName] || strings.Contains(issueLower, strippedName)) {
			score += 10.0
			if isDef {
				score += 4.0
			}
		}

		tokScore := 0.0
		for _, tok := range TokenizeIdentifier(name) {
			if issueSet[tok] && !searchStopwords[tok] {
				tokScore += 3.5
			}
		}
		if tokScore > 8.0 {
			tokScore = 8.0
		}
		score += tokScore

		bodyEnd := i + 40
		if isClass && length <= 180 {
			bodyEnd = i + 45
		}
		if bodyEnd > rng[1] {
			bodyEnd = rng[1]
		}
		if bodyEnd > len(lines) {
			bodyEnd = len(lines)
		}
		seenBodyTok := make(map[string]bool)
		bodyScore := 0.0
		for j := i + 1; j < bodyEnd; j++ {
			for _, tok := range TokenizeIdentifier(lines[j]) {
				if issueSet[tok] && !seenBodyTok[tok] {
					seenBodyTok[tok] = true
					if strings.HasPrefix(tok, "__") && strings.HasSuffix(tok, "__") {
						bodyScore += 2.50
					} else {
						bodyScore += 0.40
					}
				}
			}
		}
		if bodyScore > 6.0 {
			bodyScore = 6.0
		}
		score += bodyScore

		// Prefer specific methods over large class shells (>60 lines), unless the exact CamelCase class name is targeted
		if isClass {
			if isExactCaseClass {
				if length > 180 {
					score *= 0.65
				} else if length > 60 {
					score *= 0.85
				}
			} else if length > 180 {
				score *= 0.25
			} else if length > 60 {
				score *= 0.45
			}
		}
		if score > 0 {
			candidates = append(candidates, implCand{
				defLine:   i + 1,
				name:      name,
				lineRange: rng,
				score:     score,
			})
		}
	}

	if len(candidates) == 0 {
		return 0, "", [2]int{0, 0}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	top := candidates[0]
	return top.defLine, top.name, top.lineRange
}

// ImplementationSymbolCandidate represents a ranked symbol region within an implementation file.
type ImplementationSymbolCandidate struct {
	DefLine   int
	Name      string
	LineRange [2]int
	Score     float64
}

// FindTopImplementationSymbolsInFile returns up to topK non-overlapping symbol regions in a file
// ranked by relevance to issueDesc, enabling multi-perspective speculative branching.
func FindTopImplementationSymbolsInFile(repoDir, file string, issueDesc string, topK int) []ImplementationSymbolCandidate {
	if topK <= 0 {
		topK = 2
	}
	targetPath := filepath.Join(repoDir, filepath.FromSlash(file))
	data, err := os.ReadFile(targetPath)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	issueTokens := TokenizeIdentifier(issueDesc)
	if len(issueTokens) == 0 {
		return nil
	}
	issueSet := make(map[string]bool)
	for _, tok := range issueTokens {
		issueSet[tok] = true
	}
	if issueSet["pickle"] || issueSet["pickling"] || issueSet["unpickle"] || issueSet["loads"] || issueSet["dumps"] {
		issueSet["getstate"] = true
		issueSet["setstate"] = true
		issueSet["__getstate__"] = true
		issueSet["__setstate__"] = true
	}
	if issueSet["copy"] || issueSet["deepcopy"] {
		issueSet["__copy__"] = true
		issueSet["__deepcopy__"] = true
		issueSet["getstate"] = true
		issueSet["setstate"] = true
	}
	issueLower := strings.ToLower(issueDesc)

	var all []ImplementationSymbolCandidate
	defNameCount := make(map[string]int)
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "def ") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				n := strings.ToLower(strings.Split(strings.Split(parts[1], "(")[0], ":")[0])
				if n != "" {
					defNameCount[n]++
				}
			}
		}
	}
	genericClassToks := map[string]bool{
		"serializer": true, "documenter": true, "docstring": true, "operation": true,
		"validator": true, "mixin": true, "base": true, "field": true, "error": true,
		"node": true, "visitor": true, "handler": true, "manager": true, "compiler": true,
	}
	enclosingClassEndLine := 0
	enclosingClassIndent := 0
	enclosingClassBonus := 0.0
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		isDef := strings.HasPrefix(trimmed, "def ")
		isClass := strings.HasPrefix(trimmed, "class ")
		if !isDef && !isClass {
			continue
		}
		name := ""
		parts := strings.Fields(trimmed)
		if len(parts) >= 2 {
			name = strings.Split(strings.Split(parts[1], "(")[0], ":")[0]
		}
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		rng := FindEnclosingSymbolRange(repoDir, file, i+1, name)
		length := rng[1] - rng[0] + 1
		if length < 4 {
			continue
		}
		if (strings.HasSuffix(lower, "warning") || strings.HasSuffix(lower, "error") || strings.HasSuffix(lower, "exception")) && length <= 12 {
			continue
		}
		curIndent := getIndentationLevel(l)
		if i+1 > enclosingClassEndLine || (isClass && curIndent <= enclosingClassIndent) {
			enclosingClassBonus = 0.0
		}
		score := 0.0
		fileStem := strings.TrimSuffix(strings.ToLower(filepath.Base(file)), ".py")
		isModulePrimaryClass := isClass && curIndent == 0 && len(fileStem) >= 3 && lower == fileStem
		isExactCaseClass := isClass && name != lower && len(name) >= 3 && strings.Contains(issueDesc, name)
		isCalledClass := isClass && len(name) >= 3 && !searchStopwords[lower] && (strings.Contains(issueDesc, name+"(") || strings.Contains(issueLower, lower+"("))
		if isClass {
			classTokMatches := 0
			for _, ct := range TokenizeIdentifier(name) {
				if len(ct) >= 3 && !searchStopwords[ct] && !genericClassToks[ct] && issueSet[ct] {
					classTokMatches++
				}
			}
			if isExactCaseClass || isCalledClass {
				enclosingClassBonus = 18.0
				enclosingClassEndLine = rng[1]
				enclosingClassIndent = curIndent
			} else if isModulePrimaryClass || (issueSet[lower] && !searchStopwords[lower] && len(lower) >= 4) {
				enclosingClassBonus = 12.0
				enclosingClassEndLine = rng[1]
				enclosingClassIndent = curIndent
			} else if classTokMatches > 0 {
				enclosingClassBonus = 8.0 + 4.0*float64(classTokMatches)
				if enclosingClassBonus > 16.0 {
					enclosingClassBonus = 16.0
				}
				enclosingClassEndLine = rng[1]
				enclosingClassIndent = curIndent
			}
		} else if isDef && i+1 <= enclosingClassEndLine && curIndent > enclosingClassIndent && enclosingClassBonus > 0 {
			score += enclosingClassBonus
			strippedEval := strings.Trim(strings.TrimPrefix(lower, "_eval_"), "_")
			if lower == "eval" || (strings.HasPrefix(lower, "_eval_") && issueSet[strippedEval]) || ((lower == "__init__" || lower == "__new__") && (issueSet["__init__"] || issueSet["__new__"])) {
				score += 4.0
			}
		}
		strippedName := strings.Trim(strings.TrimPrefix(strings.TrimPrefix(lower, "_print_"), "_eval_"), "_")
		isPolymorphicMethod := isDef && (defNameCount[lower] >= 3 || defNameCount[strippedName] >= 3)
		isGenericMethod := lower == "eval" || lower == "doit" || lower == "func" || lower == "args"
		if isExactCaseClass || isCalledClass {
			score += 22.0
		} else if isModulePrimaryClass {
			score += 18.0
		} else if !isGenericMethod && len(lower) >= 4 && !searchStopwords[lower] && (issueSet[lower] || (len(lower) >= 6 && strings.Contains(issueLower, lower))) {
			if isPolymorphicMethod {
				score += 6.0
			} else {
				score += 15.0
				if isDef {
					score += 8.0
				} else if isClass && length <= 60 {
					score += 5.0
				}
			}
		} else if !isGenericMethod && len(strippedName) >= 3 && !searchStopwords[strippedName] && (issueSet[strippedName] || (len(strippedName) >= 5 && strings.Contains(issueLower, strippedName))) {
			if isPolymorphicMethod {
				score += 5.0
			} else {
				score += 10.0
				if isDef {
					score += 4.0
				}
			}
		}
		tokScore := 0.0
		if !isGenericMethod {
			for _, tok := range TokenizeIdentifier(name) {
				if issueSet[tok] && !searchStopwords[tok] {
					tokScore += 3.5
				}
			}
		}
		if tokScore > 8.0 {
			tokScore = 8.0
		}
		score += tokScore

		sigEndIdx := i
		sigParens := strings.Count(trimmed, "(") - strings.Count(trimmed, ")")
		for sigParens > 0 && sigEndIdx+1 < len(lines) && sigEndIdx+1 < i+20 {
			sigEndIdx++
			st := strings.TrimSpace(lines[sigEndIdx])
			sigParens += strings.Count(st, "(") - strings.Count(st, ")")
		}
		bodyStart := sigEndIdx + 1
		bodyEnd := bodyStart + 75
		if isClass {
			bodyEnd = bodyStart + 35
		}
		if bodyStart < len(lines) {
			tNext := strings.TrimSpace(lines[bodyStart])
			for _, q := range []string{`"""`, `'''`} {
				hasOpen := strings.HasPrefix(tNext, q) || strings.HasPrefix(tNext, "r"+q) || strings.HasPrefix(tNext, "u"+q)
				if hasOpen && strings.Count(tNext, q) == 1 {
					for dsIdx := bodyStart + 1; dsIdx < len(lines) && dsIdx <= bodyStart+240; dsIdx++ {
						if strings.Contains(lines[dsIdx], q) {
							bodyStart = dsIdx + 1
							bodyEnd = bodyStart + 80
							break
						}
					}
					break
				}
			}
		}
		if bodyEnd > rng[1] {
			bodyEnd = rng[1]
		}
		if bodyEnd > len(lines) {
			bodyEnd = len(lines)
		}
		seenBodyTok := make(map[string]bool)
		bodyScore := 0.0
		highSpecBodyMatches := 0
		for j := bodyStart; j < bodyEnd; j++ {
			if isClass {
				jt := strings.TrimSpace(lines[j])
				if strings.HasPrefix(jt, "def ") && getIndentationLevel(lines[j]) > curIndent {
					break
				}
			}
			for _, tok := range TokenizeIdentifier(lines[j]) {
				if issueSet[tok] && !seenBodyTok[tok] {
					seenBodyTok[tok] = true
					if strings.HasPrefix(tok, "__") && strings.HasSuffix(tok, "__") {
						bodyScore += 3.00
						highSpecBodyMatches += 2
					} else if len(tok) >= 6 && !searchStopwords[tok] {
						bodyScore += 0.85
						highSpecBodyMatches++
					} else {
						bodyScore += 0.40
					}
				}
			}
		}
		if score == 0 && highSpecBodyMatches < 2 && bodyScore > 2.0 {
			bodyScore = 2.0
		} else if bodyScore > 8.5 {
			bodyScore = 8.5
		}
		score += bodyScore
		if isClass {
			if isExactCaseClass || isCalledClass || isModulePrimaryClass {
				if length > 180 {
					score *= 0.65
				} else if length > 60 {
					score *= 0.85
				}
			} else if length > 180 {
				score *= 0.25
			} else if length > 60 {
				score *= 0.45
			}
		}
		if score > 0 {
			all = append(all, ImplementationSymbolCandidate{
				DefLine:   i + 1,
				Name:      name,
				LineRange: rng,
				Score:     score,
			})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Score > all[j].Score
	})

	var selected []ImplementationSymbolCandidate
	for _, c := range all {
		overlaps := false
		for _, s := range selected {
			if absInt(c.DefLine-s.DefLine) < 35 {
				overlaps = true
				break
			}
		}
		if !overlaps {
			selected = append(selected, c)
			if len(selected) >= topK {
				break
			}
		}
	}
	return selected
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ExtractIntraFileCallGraphSlices extracts compact, line-numbered code slices for:
// 1. The enclosing class's __init__ method (if outside focusRange), exposing instance state attributes.
// 2. Up to 2 intra-file helper methods/functions (self.<helper>() or <func>()) called from within focusRange
//    whose definitions lie outside focusRange.
func ExtractIntraFileCallGraphSlices(repoDir, file string, srcLines []string, focusRange [2]int) []string {
	if len(srcLines) <= 340 || focusRange[0] <= 0 || focusRange[1] < focusRange[0] {
		return nil
	}
	fStart := focusRange[0]
	fEnd := focusRange[1]
	if fEnd > len(srcLines) {
		fEnd = len(srcLines)
	}

	// 1. Map all function/method definitions in the file outside focusRange
	type funcDef struct {
		name      string
		startLine int // 1-indexed
		endLine   int // 1-indexed
	}
	defsByName := make(map[string]funcDef)
	for i, l := range srcLines {
		lineNum := i + 1
		if lineNum >= fStart && lineNum <= fEnd {
			continue
		}
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "def ") {
			rest := strings.TrimPrefix(trimmed, "def ")
			name := strings.TrimSpace(strings.Split(rest, "(")[0])
			if name != "" && name != "__repr__" && name != "__str__" {
				rng := FindEnclosingSymbolRange(repoDir, file, lineNum, name)
				if rng[1] >= rng[0] {
					defsByName[name] = funcDef{name: name, startLine: rng[0], endLine: rng[1]}
				}
			}
		} else if len(l) > 0 && l[0] != ' ' && l[0] != '\t' && strings.Contains(trimmed, "=") && !strings.HasPrefix(trimmed, "#") {
			lhs := strings.TrimSpace(strings.SplitN(trimmed, "=", 2)[0])
			if len(lhs) >= 3 && identRefRegex.MatchString(lhs) && !strings.ContainsAny(lhs, " .([") {
				endAssign := lineNum
				pDepth := strings.Count(trimmed, "(") + strings.Count(trimmed, "[") + strings.Count(trimmed, "{") - strings.Count(trimmed, ")") - strings.Count(trimmed, "]") - strings.Count(trimmed, "}")
				for pDepth > 0 && endAssign < len(srcLines) && endAssign < lineNum+8 {
					nt := strings.TrimSpace(srcLines[endAssign])
					pDepth += strings.Count(nt, "(") + strings.Count(nt, "[") + strings.Count(nt, "{") - strings.Count(nt, ")") - strings.Count(nt, "]") - strings.Count(nt, "}")
					endAssign++
				}
				defsByName[lhs] = funcDef{name: lhs, startLine: lineNum, endLine: endAssign}
			}
		}
	}

	var slices []string

	// 2. Check enclosing class __init__ if focusRange is inside a class and __init__ is above focusRange
	for i := fStart - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(srcLines[i])
		if strings.HasPrefix(trimmed, "class ") && !strings.HasPrefix(trimmed, "#") {
			classIndent := len(srcLines[i]) - len(strings.TrimLeft(srcLines[i], " \t"))
			foundInit := false
			firstMethodIdx := fStart - 1
			for j := i + 1; j < fStart-1 && j < i+120; j++ {
				jt := strings.TrimSpace(srcLines[j])
				jIndent := len(srcLines[j]) - len(strings.TrimLeft(srcLines[j], " \t"))
				if jIndent <= classIndent && (strings.HasPrefix(jt, "class ") || strings.HasPrefix(jt, "def ")) {
					break
				}
				if strings.HasPrefix(jt, "def ") && firstMethodIdx == fStart-1 {
					firstMethodIdx = j
				}
				if strings.HasPrefix(jt, "def __init__(") {
					foundInit = true
					initRng := FindEnclosingSymbolRange(repoDir, file, j+1, "__init__")
					iHardEnd := initRng[1]
					if iHardEnd >= fStart {
						iHardEnd = fStart - 1
					}
					iEnd := iHardEnd
					if iEnd-j > 28 {
						iEnd = j + 28
					}
					if iEnd > j {
						var initLines []string
						initLines = append(initLines, fmt.Sprintf("  # --- Enclosing %s (L%d) constructor __init__ (L%d-L%d) ---", strings.TrimSpace(srcLines[i]), i+1, j+1, iHardEnd))
						initLines = append(initLines, FormatNumberedSliceElidingDocstrings(srcLines, j+1, iEnd, iHardEnd, 0, 26, true)...)
						slices = append(slices, strings.Join(initLines, "\n"))
					}
					break
				}
			}
			if !foundInit && firstMethodIdx > i+1 {
				cEnd := firstMethodIdx
				if cEnd-i > 35 {
					cEnd = i + 35
				}
				var clsLines []string
				clsLines = append(clsLines, fmt.Sprintf("  # --- Enclosing %s (L%d-L%d) ---", strings.TrimSpace(srcLines[i]), i+1, cEnd))
				clsLines = append(clsLines, FormatNumberedSliceElidingDocstrings(srcLines, i+1, cEnd, cEnd, 0, 16, true)...)
				slices = append(slices, strings.Join(clsLines, "\n"))
			}
			break
		}
	}

	// 3. Scan focusRange for intra-file helper calls (self.<name>( or <name>() and getter/setter pairs
	var calledHelpers []funcDef
	seenHelper := make(map[string]bool)
	for i := fStart - 1; i < fEnd && i < len(srcLines); i++ {
		line := srcLines[i]
		for _, tok := range identRefRegex.FindAllString(line, -1) {
			if tok == "__init__" || seenHelper[tok] {
				continue
			}
			if def, exists := defsByName[tok]; exists {
				if strings.Contains(line, tok+"(") || strings.Contains(line, "self."+tok) || strings.Contains(line, tok+".") || strings.Contains(line, tok+"[") {
					seenHelper[tok] = true
					calledHelpers = append(calledHelpers, def)
				}
			}
			if strings.HasPrefix(tok, "_") && len(tok) >= 4 && strings.Contains(line, "self."+tok) {
				stem := tok[1:]
				for _, candName := range []string{"set_" + stem, "get_" + stem, stem} {
					if !seenHelper[candName] {
						if def, exists := defsByName[candName]; exists {
							seenHelper[candName] = true
							calledHelpers = append(calledHelpers, def)
						}
					}
				}
			} else if strings.HasPrefix(tok, "set_") && len(tok) > 4 {
				pairName := "get_" + tok[4:]
				if !seenHelper[pairName] {
					if def, exists := defsByName[pairName]; exists {
						seenHelper[pairName] = true
						calledHelpers = append(calledHelpers, def)
					}
				}
			} else if strings.HasPrefix(tok, "get_") && len(tok) > 4 {
				pairName := "set_" + tok[4:]
				if !seenHelper[pairName] {
					if def, exists := defsByName[pairName]; exists {
						seenHelper[pairName] = true
						calledHelpers = append(calledHelpers, def)
					}
				}
			}
		}
	}

	for idx, h := range calledHelpers {
		if idx >= 2 {
			break
		}
		hStart := h.startLine - 1
		if hStart < 0 {
			hStart = 0
		}
		hEnd := h.endLine
		if hEnd-hStart > 32 {
			hEnd = hStart + 32
		}
		if hEnd > len(srcLines) {
			hEnd = len(srcLines)
		}
		var hLines []string
		for k := hStart; k < hEnd; k++ {
			hLines = append(hLines, fmt.Sprintf("  %4d: %s", k+1, srcLines[k]))
		}
		slices = append(slices, fmt.Sprintf("  # --- Intra-file helper: %s (L%d-L%d) ---\n%s", h.name, hStart+1, hEnd, strings.Join(hLines, "\n")))
	}

	return slices
}

// ExtractFileSymbolOutline extracts a compact single-line outline of top-level
// classes and functions in the file (e.g. "# Module Outline: Media (L78), merge (L123), Widget (L160)")
func ExtractFileSymbolOutline(repoDir, file string) string {
	targetPath := filepath.Join(repoDir, filepath.FromSlash(file))
	data, err := os.ReadFile(targetPath)
	if err != nil {
		return ""
	}

	lines := strings.Split(string(data), "\n")
	var topSymbols []string
	var nestedSymbols []string

	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "class ") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				name := strings.Split(strings.Split(parts[1], "(")[0], ":")[0]
				if len(name) > 1 && !strings.HasPrefix(name, "__") {
					entry := fmt.Sprintf("%s (L%d)", name, i+1)
					isTopLevel := len(l) > 0 && l[0] != ' ' && l[0] != '\t'
					if isTopLevel {
						if len(topSymbols) < 22 {
							topSymbols = append(topSymbols, entry)
						}
					} else if len(nestedSymbols) < 16 {
						nestedSymbols = append(nestedSymbols, entry)
					}
				}
			}
		}
	}

	symbols := topSymbols
	if len(symbols) < 6 {
		for _, ns := range nestedSymbols {
			if len(symbols) >= 18 {
				break
			}
			symbols = append(symbols, ns)
		}
	}

	if len(symbols) == 0 {
		return ""
	}
	return "# Module Outline: " + strings.Join(symbols, ", ")
}

// FormatNumberedSliceElidingDocstrings renders a 1-based line slice [startLine, endLine] while eliding
// long multi-line Python docstrings (>6 lines) so large docstrings do not push executable class/function
// code out of the context window. When docstrings are elided, the window automatically advances up to
// hardEndLine so up to maxCodeLines of actual code are shown.
func FormatNumberedSliceElidingDocstrings(srcLines []string, startLine, endLine, hardEndLine, focusLine, maxCodeLines int, useFocusMarker bool) []string {
	if len(srcLines) == 0 {
		return nil
	}
	if startLine < 1 {
		startLine = 1
	}
	if hardEndLine > len(srcLines) || hardEndLine <= 0 {
		hardEndLine = len(srcLines)
	}
	if endLine > hardEndLine {
		endLine = hardEndLine
	}
	if maxCodeLines <= 0 {
		maxCodeLines = endLine - startLine + 1
	}
	if focusLine > 0 && maxCodeLines > 50 && focusLine-startLine > maxCodeLines-45 {
		startLine = focusLine - (maxCodeLines - 45)
		if startLine < 1 {
			startLine = 1
		}
	}

	var out []string
	codeEmitted := 0
	idx := startLine - 1
	for idx < hardEndLine && ((idx < endLine || codeEmitted < maxCodeLines) && codeEmitted < maxCodeLines+25 || (focusLine > 0 && idx < focusLine+45)) {
		ln := idx + 1
		raw := strings.TrimRight(srcLines[idx], "\r")
		trimmed := strings.TrimSpace(raw)

		// Detect opening of multi-line docstring
		var quote string
		if strings.HasPrefix(trimmed, `"""`) || strings.HasPrefix(trimmed, `r"""`) || strings.HasPrefix(trimmed, `u"""`) {
			quote = `"""`
		} else if strings.HasPrefix(trimmed, `'''`) || strings.HasPrefix(trimmed, `r'''`) || strings.HasPrefix(trimmed, `u'''`) {
			quote = `'''`
		}

		if quote != "" && strings.Count(trimmed, quote) == 1 {
			closeIdx := -1
			for j := idx + 1; j < len(srcLines) && j <= idx+300; j++ {
				if strings.Contains(srcLines[j], quote) {
					closeIdx = j
					break
				}
			}
			if closeIdx > idx && (closeIdx-idx) >= 6 {
				closeLn := closeIdx + 1
				if focusLine <= ln || focusLine >= closeLn {
					out = append(out, fmt.Sprintf("  # ... [lines %d-%d docstring elided; place new attributes/methods AFTER closing %s on line %d] ...", ln, closeLn-1, quote, closeLn))
					closeRaw := strings.TrimRight(srcLines[closeIdx], "\r")
					if useFocusMarker {
						marker := "  "
						if closeLn == focusLine {
							marker = "> "
						}
						out = append(out, fmt.Sprintf("%s%4d: %s", marker, closeLn, closeRaw))
					} else {
						out = append(out, fmt.Sprintf("%4d: %s", closeLn, closeRaw))
					}
					codeEmitted += 2
					idx = closeIdx + 1
					continue
				}
			}
		}

		if useFocusMarker {
			marker := "  "
			if ln == focusLine {
				marker = "> "
			}
			out = append(out, fmt.Sprintf("%s%4d: %s", marker, ln, raw))
		} else {
			out = append(out, fmt.Sprintf("%4d: %s", ln, raw))
		}
		codeEmitted++
		idx++
	}
	return out
}

// ResolveInRepoBaseClassAndRelativeImports inspects a target Python file's imports and class headers,
// returning in-repo module paths that define base classes used in filePath or symbols referenced in issueContext.
func ResolveInRepoBaseClassAndRelativeImports(repoDir, filePath, issueContext string) []string {
	fullPath := filepath.Join(repoDir, filepath.FromSlash(filePath))
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	baseClasses := make(map[string]bool)
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "class ") && strings.Contains(trimmed, "(") {
			openIdx := strings.Index(trimmed, "(")
			closeIdx := strings.LastIndex(trimmed, ")")
			if openIdx > 0 && closeIdx > openIdx {
				for _, b := range strings.Split(trimmed[openIdx+1:closeIdx], ",") {
					b = strings.TrimSpace(b)
					if strings.Contains(b, "=") {
						b = strings.TrimSpace(strings.SplitN(b, "=", 2)[1])
					}
					if b != "" && b != "object" {
						baseClasses[b] = true
					}
				}
			}
		}
	}

	dir := filepath.ToSlash(filepath.Dir(filePath))
	var results []string
	seen := make(map[string]bool)
	limit := 90
	if len(lines) < limit {
		limit = len(lines)
	}
	bodyStr := string(data)
	for i := 0; i < limit; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "from ") || !strings.Contains(trimmed, " import ") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(trimmed, "from "), " import ", 2)
		if len(parts) != 2 {
			continue
		}
		modSpec := strings.TrimSpace(parts[0])
		impNamesRaw := strings.Trim(strings.TrimSpace(parts[1]), "()")
		var candRel string
		if strings.HasPrefix(modSpec, ".") {
			dots := 0
			for dots < len(modSpec) && modSpec[dots] == '.' {
				dots++
			}
			relMod := strings.ReplaceAll(modSpec[dots:], ".", "/")
			baseDir := dir
			for d := 1; d < dots; d++ {
				baseDir = filepath.ToSlash(filepath.Dir(baseDir))
			}
			if relMod != "" {
				candRel = baseDir + "/" + relMod + ".py"
			}
		} else {
			candRel = strings.ReplaceAll(modSpec, ".", "/") + ".py"
		}
		isSamePkg := strings.HasPrefix(modSpec, ".") || (dir != "" && dir != "." && strings.HasPrefix(candRel, dir+"/"))
		var matchedSym bool
		for _, sym := range strings.Split(impNamesRaw, ",") {
			sym = strings.TrimSpace(strings.Split(sym, " as ")[0])
			if sym == "" {
				continue
			}
			if baseClasses[sym] || (len(sym) >= 4 && strings.Contains(issueContext, sym)) || (isSamePkg && len(sym) >= 4 && strings.Contains(bodyStr, sym+"(")) {
				matchedSym = true
				break
			}
		}
		if !matchedSym {
			continue
		}
		if candRel != "" && !seen[candRel] && candRel != filepath.ToSlash(filePath) && !isTestPath(candRel) {
			if fi, statErr := os.Stat(filepath.Join(repoDir, filepath.FromSlash(candRel))); statErr == nil && !fi.IsDir() {
				seen[candRel] = true
				results = append(results, candRel)
			}
		}
	}
	return results
}

// ResolveTestFileToImplementation attempts to map a test file path (e.g. sympy/printing/tests/test_mathematica.py)
// to its direct corresponding implementation source file (e.g. sympy/printing/mathematica.py).
func ResolveTestFileToImplementation(repoDir, testFile string) string {
	if repoDir == "" {
		repoDir = "."
	}
	cleanTest := filepath.ToSlash(testFile)
	base := filepath.Base(cleanTest)

	implBase := base
	if strings.HasPrefix(base, "test_") {
		implBase = strings.TrimPrefix(base, "test_")
	} else if strings.Contains(base, "_test.") {
		implBase = strings.Replace(base, "_test.", ".", 1)
	} else if strings.Contains(base, "-test.") {
		implBase = strings.Replace(base, "-test.", ".", 1)
	}

	if implBase == base && (!isTestPath(cleanTest) || base == "tests.py" || base == "test.py" || base == "conftest.py" || base == "__init__.py") {
		return ""
	}

	dir := filepath.Dir(cleanTest)

	// Strategy 1: Direct parent peer (e.g. sympy/printing/tests/test_foo.py -> sympy/printing/foo.py)
	if filepath.Base(dir) == "tests" || filepath.Base(dir) == "test" || filepath.Base(dir) == "testing" {
		parentDir := filepath.Dir(dir)
		cand := filepath.ToSlash(filepath.Join(parentDir, implBase))
		if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(cand))); err == nil {
			return cand
		}
	}

	// Strategy 2: Same directory (e.g. foo/test_bar.py -> foo/bar.py)
	candSameDir := filepath.ToSlash(filepath.Join(dir, implBase))
	if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(candSameDir))); err == nil {
		return candSameDir
	}

	// Strategy 3: Mirrored source directory (e.g. tests/foo/test_bar.py -> src/foo/bar.py or foo/bar.py)
	trimmedPrefixes := []string{"tests/", "test/", "testing/"}
	for _, p := range trimmedPrefixes {
		if strings.HasPrefix(cleanTest, p) {
			relSub := strings.TrimPrefix(cleanTest, p)
			subDir := filepath.Dir(relSub)
			// Try directly without tests/
			candMirror := filepath.ToSlash(filepath.Join(subDir, implBase))
			if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(candMirror))); err == nil {
				return candMirror
			}
			// Try with src/ prefix
			candSrc := filepath.ToSlash(filepath.Join("src", subDir, implBase))
			if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(candSrc))); err == nil {
				return candSrc
			}
		}
	}

	// Strategy 4: Search workspace for unique matching implBase (excluding test directories)
	var matches []string
	var allNonTestPy []string
	_ = filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == ".venv" || name == "venv" || name == "__pycache__" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".py") {
			return nil
		}
		rel, relErr := filepath.Rel(repoDir, path)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if isTestPath(relSlash) {
			return nil
		}
		allNonTestPy = append(allNonTestPy, relSlash)
		if filepath.Base(path) == implBase {
			matches = append(matches, relSlash)
		}
		return nil
	})

	if len(matches) == 1 {
		return matches[0]
	}
	if len(matches) > 1 {
		dirSegs := strings.Split(filepath.ToSlash(dir), "/")
		var cleanSegs []string
		for _, s := range dirSegs {
			if s != "" && s != "." && s != "tests" && s != "test" && s != "testing" {
				cleanSegs = append(cleanSegs, s)
			}
		}
		subSuffix := ""
		if len(cleanSegs) > 0 {
			subSuffix = "/" + strings.Join(cleanSegs, "/") + "/" + implBase
		}
		bestMatch := ""
		bestScore := 0
		secondScore := 0
		for _, m := range matches {
			score := 0
			if subSuffix != "" && strings.HasSuffix(m, subSuffix) {
				score += 10
			}
			mDir := filepath.ToSlash(filepath.Dir(m))
			if filepath.Base(mDir) == filepath.Base(dir) {
				score += 4
			}
			mSegs := strings.Split(mDir, "/")
			for _, cs := range cleanSegs {
				for _, ms := range mSegs {
					if cs == ms {
						score += 2
						break
					}
				}
			}
			if score > bestScore {
				secondScore = bestScore
				bestScore = score
				bestMatch = m
			} else if score > secondScore {
				secondScore = score
			}
		}
		if bestScore > 0 && bestScore > secondScore {
			return bestMatch
		}
	}

	// Strategy 5: Underscore-separated module hierarchy (e.g. tests/test_ext_napoleon_docstring.py -> sphinx/ext/napoleon/docstring.py,
	// tests/test_domain_std.py -> sphinx/domains/std.py, tests/test_ext_autodoc_configs.py -> sphinx/ext/autodoc/__init__.py)
	if len(matches) == 0 && strings.Contains(implBase, "_") && strings.HasSuffix(implBase, ".py") {
		stem := strings.TrimSuffix(implBase, ".py")
		slashStem := strings.ReplaceAll(stem, "_", "/")
		altStem := slashStem
		altStem = strings.Replace(altStem, "domain/", "domains/", 1)
		altStem = strings.Replace(altStem, "domains/py", "domains/python", 1)
		altStem = strings.Replace(altStem, "build/", "builders/", 1)
		candidates := []string{slashStem}
		if altStem != slashStem {
			candidates = append(candidates, altStem)
		}
		for _, candS := range candidates {
			var suffixMatches []string
			for _, f := range allNonTestPy {
				if strings.HasSuffix(f, "/"+candS+".py") || f == candS+".py" {
					suffixMatches = append(suffixMatches, f)
				}
			}
			if len(suffixMatches) == 1 {
				return suffixMatches[0]
			}
			var initMatches []string
			for _, f := range allNonTestPy {
				if strings.HasSuffix(f, "/"+candS+"/__init__.py") || f == candS+"/__init__.py" {
					initMatches = append(initMatches, f)
				}
			}
			if len(initMatches) == 1 {
				return initMatches[0]
			}
		}
		// Try stripping trailing test-specific sub-stem (e.g. ext/autodoc/configs -> ext/autodoc/__init__.py)
		for _, candS := range candidates {
			if idx := strings.LastIndex(candS, "/"); idx > 0 {
				parentS := candS[:idx]
				for _, f := range allNonTestPy {
					if strings.HasSuffix(f, "/"+parentS+".py") || f == parentS+".py" {
						return f
					}
				}
				for _, f := range allNonTestPy {
					if strings.HasSuffix(f, "/"+parentS+"/__init__.py") || f == parentS+"/__init__.py" {
						return f
					}
				}
			}
		}
	}

	// Strategy 6: Package directory or <app>_tests/<subapp>_tests/ mapping (e.g. testing/test_assertion.py -> src/_pytest/assertion/util.py,
	// tests/forms_tests/field_tests/test_urlfield.py -> django/forms/fields.py)
	if len(matches) == 0 && strings.HasSuffix(implBase, ".py") {
		stem := strings.TrimSuffix(implBase, ".py")
		// Check if testFile imports a specific submodule from package `stem` (e.g. `import _pytest.assertion.util`)
		if testBytes, readErr := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(cleanTest))); readErr == nil {
			testLines := strings.Split(string(testBytes), "\n")
			for i := 0; i < len(testLines) && i < 60; i++ {
				tl := strings.TrimSpace(testLines[i])
				if strings.HasPrefix(tl, "import ") || strings.HasPrefix(tl, "from ") {
					for _, f := range allNonTestPy {
						if strings.Contains(f, "/"+stem+"/") && filepath.Base(f) != "__init__.py" {
							subStem := strings.TrimSuffix(filepath.Base(f), ".py")
							if strings.Contains(tl, stem+"."+subStem) || (strings.Contains(tl, "."+stem) && strings.Contains(tl, subStem)) {
								return f
							}
						}
					}
				}
			}
		}
		var initMatches []string
		for _, f := range allNonTestPy {
			if strings.HasSuffix(f, "/"+stem+"/__init__.py") || f == stem+"/__init__.py" {
				initMatches = append(initMatches, f)
			}
		}
		if len(initMatches) == 1 {
			return initMatches[0]
		}
		// Check <app>_tests/<singular>_tests directory pattern (e.g. tests/forms_tests/field_tests/...)
		dirParts := strings.Split(filepath.ToSlash(dir), "/")
		if len(dirParts) >= 2 {
			lastDir := strings.TrimSuffix(dirParts[len(dirParts)-1], "_tests")
			prevDir := strings.TrimSuffix(dirParts[len(dirParts)-2], "_tests")
			for _, candRel := range []string{prevDir + "/" + lastDir + "s.py", prevDir + "/" + lastDir + ".py"} {
				for _, f := range allNonTestPy {
					if strings.HasSuffix(f, "/"+candRel) || f == candRel {
						return f
					}
				}
			}
		}
	}

	return ""
}

// LocalizeFault evaluates issue descriptions, tracebacks, and repository symbols
// using the 4-tier localization algorithm to pinpoint causal files and multi-file dependencies.
func LocalizeFault(repoDir string, issueDesc string, traceback string) (*LocalizationResult, error) {
	issueDesc = strings.TrimSpace(issueDesc)
	traceback = strings.TrimSpace(traceback)

	if issueDesc == "" && traceback == "" {
		return nil, ErrNoDiagnostics
	}

	if repoDir == "" {
		repoDir = "."
	}
	absRepoDir, err := filepath.Abs(repoDir)
	if err == nil {
		repoDir = absRepoDir
	}

	// 1. Scan repository files and extract symbols
	allSymbols, fileSymbols := scanRepositorySymbols(repoDir)

	// 2. Build BM25 index over all repository symbols
	bm25Index := NewBM25SymbolIndexer(allSymbols)

	// 3. Parse traceback frames and reconcile against actual repository files when available
	frames := ParseTraceback(traceback)
	if len(fileSymbols) > 0 {
		for i := range frames {
			f := &frames[i]
			if strings.HasPrefix(f.File, "<") {
				f.IsStdlib = true
				continue
			}
			rel := resolveRelPath(repoDir, f.File)
			_, inSymbols := fileSymbols[rel]
			if !inSymbols {
				if fi, statErr := os.Stat(filepath.Join(repoDir, filepath.FromSlash(rel))); statErr == nil && !fi.IsDir() {
					inSymbols = true
				}
			}
			if inSymbols {
				f.File = rel
				f.IsStdlib = false
				f.IsTest = isTestPath(rel)
			} else if !f.IsStdlib {
				// External user reproduction script in issue description (e.g. test-mm.py, ./tmp-pv.py)
				f.IsTest = true
			}
		}
	}

	// 4. Perform AST backward-slicing
	sliceResult, _ := SliceASTBackward(repoDir, frames, allSymbols, issueDesc)

	// 5. Rank candidates across repository files
	candidates := rankCandidates(repoDir, issueDesc, traceback, frames, allSymbols, fileSymbols, bm25Index, sliceResult)

	if len(candidates) == 0 {
		// Fallback: If no candidate passed filters, pick the first non-test file discovered
		for file := range fileSymbols {
			if !isTestPath(file) {
				return &LocalizationResult{
					PrimaryFile:     file,
					CausalSymbol:    filepath.Base(file),
					LineRange:       [2]int{1, 30},
					Confidence:      0.25,
					SupportingFiles: []string{},
				}, nil
			}
		}
		return nil, errors.New("localizer: no suitable source candidates found in repository")
	}

	allowTests := isTestAuthoringIssue(issueDesc)
	var top CandidateScore
	var topFound bool
	for _, cand := range candidates {
		if !allowTests && isTestPath(cand.File) {
			continue
		}
		top = cand
		topFound = true
		break
	}
	if !topFound {
		if len(candidates) > 0 {
			top = candidates[0]
		} else {
			// Fallback: If no candidate passed filters, pick the first non-test file discovered
			for file := range fileSymbols {
				if !isTestPath(file) {
					return &LocalizationResult{
						PrimaryFile:     file,
						CausalSymbol:    filepath.Base(file),
						LineRange:       [2]int{1, 30},
						Confidence:      0.25,
						SupportingFiles: []string{},
					}, nil
				}
			}
			return nil, errors.New("localizer: no suitable source candidates found in repository")
		}
	}
	var supporting []string
	seenSupporting := make(map[string]bool)
	seenSupporting[top.File] = true

	topExt := strings.ToLower(filepath.Ext(top.File))
	for i := 1; i < len(candidates) && len(supporting) < 4; i++ {
		cand := candidates[i]
		candExt := strings.ToLower(filepath.Ext(cand.File))
		if topExt != "" && candExt != topExt {
			continue // Do not cross language boundaries (e.g. .js into .py)
		}
		if isTestPath(cand.File) {
			continue
		}
		if !seenSupporting[cand.File] && cand.TotalScore > 0.15 {
			seenSupporting[cand.File] = true
			supporting = append(supporting, cand.File)
		}
	}

	confidence := top.TotalScore
	if confidence > 0.99 {
		confidence = 0.99
	}
	if confidence < 0.50 && top.BM25Score > 0 {
		confidence = 0.55
	}

	primarySymbol := top.Symbol
	primaryLineRange := top.LineRange

	// Exception / Warning Stub De-referencing:
	// If the top candidate is an exception or warning stub (or <= 4 lines),
	// search within the same file for where this symbol is used / raised, or for the
	// primary implementation class/method matching the issue description.
	if isExceptionOrWarningStub(repoDir, top.File, primarySymbol, primaryLineRange) {
		if usageLine, usageSym, usageRange := FindSymbolUsageSiteInFile(repoDir, top.File, primarySymbol, issueDesc); usageLine > 0 && usageSym != "" {
			primarySymbol = usageSym
			primaryLineRange = usageRange
		} else if implLine, implSym, implRange := FindBestImplementationSymbolInFile(repoDir, top.File, issueDesc); implLine > 0 && implSym != "" {
			primarySymbol = implSym
			primaryLineRange = implRange
		}
	}

	return &LocalizationResult{
		PrimaryFile:     top.File,
		CausalSymbol:    primarySymbol,
		LineRange:       primaryLineRange,
		Confidence:      math.Round(confidence*100) / 100,
		SupportingFiles: supporting,
	}, nil
}

// scanRepositorySymbols walks repoDir and extracts SymbolTags for all supported source files.
func scanRepositorySymbols(repoDir string) ([]SymbolTag, map[string][]SymbolTag) {
	var allSymbols []SymbolTag
	fileSymbols := make(map[string][]SymbolTag)

	_ = filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if ignoredDirectories[base] || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs":
			// Process supported files
		default:
			return nil
		}

		rel, err := filepath.Rel(repoDir, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		tags, _ := extractFileSymbolsAndRefs(rel, string(data), ext)
		if len(tags) > 0 {
			fileSymbols[rel] = tags
			allSymbols = append(allSymbols, tags...)
		}
		return nil
	})

	return allSymbols, fileSymbols
}

// rankCandidates computes weighted scores for all candidate file and symbol pairs.
func rankCandidates(
	repoDir string,
	issueDesc string,
	traceback string,
	frames []TracebackFrame,
	allSymbols []SymbolTag,
	fileSymbols map[string][]SymbolTag,
	bm25Index *BM25SymbolIndex,
	sliceResult *ASTSliceResult,
) []CandidateScore {
	allowTests := isTestAuthoringIssue(issueDesc)

	// Build query string combining issue description and traceback error text
	queryParts := []string{issueDesc}
	if traceback != "" {
		for _, line := range strings.Split(traceback, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.Contains(trimmed, "Error") || strings.Contains(trimmed, "Exception") || strings.Contains(trimmed, "assert") {
				queryParts = append(queryParts, trimmed)
			}
		}
	}
	combinedQuery := strings.Join(queryParts, " ")

	// 1. Compute BM25 matches
	bm25Matches := bm25Index.Score(combinedQuery, 100)
	bm25ScoreMap := make(map[string]float64) // "file:symbol" -> score
	maxBM25 := 1.0
	for _, m := range bm25Matches {
		key := fmt.Sprintf("%s:%s", m.Symbol.File, m.Symbol.Name)
		bm25ScoreMap[key] = m.Score
		if m.Score > maxBM25 {
			maxBM25 = m.Score
		}
	}

	// 2. Compute Traceback proximity scores
	tracebackFileScores := make(map[string]float64)   // file -> max score
	tracebackSymbolScores := make(map[string]float64) // "file:symbol" -> score

	// Locate innermost non-test repository frame
	innermostRepoIdx := -1
	for i := len(frames) - 1; i >= 0; i-- {
		f := frames[i]
		if !f.IsStdlib && (!f.IsTest || allowTests) {
			innermostRepoIdx = i
			break
		}
	}

	issueProseLower := strings.ToLower(issueDesc)
	if traceback != "" {
		issueProseLower = strings.ReplaceAll(issueProseLower, strings.ToLower(traceback), "")
	}

	for idx, f := range frames {
		if f.IsStdlib {
			continue
		}
		if f.IsTest && !allowTests {
			continue
		}
		relFile := resolveRelPath(repoDir, f.File)
		// Innermost repo frame gets 1.0; preceding frames decrease by 0.15
		depthWeight := 1.0
		if innermostRepoIdx >= 0 && idx < innermostRepoIdx {
			depthWeight = 1.0 - float64(innermostRepoIdx-idx)*0.15
			if depthWeight < 0.2 {
				depthWeight = 0.2
			}
		}
		// If a traceback frame's non-dunder function is explicitly discussed in the issue prose, boost it
		fnLower := strings.ToLower(f.Function)
		if len(fnLower) >= 4 && !strings.HasPrefix(fnLower, "__") && !strings.HasPrefix(fnLower, "<") && strings.Contains(issueProseLower, fnLower) {
			depthWeight = 1.15
		}
		if depthWeight > tracebackFileScores[relFile] {
			tracebackFileScores[relFile] = depthWeight
		}

		if f.Function != "" && f.Function != "<module>" {
			symKey := fmt.Sprintf("%s:%s", relFile, f.Function)
			if depthWeight > tracebackSymbolScores[symKey] {
				tracebackSymbolScores[symKey] = depthWeight
			}
		}
	}

	// 3. Assemble candidate pool
	var candidates []CandidateScore
	seenCandidates := make(map[string]bool)

	// Add candidates from AST slice result
	if sliceResult != nil {
		if allowTests || !isTestPath(sliceResult.SourceFile) {
			key := fmt.Sprintf("%s:%s", sliceResult.SourceFile, sliceResult.Symbol)
			seenCandidates[key] = true
			tbVal := tracebackSymbolScores[key]
			if tbVal == 0 {
				tbVal = tracebackFileScores[sliceResult.SourceFile]
			}
			candidates = append(candidates, CandidateScore{
				File:           sliceResult.SourceFile,
				Symbol:         sliceResult.Symbol,
				LineRange:      sliceResult.LineRange,
				BM25Score:      bm25ScoreMap[key],
				ASTScore:       1.0,
				TracebackScore: tbVal,
			})
		}
	}

	// Add candidates from repository symbols
	for _, s := range allSymbols {
		if !allowTests && isTestPath(s.File) {
			continue
		}
		key := fmt.Sprintf("%s:%s", s.File, s.Name)
		if seenCandidates[key] {
			continue
		}
		seenCandidates[key] = true

		bm25Val := bm25ScoreMap[key]
		tbVal := tracebackSymbolScores[key]
		if tbVal == 0 {
			// Subordinate symbols in same file receive secondary proximity boost
			tbVal = tracebackFileScores[s.File] * 0.25
		}

		if bm25Val > 0 || tbVal > 0 {
			lineRange := findEnclosingSymbolRange(repoDir, s.File, s.Line, s.Name)
			astVal := 0.0
			if sliceResult != nil && s.File == sliceResult.SourceFile {
				astVal = 0.5
			}
			candidates = append(candidates, CandidateScore{
				File:           s.File,
				Symbol:         s.Name,
				LineRange:      lineRange,
				BM25Score:      bm25Val,
				TracebackScore: tbVal,
				ASTScore:       astVal,
			})
		}
	}

	// 4. Calculate weighted total scores
	// Score(f, s) = 0.35 * BM25 + 0.35 * TracebackDepth + 0.20 * ASTDependency + 0.10 * GitChurn
	const (
		w1 = 0.35 // BM25
		w2 = 0.35 // TracebackDepth
		w3 = 0.20 // ASTDependency
		w4 = 0.10 // GitChurn (file heuristic)
	)

	for i := range candidates {
		c := &candidates[i]
		normBM25 := c.BM25Score / maxBM25
		if normBM25 > 1.0 {
			normBM25 = 1.0
		}

		// Simple churn heuristic: files with more symbols or larger context get modest stability weight
		churnHeuristic := 0.5
		if tags := fileSymbols[c.File]; len(tags) > 5 {
			churnHeuristic = 0.8
		}
		c.GitChurnScore = churnHeuristic

		c.TotalScore = (w1 * normBM25) + (w2 * c.TracebackScore) + (w3 * c.ASTScore) + (w4 * c.GitChurnScore)
	}

	// 5. Sort candidates descending by TotalScore
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].TotalScore > candidates[j].TotalScore
	})

	return candidates
}

// FindInRepoFaultOrigin walks traceback frames backwards to locate the genuine semantic origin,
// skipping assertion, warning, and exception raise sites to identify the true in-repo caller.
// It also returns supportingFiles containing all other unique in-repo files present in the traceback.
func FindInRepoFaultOrigin(repoRoot string, rawOutput string) (targetFile string, targetLine int, targetFunc string, isStub bool, supportingFiles []string) {
	frames := ParseTraceback(rawOutput)
	if len(frames) == 0 {
		return "", 0, "", false, nil
	}

	absRepo, _ := filepath.Abs(repoRoot)

	var inRepoFrames []TracebackFrame
	for _, f := range frames {
		if f.IsStdlib || f.IsTest {
			continue
		}
		rel := filepath.ToSlash(f.File)
		if absRepo != "" && filepath.IsAbs(rel) {
			if r, err := filepath.Rel(absRepo, rel); err == nil && !strings.HasPrefix(r, "..") {
				rel = filepath.ToSlash(r)
			}
		}
		if isTestPath(rel) || isStdlibOrVendorPath(rel) {
			continue
		}
		f.File = rel
		inRepoFrames = append(inRepoFrames, f)
	}

	if len(inRepoFrames) == 0 {
		return "", 0, "", false, nil
	}

	// Walk backwards to find the first non-stub caller
	idx := len(inRepoFrames) - 1
	var stubFound bool

	for idx >= 0 {
		frame := inRepoFrames[idx]
		codeTrimmed := strings.TrimSpace(frame.Code)
		if codeTrimmed == "" && repoRoot != "" {
			fullPath := filepath.Join(repoRoot, frame.File)
			if data, err := os.ReadFile(fullPath); err == nil {
				fLines := strings.Split(string(data), "\n")
				if frame.Line > 0 && frame.Line <= len(fLines) {
					codeTrimmed = strings.TrimSpace(fLines[frame.Line-1])
				}
			}
		}

		frameIsStub := strings.HasPrefix(codeTrimmed, "raise ") ||
			strings.HasPrefix(codeTrimmed, "assert ") ||
			strings.Contains(codeTrimmed, "warnings.warn(") ||
			strings.Contains(codeTrimmed, "raise_from(")

		if frameIsStub {
			stubFound = true
			if idx > 0 {
				idx--
				continue
			}
		}
		break
	}

	targetFrame := inRepoFrames[idx]
	targetFile = targetFrame.File
	targetLine = targetFrame.Line
	targetFunc = targetFrame.Function
	isStub = stubFound

	// Collect supporting files (all unique in-repo files from frames except targetFile)
	seen := make(map[string]bool)
	seen[targetFile] = true
	for _, f := range inRepoFrames {
		if !seen[f.File] {
			seen[f.File] = true
			supportingFiles = append(supportingFiles, f.File)
		}
	}

	return targetFile, targetLine, targetFunc, isStub, supportingFiles
}

// ComputeSubtokenSynergyBoost computes the Ascent 5 Phase 4 CamelCase <-> snake_case
// subtoken synergy multiplier across file stem, parent class, and method/symbol name.
func ComputeSubtokenSynergyBoost(queryTokens []string, s SymbolTag) float64 {
	if len(queryTokens) == 0 {
		return 1.0
	}
	qSet := make(map[string]bool, len(queryTokens))
	for _, qt := range queryTokens {
		qSet[strings.ToLower(qt)] = true
	}

	hasOverlap := func(ident string) bool {
		if ident == "" {
			return false
		}
		for _, sub := range TokenizeIdentifier(ident) {
			if qSet[sub] {
				return true
			}
		}
		norm := strings.ReplaceAll(strings.ToLower(ident), "_", "")
		return len(norm) >= 4 && qSet[norm]
	}

	stem := strings.TrimSuffix(filepath.Base(s.File), filepath.Ext(s.File))
	matchedStem := hasOverlap(stem)
	matchedClass := hasOverlap(s.ParentClass)
	matchedSym := hasOverlap(s.Name)

	dimensionsMatched := 0
	if matchedStem {
		dimensionsMatched++
	}
	if matchedClass {
		dimensionsMatched++
	}
	if matchedSym {
		dimensionsMatched++
	}

	switch dimensionsMatched {
	case 3:
		return 1.65 // File stem + ParentClass + Method triple synergy
	case 2:
		return 1.30 // Pairwise Class+Method or File+Method synergy
	default:
		return 1.0
	}
}

// DiscoverMirrorTwins discovers coupled sync/async twins, 0-parameter mirror class/file methods,
// and sibling methods across a repository so the local model never patches only half of a twin pair.
func DiscoverMirrorTwins(primary SymbolTag, allSymbols []SymbolTag) []SymbolTag {
	if primary.Name == "" || len(allSymbols) == 0 {
		return nil
	}
	pName := primary.Name
	pLower := strings.ToLower(pName)
	pBase := filepath.Base(primary.File)

	// Candidate sync/async twin names
	twinNames := map[string]bool{
		pLower: true,
	}
	if strings.HasPrefix(pLower, "async_") {
		twinNames[strings.TrimPrefix(pLower, "async_")] = true
		twinNames["a"+strings.TrimPrefix(pLower, "async_")] = true
	} else if strings.HasPrefix(pLower, "a") && len(pLower) > 3 {
		twinNames[pLower[1:]] = true
		twinNames["async_"+pLower[1:]] = true
	} else {
		twinNames["async_"+pLower] = true
		twinNames["a"+pLower] = true
	}

	var twins []SymbolTag
	seen := make(map[string]bool)
	primaryKey := fmt.Sprintf("%s:%s:%s:%d", primary.File, primary.ParentClass, primary.Name, primary.Line)
	seen[primaryKey] = true

	for _, cand := range allSymbols {
		if isTestPath(cand.File) {
			continue
		}
		candKey := fmt.Sprintf("%s:%s:%s:%d", cand.File, cand.ParentClass, cand.Name, cand.Line)
		if seen[candKey] {
			continue
		}
		cLower := strings.ToLower(cand.Name)
		if !twinNames[cLower] {
			continue
		}

		isSyncAsyncPair := cLower != pLower && (cand.File == primary.File || cand.ParentClass == primary.ParentClass)
		isMirrorClassMethod := cLower == pLower && cand.File != primary.File &&
			((primary.ParentClass != "" && cand.ParentClass == primary.ParentClass) || filepath.Base(cand.File) == pBase)
		isSiblingClassMethod := cLower == pLower && cand.File == primary.File &&
			primary.ParentClass != "" && cand.ParentClass != "" && cand.ParentClass != primary.ParentClass

		if isSyncAsyncPair || isMirrorClassMethod || isSiblingClassMethod {
			seen[candKey] = true
			twins = append(twins, cand)
		}
	}
	return twins
}

// FormatDocstringSkippingPreview formats a compact inline code preview for [startLine, endLine]
// that retains the function/method signature, skips multi-line Python docstrings, and emits
// high-density executable body lines.
func FormatDocstringSkippingPreview(source string, startLine, endLine, maxLines int) string {
	lines := strings.Split(source, "\n")
	if len(lines) == 0 {
		return ""
	}
	if maxLines <= 0 {
		maxLines = 18
	}
	if startLine < 1 {
		startLine = 1
	}
	if endLine > len(lines) || endLine < startLine {
		endLine = len(lines)
	}

	var out []string
	inDocstring := false
	docDelim := ""
	skippedLines := 0

	for l := startLine; l <= endLine && len(out) < maxLines; l++ {
		raw := lines[l-1]
		trimmed := strings.TrimSpace(raw)

		if l == startLine {
			out = append(out, fmt.Sprintf("%4d: %s", l, raw))
			continue
		}

		if !inDocstring && skippedLines == 0 && (strings.HasPrefix(trimmed, `"""`) || strings.HasPrefix(trimmed, `'''`)) {
			delim := trimmed[:3]
			rest := trimmed[3:]
			if strings.Contains(rest, delim) {
				// Single-line docstring
				skippedLines++
				continue
			}
			inDocstring = true
			docDelim = delim
			skippedLines++
			continue
		}

		if inDocstring {
			skippedLines++
			if strings.Contains(trimmed, docDelim) {
				inDocstring = false
				out = append(out, fmt.Sprintf("      ... [%d docstring lines skipped] ...", skippedLines))
			}
			continue
		}

		out = append(out, fmt.Sprintf("%4d: %s", l, raw))
	}
	return strings.Join(out, "\n")
}

