// SPDX-License-Identifier: Apache-2.0
package mcp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
)

// ----------------------------------------------------------------------------
// 1. Workspace Root & Boundary Detection Tests
// ----------------------------------------------------------------------------

func TestAutoDetect_WorkspaceBoundaries(t *testing.T) {
	tempRoot := t.TempDir()

	// Test case A: go.mod project boundary
	goProject := filepath.Join(tempRoot, "goproject")
	_ = os.MkdirAll(filepath.Join(goProject, "pkg", "core", "deep"), 0755)
	_ = os.WriteFile(filepath.Join(goProject, "go.mod"), []byte("module testgo\n"), 0644)

	cfg := mcp.AutoDetectEnvironment(filepath.Join(goProject, "pkg", "core", "deep"))
	if cfg.WorkspaceDir != goProject {
		t.Errorf("expected go.mod workspace %s, got %s", goProject, cfg.WorkspaceDir)
	}

	// Test case B: package.json project boundary
	jsProject := filepath.Join(tempRoot, "jsproject")
	_ = os.MkdirAll(filepath.Join(jsProject, "src", "components"), 0755)
	_ = os.WriteFile(filepath.Join(jsProject, "package.json"), []byte(`{"name":"test"}`), 0644)

	cfgJS := mcp.AutoDetectEnvironment(filepath.Join(jsProject, "src", "components"))
	if cfgJS.WorkspaceDir != jsProject {
		t.Errorf("expected package.json workspace %s, got %s", jsProject, cfgJS.WorkspaceDir)
	}

	// Test case C: .git boundary & git detection
	gitProject := filepath.Join(tempRoot, "gitproject")
	_ = os.MkdirAll(filepath.Join(gitProject, ".git"), 0755)
	_ = os.MkdirAll(filepath.Join(gitProject, "docs", "specs"), 0755)

	cfgGit := mcp.AutoDetectEnvironment(filepath.Join(gitProject, "docs", "specs"))
	if cfgGit.WorkspaceDir != gitProject {
		t.Errorf("expected git workspace %s, got %s", gitProject, cfgGit.WorkspaceDir)
	}
	if !cfgGit.HasGit {
		t.Errorf("expected HasGit true")
	}
	if cfgGit.GitRoot != gitProject {
		t.Errorf("expected GitRoot %s, got %s", gitProject, cfgGit.GitRoot)
	}

	// Test case D: Non-git isolated folder
	isolatedDir := filepath.Join(tempRoot, "isolated")
	_ = os.MkdirAll(isolatedDir, 0755)
	cfgIsolated := mcp.AutoDetectEnvironment(isolatedDir)
	if cfgIsolated.WorkspaceDir != isolatedDir {
		t.Errorf("expected workspace %s, got %s", isolatedDir, cfgIsolated.WorkspaceDir)
	}
	// In temp directory where no parent has .git, HasGit should be false
	// (or true if temp dir happens to be within a git repo on the host machine)
	// Test case E: IDE Directory Rejection and Auto-Discovery
	ideFakeDir := filepath.Join(tempRoot, "AppData", "Local", "Programs", "Microsoft VS Code")
	_ = os.MkdirAll(ideFakeDir, 0755)
	_ = os.WriteFile(filepath.Join(ideFakeDir, "Code.exe"), []byte("fake binary"), 0755)

	realProject := filepath.Join(tempRoot, "MyRealProject")
	_ = os.MkdirAll(realProject, 0755)
	_ = os.WriteFile(filepath.Join(realProject, "go.mod"), []byte("module realproject\n"), 0644)

	// Set OMNICODER_WORKSPACE to point to real project as fallback discovery
	origEnv := os.Getenv("OMNICODER_WORKSPACE")
	_ = os.Setenv("OMNICODER_WORKSPACE", realProject)
	defer func() { _ = os.Setenv("OMNICODER_WORKSPACE", origEnv) }()

	if !mcp.IsIDEOsOrSystemDir(ideFakeDir) {
		t.Errorf("expected IsIDEOsOrSystemDir(%s) to be true", ideFakeDir)
	}

	cfgIDE := mcp.AutoDetectEnvironment(ideFakeDir)
	if cfgIDE.WorkspaceDir != realProject {
		t.Errorf("expected IDE path %s to be rejected in favor of %s, got %s", ideFakeDir, realProject, cfgIDE.WorkspaceDir)
	}
}

// ----------------------------------------------------------------------------
// 2. Cloud Environment Variables & ICX Env Tests
// ----------------------------------------------------------------------------

