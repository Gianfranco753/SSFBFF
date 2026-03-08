package transpiler_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gcossani/ssfbff/internal/transpiler"
	"github.com/xiatechs/jsonata-go/jparse"
)

func TestStripJSONataComments(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no comments", `foo.bar`, `foo.bar`},
		{"empty", ``, ``},
		{"comment at start", `/* x */ expr`, ` expr`},
		{"comment in middle", `a /* x */ b`, `a  b`},
		{"comment at end", `expr /* x */`, `expr `},
		{"double-quoted string preserves comment", `"foo /* not */ bar"`, `"foo /* not */ bar"`},
		{"single-quoted string preserves comment", `'foo /* not */ bar'`, `'foo /* not */ bar'`},
		{"escaped double quote in string", `"a \" /* still in string */ b"`, `"a \" /* still in string */ b"`},
		{"escaped single quote in string", `'a \' /* still in string */ b'`, `'a \' /* still in string */ b'`},
		{"consecutive comments", `a /* c1 */ /* c2 */ b`, `a   b`},
		{"comment to end of input", `code /* unclosed`, `code `},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := transpiler.StripJSONataComments(tt.in)
			if got != tt.want {
				t.Errorf("StripJSONataComments(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
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
			name: "$params() call",
			expr: `{"user_id": $params().id}`,
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

func TestAnalyzeParamsFunctions(t *testing.T) {
	expr := `{"user_id": $params().id, "raw": $params()}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformParams")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if len(plan.Fields) != 2 {
		t.Fatalf("Fields count = %d, want 2", len(plan.Fields))
	}

	idField := plan.Fields[0]
	if idField.Kind != "serviceParam" {
		t.Fatalf("field[0].Kind = %q, want serviceParam", idField.Kind)
	}
	if len(idField.ParamsPath) != 1 || idField.ParamsPath[0] != "id" {
		t.Errorf("field[0].ParamsPath = %v, want [id]", idField.ParamsPath)
	}

	rawField := plan.Fields[1]
	if rawField.Kind != "serviceParam" {
		t.Fatalf("field[1].Kind = %q, want serviceParam", rawField.Kind)
	}
	if len(rawField.ParamsPath) != 0 {
		t.Errorf("field[1].ParamsPath = %v, want empty path", rawField.ParamsPath)
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

// TestGenerateProviderFetchValuePassThrough verifies that when $fetch is used
// as a value (no path), the generated code uses jsontext.Value() so scalars
// and any JSON are passed through to the client.
func TestGenerateProviderFetchValuePassThrough(t *testing.T) {
	expr := `{"value": $fetch("p1", "e1")}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformValue")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	src, err := transpiler.GenerateProvider(plan, "testpkg", "value.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}
	code := string(src)
	if !strings.Contains(code, `jsontext.Value(results["p1.e1"])`) {
		t.Errorf("generated code should use jsontext.Value for no-path fetch so scalars pass through; got:\n%s", code)
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

// TestHttpResponseRedirectEndToEnd runs generated code for $httpResponse(301, ..., Location) and checks redirect semantics.
func TestHttpResponseRedirectEndToEnd(t *testing.T) {
	expr := `$httpResponse(301, null, {"Location": "/next"})`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformRedirect")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	src, err := transpiler.GenerateProvider(plan, "main", "redirect.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	dir := t.TempDir()
	copyRuntimePackage(t, dir)
	copyAggregatorPackage(t, dir)

	harness := `//go:build goexperiment.jsonv2

package main

import (
	"fmt"
	"os"
	"testharness/runtime"
)

func main() {
	req := runtime.RequestContext{}
	resp, err := TransformRedirect(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if resp == nil {
		fmt.Fprintf(os.Stderr, "response is nil\n")
		os.Exit(1)
	}
	if resp.StatusCode != 301 {
		fmt.Fprintf(os.Stderr, "StatusCode = %d, want 301\n", resp.StatusCode)
		os.Exit(1)
	}
	if resp.Headers["Location"] != "/next" {
		fmt.Fprintf(os.Stderr, "Location = %q, want /next\n", resp.Headers["Location"])
		os.Exit(1)
	}
	fmt.Println("PASS")
}
`
	writeTestFiles(t, dir, src, harness)
	runGoModTidyAndRun(t, dir)
}

// TestHttpResponse204EndToEnd runs generated code for $httpResponse(204, null) and checks 204 No Content.
func TestHttpResponse204EndToEnd(t *testing.T) {
	expr := `$httpResponse(204, null)`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformNoContent")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	src, err := transpiler.GenerateProvider(plan, "main", "no_content.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	dir := t.TempDir()
	copyRuntimePackage(t, dir)
	copyAggregatorPackage(t, dir)

	harness := `//go:build goexperiment.jsonv2

package main

import (
	"fmt"
	"os"
	"testharness/runtime"
)

func main() {
	req := runtime.RequestContext{}
	resp, err := TransformNoContent(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if resp == nil {
		fmt.Fprintf(os.Stderr, "response is nil\n")
		os.Exit(1)
	}
	if resp.StatusCode != 204 {
		fmt.Fprintf(os.Stderr, "StatusCode = %d, want 204\n", resp.StatusCode)
		os.Exit(1)
	}
	// Body may be nil or JSON "null" (4 bytes) depending on codegen
	if resp.Body != nil && len(resp.Body) != 0 && string(resp.Body) != "null" {
		fmt.Fprintf(os.Stderr, "204 body should be empty or null, got %q\n", resp.Body)
		os.Exit(1)
	}
	fmt.Println("PASS")
}
`
	writeTestFiles(t, dir, src, harness)
	runGoModTidyAndRun(t, dir)
}

// TestHttpErrorInConditionalElseEndToEnd runs generated code for cond ? normal : $httpError() and verifies the else branch returns the error.
func TestHttpErrorInConditionalElseEndToEnd(t *testing.T) {
	expr := `false ? {"a": 1} : $httpError(404, "not found")`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformCondErr")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	src, err := transpiler.GenerateProvider(plan, "main", "cond_err.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	dir := t.TempDir()
	copyRuntimePackage(t, dir)
	copyAggregatorPackage(t, dir)

	harness := `//go:build goexperiment.jsonv2

package main

import (
	"fmt"
	"os"
	"testharness/runtime"
)

func main() {
	req := runtime.RequestContext{}
	resp, err := TransformCondErr(req)
	if err == nil {
		fmt.Fprintf(os.Stderr, "expected error from else branch, got nil\n")
		os.Exit(1)
	}
	if resp != nil {
		fmt.Fprintf(os.Stderr, "expected nil response when error, got %v\n", resp)
		os.Exit(1)
	}
	httpErr, ok := err.(*runtime.HTTPError)
	if !ok {
		fmt.Fprintf(os.Stderr, "expected *runtime.HTTPError, got %T\n", err)
		os.Exit(1)
	}
	if httpErr.StatusCode != 404 {
		fmt.Fprintf(os.Stderr, "StatusCode = %d, want 404\n", httpErr.StatusCode)
		os.Exit(1)
	}
	if httpErr.Message != "not found" {
		fmt.Fprintf(os.Stderr, "Message = %q, want \"not found\"\n", httpErr.Message)
		os.Exit(1)
	}
	fmt.Println("PASS")
}
`
	writeTestFiles(t, dir, src, harness)
	runGoModTidyAndRun(t, dir)
}

func runGoModTidyAndRun(t *testing.T, dir string) {
	t.Helper()
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
	expr := `{"user": $service("get_user", {"id": $request().params.id, "auth": $request().headers.Authorization}).name, "balance": $fetch("bank_svc", "accounts").amount}`
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
	if userField.ServiceParamsExpr == nil {
		t.Fatal("field[0].ServiceParamsExpr should not be nil")
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
	if len(plan.ServiceCalls) != 1 {
		t.Fatalf("ServiceCalls count = %d, want 1", len(plan.ServiceCalls))
	}
	if plan.ServiceCalls[0].ResultKey != "$service.get_user" {
		t.Errorf("ServiceCalls[0].ResultKey = %q, want %q", plan.ServiceCalls[0].ResultKey, "$service.get_user")
	}
}

func TestAnalyzeServiceCallValidation(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want string
	}{
		{
			name: "too many args",
			expr: `{"user": $service("get_user", {"id": "1"}, {"extra": true})}`,
			want: "$service() requires 1 or 2 arguments",
		},
		{
			name: "non object params",
			expr: `{"user": $service("get_user", "abc")}`,
			want: "$service() second argument must be an object",
		},
		{
			name: "fetch in params",
			expr: `{"user": $service("get_user", {"id": $fetch("user_svc", "profile").id})}`,
			want: "$service() params cannot contain $fetch()",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, err := jparse.Parse(tt.expr)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			_, err = transpiler.AnalyzeFetchCalls(ast, "TransformDashboard")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("AnalyzeFetchCalls error = %v, want substring %q", err, tt.want)
			}
		})
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
		"childReq := req",
		"childReq.ServiceParams = nil",
		"ExecuteGetUser(gctx, agg, childReq)",
		`results["$service.get_user"]`,
		"TransformDashboard(results, req)",
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q\n\ngenerated:\n%s", s, code)
		}
	}
}

func TestGenerateExecuteFuncWithServiceParams(t *testing.T) {
	expr := `{"user": $service("get_user", {"id": $request().params.id, "auth": $request().headers.Authorization}).name}`
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
		`childReq.ServiceParams = map[string]any{`,
		`req.Params["id"]`,
		`req.Headers["Authorization"]`,
		`ExecuteGetUser(gctx, agg, childReq)`,
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q\n\ngenerated:\n%s", s, code)
		}
	}
	if strings.Contains(code, `jsonv2.Marshal(`) {
		t.Errorf("generated code should not marshal service params:\n%s", code)
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

func TestServiceCompositionWithParamsEndToEnd(t *testing.T) {
	innerExpr := `{"id": $fetch("user_svc", "profile", {"headers": {"Authorization": $params().auth, "X-User-ID": $params().id}}).id, "name": $fetch("user_svc", "profile", {"headers": {"Authorization": $params().auth, "X-User-ID": $params().id}}).name}`
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

	outerExpr := `{"user_name": $service("get_user", {"id": $request().params.id, "auth": $request().headers.Authorization}).name, "balance": $fetch("bank_svc", "accounts").amount}`
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
		Headers: map[string]string{"Authorization": "Bearer child-token"},
		Params:  map[string]string{"id": "user-123"},
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

	var parsed map[string]any
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "parse error: %v\n", err)
		os.Exit(1)
	}

	if parsed["user_name"] != "Alice-user-123" {
		fmt.Fprintf(os.Stderr, "expected user_name=Alice-user-123, got %v\n", parsed["user_name"])
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

	modCmd := exec.Command("go", "mod", "tidy")
	modCmd.Dir = dir
	modCmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	if err := modCmd.Run(); err != nil {
		t.Fatalf("go mod tidy failed: %v", err)
	}

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/profile":
			if got := r.Header.Get("Authorization"); got != "Bearer child-token" {
				http.Error(w, "missing Authorization", http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("X-User-ID"); got != "user-123" {
				http.Error(w, "missing X-User-ID", http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"id": 42, "name": "Alice-user-123"}`))
		case "/accounts":
			w.Write([]byte(`{"amount": 42500.75, "currency": "USD"}`))
		default:
			http.NotFound(w, r)
		}
	}))
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

// TestAnalyzeFetchFilterProjectionOnly verifies that $fetch(p,e).{proj} (no filter)
// is recognized as fetchFilter with empty Filters.
func TestAnalyzeFetchFilterProjectionOnly(t *testing.T) {
	expr := `$fetch("orders_service", "data").{id: userId, title: title}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformOrders")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if len(plan.Fields) != 1 {
		t.Fatalf("Fields count = %d, want 1", len(plan.Fields))
	}
	field := plan.Fields[0]
	if field.Kind != "fetchFilter" {
		t.Errorf("Kind = %q, want fetchFilter", field.Kind)
	}
	if field.Provider != "orders_service" || field.Endpoint != "data" {
		t.Errorf("Provider = %q Endpoint = %q, want orders_service data", field.Provider, field.Endpoint)
	}
	if field.FilterPlan == nil {
		t.Fatal("FilterPlan should not be nil")
	}
	if len(field.FilterPlan.Filters) != 0 {
		t.Errorf("FilterPlan.Filters count = %d, want 0 (projection only)", len(field.FilterPlan.Filters))
	}
	if len(field.FilterPlan.OutputFields) != 2 {
		t.Fatalf("FilterPlan.OutputFields count = %d, want 2", len(field.FilterPlan.OutputFields))
	}
	names := make(map[string]bool)
	for _, of := range field.FilterPlan.OutputFields {
		names[of.JSONName] = true
	}
	if !names["id"] || !names["title"] {
		t.Errorf("OutputFields = %v, want id and title", names)
	}
}

// TestGenerateFetchFilterProjectionOnlyCode verifies that GenerateProvider for
// $fetch(p,e).{proj} (no filter) produces compilable code.
func TestGenerateFetchFilterProjectionOnlyCode(t *testing.T) {
	expr := `$fetch("orders_service", "data").{id: userId, title: title}`
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
	for _, s := range []string{"package testpkg", "TransformOrders", "OrdersResult", "ExecuteOrders", `Provider: "orders_service"`, `Endpoint: "data"`} {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q", s)
		}
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

// TestFetchFilterArrayRootAndScalarRoot verifies that the transform accepts
// array-at-root and scalar-at-root: array is processed like object-with-key,
// scalar returns empty slice with no error.
func TestFetchFilterArrayRootAndScalarRoot(t *testing.T) {
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
	harness := `//go:build goexperiment.jsonv2

package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	// Array at root: same elements as object-with-data would have.
	arrayInput := []byte(` + "`" + `[
		{"order_id": "A1", "price": 50,  "items": [{"price": 10}]},
		{"order_id": "A2", "price": 200, "items": [{"price": 30}, {"price": 40}, {"price": 50}]},
		{"order_id": "A3", "price": 150, "items": [{"price": 100}]}
	]` + "`" + `)
	results, err := TransformOrders(arrayInput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "array root error: %v\n", err)
		os.Exit(1)
	}
	if len(results) != 2 {
		fmt.Fprintf(os.Stderr, "array root: expected 2 results, got %d\n", len(results))
		os.Exit(1)
	}
	out, _ := json.Marshal(results)
	fmt.Println("array:", string(out))

	// Scalar at root: empty slice, no error.
	for _, scalar := range []string{"42", "true", "null"} {
		results, err := TransformOrders([]byte(scalar))
		if err != nil {
			fmt.Fprintf(os.Stderr, "scalar %s error: %v\n", scalar, err)
			os.Exit(1)
		}
		if len(results) != 0 {
			fmt.Fprintf(os.Stderr, "scalar %s: expected 0 results, got %d\n", scalar, len(results))
			os.Exit(1)
		}
	}
	fmt.Println("PASS")
}
`
	writeTestFiles(t, dir, src, harness)
	modCmd := exec.Command("go", "mod", "tidy")
	modCmd.Dir = dir
	modCmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	if err := modCmd.Run(); err != nil {
		t.Fatalf("go mod tidy: %v", err)
	}
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "PASS") {
		t.Fatalf("expected PASS in output:\n%s", output)
	}
}

// TestFetchFilterSingleObjectProjectionEndToEnd verifies that when the provider
// returns a single object (no "data" key) and the expression is projection-only,
// the transform returns one projected object.
func TestFetchFilterSingleObjectProjectionEndToEnd(t *testing.T) {
	expr := `$fetch("orders_service", "data").{id: userId, title: title}`
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
	harness := `//go:build goexperiment.jsonv2

package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	// Single object at root (no "data" key).
	input := []byte(` + "`" + `{"userId": 1, "id": 1, "title": "tit", "body": "bod"}` + "`" + `)
	results, err := TransformOrders(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if len(results) != 1 {
		fmt.Fprintf(os.Stderr, "expected 1 result, got %d\n", len(results))
		os.Exit(1)
	}
	if results[0].ID != float64(1) && results[0].ID != 1 {
		fmt.Fprintf(os.Stderr, "expected id=1, got %v\n", results[0].ID)
		os.Exit(1)
	}
	if results[0].Title != "tit" {
		fmt.Fprintf(os.Stderr, "expected title=tit, got %q\n", results[0].Title)
		os.Exit(1)
	}
	out, _ := json.Marshal(results[0])
	fmt.Println(string(out))
	fmt.Println("PASS")
}
`
	writeTestFiles(t, dir, src, harness)
	modCmd := exec.Command("go", "mod", "tidy")
	modCmd.Dir = dir
	modCmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	if err := modCmd.Run(); err != nil {
		t.Fatalf("go mod tidy: %v", err)
	}
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "PASS") {
		t.Fatalf("expected PASS in output:\n%s", output)
	}
	if !strings.Contains(string(output), `"id":1`) || !strings.Contains(string(output), `"title":"tit"`) {
		t.Errorf("expected single projected object in output:\n%s", output)
	}
}

// --- range map ([a..b].{key: value}) tests ---

// TestAnalyzeRangeMap verifies that [a..b].{proj} is recognized as RangeMap plan
// with correct StartExpr, EndExpr, and OutputFields.
func TestAnalyzeRangeMap(t *testing.T) {
	expr := `[0..3].{n: $}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformRangeMap")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if plan.RangeMap == nil {
		t.Fatal("Plan.RangeMap should not be nil")
	}
	rm := plan.RangeMap

	if rm.StartExpr == nil || rm.StartExpr.Kind != "literal" || rm.StartExpr.LiteralValue != "0" {
		t.Errorf("StartExpr = %+v, want literal 0", rm.StartExpr)
	}
	if rm.EndExpr == nil || rm.EndExpr.Kind != "literal" || rm.EndExpr.LiteralValue != "3" {
		t.Errorf("EndExpr = %+v, want literal 3", rm.EndExpr)
	}
	if len(rm.OutputFields) != 1 {
		t.Fatalf("OutputFields count = %d, want 1", len(rm.OutputFields))
	}
	if rm.OutputFields[0].JSONName != "n" || rm.OutputFields[0].Value == nil || rm.OutputFields[0].Value.Kind != "rootRef" {
		t.Errorf("OutputFields[0] = %+v, want JSONName n and Value rootRef", rm.OutputFields[0])
	}
}

// TestAnalyzeRangeMapWithBindings verifies (bindings; [a..b].{...}) is recognized.
func TestAnalyzeRangeMapWithBindings(t *testing.T) {
	expr := `($start := 1; $end := 2; [$start..$end].{x: $})`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformRangeMap")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if plan.RangeMap == nil {
		t.Fatal("Plan.RangeMap should not be nil")
	}
	if len(plan.RootBindings) != 2 {
		t.Errorf("RootBindings count = %d, want 2", len(plan.RootBindings))
	}
	rm := plan.RangeMap
	if len(rm.OutputFields) != 1 || rm.OutputFields[0].JSONName != "x" {
		t.Errorf("OutputFields = %+v, want one field x", rm.OutputFields)
	}
}

// TestGenerateRangeMapCode verifies that GenerateProvider for a RangeMap plan
// emits runtime.Range, loop, and marshal.
func TestGenerateRangeMapCode(t *testing.T) {
	expr := `[0..3].{n: $}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformRangeMap")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "testpkg", "range_map.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	code := string(src)
	mustContain := []string{
		"package testpkg",
		"goexperiment.jsonv2",
		"runtime.Range",
		"for _, elem := range arr",
		`m["n"]`,
		"jsonv2.Marshal(results)",
		"TransformRangeMap",
	}

	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q", s)
		}
	}
}

