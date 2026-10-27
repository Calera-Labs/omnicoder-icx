// SPDX-License-Identifier: Apache-2.0
// Package mcp_test implements aggressive adversarial penetration tests and stress harnesses
// for the OmniCoder-ICX MCP filesystem security and concurrency subsystem.

package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
)

// ----------------------------------------------------------------------------
// 1. Aggressive Path Traversal Fuzzing Matrix
// ----------------------------------------------------------------------------

func TestAdversarial_ResolveSecurePath_ComprehensiveMatrix(t *testing.T) {
	workDir := t.TempDir()
	if realDir, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = realDir
	}

	payloads := []struct {
		category string
		payload  string
	}{
		// Deep traversal prefixes
		{"deep_traversal", "../../../../../etc/passwd"},
		{"deep_traversal", `..\..\..\Windows\win.ini`},
		{"deep_traversal", "../../../../../../../../../../../../etc/shadow"},
		{"deep_traversal", `..\..\..\..\..\..\..\..\..\..\..\Windows\System32\drivers\etc\hosts`},
		{"deep_traversal", `....//....//....//etc/passwd`},
		{"deep_traversal", `..\/..\/..\/etc/passwd`},
		{"deep_traversal", `/etc/passwd`},
		{"deep_traversal", `\Windows\win.ini`},

		// Cross-volume drive letters on Windows
		{"cross_volume", `D:\secret`},
		{"cross_volume", `D:secret`},
		{"cross_volume", `E:\foo\bar`},
		{"cross_volume", `Z:\`},
		{"cross_volume", `C:\Windows\System32\cmd.exe`},

		// UNC paths
		{"unc", `\\attacker\share`},
		{"unc", `//attacker/share`},
		{"unc", `\\127.0.0.1\c$\secret`},
		{"unc", `//127.0.0.1/c$/secret`},
		{"unc", `\\?\C:\Windows\win.ini`},
		{"unc", `//?/C:/Windows/win.ini`},
		{"unc", `\\.\PhysicalDrive0`},

		// Poison null bytes
		{"null_byte", "\x00"},
		{"null_byte", "path/to/file\x00.png"},
		{"null_byte", "safe.txt\x00/../../etc/passwd"},
		{"null_byte", "\x00../../../etc/passwd"},
		{"null_byte", "dir/file\x00"},

		// Windows Alternate Data Streams
		{"ads", "file.txt:stream"},
		{"ads", "file.txt::$DATA"},
		{"ads", "dir/file.txt:hidden"},
		{"ads", ":stream"},
		{"ads", "file.txt:stream:$DATA"},

		// URL-encoded traversals
		{"url_encoded", "%2e%2e%2f"},
		{"url_encoded", "%2e%2e/"},
		{"url_encoded", "%2e%2e%5c"},
		{"url_encoded", "..%2f"},
		{"url_encoded", "%252e%252e%252f"},
		{"url_encoded", "%252e%252e%255c"},
		{"url_encoded", "%25252e%25252e%25252f"},
		{"url_encoded", "..%252f"},
		{"url_encoded", "%2e%2e%2fetc%2fpasswd"},
		{"url_encoded", "%252e%252e%252f%252e%252e%252fetc%2fpasswd"},

		// Windows reserved device names
		{"reserved_name", "CON"},
		{"reserved_name", "PRN"},
		{"reserved_name", "AUX"},
		{"reserved_name", "NUL"},
		{"reserved_name", "COM1"},
		{"reserved_name", "COM2"},
		{"reserved_name", "COM3"},
		{"reserved_name", "COM4"},
		{"reserved_name", "COM5"},
		{"reserved_name", "COM6"},
		{"reserved_name", "COM7"},
		{"reserved_name", "COM8"},
		{"reserved_name", "COM9"},
		{"reserved_name", "LPT1"},
		{"reserved_name", "LPT2"},
		{"reserved_name", "LPT3"},
		{"reserved_name", "LPT4"},
		{"reserved_name", "LPT5"},
		{"reserved_name", "LPT6"},
		{"reserved_name", "LPT7"},
		{"reserved_name", "LPT8"},
		{"reserved_name", "LPT9"},
		{"reserved_name", "con"},
		{"reserved_name", "aux"},
		{"reserved_name", "nul"},
		{"reserved_name", "com1"},
		{"reserved_name", "lpt1"},
		{"reserved_name", "CON.txt"},
		{"reserved_name", "PRN.dat"},
		{"reserved_name", "AUX.c"},
		{"reserved_name", "NUL.log"},
		{"reserved_name", "COM1.json"},
		{"reserved_name", "LPT1.xml"},
		{"reserved_name", "sub/CON"},
		{"reserved_name", "nested/dir/PRN.txt"},
		{"reserved_name", "dir/AUX"},
	}

	for _, tc := range payloads {
		name := fmt.Sprintf("%s/%s", tc.category, tc.payload)
		t.Run(name, func(t *testing.T) {
			resolved, err := mcp.ResolveSecurePath(workDir, tc.payload)
			if err == nil {
				t.Errorf("SECURITY FAILURE [%s]: payload '%s' was NOT rejected! Resolved to: %s", tc.category, tc.payload, resolved)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// 2. Symlink Breakout Penetration Test
// ----------------------------------------------------------------------------

func TestAdversarial_SymlinkBreakout(t *testing.T) {
	workDir := t.TempDir()
	outsideDir := t.TempDir()

	if realWork, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = realWork
	}
	if realOut, err := filepath.EvalSymlinks(outsideDir); err == nil {
		outsideDir = realOut
	}

	// Create a secret file outside workspace
	outsideSecret := filepath.Join(outsideDir, "secret_outside.txt")
	if err := os.WriteFile(outsideSecret, []byte("TOP_SECRET_DATA"), 0o644); err != nil {
		t.Fatalf("failed creating outside secret file: %v", err)
	}

	// Attempt to create a symlink inside workspace pointing to outside secret
	symlinkPath := filepath.Join(workDir, "leak_symlink.txt")
	err := os.Symlink(outsideSecret, symlinkPath)
	if err != nil {
		t.Logf("Skipping direct symlink test: symlink creation not supported/permitted on this platform (%v)", err)
		return
	}

	// 1. ResolveSecurePath must detect and reject symlink pointing outside
	_, err = mcp.ResolveSecurePath(workDir, "leak_symlink.txt")
	if err == nil {
		t.Fatalf("SECURITY FAILURE: Symlink breakout was NOT rejected by ResolveSecurePath!")
	}
	if !strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "escapes") {
		t.Errorf("expected symlink breakout error, got: %v", err)
	}

	// 2. Also test directory symlink pointing outside
	symlinkDir := filepath.Join(workDir, "outside_dir_link")
	err = os.Symlink(outsideDir, symlinkDir)
	if err == nil {
		_, err = mcp.ResolveSecurePath(workDir, "outside_dir_link/secret_outside.txt")
		if err == nil {
			t.Fatalf("SECURITY FAILURE: Directory symlink breakout was NOT rejected!")
		}
	}
}

// ----------------------------------------------------------------------------
// 3. Tool Dispatch Verification (read_file, write_file, list_directory)
// ----------------------------------------------------------------------------

func TestAdversarial_Tools_StrictlyRejectTraversals(t *testing.T) {
	workDir := t.TempDir()
	srv := mcp.NewServer(workDir)
	ctx := context.Background()

	maliciousPaths := []string{
		"../../../../../etc/passwd",
		`..\..\..\Windows\win.ini`,
		`D:\secret.txt`,
		`\\attacker\share\loot`,
		"safe.txt\x00evil",
		"file.txt:hidden",
		"%2e%2e%2fetc%2fpasswd",
		"%252e%252e%252fetc%252fpasswd",
		"CON",
		"PRN.txt",
	}

	for _, evilPath := range maliciousPaths {
		// Test read_file
		t.Run("read_file/"+evilPath, func(t *testing.T) {
			res, err := srv.Dispatch(ctx, "read_file", map[string]any{
				"path": evilPath,
			})
			if err == nil && res != nil {
				t.Errorf("read_file failed to reject traversal payload '%s'", evilPath)
			}
		})

		// Test write_file
		t.Run("write_file/"+evilPath, func(t *testing.T) {
			res, err := srv.Dispatch(ctx, "write_file", map[string]any{
				"path":      evilPath,
				"content":   "malicious content",
				"overwrite": true,
			})
			if err == nil && res != nil {
				t.Errorf("write_file failed to reject traversal payload '%s'", evilPath)
			}
		})

		// Test list_directory
		t.Run("list_directory/"+evilPath, func(t *testing.T) {
			res, err := srv.Dispatch(ctx, "list_directory", map[string]any{
				"path": evilPath,
			})
			if err == nil && res != nil {
				t.Errorf("list_directory failed to reject traversal payload '%s'", evilPath)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// 4. Atomic Write Stress Harness: Rapid Concurrent Overwrites
// ----------------------------------------------------------------------------

func TestAdversarial_AtomicWrite_RapidConcurrentOverwrites(t *testing.T) {
	workDir := t.TempDir()
	ctx := context.Background()

	targetFile := "shared_critical.dat"

	// Initial seed
	initialContent := "INITIAL_SEED_DATA"
	_, err := mcp.HandleWriteFile(ctx, workDir, map[string]any{
		"path":      targetFile,
		"content":   initialContent,
		"overwrite": true,
	})
	if err != nil {
		t.Fatalf("initial write failed: %v", err)
	}

	const numWriters = 20
	const writesPerWorker = 30
	const numReaders = 10
	const readsPerWorker = 30

	var wg sync.WaitGroup
	errCh := make(chan error, (numWriters*writesPerWorker)+(numReaders*readsPerWorker))

	// Known valid content generators
	generateContent := func(workerID, seq int) (string, string) {
		payload := fmt.Sprintf("WORKER_%03d_SEQ_%05d_PAYLOAD_%s", workerID, seq, strings.Repeat("X", 1024))
		sum := sha256.Sum256([]byte(payload))
		hash := hex.EncodeToString(sum[:])
		return payload, hash
	}

	validHashes := sync.Map{}
	sum0 := sha256.Sum256([]byte(initialContent))
	validHashes.Store(hex.EncodeToString(sum0[:]), true)

	// Pre-populate valid hashes so readers can verify
	for w := 0; w < numWriters; w++ {
		for s := 0; s < writesPerWorker; s++ {
			_, h := generateContent(w, s)
			validHashes.Store(h, true)
		}
	}

	// Spawn Writers
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for s := 0; s < writesPerWorker; s++ {
				content, expectedHash := generateContent(workerID, s)
				res, err := mcp.HandleWriteFile(ctx, workDir, map[string]any{
					"path":      targetFile,
					"content":   content,
					"overwrite": true,
				})
				if err != nil {
					// On Windows, simultaneous os.Rename to the same destination can occasionally
					// encounter ERROR_ACCESS_DENIED if another process just opened/replaced it.
					// HandleWriteFile does os.Remove before os.Rename on Windows.
					// If an error occurs, record it.
					errCh <- fmt.Errorf("writer %d write %d failed: %w", workerID, s, err)
					return
				}
				if res.SHA256 != expectedHash {
					errCh <- fmt.Errorf("writer %d write %d hash mismatch: expected %s, got %s", workerID, s, expectedHash, res.SHA256)
					return
				}
				time.Sleep(time.Duration(rand.Intn(500)) * time.Microsecond)
			}
		}(w)
	}

	// Spawn Concurrent Readers to detect tears or partial writes
	for r := 0; r < numReaders; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for s := 0; s < readsPerWorker; s++ {
				readRes, err := mcp.HandleReadFile(ctx, workDir, map[string]any{
					"path": targetFile,
				})
				if err != nil {
					errCh <- fmt.Errorf("reader %d read %d failed: %w", readerID, s, err)
					return
				}

				// Verify read content is not empty and matches a known valid write
				if len(readRes.Content) == 0 {
					errCh <- fmt.Errorf("reader %d read %d observed EMPTY file content (torn write!)", readerID, s)
					return
				}

				actualHash := canonical.SHA256Bytes([]byte(readRes.Content))
				if _, ok := validHashes.Load(actualHash); !ok {
					errCh <- fmt.Errorf("reader %d read %d observed CORRUPT / TORN content hash: %s", readerID, s, actualHash)
					return
				}
				time.Sleep(time.Duration(rand.Intn(500)) * time.Microsecond)
			}
		}(r)
	}

	wg.Wait()
	close(errCh)

	var errList []error
	var toctouCount, accessDeniedCount, otherCount int
	for err := range errCh {
		errList = append(errList, err)
		errStr := err.Error()
		if strings.Contains(errStr, "symlink breakout detected") {
			toctouCount++
		} else if strings.Contains(errStr, "Access is denied") {
			accessDeniedCount++
		} else {
			otherCount++
			t.Logf("OTHER CONCURRENCY ERROR: %v", err)
		}
	}

	t.Logf("CONCURRENCY STRESS AUDIT: Total ops: %d, Errors: %d (TOCTOU symlink false-positives: %d, Windows Access Denied: %d, Other: %d)",
		(numWriters*writesPerWorker)+(numReaders*readsPerWorker), len(errList), toctouCount, accessDeniedCount, otherCount)

	if len(errList) > 0 {
		t.Fatalf("encountered %d concurrency errors during atomic overwrite stress: TOCTOU=%d, AccessDenied=%d, Other=%d (First error: %v)",
			len(errList), toctouCount, accessDeniedCount, otherCount, errList[0])
	}

	// Final verification of file on disk
	finalDisk, err := os.ReadFile(filepath.Join(workDir, targetFile))
	if err != nil {
		t.Fatalf("failed reading final disk state: %v", err)
	}
	finalHash := canonical.SHA256Bytes(finalDisk)
	if _, ok := validHashes.Load(finalHash); !ok {
		t.Fatalf("final disk state has invalid hash: %s", finalHash)
	}
}

// ----------------------------------------------------------------------------
// 5. Windows Drive Letter Isolation & Relative Drive Path Attack
// ----------------------------------------------------------------------------

func TestAdversarial_WindowsDriveBreakouts(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific test")
	}

	workDir := t.TempDir()

	testVectors := []string{
		`D:\secret.txt`,
		`D:secret.txt`,
		`d:\test`,
		`d:test`,
		`X:\windows\system32`,
		`\\?\Volume{b75e2c83-0000-0000-0000-602f00000000}\secret`,
	}

	for _, vec := range testVectors {
		t.Run(vec, func(t *testing.T) {
			_, err := mcp.ResolveSecurePath(workDir, vec)
			if err == nil {
				t.Errorf("SECURITY VULNERABILITY: Windows drive breakout '%s' was NOT rejected!", vec)
			}
		})
	}
}
