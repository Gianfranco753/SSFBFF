# SSFBFF

Super Simple and Fast Backend For Frontend

A two-stage code generation pipeline that turns JSONata expressions into a high-performance Fiber v3 BFF server. JSONata queries are transpiled into native Go functions at build time - the server never interprets JSONata at runtime. All JSON processing uses `encoding/json/v2` with `jsontext.Decoder` streaming: no reflection, no `map[string]any`.

## Requirements

- Go 1.26+ with `GOEXPERIMENT=jsonv2` enabled

## How It Works

There are two code generators that run at build time:

1. **`cmd/transpiler`** reads a `.jsonata` file and produces a Go function that evaluates the expression using `jsontext` streaming.
2. **`cmd/apigen`** reads `data/routes.yaml` (or an OpenAPI spec) and produces Fiber route registration code. It auto-detects whether each route uses `$fetch()` calls (multi-provider aggregation) or array filtering (single upstream).

```
data/routes.yaml              --> cmd/apigen    --> cmd/server/routes_gen.go
data/services/dashboard.jsonata --> cmd/transpiler --> internal/generated/dashboard_gen.go
data/services/get_user.jsonata  --> cmd/transpiler --> internal/generated/get_user_gen.go
data/services/orders.jsonata    --> cmd/transpiler --> internal/generated/orders_gen.go
data/services/products.jsonata  --> cmd/transpiler --> internal/generated/products_gen.go
```

At runtime, each request either fans out to multiple upstream services in parallel (via the aggregator), composes sub-service pipelines (via `$service()`), or fetches from a single upstream — then passes the raw bytes through the compiled transform and returns the shaped result.

## Two Expression Modes

### Filter mode - single upstream, array filtering

Standard JSONata array filtering and projection. The transpiler generates a streaming Go function that navigates to the array, filters elements, and projects the output.

```jsonata
orders[price > 100].{id: order_id, total: $sum(items.price)}
```

### Fetch mode - multiple upstreams, parallel aggregation

Uses the `$fetch("provider", "endpoint")` convention to declare dependencies on upstream services. At build time the transpiler extracts these as metadata; at runtime the aggregator fans out all requests in parallel. Can include `$request()` paths and extended `$fetch()` config.

```jsonata
{"user": $fetch("user_service", "profile", {
  "headers": {"Authorization": $request().headers.Authorization}
}).name,
"balance": $fetch("bank_service", "accounts").amount,
"request_path": $request().path}
```

### `$request()` - access incoming HTTP request data

Expressions can read data from the incoming HTTP request using `$request()` with dot-path navigation. One function, consistent pattern:

| Path | Returns | Example |
|---|---|---|
| `$request().headers.Name` | Value of the specified HTTP header | `$request().headers.Authorization` |
| `$request().cookies.Name` | Value of the specified cookie | `$request().cookies.session` |
| `$request().query.Name` | Value of a query parameter | `$request().query.page` |
| `$request().params.Name` | Value of a route parameter | `$request().params.id` |
| `$request().path` | The request path | `$request().path` |
| `$request().method` | The HTTP method (GET, POST, etc.) | `$request().method` |
| `$request().body` | The entire request body (raw JSON) | `$request().body` |
| `$request().body.field` | A field from the request body | `$request().body.user.name` |

These can be used in output fields alongside `$fetch()`:

```jsonata
{
  "data": $fetch("svc", "ep").value,
  "auth": $request().headers.Authorization,
  "url": $request().path
}
```

### Extended `$fetch()` - shaping outgoing requests

`$fetch()` accepts an optional 3rd argument — an object that configures the outgoing HTTP request to the upstream provider. Values inside this config can use `$request()` paths, allowing you to forward or transform request data:

```jsonata
{
  "result": $fetch("payment_service", "charge", {
    "method": "POST",
    "headers": {
      "Authorization": $request().headers.Authorization,
      "Content-Type": "application/json"
    },
    "body": {
      "user": $request().cookies.user_id,
      "amount": "100"
    }
  }).status
}
```

The config object supports three keys:
- `method` — HTTP method string (default: `"GET"`)
- `headers` — object mapping header names to values (static strings or `$request()` paths)
- `body` — object whose fields become a JSON body (values can be static strings or `$request()` paths)

