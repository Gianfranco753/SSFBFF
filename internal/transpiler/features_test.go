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

// TestNewFeaturesParsing verifies that the analyzer + codegen pipeline handles
// every new expression form without errors. These are fast, no-compile tests.
func TestNewFeaturesParsing(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{"and/or filter", `items[price > 50 and active = true].{id: id}`},
		{"string literal filter", `items[status = "active"].{id: id}`},
		{"boolean literal filter", `items[active = true].{id: id}`},
		{"arithmetic projection", `items[price > 0].{total: price * quantity}`},
		{"string concat", `items[price > 0].{label: name & " - " & category}`},
		{"conditional", `items[price > 0].{tag: price > 100 ? "expensive" : "cheap"}`},
		{"aggregate min", `items[price > 0].{id: id, min_price: $min(children.price)}`},
		{"aggregate max", `items[price > 0].{id: id, max_price: $max(children.price)}`},
		{"aggregate avg", `items[price > 0].{id: id, avg_price: $average(children.price)}`},
		{"uppercase", `items[price > 0].{upper: $uppercase(name)}`},
		{"lowercase", `items[price > 0].{lower: $lowercase(name)}`},
		{"trim", `items[price > 0].{clean: $trim(name)}`},
		{"length", `items[price > 0].{len: $length(name)}`},
		{"substring", `items[price > 0].{sub: $substring(name, 0, 3)}`},
		{"abs", `items[price > 0].{val: $abs(price)}`},
		{"floor", `items[price > 0].{val: $floor(price)}`},
		{"ceil", `items[price > 0].{val: $ceil(price)}`},
		{"round", `items[price > 0].{val: $round(price)}`},
		{"not", `items[$not(active)].{id: id}`},
		{"negation", `items[price > 0].{neg: -price}`},
		{"null literal", `items[val != null].{id: id}`},
		{"modulo", `items[price > 0].{mod: price % 10}`},
		{"complex filter", `items[price > 50 and (status = "active" or featured = true)].{id: id}`},
		{"nested arithmetic", `items[price > 0].{discounted: price * (1 - discount)}`},
		{"contains filter", `items[$contains(name, "widget")].{id: id}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, err := jparse.Parse(tt.expr)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			plan, err := transpiler.Analyze(ast)
			if err != nil {
				t.Fatalf("analyze error: %v", err)
			}
			src, err := transpiler.Generate(plan, "test", "", tt.expr)
			if err != nil {
				t.Fatalf("generate error: %v", err)
			}
			if len(src) == 0 {
				t.Fatal("generated code is empty")
			}
		})
	}
}

// TestNewFeaturesExprTree verifies the Expr tree structure for complex expressions.
func TestNewFeaturesExprTree(t *testing.T) {
	t.Run("and filter", func(t *testing.T) {
		ast, _ := jparse.Parse(`items[price > 50 and qty < 10].{id: id}`)
		plan, err := transpiler.Analyze(ast)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Filters) != 1 {
			t.Fatalf("expected 1 filter (AND node), got %d", len(plan.Filters))
		}
		f := plan.Filters[0]
		if f.Kind != "binary" || f.Op != "&&" {
			t.Errorf("filter: Kind=%q Op=%q, want binary/&&", f.Kind, f.Op)
		}
		if f.Left == nil || f.Left.Kind != "binary" || f.Left.Op != ">" {
			t.Errorf("filter.Left: want comparison >, got %+v", f.Left)
		}
		if f.Right == nil || f.Right.Kind != "binary" || f.Right.Op != "<" {
			t.Errorf("filter.Right: want comparison <, got %+v", f.Right)
		}
	})

	t.Run("arithmetic expr", func(t *testing.T) {
		ast, _ := jparse.Parse(`items[price > 0].{total: price * quantity}`)
		plan, err := transpiler.Analyze(ast)
		if err != nil {
			t.Fatal(err)
		}
		val := plan.OutputFields[0].Value
		if val.Kind != "binary" || val.Op != "*" {
			t.Errorf("output value: Kind=%q Op=%q, want binary/*", val.Kind, val.Op)
		}
		if val.Left.FieldName != "Price" || val.Right.FieldName != "Quantity" {
			t.Errorf("operands: %q * %q, want Price * Quantity", val.Left.FieldName, val.Right.FieldName)
		}
	})

	t.Run("conditional expr", func(t *testing.T) {
		ast, _ := jparse.Parse(`items[price > 0].{tag: price > 100 ? "expensive" : "cheap"}`)
		plan, err := transpiler.Analyze(ast)
		if err != nil {
			t.Fatal(err)
		}
		val := plan.OutputFields[0].Value
		if val.Kind != "conditional" {
			t.Fatalf("output value Kind=%q, want conditional", val.Kind)
		}
		if val.Cond.Kind != "binary" || val.Cond.Op != ">" {
			t.Errorf("condition: %+v, want binary >", val.Cond)
		}
		if val.Then.LiteralValue != `"expensive"` {
			t.Errorf("then: %q, want \"expensive\"", val.Then.LiteralValue)
		}
		if val.Else.LiteralValue != `"cheap"` {
			t.Errorf("else: %q, want \"cheap\"", val.Else.LiteralValue)
		}
	})

	t.Run("string concat expr", func(t *testing.T) {
		ast, _ := jparse.Parse(`items[price > 0].{label: name & " " & category}`)
		plan, err := transpiler.Analyze(ast)
		if err != nil {
			t.Fatal(err)
		}
		val := plan.OutputFields[0].Value
		if val.Kind != "binary" || val.Op != "&" {
			t.Errorf("output value: Kind=%q Op=%q, want binary/&", val.Kind, val.Op)
		}
	})

	t.Run("function call expr", func(t *testing.T) {
		ast, _ := jparse.Parse(`items[price > 0].{upper: $uppercase(name)}`)
		plan, err := transpiler.Analyze(ast)
		if err != nil {
			t.Fatal(err)
		}
		val := plan.OutputFields[0].Value
		if val.Kind != "funcCall" || val.FuncName != "uppercase" {
			t.Errorf("output value: Kind=%q FuncName=%q, want funcCall/uppercase", val.Kind, val.FuncName)
		}
		if len(val.FuncArgs) != 1 || val.FuncArgs[0].FieldName != "Name" {
			t.Errorf("func args: want [Name], got %+v", val.FuncArgs)
		}
	})

	t.Run("negation expr", func(t *testing.T) {
		ast, _ := jparse.Parse(`items[price > 0].{neg: -price}`)
		plan, err := transpiler.Analyze(ast)
		if err != nil {
			t.Fatal(err)
		}
		val := plan.OutputFields[0].Value
		if val.Kind != "unary" || val.Op != "-" {
			t.Errorf("output value: Kind=%q Op=%q, want unary/-", val.Kind, val.Op)
		}
		if val.Left.FieldName != "Price" {
			t.Errorf("operand: %q, want Price", val.Left.FieldName)
		}
	})
}

// TestNewFeaturesCodegenContent checks that the generated code contains
// expected patterns for each new feature.
func TestNewFeaturesCodegenContent(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		contains []string
	}{
		{
			name: "and/or in filter",
			expr: `items[price > 50 and qty < 10].{id: id}`,
			contains: []string{
				"(elem.Price > 50) && (elem.Qty < 10)",
				"passesFilter",
			},
		},
		{
			name: "string literal in filter",
			expr: `items[status = "active"].{id: id}`,
			contains: []string{
				`elem.Status == "active"`,
			},
		},
		{
			name: "boolean literal in filter",
			expr: `items[active = true].{id: id}`,
			contains: []string{
				"elem.Active == true",
			},
		},
		{
			name: "arithmetic in projection",
			expr: `items[price > 0].{total: price * quantity}`,
			contains: []string{
				"(elem.Price * elem.Quantity)",
			},
		},
		{
			name: "string concatenation",
			expr: `items[price > 0].{label: name & " "}`,
			contains: []string{
				`runtime.ToString(elem.Name) + runtime.ToString(" ")`,
				`"github.com/gcossani/ssfbff/runtime"`,
			},
		},
		{
			name: "conditional expression",
			expr: `items[price > 0].{tag: price > 100 ? "expensive" : "cheap"}`,
			contains: []string{
				"runtime.Truthy",
				`"expensive"`,
				`"cheap"`,
			},
		},
		{
			name: "aggregate min",
			expr: `items[price > 0].{id: id, m: $min(children.price)}`,
			contains: []string{
				"runtime.MinFloat64",
			},
		},
		{
			name: "aggregate max",
			expr: `items[price > 0].{id: id, m: $max(children.price)}`,
			contains: []string{
				"runtime.MaxFloat64",
			},
		},
		{
			name: "aggregate average",
			expr: `items[price > 0].{id: id, a: $average(children.price)}`,
			contains: []string{
				"runtime.AverageFloat64",
			},
		},
		{
			name: "uppercase function",
			expr: `items[price > 0].{upper: $uppercase(name)}`,
			contains: []string{
				"runtime.Uppercase(elem.Name)",
			},
		},
		{
			name: "lowercase function",
			expr: `items[price > 0].{lower: $lowercase(name)}`,
			contains: []string{
				"runtime.Lowercase(elem.Name)",
			},
		},
		{
			name: "trim function",
			expr: `items[price > 0].{clean: $trim(name)}`,
			contains: []string{
				"runtime.Trim(elem.Name)",
			},
		},
		{
			name: "length function",
			expr: `items[price > 0].{len: $length(name)}`,
			contains: []string{
				"runtime.Length(elem.Name)",
			},
		},
		{
			name: "abs function",
			expr: `items[price > 0].{val: $abs(price)}`,
			contains: []string{
				"runtime.Abs(elem.Price)",
			},
		},
		{
			name: "floor function",
			expr: `items[price > 0].{val: $floor(price)}`,
			contains: []string{
				"runtime.Floor(elem.Price)",
			},
		},
		{
			name: "ceil function",
			expr: `items[price > 0].{val: $ceil(price)}`,
			contains: []string{
				"runtime.Ceil(elem.Price)",
			},
		},
		{
			name: "round function",
			expr: `items[price > 0].{val: $round(price)}`,
			contains: []string{
				"runtime.Round(elem.Price)",
			},
		},
		{
			name: "not function in filter",
			expr: `items[$not(active)].{id: id}`,
			contains: []string{
				"runtime.Not(elem.Active)",
			},
		},
		{
			name: "negation in projection",
			expr: `items[price > 0].{neg: -price}`,
			contains: []string{
				"(-elem.Price)",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, err := jparse.Parse(tt.expr)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			plan, err := transpiler.Analyze(ast)
			if err != nil {
				t.Fatalf("analyze error: %v", err)
			}
			src, err := transpiler.Generate(plan, "test", "", tt.expr)
			if err != nil {
				t.Fatalf("generate error: %v", err)
			}
			code := string(src)
			for _, want := range tt.contains {
				if !strings.Contains(code, want) {
					t.Errorf("generated code missing %q\n\nGenerated:\n%s", want, code)
				}
			}
		})
	}
}

// TestNewFeaturesEndToEnd compiles and runs generated code against real JSON data
// to verify correctness of the full pipeline.
func TestNewFeaturesEndToEnd(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		input    string
		expected string
	}{
		{
			name: "and filter",
			expr: `items[price > 50 and qty > 2].{id: id, price: price}`,
			input: `{"items":[
				{"id":"A","price":100,"qty":5},
				{"id":"B","price":30,"qty":10},
				{"id":"C","price":80,"qty":1}
			]}`,
			expected: `[{"id":"A","price":100}]`,
		},
		{
			name: "or filter",
			expr: `items[price > 200 or featured = true].{id: id}`,
			input: `{"items":[
				{"id":"A","price":100,"featured":false},
				{"id":"B","price":300,"featured":false},
				{"id":"C","price":50,"featured":true}
			]}`,
			expected: `[{"id":"B"},{"id":"C"}]`,
		},
		{
			name: "string literal filter",
			expr: `items[status = "active"].{id: id}`,
			input: `{"items":[
				{"id":"A","status":"active"},
				{"id":"B","status":"inactive"},
				{"id":"C","status":"active"}
			]}`,
			expected: `[{"id":"A"},{"id":"C"}]`,
		},
		{
			name: "arithmetic projection",
			expr: `items[price > 0].{id: id, total: price * quantity}`,
			input: `{"items":[
				{"id":"A","price":10,"quantity":3},
				{"id":"B","price":20,"quantity":2}
			]}`,
			expected: `[{"id":"A","total":30},{"id":"B","total":40}]`,
		},
		{
			name: "negation",
			expr: `items[price > 0].{id: id, neg: -price}`,
			input: `{"items":[{"id":"A","price":42}]}`,
			expected: `[{"id":"A","neg":-42}]`,
		},
		{
			name: "aggregate min",
			expr: `items[price > 0].{id: id, m: $min(parts.cost)}`,
			input: `{"items":[{"id":"A","price":1,"parts":[{"cost":5},{"cost":2},{"cost":8}]}]}`,
			expected: `[{"id":"A","m":2}]`,
		},
		{
			name: "aggregate max",
			expr: `items[price > 0].{id: id, m: $max(parts.cost)}`,
			input: `{"items":[{"id":"A","price":1,"parts":[{"cost":5},{"cost":2},{"cost":8}]}]}`,
			expected: `[{"id":"A","m":8}]`,
		},
		{
			name: "aggregate average",
			expr: `items[price > 0].{id: id, a: $average(parts.cost)}`,
			input: `{"items":[{"id":"A","price":1,"parts":[{"cost":3},{"cost":6},{"cost":9}]}]}`,
			expected: `[{"id":"A","a":6}]`,
		},
		{
			name: "uppercase",
			expr: `items[price > 0].{upper: $uppercase(name)}`,
			input: `{"items":[{"name":"hello","price":10}]}`,
			expected: `[{"upper":"HELLO"}]`,
		},
		{
			name: "lowercase",
			expr: `items[price > 0].{lower: $lowercase(name)}`,
			input: `{"items":[{"name":"WORLD","price":10}]}`,
			expected: `[{"lower":"world"}]`,
		},
		{
			name: "length",
			expr: `items[price > 0].{len: $length(name)}`,
			input: `{"items":[{"name":"hello","price":10}]}`,
			expected: `[{"len":5}]`,
		},
		{
			name: "abs",
			expr: `items[price > 0].{val: $abs(delta)}`,
			input: `{"items":[{"delta":-7,"price":1}]}`,
			// $abs(-7) = 7
			expected: `[{"val":7}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, err := jparse.Parse(tt.expr)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			plan, err := transpiler.Analyze(ast)
			if err != nil {
				t.Fatalf("analyze: %v", err)
			}
			src, err := transpiler.Generate(plan, "main", "", tt.expr)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}

			// Build a temp Go program that calls the generated function.
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "transform_gen.go"), src, 0644); err != nil {
				t.Fatal(err)
			}

			mainSrc := `//go:build goexperiment.jsonv2
package main

import (
	"fmt"
	"encoding/json"
	"os"
)

func main() {
	data, _ := os.ReadFile(os.Args[1])
	results, err := ` + plan.FuncName + `(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "transform error: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.Marshal(results)
	fmt.Print(string(out))
}
`
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSrc), 0644); err != nil {
				t.Fatal(err)
			}

			// Write go.mod.
			goMod := "module testharness\n\ngo 1.25.0\n"
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
				t.Fatal(err)
			}

			// Copy runtime package and fix import paths in generated code.
			copyRuntimePackage(t, dir)
			fixImports(t, filepath.Join(dir, "transform_gen.go"))

			// Write input file.
			inputFile := filepath.Join(dir, "input.json")
			if err := os.WriteFile(inputFile, []byte(tt.input), 0644); err != nil {
				t.Fatal(err)
			}

			// Build.
			buildCmd := exec.Command("go", "build", "-o", filepath.Join(dir, "test"), ".")
			buildCmd.Dir = dir
			buildCmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
			if out, err := buildCmd.CombinedOutput(); err != nil {
				t.Fatalf("build failed: %v\n%s\n\nGenerated code:\n%s", err, out, string(src))
			}

			// Run.
			runCmd := exec.Command(filepath.Join(dir, "test"), inputFile)
			runCmd.Dir = dir
			output, err := runCmd.CombinedOutput()
			if err != nil {
				t.Fatalf("run failed: %v\n%s", err, output)
			}

			got := strings.TrimSpace(string(output))
			if got != tt.expected {
				t.Errorf("output mismatch:\n  got:  %s\n  want: %s", got, tt.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Variable binding tests
// ---------------------------------------------------------------------------

// TestVariableParsing verifies that variable expressions parse and analyze
// without errors, and that the generated code is non-empty.
func TestVariableParsing(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{"simple binding", `items[price > 0].($total := price * quantity; {id: id, total: $total})`},
		{"multi binding", `items[price > 0].($t := price * quantity; $d := $t * 0.9; {total: $t, discounted: $d})`},
		{"var in conditional", `items[price > 0].($t := price * quantity; {label: $t > 100 ? "big" : "small"})`},
		{"var from function", `items[price > 0].($u := $uppercase(name); {upper: $u})`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, err := jparse.Parse(tt.expr)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			plan, err := transpiler.Analyze(ast)
			if err != nil {
				t.Fatalf("analyze error: %v", err)
			}
			src, err := transpiler.Generate(plan, "test", "", tt.expr)
			if err != nil {
				t.Fatalf("generate error: %v", err)
			}
			if len(src) == 0 {
				t.Fatal("generated code is empty")
			}
		})
	}
}

