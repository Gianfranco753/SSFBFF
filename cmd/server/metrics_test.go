//go:build goexperiment.jsonv2

package main

import (
	"os"
	"testing"
	"time"
)

func BenchmarkRecordHTTPError(b *testing.B) {
	os.Setenv("ENABLE_METRICS", "true")
	os.Setenv("METRICS_BATCHING_ENABLED", "false")
	os.Setenv("METRICS_LABEL_CACHE_ENABLED", "false")
	metricsEnabled = true
	initMetricsBatcher()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordHTTPError("/api/test", "GET", 500)
		}
	})
}

func BenchmarkRecordHTTPErrorWithBatching(b *testing.B) {
	os.Setenv("ENABLE_METRICS", "true")
	os.Setenv("METRICS_BATCHING_ENABLED", "true")
	os.Setenv("METRICS_LABEL_CACHE_ENABLED", "false")
	metricsEnabled = true
	initMetricsBatcher()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordHTTPError("/api/test", "GET", 500)
		}
	})
}

func BenchmarkRecordHTTPErrorWithCache(b *testing.B) {
	os.Setenv("ENABLE_METRICS", "true")
	os.Setenv("METRICS_BATCHING_ENABLED", "false")
	os.Setenv("METRICS_LABEL_CACHE_ENABLED", "true")
	metricsEnabled = true
	labelCacheEnabled = true
	initMetricsBatcher()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordHTTPError("/api/test", "GET", 500)
		}
	})
}

func BenchmarkRecordHTTPErrorWithBatchingAndCache(b *testing.B) {
	os.Setenv("ENABLE_METRICS", "true")
	os.Setenv("METRICS_BATCHING_ENABLED", "true")
	os.Setenv("METRICS_LABEL_CACHE_ENABLED", "true")
	metricsEnabled = true
	labelCacheEnabled = true
	initMetricsBatcher()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordHTTPError("/api/test", "GET", 500)
		}
	})
}

func BenchmarkRecordUpstreamCall(b *testing.B) {
	os.Setenv("ENABLE_METRICS", "true")
	os.Setenv("METRICS_BATCHING_ENABLED", "false")
	os.Setenv("METRICS_LABEL_CACHE_ENABLED", "false")
	metricsEnabled = true
	initMetricsBatcher()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordUpstreamCall("provider1", "/endpoint", 100*time.Millisecond, "success")
		}
	})
}

func BenchmarkRecordUpstreamCallWithBatching(b *testing.B) {
	os.Setenv("ENABLE_METRICS", "true")
	os.Setenv("METRICS_BATCHING_ENABLED", "true")
	os.Setenv("METRICS_LABEL_CACHE_ENABLED", "false")
	metricsEnabled = true
	initMetricsBatcher()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordUpstreamCall("provider1", "/endpoint", 100*time.Millisecond, "success")
		}
	})
}

func BenchmarkRecordUpstreamCallWithCache(b *testing.B) {
	os.Setenv("ENABLE_METRICS", "true")
	os.Setenv("METRICS_BATCHING_ENABLED", "false")
	os.Setenv("METRICS_LABEL_CACHE_ENABLED", "true")
	metricsEnabled = true
	labelCacheEnabled = true
	initMetricsBatcher()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordUpstreamCall("provider1", "/endpoint", 100*time.Millisecond, "success")
		}
	})
}

func BenchmarkRecordUpstreamCallWithBatchingAndCache(b *testing.B) {
	os.Setenv("ENABLE_METRICS", "true")
	os.Setenv("METRICS_BATCHING_ENABLED", "true")
	os.Setenv("METRICS_LABEL_CACHE_ENABLED", "true")
	metricsEnabled = true
	labelCacheEnabled = true
	initMetricsBatcher()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordUpstreamCall("provider1", "/endpoint", 100*time.Millisecond, "success")
		}
	})
}

func BenchmarkRecordUpstreamCallWithSampling(b *testing.B) {
	os.Setenv("ENABLE_METRICS", "true")
	os.Setenv("METRICS_BATCHING_ENABLED", "true")
	os.Setenv("METRICS_LABEL_CACHE_ENABLED", "true")
	os.Setenv("METRICS_SAMPLE_RATE", "0.1")
	metricsEnabled = true
	labelCacheEnabled = true
	initMetricsBatcher()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordUpstreamCall("provider1", "/endpoint", 100*time.Millisecond, "success")
		}
	})
}