### `$service()` - composing transform pipelines

`$service("name")` calls another generated transform pipeline in-process. This enables service composition: complex orchestrations are built by combining simple, reusable services.

```jsonata
{"user": $service("get_user").name, "balance": $fetch("bank_service", "accounts").amount}
```

**Execution model within a single service:**

1. **All `$fetch()` calls** run in parallel first (fan-out via the aggregator)
2. **All `$service()` calls** run in parallel next (each sub-service recursively executes its own pipeline)
3. **Transform** assembles the final output from all results

This is a flat, two-phase model. Sequential provider calls are achieved by composing services — not by nesting `$fetch()` calls:

```
┌─ dashboard service ──────────────────────────────────────────────┐
│  Phase 1 (parallel providers):  $fetch("bank_svc", "accounts")  │
│  Phase 2 (parallel services):   $service("get_user")            │
│    └─ get_user service ─────────────────────────────────────┐    │
│       Phase 1: $fetch("user_svc", "profile")                │    │
│       Phase 2: (none)                                        │    │
│       Transform: {"id": .id, "name": .name}                  │    │
│    └─────────────────────────────────────────────────────────┘    │
│  Transform: {"user": ..., "balance": ...}                        │
└──────────────────────────────────────────────────────────────────┘
```

**Rules:**
- `$service()` is **output-only** — it cannot be used inside `$fetch()` configs
- Each service is a separate `.jsonata` file with its own `go:generate` directive
- Sub-services receive the same `RequestContext` as the parent

### Optional providers

Providers can be marked as `optional: true` in their YAML file under `data/providers/`. When an optional provider fails (timeout, connection error, etc.), the pipeline continues with `null` instead of aborting:

```yaml
providers:
  rec_service:
    base_url: http://rec-svc:8080
    timeout: 2s
    optional: true   # failure stores null, pipeline continues
```

In the JSONata expression, fields from optional providers simply resolve to `null` on failure — no special syntax needed.

#### Why `$fetch()` instead of `$providers.x.y`?

In JSONata, `$name` is variable syntax - variables can be assigned with `:=` and read anywhere. Using `$providers` as a magic namespace would:

- Look like a regular variable that was defined somewhere
- Risk being accidentally shadowed by `$providers := ...`
- Confuse the AST analysis (VariableNode vs FunctionCallNode)

`$fetch()` uses function-call syntax instead, which is:

- **Idiomatic** - custom functions are how JSONata is extended (`$sum`, `$count`, etc.)
- **Unambiguous** - the AST produces `FunctionCallNode`, not `VariableNode`
- **Self-documenting** - it's clear that data is being fetched, not read from a variable

## Project Structure

```
SSFBFF/
+-- Dockerfile                       # Multi-stage: generate + compile -> distroless
+-- .dockerignore
+-- data/                            # All BFF customization lives here
|   +-- routes.yaml                  # Path-to-service mapping
|   +-- openapi.yaml                 # Legacy: BFF endpoints with x-service-name
|   +-- providers/                   # One YAML per upstream service
|   |   +-- user_service.yaml
|   |   +-- bank_service.yaml
|   |   +-- orders_service.yaml
|   |   +-- products_service.yaml
|   +-- services/                    # JSONata expressions
|       +-- dashboard.jsonata        # $fetch() + $service() expression
|       +-- get_user.jsonata         # Reusable service (called via $service("get_user"))
|       +-- orders.jsonata           # Filter mode expression
|       +-- products.jsonata         # Filter mode expression
+-- cmd/
|   +-- apigen/main.go               # routes.yaml -> Fiber routes generator
|   +-- server/
|   |   +-- main.go                  # Fiber v3 server entry point
|   |   +-- fetch.go                 # HTTP fetcher with sync.Pool
|   |   +-- routes_gen.go            # Generated route wiring
|   +-- transpiler/main.go           # JSONata -> Go generator
+-- examples/
|   +-- docker-compose.yaml          # Full-stack demo: BFF + mock upstreams
|   +-- mockserver/
|       +-- main.go                  # Canned JSON responses for all upstreams
|       +-- Dockerfile
+-- internal/
|   +-- aggregator/
|   |   +-- aggregator.go            # Parallel upstream fetcher (errgroup)
|   +-- generated/
|   |   +-- generate.go              # go:generate directives
|   |   +-- *_gen.go                 # Generated transform functions
|   +-- transpiler/
|       +-- analyze.go               # AST -> QueryPlan / ProviderPlan
|       +-- codegen.go               # Plan -> Go source
|       +-- transpiler_test.go
+-- runtime/helpers.go               # Shared helpers (ExtractPath, ProviderDep, etc.)
```

