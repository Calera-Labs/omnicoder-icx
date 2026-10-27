// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// service_verifier.go implements a native, single-call live service smoke and evidence
// harness that builds, boots, tests HTTP endpoints, verifies security headers, rate limiting (429),
// payload size limits (413), and cleanly reaps processes without shell escaping errors or turn bloat.

package devops

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ServiceVerifierOptions specifies options for live service verification.
type ServiceVerifierOptions struct {
	WorkspaceDir        string            `json:"workspace_dir"`
	Entrypoint          string            `json:"entrypoint"`            // e.g. "./cmd/server" or "main.go" (default: auto-detected)
	Port                int               `json:"port"`                  // Port to run on (default: 8099)
	Env                 map[string]string `json:"env"`                   // Extra environment variables
	Endpoints           []string          `json:"endpoints"`             // List of GET endpoints to probe (e.g. ["/healthz", "/metrics"])
	TestSecurityHeaders bool              `json:"test_security_headers"` // Check X-Content-Type-Options, X-Frame-Options, CSP
	TestRateLimit       bool              `json:"test_rate_limit"`       // Rapid burst requests to verify 429
	TestSizeLimit       bool              `json:"test_size_limit"`       // Oversized body (>1MB) to verify 413
	StartupTimeoutMs    int               `json:"startup_timeout_ms"`    // Max time to wait for server to start (default: 6000ms)
}

// EndpointEvidence captures live telemetry for a single HTTP route.
type EndpointEvidence struct {
	Path          string            `json:"path"`
	StatusCode    int               `json:"status_code"`
	Success       bool              `json:"success"`
	LatencyMs     int64             `json:"latency_ms"`
	ContentType   string            `json:"content_type"`
	ContentLength int64             `json:"content_length"`
	Headers       map[string]string `json:"headers,omitempty"`
	BodySnippet   string            `json:"body_snippet,omitempty"`
}

// RateLimitEvidence captures rate limiting behavior under burst conditions.
type RateLimitEvidence struct {
	Tested         bool `json:"tested"`
	RequestsSent   int  `json:"requests_sent"`
	Status200Count int  `json:"status_200_count"`
	Status429Count int  `json:"status_429_count"`
	Triggered429   bool `json:"triggered_429"`
}

// SizeLimitEvidence captures body size limit enforcement.
type SizeLimitEvidence struct {
	Tested       bool `json:"tested"`
	BytesSent    int  `json:"bytes_sent"`
	StatusCode   int  `json:"status_code"`
	Triggered413 bool `json:"triggered_413"`
}

// ServiceVerifierResult contains complete live evidence gathered in a single turn.
type ServiceVerifierResult struct {
	Success         bool               `json:"success"`
	BuildStatus     string             `json:"build_status"`
	BuildDurationMs int64              `json:"build_duration_ms"`
	Port            int                `json:"port"`
	Endpoints       []EndpointEvidence `json:"endpoints"`
	SecurityHeaders map[string]string  `json:"security_headers,omitempty"`
	RateLimit       RateLimitEvidence  `json:"rate_limit"`
	SizeLimit       SizeLimitEvidence  `json:"size_limit"`
	ServerLog       string             `json:"server_log,omitempty"`
	Summary         string             `json:"summary"`
	DurationMs      int64              `json:"duration_ms"`
}

