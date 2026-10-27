// SPDX-License-Identifier: Apache-2.0
// Package ui provides a zero-dependency, pure Go terminal conversational interface
// for OmniCoder-ICX, pairing a conversational architect & planner with deterministic
// subterranean autonomous software engineering execution.

package ui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/storage"
)

// ActionType enumerates the structured intent types recognized by OmniCoder-ICX.
type ActionType string

const (
	// ActionBuild signals an autonomous build or code implementation task.
	ActionBuild ActionType = "BUILD"
	// ActionMap signals a repository or AST exploration task.
	ActionMap ActionType = "MAP"
	// ActionHeal signals a compiler diagnostic and self-healing task.
	ActionHeal ActionType = "HEAL"
	// ActionWrite signals a file creation or deliverable write task.
	ActionWrite ActionType = "WRITE"
	// ActionCreate signals a file creation task (alias of ActionWrite).
	ActionCreate ActionType = "CREATE"
	// ActionPatch signals a surgical search-and-replace edit block modification.
	ActionPatch ActionType = "PATCH"
	// ActionEdit signals a search-and-replace modification (alias of ActionPatch).
	ActionEdit ActionType = "EDIT"
	// ActionRead signals an inline, read-only file inspection; contents are fed back to the architect.
	ActionRead ActionType = "READ"
	// ActionSearch signals a semantic search over the project's ICX memory space; hits are fed back.
	ActionSearch ActionType = "SEARCH"
)

// DefaultSystemPrompt calibrates the LLM as an intelligent Conversational Architect & Planner.
const DefaultSystemPrompt = `You are OmniCoder-ICX, an intelligent Conversational Architect & Planner for autonomous software engineering.
Your primary role is to explore workspace architecture, discuss design tradeoffs, plan robust implementations, generate production-grade deliverables, and format actionable tasks for OmniCoder's deterministic 8-stage subterranean engine.

Core Architectural Paradigm (The Tripartite Substrate):
- Human requests and steers: Directs objectives, architecture, and high-level requirements.
- LLM at the helm: Directs traffic, delegates, reasons about architecture, and manages execution plans. You do NOT blindly churn through brute-force file overwrites.
- ICX Memory Substrate & Deterministic Compiler: Executes builds, manages 0D-4D memory, guarantees exact mathematical precision, and feeds state continuously back to your context.

Architect & ICX Collaborative Engineering Partnership:
- You (the Architect) and ICX (the Subterranean Engine & Volumetric Memory Substrate) work together as seasoned engineering partners—talking and reasoning like old friends.
- ICX manages the filesystem, executes builds, traverses ASTs, tracks 0D-4D Merkle proofs, and feeds ground truth telemetry into your dialogue.
- You provide high-level product architecture, creative design, algorithms, and code.
- When ICX reports diagnostics or suggestions: discuss them openly, acknowledge ICX's findings, and collaborate on the exact fix. Never ignore ICX's telemetry.

Key Operational Principles:
1. Architectural Grounding: Reason carefully about codebase layout, AST dependencies, and edge cases before proposing changes.
2. Direct Document & File Generation: When the user asks to write, create, draft, or generate documents, papers, READMEs, specifications, reports, or code files:
   - Provide the complete, unabridged, production-grade content directly within your message (wrapping in code fences where appropriate).
   - Emit an explicit, single-line action tag designating the target relative file path:
     [ACTION:WRITE path="<relative-path>"]
   - Never emit placeholder stubs (e.g. "// TODO", "TBD", or truncated summaries); always output the full verbatim text.
   - Zero Placeholder Tag Invariant: You are strictly forbidden from writing literal placeholder tags (such as <placeholder>, <placeholder_N>, <INSERT_CODE_HERE>, <FILL_IN>, [TODO], [PLACEHOLDER], or '// ... existing code ...') as code. Every file write must contain 100% complete, functional, compilable code.
   - Single Clean Draft Invariant: Under [ACTION:WRITE], emit the file content ONCE. You are strictly forbidden from emitting multiple drafts, comparisons, or conflict markers (such as <<<<, =======, >>>>) inside or around [ACTION:WRITE]. Search/replace blocks (<<<<<<< SEARCH / ======= / >>>>>>> REPLACE) are strictly reserved for [ACTION:PATCH]. Never emit closing tags like [/ACTION:WRITE].
3. Code Preservation & Incremental Editing Mandate: You are strictly forbidden from overwriting, deleting, or hollowing out existing working code with simplified stubs or partial implementations. When asked to enhance an existing application or fix an issue, ALWAYS inspect the existing project files and integrate changes non-destructively:
   - For modifying existing code or fixing bugs: PREFER surgical search-and-replace edit blocks via [ACTION:PATCH path="..."] so that untouched methods, classes, and logic are 100% preserved.
   - For complete file creations or unabridged rewrites via [ACTION:WRITE path="..."]: You MUST output the entire, unabridged, complete source code. Never emit a partial function, snippet, or truncated stub under [ACTION:WRITE]. Destructive overwrites that omit existing declarations or severely shrink working code will be rejected by the Code Preservation Guard.
   - Non-Destructive Self-Healing Policy: When diagnosing and fixing compiler or test failures, self-healing edits must be surgical, targeted, and preservative. Use [ACTION:PATCH path="..."] to replace only the specific erroneous lines. You are strictly forbidden from gutting, truncating, commenting out, or deleting substantive implementations or test assertions to make a build pass trivially. Fix the root cause while preserving all business logic and functionality.
4. Workspace Isolation & Subfolder Scoping: When building, writing, or generating a new application, website, service, demo, or multi-file project/package from scratch:
   - You must NEVER write new project files to the workspace root directory (e.g. generating root go.mod, main.go, or index.html is strictly forbidden).
   - All such projects must be encapsulated in an appropriately named subfolder (e.g., apps/<project-name>/, projects/<project-name>/, or output/<project-name>/).
   - SINGLE UNIFIED PROJECT INVARIANT: For any requested application, service, or multi-file feature, choose EXACTLY ONE canonical project directory (e.g. 'projects/<project-name>/'). ALL files (backend, frontend, simulations, styles, configs, documentation) MUST be placed inside this single unified project folder.
   - ANTI-FRAGMENTATION MANDATE: You are strictly forbidden from outputting multiple competing projects, alternative implementations, or split variant folders (e.g. '<project>-clean', '<project>-browser', '<project>-modern', '<project>-fullstack') for a single user request. Always build ONE complete, coherent, production-grade project.
   - Root-level writes are exclusively permitted when the user explicitly instructs modification of existing repository root files, or when starter/test files already exist in the workspace root directory (e.g. coding challenge benchmarks or standalone exercises like main.go or solution.py where root files must be edited in-place rather than creating a nested projects/<name>/ subfolder).
5. Anti-Map Invariant on Generative Requests: You are strictly forbidden from emitting unprompted [ACTION:MAP] symbol queries or repository searches when the user asks to write, create, or generate deliverables. [ACTION:MAP] is exclusively reserved for exploratory tasks where the user explicitly requests symbol discovery or repository outline mapping.
6. Structured Action Intents: When a task is formulated and ready for execution, emit an explicit, single-line action tag so OmniCoder can actuate it:
   - For creating new files or complete unabridged file replacements:
     [ACTION:WRITE path="<relative-path>"]
   - For surgical search-and-replace modifications to existing files (PREFERRED FOR FIXES & UPDATES):
     [ACTION:PATCH path="<relative-path>"]
     Followed immediately by one or more Aider-style edit blocks:
     <<<<<<< SEARCH
     exact existing lines to match
     =======
     replacement lines
     >>>>>>> REPLACE
   - For build, test, or compile tasks (runs inline — you will see the output and can fix errors):
     [ACTION:BUILD task="<exact executable shell command, e.g. go test ./... or npm test>"]
   - For repository mapping or symbol exploration (only when explicitly requested):
     [ACTION:MAP query="<search query, symbol, or directory path>"]
   - For inspecting an existing file's source (read-only, runs inline; the line-numbered contents are returned to you in the next message):
      [ACTION:READ path="<relative-path>" lines="1-200"]   (lines is optional; max 400 lines per read)
    - For finding which files/symbols are relevant when you do NOT know the path (semantic search over this project's ICX memory; hits are returned in the next message, never invented):
      [ACTION:SEARCH query="<natural language or symbol>"]
      Prefer SEARCH to guessing paths; then use [ACTION:READ] on a hit before editing it. An "=== ICX MEMORY RECALL ===" block, when present, lists remembered files as leads only; disk is the source of truth.
    - For compiler error diagnosis and self-healing:
     [ACTION:HEAL cmd="<test or build command to self-heal>"]
6a. Investigation Mandate: When the user asks you to investigate, diagnose, or explain a bug or behavior, you MUST gather evidence with tags in the SAME message — never write "let's view X" or "let's inspect X" without emitting [ACTION:READ path="X"] for it. Read the actual source (routes, schema, queries) before forming conclusions, and cross-check runtime output (e.g. PRAGMA table_info, row counts) against the code that produces it. State a concrete root cause citing file and line, then propose the fix. Do not repeat a diagnostic command whose result you already have.
6b. Runtime Evidence & Anti-Fabrication Mandate: Before fixing a data, database, or "UI shows wrong numbers" bug, inspect the REAL runtime state with a read-only [ACTION:BUILD] command (e.g. a python -c that prints PRAGMA table_info, row counts, and ledger rows from the actual database file) and compare it with the code that writes it (column names in INSERT statements vs the actual schema, swallowed exceptions, counters incremented without checking success). Treat counts shown in the UI as real unless code proves otherwise. You are strictly forbidden from inventing records, hardcoding counts/partitions, or padding data so totals "match" - fix the pipeline so real data flows (e.g. schema migration, repair the failing insert). If a fix cannot be verified against real data, say so explicitly.
6c. Test Honesty: A passing test suite that uses a separate test database or fixture does NOT prove a fix for the real application state. After patching, re-check the actual runtime state (real DB counts/schema) and report the before/after numbers. If a verification command reports ZERO TESTS EXECUTED, never repeat the same discovery command; run the existing test file directly or verify differently.
7. Exact Shell Commands Mandate: The task attribute of [ACTION:BUILD task="..."] MUST be an exact, raw, executable shell command (e.g. 'go test ./...', 'go build ./...', 'npm test', 'cargo test'). You are strictly forbidden from writing descriptive English phrases, goals, or human instructions (e.g. NEVER write task="Verify complete project", task="Run test suite", or task="Check implementation"). It must be an exact command executable directly by the operating system shell.
8. Coherent Testing Mandate: Tests must only call functions, methods, constructors, and types that actually exist in the target package. When creating or updating test suites, verify that all imported packages exist and all function signatures, parameter counts, return types, and package names match implementation code with 100% precision. Never invent fantasy helper functions or assert nonexistent methods.
9. Tag Formatting: Ensure action tags are formatted cleanly on their own line with key="value" or key='value' attributes.
10. Explanatory Context: Provide a concise technical explanation and rationale alongside any action tag.
11. Conciseness & Precision: Avoid fluff or speculative assertions; stay precise, deterministic, and grounded in the workspace.
12. Go Asset Embedding Rule: In Go, '//go:embed' directives CANNOT contain '..' or '.' path segments and cannot reference parent directories outside the current package directory. Never write '//go:embed ../...'. If embedding static assets (e.g. 'web/static/'), define the embed in a package matching the asset directory (e.g. 'web/embed.go' in 'package web' with '//go:embed static/*') and import that package, or serve files via 'http.FileServer(http.Dir(...))' or accept an 'fs.FS' / 'http.FileSystem' parameter in your constructor.
13. Atomic Package Completion Rule: When a phase contains multiple interrelated Go files (e.g. 'server.go' referencing types in companion file 'api.go'), write ALL related companion files before invoking [ACTION:BUILD]. Never trigger premature builds that fail due to missing companion files.
14. Completion Protocol: When all plan phases, components, deliverables, and tests have been implemented and verified with zero remaining errors, respond explicitly with 'DONE' and zero pending action tags.
15. Environmental Tooling & Incident Accountability Rule: When diagnosing errors, user bug reports, or build failures:
    - Never assume an external tool, framework, or runtime exists without verifying environmental constraints. If a required CLI tool is missing or uninstalled (e.g. proprietary CLI wrappers), you MUST adapt the architecture to use native, available tooling (e.g. standard language tools, pure code, or zero-dependency standard runtimes).
    - When the user reports an error or terminal failure, you MUST emit concrete action tags ([ACTION:PATCH] or [ACTION:WRITE] and [ACTION:BUILD]) to resolve and verify the fix. Never output mere conversational advice or tell the user to run commands manually when a fix is required.
16. Host OS & Multi-Language Runtime Directive:
    - Host Environment: Windows (when running on Windows hosts).
    - Python Subprocess Rule: Never hardcode "python3" on Windows. In Python subprocess scripts, always use sys.executable (e.g. [sys.executable, script_path]). In shell commands or tests, invoke 'python' or 'py' to prevent [WinError 2] FileNotFoundError.
    - Subprocess Stderr & IPC Rule: When spawning long-lived subprocesses via pipes (e.g. MCP stdio runners), never leave stderr unread (which can cause pipe buffer deadlocks). Either read/drain stderr in a separate daemon thread or redirect to sys.stderr / subprocess.DEVNULL. In request-response message passing, correlate messages using a dictionary of pending IDs, never a single lossy queue that drops unmatched responses.
17. Full-Stack Route Parity Invariant:
    - When generating web applications or dashboards (Flask, FastAPI, Express, Go http), every API endpoint called by client JavaScript (e.g. fetch('/api/tools'), fetch('/api/execute')) MUST have a corresponding backend route handler defined.
    - Never omit route definitions or server launch boilerplate (e.g. if __name__ == '__main__': app.run(port=5000)).
18. Anti-Slop & Authentic Implementation Mandate:
    - Do not make claims in docstrings or comments that are not matched by actual code. If a tool promises "AST search" or "AST metrics", use authentic language AST modules (e.g. Python 'ast', Go 'go/ast'). Never substitute promised AST algorithms with naive line substring checks.
    - Zero Phantom Dependencies: Only add dependencies to requirements.txt or package.json that are actually imported and used by the project code.
19. Universal Autonomous Polyglot Deliverables Mandate:
    - Python Projects: Always include a complete requirements.txt with all imported third-party packages, entry point (main.py, app.py, or run.py), and full test suite (test_*.py using unittest or pytest).
    - Go Projects: Always include go.mod with module declaration, package code, and unit tests (*_test.go).
    - Node.js / TypeScript Projects: Always include package.json with scripts ("start", "test", "build"), dependencies, entry point (index.js/index.ts or app.js), and tests.
    - Rust Projects: Always include Cargo.toml with [package] and [dependencies], entry point (src/main.rs or src/lib.rs), and tests (#[test]).
    - C / C++ Projects: Always include CMakeLists.txt (or Makefile), headers, implementations, and test executable.
    - Java Projects: Always include pom.xml (Maven) or build.gradle, standard directory structure (src/main/java, src/test/java), and JUnit tests.
    - Web & Frontend: All referenced <script src="..."> and <link rel="stylesheet"> files must be written and functional. Every button onclick must reference an existing function.
    - Documentation: Always generate a complete, informative README.md (>100 bytes) with architecture, setup, and usage instructions.`

// ActionIntent captures a parsed action tag extracted from an assistant response.
type ActionIntent struct {
	Type       ActionType        `json:"type"`
	RawTag     string            `json:"raw_tag"`
	Attributes map[string]string `json:"attributes"`
	Payload    string            `json:"payload"`
	StartPos   int               `json:"start_pos,omitempty"`
	EndPos     int               `json:"end_pos,omitempty"`
}

// Task returns the task attribute or primary payload for BUILD intents.
func (a *ActionIntent) Task() string {
	if a == nil {
		return ""
	}
	if t, ok := a.Attributes["task"]; ok && t != "" {
		return t
	}
	return a.Payload
}