## Quick Start

### 1. Generate everything

```bash
GOEXPERIMENT=jsonv2 go generate ./internal/generated/
```

This runs all generators: transform functions for each `.jsonata` file, plus route wiring.

### 2. Start the server

```bash
UPSTREAM_USER_SERVICE_URL=http://user-svc:8080 \
UPSTREAM_BANK_SERVICE_URL=http://bank-svc:8080 \
UPSTREAM_ORDERS_URL=http://orders-svc:8080/data \
UPSTREAM_PRODUCTS_URL=http://products-svc:8080/data \
GOEXPERIMENT=jsonv2 go run ./cmd/server/
```

### 3. Call the endpoints

```bash
# Multi-provider aggregation (fetch mode)
curl http://localhost:3000/dashboard

# Single-upstream filtering (filter mode)
curl http://localhost:3000/api/v1/orders
curl http://localhost:3000/api/v1/products

# Health check
curl http://localhost:3000/health
```

## Configuration

All BFF configuration lives in the `data/` folder. To customize the BFF, modify the files inside `data/` and rebuild.

### `data/routes.yaml`

Maps HTTP paths to JSONata service files:

```yaml
routes:
  - path: /dashboard
    method: GET
    jsonata: dashboard.jsonata      # uses $fetch() + $service() -> aggregator mode
  - path: /api/v1/orders
    method: GET
    jsonata: orders.jsonata         # array filter -> single upstream mode
  - path: /api/v1/products
    method: GET
    jsonata: products.jsonata
```

### `data/providers/`

One YAML file per upstream service. The filename (minus `.yaml`) is the provider name:

```yaml
# data/providers/user_service.yaml
base_url: http://user-svc:8080
timeout: 5s
endpoints:
  profile: /api/profile
```

```yaml
# data/providers/bank_service.yaml
base_url: http://bank-svc:8080
timeout: 3s
optional: true              # failure stores null, pipeline continues
endpoints:
  accounts: /api/accounts
```

Provider base URLs can be overridden at runtime via `UPSTREAM_<PROVIDER>_URL` environment variables.

The `optional: true` flag makes a provider non-critical. If the call fails (timeout, connection error, HTTP error), the result is stored as `null` and the pipeline continues. Fields that depend on an optional provider will resolve to `null` in the output.

### `data/services/`

JSONata expressions — one file per service. These are transpiled into native Go at build time.

### `data/openapi.yaml`

Optional OpenAPI spec for the legacy `--spec` mode.

## Adding a New Endpoint

### Filter mode (single upstream)

**Step 1.** Write the JSONata expression:

```bash
echo 'users[active = 1].{name: full_name, email: email_address}' > data/services/users.jsonata
```

**Step 2.** Add a `go:generate` directive in `internal/generated/generate.go`:

```go
//go:generate go run ../../cmd/transpiler --input=../../data/services/users.jsonata --output=users_gen.go --package=generated
```

**Step 3.** Add the route to `data/routes.yaml`:

```yaml
routes:
  - path: /api/v1/users
    method: GET
    jsonata: users.jsonata
```

**Step 4.** Regenerate and run:

```bash
GOEXPERIMENT=jsonv2 go generate ./internal/generated/
UPSTREAM_USERS_URL=http://users-svc:8080/data GOEXPERIMENT=jsonv2 go run ./cmd/server/
```

### Fetch mode (multi-provider aggregation)

**Step 1.** Write the JSONata expression using `$fetch()`:

```bash
cat > data/services/account_summary.jsonata << 'EOF'
{"owner": $fetch("user_service", "profile").name, "total": $fetch("bank_service", "accounts").amount}
EOF
```

**Step 2.** Define the providers in `data/providers/` (if not already defined):

```bash
cat > data/providers/user_service.yaml << 'EOF'
base_url: http://user-svc:8080
timeout: 5s
endpoints:
  profile: /api/profile
EOF
```

