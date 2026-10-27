// SPDX-License-Identifier: Apache-2.0
// Package icx defines the OmniCoder client wire types, interface contracts, and
// configuration for communicating with Calera ICX (https://icx.caleralabs.com).
// Note: The Calera ICX platform is governed separately by https://icx.caleralabs.com licensing terms.

package icx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// JSON-RPC 2.0 and ICX Protocol Constants.
const (
	JSONRPCVersion = "2.0"
	DefaultBaseURL = "https://icx.api.caleralabs.com/v1"
	DefaultSpaceID = "default"
	DefaultMCPMode = "virtual"
	DefaultTimeout = 15 * time.Second
	MaxRecallTopK  = 10
	MaxHeaderBytes = 64 * 1024 * 1024 // 64 MB
)

// Standard JSON-RPC 2.0 error codes and ICX custom error codes.
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603
	ErrCodeCASConflict    = -32001
	ErrCodeNotFound       = -32004
)

// JSONRPCRequest models a standard JSON-RPC 2.0 request payload.
type JSONRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// JSONRPCResponse models a standard JSON-RPC 2.0 response envelope.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
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
	return e.Message
}

// MCP Envelope Types for tools/call protocol.
type MCPToolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type MCPToolCallResult struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError"`
}

type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ClientConfig configures the ICX client transport and fallback behavior.
type ClientConfig struct {
	BaseURL       string        `json:"base_url"`
	APIKey        string        `json:"api_key"`
	SpaceID       string        `json:"space_id"`
	MCPMode       string        `json:"mcp_mode"`       // "virtual" or "direct"
	TransportMode string        `json:"transport_mode"` // "http", "sse", "stdio", "local"
	Timeout       time.Duration `json:"timeout"`
	Offline       bool          `json:"offline"`
	Strict        bool          `json:"strict"` // if true, do not fall back to local on network err
	StdioReader   io.Reader     `json:"-"`
	StdioWriter   io.Writer     `json:"-"`
	Command       []string      `json:"command,omitempty"`
	HTTPClient    *http.Client  `json:"-"`
}

// ----------------------------------------------------------------------------
// Certified Tool 1: icx_remember
// ----------------------------------------------------------------------------
type RememberRequest struct {
	Text     string         `json:"text"`
	SourceID string         `json:"source_id"`
	SpaceID  string         `json:"space_id,omitempty"`
	Family   string         `json:"family,omitempty"`
	NodeID   string         `json:"node_id,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type RememberResponse struct {
	Status                string   `json:"status"`
	FactID                string   `json:"fact_id"`
	NodeID                string   `json:"node_id,omitempty"`
	SpaceID               string   `json:"space_id"`
	SourceID              string   `json:"source_id"`
	SHA256                string   `json:"sha256"`
	Redactions            []string `json:"redactions,omitempty"`
	NodesCrystallized     int      `json:"nodes_crystallized"`
	CrystallizationTimeUs float64  `json:"crystallization_time_us,omitempty"`
}

// Constants for 1D Geodesic Ribbon Mode.
const (
	DomainRibbon = 61
	FormatRibbon = "ribbon"
)

// ----------------------------------------------------------------------------
// Certified Tool 2: icx_recall_scoped (Clamped strictly top_k <= 10)
// ----------------------------------------------------------------------------
type RecallRequest struct {
	Query   string `json:"query"`
	SpaceID string `json:"space_id,omitempty"`
	TopK    int    `json:"top_k,omitempty"` // clamped strictly <= 10
	Family  string `json:"family,omitempty"`
	Format  string `json:"format,omitempty"` // "ribbon"
}

type RecallMatch struct {
	FactID   string         `json:"fact_id,omitempty"`
	Key      string         `json:"key,omitempty"`
	Content  string         `json:"content"`
	Family   string         `json:"family,omitempty"`
	Score    float64        `json:"score,omitempty"`
	SHA256   string         `json:"sha256,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type RecallResponse struct {
	Status    string        `json:"status"`
	Query     string        `json:"query"`
	SpaceID   string        `json:"space_id"`
	Count     int           `json:"count"`
	Matches   []RecallMatch `json:"matches"`
	Ribbon    string        `json:"ribbon,omitempty"` // <nav:ribbon: ... />
	LatencyUs float64       `json:"latency_us,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 3: icx_search_facts
// ----------------------------------------------------------------------------
type SearchRequest struct {
	Query   string `json:"query"`
	SpaceID string `json:"space_id,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type SearchFact struct {
	FactID         string  `json:"fact_id"`
	Text           string  `json:"text"`
	SourceID       string  `json:"source_id"`
	Family         string  `json:"family"`
	RelevanceScore float64 `json:"relevance_score"`
	SHA256         string  `json:"sha256"`
}

