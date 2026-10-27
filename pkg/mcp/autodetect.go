// SPDX-License-Identifier: Apache-2.0
// Package mcp implements the Model Context Protocol (MCP) JSON-RPC 2.0 stdio server,
// tool registry, and sandboxed filesystem/git tools for OmniCoder-ICX.

package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// BackendType identifies the detected LLM inference or execution provider.
type BackendType string

const (
	BackendCloudGemini       BackendType = "cloud-gemini"
	BackendCloudOpenAI       BackendType = "cloud-openai"
	BackendCloudAnthropic    BackendType = "cloud-anthropic"
	BackendCaleraVault       BackendType = "calera-vault"
	BackendLocalOllama       BackendType = "local-ollama"
	BackendLocalLMStudio     BackendType = "local-lmstudio"
	BackendLocalVLLM         BackendType = "local-vllm"
	BackendLocalAI           BackendType = "local-localai"
	BackendOffline           BackendType = "offline-local-cache"
	BackendOfflineMechanical BackendType = "offline-mechanical" // alias for backwards compatibility
	BackendOfflineLocal         BackendType = "offline-local-cache" // canonical local cache offline mode
)

// DetectedConfig encapsulates auto-discovered workspace and LLM settings.
type DetectedConfig struct {
	WorkspaceDir string      `json:"workspace_dir"`
	Backend      BackendType `json:"backend"`
	Model        string      `json:"model"`
	BaseURL      string      `json:"base_url,omitempty"`
	APIKey       string      `json:"api_key,omitempty"` // Internal use, masked on serialization
	IsOffline    bool        `json:"is_offline"`
	HasGit       bool        `json:"has_git"`
	GitRoot      string      `json:"git_root,omitempty"`
	ActiveModel  string      `json:"active_model,omitempty"`
	ProbedAt     time.Time   `json:"probed_at"`
}

// MaskedCopy returns a copy suitable for public telemetry or MCP status output.
func (d DetectedConfig) MaskedCopy() DetectedConfig {
	cpy := d
	if len(cpy.APIKey) > 8 {
		cpy.APIKey = cpy.APIKey[:4] + "..." + cpy.APIKey[len(cpy.APIKey)-4:]
	} else if len(cpy.APIKey) > 0 {
		cpy.APIKey = "***"
	}
	return cpy
}

// AutoDetectOptions enables overriding endpoints, client, or timeout for probing.
type AutoDetectOptions struct {
	BaseDir      string
	HTTPClient   *http.Client
	OllamaURL    string
	LMStudioURL  string
	VLLMURL      string
	LocalAIURL   string
	ProbeTimeout time.Duration
	EnvFile      string // Optional override path for ~/.calera/icx.env
}

// AutoDetectEnvironment runs zero-config discovery for workspace root and LLM backends.
// It is guaranteed to return a valid configuration without throwing errors or halting.
func AutoDetectEnvironment(baseDir string) DetectedConfig {
	return AutoDetectEnvironmentWithOptions(AutoDetectOptions{
		BaseDir: baseDir,
	})
}

// AutoDetectEnvironmentWithOptions executes environment discovery with customized options.
func AutoDetectEnvironmentWithOptions(opts AutoDetectOptions) DetectedConfig {
	cfg := DetectedConfig{
		ProbedAt: time.Now().UTC(),
	}

	// 1. Resolve Workspace Root and Git Boundary
	cfg.WorkspaceDir = resolveWorkspaceRoot(opts.BaseDir)
	gitRoot, hasGit := findGitRoot(cfg.WorkspaceDir)
	cfg.HasGit = hasGit
	cfg.GitRoot = gitRoot

	// 2. Discover LLM Engine (Cloud -> Local Active Model -> Offline Embedded Local Cache)
	discoverBrainWithOptions(&cfg, opts)

	return cfg
}

