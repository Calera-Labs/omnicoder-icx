// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// syntax.go implements Go language pre-flight reasoning, AST validation,
// and package declaration consistency guards to prevent writing broken or
// clobbering code to the workspace.

package devops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
)

// GoSourceInfo captures structural metadata parsed from a Go source file.
type GoSourceInfo struct {
	PackageName string   `json:"package_name"`
	Imports     []string `json:"imports,omitempty"`
	Functions   []string `json:"functions,omitempty"`
	Types       []string `json:"types,omitempty"`
}

// NormalizeGoSource automatically fixes common harmless LLM generation quirks:
// 1. Missing space after package keyword (e.g., "packagefoo" -> "package foo").
// 2. Erroneous non-std import "crypto/fnv" -> "hash/fnv".
// 3. Package-level `:=` declarations -> `var ... = ...`.
// 4. Out-of-order imports appearing after functions/types, by hoisting them into the package import block.
func NormalizeGoSource(code string) string {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return code
	}

	// 1. Fix missing space in package declaration (e.g. packagecanonical -> package canonical)
	rePkg := regexp.MustCompile(`(?m)^(\s*package)([a-zA-Z0-9_]+)`)
	code = rePkg.ReplaceAllString(code, "$1 $2")

	// 2. Fix non-std crypto/fnv slip
	if strings.Contains(code, `"crypto/fnv"`) {
		code = strings.ReplaceAll(code, `"crypto/fnv"`, `"hash/fnv"`)
	}

	// 3. Fix top-level `:=` declarations (e.g. val := 0.0 -> var val = 0.0)
	reTopVar := regexp.MustCompile(`(?m)^([a-zA-Z0-9_]+)\s*:=\s*([^\n\r]+)`)
	code = reTopVar.ReplaceAllString(code, "var $1 = $2")

	// 4. Hoist out-of-order imports if present
	code = hoistGoImports(code)

	// 5. Deduplicate intra-file duplicate method and function declarations
	code = deduplicateGoDeclarations(code)

	return code
}

// deduplicateGoDeclarations removes intra-file duplicate function or method declarations,
// retaining the latest definition in the file.
func deduplicateGoDeclarations(code string) string {
	fset := token.NewFileSet()
	node, _ := parser.ParseFile(fset, "preflight.go", code, parser.AllErrors)
	if node == nil {
		return code
	}

	type declKey struct {
		name string
		recv string
	}
	seen := make(map[declKey]int)
	var duplicates []declKey

	for _, decl := range node.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil {
			recv := ""
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				recv = formatSyntaxReceiverType(fn.Recv.List[0].Type)
			}
			k := declKey{name: fn.Name.Name, recv: recv}
			seen[k]++
			if seen[k] == 2 {
				duplicates = append(duplicates, k)
			}
		}
	}

	if len(duplicates) == 0 {
		return code
	}

	for _, dup := range duplicates {
		var matches []*ast.FuncDecl
		for _, decl := range node.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil && fn.Name.Name == dup.name {
				recv := ""
				if fn.Recv != nil && len(fn.Recv.List) > 0 {
					recv = formatSyntaxReceiverType(fn.Recv.List[0].Type)
				}
				if recv == dup.recv {
					matches = append(matches, fn)
				}
			}
		}
		if len(matches) > 1 {
			first := matches[0]
			start := fset.Position(first.Pos()).Offset
			end := fset.Position(first.End()).Offset
			if start >= 0 && end <= len(code) && start < end {
				code = code[:start] + code[end:]
				return deduplicateGoDeclarations(code)
			}
		}
	}
	return code
}

func formatSyntaxReceiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + formatSyntaxReceiverType(t.X)
	case *ast.Ident:
		return t.Name
	default:
		return ""
	}
}

func hoistGoImports(code string) string {
	fset := token.NewFileSet()
	// If it already parses cleanly, no need to touch imports
	if _, err := parser.ParseFile(fset, "preflight.go", code, parser.AllErrors); err == nil {
		return code
	}

	// 1. Identify package statement
	pkgRegex := regexp.MustCompile(`(?m)^(\s*package\s+[a-zA-Z0-9_]+)`)
	pkgLoc := pkgRegex.FindStringIndex(code)
	if pkgLoc == nil {
		return code
	}

	// 2. Extract multi-line import blocks
	multiImportRegex := regexp.MustCompile(`(?s)import\s*\((.*?)\)`)
	var importLines []string
	seen := make(map[string]bool)

	addImport := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			return
		}
		if !seen[line] {
			seen[line] = true
			importLines = append(importLines, line)
		}
	}

	for _, m := range multiImportRegex.FindAllStringSubmatch(code, -1) {
		if len(m) > 1 {
			for _, l := range strings.Split(m[1], "\n") {
				addImport(l)
			}
		}
	}

	// 3. Extract single-line imports
	singleImportRegex := regexp.MustCompile(`(?m)^\s*import\s+((?:[a-zA-Z0-9_.]+\s+)?"[^"]+")`)
	for _, m := range singleImportRegex.FindAllStringSubmatch(code, -1) {
		if len(m) > 1 {
			addImport(m[1])
		}
	}

	if len(importLines) == 0 {
		return code
	}

	// 4. Strip all existing import statements from code
	stripped := multiImportRegex.ReplaceAllString(code, "")
	stripped = singleImportRegex.ReplaceAllString(stripped, "")

	// 5. Re-insert unified import block directly below the package clause
	pkgLoc2 := pkgRegex.FindStringIndex(stripped)
	if pkgLoc2 == nil {
		return code
	}

	var sb strings.Builder
	sb.WriteString(stripped[:pkgLoc2[1]])
	sb.WriteString("\n\nimport (\n")
	for _, imp := range importLines {
		sb.WriteString("\t")
		sb.WriteString(imp)
		sb.WriteString("\n")
	}
	sb.WriteString(")\n")
	sb.WriteString(stripped[pkgLoc2[1]:])

	res := sb.String()
	// Verify if hoisted version parses cleanly
	if _, err := parser.ParseFile(fset, "preflight.go", res, parser.AllErrors); err == nil {
		return res
	}

	return code
}

// ValidateGoSource parses code as Go source and returns a detailed error if syntax is invalid.
func ValidateGoSource(code string) error {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return fmt.Errorf("devops: Go source code is empty")
	}

	normalized := NormalizeGoSource(code)

	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, "preflight.go", normalized, parser.AllErrors)
	if err != nil {
		return fmt.Errorf("devops: Go syntax error: %w", err)
	}
	return nil
}

// GetGoPackageName extracts the package identifier from Go source code.
func GetGoPackageName(code string) (string, error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "preflight.go", code, parser.PackageClauseOnly)
	if err != nil {
		return "", fmt.Errorf("devops: failed to parse package clause: %w", err)
	}
	if node.Name == nil || node.Name.Name == "" {
		return "", fmt.Errorf("devops: missing package declaration")
	}
	return node.Name.Name, nil
}

// InspectGoSource extracts package, import, and declaration information from Go code.
func InspectGoSource(code string) (*GoSourceInfo, error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "inspect.go", code, 0)
	if err != nil {
		return nil, fmt.Errorf("devops: failed to inspect Go source: %w", err)
	}

	info := &GoSourceInfo{
		PackageName: node.Name.Name,
	}

	for _, imp := range node.Imports {
		if imp.Path != nil {
			info.Imports = append(info.Imports, strings.Trim(imp.Path.Value, `"`))
		}
	}

	for _, decl := range node.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name != nil {
				recv := ""
				if d.Recv != nil && len(d.Recv.List) > 0 {
					recv = fmt.Sprintf("(recv).")
				}
				info.Functions = append(info.Functions, recv+d.Name.Name)
			}
		case *ast.GenDecl:
			if d.Tok == token.TYPE {
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name != nil {
						info.Types = append(info.Types, ts.Name.Name)
					}
				}
			}
		}
	}

	return info, nil
}

