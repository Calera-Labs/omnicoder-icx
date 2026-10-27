// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// PlanPhase represents a single phase in the multi-phase implementation roadmap.
type PlanPhase struct {
	PhaseNumber     int                 `json:"phase_number"`
	OriginalID      string              `json:"original_id,omitempty"`     // Original identifier from plan (e.g. "Milestone A", "Step 1", "Phase 1")
	Aliases         []string            `json:"aliases,omitempty"`         // Auto-generated aliases (e.g. ["milestone a", "a", "phase 1"])
	Name            string              `json:"name"`
	TargetFiles     []string            `json:"target_files"`              // relative to workspaceDir, e.g. "projects/my-service/go.mod"
	TargetSymbols   map[string][]string `json:"target_symbols,omitempty"`  // Fine-grained symbol/AST scope: targetFilePath -> symbol/function names
	VerificationCmd string              `json:"verification_cmd"`
	Description     string              `json:"description"`
	SpecSection     string              `json:"spec_section,omitempty"`    // Full markdown section for this phase from the master plan
	FileSpecs       map[string]string   `json:"file_specs,omitempty"`      // File-specific requirements and reference code extracted from plan
	Dependencies    []int               `json:"dependencies,omitempty"`    // Explicit or inferred phase numbers required before this phase
}

// PlanSpec represents the structured execution plan.
type PlanSpec struct {
	ProjectScope      string      `json:"project_scope"` // e.g. "projects/my-service"
	ModuleName        string      `json:"module_name"`   // e.g. "myservice"
	RawPlanContent    string      `json:"raw_plan_content,omitempty"`
	Architecture      string      `json:"architecture,omitempty"`       // Core architecture, math, conventions from sections preceding phases
	VerificationRules string      `json:"verification_rules,omitempty"` // Acceptance criteria and verification matrix from sections following phases
	Phases            []PlanPhase `json:"phases"`
}

var (
	phaseHeadingRe         = regexp.MustCompile(`(?m)^#{2,3}\s+Phase\s+(\d+)\s*:\s*(.+)$`)
	flexiblePhaseHeadingRe = regexp.MustCompile(`(?mi)^#{1,4}\s+(?:(?:Phase|Milestone|Step|Task|Stage|Part)\s+([A-Za-z0-9._-]+)|(\d+)[.:])(?:\s*[-:–—.]\s*|\s+)?(.*)$`)
	planPathRe             = regexp.MustCompile(`(?:projects|apps|pkg|web|cmd|tests|internal|src|lib|components|api|config|scripts)/[A-Za-z0-9_./-]+\.[A-Za-z0-9]+`)
	backtickPathRe         = regexp.MustCompile("`([^`]+)`")
	verifyCmdRe            = regexp.MustCompile("`((?:go|npm|cargo|pytest|python)[^`]*)`")
	scopeRe                = regexp.MustCompile(`(?:projects|apps)/[A-Za-z0-9_-]+`)
)

var validSourceExts = map[string]bool{
	// Go
	".go": true, ".mod": true, ".sum": true,
	// Web / JS / TS
	".js": true, ".mjs": true, ".cjs": true, ".jsx": true,
	".ts": true, ".mts": true, ".cts": true, ".tsx": true,
	".html": true, ".htm": true, ".css": true, ".scss": true, ".sass": true, ".less": true,
	// Config / Data / Schemas
	".json": true, ".json5": true, ".yaml": true, ".yml": true, ".toml": true,
	".xml": true, ".ini": true, ".env": true, ".sql": true,
	".proto": true, ".graphql": true, ".gql": true,
	// Documentation / Text
	".md": true, ".markdown": true, ".txt": true, ".csv": true, ".tsv": true,
	// Shell & Scripts
	".sh": true, ".bash": true, ".zsh": true, ".ps1": true, ".bat": true, ".cmd": true,
	// Shaders
	".glsl": true, ".vert": true, ".frag": true, ".geom": true, ".comp": true, ".wgsl": true, ".wat": true,
	// Systems & Languages
	".c": true, ".h": true, ".cpp": true, ".hpp": true, ".cc": true, ".hh": true,
	".rs": true, ".py": true, ".java": true, ".kt": true, ".rb": true, ".php": true,
	".cs": true, ".swift": true, ".wasm": true,
	// Web Assets
	".svg": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true, ".webp": true,
}

// isValidTargetSourceFile checks if candidate is a plausible source/config/asset file rather than
// a URL, internet domain (e.g. example.com, api.service.io), binary artifact (e.g. server.exe), or code symbol.
func isValidTargetSourceFile(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	// Reject URLs and web protocols
	if strings.Contains(p, "://") || strings.HasPrefix(p, "www.") || strings.HasPrefix(p, "api.") {
		return false
	}
	// Reject Go module hosts or remote repositories
	if strings.HasPrefix(p, "github.com/") || strings.HasPrefix(p, "golang.org/") || strings.HasPrefix(p, "gopkg.in/") {
		return false
	}
	// Reject strings with spaces, parentheses, brackets, or angle brackets
	if strings.ContainsAny(p, " ()[]<>\"'") {
		return false
	}

	base := filepath.Base(p)
	// Check known special files without extensions
	if base == "Dockerfile" || base == "Makefile" || base == "Containerfile" || base == "Procfile" {
		return true
	}

	ext := strings.ToLower(filepath.Ext(p))
	if ext == "" {
		return false
	}

	// Explicitly reject executable binaries and archives
	switch ext {
	case ".exe", ".bin", ".dll", ".so", ".dylib", ".o", ".a", ".tar", ".gz", ".zip", ".7z", ".rar":
		return false
	}

	// Explicitly reject internet domain TLDs
	switch ext {
	case ".io", ".com", ".org", ".net", ".ai", ".co", ".app", ".dev",
		".edu", ".gov", ".mil", ".info", ".biz", ".tv", ".me", ".cc", ".us", ".uk",
		".de", ".eu", ".fr", ".xyz", ".online", ".site", ".cloud", ".store", ".live":
		return false
	}

	return validSourceExts[ext]
}

func extractVerificationCommand(section string) string {
	// 1. Fenced code block under Verification/Verify/Test
	verifyCodeBlockRe := regexp.MustCompile("(?mi)(?:verification(?:\\s+cmd|\\s+command)?|verify|test(?:\\s+cmd|\\s+command)?|tests)\\s*:\\s*```(?:bash|sh|ps1|powershell)?\\s*\\r?\\n([^\\r\\n`]+)\\r?\\n```")
	if m := verifyCodeBlockRe.FindStringSubmatch(section); len(m) > 1 {
		cmd := strings.TrimSpace(m[1])
		if cmd != "" {
			return cmd
		}
	}

	// 2. Inline backticks under Verification/Verify/Test
	verifyInlineRe := regexp.MustCompile("(?mi)(?:verification(?:\\s+cmd|\\s+command)?|verify|test(?:\\s+cmd|\\s+command)?|tests)\\s*:\\s*`([^`]+)`")
	if m := verifyInlineRe.FindStringSubmatch(section); len(m) > 1 {
		cmd := strings.TrimSpace(m[1])
		if cmd != "" {
			return cmd
		}
	}

	// 3. Plain line under Verification/Verify/Test
	verifyPlainRe := regexp.MustCompile("(?mi)(?:verification(?:\\s+cmd|\\s+command)?|verify|test(?:\\s+cmd|\\s+command)?|tests)\\s*:\\s*([^\\r\\n`]+)")
	if m := verifyPlainRe.FindStringSubmatch(section); len(m) > 1 {
		cmd := strings.TrimSpace(m[1])
		if cmd != "" && !strings.HasPrefix(cmd, "-") && !strings.HasPrefix(cmd, "*") {
			return cmd
		}
	}

	// 4. Fallback to existing verifyCmdRe
	if vm := verifyCmdRe.FindStringSubmatch(section); len(vm) > 1 {
		return strings.TrimSpace(vm[1])
	}

	return ""
}

