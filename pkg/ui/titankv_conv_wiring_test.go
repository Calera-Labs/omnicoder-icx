// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/storage"
)

// TestTitanKV_ConversationalTurnPersistence verifies that completed conversational turns
// in pkg/ui/cli.go are recorded into TitanKV under keys matching "turn:conv:<timestamp>:<turnNum>".
func TestTitanKV_ConversationalTurnPersistence(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Open TitanKV Vault
	v, err := storage.OpenVault(tmpDir)
	if err != nil {
		t.Fatalf("OpenVault failed: %v", err)
	}
	defer v.Close()

	// 2. Setup mock LLM that writes a complete deliverable project
	mockClient := llm.NewMockLLMClient()
	mockClient.CompleteFunc = func(ctx context.Context, req *llm.ChatCompletionRequest) (*llm.ChatCompletionResponse, error) {
		reply := "I have implemented the complete hello world project.\n\n" +
			"[ACTION:WRITE path=\"projects/hello/go.mod\"]\n" +
			"module hello\n\ngo 1.21\n\n" +
			"[ACTION:WRITE path=\"projects/hello/main.go\"]\n" +
			"package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"Hello, World!\")\n}\n\n" +
			"[ACTION:WRITE path=\"projects/hello/main_test.go\"]\n" +
			"package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {}\n\n" +
			"[ACTION:WRITE path=\"projects/hello/README.md\"]\n" +
			"# Hello App\n\nComprehensive documentation for hello app.\n" +
			"[/ACTION:WRITE]\n\nAll deliverables verified. DONE"
		return &llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: reply,
					},
				},
			},
		}, nil
	}

	session := NewSession(mockClient, nil, tmpDir)
	session.SetVault(v)
	defer session.Close()

	var out bytes.Buffer
	ctx := context.Background()
	prompt := "Build hello world app in projects/hello"
	if err := executeConversationalTurn(ctx, session, prompt, &out); err != nil {
		t.Fatalf("executeConversationalTurn failed: %v", err)
	}

	// 3. Verify file was created on disk
	writtenFile := filepath.Join(tmpDir, "projects", "hello", "main.go")
	if _, err := os.Stat(writtenFile); os.IsNotExist(err) {
		t.Fatalf("expected written file %s to exist", writtenFile)
	}

	// 4. Scan the vault for turn:conv: keys
	it, err := v.Scan([]byte("turn:conv:"), []byte("turn:conv;\x00"))
	if err != nil {
		t.Fatalf("Vault scan failed: %v", err)
	}

	foundKeys := 0
	var lastKey string
	var lastVal []byte
	for it.Valid() {
		foundKeys++
		lastKey = string(it.Key())
		lastVal = append([]byte(nil), it.Value()...)
		it.Next()
	}

	if foundKeys == 0 {
		t.Fatalf("expected at least 1 turn:conv: key in Vault, found 0")
	}

	if !strings.HasPrefix(lastKey, "turn:conv:") {
		t.Errorf("expected key prefix 'turn:conv:', got %q", lastKey)
	}

	// 5. Verify the deserialized ConversationalTurnRecord
	var rec ConversationalTurnRecord
	if err := json.Unmarshal(lastVal, &rec); err != nil {
		t.Fatalf("failed to unmarshal ConversationalTurnRecord from %s: %v", lastKey, err)
	}

	if rec.Prompt != prompt {
		t.Errorf("expected prompt %q, got %q", prompt, rec.Prompt)
	}
	if rec.TurnNumber < 1 {
		t.Errorf("expected turn number >= 1, got %d", rec.TurnNumber)
	}
	if rec.Timestamp == "" {
		t.Errorf("expected non-empty timestamp")
	}
	if !rec.Success {
		t.Errorf("expected Success to be true")
	}

	// 6. Verify stats reflect TurnCount
	stats := v.Stats()
	if stats.TurnCount < 1 {
		t.Errorf("expected TurnCount >= 1 in vault stats, got %d", stats.TurnCount)
	}
}
