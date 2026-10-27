// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// repomap.go implements pure Go AST parsing + polyglot regex extraction,
// bipartite reference graph construction, personalized PageRank with focus/query biasing,
// and token-budget clamped repository outlines (<1024 tokens).

package devops

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// SymbolTag represents an extracted identifier definition or signature.
type SymbolTag struct {
	File      string  `json:"file"`
	Kind      string  `json:"kind"` // "struct", "interface", "func", "method", "class", "type", "const", "var", "trait", "enum"
	Name      string  `json:"name"`
	Signature string  `json:"signature"`
	Line      int     `json:"line"`
	Score     float64 `json:"score"`
}

// RepoMapRequest encapsulates parameters for repository map generation.
type RepoMapRequest struct {
	WorkspaceDir string   `json:"workspace_dir"`
	TokenBudget  int      `json:"token_budget"` // Default: 1024 tokens
	FocusFiles   []string `json:"focus_files,omitempty"`
	Query        string   `json:"query,omitempty"`
	MaxFiles     int      `json:"max_files,omitempty"`
}

// RepoMapResult contains the generated repository map and telemetry.
type RepoMapResult struct {
	WorkspaceDir string      `json:"workspace_dir"`
	RepoMap      string      `json:"repo_map"`
	TokenCount   int         `json:"token_count"`
	FilesScanned int         `json:"files_scanned"`
	SymbolsFound int         `json:"symbols_found"`
	TopSymbols   []SymbolTag `json:"top_symbols,omitempty"`
}

// Polyglot regex patterns for TS, Python, Rust
var (
	// TypeScript/JavaScript symbol patterns
	tsClassReg      = regexp.MustCompile(`^(?:export\s+)?(?:abstract\s+)?class\s+([a-zA-Z0-9_]+)`)
	tsInterfaceReg  = regexp.MustCompile(`^(?:export\s+)?interface\s+([a-zA-Z0-9_]+)`)
	tsTypeReg       = regexp.MustCompile(`^(?:export\s+)?type\s+([a-zA-Z0-9_]+)\s*=`)
	tsFuncReg       = regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+([a-zA-Z0-9_]+)\s*\(([^)]*)\)`)
	tsConstArrowReg = regexp.MustCompile(`^(?:export\s+)?const\s+([a-zA-Z0-9_]+)\s*=\s*(?:async\s*)?\(([^)]*)\)\s*(?::\s*([a-zA-Z0-9_<>[\]]+))?\s*=>`)

	// Python symbol patterns
	pyClassReg = regexp.MustCompile(`^class\s+([a-zA-Z0-9_]+)(?:\(([^)]*)\))?`)
	pyDefReg   = regexp.MustCompile(`^(?:async\s+)?def\s+([a-zA-Z0-9_]+)\s*\(([^)]*)`)

	// Rust symbol patterns
	rsStructReg = regexp.MustCompile(`^(?:pub(?:\([^)]+\))?\s+)?struct\s+([a-zA-Z0-9_]+)`)
	rsEnumReg   = regexp.MustCompile(`^(?:pub(?:\([^)]+\))?\s+)?enum\s+([a-zA-Z0-9_]+)`)
	rsTraitReg  = regexp.MustCompile(`^(?:pub(?:\([^)]+\))?\s+)?trait\s+([a-zA-Z0-9_]+)`)
	rsFnReg     = regexp.MustCompile(`^(?:pub(?:\([^)]+\))?\s+)?(?:async\s+)?fn\s+([a-zA-Z0-9_]+)\s*\(([^)]*)\)(?:\s*->\s*([^{]+))?`)

	// Identifier reference matcher (words >= 3 chars)
	identRefRegex = regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_]{2,}\b`)

	// Fallback Go regex patterns
	goTypeStructReg = regexp.MustCompile(`^type\s+([a-zA-Z0-9_]+)\s+struct`)
	goTypeInterReg  = regexp.MustCompile(`^type\s+([a-zA-Z0-9_]+)\s+interface`)
	goFuncReg       = regexp.MustCompile(`^func\s+([a-zA-Z0-9_]+)\s*\(`)
	goMethodReg     = regexp.MustCompile(`^func\s+\((?:[a-zA-Z0-9_* ]+)\)\s+([a-zA-Z0-9_]+)\s*\(`)
)

