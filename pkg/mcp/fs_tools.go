// SPDX-License-Identifier: Apache-2.0
// Package mcp implements the Model Context Protocol (MCP) JSON-RPC 2.0 stdio server,
// tool registry, and sandboxed filesystem/git tools for OmniCoder-ICX.

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go/ast"
	"go/parser"
	"go/token"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

var defaultIgnoredDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".agents":      true,
	".omnicoder":   true,
	".cache":       true,
	".vscode":      true,
}

var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// ReadFileResult represents the structured result of reading a file.
type ReadFileResult struct {
	Next       string `json:"next,omitempty"`
	Path       string `json:"path"`
	Content    string `json:"content"`
	LineCount  int    `json:"line_count"`
	ByteSize   int64  `json:"byte_size"`
	TotalLines int    `json:"total_lines"`
	TotalBytes int64  `json:"total_bytes"`
	StartLine  int    `json:"start_line,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// WriteFileResult represents the structured outcome of writing a file.
type WriteFileResult struct {
	Next              string `json:"next,omitempty"`
	Path              string `json:"path"`
	BytesWritten      int    `json:"bytes_written"`
	SHA256            string `json:"sha256"`
	Status            string `json:"status"` // "WRITTEN"
	SyntaxWarning     string `json:"syntax_warning,omitempty"`
	OverwroteExisting bool   `json:"overwrote_existing,omitempty"`
	PreviousLines     int    `json:"previous_lines,omitempty"`
}

// DirectoryEntry represents a file or folder discovered in a directory listing.
type DirectoryEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"` // RFC3339
}

// Tool Schemas (Draft 2020-12 compatible)
var ReadFileSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "Path to the file to read, relative to workspace root.",
		},
		"offset": map[string]any{
			"type":        "integer",
			"description": "Optional 1-based line number to start reading from (default: 1). (Aliases: start_line, line_start).",
		},
		"limit": map[string]any{
			"type":        "integer",
			"description": "Optional maximum number of lines to read.",
		},
		"start_line": map[string]any{
			"type":        "integer",
			"description": "Optional 1-based start line number (alias for offset).",
		},
		"end_line": map[string]any{
			"type":        "integer",
			"description": "Optional 1-based inclusive end line number.",
		},
		"workspace": map[string]any{
			"type":        "string",
			"description": "Optional explicit workspace root directory override.",
		},
	},
	"required": []string{"path"},
}

var WriteFileSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "Path to the file to write, relative to workspace root.",
		},
		"content": map[string]any{
			"type":        "string",
			"description": "Content to write into the file.",
		},
		"overwrite": map[string]any{
			"type":        "boolean",
			"description": "Whether to overwrite the file if it already exists (default: true).",
		},
		"workspace": map[string]any{
			"type":        "string",
			"description": "Optional explicit workspace root directory override.",
		},
	},
	"required": []string{"path", "content"},
}

var ListDirectorySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "Path of directory to list, relative to workspace root (default: '.').",
		},
		"recursive": map[string]any{
			"type":        "boolean",
			"description": "Whether to recursively list subdirectories (default: false).",
		},
		"max_depth": map[string]any{
			"type":        "integer",
			"description": "Maximum directory traversal depth (default: 1 for flat, 10 for recursive).",
		},
		"workspace": map[string]any{
			"type":        "string",
			"description": "Optional explicit workspace root directory override.",
		},
	},
}

// RegisterFSTools registers read_file, write_file, and list_directory on the given server.
func RegisterFSTools(s *Server, workspaceDir string) {
	if workspaceDir != "" && s.WorkspaceDir() == "" {
		_ = s.SetWorkspace(workspaceDir)
	}

	defRead, _ := NewReadFileTool(s.WorkspaceDir())
	_ = s.RegisterTool(defRead, func(ctx context.Context, args map[string]any) (any, error) {
		p, _ := args["path"].(string)
		ws := s.ResolveEffectiveWorkspace(args, p)
		return HandleReadFile(ctx, ws, args)
	})

	defWrite, _ := NewWriteFileTool(s.WorkspaceDir())
	_ = s.RegisterTool(defWrite, func(ctx context.Context, args map[string]any) (any, error) {
		p, _ := args["path"].(string)
		ws := s.ResolveEffectiveWorkspace(args, p)
		return HandleWriteFile(ctx, ws, args)
	})

	defList, _ := NewListDirectoryTool(s.WorkspaceDir())
	_ = s.RegisterTool(defList, func(ctx context.Context, args map[string]any) (any, error) {
		p, _ := args["path"].(string)
		ws := s.ResolveEffectiveWorkspace(args, p)
		return HandleListDirectory(ctx, ws, args)
	})
}