**Step 3.** Add a `go:generate` directive and route (same as filter mode):

```go
//go:generate go run ../../cmd/transpiler --input=../../data/services/account_summary.jsonata --output=account_summary_gen.go --package=generated
```

```yaml
routes:
  - path: /api/v1/account-summary
    method: GET
    jsonata: account_summary.jsonata
```

**Step 4.** Regenerate and run. The generated route will automatically use the aggregator for parallel fetching.

### Service composition (chaining transforms)

Build complex orchestrations by composing simple services. Each service is a separate `.jsonata` file.

**Step 1.** Create a reusable inner service:

```bash
cat > data/services/get_user.jsonata << 'EOF'
{"id": $fetch("user_service", "profile", {"headers": {"Authorization": $request().headers.Authorization}}).id, "name": $fetch("user_service", "profile", {"headers": {"Authorization": $request().headers.Authorization}}).name}
EOF
```

**Step 2.** Create the outer service that calls the inner one:

```bash
cat > data/services/dashboard.jsonata << 'EOF'
{"user": $service("get_user").name, "balance": $fetch("bank_service", "accounts").amount, "auth": $request().headers.Authorization}
EOF
```

**Step 3.** Add `go:generate` directives for both:

```go
//go:generate go run ../../cmd/transpiler --input=../../data/services/get_user.jsonata --output=get_user_gen.go --package=generated
//go:generate go run ../../cmd/transpiler --input=../../data/services/dashboard.jsonata --output=dashboard_gen.go --package=generated
```

**Step 4.** Add the route to `data/routes.yaml`:

```yaml
routes:
  - path: /dashboard
    method: GET
    jsonata: dashboard.jsonata
```

**Step 5.** Regenerate and run. The generated `ExecuteDashboard` function will:
1. Fetch `bank_service/accounts` in parallel
2. Call `ExecuteGetUser` in parallel (which internally fetches `user_service/profile`)
3. Assemble the final JSON from both results

## Example: What Gets Generated

### Filter mode

Given `orders[price > 100].{id: order_id, total: $sum(items.price)}`, the transpiler generates:

```go
func TransformOrders(data []byte) ([]OrdersResult, error) {
    dec := jsontext.NewDecoder(bytes.NewReader(data))
    // ... stream to "orders" field ...
    for dec.PeekKind() != ']' {
        var elem ordersElement
        jsonv2.UnmarshalDecode(dec, &elem)  // typed deserialization, no reflection

        if !(elem.Price > 100) { continue } // native Go comparison

        var totalAgg float64                // $sum inlined as a loop
        for _, v := range elem.Items {
            totalAgg += v.Price
        }
        results = append(results, OrdersResult{ID: elem.OrderID, Total: totalAgg})
    }
    return results, nil
}
```

### Fetch mode (with `$request()` and fetch config)

Given `{"user": $fetch("user_service", "profile", {"headers": {"Authorization": $request().headers.Authorization}}).name, "balance": $fetch("bank_service", "accounts").amount, "request_path": $request().path}`, the transpiler generates:

```go
// Deps is a function now — it builds the fan-out plan using request context.
func TransformDashboardDeps(req runtime.RequestContext) []runtime.ProviderDep {
    return []runtime.ProviderDep{
        {
            Provider: "user_service",
            Endpoint: "profile",
            Headers: map[string]string{
                "Authorization": req.Headers["Authorization"],
            },
        },
        {Provider: "bank_service", Endpoint: "accounts"},
    }
}

func TransformDashboard(results map[string][]byte, req runtime.RequestContext) ([]byte, error) {
    var buf bytes.Buffer
    enc := jsontext.NewEncoder(&buf)

    enc.WriteToken(jsontext.BeginObject)

    enc.WriteToken(jsontext.String("user"))
    val, _ := runtime.ExtractPath(results["user_service.profile"], "name")
    enc.WriteValue(val)

    enc.WriteToken(jsontext.String("balance"))
    val, _ = runtime.ExtractPath(results["bank_service.accounts"], "amount")
    enc.WriteValue(val)

    enc.WriteToken(jsontext.String("request_path"))
    enc.WriteToken(jsontext.String(req.Path))

    enc.WriteToken(jsontext.EndObject)
    return buf.Bytes(), nil
}
```

