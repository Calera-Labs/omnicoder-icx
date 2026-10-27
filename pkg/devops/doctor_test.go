// SPDX-License-Identifier: Apache-2.0
package devops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDoctor_ArtifactsAndVersion(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test file with version
	webDir := filepath.Join(tmpDir, "internal", "web")
	if err := os.MkdirAll(webDir, 0755); err != nil {
		t.Fatal(err)
	}
	webFile := filepath.Join(webDir, "web.go")
	if err := os.WriteFile(webFile, []byte("package web\n\nconst Version = \"0.3.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a dummy artifact
	docFile := filepath.Join(tmpDir, "Dockerfile")
	if err := os.WriteFile(docFile, []byte("FROM alpine\n"), 0644); err != nil {
		t.Fatal(err)
	}

	opts := DoctorOptions{
		WorkspaceDir:   tmpDir,
		CheckGates:     false,
		CheckArtifacts: []string{"Dockerfile", "scripts/check.ps1"},
		IncludeGit:     false,
		ExtractVersion: true,
	}

	res, err := RunDoctor(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunDoctor failed: %v", err)
	}

	if res.Version != "0.3.0" {
		t.Errorf("expected version 0.3.0, got %q", res.Version)
	}
	if !res.Artifacts["Dockerfile"] {
		t.Errorf("expected Dockerfile to exist")
	}
	if res.Artifacts["scripts/check.ps1"] {
		t.Errorf("expected scripts/check.ps1 to NOT exist")
	}
}

func TestMeasureBundleSize(t *testing.T) {
	tmpDir := t.TempDir()
	distDir := filepath.Join(tmpDir, "dist")
	_ = os.MkdirAll(distDir, 0755)

	// Create 40KB of dummy bundle files
	bundleFile := filepath.Join(distDir, "bundle.js")
	payload := make([]byte, 40*1024)
	_ = os.WriteFile(bundleFile, payload, 0644)

	sizeStr := MeasureBundleSize(tmpDir, nil)
	if sizeStr != "40.0kB" {
		t.Errorf("expected bundle size 40.0kB, got %q", sizeStr)
	}
}

func TestRunDoctor_RichDiagnosticBadges(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a dummy dist directory
	distDir := filepath.Join(tmpDir, "dist")
	_ = os.MkdirAll(distDir, 0755)
	_ = os.WriteFile(filepath.Join(distDir, "main.js"), make([]byte, 39500), 0644)

	opts := DoctorOptions{
		WorkspaceDir: tmpDir,
		CheckGates:   false,
		BundlePaths:  []string{"dist"},
	}

	res, err := RunDoctor(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunDoctor failed: %v", err)
	}

	if res.BundleSize == "" {
		t.Errorf("expected BundleSize to be measured, got empty")
	}

	if res.BadgeSummary == "" {
		t.Errorf("expected BadgeSummary to be populated, got empty")
	}

	if !strings.Contains(res.EvidenceBadge, "Verification Badge") {
		t.Errorf("expected EvidenceBadge to contain Verification Badge row, got:\n%s", res.EvidenceBadge)
	}
}