// IsIDEOsOrSystemDir checks if a directory path corresponds to an IDE installation
// directory (e.g. VS Code, Cursor, Windsurf), system root, or OS binary directory
// that must NEVER be used as a developer workspace root.
func IsIDEOsOrSystemDir(dir string) bool {
	if dir == "" {
		return true
	}
	clean := filepath.Clean(dir)
	cleanLower := strings.ToLower(clean)

	// Check if path is root of a volume (e.g. "C:\" or "/" or "\\")
	vol := filepath.VolumeName(clean)
	if clean == vol || clean == vol+string(filepath.Separator) || clean == "/" || clean == "\\" {
		return true
	}

	// Windows system directories
	if strings.Contains(cleanLower, `\windows\system32`) || cleanLower == `c:\windows` || cleanLower == `c:\windows\system32` {
		return true
	}
	if cleanLower == `c:\program files` || cleanLower == `c:\program files (x86)` {
		return true
	}

	// Temp directory itself (standalone)
	tempDir := strings.ToLower(filepath.Clean(os.TempDir()))
	if cleanLower == tempDir {
		return true
	}

	// Bare user home directory itself must never be scanned as a repo root
	if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		homeLower := strings.ToLower(filepath.Clean(homeDir))
		if cleanLower == homeLower || cleanLower == filepath.Dir(homeLower) {
			return true
		}
	}

	// AppData root folders (not inside a project)
	appData := strings.ToLower(filepath.Clean(os.Getenv("APPDATA")))
	localAppData := strings.ToLower(filepath.Clean(os.Getenv("LOCALAPPDATA")))
	if appData != "" && (cleanLower == appData || cleanLower == filepath.Join(appData, "programs")) {
		return true
	}
	if localAppData != "" && (cleanLower == localAppData || cleanLower == filepath.Join(localAppData, "programs")) {
		return true
	}

	// VS Code IDE install directory (e.g. AppData\Local\Programs\Microsoft VS Code)
	if strings.Contains(cleanLower, "microsoft vs code") || strings.Contains(cleanLower, "programs\\microsoft vs code") || strings.Contains(cleanLower, "programs/microsoft vs code") {
		return true
	}

	// Cursor IDE install directory (e.g. AppData\Local\Programs\cursor)
	if strings.Contains(cleanLower, `programs\cursor`) || strings.Contains(cleanLower, `programs/cursor`) {
		return true
	}

	// Windsurf IDE install directory
	if strings.Contains(cleanLower, `programs\windsurf`) || strings.Contains(cleanLower, `programs/windsurf`) {
		return true
	}

	// Check if directory contains Code.exe, Cursor.exe, or Windsurf.exe directly
	for _, exe := range []string{"Code.exe", "code.exe", "Cursor.exe", "cursor.exe", "Windsurf.exe"} {
		if fi, err := os.Stat(filepath.Join(clean, exe)); err == nil && !fi.IsDir() {
			return true
		}
	}

	return false
}

