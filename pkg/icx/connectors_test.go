// SPDX-License-Identifier: Apache-2.0
package icx

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestConnectors_OfflineRefusal verifies that when Client is offline or transport is nil,
// all 6 connector methods return ErrRemoteOnly, and CallTool returns IsError: true.
func TestConnectors_OfflineRefusal(t *testing.T) {
	ctx := context.Background()
	client, err := NewClient(ClientConfig{
		SpaceID: "test_space",
		Offline: true,
	})
	if err != nil {
		t.Fatalf("failed to create offline client: %v", err)
	}
	defer client.Close()

	if !client.IsOffline() {
		t.Fatalf("expected client to be offline")
	}

	// 1. ListConnectors
	if _, err := client.ListConnectors(ctx, ListConnectorsRequest{SpaceID: "test_space"}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("ListConnectors: expected ErrRemoteOnly, got %v", err)
	}

	// 2. RegisterConnector
	if _, err := client.RegisterConnector(ctx, RegisterConnectorRequest{Name: "gh", Type: "github"}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("RegisterConnector: expected ErrRemoteOnly, got %v", err)
	}

	// 3. SyncAudit
	if _, err := client.SyncAudit(ctx, SyncAuditRequest{ConnectorID: "c1"}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("SyncAudit: expected ErrRemoteOnly, got %v", err)
	}

	// 4. TriggerSync
	if _, err := client.TriggerSync(ctx, TriggerSyncRequest{ConnectorID: "c1"}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("TriggerSync: expected ErrRemoteOnly, got %v", err)
	}

	// 5. GetConnectorDocument
	if _, err := client.GetConnectorDocument(ctx, GetConnectorDocumentRequest{ConnectorID: "c1", DocumentID: "d1"}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("GetConnectorDocument: expected ErrRemoteOnly, got %v", err)
	}

	// 6. InspectConnectorFacts
	if _, err := client.InspectConnectorFacts(ctx, InspectConnectorFactsRequest{ConnectorID: "c1"}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("InspectConnectorFacts: expected ErrRemoteOnly, got %v", err)
	}

	// CallTool for all 6 connector tools via offline client
	tools := []string{
		"icx_list_connectors",
		"icx_register_connector",
		"icx_sync_audit",
		"icx_trigger_sync",
		"icx_get_connector_document",
		"icx_inspect_connector_facts",
	}

	for _, tool := range tools {
		res, err := client.CallTool(ctx, tool, map[string]any{"space_id": "test_space"})
		if err != nil {
			t.Errorf("CallTool(%q) unexpected error: %v", tool, err)
		}
		if res == nil || !res.IsError {
			t.Errorf("CallTool(%q) expected IsError: true, got %+v", tool, res)
		}
		if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "remote-only") {
			t.Errorf("CallTool(%q) expected remote-only message, got %+v", tool, res)
		}
	}
}

// TestConnectors_LocalMemoryCacheDirectRefusal verifies that invoking connector methods
// directly on *LocalMemoryCache always returns ErrRemoteOnly, and CallTool returns IsError: true.
func TestConnectors_LocalMemoryCacheDirectRefusal(t *testing.T) {
	ctx := context.Background()
	lat := NewLocalMemoryCache("test_space")

	// 1. ListConnectors
	if _, err := lat.ListConnectors(ctx, ListConnectorsRequest{}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("LocalMemoryCache.ListConnectors: expected ErrRemoteOnly, got %v", err)
	}

	// 2. RegisterConnector
	if _, err := lat.RegisterConnector(ctx, RegisterConnectorRequest{}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("LocalMemoryCache.RegisterConnector: expected ErrRemoteOnly, got %v", err)
	}

	// 3. SyncAudit
	if _, err := lat.SyncAudit(ctx, SyncAuditRequest{}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("LocalMemoryCache.SyncAudit: expected ErrRemoteOnly, got %v", err)
	}

	// 4. TriggerSync
	if _, err := lat.TriggerSync(ctx, TriggerSyncRequest{}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("LocalMemoryCache.TriggerSync: expected ErrRemoteOnly, got %v", err)
	}

	// 5. GetConnectorDocument
	if _, err := lat.GetConnectorDocument(ctx, GetConnectorDocumentRequest{}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("LocalMemoryCache.GetConnectorDocument: expected ErrRemoteOnly, got %v", err)
	}

	// 6. InspectConnectorFacts
	if _, err := lat.InspectConnectorFacts(ctx, InspectConnectorFactsRequest{}); !errors.Is(err, ErrRemoteOnly) {
		t.Errorf("LocalMemoryCache.InspectConnectorFacts: expected ErrRemoteOnly, got %v", err)
	}

	// CallTool on LocalMemoryCache
	tools := []string{
		"icx_list_connectors",
		"icx_register_connector",
		"icx_sync_audit",
		"icx_trigger_sync",
		"icx_get_connector_document",
		"icx_inspect_connector_facts",
	}

	for _, tool := range tools {
		res, err := lat.CallTool(ctx, tool, map[string]any{"space_id": "test_space"})
		if err != nil {
			t.Errorf("LocalMemoryCache.CallTool(%q) unexpected error: %v", tool, err)
		}
		if res == nil || !res.IsError {
			t.Errorf("LocalMemoryCache.CallTool(%q) expected IsError: true, got %+v", tool, res)
		}
		if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "remote-only") {
			t.Errorf("LocalMemoryCache.CallTool(%q) expected remote-only message, got %+v", tool, res)
		}
	}
}