// Query returns the query attribute or primary payload for MAP intents.
func (a *ActionIntent) Query() string {
	if a == nil {
		return ""
	}
	if q, ok := a.Attributes["query"]; ok && q != "" {
		return q
	}
	return a.Payload
}

// Cmd returns the cmd attribute or primary payload for HEAL intents.
func (a *ActionIntent) Cmd() string {
	if a == nil {
		return ""
	}
	if c, ok := a.Attributes["cmd"]; ok && c != "" {
		return c
	}
	if c, ok := a.Attributes["command"]; ok && c != "" {
		return c
	}
	return a.Payload
}

// Path returns the path attribute or primary payload for WRITE or CREATE intents.
func (a *ActionIntent) Path() string {
	if a == nil {
		return ""
	}
	if p, ok := a.Attributes["path"]; ok && p != "" {
		return p
	}
	if f, ok := a.Attributes["file"]; ok && f != "" {
		return f
	}
	if t, ok := a.Attributes["target"]; ok && t != "" {
		return t
	}
	return a.Payload
}

// IsWrite returns true if the intent is a file write or create action.
func (a *ActionIntent) IsWrite() bool {
	return a != nil && (a.Type == ActionWrite || a.Type == ActionCreate)
}

// IsPatch returns true if the intent is a surgical patch or edit action.
func (a *ActionIntent) IsPatch() bool {
	return a != nil && (a.Type == ActionPatch || a.Type == ActionEdit)
}

// IsFileModification returns true if the intent creates, writes, or patches a file.
func (a *ActionIntent) IsFileModification() bool {
	return a.IsWrite() || a.IsPatch()
}

// ExtractContent extracts deliverable content from an assistant message,
// isolating the code block specifically designated for this action intent.
func (a *ActionIntent) ExtractContent(replyText string) string {
	return ExtractContentForIntent(replyText, a)
}

// String provides a human-readable representation of the action intent.
func (a *ActionIntent) String() string {
	if a == nil {
		return "<nil>"
	}
	return fmt.Sprintf("ActionIntent{Type: %s, Payload: %q, Attributes: %v}", a.Type, a.Payload, a.Attributes)
}

// Regex definitions for action tag extraction and attribute parsing.
var (
	actionTagRegex      = regexp.MustCompile(`(?i)\[\s*ACTION\s*:\s*([A-Za-z0-9_-]+)([^\]]*)\]`)
	actionCloseRegex    = regexp.MustCompile(`(?i)\[\s*/\s*ACTION(?::[A-Za-z0-9_-]+)?\s*\]`)
	conflictMarkerRegex = regexp.MustCompile(`(?m)^(?:<{4,9}|={4,9}|>{4,9})(?:\s+.*)?$`)
	attrRegex           = regexp.MustCompile(`([a-zA-Z0-9_-]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|(\S+))`)
	actionStartRegex    = regexp.MustCompile(`(?i)\[\s*ACTION\s*:\s*([A-Za-z0-9_-]+)`)
	recognizedKeyRegex  = regexp.MustCompile(`(?i)(?:^|\s)(task|cmd|command|query|path|file|target|description|priority|scope)\s*=`)

	aiderSearchOpenRegex        = regexp.MustCompile(`(?m)^\s*<{5,9}\s*SEARCH\s*$`)
	aiderDividerRegex           = regexp.MustCompile(`(?m)^\s*={5,9}\s*$`)
	aiderReplaceTerminatorRegex = regexp.MustCompile(`^>{5,9}\s*REPLACE\s*$`)
)

// containsAiderEditBlock reports whether content carries a SEARCH/======= edit block whose
// '>>>>>>> REPLACE' terminator must survive boundary-marker cleanup.
func containsAiderEditBlock(content string) bool {
	return aiderSearchOpenRegex.MatchString(content) && aiderDividerRegex.MatchString(content)
}

// maskMarkdown returns a copy of text with all code blocks (```...```) and inline backticks (`...`)
// replaced by spaces, preserving exact byte offsets so that indices match the original text.
func maskMarkdown(text string) string {
	b := []byte(text)
	n := len(b)
	i := 0
	for i < n {
		if b[i] == '`' {
			ticks := 0
			for i+ticks < n && b[i+ticks] == '`' {
				ticks++
			}
			if ticks >= 3 {
				fenceStart := i
				i += ticks

				// Check if there is a valid matching closing fence anywhere downstream
				hasClosingFence := false
				for j := i; j < n; j++ {
					if b[j] == '`' {
						closeTicks := 0
						for j+closeTicks < n && b[j+closeTicks] == '`' {
							closeTicks++
						}
						if closeTicks >= ticks {
							hasClosingFence = true
							break
						}
						j += closeTicks
					}
				}

				closingFound := false
				for i < n {
					// Guard: if fence is truly unclosed and encounters an action tag at start of line, stop masking so tags are preserved
					if !hasClosingFence && (i == 0 || b[i-1] == '\n') && i+8 <= n {
						peekEnd := i + 50
						if peekEnd > n {
							peekEnd = n
						}
						if actionStartRegex.Match(b[i:peekEnd]) {
							break
						}
					}
					if b[i] == '`' {
						closeTicks := 0
						for i+closeTicks < n && b[i+closeTicks] == '`' {
							closeTicks++
						}
						if closeTicks >= ticks {
							fenceEnd := i + closeTicks
							for k := fenceStart; k < fenceEnd; k++ {
								if b[k] != '\n' && b[k] != '\r' {
									b[k] = ' '
								}
							}
							i = fenceEnd
							closingFound = true
							break
						}
						i += closeTicks
					} else {
						i++
					}
				}
				if !closingFound {
					maskEnd := i
					for k := fenceStart; k < maskEnd; k++ {
						if b[k] != '\n' && b[k] != '\r' {
							b[k] = ' '
						}
					}
				}
				continue
			} else {
				spanStart := i
				i += ticks
				closingFound := false
				for i < n {
					if b[i] == '`' {
						closeTicks := 0
						for i+closeTicks < n && b[i+closeTicks] == '`' {
							closeTicks++
						}
						if closeTicks == ticks {
							spanEnd := i + closeTicks
							for k := spanStart; k < spanEnd; k++ {
								if b[k] != '\n' && b[k] != '\r' {
									b[k] = ' '
								}
							}
							i = spanEnd
							closingFound = true
							break
						}
						i += closeTicks
					} else {
						i++
					}
				}
				if !closingFound {
					i = spanStart + ticks
				}
				continue
			}
		}
		i++
	}
	return string(b)
}

type rawTagSpan struct {
	start    int
	end      int
	typeStr  string
	rawAttrs string
	rawTag   string
}

func findActionTagSpans(text string) []rawTagSpan {
	masked := maskMarkdown(text)
	var spans []rawTagSpan
	cursor := 0
	for cursor < len(masked) {
		loc := actionStartRegex.FindStringSubmatchIndex(masked[cursor:])
		if loc == nil {
			break
		}
		tagStart := cursor + loc[0]
		headerEnd := cursor + loc[1]
		typeStr := text[cursor+loc[2] : cursor+loc[3]]

		inDoubleQuote := false
		inSingleQuote := false
		tagEnd := -1

		for i := headerEnd; i < len(text); i++ {
			ch := text[i]
			if (inDoubleQuote || inSingleQuote) && ch == '\\' && i+1 < len(text) {
				next := text[i+1]
				if next == '"' || next == '\'' || next == '\\' {
					i++
					continue
				}
			}
			if !inDoubleQuote && !inSingleQuote {
				if ch == '"' {
					inDoubleQuote = true
				} else if ch == '\'' {
					inSingleQuote = true
				} else if ch == ']' {
					tagEnd = i + 1
					break
				}
			} else if inDoubleQuote {
				if ch == '"' {
					inDoubleQuote = false
				}
			} else if inSingleQuote {
				if ch == '\'' {
					inSingleQuote = false
				}
			}
		}

		if tagEnd == -1 {
			cursor = headerEnd
			continue
		}

		rawTag := text[tagStart:tagEnd]
		rawAttrs := text[headerEnd : tagEnd-1]
		spans = append(spans, rawTagSpan{
			start:    tagStart,
			end:      tagEnd,
			typeStr:  typeStr,
			rawAttrs: rawAttrs,
			rawTag:   rawTag,
		})
		cursor = tagEnd
	}
	return spans
}

func parseKeyedAttributes(rawAttrs string) map[string]string {
	attrs := make(map[string]string)
	i := 0
	n := len(rawAttrs)

	for i < n {
		for i < n && (rawAttrs[i] == ' ' || rawAttrs[i] == '\t' || rawAttrs[i] == '\n' || rawAttrs[i] == '\r') {
			i++
		}
		if i >= n {
			break
		}

		keyStart := i
		for i < n && (rawAttrs[i] >= 'a' && rawAttrs[i] <= 'z' ||
			rawAttrs[i] >= 'A' && rawAttrs[i] <= 'Z' ||
			rawAttrs[i] >= '0' && rawAttrs[i] <= '9' ||
			rawAttrs[i] == '_' || rawAttrs[i] == '-') {
			i++
		}
		if keyStart == i {
			i++
			continue
		}
		key := strings.ToLower(rawAttrs[keyStart:i])

		for i < n && (rawAttrs[i] == ' ' || rawAttrs[i] == '\t' || rawAttrs[i] == '\n' || rawAttrs[i] == '\r') {
			i++
		}
		if i >= n || rawAttrs[i] != '=' {
			continue
		}
		i++ // skip '='

		for i < n && (rawAttrs[i] == ' ' || rawAttrs[i] == '\t' || rawAttrs[i] == '\n' || rawAttrs[i] == '\r') {
			i++
		}
		if i >= n {
			attrs[key] = ""
			break
		}

		var val string
		if rawAttrs[i] == '"' || rawAttrs[i] == '\'' {
			quoteChar := rawAttrs[i]
			i++ // skip opening quote
			valStart := i
			for i < n {
				if rawAttrs[i] == '\\' && i+1 < n {
					next := rawAttrs[i+1]
					if next == quoteChar || next == '\\' {
						i += 2
						continue
					}
				}
				if rawAttrs[i] == quoteChar {
					break
				}
				i++
			}
			rawVal := rawAttrs[valStart:i]
			if i < n && rawAttrs[i] == quoteChar {
				i++ // skip closing quote
			}
			val = rawVal
		} else {
			valStart := i
			for i < n {
				rest := rawAttrs[i:]
				if len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r') {
					if recognizedKeyRegex.MatchString(rest) {
						break
					}
				}
				i++
			}
			val = strings.TrimSpace(rawAttrs[valStart:i])
		}

		attrs[key] = val
	}

	return attrs
}

