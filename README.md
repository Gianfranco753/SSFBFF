# SSFBFF

Super Simple and Fast Backend For Frontend

A two-stage code generation pipeline that turns JSONata expressions into a high-performance Fiber v3 BFF server. JSONata queries are transpiled into native Go functions at build time — the server never interprets JSONata at runtime. All JSON processing uses `encoding/json/v2` with `jsontext.Decoder` streaming: no reflection, no `map[string]any`.

## Requirements

- Go 1.26+ with `GOEXPERIMENT=jsonv2` enabled

## How It Works

There are two code generators that run at build time:

1. **`cmd/transpiler`** reads a `.jsonata` file and produces a Go function that evaluates the expression using `jsontext` streaming.
2. **`cmd/apigen`** reads an OpenAPI spec and produces Fiber route registration code. Each operation's `x-service-name` extension maps to the matching transform function.

```
api/openapi.yaml ──► cmd/apigen ──► cmd/server/routes_gen.go
orders.jsonata   ──► cmd/transpiler ──► internal/generated/orders_gen.go
products.jsonata ──► cmd/transpiler ──► internal/generated/products_gen.go
```

At runtime, each request fetches upstream JSON, passes it through the compiled transform, and returns the shaped result.

## Project Structure

```
SSFBFF/
├── api/
│   └── openapi.yaml                 # BFF endpoints with x-service-name
├── cmd/
│   ├── apigen/main.go               # OpenAPI → Fiber routes generator
│   ├── server/
│   │   ├── main.go                  # Fiber v3 server entry point
│   │   ├── fetch.go                 # HTTP fetcher with sync.Pool
│   │   └── routes_gen.go            # Generated route wiring
│   └── transpiler/main.go           # JSONata → Go generator
├── internal/
│   ├── generated/
│   │   ├── generate.go              # go:generate directives
│   │   ├── orders.jsonata
│   │   ├── orders_gen.go            # Generated transform
│   │   ├── products.jsonata
│   │   └── products_gen.go          # Generated transform
│   └── transpiler/
│       ├── analyze.go               # AST → QueryPlan
│       ├── codegen.go               # QueryPlan → Go source
│       └── transpiler_test.go
└── runtime/helpers.go               # Shared aggregation helpers
```

## Quick Start

### 1. Generate everything

```bash
GOEXPERIMENT=jsonv2 go generate ./internal/generated/
```

This runs all three generators: orders transform, products transform, and route wiring.

### 2. Start the server

```bash
UPSTREAM_ORDERS_URL=http://orders-svc:8080/data \
UPSTREAM_PRODUCTS_URL=http://products-svc:8080/data \
GOEXPERIMENT=jsonv2 go run ./cmd/server/
```

### 3. Call the endpoints

```bash
curl http://localhost:3000/api/v1/orders
curl http://localhost:3000/api/v1/products
curl http://localhost:3000/health
```

## Adding a New Endpoint

Suppose you want to add a `/api/v1/users` endpoint that transforms user data.

**Step 1.** Write the JSONata expression:

```bash
echo 'users[active = 1].{name: full_name, email: email_address}' > internal/generated/users.jsonata
```

**Step 2.** Add a `go:generate` directive in `internal/generated/generate.go`:

```go
//go:generate go run ../../cmd/transpiler --input=users.jsonata --output=users_gen.go --package=generated
```

**Step 3.** Add the route to `api/openapi.yaml`:

```yaml
paths:
  /api/v1/users:
    get:
      operationId: getUsers
      x-service-name: users
```

**Step 4.** Regenerate and run:

```bash
GOEXPERIMENT=jsonv2 go generate ./internal/generated/
UPSTREAM_USERS_URL=http://users-svc:8080/data GOEXPERIMENT=jsonv2 go run ./cmd/server/
```

The `x-service-name: users` automatically maps to `TransformUsers()` and reads `UPSTREAM_USERS_URL`.

## OpenAPI Convention

Each operation uses `x-service-name` to wire the endpoint to its transform function and upstream URL:

```yaml
/api/v1/orders:
  get:
    operationId: getOrders
    x-service-name: orders          # → TransformOrders(), UPSTREAM_ORDERS_URL

/api/v1/products:
  get:
    operationId: getProducts
    x-service-name: products        # → TransformProducts(), UPSTREAM_PRODUCTS_URL
```

The naming convention:
- `x-service-name: foo` → calls `generated.TransformFoo()`
- Upstream URL from env var `UPSTREAM_FOO_URL`

## Example: What Gets Generated

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

And from the OpenAPI spec, `cmd/apigen` generates the route wiring:

```go
func RegisterRoutes(app *fiber.App, fetch FetchFunc) {
    app.Get("/api/v1/orders", func(c fiber.Ctx) error {
        url := os.Getenv("UPSTREAM_ORDERS_URL")
        data, err := fetch(c.Context(), url)
        if err != nil { return fiber.NewError(502, err.Error()) }

        results, err := generated.TransformOrders(data)
        if err != nil { return fiber.NewError(500, err.Error()) }

        return c.JSON(results)
    })
}
```

## Example Request/Response

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

## CLI Reference

### `cmd/transpiler` — JSONata to Go

```
go run ./cmd/transpiler --input=<file.jsonata> --output=<file.go> --package=<pkg>
```

| Flag | Description | Default |
|---|---|---|
| `--input` | Path to the `.jsonata` source file | **(required)** |
| `--output` | Path for the generated `.go` file | `<input>_gen.go` |
| `--package` | Go package name in the generated file | `main` |

### `cmd/apigen` — OpenAPI to Fiber Routes

```
go run ./cmd/apigen --spec=<openapi.yaml> --output=<routes.go> --package=<pkg> --generated-pkg=<import>
```

| Flag | Description | Default |
|---|---|---|
| `--spec` | Path to the OpenAPI YAML file | **(required)** |
| `--output` | Path for the generated `.go` file | **(required)** |
| `--package` | Go package name in the generated file | `main` |
| `--generated-pkg` | Import path of the transform functions package | **(required)** |

## Environment Variables

| Variable | Description |
|---|---|
| `PORT` | Server listen port (default `3000`) |
| `UPSTREAM_<NAME>_URL` | Base URL for each upstream service, derived from `x-service-name` |

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