### Service composition (with `$service()`)

When an expression uses `$service()`, the transpiler generates an `ExecuteXxx` function that orchestrates the full pipeline: parallel provider fetches, then parallel sub-service calls, then the transform.

Given `dashboard.jsonata` using `$service("get_user")`:

```go
func ExecuteDashboard(ctx context.Context, agg *aggregator.Aggregator, req runtime.RequestContext) ([]byte, error) {
    // Phase 1: fetch all providers in parallel
    deps := TransformDashboardDeps(req)
    results, err := agg.Fetch(ctx, deps)
    if err != nil { return nil, fmt.Errorf("Dashboard: fetch: %w", err) }

    // Phase 2: run all sub-services in parallel
    g, gctx := errgroup.WithContext(ctx)
    var mu sync.Mutex

    g.Go(func() error {
        r, err := ExecuteGetUser(gctx, agg, req)  // in-process call
        if err != nil { return fmt.Errorf("service get_user: %w", err) }
        mu.Lock()
        results["$service.get_user"] = r
        mu.Unlock()
        return nil
    })

    if err := g.Wait(); err != nil { return nil, err }

    // Phase 3: transform with combined results
    return TransformDashboard(results, req)
}
```

The generated route handler simply calls `ExecuteDashboard`:

```go
app.Get("/dashboard", func(c fiber.Ctx) error {
    reqCtx := runtime.RequestContext{/* ... built from Fiber Ctx ... */}

    result, err := generated.ExecuteDashboard(c.Context(), agg, reqCtx)
    if err != nil { return fiber.NewError(502, err.Error()) }

    c.Set("Content-Type", "application/json")
    return c.Send(result)
})
```

## Example Request/Response

### Filter mode

Given this upstream data at `UPSTREAM_ORDERS_URL`:

```json
{
  "orders": [
    {"order_id": "ORD-001", "price": 50,  "items": [{"price": 10}, {"price": 5}]},
    {"order_id": "ORD-002", "price": 200, "items": [{"price": 30}, {"price": 40}, {"price": 50}]},
    {"order_id": "ORD-003", "price": 150, "items": [{"price": 100}]},
    {"order_id": "ORD-004", "price": 75,  "items": [{"price": 25}]}
  ]
}
```

A `GET /api/v1/orders` returns:

```json
[
  {"id": "ORD-002", "total": 120},
  {"id": "ORD-003", "total": 100}
]
```

ORD-001 and ORD-004 are filtered out (price <= 100). Totals are the sum of each order's item prices.

### Fetch mode

Given upstream responses:
- `user_service/profile` returns `{"name": "Alice", "age": 30}`
- `bank_service/accounts` returns `{"amount": 42500.75, "currency": "USD"}`

A `GET /dashboard` returns:

```json
{"user": "Alice", "balance": 42500.75, "request_path": "/dashboard"}
```

Both upstream calls happen in parallel via `errgroup`. The `request_path` field is populated from the incoming HTTP request.

## CLI Reference

### `cmd/transpiler` - JSONata to Go

```
go run ./cmd/transpiler --input=<file.jsonata> --output=<file.go> --package=<pkg>
```

| Flag | Description | Default |
|---|---|---|
| `--input` | Path to the `.jsonata` source file | **(required)** |
| `--output` | Path for the generated `.go` file | `<input>_gen.go` |
| `--package` | Go package name in the generated file | `main` |

The transpiler auto-detects the mode:
- If the expression contains `$fetch()` or `$service()` calls -> generates a multi-provider transform with `Deps` metadata and an `ExecuteXxx` orchestration function
- Otherwise -> generates a streaming filter+projection transform

### `cmd/apigen` - Routes/OpenAPI to Fiber Routes

```
go run ./cmd/apigen --routes=<routes.yaml> --jsonata-dir=<dir> --output=<routes.go> --package=<pkg> --generated-pkg=<import>
```

| Flag | Description | Default |
|---|---|---|
| `--routes` | Path to `routes.yaml` | - |
| `--spec` | Path to OpenAPI YAML file (legacy) | - |
| `--jsonata-dir` | Directory containing `.jsonata` files | - |
| `--output` | Path for the generated `.go` file | **(required)** |
| `--package` | Go package name in the generated file | `main` |
| `--generated-pkg` | Import path of the transform functions package | **(required)** |

