// SPDX-License-Identifier: Apache-2.0
// Package llm_test contains adversarial stress tests, fault injection harnesses,
// and high-concurrency race/deadlock verification for the pkg/llm wire client.

package llm_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// ============================================================================
// 1. High-Concurrency Stress Test (50+ goroutines under go test -race)
// ============================================================================

func TestStress_HighConcurrency_50PlusGoroutines(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqNum := requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		resp := llm.ChatCompletionResponse{
			ID: fmt.Sprintf("cmpl-stress-%d", reqNum),
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: fmt.Sprintf(`{"worker_response":%d}`, reqNum),
					},
					FinishReason: "stop",
				},
			},
			Usage: llm.Usage{TotalTokens: 50},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:          ts.URL,
		APIKey:           "stress-secret-key",
		Model:            "gpt-4o-mini",
		Timeout:          10 * time.Second,
		MaxRetries:       2,
		RetryInitialWait: 5 * time.Millisecond,
		RetryMaxWait:     20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	const goroutines = 64
	const requestsPerGoroutine = 5
	const totalExpected = goroutines * requestsPerGoroutine

	// Shared request instance to verify thread-safety of request processing
	sharedReq := &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "You are a concise decision gate."},
			{Role: "user", Content: "Evaluate incoming patch"},
		},
	}

	var wg sync.WaitGroup
	errCh := make(chan error, totalExpected)

	startBarrier := make(chan struct{})

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier // synchronize all goroutines to unleash concurrent stampede

			for r := 0; r < requestsPerGoroutine; r++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				resp, err := client.Complete(ctx, sharedReq)
				cancel()

				if err != nil {
					errCh <- fmt.Errorf("worker %d req %d failed: %w", workerID, r, err)
					return
				}
				if resp == nil || resp.Content() == "" {
					errCh <- fmt.Errorf("worker %d req %d got nil or empty response", workerID, r)
					return
				}
			}
		}(g)
	}

	close(startBarrier)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrency error: %v", err)
	}

	if actual := requestCount.Load(); actual != int64(totalExpected) {
		t.Errorf("expected %d total requests handled, got %d", totalExpected, actual)
	}
}

// ============================================================================
// 2. Simulated Network Faults
// ============================================================================

// 2A. Slow server with context timeouts: verify fast exit and no hanging
func TestFault_SlowServer_ContextTimeout(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server takes 500ms to respond
		select {
		case <-time.After(500 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"late"}}]}`))
		case <-r.Context().Done():
			// Client disconnected early
			return
		}
	}))
	defer ts.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:          ts.URL,
		Timeout:          5 * time.Second,
		MaxRetries:       3,
		RetryInitialWait: 10 * time.Millisecond,
		RetryMaxWait:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// Client has tight 60ms timeout
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = client.Complete(ctx, &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{{Role: "user", Content: "hello"}},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.DeadlineExceeded or Canceled, got: %v", err)
	}

	// Ensure the client did not wait for the server's 500ms
	if elapsed > 250*time.Millisecond {
		t.Errorf("client took too long to abort on context deadline: %v", elapsed)
	}
}

// 2B. Context cancellation during backoff sleep: verify immediate cancellation
func TestFault_ContextCancellation_DuringBackoff(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always return 503 to trigger backoff sleep
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"service unavailable"}}`))
	}))
	defer ts.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:          ts.URL,
		MaxRetries:       5,
		RetryInitialWait: 500 * time.Millisecond, // long sleep
		RetryMaxWait:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = client.Complete(ctx, &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{{Role: "user", Content: "retry sleep test"}},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected context error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("expected context cancellation, got %v", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("client did not abort promptly from backoff sleep: elapsed %v", elapsed)
	}
}

// 2C. Premature connection closure (TCP RST / early termination) with recovery on retry
func TestFault_PrematureConnectionClosure_Recovery(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := attempts.Add(1)
		if att <= 2 {
			// Prematurely close TCP connection using Hijack
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("webserver doesn't support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack failed: %v", err)
				return
			}
			_ = conn.Close() // abrupt closure!
			return
		}

		// Attempt 3 succeeds
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
			Choices: []llm.Choice{{Message: llm.ChatMessage{Content: `{"recovered":true}`}}},
		})
	}))
	defer ts.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:          ts.URL,
		MaxRetries:       3,
		RetryInitialWait: 5 * time.Millisecond,
		RetryMaxWait:     20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	resp, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{{Role: "user", Content: "survive disconnect"}},
	})
	if err != nil {
		t.Fatalf("expected recovery after premature closures, got: %v", err)
	}
	if resp.Content() != `{"recovered":true}` {
		t.Errorf("unexpected content: %s", resp.Content())
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts before recovery, got %d", attempts.Load())
	}
}

