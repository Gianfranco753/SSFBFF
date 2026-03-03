# ---- Build stage ----
# Compiles the transpiler, runs go generate to turn .jsonata files into native
# Go code, then builds the final server binary — all with GOEXPERIMENT=jsonv2.
FROM golang:1.25-alpine AS builder

# Install git only for dependency fetching (removed from final image)
RUN apk add --no-cache git

WORKDIR /app

# Copy dependency files first for better layer caching
COPY go.mod go.sum ./

# Download dependencies using build cache mount for faster rebuilds
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source code
COPY . .

# Set build environment
ENV GOEXPERIMENT=jsonv2
ENV CGO_ENABLED=0

# Transpile all .jsonata files from data/services/ into *_gen.go
# and generate route wiring from data/openapi.yaml and data/proxies.yaml.
RUN go generate ./internal/generated/

# Build a statically-linked binary with stripped debug info
RUN go build -ldflags="-s -w" -o /bff ./cmd/server/

# ---- Runtime stage ----
# Distroless contains nothing but the binary — no shell, no package manager.
# Routes and services are compiled into the binary.
# Data directory (providers) should be mounted as a volume at runtime.
FROM gcr.io/distroless/static-debian12:nonroot

# Copy binary only - data directory should be mounted at runtime
COPY --from=builder /bff /bff

ENV DATA_DIR=/data

EXPOSE 3000

# Use nonroot user (distroless default, explicitly set for clarity)
USER nonroot:nonroot

ENTRYPOINT ["/bff"]
