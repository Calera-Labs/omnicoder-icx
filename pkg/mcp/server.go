// SPDX-License-Identifier: Apache-2.0
// Package mcp implements the Model Context Protocol (MCP) JSON-RPC 2.0 stdio server,
// tool registry, and sandboxed filesystem/git tools for OmniCoder-ICX.

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is an MCP JSON-RPC 2.0 server managing tool registration, schema validation,
// and bidirectional stdio/stream communication.
type Server struct {
	workspaceDir     string
	rootWorkspaceDir string
	workMu           sync.RWMutex
	workspaceHooks   []func(newDir string)

	name         string
	version      string
	protoVersion string

	mu       sync.Mutex // Serializes output stream writes
	out      io.Writer
	toolMu   sync.RWMutex
	tools    map[string]ToolHandler
	toolDefs map[string]ToolDefinition
	toolList []ToolDefinition

	initialized bool
	initMu      sync.RWMutex

	contentLengthFraming bool
	autoFraming          bool
	msgWg                sync.WaitGroup

	tracer TraceLogger
}

// Option configures Server parameters.
type Option func(*Server)

// WithWorkspace sets the workspace root directory for tools.
func WithWorkspace(dir string) Option {
	return func(s *Server) {
		s.workMu.Lock()
		defer s.workMu.Unlock()
		s.workspaceDir = dir
		if dir != "" && !IsIDEOsOrSystemDir(dir) {
			if abs, err := filepath.Abs(filepath.Clean(dir)); err == nil {
				s.rootWorkspaceDir = abs
			}
		}
	}
}

// WithServerInfo sets the server name and version reported during initialize.
func WithServerInfo(name, version string) Option {
	return func(s *Server) {
		s.name = name
		s.version = version
	}
}

// WithProtocolVersion overrides the default protocol version.
func WithProtocolVersion(protoVersion string) Option {
	return func(s *Server) {
		s.protoVersion = protoVersion
	}
}

// WithTool registers a tool definition and handler during server initialization.
func WithTool(def ToolDefinition, handler ToolHandler) Option {
	return func(s *Server) {
		_ = s.RegisterTool(def, handler)
	}
}

// WithContentLengthFraming enables Content-Length HTTP header framing on output.
func WithContentLengthFraming(enabled bool) Option {
	return func(s *Server) {
		s.contentLengthFraming = enabled
	}
}

// WithAutoFraming enables automatic detection and mirroring of client framing:
// if a client sends a Content-Length framed message, responses will be Content-Length framed.
func WithAutoFraming(enabled bool) Option {
	return func(s *Server) {
		s.autoFraming = enabled
	}
}

// WithTracer attaches a TraceLogger for auditing MCP tool calls and requests.
func WithTracer(tracer TraceLogger) Option {
	return func(s *Server) {
		s.tracer = tracer
	}
}

// WithTraceFile enables JSONL trace logging to the specified file path.
func WithTraceFile(path string, verbose bool) Option {
	return func(s *Server) {
		s.tracer = NewDefaultTracer(TracerConfig{
			FilePath: path,
			Verbose:  verbose,
		})
	}
}

// WithVerbose enables human-readable stderr trace logging.
func WithVerbose(verbose bool) Option {
	return func(s *Server) {
		if s.tracer == nil {
			s.tracer = NewDefaultTracer(TracerConfig{
				Verbose: verbose,
			})
		}
	}
}

// NewServer initializes a new Server with default filesystem and git tools registered.
func NewServer(workspaceDir string, opts ...Option) *Server {
	var rootWS string
	if workspaceDir != "" && !IsIDEOsOrSystemDir(workspaceDir) {
		if abs, err := filepath.Abs(filepath.Clean(workspaceDir)); err == nil {
			rootWS = abs
		}
	}
	s := &Server{
		workspaceDir:     workspaceDir,
		rootWorkspaceDir: rootWS,
		name:             DefaultServerName,
		version:          DefaultServerVersion,
		protoVersion:     DefaultProtocolVersion,
		tools:            make(map[string]ToolHandler),
		toolDefs:         make(map[string]ToolDefinition),
		toolList:         make([]ToolDefinition, 0),
	}

	// Register default filesystem and git tools
	RegisterFSTools(s, workspaceDir)
	RegisterGitTools(s, workspaceDir)

	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WorkspaceDir returns the configured workspace directory in a thread-safe manner.
func (s *Server) WorkspaceDir() string {
	s.workMu.RLock()
	defer s.workMu.RUnlock()
	return s.workspaceDir
}

// SetWorkspace dynamically updates the server's workspace root directory,
// validating the directory exists and invoking all registered workspace hooks.
func (s *Server) SetWorkspace(dir string) error {
	if dir == "" {
		return fmt.Errorf("workspace directory cannot be empty")
	}
	clean := filepath.Clean(dir)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return fmt.Errorf("invalid workspace path %q: %w", dir, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("workspace path does not exist %q: %w", abs, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("workspace path is not a directory: %q", abs)
	}

	s.workMu.Lock()
	oldDir := s.workspaceDir
	s.workspaceDir = abs
	if !IsIDEOsOrSystemDir(abs) {
		if s.rootWorkspaceDir == "" || IsIDEOsOrSystemDir(s.rootWorkspaceDir) || isDescendantOrEqual(s.rootWorkspaceDir, abs) {
			s.rootWorkspaceDir = abs
		} else if !isDescendantOrEqual(abs, s.rootWorkspaceDir) {
			parent := filepath.Clean(filepath.Dir(s.rootWorkspaceDir))
			if parent != s.rootWorkspaceDir && !isShellRootOrSystemDir(parent) && isDescendantOrEqual(abs, parent) {
				s.rootWorkspaceDir = parent
			} else {
				s.rootWorkspaceDir = abs
			}
		}
	}
	hooks := make([]func(string), len(s.workspaceHooks))
	copy(hooks, s.workspaceHooks)
	s.workMu.Unlock()

	if oldDir != abs {
		if s.tracer != nil {
			s.tracer.LogEvent(TraceEvent{
				Timestamp: time.Now().UTC(),
				Event:     "workspace_update",
				Workspace: abs,
				Status:    "OK",
			})
		} else {
			fmt.Fprintf(os.Stderr, "[OmniCoder-MCP] Workspace updated: %s\n", abs)
		}
		for _, hook := range hooks {
			if hook != nil {
				func() {
					defer func() { _ = recover() }()
					hook(abs)
				}()
			}
		}
	}
	return nil
}

// OnWorkspaceChanged registers a hook invoked whenever the active workspace root changes.
func (s *Server) OnWorkspaceChanged(hook func(newDir string)) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	s.workspaceHooks = append(s.workspaceHooks, hook)
}

// isShellRootOrSystemDir checks whether dir is an IDE/system/temp directory or a top-level user shell directory
// (such as ~/Desktop, ~/Documents, ~/Downloads, or ~/OneDrive) that should never be treated as a multi-project umbrella workspace.
func isShellRootOrSystemDir(dir string) bool {
	if IsIDEOsOrSystemDir(dir) {
		return true
	}
	clean := filepath.Clean(dir)
	tempRoot := filepath.Clean(os.TempDir())
	if isDescendantOrEqual(clean, tempRoot) {
		return true
	}
	if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		homeClean := filepath.Clean(homeDir)
		if strings.EqualFold(clean, homeClean) {
			return true
		}
		for _, sub := range []string{
			"Desktop",
			"Documents",
			"Downloads",
			"OneDrive",
			filepath.Join("OneDrive", "Desktop"),
			filepath.Join("OneDrive", "Documents"),
		} {
			if strings.EqualFold(clean, filepath.Join(homeClean, sub)) {
				return true
			}
		}
	}
	return false
}

