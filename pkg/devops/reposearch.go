// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// reposearch.go implements fast, polyglot repository-wide code search, AST symbol
// resolution, and decorator/convention matching to connect test failures and CLI
// invocations to their genuine underlying implementations without hardcoded heuristics.

package devops

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type queryMeta struct {
	original string
	lower    string
}


// CodeSearchResult represents a single ranked match in the codebase.
type CodeSearchResult struct {
	File         string   `json:"file"`          // Relative path to file
	Line         int      `json:"line"`          // 1-indexed line number of target definition
	MatchKind    string   `json:"match_kind"`    // "command_decorator", "function_def", "class_def", "convention_def", "cooccurrence", "text_match"
	MatchedToken string   `json:"matched_token"` // The query token that triggered the match
	Score        int      `json:"score"`         // Relevance score
	LineText     string   `json:"line_text"`     // Content of matching line
	ContextLines []string `json:"context_lines"` // Enclosing function/class block lines
}

// CodeSearchOptions configures a repository-wide code search.
type CodeSearchOptions struct {
	WorkspaceDir string   `json:"workspace_dir"`
	RawQuery     string   `json:"raw_query,omitempty"`
	QueryTokens  []string `json:"query_tokens"`
	HintFiles    []string `json:"hint_files,omitempty"`   // High-priority candidate files (e.g. implementation matching test name)
	ExcludeTests bool     `json:"exclude_tests"`          // When true, skips test directories and files (default: true)
	MaxResults   int      `json:"max_results,omitempty"`  // Max results to return (default: 5)
	Compact      bool     `json:"compact,omitempty"`      // When true, omits context lines to reduce token consumption
}

