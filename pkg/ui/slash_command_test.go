package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// TestCLI_HelpOutput verifies that /help cleanly documents /save <path>.
func TestCLI_HelpOutput(t *testing.T) {
	var buf bytes.Buffer
	PrintHelp(&buf)
	output := buf.String()

	if !strings.Contains(output, "/save <path>") {
		t.Fatalf("PrintHelp output does not contain '/save <path>'. Got:\n%s", output)
	}
	if !strings.Contains(output, "Save the last assistant turn to disk") {
		t.Fatalf("PrintHelp output missing description for /save. Got:\n%s", output)
	}
}

// TestCLI_Save_EmptyAndSystemOnlyHistory verifies that /save gracefully handles
// empty history, system-only history, and execution-attestation-only history.
func TestCLI_Save_EmptyAndSystemOnlyHistory(t *testing.T) {
	tmpDir := t.TempDir()

	// Case 1: Completely empty history
	{
		session := NewSession(nil, nil, tmpDir)
		var in bytes.Buffer
		in.WriteString("/save empty_test.txt\n/exit\n")
		var out bytes.Buffer

		err := RunConversationalUI(context.Background(), session, &in, &out)
		if err != nil {
			t.Fatalf("RunConversationalUI failed on empty history: %v", err)
		}
		res := out.String()
		if !strings.Contains(res, "No previous assistant response in history to save") {
			t.Fatalf("Expected notice on empty history, got:\n%s", res)
		}
		if _, err := os.Stat(filepath.Join(tmpDir, "empty_test.txt")); !os.IsNotExist(err) {
			t.Fatalf("File should not have been created on empty history")
		}
	}

	// Case 2: History with only system message
	{
		session := NewSessionWithConfig(SessionConfig{
			WorkspaceDir: tmpDir,
			SystemPrompt: "You are an architect.",
		}, nil, nil)
		var in bytes.Buffer
		in.WriteString("/save system_only.txt\n/exit\n")
		var out bytes.Buffer

		err := RunConversationalUI(context.Background(), session, &in, &out)
		if err != nil {
			t.Fatalf("RunConversationalUI failed on system-only history: %v", err)
		}
		res := out.String()
		if !strings.Contains(res, "No previous assistant response in history to save") {
			t.Fatalf("Expected notice on system-only history, got:\n%s", res)
		}
		if _, err := os.Stat(filepath.Join(tmpDir, "system_only.txt")); !os.IsNotExist(err) {
			t.Fatalf("File should not have been created on system-only history")
		}
	}

	// Case 3: History with system message and Execution Attestation Report
	{
		session := NewSession(nil, nil, tmpDir)
		session.FeedExecutionSummary("Build succeeded. Touched 3 files.")
		var in bytes.Buffer
		in.WriteString("/save exec_only.txt\n/exit\n")
		var out bytes.Buffer

		err := RunConversationalUI(context.Background(), session, &in, &out)
		if err != nil {
			t.Fatalf("RunConversationalUI failed on exec-summary-only history: %v", err)
		}
		res := out.String()
		if !strings.Contains(res, "No previous assistant response in history to save") {
			t.Fatalf("Expected notice on execution-summary-only history, got:\n%s", res)
		}
		if _, err := os.Stat(filepath.Join(tmpDir, "exec_only.txt")); !os.IsNotExist(err) {
			t.Fatalf("File should not have been created on execution-summary-only history")
		}
	}
}