func unescapeAttributeValue(s string, quoteChar byte) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	n := len(s)
	for i := 0; i < n; i++ {
		if s[i] == '\\' && i+1 < n {
			next := s[i+1]
			if next == quoteChar || next == '\\' || next == '"' || next == '\'' {
				sb.WriteByte(next)
				i++
				continue
			} else if next == 'n' {
				sb.WriteByte('\n')
				i++
				continue
			} else if next == 't' {
				sb.WriteByte('\t')
				i++
				continue
			} else if next == 'r' {
				sb.WriteByte('\r')
				i++
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// ParseActionIntent extracts the first valid action intent tag from the given text.
func ParseActionIntent(text string) *ActionIntent {
	intents := ParseAllActionIntents(text)
	if len(intents) == 0 {
		return nil
	}
	return intents[0]
}

// ParseAllActionIntents extracts all valid action intent tags found within the text.
func ParseAllActionIntents(text string) []*ActionIntent {
	if text == "" {
		return nil
	}

	spans := findActionTagSpans(text)
	if len(spans) == 0 {
		return nil
	}

	var results []*ActionIntent
	for _, sp := range spans {
		typeStr := strings.ToUpper(strings.TrimSpace(sp.typeStr))
		var actionType ActionType
		switch typeStr {
		case "BUILD", "RUN", "EXEC", "COMMAND", "SHELL", "TEST":
			actionType = ActionBuild
		case "MAP":
			actionType = ActionMap
		case "READ", "CAT", "VIEW", "OPEN":
			actionType = ActionRead
		case "SEARCH", "RECALL":
			actionType = ActionSearch
		case "HEAL":
			actionType = ActionHeal
		case "WRITE":
			actionType = ActionWrite
		case "CREATE":
			actionType = ActionCreate
		case "PATCH":
			actionType = ActionPatch
		case "EDIT":
			actionType = ActionEdit
		default:
			actionType = ActionType(typeStr)
		}

		attrs := make(map[string]string)
		payload := ""

		if !recognizedKeyRegex.MatchString(sp.rawAttrs) {
			// Unkeyed tag: entire attribute string is the payload
			trimmed := strings.TrimSpace(sp.rawAttrs)
			if trimmed != "" {
				payload = trimmed
				switch actionType {
				case ActionMap, ActionSearch:
					attrs["query"] = payload
				case ActionBuild:
					attrs["task"] = payload
				case ActionHeal:
					attrs["cmd"] = payload
				case ActionWrite, ActionCreate, ActionPatch, ActionEdit, ActionRead:
					attrs["path"] = payload
				}
			}
		} else {
			attrs = parseKeyedAttributes(sp.rawAttrs)
			switch actionType {
			case ActionBuild:
				if t, ok := attrs["task"]; ok && t != "" {
					payload = t
				} else if c, ok := attrs["cmd"]; ok && c != "" {
					payload = c
				} else if c, ok := attrs["command"]; ok && c != "" {
					payload = c
				} else if r, ok := attrs["run"]; ok && r != "" {
					payload = r
				} else if e, ok := attrs["exec"]; ok && e != "" {
					payload = e
				} else if d, ok := attrs["description"]; ok && d != "" {
					payload = d
				}
			case ActionMap, ActionSearch:
				if q, ok := attrs["query"]; ok && q != "" {
					payload = q
				} else if p, ok := attrs["path"]; ok && p != "" {
					payload = p
				}
			case ActionHeal:
				if c, ok := attrs["cmd"]; ok && c != "" {
					payload = c
				} else if c, ok := attrs["command"]; ok && c != "" {
					payload = c
				}
			case ActionWrite, ActionCreate, ActionPatch, ActionEdit, ActionRead:
				if p, ok := attrs["path"]; ok && p != "" {
					payload = p
				} else if f, ok := attrs["file"]; ok && f != "" {
					payload = f
				} else if t, ok := attrs["target"]; ok && t != "" {
					payload = t
				} else if n, ok := attrs["name"]; ok && n != "" {
					payload = n
				}
			}

			if payload == "" {
				for _, v := range attrs {
					if strings.TrimSpace(v) != "" {
						payload = v
						break
					}
				}
			}
		}

		// Empty Tag Guard: ignore tags where the resulting action payload is empty
		if strings.TrimSpace(payload) == "" {
			continue
		}

		results = append(results, &ActionIntent{
			Type:       actionType,
			RawTag:     sp.rawTag,
			Attributes: attrs,
			Payload:    payload,
			StartPos:   sp.start,
			EndPos:     sp.end,
		})
	}

	return results
}

// StripActionTags removes all structured [ACTION:...] and [/ACTION:...] tags from the input string.
func StripActionTags(text string) string {
	text = actionCloseRegex.ReplaceAllString(text, "")
	spans := findActionTagSpans(text)
	if len(spans) == 0 {
		return strings.TrimSpace(text)
	}

	b := []byte(text)
	for i := len(spans) - 1; i >= 0; i-- {
		sp := spans[i]
		start := sp.start
		end := sp.end

		if start > 0 && end < len(b) && (b[start-1] == ' ' || b[start-1] == '\t') && (b[end] == ' ' || b[end] == '\t') {
			end++
		} else if start == 0 && end < len(b) && (b[end] == ' ' || b[end] == '\t') {
			end++
		} else if end == len(b) && start > 0 && (b[start-1] == ' ' || b[start-1] == '\t') {
			start--
		}

		b = append(b[:start], b[end:]...)
	}

	return strings.TrimSpace(string(b))
}

// isMalformedTagContent returns true if the content begins with an action tag or CLI status tag.
func isMalformedTagContent(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "[ACTION:") ||
		strings.HasPrefix(trimmed, "[action:") ||
		strings.HasPrefix(trimmed, "[/ACTION") ||
		strings.HasPrefix(trimmed, "[/action") ||
		strings.HasPrefix(trimmed, "[File Created]") ||
		strings.HasPrefix(trimmed, "[Build]") ||
		strings.HasPrefix(trimmed, "[Auto-Write") ||
		strings.HasPrefix(trimmed, "[Auto-Engine") ||
		strings.HasPrefix(trimmed, "[Auto-Next") ||
		strings.HasPrefix(trimmed, "[Verification") {
		return true
	}
	if strings.HasPrefix(trimmed, "[") {
		if loc := actionStartRegex.FindStringIndex(trimmed); loc != nil && loc[0] == 0 {
			return true
		}
		if loc := actionCloseRegex.FindStringIndex(trimmed); loc != nil && loc[0] == 0 {
			return true
		}
	}
	return false
}

// sanitizeExtractedContent cleans extracted deliverable content by stripping leaked action tags
// and ensuring that raw action directives are never returned as file payloads.
func sanitizeExtractedContent(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	if isMalformedTagContent(trimmed) {
		return ""
	}

	// HTML boundary guard: If content contains closing </html>, truncate trailing commentary after </html>
	lowerContent := strings.ToLower(content)
	if htmlIdx := strings.LastIndex(lowerContent, "</html>"); htmlIdx != -1 {
		after := strings.TrimSpace(content[htmlIdx+len("</html>"):])
		if after != "" {
			content = content[:htmlIdx+len("</html>")] + "\n"
		}
	}

	// JSON boundary guard: If content starts with { or [, find balanced closing bracket with valid JSON
	trimmedStart := strings.TrimLeft(content, " \t\r\n")
	if strings.HasPrefix(trimmedStart, "{") || strings.HasPrefix(trimmedStart, "[") {
		startChar := trimmedStart[0]
		closeChar := byte('}')
		if startChar == '[' {
			closeChar = byte(']')
		}
		inStr := false
		escaped := false
		depth := 0
		foundCloseIdx := -1
		for i := 0; i < len(content); i++ {
			c := content[i]
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' && inStr {
				escaped = true
				continue
			}
			if c == '"' {
				inStr = !inStr
				continue
			}
			if inStr {
				continue
			}
			if c == startChar {
				depth++
			} else if c == closeChar {
				depth--
				if depth == 0 {
					foundCloseIdx = i
					break
				}
			}
		}
		if foundCloseIdx != -1 && foundCloseIdx < len(content)-1 {
			tail := strings.TrimSpace(content[foundCloseIdx+1:])
			if tail != "" {
				candidate := strings.TrimSpace(content[:foundCloseIdx+1])
				if json.Valid([]byte(candidate)) {
					content = candidate + "\n"
				}
			}
		}
	}

	// If content contains a standalone action directive line (e.g. leaked from an unclosed code block)
	// or a closing tag directive line, truncate the content right before the leaked action directive.
	lines := strings.Split(content, "\n")
	truncateIdx := -1
	for idx, line := range lines {
		tLine := strings.TrimSpace(line)
		if strings.HasPrefix(tLine, "[") && (actionStartRegex.MatchString(tLine) || actionCloseRegex.MatchString(tLine)) &&
			!strings.HasPrefix(tLine, "//") && !strings.HasPrefix(tLine, "/*") && !strings.HasPrefix(tLine, "*") {
			truncateIdx = idx
			break
		}
		// Also detect conversational prompt/commentary transitions if outside code comments
		if !strings.HasPrefix(tLine, "//") && !strings.HasPrefix(tLine, "/*") && !strings.HasPrefix(tLine, "*") && !strings.HasPrefix(tLine, "#") {
			lower := strings.ToLower(tLine)
			if strings.HasPrefix(lower, "now let's run ") ||
				strings.HasPrefix(lower, "let's run the test") ||
				strings.HasPrefix(lower, "let's run the build") ||
				strings.HasPrefix(lower, "let's run ") ||
				strings.HasPrefix(lower, "now let's test ") ||
				strings.HasPrefix(lower, "let's test ") ||
				strings.HasPrefix(lower, "now let's create ") ||
				strings.HasPrefix(lower, "next, let's create ") ||
				strings.HasPrefix(lower, "now, let's create ") ||
				strings.HasPrefix(lower, "to run ") {
				truncateIdx = idx
				break
			}
		}
	}
	if truncateIdx != -1 {
		lines = lines[:truncateIdx]
		content = strings.Join(lines, "\n")
		content = strings.TrimRight(content, "\r\n \t")
		if content == "" {
			return ""
		}
		return content + "\n"
	}
	// Strip trailing end boundary markers:
	// Many models end the turn or file with '>>>>', '<<<<', or 'DONE'.
	// Aider search/replace payloads legitimately END with '>>>>>>> REPLACE'; stripping it
	// leaves the edit block unclosed and every [ACTION:PATCH] fails with PARSE_ERROR.
	isEditBlockPayload := containsAiderEditBlock(content)
	cleanedLines := strings.Split(content, "\n")
	for len(cleanedLines) > 0 {
		last := strings.TrimSpace(cleanedLines[len(cleanedLines)-1])
		if last == "" {
			cleanedLines = cleanedLines[:len(cleanedLines)-1]
			continue
		}
		if isEditBlockPayload && aiderReplaceTerminatorRegex.MatchString(last) {
			break
		}
		if strings.HasPrefix(last, ">>>>") || strings.HasPrefix(last, "<<<<") || last == "DONE" {
			cleanedLines = cleanedLines[:len(cleanedLines)-1]
			continue
		}
		break
	}
	content = strings.Join(cleanedLines, "\n")
	if strings.TrimSpace(content) == "" {
		return ""
	}
	content = strings.TrimRight(content, "\r\n \t") + "\n"

	// Guard: if content has no actual content other than fence delimiters (e.g. ```bash)
	hasNonFence := false
	for _, l := range strings.Split(trimmed, "\n") {
		tl := strings.TrimSpace(l)
		if tl != "" && !strings.HasPrefix(tl, "```") && !strings.HasPrefix(tl, "~~~") {
			hasNonFence = true
			break
		}
	}
	if !hasNonFence {
		return ""
	}
	return content
}

// ExtractContent extracts deliverable content from an assistant message,
// unwrapping outermost markdown code fences and stripping structured action tags.
func ExtractContent(replyText string) string {
	stripped := StripActionTags(replyText)
	trimmed := strings.TrimSpace(stripped)
	if trimmed == "" {
		return ""
	}

	getFenceLen := func(line string) int {
		t := strings.TrimSpace(line)
		if len(t) < 3 || t[0] != '`' {
			return 0
		}
		count := 0
		for count < len(t) && t[count] == '`' {
			count++
		}
		if count >= 3 {
			return count
		}
		return 0
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) == 0 {
		return ""
	}

	// 1. Direct outer enclosure check:
	// If first line starts with >=3 backticks:
	firstFenceLen := getFenceLen(lines[0])
	if firstFenceLen >= 3 {
		lastLine := strings.TrimSpace(lines[len(lines)-1])
		lastFenceLen := getFenceLen(lastLine)
		if lastFenceLen >= 3 && strings.Trim(lastLine, "` ") == "" {
			if len(lines) >= 2 {
				inner := strings.Join(lines[1:len(lines)-1], "\n")
				return sanitizeExtractedContent(strings.Trim(inner, "\r\n") + "\n")
			}
		}
	}

	// 2. Preambled enclosure check:
	// Find the first line that is an opening fence:
	openIdx := -1
	for i, l := range lines {
		fLen := getFenceLen(l)
		if fLen >= 3 {
			openIdx = i
			break
		}
	}

	if openIdx != -1 {
		// Check if any line before openIdx has a markdown header (#)
		hasHeadingBefore := false
		for i := 0; i < openIdx; i++ {
			tl := strings.TrimSpace(lines[i])
			if strings.HasPrefix(tl, "#") {
				hasHeadingBefore = true
				break
			}
		}

		if !hasHeadingBefore {
			// Find the matching or closing fence from the end
			closeIdx := -1
			for i := len(lines) - 1; i > openIdx; i-- {
				tl := strings.TrimSpace(lines[i])
				fLen := getFenceLen(tl)
				if fLen >= 3 && strings.Trim(tl, "` ") == "" {
					closeIdx = i
					break
				}
			}

			if closeIdx > openIdx {
				inner := strings.Join(lines[openIdx+1:closeIdx], "\n")
				return sanitizeExtractedContent(strings.Trim(inner, "\r\n") + "\n")
			}

			// Unclosed fence: LLM opened a fence (e.g. ```python) but omitted closing fence or ended with >>>>
			if closeIdx == -1 && openIdx < len(lines)-1 {
				inner := strings.Join(lines[openIdx+1:], "\n")
				return sanitizeExtractedContent(strings.Trim(inner, "\r\n") + "\n")
			}
		}
	}

	return sanitizeExtractedContent(trimmed + "\n")
}

type codeBlockSpan struct {
	startByte  int
	endByte    int
	fenceChar  byte
	fenceLen   int
	lang       string
	content    string
	innerStart int
	innerEnd   int
}

func getFenceInfo(line string) (bool, byte, int, string) {
	trimmed := strings.TrimRight(line, "\r")
	spaces := 0
	for spaces < len(trimmed) && spaces < 3 && trimmed[spaces] == ' ' {
		spaces++
	}
	trimmed = trimmed[spaces:]

	if len(trimmed) < 3 {
		return false, 0, 0, ""
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return false, 0, 0, ""
	}
	count := 0
	for count < len(trimmed) && trimmed[count] == ch {
		count++
	}
	if count < 3 {
		return false, 0, 0, ""
	}
	rest := strings.TrimSpace(trimmed[count:])
	return true, ch, count, rest
}

func isClosingFence(line string, fenceChar byte, minLen int) bool {
	trimmed := strings.TrimRight(line, "\r")
	spaces := 0
	for spaces < len(trimmed) && spaces < 3 && trimmed[spaces] == ' ' {
		spaces++
	}
	trimmed = trimmed[spaces:]

	if len(trimmed) < minLen {
		return false
	}
	if trimmed[0] != fenceChar {
		return false
	}
	count := 0
	for count < len(trimmed) && trimmed[count] == fenceChar {
		count++
	}
	if count < minLen {
		return false
	}
	rest := strings.TrimSpace(trimmed[count:])
	return rest == ""
}

func hasClosingFenceAhead(text string, fenceChar byte, minLen int) bool {
	offset := 0
	n := len(text)
	for offset < n {
		lineEnd := strings.IndexByte(text[offset:], '\n')
		var line string
		var nextOffset int
		if lineEnd == -1 {
			line = text[offset:]
			nextOffset = n
		} else {
			line = text[offset : offset+lineEnd]
			nextOffset = offset + lineEnd + 1
		}
		if isClosingFence(line, fenceChar, minLen) {
			return true
		}
		offset = nextOffset
	}
	return false
}

func findOutermostCodeBlocks(text string) []codeBlockSpan {
	var blocks []codeBlockSpan
	n := len(text)
	offset := 0

	for offset < n {
		lineEnd := strings.IndexByte(text[offset:], '\n')
		var line string
		var nextOffset int
		if lineEnd == -1 {
			line = text[offset:]
			nextOffset = n
		} else {
			line = text[offset : offset+lineEnd]
			nextOffset = offset + lineEnd + 1
		}

		isFence, fChar, fLen, lang := getFenceInfo(line)
		if isFence {
			blockStartByte := offset
			innerStartByte := nextOffset

			curr := nextOffset
			closingFound := false
			stoppedEarly := false
			innerEndByte := n
			blockEndByte := n

			for curr < n {
				cLineEnd := strings.IndexByte(text[curr:], '\n')
				var cLine string
				var cNextOffset int
				if cLineEnd == -1 {
					cLine = text[curr:]
					cNextOffset = n
				} else {
					cLine = text[curr : curr+cLineEnd]
					cNextOffset = curr + cLineEnd + 1
				}

				if isClosingFence(cLine, fChar, fLen) {
					innerEndByte = curr
					blockEndByte = cNextOffset
					closingFound = true
					offset = cNextOffset
					break
				}
				// Stop unclosed fence early if an action tag is encountered on cLine
				trimmedLine := strings.TrimSpace(cLine)
				if (strings.HasPrefix(trimmedLine, "[ACTION:") || (actionStartRegex.MatchString(trimmedLine) && strings.HasPrefix(trimmedLine, "["))) &&
					!strings.HasPrefix(trimmedLine, "//") && !strings.HasPrefix(trimmedLine, "/*") {
					if !hasClosingFenceAhead(text[cNextOffset:], fChar, fLen) {
						innerEndByte = curr
						blockEndByte = curr
						stoppedEarly = true
						offset = curr
						break
					}
				}
				curr = cNextOffset
			}

			if !closingFound && !stoppedEarly {
				innerEndByte = n
				blockEndByte = n
				offset = n
			}

			rawInner := text[innerStartByte:innerEndByte]
			cleanInner := strings.Trim(rawInner, "\r\n") + "\n"

			blocks = append(blocks, codeBlockSpan{
				startByte:  blockStartByte,
				endByte:    blockEndByte,
				fenceChar:  fChar,
				fenceLen:   fLen,
				lang:       lang,
				content:    cleanInner,
				innerStart: innerStartByte,
				innerEnd:   innerEndByte,
			})
			continue
		}

		offset = nextOffset
	}

	return blocks
}

// ExtractContentForIntent extracts deliverable content from replyText specifically
// targeted to the given action intent, cleanly isolating its designated code block.
func ExtractContentForIntent(replyText string, intent *ActionIntent) string {
	if strings.TrimSpace(replyText) == "" {
		return ""
	}

	allIntents := ParseAllActionIntents(replyText)

	// If no intents found, or intent is nil, fallback to global ExtractContent
	if len(allIntents) == 0 || intent == nil {
		return sanitizeExtractedContent(ExtractContent(replyText))
	}

	targetPath := strings.ToLower(strings.TrimSpace(intent.Path()))
	isMarkdownFile := strings.HasSuffix(targetPath, ".md") || strings.HasSuffix(targetPath, ".markdown")

	// Single intent case:
	if len(allIntents) == 1 {
		// If it's a markdown deliverable or path is empty, check if it's a full document
		if isMarkdownFile || targetPath == "" {
			return sanitizeExtractedContent(ExtractContent(replyText))
		}
		// PATCH intents: the SEARCH/REPLACE body right after the tag wins over any code fence elsewhere
		// in the prose (an earlier ```python snippet must not be mistaken for the patch payload).
		if intent.IsPatch() {
			if sec := patchSectionAfter(replyText, allIntents[0].EndPos, len(replyText)); sec != "" {
				return sec
			}
		}
		// If it's a code/config file (e.g. main.go, go.mod), extract its specific
		// code block rather than conversational markdown with headings.
		blocks := findOutermostCodeBlocks(replyText)
		if len(blocks) > 0 {
			sp := allIntents[0]
			var closeLimit = len(replyText)
			if closeLoc := actionCloseRegex.FindStringIndex(replyText[sp.EndPos:]); closeLoc != nil {
				closeLimit = sp.EndPos + closeLoc[0]
			}

			var bestBlock *codeBlockSpan
			bestDist := int(^uint(0) >> 1)

			for i := range blocks {
				b := &blocks[i]
				if isMalformedTagContent(b.content) {
					continue
				}
				if closeLimit < len(replyText) && b.startByte >= closeLimit {
					continue
				}
				// Affinity guard: don't match blocks that have incompatible language (e.g. bash commands for html/go)
				if blockMatchScore(b, targetPath) < 0 {
					continue
				}
				var dist int
				if b.startByte >= sp.EndPos {
					dist = b.startByte - sp.EndPos
				} else if b.endByte <= sp.StartPos {
					dist = sp.StartPos - b.endByte
				} else {
					dist = 0
				}
				if dist < bestDist {
					bestDist = dist
					bestBlock = b
				}
			}
			if bestBlock != nil {
				picked := sanitizeExtractedContent(bestBlock.content)
				zone := replyText[sp.EndPos:]
				if !intent.IsPatch() && !strings.Contains(zone, "<<<<<<< SEARCH") && !strings.Contains(picked, "<<<<<<< SEARCH") {
					if alt := goSourceInZone(zone, targetPath); alt != "" && (len(picked) < 80 || !strings.Contains(picked, "package ")) {
						return alt
					}
				}
				if filepath.Base(targetPath) == "go.mod" {
					picked = cleanGoModContent(picked)
				}
				return picked
			}
		}
		if !intent.IsPatch() && !strings.Contains(replyText, "<<<<<<< SEARCH") {
			if alt := goSourceInZone(replyText[allIntents[0].EndPos:], targetPath); alt != "" {
				return alt
			}
		}
		if closeLoc := actionCloseRegex.FindStringIndex(replyText[allIntents[0].EndPos:]); closeLoc != nil {
			rawSection := replyText[allIntents[0].EndPos : allIntents[0].EndPos+closeLoc[0]]
			res := sanitizeExtractedContent(StripActionTags(rawSection) + "\n")
			if filepath.Base(targetPath) == "go.mod" {
				return cleanGoModContent(res)
			}
			return res
		}
		if allIntents[0].EndPos < len(replyText) {
			rawSection := replyText[allIntents[0].EndPos:]
			res := sanitizeExtractedContent(StripActionTags(rawSection) + "\n")
			if filepath.Base(targetPath) == "go.mod" {
				cleaned := cleanGoModContent(res)
				if strings.TrimSpace(cleaned) != "" {
					return cleaned
				}
			} else {
				extracted := sanitizeExtractedContent(ExtractContent(rawSection))
				if strings.TrimSpace(extracted) != "" {
					return extracted
				}
			}
		}
		rawExtracted := sanitizeExtractedContent(ExtractContent(replyText))
		if filepath.Base(targetPath) == "go.mod" {
			return cleanGoModContent(rawExtracted)
		}
		return rawExtracted
	}

	// Multi-intent case:
	// Find index of this intent within allIntents
	intentIdx := -1
	// Priority 1: Match by exact byte span (StartPos and EndPos)
	if intent.EndPos > 0 {
		for i, it := range allIntents {
			if it.StartPos == intent.StartPos && it.EndPos == intent.EndPos {
				intentIdx = i
				break
			}
		}
	}
	// Priority 2: Match by exact start offset if EndPos was not set
	if intentIdx == -1 && intent.StartPos > 0 {
		for i, it := range allIntents {
			if it.StartPos == intent.StartPos {
				intentIdx = i
				break
			}
		}
	}
	// Priority 3: Fallback match by exact raw tag and path (e.g. manually constructed intents)
	if intentIdx == -1 {
		for i, it := range allIntents {
			if intent.RawTag != "" && it.RawTag == intent.RawTag && it.Path() == intent.Path() {
				intentIdx = i
				break
			}
		}
	}
	// Priority 4: Fallback match by target path
	if intentIdx == -1 {
		for i, it := range allIntents {
			if it.Path() == intent.Path() {
				intentIdx = i
				break
			}
		}
	}
	if intentIdx == -1 {
		return sanitizeExtractedContent(ExtractContent(replyText))
	}

	currSp := allIntents[intentIdx]
	prevEnd := 0
	if intentIdx > 0 {
		prevEnd = allIntents[intentIdx-1].EndPos
	}
	nextStart := len(replyText)
	if intentIdx+1 < len(allIntents) {
		nextStart = allIntents[intentIdx+1].StartPos
	}
	if intent.IsPatch() {
		if sec := patchSectionAfter(replyText, currSp.EndPos, nextStart); sec != "" {
			return sec
		}
	}

	// Extract raw section text between tag and next tag / prev tag, clamping at any closing tag
	var rawSection string
	if currSp.EndPos < nextStart {
		zone := replyText[currSp.EndPos:nextStart]
		if closeLoc := actionCloseRegex.FindStringIndex(zone); closeLoc != nil {
			rawSection = zone[:closeLoc[0]]
		} else {
			rawSection = zone
		}
	} else if prevEnd < currSp.StartPos {
		zone := replyText[prevEnd:currSp.StartPos]
		if closeLoc := actionCloseRegex.FindStringIndex(zone); closeLoc != nil {
			rawSection = zone[:closeLoc[0]]
		} else {
			rawSection = zone
		}
	} else {
		rawSection = replyText
	}

	// For markdown deliverables, the entire raw section IS the document (may contain internal code fences)
	if isMarkdownFile {
		mdContent := sanitizeExtractedContent(ExtractContent(rawSection))
		if strings.TrimSpace(mdContent) != "" {
			return mdContent
		}
	}

	blocks := findOutermostCodeBlocks(replyText)

	// Look for candidate code blocks in [prevEnd, nextStart]
	var forwardBlock *codeBlockSpan
	var backwardBlock *codeBlockSpan

	for i := range blocks {
		b := &blocks[i]
		if isMalformedTagContent(b.content) {
			continue
		}
		if blockMatchScore(b, targetPath) < 0 {
			continue
		}
		if b.startByte >= currSp.EndPos && b.startByte < nextStart {
			if forwardBlock == nil || b.startByte < forwardBlock.startByte {
				forwardBlock = b
			}
		}
		if b.endByte <= currSp.StartPos && b.endByte > prevEnd {
			if backwardBlock == nil || b.endByte > backwardBlock.endByte {
				backwardBlock = b
			}
		}
	}

	hasUnfencedCodeAhead := false
	if currSp.EndPos < nextStart {
		aheadText := replyText[currSp.EndPos:nextStart]
		if !intent.IsPatch() && strings.HasSuffix(targetPath, ".go") && strings.Contains(aheadText, "package ") {
			hasUnfencedCodeAhead = true
		}
	}

	var selectedBlock *codeBlockSpan
	if forwardBlock != nil && backwardBlock != nil {
		fScore := blockMatchScore(forwardBlock, targetPath)
		bScore := blockMatchScore(backwardBlock, targetPath)
		if bScore > fScore && bScore > 0 {
			selectedBlock = backwardBlock
		} else if fScore > 0 {
			selectedBlock = forwardBlock
		}
	} else if forwardBlock != nil && blockMatchScore(forwardBlock, targetPath) > 0 {
		selectedBlock = forwardBlock
	} else if backwardBlock != nil && blockMatchScore(backwardBlock, targetPath) > 0 {
		// Only select backwardBlock if there is no unfenced code ahead and the block matches this target
		if !hasUnfencedCodeAhead {
			selectedBlock = backwardBlock
		}
	}

	if selectedBlock != nil {
		picked := sanitizeExtractedContent(selectedBlock.content)
		if !intent.IsPatch() && !strings.Contains(replyText[currSp.EndPos:nextStart], "<<<<<<< SEARCH") && !strings.Contains(picked, "<<<<<<< SEARCH") {
			if alt := goSourceInZone(replyText[currSp.EndPos:nextStart], targetPath); alt != "" && (len(picked) < 80 || !strings.Contains(picked, "package ")) {
				return alt
			}
		}
		if filepath.Base(targetPath) == "go.mod" {
			picked = cleanGoModContent(picked)
		}
		return picked
	}

	stripped := StripActionTags(rawSection)
	lines := strings.Split(stripped, "\n")
	var cleanLines []string
	for _, l := range lines {
		tl := strings.TrimSpace(l)
		if tl == "---" || tl == "***" || tl == "___" {
			continue
		}
		if strings.HasPrefix(tl, "```") || strings.HasPrefix(tl, "~~~") {
			continue
		}
		cleanLines = append(cleanLines, l)
	}
	res := strings.Trim(strings.Join(cleanLines, "\n"), "\r\n")
	if res == "" {
		return ""
	}
	if filepath.Base(targetPath) == "go.mod" {
		return cleanGoModContent(res)
	}
	if !intent.IsPatch() && !strings.Contains(res, "<<<<<<< SEARCH") {
		if alt := goSourceInZone(res, targetPath); alt != "" {
			return alt
		}
	}
	return sanitizeExtractedContent(res + "\n")
}

// blockMatchScore returns a semantic affinity score between a code block and a target file path.
func blockMatchScore(b *codeBlockSpan, path string) int {
	if b == nil {
		return -100
	}
	cleanPath := strings.ToLower(filepath.ToSlash(strings.TrimSpace(path)))
	ext := filepath.Ext(cleanPath)
	score := 0

	lang := strings.ToLower(strings.TrimSpace(b.lang))
	content := b.content

	if ext != ".sh" && ext != ".bash" && (lang == "bash" || lang == "sh" || lang == "shell") {
		score -= 50
	}

	switch ext {
	case ".go":
		if strings.Contains(content, "package ") {
			score += 20
		}
		if lang == "go" || lang == "golang" {
			score += 10
		}
		if strings.HasPrefix(strings.TrimSpace(content), "module ") {
			score -= 30 // go.mod, not a .go file
		}
		if lang == "bash" || lang == "sh" || lang == "shell" {
			score -= 50
		}
	case ".mod":
		if strings.Contains(content, "module ") {
			score += 30
		}
		if lang == "go" || lang == "golang" || lang == "" {
			score += 5
		}
	case ".py":
		if lang == "python" || lang == "py" {
			score += 30
		}
		if strings.Contains(content, "def ") || strings.Contains(content, "import ") || strings.Contains(content, "class ") {
			score += 15
		}
		if lang == "bash" || lang == "sh" || lang == "shell" {
			score -= 50
		}
	case ".rs":
		if lang == "rust" || lang == "rs" {
			score += 30
		}
		if strings.Contains(content, "fn ") || strings.Contains(content, "use ") || strings.Contains(content, "struct ") || strings.Contains(content, "impl ") {
			score += 15
		}
		if lang == "bash" || lang == "sh" || lang == "shell" {
			score -= 50
		}
	case ".c", ".h":
		if lang == "c" {
			score += 30
		}
		if strings.Contains(content, "#include") || strings.Contains(content, "int main(") {
			score += 15
		}
	case ".cpp", ".hpp", ".cc", ".cxx":
		if lang == "cpp" || lang == "c++" || lang == "cc" || lang == "cxx" {
			score += 30
		}
		if strings.Contains(content, "#include") || strings.Contains(content, "std::") || strings.Contains(content, "class ") {
			score += 15
		}
	case ".java":
		if lang == "java" {
			score += 30
		}
		if strings.Contains(content, "public class") || strings.Contains(content, "package ") || strings.Contains(content, "import ") {
			score += 15
		}
	case ".cs":
		if lang == "csharp" || lang == "cs" || lang == "c#" {
			score += 30
		}
		if strings.Contains(content, "using System") || strings.Contains(content, "namespace ") {
			score += 15
		}
	case ".yml", ".yaml":
		if lang == "yaml" || lang == "yml" {
			score += 30
		}
		if strings.Contains(content, ":") {
			score += 10
		}
	case ".toml":
		if lang == "toml" {
			score += 30
		}
		if strings.Contains(content, "[") && strings.Contains(content, "]") {
			score += 10
		}
	case ".json":
		if lang == "json" {
			score += 30
		}
		trimmed := strings.TrimSpace(content)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			score += 15
		}
	case ".html", ".htm":
		if lang == "html" || strings.Contains(content, "<html") || strings.Contains(content, "<!DOCTYPE") || strings.Contains(content, "<div") {
			score += 30
		}
	case ".css":
		if lang == "css" || strings.Contains(content, "{") {
			score += 30
		}
	case ".js", ".ts", ".jsx", ".tsx":
		if lang == "js" || lang == "javascript" || lang == "ts" || lang == "typescript" || lang == "jsx" || lang == "tsx" {
			score += 30
		}
		if strings.Contains(content, "function ") || strings.Contains(content, "const ") || strings.Contains(content, "let ") || strings.Contains(content, "import ") {
			score += 15
		}
	case ".sql":
		if lang == "sql" {
			score += 30
		}
		upper := strings.ToUpper(content)
		if strings.Contains(upper, "SELECT ") || strings.Contains(upper, "CREATE TABLE") || strings.Contains(upper, "INSERT INTO") {
			score += 15
		}
	case ".sh", ".bash":
		if lang == "bash" || lang == "sh" || lang == "shell" {
			score += 30
		}
		if strings.HasPrefix(strings.TrimSpace(content), "#!/") {
			score += 15
		}
	case ".bat", ".cmd":
		if lang == "bat" || lang == "cmd" || lang == "batch" {
			score += 30
		}
		if strings.Contains(strings.ToLower(content), "@echo") {
			score += 15
		}
	case ".md", ".markdown":
		if lang == "markdown" || lang == "md" {
			score += 30
		}
	case ".xml":
		if lang == "xml" || strings.Contains(content, "<?xml") || strings.Contains(content, "<project") {
			score += 30
		}
	}

	base := filepath.Base(cleanPath)
	if base != "" && strings.Contains(strings.ToLower(content), base) {
		score += 5
	}
	// Neutral match: if no extension-specific score and lang matches extension or is blank, give base affinity
	if score == 0 {
		if lang == "" || (ext != "" && (lang == strings.TrimPrefix(ext, ".") || strings.HasSuffix(cleanPath, "."+lang))) {
			score += 5
		}
	}
	return score
}