// 2D. Premature connection closure exhausting max retries
func TestFault_PrematureConnectionClosure_Exhausted(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer ts.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:          ts.URL,
		MaxRetries:       2,
		RetryInitialWait: 5 * time.Millisecond,
		RetryMaxWait:     20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, err = client.Complete(context.Background(), &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{{Role: "user", Content: "will fail"}},
	})
	if err == nil {
		t.Fatalf("expected network error, got nil")
	}
	// Initial attempt + 2 retries = 3 attempts
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

// 2E. HTTP 429 Rate Limit Bursts with varying Retry-After formats
func TestFault_RateLimitBursts_VaryingRetryAfter(t *testing.T) {
	t.Parallel()

	t.Run("RetryAfter_SecondsInteger", func(t *testing.T) {
		var attempts atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			att := attempts.Add(1)
			if att == 1 {
				// Retry-After in integer seconds
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				Choices: []llm.Choice{{Message: llm.ChatMessage{Content: "success-after-wait"}}},
			})
		}))
		defer ts.Close()

		client, err := llm.NewClient(llm.ClientConfig{
			BaseURL:          ts.URL,
			MaxRetries:       2,
			RetryInitialWait: 5 * time.Millisecond,
			RetryMaxWait:     1500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		start := time.Now()
		resp, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
			Messages: []llm.ChatMessage{{Role: "user", Content: "rate limit test"}},
		})
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("expected success on retry, got: %v", err)
		}
		if resp.Content() != "success-after-wait" {
			t.Errorf("unexpected content: %s", resp.Content())
		}
		// Must have honored approximately 1s wait (>= 900ms)
		if elapsed < 900*time.Millisecond {
			t.Errorf("client did not honor Retry-After: 1s, elapsed: %v", elapsed)
		}
	})

	t.Run("RetryAfter_RFC1123Date", func(t *testing.T) {
		var attempts atomic.Int32
		var sentDate string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			att := attempts.Add(1)
			if att == 1 {
				// 2 seconds in the future formatted as RFC 1123 HTTP date
				futureDate := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
				sentDate = futureDate
				w.Header().Set("Retry-After", futureDate)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				Choices: []llm.Choice{{Message: llm.ChatMessage{Content: "success-after-date"}}},
			})
		}))
		defer ts.Close()

		client, err := llm.NewClient(llm.ClientConfig{
			BaseURL:          ts.URL,
			MaxRetries:       2,
			RetryInitialWait: 5 * time.Millisecond,
			RetryMaxWait:     5 * time.Second,
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		start := time.Now()
		resp, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
			Messages: []llm.ChatMessage{{Role: "user", Content: "date test"}},
		})
		elapsed := time.Since(start)

		t.Logf("Sent HTTP-date: %s, Total elapsed: %v", sentDate, elapsed)

		if err != nil {
			t.Fatalf("expected success, got: %v", err)
		}
		if resp.Content() != "success-after-date" {
			t.Errorf("unexpected content: %s", resp.Content())
		}
		// Since HTTP-date truncates sub-seconds, 2 seconds in the future yields [1.0s, 2.0s]
		if elapsed < 900*time.Millisecond {
			t.Errorf("client did not honor RFC 1123 Retry-After date, elapsed: %v", elapsed)
		}
	})

	t.Run("RetryAfter_ClampedToRetryMaxWait", func(t *testing.T) {
		var attempts atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			att := attempts.Add(1)
			if att == 1 {
				// Unreasonable server wait of 100 seconds
				w.Header().Set("Retry-After", "100")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"huge wait"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				Choices: []llm.Choice{{Message: llm.ChatMessage{Content: "clamped-wait-success"}}},
			})
		}))
		defer ts.Close()

		// Max wait capped to 50ms
		client, err := llm.NewClient(llm.ClientConfig{
			BaseURL:          ts.URL,
			MaxRetries:       2,
			RetryInitialWait: 5 * time.Millisecond,
			RetryMaxWait:     50 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		start := time.Now()
		resp, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
			Messages: []llm.ChatMessage{{Role: "user", Content: "clamp test"}},
		})
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("expected success, got: %v", err)
		}
		if resp.Content() != "clamped-wait-success" {
			t.Errorf("unexpected content: %s", resp.Content())
		}
		// Must not have waited anywhere near 100 seconds!
		if elapsed > 500*time.Millisecond {
			t.Errorf("client failed to clamp Retry-After to RetryMaxWait: took %v", elapsed)
		}
	})

	t.Run("Concurrent_429_Stampede", func(t *testing.T) {
		// 30 concurrent workers hitting 429 once before succeeding
		var perWorker sync.Map
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			workerID := string(body)
			if idx := strings.Index(workerID, "burst worker "); idx != -1 {
				workerID = workerID[idx : idx+len("burst worker 00")]
			}
			// First call for each worker triggers 429
			if _, loaded := perWorker.LoadOrStore(workerID, true); !loaded {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"burst rate limit"}}`))
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				Choices: []llm.Choice{{Message: llm.ChatMessage{Content: "burst-ok"}}},
			})
		}))
		defer ts.Close()

		client, err := llm.NewClient(llm.ClientConfig{
			BaseURL:          ts.URL,
			MaxRetries:       3,
			RetryInitialWait: 5 * time.Millisecond,
			RetryMaxWait:     30 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		var wg sync.WaitGroup
		workers := 30
		errs := make(chan error, workers)

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				resp, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
					Messages: []llm.ChatMessage{{Role: "user", Content: fmt.Sprintf("burst worker %d", id)}},
				})
				if err != nil {
					errs <- fmt.Errorf("worker %d failed: %w", id, err)
					return
				}
				if resp.Content() != "burst-ok" {
					errs <- fmt.Errorf("worker %d unexpected content: %s", id, resp.Content())
				}
			}(w)
		}

		wg.Wait()
		close(errs)

		for err := range errs {
			t.Errorf("burst error: %v", err)
		}
	})
}

// 2F. HTTP 503 Transient Spikes: recovery and error classification
func TestFault_HTTP503_TransientSpikes(t *testing.T) {
	t.Parallel()

	t.Run("TransientSpike_Recovery", func(t *testing.T) {
		var attempts atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			att := attempts.Add(1)
			if att <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"message":"backend overloaded"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				Choices: []llm.Choice{{Message: llm.ChatMessage{Content: "recovered-from-503"}}},
			})
		}))
		defer ts.Close()

		client, err := llm.NewClient(llm.ClientConfig{
			BaseURL:          ts.URL,
			MaxRetries:       3,
			RetryInitialWait: 5 * time.Millisecond,
			RetryMaxWait:     20 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		resp, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
			Messages: []llm.ChatMessage{{Role: "user", Content: "503 spike"}},
		})
		if err != nil {
			t.Fatalf("expected recovery, got: %v", err)
		}
		if resp.Content() != "recovered-from-503" {
			t.Errorf("unexpected content: %s", resp.Content())
		}
		if attempts.Load() != 3 {
			t.Errorf("expected 3 attempts, got %d", attempts.Load())
		}
	})

	t.Run("ServerErrors_500_502_504_Exhausted", func(t *testing.T) {
		codes := []int{
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		}

		for _, code := range codes {
			t.Run(fmt.Sprintf("HTTP_%d", code), func(t *testing.T) {
				var calls atomic.Int32
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.WriteHeader(code)
					_, _ = w.Write([]byte(`{"error":{"message":"transient server error"}}`))
				}))
				defer ts.Close()

				client, err := llm.NewClient(llm.ClientConfig{
					BaseURL:          ts.URL,
					MaxRetries:       2,
					RetryInitialWait: 5 * time.Millisecond,
					RetryMaxWait:     15 * time.Millisecond,
				})
				if err != nil {
					t.Fatalf("NewClient error: %v", err)
				}

				_, err = client.Complete(context.Background(), &llm.ChatCompletionRequest{
					Messages: []llm.ChatMessage{{Role: "user", Content: "test"}},
				})
				if err == nil {
					t.Fatalf("expected error for HTTP %d, got nil", code)
				}
				if !errors.Is(err, llm.ErrServerError) {
					t.Errorf("expected ErrServerError, got %v", err)
				}
				// Initial attempt + 2 retries = 3 calls
				if calls.Load() != 3 {
					t.Errorf("expected 3 calls, got %d", calls.Load())
				}
			})
		}
	})
}

// ============================================================================
// 3. Fast Failure Verification on Non-Retryable Errors (401, 400, 404, 403)
// ============================================================================

func TestFailFast_NonRetryable_ZeroWastedRetries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		statusCode int
		sentinel   error
	}{
		{"Unauthorized_401", http.StatusUnauthorized, llm.ErrUnauthorized},
		{"Forbidden_403", http.StatusForbidden, llm.ErrForbidden},
		{"BadRequest_400", http.StatusBadRequest, llm.ErrInvalidRequest},
		{"NotFound_404", http.StatusNotFound, llm.ErrNotFound},
		{"MethodNotAllowed_405", http.StatusMethodNotAllowed, nil},
		{"UnprocessableEntity_422", http.StatusUnprocessableEntity, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requestCounter atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCounter.Add(1)
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"non-retryable %d"}}`, tc.statusCode)))
			}))
			defer ts.Close()

			// MaxRetries configured to 10 to catch any accidental retry loops
			client, err := llm.NewClient(llm.ClientConfig{
				BaseURL:          ts.URL,
				MaxRetries:       10,
				RetryInitialWait: 5 * time.Millisecond,
				RetryMaxWait:     20 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("NewClient error: %v", err)
			}

			// Run 50 concurrent requests to verify non-retryable fast-fail under concurrent load
			const concurrency = 50
			var wg sync.WaitGroup
			errs := make(chan error, concurrency)

			for i := 0; i < concurrency; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					_, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
						Messages: []llm.ChatMessage{{Role: "user", Content: fmt.Sprintf("req-%d", idx)}},
					})
					if err == nil {
						errs <- fmt.Errorf("expected error for status %d, got nil", tc.statusCode)
						return
					}
					if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
						errs <- fmt.Errorf("expected sentinel %v, got %v", tc.sentinel, err)
						return
					}
					var statusErr *llm.StatusError
					if !errors.As(err, &statusErr) {
						errs <- fmt.Errorf("expected StatusError type, got %T: %v", err, err)
						return
					}
					if statusErr.StatusCode != tc.statusCode {
						errs <- fmt.Errorf("expected status code %d, got %d", tc.statusCode, statusErr.StatusCode)
						return
					}
				}(i)
			}

			wg.Wait()
			close(errs)

			for err := range errs {
				t.Errorf("concurrency error: %v", err)
			}

			// Invariant: Exactly 50 requests must be made — exactly 1 per caller, 0 retries!
			if actual := requestCounter.Load(); actual != concurrency {
				t.Errorf("FAIL FAST VIOLATION: expected exactly %d requests (1 per call), but server received %d (wasted retries occurred!)",
					concurrency, actual)
			}
		})
	}
}