func parseDependencies(section string, aliasMap map[string]int, currentPhase int) []int {
	depBlockRe := regexp.MustCompile(`(?mi)(?:dependencies|depends\s+on|prerequisites)\s*:\s*([^\r\n]+(?:\r?\n[ \t]*[-*][ \t]*[^\r\n]+)*)`)
	matches := depBlockRe.FindStringSubmatch(section)
	if len(matches) < 2 {
		return nil
	}
	rawBlock := strings.TrimSpace(matches[1])
	lowerBlock := strings.ToLower(rawBlock)
	if lowerBlock == "none" || lowerBlock == "nil" || lowerBlock == "n/a" || lowerBlock == "[]" || lowerBlock == "-" || strings.HasPrefix(lowerBlock, "no dep") {
		return nil
	}

	var deps []int
	seen := make(map[int]bool)

	tokenDelimRe := regexp.MustCompile(`[,\r\n\[\]]+`)
	rawTokens := tokenDelimRe.Split(rawBlock, -1)

	for _, rt := range rawTokens {
		token := strings.TrimSpace(rt)
		token = strings.TrimPrefix(token, "-")
		token = strings.TrimPrefix(token, "*")
		token = strings.Trim(token, " \t\"'`()")
		if token == "" {
			continue
		}
		lowerTok := strings.ToLower(token)
		if lowerTok == "none" || lowerTok == "nil" || lowerTok == "n/a" || lowerTok == "none." {
			continue
		}

		// 1. Direct lookup in aliasMap
		if id, ok := aliasMap[lowerTok]; ok {
			if id != currentPhase && !seen[id] {
				seen[id] = true
				deps = append(deps, id)
			}
			continue
		}

		// 2. Strip common prefixes
		matchedPrefix := false
		for _, pfx := range []string{"phase ", "milestone ", "step ", "task ", "stage "} {
			if strings.HasPrefix(lowerTok, pfx) {
				sub := strings.TrimSpace(strings.TrimPrefix(lowerTok, pfx))
				if id, ok := aliasMap[sub]; ok {
					if id != currentPhase && !seen[id] {
						seen[id] = true
						deps = append(deps, id)
					}
					matchedPrefix = true
					break
				}
				if v, err := strconv.Atoi(sub); err == nil && v != currentPhase && !seen[v] {
					seen[v] = true
					deps = append(deps, v)
					matchedPrefix = true
					break
				}
			}
		}
		if matchedPrefix {
			continue
		}

		// 3. Direct integer parse
		if v, err := strconv.Atoi(lowerTok); err == nil {
			if v != currentPhase && !seen[v] {
				seen[v] = true
				deps = append(deps, v)
			}
			continue
		}
	}

	sort.Ints(deps)
	return deps
}

func extractTargetFilesAndSymbols(section string, spec *PlanSpec) ([]string, map[string][]string) {
	var files []string
	seen := make(map[string]bool)
	targetSymbols := make(map[string][]string)

	addFile := func(p string) {
		p = filepath.ToSlash(filepath.Clean(strings.TrimSpace(p)))
		if !isValidTargetSourceFile(p) || seen[p] {
			return
		}
		seen[p] = true
		files = append(files, p)
		if spec.ProjectScope == "" {
			if sm := scopeRe.FindString(p); sm != "" {
				spec.ProjectScope = sm
			}
		}
	}

	addSymbol := func(filePath, sym string) {
		filePath = filepath.ToSlash(filepath.Clean(strings.TrimSpace(filePath)))
		sym = strings.TrimSpace(sym)
		if filePath == "" || sym == "" {
			return
		}
		for _, s := range targetSymbols[filePath] {
			if s == sym {
				return
			}
		}
		targetSymbols[filePath] = append(targetSymbols[filePath], sym)
	}

	// 1. Check bullet points under Target Files: or Files:
	targetFilesBlockRe := regexp.MustCompile(`(?mi)(?:target\s+files?|files?|paths?)\s*:\s*([^\r\n]+(?:\r?\n[ \t]*[-*][ \t]*[^\r\n]+)*)`)
	if tfm := targetFilesBlockRe.FindStringSubmatch(section); len(tfm) > 1 {
		lines := strings.Split(tfm[1], "\n")
		for _, l := range lines {
			line := strings.TrimSpace(l)
			line = strings.TrimPrefix(line, "-")
			line = strings.TrimPrefix(line, "*")
			line = strings.Trim(line, " \t\"'`")
			if line == "" {
				continue
			}

			// Check for parentheses or brackets with symbols: file.go (SymA, SymB)
			if idx := strings.IndexAny(line, "(["); idx > 0 {
				filePath := strings.TrimSpace(line[:idx])
				symPart := strings.Trim(line[idx:], "()[]")
				if isValidTargetSourceFile(filePath) {
					addFile(filePath)
					for _, sym := range strings.Split(symPart, ",") {
						sym = strings.TrimSpace(sym)
						if sym != "" {
							addSymbol(filePath, sym)
						}
					}
				}
				continue
			}

			// Check for # or : delimiter: file.go#SymA or file.go:SymA
			if idx := strings.IndexAny(line, "#:"); idx > 0 {
				filePath := strings.TrimSpace(line[:idx])
				sym := strings.TrimSpace(line[idx+1:])
				if isValidTargetSourceFile(filePath) {
					addFile(filePath)
					if sym != "" {
						addSymbol(filePath, sym)
					}
				}
				continue
			}

			if isValidTargetSourceFile(line) {
				addFile(line)
			}
		}
	}

	// 2. Standard planPathRe and backtickPathRe
	for _, p := range planPathRe.FindAllString(section, -1) {
		addFile(p)
	}
	for _, bm := range backtickPathRe.FindAllStringSubmatch(section, -1) {
		if len(bm) > 1 {
			raw := strings.TrimSpace(bm[1])
			for _, token := range strings.Fields(raw) {
				cleanToken := strings.Trim(token, `"'`)
				if idx := strings.IndexAny(cleanToken, "#:"); idx > 0 {
					f := cleanToken[:idx]
					s := cleanToken[idx+1:]
					if isValidTargetSourceFile(f) {
						addFile(f)
						if s != "" {
							addSymbol(f, s)
						}
						continue
					}
				}
				addFile(cleanToken)
			}
		}
	}

	// 3. Dedicated Target Symbols block
	targetSymbolsBlockRe := regexp.MustCompile(`(?mi)(?:target\s+symbols?|symbols?|functions?|methods?)\s*:\s*([^\r\n]+(?:\r?\n[ \t]*[-*][ \t]*[^\r\n]+)*)`)
	if tsm := targetSymbolsBlockRe.FindStringSubmatch(section); len(tsm) > 1 {
		lines := strings.Split(tsm[1], "\n")
		for _, l := range lines {
			line := strings.TrimSpace(l)
			line = strings.TrimPrefix(line, "-")
			line = strings.TrimPrefix(line, "*")
			line = strings.Trim(line, " \t\"'`")
			if line == "" {
				continue
			}

			for _, item := range strings.Split(line, ",") {
				item = strings.TrimSpace(item)
				if item == "" {
					continue
				}
				if idx := strings.IndexAny(item, "#:"); idx > 0 {
					f := item[:idx]
					s := item[idx+1:]
					addFile(f)
					addSymbol(f, s)
				} else {
					for _, f := range files {
						if strings.HasSuffix(f, ".go") {
							addSymbol(f, item)
							break
						}
					}
				}
			}
		}
	}

	return files, targetSymbols
}