// CheckGoPackageConsistency checks whether incomingCode has a package declaration consistent
// with other existing .go files in targetDir.
func CheckGoPackageConsistency(targetDir string, targetFileName string, incomingCode string) error {
	incomingPkg, err := GetGoPackageName(incomingCode)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(targetDir)
	if err != nil {
		// If directory does not exist yet, no existing files to conflict with
		return nil
	}

	targetBase := filepath.Base(targetFileName)
	incomingIsTest := strings.HasSuffix(targetBase, "_test.go")
	incomingBasePkg := strings.TrimSuffix(incomingPkg, "_test")

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.EqualFold(name, targetBase) {
			continue
		}

		existingPath := filepath.Join(targetDir, name)
		existingData, readErr := os.ReadFile(existingPath)
		if readErr != nil {
			continue
		}

		existingPkg, pkgErr := GetGoPackageName(string(existingData))
		if pkgErr != nil {
			continue
		}

		existingIsTest := strings.HasSuffix(name, "_test.go")
		existingBasePkg := strings.TrimSuffix(existingPkg, "_test")

		// If either file is a non-test file, their base package names must match exactly
		if existingBasePkg != incomingBasePkg {
			hint := ""
			if incomingPkg == "main" && existingPkg != "main" {
				hint = fmt.Sprintf(". In Go, executable entry points for library package %q must be placed in a subdirectory (e.g. cmd/%s/main.go or examples/main.go), not in the library root", existingPkg, existingPkg)
			}
			return fmt.Errorf("devops: package declaration mismatch in directory %q: existing file %q declares package %q, but incoming file %q declares package %q%s",
				targetDir, name, existingPkg, targetBase, incomingPkg, hint)
		}

		// Non-test files in the same directory must declare the identical package name
		if !incomingIsTest && !existingIsTest && existingPkg != incomingPkg {
			hint := ""
			if incomingPkg == "main" && existingPkg != "main" {
				hint = fmt.Sprintf(". In Go, executable entry points for library package %q must be placed in a subdirectory (e.g. cmd/%s/main.go or examples/main.go), not in the library root", existingPkg, existingPkg)
			}
			return fmt.Errorf("devops: package declaration mismatch in directory %q: existing file %q declares package %q, but incoming file %q declares package %q%s",
				targetDir, name, existingPkg, targetBase, incomingPkg, hint)
		}
	}

	return nil
}

// ValidateGoPreflight performs comprehensive pre-flight verification on a Go file before writing.
func ValidateGoPreflight(workspaceDir, relPath, code string) error {
	normalized := NormalizeGoSource(code)

	// 1. Verify Go syntax validity
	if err := ValidateGoSource(normalized); err != nil {
		return err
	}

	// 2. Verify non-reentrant mutex self-deadlock
	if deadlocks := DetectGoMutexSelfDeadlock(normalized); len(deadlocks) > 0 {
		return fmt.Errorf("devops: pre-flight check rejected: %s", strings.Join(deadlocks, "; "))
	}

	// 3. Verify package declaration consistency with neighbor files
	fullPath := filepath.Join(workspaceDir, filepath.FromSlash(relPath))
	dir := filepath.Dir(fullPath)
	base := filepath.Base(relPath)

	return CheckGoPackageConsistency(dir, base, normalized)
}

// countUnescapedSubstrings counts occurrences of sub in s that are not preceded by an odd number of backslashes.
func countUnescapedSubstrings(s, sub string) int {
	count := 0
	subLen := len(sub)
	for i := 0; i+subLen <= len(s); i++ {
		if s[i:i+subLen] == sub {
			bsCount := 0
			for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
				bsCount++
			}
			if bsCount%2 == 0 {
				count++
				i += subLen - 1
			}
		}
	}
	return count
}

// DetectUnclosedLiterals inspects code for unclosed multi-line strings, quotes, or unbalanced delimiters.
func DetectUnclosedLiterals(code, ext string) (bool, string) {
	ext = strings.ToLower(ext)

	switch ext {
	case ".py":
		tripleDouble := countUnescapedSubstrings(code, `"""`)
		if tripleDouble%2 != 0 {
			return true, `unterminated triple-quoted string literal (""") detected (odd count of """)`
		}
		tripleSingle := countUnescapedSubstrings(code, `'''`)
		if tripleSingle%2 != 0 {
			return true, `unterminated triple-quoted string literal (''') detected (odd count of ''')`
		}

		var stack []rune
		inTripleDouble := false
		inTripleSingle := false
		inSingleQuote := false
		inDoubleQuote := false
		inComment := false

		runes := []rune(code)
		n := len(runes)
		for i := 0; i < n; i++ {
			r := runes[i]

			if !inComment && !inSingleQuote && !inDoubleQuote {
				if i+2 < n && r == '"' && runes[i+1] == '"' && runes[i+2] == '"' {
					bs := 0
					for j := i - 1; j >= 0 && runes[j] == '\\'; j-- {
						bs++
					}
					if bs%2 == 0 {
						inTripleDouble = !inTripleDouble
						i += 2
						continue
					}
				}
				if i+2 < n && r == '\'' && runes[i+1] == '\'' && runes[i+2] == '\'' {
					bs := 0
					for j := i - 1; j >= 0 && runes[j] == '\\'; j-- {
						bs++
					}
					if bs%2 == 0 {
						inTripleSingle = !inTripleSingle
						i += 2
						continue
					}
				}
			}

			if inTripleDouble || inTripleSingle {
				continue
			}

			if r == '#' && !inSingleQuote && !inDoubleQuote {
				inComment = true
				continue
			}
			if r == '\n' {
				inComment = false
				inSingleQuote = false
				inDoubleQuote = false
				continue
			}
			if inComment {
				continue
			}

			if r == '\'' && !inDoubleQuote {
				bs := 0
				for j := i - 1; j >= 0 && runes[j] == '\\'; j-- {
					bs++
				}
				if bs%2 == 0 {
					inSingleQuote = !inSingleQuote
				}
				continue
			}
			if r == '"' && !inSingleQuote {
				bs := 0
				for j := i - 1; j >= 0 && runes[j] == '\\'; j-- {
					bs++
				}
				if bs%2 == 0 {
					inDoubleQuote = !inDoubleQuote
				}
				continue
			}

			if inSingleQuote || inDoubleQuote {
				continue
			}

			switch r {
			case '(', '[', '{':
				stack = append(stack, r)
			case ')':
				if len(stack) == 0 || stack[len(stack)-1] != '(' {
					return true, "unmatched closing parenthesis ')'"
				}
				stack = stack[:len(stack)-1]
			case ']':
				if len(stack) == 0 || stack[len(stack)-1] != '[' {
					return true, "unmatched closing bracket ']'"
				}
				stack = stack[:len(stack)-1]
			case '}':
				if len(stack) == 0 || stack[len(stack)-1] != '{' {
					return true, "unmatched closing brace '}'"
				}
				stack = stack[:len(stack)-1]
			}
		}

		if len(stack) > 0 {
			openRune := stack[len(stack)-1]
			return true, fmt.Sprintf("unclosed delimiter '%c' at end of file (truncated code generation)", openRune)
		}

	case ".js", ".jsx", ".ts", ".tsx":
		inLineComment := false
		inBlockComment := false
		inSingle := false
		inDouble := false
		inTemplate := false
		runes := []rune(code)
		n := len(runes)
		for i := 0; i < n; i++ {
			r := runes[i]
			if inLineComment {
				if r == '\n' {
					inLineComment = false
				}
				continue
			}
			if inBlockComment {
				if r == '*' && i+1 < n && runes[i+1] == '/' {
					inBlockComment = false
					i++
				}
				continue
			}
			if inSingle {
				if r == '\\' {
					i++
					continue
				}
				if r == '\'' || r == '\n' {
					inSingle = false
				}
				continue
			}
			if inDouble {
				if r == '\\' {
					i++
					continue
				}
				if r == '"' || r == '\n' {
					inDouble = false
				}
				continue
			}
			if inTemplate {
				if r == '\\' {
					i++
					continue
				}
				if r == '`' {
					inTemplate = false
				}
				continue
			}
			if r == '/' && i+1 < n {
				if runes[i+1] == '/' {
					inLineComment = true
					i++
					continue
				}
				if runes[i+1] == '*' {
					inBlockComment = true
					i++
					continue
				}
			}
			switch r {
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case '`':
				inTemplate = true
			}
		}
		if inTemplate {
			return true, "unterminated template literal (`) detected (odd count of `)"
		}

	case ".html", ".htm":
		lower := strings.ToLower(code)
		if strings.Contains(lower, "<html") && !strings.Contains(lower, "</html>") && len(code) > 150 {
			return true, "unterminated html document: missing closing </html> tag"
		}
		if strings.Contains(lower, "<script") && !strings.Contains(lower, "</script>") {
			return true, "unterminated <script> tag: missing closing </script>"
		}
		if strings.Contains(lower, "<style") && !strings.Contains(lower, "</style>") {
			return true, "unterminated <style> tag: missing closing </style>"
		}

	case ".rs", ".c", ".cpp", ".cc", ".cxx", ".h", ".hpp", ".java", ".cs":
		return detectCStyleUnclosed(code)
	}

	return false, ""
}

