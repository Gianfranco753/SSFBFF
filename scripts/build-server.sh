#!/bin/sh
set -e

# Build script for SSFBFF builder image
# This script generates code from user's data directory and builds a Docker image

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Default values
DATA_DIR="${DATA_DIR:-/data}"
OUTPUT_IMAGE=""
PUSH_IMAGE=false
REGISTRY_USER=""
REGISTRY_PASS=""
MODULE_PATH="github.com/gcossani/ssfbff"
EXCLUDE_DATA=false

# Parse arguments
while [ $# -gt 0 ]; do
  case "$1" in
    --output-image)
      OUTPUT_IMAGE="$2"
      shift 2
      ;;
    --push)
      PUSH_IMAGE=true
      shift
      ;;
    --registry-user)
      REGISTRY_USER="$2"
      shift 2
      ;;
    --registry-pass)
      REGISTRY_PASS="$2"
      shift 2
      ;;
    --module-path)
      MODULE_PATH="$2"
      shift 2
      ;;
    --data-dir)
      DATA_DIR="$2"
      shift 2
      ;;
    --exclude-data)
      EXCLUDE_DATA=true
      shift
      ;;
    --help)
      echo "Usage: $0 [OPTIONS]"
      echo ""
      echo "Options:"
      echo "  --output-image TAG     Docker image tag for the generated server (required)"
      echo "  --push                Push the image to registry after building"
      echo "  --registry-user USER  Registry username for pushing"
      echo "  --registry-pass PASS  Registry password for pushing"
      echo "  --module-path PATH     Go module path (default: github.com/gcossani/ssfbff)"
      echo "  --data-dir DIR        Path to data directory (default: /data)"
      echo "  --exclude-data         Exclude providers and openapi.yaml from final image (mount at runtime)"
      echo "  --help                Show this help message"
      exit 0
      ;;
    *)
      echo "Unknown option: $1"
      echo "Use --help for usage information"
      exit 1
      ;;
  esac
done

# Validate required arguments
if [ -z "$OUTPUT_IMAGE" ]; then
  echo "Error: --output-image is required"
  echo "Use --help for usage information"
  exit 1
fi

# Validate data directory exists
if [ ! -d "$DATA_DIR" ]; then
  echo "Error: Data directory not found: $DATA_DIR"
  exit 1
fi

# Validate required files
if [ ! -f "$DATA_DIR/openapi.yaml" ] && [ ! -f "$DATA_DIR/routes.yaml" ]; then
  echo "Error: Neither openapi.yaml nor routes.yaml found in $DATA_DIR"
  exit 1
fi

if [ ! -d "$DATA_DIR/services" ]; then
  echo "Error: services directory not found: $DATA_DIR/services"
  exit 1
fi

# Check for JSONata files
JSONATA_COUNT=$(find "$DATA_DIR/services" -name "*.jsonata" 2>/dev/null | wc -l)
if [ "$JSONATA_COUNT" -eq 0 ]; then
  echo "Error: No .jsonata files found in $DATA_DIR/services"
  exit 1
fi

echo "Building BFF server from data directory: $DATA_DIR"
echo "Output image: $OUTPUT_IMAGE"

# Create temporary build directory
BUILD_DIR=$(mktemp -d)
trap "rm -rf $BUILD_DIR" EXIT

echo "Using temporary build directory: $BUILD_DIR"

# Copy necessary source files to build directory
echo "Copying source files..."
mkdir -p "$BUILD_DIR/cmd/apigen" "$BUILD_DIR/cmd/transpiler" "$BUILD_DIR/cmd/server" \
         "$BUILD_DIR/internal/generated" "$BUILD_DIR/internal/transpiler" \
         "$BUILD_DIR/internal/aggregator" "$BUILD_DIR/runtime"

