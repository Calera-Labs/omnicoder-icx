// SPDX-License-Identifier: Apache-2.0
package ui_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/ui"
)

// ============================================================================
// Section 1: Empirical Adversarial Path Traversal Penetration Suite
// ============================================================================

func TestPathTraversal_ComprehensiveMatrix(t *testing.T) {
	parentDir := t.TempDir()
	workspaceDir := filepath.Join(parentDir, "workspace")
	siblingDir := filepath.Join(parentDir, "sibling")

	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		t.Fatalf("failed to create workspace dir: %v", err)
	}
	if err := os.MkdirAll(siblingDir, 0755); err != nil {
		t.Fatalf("failed to create sibling dir: %v", err)
	}

	canaryPath := filepath.Join(siblingDir, "canary.txt")
	canarySecret := []byte("IMMUTABLE_CANARY_SECRET_12345")
	if err := os.WriteFile(canaryPath, canarySecret, 0644); err != nil {
		t.Fatalf("failed to create canary file: %v", err)
	}

	session := ui.NewSession(nil, nil, workspaceDir)

	vectors := []struct {
		category string
		vector   string
	}{
		// 1. Upward traversals
		{"upward", "../../sibling/canary.txt"},
		{"upward", `..\..\sibling\canary.txt`},
		{"upward", "....//....//sibling/canary.txt"},
		{"upward", `..../\..../\sibling\canary.txt`},
		{"upward", "sub/dir/../../../../canary.txt"},
		{"upward", "./../../sibling/canary.txt"},
		{"upward", "sub/../../../outside.txt"},
		{"upward", "sub/./../../outside.txt"},
		{"upward", ".."},
		{"upward", "../"},
		{"upward", `..\`},
		{"upward", "dir/.."},

		// 2. Windows drive letters & raw roots
		{"drive_letter", `C:\Windows\win.ini`},
		{"drive_letter", `C:/Windows/win.ini`},
		{"drive_letter", `D:\secret.txt`},
		{"drive_letter", `D:/secret.txt`},
		{"drive_letter", `Z:\nonexistent_drive\target.txt`},
		{"drive_letter", `c:relative_drive_breakout.txt`},
		{"drive_letter", `\\?\C:\Windows\win.ini`},
		{"drive_letter", `\\.\COM1`},

		// 3. UNC network paths
		{"unc", `\\attacker\share\payload.exe`},
		{"unc", `//attacker/share/payload.sh`},
		{"unc", `\\127.0.0.1\c$\canary.txt`},
		{"unc", `//127.0.0.1/c$/canary.txt`},
		{"unc", `\\localhost\admin$\payload.bat`},

		// 4. Alternate Data Streams
		{"ads", "file.txt:stream"},
		{"ads", "file.txt:stream:$DATA"},
		{"ads", "nested/file.txt:stream"},
		{"ads", "clean.txt:$INDEX_ALLOCATION"},
		{"ads", "clean.txt::$DATA"},

		// 5. Poison null bytes
		{"null_byte", "file.txt\x00.exe"},
		{"null_byte", "\x00file.txt"},
		{"null_byte", "dir/\x00/file.txt"},
		{"null_byte", "payload.bin\x00"},

		// 6. Windows reserved device names
		{"reserved", "CON"},
		{"reserved", "con"},
		{"reserved", "CON.txt"},
		{"reserved", "con.json"},
		{"reserved", "PRN"},
		{"reserved", "prn"},
		{"reserved", "PRN.txt"},
		{"reserved", "AUX"},
		{"reserved", "aux"},
		{"reserved", "AUX.log"},
		{"reserved", "NUL"},
		{"reserved", "nul"},
		{"reserved", "NUL.dat"},
		{"reserved", "COM1"},
		{"reserved", "com1"},
		{"reserved", "COM1.txt"},
		{"reserved", "COM2"},
		{"reserved", "COM3"},
		{"reserved", "COM4"},
		{"reserved", "COM5"},
		{"reserved", "COM6"},
		{"reserved", "COM7"},
		{"reserved", "COM8"},
		{"reserved", "COM9"},
		{"reserved", "LPT1"},
		{"reserved", "lpt1"},
		{"reserved", "LPT1.txt"},
		{"reserved", "LPT2"},
		{"reserved", "LPT3"},
		{"reserved", "LPT4"},
		{"reserved", "LPT5"},
		{"reserved", "LPT6"},
		{"reserved", "LPT7"},
		{"reserved", "LPT8"},
		{"reserved", "LPT9"},
		{"reserved", "sub/dir/con.txt"},
		{"reserved", "nested/aux.log"},
		{"reserved", "nested/NUL"},

		// 7. Encoded traversals
		{"encoded", "%2e%2e%2f"},
		{"encoded", "%2e%2e/"},
		{"encoded", "..%2f"},
		{"encoded", "%252e%252e%252f"},
		{"encoded", "%2e%2e%5c"},
		{"encoded", "..%5c"},
		{"encoded", "%2e%2e%255c"},
		{"encoded", "%2e%2e%2fcanary.txt"},
		{"encoded", "%252e%252e%252fcanary.txt"},
		{"encoded", "sub%2f%2e%2e%2f%2e%2e%2fcanary.txt"},
		{"encoded", "file%00.exe"},

		// 8. Directory roots & Directories
		{"root", "."},
		{"root", "/"},
		{"root", `\`},
		{"root", "//"},
		{"root", `\\`},
		{"root", "./"},
		{"root", `.\`},

		// 9. Empty & whitespace
		{"empty", ""},
		{"empty", "   "},
		{"empty", "\t\r\n"},
	}

	payloadData := []byte("CORRUPTED_BY_ADVERSARIAL_ATTACK_VECTOR")

	for idx, tc := range vectors {
		name := fmt.Sprintf("[%d]_%s_%s", idx, tc.category, strings.ReplaceAll(tc.vector, string(filepath.Separator), "_"))
		t.Run(name, func(t *testing.T) {
			targetRel, seal, err := session.WriteFile(tc.vector, payloadData)
			if err == nil {
				t.Fatalf("[VULNERABILITY] Vector %q unexpectedly SUCCEEDED! Target: %s, Seal: %s", tc.vector, targetRel, seal)
			}

			// Verify canary integrity
			currentCanary, readErr := os.ReadFile(canaryPath)
			if readErr != nil {
				t.Fatalf("canary file unreadable: %v", readErr)
			}
			if !bytes.Equal(currentCanary, canarySecret) {
				t.Fatalf("[CRITICAL ESCAPE] Vector %q corrupted canary file outside workspace!", tc.vector)
			}
		})
	}

	// Verify existing directory writing prevention
	t.Run("existing_directory_rejection", func(t *testing.T) {
		subDir := filepath.Join(workspaceDir, "existing_sub")
		if err := os.MkdirAll(subDir, 0755); err != nil {
			t.Fatalf("failed to create subDir: %v", err)
		}
		_, _, err := session.WriteFile("existing_sub", payloadData)
		if err == nil {
			t.Fatal("expected writing to existing directory to fail, but succeeded")
		}
	})

	// Verify no files escaped to parentDir or siblingDir
	siblingEntries, err := os.ReadDir(siblingDir)
	if err != nil {
		t.Fatalf("failed to read sibling dir: %v", err)
	}
	if len(siblingEntries) != 1 || siblingEntries[0].Name() != "canary.txt" {
		t.Fatalf("[LEAK] unexpected files found in siblingDir: %v", siblingEntries)
	}
}

// ============================================================================
// Section 2: Adversarial Slash Command /save Traversal Defense
// ============================================================================

func TestSlashCommand_Save_AdversarialDefense(t *testing.T) {
	parentDir := t.TempDir()
	workspaceDir := filepath.Join(parentDir, "workspace")
	siblingDir := filepath.Join(parentDir, "sibling")

	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		t.Fatalf("failed to create workspace dir: %v", err)
	}
	if err := os.MkdirAll(siblingDir, 0755); err != nil {
		t.Fatalf("failed to create sibling dir: %v", err)
	}

	canaryPath := filepath.Join(siblingDir, "canary_slash.txt")
	canarySecret := []byte("ORIGINAL_SLASH_CANARY")
	if err := os.WriteFile(canaryPath, canarySecret, 0644); err != nil {
		t.Fatalf("failed to create canary file: %v", err)
	}

	session := ui.NewSession(nil, nil, workspaceDir)
	session.AddMessage(llm.ChatMessage{
		Role:    "assistant",
		Content: "MALICIOUS_SLASH_PAYLOAD_CONTENT",
	})

	slashAttackVectors := []string{
		"../../sibling/canary_slash.txt",
		`..\..\sibling\canary_slash.txt`,
		"....//....//sibling/canary_slash.txt",
		`C:\Windows\win.ini`,
		`D:\secret.txt`,
		`\\attacker\share\payload.exe`,
		"clean.txt:hidden_ads",
		"file.txt\x00.exe",
		"CON",
		"PRN",
		"AUX",
		"NUL",
		"COM1",
		"LPT1",
		"%2e%2e%2fcanary_slash.txt",
		".",
		"..",
		"/",
		`\`,
	}

	for _, vec := range slashAttackVectors {
		t.Run("slash_"+vec, func(t *testing.T) {
			inputCmds := fmt.Sprintf("/save %s\n/write %s\n/exit\n", vec, vec)
			input := strings.NewReader(inputCmds)
			var out bytes.Buffer

			err := ui.RunConversationalUI(context.Background(), session, input, &out)
			if err != nil {
				t.Fatalf("RunConversationalUI failed with error: %v", err)
			}

			outStr := out.String()
			// If it's a directory root, it might fail with "target cannot be a directory or root" or "[Write Error]"
			if !strings.Contains(outStr, "[Write Error]") && !strings.Contains(outStr, "target cannot be a directory or root") {
				t.Errorf("expected error badge in output for vector %q, got output:\n%s", vec, outStr)
			}

			// Verify canary untouched
			cur, err := os.ReadFile(canaryPath)
			if err != nil {
				t.Fatalf("canary unreadable: %v", err)
			}
			if !bytes.Equal(cur, canarySecret) {
				t.Fatalf("[CRITICAL] Canary overwritten via /save vector: %q", vec)
			}
		})
	}
}

// ============================================================================
// Section 3: 75-Goroutine Concurrency & Race Detector Stress Test
// ============================================================================

func TestSession_WriteFile_75GoroutineRaceStress(t *testing.T) {
	tempWs := t.TempDir()
	session := ui.NewSession(nil, nil, tempWs)

	// Pre-populate some assistant messages
	session.AddMessage(llm.ChatMessage{
		Role:    "user",
		Content: "Hello architect",
	})
	session.AddMessage(llm.ChatMessage{
		Role:    "assistant",
		Content: "```markdown\n# Concurrency Baseline\n```",
	})

	const numGoroutines = 75
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	sharedContestedFile := "shared_contended.txt"

	// Tracking slice for unique worker files
	type workerResult struct {
		path string
		data []byte
		seal string
	}
	results := make([]workerResult, numGoroutines)
	var errCount int64

	for i := 0; i < numGoroutines; i++ {
		workerID := i
		go func() {
			defer wg.Done()

			// Phase 1: Heavy concurrent contention on a single shared file
			sharedData := []byte(fmt.Sprintf("Worker %03d writing to shared at %d", workerID, time.Now().UnixNano()))
			_, _, _ = session.WriteFile(sharedContestedFile, sharedData)

			// Phase 2: Concurrent write to deep isolated directory hierarchy
			isoPath := fmt.Sprintf("deep/nested/pool_%02d/worker_%03d/deliverable.md", workerID%5, workerID)
			isoData := []byte(fmt.Sprintf("# Deliverable Report from Worker %d\nData: %s", workerID, strings.Repeat("A", 128)))

			writtenPath, seal, err := session.WriteFile(isoPath, isoData)
			if err != nil {
				t.Errorf("worker %d WriteFile failed: %v", workerID, err)
				return
			}

			// Verify SHA-256 seal matches computed
			expectedSha := sha256.Sum256(isoData)
			expectedHex := hex.EncodeToString(expectedSha[:])
			if seal != expectedHex {
				t.Errorf("worker %d seal mismatch: expected %s, got %s", workerID, expectedHex, seal)
			}

			results[workerID] = workerResult{
				path: writtenPath,
				data: isoData,
				seal: seal,
			}

			// Phase 3: Concurrent reads of LastAssistantMessage
			lastMsg := session.LastAssistantMessage()
			if lastMsg == "" {
				// Might be empty if reset occurred concurrently, which is valid
			}

			// Phase 4: Concurrent session mutations
			if workerID%10 == 0 {
				session.AddMessage(llm.ChatMessage{
					Role:    "assistant",
					Content: fmt.Sprintf("Assistant turn from worker %d", workerID),
				})
			} else if workerID%15 == 0 {
				session.FeedExecutionSummary(fmt.Sprintf("Attestation summary %d", workerID))
			}

			// Phase 5: Concurrent status and configuration queries
			_ = session.Status()
			_ = session.Model()
			_ = session.WorkspaceDir()
			_ = session.SystemPrompt()
		}()
	}

	wg.Wait()

	if errCount > 0 {
		t.Fatalf("%d workers encountered errors during concurrent execution", errCount)
	}

	// Post-condition 1: Verify all 75 isolated files exist and contain exact byte contents
	for i := 0; i < numGoroutines; i++ {
		res := results[i]
		if res.path == "" {
			continue
		}
		fullPath := filepath.Join(tempWs, filepath.FromSlash(res.path))
		onDisk, err := os.ReadFile(fullPath)
		if err != nil {
			t.Fatalf("worker %d file missing on disk: %v", i, err)
		}
		if !bytes.Equal(onDisk, res.data) {
			t.Fatalf("worker %d file data corrupted on disk!", i)
		}
		diskSha := canonical.SHA256Bytes(onDisk)
		if diskSha != res.seal {
			t.Fatalf("worker %d on-disk SHA %s != returned seal %s", i, diskSha, res.seal)
		}
	}

	// Post-condition 2: Verify shared contested file exists and is valid
	sharedFullPath := filepath.Join(tempWs, sharedContestedFile)
	sharedDiskData, err := os.ReadFile(sharedFullPath)
	if err != nil {
		t.Fatalf("shared contested file missing: %v", err)
	}
	if len(sharedDiskData) == 0 {
		t.Fatal("shared contested file is empty")
	}
	sharedSeal := canonical.SHA256Bytes(sharedDiskData)
	if len(sharedSeal) != 64 {
		t.Fatalf("invalid SHA-256 seal for shared file: %s", sharedSeal)
	}
}