// ResolveEffectiveWorkspace determines the active workspace directory for a tool call,
// honoring per-call workspace overrides (args["workspace"], args["workspace_dir"], etc.),
// recovering from IDE/OS system directories, and resolving sibling project paths under a shared parent workspace.
func (s *Server) ResolveEffectiveWorkspace(args map[string]any, pathHint string) string {
	if args != nil {
		for _, k := range []string{"workspace", "workspace_dir", "workspace_root", "workspace_path", "WorkspaceDir", "workspaceDir", "cwd"} {
			if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
				cand := filepath.Clean(strings.TrimSpace(v))
				if abs, err := filepath.Abs(cand); err == nil {
					if fi, sErr := os.Stat(abs); sErr == nil && fi.IsDir() && !IsIDEOsOrSystemDir(abs) {
						return abs
					}
				}
			}
		}
	}

	ws := s.WorkspaceDir()
	if IsIDEOsOrSystemDir(ws) {
		if pathHint != "" && filepath.IsAbs(pathHint) {
			targetDir := pathHint
			if fi, err := os.Stat(pathHint); err == nil {
				if !fi.IsDir() {
					targetDir = filepath.Dir(pathHint)
				}
			} else {
				targetDir = filepath.Dir(pathHint)
			}
			resolved := resolveWorkspaceRoot(targetDir)
			if fi, err := os.Stat(resolved); err == nil && fi.IsDir() && !IsIDEOsOrSystemDir(resolved) {
				_ = s.SetWorkspace(resolved)
				return s.WorkspaceDir()
			}
		}
		if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
			_ = s.SetWorkspace(ideWS)
			return s.WorkspaceDir()
		}
	}

	s.workMu.RLock()
	rootWS := s.rootWorkspaceDir
	s.workMu.RUnlock()

	// If pathHint is a clean relative path that does not exist inside current ws (e.g. after ws was narrowed
	// to a sub-package like cmd/server), check ancestor workspaces up to rootWS.
	if pathHint != "" && !filepath.IsAbs(pathHint) && !strings.Contains(pathHint, "..") && rootWS != "" && rootWS != ws && isDescendantOrEqual(ws, rootWS) && !isShellRootOrSystemDir(rootWS) {
		if _, err := os.Stat(filepath.Join(ws, pathHint)); os.IsNotExist(err) {
			cur := filepath.Clean(filepath.Dir(ws))
			for cur != "" && isDescendantOrEqual(cur, rootWS) {
				if _, aErr := os.Stat(filepath.Join(cur, pathHint)); aErr == nil {
					_ = s.SetWorkspace(cur)
					return s.WorkspaceDir()
				}
				next := filepath.Clean(filepath.Dir(cur))
				if next == cur {
					break
				}
				cur = next
			}
		}
	}

	// If pathHint is a clean absolute path outside current ws, check if it belongs to a valid sibling project
	// or folder under the shared umbrella/parent workspace (e.g. switching between sibling repositories under a shared parent projects directory).
	if pathHint != "" && filepath.IsAbs(pathHint) && !strings.HasPrefix(pathHint, `\\`) && !strings.HasPrefix(pathHint, "//") && !strings.Contains(pathHint, "..") {
		cleanHint := filepath.Clean(pathHint)
		if !isDescendantOrEqual(cleanHint, ws) && !IsIDEOsOrSystemDir(cleanHint) {
			var umbrellas []string
			if rootWS != "" && !isShellRootOrSystemDir(rootWS) {
				umbrellas = append(umbrellas, rootWS)
				if rParent := filepath.Clean(filepath.Dir(rootWS)); rParent != rootWS && !isShellRootOrSystemDir(rParent) {
					umbrellas = append(umbrellas, rParent)
				}
			}
			if parentWS := filepath.Clean(filepath.Dir(ws)); parentWS != ws && !isShellRootOrSystemDir(parentWS) {
				umbrellas = append(umbrellas, parentWS)
			}

			for _, umbrellaWS := range umbrellas {
				if isDescendantOrEqual(cleanHint, umbrellaWS) {
					hintDir := cleanHint
					if fi, err := os.Stat(cleanHint); err != nil || !fi.IsDir() {
						hintDir = filepath.Dir(cleanHint)
					}
					candRoot := resolveWorkspaceRoot(hintDir)
					if candRoot != "" && candRoot != umbrellaWS && isDescendantOrEqual(candRoot, umbrellaWS) && !IsIDEOsOrSystemDir(candRoot) {
						for _, marker := range []string{".git", "go.mod", "package.json", "pyproject.toml", "Cargo.toml"} {
							if _, mErr := os.Stat(filepath.Join(candRoot, marker)); mErr == nil {
								_ = s.SetWorkspace(candRoot)
								return s.WorkspaceDir()
							}
						}
					}
					// Also allow markerless sibling folders (e.g. scratch, backups, .omnicoder)
					// when inside rootWS or when umbrellaWS/ws is a confirmed project workspace.
					allowUmbrella := (rootWS != "" && isDescendantOrEqual(cleanHint, rootWS))
					if !allowUmbrella {
						if _, err := os.Stat(filepath.Join(umbrellaWS, ".omnicoder")); err == nil {
							allowUmbrella = true
						} else {
							for _, marker := range []string{".git", "go.mod", "package.json", "pyproject.toml", "Cargo.toml"} {
								if _, mErr := os.Stat(filepath.Join(ws, marker)); mErr == nil {
									allowUmbrella = true
									break
								}
							}
						}
					}
					if allowUmbrella {
						if rootWS != "" && isDescendantOrEqual(cleanHint, rootWS) && !isShellRootOrSystemDir(rootWS) {
							_ = s.SetWorkspace(rootWS)
							return s.WorkspaceDir()
						}
						_ = s.SetWorkspace(umbrellaWS)
						return s.WorkspaceDir()
					}
				}
			}
		}
	}

	return ws
}