## Environment Variables

| Variable | Description |
|---|---|
| `PORT` | Server listen port (default `3000`) |
| `DATA_DIR` | Path to the data directory (default `data`) |
| `UPSTREAM_<PROVIDER>_URL` | Override base URL for a provider defined in `data/providers/` |
| `UPSTREAM_<NAME>_URL` | Base URL for filter-mode upstream services |

## Running Tests

```bash
GOEXPERIMENT=jsonv2 go test ./internal/transpiler/ -v
```

## Docker

The Dockerfile uses a multi-stage build that performs all code generation at build time. The final image is a distroless container with only the compiled binary and `data/providers/` -- no Go toolchain, no source code, no `.jsonata` files.

### Build the image

```bash
docker build -t bff-app .
```

What happens during the build:
1. Dependencies are downloaded and cached (`go mod download`)
2. `go generate` reads `.jsonata` files from `data/services/` and routes from `data/routes.yaml`, transpiles them into native Go code, and generates route wiring
3. The server is compiled into a single static binary
4. The binary and `data/providers/` are copied into a minimal distroless image (routes and services are compiled in)

### Run with Docker Compose (recommended)

The `examples/` directory includes a Docker Compose setup with mock upstream services so you can test the full pipeline without any external dependencies:

```bash
docker compose -f examples/docker-compose.yaml up --build
```

Then test the endpoints:

```bash
# Health check
curl http://localhost:3000/health

# Multi-provider aggregation with service composition
curl http://localhost:3000/dashboard

# Single-upstream filtering
curl http://localhost:3000/api/v1/orders
curl http://localhost:3000/api/v1/products
```

Expected responses:

```bash
# /dashboard — aggregates user_service + bank_service via $service() + $fetch()
{"user":"Alice","balance":42500.75,"auth":""}

# /api/v1/orders — filters orders where price > 100, sums item prices
[{"id":"ORD-002","total":120},{"id":"ORD-003","total":100}]

# /api/v1/products — filters products where price > 50, sums review ratings
[{"name":"Gadget","total":12},{"name":"Gizmo","total":2}]
```

### Run standalone

```bash
docker build -t bff-app .
docker run -p 3000:3000 \
  -e UPSTREAM_USER_SERVICE_URL=http://host.docker.internal:9999 \
  -e UPSTREAM_BANK_SERVICE_URL=http://host.docker.internal:9999 \
  -e UPSTREAM_ORDERS_URL=http://host.docker.internal:9999/data \
  -e UPSTREAM_PRODUCTS_URL=http://host.docker.internal:9999/data/products \
  bff-app
```

### Image size

The runtime image is typically under 15 MB since it contains only the compiled binary and provider YAML files on top of the distroless base.

## Supported JSONata Subset

| Feature | Syntax | Example |
|---|---|---|
| Field access | `field.subfield` | `order_id` |
| Array filter | `array[predicate]` | `orders[price > 100]` |
| Comparisons | `>` `<` `=` `!=` `>=` `<=` | `price > 100` |
| Object projection | `{key: expr, ...}` | `{id: order_id}` |
| `$sum` | `$sum(path)` | `$sum(items.price)` |
| `$count` | `$count(path)` | `$count(items)` |
| `$fetch` | `$fetch("provider", "endpoint").path` | `$fetch("user_service", "profile").name` |
| `$fetch` with config | `$fetch("p", "e", {config}).path` | `$fetch("svc", "ep", {"method": "POST", "headers": {...}}).val` |
| `$request()` | `$request().category.key` | `$request().headers.Authorization` |
| Request headers | `$request().headers.Name` | `$request().headers.Authorization` |
| Request cookies | `$request().cookies.Name` | `$request().cookies.session` |
| Request query | `$request().query.Name` | `$request().query.page` |
| Request params | `$request().params.Name` | `$request().params.id` |
| Request path | `$request().path` | `$request().path` |
| Request method | `$request().method` | `$request().method` |
| Request body | `$request().body.path` | `$request().body.user.name` |
| `$service` | `$service("name").path` | `$service("get_user").name` |