// ============================================================================
// 4. Heterogeneous Chaos Stress Harness: Race & Deadlock Hunter
// ============================================================================

func TestChaos_HeterogeneousTraffic_RaceFreeZeroDeadlock(t *testing.T) {
	t.Parallel()

	var (
		totalHits    atomic.Int64
		successCount atomic.Int64
		retry429Hits atomic.Int64
		spike503Hits atomic.Int64
		failFastHits atomic.Int64
		hijackHits   atomic.Int64
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := totalHits.Add(1)

		// Pseudo-random route based on crypto/rand
		nBig, _ := rand.Int(rand.Reader, big.NewInt(100))
		pct := nBig.Int64()

		switch {
		case pct < 40:
			// 40% Success
			successCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				ID: fmt.Sprintf("cmpl-chaos-%d", h),
				Choices: []llm.Choice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `{"chaos":"pass"}`}},
				},
			})

		case pct < 60:
			// 20% 429 Rate Limit (succeeds on retry if client attempts)
			retry429Hits.Add(1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"chaos rate limit"}}`))

		case pct < 75:
			// 15% 503 Transient Spike
			spike503Hits.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"chaos overloaded"}}`))

		case pct < 90:
			// 15% Non-retryable 401/400
			failFastHits.Add(1)
			if pct%2 == 0 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"chaos unauthorized"}}`))
			} else {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"chaos bad request"}}`))
			}

		default:
			// 10% Connection Hijack abrupt disconnect
			hijackHits.Add(1)
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:          ts.URL,
		APIKey:           "chaos-key",
		MaxRetries:       2,
		RetryInitialWait: 5 * time.Millisecond,
		RetryMaxWait:     20 * time.Millisecond,
		Timeout:          2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	const concurrentWorkers = 50
	const iterationsPerWorker = 6

	var wg sync.WaitGroup

	for w := 0; w < concurrentWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for i := 0; i < iterationsPerWorker; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				_, _ = client.Complete(ctx, &llm.ChatCompletionRequest{
					Messages: []llm.ChatMessage{
						{Role: "user", Content: fmt.Sprintf("chaos-worker-%d-%d", workerID, i)},
					},
				})
				cancel()
			}
		}(w)
	}

	// Deadlock guard: wait with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Completed cleanly without deadlock
	case <-time.After(15 * time.Second):
		t.Fatal("DEADLOCK DETECTED: chaos test failed to complete within 15 seconds")
	}

	t.Logf("Chaos Stress Test completed: %d total server hits (Success: %d, 429: %d, 503: %d, FailFast: %d, Hijack: %d)",
		totalHits.Load(), successCount.Load(), retry429Hits.Load(), spike503Hits.Load(), failFastHits.Load(), hijackHits.Load())
}
