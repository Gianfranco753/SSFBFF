package transpiler_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blues/jsonata-go/jparse"
	"github.com/gcossani/ssfbff/internal/transpiler"
)

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
		{
			name: "$request().headers",
			expr: `{"auth": $request().headers.Authorization}`,
			want: true,
		},
		{
			name: "$request().path",
			expr: `{"p": $request().path}`,
			want: true,
		},
		{
			name: "$request().cookies",
			expr: `{"s": $request().cookies.session}`,
			want: true,
		},
		{
			name: "$request().body with path",
			expr: `{"name": $request().body.user.name}`,
			want: true,
		},
		{
			name: "$service() call",
			expr: `{"user": $service("get_user").name}`,
			want: true,
		},
		{
			name: "$service() with $fetch()",
			expr: `{"user": $service("get_user").name, "data": $fetch("svc", "ep").val}`,
			want: true,
		},
		{
			name: "$httpError() call",
			expr: `$httpError(404, "Not found")`,
			want: true,
		},
		{
			name: "$httpResponse() call",
			expr: `$httpResponse(201, {"id": 123})`,
			want: true,
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
	if plan.NeedsRequest {
		t.Error("NeedsRequest should be false for pure $fetch expressions")
	}

	if len(plan.Fields) != 2 {
		t.Fatalf("Fields count = %d, want 2", len(plan.Fields))
	}

	user := plan.Fields[0]
	if user.Kind != "fetch" || user.OutputKey != "user" || user.Provider != "user_service" || user.Endpoint != "profile" {
		t.Errorf("field[0] = %+v, want fetch/user/user_service/profile", user)
	}
	if len(user.JSONPath) != 1 || user.JSONPath[0] != "name" {
		t.Errorf("field[0] JSONPath = %v, want [name]", user.JSONPath)
	}

	balance := plan.Fields[1]
	if balance.Kind != "fetch" || balance.OutputKey != "balance" || balance.Provider != "bank_service" || balance.Endpoint != "accounts" {
		t.Errorf("field[1] = %+v, want fetch/balance/bank_service/accounts", balance)
	}

	if len(plan.Deps) != 2 {
		t.Fatalf("Deps count = %d, want 2", len(plan.Deps))
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
		"TransformDashboardDeps(req runtime.RequestContext)",
		"runtime.ProviderDep",
		`Provider: "user_service"`,
		`Endpoint: "profile"`,
		`Provider: "bank_service"`,
		`Endpoint: "accounts"`,
		"TransformDashboard(results map[string][]byte, req runtime.RequestContext)",
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
}

// TestEndToEnd generates code, writes it to a temp dir alongside a main.go
// that exercises the generated function, then compiles and runs it.
// TestAnalyzeRequestFunctions verifies that $request() path expressions are
// correctly parsed into ProviderField with the right Kind.
func TestAnalyzeRequestFunctions(t *testing.T) {
	expr := `{"auth": $request().headers.Authorization, "session": $request().cookies.session, "page": $request().query.page, "user_id": $request().params.id, "url": $request().path, "verb": $request().method, "name": $request().body.user.name}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformRequest")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if !plan.NeedsRequest {
		t.Error("NeedsRequest should be true")
	}
	if len(plan.Deps) != 0 {
		t.Errorf("Deps should be empty for request-only expressions, got %d", len(plan.Deps))
	}

	expected := []struct {
		key  string
		kind string
		arg  string
	}{
		{"auth", "header", "Authorization"},
		{"session", "cookie", "session"},
		{"page", "query", "page"},
		{"user_id", "param", "id"},
		{"url", "path", ""},
		{"verb", "method", ""},
		{"name", "body", ""},
	}

	if len(plan.Fields) != len(expected) {
		t.Fatalf("Fields count = %d, want %d", len(plan.Fields), len(expected))
	}

	for i, e := range expected {
		f := plan.Fields[i]
		if f.OutputKey != e.key || f.Kind != e.kind {
			t.Errorf("field[%d] = %q/%q, want %q/%q", i, f.OutputKey, f.Kind, e.key, e.kind)
		}
		if e.arg != "" && f.Arg != e.arg {
			t.Errorf("field[%d] Arg = %q, want %q", i, f.Arg, e.arg)
		}
	}

	// Verify $request().body.user.name has the right BodyPath.
	bodyField := plan.Fields[6]
	if len(bodyField.BodyPath) != 2 || bodyField.BodyPath[0] != "user" || bodyField.BodyPath[1] != "name" {
		t.Errorf("body field BodyPath = %v, want [user name]", bodyField.BodyPath)
	}
}

// TestAnalyzeFetchConfig verifies parsing of $fetch() with a 3rd config argument.
func TestAnalyzeFetchConfig(t *testing.T) {
	expr := `{"data": $fetch("svc", "ep", {"method": "POST", "headers": {"Authorization": $request().headers.Authorization, "X-Custom": "static-val"}, "body": {"user": $request().cookies.user}}).result}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformWithConfig")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if !plan.NeedsRequest {
		t.Error("NeedsRequest should be true when fetch config uses $request()")
	}

	if len(plan.Fields) != 1 {
		t.Fatalf("Fields count = %d, want 1", len(plan.Fields))
	}

	f := plan.Fields[0]
	if f.Kind != "fetch" || f.Provider != "svc" || f.Endpoint != "ep" {
		t.Errorf("field = %+v, want fetch/svc/ep", f)
	}
	if f.FetchConfig == nil {
		t.Fatal("FetchConfig should not be nil")
	}
	if f.FetchConfig.Method != "POST" {
		t.Errorf("config Method = %q, want POST", f.FetchConfig.Method)
	}
	if len(f.FetchConfig.Headers) != 2 {
		t.Fatalf("config Headers count = %d, want 2", len(f.FetchConfig.Headers))
	}

	authHeader := f.FetchConfig.Headers[0]
	if authHeader.Key != "Authorization" || authHeader.Value.Kind != "header" || authHeader.Value.Arg != "Authorization" {
		t.Errorf("header[0] = %+v, want Authorization/header", authHeader)
	}

	customHeader := f.FetchConfig.Headers[1]
	if customHeader.Key != "X-Custom" || customHeader.Value.Kind != "static" || customHeader.Value.Static != "static-val" {
		t.Errorf("header[1] = %+v, want X-Custom/static", customHeader)
	}

	if len(f.FetchConfig.Body) != 1 {
		t.Fatalf("config Body count = %d, want 1", len(f.FetchConfig.Body))
	}
	bodyEntry := f.FetchConfig.Body[0]
	if bodyEntry.Key != "user" || bodyEntry.Value.Kind != "cookie" || bodyEntry.Value.Arg != "user" {
		t.Errorf("body[0] = %+v, want user/cookie", bodyEntry)
	}
}