// IsContentLengthFraming returns whether output messages use Content-Length header framing.
func (s *Server) IsContentLengthFraming() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contentLengthFraming
}

// Tracer returns the active TraceLogger, or nil if none is configured.
func (s *Server) Tracer() TraceLogger {
	return s.tracer
}

// Close shuts down the server and flushes any pending trace logs.
func (s *Server) Close() error {
	s.msgWg.Wait()
	if s.tracer != nil {
		return s.tracer.Close()
	}
	return nil
}

// SetContentLengthFraming sets whether output messages use Content-Length header framing.
func (s *Server) SetContentLengthFraming(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contentLengthFraming = enabled
}

// IsAutoFraming returns whether auto-framing is enabled.
func (s *Server) IsAutoFraming() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autoFraming
}

// SetAutoFraming sets whether auto-framing is enabled.
func (s *Server) SetAutoFraming(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoFraming = enabled
}

// RegisterTool adds a tool to the registry and updates the definition list.
func (s *Server) RegisterTool(def ToolDefinition, handler ToolHandler) error {
	if def.Name == "" {
		return fmt.Errorf("tool definition missing required 'Name'")
	}
	if handler == nil {
		return fmt.Errorf("tool handler for '%s' cannot be nil", def.Name)
	}

	s.toolMu.Lock()
	defer s.toolMu.Unlock()

	s.tools[def.Name] = handler
	s.toolDefs[def.Name] = def

	found := false
	for i, t := range s.toolList {
		if t.Name == def.Name {
			s.toolList[i] = def
			found = true
			break
		}
	}
	if !found {
		s.toolList = append(s.toolList, def)
	}

	return nil
}

// UnregisterTool removes a tool from the registry.
func (s *Server) UnregisterTool(name string) bool {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()

	if _, exists := s.tools[name]; !exists {
		return false
	}
	delete(s.tools, name)
	delete(s.toolDefs, name)

	newList := make([]ToolDefinition, 0, len(s.toolList))
	for _, t := range s.toolList {
		if t.Name != name {
			newList = append(newList, t)
		}
	}
	s.toolList = newList
	return true
}

// GetTool retrieves a tool definition and handler by name.
func (s *Server) GetTool(name string) (ToolDefinition, ToolHandler, bool) {
	s.toolMu.RLock()
	defer s.toolMu.RUnlock()

	h, ok := s.tools[name]
	if !ok {
		return ToolDefinition{}, nil, false
	}
	def, hasDef := s.toolDefs[name]
	if !hasDef {
		def = ToolDefinition{Name: name}
	}
	return def, h, true
}

// HasTool checks if a tool exists in the active registry.
func (s *Server) HasTool(name string) bool {
	s.toolMu.RLock()
	defer s.toolMu.RUnlock()
	_, ok := s.tools[name]
	return ok
}

// ListTools returns a copy of all registered tool definitions.
func (s *Server) ListTools() []ToolDefinition {
	s.toolMu.RLock()
	defer s.toolMu.RUnlock()

	res := make([]ToolDefinition, len(s.toolList))
	copy(res, s.toolList)
	return res
}

// GetToolDefinitions is an alias for ListTools.
func (s *Server) GetToolDefinitions() []ToolDefinition {
	return s.ListTools()
}

// Dispatch directly executes a registered tool with schema validation and panic recovery.
func (s *Server) Dispatch(ctx context.Context, name string, args map[string]any) (any, error) {
	def, handler, ok := s.GetTool(name)
	if !ok {
		return nil, fmt.Errorf("method not found (code %d): %s", ErrCodeMethodNotFound, name)
	}

	normalizeToolArguments(name, args)

	if def.InputSchema != nil {
		if violations := ValidateArguments(def.InputSchema, args); len(violations) > 0 {
			return nil, fmt.Errorf("schema validation failed: %s", FormatViolations(violations))
		}
	}

	var result any
	var execErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				execErr = fmt.Errorf("tool '%s' execution recovered from panic: %v", name, r)
			}
		}()
		result, execErr = handler(ctx, args)
	}()

	return result, execErr
}

// ServeStdio executes the server loop over standard input and output.
func (s *Server) ServeStdio(ctx context.Context) error {
	return s.Serve(ctx, os.Stdin, os.Stdout)
}

// Serve runs the JSON-RPC message processing loop on in/out streams until EOF or context cancellation.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.mu.Lock()
	s.out = out
	s.mu.Unlock()

	reader := bufio.NewReader(in)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msgBytes, isFramed, err := readNextMessageFramed(reader)
		if err != nil {
			if err == io.EOF {
				s.msgWg.Wait()
				if s.tracer != nil {
					_ = s.tracer.Close()
				}
				return nil
			}
			// Check if context cancelled while reading
			select {
			case <-ctx.Done():
				s.msgWg.Wait()
				if s.tracer != nil {
					_ = s.tracer.Close()
				}
				return ctx.Err()
			default:
				s.msgWg.Wait()
				if s.tracer != nil {
					_ = s.tracer.Close()
				}
				return err
			}
		}

		if isFramed && s.autoFraming {
			s.mu.Lock()
			s.contentLengthFraming = true
			s.mu.Unlock()
		}

		if len(bytes.TrimSpace(msgBytes)) == 0 {
			continue
		}

		s.msgWg.Add(1)
		go func(raw []byte) {
			defer s.msgWg.Done()
			s.handleMessage(ctx, raw)
		}(msgBytes)
	}
}