// NewReadFileTool returns the definition and handler for read_file.
func NewReadFileTool(workspaceDir string) (ToolDefinition, ToolHandler) {
	return ToolDefinition{
		Name:        "read_file",
		Description: "Reads file contents from within the workspace with optional line offset and limit.",
		InputSchema: ReadFileSchema,
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return HandleReadFile(ctx, workspaceDir, args)
	}
}

// NewWriteFileTool returns the definition and handler for write_file.
func NewWriteFileTool(workspaceDir string) (ToolDefinition, ToolHandler) {
	return ToolDefinition{
		Name:        "write_file",
		Description: "Writes full file content to a file in the workspace with automatic parent directory creation and safe atomic sync. CAUTION: Overwrites the ENTIRE file. To edit or replace a section of an existing file, use 'omnicoder_patch' with {path, old, new} instead.",
		InputSchema: WriteFileSchema,
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return HandleWriteFile(ctx, workspaceDir, args)
	}
}

// NewListDirectoryTool returns the definition and handler for list_directory.
func NewListDirectoryTool(workspaceDir string) (ToolDefinition, ToolHandler) {
	return ToolDefinition{
		Name:        "list_directory",
		Description: "Lists directory contents with optional recursion, filtering out noisy paths (.git, node_modules, vendor, .agents).",
		InputSchema: ListDirectorySchema,
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return HandleListDirectory(ctx, workspaceDir, args)
	}
}

func isWindowsSharingOrAccessViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "access is denied") ||
		strings.Contains(msg, "sharing violation") ||
		strings.Contains(msg, "being used by another process")
}

// HandleReadFile reads a file within the workspace with optional offset and limit.
func HandleReadFile(ctx context.Context, workspaceDir string, args map[string]any) (*ReadFileResult, error) {
	pathStr, err := getStringArg(args, "path", true)
	if err != nil {
		return nil, err
	}
	pathStr = CleanLLMPathArtifacts(pathStr)
	offset := getOptionalIntArg(args, "offset", 0)
	if offset <= 0 {
		for _, k := range []string{"start_line", "line_start", "lineStart", "StartLine"} {
			if v := getOptionalIntArg(args, k, 0); v > 0 {
				offset = v
				break
			}
		}
	}
	if offset <= 0 {
		offset = 1
	}
	limit := getOptionalIntArg(args, "limit", -1)
	if limit <= 0 {
		for _, k := range []string{"line_count", "max_lines", "count"} {
			if v := getOptionalIntArg(args, k, 0); v > 0 {
				limit = v
				break
			}
		}
	}
	if limit <= 0 {
		for _, k := range []string{"end_line", "line_end", "lineEnd", "EndLine"} {
			if endLine := getOptionalIntArg(args, k, 0); endLine > 0 && endLine >= offset {
				limit = endLine - offset + 1
				break
			}
		}
	}

	targetPath, err := ResolveSecurePath(workspaceDir, pathStr)
	if err != nil {
		return nil, err
	}

	var info os.FileInfo
	var statErr error
	for attempt := 0; attempt < 12; attempt++ {
		info, statErr = os.Stat(targetPath)
		if statErr == nil {
			break
		}
		if isWindowsSharingOrAccessViolation(statErr) || (attempt < 10 && os.IsNotExist(statErr) && atomicWriteInFlight.Load() > 0) {
			time.Sleep(time.Duration((attempt+1)*5) * time.Millisecond)
			continue
		}
		break
	}
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, fmt.Errorf("%w: %s", ErrFileNotFound, pathStr)
		}
		return nil, statErr
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: path '%s' is a directory, not a file", ErrInvalidPath, pathStr)
	}

	var data []byte
	var readErr error
	for attempt := 0; attempt < 12; attempt++ {
		data, readErr = os.ReadFile(targetPath)
		if readErr == nil {
			break
		}
		if isWindowsSharingOrAccessViolation(readErr) || (attempt < 10 && os.IsNotExist(readErr) && atomicWriteInFlight.Load() > 0) {
			time.Sleep(time.Duration((attempt+1)*5) * time.Millisecond)
			continue
		}
		break
	}
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, fmt.Errorf("%w: %s", ErrFileNotFound, pathStr)
		}
		return nil, readErr
	}

	if len(data) == 0 {
		return &ReadFileResult{
			Path:       filepath.ToSlash(pathStr),
			Content:    "",
			LineCount:  0,
			ByteSize:   0,
			TotalLines: 0,
			TotalBytes: 0,
		}, nil
	}

	rawStr := string(data)
	normalized := strings.ReplaceAll(rawStr, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	totalLines := len(lines)
	totalBytes := int64(len(data))

	if offset < 1 {
		offset = 1
	}

	if offset > totalLines {
		return &ReadFileResult{
			Path:       filepath.ToSlash(pathStr),
			Content:    "",
			LineCount:  0,
			ByteSize:   0,
			TotalLines: totalLines,
			TotalBytes: totalBytes,
		}, nil
	}

	startIndex := offset - 1
	endIndex := totalLines
	if limit > 0 && startIndex+limit < endIndex {
		endIndex = startIndex + limit
	}

	selectedLines := lines[startIndex:endIndex]
	content := strings.Join(selectedLines, "\n")

	return &ReadFileResult{
		Path:       filepath.ToSlash(pathStr),
		Content:    content,
		LineCount:  len(selectedLines),
		ByteSize:   int64(len(content)),
		TotalLines: totalLines,
		TotalBytes: totalBytes,
		StartLine:  offset,
	}, nil
}

