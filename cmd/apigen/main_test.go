package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportedName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"orders", "Orders"},
		{"order_id", "OrderID"},
		{"id", "ID"},
		{"url", "URL"},
		{"api", "API"},
		{"http", "HTTP"},
		{"user_service", "UserService"},
		{"get_user", "GetUser"},
		{"dashboard", "Dashboard"},
		{"account_summary", "AccountSummary"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := exportedName(tt.input); got != tt.want {
				t.Errorf("exportedName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCapitalizeFirst(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"get", "Get"},
		{"GET", "Get"},
		{"post", "Post"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := capitalizeFirst(tt.input); got != tt.want {
				t.Errorf("capitalizeFirst(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}


func TestParseSpec(t *testing.T) {
	spec := `openapi: "3.0.0"
paths:
  /api/v1/orders:
    get:
      x-service-name: orders
  /api/v1/products:
    get:
      x-service-name: products
`
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create jsonata files for the services
	jsonataDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(jsonataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(jsonataDir, "orders.jsonata"), []byte(`{"test": "data"}`), 0o644)
	os.WriteFile(filepath.Join(jsonataDir, "products.jsonata"), []byte(`{"test": "data"}`), 0o644)

	routes, err := parseSpec(specPath, jsonataDir)
	if err != nil {
		t.Fatalf("parseSpec error: %v", err)
	}

	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}

	// Routes from map iteration may be in any order; find each.
	found := map[string]bool{}
	for _, r := range routes {
		// Extract service name from FuncName (e.g., "TransformOrders" -> "orders")
		serviceName := strings.ToLower(strings.TrimPrefix(r.FuncName, "Transform"))
		found[serviceName] = true
		if r.Method != "Get" {
			t.Errorf("route %s: Method = %q, want Get", serviceName, r.Method)
		}
	}
	if !found["orders"] || !found["products"] {
		t.Errorf("missing routes: %v", found)
	}
}

func TestParseConfig(t *testing.T) {
	dir := t.TempDir()

	// Create a routes file.
	routesYAML := `routes:
  - path: /dashboard
    method: GET
    jsonata: dashboard.jsonata
  - path: /api/v1/orders
    method: GET
    jsonata: orders.jsonata
`
	routesPath := filepath.Join(dir, "routes.yaml")
	if err := os.WriteFile(routesPath, []byte(routesYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create the jsonata files.
	svcDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(svcDir, "dashboard.jsonata"),
		[]byte(`{"user": $fetch("user_svc", "profile").name}`), 0o644)
	os.WriteFile(filepath.Join(svcDir, "orders.jsonata"),
		[]byte(`orders[price > 100].{id: order_id}`), 0o644)

	routes, err := parseConfig(routesPath, svcDir)
	if err != nil {
		t.Fatalf("parseConfig error: %v", err)
	}

	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}

	dashboard := routes[0]
	if dashboard.Path != "/dashboard" || dashboard.Method != "Get" {
		t.Errorf("dashboard: path=%q method=%q", dashboard.Path, dashboard.Method)
	}
	if dashboard.FuncName != "TransformDashboard" {
		t.Errorf("dashboard FuncName = %q, want TransformDashboard", dashboard.FuncName)
	}

	orders := routes[1]
	if orders.FuncName != "TransformOrders" {
		t.Errorf("orders FuncName = %q, want TransformOrders", orders.FuncName)
	}
}

func TestGenerateSpecRoutes(t *testing.T) {
	routes := []configRoute{
		{Method: "Get", Path: "/api/v1/orders", FuncName: "TransformOrders"},
	}

	src, err := generateConfigRoutes(routes, "main", "example.com/pkg/generated")
	if err != nil {
		t.Fatalf("generateConfigRoutes error: %v", err)
	}

	code := string(src)
	mustContain := []string{
		"package main",
		"goexperiment.jsonv2",
		"RegisterRoutes",
		`app.Get("/api/v1/orders"`,
		"generated.ExecuteOrders",
	}
	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated config routes missing %q\n\n%s", s, code)
		}
	}
}

func TestGenerateConfigRoutes(t *testing.T) {
	routes := []configRoute{
		{Method: "Get", Path: "/dashboard", FuncName: "TransformDashboard"},
		{Method: "Get", Path: "/api/v1/orders", FuncName: "TransformOrders"},
	}

	src, err := generateConfigRoutes(routes, "main", "example.com/pkg/generated")
	if err != nil {
		t.Fatalf("generateConfigRoutes error: %v", err)
	}

	code := string(src)
	mustContain := []string{
		"package main",
		"goexperiment.jsonv2",
		"RegisterRoutes",
		`app.Get("/dashboard"`,
		"generated.ExecuteDashboard",
		"c.Context()",
		"runtime.RequestContext",
		`app.Get("/api/v1/orders"`,
		"generated.ExecuteOrders",
	}
	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated config routes missing %q\n\n%s", s, code)
		}
	}
}

func TestParseConfigMissingFile(t *testing.T) {
	_, err := parseConfig("/nonexistent/routes.yaml", "")
	if err == nil {
		t.Fatal("expected error for missing routes file")
	}
}

func TestParseConfigMissingJsonata(t *testing.T) {
	dir := t.TempDir()
	routesYAML := `routes:
  - path: /test
    method: GET
    jsonata: missing.jsonata
`
	routesPath := filepath.Join(dir, "routes.yaml")
	os.WriteFile(routesPath, []byte(routesYAML), 0o644)

	_, err := parseConfig(routesPath, dir)
	if err == nil {
		t.Fatal("expected error for missing jsonata file")
	}
}

func TestParseSpecWithSchema(t *testing.T) {
	spec := `openapi: "3.0.0"
paths:
  /dashboard:
    get:
      x-service-name: dashboard
      parameters:
        - name: Authorization
          in: header
          required: true
          schema:
            type: string
  /api/v1/users:
    post:
      x-service-name: users
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [email, name]
              properties:
                email:
                  type: string
                  format: email
                name:
                  type: string
                  minLength: 1
                  maxLength: 100
      parameters:
        - name: X-Request-ID
          in: header
          required: true
          schema:
            type: string
        - name: page
          in: query
          schema:
            type: integer
            minimum: 1
`
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	jsonataDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(jsonataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(jsonataDir, "dashboard.jsonata"), []byte(`{"test": "data"}`), 0o644)
	os.WriteFile(filepath.Join(jsonataDir, "users.jsonata"), []byte(`{"test": "data"}`), 0o644)

	routes, err := parseSpec(specPath, jsonataDir)
	if err != nil {
		t.Fatalf("parseSpec error: %v", err)
	}

	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}

	// Find dashboard route
	var dashboardRoute *configRoute
	for i := range routes {
		if routes[i].Path == "/dashboard" {
			dashboardRoute = &routes[i]
			break
		}
	}
	if dashboardRoute == nil {
		t.Fatal("dashboard route not found")
	}

	if dashboardRoute.RequestSchema == nil {
		t.Fatal("dashboard route should have request schema")
	}
	if len(dashboardRoute.RequestSchema.Header) != 1 {
		t.Fatalf("dashboard should have 1 header parameter, got %d", len(dashboardRoute.RequestSchema.Header))
	}
	if dashboardRoute.RequestSchema.Header[0].Name != "Authorization" {
		t.Errorf("header parameter name = %q, want Authorization", dashboardRoute.RequestSchema.Header[0].Name)
	}
	if !dashboardRoute.RequestSchema.Header[0].Required {
		t.Error("Authorization header should be required")
	}

	// Find users route
	var usersRoute *configRoute
	for i := range routes {
		if routes[i].Path == "/api/v1/users" {
			usersRoute = &routes[i]
			break
		}
	}
	if usersRoute == nil {
		t.Fatal("users route not found")
	}

	if usersRoute.RequestSchema == nil {
		t.Fatal("users route should have request schema")
	}
	if usersRoute.RequestSchema.Body == nil {
		t.Fatal("users route should have request body schema")
	}
	if !usersRoute.RequestSchema.Body.Required {
		t.Error("users request body should be required")
	}
	if len(usersRoute.RequestSchema.Body.Schema.Required) != 2 {
		t.Fatalf("body should have 2 required fields, got %d", len(usersRoute.RequestSchema.Body.Schema.Required))
	}
	if len(usersRoute.RequestSchema.Header) != 1 {
		t.Fatalf("users should have 1 header parameter, got %d", len(usersRoute.RequestSchema.Header))
	}
	if len(usersRoute.RequestSchema.Query) != 1 {
		t.Fatalf("users should have 1 query parameter, got %d", len(usersRoute.RequestSchema.Query))
	}
}

