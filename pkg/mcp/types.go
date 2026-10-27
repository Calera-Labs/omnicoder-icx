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
	"path/filepath"
	"runtime"
	"strings"
)

// MCP Protocol and Server Constants.
const (
	JSONRPCVersion         = "2.0"
	DefaultProtocolVersion = "2024-11-05"
	DefaultServerName      = "omnicoder-mcp"
	DefaultServerVersion   = "1.0.0"
	MaxMessageSize         = 64 * 1024 * 1024 // 64 MB maximum message payload
	MaxHeaderLines         = 32               // Maximum header lines for Content-Length framing
)

// Standard JSON-RPC 2.0 Error Codes.
const (
	ErrCodeParseError     = -32700 // Invalid JSON was received by the server.
	ErrCodeInvalidRequest = -32600 // The JSON sent is not a valid Request object.
	ErrCodeMethodNotFound = -32601 // The method does not exist / is not available.
	ErrCodeInvalidParams  = -32602 // Invalid method parameter(s).
	ErrCodeInternalError  = -32603 // Internal JSON-RPC error.
)

// Sentinel errors across MCP server and tool operations.
var (
	ErrPathTraversal   = errors.New("mcp: path traversal: path escapes workspace boundary")
	ErrFileNotFound    = errors.New("mcp: file not found")
	ErrFileExists      = errors.New("mcp: file already exists and overwrite is false")
	ErrInvalidPath     = errors.New("mcp: invalid path")
	ErrGitExecution    = errors.New("mcp: git command execution failed")
	ErrGitNotFound     = errors.New("mcp: git binary not found in PATH")
	ErrNotGitRepo      = errors.New("mcp: not a git repository")
	ErrNothingToCommit = errors.New("mcp: nothing to commit, working tree clean")
	ErrSymlinkBreakout = errors.New("mcp: symlink breakout detected")
)

// JSONRPCRequest models an incoming JSON-RPC 2.0 request or notification.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"` // string, number, or null; nil indicates notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification returns true if the request has no ID member.
func (r *JSONRPCRequest) IsNotification() bool {
	return r.ID == nil
}

// JSONRPCResponse models an outgoing JSON-RPC 2.0 response envelope.
type JSONRPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"` // Required in response; null if ID could not be determined
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

// RPCError models a standard JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("jsonrpc error (code %d): %s", e.Code, e.Message)
}

// Error constructor helpers.
func NewParseError(details string) *RPCError {
	return &RPCError{Code: ErrCodeParseError, Message: "Parse error: " + details}
}

func NewInvalidRequestError(details string) *RPCError {
	return &RPCError{Code: ErrCodeInvalidRequest, Message: "Invalid Request: " + details}
}

func NewMethodNotFoundError(method string) *RPCError {
	return &RPCError{Code: ErrCodeMethodNotFound, Message: fmt.Sprintf("Method not found: %s", method)}
}

func NewInvalidParamsError(details string) *RPCError {
	return &RPCError{Code: ErrCodeInvalidParams, Message: "Invalid params: " + details}
}

func NewInternalError(details string) *RPCError {
	return &RPCError{Code: ErrCodeInternalError, Message: "Internal error: " + details}
}

// ToolHandler represents the execution logic for a registered MCP tool.
type ToolHandler func(ctx context.Context, args map[string]any) (any, error)

// ToolDefinition defines the metadata and Draft 2020-12 input schema for an MCP tool.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// WorkspaceFolder represents a client workspace folder in MCP / LSP.
type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// WorkspaceRoot represents an MCP root directory.
type WorkspaceRoot struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// RootsListResult represents the response to a roots/list request.
type RootsListResult struct {
	Roots []WorkspaceRoot `json:"roots"`
}