# Copy generator tools and dependencies
cp -r /builder/cmd/apigen/* "$BUILD_DIR/cmd/apigen/"
cp -r /builder/cmd/transpiler/* "$BUILD_DIR/cmd/transpiler/"
cp -r /builder/internal/transpiler/* "$BUILD_DIR/internal/transpiler/"
cp -r /builder/internal/aggregator/* "$BUILD_DIR/internal/aggregator/"
cp -r /builder/runtime/* "$BUILD_DIR/runtime/"

# Copy server source (without generated files)
cp /builder/cmd/server/main.go "$BUILD_DIR/cmd/server/"
cp /builder/cmd/server/env.go "$BUILD_DIR/cmd/server/"
cp /builder/cmd/server/health.go "$BUILD_DIR/cmd/server/"
cp /builder/cmd/server/health_test.go "$BUILD_DIR/cmd/server/" 2>/dev/null || true
cp /builder/cmd/server/metrics.go "$BUILD_DIR/cmd/server/"
cp /builder/cmd/server/metrics_test.go "$BUILD_DIR/cmd/server/" 2>/dev/null || true
cp /builder/cmd/server/metrics_batcher.go "$BUILD_DIR/cmd/server/"
cp /builder/cmd/server/middleware.go "$BUILD_DIR/cmd/server/"
cp /builder/cmd/server/server_test.go "$BUILD_DIR/cmd/server/" 2>/dev/null || true
cp /builder/cmd/server/shutdown_test.go "$BUILD_DIR/cmd/server/" 2>/dev/null || true
cp /builder/cmd/server/telemetry.go "$BUILD_DIR/cmd/server/"

# Copy go.mod and go.sum
cp /builder/go.mod "$BUILD_DIR/"
cp /builder/go.sum "$BUILD_DIR/"

# Update module path in go.mod if different
if [ "$MODULE_PATH" != "github.com/gcossani/ssfbff" ]; then
  sed -i "s|module github.com/gcossani/ssfbff|module $MODULE_PATH|g" "$BUILD_DIR/go.mod"
fi

# Copy data directory
cp -r "$DATA_DIR" "$BUILD_DIR/data"

# Ensure providers directory exists (even if empty, needed for Docker COPY)
mkdir -p "$BUILD_DIR/data/providers"

# Generate generate.go file dynamically based on JSONata files
echo "Generating generate.go from JSONata files..."
# Use the script from /usr/local/bin (in builder image) or from script directory (local)
if [ -f "/usr/local/bin/generate-generate-go.sh" ]; then
  GENERATE_SCRIPT="/usr/local/bin/generate-generate-go.sh"
else
  GENERATE_SCRIPT="$SCRIPT_DIR/generate-generate-go.sh"
fi

"$GENERATE_SCRIPT" \
  --project-root "$BUILD_DIR" \
  --data-dir "$BUILD_DIR/data" \
  --module-path "$MODULE_PATH" \
  --output "$BUILD_DIR/internal/generated/generate.go"

# Set build environment
export GOEXPERIMENT=jsonv2
export CGO_ENABLED=0

# Download dependencies
echo "Downloading Go dependencies..."
cd "$BUILD_DIR"
go mod download

# Generate code
echo "Generating code from JSONata and OpenAPI specs..."
go generate ./internal/generated/

# Build the server binary
echo "Building server binary..."
go build -ldflags="-s -w" -o "$BUILD_DIR/bff" ./cmd/server/

# Create final Docker image using multi-stage build
echo "Creating Docker image: $OUTPUT_IMAGE"

# Create a temporary Dockerfile for the final image
FINAL_DOCKERFILE="$BUILD_DIR/Dockerfile.final"
cat > "$FINAL_DOCKERFILE" << EOF
FROM gcr.io/distroless/static-debian12:nonroot

COPY bff /bff
EOF

# Include data files only if not excluded
if [ "$EXCLUDE_DATA" != "true" ]; then
  echo "COPY data/providers /data/providers" >> "$FINAL_DOCKERFILE"
  if [ -f "$BUILD_DIR/data/openapi.yaml" ]; then
    echo "COPY data/openapi.yaml /data/openapi.yaml" >> "$FINAL_DOCKERFILE"
  fi
fi

cat >> "$FINAL_DOCKERFILE" << EOF

ENV DATA_DIR=/data

EXPOSE 3000

USER nonroot:nonroot

ENTRYPOINT ["/bff"]
EOF

# Build the final image
cd "$BUILD_DIR"
docker build -f Dockerfile.final -t "$OUTPUT_IMAGE" .

echo "Successfully built image: $OUTPUT_IMAGE"

# Push image if requested
if [ "$PUSH_IMAGE" = true ]; then
  if [ -n "$REGISTRY_USER" ] && [ -n "$REGISTRY_PASS" ]; then
    echo "Logging in to registry..."
    echo "$REGISTRY_PASS" | docker login -u "$REGISTRY_USER" --password-stdin
  fi
  
  echo "Pushing image to registry..."
  docker push "$OUTPUT_IMAGE"
  echo "Successfully pushed image: $OUTPUT_IMAGE"
fi

echo "Build complete!"