func TestGenerateValidator(t *testing.T) {
	schema := &RequestSchema{
		Header: []Parameter{
			{Name: "Authorization", Required: true, Schema: SchemaInfo{Type: "string"}},
		},
		Query: []Parameter{
			{Name: "page", Required: false, Schema: SchemaInfo{Type: "integer", Constraints: Constraints{Minimum: floatPtr(1)}}},
		},
		Body: &BodySchema{
			Required: true,
			Schema: SchemaInfo{
				Type:     "object",
				Required: []string{"email", "name"},
				Properties: map[string]SchemaInfo{
					"email": {Type: "string", Format: "email"},
					"name":  {Type: "string", Constraints: Constraints{MinLength: intPtr(1), MaxLength: intPtr(100)}},
				},
			},
		},
	}

	validatorCode := generateValidator("Post", "/api/v1/users", schema)
	if validatorCode == "" {
		t.Fatal("validator code should not be empty")
	}

	mustContain := []string{
		"func ValidatePostAPIV1UsersRequest",
		"runtime.RequestContext",
		"required header 'Authorization'",
		"query parameter 'page'",
		"request body is required",
		"required field 'email'",
		"required field 'name'",
	}
	for _, s := range mustContain {
		if !strings.Contains(validatorCode, s) {
			t.Errorf("validator code missing %q\n\n%s", s, validatorCode)
		}
	}
}

func TestSanitizeRouteName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/dashboard", "Dashboard"},
		{"/api/v1/users", "APIV1Users"},
		{"/api/v1/orders", "APIV1Orders"},
		{"/", "Root"},
		{"", "Root"},
		{"/user-profile", "UserProfile"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := sanitizeRouteName(tt.input); got != tt.want {
				t.Errorf("sanitizeRouteName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGenerateConfigRoutesWithValidation(t *testing.T) {
	routes := []configRoute{
		{
			Method:   "Get",
			Path:     "/dashboard",
			FuncName: "TransformDashboard",
			RequestSchema: &RequestSchema{
				Header: []Parameter{
					{Name: "Authorization", Required: true, Schema: SchemaInfo{Type: "string"}},
				},
			},
		},
	}

	src, err := generateConfigRoutes(routes, "main", "example.com/pkg/generated")
	if err != nil {
		t.Fatalf("generateConfigRoutes error: %v", err)
	}

	code := string(src)
	mustContain := []string{
		"func ValidateGetDashboardRequest",
		"required header 'Authorization'",
		"if err := ValidateGetDashboardRequest(reqCtx); err != nil",
		"fiber.StatusBadRequest",
	}
	for _, s := range mustContain {
		if !strings.Contains(code, s) {
			t.Errorf("generated code missing %q\n\n%s", s, code)
		}
	}
}

func floatPtr(f float64) *float64 {
	return &f
}

func intPtr(i int) *int {
	return &i
}