// readNextMessageFramed reads a JSON-RPC message supporting both Content-Length header framing
// and newline-delimited JSON. It returns the raw message payload, a boolean indicating whether
// Content-Length framing was detected, and any read/framing error.
func readNextMessageFramed(r *bufio.Reader) ([]byte, bool, error) {
	// 1. Skip any leading blank/whitespace lines (e.g. residual \r\n between messages or keepalive pings)
	var firstLine string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if len(line) > 0 && err == io.EOF {
				firstLine = line
				break
			}
			return nil, false, err
		}
		// Strip UTF-8 BOM if present on initial stream
		line = strings.TrimPrefix(line, "\xef\xbb\xbf")
		if strings.TrimSpace(line) != "" {
			firstLine = line
			break
		}
	}

	trimmedFirst := strings.TrimSpace(firstLine)
	if trimmedFirst == "" {
		return nil, false, io.EOF
	}

	// 2. If it begins with '{' or '[', it is newline-delimited JSON (NDJSON)
	if strings.HasPrefix(trimmedFirst, "{") || strings.HasPrefix(trimmedFirst, "[") {
		return []byte(firstLine), false, nil
	}

	// 3. Check if this is an HTTP / LSP style header block (e.g. Content-Length:, Content-Type:)
	if strings.Contains(trimmedFirst, ":") {
		contentLength := -1
		headerLines := 0

		parseHeader := func(h string) error {
			trimmed := strings.TrimSpace(h)
			if strings.HasPrefix(strings.ToLower(trimmed), "content-length:") {
				parts := strings.SplitN(trimmed, ":", 2)
				if len(parts) < 2 {
					return fmt.Errorf("malformed Content-Length header")
				}
				val := strings.TrimSpace(parts[1])
				length, err := strconv.Atoi(val)
				if err != nil {
					return fmt.Errorf("invalid Content-Length value: %w", err)
				}
				if length < 0 || length > MaxMessageSize {
					return fmt.Errorf("Content-Length %d exceeds maximum allowed buffer size (%d bytes)", length, MaxMessageSize)
				}
				contentLength = length
			}
			return nil
		}

		headerLines++
		if err := parseHeader(trimmedFirst); err != nil {
			return nil, true, err
		}

		// Read subsequent header lines until an empty line (delimited by \r\n or \n)
		for {
			headerLines++
			if headerLines > MaxHeaderLines {
				return nil, true, fmt.Errorf("exceeded maximum header line count (%d)", MaxHeaderLines)
			}
			headerLine, err := r.ReadString('\n')
			if err != nil {
				return nil, true, err
			}
			if strings.TrimSpace(headerLine) == "" {
				break
			}
			if err := parseHeader(headerLine); err != nil {
				return nil, true, err
			}
		}

		if contentLength < 0 {
			return nil, true, fmt.Errorf("header block missing Content-Length")
		}

		if contentLength == 0 {
			return []byte{}, true, nil
		}

		body := make([]byte, contentLength)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, true, err
		}
		return body, true, nil
	}

	// 4. Fallback: return line as raw bytes (handleMessage will report JSON-RPC ParseError)
	return []byte(firstLine), false, nil
}

// readNextMessage supports both Content-Length header framing and newline-delimited JSON.
func readNextMessage(r *bufio.Reader) ([]byte, error) {
	b, _, err := readNextMessageFramed(r)
	return b, err
}

// ReadNextMessageForTest exposes readNextMessage for testing purposes.
func ReadNextMessageForTest(r *bufio.Reader) ([]byte, error) {
	return readNextMessage(r)
}

// ReadNextMessageFramedForTest exposes readNextMessageFramed for testing purposes.
func ReadNextMessageFramedForTest(r *bufio.Reader) ([]byte, bool, error) {
	return readNextMessageFramed(r)
}

