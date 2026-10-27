// SPDX-License-Identifier: Apache-2.0
package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

type MockTurnData struct {
	TurnNumber int      `json:"turn_number"`
	Task       string   `json:"task"`
	Files      []string `json:"files"`
	Success    bool     `json:"success"`
}

func TestVault_Lifecycle(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vault_lifecycle_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	v, err := OpenVault(tmpDir)
	if err != nil {
		t.Fatalf("OpenVault failed: %v", err)
	}

	expectedStoreDir := filepath.Join(tmpDir, ".omnicoder", "store")
	if v.Dir() != expectedStoreDir {
		t.Errorf("expected dir %s, got %s", expectedStoreDir, v.Dir())
	}

	if err := v.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen existing vault
	v2, err := OpenVault(tmpDir)
	if err != nil {
		t.Fatalf("Reopening vault failed: %v", err)
	}
	defer v2.Close()
}

func TestVault_TurnCheckpoints(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vault_turns_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	v, err := OpenVault(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	sessionID := "sess-alpha-001"

	turn1 := MockTurnData{TurnNumber: 1, Task: "Scaffold project", Files: []string{"main.go"}, Success: true}
	turn2 := MockTurnData{TurnNumber: 2, Task: "Implement core logic", Files: []string{"main.go", "core.go"}, Success: true}
	turn3 := MockTurnData{TurnNumber: 3, Task: "Run tests", Files: []string{"core_test.go"}, Success: true}

	if err := SaveTurnState(v, sessionID, 1, turn1); err != nil {
		t.Fatalf("SaveTurnState 1 failed: %v", err)
	}
	if err := SaveTurnState(v, sessionID, 2, turn2); err != nil {
		t.Fatalf("SaveTurnState 2 failed: %v", err)
	}
	if err := SaveTurnState(v, sessionID, 3, turn3); err != nil {
		t.Fatalf("SaveTurnState 3 failed: %v", err)
	}

	// Fetch single turn
	var fetchedTurn2 MockTurnData
	if err := GetTurnState(v, sessionID, 2, &fetchedTurn2); err != nil {
		t.Fatalf("GetTurnState 2 failed: %v", err)
	}
	if fetchedTurn2.TurnNumber != 2 || fetchedTurn2.Task != "Implement core logic" {
		t.Errorf("fetched turn mismatch: %+v", fetchedTurn2)
	}

	// List all turns in order
	turns, err := ListTurnStates[MockTurnData](v, sessionID)
	if err != nil {
		t.Fatalf("ListTurnStates failed: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(turns))
	}
	if turns[0].TurnNumber != 1 || turns[1].TurnNumber != 2 || turns[2].TurnNumber != 3 {
		t.Errorf("turns out of order: %+v", turns)
	}
}

func TestVault_ContentAddressedStorage(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vault_cas_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	v, err := OpenVault(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	content := []byte("package main\n\nfunc main() {}\n")
	hash, err := v.PutCAS(content)
	if err != nil {
		t.Fatalf("PutCAS failed: %v", err)
	}
	if len(hash) != 64 {
		t.Fatalf("invalid sha256 hex length: %d", len(hash))
	}

	if !v.HasCAS(hash) {
		t.Errorf("HasCAS returned false for newly inserted hash %s", hash)
	}

	retrieved, err := v.GetCAS(hash)
	if err != nil {
		t.Fatalf("GetCAS failed: %v", err)
	}
	if !bytes.Equal(retrieved, content) {
		t.Errorf("content mismatch: got %s, expected %s", string(retrieved), string(content))
	}

	if v.HasCAS("0000000000000000000000000000000000000000000000000000000000000000") {
		t.Errorf("HasCAS returned true for non-existent hash")
	}
}

func TestVault_SelfHealingCache(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vault_heal_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	v, err := OpenVault(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	errSig := "mismatched types uint16 and uint32"
	patch := "<<<<<<< SEARCH\n11 + keyLen + 4 + valLen\n=======\n11 + int(keyLen) + 4 + int(valLen)\n>>>>>>> REPLACE"

	if err := v.PutHealPattern(errSig, patch); err != nil {
		t.Fatalf("PutHealPattern failed: %v", err)
	}

	cachedPatch, found := v.GetHealPattern(errSig)
	if !found {
		t.Fatal("expected cached patch to be found")
	}
	if cachedPatch != patch {
		t.Errorf("patch mismatch: got %s, expected %s", cachedPatch, patch)
	}

	_, found = v.GetHealPattern("non_existent_error")
	if found {
		t.Errorf("expected false for non-existent error")
	}
}

func TestVault_Stats(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vault_stats_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	v, err := OpenVault(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	v.SaveTurn("sess1", 1, []byte(`{"turn":1}`))
	v.SaveTurn("sess1", 2, []byte(`{"turn":2}`))
	v.PutCAS([]byte("file1"))
	v.PutCAS([]byte("file2"))
	v.PutCAS([]byte("file3"))
	v.PutHealPattern("err1", "fix1")

	stats := v.Stats()
	if stats.TurnCount != 2 {
		t.Errorf("expected 2 turns, got %d", stats.TurnCount)
	}
	if stats.CASCount != 3 {
		t.Errorf("expected 3 CAS entries, got %d", stats.CASCount)
	}
	if stats.HealCount != 1 {
		t.Errorf("expected 1 heal entry, got %d", stats.HealCount)
	}
}