var (
	fileWriteMu         sync.Mutex
	atomicWriteInFlight atomic.Int32
)

func atomicCommitFile(tmpName, targetPath string) error {
	atomicWriteInFlight.Add(1)
	defer atomicWriteInFlight.Add(-1)

	fileWriteMu.Lock()
	defer fileWriteMu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 12; attempt++ {
		err := os.Rename(tmpName, targetPath)
		if err == nil {
			return nil
		}
		lastErr = err

		if runtime.GOOS == "windows" {
			bakName := fmt.Sprintf("%s.tmp-bak-%d", targetPath, time.Now().UnixNano())
			if rErr := os.Rename(targetPath, bakName); rErr == nil {
				if renErr := os.Rename(tmpName, targetPath); renErr == nil {
					_ = os.Remove(bakName)
					return nil
				}
				_ = os.Rename(bakName, targetPath)
			}
		}

		time.Sleep(time.Duration((attempt+1)*15) * time.Millisecond)
	}
	return fmt.Errorf("failed to commit file: %w", lastErr)
}

// HandleWriteFile writes a file atomically within the workspace.
func HandleWriteFile(ctx context.Context, workspaceDir string, args map[string]any) (*WriteFileResult, error) {
	pathStr, err := getStringArg(args, "path", true)
	if err != nil {
		return nil, err
	}
	pathStr = CleanLLMPathArtifacts(pathStr)
	content, err := getStringArg(args, "content", true)
	if err != nil {
		return nil, err
	}
	overwrite := getOptionalBoolArg(args, "overwrite", true)

	targetPath, err := ResolveSecurePath(workspaceDir, pathStr)
	if err != nil {
		return nil, err
	}

	overwroteExisting := false
	previousLines := 0
	if _, err := os.Stat(targetPath); err == nil {
		if !overwrite {
			return nil, fmt.Errorf("%w: %s (use overwrite=true to replace)", ErrFileExists, pathStr)
		}
		overwroteExisting = true
		// Preserve CRLF if existing file used CRLF
		if raw, readErr := os.ReadFile(targetPath); readErr == nil {
			rawStr := string(raw)
			if len(strings.TrimSpace(rawStr)) > 0 {
				previousLines = strings.Count(strings.ReplaceAll(rawStr, "\r\n", "\n"), "\n") + 1
			}
			if strings.Contains(rawStr, "\r\n") && !strings.Contains(content, "\r\n") {
				content = strings.ReplaceAll(content, "\n", "\r\n")
			}
			force := getOptionalBoolArg(args, "force", false)
			if !force && len(rawStr) > 400 && len(strings.TrimSpace(content)) > 20 && len(content) < len(rawStr)/2 {
				normRaw := canonical.NormalizeLF(rawStr)
				normContent := canonical.NormalizeLF(strings.TrimSpace(content))
				if strings.Contains(normRaw, normContent) {
					return nil, fmt.Errorf("code preservation guard: write_file rejected accidental partial section overwrite on %s (content is an existing %d-byte substring of %d-byte file). To edit a section, use omnicoder_patch {path, old, new}. To force full overwrite, pass force=true", pathStr, len(content), len(rawStr))
				}
			}
		}
	}

	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create parent directories: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, ".tmp-omnicoder-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	n, err := tmpFile.WriteString(content)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("failed to write to temp file: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("failed to sync temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := atomicCommitFile(tmpName, targetPath); err != nil {
		return nil, err
	}
	committed = true

	sha := canonical.SHA256Bytes([]byte(content))
	syntaxWarn := checkWrittenFileSyntaxAndCollisions(targetPath, pathStr, content)

	return &WriteFileResult{
		Path:              filepath.ToSlash(pathStr),
		BytesWritten:      n,
		SHA256:            sha,
		Status:            "WRITTEN",
		SyntaxWarning:     syntaxWarn,
		OverwroteExisting: overwroteExisting,
		PreviousLines:     previousLines,
	}, nil
}

