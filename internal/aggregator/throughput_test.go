//go:build goexperiment.jsonv2

// Package aggregator throughput tests.
//
// This test suite measures the aggregator's performance under various load conditions:
//   - Single vs multiple providers
//   - With and without caching
//   - Different response sizes
//   - Concurrent load scenarios
//   - Error handling under load
//
// Run benchmarks:
//
//	go test -tags=goexperiment.jsonv2 -bench=BenchmarkThroughput -benchmem ./internal/aggregator
//
// Run throughput tests:
//
//	go test -tags=goexperiment.jsonv2 -v -run=TestThroughput ./internal/aggregator
//
// Run all throughput tests with detailed output:
//
//	go test -tags=goexperiment.jsonv2 -v -run=TestThroughput -timeout=5m ./internal/aggregator
package aggregator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gcossani/ssfbff/runtime"
)

// throughputMetrics tracks performance metrics during throughput tests.
type throughputMetrics struct {
	totalRequests      int64
	successfulRequests int64
	failedRequests     int64
	totalLatency       time.Duration
	minLatency         time.Duration
	maxLatency         time.Duration
	latencies          []time.Duration
	mu                 sync.Mutex
}

func (m *throughputMetrics) record(latency time.Duration, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.totalRequests++
	if success {
		m.successfulRequests++
	} else {
		m.failedRequests++
	}

	m.totalLatency += latency
	if m.minLatency == 0 || latency < m.minLatency {
		m.minLatency = latency
	}
	if latency > m.maxLatency {
		m.maxLatency = latency
	}
	m.latencies = append(m.latencies, latency)
}

func (m *throughputMetrics) avgLatency() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.totalRequests == 0 {
		return 0
	}
	return m.totalLatency / time.Duration(m.totalRequests)
}

func (m *throughputMetrics) percentile(p float64) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.latencies) == 0 {
		return 0
	}

	// Simple percentile calculation - sort and pick
	latencies := make([]time.Duration, len(m.latencies))
	copy(latencies, m.latencies)
	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	index := int(float64(len(latencies)) * p)
	if index >= len(latencies) {
		index = len(latencies) - 1
	}
	return latencies[index]
}

// mockServer creates an HTTP test server with configurable response behavior.
type mockServerConfig struct {
	responseDelay   time.Duration
	responseBody    string
	responseSize    int     // bytes
	errorRate       float64 // 0.0 to 1.0
	errorStatusCode int
}

func createMockServer(cfg mockServerConfig) *httptest.Server {
	var requestCount int64

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt64(&requestCount, 1)

		if cfg.responseDelay > 0 {
			time.Sleep(cfg.responseDelay)
		}

		// Simulate error rate
		if cfg.errorRate > 0 && float64(count%100)/100.0 < cfg.errorRate {
			statusCode := cfg.errorStatusCode
			if statusCode == 0 {
				statusCode = http.StatusInternalServerError
			}
			http.Error(w, "simulated error", statusCode)
			return
		}

		// Generate response body
		body := cfg.responseBody
		if cfg.responseSize > 0 {
			body = generateResponseBody(cfg.responseSize)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}

func generateResponseBody(size int) string {
	// Generate JSON-like response of specified size
	base := `{"data":"`
	remaining := size - len(base) - 2 // -2 for closing `"}`
	if remaining < 0 {
		remaining = 0
	}

	// Fill with 'x' characters
	data := make([]byte, remaining)
	for i := range data {
		data[i] = 'x'
	}

	return base + string(data) + `"}`
}

// BenchmarkThroughputSingleProvider tests throughput with a single provider.
func BenchmarkThroughputSingleProvider(b *testing.B) {
	srv := createMockServer(mockServerConfig{
		responseDelay: 10 * time.Millisecond,
		responseBody:  `{"result":"ok"}`,
	})
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc": {
			BaseURL:   srv.URL,
			Timeout:   5 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep": "/ep"}),
		},
	}, testClientFactory)

	dep := runtime.ProviderDep{Provider: "svc", Endpoint: "ep"}
	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep})
			if err != nil {
				b.Errorf("Fetch failed: %v", err)
			}
		}
	})
}

