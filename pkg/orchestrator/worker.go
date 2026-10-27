// SPDX-License-Identifier: Apache-2.0
// Package orchestrator implements the isolated multi-process parallel agent swarm engine
// for OmniCoder-ICX.
//
// Supervised Worker Process Lifecycle & Stdio IPC Protocol.
// The Master launches independent OS-level child instances of omnicoder.exe run,
// supervises their PIDs, streams structured JSON telemetry, and enforces watchdog timeouts.

package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

// WorkerState models child process execution status.
type WorkerState string

const (
	WorkerPending   WorkerState = "PENDING"
	WorkerSpawning  WorkerState = "SPAWNING"
	WorkerActive    WorkerState = "ACTIVE"
	WorkerVerifying WorkerState = "VERIFYING"
	WorkerHealLoop  WorkerState = "HEAL_LOOP"
	WorkerPassed    WorkerState = "PASSED"
	WorkerFailed    WorkerState = "FAILED"
	WorkerMerged    WorkerState = "MERGED"
)

// SwarmEvent captures real-time progress for the unified terminal dashboard.
type SwarmEvent struct {
	Timestamp  time.Time   `json:"timestamp"`
	WorkerID   int         `json:"worker_id"`
	Phase      int         `json:"phase"`
	State      WorkerState `json:"state"`
	Message    string      `json:"message"`
	Tokens     int         `json:"tokens"`
	ActiveFile string      `json:"active_file"`
}

// SwarmWorker manages a supervised child omnicoder process.
type SwarmWorker struct {
	ID              int                   `json:"id"`
	PhaseNumber     int                   `json:"phase_number"`
	PhaseName       string                `json:"phase_name"`
	WorktreeDir     string                `json:"worktree_dir"`
	BranchName      string                `json:"branch_name"`
	ScopePrefix     string                `json:"scope_prefix"`
	TargetFiles     []string              `json:"target_files"`
	VerificationCmd string                `json:"verification_cmd"`
	State           WorkerState           `json:"state"`
	PID             int                   `json:"pid"`
	Cmd             *exec.Cmd             `json:"-"`
	StartTime       time.Time             `json:"start_time"`
	EndTime         time.Time             `json:"end_time"`
	TokensUsed      int                   `json:"tokens_used"`
	TurnCount       int                   `json:"turn_count"`
	LastError       string                `json:"last_error,omitempty"`
	MerkleSeal      string                `json:"merkle_seal,omitempty"`
	Receipt         *icx.ActuationReceipt `json:"receipt,omitempty"`
	mu              sync.RWMutex

	// internal supervision controls
	done         chan struct{}
	err          error
	lastActivity time.Time
	stderrBuf    *strings.Builder
	watchdogDone chan struct{}
	cancelWorker context.CancelFunc
}

// WorkerConfig models configuration for launching a child worker.
type WorkerConfig struct {
	ID                     int           `json:"id"`
	PhaseNumber            int           `json:"phase_number"`
	PhaseName              string        `json:"phase_name"`
	WorktreeDir            string        `json:"worktree_dir"`
	BranchName             string        `json:"branch_name"`
	ScopePrefix            string        `json:"scope_prefix"`
	TargetFiles            []string      `json:"target_files"`
	VerificationCmd        string        `json:"verification_cmd"`
	TaskPrompt             string        `json:"task_prompt"`
	MaxTurns               int           `json:"max_turns"`
	Timeout                time.Duration `json:"timeout"`
	WatchdogSilenceTimeout time.Duration `json:"watchdog_silence_timeout"`
	WatchdogInterval       time.Duration `json:"watchdog_interval"`
	BinaryPath             string        `json:"binary_path"`
	Model                  string        `json:"model"`
	BaseURL                string        `json:"base_url"`
	APIKey                 string        `json:"api_key"`
	Offline                bool          `json:"offline"`
	AuditOnly              bool          `json:"audit_only"`
	JSON                   bool          `json:"json"`
	ExtraArgs              []string      `json:"extra_args"`
	CustomCmd              *exec.Cmd     `json:"-"`
}

