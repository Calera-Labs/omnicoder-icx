// SPDX-License-Identifier: Apache-2.0
// Package mcp provides the Model Context Protocol (MCP) JSON-RPC 2.0 stdio server,
// tool registry, and QA trace logging infrastructure for OmniCoder-ICX.

package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LogLevel represents logging severity.
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelNotice
	LogLevelWarning
	LogLevelError
	LogLevelCritical
)

// ParseLogLevel converts a string to LogLevel.
func ParseLogLevel(l string) LogLevel {
	switch strings.ToLower(strings.TrimSpace(l)) {
	case "debug":
		return LogLevelDebug
	case "info":
		return LogLevelInfo
	case "notice":
		return LogLevelNotice
	case "warn", "warning":
		return LogLevelWarning
	case "error":
		return LogLevelError
	case "crit", "critical", "alert", "emergency":
		return LogLevelCritical
	default:
		return LogLevelInfo
	}
}

func (l LogLevel) String() string {
	switch l {
	case LogLevelDebug:
		return "debug"
	case LogLevelInfo:
		return "info"
	case LogLevelNotice:
		return "notice"
	case LogLevelWarning:
		return "warning"
	case LogLevelError:
		return "error"
	case LogLevelCritical:
		return "critical"
	default:
		return "info"
	}
}

// TraceEvent represents a single recorded MCP tool call or operational event.
type TraceEvent struct {
	Timestamp   time.Time      `json:"timestamp"`
	Event       string         `json:"event"`                  // "tool_call", "request", "server_start", "workspace_update"
	RequestID   any            `json:"request_id,omitempty"`
	Method      string         `json:"method,omitempty"`       // e.g. "tools/call", "initialize"
	Tool        string         `json:"tool,omitempty"`         // e.g. "omnicoder_patch", "read_file"
	Arguments   map[string]any `json:"arguments,omitempty"`    // Sanitized arguments
	DurationMS  int64          `json:"duration_ms"`            // Latency in milliseconds
	Status      string         `json:"status"`                 // "OK", "ERROR", "PANIC", "VALIDATION_ERROR"
	Error       string         `json:"error,omitempty"`        // Error message if any
	ResultBytes int            `json:"result_bytes,omitempty"` // Size of response in bytes
	Workspace   string         `json:"workspace,omitempty"`
}

// TraceLogger is the interface for recording and auditing MCP server interactions.
type TraceLogger interface {
	LogEvent(event TraceEvent)
	SetLevel(level LogLevel)
	GetLevel() LogLevel
	FilePath() string
	Close() error
}

// TracerConfig configures a DefaultTracer instance.
type TracerConfig struct {
	FilePath string    // Path to write JSONL trace events (if empty, file logging is disabled)
	Verbose  bool      // If true, write concise single-line human-readable logs to Stderr
	Stderr   io.Writer // Writer for verbose diagnostic logs (defaults to os.Stderr; NEVER os.Stdout!)
	Level    LogLevel  // Default log level filter
}

// DefaultTracer is the standard thread-safe JSONL and stderr trace logger.
type DefaultTracer struct {
	mu       sync.Mutex
	filePath string
	file     *os.File
	stderr   io.Writer
	verbose  bool
	level    LogLevel
	closed   bool
}

// NewDefaultTracer initializes a new DefaultTracer.
func NewDefaultTracer(cfg TracerConfig) *DefaultTracer {
	t := &DefaultTracer{
		filePath: cfg.FilePath,
		stderr:   cfg.Stderr,
		verbose:  cfg.Verbose,
		level:    cfg.Level,
	}

	if t.stderr == nil && t.verbose {
		t.stderr = os.Stderr
	}

	if t.filePath != "" {
		dir := filepath.Dir(t.filePath)
		if dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0755)
		}
		f, err := os.OpenFile(t.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			t.file = f
		} else if t.stderr != nil {
			fmt.Fprintf(t.stderr, "[OmniCoder-MCP] Failed to open trace file %s: %v\n", t.filePath, err)
		}
	}

	return t
}

// SetLevel updates the logging level threshold.
func (t *DefaultTracer) SetLevel(level LogLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.level = level
}

// GetLevel returns the current logging level threshold.
func (t *DefaultTracer) GetLevel() LogLevel {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.level
}