var ignoredDirectories = map[string]bool{
	".git":       true,
	"node_modules": true,
	"vendor":     true,
	".icx_cas":   true,
	".icx_space": true,
	"bin":        true,
	"dist":       true,
	".vscode":    true,
	".idea":      true,
	"build":      true,
	"target":     true,
}

// SynthesizeRepoMap discovers code files, constructs the bipartite graph,
// calculates personalized PageRank, and packs the outline within the token budget.
func SynthesizeRepoMap(req RepoMapRequest) (*RepoMapResult, error) {
	if req.WorkspaceDir == "" {
		req.WorkspaceDir = "."
	}
	if req.TokenBudget <= 0 {
		req.TokenBudget = 1024
	}

	filesScanned := 0
	fileSymbols := make(map[string][]SymbolTag)
	fileReferences := make(map[string]map[string]bool)
	symbolDefiners := make(map[string][]string) // symbol -> []files declaring it

	err := filepath.WalkDir(req.WorkspaceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if ignoredDirectories[base] {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs":
			// Process supported file
		default:
			return nil
		}

		rel, relErr := filepath.Rel(req.WorkspaceDir, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		var tags []SymbolTag
		var refs map[string]bool
		info, infoErr := d.Info()

		if infoErr == nil {
			if cachedTags, cachedRefs, ok := globalASTCache.Get(rel, info.ModTime(), info.Size()); ok {
				tags = cachedTags
				refs = cachedRefs
			}
		}

		if tags == nil && refs == nil {
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			content := string(raw)
			tags, refs = extractFileSymbolsAndRefs(rel, content, ext)
			if infoErr == nil {
				globalASTCache.Put(rel, info.ModTime(), info.Size(), tags, refs)
			}
		}

		filesScanned++

		if len(tags) > 0 {
			fileSymbols[rel] = tags
			for _, t := range tags {
				symbolDefiners[t.Name] = append(symbolDefiners[t.Name], rel)
			}
		}
		if len(refs) > 0 {
			fileReferences[rel] = refs
		}

		if req.MaxFiles > 0 && filesScanned >= req.MaxFiles {
			return filepath.SkipAll
		}
		return nil
	})

	if err != nil && err != filepath.SkipAll {
		return nil, fmt.Errorf("WALK_ERROR: %w", err)
	}

	if filesScanned == 0 || len(fileSymbols) == 0 {
		return &RepoMapResult{
			WorkspaceDir: req.WorkspaceDir,
			RepoMap:      "No source files discovered.",
			TokenCount:   1,
			FilesScanned: filesScanned,
			SymbolsFound: 0,
		}, nil
	}

	// List of all files with symbols
	allFiles := make([]string, 0, len(fileSymbols))
	for f := range fileSymbols {
		allFiles = append(allFiles, f)
	}
	sort.Strings(allFiles)

	// Compute Personalized PageRank
	scores := computePersonalizedPageRank(allFiles, fileReferences, symbolDefiners, fileSymbols, req)

	// Score individual symbols: base file score * (1.5 if exported)
	totalSymbols := 0
	var allScoredSymbols []SymbolTag
	for file, tags := range fileSymbols {
		fileScore := scores[file]
		for i := range tags {
			s := tags[i]
			boost := 1.0
			if isExportedSymbol(s.Name) {
				boost = 1.5
			}
			tags[i].Score = fileScore * boost
			allScoredSymbols = append(allScoredSymbols, tags[i])
			totalSymbols++
		}
		fileSymbols[file] = tags
	}

	// Sort symbols globally by score descending
	sort.Slice(allScoredSymbols, func(i, j int) bool {
		if allScoredSymbols[i].Score != allScoredSymbols[j].Score {
			return allScoredSymbols[i].Score > allScoredSymbols[j].Score
		}
		if allScoredSymbols[i].File != allScoredSymbols[j].File {
			return allScoredSymbols[i].File < allScoredSymbols[j].File
		}
		return allScoredSymbols[i].Line < allScoredSymbols[j].Line
	})

	topK := 20
	if len(allScoredSymbols) < topK {
		topK = len(allScoredSymbols)
	}
	topSymbols := make([]SymbolTag, topK)
	copy(topSymbols, allScoredSymbols[:topK])

	// Sort files for outline packing: if Query is specified, files matching the query
	// (by path or by containing matching symbols) are prioritized at the top of the budget.
	sortedFiles := make([]string, len(allFiles))
	copy(sortedFiles, allFiles)
	if req.Query != "" {
		queryLower := strings.ToLower(strings.TrimSpace(req.Query))
		isMatchingFile := func(file string) bool {
			if strings.Contains(strings.ToLower(file), queryLower) {
				return true
			}
			for _, sym := range fileSymbols[file] {
				if strings.Contains(strings.ToLower(sym.Name), queryLower) || strings.Contains(strings.ToLower(sym.Signature), queryLower) {
					return true
				}
			}
			return false
		}
		sort.SliceStable(sortedFiles, func(i, j int) bool {
			matchI := isMatchingFile(sortedFiles[i])
			matchJ := isMatchingFile(sortedFiles[j])
			if matchI != matchJ {
				return matchI // matching files first
			}
			if scores[sortedFiles[i]] != scores[sortedFiles[j]] {
				return scores[sortedFiles[i]] > scores[sortedFiles[j]]
			}
			return sortedFiles[i] < sortedFiles[j]
		})
	} else {
		sort.Slice(sortedFiles, func(i, j int) bool {
			if scores[sortedFiles[i]] != scores[sortedFiles[j]] {
				return scores[sortedFiles[i]] > scores[sortedFiles[j]]
			}
			return sortedFiles[i] < sortedFiles[j]
		})
	}

	// Pack file symbol trees bounded by TokenBudget
	repoMapText := packOutline(sortedFiles, fileSymbols, req.TokenBudget)
	tokenCount := estimateTokens(repoMapText)

	return &RepoMapResult{
		WorkspaceDir: req.WorkspaceDir,
		RepoMap:      repoMapText,
		TokenCount:   tokenCount,
		FilesScanned: filesScanned,
		SymbolsFound: totalSymbols,
		TopSymbols:   topSymbols,
	}, nil
}