// handleMessage unmarshals and dispatches a JSON-RPC message.
func (s *Server) handleMessage(ctx context.Context, msgBytes []byte) {
	trimmed := bytes.TrimSpace(msgBytes)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		// Batch request handling per JSON-RPC 2.0 Section 6:
		var rawBatch []json.RawMessage
		if err := json.Unmarshal(trimmed, &rawBatch); err == nil && len(rawBatch) == 0 {
			s.sendError(nil, ErrCodeInvalidRequest, "Invalid Request: empty batch")
			return
		}
		s.sendError(nil, ErrCodeInvalidRequest, "Invalid Request: batch requests are not supported")
		return
	}

	var req JSONRPCRequest
	if err := json.Unmarshal(msgBytes, &req); err != nil {
		s.sendError(nil, ErrCodeParseError, "Parse error: "+err.Error())
		return
	}

	// Validate JSON-RPC version (must be exactly "2.0")
	if req.JSONRPC != JSONRPCVersion {
		if req.ID != nil {
			s.sendError(req.ID, ErrCodeInvalidRequest, "Invalid Request: missing or invalid jsonrpc version (must be '2.0')")
		} else {
			// Per JSON-RPC 2.0 Section 4.1: notifications without ID do not receive error replies
			// Unless the request object is completely invalid and impossible to identify as notification
			if req.Method == "" {
				s.sendError(nil, ErrCodeInvalidRequest, "Invalid Request: missing or invalid jsonrpc version (must be '2.0')")
			}
		}
		return
	}

	// Validate method is present
	if req.Method == "" {
		if req.ID != nil {
			s.sendError(req.ID, ErrCodeInvalidRequest, "Invalid Request: method is required")
		} else {
			s.sendError(nil, ErrCodeInvalidRequest, "Invalid Request: method is required")
		}
		return
	}

	// JSON-RPC 2.0 Section 4.1: The server MUST NOT reply to a Notification
	if req.IsNotification() || strings.HasPrefix(req.Method, "notifications/") || req.Method == "initialized" || req.Method == "exit" || req.Method == "$/cancelRequest" {
		if req.Method == "notifications/initialized" || req.Method == "initialized" {
			s.initMu.Lock()
			s.initialized = true
			s.initMu.Unlock()
			if IsIDEOsOrSystemDir(s.WorkspaceDir()) {
				if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
					_ = s.SetWorkspace(ideWS)
				}
			}
		} else if req.Method == "notifications/roots/list_changed" {
			if IsIDEOsOrSystemDir(s.WorkspaceDir()) {
				if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
					_ = s.SetWorkspace(ideWS)
				}
			}
		} else if req.Method == "workspace/didChangeWorkspaceFolders" {
			var changeParams struct {
				Event struct {
					Added []WorkspaceFolder `json:"added"`
				} `json:"event"`
			}
			if len(req.Params) > 0 && json.Unmarshal(req.Params, &changeParams) == nil {
				if len(changeParams.Event.Added) > 0 {
					if parsed, err := URItoPath(changeParams.Event.Added[0].URI); err == nil && parsed != "" {
						_ = s.SetWorkspace(parsed)
					}
				}
			}
		}
		return
	}

	defer func() {
		if r := recover(); r != nil {
			if req.ID != nil {
				s.sendError(req.ID, ErrCodeInternalError, fmt.Sprintf("Internal error: %v", r))
			}
		}
	}()

	switch req.Method {
	case "initialize":
		protoVersion := s.protoVersion
		var targetWS string
		if len(req.Params) > 0 {
			targetWS = ExtractWorkspaceFromHandshake(req.Params)
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(req.Params, &p); err == nil && p.ProtocolVersion != "" {
				protoVersion = p.ProtocolVersion
			}
		}

		if targetWS != "" {
			_ = s.SetWorkspace(targetWS)
		} else if s.WorkspaceDir() == "" || IsIDEOsOrSystemDir(s.WorkspaceDir()) {
			// If no workspace was sent during handshake and server workspace is empty or an IDE/system dir,
			// automatically discover active project from IDE editor state
			if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" && ideWS != s.WorkspaceDir() {
				_ = s.SetWorkspace(ideWS)
			}
		}

		res := InitializeResult{
			ProtocolVersion: protoVersion,
			Capabilities: ServerCapabilities{
				Tools: &ToolsCapability{
					ListChanged: true,
				},
				Resources: &ResourcesCapability{
					Subscribe:   false,
					ListChanged: false,
				},
				Prompts: &PromptsCapability{
					ListChanged: false,
				},
				Logging: map[string]any{},
			},
			ServerInfo: ServerInfo{
				Name:    s.name,
				Version: s.version,
			},
		}
		s.sendResponse(req.ID, res)

	case "ping":
		s.sendResponse(req.ID, map[string]any{})

	case "logging/setLevel":
		var p struct {
			Level string `json:"level"`
		}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &p)
		}
		if s.tracer != nil && p.Level != "" {
			s.tracer.SetLevel(ParseLogLevel(p.Level))
		}
		s.sendResponse(req.ID, map[string]any{})

	case "roots/list":
		currentWS := s.WorkspaceDir()
		s.sendResponse(req.ID, RootsListResult{
			Roots: []WorkspaceRoot{
				{
					URI:  "file:///" + filepath.ToSlash(currentWS),
					Name: filepath.Base(currentWS),
				},
			},
		})

	case "tools/list":
		s.sendResponse(req.ID, ToolListResult{
			Tools: s.ListTools(),
		})

	case "tools/call":
		s.handleToolCall(ctx, &req)

	default:
		s.sendError(req.ID, ErrCodeMethodNotFound, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

// handleToolCall validates and invokes a registered tool.
func (s *Server) handleToolCall(ctx context.Context, req *JSONRPCRequest) {
	start := time.Now()
	var params CallToolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.sendError(req.ID, ErrCodeInvalidParams, "Invalid params: "+err.Error())
		if s.tracer != nil {
			s.tracer.LogEvent(TraceEvent{
				Timestamp:  start,
				Event:      "tool_call",
				RequestID:  req.ID,
				Method:     "tools/call",
				DurationMS: time.Since(start).Milliseconds(),
				Status:     "ERROR",
				Error:      "Invalid params: " + err.Error(),
				Workspace:  s.WorkspaceDir(),
			})
		}
		return
	}

	def, handler, ok := s.GetTool(params.Name)
	if !ok {
		s.sendError(req.ID, ErrCodeMethodNotFound, fmt.Sprintf("Tool not found: %s", params.Name))
		if s.tracer != nil {
			s.tracer.LogEvent(TraceEvent{
				Timestamp:  start,
				Event:      "tool_call",
				RequestID:  req.ID,
				Method:     "tools/call",
				Tool:       params.Name,
				Arguments:  params.Arguments,
				DurationMS: time.Since(start).Milliseconds(),
				Status:     "ERROR",
				Error:      fmt.Sprintf("Tool not found: %s", params.Name),
				Workspace:  s.WorkspaceDir(),
			})
		}
		return
	}

	// Normalize common parameter aliases from diverse AI agents before schema validation
	normalizeToolArguments(params.Name, params.Arguments)

	// Draft 2020-12 Schema Validation Middleware
	if def.InputSchema != nil {
		if violations := ValidateArguments(def.InputSchema, params.Arguments); len(violations) > 0 {
			errMsg := fmt.Sprintf("Invalid arguments for tool '%s': %s", params.Name, FormatViolations(violations))
			s.sendResponse(req.ID, CallToolResult{
				Content: []Content{
					TextContent(errMsg),
				},
				IsError: true,
			})
			if s.tracer != nil {
				s.tracer.LogEvent(TraceEvent{
					Timestamp:  start,
					Event:      "tool_call",
					RequestID:  req.ID,
					Method:     "tools/call",
					Tool:       params.Name,
					Arguments:  params.Arguments,
					DurationMS: time.Since(start).Milliseconds(),
					Status:     "VALIDATION_ERROR",
					Error:      errMsg,
					Workspace:  s.WorkspaceDir(),
				})
			}
			return
		}
	}

	// Inject progress token if present
	callCtx := ctx
	if progToken := params.GetProgressToken(); progToken != nil {
		callCtx = WithProgressTokenContext(ctx, progToken)
	}

	// Execute tool handler with panic recovery
	var result any
	var execErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				execErr = fmt.Errorf("tool '%s' execution recovered from panic: %v", params.Name, r)
			}
		}()
		result, execErr = handler(callCtx, params.Arguments)
	}()

	durationMS := time.Since(start).Milliseconds()

	if execErr != nil {
		errMsg := fmt.Sprintf("Error executing tool '%s': %v", params.Name, execErr)
		s.sendResponse(req.ID, CallToolResult{
			Content: []Content{
				TextContent(errMsg),
			},
			IsError: true,
		})
		if s.tracer != nil {
			status := "ERROR"
			if strings.Contains(execErr.Error(), "recovered from panic") {
				status = "PANIC"
			}
			s.tracer.LogEvent(TraceEvent{
				Timestamp:  start,
				Event:      "tool_call",
				RequestID:  req.ID,
				Method:     "tools/call",
				Tool:       params.Name,
				Arguments:  params.Arguments,
				DurationMS: durationMS,
				Status:     status,
				Error:      errMsg,
				Workspace:  s.WorkspaceDir(),
			})
		}
		return
	}

	// Format result payload & track response metrics
	var isToolError bool
	var toolErrText string
	var resultBytes int

	switch res := result.(type) {
	case CallToolResult:
		isToolError = res.IsError
		if isToolError && len(res.Content) > 0 {
			toolErrText = res.Content[0].Text
		}
		s.sendResponse(req.ID, res)
	case *CallToolResult:
		if res != nil {
			isToolError = res.IsError
			if isToolError && len(res.Content) > 0 {
				toolErrText = res.Content[0].Text
			}
			s.sendResponse(req.ID, *res)
		} else {
			s.sendResponse(req.ID, CallToolResult{Content: []Content{}, IsError: false})
		}
	case []Content:
		s.sendResponse(req.ID, CallToolResult{Content: res, IsError: false})
	case Content:
		s.sendResponse(req.ID, CallToolResult{Content: []Content{res}, IsError: false})
	case string:
		resultBytes = len(res)
		s.sendResponse(req.ID, CallToolResult{
			Content: []Content{TextContent(res)},
			IsError: false,
		})
	default:
		outJSON, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			outJSON, _ = json.Marshal(result)
		}
		resultBytes = len(outJSON)
		s.sendResponse(req.ID, CallToolResult{
			Content: []Content{TextContent(string(outJSON))},
			IsError: false,
		})
	}

	if s.tracer != nil {
		status := "OK"
		if isToolError {
			status = "ERROR"
		}
		s.tracer.LogEvent(TraceEvent{
			Timestamp:   start,
			Event:       "tool_call",
			RequestID:   req.ID,
			Method:      "tools/call",
			Tool:        params.Name,
			Arguments:   params.Arguments,
			DurationMS:  durationMS,
			Status:      status,
			Error:       toolErrText,
			ResultBytes: resultBytes,
			Workspace:   s.WorkspaceDir(),
		})
	}
}

