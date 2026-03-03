# SSFBFF — Super Simple and Fast Backend For Frontend

A code-generation pipeline that compiles [JSONata](https://jsonata.org/) expressions into native Go functions at build time. The resulting Fiber v3 server never interprets JSONata at runtime — all JSON processing uses `encoding/json/v2` streaming with zero reflection.

## Requirements

- Go 1.26+ with `GOEXPERIMENT=jsonv2`

## Quick Start

```bash
# 1. Generate all code (transforms + routes)
GOEXPERIMENT=jsonv2 go generate ./internal/generated/

# 2. Start the server
UPSTREAM_USER_SERVICE_URL=http://user-svc:8080 \
UPSTREAM_BANK_SERVICE_URL=http://bank-svc:8080 \
UPSTREAM_ORDERS_URL=http://orders-svc:8080/data \
GOEXPERIMENT=jsonv2 go run ./cmd/server/

# 3. Test
curl http://localhost:3000/health
curl http://localhost:3000/dashboard
curl http://localhost:3000/api/v1/orders
```

## How It Works

Two generators run at build time via `go generate`:

| Generator | Input | Output |
|---|---|---|
| `cmd/transpiler` | `.jsonata` file | `_gen.go` transform function |
| `cmd/apigen` | `data/openapi.yaml` + `data/proxies.yaml` | `routes_gen.go` Fiber route wiring |

At runtime, each request either fans out to multiple upstreams in parallel or fetches from a single upstream, passes the raw bytes through the compiled transform, and returns the shaped result.

## Expression Modes

### Filter mode — single upstream

Standard JSONata filtering and projection. Generates a streaming Go function:

```jsonata
orders[price > 100].{id: order_id, total: $sum(items.price)}
```

### Fetch mode — multiple upstreams

Uses `$fetch()` to declare upstream dependencies. All calls run in parallel at runtime:

```jsonata
{
  "user": $fetch("user_service", "profile").name,
  "balance": $fetch("bank_service", "accounts").amount
}
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

### `data/proxies.yaml`

Pass-through proxy routes that forward requests directly to downstream services without JSONata transformation:

```yaml
routes:
  - path: /proxy/*
    method: ALL
    proxy: downstream
```

The `proxy` field value maps to an environment variable: `proxy: downstream` → `UPSTREAM_DOWNSTREAM_URL`.

### `data/providers/*.yaml`

```yaml
# data/providers/user_service.yaml
base_url: http://user-svc:8080
timeout: 5s
endpoints:
  profile: /api/profile
```

Provider base URLs can be overridden at runtime: `UPSTREAM_USER_SERVICE_URL=http://...`

Mark a provider as non-critical with `optional: true` — failures store `null` instead of aborting:

```yaml
# data/providers/rec_service.yaml
base_url: http://rec-svc:8080
timeout: 2s
optional: true
endpoints:
  suggestions: /api/suggestions
```

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

### 4. Define providers (fetch mode only)

Create `data/providers/user_service.yaml` if it doesn't exist.

### 5. Regenerate and run

```bash
GOEXPERIMENT=jsonv2 go generate ./internal/generated/
GOEXPERIMENT=jsonv2 go run ./cmd/server/
```

The generator auto-detects the mode — if the expression contains `$fetch()` or `$service()`, it generates aggregator-aware routes; otherwise, single-upstream filter routes.

## Project Structure

```
SSFBFF/
├── cmd/
│   ├── apigen/main.go              # Route generator (openapi.yaml → Fiber routes)
│   ├── server/
│   │   ├── main.go                 # Fiber v3 server entry point
│   │   ├── fetch.go                # HTTP fetcher with sync.Pool
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
| `UPSTREAM_<PROVIDER>_URL` | Override base URL for a provider | — |

### Proxy

All upstream HTTP calls honour standard proxy environment variables:

| Variable | Description |
|---|---|
| `HTTP_PROXY` | Proxy for HTTP requests |
| `HTTPS_PROXY` | Proxy for HTTPS requests |
| `NO_PROXY` | Comma-separated hosts/domains to bypass proxy |

### OpenTelemetry

Tracing is configured entirely via standard [OTEL environment variables](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/):

| Variable | Description | Default |
|---|---|---|
| `OTEL_SDK_DISABLED` | Set `true` to disable tracing | `false` |
| `OTEL_TRACES_EXPORTER` | Set `none` to disable tracing | — |
| `OTEL_SERVICE_NAME` | Service name attached to every span | `ssfbff` |
| `OTEL_RESOURCE_ATTRIBUTES` | Extra resource attributes (e.g. `env=prod,version=1.2`) | — |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP collector endpoint | `http://localhost:4318` |
| `OTEL_EXPORTER_OTLP_HEADERS` | Auth headers, e.g. `Authorization=Bearer <token>` | — |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Traces-specific endpoint override | — |
| `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | Traces-specific header override | — |
| `OTEL_PROPAGATE_UPSTREAM` | Inject `traceparent`/`tracestate` into requests sent to upstream microservices | `true` |
| `OTEL_PROPAGATE_DOWNSTREAM` | Inject `traceparent`/`tracestate` into HTTP responses sent back to clients | `true` |

## Tracing & Observability

The server is fully OpenTelemetry compatible out of the box:

- **Incoming requests** — the [Fiber OTel middleware](https://github.com/gofiber/contrib/tree/main/v3/otel) (`github.com/gofiber/contrib/v3/otel`) creates a server span for every request, records HTTP metrics, and extracts W3C TraceContext + Baggage headers from the incoming request so the BFF can join an existing distributed trace.

- **Upstream calls** — every provider/upstream fetch becomes a child span of the active request trace via `otelhttp.NewTransport`. Span creation always happens (so the BFF records the call duration and status), while header propagation to the microservice is controlled separately (see below).

- **Correlation header propagation** — two env vars independently control whether W3C `traceparent`/`tracestate` headers are forwarded:
  - `OTEL_PROPAGATE_UPSTREAM=false` — **disable** header injection into outgoing requests to upstream microservices. Useful when upstream services do not support OTel and you want to avoid unexpected header overhead. Spans are still recorded on the BFF side.
  - `OTEL_PROPAGATE_DOWNSTREAM=false` — **disable** header injection into HTTP responses back to clients. Useful when you don't want clients to observe internal trace IDs. Spans are still recorded on the BFF side.
  - Both default to `true` (propagation enabled).

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

# Run with Docker Compose (includes mock upstreams)
docker compose -f examples/docker-compose.yaml up --build

# Or run standalone
docker run -p 3000:3000 \
  -e UPSTREAM_USER_SERVICE_URL=http://host.docker.internal:9999 \
  -e UPSTREAM_BANK_SERVICE_URL=http://host.docker.internal:9999 \
  bff-app
```

The build copies `data/` into the build stage, runs `go generate` to transpile all JSONata into Go, compiles the binary, then copies only the binary and `data/providers/` into the runtime image. Routes and service logic are compiled into the binary.

## JSONata Coverage — 71% of spec

**56 of 90** features from the [JSONata specification](https://docs.jsonata.org/) are supported.
The remaining gaps are mainly higher-order functions, date/time, and regex.

| Category | Supported | Total | Coverage |
|---|:---:|:---:|:---:|
| Path & Navigation | 10 | 11 | 91% |
| Comparison Operators | 7 | 7 | 100% |
| Boolean Operators | 2 | 2 | 100% |
| Arithmetic Operators | 6 | 6 | 100% |
| Other Operators | 3 | 3 | 100% |
| Literals | 5 | 6 | 83% |
| String Functions | 10 | 14 | 71% |
| Numeric Functions | 5 | 10 | 50% |
| Aggregation Functions | 5 | 5 | 100% |
| Boolean Functions | 3 | 3 | 100% |
| Array Functions | 4 | 6 | 67% |
| Object Functions | 3 | 5 | 60% |
| Higher-Order Functions | 0 | 5 | — |
| Date/Time Functions | 0 | 4 | — |
| Other | 0 | 3 | — |

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
| `$pad()` | ❌ | `$pad("x", 5, "#")` |
| `$split()` | ❌ | `$split("a,b", ",")` |
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
| `$power()` | ❌ | `$power(2, 3)` → `8` |
| `$sqrt()` | ❌ | `$sqrt(16)` → `4` |
| `$random()` | ❌ | `$random()` |
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
| `$shuffle()` | ❌ | `$shuffle([1,2,3])` |
| `$zip()` | ❌ | `$zip([1,2], [3,4])` |

### Object Functions
| Feature | Status | Example |
|---|:---:|---|
| `$keys()` | ✅ | `$keys({"a":1})` → `["a"]` |
| `$merge()` | ✅ | `$merge([{"a":1},{"b":2}])` |
| `$type()` | ✅ | `$type(42)` → `"number"` |
| `$values()` | ❌ | `$values({"a":1})` → `[1]` |
| `$spread()` | ❌ | `$spread({"a":1})` |

### Not yet implemented
| Category | Features |
|---|---|
| Higher-Order | `$map()`, `$filter()`, `$reduce()`, `$sift()`, `$each()` |
| Date/Time | `$now()`, `$millis()`, `$fromMillis()`, `$toMillis()` |
| Other | Lambda expressions, `$eval()`, `$error()` |

</details>

### BFF Extensions (not part of JSONata spec)

| Feature | Example |
|---|---|
| `$fetch(provider, endpoint)` | `$fetch("user_service", "profile").name` |
| `$fetch()` with config | `$fetch("svc", "ep", {"method": "POST"}).val` |
| `$request()` context | `$request().headers.Authorization` |
| `$service(name)` composition | `$service("get_user").name` |