// computePersonalizedPageRank implements personalized power iteration (15 iterations, damping 0.85).
func computePersonalizedPageRank(
	files []string,
	fileRefs map[string]map[string]bool,
	symbolDefiners map[string][]string,
	fileSymbols map[string][]SymbolTag,
	req RepoMapRequest,
) map[string]float64 {
	n := len(files)
	scores := make(map[string]float64, n)
	if n == 0 {
		return scores
	}

	fileSet := make(map[string]bool, n)
	for _, f := range files {
		fileSet[f] = true
	}

	// 1. Build Adjacency Matrix (File U -> Target File V)
	adj := make(map[string][]string, n)
	for _, u := range files {
		refs := fileRefs[u]
		targetSet := make(map[string]bool)
		for sym := range refs {
			definers := symbolDefiners[sym]
			for _, v := range definers {
				if v != u && fileSet[v] {
					targetSet[v] = true
				}
			}
		}
		targets := make([]string, 0, len(targetSet))
		for t := range targetSet {
			targets = append(targets, t)
		}
		adj[u] = targets
	}

	// 2. Compute Personalization Vector (pVector)
	focusSet := make(map[string]bool)
	for _, ff := range req.FocusFiles {
		focusSet[filepath.ToSlash(ff)] = true
	}

	queryLower := strings.ToLower(strings.TrimSpace(req.Query))
	biases := make(map[string]float64, n)
	var sumBias float64

	for _, f := range files {
		bias := 1.0 // Base bias
		if focusSet[f] {
			bias += 5.0 // Focus files +5.0
		}
		if queryLower != "" {
			if strings.Contains(strings.ToLower(f), queryLower) {
				bias += 10.0 // File path matching query
			} else {
				for _, sym := range fileSymbols[f] {
					if strings.Contains(strings.ToLower(sym.Name), queryLower) || strings.Contains(strings.ToLower(sym.Signature), queryLower) {
						bias += 8.0 // Symbol matching query
						break
					}
				}
			}
		}
		biases[f] = bias
		sumBias += bias
	}

	pVector := make(map[string]float64, n)
	for _, f := range files {
		pVector[f] = biases[f] / sumBias
	}

	// Initial uniform distribution
	for _, f := range files {
		scores[f] = 1.0 / float64(n)
	}

	const damping = 0.85
	const iterations = 15

	for iter := 0; iter < iterations; iter++ {
		nextScores := make(map[string]float64, n)
		var danglingWeight float64

		for _, u := range files {
			outTargets := adj[u]
			outDegree := len(outTargets)
			if outDegree == 0 {
				danglingWeight += scores[u]
			} else {
				share := (scores[u] * damping) / float64(outDegree)
				for _, v := range outTargets {
					nextScores[v] += share
				}
			}
		}

		for _, u := range files {
			// Teleportation from (1 - damping) + dangling redistribution
			nextScores[u] += (1.0-damping)*pVector[u] + (damping*danglingWeight)*pVector[u]
		}
		scores = nextScores
	}

	return scores
}