func firstNonEmptyStringArg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstStringArgPresent(args map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := args[k].(string); ok {
			return v, true
		}
	}
	return "", false
}

// normalizeToolArguments normalizes common parameter aliases emitted by diverse AI agent frameworks
// (e.g. workspace_dir -> path, patch -> content, cmd -> command, file_path -> path) before schema validation.
func normalizeToolArguments(toolName string, args map[string]any) {
	if args == nil {
		return
	}
	switch toolName {
	case "omnicoder_set_workspace":
		if v := firstNonEmptyStringArg(args, "path", "workspace", "workspace_dir", "workspace_root", "workspace_path", "WorkspaceDir", "workspaceDir", "dir", "directory", "folder", "root", "cwd", "target"); v != "" {
			args["path"] = v
		}
	case "omnicoder_patch":
		if v := firstNonEmptyStringArg(args, "path", "file", "file_path", "filepath", "filePath", "FilePath", "target_file", "target", "TargetFile"); v != "" {
			args["path"] = v
		}
		if args["chunks"] == nil {
			for _, k := range []string{"edits", "hunks", "replacements", "changes", "ReplacementChunks"} {
				if args[k] != nil {
					args["chunks"] = args[k]
					break
				}
			}
		}
		if rawChunks, ok := args["chunks"].([]any); ok {
			for _, item := range rawChunks {
				if m, ok := item.(map[string]any); ok {
					if m["content"] == nil || m["content"] == "" {
						if rStr, hasR := firstStringArgPresent(m, "replace_block", "new_str", "new_string", "new_content", "new_text", "new", "replacement_content", "ReplacementContent", "replace", "replacement"); hasR {
							m["content"] = rStr
						}
					}
				}
			}
		}
		if args["content"] == nil || args["content"] == "" {
			searchStr, hasSearch := firstStringArgPresent(args, "search_block", "old_str", "old_string", "old_content", "old_text", "old", "target_content", "TargetContent", "search", "find")
			replaceStr, hasReplace := firstStringArgPresent(args, "replace_block", "new_str", "new_string", "new_content", "new_text", "new", "replacement_content", "ReplacementContent", "replace", "replacement")
			if hasSearch && hasReplace && searchStr != "" {
				args["content"] = fmt.Sprintf("<<<<<<< SEARCH\n%s\n=======\n%s\n>>>>>>> REPLACE", searchStr, replaceStr)
			} else if v := firstNonEmptyStringArg(args, "patch", "diff", "patch_content", "replacement", "replacement_content", "ReplacementContent", "new_content", "new_text", "new", "code"); v != "" {
				args["content"] = v
			}
		}
		if args["start_line"] == nil {
			for _, k := range []string{"line_start", "lineStart", "StartLine", "start"} {
				if args[k] != nil {
					args["start_line"] = args[k]
					break
				}
			}
		}
		if args["end_line"] == nil {
			for _, k := range []string{"line_end", "lineEnd", "EndLine", "end"} {
				if args[k] != nil {
					args["end_line"] = args[k]
					break
				}
			}
		}
	case "omnicoder_heal", "omnicoder_sandbox":
		if v := firstNonEmptyStringArg(args, "command", "cmd", "test_command", "build_command"); v != "" {
			args["command"] = v
		}
	case "omnicoder_run":
		if v := firstNonEmptyStringArg(args, "task", "command", "cmd", "prompt", "objective", "description", "query"); v != "" {
			args["task"] = v
		}
	case "omnicoder_search":
		if v := firstNonEmptyStringArg(args, "query", "pattern", "term", "search", "symbol", "keyword", "q"); v != "" {
			args["query"] = v
		}
	case "omnicoder_refactor":
		if v := firstNonEmptyStringArg(args, "symbol", "name", "identifier", "target", "query", "function"); v != "" {
			args["symbol"] = v
		}
	case "omnicoder_toggle_checklist":
		if v := firstNonEmptyStringArg(args, "path", "file_path", "filepath", "filePath", "FilePath", "file"); v != "" {
			args["path"] = v
			args["file_path"] = v
		}
		if v := firstNonEmptyStringArg(args, "item_text", "pattern", "text", "item"); v != "" {
			args["item_text"] = v
			if args["items"] == nil {
				checked := true
				if c, ok := args["checked"].(bool); ok {
					checked = c
				}
				note := firstNonEmptyStringArg(args, "note", "evidence")
				args["items"] = []any{
					map[string]any{
						"pattern": v,
						"checked": checked,
						"note":    note,
					},
				}
			}
		}
	case "omnicoder_update_section":
		if v := firstNonEmptyStringArg(args, "path", "file_path", "filepath", "filePath", "FilePath", "file"); v != "" {
			args["path"] = v
			args["file_path"] = v
		}
		if v := firstNonEmptyStringArg(args, "heading", "section", "header", "title", "section_heading"); v != "" {
			args["heading"] = v
		}
		if v, ok := firstStringArgPresent(args, "content", "replacement_text", "replacement", "body", "text", "new_content"); ok {
			args["content"] = v
			args["replacement_text"] = v
		}
	case "read_file", "write_file", "list_directory":
		if v := firstNonEmptyStringArg(args, "path", "file", "file_path", "filepath", "filePath", "FilePath", "target_file", "dir", "directory", "folder"); v != "" {
			args["path"] = v
		}
		if toolName == "read_file" {
			if args["offset"] == nil {
				for _, k := range []string{"start_line", "line_start", "lineStart", "StartLine"} {
					if v := getOptionalIntArg(args, k, 0); v > 0 {
						args["offset"] = float64(v)
						break
					}
				}
			}
			if args["limit"] == nil {
				for _, k := range []string{"line_count", "max_lines", "count"} {
					if v := getOptionalIntArg(args, k, 0); v > 0 {
						args["limit"] = float64(v)
						break
					}
				}
			}
			if args["limit"] == nil {
				for _, k := range []string{"end_line", "line_end", "lineEnd", "EndLine"} {
					if endLine := getOptionalIntArg(args, k, 0); endLine > 0 {
						startLine := getOptionalIntArg(args, "offset", 1)
						if startLine < 1 {
							startLine = 1
						}
						if endLine >= startLine {
							args["limit"] = float64(endLine - startLine + 1)
						}
						break
					}
				}
			}
		}
	case "git_status", "git_diff", "git_commit":
		if v := firstNonEmptyStringArg(args, "repo_path", "workspace", "workspace_dir", "workspace_root", "workspace_path", "cwd", "dir"); v != "" {
			args["repo_path"] = v
		}
		if toolName == "git_diff" {
			if v := firstNonEmptyStringArg(args, "path", "file", "file_path", "filepath", "filePath", "FilePath"); v != "" {
				args["path"] = v
			}
		}
	}
}