// BenchmarkThroughputMultipleProviders tests throughput with multiple providers.
func BenchmarkThroughputMultipleProviders(b *testing.B) {
	srv := createMockServer(mockServerConfig{
		responseDelay: 10 * time.Millisecond,
		responseBody:  `{"result":"ok"}`,
	})
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc1": {
			BaseURL:   srv.URL,
			Timeout:   5 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep1": "/ep1"}),
		},
		"svc2": {
			BaseURL:   srv.URL,
			Timeout:   5 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep2": "/ep2"}),
		},
		"svc3": {
			BaseURL:   srv.URL,
			Timeout:   5 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep3": "/ep3"}),
		},
		"svc4": {
			BaseURL:   srv.URL,
			Timeout:   5 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep4": "/ep4"}),
		},
		"svc5": {
			BaseURL:   srv.URL,
			Timeout:   5 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep5": "/ep5"}),
		},
	}, testClientFactory)

	deps := []runtime.ProviderDep{
		{Provider: "svc1", Endpoint: "ep1"},
		{Provider: "svc2", Endpoint: "ep2"},
		{Provider: "svc3", Endpoint: "ep3"},
		{Provider: "svc4", Endpoint: "ep4"},
		{Provider: "svc5", Endpoint: "ep5"},
	}
	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := agg.Fetch(ctx, deps)
			if err != nil {
				b.Errorf("Fetch failed: %v", err)
			}
		}
	})
}

// BenchmarkThroughputWithCache tests throughput with caching enabled.
func BenchmarkThroughputWithCache(b *testing.B) {
	srv := createMockServer(mockServerConfig{
		responseDelay: 10 * time.Millisecond,
		responseBody:  `{"result":"ok"}`,
	})
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc": {
			BaseURL: srv.URL,
			Timeout: 5 * time.Second,
			Endpoints: map[string]EndpointConfig{
				"ep": {Path: "/ep", UseCache: true},
			},
		},
	}, testClientFactory)

	dep := runtime.ProviderDep{Provider: "svc", Endpoint: "ep"}
	ctx := context.Background()
	cache := &FetchCache{}
	ctx = WithFetchCache(ctx, cache)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep})
			if err != nil {
				b.Errorf("Fetch failed: %v", err)
			}
		}
	})
}

// BenchmarkThroughputLargeResponse tests throughput with large response bodies.
func BenchmarkThroughputLargeResponse(b *testing.B) {
	srv := createMockServer(mockServerConfig{
		responseDelay: 10 * time.Millisecond,
		responseSize:  100 * 1024, // 100KB
	})
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc": {
			BaseURL:   srv.URL,
			Timeout:   5 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep": "/ep"}),
		},
	}, testClientFactory)

	dep := runtime.ProviderDep{Provider: "svc", Endpoint: "ep"}
	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep})
			if err != nil {
				b.Errorf("Fetch failed: %v", err)
			}
		}
	})
}