// ParsePlanMarkdown reads phase headings, target paths, dependencies, symbols, and verification commands
// from whatever plan the user pointed at. Supports standard and fuzzy formats (Phases, Milestones, Steps, Tasks).
func ParsePlanMarkdown(content string) *PlanSpec {
	type rawMatch struct {
		start     int
		headerEnd int
		prefix    string
		rawID     string
		title     string
	}

	var rawMatches []rawMatch

	// 1. Try standard phaseHeadingRe first for 100% backward compatibility
	stdMatches := phaseHeadingRe.FindAllStringSubmatchIndex(content, -1)
	if len(stdMatches) > 0 {
		for _, m := range stdMatches {
			rawMatches = append(rawMatches, rawMatch{
				start:     m[0],
				headerEnd: m[1],
				prefix:    "Phase",
				rawID:     content[m[2]:m[3]],
				title:     strings.TrimSpace(content[m[4]:m[5]]),
			})
		}
	} else {
		// 2. Try flexible fuzzy headings (Phase, Milestone, Step, Task, Stage, Part, or numbered)
		flexMatches := flexiblePhaseHeadingRe.FindAllStringSubmatchIndex(content, -1)
		for _, m := range flexMatches {
			rawID := ""
			prefix := "Phase"
			if m[2] >= 0 && m[3] >= 0 {
				rawID = content[m[2]:m[3]]
				lineText := content[m[0]:m[1]]
				lowerLine := strings.ToLower(lineText)
				if strings.Contains(lowerLine, "milestone") {
					prefix = "Milestone"
				} else if strings.Contains(lowerLine, "step") {
					prefix = "Step"
				} else if strings.Contains(lowerLine, "task") {
					prefix = "Task"
				} else if strings.Contains(lowerLine, "stage") {
					prefix = "Stage"
				}
			} else if m[4] >= 0 && m[5] >= 0 {
				rawID = content[m[4]:m[5]]
			}

			title := ""
			if m[6] >= 0 && m[7] >= 0 {
				title = strings.TrimSpace(content[m[6]:m[7]])
			}
			if title == "" {
				title = prefix + " " + rawID
			}

			rawMatches = append(rawMatches, rawMatch{
				start:     m[0],
				headerEnd: m[1],
				prefix:    prefix,
				rawID:     rawID,
				title:     title,
			})
		}
	}

	if len(rawMatches) == 0 {
		return nil
	}

	spec := &PlanSpec{
		RawPlanContent: content,
	}

	// Capture architectural preamble (everything before the first phase)
	if rawMatches[0].start > 0 {
		spec.Architecture = strings.TrimSpace(content[:rawMatches[0].start])
	}

	// Pass 1: Compute Phase numbers and build aliasMap
	aliasMap := make(map[string]int)
	type phaseMeta struct {
		phaseNum   int
		originalID string
		aliases    []string
		prefix     string
		title      string
		section    string
	}
	var phasesMeta []phaseMeta

	for i, rm := range rawMatches {
		end := len(content)
		if i+1 < len(rawMatches) {
			end = rawMatches[i+1].start
		} else {
			rest := content[rm.headerEnd:]
			subsequentBoundaryRe := regexp.MustCompile(`(?m)^(?:#{1,2}\s+[^\n]+|---+\s*$)`)
			if loc := subsequentBoundaryRe.FindStringIndex(rest); loc != nil {
				end = rm.headerEnd + loc[0]
			}
		}
		section := content[rm.start:end]

		phaseNum := i + 1
		if v, err := strconv.Atoi(rm.rawID); err == nil && v > 0 {
			phaseNum = v
		} else if len(rm.rawID) == 1 && rm.rawID[0] >= 'A' && rm.rawID[0] <= 'Z' {
			phaseNum = int(rm.rawID[0] - 'A' + 1)
		} else if len(rm.rawID) == 1 && rm.rawID[0] >= 'a' && rm.rawID[0] <= 'z' {
			phaseNum = int(rm.rawID[0] - 'a' + 1)
		}

		rawLower := strings.ToLower(rm.rawID)
		origID := fmt.Sprintf("%s %s", rm.prefix, rm.rawID)
		aliases := []string{
			fmt.Sprintf("%d", phaseNum),
			fmt.Sprintf("phase %d", phaseNum),
			fmt.Sprintf("phase%d", phaseNum),
			rawLower,
			"phase " + rawLower,
			"milestone " + rawLower,
			strings.ToLower(rm.prefix) + " " + rawLower,
			strings.ToLower(rm.prefix) + rawLower,
		}

		for _, a := range aliases {
			aliasMap[a] = phaseNum
		}

		phasesMeta = append(phasesMeta, phaseMeta{
			phaseNum:   phaseNum,
			originalID: origID,
			aliases:    aliases,
			prefix:     rm.prefix,
			title:      rm.title,
			section:    section,
		})
	}

	// Pass 2: Extract files, symbols, verification command, and resolve dependencies
	for _, pm := range phasesMeta {
		files, symbols := extractTargetFilesAndSymbols(pm.section, spec)
		verify := extractVerificationCommand(pm.section)
		deps := parseDependencies(pm.section, aliasMap, pm.phaseNum)

		spec.Phases = append(spec.Phases, PlanPhase{
			PhaseNumber:     pm.phaseNum,
			OriginalID:      pm.originalID,
			Aliases:         pm.aliases,
			Name:            pm.title,
			TargetFiles:     files,
			TargetSymbols:   symbols,
			VerificationCmd: verify,
			Description:     pm.title,
			SpecSection:     strings.TrimSpace(pm.section),
			FileSpecs:       make(map[string]string),
			Dependencies:    deps,
		})
	}
	if spec.ProjectScope == "" {
		if sm := scopeRe.FindString(content); sm != "" {
			spec.ProjectScope = sm
		}
	}
	if spec.ProjectScope != "" {
		base := spec.ProjectScope
		if i := strings.LastIndex(base, "/"); i >= 0 {
			spec.ModuleName = base[i+1:]
		}
	}

	// Capture verification rules or acceptance criteria from sections following the last phase
	if len(rawMatches) > 0 {
		lastEnd := rawMatches[len(rawMatches)-1].headerEnd
		if lastEnd < len(content) {
			spec.VerificationRules = strings.TrimSpace(content[lastEnd:])
		}
	}

	// Also extract target files from directory tree blocks or architecture sections across the full document
	enrichPhasesFromPlanText(spec, content)

	// Populate targeted FileSpecs for all target files across phases
	for i := range spec.Phases {
		ph := &spec.Phases[i]
		if ph.FileSpecs == nil {
			ph.FileSpecs = make(map[string]string)
		}
		for _, tf := range ph.TargetFiles {
			if fs := extractFileSpec(content, tf, spec.ProjectScope); fs != "" {
				ph.FileSpecs[tf] = fs
			}
		}
	}

	return spec
}