// ============================================================================
// Section 4: Legitimate In-Workspace Deep Writing & Edge Cases
// ============================================================================

func TestSession_WriteFile_LegitimateDeepWrites(t *testing.T) {
	tempWs := t.TempDir()
	session := ui.NewSession(nil, nil, tempWs)

	validCases := []struct {
		name     string
		relPath  string
		content  string
		expected string
	}{
		{"root level file", "README.md", "# Hello", "README.md"},
		{"single subfolder", "docs/architecture.md", "## Arch", "docs/architecture.md"},
		{"deep nested folder", "src/pkg/internal/utils/helper.go", "package utils", "src/pkg/internal/utils/helper.go"},
		{"dots in filename", "archive.v1.0.tar.gz", "binary", "archive.v1.0.tar.gz"},
		{"spaces in filename", "my reports/Q3 summary.txt", "Q3 Report", "my reports/Q3 summary.txt"},
		{"empty content write", "empty.txt", "", "empty.txt"},
		{"relative current dir prefix", "./local/file.txt", "content", "local/file.txt"},
	}

	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			written, seal, err := session.WriteFile(tc.relPath, []byte(tc.content))
			if err != nil {
				t.Fatalf("unexpected failure for valid path %q: %v", tc.relPath, err)
			}
			if written != tc.expected {
				t.Errorf("expected writtenPath %q, got %q", tc.expected, written)
			}

			// Verify file on disk
			fullPath := filepath.Join(tempWs, filepath.FromSlash(written))
			data, err := os.ReadFile(fullPath)
			if err != nil {
				t.Fatalf("failed reading file from disk: %v", err)
			}
			if string(data) != tc.content {
				t.Errorf("content mismatch: expected %q, got %q", tc.content, string(data))
			}

			expectedSeal := canonical.SHA256Bytes([]byte(tc.content))
			if seal != expectedSeal {
				t.Errorf("seal mismatch: expected %s, got %s", expectedSeal, seal)
			}
		})
	}
}