// TestGenerateProviderWithRequestFunctions verifies that generated code for
// $request() references req.Headers, req.Path, etc.
func TestGenerateProviderWithRequestFunctions(t *testing.T) {
	expr := `{"auth": $request().headers.Authorization, "url": $request().path, "data": $fetch("svc", "ep").value}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformMixed")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "testpkg", "mixed.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	code := string(src)

	mustContain := []string{
		"TransformMixedDeps(req runtime.RequestContext)",
		"TransformMixed(results map[string][]byte, req runtime.RequestContext)",
		`req.Headers["Authorization"]`,
		"req.Path",
		"runtime.ExtractPath",
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q\n\ngenerated:\n%s", s, code)
		}
	}
}

// TestGenerateProviderWithFetchConfig verifies generated code for $fetch()
// with a 3rd config argument that shapes the outgoing request.
func TestGenerateProviderWithFetchConfig(t *testing.T) {
	expr := `{"data": $fetch("svc", "ep", {"method": "POST", "headers": {"Authorization": $request().headers.Authorization}}).value}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformConfigged")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "testpkg", "configged.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	code := string(src)

	mustContain := []string{
		`Method:   "POST"`, // go fmt aligns struct fields
		`"Authorization": req.Headers["Authorization"]`,
		"TransformConfiggedDeps(req runtime.RequestContext)",
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q\n\ngenerated:\n%s", s, code)
		}
	}
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
	copyRuntimePackage(t, dir)
	copyAggregatorPackage(t, dir)

	harness := `//go:build goexperiment.jsonv2

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testharness/runtime"
)

func main() {
	results := map[string][]byte{
		"user_service.profile":  []byte(` + "`" + `{"name": "Alice", "age": 30}` + "`" + `),
		"bank_service.accounts": []byte(` + "`" + `{"amount": 42500.75, "currency": "USD"}` + "`" + `),
	}

	reqCtx := runtime.RequestContext{}
	resp, err := TransformDashboard(results, reqCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if resp == nil {
		fmt.Fprintf(os.Stderr, "error: response is nil\n")
		os.Exit(1)
	}

	fmt.Println(string(resp.Body))

	var parsed map[string]any
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
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

	writeTestFiles(t, dir, src, harness)

	// Download dependencies
	modCmd := exec.Command("go", "mod", "tidy")
	modCmd.Dir = dir
	modCmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	if err := modCmd.Run(); err != nil {
		t.Fatalf("go mod tidy failed: %v", err)
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

// TestRequestContextEndToEnd generates a transform using $request(),
// compiles and runs it with a populated RequestContext.
func TestRequestContextEndToEnd(t *testing.T) {
	expr := `{"auth": $request().headers.Authorization, "url": $request().path, "data": $fetch("svc", "ep").value}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformMixed")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "main", "mixed.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	dir := t.TempDir()
	copyRuntimePackage(t, dir)
	copyAggregatorPackage(t, dir)

	harness := `//go:build goexperiment.jsonv2

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testharness/runtime"
)

func main() {
	results := map[string][]byte{
		"svc.ep": []byte(` + "`" + `{"value": 42}` + "`" + `),
	}

	reqCtx := runtime.RequestContext{
		Headers: map[string]string{"Authorization": "Bearer token123"},
		Path:    "/api/v1/test",
	}

	resp, err := TransformMixed(results, reqCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if resp == nil {
		fmt.Fprintf(os.Stderr, "error: response is nil\n")
		os.Exit(1)
	}

	fmt.Println(string(resp.Body))

	var parsed map[string]any
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "parse output error: %v\n", err)
		os.Exit(1)
	}

	if parsed["auth"] != "Bearer token123" {
		fmt.Fprintf(os.Stderr, "expected auth=Bearer token123, got %v\n", parsed["auth"])
		os.Exit(1)
	}
	if parsed["url"] != "/api/v1/test" {
		fmt.Fprintf(os.Stderr, "expected url=/api/v1/test, got %v\n", parsed["url"])
		os.Exit(1)
	}
	if parsed["data"] != float64(42) {
		fmt.Fprintf(os.Stderr, "expected data=42, got %v\n", parsed["data"])
		os.Exit(1)
	}

	fmt.Println("PASS")
}
`

	writeTestFiles(t, dir, src, harness)

	// Download dependencies
	modCmd := exec.Command("go", "mod", "tidy")
	modCmd.Dir = dir
	modCmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	if err := modCmd.Run(); err != nil {
		t.Fatalf("go mod tidy failed: %v", err)
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

// --- $service() tests ---

func TestAnalyzeServiceCalls(t *testing.T) {
	expr := `{"user": $service("get_user").name, "balance": $fetch("bank_svc", "accounts").amount}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformDashboard")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if len(plan.Fields) != 2 {
		t.Fatalf("Fields count = %d, want 2", len(plan.Fields))
	}

	userField := plan.Fields[0]
	if userField.Kind != "service" || userField.ServiceName != "get_user" {
		t.Errorf("field[0] = %+v, want Kind=service, ServiceName=get_user", userField)
	}
	if len(userField.JSONPath) != 1 || userField.JSONPath[0] != "name" {
		t.Errorf("field[0] JSONPath = %v, want [name]", userField.JSONPath)
	}

	balanceField := plan.Fields[1]
	if balanceField.Kind != "fetch" || balanceField.Provider != "bank_svc" {
		t.Errorf("field[1] = %+v, want Kind=fetch, Provider=bank_svc", balanceField)
	}

	if len(plan.Deps) != 1 {
		t.Errorf("Deps count = %d, want 1", len(plan.Deps))
	}
	if len(plan.Services) != 1 || plan.Services[0] != "get_user" {
		t.Errorf("Services = %v, want [get_user]", plan.Services)
	}
}

func TestGenerateServiceCode(t *testing.T) {
	expr := `{"user": $service("get_user").name, "data": $fetch("svc", "ep").value}`
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
		"TransformDashboardDeps(req runtime.RequestContext)",
		"TransformDashboard(results map[string][]byte, req runtime.RequestContext)",
		`results["$service.get_user"]`,
		`runtime.ExtractPath(results["$service.get_user"], "name")`,
		`runtime.ExtractPath(results["svc.ep"], "value")`,
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q\n\ngenerated:\n%s", s, code)
		}
	}
}

func TestGenerateExecuteFunc(t *testing.T) {
	expr := `{"user": $service("get_user").name, "data": $fetch("svc", "ep").value}`
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
		"func ExecuteDashboard(ctx context.Context, agg *aggregator.Aggregator, req runtime.RequestContext)",
		"TransformDashboardDeps(req)",
		"agg.Fetch(ctx, deps)",
		"errgroup.WithContext(ctx)",
		"ExecuteGetUser(gctx, agg, req)",
		`results["$service.get_user"]`,
		"TransformDashboard(results, req)",
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q\n\ngenerated:\n%s", s, code)
		}
	}
}

func TestServiceCompositionEndToEnd(t *testing.T) {
	// Generate the "inner" service: get_user
	innerExpr := `{"id": $fetch("user_svc", "profile").id, "name": $fetch("user_svc", "profile").name}`
	innerAST, err := jparse.Parse(innerExpr)
	if err != nil {
		t.Fatalf("parse inner: %v", err)
	}
	innerPlan, err := transpiler.AnalyzeFetchCalls(innerAST, "TransformGetUser")
	if err != nil {
		t.Fatalf("analyze inner: %v", err)
	}
	innerSrc, err := transpiler.GenerateProvider(innerPlan, "main", "get_user.jsonata", innerExpr)
	if err != nil {
		t.Fatalf("generate inner: %v", err)
	}

	// Generate the "outer" service: dashboard (uses $service("get_user"))
	outerExpr := `{"user_name": $service("get_user").name, "balance": $fetch("bank_svc", "accounts").amount}`
	outerAST, err := jparse.Parse(outerExpr)
	if err != nil {
		t.Fatalf("parse outer: %v", err)
	}
	outerPlan, err := transpiler.AnalyzeFetchCalls(outerAST, "TransformDashboard")
	if err != nil {
		t.Fatalf("analyze outer: %v", err)
	}
	outerSrc, err := transpiler.GenerateProvider(outerPlan, "main", "dashboard.jsonata", outerExpr)
	if err != nil {
		t.Fatalf("generate outer: %v", err)
	}

	dir := t.TempDir()
	copyRuntimePackage(t, dir)
	copyAggregatorPackage(t, dir)

	// Fix imports to use local module path.
	fixImports := func(src []byte) []byte {
		s := string(src)
		s = strings.ReplaceAll(s, `"github.com/gcossani/ssfbff/runtime"`, `"testharness/runtime"`)
		s = strings.ReplaceAll(s, `"github.com/gcossani/ssfbff/internal/aggregator"`, `"testharness/aggregator"`)
		s = strings.ReplaceAll(s, `"golang.org/x/sync/errgroup"`, `"testharness/errgroup"`)
		return []byte(s)
	}

	if err := os.WriteFile(filepath.Join(dir, "get_user_gen.go"), fixImports(innerSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dashboard_gen.go"), fixImports(outerSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	harness := `//go:build goexperiment.jsonv2

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testharness/aggregator"
	"testharness/runtime"
	"time"
)

func main() {
	providers := map[string]aggregator.ProviderConfig{
		"user_svc": {
			BaseURL:   os.Getenv("UPSTREAM_USER_SVC_URL"),
			Timeout:   5 * time.Second,
			Endpoints: map[string]aggregator.EndpointConfig{"profile": {Path: "/profile"}},
		},
		"bank_svc": {
			BaseURL:   os.Getenv("UPSTREAM_BANK_SVC_URL"),
			Timeout:   5 * time.Second,
			Endpoints: map[string]aggregator.EndpointConfig{"accounts": {Path: "/accounts"}},
		},
	}

	clientFactory := func(cfg aggregator.ProviderConfig) *http.Client {
		return http.DefaultClient
	}
	agg := aggregator.New(providers, clientFactory)
	reqCtx := runtime.RequestContext{
		Headers: map[string]string{"Authorization": "Bearer test"},
	}

	resp, err := ExecuteDashboard(context.Background(), agg, reqCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if resp == nil {
		fmt.Fprintf(os.Stderr, "error: response is nil\n")
		os.Exit(1)
	}

	fmt.Println(string(resp.Body))

	var parsed map[string]any
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "parse error: %v\n", err)
		os.Exit(1)
	}

	if parsed["user_name"] != "Alice" {
		fmt.Fprintf(os.Stderr, "expected user_name=Alice, got %v\n", parsed["user_name"])
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

	gomod := `module testharness

go 1.26

require (
	github.com/rs/zerolog v1.33.0
)
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Download dependencies
	modCmd := exec.Command("go", "mod", "tidy")
	modCmd.Dir = dir
	modCmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	if err := modCmd.Run(); err != nil {
		t.Fatalf("go mod tidy failed: %v", err)
	}

	// Start a mock HTTP server that serves user and bank data.
	mockServer := startMockServer(t)
	defer mockServer.Close()

	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GOEXPERIMENT=jsonv2",
		"UPSTREAM_USER_SVC_URL="+mockServer.URL,
		"UPSTREAM_BANK_SVC_URL="+mockServer.URL,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running generated code failed:\n%s\nerror: %v", output, err)
	}

	if !strings.Contains(string(output), "PASS") {
		t.Fatalf("generated code did not PASS:\n%s", output)
	}
	t.Logf("generated code output:\n%s", output)
}

// --- fetchFilter tests ---

// TestAnalyzeFetchFilter verifies that $fetch(p,e)[filter].{proj} is recognized
// as a single fetchFilter ProviderField, and that the embedded QueryPlan has the
// right root field (endpoint name), filter, and projection.
func TestAnalyzeFetchFilter(t *testing.T) {
	expr := `$fetch("orders_service", "data")[price > 100].{id: order_id, total: $sum(items.price)}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformOrders")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if plan.FuncName != "TransformOrders" {
		t.Errorf("FuncName = %q, want TransformOrders", plan.FuncName)
	}
	if len(plan.Fields) != 1 {
		t.Fatalf("Fields count = %d, want 1", len(plan.Fields))
	}

	field := plan.Fields[0]
	if field.Kind != "fetchFilter" {
		t.Errorf("Kind = %q, want fetchFilter", field.Kind)
	}
	if field.Provider != "orders_service" {
		t.Errorf("Provider = %q, want orders_service", field.Provider)
	}
	if field.Endpoint != "data" {
		t.Errorf("Endpoint = %q, want data", field.Endpoint)
	}
	if field.FilterPlan == nil {
		t.Fatal("FilterPlan should not be nil")
	}
	if field.FilterPlan.RootField != "data" {
		t.Errorf("FilterPlan.RootField = %q, want data", field.FilterPlan.RootField)
	}
	if len(field.FilterPlan.Filters) != 1 {
		t.Errorf("FilterPlan.Filters count = %d, want 1", len(field.FilterPlan.Filters))
	}
	if len(field.FilterPlan.OutputFields) != 2 {
		t.Errorf("FilterPlan.OutputFields count = %d, want 2", len(field.FilterPlan.OutputFields))
	}

	if len(plan.Deps) != 1 {
		t.Fatalf("Deps count = %d, want 1", len(plan.Deps))
	}
	if plan.Deps[0].Provider != "orders_service" || plan.Deps[0].Endpoint != "data" {
		t.Errorf("Deps[0] = %+v, want orders_service.data", plan.Deps[0])
	}
}