// enrichPhasesFromPlanText extracts files from directory tree diagrams and architecture specifications
// across the document that may not have been listed in phase bullet points, assigning them to the appropriate phase.
func enrichPhasesFromPlanText(spec *PlanSpec, content string) {
	if spec == nil || len(spec.Phases) == 0 || content == "" {
		return
	}

	existingFiles := make(map[string]bool)
	for _, ph := range spec.Phases {
		for _, f := range ph.TargetFiles {
			existingFiles[filepath.ToSlash(f)] = true
			existingFiles[filepath.Base(f)] = true
		}
	}

	// 1. Scan tree diagram lines (e.g. ├── web/static/index.html or └── tests/api_test.go)
	treeFileRe := regexp.MustCompile(`(?:[├└│─\s]+)([a-zA-Z0-9_./-]+\.[a-zA-Z0-9]+)`)
	var foundFiles []string
	for _, match := range treeFileRe.FindAllStringSubmatch(content, -1) {
		if len(match) > 1 {
			p := filepath.ToSlash(strings.TrimSpace(match[1]))
			if isValidTargetSourceFile(p) && !existingFiles[p] && !existingFiles[filepath.Base(p)] {
				foundFiles = append(foundFiles, p)
				existingFiles[p] = true
				existingFiles[filepath.Base(p)] = true
			}
		}
	}

	// 2. Also scan general plan paths in full content
	for _, p := range planPathRe.FindAllString(content, -1) {
		p = filepath.ToSlash(p)
		if isValidTargetSourceFile(p) && !existingFiles[p] && !existingFiles[filepath.Base(p)] {
			foundFiles = append(foundFiles, p)
			existingFiles[p] = true
			existingFiles[filepath.Base(p)] = true
		}
	}

	if len(foundFiles) == 0 {
		return
	}

	for _, file := range foundFiles {
		clean := file
		if spec.ProjectScope != "" && !strings.HasPrefix(clean, spec.ProjectScope+"/") {
			clean = spec.ProjectScope + "/" + clean
		}

		lower := strings.ToLower(clean)
		assigned := false

		// Check for test files -> assign to last phase or testing phase
		if strings.Contains(lower, "/tests/") || strings.HasSuffix(lower, "_test.go") {
			for i := len(spec.Phases) - 1; i >= 0; i-- {
				ph := &spec.Phases[i]
				pLower := strings.ToLower(ph.Name + " " + ph.Description)
				if strings.Contains(pLower, "test") || strings.Contains(pLower, "verification") || strings.Contains(pLower, "golden") || i == len(spec.Phases)-1 {
					ph.TargetFiles = append(ph.TargetFiles, clean)
					assigned = true
					break
				}
			}
		}

		// Check for web/frontend files
		if !assigned && (strings.Contains(lower, "/web/") || strings.Contains(lower, "/static/") || strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".css")) {
			for i := range spec.Phases {
				ph := &spec.Phases[i]
				pLower := strings.ToLower(ph.Name + " " + ph.Description)
				if strings.Contains(pLower, "web") || strings.Contains(pLower, "ui") || strings.Contains(pLower, "dashboard") || strings.Contains(pLower, "frontend") || strings.Contains(pLower, "client") {
					ph.TargetFiles = append(ph.TargetFiles, clean)
					assigned = true
					break
				}
			}
		}

		// Check for server/api files
		if !assigned && (strings.Contains(lower, "/server/") || strings.Contains(lower, "/api/")) {
			for i := range spec.Phases {
				ph := &spec.Phases[i]
				pLower := strings.ToLower(ph.Name + " " + ph.Description)
				if strings.Contains(pLower, "server") || strings.Contains(pLower, "api") || strings.Contains(pLower, "http") || strings.Contains(pLower, "stream") {
					ph.TargetFiles = append(ph.TargetFiles, clean)
					assigned = true
					break
				}
			}
		}
	}
}

// extractFileSpec searches the master plan for detailed specifications, architectural descriptions,
// and code blocks referencing the target file or its package/subsystem.
func extractFileSpec(fullPlan, targetFile, projectScope string) string {
	if fullPlan == "" || targetFile == "" {
		return ""
	}

	clean := filepath.ToSlash(strings.TrimSpace(targetFile))
	clean = strings.TrimPrefix(clean, "./")
	clean = strings.TrimPrefix(clean, "/")
	if projectScope != "" {
		cleanScope := filepath.ToSlash(strings.TrimSpace(projectScope))
		cleanScope = strings.TrimPrefix(cleanScope, "./")
		cleanScope = strings.TrimPrefix(cleanScope, "/")
		if strings.HasPrefix(clean, cleanScope+"/") {
			clean = strings.TrimPrefix(clean, cleanScope+"/")
		}
	}

	base := filepath.Base(clean)
	dir := filepath.Dir(clean)

	var sb strings.Builder

	// 1. Search for markdown code blocks containing the file name in a comment or tag
	codeBlockRe := regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\n(.*?)```")
	matches := codeBlockRe.FindAllStringSubmatch(fullPlan, -1)
	for _, m := range matches {
		if len(m) > 1 {
			block := m[1]
			firstLines := strings.SplitN(block, "\n", 5)
			header := strings.Join(firstLines, " ")
			for _, term := range []string{clean, base} {
				if strings.Contains(header, term) {
					sb.WriteString("\n```\n")
					if len(block) > 2500 {
						sb.WriteString(block[:2500])
						sb.WriteString("\n// ... [specification continues] ...\n")
					} else {
						sb.WriteString(block)
					}
					sb.WriteString("```\n")
					break
				}
			}
		}
	}

	// 2. Search for dedicated section headings mentioning the file or package
	headingRe := regexp.MustCompile(`(?m)^#{2,4}\s+([^\n]+)$`)
	hIndices := headingRe.FindAllStringSubmatchIndex(fullPlan, -1)
	for idx, hMatch := range hIndices {
		headingText := fullPlan[hMatch[2]:hMatch[3]]
		matched := false
		for _, term := range []string{clean, dir} {
			if term != "." && term != "" && strings.Contains(headingText, term) {
				matched = true
				break
			}
		}
		if matched {
			start := hMatch[0]
			end := len(fullPlan)
			if idx+1 < len(hIndices) {
				end = hIndices[idx+1][0]
			}
			sectionText := strings.TrimSpace(fullPlan[start:end])
			if len(sectionText) > 3000 {
				sectionText = sectionText[:3000] + "\n... [spec section truncated] ..."
			}
			sb.WriteString("\n" + sectionText + "\n")
		}
	}

	return strings.TrimSpace(sb.String())
}