type InitializeParams struct {
	ProtocolVersion       string            `json:"protocolVersion"`
	Capabilities          map[string]any    `json:"capabilities,omitempty"`
	ClientInfo            ClientInfo        `json:"clientInfo,omitempty"`
	RootURI               string            `json:"rootUri,omitempty"`
	RootPath              string            `json:"rootPath,omitempty"`
	WorkspaceFolders      []WorkspaceFolder `json:"workspaceFolders,omitempty"`
	Roots                 []WorkspaceRoot   `json:"roots,omitempty"`
	InitializationOptions map[string]any    `json:"initializationOptions,omitempty"`
}

// URItoPath converts a file:// URI or local path into a canonical operating system path.
func URItoPath(uriStr string) (string, error) {
	if uriStr == "" {
		return "", nil
	}
	// If it doesn't look like a file URI, treat as plain path
	if !strings.HasPrefix(strings.ToLower(uriStr), "file://") && !strings.HasPrefix(strings.ToLower(uriStr), "file:") {
		if runtime.GOOS != "windows" {
			uriStr = strings.ReplaceAll(uriStr, "\\", "/")
		}
		return filepath.Clean(uriStr), nil
	}

	trimmed := strings.TrimPrefix(uriStr, "file://")
	trimmed = strings.TrimPrefix(trimmed, "file:")

	unescaped, err := url.PathUnescape(trimmed)
	if err != nil {
		unescaped = trimmed
	}

	// Remove leading localhost/ or 127.0.0.1/ if present
	if strings.HasPrefix(strings.ToLower(unescaped), "localhost/") || strings.HasPrefix(strings.ToLower(unescaped), "localhost\\") {
		unescaped = unescaped[10:]
	} else if strings.HasPrefix(unescaped, "127.0.0.1/") || strings.HasPrefix(unescaped, "127.0.0.1\\") {
		unescaped = unescaped[10:]
	}

	// Handle file:///C:/path, /C:/path, ///C:/path across platforms
	for len(unescaped) >= 3 && (strings.HasPrefix(unescaped, "/") || strings.HasPrefix(unescaped, "\\")) {
		if len(unescaped) >= 3 && unescaped[2] == ':' {
			unescaped = unescaped[1:]
			break
		}
		if runtime.GOOS == "windows" {
			unescaped = unescaped[1:]
		} else {
			break
		}
	}
	if runtime.GOOS == "windows" {
		unescaped = filepath.FromSlash(unescaped)
	} else {
		unescaped = strings.ReplaceAll(unescaped, "\\", "/")
	}

	return filepath.Clean(unescaped), nil
}