// TestRangeMapEndToEnd generates a range-map transform, compiles and runs it,
// and checks the response body is the expected JSON array.
func TestRangeMapEndToEnd(t *testing.T) {
	expr := `[0..3].{n: $}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformRangeMap")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "main", "range_map.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	dir := t.TempDir()
	copyRuntimePackage(t, dir)

	writeTestFiles(t, dir, src, `//go:build goexperiment.jsonv2

package main

import (
	"fmt"
	"os"

	"testharness/runtime"
)

func main() {
	req := runtime.RequestContext{}
	resp, err := TransformRangeMap(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if resp == nil {
		fmt.Fprintf(os.Stderr, "nil response\n")
		os.Exit(1)
	}
	fmt.Print(string(resp.Body))
}
`)

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

	// Expect JSON array of 4 objects: [{"n":0},{"n":1},{"n":2},{"n":3}]
	expected := `[{"n":0},{"n":1},{"n":2},{"n":3}]`
	got := strings.TrimSpace(string(output))
	if got != expected {
		t.Errorf("output = %q, want %q", got, expected)
	}
	t.Logf("generated code output:\n%s", output)
}

// --- First-class functions ---

// TestFirstClassFuncAnalyzer verifies that a block with a lambda assignment and a call
// to that variable produces a plan with RootBindings (assign with lambda) and RootExpr (funcCall "f").
func TestFirstClassFuncAnalyzer(t *testing.T) {
	expr := `($f := function($x) { $x * 2 }; $f(5))`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "FirstClass")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	if plan.RootExpr == nil {
		t.Fatal("plan.RootExpr should not be nil")
	}
	if len(plan.RootBindings) != 1 {
		t.Fatalf("RootBindings count = %d, want 1", len(plan.RootBindings))
	}
	binding := plan.RootBindings[0]
	if binding.Kind != "assign" || binding.Left == nil || binding.Left.Kind != "lambda" {
		t.Errorf("first binding should be assign of lambda, got Kind=%s Left.Kind=%s", binding.Kind, safeKind(binding.Left))
	}
	if plan.RootExpr.Kind != "funcCall" || plan.RootExpr.FuncName != "f" {
		t.Errorf("RootExpr = Kind %s FuncName %q, want funcCall \"f\"", plan.RootExpr.Kind, plan.RootExpr.FuncName)
	}
	if len(plan.RootExpr.FuncArgs) != 1 {
		t.Errorf("RootExpr.FuncArgs length = %d, want 1", len(plan.RootExpr.FuncArgs))
	}
}

func safeKind(e *transpiler.Expr) string {
	if e == nil {
		return "<nil>"
	}
	return e.Kind
}

// TestFirstClassFuncCodegen verifies that ($f := function($x) { $x * 2 }; $f(5))
// generates a call to jsonataVar_f(...) and does not emit unsupported.
func TestFirstClassFuncCodegen(t *testing.T) {
	expr := `($f := function($x) { $x * 2 }; $f(5))`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "FirstClass")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	src, err := transpiler.GenerateProvider(plan, "testpkg", "first_class.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}
	code := string(src)
	if !strings.Contains(code, "jsonataVar_f(") {
		t.Errorf("generated code should call jsonataVar_f(...); got:\n%s", code)
	}
	if strings.Contains(code, "nil /* unsupported: $f(") {
		t.Errorf("generated code should not emit unsupported for $f(...); got:\n%s", code)
	}
}

// TestStandaloneLambdaCodegen verifies that a standalone lambda in an expression
// is assigned to a temp variable (jsonataLambda_N) so the code is valid.
func TestStandaloneLambdaCodegen(t *testing.T) {
	// Block whose last expression is a lambda (no assignment).
	expr := `($x := 1; function($y) { $y })`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "StandaloneLambda")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	src, err := transpiler.GenerateProvider(plan, "testpkg", "standalone_lambda.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}
	code := string(src)
	if !strings.Contains(code, "jsonataLambda_") {
		t.Errorf("generated code should assign standalone lambda to jsonataLambda_N; got:\n%s", code)
	}
}

// TestFirstClassFuncUnknownVarRegression verifies that a call to an unknown variable
// (not bound to a lambda) still emits the unsupported comment and does not generate
// jsonataVar_<name>(...), so we do not accidentally treat any variable as callable.
func TestFirstClassFuncUnknownVarRegression(t *testing.T) {
	expr := `($x := 1; $notAFunc(1))`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "UnknownVar")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	src, err := transpiler.GenerateProvider(plan, "testpkg", "unknown_var.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}
	code := string(src)
	if !strings.Contains(code, "nil /* unsupported: $notAFunc(") {
		t.Errorf("generated code should emit unsupported for $notAFunc(...); got:\n%s", code)
	}
	if strings.Contains(code, "jsonataVar_notAFunc(") {
		t.Errorf("generated code should not call jsonataVar_notAFunc (unknown var); got:\n%s", code)
	}
}

// --- Test helpers ---

func copyRuntimePackage(t *testing.T, dir string) {
	t.Helper()
	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Copy helpers.go
	runtimeSrc, err := os.ReadFile("../../runtime/helpers.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "helpers.go"), runtimeSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	// Copy errors.go
	errorsSrc, err := os.ReadFile("../../runtime/errors.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "errors.go"), errorsSrc, 0o644); err != nil {
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
	if len(field.Headers) == 0 {
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

// TestAnalyzeHttpErrorInConditionalElse verifies that $httpError() in the else branch is parsed.
func TestAnalyzeHttpErrorInConditionalElse(t *testing.T) {
	expr := `$request().params.id = "" ? {"ok": true} : $httpError(400, "id required")`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformCheckId")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if len(plan.Fields) != 1 {
		t.Fatalf("Fields count = %d, want 1", len(plan.Fields))
	}
	field := plan.Fields[0]
	if field.Kind != "expr" || field.ValueExpr == nil || field.ValueExpr.Kind != "conditional" {
		t.Fatalf("expected expr with conditional, got Kind=%q ValueExpr=%v", field.Kind, field.ValueExpr)
	}
	if field.ValueExpr.Else == nil || field.ValueExpr.Else.Kind != "error" {
		t.Errorf("Else branch should be error, got %v", field.ValueExpr.Else)
	}
	if field.ValueExpr.Else != nil && field.ValueExpr.Else.StatusCode != 400 {
		t.Errorf("Else.StatusCode = %d, want 400", field.ValueExpr.Else.StatusCode)
	}
}

// TestAnalyzeHttpResponseInConditionalElse verifies that $httpResponse() in the else branch is parsed.
func TestAnalyzeHttpResponseInConditionalElse(t *testing.T) {
	expr := `false ? {"a": 1} : $httpResponse(204, null)`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformMaybeEmpty")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	if len(plan.Fields) != 1 {
		t.Fatalf("Fields count = %d, want 1", len(plan.Fields))
	}
	field := plan.Fields[0]
	if field.Kind != "expr" || field.ValueExpr == nil || field.ValueExpr.Kind != "conditional" {
		t.Fatalf("expected expr with conditional, got Kind=%q ValueExpr=%v", field.Kind, field.ValueExpr)
	}
	if field.ValueExpr.Else == nil || field.ValueExpr.Else.Kind != "response" {
		t.Errorf("Else branch should be response, got %v", field.ValueExpr.Else)
	}
	if field.ValueExpr.Else != nil && field.ValueExpr.Else.ResponseStatusCode != 204 {
		t.Errorf("Else.ResponseStatusCode = %d, want 204", field.ValueExpr.Else.ResponseStatusCode)
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

// TestGenerateConditionalElseHttpErrorCode verifies codegen when the else branch returns $httpError().
func TestGenerateConditionalElseHttpErrorCode(t *testing.T) {
	expr := `$request().params.id = "" ? {"ok": true} : $httpError(400, "id required")`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformCheckId")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	src, err := transpiler.GenerateProvider(plan, "testpkg", "check_id.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}

	code := string(src)
	mustContain := []string{
		"runtime.NewHTTPError(400",
		`"id required"`,
		"req.Params",
	}
	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q", s)
		}
	}
}

// TestRequestFields verifies that RequestFields() collects and dedupes refs from fields, fetch config, and root bindings.
func TestRequestFields(t *testing.T) {
	expr := `{
		"h": $request().headers.Auth,
		"c": $request().cookies.Sess,
		"q": $request().query.Page,
		"p": $request().params.Id,
		"data": $fetch("svc", "ep", {"headers": {"X-Token": $request().headers.XToken}, "body": {"user": $request().body.user}}).value
	}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformRequestFields")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	rf := plan.RequestFields()

	if len(rf.Headers) != 2 {
		t.Errorf("Headers = %v, want [Auth, XToken] (deduped)", rf.Headers)
	}
	wantHeaders := map[string]bool{"Auth": true, "XToken": true}
	for _, h := range rf.Headers {
		if !wantHeaders[h] {
			t.Errorf("unexpected header %q", h)
		}
	}
	if len(rf.Cookies) != 1 || rf.Cookies[0] != "Sess" {
		t.Errorf("Cookies = %v, want [Sess]", rf.Cookies)
	}
	if len(rf.Query) != 1 || rf.Query[0] != "Page" {
		t.Errorf("Query = %v, want [Page]", rf.Query)
	}
	if len(rf.Params) != 1 || rf.Params[0] != "Id" {
		t.Errorf("Params = %v, want [Id]", rf.Params)
	}
	if !rf.NeedBody {
		t.Error("NeedBody should be true (body used in fetch config)")
	}
}

// TestRequestFieldsDedup verifies that duplicate request refs are deduped.
func TestRequestFieldsDedup(t *testing.T) {
	expr := `{"a": $request().headers.X, "b": $request().headers.X, "c": $request().params.id}`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformDedup")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	rf := plan.RequestFields()
	if len(rf.Headers) != 1 || rf.Headers[0] != "X" {
		t.Errorf("Headers should be deduped to [X], got %v", rf.Headers)
	}
	if len(rf.Params) != 1 || rf.Params[0] != "id" {
		t.Errorf("Params = %v, want [id]", rf.Params)
	}
}

// TestRequestFieldsFromRootBindings verifies that request refs in root bindings are collected.
func TestRequestFieldsFromRootBindings(t *testing.T) {
	expr := `( $id := $request().params.id; [ {"id": $id} ] )`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformRootBinding")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}

	rf := plan.RequestFields()
	if len(rf.Params) != 1 || rf.Params[0] != "id" {
		t.Errorf("Params from root binding = %v, want [id]", rf.Params)
	}
}

// TestAnalyzeFetchCallsRootExprArray verifies that a top-level array (or block ending in array)
// produces a plan with RootExpr and no Fields.
func TestAnalyzeFetchCallsRootExprArray(t *testing.T) {
	expr := `[1, 2, 3]`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformArray")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	if plan.RootExpr == nil {
		t.Error("RootExpr should be set for array root")
	}
	if len(plan.Fields) != 0 {
		t.Errorf("Fields should be empty for root-expr plan, got %d", len(plan.Fields))
	}
}