// Regex patterns for candidate token extraction from test failures and commands
var (
	quotedStringRegex = regexp.MustCompile(`['"]([a-zA-Z0-9_\-\/]{2,})['"]`)
	methodCallRegex   = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*\.([a-zA-Z_][a-zA-Z0-9_]*)\(`)
	funcCallRegex     = regexp.MustCompile(`\b([a-zA-Z_][a-zA-Z0-9_]*)\(`)
	cliRunnerArgRegex = regexp.MustCompile(`(?:invoke|run|call)\([^,]+,\s*\[([^\]]+)\]`)

	// Polyglot definition regexes
	pyDefRegex        = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([a-zA-Z0-9_]+)\s*\(`)
	pyClassRegex      = regexp.MustCompile(`^\s*class\s+([a-zA-Z0-9_]+)(?:\(([^)]*)\))?:`)
	pyAssignRegex     = regexp.MustCompile(`^\s*([A-Z0-9_]{2,})\s*=\s*`)
	goFuncRegex       = regexp.MustCompile(`^\s*func\s+(?:\([^)]+\)\s+)?([a-zA-Z0-9_]+)\s*\(`)
	goReceiverRegex   = regexp.MustCompile(`^\s*func\s+\((?:[a-zA-Z0-9_]+\s+)?\*?([a-zA-Z0-9_]+)\)\s*([a-zA-Z0-9_]+)\s*\(`)
	goStructRegex     = regexp.MustCompile(`^\s*type\s+([a-zA-Z0-9_]+)\s+(?:struct|interface)`)
	goConstVarRegex   = regexp.MustCompile(`^\s*(?:const|var)\s+([a-zA-Z0-9_]+)\b`)
	tsFuncRegex       = regexp.MustCompile(`^\s*(?:export\s+)?(?:async\s+)?function\s+([a-zA-Z0-9_]+)\s*\(`)
	tsClassRegex      = regexp.MustCompile(`^\s*(?:export\s+)?class\s+([a-zA-Z0-9_]+)`)
	tsConstVarRegex   = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([a-zA-Z0-9_]+)\b`)
	rsFnRegex         = regexp.MustCompile(`^\s*(?:pub\s+)?(?:async\s+)?fn\s+([a-zA-Z0-9_]+)\s*\(`)
	rsStructRegex     = regexp.MustCompile(`^\s*(?:pub\s+)?struct\s+([a-zA-Z0-9_]+)`)

	camelCaseRegex        = regexp.MustCompile(`\b([A-Z][a-zA-Z0-9]{2,})\b`)

	// Command / route registration decorators across popular frameworks (Click, Flask, FastAPI, Typer, Express, etc.)
	commandDecoratorRegex = regexp.MustCompile(`@(?:[a-zA-Z0-9_]+\.)*(?:command|group|cli|app|bp|blueprint|router|route)\s*\(\s*(?:name\s*=\s*)?['"]([a-zA-Z0-9_\-\/]+)['"]`)
	routeDecoratorRegex   = regexp.MustCompile(`@(?:[a-zA-Z0-9_]+\.)*(?:route|get|post|put|delete|patch)\s*\(\s*['"]([a-zA-Z0-9_\-\/]+)['"]`)
)

// Common stopwords to filter out from candidate search tokens
var searchStopwords = map[string]bool{
	"assert": true, "assertion": true, "error": true, "failed": true, "failure": true,
	"test": true, "tests": true, "testing": true, "pytest": true, "runner": true,
	"invoke": true, "result": true, "output": true, "exit_code": true, "status": true,
	"true": true, "false": true, "none": true, "null": true, "self": true, "cls": true,
	"print": true, "len": true, "type": true, "str": true, "int": true, "bool": true,
	"list": true, "dict": true, "set": true, "tuple": true, "main": true, "args": true,
	"kwargs": true, "import": true, "from": true, "class": true, "def": true, "return": true,
	"path": true, "file": true, "name": true, "get": true, "value": true, "data": true,
	"item": true, "items": true, "key": true, "keys": true,
	"description": true, "modified": true, "last": true, "reported": true, "expected": true,
	"actual": true, "example": true, "reproducing": true, "case": true, "pass": true, "fail": true,
	"the": true, "not": true, "for": true, "with": true, "that": true, "this": true,
	"are": true, "was": true, "were": true, "when": true, "where": true, "which": true,
	"does": true, "did": true, "should": true, "would": true, "could": true, "have": true,
	"has": true, "had": true, "can": true, "will": true, "into": true, "all": true,
	"one": true, "other": true, "others": true, "code": true, "using": true, "used": true,
	"use": true, "also": true, "only": true, "some": true, "any": true, "new": true,
	"in": true, "is": true, "if": true, "or": true, "and": true, "to": true, "of": true,
	"on": true, "at": true, "by": true, "as": true, "be": true, "an": true, "it": true,
	"no": true, "so": true, "up": true, "out": true, "but": true, "diff": true, "git": true,
	"opt": true, "conda": true, "envs": true, "bin": true, "cacheprovider": true,
	"langsmith": true, "asyncio": true, "pytester": true, "filterwarnings": true,
	"addopts": true, "maxfail": true,
}

// ExtractCandidateTokens analyzes test failure messages, context lines, and the test command
// to extract meaningful candidate search tokens (quoted arguments, method calls, test names).
func ExtractCandidateTokens(failureMsg string, contextLines []string, cmdToRun string) []string {
	seen := make(map[string]bool)
	var tokens []string

	addToken := func(s string) {
		s = strings.TrimSpace(s)
		s = strings.Trim(s, `"'[](),:`)
		lower := strings.ToLower(s)
		if len(s) < 2 || len(s) > 50 {
			return
		}
		if searchStopwords[lower] {
			return
		}
		if !seen[s] {
			seen[s] = true
			tokens = append(tokens, s)
		}
	}

	// 1. Quoted string literals in CLI runner calls (e.g. runner.invoke(cli, ["routes"]))
	joinedContext := strings.Join(contextLines, "\n")
	for _, m := range cliRunnerArgRegex.FindAllStringSubmatch(joinedContext, -1) {
		if len(m) > 1 {
			for _, sub := range quotedStringRegex.FindAllStringSubmatch(m[1], -1) {
				if len(sub) > 1 {
					addToken(sub[1])
				}
			}
		}
	}

	// 2. All quoted string literals in context lines and failure message
	combined := failureMsg + "\n" + joinedContext
	for _, m := range quotedStringRegex.FindAllStringSubmatch(combined, -1) {
		if len(m) > 1 {
			addToken(m[1])
		}
	}
	multiQuoteRegex := regexp.MustCompile(`['"]([^'"]{2,})['"]`)
	for _, m := range multiQuoteRegex.FindAllStringSubmatch(combined, -1) {
		if len(m) > 1 {
			words := strings.Fields(m[1])
			for _, w := range words {
				addToken(w)
			}
		}
	}

	// 3. Method calls (e.g. obj.method_name(...))
	for _, m := range methodCallRegex.FindAllStringSubmatch(combined, -1) {
		if len(m) > 1 {
			addToken(m[1])
		}
	}

	// 4. Function calls
	for _, m := range funcCallRegex.FindAllStringSubmatch(combined, -1) {
		if len(m) > 1 {
			addToken(m[1])
		}
	}

	// 5. CamelCase class identifiers (e.g. FilePathField, ModelSerializer)
	for _, m := range camelCaseRegex.FindAllStringSubmatch(combined, -1) {
		if len(m) > 1 {
			addToken(m[1])
		}
	}

	// 6. Test name tokens from test command (e.g. test_routes_host, model_fields.test_durationfield.TestValidation.test_invalid_string)
	for _, word := range strings.Fields(cmdToRun) {
		if strings.HasPrefix(word, "-") || strings.HasPrefix(word, "/opt/") || strings.HasPrefix(word, "/usr/") ||
			strings.HasPrefix(word, "no:") || strings.HasPrefix(word, "filterwarnings") || strings.HasPrefix(word, "addopts") {
			continue
		}
		if strings.Contains(word, "::") || strings.Contains(word, ".") || strings.Contains(word, "/") {
			parts := strings.FieldsFunc(word, func(r rune) bool {
				return r == ':' || r == '.' || r == '/'
			})
			for _, p := range parts {
				p = strings.TrimSuffix(p, ".py")
				if len(p) < 3 {
					continue
				}
				cleanP := strings.TrimPrefix(strings.TrimPrefix(p, "Test"), "test_")
				for _, subPart := range strings.Split(cleanP, "_") {
					if len(subPart) >= 3 {
						addToken(subPart)
					}
				}
				if len(cleanP) >= 3 {
					addToken(cleanP)
				}
			}
		} else if strings.HasPrefix(filepath.Base(word), "test_") {
			base := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(word), "test_"), filepath.Ext(word))
			for _, subPart := range strings.Split(base, "_") {
				if len(subPart) >= 3 {
					addToken(subPart)
				}
			}
		}
	}

	return tokens
}

// isWordBoundaryMatch tests whether token appears in line as a whole identifier / bounded word.
func isWordBoundaryMatch(line string, token string) bool {
	lineLower := strings.ToLower(line)
	tokLower := strings.ToLower(token)
	idx := strings.Index(lineLower, tokLower)
	if idx == -1 {
		return false
	}
	// Check left boundary
	if idx > 0 {
		ch := line[idx-1]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' {
			return false
		}
	}
	// Check right boundary
	end := idx + len(token)
	if end < len(line) {
		ch := line[end]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' {
			return false
		}
	}
	return true
}