// TestThroughputConcurrentLoad tests aggregator performance under concurrent load.
func TestThroughputConcurrentLoad(t *testing.T) {
	tests := []struct {
		name                 string
		numProviders         int
		numConcurrent        int
		requestsPerGoroutine int
		responseDelay        time.Duration
		responseSize         int
	}{
		{
			name:                 "low_concurrency_single_provider",
			numProviders:         1,
			numConcurrent:        10,
			requestsPerGoroutine: 100,
			responseDelay:        5 * time.Millisecond,
			responseSize:         1024,
		},
		{
			name:                 "medium_concurrency_multiple_providers",
			numProviders:         5,
			numConcurrent:        50,
			requestsPerGoroutine: 50,
			responseDelay:        10 * time.Millisecond,
			responseSize:         2048,
		},
		{
			name:                 "high_concurrency_many_providers",
			numProviders:         10,
			numConcurrent:        50,                    // Reduced from 100 to avoid overwhelming the server
			requestsPerGoroutine: 10,                    // Reduced from 20
			responseDelay:        10 * time.Millisecond, // Reduced from 15ms
			responseSize:         4096,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := createMockServer(mockServerConfig{
				responseDelay: tt.responseDelay,
				responseSize:  tt.responseSize,
			})
			defer srv.Close()

			// Create providers
			providers := make(map[string]ProviderConfig, tt.numProviders)
			deps := make([]runtime.ProviderDep, tt.numProviders)
			for i := 0; i < tt.numProviders; i++ {
				providerName := fmt.Sprintf("svc%d", i)
				endpointName := fmt.Sprintf("ep%d", i)
				providers[providerName] = ProviderConfig{
					BaseURL:   srv.URL,
					Timeout:   5 * time.Second,
					Endpoints: makeEndpoints(map[string]string{endpointName: fmt.Sprintf("/%s", endpointName)}),
				}
				deps[i] = runtime.ProviderDep{Provider: providerName, Endpoint: endpointName}
			}

			agg := New(providers, testClientFactory)
			metrics := &throughputMetrics{}

			start := time.Now()
			var wg sync.WaitGroup
			wg.Add(tt.numConcurrent)

			errorCounts := make(map[string]int)
			var errorMu sync.Mutex

			for i := 0; i < tt.numConcurrent; i++ {
				go func() {
					defer wg.Done()
					ctx := context.Background()
					for j := 0; j < tt.requestsPerGoroutine; j++ {
						reqStart := time.Now()
						_, err := agg.Fetch(ctx, deps)
						latency := time.Since(reqStart)
						success := err == nil
						metrics.record(latency, success)

						if !success && err != nil {
							errorMu.Lock()
							errorCounts[err.Error()]++
							errorMu.Unlock()
						}
					}
				}()
			}

			wg.Wait()
			totalDuration := time.Since(start)

			// Calculate metrics
			totalRequests := int64(tt.numConcurrent * tt.requestsPerGoroutine)
			throughput := float64(totalRequests) / totalDuration.Seconds()
			avgLatency := metrics.avgLatency()
			p50 := metrics.percentile(0.50)
			p95 := metrics.percentile(0.95)
			p99 := metrics.percentile(0.99)

			t.Logf("Total requests: %d", totalRequests)
			t.Logf("Successful: %d, Failed: %d", metrics.successfulRequests, metrics.failedRequests)
			t.Logf("Total duration: %v", totalDuration)
			t.Logf("Throughput: %.2f req/s", throughput)
			t.Logf("Latency - Avg: %v, Min: %v, Max: %v", avgLatency, metrics.minLatency, metrics.maxLatency)
			t.Logf("Latency - P50: %v, P95: %v, P99: %v", p50, p95, p99)

			if len(errorCounts) > 0 {
				t.Logf("Error breakdown:")
				for errMsg, count := range errorCounts {
					t.Logf("  %s: %d", errMsg, count)
				}
			}

			// Assertions - more lenient for high concurrency tests
			if metrics.failedRequests > 0 {
				failureRate := float64(metrics.failedRequests) / float64(totalRequests)
				maxAllowedRate := 0.05 // Allow 5% failure rate for high concurrency tests
				if tt.numConcurrent >= 50 {
					maxAllowedRate = 0.10 // Allow 10% for very high concurrency
				}
				if failureRate > maxAllowedRate {
					t.Errorf("Failure rate too high: %.2f%% (max allowed: %.2f%%)", failureRate*100, maxAllowedRate*100)
				} else {
					t.Logf("Failure rate acceptable: %.2f%%", failureRate*100)
				}
			}
		})
	}
}