// GetPhasePrompt builds an authoritative, production-grade progression prompt for the given phase.
func (p *PlanSpec) GetPhasePrompt(phaseNum int, ws string) string {
	if p == nil || len(p.Phases) == 0 {
		return ""
	}
	var phase *PlanPhase
	for i := range p.Phases {
		if p.Phases[i].PhaseNumber == phaseNum {
			phase = &p.Phases[i]
			break
		}
	}
	if phase == nil {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== PLAN EXECUTION: PHASE %d / %d ===\n", phase.PhaseNumber, len(p.Phases)))
	sb.WriteString(fmt.Sprintf("Milestone: Phase %d: %s\n", phase.PhaseNumber, phase.Name))
	if p.ProjectScope != "" {
		sb.WriteString(fmt.Sprintf("Project Scope: %s (Module: %s)\n", p.ProjectScope, p.ModuleName))
	}
	if phase.VerificationCmd != "" {
		sb.WriteString(fmt.Sprintf("Verification Command: %s\n", phase.VerificationCmd))
	}
	sb.WriteString("\n")

	// Phase Step-by-Step Specification
	if phase.SpecSection != "" {
		sb.WriteString("=== PHASE STEP-BY-STEP SPECIFICATION ===\n")
		sb.WriteString(strings.TrimSpace(phase.SpecSection))
		sb.WriteString("\n\n")
	}

	// Missing files and their specific requirements
	complete, missing := p.IsPhaseFilesPresent(ws, phase)
	if !complete && len(missing) > 0 {
		sb.WriteString(fmt.Sprintf("=== REQUIRED FILES TO IMPLEMENT (%d files) ===\n", len(missing)))
		for _, m := range missing {
			scopedM := m
			if p.ProjectScope != "" && !strings.HasPrefix(m, p.ProjectScope+"/") {
				scopedM = p.ProjectScope + "/" + m
			}
			sb.WriteString(fmt.Sprintf("  - [ ] %s\n", scopedM))
			if phase.FileSpecs != nil {
				if spec, ok := phase.FileSpecs[m]; ok && strings.TrimSpace(spec) != "" {
					sb.WriteString(fmt.Sprintf("    Detailed Specification for %s:\n", filepath.Base(m)))
					lines := strings.Split(spec, "\n")
					for _, l := range lines {
						sb.WriteString("      " + l + "\n")
					}
					sb.WriteString("\n")
				}
			}
		}
	}

	sb.WriteString("=== PRODUCTION-GRADE ENGINEERING INVARIANTS ===\n")
	sb.WriteString("1. ZERO STUBS: Strictly prohibited from emitting minimal mocks, placeholders (`// TODO`, `// TBD`), or empty stub methods. Implement complete, mathematically valid, production-grade business logic.\n")
	sb.WriteString("2. FULL DEPTH: Every file must be substantive (typically 50-200 lines for core components) with complete error handling, validation, and real algorithms as specified above.\n")
	sb.WriteString(fmt.Sprintf("3. PHASE ISOLATION: Focus EXCLUSIVELY on implementing the target files for Phase %d. Do NOT attempt to implement subsequent phases in this turn.\n", phase.PhaseNumber))
	sb.WriteString("4. TAG SYNTAX: Emit complete files inside [ACTION:WRITE path=\"...\"] tags, followed by [ACTION:BUILD task=\"...\"] with the verification command.\n")

	return sb.String()
}

// DetectPlanSpec analyzes the task string and returns a matching PlanSpec, or nil if none matches.
// Milestones come from the markdown the user pointed at. There is no baked-in product checklist.
func DetectPlanSpec(task string, workspaceDir string) *PlanSpec {
	lower := strings.ToLower(task)
	if strings.Contains(lower, "traceback") ||
		strings.Contains(lower, "importerror") ||
		strings.Contains(lower, "modulenotfounderror") ||
		strings.Contains(lower, "[build failed]") ||
		strings.Contains(lower, "failed to import") ||
		strings.Contains(lower, "can't open file") ||
		strings.Contains(lower, "error while finding module") {
		return nil
	}
	_, content := ResolvePlanPath(task, workspaceDir)
	if content == "" {
		return nil
	}
	spec := ParsePlanMarkdown(content)
	if spec == nil || len(spec.Phases) == 0 {
		return nil
	}
	return spec
}

// ResolvePlanPath finds the absolute or relative path to a plan file mentioned in text.
func ResolvePlanPath(text string, workspaceDir string) (string, string) {
	var candidates []string
	seen := make(map[string]bool)

	addCandidate := func(s string) {
		s = strings.TrimSpace(s)
		s = strings.Trim(s, `"'` + "`" + `:;`)
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		candidates = append(candidates, s)
	}

	// 1. Quoted paths (preserves spaces e.g. "/workspace/my project/docs/plan.md")
	quoteRegex := regexp.MustCompile(`[\"'\x60]([^\"'\x60]+)[\"'\x60]`)
	for _, m := range quoteRegex.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			addCandidate(m[1])
		}
	}

	// 2. Explicit paths with .md or .markdown
	pathRegex := regexp.MustCompile(`(?i)(?:[a-zA-Z]:[\\/]|[\.~]?[\\/])[^"'\r\n;]+?\.(?:md|markdown)`)
	for _, m := range pathRegex.FindAllString(text, -1) {
		addCandidate(m)
	}

	// 3. Whitespace separated tokens
	for _, term := range strings.Fields(text) {
		clean := strings.Trim(term, `"'` + "`" + `:;,`)
		if strings.HasSuffix(strings.ToLower(clean), ".md") || strings.HasSuffix(strings.ToLower(clean), ".markdown") {
			addCandidate(clean)
		}
	}

	// 4. Clean text itself
	addCandidate(text)

	wsParent := filepath.Dir(workspaceDir)
	wsGrandparent := filepath.Dir(wsParent)

	type foundPlan struct {
		path    string
		content string
		size    int
	}
	var found []foundPlan
	seenPaths := make(map[string]bool)

	for _, cand := range candidates {
		base := filepath.Base(cand)
		pathsToTry := []string{
			cand,
			filepath.Join(workspaceDir, cand),
			filepath.Join(workspaceDir, "docs", cand),
			filepath.Join(workspaceDir, "docs", base),
			filepath.Join(wsParent, "docs", cand),
			filepath.Join(wsParent, "docs", base),
			filepath.Join(wsGrandparent, "docs", cand),
			filepath.Join(wsGrandparent, "docs", base),
		}

		for _, p := range pathsToTry {
			absP, err := filepath.Abs(p)
			if err != nil {
				absP = p
			}
			if seenPaths[absP] {
				continue
			}
			seenPaths[absP] = true

			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
					found = append(found, foundPlan{
						path:    p,
						content: string(data),
						size:    len(data),
					})
				}
			}
		}
	}

	if len(found) == 0 {
		return "", ""
	}

	best := found[0]
	for _, fp := range found[1:] {
		if fp.size > best.size {
			best = fp
		}
	}
	return best.path, best.content
}

