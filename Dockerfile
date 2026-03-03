# ---- Build stage ----
# Compiles the transpiler, runs go generate to turn .jsonata files into native
# Go code, then builds the final server binary — all with GOEXPERIMENT=jsonv2.
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

# Cache dependency downloads in a separate layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV GOEXPERIMENT=jsonv2

# Transpile all .jsonata files from data/services/ into *_gen.go
# and generate route wiring from data/openapi.yaml and data/proxies.yaml.
RUN go generate ./internal/generated/

# Build a statically-linked binary with stripped debug info.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bff ./cmd/server/

# ---- Runtime stage ----
# Distroless contains nothing but the binary — no shell, no package manager.
# Only data/providers/ is needed at runtime (routes and services are compiled in).
FROM gcr.io/distroless/static-debian12

COPY --from=builder /bff /bff
COPY --from=builder /app/data/providers/ /data/providers/

ENV DATA_DIR=/data

EXPOSE 3000

ENTRYPOINT ["/bff"]