// sendResponse writes a successful JSON-RPC response.
func (s *Server) sendResponse(id any, result any) {
	resp := JSONRPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Result:  result,
	}
	s.writeJSON(&resp)
}

// sendError writes an error JSON-RPC response.
func (s *Server) sendError(id any, code int, msg string) {
	resp := JSONRPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: msg,
		},
	}
	s.writeJSON(&resp)
}

// SendNotification writes a notification JSON-RPC message (no ID).
func (s *Server) SendNotification(method string, params any) {
	notif := map[string]any{
		"jsonrpc": JSONRPCVersion,
		"method":  method,
		"params":  params,
	}
	s.writeJSON(notif)
}

// SendLogMessage emits an MCP protocol log message notification (notifications/message).
func (s *Server) SendLogMessage(level LogLevel, loggerName, message string, data any) {
	s.SendNotification("notifications/message", map[string]any{
		"level":  level.String(),
		"logger": loggerName,
		"data":   data,
		"msg":    message,
	})
}

// progressTokenKey is context key for active MCP progress token.
type progressTokenKey struct{}

// WithProgressTokenContext attaches an active MCP progress token to context.
func WithProgressTokenContext(ctx context.Context, token any) context.Context {
	if token == nil {
		return ctx
	}
	return context.WithValue(ctx, progressTokenKey{}, token)
}

// ProgressTokenFromContext extracts any progress token attached to the context.
func ProgressTokenFromContext(ctx context.Context) any {
	if ctx == nil {
		return nil
	}
	return ctx.Value(progressTokenKey{})
}

// SendProgress emits an official MCP $/progress notification to stream real-time phase updates.
func (s *Server) SendProgress(token any, progress, total float64, message string) {
	if token == nil {
		return
	}
	params := map[string]any{
		"token":         token,
		"progressToken": token,
		"progress":      progress,
		"value":         message,
		"message":       message,
	}
	if total > 0 {
		params["total"] = total
	}
	s.SendNotification("$/progress", params)
}

// SendHeartbeat emits a keepalive $/progress ping or notification during long-running tasks.
func (s *Server) SendHeartbeat(token any, message string) {
	if token != nil {
		s.SendProgress(token, 0, 0, message)
	} else {
		s.SendNotification("notifications/heartbeat", map[string]any{
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"message":   message,
		})
	}
}

// ReportProgress inspects context for an active progress token and emits a $/progress notification.
func (s *Server) ReportProgress(ctx context.Context, progress, total float64, message string) {
	token := ProgressTokenFromContext(ctx)
	if token != nil {
		s.SendProgress(token, progress, total, message)
	}
}

// writeJSON encodes and flushes a message with newline or Content-Length delimiter under mutex lock.
func (s *Server) writeJSON(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.out == nil {
		return
	}

	data, err := json.Marshal(v)
	if err != nil {
		return
	}

	if s.contentLengthFraming {
		header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
		msg := make([]byte, 0, len(header)+len(data))
		msg = append(msg, header...)
		msg = append(msg, data...)
		_, _ = s.out.Write(msg)
		return
	}

	data = append(data, '\n')
	_, _ = s.out.Write(data)
}

// ----------------------------------------------------------------------------
// Draft 2020-12 Schema Validation Middleware
// ----------------------------------------------------------------------------

// FormatViolations converts schema violations into a semi-colon joined human-readable string.
func FormatViolations(violations []SchemaViolation) string {
	var sb strings.Builder
	for i, v := range violations {
		if i > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(v.String())
	}
	return sb.String()
}