// DiscoverIDEActiveWorkspace inspects editor state (VS Code, Cursor, Windsurf) and
// environment variables to locate the developer's actively opened workspace folder.
func DiscoverIDEActiveWorkspace() string {
	// 1. Check environment variables first
	envVars := []string{
		"OMNICODER_WORKSPACE",
		"VSCODE_WORKSPACE_FOLDER",
		"WORKSPACE_FOLDER",
		"PROJECT_DIR",
		"CLINE_WORKSPACE_ROOT",
		"ROO_WORKSPACE_ROOT",
		"WORKSPACE_ROOT",
	}
	for _, ev := range envVars {
		if val := strings.TrimSpace(os.Getenv(ev)); val != "" {
			if abs, err := filepath.Abs(val); err == nil {
				if fi, sErr := os.Stat(abs); sErr == nil && fi.IsDir() && !IsIDEOsOrSystemDir(abs) {
					return abs
				}
			}
		}
	}

	// 2. Search VS Code / Cursor / Windsurf workspaceStorage directories
	var storageRoots []string

	appData := os.Getenv("APPDATA")
	if appData != "" {
		storageRoots = append(storageRoots,
			filepath.Join(appData, "Code", "User", "workspaceStorage"),
			filepath.Join(appData, "Code - Insiders", "User", "workspaceStorage"),
			filepath.Join(appData, "Cursor", "User", "workspaceStorage"),
			filepath.Join(appData, "Windsurf", "User", "workspaceStorage"),
		)
	}

	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		// macOS
		storageRoots = append(storageRoots,
			filepath.Join(homeDir, "Library", "Application Support", "Code", "User", "workspaceStorage"),
			filepath.Join(homeDir, "Library", "Application Support", "Cursor", "User", "workspaceStorage"),
			filepath.Join(homeDir, "Library", "Application Support", "Windsurf", "User", "workspaceStorage"),
		)
		// Linux
		storageRoots = append(storageRoots,
			filepath.Join(homeDir, ".config", "Code", "User", "workspaceStorage"),
			filepath.Join(homeDir, ".config", "Cursor", "User", "workspaceStorage"),
			filepath.Join(homeDir, ".config", "Windsurf", "User", "workspaceStorage"),
		)
	}

	type candidateFolder struct {
		path    string
		modTime time.Time
	}
	var candidates []candidateFolder

	for _, sRoot := range storageRoots {
		entries, err := os.ReadDir(sRoot)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			wsFile := filepath.Join(sRoot, entry.Name(), "workspace.json")
			fi, err := os.Stat(wsFile)
			if err != nil {
				continue
			}
			data, err := os.ReadFile(wsFile)
			if err != nil {
				continue
			}
			var parsed struct {
				Folder    string `json:"folder"`
				Workspace string `json:"workspace"`
			}
			if err := json.Unmarshal(data, &parsed); err != nil {
				continue
			}
			targetURI := parsed.Folder
			if targetURI == "" {
				targetURI = parsed.Workspace
			}
			if targetURI == "" {
				continue
			}
			localPath, err := URItoPath(targetURI)
			if err != nil || localPath == "" {
				continue
			}
			if pFi, pErr := os.Stat(localPath); pErr == nil && pFi.IsDir() && !IsIDEOsOrSystemDir(localPath) {
				candidates = append(candidates, candidateFolder{
					path:    localPath,
					modTime: fi.ModTime(),
				})
			}
		}
	}

	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].modTime.After(candidates[j].modTime)
		})
		return candidates[0].path
	}

	return ""
}

// resolveWorkspaceRoot anchors to the project root by walking upward for project boundaries.
func resolveWorkspaceRoot(baseDir string) string {
	var startDir string
	if baseDir != "" && baseDir != "." {
		if abs, err := filepath.Abs(baseDir); err == nil {
			startDir = abs
		} else {
			startDir = baseDir
		}
	} else {
		if env := os.Getenv("OMNICODER_WORKSPACE"); env != "" {
			if abs, err := filepath.Abs(env); err == nil {
				return abs
			}
			return env
		}
		if cwd, err := os.Getwd(); err == nil {
			startDir = cwd
		} else {
			startDir = "."
		}
	}

	startDir = filepath.Clean(startDir)

	// Guard: If startDir is an IDE or System directory, NEVER anchor there!
	if IsIDEOsOrSystemDir(startDir) {
		if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
			return ideWS
		}
	}

	cur := startDir

	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		homeDir = filepath.Clean(homeDir)
	}
	tempDir := filepath.Clean(os.TempDir())

	markers := []string{
		".git",
		"go.mod",
		"package.json",
		"Cargo.toml",
		"pyproject.toml",
		"pom.xml",
		"build.gradle",
		"build.gradle.kts",
		"CMakeLists.txt",
		"Makefile",
		"requirements.txt",
		"composer.json",
		"deno.json",
		"pnpm-workspace.yaml",
		"lerna.json",
		"WORKSPACE",
	}

	for i := 0; i < 20; i++ {
		// Do not cross above system temp or user home directory if started beneath them
		if cur != startDir {
			if homeDir != "" && cur == homeDir {
				break
			}
			if cur == tempDir {
				break
			}
		}

		for _, m := range markers {
			target := filepath.Join(cur, m)
			if _, err := os.Stat(target); err == nil {
				if !IsIDEOsOrSystemDir(cur) {
					return cur
				}
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur || parent == "" {
			break
		}
		cur = parent
	}

	// Final check: if startDir itself was an IDE or system directory, try IDE workspace again
	if IsIDEOsOrSystemDir(startDir) {
		if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
			return ideWS
		}
	}

	return startDir
}