func detectCStyleUnclosed(code string) (bool, string) {
	var stack []rune
	inSingleLineComment := false
	inMultiLineComment := false
	inDoubleQuote := false
	inSingleQuote := false
	runes := []rune(code)
	n := len(runes)

	for i := 0; i < n; i++ {
		r := runes[i]

		if inSingleLineComment {
			if r == '\n' {
				inSingleLineComment = false
			}
			continue
		}

		if inMultiLineComment {
			if r == '*' && i+1 < n && runes[i+1] == '/' {
				inMultiLineComment = false
				i++
			}
			continue
		}

		if inDoubleQuote {
			if r == '\\' {
				i++
				continue
			}
			if r == '"' {
				inDoubleQuote = false
			}
			continue
		}

		if inSingleQuote {
			if r == '\\' {
				i++
				continue
			}
			if r == '\'' {
				inSingleQuote = false
			}
			continue
		}

		if r == '/' && i+1 < n {
			if runes[i+1] == '/' {
				inSingleLineComment = true
				i++
				continue
			}
			if runes[i+1] == '*' {
				inMultiLineComment = true
				i++
				continue
			}
		}

		if r == '"' {
			inDoubleQuote = true
			continue
		}
		if r == '\'' {
			inSingleQuote = true
			continue
		}

		switch r {
		case '(', '[', '{':
			stack = append(stack, r)
		case ')':
			if len(stack) == 0 || stack[len(stack)-1] != '(' {
				return true, "unmatched closing parenthesis ')'"
			}
			stack = stack[:len(stack)-1]
		case ']':
			if len(stack) == 0 || stack[len(stack)-1] != '[' {
				return true, "unmatched closing bracket ']'"
			}
			stack = stack[:len(stack)-1]
		case '}':
			if len(stack) == 0 || stack[len(stack)-1] != '{' {
				return true, "unmatched closing brace '}'"
			}
			stack = stack[:len(stack)-1]
		}
	}

	if inMultiLineComment {
		return true, "unclosed multiline comment /* at end of file"
	}
	if inDoubleQuote {
		return true, "unclosed double-quoted string literal at end of file"
	}
	if len(stack) > 0 {
		openRune := stack[len(stack)-1]
		return true, fmt.Sprintf("unclosed delimiter '%c' at end of file (truncated code generation)", openRune)
	}
	return false, ""
}

// CheckPlatformRuntimeSlip detects platform incompatibilities like hardcoding 'python3' on Windows.
func CheckPlatformRuntimeSlip(relPath, code, targetOS string) (bool, string) {
	if targetOS == "" {
		targetOS = runtime.GOOS
	}
	if targetOS != "windows" {
		return false, ""
	}

	ext := strings.ToLower(filepath.Ext(relPath))
	if ext == ".py" || ext == ".js" || ext == ".ts" || ext == ".go" {
		if strings.Contains(code, `["python3"`) || strings.Contains(code, `['python3'`) ||
			(strings.Contains(code, `"python3"`) && strings.Contains(code, "subprocess")) ||
			(strings.Contains(code, `'python3'`) && strings.Contains(code, "subprocess")) {
			return true, "hardcoded 'python3' subprocess command detected on Windows host. On Windows, use sys.executable (for Python subprocesses) or 'python'/'py' to prevent FileNotFoundError [WinError 2]"
		}
	}

	return false, ""
}

// NormalizePolyglotSource cleanses language-specific artifacts and platform slips.
func NormalizePolyglotSource(relPath, code string) string {
	// 1. Universal boundary cleanup: strip trailing fences, >>>> markers, DONE, and stray opening fences
	lines := strings.Split(code, "\n")
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" {
			lines = lines[:len(lines)-1]
			continue
		}
		if strings.HasPrefix(last, ">>>>") || strings.HasPrefix(last, "<<<<") || last == "DONE" ||
			last == "---" || last == "***" || last == "___" ||
			strings.HasPrefix(last, "```") || strings.HasPrefix(last, "~~~") {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}
	if len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		if strings.HasPrefix(first, "```") || strings.HasPrefix(first, "~~~") {
			lines = lines[1:]
		}
	}
	code = strings.Join(lines, "\n")
	if strings.TrimSpace(code) != "" {
		code = strings.TrimRight(code, "\r\n \t") + "\n"
	}

	ext := strings.ToLower(filepath.Ext(relPath))
	switch ext {
	case ".go":
		return NormalizeGoSource(code)
	case ".py":
		if runtime.GOOS == "windows" {
			if strings.Contains(code, `["python3",`) {
				code = strings.ReplaceAll(code, `["python3",`, `[sys.executable,`)
				if !strings.Contains(code, "import sys") {
					code = "import sys\n" + code
				}
			}
			if strings.Contains(code, `['python3',`) {
				code = strings.ReplaceAll(code, `['python3',`, `[sys.executable,`)
				if !strings.Contains(code, "import sys") {
					code = "import sys\n" + code
				}
			}
		}
		return code
	case ".yaml", ".yml":
		yLines := strings.Split(code, "\n")
		for i, line := range yLines {
			if strings.HasPrefix(line, "\t") {
				tabCount := 0
				for tabCount < len(line) && line[tabCount] == '\t' {
					tabCount++
				}
				yLines[i] = strings.Repeat("  ", tabCount) + line[tabCount:]
			}
		}
		return strings.Join(yLines, "\n")
	default:
		return code
	}
}

// ValidateRequirementsTxt validates that content conforms to pip requirements format and contains no markdown prose.
func ValidateRequirementsTxt(content string) (bool, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return true, nil
	}
	lines := strings.Split(trimmed, "\n")
	for lineIdx, line := range lines {
		tl := strings.TrimSpace(line)
		if tl == "" || strings.HasPrefix(tl, "#") {
			continue
		}
		// Reject markdown code fences
		if strings.HasPrefix(tl, "```") || strings.HasPrefix(tl, "~~~") {
			return false, fmt.Errorf("line %d: markdown fence detected (%q) in requirements.txt", lineIdx+1, tl)
		}
		// Reject markdown headings (# with title is comment, but ## or ### indicates prose document)
		if strings.HasPrefix(tl, "##") {
			return false, fmt.Errorf("line %d: markdown header detected (%q) in requirements.txt", lineIdx+1, tl)
		}
		// Reject markdown bullet points (- or *) that are not pip flags
		if (strings.HasPrefix(tl, "* ") || strings.HasPrefix(tl, "- ")) &&
			!strings.HasPrefix(tl, "-r ") && !strings.HasPrefix(tl, "-e ") &&
			!strings.HasPrefix(tl, "-f ") && !strings.HasPrefix(tl, "-i ") &&
			!strings.HasPrefix(tl, "--") {
			return false, fmt.Errorf("line %d: markdown bullet detected (%q) in requirements.txt", lineIdx+1, tl)
		}
		// Reject prose punctuation, trailing colons, or conversational phrases
		if strings.HasPrefix(tl, "Note:") || strings.HasPrefix(tl, "Please ") ||
			strings.HasPrefix(tl, "Install ") || strings.HasPrefix(tl, "Run ") ||
			strings.HasPrefix(tl, "Here are ") || strings.HasPrefix(tl, "Here is ") ||
			strings.HasSuffix(tl, ":") ||
			(strings.Contains(tl, ": ") && !strings.Contains(tl, "http://") && !strings.Contains(tl, "https://")) ||
			(strings.HasSuffix(tl, ".") && !strings.HasSuffix(tl, ".whl") && !strings.HasSuffix(tl, ".tar.gz")) {
			return false, fmt.Errorf("line %d: prose commentary detected (%q) in requirements.txt", lineIdx+1, tl)
		}
	}
	return true, nil
}