// SearchCodebase scans the workspace for candidate tokens, matching framework decorators,
// function/class definitions, conventions (e.g. _command, _handler), constants/variables,
// and literal token/substring occurrences.
func SearchCodebase(opts CodeSearchOptions) ([]CodeSearchResult, error) {
	if opts.WorkspaceDir == "" {
		opts.WorkspaceDir = "."
	}
	if opts.MaxResults <= 0 {
		opts.MaxResults = 5
	}
	if len(opts.QueryTokens) == 0 && strings.TrimSpace(opts.RawQuery) == "" {
		return nil, nil
	}

	queries := make([]queryMeta, 0, len(opts.QueryTokens)+4)
	seenQ := make(map[string]bool)

	addQuery := func(orig string) {
		orig = strings.TrimSpace(orig)
		if orig == "" {
			return
		}
		clean := strings.Trim(orig, "()*&\"',:;{}[]")
		cleanLower := strings.ToLower(clean)
		if clean != "" && !seenQ[cleanLower] {
			seenQ[cleanLower] = true
			queries = append(queries, queryMeta{original: clean, lower: cleanLower})
		}
		origLower := strings.ToLower(orig)
		if orig != clean && orig != "" && !seenQ[origLower] {
			seenQ[origLower] = true
			queries = append(queries, queryMeta{original: orig, lower: origLower})
		}
	}

	for _, t := range opts.QueryTokens {
		addQuery(t)
	}
	if opts.RawQuery != "" {
		addQuery(opts.RawQuery)
		// If raw query looks like "func (c *Collector) Reset", extract sub-parts
		if strings.Contains(opts.RawQuery, "func ") || strings.Contains(opts.RawQuery, "(") {
			for _, f := range strings.Fields(opts.RawQuery) {
				addQuery(f)
			}
		}
	}

	if len(queries) == 0 {
		return nil, nil
	}

	// Normalize hint files
	hintMap := make(map[string]bool)
	for _, h := range opts.HintFiles {
		hintMap[filepath.ToSlash(filepath.Clean(h))] = true
		hintMap[filepath.Base(h)] = true
	}

	var results []CodeSearchResult
	filesScanned := 0
	type cachedFile struct {
		rel   string
		lines []string
	}
	var cachedFiles []cachedFile

	err := filepath.WalkDir(opts.WorkspaceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, "sb_") || strings.HasPrefix(name, ".") || ignoredDirectories[name] || name == ".pytest_cache" || name == "__pycache__" || name == ".venv" || name == "venv" || name == "bin" || name == "dist" || name == "AppData" || name == "OneDrive" || name == "Library" {
				return filepath.SkipDir
			}
			if opts.ExcludeTests && (name == "tests" || name == "testing" || name == "test") {
				return filepath.SkipDir
			}
			return nil
		}

		if opts.ExcludeTests {
			if strings.HasPrefix(name, "test_") || strings.Contains(name, "_test.") || strings.Contains(name, ".test.") || strings.Contains(name, ".spec.") || name == "test.py" || name == "tests.py" {
				return nil
			}
			if strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".rst") || strings.HasSuffix(name, ".txt") || strings.EqualFold(name, "problem.md") || strings.EqualFold(name, "readme.md") {
				return nil
			}
		}

		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".py", ".go", ".ts", ".tsx", ".js", ".jsx", ".rs", ".json", ".yaml", ".yml", ".toml", ".sql", ".sh", ".ps1", ".md":
			// Process supported file
		default:
			return nil
		}

		if info, iErr := d.Info(); iErr == nil && info.Size() > 1024*1024 {
			return nil
		}
		filesScanned++
		if filesScanned > 5000 {
			return filepath.SkipAll
		}

		rel, relErr := filepath.Rel(opts.WorkspaceDir, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		lines := strings.Split(string(raw), "\n")
		if len(cachedFiles) < 500 {
			cachedFiles = append(cachedFiles, cachedFile{rel: rel, lines: lines})
		}
		fileResults := scanFileForTokens(rel, lines, queries, hintMap)
		results = append(results, fileResults...)

		return nil
	})

	// Fallback: if exact/compound query returned 0 matches, decompose CamelCase/snake_case into sub-tokens
	if len(results) == 0 && len(cachedFiles) > 0 {
		var subQueries []queryMeta
		seenSub := make(map[string]bool)
		for _, q := range queries {
			for _, sub := range TokenizeIdentifier(q.original) {
				if len(sub) >= 4 && !seenQ[sub] && !seenSub[sub] {
					seenSub[sub] = true
					subQueries = append(subQueries, queryMeta{original: sub, lower: sub})
				}
			}
		}
		if len(subQueries) > 0 {
			for _, cf := range cachedFiles {
				results = append(results, scanFileForTokens(cf.rel, cf.lines, subQueries, hintMap)...)
			}
		}
	}

	if err != nil && err != filepath.SkipAll {
		return nil, err
	}

	// Sort globally by score descending
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Line < results[j].Line
	})

	// Deduplicate by File + Line
	var deduped []CodeSearchResult
	seenMap := make(map[string]bool)
	for _, res := range results {
		key := fmt.Sprintf("%s:%d", res.File, res.Line)
		if !seenMap[key] {
			seenMap[key] = true
			deduped = append(deduped, res)
			if len(deduped) >= opts.MaxResults {
				break
			}
		}
	}

	// Populate ContextLines for top results unless Compact mode requested
	if !opts.Compact {
		for i := range deduped {
			raw, readErr := os.ReadFile(filepath.Join(opts.WorkspaceDir, deduped[i].File))
			if readErr == nil {
				lines := strings.Split(string(raw), "\n")
				deduped[i].ContextLines = ExtractFunctionBlockWithHints(lines, deduped[i].Line, opts.QueryTokens)
			}
		}
	}

	return deduped, nil
}

