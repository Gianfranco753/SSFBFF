// Command transpiler reads a .jsonata file and generates an optimized Go source
// file that evaluates the expression using jsontext streaming and json/v2 typed
// deserialization. It is designed to be invoked via //go:generate.
//
// Usage:
//
//	go run ./cmd/transpiler --input=query.jsonata --output=query_gen.go --package=mypackage
//
// Or via go:generate in a source file:
//
//	//go:generate go run github.com/gcossani/ssfbff/cmd/transpiler --input=orders.jsonata --output=orders_gen.go --package=mypackage
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blues/jsonata-go/jparse"
	"github.com/gcossani/ssfbff/internal/transpiler"
)

func main() {
	input := flag.String("input", "", "path to the .jsonata source file")
	output := flag.String("output", "", "path for the generated .go file (default: <input>_gen.go)")
	pkg := flag.String("package", "", "Go package name for the generated file (default: main)")
	flag.Parse()

	if *input == "" {
		fatal("--input is required")
	}

	if *pkg == "" {
		*pkg = "main"
	}

	if *output == "" {
		base := strings.TrimSuffix(filepath.Base(*input), filepath.Ext(*input))
		*output = base + "_gen.go"
	}

	exprBytes, err := os.ReadFile(*input)
	if err != nil {
		fatal("reading input: %v", err)
	}
	expression := strings.TrimSpace(string(exprBytes))

	ast, err := jparse.Parse(expression)
	if err != nil {
		fatal("parsing JSONata expression: %v", err)
	}

	// Always use the provider codegen path. This handles both fetch mode
	// (expressions with $fetch()/$service()/$request()) and fetchFilter mode
	// ($fetch(p,e)[filter].{projection}).
	baseName := strings.TrimSuffix(filepath.Base(*input), filepath.Ext(*input))
	funcName := "Transform" + transpiler.ExportedName(baseName)

	plan, err := transpiler.AnalyzeFetchCalls(ast, funcName)
	if err != nil {
		fatal("analyzing expression: %v", err)
	}
	src, err := transpiler.GenerateProvider(plan, *pkg, *input, expression)
	if err != nil {
		fatal("generating Go code: %v", err)
	}

	if err := os.WriteFile(*output, src, 0o644); err != nil {
		fatal("writing output: %v", err)
	}

	fmt.Fprintf(os.Stderr, "transpiler: %s -> %s (%s)\n", *input, *output, funcName)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "transpiler: "+format+"\n", args...)
	os.Exit(1)
}