// TestCLI_Save_ComplexPathsAndQuoting verifies /save with complex paths,
// surrounding single/double quotes, and leading/trailing extra spaces.
func TestCLI_Save_ComplexPathsAndQuoting(t *testing.T) {
	tmpDir := t.TempDir()
	session := NewSession(nil, nil, tmpDir)

	// Inject assistant response into history
	assistantContent := "```go\npackage calc\n\nfunc Add(a, b int) int { return a + b }\n```"
	session.AddMessage(llm.ChatMessage{
		Role:    "assistant",
		Content: assistantContent,
	})

	testCases := []struct {
		name         string
		command      string
		expectedPath string
	}{
		{
			name:         "Double quoted path with spaces",
			command:      "/save \"docs/sub folder/my notes.txt\"",
			expectedPath: filepath.Join(tmpDir, "docs", "sub folder", "my notes.txt"),
		},
		{
			name:         "Single quoted path with spaces",
			command:      "/save 'reports/q3 audit/results.md'",
			expectedPath: filepath.Join(tmpDir, "reports", "q3 audit", "results.md"),
		},
		{
			name:         "Extra spaces around unquoted path",
			command:      "/save    nested/deep/file.go   ",
			expectedPath: filepath.Join(tmpDir, "nested", "deep", "file.go"),
		},
		{
			name:         "Extra spaces and quotes with /write alias",
			command:      "/write   \"config/settings.json\"   ",
			expectedPath: filepath.Join(tmpDir, "config", "settings.json"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var in bytes.Buffer
			in.WriteString(tc.command + "\n/exit\n")
			var out bytes.Buffer

			err := RunConversationalUI(context.Background(), session, &in, &out)
			if err != nil {
				t.Fatalf("RunConversationalUI failed: %v", err)
			}

			// Verify file was written to disk
			data, err := os.ReadFile(tc.expectedPath)
			if err != nil {
				t.Fatalf("File was not written to disk at %s: %v", tc.expectedPath, err)
			}

			expectedExtracted := "package calc\n\nfunc Add(a, b int) int { return a + b }\n"
			if string(data) != expectedExtracted {
				t.Fatalf("Extracted content on disk mismatch.\nGot:\n%q\nExpected:\n%q", string(data), expectedExtracted)
			}

			// Verify SHA-256 seal in terminal output
			expectedHash := sha256.Sum256(data)
			expectedSeal := hex.EncodeToString(expectedHash[:])
			if !strings.Contains(out.String(), expectedSeal) {
				t.Fatalf("Output does not contain matching SHA-256 seal %s.\nOutput:\n%s", expectedSeal, out.String())
			}
			if !strings.Contains(out.String(), "[File Created]") {
				t.Fatalf("Output missing [File Created] badge.\nOutput:\n%s", out.String())
			}
		})
	}
}

// TestCLI_Save_EndToEndFileWritingAndShaSeal verifies end-to-end file writing,
// disk content fidelity, and SHA-256 canonical seal matching.
func TestCLI_Save_EndToEndFileWritingAndShaSeal(t *testing.T) {
	tmpDir := t.TempDir()
	session := NewSession(nil, nil, tmpDir)

	// Markdown deliverable with 4-backtick fence
	assistantPayload := "````markdown\n# Project Deliverable\n\nThis is verified content.\n````"
	session.AddMessage(llm.ChatMessage{
		Role:    "assistant",
		Content: assistantPayload,
	})

	var in bytes.Buffer
	in.WriteString("/save deliverables/final_summary.md\n/exit\n")
	var out bytes.Buffer

	err := RunConversationalUI(context.Background(), session, &in, &out)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	targetFile := filepath.Join(tmpDir, "deliverables", "final_summary.md")
	data, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("Failed to read created file: %v", err)
	}

	expectedContent := "# Project Deliverable\n\nThis is verified content.\n"
	if string(data) != expectedContent {
		t.Fatalf("Content mismatch on disk.\nGot:\n%q\nExpected:\n%q", string(data), expectedContent)
	}

	canonicalSeal := canonical.SHA256Bytes(data)
	if !strings.Contains(out.String(), canonicalSeal) {
		t.Fatalf("Terminal output does not contain canonical SHA-256 seal %s", canonicalSeal)
	}

	// Verify attestation was fed into session history
	lastAssistant := session.LastAssistantMessage()
	if lastAssistant != assistantPayload {
		t.Fatalf("LastAssistantMessage corrupted by FeedExecutionSummary")
	}
}