// TestVariableValidation verifies that the analyzer rejects invalid variable usage.
func TestVariableValidation(t *testing.T) {
	t.Run("redefined variable", func(t *testing.T) {
		ast, err := jparse.Parse(`items[price > 0].($x := 1; $x := 2; {val: $x})`)
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		_, err = transpiler.Analyze(ast)
		if err == nil {
			t.Fatal("expected error for redefined variable, got nil")
		}
		if !strings.Contains(err.Error(), "already defined") {
			t.Fatalf("expected 'already defined' error, got: %v", err)
		}
	})

	t.Run("undefined variable", func(t *testing.T) {
		ast, err := jparse.Parse(`items[price > 0].{val: $x}`)
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		_, err = transpiler.Analyze(ast)
		if err == nil {
			t.Fatal("expected error for undefined variable, got nil")
		}
		if !strings.Contains(err.Error(), "undefined variable") {
			t.Fatalf("expected 'undefined variable' error, got: %v", err)
		}
	})
}

// TestVariableExprTree verifies the Expr tree structure for variable bindings.
func TestVariableExprTree(t *testing.T) {
	ast, _ := jparse.Parse(`items[price > 0].($total := price * quantity; {id: id, total: $total})`)
	plan, err := transpiler.Analyze(ast)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(plan.Bindings))
	}

	b := plan.Bindings[0]
	if b.Kind != "assign" {
		t.Fatalf("expected binding kind='assign', got %q", b.Kind)
	}
	if b.VarName != "total" {
		t.Fatalf("expected VarName='total', got %q", b.VarName)
	}
	if b.GoType != "float64" {
		t.Fatalf("expected GoType='float64', got %q", b.GoType)
	}

	// The "total" output field should reference the variable.
	totalField := plan.OutputFields[1]
	if totalField.Value.Kind != "varRef" {
		t.Fatalf("expected output 'total' value kind='varRef', got %q", totalField.Value.Kind)
	}
	if totalField.Value.VarName != "total" {
		t.Fatalf("expected VarName='total', got %q", totalField.Value.VarName)
	}
}

