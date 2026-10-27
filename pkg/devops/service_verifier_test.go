// SPDX-License-Identifier: Apache-2.0
package devops_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

func TestRunServiceVerifier_Minimal(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder_svc_verify_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a minimal go.mod and main.go
	goMod := `module testserver

go 1.22
`
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	mainGo := `package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8099"
	}
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"status\":\"ok\"}")
	})
	http.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > 1024*1024 {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.ListenAndServe(":"+port, nil)
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatalf("failed to write main.go: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opts := devops.ServiceVerifierOptions{
		WorkspaceDir:        tmpDir,
		Entrypoint:          ".",
		Port:                8199,
		Endpoints:           []string{"/healthz"},
		TestSecurityHeaders: true,
		TestRateLimit:       false,
		TestSizeLimit:       true,
		StartupTimeoutMs:    5000,
	}

	res, err := devops.RunServiceVerifier(ctx, opts)
	if err != nil {
		t.Fatalf("RunServiceVerifier failed: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success=true, got false (summary: %s, log: %s)", res.Summary, res.ServerLog)
	}
	if len(res.Endpoints) != 1 || res.Endpoints[0].StatusCode != 200 {
		t.Fatalf("unexpected endpoints result: %+v", res.Endpoints)
	}
	if res.SecurityHeaders["X-Content-Type-Options"] != "nosniff" {
		t.Errorf("expected X-Content-Type-Options header 'nosniff', got %q", res.SecurityHeaders["X-Content-Type-Options"])
	}
	if !res.SizeLimit.Triggered413 {
		t.Errorf("expected size limit to trigger 413, got status %d", res.SizeLimit.StatusCode)
	}
}
