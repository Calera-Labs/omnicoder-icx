// SPDX-License-Identifier: Apache-2.0
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/engine"
)

// TestHelperProcess serves as the mock child subprocess for unit tests.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	args := os.Args
	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(2)
	}

	subCmd := args[0]
	switch subCmd {
	case "mock-worker":
		// Mock worker with --offline --audit-only
		rep := engine.RunReport{
			Success:      true,
			Task:         "Mock Phase Execution",
			TurnsCount:   1,
			MerkleSeal:   "seal_mock_worker_audit_only",
			ExecutionMS:  50,
			FilesTouched: []string{"pkg/test/mock.go"},
		}
		data, _ := json.Marshal(rep)
		fmt.Println(string(data))
		os.Exit(0)

	case "stream-json":
		events := []engine.TurnProgressEvent{
			{Turn: 1, MaxTurns: 5, Stage: 1, StageName: "SCAFFOLD", Status: "STAGE", Details: "pkg/test/scaffold.go"},
			{Turn: 1, MaxTurns: 5, Stage: 2, StageName: "MUTATE", Status: "STAGE", Details: "pkg/test/mock.go"},
			{Turn: 1, MaxTurns: 5, Stage: 3, StageName: "VERIFY", Status: "PASS", Details: "all unit tests passed"},
		}
		for _, ev := range events {
			b, _ := json.Marshal(ev)
			fmt.Println(string(b))
			time.Sleep(30 * time.Millisecond)
		}
		rep := engine.RunReport{
			Success:      true,
			Task:         "Streaming Phase Execution",
			TurnsCount:   1,
			MerkleSeal:   "seal_stream_merkle_provenance",
			ExecutionMS:  120,
			FilesTouched: []string{"pkg/test/mock.go"},
		}
		b, _ := json.Marshal(rep)
		fmt.Println(string(b))
		os.Exit(0)

	case "hang":
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case "long-running":
		time.Sleep(60 * time.Second)
		os.Exit(0)

	default:
		os.Exit(1)
	}
}

func createHelperCmd(ctx context.Context, helperSubCmd string, extraArgs ...string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	args := []string{"-test.run=TestHelperProcess", "--", helperSubCmd}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	return cmd
}

// TestWorker_SubprocessSpawn spawns a mock worker process with --offline --audit-only
// and asserts clean execution and exit code 0.
func TestWorker_SubprocessSpawn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	eventCh := make(chan SwarmEvent, 50)
	helperCmd := createHelperCmd(ctx, "mock-worker", "--offline", "--audit-only")

	cfg := WorkerConfig{
		PhaseNumber: 1,
		PhaseName:   "Mock Ingestion",
		Offline:     true,
		AuditOnly:   true,
		CustomCmd:   helperCmd,
	}

	worker, err := LaunchWorker(ctx, cfg, eventCh)
	if err != nil {
		t.Fatalf("failed to launch worker: %v", err)
	}

	if worker.PID <= 0 {
		t.Errorf("expected worker PID > 0, got %d", worker.PID)
	}
	if worker.State != WorkerActive && worker.State != WorkerPassed {
		t.Errorf("expected worker active or passed, got %v", worker.State)
	}

	waitErr := worker.Wait()
	if waitErr != nil {
		t.Fatalf("worker failed with error: %v", waitErr)
	}

	if worker.GetState() != WorkerPassed {
		t.Errorf("expected WorkerPassed, got %v", worker.GetState())
	}
	if worker.GetTurnCount() != 1 {
		t.Errorf("expected turn count 1, got %d", worker.GetTurnCount())
	}
	if worker.GetMerkleSeal() != "seal_mock_worker_audit_only" {
		t.Errorf("expected seal_mock_worker_audit_only, got %s", worker.GetMerkleSeal())
	}

	// Verify real omnicoder binary if present in workspace root
	omnicoderExe := filepath.Join("..", "..", "omnicoder.exe")
	if _, err := os.Stat(omnicoderExe); err == nil && !testing.Short() {
		absExe, _ := filepath.Abs(omnicoderExe)
		realCtx, realCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer realCancel()

		realEventCh := make(chan SwarmEvent, 50)
		realCfg := WorkerConfig{
			PhaseNumber: 99,
			PhaseName:   "Real Binary Check",
			BinaryPath:  absExe,
			Offline:     true,
			AuditOnly:   true,
			MaxTurns:    1,
			TaskPrompt:  "Audit check",
		}
		realWorker, rErr := LaunchWorker(realCtx, realCfg, realEventCh)
		if rErr == nil {
			_ = realWorker.Wait()
			if realWorker.PID <= 0 {
				t.Errorf("expected real worker PID > 0")
			}
		}
	}
}