// goSourceInZone returns the Go source in zone when path is a .go file and the
// body was not wrapped in a fence the block parser kept. Trailing prose after
// the last top-level closing brace is dropped.
func goSourceInZone(zone, path string) string {
	if !strings.HasSuffix(strings.ToLower(path), ".go") {
		return ""
	}
	if strings.Contains(zone, "<<<<<<< SEARCH") || strings.Contains(zone, ">>>>>>> REPLACE") {
		return ""
	}
	pkgRe := regexp.MustCompile(`(?m)^\s*package\s+[a-zA-Z0-9_]+`)
	loc := pkgRe.FindStringIndex(zone)
	if loc == nil {
		return ""
	}
	body := strings.TrimSpace(zone[loc[0]:])
	lines := strings.Split(body, "\n")
	lastBrace := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "}" {
			lastBrace = i
		}
	}
	if lastBrace < 0 {
		return ""
	}
	lines = lines[:lastBrace+1]
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	if !strings.HasPrefix(out, "package ") {
		return ""
	}
	return out + "\n"
}

// cleanGoModContent strips non-go.mod lines, leaked action tags, conflict markers,
// and conversational markdown from go.mod file content.
func cleanGoModContent(content string) string {
	lines := strings.Split(content, "\n")
	var validLines []string
	inBlock := false
	started := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if started && !inBlock {
				validLines = append(validLines, "")
			}
			continue
		}
		// Stop if an action tag or conflict marker appears after we have started
		if started && (strings.HasPrefix(trimmed, "[") || conflictMarkerRegex.MatchString(trimmed)) {
			break
		}
		if inBlock {
			if trimmed == ")" {
				inBlock = false
				validLines = append(validLines, line)
				continue
			}
			// inside require/replace/exclude/retract block
			validLines = append(validLines, line)
			continue
		}
		// Comments are valid go.mod lines
		if strings.HasPrefix(trimmed, "//") {
			validLines = append(validLines, line)
			continue
		}
		if strings.HasPrefix(trimmed, "module ") || strings.HasPrefix(trimmed, "go ") {
			started = true
			validLines = append(validLines, line)
			continue
		}
		if strings.HasPrefix(trimmed, "require ") || strings.HasPrefix(trimmed, "replace ") ||
			strings.HasPrefix(trimmed, "exclude ") || strings.HasPrefix(trimmed, "retract ") {
			started = true
			if strings.HasSuffix(trimmed, "(") {
				inBlock = true
			}
			validLines = append(validLines, line)
			continue
		}
		// If we already started reading go.mod directives, an unrecognized root line indicates
		// trailing prose or explanation from the LLM -> stop here!
		if started {
			break
		}
		// If we haven't started yet, this line is leading conversational prose.
		// Skip leading prose until the go.mod content begins!
		continue
	}
	res := strings.TrimSpace(strings.Join(validLines, "\n"))
	if res == "" {
		return ""
	}
	return res + "\n"
}