func scanFileForTokens(relFile string, lines []string, queries []queryMeta, hintMap map[string]bool) []CodeSearchResult {
	var matches []CodeSearchResult

	isHintFile := hintMap[relFile] || hintMap[filepath.Base(relFile)]
	isInSrc := strings.HasPrefix(relFile, "src/") || !strings.Contains(relFile, "/")

	for lineIdx := 0; lineIdx < len(lines); lineIdx++ {
		lineText := lines[lineIdx]
		trimmed := strings.TrimSpace(lineText)
		lineNum := lineIdx + 1

		// 1. Check Command / Route Decorators (e.g. @cli.command("routes"), @app.route("/users"))
		for _, q := range queries {
			cmdMatch := commandDecoratorRegex.FindStringSubmatch(trimmed)
			if len(cmdMatch) > 1 && strings.EqualFold(cmdMatch[1], q.lower) {
				score := 120
				if isHintFile {
					score += 25
				}
				if isInSrc {
					score += 15
				}
				// Find function line right after decorator (within next 5 lines)
				// Find function line after decorator chain (within next 25 lines)
				targetLine := lineNum
				for next := lineIdx + 1; next < len(lines) && next < lineIdx+25; next++ {
					trimmedNext := strings.TrimSpace(lines[next])
					if strings.HasPrefix(trimmedNext, "def ") ||
						strings.HasPrefix(trimmedNext, "func ") ||
						strings.HasPrefix(trimmedNext, "function ") {
						targetLine = next + 1
						break
					}
				}
				matches = append(matches, CodeSearchResult{
					File:         relFile,
					Line:         targetLine,
					MatchKind:    "command_decorator",
					MatchedToken: q.original,
					Score:        score,
					LineText:     lineText,
				})
				break
			}

			routeMatch := routeDecoratorRegex.FindStringSubmatch(trimmed)
			if len(routeMatch) > 1 && strings.EqualFold(strings.TrimPrefix(routeMatch[1], "/"), q.lower) {
				score := 115
				if isHintFile {
					score += 25
				}
				if isInSrc {
					score += 15
				}
				targetLine := lineNum
				for next := lineIdx + 1; next < len(lines) && next < lineIdx+25; next++ {
					trimmedNext := strings.TrimSpace(lines[next])
					if strings.HasPrefix(trimmedNext, "def ") ||
						strings.HasPrefix(trimmedNext, "func ") {
						targetLine = next + 1
						break
					}
				}
				matches = append(matches, CodeSearchResult{
					File:         relFile,
					Line:         targetLine,
					MatchKind:    "route_decorator",
					MatchedToken: q.original,
					Score:        score,
					LineText:     lineText,
				})
				break
			}
		}

		// 2. Check Function Definitions (e.g. def routes, func Routes, func (s *Server) Routes)
		var defName string
		var recvType string
		isDef := false
		if m := pyDefRegex.FindStringSubmatch(lineText); len(m) > 1 {
			defName = m[1]
			isDef = true
		} else if m := goReceiverRegex.FindStringSubmatch(lineText); len(m) > 2 {
			recvType = m[1]
			defName = m[2]
			isDef = true
		} else if m := goFuncRegex.FindStringSubmatch(lineText); len(m) > 1 {
			defName = m[1]
			isDef = true
		} else if m := tsFuncRegex.FindStringSubmatch(lineText); len(m) > 1 {
			defName = m[1]
			isDef = true
		} else if m := rsFnRegex.FindStringSubmatch(lineText); len(m) > 1 {
			defName = m[1]
			isDef = true
		}

		matchedThisLine := false

		if isDef {
			defLower := strings.ToLower(defName)
			recvLower := strings.ToLower(recvType)
			for _, q := range queries {
				// Exact function definition match (e.g. def routes, func Routes)
				if defLower == q.lower || (recvLower != "" && (recvLower == q.lower || recvLower+"."+defLower == q.lower)) {
					score := 105
					if recvLower != "" {
						score = 125
					}
					if isHintFile {
						score += 20
					}
					if isInSrc {
						score += 10
					}
					matches = append(matches, CodeSearchResult{
						File:         relFile,
						Line:         lineNum,
						MatchKind:    "function_def",
						MatchedToken: q.original,
						Score:        score,
						LineText:     lineText,
					})
					matchedThisLine = true
					break
				}

				// Naming convention match (e.g. routes_command, routes_cmd, handle_routes, routes_handler)
				if strings.HasPrefix(defLower, q.lower+"_") ||
					strings.HasSuffix(defLower, "_"+q.lower) ||
					strings.Contains(defLower, q.lower) {
					score := 85
					if strings.HasPrefix(defLower, q.lower+"_command") || strings.HasPrefix(defLower, q.lower+"_cmd") {
						score = 95 // CLI convention boost
					}
					if isHintFile {
						score += 20
					}
					if isInSrc {
						score += 10
					}
					matches = append(matches, CodeSearchResult{
						File:         relFile,
						Line:         lineNum,
						MatchKind:    "convention_def",
						MatchedToken: q.original,
						Score:        score,
						LineText:     lineText,
					})
					matchedThisLine = true
					break
				}
			}
		}

		// 3. Check Class Definitions (e.g. class Blueprint, struct Config, interface Collector)
		if !matchedThisLine {
			var clsName string
			isCls := false
			if m := pyClassRegex.FindStringSubmatch(lineText); len(m) > 1 {
				clsName = m[1]
				isCls = true
			} else if m := tsClassRegex.FindStringSubmatch(lineText); len(m) > 1 {
				clsName = m[1]
				isCls = true
			} else if m := goStructRegex.FindStringSubmatch(lineText); len(m) > 1 {
				clsName = m[1]
				isCls = true
			} else if m := rsStructRegex.FindStringSubmatch(lineText); len(m) > 1 {
				clsName = m[1]
				isCls = true
			}

			if isCls {
				clsLower := strings.ToLower(clsName)
				for _, q := range queries {
					if clsLower == q.lower {
						score := 110
						if isHintFile {
							score += 20
						}
						if isInSrc {
							score += 10
						}
						targetLine := lineNum
						for initIdx := lineIdx + 1; initIdx < len(lines) && initIdx < lineIdx+150; initIdx++ {
							tInit := strings.TrimSpace(lines[initIdx])
							if strings.HasPrefix(tInit, "def __init__(") {
								targetLine = initIdx + 1
								break
							}
							if strings.HasPrefix(tInit, "class ") {
								break
							}
						}
						matches = append(matches, CodeSearchResult{
							File:         relFile,
							Line:         targetLine,
							MatchKind:    "class_def",
							MatchedToken: q.original,
							Score:        score,
							LineText:     lineText,
						})
						matchedThisLine = true
						break
					}
				}
			}
		}

		// 4. Check Const & Var Definitions (e.g. const ReqTotal = ..., var inflight = ...)
		if !matchedThisLine {
			var cvName string
			isCV := false
			if m := goConstVarRegex.FindStringSubmatch(lineText); len(m) > 1 {
				cvName = m[1]
				isCV = true
			} else if m := tsConstVarRegex.FindStringSubmatch(lineText); len(m) > 1 {
				cvName = m[1]
				isCV = true
			} else if m := pyAssignRegex.FindStringSubmatch(lineText); len(m) > 1 {
				cvName = m[1]
				isCV = true
			}

			if isCV {
				cvLower := strings.ToLower(cvName)
				for _, q := range queries {
					if cvLower == q.lower {
						score := 90
						if isHintFile {
							score += 20
						}
						if isInSrc {
							score += 10
						}
						matches = append(matches, CodeSearchResult{
							File:         relFile,
							Line:         lineNum,
							MatchKind:    "const_var_def",
							MatchedToken: q.original,
							Score:        score,
							LineText:     lineText,
						})
						matchedThisLine = true
						break
					}
				}
			}
		}

		// 5. Literal Substring / Token Grep Fallback (e.g. ReqTotal, inflight, r.Pattern, struct fields)
		if !matchedThisLine {
			lineLower := strings.ToLower(lineText)
			for _, q := range queries {
				if len(q.lower) < 2 || searchStopwords[q.lower] {
					continue
				}
				if strings.Contains(lineLower, q.lower) {
					score := 50
					if isWordBoundaryMatch(lineText, q.original) {
						score = 75
						if strings.Contains(lineText, q.original) {
							score = 80 // exact-case word match
						}
					}
					if isHintFile {
						score += 20
					}
					if isInSrc {
						score += 10
					}
					matches = append(matches, CodeSearchResult{
						File:         relFile,
						Line:         lineNum,
						MatchKind:    "text_match",
						MatchedToken: q.original,
						Score:        score,
						LineText:     lineText,
					})
					break
				}
			}
		}
	}

	return matches
}