// TestWorker_JSONStreaming validates that progress events emitted by the child
// are deserialized by the Master in real time.
func TestWorker_JSONStreaming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	eventCh := make(chan SwarmEvent, 50)
	helperCmd := createHelperCmd(ctx, "stream-json")

	cfg := WorkerConfig{
		PhaseNumber: 2,
		PhaseName:   "JSON Streaming Test",
		CustomCmd:   helperCmd,
	}

	worker, err := LaunchWorker(ctx, cfg, eventCh)
	if err != nil {
		t.Fatalf("failed to launch worker: %v", err)
	}

	var receivedEvents []SwarmEvent
	doneReading := make(chan struct{})

	go func() {
		for ev := range eventCh {
			receivedEvents = append(receivedEvents, ev)
			if ev.State == WorkerPassed || ev.State == WorkerFailed {
				break
			}
		}
		close(doneReading)
	}()

	waitErr := worker.Wait()
	if waitErr != nil {
		t.Fatalf("worker failed: %v", waitErr)
	}

	select {
	case <-doneReading:
	case <-time.After(3 * time.Second):
	}

	var foundScaffold, foundMutate, foundVerify, foundReport bool
	for _, ev := range receivedEvents {
		if strings.Contains(ev.Message, "SCAFFOLD") {
			foundScaffold = true
		}
		if strings.Contains(ev.Message, "MUTATE") {
			foundMutate = true
		}
		if strings.Contains(ev.Message, "VERIFY") {
			foundVerify = true
		}
		if strings.Contains(ev.Message, "seal_stream_merkle_provenance") {
			foundReport = true
		}
	}

	if !foundScaffold {
		t.Errorf("expected to find SCAFFOLD event, received: %+v", receivedEvents)
	}
	if !foundMutate {
		t.Errorf("expected to find MUTATE event, received: %+v", receivedEvents)
	}
	if !foundVerify {
		t.Errorf("expected to find VERIFY event, received: %+v", receivedEvents)
	}
	if !foundReport {
		t.Errorf("expected to find RunReport event with seal, received: %+v", receivedEvents)
	}

	if worker.GetMerkleSeal() != "seal_stream_merkle_provenance" {
		t.Errorf("expected seal_stream_merkle_provenance, got %s", worker.GetMerkleSeal())
	}
	if worker.GetState() != WorkerPassed {
		t.Errorf("expected WorkerPassed, got %v", worker.GetState())
	}
}

// TestWorker_WatchdogTimeout launches a simulated hanging worker and asserts
// that the watchdog terminates the process within the timeout window.
func TestWorker_WatchdogTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	eventCh := make(chan SwarmEvent, 50)
	helperCmd := createHelperCmd(ctx, "hang")

	cfg := WorkerConfig{
		PhaseNumber:            3,
		PhaseName:              "Watchdog Timeout Test",
		CustomCmd:              helperCmd,
		WatchdogSilenceTimeout: 300 * time.Millisecond,
		WatchdogInterval:       50 * time.Millisecond,
		Timeout:                5 * time.Second,
	}

	start := time.Now()
	worker, err := LaunchWorker(ctx, cfg, eventCh)
	if err != nil {
		t.Fatalf("failed to launch worker: %v", err)
	}

	pid := worker.GetPID()
	if pid <= 0 {
		t.Fatalf("expected PID > 0, got %d", pid)
	}

	waitErr := worker.Wait()
	elapsed := time.Since(start)

	if waitErr == nil {
		t.Fatalf("expected watchdog failure error, got nil")
	}

	if worker.GetState() != WorkerFailed {
		t.Errorf("expected WorkerFailed, got %v", worker.GetState())
	}

	lastErr := worker.GetLastError()
	if !strings.Contains(lastErr, "watchdog timeout") && !strings.Contains(lastErr, "silent") {
		t.Errorf("expected watchdog timeout error message, got: %s", lastErr)
	}

	if elapsed > 4*time.Second {
		t.Errorf("watchdog took too long to terminate: %v", elapsed)
	}

	time.Sleep(200 * time.Millisecond)
	if IsProcessRunning(pid) {
		t.Errorf("process PID %d is still running in OS after watchdog termination", pid)
	}
}