// findGitRoot checks if dir or any parent directory contains a .git repository.
func findGitRoot(dir string) (string, bool) {
	startDir := filepath.Clean(dir)
	cur := startDir

	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		homeDir = filepath.Clean(homeDir)
	}
	tempDir := filepath.Clean(os.TempDir())

	for i := 0; i < 20; i++ {
		if cur != startDir {
			if homeDir != "" && cur == homeDir {
				break
			}
			if cur == tempDir {
				break
			}
		}

		target := filepath.Join(cur, ".git")
		if _, err := os.Stat(target); err == nil {
			return cur, true
		}
		parent := filepath.Dir(cur)
		if parent == cur || parent == "" {
			break
		}
		cur = parent
	}
	return "", false
}

// discoverBrainWithOptions checks credentials and probes local daemons concurrently.
func discoverBrainWithOptions(cfg *DetectedConfig, opts AutoDetectOptions) {
	// If explicit cloud mode requested, probe cloud first
	forceCloud := false
	if env := strings.ToLower(strings.TrimSpace(os.Getenv("OMNICODER_CLOUD"))); env == "1" || env == "true" {
		forceCloud = true
	}
	if forceCloud {
		if probeCloudEnvVars(cfg, opts.EnvFile) {
			return
		}
	}

	// Step 1: Concurrent HTTP Probe for Local LLM Daemons (Normal Operation: Local-First)
	ollamaURL := opts.OllamaURL
	if ollamaURL == "" {
		if env := os.Getenv("OLLAMA_HOST"); env != "" {
			ollamaURL = normalizeURL(env, "11434")
		} else {
			ollamaURL = "http://127.0.0.1:11434"
		}
	}

	lmStudioURL := opts.LMStudioURL
	if lmStudioURL == "" {
		if env := os.Getenv("LMSTUDIO_BASE_URL"); env != "" {
			lmStudioURL = env
		}
	}

	vllmURL := opts.VLLMURL
	if vllmURL == "" {
		if env := os.Getenv("VLLM_BASE_URL"); env != "" {
			vllmURL = env
		} else {
			vllmURL = "http://127.0.0.1:8000"
		}
	}

	localaiURL := opts.LocalAIURL
	if localaiURL == "" {
		if env := os.Getenv("LOCALAI_BASE_URL"); env != "" {
			localaiURL = env
		} else {
			localaiURL = "http://127.0.0.1:8080"
		}
	}

	timeout := opts.ProbeTimeout
	if timeout <= 0 {
		timeout = 150 * time.Millisecond
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				DialContext: (&net.Dialer{
					Timeout: 50 * time.Millisecond,
				}).DialContext,
				ResponseHeaderTimeout: 80 * time.Millisecond,
			},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if res, ok := probeLocalDaemonsConcurrent(ctx, client, ollamaURL, lmStudioURL, vllmURL, localaiURL); ok {
		cfg.Backend = res.Backend
		cfg.BaseURL = res.BaseURL
		cfg.Model = res.Model
		cfg.ActiveModel = res.ActiveModel
		cfg.IsOffline = false
		return
	}

	// Step 2: Fallback to Cloud Environment Variables if no local daemon is running
	if !forceCloud && probeCloudEnvVars(cfg, opts.EnvFile) {
		return
	}

	// Step 3: Fallback to Embedded Local Cache Offline Mode
	cfg.Backend = BackendOffline
	cfg.Model = "embedded-local-cache"
	if m := os.Getenv("OMNICODER_MODEL"); m != "" {
		cfg.Model = m
	}
	cfg.BaseURL = ""
	cfg.APIKey = ""
	cfg.IsOffline = true
}

func normalizeURL(host, defaultPort string) string {
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		if strings.HasPrefix(host, ":") {
			host = "127.0.0.1" + host
		}
		host = "http://" + host
	}
	return strings.TrimRight(host, "/")
}