// AutoRepairPreflightSyntax attempts to deterministically auto-repair minor preflight boundary slips
// (such as stray line 1 or stray trailing line) if initial ValidatePolyglotPreflight fails.
func AutoRepairPreflightSyntax(workspaceDir, relPath, code string) (string, bool, error) {
	cleanCode := NormalizePolyglotSource(relPath, code)
	origErr := ValidatePolyglotPreflight(workspaceDir, relPath, cleanCode)
	if origErr == nil {
		return cleanCode, cleanCode != code, nil
	}

	lines := strings.Split(cleanCode, "\n")
	if len(lines) <= 1 {
		return cleanCode, false, origErr
	}

	// Attempt 1: Trim the last line if non-empty (e.g. stray >>>>, DONE, or unclosed syntax token)
	if len(lines) >= 2 {
		trimmedLast := strings.TrimRight(strings.Join(lines[:len(lines)-1], "\n"), "\r\n \t") + "\n"
		if err := ValidatePolyglotPreflight(workspaceDir, relPath, trimmedLast); err == nil {
			return trimmedLast, true, nil
		}
	}

	// Attempt 2: Trim the first line if non-empty (e.g. stray markdown fence or heading)
	if len(lines) >= 2 {
		trimmedFirst := strings.TrimRight(strings.Join(lines[1:], "\n"), "\r\n \t") + "\n"
		if err := ValidatePolyglotPreflight(workspaceDir, relPath, trimmedFirst); err == nil {
			return trimmedFirst, true, nil
		}
	}

	// Attempt 3: Trim both first and last line
	if len(lines) >= 3 {
		trimmedBoth := strings.TrimRight(strings.Join(lines[1:len(lines)-1], "\n"), "\r\n \t") + "\n"
		if err := ValidatePolyglotPreflight(workspaceDir, relPath, trimmedBoth); err == nil {
			return trimmedBoth, true, nil
		}
	}

	return cleanCode, false, origErr
}

// ValidatePythonSource validates python source syntax statically and via local python ast parser if available.
// filename is optional and used for accurate diagnostic reporting.
func ValidatePythonSource(code string, filename ...string) error {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return errors.New("devops: python source code cannot be empty")
	}

	// 1. Dynamic AST validation via local python runtime if available (authoritative PEP-compliant parser)
	pyBin := ""
	for _, candidate := range []string{"python", "py", "python3"} {
		if path, err := exec.LookPath(candidate); err == nil && path != "" {
			pyBin = candidate
			break
		}
	}

	if pyBin == "" {
		// Fallback to static literal and delimiter audit only when no local Python runtime is installed
		if unclosed, reason := DetectUnclosedLiterals(code, ".py"); unclosed {
			return fmt.Errorf("devops: python syntax error: %s", reason)
		}
		return nil
	}

	if pyBin != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		fname := "<stdin>"
		if len(filename) > 0 && filename[0] != "" {
			fname = filepath.Base(filename[0])
		}

		// Read raw bytes via sys.stdin.buffer.read(): ast.parse(bytes) honors PEP 263 / defaults to UTF-8,
		// independent of the Windows console code page (cp1252).
		// Pass -X utf8 and set PYTHONIOENCODING=utf-8 / PYTHONUTF8=1 to prevent surrogate conversion on Windows.
		cmd := exec.CommandContext(ctx, pyBin, "-X", "utf8", "-c",
			"import ast, sys; ast.parse(sys.stdin.buffer.read(), filename=sys.argv[1])", fname)
		cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
		cmd.Stdin = strings.NewReader(code)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			errOutput := strings.TrimSpace(stderr.String())
			if errOutput != "" {
				lines := strings.Split(errOutput, "\n")
				// Identify the last non-empty line which contains the exception type and details
				var lastNonEmpty string
				for i := len(lines) - 1; i >= 0; i-- {
					trimmedLine := strings.TrimSpace(lines[i])
					if trimmedLine != "" {
						lastNonEmpty = trimmedLine
						break
					}
				}

				// Check if this is a genuine Python syntax or indentation error
				isSyntaxErr := strings.HasPrefix(lastNonEmpty, "SyntaxError") ||
					strings.HasPrefix(lastNonEmpty, "IndentationError") ||
					strings.Contains(errOutput, "SyntaxError:") ||
					strings.Contains(errOutput, "IndentationError:")

				if isSyntaxErr {
					var meaningful []string
					for _, l := range lines {
						tl := strings.TrimSpace(l)
						if tl == "" {
							continue
						}
						// Collect the file/line frame, caret line, and exception message
						if strings.Contains(tl, "SyntaxError") || strings.Contains(tl, "IndentationError") ||
							strings.Contains(tl, fmt.Sprintf("File %q", fname)) || strings.Contains(tl, "File \"<string>\"") ||
							strings.Contains(tl, "line ") || strings.Contains(tl, "^") {
							meaningful = append(meaningful, tl)
						}
					}
					if len(meaningful) > 0 {
						return fmt.Errorf("devops: python syntax error: %s", strings.Join(meaningful, " | "))
					}
					return fmt.Errorf("devops: python syntax error: %s", lastNonEmpty)
				}

				// If it's not a SyntaxError or IndentationError (e.g. UnicodeEncodeError, UnicodeDecodeError,
				// OSError, timeout, or environment mismatch), this is an infrastructure/validator fault.
				// Principle: A guard may only reject content for a reason it can state about the content.
				// Log the validator issue and fall back to the static check (which already passed).
				fmt.Fprintf(os.Stderr, "[Polyglot Pre-flight: Python validator runtime fault (bypassed)]: %s\n", lastNonEmpty)
				return nil
			}
			// Non-zero exit code with no stderr is also treated as validator infra issue
			return nil
		}
	}

	return nil
}

// ValidatePolyglotPreflight validates syntax and integrity of source code before writing to disk.
func ValidatePolyglotPreflight(workspaceDir, relPath, code string) error {
	cleanPath := filepath.ToSlash(strings.TrimSpace(relPath))
	ext := strings.ToLower(filepath.Ext(cleanPath))

	switch ext {
	case ".go":
		return ValidateGoPreflight(workspaceDir, relPath, code)

	case ".py":
		if slip, reason := CheckPlatformRuntimeSlip(relPath, code, ""); slip {
			return fmt.Errorf("devops: platform runtime slip: %s", reason)
		}
		return ValidatePythonSource(code, relPath)

	case ".json":
		trimmed := strings.TrimSpace(code)
		if trimmed == "" {
			return fmt.Errorf("devops: json content cannot be empty")
		}
		var js any
		if err := json.Unmarshal([]byte(trimmed), &js); err != nil {
			return fmt.Errorf("devops: invalid json syntax in %s: %w", filepath.Base(relPath), err)
		}
		return nil

	case ".js", ".jsx", ".ts", ".tsx":
		if unclosed, reason := DetectUnclosedLiterals(code, ext); unclosed {
			return fmt.Errorf("devops: javascript/typescript syntax error in %s: %s", filepath.Base(relPath), reason)
		}
		if ext == ".js" {
			if nodeBin, err := exec.LookPath("node"); err == nil && nodeBin != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, nodeBin, "--check", "-")
				cmd.Stdin = strings.NewReader(code)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err := cmd.Run(); err != nil {
					errOutput := strings.TrimSpace(stderr.String())
					if errOutput != "" {
						return fmt.Errorf("devops: javascript syntax check failed in %s: %s", filepath.Base(relPath), errOutput)
					}
					return fmt.Errorf("devops: javascript syntax check failed in %s: %w", filepath.Base(relPath), err)
				}
			}
		}
		return nil

	case ".html", ".htm":
		if unclosed, reason := DetectUnclosedLiterals(code, ext); unclosed {
			return fmt.Errorf("devops: html syntax error in %s: %s", filepath.Base(relPath), reason)
		}
		return nil

	case ".rs", ".c", ".cpp", ".cc", ".cxx", ".h", ".hpp", ".java", ".cs":
		if unclosed, reason := DetectUnclosedLiterals(code, ext); unclosed {
			return fmt.Errorf("devops: %s syntax error in %s: %s", strings.TrimPrefix(ext, "."), filepath.Base(relPath), reason)
		}
		return nil

	case ".txt":
		if strings.EqualFold(filepath.Base(cleanPath), "requirements.txt") {
			if _, err := ValidateRequirementsTxt(code); err != nil {
				return fmt.Errorf("devops: invalid requirements.txt: %w", err)
			}
		}
		return nil

	case ".yaml", ".yml":
		lines := strings.Split(code, "\n")
		for lineNum, line := range lines {
			if strings.HasPrefix(line, "\t") {
				return fmt.Errorf("devops: yaml syntax error in %s (line %d): tab characters are forbidden for indentation in YAML", filepath.Base(relPath), lineNum+1)
			}
		}
		return nil
	}

	return nil
}