// SessionConfig provides customization parameters for a conversational session.
type SessionConfig struct {
	Model         string
	SystemPrompt  string
	WorkspaceDir  string
	Temperature   *float64
	MaxTokens     int
	ICXClient     icx.ICXClient
	ActiveProject string
}

// SessionStatus reports the active metrics and configuration of a conversational session.
type SessionStatus struct {
	Model         string `json:"model"`
	WorkspaceDir  string `json:"workspace_dir"`
	TurnCount     int    `json:"turn_count"`
	HistoryLen    int    `json:"history_length"`
	HasEngine     bool   `json:"has_engine"`
	ActiveProject string `json:"active_project,omitempty"`
	LatticeFacts  int    `json:"lattice_facts,omitempty"`
	LatticeNodes  int    `json:"lattice_nodes,omitempty"`
}

// BuildRecord captures a command execution attempt and diagnostic outcome for dynamic reasoning.
type BuildRecord struct {
	Command    string    `json:"command"`
	ExitCode   int       `json:"exit_code"`
	Diagnostic string    `json:"diagnostic"`
	Timestamp  time.Time `json:"timestamp"`
}

// Session manages multi-turn dialogue, context history, system prompt calibration,
// 4D volumetric memory surfaces, and engine coordination for OmniCoder-ICX.
// SwarmHandlerFunc defines a function callback that executes a swarm plan.
type SwarmHandlerFunc func(ctx context.Context, planPath string, out io.Writer) error

type Session struct {
	llmClient        llm.LLMClient
	engineOrch       *engine.Orchestrator
	icxClient        icx.ICXClient
	workspaceDir     string
	model            string
	systemPrompt     string
	history          []llm.ChatMessage
	config           SessionConfig
	progressCallback func(event engine.TurnProgressEvent)
	activeProject    string
	writtenFiles     []string
	isHostTask       bool
	activePlanSpec   *engine.PlanSpec
	activePlanPath   string
	multiLineMode    bool
	unavailableTools map[string]bool
	recentBuilds     []BuildRecord
	quarantine       map[string]*QuarantinedPayload
	buildSession     *BuildSession
	vault            *storage.Vault
	swarmHandler     SwarmHandlerFunc
	mu               sync.RWMutex
}

// SetSwarmHandler registers a callback for delegating conversational swarm intents.
func (s *Session) SetSwarmHandler(h SwarmHandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.swarmHandler = h
}

// SwarmHandler returns the registered swarm intent callback if present.
func (s *Session) SwarmHandler() SwarmHandlerFunc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.swarmHandler
}

// ConversationalTurnRecord captures the execution outcome of an interactive conversational turn.
type ConversationalTurnRecord struct {
	SessionID    string   `json:"session_id"`
	TurnNumber   int      `json:"turn_number"`
	Prompt       string   `json:"prompt"`
	Timestamp    string   `json:"timestamp"`
	Success      bool     `json:"success"`
	Project      string   `json:"project,omitempty"`
	StepsTaken   int      `json:"steps_taken"`
	FilesTouched []string `json:"files_touched,omitempty"`
}

// BuildSession returns the active build session if present.
func (s *Session) BuildSession() *BuildSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buildSession
}

// SetBuildSession stores the active build session.
func (s *Session) SetBuildSession(b *BuildSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buildSession = b
}

// Vault returns the attached Vault or opens one lazily for the session's workspace.
func (s *Session) Vault() *storage.Vault {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vault != nil {
		return s.vault
	}
	if s.workspaceDir != "" {
		if v, err := storage.OpenVault(s.workspaceDir); err == nil {
			s.vault = v
			return s.vault
		}
	}
	return nil
}

// SetVault explicitly assigns a Vault to the session.
func (s *Session) SetVault(v *storage.Vault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vault = v
}

// Close gracefully flushes and closes any attached Vault on the session.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vault != nil {
		err := s.vault.Close()
		s.vault = nil
		return err
	}
	return nil
}

// NewSession creates a new conversational session with default prompt and model calibration.
func NewSession(llmClient llm.LLMClient, engineOrch *engine.Orchestrator, workspaceDir string) *Session {
	return NewSessionWithConfig(SessionConfig{
		WorkspaceDir: workspaceDir,
	}, llmClient, engineOrch)
}

// NewSessionWithICX creates a new conversational session wired directly to an ICXClient instance.
func NewSessionWithICX(llmClient llm.LLMClient, engineOrch *engine.Orchestrator, workspaceDir string, icxClient icx.ICXClient) *Session {
	return NewSessionWithConfig(SessionConfig{
		WorkspaceDir: workspaceDir,
		ICXClient:    icxClient,
	}, llmClient, engineOrch)
}

// NewSessionWithConfig creates a new conversational session with the specified configuration.
func NewSessionWithConfig(cfg SessionConfig, llmClient llm.LLMClient, engineOrch *engine.Orchestrator) *Session {
	if cfg.Model == "" {
		if llmClient != nil && llmClient.Config().Model != "" {
			cfg.Model = llmClient.Config().Model
		} else {
			cfg.Model = llm.DefaultModel
		}
	}

	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = DefaultSystemPrompt
	}

	if cfg.WorkspaceDir == "" {
		cfg.WorkspaceDir = "."
	}
	if absWs, err := filepath.Abs(cfg.WorkspaceDir); err == nil {
		cfg.WorkspaceDir = absWs
	}

	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 16384
	}

	icxClient := cfg.ICXClient
	if icxClient == nil && engineOrch != nil {
		icxClient = engineOrch.ICX()
	}
	if icxClient == nil {
		icxClient = icx.NewLocalLattice()
	}

	activeProj := cfg.ActiveProject
	if activeProj != "" {
		_, _ = icxClient.VarSet(context.Background(), icx.VarSetRequest{
			Key:   "swarm.active_project",
			Value: activeProj,
		})
	}

	return &Session{
		llmClient:        llmClient,
		engineOrch:       engineOrch,
		icxClient:        icxClient,
		workspaceDir:     cfg.WorkspaceDir,
		model:            cfg.Model,
		systemPrompt:     cfg.SystemPrompt,
		config:           cfg,
		activeProject:    activeProj,
		history:          make([]llm.ChatMessage, 0),
		writtenFiles:     make([]string, 0),
		unavailableTools: make(map[string]bool),
		recentBuilds:     make([]BuildRecord, 0),
		quarantine:       make(map[string]*QuarantinedPayload),
	}
}

// compactHistoricalAssistantMessage compresses large code blocks (>500 bytes) in past
// assistant messages to an ICX slot citation with rich symbol outline (<150 tokens) for outgoing LLM prompts.
// This prevents context breakdown and token exhaustion while preserving epistemic codebase awareness.
func compactHistoricalAssistantMessage(text string) string {
	if len(text) <= 2048 {
		return text
	}
	blocks := findOutermostCodeBlocks(text)
	if len(blocks) == 0 {
		return text
	}

	var sb strings.Builder
	offset := 0
	for _, b := range blocks {
		if b.startByte > offset {
			sb.WriteString(text[offset:b.startByte])
		}
		if len(b.content) > 500 {
			fenceStr := strings.Repeat(string(b.fenceChar), b.fenceLen)
			sha := canonical.SHA256Bytes([]byte(b.content))
			outline := extractCodeOutline(b.content, b.lang)
			if outline != "" {
				sb.WriteString(fmt.Sprintf("%s%s\n// [Code preserved: %d bytes, SHA-256: %s]\n%s\n%s\n",
					fenceStr, b.lang, len(b.content), sha[:16], outline, fenceStr))
			} else {
				sb.WriteString(fmt.Sprintf("%s%s\n[Code preserved: %d bytes, SHA-256: %s]\n%s\n",
					fenceStr, b.lang, len(b.content), sha[:16], fenceStr))
			}
		} else {
			sb.WriteString(text[b.startByte:b.endByte])
		}
		offset = b.endByte
	}
	if offset < len(text) {
		sb.WriteString(text[offset:])
	}
	return sb.String()
}

// compactHistoricalUserDiagnosticMessage compresses bulky historical user diagnostic logs,
// compiler dumps, and test traces (>1,024 bytes) in past turns older than the active turn window.
// It preserves the essential opening diagnostic lines (~25 lines) and summarizes the rest
// with a cryptographic hash and line count, saving thousands of tokens across multi-turn healing loops.
func compactHistoricalUserDiagnosticMessage(text string) string {
	if len(text) <= 1024 {
		return text
	}

	lower := strings.ToLower(text)
	isDiag := strings.Contains(lower, "compiler output") ||
		strings.Contains(lower, "verification test") ||
		strings.Contains(lower, "build task failed") ||
		strings.Contains(lower, "test failure") ||
		strings.Contains(lower, "execution failed") ||
		strings.Contains(lower, "healing attempt") ||
		strings.Contains(lower, "exit status") ||
		strings.Contains(lower, "exit code") ||
		strings.Contains(text, "--- FAIL:") ||
		strings.Contains(text, "FAIL\t") ||
		strings.Contains(lower, "undefined:")

	if !isDiag {
		return text
	}

	lines := strings.Split(text, "\n")
	if len(lines) <= 30 {
		return text
	}

	var sb strings.Builder
	for i := 0; i < 25; i++ {
		sb.WriteString(lines[i])
		sb.WriteString("\n")
	}

	truncatedLines := len(lines) - 25
	sha := canonical.SHA256Bytes([]byte(text))
	sb.WriteString(fmt.Sprintf("\n... [%d lines of historical diagnostic output truncated, SHA-256: %s] ...\n",
		truncatedLines, sha[:16]))

	return sb.String()
}

// extractCodeOutline extracts an ultra-concise declaration summary (packages, types, funcs)
// from code blocks to retain API contract visibility in compacted conversational turns.
func extractCodeOutline(content, lang string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}

	lowerLang := strings.ToLower(lang)
	isGo := lowerLang == "go" || strings.Contains(content, "package ")

	if isGo {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "", content, parser.AllErrors)
		if err == nil && file != nil {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("// Package: %s\n", file.Name.Name))

			var typeDecls []string
			var funcDecls []string

			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					if d.Tok == token.TYPE {
						for _, spec := range d.Specs {
							if ts, ok := spec.(*ast.TypeSpec); ok {
								kind := "type"
								switch ts.Type.(type) {
								case *ast.StructType:
									kind = "struct"
								case *ast.InterfaceType:
									kind = "interface"
								}
								typeDecls = append(typeDecls, fmt.Sprintf("//   type %s %s", ts.Name.Name, kind))
							}
						}
					}
				case *ast.FuncDecl:
					sig := formatFuncSig(d)
					if sig != "" {
						funcDecls = append(funcDecls, "//   "+sig)
					}
				}
			}

			if len(typeDecls) > 0 {
				sb.WriteString("// Types:\n")
				limit := 8
				if len(typeDecls) < limit {
					limit = len(typeDecls)
				}
				for i := 0; i < limit; i++ {
					sb.WriteString(typeDecls[i] + "\n")
				}
				if len(typeDecls) > limit {
					sb.WriteString(fmt.Sprintf("//   ... and %d more types\n", len(typeDecls)-limit))
				}
			}

			if len(funcDecls) > 0 {
				sb.WriteString("// Functions & Methods:\n")
				limit := 12
				if len(funcDecls) < limit {
					limit = len(funcDecls)
				}
				for i := 0; i < limit; i++ {
					sb.WriteString(funcDecls[i] + "\n")
				}
				if len(funcDecls) > limit {
					sb.WriteString(fmt.Sprintf("//   ... and %d more functions\n", len(funcDecls)-limit))
				}
			}

			return strings.TrimSpace(sb.String())
		}
	}

	// Fallback outline for non-Go or unparseable code
	lines := strings.Split(content, "\n")
	var outlineLines []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if isGo && (strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(trimmed, "type ") || strings.HasPrefix(trimmed, "package ")) {
			if brace := strings.Index(trimmed, "{"); brace != -1 {
				trimmed = strings.TrimSpace(trimmed[:brace])
			}
			outlineLines = append(outlineLines, "// "+trimmed)
		} else if !isGo {
			// Python decorators & route definitions
			if strings.HasPrefix(trimmed, "@app.") || strings.HasPrefix(trimmed, "@router.") || (strings.HasPrefix(trimmed, "@") && strings.Contains(trimmed, "(")) {
				outlineLines = append(outlineLines, "// "+trimmed)
				continue
			}
			// HTML interactive elements and event handlers
			if lowerLang == "html" || strings.Contains(content, "<html") || strings.Contains(content, "<!DOCTYPE") || strings.Contains(content, "<!doctype") {
				if (strings.Contains(trimmed, "onclick=") || strings.Contains(trimmed, "id=") || strings.Contains(trimmed, "<form")) && strings.HasPrefix(trimmed, "<") {
					tagSummary := trimmed
					if len(tagSummary) > 60 {
						tagSummary = tagSummary[:60] + "...>"
					}
					outlineLines = append(outlineLines, "<!-- "+tagSummary+" -->")
				}
			}
			if strings.HasPrefix(trimmed, "export ") || strings.HasPrefix(trimmed, "function ") ||
				strings.HasPrefix(trimmed, "class ") || strings.HasPrefix(trimmed, "interface ") ||
				strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "async def ") ||
				strings.HasPrefix(trimmed, "fn ") || strings.HasPrefix(trimmed, "pub fn ") ||
				strings.HasPrefix(trimmed, "struct ") || strings.HasPrefix(trimmed, "pub struct ") ||
				(strings.HasPrefix(trimmed, "const ") && strings.Contains(trimmed, "=>")) {
				if brace := strings.Index(trimmed, "{"); brace != -1 {
					trimmed = strings.TrimSpace(trimmed[:brace])
				}
				if colon := strings.Index(trimmed, ":"); colon != -1 && (strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "async def ")) {
					trimmed = strings.TrimSpace(trimmed[:colon])
				}
				outlineLines = append(outlineLines, "// "+trimmed)
			}
		}
		if len(outlineLines) >= 16 {
			outlineLines = append(outlineLines, "// ... and more declarations")
			break
		}
	}

	return strings.Join(outlineLines, "\n")
}

func formatFuncSig(d *ast.FuncDecl) string {
	if d == nil || d.Name == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("func ")
	if d.Recv != nil && len(d.Recv.List) > 0 {
		recvType := formatASTExpr(d.Recv.List[0].Type)
		sb.WriteString("(" + recvType + ") ")
	}
	sb.WriteString(d.Name.Name + "(...)")
	if d.Type != nil && d.Type.Results != nil && len(d.Type.Results.List) > 0 {
		var results []string
		for _, r := range d.Type.Results.List {
			results = append(results, formatASTExpr(r.Type))
		}
		if len(results) == 1 {
			sb.WriteString(" " + results[0])
		} else {
			sb.WriteString(" (" + strings.Join(results, ", ") + ")")
		}
	}
	return sb.String()
}

func formatASTExpr(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + formatASTExpr(t.X)
	case *ast.SelectorExpr:
		return formatASTExpr(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + formatASTExpr(t.Elt)
	case *ast.MapType:
		return "map[" + formatASTExpr(t.Key) + "]" + formatASTExpr(t.Value)
	default:
		return "any"
	}
}

// DetectProjectFromPrompt scans words in a prompt to see if any token matches
// an existing subfolder in projects/ or apps/.
func DetectProjectFromPrompt(prompt string, workspaceDir string) string {
	if workspaceDir == "" {
		workspaceDir = "."
	}
	cleanPrompt := strings.ToLower(prompt)
	words := strings.FieldsFunc(cleanPrompt, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_')
	})

	for _, prefix := range []string{"projects", "apps"} {
		parentDir := filepath.Join(workspaceDir, prefix)
		entries, err := os.ReadDir(parentDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			entryName := strings.ToLower(entry.Name())
			for _, w := range words {
				if w == entryName {
					return prefix + "/" + entry.Name()
				}
			}
		}
	}
	return ""
}