// probeCloudEnvVars checks environment variables and Calera vault token files.
func probeCloudEnvVars(cfg *DetectedConfig, customEnvFile string) bool {
	// 1. GEMINI_API_KEY
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		cfg.Backend = BackendCloudGemini
		cfg.APIKey = key
		cfg.Model = "gemini-2.5-flash"
		if m := os.Getenv("GEMINI_MODEL"); m != "" {
			cfg.Model = m
		}
		if u := os.Getenv("GEMINI_BASE_URL"); u != "" {
			cfg.BaseURL = u
		}
		cfg.IsOffline = false
		return true
	}

	// 2. OPENAI_API_KEY
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		cfg.Backend = BackendCloudOpenAI
		cfg.APIKey = key
		cfg.Model = "gpt-4o"
		if m := os.Getenv("OPENAI_MODEL"); m != "" {
			cfg.Model = m
		}
		if u := os.Getenv("OPENAI_BASE_URL"); u != "" {
			cfg.BaseURL = u
		}
		cfg.IsOffline = false
		return true
	}

	// 3. ANTHROPIC_API_KEY
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		cfg.Backend = BackendCloudAnthropic
		cfg.APIKey = key
		cfg.Model = "claude-3-5-sonnet-20241022"
		if m := os.Getenv("ANTHROPIC_MODEL"); m != "" {
			cfg.Model = m
		}
		if u := os.Getenv("ANTHROPIC_BASE_URL"); u != "" {
			cfg.BaseURL = u
		}
		cfg.IsOffline = false
		return true
	}

	// 4. OMNICODER_API_KEY
	if key := os.Getenv("OMNICODER_API_KEY"); key != "" {
		cfg.Backend = BackendCaleraVault
		cfg.APIKey = key
		cfg.Model = "qwen2.5-coder:7b"
		if m := os.Getenv("OMNICODER_MODEL"); m != "" {
			cfg.Model = m
		}
		if u := os.Getenv("OMNICODER_BASE_URL"); u != "" {
			cfg.BaseURL = u
		}
		cfg.IsOffline = false
		return true
	}

	// 5. ~/.calera/icx.env
	envPath := customEnvFile
	if envPath == "" {
		if homeDir, err := os.UserHomeDir(); err == nil {
			envPath = filepath.Join(homeDir, ".calera", "icx.env")
		}
	}
	if envPath != "" {
		if parseEnvFile(envPath, cfg) {
			return true
		}
	}

	return false
}

// parseEnvFile reads and extracts credentials from an env file.
func parseEnvFile(path string, cfg *DetectedConfig) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	content := string(raw)
	lines := strings.Split(content, "\n")
	envMap := make(map[string]string)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			v = strings.Trim(v, `"'`)
			if k != "" && v != "" {
				envMap[k] = v
			}
		}
	}

	if key, ok := envMap["CALERA_API_KEY"]; ok && key != "" {
		cfg.Backend = BackendCaleraVault
		cfg.APIKey = key
		cfg.Model = "gemini-2.5-flash"
		if m, ok := envMap["CALERA_MODEL"]; ok && m != "" {
			cfg.Model = m
		}
		if u, ok := envMap["CALERA_BASE_URL"]; ok && u != "" {
			cfg.BaseURL = u
		}
		cfg.IsOffline = false
		return true
	}

	if key, ok := envMap["ICX_API_KEY"]; ok && key != "" {
		cfg.Backend = BackendCaleraVault
		cfg.APIKey = key
		cfg.Model = "gemini-2.5-flash"
		if m, ok := envMap["ICX_MODEL"]; ok && m != "" {
			cfg.Model = m
		}
		if u, ok := envMap["ICX_BASE_URL"]; ok && u != "" {
			cfg.BaseURL = u
		}
		cfg.IsOffline = false
		return true
	}

	if key, ok := envMap["GEMINI_API_KEY"]; ok && key != "" {
		cfg.Backend = BackendCloudGemini
		cfg.APIKey = key
		cfg.Model = "gemini-2.5-flash"
		if m, ok := envMap["GEMINI_MODEL"]; ok && m != "" {
			cfg.Model = m
		}
		cfg.IsOffline = false
		return true
	}

	if key, ok := envMap["OPENAI_API_KEY"]; ok && key != "" {
		cfg.Backend = BackendCloudOpenAI
		cfg.APIKey = key
		cfg.Model = "gpt-4o"
		if m, ok := envMap["OPENAI_MODEL"]; ok && m != "" {
			cfg.Model = m
		}
		if u, ok := envMap["OPENAI_BASE_URL"]; ok && u != "" {
			cfg.BaseURL = u
		}
		cfg.IsOffline = false
		return true
	}

	if key, ok := envMap["ANTHROPIC_API_KEY"]; ok && key != "" {
		cfg.Backend = BackendCloudAnthropic
		cfg.APIKey = key
		cfg.Model = "claude-3-5-sonnet-20241022"
		if m, ok := envMap["ANTHROPIC_MODEL"]; ok && m != "" {
			cfg.Model = m
		}
		cfg.IsOffline = false
		return true
	}

	if key, ok := envMap["OMNICODER_API_KEY"]; ok && key != "" {
		cfg.Backend = BackendCaleraVault
		cfg.APIKey = key
		cfg.Model = "qwen2.5-coder:7b"
		if m, ok := envMap["OMNICODER_MODEL"]; ok && m != "" {
			cfg.Model = m
		}
		if u, ok := envMap["OMNICODER_BASE_URL"]; ok && u != "" {
			cfg.BaseURL = u
		}
		cfg.IsOffline = false
		return true
	}

	return false
}