// ExtractFunctionBlock extracts a clean, token-efficient view of the function or class
// enclosing focusLine. It includes the primary decorator, skips long docstrings,
// strips inline comments, and compacts parameters to keep token usage strictly < 300.
func ExtractFunctionBlock(lines []string, focusLine int) []string {
	if len(lines) == 0 {
		return nil
	}
	if focusLine < 1 {
		focusLine = 1
	}
	if focusLine > len(lines) {
		focusLine = len(lines)
	}

	// 1. Walk upwards to find start of block (def/class/func)
	startLine := focusLine
	for l := focusLine; l >= 1 && l >= focusLine-25; l-- {
		trimmed := strings.TrimSpace(lines[l-1])
		if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "class ") ||
			strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(trimmed, "function ") {
			startLine = l
			break
		}
	}

	// Find the primary command/route decorator if present above startLine
	var primaryDecorator string
	for decLine := startLine - 1; decLine >= 1 && decLine >= startLine-25; decLine-- {
		decTrimmed := strings.TrimSpace(lines[decLine-1])
		if strings.HasPrefix(decTrimmed, "@") {
			if strings.Contains(decTrimmed, "command") || strings.Contains(decTrimmed, "route") {
				primaryDecorator = fmt.Sprintf("  %d: %s", decLine, decTrimmed)
				break
			}
		}
	}

	// 2. Read forward from startLine, skipping docstrings, blank lines, and compacting parameters
	var result []string
	if primaryDecorator != "" {
		result = append(result, primaryDecorator)
	}

	inDocstring := false
	docDelim := ""
	inParams := false
	paramCount := 0

	seenSigEnd := false
	trimmedStart := strings.TrimSpace(lines[startLine-1])
	if strings.HasSuffix(trimmedStart, ":") || strings.HasSuffix(trimmedStart, "{") {
		seenSigEnd = true
	}

	for l := startLine; l <= len(lines) && l < startLine+65; l++ {
		lineStr := lines[l-1]
		trimmed := strings.TrimSpace(lineStr)

		// Skip blank lines to conserve token budget
		if trimmed == "" {
			continue
		}

		// Handle docstring skipping
		if !inDocstring {
			if strings.HasPrefix(trimmed, `"""`) || strings.HasPrefix(trimmed, `'''`) {
				docDelim = trimmed[:3]
				if strings.Count(trimmed, docDelim) >= 2 && len(trimmed) > 3 {
					continue // Single line docstring
				}
				inDocstring = true
				continue
			}
		} else {
			if strings.Contains(trimmed, docDelim) {
				inDocstring = false
				docDelim = ""
			}
			continue
		}

		// Handle multi-line parameter signatures
		if (strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "func ")) &&
			strings.Contains(trimmed, "(") && !strings.Contains(trimmed, ")") {
			inParams = true
			paramCount = 0
		} else if inParams {
			if strings.Contains(trimmed, ")") {
				inParams = false
			} else {
				paramCount++
				// Skip optional default keyword arguments past 3 to save token budget
				if paramCount > 3 && strings.Contains(trimmed, "=") {
					continue
				}
			}
		}

		if !seenSigEnd {
			if strings.HasSuffix(trimmed, ":") || strings.HasSuffix(trimmed, "{") {
				seenSigEnd = true
			}
		}

		// Halt if we hit the next top-level def, class, decorator, or unindented statement
		if l > focusLine+2 && seenSigEnd {
			if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "class ") || strings.HasPrefix(trimmed, "@") {
				break
			}
			if len(lineStr) > 0 && lineStr[0] != ' ' && lineStr[0] != '\t' && trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, ")") && !strings.HasPrefix(trimmed, "->") {
				break
			}
		}

		cleanLine := lineStr
		if cIdx := strings.Index(cleanLine, "  # type: ignore"); cIdx != -1 {
			cleanLine = cleanLine[:cIdx]
		}
		if cIdx := strings.Index(cleanLine, "  # noqa"); cIdx != -1 {
			cleanLine = cleanLine[:cIdx]
		}

		prefix := "  "
		if l == focusLine {
			prefix = "> "
		}
		result = append(result, fmt.Sprintf("%s%d: %s", prefix, l, cleanLine))
		if len(result) >= 35 {
			break
		}
	}

	return result
}