// buildEpistemicGroundingBlockLocked constructs a token-efficient context block (<250 tokens)
// summarizing the active project and known files grounded in ICX memory surfaces.
// Caller MUST hold s.mu (Lock or RLock).
func (s *Session) buildEpistemicGroundingBlockLocked() string {
	activeProj := s.activeProject
	files := make([]string, len(s.writtenFiles))
	copy(files, s.writtenFiles)
	client := s.icxClient

	if activeProj == "" && client != nil {
		if res, err := client.VarGet(context.Background(), icx.VarGetRequest{Key: "swarm.active_project"}); err == nil && res != nil && res.OK {
			if strVal, ok := res.Value.(string); ok && strVal != "" {
				activeProj = strVal
			}
		}
	}

	ws := s.workspaceDir
	if ws == "" {
		ws = "."
	}

	// Dynamic discovery of existing files in the active project directory
	if activeProj != "" {
		projDir := filepath.Join(ws, filepath.FromSlash(activeProj))
		if fi, err := os.Stat(projDir); err == nil && fi.IsDir() {
			seen := make(map[string]bool)
			for _, f := range files {
				seen[filepath.ToSlash(f)] = true
			}
			_ = filepath.Walk(projDir, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				rel, rErr := filepath.Rel(ws, path)
				if rErr == nil {
					cleanRel := filepath.ToSlash(rel)
					if !seen[cleanRel] {
						files = append(files, cleanRel)
						seen[cleanRel] = true
					}
				}
				return nil
			})
		}
	}

	if activeProj == "" && len(files) == 0 && len(s.unavailableTools) == 0 && len(s.recentBuilds) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[ICX Volumetric Memory State]\n")
	sb.WriteString("Surfaces: 0D-4D Synchronized in Lattice\n")
	if activeProj != "" {
		sb.WriteString(fmt.Sprintf("Active Project Anchor: %s\n", activeProj))
	}
	if len(files) > 0 {
		sb.WriteString(fmt.Sprintf("Existing Project Files (%d verified):\n", len(files)))
		maxFiles := 15
		if len(files) < maxFiles {
			maxFiles = len(files)
		}
		sort.Strings(files)
		for i := 0; i < maxFiles; i++ {
			sb.WriteString(fmt.Sprintf("  - %s\n", files[i]))
		}
		if len(files) > maxFiles {
			sb.WriteString(fmt.Sprintf("  - ... and %d more files\n", len(files)-maxFiles))
		}
		sb.WriteString("CODE PRESERVATION INVARIANT: The listed files are already implemented and working in the project. You must NEVER delete, stub, or overwrite existing working code. When enhancing or adding features, integrate with existing files or provide full upgraded versions that preserve all existing capabilities.\n")
	}
	if activeProj != "" {
		sb.WriteString(fmt.Sprintf("Grounding Invariant: All new or updated files for this application must reside strictly within '%s/'. Never create duplicate or variant project roots.\n", activeProj))
	}

	if client != nil && len(files) > 0 {
		sb.WriteString("\n[ICX 2D/3D SYMBOL & AST MANIFEST]\n")
		sb.WriteString("Known classes, functions, and routes extracted directly from 3D Volumetric AST:\n")
		count := 0
		for _, f := range files {
			if count >= 12 {
				break
			}
			if astResp, err := client.ReadFileAST(context.Background(), icx.ReadFileASTRequest{FilePath: f}); err == nil && astResp != nil {
				var parts []string
				if len(astResp.Classes) > 0 {
					parts = append(parts, "classes: "+strings.Join(astResp.Classes, ", "))
				}
				if len(astResp.Functions) > 0 {
					fnList := astResp.Functions
					if len(fnList) > 6 {
						fnList = append(fnList[:6], fmt.Sprintf("+%d more", len(fnList)-6))
					}
					parts = append(parts, "funcs/routes: "+strings.Join(fnList, ", "))
				}
				if len(astResp.Imports) > 0 {
					impList := astResp.Imports
					if len(impList) > 5 {
						impList = append(impList[:5], "...")
					}
					parts = append(parts, "imports: "+strings.Join(impList, ", "))
				}
				if len(parts) > 0 {
					sb.WriteString(fmt.Sprintf("  * %s [%s] (%s)\n", f, astResp.Language, strings.Join(parts, " | ")))
					count++
				}
			}
		}
	}

	if len(s.unavailableTools) > 0 {
		var unavail []string
		for t := range s.unavailableTools {
			unavail = append(unavail, t)
		}
		sort.Strings(unavail)
		sb.WriteString("\n[ENVIRONMENTAL TOOLING CONSTRAINTS]\n")
		sb.WriteString("The following CLI tools/frameworks are confirmed NOT INSTALLED or missing from PATH on this host machine:\n")
		for _, t := range unavail {
			sb.WriteString(fmt.Sprintf("  - %s: UNAVAILABLE\n", t))
		}
		sb.WriteString("CRITICAL REALITY CONSTRAINT: You are strictly forbidden from prescribing, scripting (e.g. in batch/shell scripts), or executing commands that rely on unavailable tools. When an application was previously planned with an unavailable tool, you MUST adapt the architecture to use native, available tooling (e.g. standard language tools, pure code, or standard web runtime) so that it builds and runs cleanly out of the box with zero missing dependencies.\n")
	}

	if len(s.recentBuilds) > 0 {
		sb.WriteString("\n[RECENT VERIFICATION & BUILD HISTORY]\n")
		count := len(s.recentBuilds)
		start := 0
		if count > 4 {
			start = count - 4
		}
		for i := start; i < count; i++ {
			b := s.recentBuilds[i]
			status := "PASSED"
			if b.ExitCode != 0 {
				status = fmt.Sprintf("FAILED (exit %d)", b.ExitCode)
			}
			summary := b.Diagnostic
			if len(summary) > 120 {
				summary = summary[:120] + "..."
			}
			summary = strings.ReplaceAll(summary, "\n", " ")
			sb.WriteString(fmt.Sprintf("  - [%s] %s", status, b.Command))
			if b.ExitCode != 0 && summary != "" {
				sb.WriteString(fmt.Sprintf(" :: %s", summary))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("REASONING MANDATE: Ground your response in the recent verification history above. Do not repeat failed commands or assume broken states have resolved without concrete fixes.\n")

		// Fetch exact line context for the failing file if available
		lastB := s.recentBuilds[len(s.recentBuilds)-1]
		if lastB.ExitCode != 0 {
			diagFiles := extractDiagFiles(lastB.Diagnostic)
			for _, df := range diagFiles {
				fullPath := filepath.Join(ws, filepath.FromSlash(df))
				if data, err := os.ReadFile(fullPath); err == nil && len(data) > 0 {
					lines := strings.Split(string(data), "\n")
					if len(lines) > 40 {
						lines = lines[:40]
					}
					sb.WriteString(fmt.Sprintf("\n[EXACT CODE CONTEXT for failing file %s]:\n", df))
					for lineNum, lineContent := range lines {
						sb.WriteString(fmt.Sprintf("%4d: %s\n", lineNum+1, lineContent))
					}
					sb.WriteString("[END CODE CONTEXT]\n")
					break
				}
			}
		}
	}

	return sb.String()
}

// buildEpistemicGroundingBlock constructs the grounding block with thread safety.
func (s *Session) buildEpistemicGroundingBlock() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buildEpistemicGroundingBlockLocked()
}

// SendMessageAll submits a user prompt to the LLM within the current dialogue context,
// appends the response to history, and extracts all embedded action intent tags.
// Employs the Fat-Payload Guard to compact bulky code blocks in past assistant turns,
// preventing context degradation and attention exhaustion across long tasks.
func (s *Session) SendMessageAll(ctx context.Context, userPrompt string) (string, []*ActionIntent, error) {
	trimmedPrompt := strings.TrimSpace(userPrompt)
	if trimmedPrompt == "" {
		return "", nil, errors.New("ui: prompt cannot be empty")
	}

	s.mu.Lock()
	if s.llmClient == nil {
		s.mu.Unlock()
		return "", nil, errors.New("ui: llm client is not configured")
	}

	client := s.llmClient
	model := s.model
	systemPrompt := s.systemPrompt
	cfg := s.config

	prevHistoryLen := len(s.history)

	// Append user turn to history
	s.history = append(s.history, llm.ChatMessage{
		Role:    "user",
		Content: trimmedPrompt,
	})

	// Set host task awareness only from user prompts, never from internal self-healing or diagnostic prompts
	if engine.IsHostPackageTask(trimmedPrompt) && !strings.HasPrefix(trimmedPrompt, "Verification test") && !strings.Contains(trimmedPrompt, "healing attempt") && !strings.Contains(trimmedPrompt, "Host Package Shield") {
		s.isHostTask = true
	}

	// Automatically discover and anchor to existing project subfolders referenced in prompt
	if s.activeProject == "" && !s.isHostTask {
		if detected := DetectProjectFromPrompt(trimmedPrompt, s.workspaceDir); detected != "" {
			s.activeProject = detected
			if s.icxClient != nil {
				_, _ = s.icxClient.VarSet(context.Background(), icx.VarSetRequest{
					Key:   "swarm.active_project",
					Value: detected,
				})
			}
		}
	}

	// Prepend system prompt augmented with live ICX Epistemic Memory Grounding
	effectiveSystemPrompt := systemPrompt
	if grounding := s.buildEpistemicGroundingBlockLocked(); grounding != "" {
		effectiveSystemPrompt = systemPrompt + "\n\n" + grounding
	}

	messages := make([]llm.ChatMessage, 0, len(s.history)+1)
	if effectiveSystemPrompt != "" {
		messages = append(messages, llm.ChatMessage{
			Role:    "system",
			Content: effectiveSystemPrompt,
		})
	}

	// Identify the most recent assistant turn in history
	lastAssistantIdx := -1
	for idx := len(s.history) - 1; idx >= 0; idx-- {
		if s.history[idx].Role == "assistant" {
			lastAssistantIdx = idx
			break
		}
	}

	// Apply Fat-Payload Guard to older historical turns when assembling outgoing prompt:
	// 1. Compares assistant messages >2,048 bytes (preserving the most recent assistant turn).
	// 2. Compares user diagnostic / compiler output messages >1,024 bytes older than the last 2 turns.
	// This preserves full context for active reasoning while preventing token bloat across long healing loops.
	for i, h := range s.history {
		if h.Role == "assistant" && len(h.Content) > 2048 && i < lastAssistantIdx {
			messages = append(messages, llm.ChatMessage{
				Role:    h.Role,
				Content: compactHistoricalAssistantMessage(h.Content),
			})
		} else if h.Role == "user" && len(h.Content) > 1024 && i < len(s.history)-2 {
			messages = append(messages, llm.ChatMessage{
				Role:    h.Role,
				Content: compactHistoricalUserDiagnosticMessage(h.Content),
			})
		} else {
			messages = append(messages, h)
		}
	}

	req := &llm.ChatCompletionRequest{
		Model:       model,
		Messages:    messages,
		Temperature: cfg.Temperature,
		MaxTokens:   cfg.MaxTokens,
	}
	s.mu.Unlock()

	// Perform network I/O without holding the lock to prevent mutex contention
	resp, err := client.Complete(ctx, req)
	if err != nil {
		s.mu.Lock()
		if len(s.history) > prevHistoryLen {
			s.history = s.history[:prevHistoryLen]
		}
		s.mu.Unlock()
		return "", nil, fmt.Errorf("ui: llm completion failed: %w", err)
	}

	if len(resp.Choices) == 0 {
		s.mu.Lock()
		if len(s.history) > prevHistoryLen {
			s.history = s.history[:prevHistoryLen]
		}
		s.mu.Unlock()
		return "", nil, llm.ErrEmptyResponse
	}

	replyContent := resp.Choices[0].Message.Content
	finishReason := resp.Choices[0].FinishReason

	// Truncation Recovery Loop: If completion stopped due to max_tokens or an unclosed code block,
	// issue continuation prompt to stitch the remaining source code seamlessly.
	continuationAttempts := 0
	for (finishReason == "length" || hasUnclosedTrailingCodeBlock(replyContent)) && continuationAttempts < 3 {
		continuationAttempts++
		contReq := &llm.ChatCompletionRequest{
			Model:       model,
			Messages:    append(messages, llm.ChatMessage{Role: "assistant", Content: replyContent}, llm.ChatMessage{Role: "user", Content: "Your previous response was truncated mid-generation. Continue outputting the code verbatim from the exact truncation point without repeating any existing code, explanation, or preamble:"}),
			Temperature: cfg.Temperature,
			MaxTokens:   cfg.MaxTokens,
		}
		contResp, contErr := client.Complete(ctx, contReq)
		if contErr != nil || len(contResp.Choices) == 0 {
			break
		}
		contChunk := contResp.Choices[0].Message.Content
		if strings.TrimSpace(contChunk) == "" {
			break
		}
		replyContent += contChunk
		finishReason = contResp.Choices[0].FinishReason
	}

	s.mu.Lock()
	// Append assistant response to history
	s.history = append(s.history, llm.ChatMessage{
		Role:    "assistant",
		Content: replyContent,
	})
	s.mu.Unlock()

	actions := ParseAllActionIntents(replyContent)
	return replyContent, actions, nil
}

// hasUnclosedTrailingCodeBlock reports whether text contains an opened action tag whose code fence was never closed.
func hasUnclosedTrailingCodeBlock(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.Contains(trimmed, "[ACTION:") {
		return false
	}
	fenceCount := 0
	lines := strings.Split(trimmed, "\n")
	for _, l := range lines {
		tl := strings.TrimSpace(l)
		if strings.HasPrefix(tl, "```") {
			fenceCount++
		}
	}
	return fenceCount%2 != 0
}

// SendMessage submits a user prompt to the LLM within the current dialogue context,
// appends the response to history, and extracts any primary embedded action intent tag.
func (s *Session) SendMessage(ctx context.Context, userPrompt string) (string, *ActionIntent, error) {
	replyContent, actions, err := s.SendMessageAll(ctx, userPrompt)
	if err != nil {
		return "", nil, err
	}
	var firstAction *ActionIntent
	if len(actions) > 0 {
		firstAction = actions[0]
	}
	return replyContent, firstAction, nil
}

// Reset clears conversational dialogue history while keeping workspace state,
// model configuration, and subterranean engine connection intact.
// Triggers an ICX Zero-Token Session Reset to clear dialogue facts while preserving 100% of 4D lattice memory.
func (s *Session) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = make([]llm.ChatMessage, 0)
	s.isHostTask = false
	s.activePlanSpec = nil
	s.activePlanPath = ""
	if s.icxClient != nil {
		_, _ = s.icxClient.ResetSession(context.Background(), "")
	}
}

// ActivePlan returns the currently active master implementation plan spec and its file path, if any.
func (s *Session) ActivePlan() (*engine.PlanSpec, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activePlanSpec, s.activePlanPath
}

// SetActivePlan records the currently active master implementation plan spec and its file path.
func (s *Session) SetActivePlan(spec *engine.PlanSpec, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activePlanSpec = spec
	s.activePlanPath = path
}

// History returns a defensive copy of the current dialogue turn history.
func (s *Session) History() []llm.ChatMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]llm.ChatMessage, len(s.history))
	copy(cp, s.history)
	return cp
}

// AddMessage appends a message directly into the session dialogue context.
func (s *Session) AddMessage(msg llm.ChatMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, msg)
}

// FeedExecutionSummary feeds subterranean engine execution results back into
// the dialogue context so subsequent conversation turns retain awareness of what was built.
func (s *Session) FeedExecutionSummary(summary string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, llm.ChatMessage{
		Role:    "user",
		Content: fmt.Sprintf("[Execution Attestation Report]\n%s", summary),
	})
}