// checkWrittenFileSyntaxAndCollisions runs a fast (<1ms) in-memory syntax and package collision check.
func checkWrittenFileSyntaxAndCollisions(targetPath, relPath, content string) string {
	if _, _, fastErr := devops.ValidateCandidatePatchFast(relPath, "", content); fastErr != nil {
		return fastErr.Error()
	}
	if strings.HasSuffix(strings.ToLower(targetPath), ".go") && !strings.HasSuffix(strings.ToLower(targetPath), "_test.go") {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, targetPath, content, 0)
		if err != nil {
			return err.Error()
		}
		hasMain := false
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name != nil && fn.Name.Name == "main" {
				hasMain = true
				break
			}
		}
		if hasMain {
			dir := filepath.Dir(targetPath)
			base := filepath.Base(targetPath)
			if entries, rErr := os.ReadDir(dir); rErr == nil {
				for _, e := range entries {
					if e.IsDir() || e.Name() == base || !strings.HasSuffix(strings.ToLower(e.Name()), ".go") || strings.HasSuffix(strings.ToLower(e.Name()), "_test.go") {
						continue
					}
					otherF, pErr := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
					if pErr != nil {
						continue
					}
					for _, decl := range otherF.Decls {
						if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name != nil && fn.Name.Name == "main" {
							return fmt.Sprintf("duplicate func main() in directory (also declared in %s); remove duplicate main or place in a separate subdirectory", e.Name())
						}
					}
				}
			}
		}
	}
	return ""
}

// HandleListDirectory lists directory contents within the workspace.
func HandleListDirectory(ctx context.Context, workspaceDir string, args map[string]any) ([]DirectoryEntry, error) {
	pathStr := CleanLLMPathArtifacts(getOptionalStringArg(args, "path", "."))
	if pathStr == "" {
		pathStr = "."
	}
	recursive := getOptionalBoolArg(args, "recursive", false)
	defaultDepth := 1
	if recursive {
		defaultDepth = 10
	}
	maxDepth := getOptionalIntArg(args, "max_depth", defaultDepth)
	if maxDepth < 1 {
		maxDepth = 1
	}

	targetDir, err := ResolveSecurePath(workspaceDir, pathStr)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(targetDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrFileNotFound, pathStr)
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: path '%s' is not a directory", ErrInvalidPath, pathStr)
	}

	rootDir, err := ResolveSecurePath(workspaceDir, ".")
	if err != nil {
		rootDir = workspaceDir
	}
	var entries []DirectoryEntry
	err = scanDirectoryTree(targetDir, rootDir, 1, maxDepth, recursive, &entries)
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	return entries, nil
}