// ExtractFunctionBlockWithHints extracts a clean, token-efficient view of the function or class
// enclosing focusLine. If hints (query tokens or error strings) match statements inside the
// function body (such as output formatting or return lines), it windows the context around
// that internal target while preserving the function signature.
func ExtractFunctionBlockWithHints(lines []string, focusLine int, hints []string) []string {
	if len(lines) == 0 {
		return nil
	}
	if focusLine < 1 {
		focusLine = 1
	}
	if focusLine > len(lines) {
		focusLine = len(lines)
	}

	// 1. Walk upwards to find start of block (def/class/func)
	startLine := focusLine
	for l := focusLine; l >= 1 && l >= focusLine-25; l-- {
		trimmed := strings.TrimSpace(lines[l-1])
		if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "class ") ||
			strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(trimmed, "function ") {
			startLine = l
			break
		}
	}

	// Find the end of this function/class block (first unindented statement or next def/class/@)
	endLine := len(lines)
	seenSigEndInHints := false
	trimmedStartHints := strings.TrimSpace(lines[startLine-1])
	if strings.HasSuffix(trimmedStartHints, ":") || strings.HasSuffix(trimmedStartHints, "{") {
		seenSigEndInHints = true
	}
	for l := startLine + 1; l <= len(lines); l++ {
		lineStr := lines[l-1]
		trimmed := strings.TrimSpace(lineStr)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !seenSigEndInHints {
			if strings.HasSuffix(trimmed, ":") || strings.HasSuffix(trimmed, "{") {
				seenSigEndInHints = true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "class ") || strings.HasPrefix(trimmed, "@") {
			endLine = l - 1
			break
		}
		if len(lineStr) > 0 && lineStr[0] != ' ' && lineStr[0] != '\t' && !strings.HasPrefix(trimmed, ")") && !strings.HasPrefix(trimmed, "->") {
			endLine = l - 1
			break
		}
	}

	// Score lines inside function body to find the most relevant statement (e.g. output formatting, headers, returns)
	internalTarget := -1
	bestScore := 0
	var hintLowers []string
	for _, h := range hints {
		h = strings.ToLower(strings.TrimSpace(h))
		if len(h) >= 3 {
			hintLowers = append(hintLowers, h)
		}
	}

	inDocstringScore := false
	docDelimScore := ""
	for l := startLine + 1; l <= endLine; l++ {
		lineStr := lines[l-1]
		trimmed := strings.TrimSpace(lineStr)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !inDocstringScore {
			if strings.HasPrefix(trimmed, `"""`) || strings.HasPrefix(trimmed, `'''`) {
				docDelimScore = trimmed[:3]
				if strings.Count(trimmed, docDelimScore) >= 2 && len(trimmed) > 3 {
					continue
				}
				inDocstringScore = true
				continue
			}
		} else {
			if strings.Contains(trimmed, docDelimScore) {
				inDocstringScore = false
				docDelimScore = ""
			}
			continue
		}

		trimmedLower := strings.ToLower(lineStr)
		score := 0
		for _, h := range hintLowers {
			if strings.Contains(trimmedLower, h) {
				// Bonus for word boundaries, quotes, dots, or tuples
				if strings.Contains(trimmedLower, " "+h) || strings.Contains(trimmedLower, "."+h) ||
					strings.Contains(trimmedLower, "\""+h) || strings.Contains(trimmedLower, "'"+h) ||
					strings.Contains(trimmedLower, "("+h) {
					score += 20
				} else {
					score += 12
				}
			}
		}
		// Strong bonus for output formatting and headers (core of CLI output tests)
		if strings.Contains(trimmedLower, "headers =") || strings.Contains(trimmedLower, "headers=") {
			score += 15
		} else if strings.Contains(trimmedLower, "click.echo") || strings.Contains(trimmedLower, "print(") {
			score += 4
		} else if strings.HasPrefix(strings.TrimSpace(trimmedLower), "return ") {
			score += 2
		}

		if score > bestScore {
			bestScore = score
			internalTarget = l
		}
	}

	// If total lines <= 20 or internal target is already near the start, take standard forward view
	if endLine-startLine <= 20 || internalTarget == -1 || internalTarget <= startLine+8 {
		return ExtractFunctionBlock(lines, focusLine)
	}

	windowStart := internalTarget
	// Walk back up to 4 lines if previous lines are control statements (if/for/try)
	for w := internalTarget - 1; w >= startLine+1 && w >= internalTarget-4; w-- {
		t := strings.TrimSpace(lines[w-1])
		if strings.HasPrefix(t, "if ") || strings.HasPrefix(t, "for ") || strings.HasPrefix(t, "try:") {
			windowStart = w
			break
		}
	}

	var result []string
	sigLine := strings.TrimSpace(lines[startLine-1])
	result = append(result, fmt.Sprintf("%d: %s", startLine, sigLine))

	inDocstring := false
	docDelim := ""
	for l := windowStart; l <= endLine && len(result) < 30; l++ {
		lineStr := lines[l-1]
		trimmed := strings.TrimSpace(lineStr)
		if trimmed == "" {
			continue
		}
		if !inDocstring {
			if strings.HasPrefix(trimmed, `"""`) || strings.HasPrefix(trimmed, `'''`) {
				docDelim = trimmed[:3]
				if strings.Count(trimmed, docDelim) >= 2 && len(trimmed) > 3 {
					continue
				}
				inDocstring = true
				continue
			}
		} else {
			if strings.Contains(trimmed, docDelim) {
				inDocstring = false
				docDelim = ""
			}
			continue
		}
		cleanLine := lineStr
		if cIdx := strings.Index(cleanLine, "  # type: ignore"); cIdx != -1 {
			cleanLine = cleanLine[:cIdx]
		}
		if cIdx := strings.Index(cleanLine, "  # noqa"); cIdx != -1 {
			cleanLine = cleanLine[:cIdx]
		}
		result = append(result, fmt.Sprintf("%d: %s", l, cleanLine))
	}

	return result
}