// Status returns a snapshot of current session metrics and connectivity.
func (s *Session) Status() SessionStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	turns := 0
	for _, m := range s.history {
		if m.Role == "user" {
			turns++
		}
	}

	activeProj := s.activeProject
	nodesCount := 0
	if s.icxClient != nil {
		if insp, err := s.icxClient.InspectSpace(context.Background(), ""); err == nil && insp != nil {
			nodesCount = insp.NodesCount
		}
		if activeProj == "" {
			if res, err := s.icxClient.VarGet(context.Background(), icx.VarGetRequest{Key: "swarm.active_project"}); err == nil && res != nil && res.OK {
				if strVal, ok := res.Value.(string); ok {
					activeProj = strVal
				}
			}
		}
	}

	return SessionStatus{
		Model:         s.model,
		WorkspaceDir:  s.workspaceDir,
		TurnCount:     turns,
		HistoryLen:    len(s.history),
		HasEngine:     s.engineOrch != nil,
		ActiveProject: activeProj,
		LatticeNodes:  nodesCount,
	}
}

// ICX returns the connected ICXClient 4D volumetric memory interface.
func (s *Session) ICX() icx.ICXClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.icxClient
}

// SetICX updates the connected ICXClient 4D volumetric memory interface.
func (s *Session) SetICX(client icx.ICXClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.icxClient = client
}

// ActiveProject returns the active canonical project root, if any.
func (s *Session) ActiveProject() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.activeProject != "" {
		return s.activeProject
	}
	if s.icxClient != nil {
		if res, err := s.icxClient.VarGet(context.Background(), icx.VarGetRequest{Key: "swarm.active_project"}); err == nil && res != nil && res.OK {
			if strVal, ok := res.Value.(string); ok && strVal != "" {
				return strVal
			}
		}
	}
	return ""
}

// SetActiveProject updates the active canonical project root across memory surfaces.
func (s *Session) SetActiveProject(proj string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeProject = proj
	if s.icxClient != nil && proj != "" {
		_, _ = s.icxClient.VarSet(context.Background(), icx.VarSetRequest{
			Key:   "swarm.active_project",
			Value: proj,
		})
	}
}

// WrittenFiles returns a defensive copy of all relative file paths written in this session.
func (s *Session) WrittenFiles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]string, len(s.writtenFiles))
	copy(cp, s.writtenFiles)
	return cp
}

// RecordUnavailableTool notes a tool/binary that is absent in the host environment.
// It verifies that the tool is not a host PATH executable, a standard runtime,
// or a local file in the project/workspace before blacklisting.
func (s *Session) RecordUnavailableTool(tool string) {
	tool = strings.TrimSpace(strings.ToLower(tool))
	if tool == "" {
		return
	}

	// Never blacklist standard language binaries if they exist on PATH
	standardTools := map[string]bool{
		"python": true, "py": true, "python3": true, "go": true,
		"npm": true, "npx": true, "node": true, "git": true,
		"cargo": true, "rustc": true, "pip": true, "pip3": true,
	}
	if standardTools[tool] {
		if _, err := exec.LookPath(tool); err == nil {
			return
		}
	}

	// Never blacklist any binary that is actually present in host PATH
	if _, err := exec.LookPath(tool); err == nil {
		return
	}

	// Never blacklist names matching local project files (e.g. worker.py, database.py, app.py)
	if s.workspaceDir != "" {
		candidates := []string{
			filepath.Join(s.workspaceDir, tool),
			filepath.Join(s.workspaceDir, tool+".py"),
			filepath.Join(s.workspaceDir, tool+".go"),
			filepath.Join(s.workspaceDir, tool+".js"),
		}
		if s.activeProject != "" {
			candidates = append(candidates,
				filepath.Join(s.workspaceDir, s.activeProject, tool),
				filepath.Join(s.workspaceDir, s.activeProject, tool+".py"),
				filepath.Join(s.workspaceDir, s.activeProject, tool+".go"),
				filepath.Join(s.workspaceDir, s.activeProject, tool+".js"),
			)
		}
		for _, cand := range candidates {
			if _, err := os.Stat(cand); err == nil {
				return // local project file, not a missing CLI tool
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailableTools == nil {
		s.unavailableTools = make(map[string]bool)
	}
	s.unavailableTools[tool] = true
}

// ClearUnavailableTool removes a tool from the blacklist.
func (s *Session) ClearUnavailableTool(tool string) {
	tool = strings.TrimSpace(strings.ToLower(tool))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailableTools != nil {
		delete(s.unavailableTools, tool)
	}
}

// ClearAllUnavailableTools resets the blacklist.
func (s *Session) ClearAllUnavailableTools() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavailableTools = make(map[string]bool)
}

// IsToolUnavailable checks if a tool is known to be missing in the host environment.
func (s *Session) IsToolUnavailable(tool string) bool {
	tool = strings.TrimSpace(strings.ToLower(tool))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.unavailableTools == nil {
		return false
	}
	return s.unavailableTools[tool]
}

// UnavailableTools returns all registered missing tools.
func (s *Session) UnavailableTools() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []string
	for t := range s.unavailableTools {
		list = append(list, t)
	}
	sort.Strings(list)
	return list
}

// RecordBuild stores a build attempt record for recent reasoning history.
func (s *Session) RecordBuild(cmd string, exitCode int, diag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := BuildRecord{
		Command:    cmd,
		ExitCode:   exitCode,
		Diagnostic: diag,
		Timestamp:  time.Now(),
	}
	s.recentBuilds = append(s.recentBuilds, rec)
	if len(s.recentBuilds) > 10 {
		s.recentBuilds = s.recentBuilds[len(s.recentBuilds)-10:]
	}
}

// RecentBuilds returns the recent build attempts in this session.
func (s *Session) RecentBuilds() []BuildRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]BuildRecord, len(s.recentBuilds))
	copy(cp, s.recentBuilds)
	return cp
}

// LastBuildRecord returns the most recent build record if available.
func (s *Session) LastBuildRecord() (BuildRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.recentBuilds) == 0 {
		return BuildRecord{}, false
	}
	return s.recentBuilds[len(s.recentBuilds)-1], true
}

// ExtractUnavailableToolFromCommand inspects command tokens and returns the first matching unavailable tool, if any.
// It ONLY checks the executable binary name (argv[0] of the command, and commands following unquoted ;, &&, ||, |).
// It NEVER inspects tokens inside string literals or arguments.
func (s *Session) ExtractUnavailableToolFromCommand(cmd string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.unavailableTools) == 0 {
		return ""
	}

	binaries := extractCommandBinaries(cmd)
	for _, bin := range binaries {
		clean := strings.ToLower(strings.TrimSpace(bin))
		clean = filepath.Base(filepath.FromSlash(clean))
		clean = strings.TrimSuffix(clean, ".exe")
		clean = strings.TrimSuffix(clean, ".bat")
		clean = strings.TrimSuffix(clean, ".cmd")
		clean = strings.TrimSuffix(clean, ".sh")
		if s.unavailableTools[clean] {
			return clean
		}
	}
	return ""
}

// extractCommandBinaries parses a command line string and returns only executable binary names,
// strictly ignoring tokens inside quotes ("...", '...', `...`) and arguments after the binary.
func extractCommandBinaries(cmd string) []string {
	var binaries []string
	n := len(cmd)
	i := 0

	for i < n {
		for i < n && (cmd[i] == ' ' || cmd[i] == '\t' || cmd[i] == '\r' || cmd[i] == '\n') {
			i++
		}
		if i >= n {
			break
		}

		if cmd[i] == '#' || (i+1 < n && cmd[i] == '/' && cmd[i+1] == '/') {
			for i < n && cmd[i] != '\n' {
				i++
			}
			continue
		}

		var tok strings.Builder
		for i < n {
			c := cmd[i]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ';' || c == '&' || c == '|' {
				break
			}
			if c == '"' || c == '\'' || c == '`' {
				q := c
				i++
				for i < n && cmd[i] != q {
					if cmd[i] == '\\' && i+1 < n {
						i++
					}
					tok.WriteByte(cmd[i])
					i++
				}
				if i < n && cmd[i] == q {
					i++
				}
				continue
			}
			tok.WriteByte(c)
			i++
		}

		token := strings.TrimSpace(tok.String())
		if strings.Contains(token, "=") && !strings.HasPrefix(token, "./") && !strings.HasPrefix(token, ".\\") {
			continue
		}

		if token != "" {
			binaries = append(binaries, token)
		}

		// Skip remaining arguments until unquoted command separator
		for i < n {
			c := cmd[i]
			if c == '"' || c == '\'' || c == '`' {
				q := c
				i++
				for i < n && cmd[i] != q {
					if cmd[i] == '\\' && i+1 < n {
						i++
					}
					i++
				}
				if i < n && cmd[i] == q {
					i++
				}
				continue
			}
			if c == ';' || c == '\n' {
				i++
				break
			}
			if c == '&' {
				if i+1 < n && cmd[i+1] == '&' {
					i += 2
				} else {
					i++
				}
				break
			}
			if c == '|' {
				if i+1 < n && cmd[i+1] == '|' {
					i += 2
				} else {
					i++
				}
				break
			}
			i++
		}
	}

	return binaries
}

// QuarantinedPayload represents a code file payload that failed pre-flight validation
// and is preserved in memory and on disk for recovery or override.
type QuarantinedPayload struct {
	Path      string    `json:"path"`
	Content   string    `json:"content"`
	Reason    string    `json:"reason"`
	SHA       string    `json:"sha"`
	Attempts  int       `json:"attempts"`
	Timestamp time.Time `json:"timestamp"`
}

// QuarantinePayload stores a rejected write payload in memory and on disk.
func (s *Session) QuarantinePayload(path, content, reason string) *QuarantinedPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quarantine == nil {
		s.quarantine = make(map[string]*QuarantinedPayload)
	}
	h := sha256.Sum256([]byte(content))
	shaStr := hex.EncodeToString(h[:])

	cleanP := filepath.ToSlash(strings.TrimSpace(path))
	existing, found := s.quarantine[cleanP]
	attempts := 1
	if found {
		attempts = existing.Attempts + 1
	}

	qp := &QuarantinedPayload{
		Path:      cleanP,
		Content:   content,
		Reason:    reason,
		SHA:       shaStr,
		Attempts:  attempts,
		Timestamp: time.Now(),
	}
	s.quarantine[cleanP] = qp

	// Persist to disk in workspace .omnicoder/quarantine/
	if s.workspaceDir != "" {
		qDir := filepath.Join(s.workspaceDir, ".omnicoder", "quarantine")
		_ = os.MkdirAll(qDir, 0755)
		sha8 := shaStr
		if len(sha8) > 8 {
			sha8 = sha8[:8]
		}
		safeName := strings.ReplaceAll(cleanP, "/", "_") + "." + sha8
		_ = os.WriteFile(filepath.Join(qDir, safeName), []byte(content), 0644)
	}

	return qp
}

// GetQuarantinedPayload retrieves a quarantined payload by path or basename.
func (s *Session) GetQuarantinedPayload(path string) (*QuarantinedPayload, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.quarantine == nil {
		return nil, false
	}
	cleanP := filepath.ToSlash(strings.TrimSpace(path))
	if qp, ok := s.quarantine[cleanP]; ok {
		return qp, true
	}
	// Fallback match by basename
	base := filepath.Base(cleanP)
	for k, v := range s.quarantine {
		if filepath.Base(k) == base {
			return v, true
		}
	}
	return nil, false
}

// AllQuarantined returns all currently quarantined payloads.
func (s *Session) AllQuarantined() []*QuarantinedPayload {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []*QuarantinedPayload
	for _, qp := range s.quarantine {
		list = append(list, qp)
	}
	return list
}

// RemoveQuarantined clears a path from quarantine.
func (s *Session) RemoveQuarantined(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quarantine != nil {
		cleanP := filepath.ToSlash(strings.TrimSpace(path))
		delete(s.quarantine, cleanP)
		base := filepath.Base(cleanP)
		for k := range s.quarantine {
			if filepath.Base(k) == base {
				delete(s.quarantine, k)
			}
		}
	}
}

// IsMultiLine returns true if multi-line input mode is currently active.
func (s *Session) IsMultiLine() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.multiLineMode
}

// SetMultiLine explicitly sets the multi-line input mode.
func (s *Session) SetMultiLine(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.multiLineMode = enabled
}

// ToggleMultiLine inverts the current multi-line input mode and returns the new state.
func (s *Session) ToggleMultiLine() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.multiLineMode = !s.multiLineMode
	return s.multiLineMode
}

// Model returns the active model identifier.
func (s *Session) Model() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model
}

// SetModel updates the active model identifier.
func (s *Session) SetModel(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model = model
}

// WorkspaceDir returns the active workspace root directory.
func (s *Session) WorkspaceDir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workspaceDir
}

// SystemPrompt returns the calibrated system prompt.
func (s *Session) SystemPrompt() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.systemPrompt
}

// IsHostTask reports whether the session is currently executing a host package task.
func (s *Session) IsHostTask() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isHostTask
}

// SetHostTask configures whether the session is currently executing a host package task.
func (s *Session) SetHostTask(val bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.isHostTask = val
}

// ClearDialogue drops chat history and resets ICX dialogue tokens. Lattice facts stay.
func (s *Session) ClearDialogue(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.history = nil
	s.activePlanSpec = nil
	s.activePlanPath = ""
	client := s.icxClient
	s.mu.Unlock()
	if client != nil {
		_, _ = client.ResetSession(ctx, "")
	}
}

// Engine returns the connected master subterranean orchestrator, if any.
func (s *Session) Engine() *engine.Orchestrator {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.engineOrch
}

// SetEngine updates the connected master subterranean orchestrator.
func (s *Session) SetEngine(orch *engine.Orchestrator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.engineOrch = orch
}

// LLMClient returns the underlying LLM client interface.
func (s *Session) LLMClient() llm.LLMClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.llmClient
}

// SetProgressCallback configures an optional real-time progress callback.
func (s *Session) SetProgressCallback(cb func(event engine.TurnProgressEvent)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progressCallback = cb
}

// ProgressCallback returns the currently configured real-time progress callback.
func (s *Session) ProgressCallback() func(event engine.TurnProgressEvent) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.progressCallback
}

// LastAssistantMessage retrieves the content of the most recent assistant message from history.
// Returns an empty string if history contains no assistant turns.
func (s *Session) LastAssistantMessage() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := len(s.history) - 1; i >= 0; i-- {
		if s.history[i].Role == "assistant" {
			return s.history[i].Content
		}
	}
	return ""
}

// ExtractModuleName parses the declared module name from a go.mod file's contents,
// ignoring line comments (//) and multi-line block comments (/* ... */), and supporting
// both single-line directives ('module <name>') and parenthesized blocks ('module ( ... )').
func ExtractModuleName(data []byte) string {
	inBlockComment := false
	inModuleBlock := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		var cleanLine strings.Builder
		i := 0
		n := len(line)
		for i < n {
			if inBlockComment {
				if i+1 < n && line[i] == '*' && line[i+1] == '/' {
					inBlockComment = false
					i += 2
				} else {
					i++
				}
				continue
			}
			if i+1 < n && line[i] == '/' && line[i+1] == '*' {
				inBlockComment = true
				i += 2
				continue
			}
			if i+1 < n && line[i] == '/' && line[i+1] == '/' {
				break // rest of line is line comment
			}
			cleanLine.WriteByte(line[i])
			i++
		}
		trimmed := strings.TrimSpace(cleanLine.String())
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}

		if inModuleBlock {
			if fields[0] == ")" {
				inModuleBlock = false
				continue
			}
			mod := strings.Trim(fields[0], "\"'`")
			if mod != "" && mod != ")" {
				return mod
			}
		}

		if fields[0] == "module" {
			if len(fields) >= 2 {
				if fields[1] == "(" {
					inModuleBlock = true
					if len(fields) >= 3 && fields[2] != ")" {
						mod := strings.Trim(fields[2], "\"'`")
						if mod != "" {
							return mod
						}
					}
					continue
				}
				mod := strings.Trim(fields[1], "\"'`")
				if mod != "" {
					return mod
				}
			} else {
				inModuleBlock = true
			}
		}
	}
	return ""
}