func packOutline(sortedFiles []string, fileSymbols map[string][]SymbolTag, budget int) string {
	if budget <= 0 {
		budget = 1024
	}

	var sb strings.Builder
	const clampMarker = "│ (outline clamped to budget)\n"

	clamped := false

	for _, file := range sortedFiles {
		tags := fileSymbols[file]
		if len(tags) == 0 {
			continue
		}

		// Sort symbols in this file by line number
		sort.Slice(tags, func(i, j int) bool {
			return tags[i].Line < tags[j].Line
		})

		header := file + ":\n"

		// If adding file header exceeds budget, stop packing files
		if estimateTokensLen(sb.Len()+len(header)) > budget {
			clamped = true
			break
		}

		var fileBlock strings.Builder
		fileBlock.WriteString(header)

		fileHasSymbols := false
		for _, sym := range tags {
			sig := sym.Signature
			if len(sig) > 90 {
				sig = sig[:87] + "..."
			}
			symLine := "│ " + sig + "\n"

			// Check if adding this symbol exceeds budget
			if estimateTokensLen(sb.Len()+fileBlock.Len()+len(symLine)) > budget {
				clamped = true
				break
			}

			fileBlock.WriteString(symLine)
			fileHasSymbols = true
		}

		// If no symbols fit into this file block, and we already packed previous files, stop
		if !fileHasSymbols && sb.Len() > 0 {
			clamped = true
			break
		}

		// If fileBlock exceeds budget, stop
		if estimateTokensLen(sb.Len()+fileBlock.Len()) > budget {
			clamped = true
			break
		}

		sb.WriteString(fileBlock.String())
		if clamped {
			break
		}
	}

	// If outline was clamped, append clampMarker if it fits within the budget
	if clamped {
		if estimateTokensLen(sb.Len()+len(clampMarker)) <= budget {
			sb.WriteString(clampMarker)
		}
	}

	result := sb.String()
	if result == "" && len(sortedFiles) > 0 {
		file := sortedFiles[0]
		cand := file + ":\n" + clampMarker
		if estimateTokensLen(len(cand)) <= budget {
			return cand
		}
		if estimateTokensLen(len(file)+2) <= budget {
			return file + ":\n"
		}
		// Budget is extremely small: clamp file name to budget
		for len(file) > 0 && estimateTokensLen(len(file)) > budget {
			file = file[:len(file)-1]
		}
		return file
	}

	// Invariant safety clamp: strictly guarantee estimateTokens(result) <= budget
	for estimateTokens(result) > budget && len(result) > 0 {
		lastNL := strings.LastIndex(strings.TrimRight(result, "\n"), "\n")
		if lastNL > 0 {
			result = result[:lastNL+1]
		} else {
			result = result[:len(result)-1]
		}
	}

	return result
}

func estimateTokens(s string) int {
	return estimateTokensLen(len(s))
}

func estimateTokensLen(length int) int {
	if length == 0 {
		return 0
	}
	t := int(float64(length) / 3.5)
	if t < 1 {
		return 1
	}
	return t
}

func isExportedSymbol(name string) bool {
	if len(name) == 0 {
		return false
	}
	first := rune(name[0])
	return unicode.IsUpper(first)
}

// extractFileSymbolsAndRefs extracts definitions and identifier references from a source file.
func extractFileSymbolsAndRefs(relPath, content, ext string) ([]SymbolTag, map[string]bool) {
	if ext == ".go" {
		return extractGoSymbolsAndRefs(relPath, content)
	}
	return extractPolyglotSymbolsAndRefs(relPath, content, ext)
}

