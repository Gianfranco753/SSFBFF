#!/bin/sh
# Smoke test: start the BFF server and hit built-in endpoints.
# Uses DATA_DIR=examples/data and READY_SKIP_UPSTREAM_CHECK=true so /ready passes without upstreams.
# Run from project root.

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

PORT="${PORT:-3099}"
BASE="http://127.0.0.1:$PORT"

# Start server in background
export DATA_DIR="${DATA_DIR:-examples/data}"
export PORT="$PORT"
export READY_SKIP_UPSTREAM_CHECK=true
export ENABLE_DOCS=true

GOEXPERIMENT=jsonv2 go run ./cmd/server/ &
PID=$!
trap 'kill $PID 2>/dev/null; wait $PID 2>/dev/null' EXIT

# Wait for listen
for i in 1 2 3 4 5 6 7 8 9 10; do
	if curl -s -o /dev/null -w "%{http_code}" "$BASE/health" 2>/dev/null | grep -q 200; then
		break
	fi
	sleep 1
done

# Expect 200 for these
for path in health live metrics ready docs; do
	code=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/$path")
	if [ "$code" != "200" ]; then
		echo "FAIL $path: expected 200, got $code"
		exit 1
	fi
	echo "OK $path"
done

echo "Smoke test passed."