func TestAutoDetect_CloudCredentials(t *testing.T) {
	// Isolate env variables for test duration
	origGemini := os.Getenv("GEMINI_API_KEY")
	origOpenAI := os.Getenv("OPENAI_API_KEY")
	origAnthropic := os.Getenv("ANTHROPIC_API_KEY")
	origOmni := os.Getenv("OMNICODER_API_KEY")
	defer func() {
		_ = os.Setenv("GEMINI_API_KEY", origGemini)
		_ = os.Setenv("OPENAI_API_KEY", origOpenAI)
		_ = os.Setenv("ANTHROPIC_API_KEY", origAnthropic)
		_ = os.Setenv("OMNICODER_API_KEY", origOmni)
	}()

	_ = os.Unsetenv("GEMINI_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OMNICODER_API_KEY")

	t.Setenv("OMNICODER_CLOUD", "1")
	// 2a. GEMINI_API_KEY
	_ = os.Setenv("GEMINI_API_KEY", "AIzaSyTestGeminiSecretKey12345")
	cfgGemini := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir: t.TempDir(),
		EnvFile: filepath.Join(t.TempDir(), "nonexistent.env"),
	})
	if cfgGemini.Backend != mcp.BackendCloudGemini {
		t.Errorf("expected BackendCloudGemini, got %s", cfgGemini.Backend)
	}
	if cfgGemini.Model != "gemini-2.5-flash" {
		t.Errorf("expected gemini-2.5-flash, got %s", cfgGemini.Model)
	}
	if cfgGemini.APIKey != "AIzaSyTestGeminiSecretKey12345" {
		t.Errorf("expected matching APIKey")
	}
	if cfgGemini.IsOffline {
		t.Errorf("expected IsOffline false")
	}

	// Masking test
	masked := cfgGemini.MaskedCopy()
	if !strings.HasPrefix(masked.APIKey, "AIza...") {
		t.Errorf("expected masked API key prefix AIza..., got %s", masked.APIKey)
	}

	// 2b. OPENAI_API_KEY
	_ = os.Unsetenv("GEMINI_API_KEY")
	_ = os.Setenv("OPENAI_API_KEY", "sk-proj-openai-test-key-999")
	cfgOpenAI := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir: t.TempDir(),
		EnvFile: filepath.Join(t.TempDir(), "nonexistent.env"),
	})
	if cfgOpenAI.Backend != mcp.BackendCloudOpenAI {
		t.Errorf("expected BackendCloudOpenAI, got %s", cfgOpenAI.Backend)
	}
	if cfgOpenAI.Model != "gpt-4o" {
		t.Errorf("expected gpt-4o, got %s", cfgOpenAI.Model)
	}

	// 2c. ANTHROPIC_API_KEY
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-anthropic-key-777")
	cfgAnthropic := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir: t.TempDir(),
		EnvFile: filepath.Join(t.TempDir(), "nonexistent.env"),
	})
	if cfgAnthropic.Backend != mcp.BackendCloudAnthropic {
		t.Errorf("expected BackendCloudAnthropic, got %s", cfgAnthropic.Backend)
	}
	if cfgAnthropic.Model != "claude-3-5-sonnet-20241022" {
		t.Errorf("expected claude-3-5-sonnet-20241022, got %s", cfgAnthropic.Model)
	}

	// 2d. OMNICODER_API_KEY
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Setenv("OMNICODER_API_KEY", "calera-omnicoder-key-888")
	cfgOmni := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir: t.TempDir(),
		EnvFile: filepath.Join(t.TempDir(), "nonexistent.env"),
	})
	if cfgOmni.Backend != mcp.BackendCaleraVault {
		t.Errorf("expected BackendCaleraVault, got %s", cfgOmni.Backend)
	}
	if cfgOmni.Model != "qwen2.5-coder:7b" {
		t.Errorf("expected qwen2.5-coder:7b, got %s", cfgOmni.Model)
	}

	// 2e. Custom ~/.calera/icx.env parsing
	_ = os.Unsetenv("OMNICODER_API_KEY")
	envFile := filepath.Join(t.TempDir(), "icx.env")
	_ = os.WriteFile(envFile, []byte("CALERA_API_KEY=calera_vault_token_abc123\nCALERA_MODEL=gemini-2.5-pro\n"), 0600)

	cfgEnv := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir: t.TempDir(),
		EnvFile: envFile,
	})
	if cfgEnv.Backend != mcp.BackendCaleraVault {
		t.Errorf("expected BackendCaleraVault from icx.env, got %s", cfgEnv.Backend)
	}
	if cfgEnv.Model != "gemini-2.5-pro" {
		t.Errorf("expected gemini-2.5-pro, got %s", cfgEnv.Model)
	}
	if cfgEnv.APIKey != "calera_vault_token_abc123" {
		t.Errorf("expected calera_vault_token_abc123, got %s", cfgEnv.APIKey)
	}
}