func scanDirectoryTree(currentDir, workspaceDir string, currentDepth, maxDepth int, recursive bool, out *[]DirectoryEntry) error {
	dirEntries, err := os.ReadDir(currentDir)
	if err != nil {
		return err
	}

	for _, d := range dirEntries {
		name := d.Name()
		if defaultIgnoredDirs[name] || strings.HasPrefix(name, ".omnicoder") || strings.HasPrefix(name, "sb_") {
			continue
		}

		fullPath := filepath.Join(currentDir, name)
		relPath, err := filepath.Rel(workspaceDir, fullPath)
		if err != nil {
			relPath = fullPath
		}

		info, err := d.Info()
		if err != nil {
			continue
		}

		entry := DirectoryEntry{
			Name:    name,
			Path:    filepath.ToSlash(relPath),
			IsDir:   d.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().UTC().Format(time.RFC3339),
		}
		*out = append(*out, entry)

		if d.IsDir() && recursive && currentDepth < maxDepth {
			if d.Type()&os.ModeSymlink != 0 {
				realTarget, err := filepath.EvalSymlinks(fullPath)
				if err != nil || !isDescendantOrEqual(realTarget, workspaceDir) {
					continue
				}
			}
			if err := scanDirectoryTree(fullPath, workspaceDir, currentDepth+1, maxDepth, recursive, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// findNearestExistingAncestor traverses up the path hierarchy to find the closest
// directory that exists on disk.
func findNearestExistingAncestor(p string) (string, error) {
	curr := filepath.Clean(p)
	for {
		parent := filepath.Dir(curr)
		if parent == curr {
			return "", os.ErrNotExist
		}
		if fi, err := os.Stat(parent); err == nil && fi.IsDir() {
			return parent, nil
		}
		curr = parent
	}
}

// ResolveSecurePath validates that reqPath resolves within workspaceDir and cannot escape.
// It handles traversal attacks (../, ..\), null bytes, UNC paths, alternate data streams,
// URL-encoded traversals, Windows reserved device names, and symlink breakouts.
// CleanLLMPathArtifacts strips markdown backticks, wrapping quotes, and trailing
// shell prompt sigils (%, $) that local LLMs occasionally append to file paths.
func CleanLLMPathArtifacts(raw string) string {
    if strings.ContainsRune(raw, 0) {
        return raw
    }
    s := strings.TrimSpace(raw)
    for len(s) > 0 {
        prev := s
        s = strings.Trim(s, "`\"'")
        s = strings.TrimSpace(s)
        if strings.HasSuffix(s, "%") || strings.HasSuffix(s, "$") {
            s = strings.TrimRight(s, "%$")
            s = strings.TrimSpace(s)
        }
        if s == prev {
            break
        }
    }
    return s
}

// ResolveSecurePath validates that reqPath resolves within workspaceDir and cannot escape.
// It handles traversal attacks (../, ..\), null bytes, UNC paths, alternate data streams,
// URL-encoded traversals, Windows reserved device names, and symlink breakouts.
func ResolveSecurePath(workspaceDir, reqPath string) (string, error) {
    if workspaceDir == "" {
        return "", fmt.Errorf("%w: workspace directory cannot be empty", ErrInvalidPath)
    }
    reqPath = CleanLLMPathArtifacts(reqPath)
    if reqPath == "" {
        return "", fmt.Errorf("%w: requested path cannot be empty", ErrInvalidPath)
    }

	// 1. Poison Null Byte Check & Traversal Sequence Check
	if strings.ContainsRune(reqPath, 0) {
		return "", fmt.Errorf("%w: path contains null byte", ErrPathTraversal)
	}
	if strings.Contains(reqPath, "..") {
		return "", fmt.Errorf("%w: path contains traversal sequence '..'", ErrPathTraversal)
	}

	// 2. Reject UNC network paths
	if strings.HasPrefix(reqPath, `\\`) || strings.HasPrefix(reqPath, `//`) {
		return "", fmt.Errorf("%w: UNC network paths not allowed", ErrPathTraversal)
	}

	// 3. Reject Windows Alternate Data Streams (":")
	vol := filepath.VolumeName(reqPath)
	body := reqPath[len(vol):]
	if strings.Contains(body, ":") {
		return "", fmt.Errorf("%w: alternate data streams not allowed", ErrPathTraversal)
	}

	// 4. Multi-pass URL-Encoded Traversal Detection
	curr := reqPath
	for i := 0; i < 5; i++ {
		if !strings.Contains(curr, "%") {
			break
		}
		unescaped, err := url.PathUnescape(curr)
		if err != nil {
			return "", fmt.Errorf("%w: malformed URL-encoded path (%s): %v", ErrPathTraversal, reqPath, err)
		}
		if unescaped == curr {
			break
		}
		curr = unescaped
		if strings.ContainsRune(curr, 0) || strings.Contains(curr, "..") {
			return "", fmt.Errorf("%w: URL-encoded traversal detected (%s)", ErrPathTraversal, reqPath)
		}
	}

	// 5. Canonicalize workspace root
	absWorkDir, err := filepath.Abs(filepath.Clean(workspaceDir))
	if err != nil {
		return "", fmt.Errorf("%w: invalid workspace dir: %v", ErrInvalidPath, err)
	}
	realWorkDir, err := filepath.EvalSymlinks(absWorkDir)
	if err != nil {
		realWorkDir = absWorkDir
	}

	// 6. Lexical candidate construction
	nativeReq := filepath.FromSlash(reqPath)
	var candidate string
	if filepath.IsAbs(nativeReq) || isExplicitRootPath(nativeReq) {
		candidate = filepath.Clean(nativeReq)
		if isDescendantOrEqual(candidate, absWorkDir) {
			if rel, errRel := filepath.Rel(absWorkDir, candidate); errRel == nil {
				candidate = filepath.Clean(filepath.Join(realWorkDir, rel))
			}
		}
	} else {
		candidate = filepath.Clean(filepath.Join(realWorkDir, nativeReq))
	}

	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrPathTraversal, err)
	}

	// 7. Lexical boundary verification
	if !isDescendantOrEqual(absCandidate, realWorkDir) {
		return "", fmt.Errorf("%w: path '%s' escapes workspace boundary '%s'", ErrPathTraversal, reqPath, realWorkDir)
	}

	// 8. Physical symlink verification
	realTarget, err := filepath.EvalSymlinks(absCandidate)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
			// Target does not exist (e.g. write_file creating new file, or file being replaced in a race).
			// Check if candidate itself is a dangling symlink.
			if fi, lErr := os.Lstat(absCandidate); lErr == nil && fi.Mode()&os.ModeSymlink != 0 {
				linkTarget, rErr := os.Readlink(absCandidate)
				if rErr == nil {
					var resolvedLink string
					if filepath.IsAbs(linkTarget) {
						resolvedLink = filepath.Clean(linkTarget)
					} else {
						resolvedLink = filepath.Clean(filepath.Join(filepath.Dir(absCandidate), linkTarget))
					}
					if !isDescendantOrEqual(resolvedLink, realWorkDir) {
						return "", fmt.Errorf("%w: symlink '%s' targets outside workspace (%s)", ErrSymlinkBreakout, reqPath, resolvedLink)
					}
				}
			}

			// Verify nearest existing ancestor directory
			ancestor, aErr := findNearestExistingAncestor(absCandidate)
			if aErr == nil {
				realAncestor, sErr := filepath.EvalSymlinks(ancestor)
				if sErr != nil {
					return "", fmt.Errorf("%w: failed resolving ancestor symlink: %v", ErrSymlinkBreakout, sErr)
				}
				if !isDescendantOrEqual(realAncestor, realWorkDir) {
					return "", fmt.Errorf("%w: ancestor directory symlink escapes workspace (%s)", ErrSymlinkBreakout, realAncestor)
				}
			}
		} else {
			return "", fmt.Errorf("%w: broken symlink or inaccessible: %v", ErrSymlinkBreakout, err)
		}
	} else {
		if !isDescendantOrEqual(realTarget, realWorkDir) {
			return "", fmt.Errorf("%w: symlink '%s' targets outside workspace (%s)", ErrSymlinkBreakout, reqPath, realTarget)
		}
		absCandidate = realTarget
	}

	// 9. Windows Reserved Device Name Verification (cross-platform hygiene)
	cleanReq := filepath.Clean(reqPath)
	parts := strings.FieldsFunc(cleanReq, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	for _, part := range parts {
		upperPart := strings.ToUpper(part)
		partExt := filepath.Ext(upperPart)
		partNoExt := strings.TrimSuffix(upperPart, partExt)
		if windowsReservedNames[upperPart] || windowsReservedNames[partNoExt] {
			return "", fmt.Errorf("%w: Windows reserved device name '%s'", ErrInvalidPath, part)
		}
	}

	return absCandidate, nil
}

// SanitizePath is an alias for ResolveSecurePath.
func SanitizePath(workspaceDir, reqPath string) (string, error) {
	return ResolveSecurePath(workspaceDir, reqPath)
}

func isExplicitRootPath(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return true
	}
	if len(p) >= 2 && p[1] == ':' {
		return true
	}
	return false
}