// TestVariableCodegen verifies the generated code contains the expected patterns.
func TestVariableCodegen(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		contains []string
	}{
		{
			name: "single variable",
			expr: `items[price > 0].($total := price * quantity; {total: $total})`,
			contains: []string{
				"jsonataVar_total :=",
				"(elem.Price * elem.Quantity)",
				"Total: jsonataVar_total",
			},
		},
		{
			name: "multiple variables",
			expr: `items[price > 0].($t := price * quantity; $d := $t * 0.9; {total: $t, discounted: $d})`,
			contains: []string{
				"jsonataVar_t :=",
				"jsonataVar_d :=",
				"jsonataVar_t * 0.9",
				"jsonataVar_t,",
				"jsonataVar_d,",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, err := jparse.Parse(tt.expr)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			plan, err := transpiler.Analyze(ast)
			if err != nil {
				t.Fatalf("analyze error: %v", err)
			}
			src, err := transpiler.Generate(plan, "test", "", tt.expr)
			if err != nil {
				t.Fatalf("generate error: %v", err)
			}
			code := string(src)
			for _, want := range tt.contains {
				if !strings.Contains(code, want) {
					t.Errorf("generated code missing %q\n\nGenerated:\n%s", want, code)
				}
			}
		})
	}
}

// TestVariableEndToEnd compiles and runs generated code with variable bindings.
func TestVariableEndToEnd(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		input    string
		expected string
	}{
		{
			name:  "single variable avoids duplication",
			expr:  `items[price > 0].($total := price * quantity; {id: id, total: $total, discounted: $total * 0.9})`,
			input: `{"items":[{"id":"A","price":100,"quantity":2},{"id":"B","price":50,"quantity":3}]}`,
			// A: total=200, discounted=180; B: total=150, discounted=135
			expected: `[{"id":"A","total":200,"discounted":180},{"id":"B","total":150,"discounted":135}]`,
		},
		{
			name:  "variable referencing another variable",
			expr:  `items[price > 0].($t := price * quantity; $d := $t * 0.5; {total: $t, half: $d})`,
			input: `{"items":[{"id":"X","price":10,"quantity":4}]}`,
			// t=40, d=20
			expected: `[{"total":40,"half":20}]`,
		},
		{
			name:  "variable in conditional",
			expr:  `items[price > 0].($t := price * quantity; {label: $t > 100 ? "big" : "small", total: $t})`,
			input: `{"items":[{"id":"A","price":10,"quantity":5},{"id":"B","price":50,"quantity":3}]}`,
			// A: t=50 → "small"; B: t=150 → "big"
			expected: `[{"label":"small","total":50},{"label":"big","total":150}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, err := jparse.Parse(tt.expr)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			plan, err := transpiler.Analyze(ast)
			if err != nil {
				t.Fatalf("analyze: %v", err)
			}
			src, err := transpiler.Generate(plan, "main", "", tt.expr)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "transform_gen.go"), src, 0644); err != nil {
				t.Fatal(err)
			}

			mainSrc := `//go:build goexperiment.jsonv2
package main

import (
	"fmt"
	"encoding/json"
	"os"
)

func main() {
	data, _ := os.ReadFile(os.Args[1])
	results, err := ` + plan.FuncName + `(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "transform error: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.Marshal(results)
	fmt.Print(string(out))
}
`
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSrc), 0644); err != nil {
				t.Fatal(err)
			}

			goMod := "module testharness\n\ngo 1.25.0\n"
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
				t.Fatal(err)
			}

			copyRuntimePackage(t, dir)
			fixImports(t, filepath.Join(dir, "transform_gen.go"))

			inputFile := filepath.Join(dir, "input.json")
			if err := os.WriteFile(inputFile, []byte(tt.input), 0644); err != nil {
				t.Fatal(err)
			}

			buildCmd := exec.Command("go", "build", "-o", filepath.Join(dir, "test"), ".")
			buildCmd.Dir = dir
			buildCmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
			if out, err := buildCmd.CombinedOutput(); err != nil {
				t.Fatalf("build failed: %v\n%s\n\nGenerated code:\n%s", err, out, string(src))
			}

			runCmd := exec.Command(filepath.Join(dir, "test"), inputFile)
			runCmd.Dir = dir
			output, err := runCmd.CombinedOutput()
			if err != nil {
				t.Fatalf("run failed: %v\n%s", err, output)
			}

			got := strings.TrimSpace(string(output))
			if got != tt.expected {
				t.Errorf("output mismatch:\n  got:  %s\n  want: %s", got, tt.expected)
			}
		})
	}
}

// projectRoot and copyRuntimePackage are defined in transpiler_test.go.

// fixImports rewrites github.com/gcossani/ssfbff/* imports to testharness/*
// in a generated file so it compiles inside the temporary test module.
func fixImports(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return // file doesn't exist yet or doesn't need fixing
	}
	fixed := strings.ReplaceAll(string(data),
		`"github.com/gcossani/ssfbff/runtime"`,
		`"testharness/runtime"`)
	fixed = strings.ReplaceAll(fixed,
		`"github.com/gcossani/ssfbff/internal/aggregator"`,
		`"testharness/aggregator"`)
	if err := os.WriteFile(path, []byte(fixed), 0644); err != nil {
		t.Fatal(err)
	}
}