var (
	activeWorkersMu sync.Mutex
	activeWorkers   = make(map[int]*SwarmWorker)
)

func registerActiveWorker(w *SwarmWorker) {
	activeWorkersMu.Lock()
	defer activeWorkersMu.Unlock()
	activeWorkers[w.PID] = w
}

func unregisterActiveWorker(pid int) {
	activeWorkersMu.Lock()
	defer activeWorkersMu.Unlock()
	delete(activeWorkers, pid)
}

// ReapAllActiveWorkers terminates all currently active child worker process trees.
func ReapAllActiveWorkers() {
	activeWorkersMu.Lock()
	workers := make([]*SwarmWorker, 0, len(activeWorkers))
	for _, w := range activeWorkers {
		workers = append(workers, w)
	}
	activeWorkersMu.Unlock()

	for _, w := range workers {
		_ = w.Kill()
	}
}

// LaunchWorker spawns an independent child process targeting the isolated worktree.
func LaunchWorker(ctx context.Context, cfg WorkerConfig, eventCh chan<- SwarmEvent) (*SwarmWorker, error) {
	if cfg.ID <= 0 {
		cfg.ID = cfg.PhaseNumber
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.WatchdogSilenceTimeout <= 0 {
		cfg.WatchdogSilenceTimeout = 180 * time.Second
	}
	if cfg.WatchdogInterval <= 0 {
		cfg.WatchdogInterval = 500 * time.Millisecond
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 5
	}

	workerCtx, workerCancel := context.WithCancel(ctx)

	worker := &SwarmWorker{
		ID:              cfg.ID,
		PhaseNumber:     cfg.PhaseNumber,
		PhaseName:       cfg.PhaseName,
		WorktreeDir:     cfg.WorktreeDir,
		BranchName:      cfg.BranchName,
		ScopePrefix:     cfg.ScopePrefix,
		TargetFiles:     cfg.TargetFiles,
		VerificationCmd: cfg.VerificationCmd,
		State:           WorkerSpawning,
		StartTime:       time.Now(),
		done:            make(chan struct{}),
		watchdogDone:    make(chan struct{}),
		stderrBuf:       &strings.Builder{},
		cancelWorker:    workerCancel,
		lastActivity:    time.Now(),
	}

	emitEvent(eventCh, SwarmEvent{
		Timestamp: time.Now(),
		WorkerID:  worker.ID,
		Phase:     worker.PhaseNumber,
		State:     WorkerSpawning,
		Message:   fmt.Sprintf("Spawning worker for Phase %d: %s", worker.PhaseNumber, worker.PhaseName),
	})

	var cmd *exec.Cmd
	if cfg.CustomCmd != nil {
		cmd = cfg.CustomCmd
	} else {
		exePath := cfg.BinaryPath
		if exePath == "" {
			var err error
			exePath, err = os.Executable()
			if err != nil {
				exePath = "omnicoder.exe"
			}
			// If executing inside a test binary, look for omnicoder.exe in vicinity
			if strings.Contains(filepath.Base(exePath), ".test") {
				candidates := []string{
					filepath.Join(cfg.WorktreeDir, "omnicoder.exe"),
					"omnicoder.exe",
					filepath.Join("..", "..", "omnicoder.exe"),
					filepath.Join("..", "omnicoder.exe"),
				}
				for _, cand := range candidates {
					if _, err := os.Stat(cand); err == nil {
						abs, _ := filepath.Abs(cand)
						exePath = abs
						break
					}
				}
			}
		}

		args := []string{"run"}
		if cfg.WorktreeDir != "" {
			args = append(args, "--workspace="+cfg.WorktreeDir)
		}
		if cfg.ScopePrefix != "" {
			args = append(args, "--scope="+cfg.ScopePrefix)
		}
		args = append(args, fmt.Sprintf("--max-turns=%d", cfg.MaxTurns))
		args = append(args, "--json")

		if cfg.Offline {
			args = append(args, "--offline")
		}
		if cfg.AuditOnly {
			args = append(args, "--audit-only")
		}
		if cfg.Model != "" {
			args = append(args, "--model="+cfg.Model)
		}
		if cfg.BaseURL != "" {
			args = append(args, "--base-url="+cfg.BaseURL)
		}
		if cfg.APIKey != "" {
			args = append(args, "--api-key="+cfg.APIKey)
		}
		if len(cfg.ExtraArgs) > 0 {
			args = append(args, cfg.ExtraArgs...)
		}

		taskPrompt := cfg.TaskPrompt
		if taskPrompt == "" {
			if cfg.PhaseName != "" {
				taskPrompt = fmt.Sprintf("Complete Phase %d: %s. Focus exclusively on target files.", cfg.PhaseNumber, cfg.PhaseName)
			} else {
				taskPrompt = fmt.Sprintf("Complete Phase %d. Focus exclusively on target files.", cfg.PhaseNumber)
			}
		}
		args = append(args, taskPrompt)

		cmd = exec.CommandContext(workerCtx, exePath, args...)
		if cfg.WorktreeDir != "" {
			cmd.Dir = cfg.WorktreeDir
		}
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		workerCancel()
		return nil, fmt.Errorf("failed to open stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdoutPipe.Close()
		workerCancel()
		return nil, fmt.Errorf("failed to open stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdoutPipe.Close()
		_ = stderrPipe.Close()
		workerCancel()
		return nil, fmt.Errorf("failed to start worker subprocess: %w", err)
	}

	worker.mu.Lock()
	worker.Cmd = cmd
	worker.PID = cmd.Process.Pid
	worker.State = WorkerActive
	worker.lastActivity = time.Now()
	worker.mu.Unlock()

	registerActiveWorker(worker)

	emitEvent(eventCh, SwarmEvent{
		Timestamp: time.Now(),
		WorkerID:  worker.ID,
		Phase:     worker.PhaseNumber,
		State:     WorkerActive,
		Message:   fmt.Sprintf("Worker PID %d active in %s", worker.PID, worker.WorktreeDir),
	})

	// 1. Drain stderr in real time for diagnostics
	go func() {
		buf := make([]byte, 2048)
		for {
			n, rErr := stderrPipe.Read(buf)
			if n > 0 {
				worker.mu.Lock()
				worker.stderrBuf.Write(buf[:n])
				worker.mu.Unlock()
			}
			if rErr != nil {
				break
			}
		}
	}()

	// 2. Stream and parse structured JSON telemetry from stdout
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			worker.updateActivity()
			worker.parseJSONLine(line, eventCh)
		}
	}()

	// 3. Watchdog Heartbeat & Silence/Timeout Enforcer
	go func() {
		ticker := time.NewTicker(cfg.WatchdogInterval)
		defer ticker.Stop()

		for {
			select {
			case <-worker.watchdogDone:
				return
			case <-workerCtx.Done():
				worker.mu.Lock()
				if worker.State != WorkerPassed && worker.State != WorkerFailed {
					worker.State = WorkerFailed
					worker.LastError = fmt.Sprintf("context cancelled: %v", workerCtx.Err())
					worker.err = workerCtx.Err()
				}
				pid := worker.PID
				c := worker.Cmd
				worker.mu.Unlock()

				emitEvent(eventCh, SwarmEvent{
					Timestamp: time.Now(),
					WorkerID:  worker.ID,
					Phase:     worker.PhaseNumber,
					State:     WorkerFailed,
					Message:   fmt.Sprintf("Context cancelled for worker PID %d: %v", pid, workerCtx.Err()),
				})

				_ = KillProcessTreeByPID(pid, c)
				return

			case <-ticker.C:
				// Silence watchdog check
				lastAct := worker.getLastActivity()
				if time.Since(lastAct) > cfg.WatchdogSilenceTimeout {
					msg := fmt.Sprintf("watchdog timeout: silent for %v (threshold: %v)", time.Since(lastAct).Round(time.Millisecond), cfg.WatchdogSilenceTimeout)
					worker.mu.Lock()
					worker.State = WorkerFailed
					worker.LastError = msg
					worker.err = errors.New(msg)
					pid := worker.PID
					c := worker.Cmd
					worker.mu.Unlock()

					emitEvent(eventCh, SwarmEvent{
						Timestamp: time.Now(),
						WorkerID:  worker.ID,
						Phase:     worker.PhaseNumber,
						State:     WorkerFailed,
						Message:   msg,
					})

					_ = KillProcessTreeByPID(pid, c)
					return
				}

				// Total phase duration timeout check
				if time.Since(worker.StartTime) > cfg.Timeout {
					msg := fmt.Sprintf("phase timeout: exceeded maximum duration %v", cfg.Timeout)
					worker.mu.Lock()
					worker.State = WorkerFailed
					worker.LastError = msg
					worker.err = errors.New(msg)
					pid := worker.PID
					c := worker.Cmd
					worker.mu.Unlock()

					emitEvent(eventCh, SwarmEvent{
						Timestamp: time.Now(),
						WorkerID:  worker.ID,
						Phase:     worker.PhaseNumber,
						State:     WorkerFailed,
						Message:   msg,
					})

					_ = KillProcessTreeByPID(pid, c)
					return
				}
			}
		}
	}()

	// 4. Subprocess Wait & Verification Lifecycle
	go func() {
		waitErr := cmd.Wait()
		unregisterActiveWorker(worker.PID)
		close(worker.watchdogDone)

		worker.mu.Lock()
		worker.EndTime = time.Now()
		stderrContent := strings.TrimSpace(worker.stderrBuf.String())

		if waitErr != nil {
			worker.State = WorkerFailed
			if worker.LastError == "" {
				if stderrContent != "" {
					worker.LastError = fmt.Sprintf("worker process exited with error: %v (stderr: %s)", waitErr, stderrContent)
				} else {
					worker.LastError = fmt.Sprintf("worker process exited with error: %v", waitErr)
				}
			}
			if worker.err == nil {
				worker.err = waitErr
			}
			worker.mu.Unlock()

			emitEvent(eventCh, SwarmEvent{
				Timestamp: time.Now(),
				WorkerID:  worker.ID,
				Phase:     worker.PhaseNumber,
				State:     WorkerFailed,
				Message:   worker.GetLastError(),
			})
			close(worker.done)
			return
		}

		// If watchdog marked it failed before exit, preserve failure
		if worker.State == WorkerFailed {
			worker.mu.Unlock()
			close(worker.done)
			return
		}

		// Run post-execution verification command if specified
		if cfg.VerificationCmd != "" {
			worker.State = WorkerVerifying
			worker.mu.Unlock()

			emitEvent(eventCh, SwarmEvent{
				Timestamp: time.Now(),
				WorkerID:  worker.ID,
				Phase:     worker.PhaseNumber,
				State:     WorkerVerifying,
				Message:   "Running verification command: " + cfg.VerificationCmd,
			})

			vErr := runVerificationCmd(workerCtx, cfg.VerificationCmd, cfg.WorktreeDir)
			worker.mu.Lock()
			if vErr != nil {
				worker.State = WorkerFailed
				worker.LastError = fmt.Sprintf("verification failed in worktree: %v", vErr)
				worker.err = vErr
				worker.mu.Unlock()

				emitEvent(eventCh, SwarmEvent{
					Timestamp: time.Now(),
					WorkerID:  worker.ID,
					Phase:     worker.PhaseNumber,
					State:     WorkerFailed,
					Message:   worker.GetLastError(),
				})
				close(worker.done)
				return
			}
			worker.mu.Unlock()
		} else {
			worker.mu.Unlock()
		}

		worker.mu.Lock()
		worker.State = WorkerPassed
		worker.mu.Unlock()

		emitEvent(eventCh, SwarmEvent{
			Timestamp: time.Now(),
			WorkerID:  worker.ID,
			Phase:     worker.PhaseNumber,
			State:     WorkerPassed,
			Message:   "Phase completed and verified successfully",
			Tokens:    worker.GetTokensUsed(),
		})
		close(worker.done)
	}()

	return worker, nil
}

// Wait blocks until the worker process and its verification stage have concluded.
func (w *SwarmWorker) Wait() error {
	<-w.done
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.err
}

// Kill unconditionally terminates the child worker process and its subprocess tree.
func (w *SwarmWorker) Kill() error {
	w.mu.Lock()
	pid := w.PID
	cmd := w.Cmd
	if w.cancelWorker != nil {
		w.cancelWorker()
	}
	w.mu.Unlock()

	if pid <= 0 {
		return nil
	}
	return KillProcessTreeByPID(pid, cmd)
}

// GetState returns the current execution state in a thread-safe manner.
func (w *SwarmWorker) GetState() WorkerState {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.State
}

// SetState updates the worker state in a thread-safe manner.
func (w *SwarmWorker) SetState(s WorkerState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.State = s
}

// GetPID returns the child process ID.
func (w *SwarmWorker) GetPID() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.PID
}

