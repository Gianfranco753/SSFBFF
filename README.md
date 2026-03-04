# SSFBFF — Super Simple and Fast Backend For Frontend

A code-generation pipeline that compiles [JSONata](https://jsonata.org/) expressions into native Go functions at build time. The resulting Fiber v3 server never interprets JSONata at runtime — all JSON processing uses `encoding/json/v2` streaming with zero reflection.

## Requirements

- Go 1.25.0+ with `GOEXPERIMENT=jsonv2`

## Quick Start

```bash
# 1. Generate all code (transforms + routes)
GOEXPERIMENT=jsonv2 go generate ./internal/generated/

# 2. Start the server
GOEXPERIMENT=jsonv2 go run ./cmd/server/

# 3. Test
curl http://localhost:3000/health
curl http://localhost:3000/ready
curl http://localhost:3000/dashboard
curl http://localhost:3000/api/v1/orders

# 4. View API documentation
# Open http://localhost:3000/docs in your browser
```

## How It Works

Two generators run at build time via `go generate`:

| Generator | Input | Output |
|---|---|---|
| `cmd/transpiler` | `.jsonata` file | `_gen.go` transform function |
| `cmd/apigen` | `data/openapi.yaml` + `data/proxies.yaml` | `routes_gen.go` Fiber route wiring |

At runtime, each request either fans out to multiple upstreams in parallel or fetches from a single upstream, passes the raw bytes through the compiled transform, and returns the shaped result.

## Expression Modes

All expressions use provider mode with `$fetch()` to declare upstream dependencies. Filtering and projection can be applied to fetched data:

### Fetch mode — multiple upstreams

Uses `$fetch()` to declare upstream dependencies. All calls run in parallel at runtime:

```jsonata
{
  "user": $fetch("user_service", "profile").name,
  "balance": $fetch("bank_service", "accounts").amount
}
```

### Fetch with filtering

You can filter and project fetched data using the pattern `$fetch("provider", "endpoint")[filter].{projection}`:

```jsonata
$fetch("orders_service", "data")[price > 100].{id: order_id, total: $sum(items.price)}
```

## JSONata Extensions

### `$fetch(provider, endpoint [, config])`

Fetches data from an upstream provider. The optional third argument configures the outgoing request:

```jsonata
$fetch("payment_service", "charge", {
  "method": "POST",
  "headers": {"Authorization": $request().headers.Authorization},
  "body": {"user": $request().cookies.user_id, "amount": "100"}
}).status
```

Config keys: `method` (default `"GET"`), `headers`, `body`. Values can be static strings or `$request()` paths.

### `$request()`

Reads incoming HTTP request data via dot-path navigation:

| Path | Example |
|---|---|
| `$request().headers.Name` | `$request().headers.Authorization` |
| `$request().cookies.Name` | `$request().cookies.session` |
| `$request().query.Name` | `$request().query.page` |
| `$request().params.Name` | `$request().params.id` |
| `$request().path` | `$request().path` |
| `$request().method` | `$request().method` |
| `$request().body` | `$request().body` |
| `$request().body.field` | `$request().body.user.name` |

### `$service(name)`

Calls another generated transform pipeline in-process, enabling service composition:

```jsonata
{
  "user": $service("get_user").name,
  "balance": $fetch("bank_service", "accounts").amount
}
```

**Execution model within a service:**
1. All `$fetch()` calls run in parallel
2. All `$service()` calls run in parallel (each executes its own pipeline recursively)
3. Transform assembles the final output

Sequential provider calls are achieved by composing services — each service has a flat execution model.

## Data Directory

All BFF configuration lives in `data/`:

```
data/
├── openapi.yaml          # OpenAPI spec with x-service-name extensions
├── proxies.yaml          # Pass-through proxy routes (optional)
├── providers/            # One YAML per upstream service
│   ├── user_service.yaml
│   └── bank_service.yaml
└── services/             # JSONata expressions (one per service)
    ├── dashboard.jsonata
    ├── get_user.jsonata
    └── orders.jsonata
```

### `data/openapi.yaml`

OpenAPI 3.0 specification where each operation uses `x-service-name` to map to a JSONata service file:

```yaml
paths:
  /api/v1/orders:
    get:
      x-service-name: orders
      summary: Get filtered and projected orders
      # ... response schemas
```

The `x-service-name` value maps to `data/services/{service-name}.jsonata`.

**Request Validation**: The server automatically generates validation functions from OpenAPI request schemas (parameters and requestBody). Validation happens at runtime before the JSONata transform executes, ensuring invalid requests are rejected early with HTTP 400 errors. See the [Request Validation](#request-validation) section for details.

### `data/proxies.yaml`

Pass-through proxy routes that forward requests directly to downstream services without JSONata transformation:

```yaml
routes:
  - path: /proxy/*
    method: ALL
    url: http://downstream-service:8080
```

Each route specifies the target `url` where requests should be forwarded. The URL can include the full path, and the request path (after the route prefix) will be appended.

### `data/providers/*.yaml`

Each provider configuration supports per-endpoint timeouts, connection pool tuning, and request-scoped caching:

```yaml
# data/providers/user_service.yaml
base_url: http://user-svc:8080
timeout: 5s  # Provider-level default timeout
max_idle_conns_per_host: 1000  # Optional, overrides MAX_IDLE_CONNS_PER_HOST env var
max_conns_per_host: 2000  # Optional, overrides MAX_CONNS_PER_HOST env var
endpoints:
  # Simple string format (backward compatible)
  profile: /api/profile
  
  # Object format with per-endpoint timeout override
  slow_query:
    path: /api/slow
    timeout: 30s  # Override for slow endpoint
  
  # Object format with caching enabled
  cached_endpoint:
    path: /api/cached
    use_cache: true  # Enable request-scoped caching for this endpoint
```

**Timeout precedence**: Endpoint-level timeout > Provider-level timeout > Global default (10s)

**Request-Scoped Caching**: The `use_cache: true` option enables per-request caching for `$fetch()` calls. When multiple services or expressions call `$fetch("provider", "endpoint")` with the same configuration within the same client request, the cached response is reused instead of making duplicate HTTP calls. This is particularly useful for:

- Idempotent GET requests that are called multiple times in the same request
- Reducing upstream load when the same data is needed in multiple places
- Improving response time for duplicate calls

**Performance characteristics**:
- **Zero overhead when disabled** (default): `use_cache: false` adds no performance cost
- **Lock-free cache reads**: Uses `sync.Map` for high-concurrency scenarios (650k+ RPS)
- **Per-request scope**: Cache is automatically cleared after each request completes
- **Cache key includes**: provider, endpoint, HTTP method, headers, and body hash (ensures different configs get different cache entries)

Cache only stores successful responses (status < 400) and respects the endpoint's `use_cache` setting.

Mark a provider as non-critical with `optional: true` — failures store `null` instead of aborting:

```yaml
# data/providers/rec_service.yaml
base_url: http://rec-svc:8080
timeout: 2s
optional: true
endpoints:
  suggestions: /api/suggestions
```

**Connection Pool Configuration**: Each provider gets its own isolated HTTP client with a dedicated connection pool. This prevents one slow provider from exhausting connections needed by others. Pool sizes can be configured per-provider in YAML or globally via environment variables.

**Configuration Validation**: Provider configurations are validated at startup. Invalid configurations (malformed URLs, negative timeouts, empty endpoints, etc.) cause the server to fail with descriptive error messages. This ensures configuration errors are caught early rather than at runtime.

## Adding a New Endpoint

### 1. Write the JSONata expression

```bash
# Filter mode
echo 'users[active = true].{name: full_name, email: email}' > data/services/users.jsonata

# Or fetch mode
echo '{"owner": $fetch("user_service", "profile").name}' > data/services/summary.jsonata
```

### 2. Add a `go:generate` directive

In `internal/generated/generate.go`:

```go
//go:generate go run ../../cmd/transpiler --input=../../data/services/users.jsonata --output=users_gen.go --package=generated
```

### 3. Add the route

In `data/openapi.yaml`:

```yaml
  /api/v1/users:
    get:
      operationId: getUsers
      x-service-name: users
      summary: Get filtered users
      responses:
        "200":
          description: User list
          content:
            application/json:
              schema:
                type: array
                items:
                  type: object
```

**Optional**: Add request validation by including `parameters` and/or `requestBody` in the operation. See the [Request Validation](#request-validation) section for details.

### 4. Define providers (fetch mode only)

Create `data/providers/user_service.yaml` if it doesn't exist.

### 5. Regenerate and run

```bash
GOEXPERIMENT=jsonv2 go generate ./internal/generated/
GOEXPERIMENT=jsonv2 go run ./cmd/server/
```

The generator auto-detects the mode — if the expression contains `$fetch()` or `$service()`, it generates aggregator-aware routes; otherwise, single-upstream filter routes.

## Request Validation

The server automatically generates validation functions from OpenAPI request schemas. Validation runs at runtime before the JSONata transform executes, ensuring invalid requests are rejected early with HTTP 400 errors.

### How It Works

When you define request schemas in your OpenAPI specification (`data/openapi.yaml`), the code generator (`cmd/apigen`) automatically:

1. Parses request schemas (parameters and requestBody)
2. Generates validation functions that work directly with `RequestContext` (zero-allocation)
3. Injects validation calls into route handlers

Validation happens **before** the JSONata transform, so invalid requests never reach your business logic.

### Defining Request Schemas

Add `parameters` and `requestBody` to your OpenAPI operations:

```yaml
paths:
  /api/v1/users:
    post:
      x-service-name: create_user
      summary: Create a new user
      parameters:
        - name: X-Request-ID
          in: header
          required: true
          schema:
            type: string
        - name: page
          in: query
          schema:
            type: integer
            minimum: 1
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [email, name]
              properties:
                email:
                  type: string
                  format: email
                name:
                  type: string
                  minLength: 1
                  maxLength: 100
      responses:
        "200":
          description: User created
```

### Supported Validation Features

**Parameters (query, path, header):**
- Required field validation
- Type validation (string, integer, number, boolean)
- Constraints: `minimum`, `maximum`, `minLength`, `maxLength`, `pattern`, `enum`
- Format validation (e.g., `email`)

**Request Body:**
- Required field validation
- Type validation for object properties
- Constraints: `minLength`, `maxLength`, `minimum`, `maximum`, `pattern`, `enum`
- Format validation (e.g., `email`)
- Nested object validation

**Schema References:**
- Support for `$ref` references to `components/schemas`
- Reusable schema definitions

### Example: Complete Validation

```yaml
paths:
  /api/v1/orders:
    post:
      x-service-name: create_order
      parameters:
        - name: Authorization
          in: header
          required: true
          schema:
            type: string
        - name: page
          in: query
          schema:
            type: integer
            minimum: 1
            maximum: 1000
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [user_id, items]
              properties:
                user_id:
                  type: string
                  pattern: '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                items:
                  type: array
                  minItems: 1
                  items:
                    type: object
                    required: [product_id, quantity]
                    properties:
                      product_id:
                        type: string
                      quantity:
                        type: integer
                        minimum: 1
```

This generates validation that:
- Ensures `Authorization` header is present
- Validates `page` query param is an integer between 1 and 1000 (if provided)
- Ensures request body has required `user_id` and `items` fields
- Validates `user_id` matches UUID pattern
- Validates `items` array has at least one element
- Validates each item has required `product_id` and `quantity` fields
- Validates `quantity` is at least 1

### Validation Errors

When validation fails, the server returns HTTP 400 with a JSON error response:

```json
{
  "error": "required header 'Authorization' is missing"
}
```

Validation errors are also logged with the endpoint, method, and error details for debugging.

### Performance

Validation is designed for zero-allocation performance:

- **Header/Query/Path validation**: Direct map lookups, no allocations
- **Body validation**: Streaming JSON validation using `jsontext.Decoder` when possible, avoiding full unmarshaling
- **No data conversion**: Validation works directly with `RequestContext` maps and `[]byte`
- **Backward compatible**: Routes without schemas have zero validation overhead

### Disabling Validation

To disable validation for a route, simply omit `parameters` and `requestBody` from the OpenAPI operation. The route will work without validation (backward compatible).

## Project Structure

```
SSFBFF/
├── cmd/
│   ├── apigen/main.go              # Route generator (openapi.yaml → Fiber routes)
│   ├── server/
│   │   ├── main.go                 # Fiber v3 server entry point
│   │   ├── telemetry.go            # OpenTelemetry tracing setup
│   │   └── routes_gen.go           # Generated (gitignored)
│   └── transpiler/main.go          # JSONata → Go generator
├── data/                           # All BFF configuration
│   ├── openapi.yaml
│   ├── proxies.yaml
│   ├── providers/
│   └── services/
├── examples/
│   ├── docker-compose.yaml         # Full demo with mock upstreams
│   └── mockserver/
├── internal/
│   ├── aggregator/aggregator.go    # Parallel upstream fetcher (errgroup)
│   ├── generated/
│   │   ├── generate.go             # go:generate directives
│   │   └── *_gen.go                # Generated (gitignored)
│   └── transpiler/
│       ├── analyze.go              # AST → QueryPlan / ProviderPlan
│       ├── codegen.go              # Plan → Go source code
│       └── transpiler_test.go
├── runtime/helpers.go              # Shared types (RequestContext, ProviderDep, ExtractPath)
└── Dockerfile                      # Multi-stage: generate + compile → distroless
```

## CLI Reference

### `cmd/transpiler`

```bash
go run ./cmd/transpiler --input=<file.jsonata> --output=<file.go> --package=<pkg>
```

| Flag | Description | Default |
|---|---|---|
| `--input` | `.jsonata` source file | **(required)** |
| `--output` | Generated `.go` file | `<input>_gen.go` |
| `--package` | Package name in generated file | `main` |

### `cmd/apigen`

```bash
go run ./cmd/apigen --spec=<openapi.yaml> --jsonata-dir=<dir> [--proxies=<proxies.yaml>] --output=<file.go> --package=<pkg> --generated-pkg=<import>
```

| Flag | Description | Default |
|---|---|---|
| `--spec` | Path to OpenAPI YAML file | — |
| `--proxies` | Path to `proxies.yaml` (optional) | — |
| `--jsonata-dir` | Directory with `.jsonata` files | **(required with --spec)** |
| `--output` | Generated `.go` file | **(required)** |
| `--package` | Package name | `main` |
| `--generated-pkg` | Import path for transform functions | **(required)** |
| `--routes` | Path to `routes.yaml` (legacy mode) | — |

## Environment Variables

### Server

| Variable | Description | Default |
|---|---|---|
| `PORT` | Server listen port | `3000` |
| `DATA_DIR` | Path to the data directory | `data` |
| `MAX_IDLE_CONNS_PER_HOST` | Maximum idle connections per host in connection pool | `2000` |
| `MAX_CONNS_PER_HOST` | Maximum total connections per host in connection pool | `5000` |
| `IDLE_CONN_TIMEOUT` | Idle connection timeout (e.g., `90s`) | `90s` |
| `DIAL_TIMEOUT` | Dial timeout for new connections (e.g., `3s`) | `3s` |
| `KEEP_ALIVE` | TCP keep-alive interval (e.g., `30s`) | `30s` |
| `FIBER_PREFORK` | Enable prefork mode (multi-process, one per CPU core). **Disabled by default for containerized deployments** - scale horizontally (multiple containers) instead. Enable only if you have multiple dedicated CPU cores per container and aren't using an orchestrator | `false` |
| `FIBER_CONCURRENCY` | Maximum concurrent connections per worker | `256 * CPU count` |
| `FIBER_BODY_LIMIT` | Maximum request body size in bytes | `10485760` (10MB) |
| `MAX_RESPONSE_BODY_SIZE` | Maximum response body size in bytes for upstream responses (prevents OOM, truncation detected and returns error) | `10485760` (10MB) |
| `FIBER_READ_TIMEOUT` | Read timeout (e.g., `5s`) | `5s` |
| `FIBER_WRITE_TIMEOUT` | Write timeout (e.g., `10s`) | `10s` |
| `FIBER_IDLE_TIMEOUT` | Idle timeout (e.g., `120s`) | `120s` |
| `SHUTDOWN_TIMEOUT` | Graceful shutdown timeout (e.g., `30s`) | `30s` |
| `HEALTH_CHECK_TIMEOUT` | Health check timeout per provider (e.g., `500ms`) | `500ms` |
| `HEALTH_CHECK_FAILURE_THRESHOLD` | Maximum allowed provider failures for health check (0 = all must be healthy) | `0` |
| `ENABLE_DOCS` | Enable the `/docs` endpoint with interactive API documentation (Scalar) | `true` |

### Proxy

All upstream HTTP calls honour standard proxy environment variables:

| Variable | Description |
|---|---|
| `HTTP_PROXY` | Proxy for HTTP requests |
| `HTTPS_PROXY` | Proxy for HTTPS requests |
| `NO_PROXY` | Comma-separated hosts/domains to bypass proxy |

### OpenTelemetry

Tracing is configured via standard [OTEL environment variables](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/):

| Variable | Description | Default |
|---|---|---|
| `OTEL_SDK_DISABLED` | Set `true` to disable tracing entirely (no-op) | `false` |
| `OTEL_TRACES_EXPORTER` | Set `none` to disable tracing entirely (no-op) | — |
| `OTEL_DISABLE_TRACING` | Set `true` to disable tracing (spans created but not exported, supports per-request override) | `false` |
| `OTEL_SERVICE_NAME` | Service name attached to every span | `ssfbff` |
| `OTEL_RESOURCE_ATTRIBUTES` | Extra resource attributes (e.g. `env=prod,version=1.2`) | — |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP collector endpoint | `http://localhost:4318` |
| `OTEL_EXPORTER_OTLP_HEADERS` | Auth headers, e.g. `Authorization=Bearer <token>` | — |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Traces-specific endpoint override | — |
| `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | Traces-specific header override | — |
| `OTEL_PROPAGATE_UPSTREAM` | Inject `traceparent`/`tracestate` into requests sent to upstream microservices | `true` |
| `OTEL_PROPAGATE_DOWNSTREAM` | Inject `traceparent`/`tracestate` into HTTP responses sent back to clients | `true` |
| `USE_TRACE_ID_AS_REQUEST_ID` | Use OpenTelemetry trace ID for `X-Request-ID` header instead of generating UUIDs | `true` |

**Per-Request Tracing Override**: When `OTEL_DISABLE_TRACING=true`, you can still enable tracing for specific requests by including the `x-enable-trace: true` or `x-enable-trace: 1` header. This is useful for debugging high-load scenarios without impacting overall performance.

### Performance Tuning

The server includes several performance optimizations for high-throughput scenarios (650k+ RPS):

| Variable | Description | Default |
|---|---|---|
| `ENABLE_METRICS` | Enable Prometheus metrics recording | `true` |
| `ENABLE_ERROR_LOGGING` | Enable error logging | `true` |
| `ENABLE_RESOURCE_METRICS` | Enable resource metrics (goroutines, memory) | `true` |
| `RESOURCE_METRICS_INTERVAL` | Resource metrics collection interval (e.g., `10s`, `30s`) | `10s` |
| `METRICS_CACHE_TTL` | Cache metrics endpoint output for N seconds (0 = no cache) | `0` |
| `METRICS_BATCHING_ENABLED` | Enable batched metrics updates (reduces contention at high RPS) | `true` |
| `METRICS_BATCH_SIZE` | Batch size for metric updates before flushing | `1000` |
| `METRICS_BATCH_INTERVAL` | Max time to wait before flushing metric batch (e.g., `100ms`) | `100ms` |
| `METRICS_SAMPLE_RATE` | Metrics sampling rate (0.0-1.0, where 1.0 = no sampling) | `1.0` |
| `METRICS_LABEL_CACHE_ENABLED` | Enable label value caching to avoid WithLabelValues() lookups | `true` |
| `ASYNC_LOGGING` | Use async logging channel to avoid blocking request path | `false` |
| `ASYNC_LOGGING_BUFFER_SIZE` | Size of async logging buffer | `1000` |

### Available Metrics

The server exposes the following Prometheus metrics:

**HTTP Metrics:**
- `http_errors_total` — Counter of HTTP errors by endpoint, method, and status code

**Upstream Metrics:**
- `upstream_call_duration_seconds` — Histogram of upstream HTTP call durations by provider, endpoint, and status
- `upstream_errors_total` — Counter of upstream errors by provider, endpoint, and error type

**Aggregator Metrics:**
- `aggregator_operations_total` — Counter of aggregator operations by status (success/failure)

**Health Check Metrics:**
- `health_check_duration_seconds` — Histogram of health check durations

**Shutdown Metrics:**
- `shutdown_duration_seconds` — Histogram of graceful shutdown durations

**Metrics Batcher Metrics:**
- `metrics_dropped_total` — Counter of dropped metrics by reason (batcher_full, sampling)
- `metrics_batcher_channel_size` — Gauge of current metrics in the batcher channel

**Async Logging Metrics:**
- `async_logs_dropped_total` — Counter of async log entries dropped (channel full or during shutdown)

**Resource Metrics (if enabled):**
- `go_goroutines` — Number of goroutines
- `go_memstats_alloc_bytes` — Bytes allocated and still in use
- `go_memstats_sys_bytes` — Bytes obtained from system

**High-Throughput Configuration Example**:

```bash
# Optimize for maximum throughput (650k+ RPS)
ENABLE_METRICS=false \
ENABLE_ERROR_LOGGING=false \
ENABLE_RESOURCE_METRICS=false \
OTEL_SDK_DISABLED=true \
FIBER_PREFORK=true \
FIBER_CONCURRENCY=512 \
MAX_IDLE_CONNS_PER_HOST=5000 \
MAX_CONNS_PER_HOST=10000 \
GOEXPERIMENT=jsonv2 go run ./cmd/server/
```

**Balanced Configuration** (observability + performance):

```bash
# Good balance for production
ENABLE_METRICS=true \
ENABLE_ERROR_LOGGING=true \
ENABLE_RESOURCE_METRICS=true \
RESOURCE_METRICS_INTERVAL=30s \
METRICS_CACHE_TTL=5 \
METRICS_BATCHING_ENABLED=true \
METRICS_BATCH_SIZE=1000 \
METRICS_BATCH_INTERVAL=100ms \
METRICS_LABEL_CACHE_ENABLED=true \
ASYNC_LOGGING=true \
OTEL_DISABLE_TRACING=true \
GOEXPERIMENT=jsonv2 go run ./cmd/server/
```

**High-Throughput with Observability** (650k+ RPS with metrics enabled):

```bash
# Optimize for high throughput while maintaining observability
ENABLE_METRICS=true \
METRICS_BATCHING_ENABLED=true \
METRICS_BATCH_SIZE=2000 \
METRICS_BATCH_INTERVAL=50ms \
METRICS_SAMPLE_RATE=0.1 \
METRICS_LABEL_CACHE_ENABLED=true \
METRICS_CACHE_TTL=5 \
ENABLE_ERROR_LOGGING=true \
ASYNC_LOGGING=true \
ASYNC_LOGGING_BUFFER_SIZE=5000 \
ENABLE_RESOURCE_METRICS=false \
OTEL_DISABLE_TRACING=true \
FIBER_PREFORK=true \
FIBER_CONCURRENCY=512 \
MAX_IDLE_CONNS_PER_HOST=5000 \
MAX_CONNS_PER_HOST=10000 \
GOEXPERIMENT=jsonv2 go run ./cmd/server/
```

## API Documentation

The server provides an interactive API documentation endpoint powered by [Scalar](https://scalar.com/):

- `/docs` — Interactive API documentation showing all available endpoints from `data/openapi.yaml`

The documentation endpoint is **enabled by default** and can be disabled by setting `ENABLE_DOCS=false`. When enabled, visit `/docs` in your browser to explore all available endpoints, request/response schemas, and test API calls directly from the documentation interface.

The documentation is automatically generated from your OpenAPI specification (`data/openapi.yaml`) and reflects all endpoints defined there.

## Health Checks

The server provides three health check endpoints:

- `/health` — Basic liveness check (always returns "ok" if server is running)
- `/live` — Liveness check (always returns "ok" if server is running)
- `/ready` — Readiness check with detailed upstream provider status

### `/ready` Endpoint

The `/ready` endpoint performs health checks on all required (non-optional) upstream providers and returns detailed status information.

**When healthy** (HTTP 200):
```json
{
  "healthy": true,
  "failure_threshold": 0,
  "failure_count": 0,
  "total_required": 2,
  "providers": {
    "user_service": {
      "healthy": true,
      "status": "healthy",
      "endpoint": "profile"
    },
    "bank_service": {
      "healthy": true,
      "status": "healthy",
      "endpoint": "accounts"
    }
  }
}
```

**When unhealthy** (HTTP 503):
```json
{
  "healthy": false,
  "failure_threshold": 0,
  "failure_count": 1,
  "total_required": 2,
  "providers": {
    "user_service": {
      "healthy": true,
      "status": "healthy",
      "endpoint": "profile"
    },
    "bank_service": {
      "healthy": false,
      "status": "unhealthy",
      "error": "GET request to http://bank-svc:8080/api/accounts failed: context deadline exceeded",
      "endpoint": "accounts"
    }
  }
}
```

**Health Check Behavior:**
- Only required (non-optional) providers are checked
- Optional providers are marked as "unchecked" and don't affect health status
- Providers with no endpoints are marked as "unchecked" (logs a warning)
- Each provider is checked individually with a short timeout (configurable via `HEALTH_CHECK_TIMEOUT`)
- Overall health is determined by comparing failure count against `HEALTH_CHECK_FAILURE_THRESHOLD`
- Health check duration is recorded in the `health_check_duration_seconds` metric

## Graceful Shutdown

The server implements comprehensive graceful shutdown to ensure in-flight requests complete and resources are properly cleaned up.

**Shutdown Sequence:**
1. Stop accepting new HTTP requests (Fiber shutdown)
2. Wait for in-flight aggregator requests to complete (context cancellation)
3. Drain metrics batcher (flush remaining metric updates)
4. Shutdown async logging worker (close channel, wait for remaining entries)
5. Shutdown OpenTelemetry (flush traces)
6. Shutdown metrics (cleanup)

The entire shutdown process is bounded by `SHUTDOWN_TIMEOUT` (default 30s). Shutdown duration is recorded in the `shutdown_duration_seconds` metric.

**Configuration:**
- `SHUTDOWN_TIMEOUT` — Maximum time allowed for graceful shutdown (default: 30s)

## Error Handling

The server provides standardized error responses with error codes for programmatic handling. All errors follow a consistent format while protecting internal implementation details.

### Error Response Format

All error responses use the following JSON structure:

```json
{
  "error": "Human-readable error message",
  "status": 500,
  "code": "ERROR_CODE"
}
```

### Error Codes

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `VALIDATION_ERROR` | 400 | Request validation failed |
| `UPSTREAM_TIMEOUT` | 504 | Upstream service timeout |
| `UPSTREAM_UNAVAILABLE` | 502 | Upstream service unavailable |
| `UPSTREAM_ERROR` | 502 | Upstream service error (4xx/5xx) |
| `BAD_GATEWAY` | 502 | Gateway/proxy error |
| `INTERNAL_ERROR` | 500 | Internal server error |
| `INVALID_REQUEST` | 400 | Invalid request format |

### Error Sanitization

To protect internal implementation details, all errors are sanitized before being sent to clients:

- **Provider/endpoint names** are removed (e.g., "user_service/profile" → sanitized)
- **URLs and internal paths** are removed
- **System-level error details** are replaced with user-friendly messages
- **Full error details** are logged server-side for debugging

**Example**:

- Internal error: `user_service/profile: connection refused`
- Client receives: `{"error": "Service temporarily unavailable", "status": 502, "code": "UPSTREAM_UNAVAILABLE"}`

### Validation Errors

Validation errors include the specific field name and validation rule that failed:

```json
{
  "error": "required query parameter 'userId' is missing",
  "status": 400,
  "code": "VALIDATION_ERROR"
}
```

```json
{
  "error": "field 'email' does not match required pattern",
  "status": 400,
  "code": "VALIDATION_ERROR"
}
```

### Using Error Codes in JSONata

The `$httpError()` function supports error codes as an optional third parameter:

```jsonata
$httpError(404, "Order not found", "NOT_FOUND")
```

This returns an error response with the specified status code, message, and error code. If no error code is provided, one will be inferred from the status code.

## Tracing & Observability

The server is fully OpenTelemetry compatible out of the box:

- **Incoming requests** — the [Fiber OTel middleware](https://github.com/gofiber/contrib/tree/main/v3/otel) (`github.com/gofiber/contrib/v3/otel`) creates a server span for every request, records HTTP metrics, and extracts W3C TraceContext + Baggage headers from the incoming request so the BFF can join an existing distributed trace. When `OTEL_SDK_DISABLED=true`, the middleware is skipped entirely for maximum performance.

- **Trace IDs instead of UUIDs** — The server uses OpenTelemetry trace IDs for request correlation instead of generating UUIDs. The `otelzerolog` wrapper automatically injects `trace_id` and `span_id` into all log entries, eliminating the need for separate request ID generation. When `USE_TRACE_ID_AS_REQUEST_ID=true` (default), the trace ID is also set as the `X-Request-ID` response header for client compatibility.

- **Upstream calls** — every provider/upstream fetch becomes a child span of the active request trace via `otelhttp.NewTransport`. Span creation always happens (so the BFF records the call duration and status), while header propagation to the microservice is controlled separately (see below).

- **Correlation header propagation** — two env vars independently control whether W3C `traceparent`/`tracestate` headers are forwarded:
  - `OTEL_PROPAGATE_UPSTREAM=false` — **disable** header injection into outgoing requests to upstream microservices. Useful when upstream services do not support OTel and you want to avoid unexpected header overhead. Spans are still recorded on the BFF side.
  - `OTEL_PROPAGATE_DOWNSTREAM=false` — **disable** header injection into HTTP responses back to clients. Useful when you don't want clients to observe internal trace IDs. Spans are still recorded on the BFF side.
  - Both default to `true` (propagation enabled).

- **High-load optimization** — set `OTEL_DISABLE_TRACING=true` to disable tracing globally for maximum performance. Spans are still created (minimal overhead) but not exported. Use the `x-enable-trace` header on specific requests to enable tracing for debugging.

- **Per-provider connection pools** — each provider gets its own isolated HTTP client with a dedicated connection pool, preventing one slow provider from exhausting connections needed by others. Pool sizes are configurable per-provider in YAML or globally via environment variables.

- **Proxy support** — `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` are respected by all upstream calls, making the BFF compatible with corporate proxies and cloud egress gateways.

- **Zero-config for development** — set `OTEL_SDK_DISABLED=true` or `OTEL_TRACES_EXPORTER=none` to disable OTel with no code changes.

### Sending traces to Jaeger (example)

```bash
# Start Jaeger all-in-one
docker run -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one:latest

# Run the BFF — traces go to Jaeger automatically
OTEL_SERVICE_NAME=ssfbff \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
GOEXPERIMENT=jsonv2 go run ./cmd/server/

# View traces at http://localhost:16686
```

## Running Tests

```bash
GOEXPERIMENT=jsonv2 go test ./... -count=1
```

## Docker

Multi-stage build: generates all code, compiles a static binary, produces a minimal image (~15 MB).

```bash
# Build
docker build -t bff-app .

# Run standalone (mount data directory as volume)
docker run -p 3000:3000 -v $(pwd)/data:/data bff-app

# Or run with Docker Compose (includes mock upstreams)
docker compose -f examples/docker-compose.yaml up --build
```

The build copies `data/` into the build stage, runs `go generate` to transpile all JSONata into Go, compiles the binary, then copies only the binary into the runtime image. Routes and service logic are compiled into the binary. The `data/` directory (specifically `data/providers/`) must be mounted as a volume at runtime.

## JSONata Coverage — 86% of spec

**69 of 80** core features from the [JSONata specification](https://docs.jsonata.org/) are supported.
The remaining gaps are mainly higher-order functions and regex functions.

| Category | Supported | Total | Coverage |
|---|:---:|:---:|:---:|
| Path & Navigation | 10 | 11 | 91% |
| Comparison Operators | 7 | 7 | 100% |
| Boolean Operators | 2 | 2 | 100% |
| Arithmetic Operators | 6 | 6 | 100% |
| Other Operators | 3 | 3 | 100% |
| Literals | 5 | 6 | 83% |
| String Functions | 12 | 14 | 86% |
| Numeric Functions | 8 | 10 | 80% |
| Aggregation Functions | 5 | 5 | 100% |
| Boolean Functions | 3 | 3 | 100% |
| Array Functions | 6 | 6 | 100% |
| Object Functions | 5 | 5 | 100% |
| Higher-Order Functions | 0 | 5 | 0% |
| Date/Time Functions | 4 | 4 | 100% |
| Other | 0 | 3 | 0% |

<details>
<summary>Full feature matrix</summary>

### Path & Navigation
| Feature | Status | Example |
|---|:---:|---|
| `.` field access | ✅ | `order_id`, `items.price` |
| `[predicate]` filter | ✅ | `orders[price > 100]` |
| `{}` object constructor | ✅ | `{id: order_id}` |
| `()` grouping | ✅ | `price * (1 - discount)` |
| `[n]` array index | ✅ | `orders[0]` |
| `^()` order-by | ✅ | `orders^(price)` |
| `*` wildcard | ✅ | `address.*` |
| `**` descendant | — | not planned |
| `~>` chain | ✅ | `$ ~> $sum()` |
| `:=` binding | ✅ | `$x := price * qty` |
| `$` context | ✅ | `$.orders` |

### Operators
| Feature | Status | Example |
|---|:---:|---|
| `=` `!=` `<` `<=` `>` `>=` | ✅ | `price > 100` |
| `and` `or` | ✅ | `price > 50 and active` |
| `+` `-` `*` `/` `%` | ✅ | `price * quantity` |
| Unary `-` | ✅ | `-price` |
| `&` string concat | ✅ | `first & " " & last` |
| `? :` conditional | ✅ | `price > 100 ? "high" : "low"` |
| `in` membership | ✅ | `status in ["active", "pending"]` |
| `..` range | ✅ | `[1..5]` |

### Literals
| Feature | Status | Example |
|---|:---:|---|
| Numbers | ✅ | `42`, `3.14` |
| Strings | ✅ | `"hello"` |
| Booleans | ✅ | `true`, `false` |
| null | ✅ | `null` |
| Array literals | ✅ | `[1, 2, 3]` |
| Regex | ❌ | `/pattern/i` |

### String Functions
| Feature | Status | Example |
|---|:---:|---|
| `$string()` | ✅ | `$string(42)` → `"42"` |
| `$length()` | ✅ | `$length("hello")` → `5` |
| `$substring()` | ✅ | `$substring("hello", 0, 3)` → `"hel"` |
| `$substringBefore()` | ✅ | `$substringBefore("a-b", "-")` → `"a"` |
| `$substringAfter()` | ✅ | `$substringAfter("a-b", "-")` → `"b"` |
| `$uppercase()` | ✅ | `$uppercase("hello")` → `"HELLO"` |
| `$lowercase()` | ✅ | `$lowercase("HELLO")` → `"hello"` |
| `$trim()` | ✅ | `$trim("  hi  ")` → `"hi"` |
| `$contains()` | ✅ | `$contains("hello", "ell")` → `true` |
| `$join()` | ✅ | `$join(tags, ", ")` |
| `$pad()` | ✅ | `$pad("x", 5, "#")` |
| `$split()` | ✅ | `$split("a,b", ",")` |
| `$match()` | ❌ | `$match("abc", /[a-z]/)` |
| `$replace()` | ❌ | `$replace("hello", "l", "r")` |

### Numeric Functions
| Feature | Status | Example |
|---|:---:|---|
| `$number()` | ✅ | `$number("42")` → `42` |
| `$abs()` | ✅ | `$abs(-5)` → `5` |
| `$floor()` | ✅ | `$floor(3.7)` → `3` |
| `$ceil()` | ✅ | `$ceil(3.2)` → `4` |
| `$round()` | ✅ | `$round(3.456, 2)` → `3.46` |
| `$power()` | ✅ | `$power(2, 3)` → `8` |
| `$sqrt()` | ✅ | `$sqrt(16)` → `4` |
| `$random()` | ✅ | `$random()` |
| `$formatNumber()` | ❌ | `$formatNumber(1234.5, "#,###.00")` |
| `$parseInteger()` | ❌ | `$parseInteger("FF", 16)` |

### Aggregation Functions
| Feature | Status | Example |
|---|:---:|---|
| `$sum()` | ✅ | `$sum(items.price)` |
| `$count()` | ✅ | `$count(items)` |
| `$min()` | ✅ | `$min(items.price)` |
| `$max()` | ✅ | `$max(items.price)` |
| `$average()` | ✅ | `$average(items.price)` |

### Boolean Functions
| Feature | Status | Example |
|---|:---:|---|
| `$boolean()` | ✅ | `$boolean(0)` → `false` |
| `$not()` | ✅ | `$not(true)` → `false` |
| `$exists()` | ✅ | `$exists(field)` |

### Array Functions
| Feature | Status | Example |
|---|:---:|---|
| `$append()` | ✅ | `$append([1,2], [3,4])` |
| `$sort()` | ✅ | `$sort(items)` |
| `$reverse()` | ✅ | `$reverse([1,2,3])` |
| `$distinct()` | ✅ | `$distinct([1,1,2])` |
| `$shuffle()` | ✅ | `$shuffle([1,2,3])` |
| `$zip()` | ✅ | `$zip([1,2], [3,4])` |

### Object Functions
| Feature | Status | Example |
|---|:---:|---|
| `$keys()` | ✅ | `$keys({"a":1})` → `["a"]` |
| `$merge()` | ✅ | `$merge([{"a":1},{"b":2}])` |
| `$type()` | ✅ | `$type(42)` → `"number"` |
| `$values()` | ✅ | `$values({"a":1})` → `[1]` |
| `$spread()` | ✅ | `$spread({"a":1})` |

### Date/Time Functions
| Feature | Status | Example | Notes |
|---|:---:|---|---|
| `$now()` | ✅ | `$now()` → `"2024-01-15T10:30:00.123456789Z"` | Returns UTC timestamp in ISO 8601 format |
| `$millis()` | ✅ | `$millis()` → `1705315800123.0` | Returns milliseconds since Unix Epoch |
| `$fromMillis(number [, picture [, timezone]])` | ✅ | `$fromMillis(1705315800123)` → `"2024-01-15T10:30:00.123Z"` | **Limitation**: Picture string and timezone parameters are accepted for API compatibility but only ISO 8601 format and UTC timezone are supported for performance |
| `$toMillis(timestamp [, picture])` | ✅ | `$toMillis("2024-01-15T10:30:00.123Z")` → `1705315800123.0` | **Limitation**: Picture parameter is accepted for API compatibility but only ISO 8601 format is supported for performance |

### Missing Functions

#### String Functions (2 missing)
| Function | Effort | Notes |
|---|---|---|
| `$match()` | Medium | Requires regex support (depends on regex literal) |
| `$replace()` | Medium | Requires regex support (depends on regex literal) |

#### Numeric Functions (2 missing)
| Function | Effort | Notes |
|---|---|---|
| `$formatNumber()` | High | Complex formatting patterns (e.g., `"#,###.00"`) |
| `$parseInteger()` | Medium | Base conversion parsing |

#### Higher-Order Functions (5 missing)
| Function | Effort | Notes |
|---|---|---|
| `$map()` | High | Requires lambda expression support |
| `$filter()` | Medium | Can be partially covered by `[predicate]` syntax |
| `$reduce()` | High | Requires lambda expression support |
| `$sift()` | High | Requires lambda expression support |
| `$each()` | High | Requires lambda expression support |

**Higher-Order Functions Effort**: These require implementing lambda expressions (anonymous functions), which is a significant architectural change. Estimated effort: **High** (2-3 weeks for full lambda support).

#### Date/Time Functions
| Function | Status | Example | Notes |
|---|---|---|---|
| `$now()` | ✅ | `$now()` → `"2024-01-15T10:30:00.123456789Z"` | Returns UTC timestamp in ISO 8601 format |
| `$millis()` | ✅ | `$millis()` → `1705315800123.0` | Returns milliseconds since Unix Epoch |
| `$fromMillis(number [, picture [, timezone]])` | ✅ | `$fromMillis(1705315800123)` → `"2024-01-15T10:30:00.123Z"` | **Limitation**: Picture string and timezone parameters are accepted for API compatibility but only ISO 8601 format and UTC timezone are supported for performance |
| `$toMillis(timestamp [, picture])` | ✅ | `$toMillis("2024-01-15T10:30:00.123Z")` → `1705315800123.0` | **Limitation**: Picture parameter is accepted for API compatibility but only ISO 8601 format is supported for performance |

#### Other Missing Features
| Feature | Effort | Notes |
|---|---|---|
| Regex literals (`/pattern/i`) | Medium | Requires regex parsing and compilation |
| Lambda expressions | High | Required for higher-order functions |
| `$eval()` | High | Dynamic expression evaluation (security concerns) |

### Implementation Priority Recommendations

**Quick Wins (Low effort, high value):**
- ✅ Date/Time functions (4 functions) - **Implemented**
- ✅ `$values()`, `$spread()`, `$shuffle()`, `$zip()` (4 functions) - **Implemented**
- ✅ `$power()`, `$sqrt()`, `$random()` (3 functions) - **Implemented**
- ✅ `$pad()`, `$split()` (2 functions) - **Implemented**

**Medium Priority:**
1. `$match()`, `$replace()` (requires regex literal support first)
2. `$parseInteger()` (base conversion)

**High Priority (Architectural):**
1. Lambda expressions (enables all higher-order functions)
2. Regex literal support (enables `$match()`, `$replace()`)

**Not Recommended:**
- `$eval()` - Security risk, dynamic evaluation conflicts with compile-time generation model
- `$formatNumber()` - Complex formatting patterns, low usage

</details>

### BFF Extensions (not part of JSONata spec)

| Feature | Example |
|---|---|
| `$fetch(provider, endpoint)` | `$fetch("user_service", "profile").name` |
| `$fetch()` with config | `$fetch("svc", "ep", {"method": "POST"}).val` |
| `$request()` context | `$request().headers.Authorization` |
| `$service(name)` composition | `$service("get_user").name` |
| `$httpError(statusCode, message, code?)` | `$httpError(404, "Not found", "NOT_FOUND")` |
| `$httpResponse(statusCode, body, headers?)` | `$httpResponse(201, $fetch("orders", "create"))` |

#### `$httpError(statusCode, message, code?)`

Returns an HTTP error response with the specified status code and message. An optional error code can be provided for programmatic handling. Use in conditionals to return errors when conditions aren't met:

```jsonata
$count($fetch("orders_service", "data")[order_id = $request().params.id]) = 0 
  ? $httpError(404, "Order not found", "NOT_FOUND")
  : $fetch("orders_service", "data")[order_id = $request().params.id][0]
```

If no error code is provided, one will be inferred from the status code (e.g., 404 → `INVALID_REQUEST`, 500 → `INTERNAL_ERROR`).

**Note**: This is a BFF extension function. JSONata has a built-in `$error()` function that throws exceptions, so we use `$httpError()` to avoid conflicts.

#### `$httpResponse(statusCode, body, headers?)`

Returns a custom HTTP response with status code, body, and optional headers. Use for non-200 responses, redirects, or custom headers:

```jsonata
// 403 Forbidden
$fetch("user_service", "permissions").can_view_order = false
  ? $httpResponse(403, {"error": "Access denied"}, {"X-Reason": "insufficient_permissions"})
  : $fetch("orders_service", "data")[order_id = $request().params.id][0]

// 201 Created
$httpResponse(201, $fetch("orders_service", "create", {
  "method": "POST",
  "body": $request().body
}), {"Location": $request().path & "/" & $fetch("orders_service", "create").id})

// 301 Redirect
$httpResponse(301, null, {"Location": "https://example.com/new-path"})

// 204 No Content
$httpResponse(204, null)
```

**Note**: Normal data returns default to 200 OK. You only need to use `$httpError()` or `$httpResponse()` when you want non-default status codes, custom headers, or redirects.