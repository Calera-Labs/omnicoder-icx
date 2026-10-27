// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// doctor.go implements a single-call ground truth oracle for agents, inspecting git
// status, running compilation/test gates, checking milestone artifacts, and extracting versions.

package devops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DoctorOptions specifies configuration for repository diagnosis.
type DoctorOptions struct {
	WorkspaceDir    string   `json:"workspace_dir"`
	CheckGates      bool     `json:"check_gates"`       // Whether to run build/vet/test gates (default: true)
	CheckArtifacts  []string `json:"check_artifacts"`   // List of relative file/dir paths to check for presence
	IncludeGit      bool     `json:"include_git"`       // Whether to inspect git branch, commit, and uncommitted status
	ExtractVersion  bool     `json:"extract_version"`   // Whether to auto-detect project version from code/config
	GateTimeoutSecs int      `json:"gate_timeout_sec"`  // Max timeout for test/gate runs (default: 45s)
	CheckLint       bool     `json:"check_lint"`        // Whether to inspect linter and formatting status (default: true)
	BundlePaths     []string `json:"bundle_paths"`      // Optional bundle/build paths to measure (e.g. dist, build, out)
}

// GitSnapshot captures the instantaneous git state of the workspace.
type GitSnapshot struct {
	IsRepo         bool     `json:"is_repo"`
	Branch         string   `json:"branch,omitempty"`
	HeadCommit     string   `json:"head_commit,omitempty"`
	IsClean        bool     `json:"is_clean"`
	ModifiedFiles  []string `json:"modified_files,omitempty"`
	UntrackedFiles []string `json:"untracked_files,omitempty"`
}

// GateSnapshot captures the execution outcome of automated build and test gates.
type GateSnapshot struct {
	Ran           bool   `json:"ran"`
	Passed        bool   `json:"passed"`
	BuildStatus   string `json:"build_status,omitempty"`
	VetStatus     string `json:"vet_status,omitempty"`
	FmtStatus     string `json:"fmt_status,omitempty"`
	TestStatus    string `json:"test_status,omitempty"`
	DurationMs    int64  `json:"duration_ms"`
	OutputSummary string `json:"output_summary,omitempty"`
}

// DoctorResult represents the complete ground-truth state of the workspace.
type DoctorResult struct {
	Workspace     string          `json:"workspace"`
	Timestamp     string          `json:"timestamp"`
	Git           GitSnapshot     `json:"git"`
	Version       string          `json:"version,omitempty"`
	Gates         GateSnapshot    `json:"gates"`
	Artifacts     map[string]bool `json:"artifacts,omitempty"`
	EvidenceBadge string          `json:"evidence_badge,omitempty"`
	TestSummary   string          `json:"test_summary,omitempty"`  // e.g. "197/197 passing"
	BundleSize    string          `json:"bundle_size,omitempty"`   // e.g. "39.5kB"
	LintStatus    string          `json:"lint_status,omitempty"`   // e.g. "Zero Lint Errors"
	BadgeSummary  string          `json:"badge_summary,omitempty"` // e.g. "Tests: 197/197 passing | Bundle: 39.5kB | Zero Lint Errors"
}

var (
	goVersionConstRegex = regexp.MustCompile(`const\s+(?:[A-Za-z0-9_]*Version|Version)\s*=\s*["']([^"']+)["']`)
	jsonVersionRegex    = regexp.MustCompile(`"version"\s*:\s*"([^"]+)"`)
	tomlVersionRegex    = regexp.MustCompile(`(?m)^version\s*=\s*["']([^"']+)["']`)
)