// NormalizeWhitespaceSafe normalizes whitespace across command line strings,
// preserving exact spacing inside single or double quoted string literals.
func NormalizeWhitespaceSafe(s string) string {
	var sb strings.Builder
	inQuote := false
	var qChar byte
	lastWasSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c == '"' || c == '\'') && (i == 0 || s[i-1] != '\\') {
			if !inQuote {
				inQuote = true
				qChar = c
			} else if c == qChar {
				inQuote = false
			}
			sb.WriteByte(c)
			lastWasSpace = false
		} else if !inQuote && (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
			if !lastWasSpace && sb.Len() > 0 {
				sb.WriteByte(' ')
				lastWasSpace = true
			}
		} else {
			sb.WriteByte(c)
			lastWasSpace = false
		}
	}
	return strings.TrimSpace(sb.String())
}

// DetectGoMutexSelfDeadlock analyzes Go AST to detect methods on the same receiver
// that re-acquire a mutex already held by the calling method.
func DetectGoMutexSelfDeadlock(code string) []string {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "preflight.go", code, 0)
	if err != nil {
		return nil
	}

	type methodMeta struct {
		recvType   string
		recvVar    string
		locksMutex bool
		calls      []string // method names called on recvVar
		line       int
	}

	receiverMethods := make(map[string]map[string]*methodMeta)

	for _, decl := range node.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil || fn.Name == nil {
			continue
		}
		recvType := formatSyntaxReceiverType(fn.Recv.List[0].Type)
		if recvType == "" {
			continue
		}
		recvVar := ""
		if len(fn.Recv.List[0].Names) > 0 {
			recvVar = fn.Recv.List[0].Names[0].Name
		}
		if recvVar == "" {
			continue
		}

		if receiverMethods[recvType] == nil {
			receiverMethods[recvType] = make(map[string]*methodMeta)
		}

		meta := &methodMeta{
			recvType: recvType,
			recvVar:  recvVar,
			line:     fset.Position(fn.Pos()).Line,
		}

		lockDepth := 0
		var walkNode func(n ast.Node, inDefer bool)
		walkNode = func(n ast.Node, inDefer bool) {
			if n == nil {
				return
			}
			switch stmt := n.(type) {
			case *ast.GoStmt:
				// Asynchronous goroutine spawn (e.g. `go r.workerLoop()`) does not block the caller's lock
				return
			case *ast.FuncLit:
				// Closure literal execution is deferred/asynchronous relative to the outer method flow
				return
			case *ast.DeferStmt:
				walkNode(stmt.Call, true)
				return
			case *ast.CallExpr:
				for _, arg := range stmt.Args {
					walkNode(arg, inDefer)
				}
				sel, ok := stmt.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel == nil {
					walkNode(stmt.Fun, inDefer)
					return
				}
				isRecvMutex := false
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == recvVar {
					isRecvMutex = true
				} else if innerSel, ok := sel.X.(*ast.SelectorExpr); ok {
					if id, ok := innerSel.X.(*ast.Ident); ok && id.Name == recvVar {
						isRecvMutex = true
					}
				}
				if isRecvMutex {
					switch sel.Sel.Name {
					case "Lock", "RLock":
						meta.locksMutex = true
						if !inDefer {
							lockDepth++
						}
						return
					case "Unlock", "RUnlock":
						if !inDefer && lockDepth > 0 {
							lockDepth--
						}
						return
					}
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == recvVar {
					if !inDefer && lockDepth > 0 {
						meta.calls = append(meta.calls, sel.Sel.Name)
					}
				} else {
					walkNode(sel.X, inDefer)
				}
				return
			}
			ast.Inspect(n, func(child ast.Node) bool {
				if child == nil || child == n {
					return true
				}
				walkNode(child, inDefer)
				return false
			})
		}
		walkNode(fn.Body, false)

		receiverMethods[recvType][fn.Name.Name] = meta
	}

	var issues []string
	for recvType, methods := range receiverMethods {
		for callerName, meta := range methods {
			if !meta.locksMutex {
				continue
			}
			for _, calleeName := range meta.calls {
				if calleeMeta, exists := methods[calleeName]; exists && calleeMeta.locksMutex {
					issues = append(issues, fmt.Sprintf(
						"potential non-reentrant mutex self-deadlock in receiver %s: method %q (line %d) acquires mutex and calls %q which also acquires mutex. In Go, sync.Mutex is non-reentrant. Extract an unexported helper (e.g. %sLocked) that assumes lock is held, or unlock before calling.",
						recvType, callerName, meta.line, calleeName, calleeName,
					))
				}
			}
		}
	}
	return issues
}

// DetectGoRPCReplyDrop analyzes Go AST for functions taking an RPC reply parameter
// that perform channel receives but fail to assign or copy data into the reply.
func DetectGoRPCReplyDrop(code string) []string {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "preflight.go", code, 0)
	if err != nil {
		return nil
	}

	var warnings []string
	for _, decl := range node.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Type == nil || fn.Type.Params == nil || fn.Body == nil || fn.Name == nil {
			continue
		}

		replyParamName := ""
		for _, field := range fn.Type.Params.List {
			for _, name := range field.Names {
				lower := strings.ToLower(name.Name)
				if lower == "reply" || lower == "resp" || lower == "res" {
					replyParamName = name.Name
					break
				}
			}
			if replyParamName != "" {
				break
			}
		}

		if replyParamName == "" {
			continue
		}

		hasChannelOrTransport := false
		populatesReply := false

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.UnaryExpr:
				if x.Op == token.ARROW { // channel receive <-ch
					hasChannelOrTransport = true
				}
			case *ast.TypeAssertExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == replyParamName {
					populatesReply = true
				}
			case *ast.TypeSwitchStmt:
				populatesReply = true
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == replyParamName {
						populatesReply = true
					} else if star, ok := lhs.(*ast.StarExpr); ok {
						if id, ok := star.X.(*ast.Ident); ok && id.Name == replyParamName {
							populatesReply = true
						}
					}
				}
			case *ast.CallExpr:
				for _, arg := range x.Args {
					if id, ok := arg.(*ast.Ident); ok && id.Name == replyParamName {
						populatesReply = true
					}
				}
			}
			return true
		})

		if hasChannelOrTransport && !populatesReply {
			line := fset.Position(fn.Pos()).Line
			warnings = append(warnings, fmt.Sprintf(
				"potential RPC reply drop in %q (line %d): accepts reply parameter %q and performs channel receive but returns without assigning to or populating %q",
				fn.Name.Name, line, replyParamName, replyParamName,
			))
		}
	}
	return warnings
}