// FilePath returns the configured trace log file path.
func (t *DefaultTracer) FilePath() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.filePath
}

// Close flushes and closes the underlying file handle.
func (t *DefaultTracer) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if t.file != nil {
		_ = t.file.Sync()
		err := t.file.Close()
		t.file = nil
		return err
	}
	return nil
}

// LogEvent records a trace event to the JSONL file and/or stderr.
func (t *DefaultTracer) LogEvent(event TraceEvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}

	// Sanitize arguments to protect credentials and prevent oversized logs
	if event.Arguments != nil {
		event.Arguments = SanitizeArguments(event.Arguments)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// 1. Write structured JSON Lines entry if file is open
	if t.file != nil && !t.closed {
		data, err := json.Marshal(event)
		if err == nil {
			data = append(data, '\n')
			_, _ = t.file.Write(data)
			_ = t.file.Sync()
		}
	}

	// 2. Write concise single-line human-readable summary to stderr if verbose
	if t.verbose && t.stderr != nil {
		var extra string
		if event.Tool != "" {
			if p, ok := event.Arguments["path"].(string); ok && p != "" {
				extra = fmt.Sprintf(" path=%q", p)
			} else if task, ok := event.Arguments["task"].(string); ok && task != "" {
				if len(task) > 40 {
					task = task[:40] + "..."
				}
				extra = fmt.Sprintf(" task=%q", task)
			} else if cmd, ok := event.Arguments["command"].(string); ok && cmd != "" {
				extra = fmt.Sprintf(" cmd=%q", cmd)
			} else if sym, ok := event.Arguments["symbol"].(string); ok && sym != "" {
				extra = fmt.Sprintf(" symbol=%q", sym)
			} else if query, ok := event.Arguments["query"].(string); ok && query != "" {
				extra = fmt.Sprintf(" query=%q", query)
			}

			if event.Error != "" {
				fmt.Fprintf(t.stderr, "[OmniCoder-MCP] [tool:%s] status=%s duration=%dms err=%q%s\n",
					event.Tool, event.Status, event.DurationMS, event.Error, extra)
			} else {
				fmt.Fprintf(t.stderr, "[OmniCoder-MCP] [tool:%s] status=%s duration=%dms%s\n",
					event.Tool, event.Status, event.DurationMS, extra)
			}
		} else {
			fmt.Fprintf(t.stderr, "[OmniCoder-MCP] [event:%s] method=%s status=%s duration=%dms\n",
				event.Event, event.Method, event.Status, event.DurationMS)
		}
	}
}

const (
	maxLoggedStringLength = 256
	truncatedPrefixLength = 128
)

// SanitizeArguments scrubs sensitive values (API keys, tokens, passwords)
// and truncates large string buffers (file contents, patches) to preserve compact logs.
func SanitizeArguments(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = sanitizeValue(k, v)
	}
	return out
}

func sanitizeValue(key string, val any) any {
	lowerKey := strings.ToLower(key)
	if isSensitiveKey(lowerKey) {
		return "[REDACTED]"
	}

	switch v := val.(type) {
	case string:
		if isSensitiveString(v) {
			return "[REDACTED]"
		}
		if len(v) > maxLoggedStringLength {
			return fmt.Sprintf("%s... [truncated %d bytes total]", v[:truncatedPrefixLength], len(v))
		}
		return v

	case map[string]any:
		nested := make(map[string]any, len(v))
		for nk, nv := range v {
			nested[nk] = sanitizeValue(nk, nv)
		}
		return nested

	case []any:
		sliceOut := make([]any, len(v))
		for i, el := range v {
			sliceOut[i] = sanitizeValue(key, el)
		}
		return sliceOut

	default:
		return val
	}
}

func isSensitiveKey(key string) bool {
	return strings.Contains(key, "password") ||
		strings.Contains(key, "api_key") ||
		strings.Contains(key, "apikey") ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "token") ||
		strings.Contains(key, "auth") ||
		strings.Contains(key, "credential") ||
		strings.Contains(key, "private_key")
}

func isSensitiveString(str string) bool {
	trimmed := strings.TrimSpace(str)
	if strings.HasPrefix(trimmed, "AQ.") || strings.HasPrefix(trimmed, "sk-") || strings.HasPrefix(trimmed, "ghp_") {
		return true
	}
	return false
}