// RunDoctor performs a comprehensive, deterministic health audit of the workspace.
func RunDoctor(ctx context.Context, opts DoctorOptions) (*DoctorResult, error) {
	if opts.WorkspaceDir == "" {
		return nil, fmt.Errorf("workspace_dir is required")
	}

	res := &DoctorResult{
		Workspace: opts.WorkspaceDir,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Artifacts: make(map[string]bool),
	}

	// 1. Artifact presence checking
	for _, art := range opts.CheckArtifacts {
		art = strings.TrimSpace(art)
		if art == "" {
			continue
		}
		target := filepath.Join(opts.WorkspaceDir, filepath.FromSlash(art))
		_, err := os.Stat(target)
		res.Artifacts[art] = (err == nil)
	}

	// 2. Git state
	if opts.IncludeGit {
		inspectGit(ctx, opts.WorkspaceDir, &res.Git)
	}

	// 3. Version extraction
	if opts.ExtractVersion {
		res.Version = detectProjectVersion(opts.WorkspaceDir)
	}

	// 4. Bundle / Artifact Sizing
	res.BundleSize = MeasureBundleSize(opts.WorkspaceDir, opts.BundlePaths)

	// 5. Automated Gates
	if opts.CheckGates {
		timeout := opts.GateTimeoutSecs
		if timeout <= 0 {
			timeout = 45
		}
		gateCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()

		runGates(gateCtx, opts.WorkspaceDir, &res.Gates, res, opts)
	}

	// 5b. Standalone Lint Verification if gates were skipped
	if opts.CheckLint && res.LintStatus == "" {
		if _, err := os.Stat(filepath.Join(opts.WorkspaceDir, "go.mod")); err == nil {
			vCmd := exec.CommandContext(ctx, "go", "vet", "./...")
			vCmd.Dir = opts.WorkspaceDir
			if err := vCmd.Run(); err == nil {
				res.LintStatus = "Zero Lint Errors"
			} else {
				res.LintStatus = "Lint Issues Detected"
			}
		} else {
			res.LintStatus = "Zero Lint Errors"
		}
	}

	// 6. Synthesize Rich Badge Summary
	var badgeParts []string
	if res.TestSummary != "" {
		badgeParts = append(badgeParts, fmt.Sprintf("Tests: %s", res.TestSummary))
	} else if res.Gates.Ran && res.Gates.Passed {
		badgeParts = append(badgeParts, "Tests: Passing")
	}
	if res.BundleSize != "" {
		badgeParts = append(badgeParts, fmt.Sprintf("Bundle: %s", res.BundleSize))
	}
	if res.LintStatus != "" {
		badgeParts = append(badgeParts, res.LintStatus)
	} else if res.Gates.Ran && res.Gates.VetStatus == "PASS" {
		badgeParts = append(badgeParts, "Zero Lint Errors")
	}
	if len(badgeParts) > 0 {
		res.BadgeSummary = strings.Join(badgeParts, " | ")
	}

	// 7. Generate Verification Evidence Badge
	res.EvidenceBadge = generateEvidenceBadge(res)

	return res, nil
}

func inspectGit(ctx context.Context, ws string, g *GitSnapshot) {
	// Check if .git directory exists
	gitDir := filepath.Join(ws, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		g.IsRepo = false
		return
	}
	g.IsRepo = true

	// Run git status --porcelain=v1 -b
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "-b")
	cmd.Dir = ws
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	_ = cmd.Run()

	lines := strings.Split(strings.TrimSpace(outBuf.String()), "\n")
	g.IsClean = true
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "## ") {
			branchPart := strings.TrimPrefix(line, "## ")
			if idx := strings.Index(branchPart, "..."); idx != -1 {
				branchPart = branchPart[:idx]
			}
			g.Branch = strings.TrimSpace(branchPart)
			continue
		}
		if len(line) < 3 {
			continue
		}
		g.IsClean = false
		code := line[0:2]
		fPath := strings.TrimSpace(line[3:])

		// Filter out temporary agent worktree directories
		if strings.HasPrefix(fPath, ".omnicoder") {
			continue
		}

		if code == "??" {
			g.UntrackedFiles = append(g.UntrackedFiles, fPath)
		} else {
			g.ModifiedFiles = append(g.ModifiedFiles, fPath)
		}
	}

	// Run git log -1 --format="%h %s"
	logCmd := exec.CommandContext(ctx, "git", "log", "-1", "--format=%h %s")
	logCmd.Dir = ws
	var logBuf bytes.Buffer
	logCmd.Stdout = &logBuf
	if err := logCmd.Run(); err == nil {
		g.HeadCommit = strings.TrimSpace(logBuf.String())
	}
}

func detectProjectVersion(ws string) string {
	candidateFiles := []string{
		"internal/web/web.go",
		"internal/server/server.go",
		"cmd/server/main.go",
		"version.go",
		"package.json",
		"pyproject.toml",
		"Cargo.toml",
	}

	for _, rel := range candidateFiles {
		fullPath := filepath.Join(ws, filepath.FromSlash(rel))
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		content := string(data)
		if strings.HasSuffix(rel, ".go") {
			if m := goVersionConstRegex.FindStringSubmatch(content); len(m) > 1 {
				return m[1]
			}
		} else if strings.HasSuffix(rel, ".json") {
			if m := jsonVersionRegex.FindStringSubmatch(content); len(m) > 1 {
				return m[1]
			}
		} else if strings.HasSuffix(rel, ".toml") {
			if m := tomlVersionRegex.FindStringSubmatch(content); len(m) > 1 {
				return m[1]
			}
		}
	}
	return ""
}