// extractGoSymbolsAndRefs parses Go AST using standard go/parser and go/ast.
func extractGoSymbolsAndRefs(relPath, content string) ([]SymbolTag, map[string]bool) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, relPath, content, parser.ParseComments)

	if node == nil {
		// Fallback to regex scanner if parser failed completely
		return extractRegexFallbackGo(relPath, content)
	}

	var tags []SymbolTag
	refs := make(map[string]bool)

	// 1. Inspect all identifiers across the AST to capture usages/references
	ast.Inspect(node, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok {
			if len(ident.Name) >= 3 {
				refs[ident.Name] = true
			}
		}
		return true
	})

	// 2. Extract top-level type, const, var, and func declarations
	for _, decl := range node.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					line := fset.Position(ts.Pos()).Line
					name := ts.Name.Name
					kind := "type"
					sig := fmt.Sprintf("type %s", name)

					switch ts.Type.(type) {
					case *ast.StructType:
						kind = "struct"
						sig = fmt.Sprintf("type %s struct", name)
					case *ast.InterfaceType:
						kind = "interface"
						sig = fmt.Sprintf("type %s interface", name)
					default:
						var typeBuf strings.Builder
						_ = printer.Fprint(&typeBuf, fset, ts.Type)
						typeStr := strings.Join(strings.Fields(typeBuf.String()), " ")
						sig = fmt.Sprintf("type %s %s", name, typeStr)
					}
					tags = append(tags, SymbolTag{
						File:      relPath,
						Kind:      kind,
						Name:      name,
						Signature: sig,
						Line:      line,
					})
				}
			case token.CONST:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					line := fset.Position(vs.Pos()).Line
					for _, name := range vs.Names {
						tags = append(tags, SymbolTag{
							File:      relPath,
							Kind:      "const",
							Name:      name.Name,
							Signature: fmt.Sprintf("const %s", name.Name),
							Line:      line,
						})
					}
				}
			case token.VAR:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					line := fset.Position(vs.Pos()).Line
					for _, name := range vs.Names {
						tags = append(tags, SymbolTag{
							File:      relPath,
							Kind:      "var",
							Name:      name.Name,
							Signature: fmt.Sprintf("var %s", name.Name),
							Line:      line,
						})
					}
				}
			}

		case *ast.FuncDecl:
			line := fset.Position(d.Pos()).Line
			kind, name, sig := formatFuncDecl(fset, d)
			tags = append(tags, SymbolTag{
				File:      relPath,
				Kind:      kind,
				Name:      name,
				Signature: sig,
				Line:      line,
			})
		}
	}

	if len(tags) == 0 && err != nil {
		// If AST produced no tags and had an error, try regex fallback
		return extractRegexFallbackGo(relPath, content)
	}

	return tags, refs
}

func formatFuncDecl(fset *token.FileSet, decl *ast.FuncDecl) (kind, name, sig string) {
	name = decl.Name.Name
	kind = "func"
	var recvStr string
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		kind = "method"
		var recvBuf strings.Builder
		_ = printer.Fprint(&recvBuf, fset, decl.Recv)
		recvStr = recvBuf.String()
	}

	var paramsBuf strings.Builder
	if decl.Type.Params != nil {
		_ = printer.Fprint(&paramsBuf, fset, decl.Type.Params)
	}
	paramsStr := paramsBuf.String()

	var resultsStr string
	if decl.Type.Results != nil {
		var resBuf strings.Builder
		_ = printer.Fprint(&resBuf, fset, decl.Type.Results)
		resultsStr = " " + resBuf.String()
	}

	if recvStr != "" {
		sig = fmt.Sprintf("func %s %s%s%s", recvStr, name, paramsStr, resultsStr)
	} else {
		sig = fmt.Sprintf("func %s%s%s", name, paramsStr, resultsStr)
	}
	sig = strings.Join(strings.Fields(sig), " ")
	return kind, name, sig
}

func extractRegexFallbackGo(relPath, content string) ([]SymbolTag, map[string]bool) {
	var tags []SymbolTag
	refs := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		for _, match := range identRefRegex.FindAllString(line, -1) {
			refs[match] = true
		}

		if m := goTypeStructReg.FindStringSubmatch(line); len(m) > 1 {
			tags = append(tags, SymbolTag{
				File:      relPath,
				Kind:      "struct",
				Name:      m[1],
				Signature: fmt.Sprintf("type %s struct", m[1]),
				Line:      lineNum,
			})
		} else if m := goTypeInterReg.FindStringSubmatch(line); len(m) > 1 {
			tags = append(tags, SymbolTag{
				File:      relPath,
				Kind:      "interface",
				Name:      m[1],
				Signature: fmt.Sprintf("type %s interface", m[1]),
				Line:      lineNum,
			})
		} else if m := goMethodReg.FindStringSubmatch(line); len(m) > 1 {
			tags = append(tags, SymbolTag{
				File:      relPath,
				Kind:      "method",
				Name:      m[1],
				Signature: line,
				Line:      lineNum,
			})
		} else if m := goFuncReg.FindStringSubmatch(line); len(m) > 1 {
			tags = append(tags, SymbolTag{
				File:      relPath,
				Kind:      "func",
				Name:      m[1],
				Signature: line,
				Line:      lineNum,
			})
		}
	}
	return tags, refs
}