// TestGenerateFetchFilterCode verifies that GenerateProvider for a fetchFilter plan
// emits both the streaming filter function (TransformOrders) and the Execute wrapper.
func TestGenerateFetchFilterCode(t *testing.T) {
	expr := `$fetch("orders_service", "data")[price > 100].{id: order_id, total: $sum(items.price)}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformOrders")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "testpkg", "orders.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	code := string(src)
	mustContain := []string{
		"package testpkg",
		"goexperiment.jsonv2",
		// Filter pipeline
		"jsontext.NewDecoder",
		"jsonv2.UnmarshalDecode",
		"TransformOrders",
		"OrdersResult",
		`nameTok.String() != "data"`,
		"elem.Price > 100",
		// Execute wrapper
		"func ExecuteOrders(ctx context.Context, agg *aggregator.Aggregator, req runtime.RequestContext)",
		`Provider: "orders_service"`,
		`Endpoint: "data"`,
		`TransformOrders(results["orders_service.data"])`,
		"jsonv2.Marshal(items)",
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q", s)
		}
	}
}

// TestFetchFilterEndToEnd generates a fetchFilter transform, writes it alongside
// a harness that simulates a pre-fetched upstream response, compiles and runs it.
func TestFetchFilterEndToEnd(t *testing.T) {
	expr := `$fetch("orders_service", "data")[price > 100].{id: order_id, total: $sum(items.price)}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformOrders")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "main", "orders.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	dir := t.TempDir()
	copyRuntimePackage(t, dir)
	copyAggregatorPackage(t, dir)

	// The generated TransformOrders reads {"data": [...]} from upstream bytes.
	harness := `//go:build goexperiment.jsonv2

package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	// Simulate what the aggregator returns: raw bytes from the upstream endpoint.
	// TransformOrders expects {"data": [...]} because the endpoint name is "data".
	input := []byte(` + "`" + `{
		"data": [
			{"order_id": "A1", "price": 50,  "items": [{"price": 10}]},
			{"order_id": "A2", "price": 200, "items": [{"price": 30}, {"price": 40}, {"price": 50}]},
			{"order_id": "A3", "price": 150, "items": [{"price": 100}]}
		]
	}` + "`" + `)

	// Call the internal filter function directly.
	results, err := TransformOrders(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	out, _ := json.Marshal(results)
	fmt.Println(string(out))

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
	writeTestFiles(t, dir, src, harness)

	// Download dependencies
	modCmd := exec.Command("go", "mod", "tidy")
	modCmd.Dir = dir
	modCmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	if err := modCmd.Run(); err != nil {
		t.Fatalf("go mod tidy failed: %v", err)
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

// --- Test helpers ---

func copyRuntimePackage(t *testing.T, dir string) {
	t.Helper()
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
}

func copyAggregatorPackage(t *testing.T, dir string) {
	t.Helper()
	aggDir := filepath.Join(dir, "aggregator")
	if err := os.MkdirAll(aggDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Copy aggregator.go
	aggSrc, err := os.ReadFile("../../internal/aggregator/aggregator.go")
	if err != nil {
		t.Fatal(err)
	}
	// Fix imports to use local module path.
	fixed := strings.ReplaceAll(string(aggSrc), `"github.com/gcossani/ssfbff/runtime"`, `"testharness/runtime"`)
	fixed = strings.ReplaceAll(fixed, `"golang.org/x/sync/errgroup"`, `"testharness/errgroup"`)
	if err := os.WriteFile(filepath.Join(aggDir, "aggregator.go"), []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	// Copy observability.go
	obsSrc, err := os.ReadFile("../../internal/aggregator/observability.go")
	if err != nil {
		t.Fatal(err)
	}
	obsFixed := strings.ReplaceAll(string(obsSrc), `"github.com/gcossani/ssfbff/runtime"`, `"testharness/runtime"`)
	if err := os.WriteFile(filepath.Join(aggDir, "observability.go"), []byte(obsFixed), 0o644); err != nil {
		t.Fatal(err)
	}

	// The aggregator depends on errgroup — provide a minimal shim.
	copyErrgroupShim(t, dir)
}

func copyErrgroupShim(t *testing.T, dir string) {
	t.Helper()
	errgroupDir := filepath.Join(dir, "errgroup")
	if err := os.MkdirAll(errgroupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := `package errgroup

import (
	"context"
	"sync"
)

type Group struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	err    error
}

