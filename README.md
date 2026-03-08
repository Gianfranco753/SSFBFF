# SSFBFF — Super Simple and Fast Backend For Frontend

A code-generation pipeline that compiles [JSONata](https://jsonata.org/) expressions into native Go functions at build time. The resulting Fiber v3 server never interprets JSONata at runtime — all JSON processing uses `encoding/json/v2` streaming with zero reflection.

## Requirements

- Go 1.25.0+ with `GOEXPERIMENT=jsonv2`

## Quick Start

```bash
# 1. Generate all code (transforms + routes)
# If using the example data (no top-level data/), create generate.go from examples/data first:
./scripts/generate-generate-go.sh --data-dir examples/data
GOEXPERIMENT=jsonv2 go generate ./internal/generated/

# 2. Start the server
# DATA_DIR must point to a directory containing openapi.yaml and providers/
DATA_DIR=examples/data GOEXPERIMENT=jsonv2 go run ./cmd/server/

# 3. Test built-in endpoints
curl http://localhost:3000/health
curl http://localhost:3000/ready
curl http://localhost:3000/metrics
# OpenAPI and proxy routes require upstreams to be running; without them you get 502.
curl http://localhost:3000/dashboard   # needs user/bank (or mocks)
curl http://localhost:3000/api/v1/orders

# 4. View API documentation (set ENABLE_DOCS=true to serve /docs)
ENABLE_DOCS=true curl -s http://localhost:3000/docs | head -1

# Smoke test (built-in endpoints only; uses READY_SKIP_UPSTREAM_CHECK so /ready passes without upstreams)
./scripts/smoke-test.sh
```

## How It Works

Two generators run at build time via `go generate`:

| Generator | Input | Output |
|---|---|---|
| `cmd/transpiler` | `.jsonata` file | `_gen.go` transform function |
| `cmd/apigen` | `data/openapi.yaml` + `data/proxies.yaml` | `routes_gen.go` Fiber route wiring |

At runtime, each request either fans out to multiple upstreams in parallel or fetches from a single upstream, passes the raw bytes through the compiled transform, and returns the shaped result.

## Expression Modes

The **top-level program** can be any valid JSONata expression. The HTTP response body is that value: an object, an array, or a primitive (string, number, boolean, null). You can use a block `( expr1; expr2; ...; lastExpr )` so earlier expressions are variable bindings and the last expression is the result.

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

You can filter and project fetched data using the pattern `$fetch("provider", "endpoint")[filter].{projection}`. Projection only (no filter) is also supported: `$fetch("provider", "endpoint").{projection}`.

```jsonata
$fetch("orders_service", "data")[price > 100].{id: order_id, total: $sum(items.price)}
```

```jsonata
$fetch("orders_service", "data").{id: userId, title: title}
```

**Provider response shape**: Upstream responses can be any valid JSON. When `$fetch` is used as a value (e.g. `{"value": $fetch("p","e")}`), that value is passed through to the client (e.g. provider returns `1` → client gets `{"value": 1}`). When `$fetch` is used with projection only and the provider returns a single object, the BFF returns one projected object (e.g. `{"id": 1, "title": "tit"}`), not an array. When `$fetch` is used in an array pipeline with a filter, a root-level array or an object with the endpoint key holding an array is processed; a scalar at root yields an empty list (no error).

### Range + array map

You can return an array built from a numeric range in two ways:

- **Object projection** — `[a..b].{key: value}`: the response body is a JSON array of objects; each element is the projection with `$` bound to the current number (inclusive).
- **Generic array-map** — `[a..b].(expr)` or chained maps `[0..n].f($).g($)`: the first step is an array or range; each following step is a transform applied to every element, with `$` as the current element. The transform can be any expression (literal, function call, arithmetic, etc.).

Examples:

```jsonata
[0..24].{index: $, label: $string($)}
```

```jsonata
[1..5].($ * 2)
```

With bindings for the range bounds:

```jsonata
($start := 1; $end := 5; [$start..$end].{n: $})
```

**Important**: Generator expressions that only fetch data without any transformation are not allowed and will result in a build-time error. Field access (e.g., `$fetch(...).field`) is considered transformation and is allowed. If you need to pass data through completely unchanged, use a proxy route in `proxies.yaml` instead. For example:

- ❌ **Invalid**: `$fetch("provider", "endpoint")` (completely bare fetch without any transformation)
- ✅ **Valid**: `$fetch("provider", "endpoint").field` (field access is transformation)
- ✅ **Valid**: `$fetch("provider", "endpoint").{projection}` (projection only, no filter)
- ✅ **Valid**: `$fetch("provider", "endpoint")[filter].{projection}` (has filter and projection)
- ✅ **Valid**: `{"data": $fetch("provider", "endpoint").field}` (inside object literal, mapping data)
- ✅ **Valid**: `[1, 2, 3]` or `( $x := 1; [ $x, 2, 3 ] )` — top-level array (response body is the array)
- ✅ **Valid**: `[0..24].{index: $}` — range + array map (response body is array of objects)
- ✅ **Valid**: `[1..10].($ * 2)` or `[0..n].f($).g($)` — generic array-map paths (array/range then transform steps)
- ✅ **Valid**: `42` or a block whose last expression is a number or string — response body is that value
- ✅ **Use proxy**: For complete pass-through without any transformation, define a route in `data/proxies.yaml`

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

Config keys: `method` (default `"GET"`), `headers`, `body`. Values can be static strings or `$request()` paths. Inside child services, fetch config values can also read `$params()`.

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

### `$service(name, params?)`

Calls another generated transform pipeline in-process, enabling service composition. The optional second argument is an explicit params object that the child service can read via `$params()`:

```jsonata
{
  "user": $service("get_user", {
    "auth": $request().headers.Authorization
  }).name,
  "balance": $fetch("bank_service", "accounts").amount
}
```

`params` should be a plain value object built from literals, `$request()`, `$params()`, and simple expression composition. It must not contain `$fetch()` calls; fetch the value in the parent service first and then pass the result explicitly.

### `$params()`

Reads the immutable params object passed to a child service by `$service(name, params?)`:

```jsonata
{
  "id": $fetch("user_service", "profile", {
    "headers": {"Authorization": $params().auth}
  }).id,
  "name": $fetch("user_service", "profile", {
    "headers": {"Authorization": $params().auth}
  }).name
}
```

**Execution model within a service:**
1. All `$fetch()` calls run in parallel
2. All `$service()` calls run in parallel (each executes its own pipeline recursively)
3. Transform assembles the final output

Sequential provider calls are achieved by composing services — each service has a flat execution model. Child params are explicit and immutable, so sibling `$service()` calls never communicate through shared mutable request state.

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

You can use `/* ... */` comments in `.jsonata` files for documentation; they are removed at build time.

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

**Slow Request Threshold Override**: You can override the global slow request threshold per route using the `x-slow-request-threshold` extension. This allows fine-grained control over which requests are considered slow:

```yaml
paths:
  /api/v1/orders:
    get:
      x-service-name: orders
      x-slow-request-threshold: 500ms  # Override default threshold for this route
      summary: Get orders
```

The threshold accepts duration format (e.g., `500ms`, `2s`, `1.5s`). If not specified, the route uses the global `SLOW_REQUEST_THRESHOLD` environment variable (default: `1s`). Requests exceeding the threshold are recorded in the `slow_requests_total` metric.

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

**Path templates**: Endpoint paths may contain placeholders like `{order_id}`. When a route has path parameters (e.g. OpenAPI path `/api/v1/orders/:order_id`), the BFF passes them into the aggregator and substitutes each placeholder with the value from the request (e.g. `params["order_id"]`). Placeholder names in the provider path must match the route's path parameter names. Example: `data: /posts/{order_id}` so that `GET /api/v1/orders/42` requests `https://example.com/posts/42`. No change to JSONata is required; the same `$fetch("provider", "endpoint")` works.

**Request-Scoped Caching**: The `use_cache: true` option enables per-request caching for `$fetch()` calls. When multiple services or expressions call `$fetch("provider", "endpoint")` with the same configuration within the same client request, the cached response is reused instead of making duplicate HTTP calls. This is particularly useful for:

- Idempotent GET requests that are called multiple times in the same request
- Reducing upstream load when the same data is needed in multiple places
- Improving response time for duplicate calls

**Performance characteristics**:
- **Zero overhead when disabled** (default): `use_cache: false` adds no performance cost
- **Lock-free cache reads**: Uses `sync.Map` for high-concurrency scenarios (650k+ RPS)
- **Per-request scope**: Cache is automatically cleared after each request completes
- **Cache key includes**: provider, endpoint, HTTP method, headers, body hash, and (when the path has placeholders) the resolved URL (ensures different configs and path-param values get different cache entries)

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
# Pure transform expression
echo '$fetch("user_service", "profile")[active = true].{name: full_name, email: email}' > data/services/users.jsonata

# Expression with upstream fetch
echo '{"owner": $fetch("user_service", "profile").name}' > data/services/summary.jsonata
```

### 2. Regenerate `generate.go` file

The `internal/generated/generate.go` file contains `go:generate` directives for each JSONata file. After adding a new JSONata file, regenerate it:

```bash
./scripts/generate-generate-go.sh
```

This script automatically scans `data/services/` and updates `generate.go` with the correct directives. Alternatively, you can manually add the directive to `internal/generated/generate.go`:

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

### 4. Define providers

Create `data/providers/user_service.yaml` if it doesn't exist.

### 5. Regenerate and run

```bash
# Regenerate generate.go if you added a new JSONata file
./scripts/generate-generate-go.sh

# Generate all code (transforms + routes)
GOEXPERIMENT=jsonv2 go generate ./internal/generated/

# Start the server
GOEXPERIMENT=jsonv2 go run ./cmd/server/
```

**Note:** The `generate-generate-go.sh` script automatically keeps `internal/generated/generate.go` in sync with your `data/services/` directory. Run it whenever you add, remove, or rename JSONata files.

The generator automatically detects whether the expression depends on upstream services. Expressions that use `$fetch()` or `$service()` generate aggregator-aware routes; expressions without upstream dependencies generate direct transform routes.

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

Source files may contain C-style block comments (`/* ... */`); they are stripped before parsing.

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
| `SLOW_REQUEST_THRESHOLD` | Duration threshold for slow request detection (e.g., `1s`, `500ms`). Requests exceeding this duration are recorded in `slow_requests_total` metric | `1s` |
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

### OpenFeature

OpenFeature integration allows environment variables to be overridden by feature flags at runtime. When OpenFeature is not configured, the system uses the standard environment variable caching (zero overhead).

| Variable | Description | Default |
|---|---|---|
| `OPENFEATURE_PROVIDER` | Provider type: `"envvar"`, `"inmemory"`, or `"http"`. If not set, OpenFeature is disabled | — |
| `OPENFEATURE_CACHE_TTL` | Cache TTL in seconds for flag values (0 = no cache, enables instant flag changes). Recommended: 60-300 for high performance | `0` |

**How it works:**
- If `OPENFEATURE_PROVIDER` is not set, the system behaves exactly as before (all values from cached env vars)
- If OpenFeature is enabled, flag values take precedence over environment variables
- If a flag is not found in OpenFeature, the system falls back to the cached environment variable value
- **Push/Streaming Priority**: When providers support eventing (implement `EventHandler`), flag changes are pushed via events and cache is invalidated immediately for real-time updates
- **TTL Fallback**: When `OPENFEATURE_CACHE_TTL=0`, flags are evaluated on every access. When `OPENFEATURE_CACHE_TTL>0`, TTL caching is used as a fallback for providers without push/streaming support
- Flag keys use the same names as environment variables (e.g., `PORT`, `LOG_LEVEL`, `MAX_IDLE_CONNS_PER_HOST`)

**Evaluation Model (Push/Streaming Priority):**
- **In-process**: All flag evaluation happens synchronously within the same process (no external calls during evaluation for built-in providers)
- **Push/Streaming Priority**: The implementation prioritizes push/streaming updates via OpenFeature's eventing system. When providers emit `ProviderConfigChange` events, cache entries are invalidated immediately, ensuring real-time flag updates
- **TTL Fallback**: When `OPENFEATURE_CACHE_TTL>0`, TTL caching is used as a fallback mechanism for providers that don't support push/streaming events. Cache entries are invalidated by events when available, or expire after TTL when events are not supported
- **Event-driven cache invalidation**: Providers that implement the `EventHandler` interface can emit events when flags change. The system listens to these events and invalidates the cache for changed flags immediately, ensuring instant updates without waiting for TTL expiration
- **Provider support**: External providers (e.g., LaunchDarkly, Split.io) that support push/streaming will automatically benefit from real-time updates. Built-in providers (`envvar`, `inmemory`) can be extended to support eventing

**Provider Types:**

1. **`envvar`** - Reads flags from environment variables
   - Useful for testing or when you want flags to come from env vars
   - Example: `OPENFEATURE_PROVIDER=envvar`
   - Flags are read from environment variables with the same name (e.g., `PORT` flag reads from `PORT` env var)

2. **`inmemory`** - Simple in-memory provider (currently empty, can be extended)
   - Best for testing or simple use cases
   - Example: `OPENFEATURE_PROVIDER=inmemory`
   - Note: This provider currently has no flags configured by default. You would need to extend the implementation to populate flags.

3. **`http`** - HTTP-based provider (placeholder, requires implementation)
   - For integration with external feature flag services
   - Example: `OPENFEATURE_PROVIDER=http`
   - Note: This is a placeholder and requires additional implementation to connect to an actual HTTP-based feature flag service.

**Configuration Examples:**

**Basic setup with envvar provider:**
```bash
export OPENFEATURE_PROVIDER=envvar
export OPENFEATURE_CACHE_TTL=60
# Now flags can override env vars
export PORT=8080  # This will be used as the flag value
```

**High-performance setup with caching:**
```bash
export OPENFEATURE_PROVIDER=inmemory
export OPENFEATURE_CACHE_TTL=300  # Cache for 5 minutes
```

**Instant flag changes (no cache):**
```bash
export OPENFEATURE_PROVIDER=envvar
export OPENFEATURE_CACHE_TTL=0  # No caching, changes take effect immediately
```

**Using with Docker:**
```bash
docker run -e OPENFEATURE_PROVIDER=envvar \
           -e OPENFEATURE_CACHE_TTL=60 \
           -e PORT=3000 \
           your-image
```

**Flag Naming Convention:**
- Flags use the same names as environment variables
- Example: To override `LOG_LEVEL`, create a flag named `LOG_LEVEL` in your provider
- Example: To override `MAX_IDLE_CONNS_PER_HOST`, create a flag named `MAX_IDLE_CONNS_PER_HOST`

**Performance Recommendations for High Throughput:**

1. **Use in-memory or file-based providers** for hot-path variables (avoid HTTP provider for variables accessed in request handling)
2. **Set `OPENFEATURE_CACHE_TTL=60` or higher** for high-throughput scenarios to minimize flag evaluation overhead
3. **Only enable OpenFeature for variables that need runtime changes** - keep static configuration in environment variables
4. **Keep critical path variables (connection pool sizes, timeouts) with TTL > 0** to maintain performance

**Performance Impact:**
- **OpenFeature disabled**: Zero overhead (same performance as baseline)
- **OpenFeature enabled, TTL = 0**: ~100-1000ns+ per call (depends on provider)
- **OpenFeature enabled, TTL > 0**: Cache hits ~5-10ns (similar to baseline), cache misses ~100-1000ns+

**Advanced: Using External Providers**

To use external feature flag services (e.g., LaunchDarkly, Split.io, etc.), you'll need to:
1. Install the provider package: `go get github.com/open-feature/go-sdk-contrib/providers/<provider-name>`
2. Modify `cmd/server/openfeature.go` to import and initialize the external provider
3. Set `OPENFEATURE_PROVIDER` to your provider type and configure provider-specific environment variables

See the [OpenFeature providers documentation](https://openfeature.dev/docs/reference/concepts/provider) for available providers.

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
- `http_request_duration_seconds` — Histogram of HTTP request durations by endpoint, method, and status code
- `http_response_size_bytes` — Histogram of HTTP response body sizes by endpoint, method, and status code
- `slow_requests_total` — Counter of slow requests that exceeded the threshold by endpoint and method

**Metrics and Path Parameters:**

All HTTP metrics use static route template paths (e.g., `/api/v1/users/{id}`) instead of dynamic resolved paths (e.g., `/api/v1/users/123`). This prevents metric cardinality explosion when endpoints have path parameters. For example, requests to `/api/v1/users/123`, `/api/v1/users/456`, etc. are all aggregated under the `/api/v1/users/{id}` metric label.

Routes defined in OpenAPI automatically use their template paths for metrics. Routes not defined via OpenAPI (e.g., `/health`, `/metrics`) use their actual paths since they don't have parameters.

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

**Local development:** If upstreams are not running, `/ready` returns 503. Set `READY_SKIP_UPSTREAM_CHECK=true` to report ready without probing upstreams (server listening only).

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

- **Enhanced upstream error logging** — when upstream requests fail, logs include request method, sanitized headers (sensitive headers like Authorization are redacted), and request body size for improved debugging without exposing sensitive data.

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

## Cleaning Generated Files

To remove all generated files, binaries, and test artifacts:

```bash
./scripts/clean.sh
```

This script removes:
- The entire `internal/generated/` directory and all its contents
- `cmd/server/routes_gen.go`
- Binaries (`apigen`, `transpiler`, `server`, `mockserver`)
- Test binaries (`*.test`)
- Coverage files (`coverage.out`, `*.coverprofile`)

**Note:** The `internal/generated/` directory will be recreated by `generate-generate-go.sh` when you regenerate files.

## Docker

Multi-stage build: generates all code, compiles a static binary, produces a minimal image (~15 MB).

### Building Locally

To create the image locally (using podman or docker):

```bash
./scripts/generate-generate-go.sh && GOEXPERIMENT=jsonv2 go generate ./internal/generated/ && GOEXPERIMENT=jsonv2 go test ./... && podman build -t bff-app . && ./scripts/clean.sh
```

This command:
1. Regenerates `generate.go` from your JSONata files
2. Generates all transform and route code
3. Runs tests
4. Builds the Docker image
5. Cleans up generated files

### Running the Image

```bash
# Build
docker build -t bff-app .

# Run standalone (mount data directory as volume)
docker run -p 3000:3000 -v $(pwd)/data:/data bff-app

# Or run with Docker Compose (includes mock upstreams)
docker compose -f examples/docker-compose.yaml up --build
```

The build copies `data/` into the build stage, runs `go generate` to transpile all JSONata into Go, compiles the binary, then copies only the binary into the runtime image. Routes and service logic are compiled into the binary. The `data/` directory (specifically `data/providers/`) must be mounted as a volume at runtime.

## Distribution

You can use SSFBFF to generate BFF servers from your data directory without needing the generator source code. Two methods are available:

### Method 1: Docker Builder Image

Use the pre-built builder image to generate and build your BFF server from your data directory:

```bash
# Build your BFF server image
docker run --rm \
  -v /path/to/your/data:/data:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \
  gcossani/ssfbff-builder:latest \
  --output-image my-bff:latest
```

**Options:**
- `--output-image TAG` - Docker image tag for the generated server (required)
- `--push` - Push the image to registry after building
- `--registry-user USER` - Registry username for pushing
- `--registry-pass PASS` - Registry password for pushing
- `--module-path PATH` - Go module path (default: `github.com/gcossani/ssfbff`)
- `--data-dir DIR` - Path to data directory (default: `/data`)

**Example with push:**
```bash
docker run --rm \
  -v /path/to/your/data:/data:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \
  gcossani/ssfbff-builder:latest \
  --output-image myorg/my-bff:v1.0.0 \
  --push \
  --registry-user myuser \
  --registry-pass mypassword
```

**Requirements:**
- Your `data/` directory must contain:
  - `openapi.yaml` (or `routes.yaml` for legacy mode)
  - `services/` directory with `.jsonata` files
  - `providers/` directory with provider YAML files (optional, but required if using `$fetch()`)

**Final Image Contents:**
By default, the generated Docker image includes:
- The compiled server binary
- `data/providers/` directory (required at runtime)
- `data/openapi.yaml` (if present, for `/docs` endpoint)

The `data/services/` directory is not included since JSONata expressions are compiled into the binary at build time.

**Excluding Data from Image:**
You can exclude providers and openapi.yaml from the image using the `--exclude-data` flag. This is useful when you want to:
- Use environment-specific configurations (dev/staging/prod)
- Keep sensitive provider configs out of the image
- Mount data at runtime for flexibility

```bash
docker run --rm \
  -v /path/to/your/data:/data:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \
  gcossani/ssfbff-builder:latest \
  --output-image my-bff:latest \
  --exclude-data
```

### Method 2: GitHub Actions

Automatically build and push your BFF server image when you push changes to your data directory.

1. **Copy the workflow template** to your repository:
   ```bash
   mkdir -p .github/workflows
   cp .github/workflows/build.yml your-repo/.github/workflows/
   ```

2. **Set up repository secrets** in GitHub:
   - `DOCKER_USERNAME` - Your Docker Hub username
   - `DOCKER_PASSWORD` - Your Docker Hub password or access token
   - `IMAGE_NAME` (optional) - Your Docker image name (e.g., `myorg/my-bff`). Defaults to repository name if not set.
   - `EXCLUDE_DATA_FROM_IMAGE` (optional) - Set to `true` to exclude providers and openapi.yaml from the image. Defaults to `false`.

3. **Push your data directory** to GitHub:
   ```bash
   git add data/
   git commit -m "Add BFF configuration"
   git push origin main
   ```

The workflow will automatically:
- Detect changes in the `data/` directory
- Build your BFF server using the builder image
- Push the resulting Docker image to your registry

**Workflow triggers:**
- Push to `main`/`master` branch (when `data/` changes)
- Pull requests (for validation, doesn't push)
- Manual workflow dispatch

**Customization:**
Edit `.github/workflows/build.yml` to:
- Change the builder image version
- Use a different registry (GHCR, ECR, etc.)
- Add custom build steps
- Configure version tagging strategy

**Excluding Data from Image:**
To exclude data from the image in GitHub Actions, either:
- Set the repository secret `EXCLUDE_DATA_FROM_IMAGE` = `true`
- Use workflow_dispatch with the `exclude_data` input set to `true`

### Deployment Options

#### Option 1: Data Included in Image (Default)

**Pros:**
- Self-contained image - no external dependencies
- Simple deployment - just run the image
- Immutable - data is versioned with the image

**Usage:**
```bash
docker run -p 3000:3000 my-bff:latest
```

#### Option 2: Mount Data at Runtime

**Pros:**
- Change providers without rebuilding
- Environment-specific configs (dev/staging/prod)
- Keep sensitive data out of images
- Smaller image size

**Docker:**
```bash
# Mount providers directory
docker run -p 3000:3000 \
  -v /path/to/providers:/data/providers:ro \
  -v /path/to/openapi.yaml:/data/openapi.yaml:ro \
  my-bff:latest

# Or mount entire data directory
docker run -p 3000:3000 \
  -v /path/to/data:/data:ro \
  my-bff:latest
```

**Kubernetes with ConfigMap:**

1. Create ConfigMap for providers:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: bff-providers
data:
  user_service.yaml: |
    base_url: http://user-svc:8080
    timeout: 5s
    endpoints:
      profile: /api/profile
  orders_service.yaml: |
    base_url: http://orders-svc:8080
    timeout: 5s
    endpoints:
      list: /api/orders
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: bff-openapi
data:
  openapi.yaml: |
    openapi: 3.0.0
    info:
      title: My BFF API
      version: 1.0.0
    # ... your OpenAPI spec
```

2. Create Deployment:
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: bff
spec:
  replicas: 3
  selector:
    matchLabels:
      app: bff
  template:
    metadata:
      labels:
        app: bff
    spec:
      containers:
      - name: bff
        image: my-bff:latest
        ports:
        - containerPort: 3000
        env:
        - name: DATA_DIR
          value: /data
        volumeMounts:
        - name: providers
          mountPath: /data/providers
        - name: openapi
          mountPath: /data/openapi.yaml
          subPath: openapi.yaml
      volumes:
      - name: providers
        configMap:
          name: bff-providers
      - name: openapi
        configMap:
          name: bff-openapi
---
apiVersion: v1
kind: Service
metadata:
  name: bff
spec:
  selector:
    app: bff
  ports:
  - port: 80
    targetPort: 3000
```

**Kubernetes with Secrets (for sensitive data):**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: bff-providers
type: Opaque
stringData:
  user_service.yaml: |
    base_url: https://secure-api.example.com
    timeout: 5s
    endpoints:
      profile: /api/profile
---
# In Deployment, use secret instead of ConfigMap:
      volumes:
      - name: providers
        secret:
          secretName: bff-providers
```

**Updating ConfigMaps/Secrets:**
```bash
# Update ConfigMap
kubectl create configmap bff-providers --from-file=providers/ --dry-run=client -o yaml | kubectl apply -f -

# Restart pods to pick up changes (or use a tool like Reloader)
kubectl rollout restart deployment/bff
```

**Recommendation:**
- **Include in image** for simple deployments, single environments, or when data is not sensitive
- **Mount at runtime** for multi-environment deployments, sensitive configs, or when you need to change providers without rebuilding

### Building the Builder Image

**Automatic Publishing:**
The builder image is automatically built and published to Docker Hub via GitHub Actions when:
- Changes are pushed to `main`/`master` branch affecting builder-related files
- Manual workflow dispatch is triggered

The workflow (`.github/workflows/publish-builder.yml`) publishes:
- `gcossani/ssfbff-builder:latest` - Latest version from main/master branch
- `gcossani/ssfbff-builder:<branch>` - Branch-specific tags
- `gcossani/ssfbff-builder:<sha>` - Commit SHA tags
- `gcossani/ssfbff-builder:<version>` - Semantic version tags (if using workflow_dispatch with version input)

**Manual Build:**
To build the builder image yourself (for development or custom versions):

```bash
docker build -f Dockerfile.builder -t gcossani/ssfbff-builder:latest .
```

**Requirements for Publishing:**
- Set `DOCKER_USERNAME` and `DOCKER_PASSWORD` secrets in GitHub repository settings
- The workflow supports multi-arch builds (amd64, arm64)

## JSONata Coverage — 86% of spec

**69 of 80** core features from the [JSONata specification](https://docs.jsonata.org/) are supported.
The remaining gaps are mainly higher-order functions and regex functions. The counts below use a curated core set; the full feature matrix also lists additional spec features (e.g. encoding/URL and object transform) for completeness. **First-class functions** are supported: variables holding lambdas can be called (`$f(args)`), lambdas can be assigned and used in `~>` chains, and standalone lambdas in expressions are emitted via a temp variable.

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

*Other = regex literals, lambda expressions, `$eval()`.*

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
| `@` (join) | ❌ | Not supported (parser recognizes it; transpiler does not) |
| Object transform (targeted copy/update) | ❌ | Spec: `head ~> | location | update |` — not supported |
| Functions as first-class values | ✅ | `$f := function($x){ $x*2 }; $f(5)` — call variable as function; standalone lambdas supported |

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
| `$base64encode()` / `$base64decode()` | ❌ | Encoding/decoding (spec) |
| `$encodeUrl()` / `$decodeUrl()` | ❌ | URL encoding (spec) |
| `$encodeUrlComponent()` / `$decodeUrlComponent()` | ❌ | URL component encoding (spec) |

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
| `$formatBase()` | ❌ | `$formatBase(255, 16)` → `"ff"` (spec) |
| `$formatInteger()` | ❌ | `$formatInteger(1999, 'I')` → `"MCMXCIX"` (spec) |

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

**Higher-Order Functions**: First-class *calls* (calling a variable that holds a lambda, e.g. `$f(5)`) are implemented. The missing piece is the built-in implementations of `$map`, `$filter`, etc. that accept a function argument.

#### String Functions — encoding/URL (6 missing, spec)
| Function | Effort | Notes |
|---|---|---|
| `$base64encode()` / `$base64decode()` | Low | Base64 encoding/decoding |
| `$encodeUrl()` / `$decodeUrl()` | Low | Full URL encoding |
| `$encodeUrlComponent()` / `$decodeUrlComponent()` | Low | URL component encoding |

#### Numeric Functions — additional (2 missing, spec)
| Function | Effort | Notes |
|---|---|---|
| `$formatBase(number [, radix])` | Low | Integer to string in given base (2–36) |
| `$formatInteger(number, picture)` | High | Picture string (e.g. Roman numerals, words) |

#### Other Missing Features
| Feature | Effort | Notes |
|---|---|---|
| Regex literals (`/pattern/i`) | Medium | Requires regex parsing and compilation |
| Lambda expressions | Partial | Define/assign lambdas, call variables that hold lambdas, use in `~>` chains — supported. Passing functions to built-ins (`$map`, `$filter`, etc.) not yet supported. |
| `$eval()` | High | Dynamic expression evaluation (security concerns) |
| Object transform (targeted copy/update) | High | Spec pipe syntax: copy object and apply targeted updates/deletes |

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
1. Higher-order built-ins (`$map`, `$filter`, etc.) — first-class calls are done; built-ins that accept a function argument remain.
2. Regex literal support (enables `$match()`, `$replace()`)

**Not Recommended:**
- `$eval()` - Security risk, dynamic evaluation conflicts with compile-time generation model
- `$formatNumber()` - Complex formatting patterns, low usage

</details>

### BFF Extensions (not part of JSONata spec)

| Feature | Example |
|---|---|
| `$fetch(provider, endpoint)` | `$fetch("user_service", "profile").name` |
| `$fetch()` with projection only | `$fetch("svc", "ep").{id: id, title: title}` |
| `$fetch()` with config | `$fetch("svc", "ep", {"method": "POST"}).val` |
| `$request()` context | `$request().headers.Authorization` |
| `$service(name, params?)` composition | `$service("get_user", {"auth": $request().headers.Authorization}).name` |
| `$params()` child inputs | `$params().auth` |
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