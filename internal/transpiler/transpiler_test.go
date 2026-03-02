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

// --- $fetch() mode tests ---

func TestHasFetchCalls(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want bool
	}{
		{
			name: "fetch expression",
			expr: `{"user": $fetch("user_service", "profile").name}`,
			want: true,
		},
		{
			name: "filter expression (no fetch)",
			expr: `orders[price > 100].{id: order_id}`,
			want: false,
		},
		{
			name: "other function call (not fetch)",
			expr: `orders[price > 100].{total: $sum(items.price)}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, err := jparse.Parse(tt.expr)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			got := transpiler.HasFetchCalls(ast)
			if got != tt.want {
				t.Errorf("HasFetchCalls(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestAnalyzeFetchCalls(t *testing.T) {
	expr := `{"user": $fetch("user_service", "profile").name, "balance": $fetch("bank_service", "accounts").amount}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformDashboard")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if plan.FuncName != "TransformDashboard" {
		t.Errorf("FuncName = %q, want %q", plan.FuncName, "TransformDashboard")
	}

	if len(plan.Fields) != 2 {
		t.Fatalf("Fields count = %d, want 2", len(plan.Fields))
	}

	user := plan.Fields[0]
	if user.OutputKey != "user" || user.Provider != "user_service" || user.Endpoint != "profile" {
		t.Errorf("field[0] = %+v, want user/user_service/profile", user)
	}
	if len(user.JSONPath) != 1 || user.JSONPath[0] != "name" {
		t.Errorf("field[0] JSONPath = %v, want [name]", user.JSONPath)
	}

	balance := plan.Fields[1]
	if balance.OutputKey != "balance" || balance.Provider != "bank_service" || balance.Endpoint != "accounts" {
		t.Errorf("field[1] = %+v, want balance/bank_service/accounts", balance)
	}
	if len(balance.JSONPath) != 1 || balance.JSONPath[0] != "amount" {
		t.Errorf("field[1] JSONPath = %v, want [amount]", balance.JSONPath)
	}

	if len(plan.Deps) != 2 {
		t.Fatalf("Deps count = %d, want 2", len(plan.Deps))
	}
	if plan.Deps[0].Provider != "user_service" || plan.Deps[0].Endpoint != "profile" {
		t.Errorf("dep[0] = %+v, want user_service/profile", plan.Deps[0])
	}
	if plan.Deps[1].Provider != "bank_service" || plan.Deps[1].Endpoint != "accounts" {
		t.Errorf("dep[1] = %+v, want bank_service/accounts", plan.Deps[1])
	}
}

func TestAnalyzeFetchCallsDedup(t *testing.T) {
	// Two fields from the same provider+endpoint should produce only one dep.
	expr := `{"name": $fetch("user_svc", "profile").name, "email": $fetch("user_svc", "profile").email}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformUser")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if len(plan.Fields) != 2 {
		t.Fatalf("Fields count = %d, want 2", len(plan.Fields))
	}
	if len(plan.Deps) != 1 {
		t.Errorf("Deps count = %d, want 1 (should deduplicate)", len(plan.Deps))
	}
}

func TestGenerateProviderCode(t *testing.T) {
	expr := `{"user": $fetch("user_service", "profile").name, "balance": $fetch("bank_service", "accounts").amount}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformDashboard")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "testpkg", "dashboard.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	code := string(src)

	mustContain := []string{
		"package testpkg",
		"goexperiment.jsonv2",
		"TransformDashboardDeps",
		"runtime.ProviderDep",
		`Provider: "user_service"`,
		`Endpoint: "profile"`,
		`Provider: "bank_service"`,
		`Endpoint: "accounts"`,
		"TransformDashboard(results map[string][]byte)",
		"jsontext.NewEncoder",
		"jsontext.BeginObject",
		"jsontext.EndObject",
		"runtime.ExtractPath",
		`"name"`,
		`"amount"`,
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q", s)
		}
	}

	// Ensure old $providers syntax is NOT present.
	mustNotContain := []string{
		"$providers",
		"VariableNode",
	}
	for _, s := range mustNotContain {
		if strings.Contains(code, s) {
			t.Errorf("generated code should NOT contain %q", s)
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

// TestFetchEndToEnd generates a $fetch()-based transform, writes it alongside
// a harness that simulates pre-fetched provider results, compiles and runs it.
func TestFetchEndToEnd(t *testing.T) {
	expr := `{"user": $fetch("user_service", "profile").name, "balance": $fetch("bank_service", "accounts").amount}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformDashboard")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "main", "dashboard.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "dashboard_gen.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	// The generated code imports runtime.ExtractPath and runtime.ProviderDep.
	// We need to make the runtime package available, so we copy it.
	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	runtimeSrc, err := os.ReadFile("../../runtime/helpers.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "helpers.go"), runtimeSrc, 0o644); err != nil {
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
	// Simulate what the aggregator would produce after fan-out fetching.
	results := map[string][]byte{
		"user_service.profile":  []byte(` + "`" + `{"name": "Alice", "age": 30}` + "`" + `),
		"bank_service.accounts": []byte(` + "`" + `{"amount": 42500.75, "currency": "USD"}` + "`" + `),
	}

	out, err := TransformDashboard(results)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(string(out))

	// Parse and validate.
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "parse output error: %v\n", err)
		os.Exit(1)
	}

	if parsed["user"] != "Alice" {
		fmt.Fprintf(os.Stderr, "expected user=Alice, got %v\n", parsed["user"])
		os.Exit(1)
	}
	if parsed["balance"] != 42500.75 {
		fmt.Fprintf(os.Stderr, "expected balance=42500.75, got %v\n", parsed["balance"])
		os.Exit(1)
	}

	fmt.Println("PASS")
}
`

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}

	gomod := "module testharness\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fix the import path in the generated code: replace the module import
	// with a local relative one.
	genCode := string(src)
	genCode = strings.ReplaceAll(genCode, `"github.com/gcossani/ssfbff/runtime"`, `"testharness/runtime"`)
	if err := os.WriteFile(filepath.Join(dir, "dashboard_gen.go"), []byte(genCode), 0o644); err != nil {
		t.Fatal(err)
	}

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