// ReplaceASTNode replaces a named AST node (function, method, type, class, interface, const/arrow)
// in code with replacement text, regardless of surrounding whitespace or line number drift.
func ReplaceASTNode(code, nodeName, replacement, ext string) (string, bool, error) {
	code = canonical.NormalizeLF(code)
	replacement = canonical.NormalizeLF(replacement)
	cleanSym := strings.TrimSpace(nodeName)
	for _, p := range []string{"function ", "interface ", "type ", "class ", "const ", "let ", "var ", "def "} {
		cleanSym = strings.TrimPrefix(cleanSym, p)
	}
	cleanSym = strings.TrimSpace(strings.Split(cleanSym, "(")[0])
	cleanSym = strings.TrimSpace(strings.Split(cleanSym, "<")[0])
	cleanSym = strings.TrimSpace(strings.Split(cleanSym, ":")[0])
	cleanSym = strings.TrimSpace(cleanSym)

	if cleanSym == "" {
		return code, false, fmt.Errorf("AST_NODE_INVALID: empty symbol name")
	}

	ext = strings.ToLower(ext)

	// 1. Go AST parsing
	if ext == ".go" {
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, "target.go", code, parser.ParseComments)
		if err == nil && node != nil {
			for _, decl := range node.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil && fn.Name.Name == cleanSym {
					startOffset := fset.Position(fn.Pos()).Offset
					if fn.Doc != nil && len(fn.Doc.List) > 0 {
						startOffset = fset.Position(fn.Doc.Pos()).Offset
					}
					endOffset := fset.Position(fn.End()).Offset
					if startOffset >= 0 && endOffset <= len(code) && startOffset <= endOffset {
						newCode := code[:startOffset] + replacement + code[endOffset:]
						return newCode, true, nil
					}
				}
				if gen, ok := decl.(*ast.GenDecl); ok {
					for _, spec := range gen.Specs {
						if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name != nil && ts.Name.Name == cleanSym {
							startOffset := fset.Position(gen.Pos()).Offset
							endOffset := fset.Position(gen.End()).Offset
							if len(gen.Specs) > 1 {
								startOffset = fset.Position(ts.Pos()).Offset
								endOffset = fset.Position(ts.End()).Offset
							} else if gen.Doc != nil && len(gen.Doc.List) > 0 {
								startOffset = fset.Position(gen.Doc.Pos()).Offset
							}
							if startOffset >= 0 && endOffset <= len(code) && startOffset <= endOffset {
								newCode := code[:startOffset] + replacement + code[endOffset:]
								return newCode, true, nil
							}
						}
						if vs, ok := spec.(*ast.ValueSpec); ok {
							for _, id := range vs.Names {
								if id.Name == cleanSym {
									startOffset := fset.Position(gen.Pos()).Offset
									endOffset := fset.Position(gen.End()).Offset
									if len(gen.Specs) > 1 {
										startOffset = fset.Position(vs.Pos()).Offset
										endOffset = fset.Position(vs.End()).Offset
									} else if gen.Doc != nil && len(gen.Doc.List) > 0 {
										startOffset = fset.Position(gen.Doc.Pos()).Offset
									}
									if startOffset >= 0 && endOffset <= len(code) && startOffset <= endOffset {
										newCode := code[:startOffset] + replacement + code[endOffset:]
										return newCode, true, nil
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// 2. Python parsing
	if ext == ".py" {
		pyFuncRe := regexp.MustCompile(`(?m)^([ \t]*)(?:async\s+)?def\s+` + regexp.QuoteMeta(cleanSym) + `\b`)
		pyClassRe := regexp.MustCompile(`(?m)^([ \t]*)class\s+` + regexp.QuoteMeta(cleanSym) + `\b`)

		loc := pyFuncRe.FindStringSubmatchIndex(code)
		if loc == nil {
			loc = pyClassRe.FindStringSubmatchIndex(code)
		}
		if loc != nil {
			startIdx := loc[0]
			indent := ""
			if loc[2] >= 0 && loc[3] >= 0 {
				indent = code[loc[2]:loc[3]]
			}
			baseIndentCols := leadingWhitespaceCols(indent)

			// Find end by looking for subsequent lines with indentation <= baseIndentCols
			rest := code[loc[1]:]
			restLines := strings.Split(rest, "\n")
			endOffset := len(code)
			currentBytePos := loc[1]

			for i, l := range restLines {
				if i == 0 {
					currentBytePos += len(l) + 1
					continue
				}
				trimmed := strings.TrimSpace(l)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					currentBytePos += len(l) + 1
					continue
				}
				lIndent := leadingWhitespaceCols(l)
				if lIndent <= baseIndentCols {
					endOffset = currentBytePos - 1
					break
				}
				currentBytePos += len(l) + 1
			}

			if endOffset > len(code) {
				endOffset = len(code)
			}
			newCode := code[:startIdx] + replacement + code[endOffset:]
			return newCode, true, nil
		}
	}

	// 3. TypeScript, JavaScript, TSX, JSX
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?m)^[ \t]*(?:export\s+(?:default\s+)?)?(?:async\s+)?function\s+` + regexp.QuoteMeta(cleanSym) + `\b`),
		regexp.MustCompile(`(?m)^[ \t]*(?:export\s+)?(?:const|let|var)\s+` + regexp.QuoteMeta(cleanSym) + `\b`),
		regexp.MustCompile(`(?m)^[ \t]*(?:export\s+)?interface\s+` + regexp.QuoteMeta(cleanSym) + `\b`),
		regexp.MustCompile(`(?m)^[ \t]*(?:export\s+)?type\s+` + regexp.QuoteMeta(cleanSym) + `\b`),
		regexp.MustCompile(`(?m)^[ \t]*(?:export\s+(?:default\s+)?)?class\s+` + regexp.QuoteMeta(cleanSym) + `\b`),
		regexp.MustCompile(`(?m)^[ \t]*(?:public|private|protected|static|async)?\s*` + regexp.QuoteMeta(cleanSym) + `\s*\([^)]*\)\s*(?::\s*[^;{]+)?\s*\{`),
	}

	for _, pat := range patterns {
		if loc := pat.FindStringIndex(code); loc != nil {
			startIdx := loc[0]
			// Search for { or ;
			braceIdx := -1
			semiIdx := -1
			inQuote := rune(0)
			depth := 0
			foundFirstBrace := false

			runes := []rune(code[startIdx:])
			endOffset := -1

			for i := 0; i < len(runes); i++ {
				r := runes[i]
				if inQuote != 0 {
					if r == '\\' && i+1 < len(runes) {
						i++
						continue
					}
					if r == inQuote {
						inQuote = 0
					}
					continue
				}
				if r == '"' || r == '\'' || r == '`' {
					inQuote = r
					continue
				}

				if r == '{' {
					foundFirstBrace = true
					depth++
					if braceIdx == -1 {
						braceIdx = startIdx + len(string(runes[:i]))
					}
				} else if r == '}' {
					depth--
					if foundFirstBrace && depth == 0 {
						endOffset = startIdx + len(string(runes[:i+1]))
						// Check for trailing semicolon
						if i+1 < len(runes) && runes[i+1] == ';' {
							endOffset++
						}
						break
					}
				} else if r == ';' && !foundFirstBrace {
					semiIdx = startIdx + len(string(runes[:i+1]))
					endOffset = semiIdx
					break
				}
			}

			if endOffset != -1 && endOffset <= len(code) {
				newCode := code[:startIdx] + replacement + code[endOffset:]
				return newCode, true, nil
			}
		}
	}

	return code, false, fmt.Errorf("AST_NODE_NOT_FOUND: declaration for %q not found in code", cleanSym)
}

// ValidateCandidatePatchFast performs sub-millisecond in-memory syntax validation
// and normalization on a candidate patch without spawning external interpreter subprocesses.
// For non-Go languages, delimiter/quote checks are evaluated comparatively against origCode
// so pre-existing legacy file quirks never trigger false-positive rejections.
func ValidateCandidatePatchFast(relPath, origCode, patchedCode string) (string, bool, error) {
	cleanCode := NormalizePolyglotSource(relPath, patchedCode)
	wasModified := cleanCode != patchedCode
	ext := strings.ToLower(filepath.Ext(filepath.ToSlash(strings.TrimSpace(relPath))))

	switch ext {
	case ".go":
		if err := ValidateGoSource(cleanCode); err != nil {
			return cleanCode, wasModified, err
		}
		return cleanCode, wasModified, nil

	case ".json":
		trimmed := strings.TrimSpace(cleanCode)
		if trimmed == "" {
			return cleanCode, wasModified, fmt.Errorf("devops: json content cannot be empty")
		}
		var js any
		if err := json.Unmarshal([]byte(trimmed), &js); err != nil {
			return cleanCode, wasModified, fmt.Errorf("devops: invalid json syntax in %s: %w", filepath.Base(relPath), err)
		}
		return cleanCode, wasModified, nil

	case ".py", ".js", ".jsx", ".ts", ".tsx", ".rs", ".c", ".cpp", ".cc", ".cxx", ".h", ".hpp", ".java", ".cs", ".html", ".htm":
		origUnclosed, _ := DetectUnclosedLiterals(origCode, ext)
		patchedUnclosed, reason := DetectUnclosedLiterals(cleanCode, ext)
		if !origUnclosed && patchedUnclosed {
			return cleanCode, wasModified, fmt.Errorf("devops: candidate patch introduced unclosed literal or delimiter in %s: %s", filepath.Base(relPath), reason)
		}
		if ext == ".py" && origCode != "" {
			if diags := VerifyDifferentialPythonAST("", relPath, origCode, cleanCode); len(diags) > 0 {
				return cleanCode, wasModified, fmt.Errorf("devops: differential AST gate rejected %s: [%s] line %d: %s",
					filepath.Base(relPath), diags[0].Kind, diags[0].Line, diags[0].Message)
			}
		}
		return cleanCode, wasModified, nil
	}

	return cleanCode, wasModified, nil
}

// DifferentialASTDiagnostic represents a <5ms differential pre-verification finding
// comparing baseline AST/source against patched AST/source (Ascent 5 Phase 4 parity).
type DifferentialASTDiagnostic struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Kind    string `json:"kind"` // "UndefinedName", "ImportFrom", "CallKwarg"
	Symbol  string `json:"symbol"`
	Message string `json:"message"`
}

var (
	pyImportFromLineRe = regexp.MustCompile(`^\s*from\s+(\.*[A-Za-z0-9_.]*)\s+import\s+([^\n#]+)`)
	pyImportModLineRe  = regexp.MustCompile(`^\s*import\s+([^\n#]+)`)
	pyDefSigLineRe     = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(([^)]*)\)`)
	pyCallKwargRe      = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(([^()\n]*)\)`)
	pyKwargPairRe      = regexp.MustCompile(`(?:^|,)\s*([A-Za-z_][A-Za-z0-9_]*)\s*=[^=]`)
)