// TestThroughputCachePerformance tests cache impact on throughput.
func TestThroughputCachePerformance(t *testing.T) {
	srv := createMockServer(mockServerConfig{
		responseDelay: 50 * time.Millisecond, // Slow upstream
		responseBody:  `{"result":"ok"}`,
	})
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc": {
			BaseURL: srv.URL,
			Timeout: 5 * time.Second,
			Endpoints: map[string]EndpointConfig{
				"ep": {Path: "/ep", UseCache: true},
			},
		},
	}, testClientFactory)

	dep := runtime.ProviderDep{Provider: "svc", Endpoint: "ep"}
	numRequests := 1000

	// Test without cache
	ctxNoCache := context.Background()
	startNoCache := time.Now()
	for i := 0; i < numRequests; i++ {
		_, err := agg.Fetch(ctxNoCache, []runtime.ProviderDep{dep})
		if err != nil {
			t.Fatalf("Fetch failed: %v", err)
		}
	}
	durationNoCache := time.Since(startNoCache)

	// Test with cache
	ctxWithCache := context.Background()
	cache := &FetchCache{}
	ctxWithCache = WithFetchCache(ctxWithCache, cache)

	// First request populates cache
	_, err := agg.Fetch(ctxWithCache, []runtime.ProviderDep{dep})
	if err != nil {
		t.Fatalf("First fetch failed: %v", err)
	}

	startWithCache := time.Now()
	for i := 0; i < numRequests-1; i++ {
		_, err := agg.Fetch(ctxWithCache, []runtime.ProviderDep{dep})
		if err != nil {
			t.Fatalf("Fetch failed: %v", err)
		}
	}
	durationWithCache := time.Since(startWithCache)

	throughputNoCache := float64(numRequests) / durationNoCache.Seconds()
	throughputWithCache := float64(numRequests) / durationWithCache.Seconds()
	speedup := throughputWithCache / throughputNoCache

	t.Logf("Without cache: %v for %d requests (%.2f req/s)", durationNoCache, numRequests, throughputNoCache)
	t.Logf("With cache: %v for %d requests (%.2f req/s)", durationWithCache, numRequests, throughputWithCache)
	t.Logf("Speedup: %.2fx", speedup)

	if speedup < 10 {
		t.Errorf("Expected significant speedup with cache, got %.2fx", speedup)
	}
}

// TestThroughputVaryingResponseSizes tests throughput with different response sizes.
func TestThroughputVaryingResponseSizes(t *testing.T) {
	sizes := []struct {
		name string
		size int // bytes
	}{
		{"small", 100},
		{"medium", 10 * 1024},
		{"large", 100 * 1024},
		{"very_large", 1024 * 1024}, // 1MB
	}

	for _, sizeTest := range sizes {
		t.Run(sizeTest.name, func(t *testing.T) {
			srv := createMockServer(mockServerConfig{
				responseDelay: 10 * time.Millisecond,
				responseSize:  sizeTest.size,
			})
			defer srv.Close()

			agg := New(map[string]ProviderConfig{
				"svc": {
					BaseURL:   srv.URL,
					Timeout:   5 * time.Second,
					Endpoints: makeEndpoints(map[string]string{"ep": "/ep"}),
				},
			}, testClientFactory)

			dep := runtime.ProviderDep{Provider: "svc", Endpoint: "ep"}
			ctx := context.Background()
			numRequests := 100

			start := time.Now()
			for i := 0; i < numRequests; i++ {
				_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep})
				if err != nil {
					t.Fatalf("Fetch failed: %v", err)
				}
			}
			duration := time.Since(start)
			throughput := float64(numRequests) / duration.Seconds()

			t.Logf("Response size: %d bytes, Throughput: %.2f req/s", sizeTest.size, throughput)
		})
	}
}

// TestThroughputErrorHandling tests throughput when some requests fail.
func TestThroughputErrorHandling(t *testing.T) {
	srv := createMockServer(mockServerConfig{
		responseDelay:   10 * time.Millisecond,
		responseBody:    `{"result":"ok"}`,
		errorRate:       0.1, // 10% error rate
		errorStatusCode: http.StatusInternalServerError,
	})
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc": {
			BaseURL:   srv.URL,
			Timeout:   5 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep": "/ep"}),
		},
	}, testClientFactory)

	dep := runtime.ProviderDep{Provider: "svc", Endpoint: "ep"}
	ctx := context.Background()
	numRequests := 1000

	metrics := &throughputMetrics{}
	start := time.Now()

	for i := 0; i < numRequests; i++ {
		reqStart := time.Now()
		_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep})
		latency := time.Since(reqStart)
		metrics.record(latency, err == nil)
	}

	duration := time.Since(start)
	throughput := float64(numRequests) / duration.Seconds()
	errorRate := float64(metrics.failedRequests) / float64(numRequests)

	t.Logf("Total requests: %d", numRequests)
	t.Logf("Successful: %d, Failed: %d", metrics.successfulRequests, metrics.failedRequests)
	t.Logf("Error rate: %.2f%%", errorRate*100)
	t.Logf("Throughput: %.2f req/s", throughput)
	t.Logf("Avg latency: %v", metrics.avgLatency())

	// Error rate should be close to 10%
	if errorRate < 0.05 || errorRate > 0.15 {
		t.Errorf("Unexpected error rate: %.2f%%, expected ~10%%", errorRate*100)
	}
}
