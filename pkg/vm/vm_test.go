// SPDX-License-Identifier: Apache-2.0
package vm_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

func TestHermeticLocalSandbox_FullLifecycle(t *testing.T) {
	tempBase := t.TempDir()
	client := vm.NewHermeticLocalSandbox(tempBase)
	ctx := context.Background()

	// 1. Create Sandbox
	sb, err := client.CreateSandbox(ctx, vm.SandboxConfig{
		MemoryMB:       128,
		VCPUs:          1,
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("CreateSandbox failed: %v", err)
	}
	if sb.ID == "" {
		t.Fatal("expected non-empty sandbox ID")
	}

	// 2. Write and Read File
	testContent := "package main\n\nfunc main() {}\n"
	if err := client.WriteFile(ctx, sb.ID, "main.go", testContent); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	readBack, err := client.ReadFile(ctx, sb.ID, "main.go")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if readBack != testContent {
		t.Fatalf("expected file content %q, got %q", testContent, readBack)
	}

	// 3. Take Point-in-Time Snapshot Checkpoint
	startSnap := time.Now()
	chkID, err := client.Snapshot(ctx, sb.ID, "baseline")
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	if chkID == "" {
		t.Fatal("expected non-empty checkpoint ID")
	}
	snapDuration := time.Since(startSnap)
	if snapDuration > 50*time.Millisecond {
		t.Logf("Warning: Snapshot took %v", snapDuration)
	}

	// 4. Mutate file tree (corrupt/break file and add bad file)
	_ = client.WriteFile(ctx, sb.ID, "main.go", "BROKEN CODE SYNTAX ERROR")
	_ = client.WriteFile(ctx, sb.ID, "bad.txt", "unwanted side effect")

	mutated, _ := client.ReadFile(ctx, sb.ID, "main.go")
	if !strings.Contains(mutated, "BROKEN") {
		t.Fatalf("expected broken content before rollback")
	}

	// 5. Instant CoW Rollback
	startRoll := time.Now()
	if err := client.Rollback(ctx, sb.ID, chkID); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	rollDuration := time.Since(startRoll)
	if rollDuration > 50*time.Millisecond {
		t.Logf("Warning: Rollback took %v", rollDuration)
	}

	// 6. Verify restored state
	restored, err := client.ReadFile(ctx, sb.ID, "main.go")
	if err != nil {
		t.Fatalf("ReadFile after rollback failed: %v", err)
	}
	if restored != testContent {
		t.Fatalf("expected restored content %q, got %q", testContent, restored)
	}

	// Verify unwanted file was deleted by rollback
	_, err = client.ReadFile(ctx, sb.ID, "bad.txt")
	if err == nil {
		t.Fatal("expected bad.txt to be removed after rollback")
	}

	// 7. Verify Containment
	rep, err := client.VerifyContainment(ctx, sb.ID)
	if err != nil {
		t.Fatalf("VerifyContainment failed: %v", err)
	}
	if !rep.AirgapVerified || !rep.ReadOnlyRootfs {
		t.Errorf("expected airgap and readonly rootfs, got: %+v", rep)
	}

	// 8. Destroy Sandbox
	if err := client.DestroySandbox(ctx, sb.ID); err != nil {
		t.Fatalf("DestroySandbox failed: %v", err)
	}
}

func TestHermeticLocalSandbox_FatPayloadGuard(t *testing.T) {
	tempBase := t.TempDir()
	client := vm.NewHermeticLocalSandbox(tempBase)
	ctx := context.Background()

	sb, err := client.CreateSandbox(ctx, vm.SandboxConfig{})
	if err != nil {
		t.Fatalf("CreateSandbox failed: %v", err)
	}
	defer client.DestroySandbox(ctx, sb.ID)

	// Write a file with > 4096 bytes and run command to output it
	largeContent := strings.Repeat("Error diagnostic trace line in compiler log output\n", 120)
	if err := client.WriteFile(ctx, sb.ID, "huge.log", largeContent); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	cmd := "type huge.log"
	if runtime.GOOS != "windows" {
		cmd = "cat huge.log"
	}
	res, err := client.RunCommand(ctx, vm.CommandRequest{
		SandboxID: sb.ID,
		Command:   cmd,
		Raw:       false,
	})
	if err != nil {
		t.Fatalf("RunCommand failed: %v", err)
	}

	if res.VaultedSlot == "" || !res.IsTruncated {
		t.Fatalf("expected output to be vaulted with FatPayload guard, got slot=%q, truncated=%v", res.VaultedSlot, res.IsTruncated)
	}
	if !strings.Contains(res.Summary, "vaulted to quote.slot:vm_") {
		t.Errorf("expected summary to cite quote.slot:vm_, got: %s", res.Summary)
	}
}

func TestHermeticLocalSandbox_ClaimFreeTokens(t *testing.T) {
	client := vm.NewHermeticLocalSandbox(t.TempDir())
	ctx := context.Background()

	key, err := client.ClaimFreeTokens(ctx, "support@caleralabs.com", "TestAgent")
	if err != nil {
		t.Fatalf("ClaimFreeTokens failed: %v", err)
	}
	if !strings.HasPrefix(key, "calera_pilot_1m_") {
		t.Fatalf("expected key starting with calera_pilot_1m_, got: %s", key)
	}
}