func WithContext(ctx context.Context) (*Group, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &Group{ctx: ctx, cancel: cancel}, ctx
}

func (g *Group) Go(f func() error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := f(); err != nil {
			g.mu.Lock()
			if g.err == nil {
				g.err = err
				g.cancel()
			}
			g.mu.Unlock()
		}
	}()
}

func (g *Group) Wait() error {
	g.wg.Wait()
	g.cancel()
	return g.err
}
`
	if err := os.WriteFile(filepath.Join(errgroupDir, "errgroup.go"), []byte(shim), 0o644); err != nil {
		t.Fatal(err)
	}
}

func startMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id": 42, "name": "Alice", "email": "alice@example.com"}`))
	})
	mux.HandleFunc("/accounts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"amount": 42500.75, "currency": "USD"}`))
	})
	return httptest.NewServer(mux)
}

// TestAnalyzeHttpError verifies that $httpError() is correctly parsed.
func TestAnalyzeHttpError(t *testing.T) {
	expr := `$httpError(404, "Order not found")`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformGetOrder")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if len(plan.Fields) != 1 {
		t.Fatalf("Fields count = %d, want 1", len(plan.Fields))
	}

	field := plan.Fields[0]
	if field.Kind != "error" {
		t.Errorf("field.Kind = %q, want %q", field.Kind, "error")
	}
	if field.StatusCode != 404 {
		t.Errorf("field.StatusCode = %d, want 404", field.StatusCode)
	}
	if field.ErrorMessage != "Order not found" {
		t.Errorf("field.ErrorMessage = %q, want %q", field.ErrorMessage, "Order not found")
	}
}