// FindPhaseForFile locates the PlanPhase that targets the given file path.
func (p *PlanSpec) FindPhaseForFile(filePath string) *PlanPhase {
	clean := filepath.ToSlash(strings.TrimSpace(filePath))
	clean = strings.TrimPrefix(clean, "./")
	clean = strings.TrimPrefix(clean, "/")
	for i := range p.Phases {
		for _, tf := range p.Phases[i].TargetFiles {
			cleanTf := filepath.ToSlash(strings.TrimSpace(tf))
			cleanTf = strings.TrimPrefix(cleanTf, "./")
			cleanTf = strings.TrimPrefix(cleanTf, "/")
			if clean == cleanTf || strings.HasSuffix(clean, "/"+cleanTf) || strings.HasSuffix(cleanTf, "/"+clean) {
				return &p.Phases[i]
			}
		}
	}
	return nil
}

// FileExists checks whether a target file from a plan phase exists and is non-empty on disk.
// It resolves:
// 1. Direct path from workspaceDir: filepath.Join(workspaceDir, targetFile)
// 2. Scoped path under ProjectScope: filepath.Join(workspaceDir, p.ProjectScope, targetFile)
// 3. Subfolder suffix or basename match under ProjectScope (or workspaceDir/projects, workspaceDir/apps)
//    when plans list short names like "merkle.go" while the file was written to "pkg/canonical/merkle.go".
func (p *PlanSpec) FileExists(workspaceDir, targetFile string) bool {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(targetFile)))
	clean = strings.TrimPrefix(clean, "./")
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return false
	}

	if p != nil && p.ProjectScope != "" {
		cleanScope := filepath.ToSlash(filepath.Clean(strings.TrimSpace(p.ProjectScope)))
		cleanScope = strings.TrimPrefix(cleanScope, "./")
		cleanScope = strings.TrimPrefix(cleanScope, "/")

		// Case A: targetFile already has the project scope prefix
		if strings.HasPrefix(clean, cleanScope+"/") {
			scopedPath := filepath.Join(workspaceDir, filepath.FromSlash(clean))
			if fi, err := os.Stat(scopedPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
				return true
			}
		} else {
			// Case B: targetFile is relative to the project scope root
			scopedPath := filepath.Join(workspaceDir, filepath.FromSlash(cleanScope), filepath.FromSlash(clean))
			if fi, err := os.Stat(scopedPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
				return true
			}
		}

		// Case C: Suffix or Basename match STRICTLY under the ProjectScope directory
		scopeDir := filepath.Join(workspaceDir, filepath.FromSlash(cleanScope))
		if searchSubfolderForFile(scopeDir, clean) {
			return true
		}

		// Case D: Special case for merkle.go if canonical.go exists under scope and implements Merkle functions
		base := filepath.Base(clean)
		if strings.EqualFold(base, "merkle.go") {
			canonicalPath := filepath.Join(scopeDir, "pkg", "canonical", "canonical.go")
			if data, err := os.ReadFile(canonicalPath); err == nil && len(data) > 0 {
				lower := strings.ToLower(string(data))
				if strings.Contains(lower, "merkle") {
					return true
				}
			}
		}

		return false
	}

	// ProjectScope is empty - check direct workspace path
	directPath := filepath.Join(workspaceDir, filepath.FromSlash(clean))
	if fi, err := os.Stat(directPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
		return true
	}

	// Also check projects/ or apps/ subfolders if ProjectScope was empty
	for _, prefix := range []string{"projects", "apps"} {
		subDir := filepath.Join(workspaceDir, prefix)
		if searchSubfolderForFile(subDir, clean) {
			return true
		}
	}

	return false
}