// TestWorker_GracefulReaping kills the parent context and verifies that child
// processes are completely eliminated from the OS process tree.
func TestWorker_GracefulReaping(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())

	eventCh := make(chan SwarmEvent, 50)
	helperCmd := createHelperCmd(parentCtx, "long-running")

	cfg := WorkerConfig{
		PhaseNumber:            4,
		PhaseName:              "Graceful Reaping Test",
		CustomCmd:              helperCmd,
		WatchdogSilenceTimeout: 60 * time.Second,
		WatchdogInterval:       50 * time.Millisecond,
		Timeout:                60 * time.Second,
	}

	worker, err := LaunchWorker(parentCtx, cfg, eventCh)
	if err != nil {
		t.Fatalf("failed to launch worker: %v", err)
	}

	pid := worker.GetPID()
	if pid <= 0 {
		t.Fatalf("expected PID > 0, got %d", pid)
	}

	if !IsProcessRunning(pid) {
		t.Fatalf("expected PID %d to be actively running in OS", pid)
	}

	// Cancel parent context
	parentCancel()

	// Wait for worker termination
	_ = worker.Wait()

	time.Sleep(300 * time.Millisecond)
	if IsProcessRunning(pid) {
		t.Errorf("process PID %d still exists in OS process tree after context cancellation", pid)
	}

	if worker.GetState() != WorkerFailed {
		t.Errorf("expected WorkerFailed after reaping, got %v", worker.GetState())
	}
}

// TestWorker_VerificationCommand verifies that post-execution verification commands
// execute correctly upon child completion.
func TestWorker_VerificationCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	eventCh := make(chan SwarmEvent, 50)
	helperCmd := createHelperCmd(ctx, "mock-worker")

	cfg := WorkerConfig{
		PhaseNumber:     5,
		PhaseName:       "Verification Command Test",
		CustomCmd:       helperCmd,
		VerificationCmd: "go version",
	}

	worker, err := LaunchWorker(ctx, cfg, eventCh)
	if err != nil {
		t.Fatalf("failed to launch worker: %v", err)
	}

	waitErr := worker.Wait()
	if waitErr != nil {
		t.Fatalf("expected verification to succeed, got error: %v", waitErr)
	}

	if worker.GetState() != WorkerPassed {
		t.Errorf("expected WorkerPassed, got %v", worker.GetState())
	}
}

// TestWorker_ReapAllActiveWorkers tests global reaping on master shutdown.
func TestWorker_ReapAllActiveWorkers(t *testing.T) {
	ctx := context.Background()
	eventCh := make(chan SwarmEvent, 50)
	helperCmd := createHelperCmd(ctx, "long-running")

	cfg := WorkerConfig{
		PhaseNumber:            6,
		PhaseName:              "Global Reap Test",
		CustomCmd:              helperCmd,
		WatchdogSilenceTimeout: 60 * time.Second,
		WatchdogInterval:       50 * time.Millisecond,
		Timeout:                60 * time.Second,
	}

	worker, err := LaunchWorker(ctx, cfg, eventCh)
	if err != nil {
		t.Fatalf("failed to launch worker: %v", err)
	}

	pid := worker.GetPID()
	if !IsProcessRunning(pid) {
		t.Fatalf("expected PID %d to be running", pid)
	}

	// Trigger unconditional reap of all active workers
	ReapAllActiveWorkers()

	_ = worker.Wait()
	time.Sleep(300 * time.Millisecond)

	if IsProcessRunning(pid) {
		t.Errorf("expected PID %d to be terminated after ReapAllActiveWorkers", pid)
	}
}