// ----------------------------------------------------------------------------
// 3. Concurrent Local LLM Daemon Probes Tests
// ----------------------------------------------------------------------------

func TestAutoDetect_ConcurrentLocalProbes_OllamaActiveModel(t *testing.T) {
	// Set up mock Ollama server with active resident model on /api/ps
	var psCalled int32
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			atomic.AddInt32(&psCalled, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{
						"name":  "qwen2.5-coder:14b",
						"model": "qwen2.5-coder:14b",
						"size":  9000000000,
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ollamaSrv.Close()

	// Clear env keys
	_ = os.Unsetenv("GEMINI_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OMNICODER_API_KEY")

	cfg := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir:      t.TempDir(),
		EnvFile:      filepath.Join(t.TempDir(), "nonexistent.env"),
		OllamaURL:    ollamaSrv.URL,
		LMStudioURL:  "http://127.0.0.1:59999", // Unreachable port
		VLLMURL:      "http://127.0.0.1:59998", // Unreachable port
		LocalAIURL:   "http://127.0.0.1:59997", // Unreachable port
		ProbeTimeout: 100 * time.Millisecond,
	})

	if cfg.Backend != mcp.BackendLocalOllama {
		t.Errorf("expected BackendLocalOllama, got %s", cfg.Backend)
	}
	if cfg.Model != "qwen2.5-coder:14b" {
		t.Errorf("expected active model qwen2.5-coder:14b, got %s", cfg.Model)
	}
	if cfg.ActiveModel != "qwen2.5-coder:14b" {
		t.Errorf("expected active_model qwen2.5-coder:14b, got %s", cfg.ActiveModel)
	}
	if cfg.IsOffline {
		t.Errorf("expected IsOffline false")
	}
	if atomic.LoadInt32(&psCalled) == 0 {
		t.Errorf("expected /api/ps to be queried")
	}
}

func TestAutoDetect_ConcurrentLocalProbes_OllamaFallbackToTags(t *testing.T) {
	// Mock Ollama with empty /api/ps, but installed models in /api/tags
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ps" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{}, // No model resident in RAM
			})
			return
		}
		if r.URL.Path == "/api/tags" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{"name": "deepseek-coder:6.7b"},
					{"name": "llama3:latest"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ollamaSrv.Close()

	_ = os.Unsetenv("GEMINI_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OMNICODER_API_KEY")

	cfg := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir:      t.TempDir(),
		EnvFile:      filepath.Join(t.TempDir(), "nonexistent.env"),
		OllamaURL:    ollamaSrv.URL,
		LMStudioURL:  "http://127.0.0.1:59999",
		VLLMURL:      "http://127.0.0.1:59998",
		LocalAIURL:   "http://127.0.0.1:59997",
		ProbeTimeout: 100 * time.Millisecond,
	})

	if cfg.Backend != mcp.BackendLocalOllama {
		t.Errorf("expected BackendLocalOllama, got %s", cfg.Backend)
	}
	// Preferred coder model from tags
	if cfg.Model != "deepseek-coder:6.7b" {
		t.Errorf("expected deepseek-coder:6.7b, got %s", cfg.Model)
	}
	if cfg.IsOffline {
		t.Errorf("expected IsOffline false")
	}
}

func TestAutoDetect_ConcurrentLocalProbes_LMStudio(t *testing.T) {
	// Mock LM Studio /v1/models
	lmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "qwen2.5-coder-7b-instruct"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer lmSrv.Close()

	_ = os.Unsetenv("GEMINI_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OMNICODER_API_KEY")

	cfg := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir:      t.TempDir(),
		EnvFile:      filepath.Join(t.TempDir(), "nonexistent.env"),
		OllamaURL:    "http://127.0.0.1:59999", // closed
		LMStudioURL:  lmSrv.URL,
		VLLMURL:      "http://127.0.0.1:59998", // closed
		LocalAIURL:   "http://127.0.0.1:59997", // closed
		ProbeTimeout: 100 * time.Millisecond,
	})

	if cfg.Backend != mcp.BackendLocalLMStudio {
		t.Errorf("expected BackendLocalLMStudio, got %s", cfg.Backend)
	}
	if cfg.Model != "qwen2.5-coder-7b-instruct" {
		t.Errorf("expected qwen2.5-coder-7b-instruct, got %s", cfg.Model)
	}
	if cfg.BaseURL != lmSrv.URL+"/v1" {
		t.Errorf("expected BaseURL %s, got %s", lmSrv.URL+"/v1", cfg.BaseURL)
	}
}