// isWorkspaceRootFile checks if targetPath refers to a specific filename directly in the workspace root.
func isWorkspaceRootFile(ws, targetPath, filename string) bool {
	cleanTarget := filepath.Clean(targetPath)
	if !filepath.IsAbs(cleanTarget) {
		cleanTarget = filepath.Clean(filepath.Join(ws, cleanTarget))
	}
	cleanRoot := filepath.Clean(filepath.Join(ws, filename))
	if strings.EqualFold(cleanTarget, cleanRoot) {
		return true
	}
	trimmedTarget := strings.TrimRight(cleanTarget, ". ")
	trimmedRoot := strings.TrimRight(cleanRoot, ". ")
	if strings.EqualFold(trimmedTarget, trimmedRoot) {
		return true
	}
	absTarget, err1 := filepath.Abs(cleanTarget)
	absRoot, err2 := filepath.Abs(cleanRoot)
	if err1 == nil && err2 == nil && (strings.EqualFold(absTarget, absRoot) || strings.EqualFold(strings.TrimRight(absTarget, ". "), strings.TrimRight(absRoot, ". "))) {
		return true
	}
	realTarget, err3 := filepath.EvalSymlinks(absTarget)
	realRoot, err4 := filepath.EvalSymlinks(absRoot)
	if err3 == nil && err4 == nil && (strings.EqualFold(realTarget, realRoot) || strings.EqualFold(strings.TrimRight(realTarget, ". "), strings.TrimRight(realRoot, ". "))) {
		return true
	}
	return false
}

var (
	xmlPlaceholderRegex     = regexp.MustCompile(`(?i)<\s*(placeholder[a-z0-9_-]*|todo[a-z0-9_-]*|insert_code[a-z0-9_-]*|fill_in|code_here)\s*\/?>`)
	bracketPlaceholderRegex = regexp.MustCompile(`(?i)\[\s*(placeholder|insert[-_ ]code|code[-_ ]here|implement[-_ ]here|todo)\s*\]`)
	ellipsisCommentRegex    = regexp.MustCompile(`(?i)(?://|/\*)\s*\.\.\.\s*(?:existing|rest of|remaining|other|unmodified)\s*(?:code|implementation|logic|methods|functions)?\s*(?:\*/)?`)
)

// detectPlaceholderTags detects literal placeholder tags, tokens, or truncation comments
// that represent incomplete or stubbed code (e.g. <placeholder>, [TODO], // ... existing code ...).
func detectPlaceholderTags(content string) (bool, string) {
	if m := xmlPlaceholderRegex.FindString(content); m != "" {
		return true, strings.TrimSpace(m)
	}
	if m := bracketPlaceholderRegex.FindString(content); m != "" {
		return true, strings.TrimSpace(m)
	}
	if m := ellipsisCommentRegex.FindString(content); m != "" {
		return true, strings.TrimSpace(m)
	}
	if m := conflictMarkerRegex.FindString(content); m != "" {
		if strings.Contains(content, "<<<<") && (strings.Contains(content, "====") || strings.Contains(content, ">>>>")) {
			return true, strings.TrimSpace(m)
		}
	}
	return false, ""
}

// WriteFile atomically writes data to a file inside the session's workspace directory.
// It enforces strict path traversal containment (ensuring files are created inside WorkspaceDir),
// creates parent directories automatically (os.MkdirAll 0755), commits the file atomically with mode 0644,
// and returns the target file path, its SHA-256 seal hex digest, and any error encountered.
func (s *Session) WriteFile(relPath string, data []byte) (string, string, error) {
	// Placeholder tag detection guard: refuse writes containing literal placeholder tags
	if hasPlaceholder, tag := detectPlaceholderTags(string(data)); hasPlaceholder {
		return "", "", fmt.Errorf("ui: refusing to write file containing placeholder tag %q: complete implementation required", tag)
	}

	trimmed := strings.TrimSpace(relPath)
	if trimmed == "" {
		return "", "", errors.New("ui: file path cannot be empty")
	}
	clean := filepath.Clean(strings.ReplaceAll(trimmed, "\\", "/"))
	if clean == "." || clean == "/" || clean == "\\" {
		return "", "", errors.New("ui: target cannot be a directory or root")
	}

	ws := s.WorkspaceDir()
	if ws == "" {
		ws = "."
	}

	realWs := ws
	if absWs, errAbs := filepath.Abs(filepath.Clean(ws)); errAbs == nil {
		if evalWs, errEval := filepath.EvalSymlinks(absWs); errEval == nil {
			realWs = evalWs
		} else {
			realWs = absWs
		}
	}

	// 1. Strict path traversal containment via resolveSecurePath
	targetPath, err := resolveSecurePath(ws, relPath)
	if err != nil {
		return "", "", fmt.Errorf("ui: invalid or insecure path: %w", err)
	}
	if targetPath == ws || targetPath == realWs {
		return "", "", errors.New("ui: target path cannot be the workspace directory itself")
	}
	if fi, err := os.Stat(targetPath); err == nil && fi.IsDir() {
		return "", "", errors.New("ui: target path is an existing directory")
	}

	cleanRel := filepath.ToSlash(filepath.Clean(trimmed))
	if relFromWs, errRel := filepath.Rel(realWs, targetPath); errRel == nil && !strings.HasPrefix(relFromWs, "..") {
		cleanRel = filepath.ToSlash(filepath.Clean(relFromWs))
	}
	cleanRel = strings.TrimPrefix(cleanRel, "./")
	cleanRel = strings.TrimPrefix(cleanRel, "/")

	// Protection of critical repository infrastructure (.git, .gitmodules, .gitattributes, etc.)
	lowerRel := strings.ToLower(cleanRel)
	lowerRelTrimmed := strings.TrimRight(lowerRel, ". ")
	if lowerRelTrimmed == ".git" || strings.HasPrefix(lowerRelTrimmed, ".git/") || strings.Contains(lowerRelTrimmed, "/.git/") || strings.HasSuffix(lowerRelTrimmed, "/.git") ||
		lowerRelTrimmed == ".gitmodules" || lowerRelTrimmed == ".gitattributes" {
		return "", "", errors.New("ui: target path cannot be inside .git directory or overwrite git infrastructure")
	}

	// Host root go.mod protection guard (R2)
	cleanRelTrimmed := strings.TrimRight(cleanRel, ". ")
	if strings.EqualFold(cleanRelTrimmed, "go.mod") || isWorkspaceRootFile(ws, targetPath, "go.mod") {
		rootModPath := filepath.Join(ws, "go.mod")
		if hostData, err := os.ReadFile(rootModPath); err == nil {
			hostMod := ExtractModuleName(hostData)
			newMod := ExtractModuleName(data)
			if hostMod != newMod {
				return "", "", fmt.Errorf("ui: refusing to overwrite workspace root go.mod (module %q) with foreign module %q; new applications must be placed in a subfolder (e.g. 'apps/<name>/go.mod')", hostMod, newMod)
			}
		}
	}

	// Host engine package protection guard: protect OmniCoder's own core packages from unintentional mutation during app tasks
	if engine.IsHostPackagePath(cleanRel) && !s.IsHostTask() {
		return "", "", fmt.Errorf("ui: refusing to modify host engine package %q during non-host task; applications must be placed in a subfolder (e.g. 'projects/<name>/' or 'apps/<name>/')", cleanRel)
	}

	// Pre-flight Go language check: validate syntax and package consistency before touching disk
	if strings.HasSuffix(strings.ToLower(cleanRel), ".go") {
		if err := devops.ValidateGoPreflight(ws, cleanRel, string(data)); err != nil {
			return "", "", fmt.Errorf("ui: pre-flight Go language check failed: %w", err)
		}
	}

	// Code Preservation Write Guard: block destructive overwrites that would shrink existing CODE files.
	// Only applies to code files (.go, .ts, .py, .rs, .js, .tsx, .jsx) — non-code files (HTML, JSON, CSS, etc.)
	// are freely rewritable since complete rewrites are normal for those formats.
	if isCodeFileForWriteGuard(cleanRel) {
		if existing, readErr := os.ReadFile(targetPath); readErr == nil && len(strings.TrimSpace(string(existing))) > 0 {
			// If existing file is corrupted (e.g. invalid syntax), allow replacing it
			existingIsCorrupted := devops.ValidatePolyglotPreflight(ws, cleanRel, string(existing)) != nil
			if !existingIsCorrupted {
				// Allow clean, syntactically valid rewrites
				isCleanRewrite := false
				ext := strings.ToLower(filepath.Ext(cleanRel))
				if ext == ".py" && devops.ValidatePythonSource(string(data)) == nil && len(strings.TrimSpace(string(data))) > 30 {
					isCleanRewrite = true
				} else if ext == ".go" && devops.ValidateGoSource(string(data)) == nil && len(strings.TrimSpace(string(data))) > 30 {
					if len(detectMissingGoFunctions(string(existing), string(data))) == 0 && len(detectMissingGoTypes(string(existing), string(data))) == 0 {
						isCleanRewrite = true
					}
				}
				if !isCleanRewrite {
					oldLines := strings.Count(string(existing), "\n") + 1
					newLines := strings.Count(string(data), "\n") + 1
					if oldLines > 20 && float64(newLines)/float64(oldLines) < 0.60 {
						return "", "", fmt.Errorf("ui: write guard blocked destructive overwrite of %s (old: %d lines, new: %d lines, %.0f%% retention); use [ACTION:PATCH] for surgical edits",
							cleanRel, oldLines, newLines, float64(newLines)/float64(oldLines)*100)
					}
					if oldLines-newLines > 50 {
						return "", "", fmt.Errorf("ui: write guard blocked — would delete %d lines from %s; use [ACTION:PATCH] for surgical edits",
							oldLines-newLines, cleanRel)
					}
				}
			}
		}
	}

	// 2. Automatic directory creation
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", fmt.Errorf("ui: failed to create directory: %w", err)
	}

	// 3. Atomic file writing with temporary staging
	tmpFile, err := os.CreateTemp(dir, ".tmp-write-*")
	if err != nil {
		return "", "", fmt.Errorf("ui: failed to create temporary file: %w", err)
	}
	tmpName := tmpFile.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return "", "", fmt.Errorf("ui: failed to write data: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return "", "", fmt.Errorf("ui: failed to sync file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return "", "", fmt.Errorf("ui: failed to close file: %w", err)
	}

	_ = os.Chmod(tmpName, 0644)

	// Atomic commit with Windows retry and backup swap
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		err := os.Rename(tmpName, targetPath)
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err

		if runtime.GOOS == "windows" {
			bakName := fmt.Sprintf("%s.tmp-bak-%d", targetPath, time.Now().UnixNano())
			if rErr := os.Rename(targetPath, bakName); rErr == nil {
				if renErr := os.Rename(tmpName, targetPath); renErr == nil {
					_ = os.Remove(bakName)
					lastErr = nil
					break
				}
				_ = os.Rename(bakName, targetPath)
			}
		}

		time.Sleep(time.Duration((attempt+1)*10) * time.Millisecond)
	}

	if lastErr != nil {
		return "", "", fmt.Errorf("ui: failed to commit file atomically: %w", lastErr)
	}
	committed = true

	_ = os.Chmod(targetPath, 0644)

	// 4. SHA-256 seal computation
	sha := canonical.SHA256Bytes(data)

	relOutput, err := filepath.Rel(realWs, targetPath)
	if err != nil || strings.HasPrefix(relOutput, "..") {
		relOutput = cleanRel
	}

	finalRel := filepath.ToSlash(relOutput)

	// 5. Ingest into ICX Volumetric Memory Surfaces (0D-4D)
	s.recordWriteToICXSurfaces(finalRel, data, sha)

	return finalRel, sha, nil
}

// recordWriteToICXSurfaces registers a committed file write across ICX 0D-4D memory surfaces,
// anchoring the active project, crystallizing file slots in the lattice, and synchronizing swarm state.
func (s *Session) recordWriteToICXSurfaces(relPath string, data []byte, sha string) {
	s.mu.Lock()
	s.writtenFiles = append(s.writtenFiles, relPath)
	activeProj := s.activeProject
	if activeProj == "" {
		cleanSlash := filepath.ToSlash(relPath)
		for _, prefix := range []string{"projects/", "apps/", "output/"} {
			if strings.HasPrefix(cleanSlash, prefix) {
				sub := strings.TrimPrefix(cleanSlash, prefix)
				parts := strings.Split(sub, "/")
				if len(parts) > 1 && parts[0] != "" {
					activeProj = prefix + parts[0]
					s.activeProject = activeProj
					break
				}
			}
		}
	}
	client := s.icxClient
	fileList := make([]string, len(s.writtenFiles))
	copy(fileList, s.writtenFiles)
	s.mu.Unlock()

	if client == nil {
		return
	}

	ctx := context.Background()

	// 0D Surface: Scalar facts and seals
	_, _ = client.IngestDimension(ctx, icx.IngestDimensionRequest{
		Dimension: "0D_SCALAR",
		Payload: map[string]any{
			"type":      "file_seal",
			"file_path": relPath,
			"sha256":    sha,
			"size":      len(data),
			"timestamp": time.Now().Unix(),
		},
	})

	// 1D Surface: Sequential streams (remember in file_slots for byte-exact quote retrieval)
	_, _ = client.Remember(ctx, icx.RememberRequest{
		Family:   "file_slots",
		SourceID: relPath,
		Text:     string(data),
		NodeID:   fmt.Sprintf("file_%s", relPath),
		Metadata: map[string]any{
			"sha256": sha,
			"size":   len(data),
		},
	})
	_, _ = client.IngestDimension(ctx, icx.IngestDimensionRequest{
		Dimension: "1D_SEQUENCE",
		Payload: map[string]any{
			"action":    "WRITE",
			"file_path": relPath,
			"size":      len(data),
			"sha256":    sha,
			"summary":   fmt.Sprintf("WRITE %s (%d bytes) SHA:%s", relPath, len(data), sha),
		},
	})

	// 2D Surface: Tabular project manifest
	_, _ = client.IngestDimension(ctx, icx.IngestDimensionRequest{
		Dimension: "2D_TABULAR",
		Payload: map[string]any{
			"path":   relPath,
			"size":   len(data),
			"sha256": sha,
		},
	})

	// 3D Surface: Volumetric relations & Polyglot AST ingestion (Go, Python, JS/TS, HTML)
	_ = client.IngestPolyglotAST(relPath, data)

	// 4D Surface: Spatiotemporal CAS Variables
	if activeProj != "" {
		_, _ = client.VarSet(ctx, icx.VarSetRequest{
			Key:   "swarm.active_project",
			Value: activeProj,
		})
	}
	_, _ = client.VarSet(ctx, icx.VarSetRequest{
		Key:   "swarm.active_files",
		Value: fileList,
	})
	_, _ = client.VarSet(ctx, icx.VarSetRequest{
		Key:   "swarm.last_seal",
		Value: sha,
	})
}

// RecordWrite registers a committed file modification across ICX memory surfaces.
func (s *Session) RecordWrite(relPath string, data []byte, sha string) {
	s.recordWriteToICXSurfaces(relPath, data, sha)
}

// isCodeFileForWriteGuard returns true if the file extension indicates a code file
// that should be protected by the destructive write guard. Non-code files (HTML, JSON,
// CSS, markdown, config files) are excluded since complete rewrites are normal.
func isCodeFileForWriteGuard(relPath string) bool {
	ext := strings.ToLower(filepath.Ext(relPath))
	switch ext {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java", ".kt", ".c", ".cpp", ".h", ".cs", ".rb", ".swift":
		return true
	default:
		return false
	}
}