// ExtractWorkspaceFromHandshake parses the MCP initialize params to locate
// the active project workspace root URI or path without requiring extra round-trips.
func ExtractWorkspaceFromHandshake(rawParams json.RawMessage) string {
	if len(rawParams) == 0 {
		return ""
	}

	// 1. Try strongly typed InitializeParams
	var p InitializeParams
	if err := json.Unmarshal(rawParams, &p); err == nil {
		if p.RootURI != "" {
			if parsed, err := URItoPath(p.RootURI); err == nil && parsed != "" {
				return parsed
			}
		}
		if p.RootPath != "" {
			if parsed, err := URItoPath(p.RootPath); err == nil && parsed != "" {
				return parsed
			}
		}
		if len(p.WorkspaceFolders) > 0 && p.WorkspaceFolders[0].URI != "" {
			if parsed, err := URItoPath(p.WorkspaceFolders[0].URI); err == nil && parsed != "" {
				return parsed
			}
		}
		if len(p.Roots) > 0 && p.Roots[0].URI != "" {
			if parsed, err := URItoPath(p.Roots[0].URI); err == nil && parsed != "" {
				return parsed
			}
		}
		if p.InitializationOptions != nil {
			for _, k := range []string{"workspace", "rootUri", "root_uri", "workspaceRoot", "workspace_root", "projectPath", "project_path", "cwd"} {
				if v, ok := p.InitializationOptions[k].(string); ok && v != "" {
					if parsed, err := URItoPath(v); err == nil && parsed != "" {
						return parsed
					}
				}
			}
		}
	}

	// 2. Fallback to generic map traversal for non-standard or IDE-specific naming
	var rawMap map[string]any
	if err := json.Unmarshal(rawParams, &rawMap); err == nil && rawMap != nil {
		checkKeys := []string{
			"rooturi", "root_uri", "rootpath", "root_path",
			"workspace", "workspacedir", "workspace_dir", "workspaceroot", "workspace_root",
			"projectpath", "project_path", "projectdir", "project_dir", "cwd",
		}
		for _, targetKey := range checkKeys {
			for k, v := range rawMap {
				if strings.EqualFold(strings.ReplaceAll(k, "_", ""), strings.ReplaceAll(targetKey, "_", "")) {
					if strVal, ok := v.(string); ok && strVal != "" {
						if parsed, err := URItoPath(strVal); err == nil && parsed != "" {
							return parsed
						}
					}
				}
			}
		}

		for _, arrayKey := range []string{"workspacefolders", "workspace_folders", "roots"} {
			for k, v := range rawMap {
				if strings.EqualFold(strings.ReplaceAll(k, "_", ""), strings.ReplaceAll(arrayKey, "_", "")) {
					if list, ok := v.([]any); ok && len(list) > 0 {
						for _, item := range list {
							if strItem, ok := item.(string); ok && strItem != "" {
								if parsed, err := URItoPath(strItem); err == nil && parsed != "" {
									return parsed
								}
							} else if mapItem, ok := item.(map[string]any); ok {
								for _, subKey := range []string{"uri", "path", "url", "folder"} {
									if subVal, ok := mapItem[subKey].(string); ok && subVal != "" {
										if parsed, err := URItoPath(subVal); err == nil && parsed != "" {
											return parsed
										}
									}
								}
							}
						}
					}
				}
			}
		}

		for k, v := range rawMap {
			if strings.EqualFold(strings.ReplaceAll(k, "_", ""), "initializationoptions") {
				if optsMap, ok := v.(map[string]any); ok {
					for _, targetKey := range checkKeys {
						for okKey, ov := range optsMap {
							if strings.EqualFold(strings.ReplaceAll(okKey, "_", ""), strings.ReplaceAll(targetKey, "_", "")) {
								if strVal, ok := ov.(string); ok && strVal != "" {
									if parsed, err := URItoPath(strVal); err == nil && parsed != "" {
										return parsed
									}
								}
							}
						}
					}
				}
			}
		}
	}

	return ""
}

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ServerInfo         `json:"serverInfo"`
}

type ServerCapabilities struct {
	Tools     *ToolsCapability     `json:"tools,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
	Prompts   *PromptsCapability   `json:"prompts,omitempty"`
	Logging   map[string]any       `json:"logging,omitempty"`
}

type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ToolListResult struct {
	Tools []ToolDefinition `json:"tools"`
}

type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

// GetProgressToken retrieves any progress token supplied in _meta or arguments.
func (p *CallToolParams) GetProgressToken() any {
	if p == nil {
		return nil
	}
	if p.Meta != nil {
		if tok, ok := p.Meta["progressToken"]; ok && tok != nil {
			return tok
		}
	}
	if p.Arguments != nil {
		if tok, ok := p.Arguments["progress_token"]; ok && tok != nil {
			return tok
		}
		if tok, ok := p.Arguments["token"]; ok && tok != nil {
			return tok
		}
	}
	return nil
}

type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError"`
}

type Content struct {
	Type     string `json:"type"` // "text", "image", "resource"
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

func TextContent(text string) Content {
	return Content{Type: "text", Text: text}
}

// SchemaViolation represents a specific validation violation against Draft 2020-12 schema.
type SchemaViolation struct {
	Field       string `json:"field"`
	Rule        string `json:"rule"`
	Message     string `json:"message"`
	ActualValue any    `json:"actual_value,omitempty"`
}

func (v SchemaViolation) String() string {
	return fmt.Sprintf("field '%s': %s", v.Field, v.Message)
}
