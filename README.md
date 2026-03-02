# SSFBFF
Super Simple and Fast Backend For Frontend

A code generation tool that transpiles [JSONata](https://jsonata.org/) expressions into native Go functions. The generated code uses `encoding/json/jsontext` for streaming and `encoding/json/v2` for typed deserialization — no reflection, no `map[string]any`.

## Requirements

- Go 1.26+ with `GOEXPERIMENT=jsonv2` enabled

## Quick Start

### 1. Write a JSONata expression

```
orders[price > 100].{id: order_id, total: $sum(items.price)}
```

Save it as `orders.jsonata`.

### 2. Run the transpiler

```bash
GOEXPERIMENT=jsonv2 go run github.com/gcossani/ssfbff/cmd/transpiler \
  --input=orders.jsonata \
  --output=orders_gen.go \
  --package=mypackage
```

This produces `orders_gen.go` containing a `TransformOrders(data []byte) ([]OrdersResult, error)` function.

### 3. Use the generated function

```go
//go:build goexperiment.jsonv2

package mypackage

import "fmt"

func Example() {
    input := []byte(`{
        "orders": [
            {"order_id": "A1", "price": 50,  "items": [{"price": 10}]},
            {"order_id": "A2", "price": 200, "items": [{"price": 30}, {"price": 40}]}
        ]
    }`)

    results, err := TransformOrders(input)
    if err != nil {
        panic(err)
    }

    for _, r := range results {
        fmt.Printf("id=%v total=%.0f\n", r.ID, r.Total)
    }
    // Output: id=A2 total=70
}
```

Build with `GOEXPERIMENT=jsonv2 go build .`

## Using `go:generate`

Add this directive to any Go source file in your package:

```go
//go:generate go run github.com/gcossani/ssfbff/cmd/transpiler --input=orders.jsonata --output=orders_gen.go --package=mypackage
```

Then run:

```bash
GOEXPERIMENT=jsonv2 go generate ./...
```

## CLI Flags

| Flag | Description | Default |
|---|---|---|
| `--input` | Path to the `.jsonata` source file | **(required)** |
| `--output` | Path for the generated `.go` file | `<input>_gen.go` |
| `--package` | Go package name in the generated file | `main` |

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
