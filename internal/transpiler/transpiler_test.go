package transpiler_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blues/jsonata-go/jparse"
	"github.com/gcossani/ssfbff/internal/transpiler"
)

func TestAnalyzeOrdersExpression(t *testing.T) {
	expr := `orders[price > 100].{id: order_id, total: $sum(items.price)}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.Analyze(ast)
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if plan.RootField != "orders" {
		t.Errorf("RootField = %q, want %q", plan.RootField, "orders")
	}
	if plan.FuncName != "TransformOrders" {
		t.Errorf("FuncName = %q, want %q", plan.FuncName, "TransformOrders")
	}
	if plan.OutputName != "OrdersResult" {
		t.Errorf("OutputName = %q, want %q", plan.OutputName, "OrdersResult")
	}
	if len(plan.Filters) != 1 {
		t.Fatalf("Filters count = %d, want 1", len(plan.Filters))
	}
	if plan.Filters[0].Op != ">" {
		t.Errorf("Filter op = %q, want %q", plan.Filters[0].Op, ">")
	}
	if plan.Filters[0].Literal != "100" {
		t.Errorf("Filter literal = %q, want %q", plan.Filters[0].Literal, "100")
	}
	if len(plan.OutputFields) != 2 {
		t.Fatalf("OutputFields count = %d, want 2", len(plan.OutputFields))
	}

	idField := plan.OutputFields[0]
	if idField.GoName != "ID" || idField.SourceField != "OrderID" {
		t.Errorf("output[0]: GoName=%q SourceField=%q, want ID/OrderID", idField.GoName, idField.SourceField)
	}

	totalField := plan.OutputFields[1]
	if totalField.GoName != "Total" || totalField.AggregateFunc != "sum" {
		t.Errorf("output[1]: GoName=%q AggFunc=%q, want Total/sum", totalField.GoName, totalField.AggregateFunc)
	}
}

func TestGenerateOrdersCode(t *testing.T) {
	expr := `orders[price > 100].{id: order_id, total: $sum(items.price)}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.Analyze(ast)
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.Generate(plan, "testpkg", "orders.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	code := string(src)

	mustContain := []string{
		"package testpkg",
		"goexperiment.jsonv2",
		"jsontext.NewDecoder",
		"jsonv2.UnmarshalDecode",
		"TransformOrders",
		"OrdersResult",
		`json:"order_id"`,
		"elem.Price > 100",
		"totalAgg += v.Price",
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q", s)
		}
	}
}

// TestEndToEnd generates code, writes it to a temp dir alongside a main.go
// that exercises the generated function, then compiles and runs it.
func TestEndToEnd(t *testing.T) {
	expr := `orders[price > 100].{id: order_id, total: $sum(items.price)}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.Analyze(ast)
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.Generate(plan, "main", "orders.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	// Write generated code + a test harness into a temp directory.
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "orders_gen.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	harness := `//go:build goexperiment.jsonv2

package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	input := []byte(` + "`" + `{
		"orders": [
			{
				"order_id": "A1",
				"price": 50,
				"items": [{"price": 10}, {"price": 20}]
			},
			{
				"order_id": "A2",
				"price": 200,
				"items": [{"price": 30}, {"price": 40}, {"price": 50}]
			},
			{
				"order_id": "A3",
				"price": 150,
				"items": [{"price": 100}]
			}
		]
	}` + "`" + `)

	results, err := TransformOrders(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	out, _ := json.Marshal(results)
	fmt.Println(string(out))

	// Validate expectations.
	if len(results) != 2 {
		fmt.Fprintf(os.Stderr, "expected 2 results, got %d\n", len(results))
		os.Exit(1)
	}
	if results[0].Total != 120 {
		fmt.Fprintf(os.Stderr, "expected total=120 for A2, got %v\n", results[0].Total)
		os.Exit(1)
	}
	if results[1].Total != 100 {
		fmt.Fprintf(os.Stderr, "expected total=100 for A3, got %v\n", results[1].Total)
		os.Exit(1)
	}
	fmt.Println("PASS")
}
`

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a go.mod so the temp dir is a valid module.
	gomod := "module testharness\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Compile and run.
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running generated code failed:\n%s\nerror: %v", output, err)
	}

	if !strings.Contains(string(output), "PASS") {
		t.Fatalf("generated code did not PASS:\n%s", output)
	}
	t.Logf("generated code output:\n%s", output)
}