func isDescendantOrEqual(target, base string) bool {
	target = filepath.Clean(target)
	base = filepath.Clean(base)

	if runtime.GOOS == "windows" {
		targetLower := strings.ToLower(target)
		baseLower := strings.ToLower(base)
		rel, err := filepath.Rel(baseLower, targetLower)
		if err != nil {
			return false
		}
		if rel == "." {
			return true
		}
		return !strings.HasPrefix(rel, "..") && !strings.HasPrefix(rel, string(filepath.Separator)) && !filepath.IsAbs(rel)
	}

	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..") && !strings.HasPrefix(rel, string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Argument coercion helpers

func getStringArg(args map[string]any, key string, required bool) (string, error) {
	val, ok := args[key]
	if !ok || val == nil {
		if required {
			return "", fmt.Errorf("missing required argument '%s'", key)
		}
		return "", nil
	}
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("argument '%s' must be string, got %T", key, val)
	}
	return s, nil
}

func getOptionalStringArg(args map[string]any, key, defaultVal string) string {
	val, ok := args[key]
	if !ok || val == nil {
		return defaultVal
	}
	if s, ok := val.(string); ok {
		return s
	}
	return defaultVal
}

func getOptionalIntArg(args map[string]any, key string, defaultVal int) int {
	val, ok := args[key]
	if !ok || val == nil {
		return defaultVal
	}
	switch v := val.(type) {
	case int:
		return v
	case int8:
		return int(v)
	case int16:
		return int(v)
	case int32:
		return int(v)
	case int64:
		return int(v)
	case uint:
		return int(v)
	case uint8:
		return int(v)
	case uint16:
		return int(v)
	case uint32:
		return int(v)
	case uint64:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
	}
	return defaultVal
}

