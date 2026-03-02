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
| `cmd/apigen` | `data/routes.yaml` | `routes_gen.go` Fiber route wiring |

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
├── routes.yaml           # HTTP path → JSONata service mapping
├── providers/            # One YAML per upstream service
│   ├── user_service.yaml
│   └── bank_service.yaml
├── services/             # JSONata expressions (one per service)
│   ├── dashboard.jsonata
│   ├── get_user.jsonata
│   └── orders.jsonata
└── openapi.yaml          # Optional: legacy OpenAPI spec mode
```

### `data/routes.yaml`

```yaml
routes:
  - path: /dashboard
    method: GET
    jsonata: dashboard.jsonata
  - path: /api/v1/orders
    method: GET
    jsonata: orders.jsonata
```

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

In `data/routes.yaml`:

```yaml
  - path: /api/v1/users
    method: GET
    jsonata: users.jsonata
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
│   ├── apigen/main.go              # Route generator (routes.yaml → Fiber routes)
│   ├── server/
│   │   ├── main.go                 # Fiber v3 server entry point
│   │   ├── fetch.go                # HTTP fetcher with sync.Pool
│   │   └── routes_gen.go           # Generated (gitignored)
│   └── transpiler/main.go          # JSONata → Go generator
├── data/                           # All BFF configuration
│   ├── routes.yaml
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
go run ./cmd/apigen --routes=<routes.yaml> --jsonata-dir=<dir> --output=<file.go> --package=<pkg> --generated-pkg=<import>
```

| Flag | Description | Default |
|---|---|---|
| `--routes` | Path to `routes.yaml` | — |
| `--spec` | OpenAPI YAML file (legacy mode) | — |
| `--jsonata-dir` | Directory with `.jsonata` files | — |
| `--output` | Generated `.go` file | **(required)** |
| `--package` | Package name | `main` |
| `--generated-pkg` | Import path for transform functions | **(required)** |

## Environment Variables

| Variable | Description | Default |
|---|---|---|
| `PORT` | Server listen port | `3000` |
| `DATA_DIR` | Path to the data directory | `data` |
| `UPSTREAM_<PROVIDER>_URL` | Override base URL for a provider | — |

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

## Supported JSONata Subset

| Feature | Example |
|---|---|
| Field access | `order_id`, `items.price` |
| Array filter | `orders[price > 100]` |
| Comparisons | `>`, `<`, `=`, `!=`, `>=`, `<=` |
| Object projection | `{id: order_id, total: $sum(items.price)}` |
| `$sum(path)` | `$sum(items.price)` |
| `$count(path)` | `$count(items)` |
| `$fetch(p, e)` | `$fetch("user_service", "profile").name` |
| `$fetch(p, e, config)` | `$fetch("svc", "ep", {"method": "POST"}).val` |
| `$request().path` | `$request().headers.Authorization` |
| `$service(name)` | `$service("get_user").name` |