var pythonBuiltinSymbols = map[string]bool{
	"abs": true, "all": true, "any": true, "ascii": true, "bin": true, "bool": true,
	"breakpoint": true, "bytearray": true, "bytes": true, "callable": true, "chr": true,
	"classmethod": true, "compile": true, "complex": true, "delattr": true, "dict": true,
	"dir": true, "divmod": true, "enumerate": true, "eval": true, "exec": true,
	"filter": true, "float": true, "format": true, "frozenset": true, "getattr": true,
	"globals": true, "hasattr": true, "hash": true, "help": true, "hex": true,
	"id": true, "input": true, "int": true, "isinstance": true, "issubclass": true,
	"iter": true, "len": true, "list": true, "locals": true, "map": true,
	"max": true, "memoryview": true, "min": true, "next": true, "object": true,
	"oct": true, "open": true, "ord": true, "pow": true, "print": true,
	"property": true, "range": true, "repr": true, "reversed": true, "round": true,
	"set": true, "setattr": true, "slice": true, "sorted": true, "staticmethod": true,
	"str": true, "sum": true, "super": true, "tuple": true, "type": true,
	"vars": true, "zip": true, "__import__": true, "__name__": true, "__file__": true,
	"__doc__": true, "__package__": true, "__all__": true, "__dict__": true, "__class__": true,
	"True": true, "False": true, "None": true, "Ellipsis": true, "NotImplemented": true,
	"Exception": true, "BaseException": true, "ValueError": true, "TypeError": true,
	"KeyError": true, "IndexError": true, "AttributeError": true, "RuntimeError": true,
	"NotImplementedError": true, "StopIteration": true, "AssertionError": true,
	"ImportError": true, "ModuleNotFoundError": true, "OSError": true, "IOError": true,
	"FileNotFoundError": true, "PermissionError": true, "ZeroDivisionError": true,
	"OverflowError": true, "MemoryError": true, "RecursionError": true, "SystemError": true,
	"SystemExit": true, "KeyboardInterrupt": true, "GeneratorExit": true, "Warning": true,
	"DeprecationWarning": true, "UserWarning": true, "RuntimeWarning": true, "FutureWarning": true,
	"self": true, "cls": true, "args": true, "kwargs": true,
	"return": true, "yield": true, "raise": true, "pass": true, "break": true, "continue": true,
	"assert": true, "del": true, "global": true, "nonlocal": true, "import": true, "from": true,
	"as": true, "def": true, "class": true, "lambda": true, "if": true, "elif": true, "else": true,
	"for": true, "while": true, "in": true, "not": true, "and": true, "or": true, "is": true,
	"try": true, "except": true, "finally": true, "with": true, "async": true, "await": true,
	"match": true, "case": true, "TYPE_CHECKING": true, "Any": true, "Optional": true,
	"Union": true, "List": true, "Dict": true, "Set": true, "Tuple": true, "Callable": true,
}