// extractPolyglotSymbolsAndRefs uses specialized regexes for TypeScript, Python, and Rust.
func extractPolyglotSymbolsAndRefs(relPath, content, ext string) ([]SymbolTag, map[string]bool) {
	var tags []SymbolTag
	refs := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		for _, match := range identRefRegex.FindAllString(line, -1) {
			refs[match] = true
		}

		switch ext {
		case ".ts", ".tsx", ".js", ".jsx":
			if m := tsClassReg.FindStringSubmatch(line); len(m) > 1 {
				tags = append(tags, SymbolTag{File: relPath, Kind: "class", Name: m[1], Signature: fmt.Sprintf("class %s", m[1]), Line: lineNum})
			} else if m := tsInterfaceReg.FindStringSubmatch(line); len(m) > 1 {
				tags = append(tags, SymbolTag{File: relPath, Kind: "interface", Name: m[1], Signature: fmt.Sprintf("interface %s", m[1]), Line: lineNum})
			} else if m := tsTypeReg.FindStringSubmatch(line); len(m) > 1 {
				tags = append(tags, SymbolTag{File: relPath, Kind: "type", Name: m[1], Signature: fmt.Sprintf("type %s", m[1]), Line: lineNum})
			} else if m := tsFuncReg.FindStringSubmatch(line); len(m) > 1 {
				tags = append(tags, SymbolTag{File: relPath, Kind: "func", Name: m[1], Signature: fmt.Sprintf("function %s(%s)", m[1], m[2]), Line: lineNum})
			} else if m := tsConstArrowReg.FindStringSubmatch(line); len(m) > 1 {
				tags = append(tags, SymbolTag{File: relPath, Kind: "func", Name: m[1], Signature: fmt.Sprintf("const %s = (%s) =>", m[1], m[2]), Line: lineNum})
			}

		case ".py":
			if m := pyClassReg.FindStringSubmatch(line); len(m) > 1 {
				sig := fmt.Sprintf("class %s", m[1])
				if m[2] != "" {
					sig += fmt.Sprintf("(%s)", m[2])
				}
				tags = append(tags, SymbolTag{File: relPath, Kind: "class", Name: m[1], Signature: sig, Line: lineNum})
			} else if m := pyDefReg.FindStringSubmatch(line); len(m) > 1 {
				sig := fmt.Sprintf("def %s(%s)", m[1], m[2])
				if len(m) > 3 && m[3] != "" {
					sig += fmt.Sprintf(" -> %s", m[3])
				}
				tags = append(tags, SymbolTag{File: relPath, Kind: "func", Name: m[1], Signature: sig, Line: lineNum})
			}

		case ".rs":
			if m := rsStructReg.FindStringSubmatch(line); len(m) > 1 {
				tags = append(tags, SymbolTag{File: relPath, Kind: "struct", Name: m[1], Signature: fmt.Sprintf("struct %s", m[1]), Line: lineNum})
			} else if m := rsEnumReg.FindStringSubmatch(line); len(m) > 1 {
				tags = append(tags, SymbolTag{File: relPath, Kind: "enum", Name: m[1], Signature: fmt.Sprintf("enum %s", m[1]), Line: lineNum})
			} else if m := rsTraitReg.FindStringSubmatch(line); len(m) > 1 {
				tags = append(tags, SymbolTag{File: relPath, Kind: "trait", Name: m[1], Signature: fmt.Sprintf("trait %s", m[1]), Line: lineNum})
			} else if m := rsFnReg.FindStringSubmatch(line); len(m) > 1 {
				sig := fmt.Sprintf("fn %s(%s)", m[1], m[2])
				if len(m) > 3 && m[3] != "" {
					sig += fmt.Sprintf(" -> %s", m[3])
				}
				tags = append(tags, SymbolTag{File: relPath, Kind: "func", Name: m[1], Signature: sig, Line: lineNum})
			}
		}
	}

	return tags, refs
}