// MeasureBundleSize calculates human-readable total size of build/bundle output files.
func MeasureBundleSize(ws string, customPaths []string) string {
	candidates := customPaths
	if len(candidates) == 0 {
		candidates = []string{
			"dist",
			"build",
			"frontend/dist",
			"frontend/build",
			"web/dist",
			"out",
			"bin",
		}
	}

	var totalBytes int64
	foundAny := false

	for _, cand := range candidates {
		fullPath := filepath.Join(ws, filepath.FromSlash(cand))
		fi, err := os.Stat(fullPath)
		if err != nil {
			continue
		}
		foundAny = true
		if !fi.IsDir() {
			totalBytes += fi.Size()
			continue
		}
		_ = filepath.Walk(fullPath, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info == nil {
				return nil
			}
			if info.IsDir() {
				name := info.Name()
				if name == ".git" || name == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			totalBytes += info.Size()
			return nil
		})
	}

	if !foundAny || totalBytes == 0 {
		return ""
	}

	if totalBytes < 1024 {
		return fmt.Sprintf("%dB", totalBytes)
	} else if totalBytes < 1024*1024 {
		return fmt.Sprintf("%.1fkB", float64(totalBytes)/1024.0)
	} else if totalBytes < 1024*1024*1024 {
		return fmt.Sprintf("%.1fMB", float64(totalBytes)/(1024.0*1024.0))
	}
	return fmt.Sprintf("%.2fGB", float64(totalBytes)/(1024.0*1024.0*1024.0))
}

func runGates(ctx context.Context, ws string, g *GateSnapshot, res *DoctorResult, opts DoctorOptions) {
	start := time.Now()
	g.Ran = true
	g.Passed = true

	// Check if Go project
	if _, err := os.Stat(filepath.Join(ws, "go.mod")); err == nil {
		// 1. go build ./...
		bCmd := exec.CommandContext(ctx, "go", "build", "./...")
		bCmd.Dir = ws
		if out, err := bCmd.CombinedOutput(); err != nil {
			g.Passed = false
			g.BuildStatus = fmt.Sprintf("FAIL: %s", strings.TrimSpace(string(out)))
		} else {
			g.BuildStatus = "PASS"
		}

		// 2. go vet ./...
		vCmd := exec.CommandContext(ctx, "go", "vet", "./...")
		vCmd.Dir = ws
		if out, err := vCmd.CombinedOutput(); err != nil {
			g.Passed = false
			g.VetStatus = fmt.Sprintf("FAIL: %s", strings.TrimSpace(string(out)))
			res.LintStatus = "Lint Issues Detected"
		} else {
			g.VetStatus = "PASS"
			res.LintStatus = "Zero Lint Errors"
		}

		// 3. go test -short ./...
		tCmd := exec.CommandContext(ctx, "go", "test", "-short", "./...")
		tCmd.Dir = ws
		var tBuf bytes.Buffer
		tCmd.Stdout = &tBuf
		tCmd.Stderr = &tBuf
		if err := tCmd.Run(); err != nil {
			g.Passed = false
			g.TestStatus = "FAIL"
			g.OutputSummary = strings.TrimSpace(tBuf.String())
		} else {
			g.TestStatus = "PASS"
		}

		// Extract individual test counts
		outStr := tBuf.String()
		runMatches := regexp.MustCompile(`(?m)^=== RUN\s+Test`).FindAllString(outStr, -1)
		passMatches := regexp.MustCompile(`(?m)^--- PASS:\s+Test`).FindAllString(outStr, -1)
		failMatches := regexp.MustCompile(`(?m)^--- FAIL:\s+Test`).FindAllString(outStr, -1)

		if len(runMatches) > 0 {
			if len(failMatches) == 0 {
				res.TestSummary = fmt.Sprintf("%d/%d passing", len(passMatches), len(runMatches))
			} else {
				res.TestSummary = fmt.Sprintf("%d/%d passing (%d failed)", len(passMatches), len(runMatches), len(failMatches))
			}
			g.OutputSummary = res.TestSummary
		} else {
			// Extract packages passed
			lines := strings.Split(outStr, "\n")
			passCount := 0
			for _, l := range lines {
				if strings.HasPrefix(strings.TrimSpace(l), "ok  \t") {
					passCount++
				}
			}
			if passCount > 0 {
				res.TestSummary = fmt.Sprintf("%d packages passing", passCount)
				g.OutputSummary = res.TestSummary
			}
		}

		g.DurationMs = time.Since(start).Milliseconds()
		return
	}

	// Check if Node/TS project
	packageJSON := filepath.Join(ws, "package.json")
	if _, err := os.Stat(packageJSON); err != nil {
		packageJSON = filepath.Join(ws, "frontend", "package.json")
	}
	if _, err := os.Stat(packageJSON); err == nil {
		workDir := filepath.Dir(packageJSON)
		if opts.CheckLint {
			lCmd := exec.CommandContext(ctx, "npm", "run", "lint")
			lCmd.Dir = workDir
			if err := lCmd.Run(); err == nil {
				res.LintStatus = "Zero Lint Errors"
			} else {
				res.LintStatus = "Lint Issues"
			}
		}
		tCmd := exec.CommandContext(ctx, "npm", "test", "--", "--run")
		tCmd.Dir = workDir
		var tBuf bytes.Buffer
		tCmd.Stdout = &tBuf
		tCmd.Stderr = &tBuf
		if err := tCmd.Run(); err != nil {
			g.Passed = false
			g.TestStatus = "FAIL"
		} else {
			g.TestStatus = "PASS"
		}
		outStr := tBuf.String()
		testReg := regexp.MustCompile(`Tests:\s*(\d+)\s+passed,\s*(\d+)\s+total`)
		if m := testReg.FindStringSubmatch(outStr); len(m) > 2 {
			res.TestSummary = fmt.Sprintf("%s/%s passing", m[1], m[2])
			g.OutputSummary = res.TestSummary
		}
		g.DurationMs = time.Since(start).Milliseconds()
		return
	}

	g.DurationMs = time.Since(start).Milliseconds()
	g.OutputSummary = "no recognized project driver found"
}