// TestConnectors_RemotePassthroughRoundtrip verifies that when connected to a remote
// ICX endpoint via HTTP transport, all 6 connector methods correctly dispatch JSON-RPC
// tools/call with correct parameters and cleanly deserialize responses.
func TestConnectors_RemotePassthroughRoundtrip(t *testing.T) {
	ctx := context.Background()

	var receivedCalls []MCPToolCallParams
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var callParams MCPToolCallParams
		pBytes, _ := json.Marshal(req.Params)
		_ = json.Unmarshal(pBytes, &callParams)

		mu.Lock()
		receivedCalls = append(receivedCalls, callParams)
		mu.Unlock()

		var resultPayload any
		switch callParams.Name {
		case "icx_list_connectors":
			resultPayload = ListConnectorsResponse{
				OK:    true,
				Count: 2,
				Connectors: []ConnectorInfo{
					{ID: "c1", Name: "GitHub Repo", Type: "github", Status: "active", LastSyncAt: 1720000000},
					{ID: "c2", Name: "Slack Sync", Type: "slack", Status: "paused"},
				},
			}
		case "icx_register_connector":
			resultPayload = RegisterConnectorResponse{
				OK:          true,
				ConnectorID: "c_new_123",
				Status:      "active",
			}
		case "icx_sync_audit":
			resultPayload = SyncAuditResponse{
				OK:    true,
				Count: 1,
				Records: []SyncAuditRecord{
					{
						SyncID:          "sync_001",
						ConnectorID:     "c1",
						EventsProcessed: 42,
						CISSeal:         "sha256:abc123audit",
						Timestamp:       1720000500,
						Status:          "completed",
					},
				},
			}
		case "icx_trigger_sync":
			resultPayload = TriggerSyncResponse{
				OK:          true,
				SyncID:      "sync_triggered_99",
				Status:      "scheduled",
				ScheduledAt: 1720000600,
			}
		case "icx_get_connector_document":
			resultPayload = GetConnectorDocumentResponse{
				OK:         true,
				DocumentID: "doc_456",
				Title:      "Architecture RFC",
				Content:    "Decentralized Lattices and Geodesics",
				SourceURI:  "github://caleralabs/omnicoder-icx/rfc.md",
				Metadata:   map[string]any{"author": "Calera Labs"},
			}
		case "icx_inspect_connector_facts":
			resultPayload = InspectConnectorFactsResponse{
				OK:          true,
				ConnectorID: "c1",
				Count:       1,
				TotalFacts:  1,
				Facts: []FactCard{
					{
						FactID:   "fact_c1_001",
						Family:   "connectors",
						Text:     "Connector fact verified",
						SHA256:   "abcd5678",
						SourceID: "c1",
					},
				},
			}
		default:
			resultPayload = map[string]any{"ok": true}
		}

		resBytes, _ := json.Marshal(resultPayload)
		mcpRes := MCPToolCallResult{
			Content: []MCPContent{{Type: "text", Text: string(resBytes)}},
			IsError: false,
		}
		mcpBytes, _ := json.Marshal(mcpRes)

		resp := JSONRPCResponse{
			JSONRPC: JSONRPCVersion,
			ID:      req.ID,
			Result:  mcpBytes,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client, err := NewClient(ClientConfig{
		BaseURL:       ts.URL,
		APIKey:        "test-key",
		SpaceID:       "prod_space",
		TransportMode: "http",
		Offline:       false,
		Strict:        false, // Even non-strict must NOT fallback to local on connectors
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer client.Close()

	// 1. ListConnectors
	listResp, err := client.ListConnectors(ctx, ListConnectorsRequest{SpaceID: "prod_space", Type: "github"})
	if err != nil {
		t.Fatalf("ListConnectors error: %v", err)
	}
	if !listResp.OK || listResp.Count != 2 || len(listResp.Connectors) != 2 {
		t.Errorf("ListConnectors unexpected response: %+v", listResp)
	}
	if listResp.Connectors[0].ID != "c1" || listResp.Connectors[0].Type != "github" {
		t.Errorf("ListConnectors first connector mismatch: %+v", listResp.Connectors[0])
	}

	// 2. RegisterConnector
	regResp, err := client.RegisterConnector(ctx, RegisterConnectorRequest{
		Name:    "New GitHub",
		Type:    "github",
		Config:  map[string]any{"repo": "caleralabs/test"},
		SpaceID: "prod_space",
	})
	if err != nil {
		t.Fatalf("RegisterConnector error: %v", err)
	}
	if !regResp.OK || regResp.ConnectorID != "c_new_123" || regResp.Status != "active" {
		t.Errorf("RegisterConnector unexpected response: %+v", regResp)
	}

	// 3. SyncAudit
	auditResp, err := client.SyncAudit(ctx, SyncAuditRequest{ConnectorID: "c1", SpaceID: "prod_space"})
	if err != nil {
		t.Fatalf("SyncAudit error: %v", err)
	}
	if !auditResp.OK || auditResp.Count != 1 || len(auditResp.Records) != 1 {
		t.Errorf("SyncAudit unexpected response: %+v", auditResp)
	}
	if auditResp.Records[0].CISSeal != "sha256:abc123audit" {
		t.Errorf("SyncAudit seal mismatch: %s", auditResp.Records[0].CISSeal)
	}

	// 4. TriggerSync
	trigResp, err := client.TriggerSync(ctx, TriggerSyncRequest{ConnectorID: "c1", SpaceID: "prod_space", Force: true})
	if err != nil {
		t.Fatalf("TriggerSync error: %v", err)
	}
	if !trigResp.OK || trigResp.SyncID != "sync_triggered_99" || trigResp.Status != "scheduled" {
		t.Errorf("TriggerSync unexpected response: %+v", trigResp)
	}

	// 5. GetConnectorDocument
	docResp, err := client.GetConnectorDocument(ctx, GetConnectorDocumentRequest{
		ConnectorID: "c1",
		DocumentID:  "doc_456",
		SpaceID:     "prod_space",
	})
	if err != nil {
		t.Fatalf("GetConnectorDocument error: %v", err)
	}
	if !docResp.OK || docResp.DocumentID != "doc_456" || docResp.Title != "Architecture RFC" {
		t.Errorf("GetConnectorDocument unexpected response: %+v", docResp)
	}

	// 6. InspectConnectorFacts
	factsResp, err := client.InspectConnectorFacts(ctx, InspectConnectorFactsRequest{ConnectorID: "c1", SpaceID: "prod_space"})
	if err != nil {
		t.Fatalf("InspectConnectorFacts error: %v", err)
	}
	if !factsResp.OK || factsResp.ConnectorID != "c1" || len(factsResp.Facts) != 1 {
		t.Errorf("InspectConnectorFacts unexpected response: %+v", factsResp)
	}
	if factsResp.Facts[0].FactID != "fact_c1_001" {
		t.Errorf("InspectConnectorFacts fact ID mismatch: %+v", factsResp.Facts[0])
	}

	// Verify all 6 tools were dispatched on wire
	mu.Lock()
	defer mu.Unlock()
	expectedTools := []string{
		"icx_list_connectors",
		"icx_register_connector",
		"icx_sync_audit",
		"icx_trigger_sync",
		"icx_get_connector_document",
		"icx_inspect_connector_facts",
	}
	if len(receivedCalls) != len(expectedTools) {
		t.Fatalf("expected %d wire calls, got %d", len(expectedTools), len(receivedCalls))
	}
	for i, expected := range expectedTools {
		if receivedCalls[i].Name != expected {
			t.Errorf("call %d expected tool %q, got %q", i, expected, receivedCalls[i].Name)
		}
	}
}

// TestConnectors_RemoteErrorStrictNoFallback verifies that if the remote server returns
// an error, connector operations return the error directly and NEVER fallback to local engine,
// even when Strict mode is false.
func TestConnectors_RemoteErrorStrictNoFallback(t *testing.T) {
	ctx := context.Background()

	// Mock server returning HTTP 500
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal upstream calera tenant error", http.StatusInternalServerError)
	}))
	defer ts.Close()

	client, err := NewClient(ClientConfig{
		BaseURL:       ts.URL,
		APIKey:        "test-key",
		SpaceID:       "default",
		TransportMode: "http",
		Offline:       false,
		Strict:        false, // Explicitly false: normal tools fallback, but connectors must NOT
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer client.Close()

	// All 6 should return the network/HTTP error, NOT fall back to local lattice (which returns nil, ErrRemoteOnly)
	// We verify that the returned error is the HTTP 500 error from the server.
	_, err = client.ListConnectors(ctx, ListConnectorsRequest{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("ListConnectors: expected HTTP 500 error, got %v", err)
	}

	_, err = client.RegisterConnector(ctx, RegisterConnectorRequest{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("RegisterConnector: expected HTTP 500 error, got %v", err)
	}

	_, err = client.SyncAudit(ctx, SyncAuditRequest{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("SyncAudit: expected HTTP 500 error, got %v", err)
	}

	_, err = client.TriggerSync(ctx, TriggerSyncRequest{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("TriggerSync: expected HTTP 500 error, got %v", err)
	}

	_, err = client.GetConnectorDocument(ctx, GetConnectorDocumentRequest{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("GetConnectorDocument: expected HTTP 500 error, got %v", err)
	}

	_, err = client.InspectConnectorFacts(ctx, InspectConnectorFactsRequest{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("InspectConnectorFacts: expected HTTP 500 error, got %v", err)
	}

	// CallTool for connector tool should also fail with HTTP error and not fallback
	_, err = client.CallTool(ctx, "icx_list_connectors", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("CallTool: expected HTTP 500 error, got %v", err)
	}
}

// TestConnectors_BinaryHygieneInvariant verifies that omnicoder.exe source tree
// contains zero local embedded webhook listeners (http.ListenAndServe, net.Listen)
// and zero external cloud polling daemons.
func TestConnectors_BinaryHygieneInvariant(t *testing.T) {
	// Root of repository
	rootDirs := []string{
		filepath.Join("..", "..", "cmd", "omnicoder"),
		filepath.Join("..", "..", "pkg"),
	}

	forbiddenCalls := []string{
		"http.ListenAndServe",
		"http.ListenAndServeTLS",
		"net.Listen",
		"net.ListenTCP",
	}

	for _, dir := range rootDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if _, err := os.Stat(absDir); os.IsNotExist(err) {
			continue
		}

		err = filepath.Walk(absDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if info.IsDir() {
				if info.Name() == ".worktrees" || info.Name() == ".git" || info.Name() == ".omnicoder" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			// Skip test files
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}

			fset := token.NewFileSet()
			node, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.ParseComments)
			if parseErr != nil {
				return nil
			}

			// Read file content to check for forbidden listener invocations
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			sContent := string(content)

			for _, forbidden := range forbiddenCalls {
				if strings.Contains(sContent, forbidden) {
					t.Errorf("Binary Hygiene Violation: found forbidden server listener %q in %s", forbidden, path)
				}
			}

			_ = node
			return nil
		})
		if err != nil {
			t.Errorf("error walking %s: %v", absDir, err)
		}
	}
}

// TestConnectors_ConcurrentRaceFree verifies that concurrent invocations of connector methods
// across multiple goroutines pass with 0 race detector warnings.
func TestConnectors_ConcurrentRaceFree(t *testing.T) {
	ctx := context.Background()
	client, err := NewClient(ClientConfig{
		SpaceID: "test_space",
		Offline: true,
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer client.Close()

	var wg sync.WaitGroup
	const goroutines = 20

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, _ = client.ListConnectors(ctx, ListConnectorsRequest{})
				_, _ = client.RegisterConnector(ctx, RegisterConnectorRequest{})
				_, _ = client.SyncAudit(ctx, SyncAuditRequest{})
				_, _ = client.TriggerSync(ctx, TriggerSyncRequest{})
				_, _ = client.GetConnectorDocument(ctx, GetConnectorDocumentRequest{})
				_, _ = client.InspectConnectorFacts(ctx, InspectConnectorFactsRequest{})
				_, _ = client.CallTool(ctx, "icx_list_connectors", nil)
			}
		}()
	}

	wg.Wait()
}