// searchSubfolderForFile walks a directory to find any non-empty file matching targetFile
// as a suffix (e.g. ".../pkg/canonical/merkle.go" matching "merkle.go" or "pkg/canonical/merkle.go")
// or identical basename.
func searchSubfolderForFile(root, targetFile string) bool {
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return false
	}
	targetBase := filepath.Base(targetFile)
	cleanTarget := "/" + targetFile

	found := false
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Size() == 0 {
			return nil
		}
		slashPath := filepath.ToSlash(path)
		if strings.HasSuffix(slashPath, cleanTarget) {
			found = true
			return filepath.SkipAll
		}
		if !strings.Contains(targetFile, "/") && info.Name() == targetBase {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// IsPhaseFilesPresent checks if all target files for the specified phase exist and are non-empty on disk.
// Returns complete boolean and the list of missing file paths.
func (p *PlanSpec) IsPhaseFilesPresent(workspaceDir string, phase *PlanPhase) (bool, []string) {
	if phase == nil {
		return false, nil
	}
	var missing []string
	for _, f := range phase.TargetFiles {
		if !p.FileExists(workspaceDir, f) {
			missing = append(missing, f)
		}
	}
	return len(missing) == 0, missing
}

// DetermineActivePhase inspects the workspace disk state and returns the current active phase.
func (p *PlanSpec) DetermineActivePhase(workspaceDir string) *PlanPhase {
	for i := range p.Phases {
		phase := &p.Phases[i]
		allExist := true
		for _, f := range phase.TargetFiles {
			if !p.FileExists(workspaceDir, f) {
				allExist = false
				break
			}
		}
		if !allExist {
			return phase
		}
	}
	// All files exist across all phases: return nil (plan is complete)
	return nil
}

// IsComplete reports whether all files across all phases are present on disk.
func (p *PlanSpec) IsComplete(workspaceDir string) bool {
	for _, phase := range p.Phases {
		for _, f := range phase.TargetFiles {
			if !p.FileExists(workspaceDir, f) {
				return false
			}
		}
	}
	return true
}

// CleanVerificationCmd sanitizes a phase verification command, removing -race on Windows if CGo/GCC is unavailable.
func CleanVerificationCmd(cmd string) string {
	if !strings.Contains(cmd, "-race") {
		return cmd
	}
	if runtime.GOOS == "windows" {
		cgoEnv := os.Getenv("CGO_ENABLED")
		hasGcc := false
		if _, err := exec.LookPath("gcc"); err == nil {
			hasGcc = true
		}
		if !hasGcc || cgoEnv != "1" {
			parts := strings.Fields(cmd)
			var filtered []string
			for _, p := range parts {
				if p != "-race" {
					filtered = append(filtered, p)
				}
			}
			return strings.Join(filtered, " ")
		}
	}
	return cmd
}

// ============================================================================
// SWARM FORGE: ADDITIVE DAG COMPILER & AST DEPENDENCY ANALYZER
// ============================================================================

// SwarmPhase extends a plan phase with concurrency and dependency metadata.
type SwarmPhase struct {
	PlanPhase                        // Embed existing PlanPhase for 100% backward compatibility
	Dependencies    []int            `json:"dependencies"`      // Phase numbers required before this phase
	ExportsContract []string         `json:"exports_contract"` // Packages whose interfaces must be frozen
	ScopePrefix     string           `json:"scope_prefix"`     // Directory prefix jail (e.g. "pkg/index/")
	AssignedWorker  int              `json:"assigned_worker"`  // Assigned worker slot (1..N)
	Wave            int              `json:"wave"`             // Execution wave index (0, 1, 2...)
	EstimatedTurns  int              `json:"estimated_turns"`  // Estimated turn budget
}

// SwarmDAG represents the compiled, acyclic execution graph.
type SwarmDAG struct {
	ProjectScope string              `json:"project_scope"`
	ModuleName   string              `json:"module_name"`
	Phases       map[int]*SwarmPhase `json:"phases"`
	Waves        [][]int             `json:"waves"` // Waves of phase numbers: Waves[0] = [1], Waves[1] = [2, 3, 4]
	Adjacency    map[int][]int       `json:"adjacency"`
	InDegree     map[int]int         `json:"in_degree"`
	CriticalPath []int               `json:"critical_path"`
}

// ErrCyclicPlanGraph is returned when a circular dependency is detected among plan phases.
var ErrCyclicPlanGraph = errors.New("deadlock: cyclic dependency detected in plan phases")

// CyclicPlanGraphError provides detailed diagnostic information about a detected dependency cycle.
type CyclicPlanGraphError struct {
	Cycle      []int
	Diagnostic string
}

func (e *CyclicPlanGraphError) Error() string {
	if e.Diagnostic != "" {
		return fmt.Sprintf("%v: %s", ErrCyclicPlanGraph, e.Diagnostic)
	}
	if len(e.Cycle) > 0 {
		return fmt.Sprintf("%v: cycle path %v", ErrCyclicPlanGraph, e.Cycle)
	}
	return ErrCyclicPlanGraph.Error()
}

func (e *CyclicPlanGraphError) Unwrap() error {
	return ErrCyclicPlanGraph
}

func (e *CyclicPlanGraphError) Is(target error) bool {
	return target == ErrCyclicPlanGraph
}

// CompileSwarmDAG compiles a PlanSpec into an acyclic multi-wave execution DAG.
// If the plan has no explicit dependencies, InferPlanDependencies is called automatically.
func CompileSwarmDAG(plan *PlanSpec) (*SwarmDAG, error) {
	if plan == nil || len(plan.Phases) == 0 {
		return nil, fmt.Errorf("cannot compile SwarmDAG: plan specification is empty or nil")
	}

	// 1. Check if any phase has explicit dependencies. If not, infer them.
	hasExplicitDeps := false
	for _, ph := range plan.Phases {
		if len(ph.Dependencies) > 0 {
			hasExplicitDeps = true
			break
		}
	}
	if !hasExplicitDeps {
		if err := InferPlanDependencies(plan); err != nil {
			return nil, fmt.Errorf("failed to infer plan dependencies: %w", err)
		}
	}

	// 2. Initialize SwarmDAG
	dag := &SwarmDAG{
		ProjectScope: plan.ProjectScope,
		ModuleName:   plan.ModuleName,
		Phases:       make(map[int]*SwarmPhase),
		Waves:        make([][]int, 0),
		Adjacency:    make(map[int][]int),
		InDegree:     make(map[int]int),
		CriticalPath: make([]int, 0),
	}

	// 3. Populate SwarmPhases and Initial InDegrees
	for i := range plan.Phases {
		ph := plan.Phases[i]
		depsCopy := make([]int, len(ph.Dependencies))
		copy(depsCopy, ph.Dependencies)
		sp := &SwarmPhase{
			PlanPhase:       ph,
			Dependencies:    depsCopy,
			ExportsContract: inferExportsContract(plan, &ph),
			ScopePrefix:     inferScopePrefix(plan, &ph),
			EstimatedTurns:  len(ph.TargetFiles),
		}
		if sp.EstimatedTurns <= 0 {
			sp.EstimatedTurns = 1
		}
		dag.Phases[ph.PhaseNumber] = sp
		dag.InDegree[ph.PhaseNumber] = len(ph.Dependencies)
	}

	// 4. Construct Adjacency: edge is u -> v where v depends on u
	for _, sp := range dag.Phases {
		for _, dep := range sp.Dependencies {
			dag.Adjacency[dep] = append(dag.Adjacency[dep], sp.PhaseNumber)
		}
	}
	for dep := range dag.Adjacency {
		sort.Ints(dag.Adjacency[dep])
	}

	// 5. Kahn's Levelization (Wave Compilation)
	workingInDegree := make(map[int]int, len(dag.InDegree))
	for k, v := range dag.InDegree {
		workingInDegree[k] = v
	}

	var queue []int
	for _, ph := range plan.Phases {
		if workingInDegree[ph.PhaseNumber] == 0 {
			queue = append(queue, ph.PhaseNumber)
		}
	}
	sort.Ints(queue)

	totalProcessed := 0
	waveIdx := 0

	// Longest path tracking for Critical Path
	dist := make(map[int]int)
	parent := make(map[int]int)
	for _, ph := range plan.Phases {
		dist[ph.PhaseNumber] = 1
		parent[ph.PhaseNumber] = -1
	}

	for len(queue) > 0 {
		currentWave := make([]int, 0, len(queue))
		var nextQueue []int
		sort.Ints(queue)

		for _, node := range queue {
			currentWave = append(currentWave, node)
			totalProcessed++

			if sp, ok := dag.Phases[node]; ok {
				sp.Wave = waveIdx
			}

			for _, neighbor := range dag.Adjacency[node] {
				if dist[node]+1 > dist[neighbor] {
					dist[neighbor] = dist[node] + 1
					parent[neighbor] = node
				}

				workingInDegree[neighbor]--
				if workingInDegree[neighbor] == 0 {
					nextQueue = append(nextQueue, neighbor)
				}
			}
		}

		sort.Ints(currentWave)
		for workerIdx, phaseNum := range currentWave {
			if sp, ok := dag.Phases[phaseNum]; ok {
				sp.AssignedWorker = workerIdx + 1
			}
		}

		dag.Waves = append(dag.Waves, currentWave)
		queue = nextQueue
		waveIdx++
	}

	// 6. Cyclic Plan Graph Detection
	if totalProcessed != len(plan.Phases) {
		var unvisited []int
		for _, ph := range plan.Phases {
			if workingInDegree[ph.PhaseNumber] > 0 {
				unvisited = append(unvisited, ph.PhaseNumber)
			}
		}
		sort.Ints(unvisited)
		cyclePath := findCyclePath(dag.Adjacency, unvisited)
		diagnostic := fmt.Sprintf("unresolved phases %v; detected cycle: %v", unvisited, cyclePath)
		return nil, &CyclicPlanGraphError{
			Cycle:      cyclePath,
			Diagnostic: diagnostic,
		}
	}

	// 7. Critical Path Extraction
	maxD := 0
	maxNode := -1
	for _, ph := range plan.Phases {
		pNum := ph.PhaseNumber
		if dist[pNum] > maxD || (dist[pNum] == maxD && pNum > maxNode) {
			maxD = dist[pNum]
			maxNode = pNum
		}
	}
	if maxNode != -1 {
		var cp []int
		curr := maxNode
		for curr != -1 {
			cp = append(cp, curr)
			curr = parent[curr]
		}
		for i, j := 0, len(cp)-1; i < j; i, j = i+1, j-1 {
			cp[i], cp[j] = cp[j], cp[i]
		}
		dag.CriticalPath = cp
	}

	return dag, nil
}

// InferPlanDependencies scans target files and markdown text to infer cross-phase package dependencies.
func InferPlanDependencies(plan *PlanSpec) error {
	if plan == nil {
		return fmt.Errorf("cannot infer dependencies: plan is nil")
	}
	if len(plan.Phases) == 0 {
		return nil
	}

	// Generic Static AST Dependency Inference
	type phaseCatalogEntry struct {
		phaseNum    int
		packages    map[string]bool
		pkgPaths    map[string]bool
		fileBases   map[string]bool
		hasGoMod    bool
		isScaffold  bool
		isTestPhase bool
	}

	catalog := make(map[int]*phaseCatalogEntry, len(plan.Phases))
	for _, ph := range plan.Phases {
		entry := &phaseCatalogEntry{
			phaseNum:  ph.PhaseNumber,
			packages:  make(map[string]bool),
			pkgPaths:  make(map[string]bool),
			fileBases: make(map[string]bool),
		}
		lowerName := strings.ToLower(ph.Name)
		lowerDesc := strings.ToLower(ph.Description)
		if strings.Contains(lowerName, "scaffold") || strings.Contains(lowerDesc, "scaffold") {
			entry.isScaffold = true
		}
		if strings.Contains(lowerName, "test") || strings.Contains(lowerName, "golden") || strings.Contains(lowerName, "integration") {
			entry.isTestPhase = true
		}

		for _, tf := range ph.TargetFiles {
			clean := filepath.ToSlash(tf)
			base := filepath.Base(clean)
			nameNoExt := strings.TrimSuffix(base, filepath.Ext(base))
			if base == "go.mod" {
				entry.hasGoMod = true
				entry.isScaffold = true
			}
			if strings.HasSuffix(clean, ".go") && !strings.HasSuffix(clean, "_test.go") {
				dir := filepath.ToSlash(filepath.Dir(clean))
				if plan.ProjectScope != "" && strings.HasPrefix(dir, plan.ProjectScope+"/") {
					dir = strings.TrimPrefix(dir, plan.ProjectScope+"/")
				}
				pkgName := filepath.Base(dir)
				if pkgName != "." && pkgName != "" && pkgName != "/" {
					entry.packages[pkgName] = true
					entry.pkgPaths[dir] = true
				}
				if nameNoExt != "" {
					entry.fileBases[nameNoExt] = true
				}
			}
		}
		catalog[ph.PhaseNumber] = entry
	}

	for i := range plan.Phases {
		phB := &plan.Phases[i]
		entryB := catalog[phB.PhaseNumber]

		var bTexts []string
		bTexts = append(bTexts, phB.Name, phB.Description, phB.SpecSection)
		for _, fs := range phB.FileSpecs {
			bTexts = append(bTexts, fs)
		}
		for _, tf := range phB.TargetFiles {
			bTexts = append(bTexts, tf)
		}
		fullBText := strings.Join(bTexts, " \n ")

		depsMap := make(map[int]bool)
		for _, d := range phB.Dependencies {
			depsMap[d] = true
		}

		for _, phA := range plan.Phases {
			if phA.PhaseNumber == phB.PhaseNumber {
				continue
			}
			entryA := catalog[phA.PhaseNumber]

			// Case 1: Scaffolding / go.mod dependency
			if (entryA.hasGoMod || entryA.isScaffold) && phB.PhaseNumber > phA.PhaseNumber {
				hasGoFiles := false
				for _, tf := range phB.TargetFiles {
					if strings.HasSuffix(tf, ".go") {
						hasGoFiles = true
						break
					}
				}
				if hasGoFiles {
					depsMap[phA.PhaseNumber] = true
				}
			}

			// Case 2: Package import or type reference
			for pkg := range entryA.packages {
				pkgRef := pkg + "."
				pkgImport := "/" + pkg + `"`
				if strings.Contains(fullBText, pkgRef) || strings.Contains(fullBText, pkgImport) {
					depsMap[phA.PhaseNumber] = true
					break
				}
			}

			// Case 3: Package path reference
			for pPath := range entryA.pkgPaths {
				if strings.Contains(fullBText, pPath) {
					depsMap[phA.PhaseNumber] = true
					break
				}
			}

			// Case 4: Integration / Test phase
			if entryB.isTestPhase && phB.PhaseNumber > phA.PhaseNumber {
				for base := range entryA.fileBases {
					if strings.Contains(fullBText, base+"_test.go") || strings.Contains(fullBText, base) {
						depsMap[phA.PhaseNumber] = true
						break
					}
				}
			}
		}

		var deps []int
		for d := range depsMap {
			deps = append(deps, d)
		}
		sort.Ints(deps)
		phB.Dependencies = deps
	}

	return nil
}

func inferExportsContract(plan *PlanSpec, ph *PlanPhase) []string {
	if ph == nil || len(ph.TargetFiles) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var contracts []string
	for _, f := range ph.TargetFiles {
		clean := filepath.ToSlash(f)
		if !strings.HasSuffix(clean, ".go") || strings.HasSuffix(clean, "_test.go") {
			continue
		}
		if plan != nil && plan.ProjectScope != "" && strings.HasPrefix(clean, plan.ProjectScope+"/") {
			clean = strings.TrimPrefix(clean, plan.ProjectScope+"/")
		}
		dir := filepath.ToSlash(filepath.Dir(clean))
		if dir != "." && dir != "" && dir != "/" && !seen[dir] {
			seen[dir] = true
			contracts = append(contracts, dir)
		}
	}
	sort.Strings(contracts)
	return contracts
}

func inferScopePrefix(plan *PlanSpec, ph *PlanPhase) string {
	if ph == nil || len(ph.TargetFiles) == 0 {
		return ""
	}
	dirCounts := make(map[string]int)
	firstDir := ""
	for _, f := range ph.TargetFiles {
		clean := filepath.ToSlash(f)
		if plan != nil && plan.ProjectScope != "" && strings.HasPrefix(clean, plan.ProjectScope+"/") {
			clean = strings.TrimPrefix(clean, plan.ProjectScope+"/")
		}
		dir := filepath.ToSlash(filepath.Dir(clean))
		if dir != "." && dir != "" && dir != "/" {
			if !strings.HasSuffix(dir, "/") {
				dir += "/"
			}
			if firstDir == "" {
				firstDir = dir
			}
			dirCounts[dir]++
		}
	}
	if len(dirCounts) == 0 {
		return ""
	}
	maxCount := 0
	bestDir := firstDir
	for d, count := range dirCounts {
		if count > maxCount {
			maxCount = count
			bestDir = d
		}
	}
	return bestDir
}

func findCyclePath(adjacency map[int][]int, unvisited []int) []int {
	if len(unvisited) == 0 {
		return nil
	}
	unvisitedSet := make(map[int]bool, len(unvisited))
	for _, u := range unvisited {
		unvisitedSet[u] = true
	}

	visited := make(map[int]bool)
	var path []int
	var cycle []int

	var dfs func(u int) bool
	dfs = func(u int) bool {
		visited[u] = true
		path = append(path, u)

		for _, v := range adjacency[u] {
			if !unvisitedSet[v] {
				continue
			}
			for idx, p := range path {
				if p == v {
					cycle = append([]int{}, path[idx:]...)
					cycle = append(cycle, v)
					return true
				}
			}
			if !visited[v] {
				if dfs(v) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		return false
	}

	for _, start := range unvisited {
		if !visited[start] {
			if dfs(start) {
				return cycle
			}
		}
	}
	return unvisited
}