// TestAnalyzeHttpResponse verifies that $httpResponse() is correctly parsed.
func TestAnalyzeHttpResponse(t *testing.T) {
	expr := `$httpResponse(201, {"id": 123, "name": "test"}, {"Location": "/orders/123"})`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformCreateOrder")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if len(plan.Fields) != 1 {
		t.Fatalf("Fields count = %d, want 1", len(plan.Fields))
	}

	field := plan.Fields[0]
	if field.Kind != "response" {
		t.Errorf("field.Kind = %q, want %q", field.Kind, "response")
	}
	if field.StatusCode != 201 {
		t.Errorf("field.StatusCode = %d, want 201", field.StatusCode)
	}
	if field.BodyExpr == nil {
		t.Error("field.BodyExpr should not be nil")
	}
	if field.Headers == nil || len(field.Headers) == 0 {
		t.Error("field.Headers should not be empty")
	}
}

// TestAnalyzeHttpErrorInConditional verifies that $httpError() works in conditionals.
func TestAnalyzeHttpErrorInConditional(t *testing.T) {
	expr := `$count($fetch("orders_service", "data")[order_id = $request().params.id]) = 0 
		? $httpError(404, "Order not found")
		: $fetch("orders_service", "data")[order_id = $request().params.id][0]`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformGetOrder")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	// Should have one field with kind "expr" containing a conditional
	if len(plan.Fields) != 1 {
		t.Fatalf("Fields count = %d, want 1", len(plan.Fields))
	}

	field := plan.Fields[0]
	if field.Kind != "expr" {
		t.Errorf("field.Kind = %q, want %q", field.Kind, "expr")
	}
	if field.ValueExpr == nil {
		t.Fatal("field.ValueExpr should not be nil")
	}
	if field.ValueExpr.Kind != "conditional" {
		t.Errorf("field.ValueExpr.Kind = %q, want %q", field.ValueExpr.Kind, "conditional")
	}
	if field.ValueExpr.Then == nil || field.ValueExpr.Then.Kind != "error" {
		t.Errorf("Then branch should be error, got %v", field.ValueExpr.Then)
	}
	if field.ValueExpr.Then.StatusCode != 404 {
		t.Errorf("Then.StatusCode = %d, want 404", field.ValueExpr.Then.StatusCode)
	}
}