// GetTurnCount returns the latest turn number processed by the worker.
func (w *SwarmWorker) GetTurnCount() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.TurnCount
}

// GetTokensUsed returns the token or runtime count.
func (w *SwarmWorker) GetTokensUsed() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.TokensUsed
}

// GetLastError returns the last recorded failure diagnostic.
func (w *SwarmWorker) GetLastError() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.LastError
}

// GetDetailedDiagnostic returns a comprehensive diagnostic report of the worker's execution,
// including duration, phase metadata, exit error, and tail of stderr or verification output.
func (w *SwarmWorker) GetDetailedDiagnostic() string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var parts []string
	parts = append(parts, fmt.Sprintf("Phase %d (%s) [PID %d]", w.PhaseNumber, w.PhaseName, w.PID))
	parts = append(parts, fmt.Sprintf("State: %s", w.State))
	if !w.StartTime.IsZero() {
		dur := time.Since(w.StartTime)
		if !w.EndTime.IsZero() {
			dur = w.EndTime.Sub(w.StartTime)
		}
		parts = append(parts, fmt.Sprintf("Duration: %v", dur.Round(time.Millisecond)))
	}
	if w.LastError != "" {
		parts = append(parts, fmt.Sprintf("Failure: %s", w.LastError))
	} else if w.err != nil {
		parts = append(parts, fmt.Sprintf("Exit Error: %v", w.err))
	}
	if w.stderrBuf != nil {
		stderrStr := strings.TrimSpace(w.stderrBuf.String())
		if stderrStr != "" {
			lines := strings.Split(stderrStr, "\n")
			if len(lines) > 20 {
				lines = lines[len(lines)-20:]
			}
			parts = append(parts, fmt.Sprintf("Stderr Tail:\n%s", strings.Join(lines, "\n")))
		}
	}
	return strings.Join(parts, " | ")
}