// TestAnalyzeFetchCallsRootExprBlockArray verifies that a block whose last expression is an array
// produces RootExpr and RootBindings.
func TestAnalyzeFetchCallsRootExprBlockArray(t *testing.T) {
	expr := `( $x := 1; [ $x, 2, 3 ] )`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformBlockArray")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	if plan.RootExpr == nil {
		t.Error("RootExpr should be set")
	}
	if len(plan.RootBindings) != 1 {
		t.Errorf("RootBindings length = %d, want 1", len(plan.RootBindings))
	}
	if len(plan.Fields) != 0 {
		t.Errorf("Fields should be empty, got %d", len(plan.Fields))
	}
}

// TestAnalyzeFetchCallsRootExprPrimitive verifies that a top-level primitive produces RootExpr.
func TestAnalyzeFetchCallsRootExprPrimitive(t *testing.T) {
	expr := `42`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformNum")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	if plan.RootExpr == nil {
		t.Error("RootExpr should be set for primitive root")
	}
}

// TestAnalyzeFetchCallsRootExprFetchField verifies that $fetch("provider", "endpoint").field
// produces a RootExpr of kind fetchAtPath with the correct dep and path.
func TestAnalyzeFetchCallsRootExprFetchField(t *testing.T) {
	expr := `$fetch("orders_service", "data").id`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformGetId")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	if plan.RootExpr == nil {
		t.Fatal("RootExpr should be set")
	}
	if plan.RootExpr.Kind != "fetchAtPath" {
		t.Errorf("RootExpr.Kind = %q, want fetchAtPath", plan.RootExpr.Kind)
	}
	if plan.RootExpr.FetchProvider != "orders_service" || plan.RootExpr.FetchEndpoint != "data" {
		t.Errorf("FetchProvider = %q FetchEndpoint = %q, want orders_service data", plan.RootExpr.FetchProvider, plan.RootExpr.FetchEndpoint)
	}
	if len(plan.RootExpr.FetchPath) != 1 || plan.RootExpr.FetchPath[0] != "id" {
		t.Errorf("FetchPath = %v, want [id]", plan.RootExpr.FetchPath)
	}
	if len(plan.Deps) != 1 || plan.Deps[0].Provider != "orders_service" || plan.Deps[0].Endpoint != "data" {
		t.Errorf("Deps = %v, want single orders_service.data", plan.Deps)
	}
}