type localProbeResult struct {
	Backend     BackendType
	BaseURL     string
	Model       string
	ActiveModel string
	HasActive   bool
	Priority    int
	Found       bool
}

// probeLocalDaemonsConcurrent probes Ollama, LM Studio, vLLM, and LocalAI in parallel goroutines.
func probeLocalDaemonsConcurrent(ctx context.Context, client *http.Client, ollamaURL, lmStudioURL, vllmURL, localaiURL string) (localProbeResult, bool) {
	var wg sync.WaitGroup
	results := make([]localProbeResult, 4)

	// Target 1: Ollama (Priority 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0] = probeOllama(ctx, client, ollamaURL)
	}()

	// Target 2: LM Studio (Priority 1 - Local sovereign engine)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var candidates []string
		if lmStudioURL != "" {
			candidates = []string{lmStudioURL}
		} else {
			candidates = []string{"http://127.0.0.1:1234", "http://localhost:1234"}
		}
		for _, u := range candidates {
			res := probeOpenAICompatibleDaemon(ctx, client, BackendLocalLMStudio, u, 1)
			if res.Found {
				results[1] = res
				return
			}
		}
	}()

	// Target 3: vLLM (Priority 3)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[2] = probeOpenAICompatibleDaemon(ctx, client, BackendLocalVLLM, vllmURL, 3)
	}()

	// Target 4: LocalAI (Priority 4)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[3] = probeOpenAICompatibleDaemon(ctx, client, BackendLocalAI, localaiURL, 4)
	}()

	wg.Wait()

	var (
		bestActive   localProbeResult
		foundActive  bool
		bestFallback localProbeResult
		foundAny     bool
	)

	// Check 1: Active Model First (prioritize any daemon with in-memory resident model)
	for _, r := range results {
		if r.Found && r.HasActive {
			if !foundActive || r.Priority < bestActive.Priority {
				bestActive = r
				foundActive = true
			}
		}
	}
	if foundActive {
		return bestActive, true
	}

	// Check 2: Reachable daemon by priority order (Ollama > LM Studio > vLLM > LocalAI)
	for _, r := range results {
		if r.Found {
			if !foundAny || r.Priority < bestFallback.Priority {
				bestFallback = r
				foundAny = true
			}
		}
	}
	if foundAny {
		return bestFallback, true
	}

	return localProbeResult{}, false
}