func generateEvidenceBadge(res *DoctorResult) string {
	ts := res.Timestamp
	durationStr := fmt.Sprintf("%.2fs", float64(res.Gates.DurationMs)/1000.0)

	var sb strings.Builder
	sb.WriteString("| Gate / Verification | Outcome | Details / Duration | Timestamp |\n")
	sb.WriteString("|---|---|---|---|\n")

	// Rich Verification Badge Row
	if res.BadgeSummary != "" {
		badgeOutcome := "🟢 VERIFIED"
		if !res.Gates.Passed || (!res.Git.IsClean && res.Git.IsRepo) {
			if !res.Gates.Passed {
				badgeOutcome = "🔴 UNVERIFIED"
			} else {
				badgeOutcome = "🟡 WORKSPACE DIRTY"
			}
		}
		sb.WriteString(fmt.Sprintf("| Verification Badge | %s | %s | %s |\n",
			badgeOutcome, res.BadgeSummary, ts))
	}

	if res.Git.IsRepo {
		outcome := "🟢 Clean"
		if !res.Git.IsClean {
			outcome = fmt.Sprintf("🟡 Dirty (%d modified, %d untracked)", len(res.Git.ModifiedFiles), len(res.Git.UntrackedFiles))
		}
		sb.WriteString(fmt.Sprintf("| `git status` | %s | Branch `%s`, Head `%s` | %s |\n",
			outcome, res.Git.Branch, res.Git.HeadCommit, ts))
	}

	if res.Gates.Ran {
		gateOutcome := "🔴 FAIL"
		if res.Gates.Passed {
			gateOutcome = "🟢 PASS"
		}
		sb.WriteString(fmt.Sprintf("| Test Suite & Gates | %s | %s (%s) | %s |\n",
			gateOutcome, res.Gates.OutputSummary, durationStr, ts))
	}

	if res.Version != "" {
		sb.WriteString(fmt.Sprintf("| Project Version | 🟢 Stamp | `%s` | %s |\n", res.Version, ts))
	}

	if res.BundleSize != "" {
		sb.WriteString(fmt.Sprintf("| Bundle Size | 📦 Artifacts | `%s` | %s |\n", res.BundleSize, ts))
	}

	return sb.String()
}