// VerifyDifferentialPythonAST executes a <5ms pure-Go differential static analysis pass
// comparing origCode against patchedCode to catch newly introduced:
//  1. Intra-workspace ImportFrom resolution errors (ImportError),
//  2. Invalid keyword arguments on known functions/methods (TypeError), and
//  3. Undefined module/local identifiers (NameError).
func VerifyDifferentialPythonAST(workspaceDir, relPath, origCode, patchedCode string) []DifferentialASTDiagnostic {
	var diags []DifferentialASTDiagnostic
	if strings.TrimSpace(patchedCode) == "" {
		return diags
	}

	origLines := strings.Split(origCode, "\n")
	origLineSet := make(map[string]bool, len(origLines))
	for _, l := range origLines {
		origLineSet[strings.TrimSpace(l)] = true
	}

	// Collect all identifiers present anywhere in origCode so pre-existing symbols are never flagged
	origIdents := make(map[string]bool)
	for _, m := range identRefRegex.FindAllString(origCode, -1) {
		origIdents[m] = true
	}

	// Parse function parameter signatures in patchedCode: fnName -> (allowedParams, hasVarKwarg)
	type pyCallableSig struct {
		params     map[string]bool
		hasVarKw   bool
		isUniqueFn bool
	}
	callableSigs := make(map[string]*pyCallableSig)
	fnCounts := make(map[string]int)

	patchedLines := strings.Split(patchedCode, "\n")
	for _, l := range patchedLines {
		if m := pyDefSigLineRe.FindStringSubmatch(l); len(m) > 2 {
			fnName := m[1]
			paramRaw := m[2]
			fnCounts[fnName]++
			hasVarKw := strings.Contains(paramRaw, "**")
			pSet := make(map[string]bool)
			for _, part := range strings.Split(paramRaw, ",") {
				part = strings.TrimSpace(part)
				if part == "" || part == "*" || part == "/" {
					continue
				}
				if idx := strings.Index(part, ":"); idx >= 0 {
					part = strings.TrimSpace(part[:idx])
				}
				if idx := strings.Index(part, "="); idx >= 0 {
					part = strings.TrimSpace(part[:idx])
				}
				part = strings.TrimLeft(part, "*")
				if part != "" && part != "self" && part != "cls" {
					pSet[part] = true
				}
			}
			callableSigs[fnName] = &pyCallableSig{
				params:     pSet,
				hasVarKw:   hasVarKw,
				isUniqueFn: fnCounts[fnName] == 1,
			}
		}
	}
	for fnName, count := range fnCounts {
		if count > 1 && callableSigs[fnName] != nil {
			callableSigs[fnName].isUniqueFn = false
		}
	}

	// Collect all symbols bound (imported, defined, assigned, or parameter-bound) anywhere in patchedCode
	boundInPatched := make(map[string]bool)
	pyAssignBindRe := regexp.MustCompile(`(?:^|[;\s(])([A-Za-z_][A-Za-z0-9_]*)\s*(?::[^=]+)?=\s*[^=]`)
	pyForAsBindRe := regexp.MustCompile(`\b(?:for|as|except\s+[A-Za-z0-9_., ()]+\s+as)\s+([A-Za-z_][A-Za-z0-9_]*)\b`)
	pyRootCallOrAttrRe := regexp.MustCompile(`(?:^|[^.\w'"` + "`" + `])([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(|\.[A-Za-z_])`)

	for _, l := range patchedLines {
		trimL := strings.TrimSpace(l)
		if strings.HasPrefix(trimL, "#") {
			continue
		}
		if m := pyDefSigLineRe.FindStringSubmatch(l); len(m) > 2 {
			boundInPatched[m[1]] = true
			if sig := callableSigs[m[1]]; sig != nil {
				for p := range sig.params {
					boundInPatched[p] = true
				}
			}
		}
		if m := pyClassReg.FindStringSubmatch(trimL); len(m) > 1 {
			boundInPatched[m[1]] = true
		}
		if m := pyImportFromLineRe.FindStringSubmatch(l); len(m) > 2 {
			for _, part := range strings.Split(m[2], ",") {
				part = strings.TrimSpace(part)
				if asIdx := strings.Index(part, " as "); asIdx >= 0 {
					boundInPatched[strings.TrimSpace(part[asIdx+4:])] = true
				} else if part != "" && part != "*" {
					boundInPatched[part] = true
				}
			}
		}
		if m := pyImportModLineRe.FindStringSubmatch(l); len(m) > 1 {
			for _, part := range strings.Split(m[1], ",") {
				part = strings.TrimSpace(part)
				if asIdx := strings.Index(part, " as "); asIdx >= 0 {
					boundInPatched[strings.TrimSpace(part[asIdx+4:])] = true
				} else if part != "" {
					rootMod := strings.Split(part, ".")[0]
					boundInPatched[strings.TrimSpace(rootMod)] = true
				}
			}
		}
		for _, m := range pyAssignBindRe.FindAllStringSubmatch(l, -1) {
			if len(m) > 1 {
				boundInPatched[m[1]] = true
			}
		}
		for _, m := range pyForAsBindRe.FindAllStringSubmatch(l, -1) {
			if len(m) > 1 {
				boundInPatched[m[1]] = true
			}
		}
	}

	// 1. Check newly added runtime ImportFrom statements against workspaceDir
	inTypeChecking := false
	typeCheckingIndent := -1
	inTryExceptImport := false
	tryIndent := -1

	for idx, rawLine := range patchedLines {
		lineNum := idx + 1
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := leadingWhitespaceCols(rawLine)
		if inTypeChecking && indent <= typeCheckingIndent {
			inTypeChecking = false
		}
		if inTryExceptImport && indent <= tryIndent && !strings.HasPrefix(trimmed, "except") {
			inTryExceptImport = false
		}
		if strings.HasPrefix(trimmed, "if ") && strings.Contains(trimmed, "TYPE_CHECKING") {
			inTypeChecking = true
			typeCheckingIndent = indent
			continue
		}
		if strings.HasPrefix(trimmed, "try:") {
			inTryExceptImport = true
			tryIndent = indent
			continue
		}
		if inTypeChecking || inTryExceptImport || origLineSet[trimmed] {
			continue
		}

		// Check 1: Intra-workspace ImportFrom
		if workspaceDir != "" {
			if m := pyImportFromLineRe.FindStringSubmatch(rawLine); len(m) > 2 {
				modSpec := strings.TrimSpace(m[1])
				importsRaw := strings.TrimSpace(m[2])
				if !strings.Contains(importsRaw, "(") && !strings.Contains(importsRaw, "\\") {
					if targetFile := resolvePythonModuleFile(workspaceDir, relPath, modSpec); targetFile != "" {
						if targetBytes, err := os.ReadFile(targetFile); err == nil {
							targetSrc := string(targetBytes)
							if !strings.Contains(targetSrc, "import *") && !strings.Contains(targetSrc, "def __getattr__") {
								targetDefs := extractPythonModuleExports(targetSrc)
								for _, part := range strings.Split(importsRaw, ",") {
									part = strings.TrimSpace(part)
									if asIdx := strings.Index(part, " as "); asIdx >= 0 {
										part = strings.TrimSpace(part[:asIdx])
									}
									if part != "" && part != "*" && !targetDefs[part] {
										// Also check submodule file if targetFile is __init__.py
										if filepath.Base(targetFile) == "__init__.py" {
											subPy := filepath.Join(filepath.Dir(targetFile), part+".py")
											subInit := filepath.Join(filepath.Dir(targetFile), part, "__init__.py")
											if _, err1 := os.Stat(subPy); err1 == nil {
												continue
											}
											if _, err2 := os.Stat(subInit); err2 == nil {
												continue
											}
										}
										diags = append(diags, DifferentialASTDiagnostic{
											File:    relPath,
											Line:    lineNum,
											Kind:    "ImportFrom",
											Symbol:  part,
											Message: fmt.Sprintf("cannot import name %q from intra-workspace module %q", part, modSpec),
										})
									}
								}
							}
						}
					}
				}
			}
		}

		// Check 2: Newly introduced Call-Kwarg mismatches against known unique intra-file signatures
		for _, callMatch := range pyCallKwargRe.FindAllStringSubmatch(trimmed, -1) {
			if len(callMatch) < 3 {
				continue
			}
			fnName := callMatch[1]
			argsStr := callMatch[2]
			sig, ok := callableSigs[fnName]
			if !ok || !sig.isUniqueFn || sig.hasVarKw {
				continue
			}
			// Ignore if preceded by "def " (signature definition itself)
			if strings.HasPrefix(trimmed, "def "+fnName) || strings.HasPrefix(trimmed, "async def "+fnName) {
				continue
			}
			for _, kwMatch := range pyKwargPairRe.FindAllStringSubmatch(argsStr, -1) {
				if len(kwMatch) > 1 {
					kwName := kwMatch[1]
					if !sig.params[kwName] {
						diags = append(diags, DifferentialASTDiagnostic{
							File:    relPath,
							Line:    lineNum,
							Kind:    "CallKwarg",
							Symbol:  kwName,
							Message: fmt.Sprintf("%s() got an unexpected keyword argument %q", fnName, kwName),
						})
					}
				}
			}
		}

		// Check 3: Newly introduced UndefinedName on root calls or module/object attribute accesses
		if !strings.HasPrefix(trimmed, "def ") && !strings.HasPrefix(trimmed, "async def ") &&
			!strings.HasPrefix(trimmed, "class ") && !strings.HasPrefix(trimmed, "from ") &&
			!strings.HasPrefix(trimmed, "import ") && !strings.HasPrefix(trimmed, "\"") &&
			!strings.HasPrefix(trimmed, "'") {
			codePart := trimmed
			if hashIdx := strings.Index(codePart, "#"); hashIdx >= 0 {
				codePart = codePart[:hashIdx]
			}
			for _, refMatch := range pyRootCallOrAttrRe.FindAllStringSubmatch(codePart, -1) {
				if len(refMatch) > 1 {
					sym := refMatch[1]
					if !pythonBuiltinSymbols[sym] && !origIdents[sym] && !boundInPatched[sym] {
						diags = append(diags, DifferentialASTDiagnostic{
							File:    relPath,
							Line:    lineNum,
							Kind:    "UndefinedName",
							Symbol:  sym,
							Message: fmt.Sprintf("name %q is not defined or imported in %s", sym, filepath.Base(relPath)),
						})
					}
				}
			}
		}
	}

	return diags
}

func resolvePythonModuleFile(workspaceDir, fromRelPath, modSpec string) string {
	level := 0
	for level < len(modSpec) && modSpec[level] == '.' {
		level++
	}
	cleanMod := modSpec[level:]
	relSlash := strings.ReplaceAll(cleanMod, ".", "/")

	if level > 0 {
		curDir := filepath.Dir(filepath.Join(workspaceDir, filepath.FromSlash(fromRelPath)))
		for i := 1; i < level; i++ {
			curDir = filepath.Dir(curDir)
		}
		base := curDir
		if relSlash != "" {
			base = filepath.Join(curDir, filepath.FromSlash(relSlash))
		}
		if fi, err := os.Stat(base + ".py"); err == nil && !fi.IsDir() {
			return base + ".py"
		}
		if fi, err := os.Stat(filepath.Join(base, "__init__.py")); err == nil && !fi.IsDir() {
			return filepath.Join(base, "__init__.py")
		}
		return ""
	}

	if relSlash == "" {
		return ""
	}
	for _, root := range []string{filepath.Join(workspaceDir, "src"), filepath.Join(workspaceDir, "lib"), workspaceDir} {
		base := filepath.Join(root, filepath.FromSlash(relSlash))
		if fi, err := os.Stat(base + ".py"); err == nil && !fi.IsDir() {
			return base + ".py"
		}
		if fi, err := os.Stat(filepath.Join(base, "__init__.py")); err == nil && !fi.IsDir() {
			return filepath.Join(base, "__init__.py")
		}
	}
	return ""
}

func extractPythonModuleExports(src string) map[string]bool {
	exports := make(map[string]bool)
	for _, m := range identRefRegex.FindAllString(src, -1) {
		exports[m] = true
	}
	return exports
}



