// SPDX-License-Identifier: Apache-2.0
// Package icx implements a high-performance JSON-RPC 2.0 client for Calera ICX.
// Supports HTTP POST, SSE, and Stdio pipes with Enterprise Headers
// (X-License-Key, X-Space-ID, Authorization: Bearer, X-MCP-Mode: virtual).
// Dynamically routes to the in-process local memory cache when offline or API key is absent.

package icx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrClientOffline is returned when the ICX client is in offline mode or has no remote transport.
var ErrClientOffline = errors.New("icx client is offline")

// Transport abstracts wire communication for JSON-RPC 2.0 requests.
type Transport interface {
	RoundTrip(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error)
	Close() error
}

// Client coordinates remote JSON-RPC transport with local in-process lattice fallback.
type Client struct {
	cfg         ClientConfig
	transport   Transport
	localEngine *LocalMemoryCache
	isOffline   atomic.Bool
	reqID       atomic.Uint64
}

// NewClient initializes an ICXClient according to the provided ClientConfig.
func NewClient(cfg ClientConfig) (ICXClient, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.SpaceID == "" {
		cfg.SpaceID = DefaultSpaceID
	}
	if cfg.MCPMode == "" {
		cfg.MCPMode = DefaultMCPMode
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.Timeout}
	}

	c := &Client{
		cfg:         cfg,
		localEngine: NewLocalLattice(),
	}

	// Fallback to local lattice if APIKey is missing or Offline is requested
	if cfg.APIKey == "" || cfg.Offline || cfg.TransportMode == "local" {
		c.isOffline.Store(true)
		return c, nil
	}

	// Initialize chosen transport
	switch cfg.TransportMode {
	case "stdio":
		tr, err := newStdioTransport(cfg)
		if err != nil {
			if !cfg.Strict {
				c.isOffline.Store(true)
				return c, nil
			}
			return nil, err
		}
		c.transport = tr

	case "sse":
		tr := newSSETransport(cfg)
		c.transport = tr

	default: // "http" or unset
		tr := newHTTPPostTransport(cfg)
		c.transport = tr
	}

	return c, nil
}

func (c *Client) IsOffline() bool {
	return c.isOffline.Load()
}

// SetNextRequestID sets the base ID for subsequent JSON-RPC requests.
func (c *Client) SetNextRequestID(id uint64) {
	c.reqID.Store(id)
}

func (c *Client) Close() error {
	if c.transport != nil {
		return c.transport.Close()
	}
	return nil
}

// IngestPolyglotAST delegates polyglot AST ingestion to the local engine.
func (c *Client) IngestPolyglotAST(filePath string, src []byte) error {
	if c.localEngine != nil {
		return c.localEngine.IngestPolyglotAST(filePath, src)
	}
	return nil
}

// DispatchForTest exposes internal dispatch for verifying ErrClientOffline and fallback routing.
func (c *Client) DispatchForTest(ctx context.Context, toolName string, params any) error {
	var target any
	return c.dispatch(ctx, toolName, params, &target)
}

// dispatch sends a tool call over the wire or falls back to local engine.
func (c *Client) dispatch(ctx context.Context, toolName string, params any, resultTarget any) error {
	if c.isOffline.Load() || c.transport == nil {
		return ErrClientOffline
	}

	reqID := c.reqID.Add(1)
	argsMap, err := structToMap(params)
	if err != nil {
		return err
	}

	rpcReq := &JSONRPCRequest{
		JSONRPC: JSONRPCVersion,
		ID:      reqID,
		Method:  "tools/call",
		Params: MCPToolCallParams{
			Name:      toolName,
			Arguments: argsMap,
		},
	}

	rpcResp, err := c.transport.RoundTrip(ctx, rpcReq)
	if err != nil {
		if isConnectorTool(toolName) {
			return err
		}
		if c.cfg.Strict {
			return fmt.Errorf("remote ICX failure in strict mode: %w", err)
		}
		c.isOffline.Store(true)
		return err // caller will fall back to local
	}

	if rpcResp.Error != nil {
		return rpcResp.Error
	}

	if resultTarget != nil && len(rpcResp.Result) > 0 {
		var mcpRes MCPToolCallResult
		if err := json.Unmarshal(rpcResp.Result, &mcpRes); err == nil && len(mcpRes.Content) > 0 {
			if mcpRes.IsError {
				return fmt.Errorf("MCP error: %s", mcpRes.Content[0].Text)
			}
			return json.Unmarshal([]byte(mcpRes.Content[0].Text), resultTarget)
		}
		return json.Unmarshal(rpcResp.Result, resultTarget)
	}

	return nil
}