// ValidateArguments validates args against Draft 2020-12 JSON Schema.
func ValidateArguments(schema map[string]any, args map[string]any) []SchemaViolation {
	var violations []SchemaViolation
	if schema == nil {
		return nil
	}

	expectedType, _ := schema["type"].(string)
	if expectedType != "" && expectedType != "object" {
		violations = append(violations, SchemaViolation{
			Field:       "$",
			Rule:        "type",
			Message:     fmt.Sprintf("expected top-level type 'object', got '%s'", expectedType),
			ActualValue: args,
		})
		return violations
	}

	if args == nil {
		args = map[string]any{}
	}

	// Required properties check
	if rawReq, ok := schema["required"].([]any); ok {
		for _, r := range rawReq {
			if propName, ok := r.(string); ok {
				val, exists := args[propName]
				if !exists || val == nil {
					violations = append(violations, SchemaViolation{
						Field:       propName,
						Rule:        "required",
						Message:     fmt.Sprintf("missing required property '%s'", propName),
						ActualValue: nil,
					})
				}
			}
		}
	} else if strReq, ok := schema["required"].([]string); ok {
		for _, propName := range strReq {
			val, exists := args[propName]
			if !exists || val == nil {
				violations = append(violations, SchemaViolation{
					Field:       propName,
					Rule:        "required",
					Message:     fmt.Sprintf("missing required property '%s'", propName),
					ActualValue: nil,
				})
			}
		}
	}

	// Property types and constraints
	properties, _ := schema["properties"].(map[string]any)
	for propName, rawPropSchema := range properties {
		propSchema, ok := rawPropSchema.(map[string]any)
		if !ok {
			continue
		}
		val, exists := args[propName]
		if !exists || val == nil {
			continue
		}
		validateValue(propName, val, propSchema, &violations)
	}

	return violations
}

func validateValue(fieldPath string, val any, schema map[string]any, violations *[]SchemaViolation) {
	if schema == nil || val == nil {
		return
	}

	expectedType, _ := schema["type"].(string)
	if expectedType != "" && !checkType(val, expectedType) {
		*violations = append(*violations, SchemaViolation{
			Field:       fieldPath,
			Rule:        "type",
			Message:     fmt.Sprintf("expected type '%s', got %T", expectedType, val),
			ActualValue: val,
		})
		return
	}

	// Enum check
	if rawEnum, ok := schema["enum"].([]any); ok && len(rawEnum) > 0 {
		matched := false
		for _, enumVal := range rawEnum {
			if checkEqual(val, enumVal) {
				matched = true
				break
			}
		}
		if !matched {
			*violations = append(*violations, SchemaViolation{
				Field:       fieldPath,
				Rule:        "enum",
				Message:     fmt.Sprintf("value '%v' not in allowed enum values: %v", val, rawEnum),
				ActualValue: val,
			})
		}
	} else if strEnum, ok := schema["enum"].([]string); ok && len(strEnum) > 0 {
		matched := false
		strVal := fmt.Sprintf("%v", val)
		for _, s := range strEnum {
			if s == strVal {
				matched = true
				break
			}
		}
		if !matched {
			*violations = append(*violations, SchemaViolation{
				Field:       fieldPath,
				Rule:        "enum",
				Message:     fmt.Sprintf("value '%v' not in allowed enum values: %v", val, strEnum),
				ActualValue: val,
			})
		}
	}

	// String length checks
	if strVal, ok := val.(string); ok {
		if minL, ok := toFloat64(schema["minLength"]); ok && float64(len(strVal)) < minL {
			*violations = append(*violations, SchemaViolation{
				Field:       fieldPath,
				Rule:        "minLength",
				Message:     fmt.Sprintf("string length %d < minLength %d", len(strVal), int(minL)),
				ActualValue: val,
			})
		}
		if maxL, ok := toFloat64(schema["maxLength"]); ok && float64(len(strVal)) > maxL {
			*violations = append(*violations, SchemaViolation{
				Field:       fieldPath,
				Rule:        "maxLength",
				Message:     fmt.Sprintf("string length %d > maxLength %d", len(strVal), int(maxL)),
				ActualValue: val,
			})
		}
	}

	// Numeric range checks
	numVal, isNum := toFloat64(val)
	if isNum {
		if minV, ok := toFloat64(schema["minimum"]); ok && numVal < minV {
			*violations = append(*violations, SchemaViolation{
				Field:       fieldPath,
				Rule:        "minimum",
				Message:     fmt.Sprintf("value %v < minimum %v", numVal, minV),
				ActualValue: val,
			})
		}
		if maxV, ok := toFloat64(schema["maximum"]); ok && numVal > maxV {
			*violations = append(*violations, SchemaViolation{
				Field:       fieldPath,
				Rule:        "maximum",
				Message:     fmt.Sprintf("value %v > maximum %v", numVal, maxV),
				ActualValue: val,
			})
		}
	}

	// Array items check
	if itemsSchema, ok := schema["items"].(map[string]any); ok {
		if arr, ok := val.([]any); ok {
			for i, elem := range arr {
				validateValue(fmt.Sprintf("%s[%d]", fieldPath, i), elem, itemsSchema, violations)
			}
		} else {
			rv := reflect.ValueOf(val)
			if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
				for i := 0; i < rv.Len(); i++ {
					validateValue(fmt.Sprintf("%s[%d]", fieldPath, i), rv.Index(i).Interface(), itemsSchema, violations)
				}
			}
		}
	}
}

func checkType(val any, expectedType string) bool {
	switch expectedType {
	case "string":
		_, ok := val.(string)
		return ok
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "number":
		switch val.(type) {
		case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		default:
			return false
		}
	case "integer":
		switch v := val.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			return v == math.Trunc(v) && !math.IsNaN(v) && !math.IsInf(v, 0)
		default:
			return false
		}
	case "array":
		rt := reflect.TypeOf(val)
		return rt != nil && (rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array)
	case "object":
		_, ok := val.(map[string]any)
		return ok
	case "null":
		return val == nil
	default:
		return true
	}
}

func checkEqual(a, b any) bool {
	if a == b {
		return true
	}
	aNum, aIsNum := toFloat64(a)
	bNum, bIsNum := toFloat64(b)
	if aIsNum && bIsNum {
		return aNum == bNum
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}