type ollamaPsResponse struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// probeOllama checks /api/ps for active running models, falling back to /api/tags for installed models.
func probeOllama(ctx context.Context, client *http.Client, baseURL string) localProbeResult {
	baseURL = strings.TrimRight(baseURL, "/")
	v1URL := baseURL + "/v1"

	// 1. Probe /api/ps for active running model resident in memory
	psURL := baseURL + "/api/ps"
	req, err := http.NewRequestWithContext(ctx, "GET", psURL, nil)
	if err == nil {
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			var ps ollamaPsResponse
			if json.NewDecoder(resp.Body).Decode(&ps) == nil && len(ps.Models) > 0 {
				mName := ps.Models[0].Name
				if mName == "" {
					mName = ps.Models[0].Model
				}
				if mName != "" {
					return localProbeResult{
						Backend:     BackendLocalOllama,
						BaseURL:     v1URL,
						Model:       mName,
						ActiveModel: mName,
						HasActive:   true,
						Priority:    1,
						Found:       true,
					}
				}
			}
		}
	}

	// 2. Fallback to /api/tags for installed models
	tagsURL := baseURL + "/api/tags"
	reqTags, err := http.NewRequestWithContext(ctx, "GET", tagsURL, nil)
	if err != nil {
		return localProbeResult{}
	}
	respTags, errTags := client.Do(reqTags)
	if errTags == nil && respTags.StatusCode == http.StatusOK {
		defer respTags.Body.Close()
		var tags ollamaTagsResponse
		if json.NewDecoder(respTags.Body).Decode(&tags) == nil && len(tags.Models) > 0 {
			chosen := tags.Models[0].Name
			for _, m := range tags.Models {
				lower := strings.ToLower(m.Name)
				if strings.Contains(lower, "code") || strings.Contains(lower, "qwen") || strings.Contains(lower, "deepseek") {
					chosen = m.Name
					break
				}
			}
			return localProbeResult{
				Backend:     BackendLocalOllama,
				BaseURL:     v1URL,
				Model:       chosen,
				ActiveModel: "",
				HasActive:   false,
				Priority:    1,
				Found:       true,
			}
		}
		// Endpoint is up, but no models found
		return localProbeResult{
			Backend:     BackendLocalOllama,
			BaseURL:     v1URL,
			Model:       "llama3.2",
			ActiveModel: "",
			HasActive:   false,
			Priority:    1,
			Found:       true,
		}
	}

	return localProbeResult{}
}

type openAIModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// probeOpenAICompatibleDaemon queries /v1/models endpoints for LM Studio, vLLM, and LocalAI.
func probeOpenAICompatibleDaemon(ctx context.Context, client *http.Client, backend BackendType, baseURL string, priority int) localProbeResult {
	baseURL = strings.TrimRight(baseURL, "/")
	modelsURL := baseURL
	if !strings.HasSuffix(modelsURL, "/v1") {
		modelsURL += "/v1"
	}
	v1URL := modelsURL
	modelsEndpoint := modelsURL + "/models"

	req, err := http.NewRequestWithContext(ctx, "GET", modelsEndpoint, nil)
	if err != nil {
		return localProbeResult{}
	}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return localProbeResult{}
	}
	defer resp.Body.Close()

	var models openAIModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return localProbeResult{}
	}

	chosen := "default"
	hasModels := len(models.Data) > 0
	if hasModels {
		chosen = models.Data[0].ID
		// 1. Highest priority: google/gemma-4-12b-qat or any gemma model
		for _, m := range models.Data {
			lower := strings.ToLower(m.ID)
			if strings.Contains(lower, "gemma-4-12b") || strings.Contains(lower, "gemma") {
				chosen = m.ID
				break
			}
		}
		// 2. Secondary priority: code/qwen/deepseek if no gemma model is present
		if !strings.Contains(strings.ToLower(chosen), "gemma") {
			for _, m := range models.Data {
				lower := strings.ToLower(m.ID)
				if strings.Contains(lower, "code") || strings.Contains(lower, "qwen") || strings.Contains(lower, "deepseek") {
					chosen = m.ID
					break
				}
			}
		}
	}

	return localProbeResult{
		Backend:     backend,
		BaseURL:     v1URL,
		Model:       chosen,
		ActiveModel: chosen,
		HasActive:   hasModels,
		Priority:    priority,
		Found:       true,
	}
}