// GetMerkleSeal returns the cryptographic audit seal.
func (w *SwarmWorker) GetMerkleSeal() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.MerkleSeal
}

func (w *SwarmWorker) updateActivity() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastActivity = time.Now()
}

func (w *SwarmWorker) getLastActivity() time.Time {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lastActivity
}

func (w *SwarmWorker) parseJSONLine(line string, eventCh chan<- SwarmEvent) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return
	}

	// 1. Check for engine.TurnProgressEvent
	var progress engine.TurnProgressEvent
	if err := json.Unmarshal([]byte(trimmed), &progress); err == nil && (progress.StageName != "" || progress.Status != "" || progress.Stage > 0) {
		w.mu.Lock()
		if progress.Turn > 0 {
			w.TurnCount = progress.Turn
		}
		switch progress.Status {
		case "START", "STAGE":
			w.State = WorkerActive
		case "HEAL":
			w.State = WorkerHealLoop
		case "PASS":
			w.State = WorkerActive
		case "FAIL":
			w.State = WorkerFailed
			w.LastError = progress.Details
		}
		curState := w.State
		tokens := w.TokensUsed
		w.mu.Unlock()

		emitEvent(eventCh, SwarmEvent{
			Timestamp:  time.Now(),
			WorkerID:   w.ID,
			Phase:      w.PhaseNumber,
			State:      curState,
			Message:    fmt.Sprintf("[%s] Stage %d (%s): %s", progress.Status, progress.Stage, progress.StageName, progress.Details),
			Tokens:     tokens,
			ActiveFile: progress.Details,
		})
		return
	}

	// 2. Check for engine.RunReport
	var report engine.RunReport
	if err := json.Unmarshal([]byte(trimmed), &report); err == nil && (report.Task != "" || report.MerkleSeal != "" || report.TurnsCount > 0) {
		w.mu.Lock()
		if report.TurnsCount > 0 {
			w.TurnCount = report.TurnsCount
		}
		if report.ExecutionMS > 0 {
			w.TokensUsed = int(report.ExecutionMS)
		}
		if report.MerkleSeal != "" {
			w.MerkleSeal = report.MerkleSeal
		}
		if !report.Success && report.ErrorMessage != "" {
			w.LastError = report.ErrorMessage
		}
		curState := w.State
		w.mu.Unlock()

		emitEvent(eventCh, SwarmEvent{
			Timestamp:  time.Now(),
			WorkerID:   w.ID,
			Phase:      w.PhaseNumber,
			State:      curState,
			Message:    fmt.Sprintf("RunReport: success=%v, turns=%d, seal=%s", report.Success, report.TurnsCount, report.MerkleSeal),
			Tokens:     w.GetTokensUsed(),
			ActiveFile: strings.Join(report.FilesTouched, ", "),
		})
		return
	}

	// 3. Check for generic JSON telemetry or CLI output mapping
	var generic map[string]any
	if err := json.Unmarshal([]byte(trimmed), &generic); err == nil {
		w.mu.Lock()
		if seal, ok := generic["merkle_seal"].(string); ok && seal != "" {
			w.MerkleSeal = seal
		}
		if ms, ok := generic["execution_ms"].(float64); ok && ms > 0 {
			w.TokensUsed = int(ms)
		}
		if status, ok := generic["status"].(string); ok && status == "failed" {
			w.State = WorkerFailed
		}
		w.mu.Unlock()
	}
}

// KillProcessTree terminates the process tree for the given PID.
func KillProcessTree(pid int) error {
	return KillProcessTreeByPID(pid, nil)
}

// KillProcessTreeByPID terminates the process and its subprocess tree.
func KillProcessTreeByPID(pid int, cmd *exec.Cmd) error {
	if pid <= 0 {
		return nil
	}
	killSubprocessTree(pid)
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}

// IsProcessRunning checks if a process with the given PID is currently active.
func IsProcessRunning(pid int) bool {
	return isProcessRunningOS(pid)
}

func runVerificationCmd(ctx context.Context, verifyCmd string, dir string) error {
	if strings.TrimSpace(verifyCmd) == "" {
		return nil
	}
	cleaned := engine.CleanVerificationCmd(verifyCmd)
	if dir == "" {
		dir = "."
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cleaned)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cleaned)
	}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: output: %s", err, string(out))
	}
	return nil
}

func emitEvent(ch chan<- SwarmEvent, ev SwarmEvent) {
	if ch == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	select {
	case ch <- ev:
	case <-time.After(100 * time.Millisecond):
	}
}