func getOptionalBoolArg(args map[string]any, key string, defaultVal bool) bool {
	val, ok := args[key]
	if !ok || val == nil {
		return defaultVal
	}
	if b, ok := val.(bool); ok {
		return b
	}
	return defaultVal
}

func extractStringSlice(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	switch val := v.(type) {
	case []string:
		return val, nil
	case []any:
		res := make([]string, len(val))
		for i, item := range val {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("item %d is not string (%T)", i, item)
			}
			res[i] = s
		}
		return res, nil
	default:
		return nil, fmt.Errorf("expected string array, got %T", v)
	}
}

// getString provides a convenient getter for string arguments with optional default.
func getString(args map[string]any, key string, defaultVal ...string) string {
	def := ""
	if len(defaultVal) > 0 {
		def = defaultVal[0]
	}
	return getOptionalStringArg(args, key, def)
}

// getInt provides a convenient getter for integer arguments with optional default.
func getInt(args map[string]any, key string, defaultVal ...int) int {
	def := 0
	if len(defaultVal) > 0 {
		def = defaultVal[0]
	}
	return getOptionalIntArg(args, key, def)
}

// getBool provides a convenient getter for boolean arguments with optional default.
func getBool(args map[string]any, key string, defaultVal ...bool) bool {
	def := false
	if len(defaultVal) > 0 {
		def = defaultVal[0]
	}
	return getOptionalBoolArg(args, key, def)
}

// getStringSlice provides a convenient getter for string slice arguments.
func getStringSlice(args map[string]any, key string) ([]string, error) {
	val, ok := args[key]
	if !ok || val == nil {
		return nil, nil
	}
	return extractStringSlice(val)
}