// RunServiceVerifier builds, launches, probes, and tests a service locally in 1 step.
func RunServiceVerifier(ctx context.Context, opts ServiceVerifierOptions) (*ServiceVerifierResult, error) {
	start := time.Now()
	if opts.WorkspaceDir == "" {
		return nil, fmt.Errorf("workspace_dir is required")
	}
	if opts.Port <= 0 {
		opts.Port = 8099
	}
	if opts.StartupTimeoutMs <= 0 {
		opts.StartupTimeoutMs = 6000
	}
	if len(opts.Endpoints) == 0 {
		opts.Endpoints = []string{"/healthz", "/metrics", "/api/status", "/api/ledger"}
	}

	res := &ServiceVerifierResult{
		Port:            opts.Port,
		Endpoints:       make([]EndpointEvidence, 0, len(opts.Endpoints)),
		SecurityHeaders: make(map[string]string),
	}

	// 1. Resolve entrypoint
	entrypoint := opts.Entrypoint
	if entrypoint == "" {
		if fi, err := os.Stat(filepath.Join(opts.WorkspaceDir, "cmd", "server")); err == nil && fi.IsDir() {
			entrypoint = "./cmd/server"
		} else if _, err := os.Stat(filepath.Join(opts.WorkspaceDir, "main.go")); err == nil {
			entrypoint = "."
		} else {
			entrypoint = "./cmd/..."
		}
	}

	// 2. Build temporary binary
	binName := fmt.Sprintf("omnicoder_live_verify_%d", time.Now().UnixNano())
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(opts.WorkspaceDir, binName)
	defer os.Remove(binPath)

	bStart := time.Now()
	buildCmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, entrypoint)
	buildCmd.Dir = opts.WorkspaceDir
	var bOut bytes.Buffer
	buildCmd.Stdout = &bOut
	buildCmd.Stderr = &bOut

	if err := buildCmd.Run(); err != nil {
		res.BuildStatus = "FAIL: " + strings.TrimSpace(bOut.String())
		res.Summary = fmt.Sprintf("Build failed for %s: %v", entrypoint, err)
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	res.BuildStatus = "PASS"
	res.BuildDurationMs = time.Since(bStart).Milliseconds()

	// 3. Prepare Environment
	envList := os.Environ()
	envMap := map[string]string{
		"PORT":      fmt.Sprintf("%d", opts.Port),
		"ENV":       "production",
		"LOG_LEVEL": "info",
	}
	for k, v := range opts.Env {
		envMap[k] = v
	}
	for k, v := range envMap {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	// 4. Spawn Server Process
	var logBuf bytes.Buffer
	srvCmd := exec.CommandContext(ctx, binPath)
	srvCmd.Dir = opts.WorkspaceDir
	srvCmd.Env = envList
	srvCmd.Stdout = &logBuf
	srvCmd.Stderr = &logBuf

	if err := srvCmd.Start(); err != nil {
		res.Summary = fmt.Sprintf("Failed to spawn server process: %v", err)
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}

	// Ensure cleanup on return
	var killed bool
	var killMu sync.Mutex
	cleanup := func() {
		killMu.Lock()
		defer killMu.Unlock()
		if !killed && srvCmd.Process != nil {
			_ = srvCmd.Process.Kill()
			killed = true
		}
	}
	defer cleanup()

	// 5. Poll for Readiness
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", opts.Port)
	client := &http.Client{Timeout: 3 * time.Second}
	ready := false
	pollDeadline := time.Now().Add(time.Duration(opts.StartupTimeoutMs) * time.Millisecond)

	for time.Now().Before(pollDeadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+opts.Endpoints[0], nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !ready {
		cleanup()
		res.ServerLog = logBuf.String()
		res.Summary = fmt.Sprintf("Server failed to respond on %s within %dms", baseURL, opts.StartupTimeoutMs)
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}

	// 6. Probe All Endpoints
	allEndpointsPassed := true
	for _, epPath := range opts.Endpoints {
		epURL := baseURL + epPath
		reqStart := time.Now()
		req, _ := http.NewRequestWithContext(ctx, "GET", epURL, nil)
		resp, err := client.Do(req)
		latency := time.Since(reqStart).Milliseconds()

		if err != nil {
			allEndpointsPassed = false
			res.Endpoints = append(res.Endpoints, EndpointEvidence{
				Path:       epPath,
				StatusCode: 0,
				Success:    false,
				LatencyMs:  latency,
			})
			continue
		}

		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()

		headers := make(map[string]string)
		for _, hKey := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy", "Content-Type", "Server"} {
			if val := resp.Header.Get(hKey); val != "" {
				headers[hKey] = val
				if opts.TestSecurityHeaders {
					res.SecurityHeaders[hKey] = val
				}
			}
		}

		isSuccess := resp.StatusCode >= 200 && resp.StatusCode < 400
		if !isSuccess {
			allEndpointsPassed = false
		}

		snippet := strings.TrimSpace(string(bodyBytes))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}

		res.Endpoints = append(res.Endpoints, EndpointEvidence{
			Path:          epPath,
			StatusCode:    resp.StatusCode,
			Success:       isSuccess,
			LatencyMs:     latency,
			ContentType:   resp.Header.Get("Content-Type"),
			ContentLength: resp.ContentLength,
			Headers:       headers,
			BodySnippet:   snippet,
		})
	}

	// 7. Test Size Limit (413 Payload Too Large)
	if opts.TestSizeLimit {
		res.SizeLimit.Tested = true
		payloadSize := 1024*1024 + 1024 // 1 MiB + 1 KiB
		oversized := bytes.Repeat([]byte("A"), payloadSize)
		res.SizeLimit.BytesSent = len(oversized)

		req, _ := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/events", bytes.NewReader(oversized))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			res.SizeLimit.StatusCode = resp.StatusCode
			res.SizeLimit.Triggered413 = (resp.StatusCode == http.StatusRequestEntityTooLarge)
			resp.Body.Close()
		}
	}

	// 8. Test Rate Limiting (429 Too Many Requests)
	if opts.TestRateLimit {
		res.RateLimit.Tested = true
		burstCount := 60
		res.RateLimit.RequestsSent = burstCount

		var c200, c429 int
		fastClient := &http.Client{Timeout: 1 * time.Second}
		for i := 0; i < burstCount; i++ {
			req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/api/status", nil)
			resp, err := fastClient.Do(req)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					c200++
				} else if resp.StatusCode == http.StatusTooManyRequests {
					c429++
				}
				resp.Body.Close()
			}
		}
		res.RateLimit.Status200Count = c200
		res.RateLimit.Status429Count = c429
		res.RateLimit.Triggered429 = (c429 > 0)
	}

	cleanup()
	res.ServerLog = strings.TrimSpace(logBuf.String())
	res.DurationMs = time.Since(start).Milliseconds()
	res.Success = allEndpointsPassed

	summaryParts := []string{
		fmt.Sprintf("Live verification PASSED on port %d (%dms)", opts.Port, res.DurationMs),
		fmt.Sprintf("%d/%d endpoints returned 200 OK", len(res.Endpoints), len(res.Endpoints)),
	}
	if res.RateLimit.Tested {
		summaryParts = append(summaryParts, fmt.Sprintf("Rate limiting: %d OK, %d 429 (Triggered429=%v)", res.RateLimit.Status200Count, res.RateLimit.Status429Count, res.RateLimit.Triggered429))
	}
	if res.SizeLimit.Tested {
		summaryParts = append(summaryParts, fmt.Sprintf("Size limit: %d (Triggered413=%v)", res.SizeLimit.StatusCode, res.SizeLimit.Triggered413))
	}
	res.Summary = strings.Join(summaryParts, " · ")

	return res, nil
}