// TestGenerateHttpErrorCode verifies that generated code for $httpError() is correct.
func TestGenerateHttpErrorCode(t *testing.T) {
	expr := `$httpError(404, "Not found")`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformError")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "testpkg", "error.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	code := string(src)
	mustContain := []string{
		"package testpkg",
		"runtime.NewHTTPError(404",
		`"Not found"`,
		"*runtime.Response",
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q", s)
		}
	}
}

// TestGenerateHttpResponseCode verifies that generated code for $httpResponse() is correct.
func TestGenerateHttpResponseCode(t *testing.T) {
	expr := `$httpResponse(201, {"id": 123}, {"Location": "/orders/123"})`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformCreate")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "testpkg", "create.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	code := string(src)
	mustContain := []string{
		"package testpkg",
		"StatusCode: 201",
		"*runtime.Response",
		"headers",
		"Location",
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q", s)
		}
	}
}

func writeTestFiles(t *testing.T, dir string, generatedSrc []byte, harness string) {
	t.Helper()

	// Fix import paths in generated code to use local module.
	genCode := string(generatedSrc)
	genCode = strings.ReplaceAll(genCode, `"github.com/gcossani/ssfbff/runtime"`, `"testharness/runtime"`)
	genCode = strings.ReplaceAll(genCode, `"github.com/gcossani/ssfbff/internal/aggregator"`, `"testharness/aggregator"`)
	genCode = strings.ReplaceAll(genCode, `"golang.org/x/sync/errgroup"`, `"testharness/errgroup"`)
	if err := os.WriteFile(filepath.Join(dir, "transform_gen.go"), []byte(genCode), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}

	gomod := `module testharness

go 1.26

require (
	github.com/rs/zerolog v1.33.0
)
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
}
