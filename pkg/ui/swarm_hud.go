// SPDX-License-Identifier: Apache-2.0
// Package ui provides terminal interface components for OmniCoder-ICX.
//
// Phase 6: Unified ANSI Terminal Matrix HUD.
// Real-time split-screen observability dashboard monitoring active swarm workers,
// turns, token consumption, and heal loops without stdout flicker or CI log corruption.

package ui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// SwarmWorkerStatus models live display state for a single row in the HUD matrix.
type SwarmWorkerStatus struct {
	WorkerID   int    `json:"worker_id"`
	PID        int    `json:"pid"`
	PhaseName  string `json:"phase_name"`
	Stage      string `json:"stage"`
	TurnCount  int    `json:"turn_count"`
	MaxTurns   int    `json:"max_turns"`
	TokensUsed int    `json:"tokens_used"`
	Status     string `json:"status"` // "[ACTIVE]", "[HEALING]", "[PASSED]", "[QUEUED]"
	ActiveFile string `json:"active_file"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

// SwarmHUD renders a double-buffered ANSI split-screen terminal display.
type SwarmHUD struct {
	writer      io.Writer
	workers     map[int]*SwarmWorkerStatus
	events      []string
	startTime   time.Time
	totalTokens int
	isTerminal  bool
	mu          sync.Mutex

	eventCursor      int
	lastWorkerStatus map[int]string
}

// NewSwarmHUD creates a new unified swarm observability dashboard.
// If w is nil, os.Stdout is used. Terminal detection is performed automatically,
// degrading gracefully to linear status output in non-TTY/CI environments.
func NewSwarmHUD(w io.Writer) *SwarmHUD {
	if w == nil {
		w = os.Stdout
	}

	isTerm := true
	if f, ok := w.(*os.File); ok {
		isTerm = isTerminalFile(f)
	}
	if os.Getenv("TERM") == "dumb" {
		isTerm = false
	}

	return &SwarmHUD{
		writer:           w,
		workers:          make(map[int]*SwarmWorkerStatus),
		events:           make([]string, 0, 16),
		startTime:        time.Now(),
		isTerminal:       isTerm,
		lastWorkerStatus: make(map[int]string),
	}
}

// SetTerminal explicitly enables or disables interactive terminal rendering.
func (hud *SwarmHUD) SetTerminal(isTerm bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	hud.isTerminal = isTerm
}

// IsTerminal returns true if interactive ANSI terminal rendering is active.
func (hud *SwarmHUD) IsTerminal() bool {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	return hud.isTerminal
}

// TotalTokens returns the aggregate token consumption across all registered workers.
func (hud *SwarmHUD) TotalTokens() int {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	return hud.totalTokens
}

// WorkerCount returns the number of workers registered in the HUD.
func (hud *SwarmHUD) WorkerCount() int {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	return len(hud.workers)
}

// GetWorker returns a copy of a worker's status by ID.
func (hud *SwarmHUD) GetWorker(id int) (*SwarmWorkerStatus, bool) {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	w, ok := hud.workers[id]
	if !ok {
		return nil, false
	}
	cp := *w
	return &cp, true
}

// GetEvents returns a slice of all recorded event log strings.
func (hud *SwarmHUD) GetEvents() []string {
	hud.mu.Lock()
	defer hud.mu.Unlock()
	res := make([]string, len(hud.events))
	copy(res, hud.events)
	return res
}

// UpdateWorker updates or registers the live status for a swarm worker slot.
func (hud *SwarmHUD) UpdateWorker(status SwarmWorkerStatus) {
	hud.mu.Lock()
	defer hud.mu.Unlock()

	st := status
	hud.workers[status.WorkerID] = &st

	// Recalculate aggregate token consumption across all workers
	tot := 0
	for _, w := range hud.workers {
		tot += w.TokensUsed
	}
	hud.totalTokens = tot
}

// LogEvent adds an event to the rolling bottom ticker in TTY mode,
// or emits it immediately as a linear log entry in non-TTY / CI mode.
func (hud *SwarmHUD) LogEvent(msg string) {
	hud.mu.Lock()
	defer hud.mu.Unlock()

	hud.events = append(hud.events, msg)

	if !hud.isTerminal {
		line := msg
		if !strings.HasPrefix(line, "[Swarm]") && !strings.HasPrefix(line, "[") {
			line = fmt.Sprintf("[Swarm] %s", line)
		}
		fmt.Fprintln(hud.writer, line)
		hud.eventCursor = len(hud.events)
	} else {
		// Retain rolling history buffer in memory
		if len(hud.events) > 50 {
			hud.events = hud.events[len(hud.events)-50:]
		}
	}
}

// Render writes a frame to the writer.
// In interactive terminal mode, it uses standard ANSI escape codes (\033[H and \033[J)
// with double-buffered rendering to prevent flicker.
// In non-TTY / CI mode, it outputs simple linear status lines.
func (hud *SwarmHUD) Render() {
	hud.mu.Lock()
	defer hud.mu.Unlock()

	if !hud.isTerminal {
		hud.renderNonTTY()
		return
	}

	hud.renderANSI()
}

// renderNonTTY emits clean, linear status lines suitable for CI logs and pipes.
func (hud *SwarmHUD) renderNonTTY() {
	// Flush any pending events not yet emitted
	for ; hud.eventCursor < len(hud.events); hud.eventCursor++ {
		line := hud.events[hud.eventCursor]
		if !strings.HasPrefix(line, "[Swarm]") && !strings.HasPrefix(line, "[") {
			line = fmt.Sprintf("[Swarm] %s", line)
		}
		fmt.Fprintln(hud.writer, line)
	}

	// Sort workers by ID for deterministic output
	sortedIDs := make([]int, 0, len(hud.workers))
	for id := range hud.workers {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Ints(sortedIDs)

	for _, id := range sortedIDs {
		w := hud.workers[id]
		statusName := formatLinearStatus(w.Status)
		tokensFormatted := formatNumberWithCommas(w.TokensUsed)

		var line string
		if w.MaxTurns > 0 {
			line = fmt.Sprintf("[Swarm] %s: %s (Turn %d/%d, %s tokens)",
				w.PhaseName, statusName, w.TurnCount, w.MaxTurns, tokensFormatted)
		} else {
			line = fmt.Sprintf("[Swarm] %s: %s (%s tokens)",
				w.PhaseName, statusName, tokensFormatted)
		}

		if hud.lastWorkerStatus[id] != line {
			fmt.Fprintln(hud.writer, line)
			if w.Diagnostic != "" && (strings.Contains(w.Status, "HEAL") || strings.Contains(w.Status, "FAIL")) {
				fmt.Fprintf(hud.writer, "        Diagnostic: %s\n", w.Diagnostic)
			}
			hud.lastWorkerStatus[id] = line
		}
	}
}

// renderANSI generates a flicker-free, double-buffered ANSI split-screen terminal frame.
func (hud *SwarmHUD) renderANSI() {
	var buf bytes.Buffer

	// Standard ANSI escape sequences: cursor to top-left and clear below
	buf.WriteString("\033[H\033[J")

	// Calculate uptime and active worker counts
	uptime := time.Since(hud.startTime).Round(time.Second)
	uptimeStr := fmt.Sprintf("%02d:%02d:%02d",
		int(uptime.Hours()), int(uptime.Minutes())%60, int(uptime.Seconds())%60)

	activeWorkers := 0
	for _, w := range hud.workers {
		if strings.Contains(w.Status, "ACTIVE") || strings.Contains(w.Status, "HEAL") {
			activeWorkers++
		}
	}

	totalTokensFormatted := formatNumberWithCommas(hud.totalTokens)

	// Top Border & Header Banner
	buf.WriteString("┌────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐\n")
	buf.WriteString("│  👑 OMNICODER-ICX :: PARALLEL AGENT SWARM OBSERVABILITY MATRIX HUD                                                    │\n")
	summaryLine := fmt.Sprintf("  Uptime: %s  |  Workers: %d active (%d total)  |  Total Tokens: %s  |  Status: ACTIVE",
		uptimeStr, activeWorkers, len(hud.workers), totalTokensFormatted)
	fmt.Fprintf(&buf, "│ %-118s │\n", truncate(summaryLine, 118))
	buf.WriteString("├──────┬────────┬──────────────────────────┬────────────────┬─────────┬────────────┬────────────┬────────────────────────────┤\n")
	buf.WriteString("│ SLOT │  PID   │ PHASE                    │ STAGE          │  TURNS  │   TOKENS   │ STATUS     │ ACTIVE FILE                │\n")
	buf.WriteString("├──────┼────────┼──────────────────────────┼────────────────┼─────────┼────────────┼────────────┼────────────────────────────┤\n")

	// Sort workers by ID
	sortedIDs := make([]int, 0, len(hud.workers))
	for id := range hud.workers {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Ints(sortedIDs)

	if len(sortedIDs) == 0 {
		buf.WriteString("│                                      (No active swarm workers currently registered)                                    │\n")
	} else {
		for _, id := range sortedIDs {
			w := hud.workers[id]
			slotStr := fmt.Sprintf("#%d", w.WorkerID)
			pidStr := fmt.Sprintf("%d", w.PID)
			phaseStr := truncate(w.PhaseName, 24)
			stageStr := truncate(w.Stage, 14)

			turnsStr := fmt.Sprintf("%d/%d", w.TurnCount, w.MaxTurns)
			if w.MaxTurns <= 0 {
				turnsStr = fmt.Sprintf("%d", w.TurnCount)
			}

			tokensStr := formatNumberWithCommas(w.TokensUsed)
			fileStr := truncatePath(w.ActiveFile, 26)
			statusBadge := formatStatusBadge(w.Status)

			fmt.Fprintf(&buf, "│ %-4s │ %-6s │ %-24s │ %-14s │ %-7s │ %10s │ %s │ %-26s │\n",
				slotStr, pidStr, phaseStr, stageStr, turnsStr, tokensStr, statusBadge, fileStr)

			if w.Diagnostic != "" && (strings.Contains(w.Status, "HEAL") || strings.Contains(w.Status, "FAIL")) {
				diagLine := fmt.Sprintf("   └─► Diagnostic: %s", w.Diagnostic)
				fmt.Fprintf(&buf, "│ %-118s │\n", truncate(diagLine, 118))
			}
		}
	}

	// Split-Screen Divider & Bottom Rolling Event Ticker
	buf.WriteString("├──────┴────────┴──────────────────────────┴────────────────┴─────────┴────────────┴────────────┴────────────────────────────┤\n")
	buf.WriteString("│ LIVE SWARM EVENT TICKER (Rolling Recent Events)                                                                        │\n")
	buf.WriteString("├────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤\n")

	// Fixed-height 5-line event ticker
	startIdx := 0
	if len(hud.events) > 5 {
		startIdx = len(hud.events) - 5
	}
	recentEvents := hud.events[startIdx:]

	for i := 0; i < 5; i++ {
		if i < len(recentEvents) {
			eventLine := truncate(recentEvents[i], 118)
			fmt.Fprintf(&buf, "│ %-118s │\n", eventLine)
		} else {
			if len(recentEvents) == 0 && i == 0 {
				fmt.Fprintf(&buf, "│ %-118s │\n", "(Awaiting swarm event stream...)")
			} else {
				fmt.Fprintf(&buf, "│ %-118s │\n", "")
			}
		}
	}

	buf.WriteString("└────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘\n")

	// Atomic flush of buffer to writer (Zero flicker)
	hud.writer.Write(buf.Bytes())
}

// RunRenderLoop executes continuous background rendering at the specified interval
// until ctx is cancelled.
func (hud *SwarmHUD) RunRenderLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial render
	hud.Render()

	for {
		select {
		case <-ctx.Done():
			hud.Render()
			return
		case <-ticker.C:
			hud.Render()
		}
	}
}

// formatLinearStatus normalizes a status code like "[ACTIVE]" into "Active".
func formatLinearStatus(st string) string {
	cleaned := strings.Trim(st, "[]")
	if len(cleaned) == 0 {
		return "Unknown"
	}
	if len(cleaned) == 1 {
		return strings.ToUpper(cleaned)
	}
	return strings.ToUpper(cleaned[:1]) + strings.ToLower(cleaned[1:])
}

// formatStatusBadge formats worker status with fixed width (10 chars) for ANSI table.
func formatStatusBadge(st string) string {
	if st == "" {
		st = "[QUEUED]"
	}
	if !strings.HasPrefix(st, "[") {
		st = "[" + st + "]"
	}

	// Fixed width 10 characters: e.g. "[ACTIVE]  "
	if len(st) > 10 {
		st = st[:10]
	}
	return fmt.Sprintf("%-10s", st)
}

// formatNumberWithCommas formats an integer with thousands comma separators.
func formatNumberWithCommas(n int) string {
	if n < 0 {
		return "-" + formatNumberWithCommas(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var res []byte
	rem := len(s) % 3
	if rem > 0 {
		res = append(res, s[:rem]...)
		if len(s) > rem {
			res = append(res, ',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		res = append(res, s[i:i+3]...)
		if i+3 < len(s) {
			res = append(res, ',')
		}
	}
	return string(res)
}

// truncate clamps a string to maxLen characters.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// truncatePath shortens a file path, preserving the filename tail if too long.
func truncatePath(p string, maxLen int) string {
	if len(p) <= maxLen {
		return p
	}
	if maxLen <= 3 {
		return p[:maxLen]
	}
	return "..." + p[len(p)-(maxLen-3):]
}