func TestAutoDetect_ConcurrentLocalProbes_VLLM(t *testing.T) {
	// Mock vLLM /v1/models
	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "meta-llama/Llama-3.1-70B-Instruct"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer vllmSrv.Close()

	_ = os.Unsetenv("GEMINI_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OMNICODER_API_KEY")

	cfg := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir:      t.TempDir(),
		EnvFile:      filepath.Join(t.TempDir(), "nonexistent.env"),
		OllamaURL:    "http://127.0.0.1:59999",
		LMStudioURL:  "http://127.0.0.1:59998",
		VLLMURL:      vllmSrv.URL,
		LocalAIURL:   "http://127.0.0.1:59997",
		ProbeTimeout: 100 * time.Millisecond,
	})

	if cfg.Backend != mcp.BackendLocalVLLM {
		t.Errorf("expected BackendLocalVLLM, got %s", cfg.Backend)
	}
	if cfg.Model != "meta-llama/Llama-3.1-70B-Instruct" {
		t.Errorf("expected meta-llama/Llama-3.1-70B-Instruct, got %s", cfg.Model)
	}
}

func TestAutoDetect_ConcurrentLocalProbes_LocalAI(t *testing.T) {
	// Mock LocalAI /v1/models
	localaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "gpt-4-local"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer localaiSrv.Close()

	_ = os.Unsetenv("GEMINI_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OMNICODER_API_KEY")

	cfg := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir:      t.TempDir(),
		EnvFile:      filepath.Join(t.TempDir(), "nonexistent.env"),
		OllamaURL:    "http://127.0.0.1:59999",
		LMStudioURL:  "http://127.0.0.1:59998",
		VLLMURL:      "http://127.0.0.1:59997",
		LocalAIURL:   localaiSrv.URL,
		ProbeTimeout: 100 * time.Millisecond,
	})

	if cfg.Backend != mcp.BackendLocalAI {
		t.Errorf("expected BackendLocalAI, got %s", cfg.Backend)
	}
	if cfg.Model != "gpt-4-local" {
		t.Errorf("expected gpt-4-local, got %s", cfg.Model)
	}
}

func TestAutoDetect_ActiveModelPriorityOverInactive(t *testing.T) {
	// Ollama is reachable but has NO active model running in RAM (/api/ps empty, tags has llama3)
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ps" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{}})
			return
		}
		if r.URL.Path == "/api/tags" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "llama3:8b"}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer ollamaSrv.Close()

	// LM Studio IS running with an ACTIVE model resident in memory!
	lmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "qwen2.5-coder-32b-instruct"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer lmSrv.Close()

	_ = os.Unsetenv("GEMINI_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OMNICODER_API_KEY")

	// Under "Active Model First" doctrine:
	// LM Studio has an active resident model, so it takes precedence over inactive Ollama tags!
	cfg := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir:      t.TempDir(),
		EnvFile:      filepath.Join(t.TempDir(), "nonexistent.env"),
		OllamaURL:    ollamaSrv.URL,
		LMStudioURL:  lmSrv.URL,
		ProbeTimeout: 100 * time.Millisecond,
	})

	if cfg.Backend != mcp.BackendLocalLMStudio {
		t.Errorf("expected LM Studio to win due to active model, got %s", cfg.Backend)
	}
	if cfg.Model != "qwen2.5-coder-32b-instruct" {
		t.Errorf("expected qwen2.5-coder-32b-instruct, got %s", cfg.Model)
	}
}

// ----------------------------------------------------------------------------
// 4. Offline Fallback to Embedded Local Cache Mode
// ----------------------------------------------------------------------------

func TestAutoDetect_OfflineLocalMemoryCacheFallback(t *testing.T) {
	_ = os.Unsetenv("GEMINI_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OMNICODER_API_KEY")

	cfg := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir:      t.TempDir(),
		EnvFile:      filepath.Join(t.TempDir(), "nonexistent.env"),
		OllamaURL:    "http://127.0.0.1:59999",
		LMStudioURL:  "http://127.0.0.1:59998",
		VLLMURL:      "http://127.0.0.1:59997",
		LocalAIURL:   "http://127.0.0.1:59996",
		ProbeTimeout: 40 * time.Millisecond,
	})

	if cfg.Backend != mcp.BackendOffline && cfg.Backend != mcp.BackendOfflineLocal {
		t.Errorf("expected BackendOffline or BackendOfflineLocal, got %s", cfg.Backend)
	}
	if cfg.Model != "embedded-local-cache" {
		t.Errorf("expected embedded-local-cache, got %s", cfg.Model)
	}
	if !cfg.IsOffline {
		t.Errorf("expected IsOffline true")
	}
	if cfg.APIKey != "" {
		t.Errorf("expected empty APIKey in offline mode")
	}
	if cfg.BaseURL != "" {
		t.Errorf("expected empty BaseURL in offline mode")
	}
}
