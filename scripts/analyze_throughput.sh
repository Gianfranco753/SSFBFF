#!/bin/bash
# Script to analyze CPU and memory profiles

echo "=== Running Benchmark Throughput ==="
go test -tags=goexperiment.jsonv2 -bench=BenchmarkThroughput -benchmem -cpuprofile=cpu.prof -memprofile=mem.prof ./internal/aggregator

echo "=== CPU Profile Analysis ==="
echo ""
go tool pprof -top -cum cpu.prof 2>&1 | head -30
echo ""
echo "=== Memory Profile Analysis ==="
echo ""
go tool pprof -top -cum mem.prof 2>&1 | head -30
echo ""
echo "=== CPU Profile (flat view) ==="
echo ""
go tool pprof -top cpu.prof 2>&1 | head -30
echo ""
echo "=== Memory Profile (allocations) ==="
echo ""
go tool pprof -top -alloc_space mem.prof 2>&1 | head -30
echo ""
echo "=== Summary Statistics ==="
echo ""
echo "CPU Profile size: $(ls -lh cpu.prof | awk '{print $5}')"
echo "Memory Profile size: $(ls -lh mem.prof | awk '{print $5}')"
echo ""
echo "For interactive analysis, run:"
echo "  go tool pprof -http=:8080 cpu.prof"
echo "  go tool pprof -http=:8081 mem.prof"