func structToMap(v any) (map[string]any, error) {
	if v == nil {
		return make(map[string]any), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	err = json.Unmarshal(b, &m)
	return m, err
}

// ----------------------------------------------------------------------------
// Certified Tool 1: icx_remember
// ----------------------------------------------------------------------------
func (c *Client) Remember(ctx context.Context, req RememberRequest) (*RememberResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.Remember(ctx, req)
	}
	var resp RememberResponse
	err := c.dispatch(ctx, "icx_remember", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.Remember(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 2: icx_recall_scoped (Clamped strictly top_k <= 10)
// ----------------------------------------------------------------------------
func (c *Client) RecallScoped(ctx context.Context, req RecallRequest) (*RecallResponse, error) {
	if req.TopK <= 0 || req.TopK > MaxRecallTopK {
		req.TopK = MaxRecallTopK
	}
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.RecallScoped(ctx, req)
	}
	var resp RecallResponse
	err := c.dispatch(ctx, "icx_recall_scoped", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.RecallScoped(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 3: icx_search_facts
// ----------------------------------------------------------------------------
func (c *Client) SearchFacts(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.SearchFacts(ctx, req)
	}
	var resp SearchResponse
	err := c.dispatch(ctx, "icx_search_facts", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.SearchFacts(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 4: icx_quote_slot (Deterministic <0.002ms byte-exact quote)
// ----------------------------------------------------------------------------
func (c *Client) QuoteSlot(ctx context.Context, req QuoteSlotRequest) (*QuoteSlotResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.QuoteSlot(ctx, req)
	}
	var resp QuoteSlotResponse
	err := c.dispatch(ctx, "icx_quote_slot", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.QuoteSlot(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 5: icx_sync_delta
// ----------------------------------------------------------------------------
func (c *Client) SyncDelta(ctx context.Context, req SyncDeltaRequest) (*SyncDeltaResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.SyncDelta(ctx, req)
	}
	var resp SyncDeltaResponse
	err := c.dispatch(ctx, "icx_sync_delta", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.SyncDelta(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 6: icx_reset_session
// ----------------------------------------------------------------------------
func (c *Client) ResetSession(ctx context.Context, spaceID string) (*ResetSessionResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.ResetSession(ctx, spaceID)
	}
	var resp ResetSessionResponse
	err := c.dispatch(ctx, "icx_reset_session", ResetSessionRequest{SpaceID: spaceID}, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.ResetSession(ctx, spaceID)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 7: icx_multihop_walk (5-hop simplicial causal AST walk)
// ----------------------------------------------------------------------------
func (c *Client) MultihopWalk(ctx context.Context, req MultihopWalkRequest) (*MultihopWalkResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.MultihopWalk(ctx, req)
	}
	var resp MultihopWalkResponse
	err := c.dispatch(ctx, "icx_multihop_walk", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.MultihopWalk(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 8: icx_var_set
// ----------------------------------------------------------------------------
func (c *Client) VarSet(ctx context.Context, req VarSetRequest) (*VarSetResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.VarSet(ctx, req)
	}
	var resp VarSetResponse
	err := c.dispatch(ctx, "icx_var_set", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.VarSet(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 9: icx_var_get
// ----------------------------------------------------------------------------
func (c *Client) VarGet(ctx context.Context, req VarGetRequest) (*VarGetResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.VarGet(ctx, req)
	}
	var resp VarGetResponse
	err := c.dispatch(ctx, "icx_var_get", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.VarGet(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 22: icx_var_list
// ----------------------------------------------------------------------------
func (c *Client) VarList(ctx context.Context, req VarListRequest) (*VarListResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.VarList(ctx, req)
	}
	var resp VarListResponse
	err := c.dispatch(ctx, "icx_var_list", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.VarList(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 10: icx_exec
// ----------------------------------------------------------------------------
func (c *Client) Exec(ctx context.Context, req ExecRequest) (*ExecResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.Exec(ctx, req)
	}
	var resp ExecResponse
	err := c.dispatch(ctx, "icx_exec", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.Exec(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 11: icx_wasm_exec
// ----------------------------------------------------------------------------
func (c *Client) WasmExec(ctx context.Context, req WasmExecRequest) (*WasmExecResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.WasmExec(ctx, req)
	}
	var resp WasmExecResponse
	err := c.dispatch(ctx, "icx_wasm_exec", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.WasmExec(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 12: icx_inspect_space
// ----------------------------------------------------------------------------
func (c *Client) InspectSpace(ctx context.Context, spaceID string) (*InspectSpaceResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.InspectSpace(ctx, spaceID)
	}
	var resp InspectSpaceResponse
	err := c.dispatch(ctx, "icx_inspect_space", InspectSpaceRequest{SpaceID: spaceID}, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.InspectSpace(ctx, spaceID)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 13: icx_purge_source
// ----------------------------------------------------------------------------
func (c *Client) PurgeSource(ctx context.Context, req PurgeSourceRequest) (*PurgeSourceResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.PurgeSource(ctx, req)
	}
	var resp PurgeSourceResponse
	err := c.dispatch(ctx, "icx_purge_source", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.PurgeSource(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 14: icx_swarm_state
// ----------------------------------------------------------------------------
func (c *Client) SwarmState(ctx context.Context, req SwarmStateRequest) (*SwarmStateResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.SwarmState(ctx, req)
	}
	var resp SwarmStateResponse
	err := c.dispatch(ctx, "icx_swarm_state", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.SwarmState(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 15: icx_query_codebase
// ----------------------------------------------------------------------------
func (c *Client) QueryCodebase(ctx context.Context, req QueryCodebaseRequest) (*QueryCodebaseResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.QueryCodebase(ctx, req)
	}
	var resp QueryCodebaseResponse
	err := c.dispatch(ctx, "icx_query_codebase", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.QueryCodebase(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 16: icx_read_dimension
// ----------------------------------------------------------------------------
func (c *Client) ReadDimension(ctx context.Context, req ReadDimensionRequest) (*ReadDimensionResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.ReadDimension(ctx, req)
	}
	var resp ReadDimensionResponse
	err := c.dispatch(ctx, "icx_read_dimension", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.ReadDimension(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 17: icx_read_file_ast
// ----------------------------------------------------------------------------
func (c *Client) ReadFileAST(ctx context.Context, req ReadFileASTRequest) (*ReadFileASTResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.ReadFileAST(ctx, req)
	}
	var resp ReadFileASTResponse
	err := c.dispatch(ctx, "icx_read_file_ast", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.ReadFileAST(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 18: icx_edit_node
// ----------------------------------------------------------------------------
func (c *Client) EditNode(ctx context.Context, req EditNodeRequest) (*EditNodeResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.EditNode(ctx, req)
	}
	var resp EditNodeResponse
	err := c.dispatch(ctx, "icx_edit_node", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.EditNode(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 19: icx_edit_file
// ----------------------------------------------------------------------------
func (c *Client) EditFile(ctx context.Context, req EditFileRequest) (*EditFileResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.EditFile(ctx, req)
	}
	var resp EditFileResponse
	err := c.dispatch(ctx, "icx_edit_file", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.EditFile(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 20: icx_ingest_dimension
// ----------------------------------------------------------------------------
func (c *Client) IngestDimension(ctx context.Context, req IngestDimensionRequest) (*IngestDimensionResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.IngestDimension(ctx, req)
	}
	var resp IngestDimensionResponse
	err := c.dispatch(ctx, "icx_ingest_dimension", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.IngestDimension(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 21: icx_execute_script
// ----------------------------------------------------------------------------
func (c *Client) ExecuteScript(ctx context.Context, req ExecuteScriptRequest) (*ExecuteScriptResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.ExecuteScript(ctx, req)
	}
	var resp ExecuteScriptResponse
	err := c.dispatch(ctx, "icx_execute_script", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.ExecuteScript(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// ----------------------------------------------------------------------------
// Virtual Mode Gateway & CallTool
// ----------------------------------------------------------------------------
func (c *Client) RecallEpistemicContext(ctx context.Context, req RecallEpistemicContextRequest) (*RecallEpistemicContextResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.RecallEpistemicContext(ctx, req)
	}
	var resp RecallEpistemicContextResponse
	err := c.dispatch(ctx, "recall_epistemic_context", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.RecallEpistemicContext(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DispatchActuation(ctx context.Context, req DispatchActuationRequest) (*DispatchActuationResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.DispatchActuation(ctx, req)
	}
	var resp DispatchActuationResponse
	err := c.dispatch(ctx, "dispatch_actuation", req, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.DispatchActuation(ctx, req)
		}
		return nil, err
	}
	return &resp, nil
}

// GetActuationReceipts retrieves all crystallized actuation receipts from the underlying lattice.
func (c *Client) GetActuationReceipts(spaceID string) []ActuationReceipt {
	if c.localEngine != nil {
		return c.localEngine.GetActuationReceipts(spaceID)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Cloud Changefeed Connectors (Remote-Only Passthroughs)
// ----------------------------------------------------------------------------

func isConnectorTool(tool string) bool {
	switch tool {
	case "icx_list_connectors", "icx_register_connector", "icx_sync_audit",
		"icx_trigger_sync", "icx_get_connector_document", "icx_inspect_connector_facts":
		return true
	default:
		return false
	}
}

func (c *Client) ListConnectors(ctx context.Context, req ListConnectorsRequest) (*ListConnectorsResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return nil, ErrRemoteOnly
	}
	var resp ListConnectorsResponse
	if err := c.dispatch(ctx, "icx_list_connectors", req, &resp); err != nil {
		return nil, err // Strict remote-only: NEVER fallback to local engine
	}
	return &resp, nil
}

func (c *Client) RegisterConnector(ctx context.Context, req RegisterConnectorRequest) (*RegisterConnectorResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return nil, ErrRemoteOnly
	}
	var resp RegisterConnectorResponse
	if err := c.dispatch(ctx, "icx_register_connector", req, &resp); err != nil {
		return nil, err // Strict remote-only: NEVER fallback to local engine
	}
	return &resp, nil
}

func (c *Client) SyncAudit(ctx context.Context, req SyncAuditRequest) (*SyncAuditResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return nil, ErrRemoteOnly
	}
	var resp SyncAuditResponse
	if err := c.dispatch(ctx, "icx_sync_audit", req, &resp); err != nil {
		return nil, err // Strict remote-only: NEVER fallback to local engine
	}
	return &resp, nil
}

func (c *Client) TriggerSync(ctx context.Context, req TriggerSyncRequest) (*TriggerSyncResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return nil, ErrRemoteOnly
	}
	var resp TriggerSyncResponse
	if err := c.dispatch(ctx, "icx_trigger_sync", req, &resp); err != nil {
		return nil, err // Strict remote-only: NEVER fallback to local engine
	}
	return &resp, nil
}

func (c *Client) GetConnectorDocument(ctx context.Context, req GetConnectorDocumentRequest) (*GetConnectorDocumentResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return nil, ErrRemoteOnly
	}
	var resp GetConnectorDocumentResponse
	if err := c.dispatch(ctx, "icx_get_connector_document", req, &resp); err != nil {
		return nil, err // Strict remote-only: NEVER fallback to local engine
	}
	return &resp, nil
}

func (c *Client) InspectConnectorFacts(ctx context.Context, req InspectConnectorFactsRequest) (*InspectConnectorFactsResponse, error) {
	if c.isOffline.Load() || c.transport == nil {
		return nil, ErrRemoteOnly
	}
	var resp InspectConnectorFactsResponse
	if err := c.dispatch(ctx, "icx_inspect_connector_facts", req, &resp); err != nil {
		return nil, err // Strict remote-only: NEVER fallback to local engine
	}
	return &resp, nil
}

func (c *Client) CallTool(ctx context.Context, tool string, args map[string]any) (*MCPToolCallResult, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.CallTool(ctx, tool, args)
	}

	reqID := c.reqID.Add(1)
	rpcReq := &JSONRPCRequest{
		JSONRPC: JSONRPCVersion,
		ID:      reqID,
		Method:  "tools/call",
		Params: MCPToolCallParams{
			Name:      tool,
			Arguments: args,
		},
	}

	rpcResp, err := c.transport.RoundTrip(ctx, rpcReq)
	if err != nil {
		if isConnectorTool(tool) {
			return nil, err
		}
		if !c.cfg.Strict {
			return c.localEngine.CallTool(ctx, tool, args)
		}
		return nil, err
	}

	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}

	var mcpRes MCPToolCallResult
	if err := json.Unmarshal(rpcResp.Result, &mcpRes); err == nil {
		return &mcpRes, nil
	}

	return &MCPToolCallResult{
		Content: []MCPContent{{Type: "text", Text: string(rpcResp.Result)}},
		IsError: false,
	}, nil
}

// ----------------------------------------------------------------------------
// Turn Milestones (Step-Addressable Trajectory Recall)
// ----------------------------------------------------------------------------

func (c *Client) CrystallizeMilestone(ctx context.Context, spaceID string, m *TurnMilestone) (string, error) {
	if c.isOffline.Load() || c.transport == nil {
		return c.localEngine.CrystallizeMilestone(ctx, spaceID, m)
	}
	var resp struct {
		FactID string `json:"fact_id"`
	}
	err := c.dispatch(ctx, "icx_crystallize_milestone", map[string]any{
		"space_id":  spaceID,
		"milestone": m.ToPayload(),
	}, &resp)
	if err != nil {
		if !c.cfg.Strict {
			return c.localEngine.CrystallizeMilestone(ctx, spaceID, m)
		}
		return "", err
	}
	return resp.FactID, nil
}

func (c *Client) GetTurnMilestones(spaceID string) []*TurnMilestone {
	return c.localEngine.GetTurnMilestones(spaceID)
}

func (c *Client) GetTurnMilestone(spaceID string, turnNumber int) (*TurnMilestone, error) {
	return c.localEngine.GetTurnMilestone(spaceID, turnNumber)
}

// ----------------------------------------------------------------------------
// TRANSPORTS
// ----------------------------------------------------------------------------

// HTTPPostTransport implements Enterprise HTTP POST JSON-RPC 2.0.
type HTTPPostTransport struct {
	cfg        ClientConfig
	httpClient *http.Client
	targetURL  string
}

func newHTTPPostTransport(cfg ClientConfig) *HTTPPostTransport {
	endpoint := cfg.BaseURL
	if !strings.HasSuffix(endpoint, "/mcp") && !strings.HasSuffix(endpoint, "/rpc") {
		endpoint = endpoint + "/mcp"
	}
	return &HTTPPostTransport{
		cfg:        cfg,
		httpClient: cfg.HTTPClient,
		targetURL:  endpoint,
	}
}

func (t *HTTPPostTransport) RoundTrip(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	// Inject Enterprise Headers
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	httpReq.Header.Set("X-License-Key", t.cfg.APIKey)
	httpReq.Header.Set("X-Space-ID", t.cfg.SpaceID)
	httpReq.Header.Set("X-MCP-Mode", t.cfg.MCPMode)

	resp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d error from ICX server: %s", resp.StatusCode, string(respBytes))
	}

	var rpcResp JSONRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, err
	}
	return &rpcResp, nil
}

func (t *HTTPPostTransport) Close() error {
	return nil
}

// SSETransport implements Server-Sent Events JSON-RPC 2.0.
type SSETransport struct {
	cfg        ClientConfig
	httpClient *http.Client
	postURL    string
}

func newSSETransport(cfg ClientConfig) *SSETransport {
	postURL := cfg.BaseURL
	if !strings.HasSuffix(postURL, "/mcp") && !strings.HasSuffix(postURL, "/rpc") {
		postURL = postURL + "/mcp"
	}
	return &SSETransport{
		cfg:        cfg,
		httpClient: cfg.HTTPClient,
		postURL:    postURL,
	}
}

func (t *SSETransport) RoundTrip(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	// For SSE transport, requests are posted to the target endpoint with Accept: text/event-stream
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.postURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream, application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	httpReq.Header.Set("X-License-Key", t.cfg.APIKey)
	httpReq.Header.Set("X-Space-ID", t.cfg.SpaceID)
	httpReq.Header.Set("X-MCP-Mode", t.cfg.MCPMode)

	resp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("SSE HTTP %d error: %s", resp.StatusCode, string(respBytes))
	}

	// Handle SSE event stream or standard JSON response
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		scanner := bufio.NewScanner(resp.Body)
		var lastData string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				lastData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if lastData != "" {
			var rpcResp JSONRPCResponse
			if err := json.Unmarshal([]byte(lastData), &rpcResp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal SSE event data: %w", err)
			}
			return &rpcResp, nil
		}
		return nil, fmt.Errorf("empty SSE event stream")
	}

	var rpcResp JSONRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, err
	}
	return &rpcResp, nil
}

func (t *SSETransport) Close() error {
	return nil
}

// StdioTransport implements Stdio pipe JSON-RPC 2.0 with Content-Length / newline framing.
type StdioTransport struct {
	r       *bufio.Reader
	w       io.Writer
	closer  io.Closer
	cmd     *exec.Cmd
	pending sync.Map
	closed  atomic.Bool
}

func newStdioTransport(cfg ClientConfig) (*StdioTransport, error) {
	var r io.Reader = cfg.StdioReader
	var w io.Writer = cfg.StdioWriter
	var closer io.Closer = nil
	var cmd *exec.Cmd

	if r == nil && len(cfg.Command) > 0 {
		cmd = exec.Command(cfg.Command[0], cfg.Command[1:]...)
		stdinPipe, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		r = stdoutPipe
		w = stdinPipe
		closer = stdinPipe
	}

	if r == nil || w == nil {
		return nil, fmt.Errorf("stdio transport requires StdioReader/StdioWriter or Command")
	}

	st := &StdioTransport{
		r:      bufio.NewReader(r),
		w:      w,
		closer: closer,
		cmd:    cmd,
	}

	go st.readLoop()
	return st, nil
}

func (st *StdioTransport) readLoop() {
	defer func() {
		// Close all pending response channels on EOF or error to prevent caller goroutines hanging.
		st.pending.Range(func(key, value any) bool {
			if chVal, ok := st.pending.LoadAndDelete(key); ok {
				if ch, ok := chVal.(chan *JSONRPCResponse); ok {
					close(ch)
				}
			}
			return true
		})
	}()

	for !st.closed.Load() {
		msg, err := st.readNextMessage()
		if err != nil {
			break
		}
		var resp JSONRPCResponse
		if err := json.Unmarshal(msg, &resp); err == nil && resp.ID != nil {
			var key string
			switch v := resp.ID.(type) {
			case float64:
				key = strconv.FormatUint(uint64(v), 10)
			case uint64:
				key = strconv.FormatUint(v, 10)
			case int64:
				key = strconv.FormatInt(v, 10)
			case string:
				key = v
			default:
				key = fmt.Sprintf("%v", v)
			}
			if chVal, ok := st.pending.LoadAndDelete(key); ok {
				ch := chVal.(chan *JSONRPCResponse)
				ch <- &resp
			}
		}
	}
}

func (st *StdioTransport) readNextMessage() ([]byte, error) {
	for {
		line, err := st.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if strings.HasPrefix(trimmed, "Content-Length:") {
			parts := strings.Split(trimmed, ":")
			if len(parts) != 2 {
				continue
			}
			length, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil || length < 0 || length > MaxHeaderBytes {
				continue
			}
			// Skip headers until empty line
			for {
				hLine, err := st.r.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimSpace(hLine) == "" {
					break
				}
			}
			body := make([]byte, length)
			if _, err := io.ReadFull(st.r, body); err != nil {
				return nil, err
			}
			return body, nil
		}

		// Fallback: newline-delimited JSON
		if strings.HasPrefix(trimmed, "{") {
			return []byte(trimmed), nil
		}
	}
}

func (st *StdioTransport) RoundTrip(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if st.closed.Load() {
		return nil, fmt.Errorf("stdio transport closed")
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	key := fmt.Sprintf("%v", req.ID)
	resChan := make(chan *JSONRPCResponse, 1)
	st.pending.Store(key, resChan)
	defer st.pending.Delete(key)

	// Send message with Content-Length framing
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(reqBytes))
	if _, err := st.w.Write([]byte(header)); err != nil {
		return nil, err
	}
	if _, err := st.w.Write(reqBytes); err != nil {
		return nil, err
	}

	select {
	case resp, ok := <-resChan:
		if !ok || resp == nil {
			return nil, fmt.Errorf("stdio transport connection closed or EOF")
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (st *StdioTransport) Close() error {
	st.closed.Store(true)
	if st.closer != nil {
		_ = st.closer.Close()
	}
	if st.cmd != nil && st.cmd.Process != nil {
		_ = st.cmd.Process.Kill()
	}
	st.pending.Range(func(key, value any) bool {
		if chVal, ok := st.pending.LoadAndDelete(key); ok {
			if ch, ok := chVal.(chan *JSONRPCResponse); ok {
				close(ch)
			}
		}
		return true
	})
	return nil
}
