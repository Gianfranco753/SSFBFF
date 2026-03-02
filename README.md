# SSFBFF

Super Simple and Fast Backend For Frontend

A two-stage code generation pipeline that turns JSONata expressions into a high-performance Fiber v3 BFF server. JSONata queries are transpiled into native Go functions at build time - the server never interprets JSONata at runtime. All JSON processing uses `encoding/json/v2` with `jsontext.Decoder` streaming: no reflection, no `map[string]any`.

## Requirements

- Go 1.26+ with `GOEXPERIMENT=jsonv2` enabled

## How It Works

There are two code generators that run at build time:

1. **`cmd/transpiler`** reads a `.jsonata` file and produces a Go function that evaluates the expression using `jsontext` streaming.
2. **`cmd/apigen`** reads a `config.yaml` (or OpenAPI spec) and produces Fiber route registration code. It auto-detects whether each route uses `$fetch()` calls (multi-provider aggregation) or array filtering (single upstream).

```
config.yaml       --> cmd/apigen    --> cmd/server/routes_gen.go
dashboard.jsonata  --> cmd/transpiler --> internal/generated/dashboard_gen.go
orders.jsonata     --> cmd/transpiler --> internal/generated/orders_gen.go
products.jsonata   --> cmd/transpiler --> internal/generated/products_gen.go
```

At runtime, each request either fans out to multiple upstream services in parallel (via the aggregator) or fetches from a single upstream, passes the raw bytes through the compiled transform, and returns the shaped result.

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
+-- config.yaml                      # Providers + routes (single source of truth)
+-- api/
|   +-- openapi.yaml                 # Legacy: BFF endpoints with x-service-name
+-- cmd/
|   +-- apigen/main.go               # config.yaml -> Fiber routes generator
|   +-- server/
|   |   +-- main.go                  # Fiber v3 server entry point
|   |   +-- fetch.go                 # HTTP fetcher with sync.Pool
|   |   +-- routes_gen.go            # Generated route wiring
|   +-- transpiler/main.go           # JSONata -> Go generator
+-- internal/
|   +-- aggregator/
|   |   +-- aggregator.go            # Parallel upstream fetcher (errgroup)
|   +-- generated/
|   |   +-- generate.go              # go:generate directives
|   |   +-- dashboard.jsonata        # $fetch() mode expression
|   |   +-- dashboard_gen.go         # Generated multi-provider transform
|   |   +-- orders.jsonata           # Filter mode expression
|   |   +-- orders_gen.go            # Generated filter transform
|   |   +-- products.jsonata         # Filter mode expression
|   |   +-- products_gen.go          # Generated filter transform
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

### `config.yaml`

The single source of truth for providers and routes:

```yaml
providers:
  user_service:
    base_url: http://user-svc:8080
    timeout: 5s
    endpoints:
      profile: /api/profile
  bank_service:
    base_url: http://bank-svc:8080
    timeout: 3s
    endpoints:
      accounts: /api/accounts

routes:
  - path: /dashboard
    method: GET
    jsonata: dashboard.jsonata      # uses $fetch() -> aggregator mode
  - path: /api/v1/orders
    method: GET
    jsonata: orders.jsonata         # array filter -> single upstream mode
  - path: /api/v1/products
    method: GET
    jsonata: products.jsonata
```

Provider base URLs can be overridden at runtime via `UPSTREAM_<PROVIDER>_URL` environment variables.

## Adding a New Endpoint

### Filter mode (single upstream)

**Step 1.** Write the JSONata expression:

```bash
echo 'users[active = 1].{name: full_name, email: email_address}' > internal/generated/users.jsonata
```

**Step 2.** Add a `go:generate` directive in `internal/generated/generate.go`:

```go
//go:generate go run ../../cmd/transpiler --input=users.jsonata --output=users_gen.go --package=generated
```

**Step 3.** Add the route to `config.yaml`:

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
cat > internal/generated/account_summary.jsonata << 'EOF'
{"owner": $fetch("user_service", "profile").name, "total": $fetch("bank_service", "accounts").amount}
EOF
```

**Step 2.** Define the providers in `config.yaml` (if not already defined):

```yaml
providers:
  user_service:
    base_url: http://user-svc:8080
    timeout: 5s
    endpoints:
      profile: /api/profile
```

**Step 3.** Add a `go:generate` directive and route (same as filter mode):

```go
//go:generate go run ../../cmd/transpiler --input=account_summary.jsonata --output=account_summary_gen.go --package=generated
```

```yaml
routes:
  - path: /api/v1/account-summary
    method: GET
    jsonata: account_summary.jsonata
```

**Step 4.** Regenerate and run. The generated route will automatically use the aggregator for parallel fetching.

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

The generated route handler builds a `RequestContext` from the Fiber `Ctx`, computes deps, fans out in parallel, and transforms:

```go
app.Get("/dashboard", func(c fiber.Ctx) error {
    reqCtx := runtime.RequestContext{
        Path:   c.Path(),
        Method: c.Method(),
        Body:   c.Body(),
    }
    // ... headers, cookies, query, params populated from c ...

    deps := generated.TransformDashboardDeps(reqCtx)
    fetched, err := agg.Fetch(c.Context(), deps)
    if err != nil { return fiber.NewError(502, err.Error()) }

    result, err := generated.TransformDashboard(fetched, reqCtx)
    if err != nil { return fiber.NewError(500, err.Error()) }

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
- If the expression contains `$fetch()` calls -> generates a multi-provider transform with `Deps` metadata
- Otherwise -> generates a streaming filter+projection transform

### `cmd/apigen` - Config/OpenAPI to Fiber Routes

```
go run ./cmd/apigen --config=<config.yaml> --jsonata-dir=<dir> --output=<routes.go> --package=<pkg> --generated-pkg=<import>
```

| Flag | Description | Default |
|---|---|---|
| `--config` | Path to `config.yaml` | - |
| `--spec` | Path to OpenAPI YAML file (legacy) | - |
| `--jsonata-dir` | Directory containing `.jsonata` files | - |
| `--output` | Path for the generated `.go` file | **(required)** |
| `--package` | Go package name in the generated file | `main` |
| `--generated-pkg` | Import path of the transform functions package | **(required)** |

## Environment Variables

| Variable | Description |
|---|---|
| `PORT` | Server listen port (default `3000`) |
| `UPSTREAM_<PROVIDER>_URL` | Override base URL for a provider defined in `config.yaml` |
| `UPSTREAM_<NAME>_URL` | Base URL for filter-mode upstream services |

## Running Tests

```bash
GOEXPERIMENT=jsonv2 go test ./internal/transpiler/ -v
```

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