type SearchResponse struct {
	Query        string       `json:"query"`
	SpaceID      string       `json:"space_id"`
	TotalMatches int          `json:"total_matches"`
	Facts        []SearchFact `json:"facts"`
	LatencyUs    float64      `json:"latency_us,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 4: icx_quote_slot (Deterministic <0.002ms byte-exact quote)
// ----------------------------------------------------------------------------
type QuoteSlotRequest struct {
	Family  string `json:"family"`
	Index   int    `json:"index"` // 1-based index
	SpaceID string `json:"space_id,omitempty"`
	// Start is a 0-based rune offset. Length is a rune count.
	// Omit both to return the whole slot. Out of range is OUT_OF_RANGE.
	Start  *int `json:"start,omitempty"`
	Length *int `json:"length,omitempty"`
}

type QuoteSlotResponse struct {
	OK        bool    `json:"ok"`
	Family    string  `json:"family"`
	Index     int     `json:"index"`
	Text      string  `json:"text"`
	SourceID  string  `json:"source_id"`
	SHA256      string  `json:"sha256"`
	SliceSHA256 string  `json:"slice_sha256,omitempty"`
	IsSlice     bool    `json:"is_slice,omitempty"`
	Verified    bool    `json:"verified"`
	LatencyUs   float64 `json:"latency_us,omitempty"`
	Error       string  `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 5: icx_sync_delta (Sub-2ms delta crystallization)
// ----------------------------------------------------------------------------
type SyncDeltaRequest struct {
	SpaceID      string         `json:"space_id,omitempty"`
	SourceURI    string         `json:"source_uri"`
	ParentHash   string         `json:"parent_hash,omitempty"`
	DeltaSimplex map[string]any `json:"delta_simplex"`
	Author       string         `json:"author,omitempty"`
}

type SyncDeltaResponse struct {
	Status               string  `json:"status"`
	CISSeal              string  `json:"cis_seal"`
	DeltaID              string  `json:"delta_id"`
	NodesCrystallized    int     `json:"nodes_crystallized"`
	CrystallizeLatencyMs float64 `json:"crystallize_latency_ms,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 6: icx_reset_session (Dialogue tokens -> 0, lattice 100% kept)
// ----------------------------------------------------------------------------
type ResetSessionRequest struct {
	SpaceID   string `json:"space_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type ResetSessionResponse struct {
	Status            string `json:"status"`
	TokensPreserved   int    `json:"tokens_preserved"` // 0 tokens
	LatticeNodesAlive int    `json:"lattice_nodes_alive"`
	SpaceID           string `json:"space_id"`
}

// ----------------------------------------------------------------------------
// Certified Tool 7: icx_multihop_walk (5-hop simplicial causal AST walk)
// ----------------------------------------------------------------------------
type MultihopWalkRequest struct {
	StartNode string `json:"start_node"`
	Relation  string `json:"relation,omitempty"` // "calls", "implements", "imports", "defines", or "" / "all"
	MaxHops   int    `json:"max_hops,omitempty"`  // clamped to <= 5 by default
	Directed  bool   `json:"directed,omitempty"`
	SpaceID   string `json:"space_id,omitempty"`
	Format    string `json:"format,omitempty"` // "ribbon"
}

type WalkStep struct {
	NodeID   string `json:"node_id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // "file", "function", "type", "interface"
	FilePath string `json:"file_path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Relation string `json:"relation,omitempty"`
	Depth    int    `json:"depth"`
	ParentID string `json:"parent_id,omitempty"`
}

type MultihopWalkResponse struct {
	OK             bool       `json:"ok"`
	Start          string     `json:"start"`
	StartNode      string     `json:"start_node,omitempty"`
	Relation       string     `json:"relation,omitempty"`
	MaxHops        int        `json:"max_hops"`
	Hops           int        `json:"hops"`
	NodesReached   int        `json:"nodes_reached"`
	Path           []string   `json:"path"`
	Visited        int        `json:"visited"`
	VisitedCount   int        `json:"visited_count,omitempty"`
	CyclesDetected int        `json:"cycles_detected"`
	Steps          []WalkStep `json:"steps,omitempty"`
	Nodes          []any      `json:"nodes,omitempty"`
	Ribbon         string     `json:"ribbon,omitempty"` // <nav:ribbon: ... />
	LatencyUs      float64    `json:"latency_us,omitempty"`
	LatencyMs      float64    `json:"latency_ms,omitempty"`
	Error          string     `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 8: icx_var_set (Compare-And-Swap Optimistic Locking)
// ----------------------------------------------------------------------------
type VarSetRequest struct {
	Key             string `json:"key"`
	Value           any    `json:"value"`
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	ExpectedSHA256  string `json:"expected_sha256,omitempty"`
	Writer          string `json:"writer,omitempty"`
	SpaceID         string `json:"space_id,omitempty"`
	TTLMs           int    `json:"ttl_ms,omitempty"`
}

type VarSetResponse struct {
	OK              bool    `json:"ok"`
	Key             string  `json:"key"`
	Version         int     `json:"version"`
	Writer          string  `json:"writer,omitempty"`
	LatencyUs       float64 `json:"latency_us,omitempty"`
	Error           string  `json:"error,omitempty"` // "CAS_CONFLICT" on version mismatch
	ExpectedVersion *int    `json:"expected_version,omitempty"`
	ActualVersion   int     `json:"actual_version,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 9: icx_var_get
// ----------------------------------------------------------------------------
type VarGetRequest struct {
	Key     string `json:"key"`
	SpaceID string `json:"space_id,omitempty"`
}

type VarGetResponse struct {
	OK        bool    `json:"ok"`
	Key       string  `json:"key"`
	Value     any     `json:"value,omitempty"`
	Version   int     `json:"version,omitempty"`
	Writer    string  `json:"writer,omitempty"`
	UpdatedAt float64 `json:"updated_at,omitempty"`
	LatencyUs float64 `json:"latency_us,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 22: icx_var_list (Swarm Variable Enumeration & Discovery)
// ----------------------------------------------------------------------------
type VarListRequest struct {
	Prefix  string `json:"prefix,omitempty"`   // Key prefix filter (e.g. "task_", "cache:")
	SpaceID string `json:"space_id,omitempty"` // Target memory space (default: "default")
	Limit   int    `json:"limit,omitempty"`    // Max entries to return (0 = unlimited)
}

type VarInfo struct {
	Key            string `json:"key"`
	Version        int    `json:"version"`
	Writer         string `json:"writer,omitempty"`
	RemainingTTLMs int64  `json:"remaining_ttl_ms,omitempty"`
	TTLMs          int64  `json:"ttl_ms,omitempty"`
	ValuePreview   string `json:"value_preview,omitempty"`
}

type VarListResponse struct {
	OK        bool           `json:"ok"`
	Status    string         `json:"status,omitempty"`
	Count     int            `json:"count"`
	Variables []VarInfo      `json:"variables"`
	Vars      map[string]any `json:"vars,omitempty"`
	LatencyUs float64        `json:"latency_us,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 10: icx_exec (Sandboxed Expression & Statement Execution)
// ----------------------------------------------------------------------------
type ExecRequest struct {
	Source   string         `json:"source"`
	LocalsIn map[string]any `json:"locals_in,omitempty"`
}

type ExecResponse struct {
	OK        bool           `json:"ok"`
	Result    any            `json:"result,omitempty"`
	LocalsOut map[string]any `json:"locals_out,omitempty"`
	LatencyUs float64        `json:"latency_us,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 11: icx_wasm_exec (In-Process Wasm Stack Machine)
// ----------------------------------------------------------------------------
type WasmExecRequest struct {
	Program []string `json:"program"`
}

type WasmExecResponse struct {
	OK        bool      `json:"ok"`
	Result    float64   `json:"result"`
	Stack     []float64 `json:"stack"`
	Steps     int       `json:"steps"`
	LatencyUs float64   `json:"latency_us,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 12: icx_inspect_space
// ----------------------------------------------------------------------------
type InspectSpaceRequest struct {
	SpaceID  string `json:"space_id,omitempty"`
	Detailed bool   `json:"detailed,omitempty"`
}

type InspectSpaceResponse struct {
	SpaceID      string            `json:"space_id"`
	NodesCount   int               `json:"nodes_count"`
	SourcesCount int               `json:"sources_count"`
	Vars         []string          `json:"vars"`
	Leases       map[string]string `json:"leases,omitempty"`
	Edges        int               `json:"edges"`
	ShardCount   int               `json:"shard_count"`
	MerkleRoot   string            `json:"merkle_root,omitempty"`
	Telemetry    map[string]any    `json:"telemetry,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 13: icx_purge_source
// ----------------------------------------------------------------------------
type PurgeSourceRequest struct {
	SourceID string `json:"source_id"`
	SpaceID  string `json:"space_id,omitempty"`
}

type PurgeSourceResponse struct {
	Status         string  `json:"status"`
	SourceID       string  `json:"source_id"`
	SpaceID        string  `json:"space_id,omitempty"`
	NodesUnlinked  int     `json:"nodes_unlinked"`
	PurgeLatencyMs float64 `json:"purge_latency_ms,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 14: icx_swarm_state
// ----------------------------------------------------------------------------
type SwarmStateRequest struct {
	Action   string `json:"action"` // "lease", "release", "inspect"
	Resource string `json:"resource"`
	AgentID  string `json:"agent_id"`
	TTLMs    int    `json:"ttl_ms,omitempty"`
}

type SwarmStateResponse struct {
	OK        bool           `json:"ok"`
	Action    string         `json:"action"`
	Resource  string         `json:"resource"`
	Holder    string         `json:"holder,omitempty"`
	Token     string         `json:"token,omitempty"`
	TTLMs     int            `json:"ttl_ms,omitempty"`
	Lease     map[string]any `json:"lease,omitempty"`
	LatencyUs float64        `json:"latency_us,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 15: icx_query_codebase
// ----------------------------------------------------------------------------
type QueryCodebaseRequest struct {
	Query     string `json:"query"`
	Dimension string `json:"dimension,omitempty"`
	TopK      int    `json:"top_k,omitempty"`
}

type CodebaseMatch struct {
	NodeID    string         `json:"node_id"`
	Dimension string         `json:"dimension"`
	Snippet   string         `json:"snippet,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

type QueryCodebaseResponse struct {
	Query   string          `json:"query"`
	Count   int             `json:"count"`
	Results []CodebaseMatch `json:"results"`
}

// ----------------------------------------------------------------------------
// Certified Tool 16: icx_read_dimension
// ----------------------------------------------------------------------------
type ReadDimensionRequest struct {
	Dimension string `json:"dimension"` // 0D_SCALAR, 1D_SEQUENCE, 2D_TABULAR, 3D_VOLUMETRIC, 4D_SPATIOTEMPORAL
	NodeID    string `json:"node_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type ReadDimensionResponse struct {
	Dimension string           `json:"dimension"`
	Nodes     []map[string]any `json:"nodes"`
}

// ----------------------------------------------------------------------------
// Certified Tool 17: icx_read_file_ast
// ----------------------------------------------------------------------------
type ReadFileASTRequest struct {
	FilePath string `json:"file_path"`
}

type ReadFileASTResponse struct {
	FilePath   string           `json:"file_path"`
	Language   string           `json:"language"`
	Classes    []string         `json:"classes,omitempty"`
	Functions  []string         `json:"functions,omitempty"`
	Imports    []string         `json:"imports,omitempty"`
	ASTNodes   []map[string]any `json:"ast_nodes,omitempty"`
	MerkleRoot string           `json:"merkle_root,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 18: icx_edit_node
// ----------------------------------------------------------------------------
type EditNodeRequest struct {
	NodeID         string `json:"node_id"`
	NewValue       any    `json:"new_value"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	WriterAgent    string `json:"writer_agent,omitempty"`
}

type EditNodeResponse struct {
	Success    bool   `json:"success"`
	NodeID     string `json:"node_id"`
	NewSHA256  string `json:"new_sha256"`
	MerkleSeal string `json:"merkle_seal"`
	Error      string `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 19: icx_edit_file
// ----------------------------------------------------------------------------
type EditFileRequest struct {
	FilePath       string `json:"file_path"`
	Content        string `json:"content"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	WriterAgent    string `json:"writer_agent,omitempty"`
}

type EditFileResponse struct {
	Success    bool   `json:"success"`
	FilePath   string `json:"file_path"`
	NewSHA256  string `json:"new_sha256"`
	MerkleSeal string `json:"merkle_seal"`
	Error      string `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Certified Tool 20: icx_ingest_dimension
// ----------------------------------------------------------------------------
type IngestDimensionRequest struct {
	Dimension string         `json:"dimension"`
	Payload   map[string]any `json:"payload"`
}

type IngestDimensionResponse struct {
	Success   bool   `json:"success"`
	Dimension string `json:"dimension"`
	NodeID    string `json:"node_id"`
	SHA256    string `json:"sha256"`
}

// ----------------------------------------------------------------------------
// Certified Tool 21: icx_execute_script
// ----------------------------------------------------------------------------
type ExecuteScriptRequest struct {
	ScriptName  string         `json:"script_name"`
	Query       string         `json:"query,omitempty"`
	Arguments   map[string]any `json:"arguments,omitempty"`
	Code        string         `json:"code,omitempty"`
	Crystallize bool           `json:"crystallize,omitempty"`
}

type ExecuteScriptResponse struct {
	Success    bool   `json:"success"`
	ScriptName string `json:"script_name"`
	Output     any    `json:"output"`
	MerkleSeal string `json:"merkle_seal,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Virtual Mode Gateway Tools
// ----------------------------------------------------------------------------
type RecallEpistemicContextRequest struct {
	Query   string `json:"query"`
	SpaceID string `json:"space_id,omitempty"`
	Depth   int    `json:"depth,omitempty"`
}

type RecallEpistemicContextResponse struct {
	Facts     []string `json:"facts"`
	ToolCards []any    `json:"tool_cards,omitempty"`
}

type DispatchActuationRequest struct {
	Surface string         `json:"surface"`
	Action  string         `json:"action"`
	Payload map[string]any `json:"payload"`
}

type DispatchActuationResponse struct {
	Success bool           `json:"success"`
	Status  string         `json:"status"`
	Receipt string         `json:"receipt,omitempty"`
	Result  map[string]any `json:"result,omitempty"`
}

// ActuationReceipt captures the structured cryptographic outcome of an actuation
// (fuzzy patch or compiler self-healing) bounded strictly to <170 tokens.
type ActuationReceipt struct {
	FileModified      string `json:"file_modified"`
	ExitCode          int    `json:"exit_code"`
	DiagnosticMessage string `json:"diagnostic_message"`
	RepairDiffSummary string `json:"repair_diff_summary"`
	SHA256Seal        string `json:"sha256_seal"`
	TurnNumber        int    `json:"turn_number"`
	Surface           string `json:"surface,omitempty"`
	Action            string `json:"action,omitempty"`
	Timestamp         int64  `json:"timestamp,omitempty"`
}

func clampReceiptField(s string, maxChars, maxWords int) string {
	s = strings.TrimSpace(s)
	if len(s) > maxChars {
		if maxChars > 3 {
			s = s[:maxChars-3] + "..."
		} else {
			s = s[:maxChars]
		}
	}
	words := strings.Fields(s)
	if len(words) > maxWords && maxWords > 0 {
		s = strings.Join(words[:maxWords], " ") + "..."
	}
	return s
}

// FormatCompact formats the actuation receipt into a token-compressed representation (<170 tokens).
// The output strictly follows: <actuation:receipt file=... exit=... seal=... diag=... diff=... />
func (r *ActuationReceipt) FormatCompact() string {
	if r == nil {
		return "<actuation:receipt empty=true />"
	}
	fileMod := clampReceiptField(r.FileModified, 60, 10)
	diag := clampReceiptField(r.DiagnosticMessage, 180, 40)
	diff := clampReceiptField(r.RepairDiffSummary, 180, 40)
	seal := r.SHA256Seal
	if len(seal) > 64 {
		seal = seal[:64]
	}

	res := fmt.Sprintf("<actuation:receipt file=%q exit=%d seal=%q diag=%q diff=%q />",
		fileMod, r.ExitCode, seal, diag, diff)

	// Hard boundary guard: ensure max(chars/3.5, words*1.25) < 168 tokens
	for len(res) > 0 {
		cEst := int(float64(len(res)) / 3.5)
		wEst := int(float64(len(strings.Fields(res))) * 1.25)
		tEst := cEst
		if wEst > tEst {
			tEst = wEst
		}
		if tEst < 168 {
			break
		}
		if len(diff) > 20 {
			diff = diff[:len(diff)-10] + "..."
		} else if len(diag) > 20 {
			diag = diag[:len(diag)-10] + "..."
		} else if len(fileMod) > 10 {
			fileMod = fileMod[:len(fileMod)-5] + "..."
		} else {
			break
		}
		res = fmt.Sprintf("<actuation:receipt file=%q exit=%d seal=%q diag=%q diff=%q />",
			fileMod, r.ExitCode, seal, diag, diff)
	}

	return res
}

// ToPayload converts the ActuationReceipt into a map suitable for DispatchActuationRequest.
func (r *ActuationReceipt) ToPayload() map[string]any {
	if r == nil {
		return nil
	}
	m := map[string]any{
		"file_modified":       r.FileModified,
		"exit_code":           r.ExitCode,
		"diagnostic_message":  r.DiagnosticMessage,
		"repair_diff_summary": r.RepairDiffSummary,
		"sha256_seal":         r.SHA256Seal,
		"turn_number":         r.TurnNumber,
	}
	if r.Surface != "" {
		m["surface"] = r.Surface
	}
	if r.Action != "" {
		m["action"] = r.Action
	}
	if r.Timestamp != 0 {
		m["timestamp"] = r.Timestamp
	}
	return m
}

// ActuationReceiptFromPayload converts a payload map into an ActuationReceipt.
func ActuationReceiptFromPayload(m map[string]any) (*ActuationReceipt, error) {
	if m == nil {
		return nil, errors.New("icx: nil payload")
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("icx: marshal payload: %w", err)
	}
	var r ActuationReceipt
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("icx: unmarshal receipt: %w", err)
	}
	return &r, nil
}

// ----------------------------------------------------------------------------
// Turn Milestone (Step-Indexed Memory Crystallization)
// ----------------------------------------------------------------------------

// TurnMilestone models a crystallized milestone transition in the execution trajectory.
type TurnMilestone struct {
	TurnNumber     int      `json:"turn_number"`
	Task           string   `json:"task"`
	TargetFiles    []string `json:"target_files"`
	Status         string   `json:"status"` // "PASS", "HEALED", "FAIL"
	PatchesApplied int      `json:"patches_applied"`
	HealCycles     int      `json:"heal_cycles"`
	Approved       bool     `json:"approved"`
	CommitHash     string   `json:"commit_hash,omitempty"`
	MerkleSeal     string   `json:"merkle_seal"`
	Timestamp      int64    `json:"timestamp"`
}

// FormatCompact produces a token-bounded single-line milestone representation strictly < 100 tokens.
func (m *TurnMilestone) FormatCompact() string {
	if m == nil {
		return "<turn:milestone empty />"
	}
	task := m.Task
	if len(task) > 40 {
		task = task[:37] + "..."
	}
	files := strings.Join(m.TargetFiles, ",")
	if len(files) > 40 {
		files = files[:37] + "..."
	}
	seal := m.MerkleSeal
	if len(seal) > 16 {
		seal = seal[:16]
	}
	return fmt.Sprintf("<turn:milestone turn=%d status=%q files=%q patches=%d heals=%d approved=%t seal=%q />",
		m.TurnNumber, m.Status, files, m.PatchesApplied, m.HealCycles, m.Approved, seal)
}

// ToPayload converts the TurnMilestone into a map suitable for lattice indexing.
func (m *TurnMilestone) ToPayload() map[string]any {
	if m == nil {
		return nil
	}
	return map[string]any{
		"turn_number":     m.TurnNumber,
		"task":            m.Task,
		"target_files":    m.TargetFiles,
		"status":          m.Status,
		"patches_applied": m.PatchesApplied,
		"heal_cycles":     m.HealCycles,
		"approved":        m.Approved,
		"commit_hash":     m.CommitHash,
		"merkle_seal":     m.MerkleSeal,
		"timestamp":       m.Timestamp,
	}
}

// TurnMilestoneFromPayload converts a payload map into a TurnMilestone.
func TurnMilestoneFromPayload(m map[string]any) (*TurnMilestone, error) {
	if m == nil {
		return nil, errors.New("icx: nil payload")
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("icx: marshal payload: %w", err)
	}
	var res TurnMilestone
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("icx: unmarshal milestone: %w", err)
	}
	return &res, nil
}

// FormatTrajectoryRibbon formats a slice of TurnMilestones into a compact Geodesic Ribbon (<80 tokens).
// Format: <nav:ribbon: trajectory turns=N | turn_1:PASS files=[...] -> turn_2:PASS files=[...] -> ... />
// If more than 5 turns, earlier turns are summarized so token consumption remains strictly <80 tokens.
func FormatTrajectoryRibbon(milestones []*TurnMilestone) string {
	if len(milestones) == 0 {
		return ""
	}
	n := len(milestones)
	var parts []string

	// If more than 5 turns, summarize earlier turns
	if n > 5 {
		earlierPass := 0
		earlierFail := 0
		for _, m := range milestones[:n-4] {
			if m != nil && m.Approved {
				earlierPass++
			} else {
				earlierFail++
			}
		}
		summary := fmt.Sprintf("turns 1..%d: %d PASS, %d FAIL", n-4, earlierPass, earlierFail)
		parts = append(parts, summary)
		milestones = milestones[n-4:]
	}

	for _, m := range milestones {
		if m == nil {
			continue
		}
		status := "FAIL"
		if m.Approved {
			status = "PASS"
		}
		var filesStr string
		if len(m.TargetFiles) == 0 {
			filesStr = "none"
		} else if len(m.TargetFiles) == 1 {
			filesStr = filepath.Base(m.TargetFiles[0])
		} else {
			var bases []string
			for _, f := range m.TargetFiles {
				bases = append(bases, filepath.Base(f))
			}
			filesStr = strings.Join(bases, ",")
		}
		parts = append(parts, fmt.Sprintf("t%d:%s[%s]", m.TurnNumber, status, filesStr))
	}

	return fmt.Sprintf("<nav:ribbon: trajectory turns=%d | %s />", n, strings.Join(parts, " -> "))
}

// ----------------------------------------------------------------------------
// Cloud Changefeed Connectors (Remote-Only Passthrough Models)
// ----------------------------------------------------------------------------

// ErrRemoteOnly is returned when attempting to invoke cloud changefeed connector operations
// without an active upstream Calera ICX tenant connection.
var ErrRemoteOnly = errors.New("icx: connector operations require active remote connection to Calera ICX tenant; local execution is disabled")

type ConnectorInfo struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`   // "github", "slack", "salesforce", "jira"
	Status     string         `json:"status"` // "active", "syncing", "paused", "error"
	LastSyncAt float64        `json:"last_sync_at,omitempty"`
	Config     map[string]any `json:"config,omitempty"`
}

type ListConnectorsRequest struct {
	SpaceID string `json:"space_id,omitempty"`
	Type    string `json:"type,omitempty"`
}

type ListConnectorsResponse struct {
	OK         bool            `json:"ok"`
	Count      int             `json:"count"`
	Connectors []ConnectorInfo `json:"connectors"`
	LatencyUs  float64         `json:"latency_us,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type RegisterConnectorRequest struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Config  map[string]any `json:"config,omitempty"`
	SpaceID string         `json:"space_id,omitempty"`
}

type RegisterConnectorResponse struct {
	OK          bool    `json:"ok"`
	ConnectorID string  `json:"connector_id"`
	Status      string  `json:"status"`
	LatencyUs   float64 `json:"latency_us,omitempty"`
	Error       string  `json:"error,omitempty"`
}

type SyncAuditRecord struct {
	SyncID          string `json:"sync_id"`
	ConnectorID     string `json:"connector_id"`
	EventsProcessed int    `json:"events_processed"`
	CISSeal         string `json:"cis_seal"`
	Timestamp       int64  `json:"timestamp"`
	Status          string `json:"status"`
}

type SyncAuditRequest struct {
	ConnectorID string `json:"connector_id,omitempty"`
	SpaceID     string `json:"space_id,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

type SyncAuditResponse struct {
	OK        bool              `json:"ok"`
	Count     int               `json:"count"`
	Records   []SyncAuditRecord `json:"records"`
	LatencyUs float64           `json:"latency_us,omitempty"`
	Error     string            `json:"error,omitempty"`
}

type TriggerSyncRequest struct {
	ConnectorID string `json:"connector_id"`
	SpaceID     string `json:"space_id,omitempty"`
	Force       bool   `json:"force,omitempty"`
}

type TriggerSyncResponse struct {
	OK          bool    `json:"ok"`
	SyncID      string  `json:"sync_id"`
	Status      string  `json:"status"`
	ScheduledAt int64   `json:"scheduled_at,omitempty"`
	LatencyUs   float64 `json:"latency_us,omitempty"`
	Error       string  `json:"error,omitempty"`
}

type GetConnectorDocumentRequest struct {
	ConnectorID string `json:"connector_id"`
	DocumentID  string `json:"document_id"`
	SpaceID     string `json:"space_id,omitempty"`
}

type GetConnectorDocumentResponse struct {
	OK         bool           `json:"ok"`
	DocumentID string         `json:"document_id"`
	Title      string         `json:"title,omitempty"`
	Content    string         `json:"content,omitempty"`
	SourceURI  string         `json:"source_uri,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	LatencyUs  float64        `json:"latency_us,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type FactCard struct {
	FactID    string         `json:"fact_id"`
	Family    string         `json:"family,omitempty"`
	Text      string         `json:"text"`
	SHA256    string         `json:"sha256,omitempty"`
	SourceID  string         `json:"source_id,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Timestamp int64          `json:"timestamp,omitempty"`
}

type InspectConnectorFactsRequest struct {
	ConnectorID string `json:"connector_id"`
	SpaceID     string `json:"space_id,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

type InspectConnectorFactsResponse struct {
	OK          bool       `json:"ok"`
	ConnectorID string     `json:"connector_id"`
	Count       int        `json:"count"`
	TotalFacts  int        `json:"total_facts"`
	Facts       []FactCard `json:"facts"`
	LatencyUs   float64    `json:"latency_us,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// ICXClient Interface
// ----------------------------------------------------------------------------
type ICXClient interface {
	// Core memory & discovery
	Remember(ctx context.Context, req RememberRequest) (*RememberResponse, error)
	RecallScoped(ctx context.Context, req RecallRequest) (*RecallResponse, error) // Clamped <= 10
	SearchFacts(ctx context.Context, req SearchRequest) (*SearchResponse, error)
	QuoteSlot(ctx context.Context, req QuoteSlotRequest) (*QuoteSlotResponse, error)
	SyncDelta(ctx context.Context, req SyncDeltaRequest) (*SyncDeltaResponse, error)
	ResetSession(ctx context.Context, spaceID string) (*ResetSessionResponse, error)
	PurgeSource(ctx context.Context, req PurgeSourceRequest) (*PurgeSourceResponse, error)
	InspectSpace(ctx context.Context, spaceID string) (*InspectSpaceResponse, error)

	// Simplicial AST Graph & Swarm Coordination
	MultihopWalk(ctx context.Context, req MultihopWalkRequest) (*MultihopWalkResponse, error)
	VarSet(ctx context.Context, req VarSetRequest) (*VarSetResponse, error)
	VarGet(ctx context.Context, req VarGetRequest) (*VarGetResponse, error)
	VarList(ctx context.Context, req VarListRequest) (*VarListResponse, error)
	SwarmState(ctx context.Context, req SwarmStateRequest) (*SwarmStateResponse, error)

	// Safe Execution Sandboxes
	Exec(ctx context.Context, req ExecRequest) (*ExecResponse, error)
	WasmExec(ctx context.Context, req WasmExecRequest) (*WasmExecResponse, error)
	ExecuteScript(ctx context.Context, req ExecuteScriptRequest) (*ExecuteScriptResponse, error)

	// Multi-Dimensional Codebase Memory (0D-4D)
	QueryCodebase(ctx context.Context, req QueryCodebaseRequest) (*QueryCodebaseResponse, error)
	ReadDimension(ctx context.Context, req ReadDimensionRequest) (*ReadDimensionResponse, error)
	ReadFileAST(ctx context.Context, req ReadFileASTRequest) (*ReadFileASTResponse, error)
	EditNode(ctx context.Context, req EditNodeRequest) (*EditNodeResponse, error)
	EditFile(ctx context.Context, req EditFileRequest) (*EditFileResponse, error)
	IngestDimension(ctx context.Context, req IngestDimensionRequest) (*IngestDimensionResponse, error)

	// Cloud Changefeed Connectors (Remote-Only Passthroughs)
	ListConnectors(ctx context.Context, req ListConnectorsRequest) (*ListConnectorsResponse, error)
	RegisterConnector(ctx context.Context, req RegisterConnectorRequest) (*RegisterConnectorResponse, error)
	SyncAudit(ctx context.Context, req SyncAuditRequest) (*SyncAuditResponse, error)
	TriggerSync(ctx context.Context, req TriggerSyncRequest) (*TriggerSyncResponse, error)
	GetConnectorDocument(ctx context.Context, req GetConnectorDocumentRequest) (*GetConnectorDocumentResponse, error)
	InspectConnectorFacts(ctx context.Context, req InspectConnectorFactsRequest) (*InspectConnectorFactsResponse, error)

	// Virtual Gateway & Generic Dispatch
	RecallEpistemicContext(ctx context.Context, req RecallEpistemicContextRequest) (*RecallEpistemicContextResponse, error)
	DispatchActuation(ctx context.Context, req DispatchActuationRequest) (*DispatchActuationResponse, error)
	CallTool(ctx context.Context, tool string, args map[string]any) (*MCPToolCallResult, error)

	// Turn Milestones (Step-Addressable Trajectory Recall)
	CrystallizeMilestone(ctx context.Context, spaceID string, m *TurnMilestone) (string, error)
	GetTurnMilestones(spaceID string) []*TurnMilestone
	GetTurnMilestone(spaceID string, turnNumber int) (*TurnMilestone, error)

	// Polyglot AST Ingestion
	IngestPolyglotAST(filePath string, src []byte) error

	// Telemetry & Lifecycle
	IsOffline() bool
	Close() error
}