// TestAnalyzeFetchCallsBlockLastObject verifies that a block whose last expression is an object
// still produces an object plan (no RootExpr), preserving existing behaviour.
func TestAnalyzeFetchCallsBlockLastObject(t *testing.T) {
	expr := `( $x := 1; { "a": 1 } )`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformBlockObject")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	if plan.RootExpr != nil {
		t.Error("RootExpr should be nil when last expression is object (use object path)")
	}
	if len(plan.Fields) != 1 || plan.Fields[0].OutputKey != "a" {
		t.Errorf("expected one field \"a\", got %v", plan.Fields)
	}
}

// TestAnalyzeFetchCallsEmptyBlockRejected verifies that an empty block () is rejected.
func TestAnalyzeFetchCallsEmptyBlockRejected(t *testing.T) {
	expr := `()`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	_, err = transpiler.AnalyzeFetchCalls(ast, "TransformEmpty")
	if err == nil {
		t.Error("expected error for empty block")
	}
	if !strings.Contains(err.Error(), "empty block") {
		t.Errorf("error should mention empty block, got: %v", err)
	}
}

// TestAnalyzeFetchCallsRootExprArrayMap verifies that a top-level array-map path
// (e.g. [a..b].(expr)) produces a plan with RootExpr of kind arrayMap.
func TestAnalyzeFetchCallsRootExprArrayMap(t *testing.T) {
	expr := `[1..3].($ * 2)`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformArrayMap")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	if plan.RootExpr == nil {
		t.Fatal("RootExpr should be set for array-map root")
	}
	if plan.RootExpr.Kind != "arrayMap" {
		t.Errorf("RootExpr.Kind = %q, want arrayMap", plan.RootExpr.Kind)
	}
	if plan.RootExpr.Left == nil || plan.RootExpr.Right == nil {
		t.Error("arrayMap should have Left (array/range) and Right (per-element expr)")
	}
	if plan.RootExpr.Left.Kind != "funcCall" || plan.RootExpr.Left.FuncName != "_range" {
		t.Errorf("arrayMap Left should be _range, got Kind=%q FuncName=%q", plan.RootExpr.Left.Kind, plan.RootExpr.Left.FuncName)
	}
}

