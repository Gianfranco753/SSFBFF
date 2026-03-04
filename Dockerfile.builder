# Builder image for SSFBFF
# This image contains the generator tools and build script
# Users mount their /data directory and this image generates code and builds a Docker image
FROM golang:1.25-alpine

# Install git and docker-cli (for building/pushing images)
RUN apk add --no-cache git docker-cli

WORKDIR /builder

# Copy dependency files
COPY go.mod go.sum ./

# Download dependencies using build cache mount for faster rebuilds
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy generator tools and dependencies
COPY cmd/apigen ./cmd/apigen
COPY cmd/transpiler ./cmd/transpiler
COPY internal/transpiler ./internal/transpiler
COPY internal/aggregator ./internal/aggregator
COPY runtime ./runtime

# Copy server source files (needed for building)
COPY cmd/server/main.go ./cmd/server/
COPY cmd/server/env.go ./cmd/server/
COPY cmd/server/health.go ./cmd/server/
COPY cmd/server/metrics.go ./cmd/server/
COPY cmd/server/metrics_batcher.go ./cmd/server/
COPY cmd/server/middleware.go ./cmd/server/
COPY cmd/server/telemetry.go ./cmd/server/

# Copy build scripts
COPY scripts/build-server.sh /usr/local/bin/build-server.sh
COPY scripts/generate-generate-go.sh /usr/local/bin/generate-generate-go.sh

# Set build environment
ENV GOEXPERIMENT=jsonv2
ENV CGO_ENABLED=0

# Make scripts executable
RUN chmod +x /usr/local/bin/build-server.sh /usr/local/bin/generate-generate-go.sh

# Set entrypoint to build script
ENTRYPOINT ["/usr/local/bin/build-server.sh"]