// TestGenerateProviderRootExprArray verifies that generating code for a root-expr array plan
// produces compilable Go that uses jsonv2.Marshal and returns a single value.
func TestGenerateProviderRootExprArray(t *testing.T) {
	expr := `[1, 2, 3]`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformArray")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	src, err := transpiler.GenerateProvider(plan, "testpkg", "array.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}
	code := string(src)
	if !strings.Contains(code, "jsonv2.Marshal") {
		t.Error("generated code should use jsonv2.Marshal for root-expr")
	}
	if !strings.Contains(code, "bodyBytes") || !strings.Contains(code, "Body:") {
		t.Error("generated code should set Body from marshalled value")
	}
	if strings.Contains(code, "BeginObject") {
		t.Error("root-expr plan should not emit BeginObject/EndObject")
	}
}

// TestGenerateProviderRootExprArrayMap verifies that generating code for a root-expr
// array-map path produces code that uses a range loop and append (arrayMap codegen).
func TestGenerateProviderRootExprArrayMap(t *testing.T) {
	expr := `[1..3].($ * 2)`
	ast, err := jparse.Parse(expr)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	plan, err := transpiler.AnalyzeFetchCalls(ast, "TransformArrayMap")
	if err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	src, err := transpiler.GenerateProvider(plan, "testpkg", "array_map.jsonata", expr)
	if err != nil {
		t.Fatalf("generate error: %v", err)
	}
	code := string(src)
	if !strings.Contains(code, "arrayMapResult_") {
		t.Error("generated code for array-map should contain arrayMapResult_ variable")
	}
	if !strings.Contains(code, "for _,") && !strings.Contains(code, "for _, ") {
		t.Error("generated code for array-map should contain for-range loop")
	}
	if !strings.Contains(code, "runtime.Range") {
		t.Error("generated code for [1..3] should use runtime.Range")
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
