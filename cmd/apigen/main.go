// Command apigen generates Fiber v3 route registration code from either:
//   - an OpenAPI spec (--spec) with x-service-name extensions, optionally combined
//     with proxy routes (--proxies), or
//   - a routes file (--routes) listing path-to-jsonata mappings (legacy mode).
//
// All routes are generated in provider mode — every handler calls Execute*
// which fetches upstream data via the aggregator and returns marshalled JSON.
//
// Usage:
//
//	go run ./cmd/apigen \
//	  --spec=data/openapi.yaml \
//	  --jsonata-dir=data/services \
//	  --proxies=data/proxies.yaml \
//	  --output=cmd/server/routes_gen.go \
//	  --package=main \
//	  --generated-pkg=github.com/example/project/internal/generated
//
//	go run ./cmd/apigen \
//	  --routes=data/routes.yaml \
//	  --jsonata-dir=data/services \
//	  --output=cmd/server/routes_gen.go \
//	  --package=main \
//	  --generated-pkg=github.com/example/project/internal/generated
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

func main() {
	spec := flag.String("spec", "", "path to OpenAPI YAML file")
	routesFile := flag.String("routes", "", "path to routes.yaml (legacy mode)")
	jsonataDir := flag.String("jsonata-dir", "", "directory containing .jsonata files")
	proxiesFile := flag.String("proxies", "", "path to proxies.yaml (optional, used with --spec)")
	output := flag.String("output", "", "path for the generated .go file")
	pkg := flag.String("package", "main", "Go package name for the generated file")
	genPkg := flag.String("generated-pkg", "", "import path of the generated transform package")
	flag.Parse()

	if *output == "" || *genPkg == "" {
		fatal("--output and --generated-pkg are required")
	}

	hasSpec := *spec != ""
	hasRoutes := *routesFile != ""
	if !hasSpec && !hasRoutes {
		fatal("either --spec or --routes is required")
	}
	if hasSpec && hasRoutes {
		fatal("--spec and --routes are mutually exclusive")
	}

	var src []byte
	var routeCount int
	var sourceFile string

	if hasSpec {
		if *jsonataDir == "" {
			fatal("--jsonata-dir is required when using --spec")
		}
		sourceFile = *spec
		routes, err := parseSpec(*spec, *jsonataDir)
		if err != nil {
			fatal("parsing spec: %v", err)
		}

		// Combine with proxy routes if provided
		if *proxiesFile != "" {
			proxyRoutes, err := parseProxies(*proxiesFile)
			if err != nil {
				fatal("parsing proxies: %v", err)
			}
			routes = append(routes, proxyRoutes...)
		}

		routeCount = len(routes)
		src, err = generateConfigRoutes(routes, *pkg, *genPkg)
		if err != nil {
			fatal("generating routes: %v", err)
		}
	} else {
		sourceFile = *routesFile
		routes, err := parseConfig(*routesFile, *jsonataDir)
		if err != nil {
			fatal("parsing routes: %v", err)
		}
		routeCount = len(routes)
		src, err = generateConfigRoutes(routes, *pkg, *genPkg)
		if err != nil {
			fatal("generating routes: %v", err)
		}
	}

	if err := os.WriteFile(*output, src, 0o644); err != nil {
		fatal("writing output: %v", err)
	}

	fmt.Fprintf(os.Stderr, "apigen: %s -> %s (%d routes)\n", sourceFile, *output, routeCount)
}

// --- OpenAPI spec mode ---

func parseSpec(path, jsonataDir string) ([]configRoute, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Parse full OpenAPI document to access components
	var fullDoc map[string]interface{}
	if err := yaml.Unmarshal(data, &fullDoc); err != nil {
		return nil, fmt.Errorf("YAML parse: %w", err)
	}

	// Extract components for $ref resolution
	components := extractComponents(fullDoc)

	// Parse paths
	paths, ok := fullDoc["paths"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid paths section")
	}

	var routes []configRoute
	for urlPath, methodsVal := range paths {
		methods, ok := methodsVal.(map[string]interface{})
		if !ok {
			continue
		}

		for method, opVal := range methods {
			op, ok := opVal.(map[string]interface{})
			if !ok {
				continue
			}

			// Extract x-service-name
			serviceName, _ := op["x-service-name"].(string)
			if serviceName == "" {
				continue
			}

			// Map service name to JSONata file: orders -> orders.jsonata
			jsonataFile := serviceName + ".jsonata"
			jsonataPath := filepath.Join(jsonataDir, jsonataFile)

			expr, err := os.ReadFile(jsonataPath)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", jsonataPath, err)
			}

			baseName := serviceName
			funcName := "Transform" + exportedName(baseName)
			rk := scanRequestKeys(string(expr))

			// Parse request schema from OpenAPI operation
			requestSchema := parseRequestSchema(op, components)

			// Parse x-slow-request-threshold extension (duration format: "500ms", "2s", etc.)
			var slowRequestThreshold time.Duration
			if thresholdStr, ok := op["x-slow-request-threshold"].(string); ok && thresholdStr != "" {
				if parsed, err := time.ParseDuration(thresholdStr); err == nil && parsed > 0 {
					slowRequestThreshold = parsed
				}
			}

			routes = append(routes, configRoute{
				Method:               capitalizeFirst(method),
				Path:                 urlPath,
				FuncName:             funcName,
				ReqKeys:              rk,
				RequestSchema:        requestSchema,
				SlowRequestThreshold: slowRequestThreshold,
			})
		}
	}
	return routes, nil
}

// --- Config mode ---

type configRoute struct {
	Method               string
	Path                 string
	FuncName             string         // e.g. "TransformOrders"
	ReqKeys              requestKeys    // which request fields are actually referenced
	ProxyURL             string         // if set, this route is a proxy with the target URL
	RequestSchema        *RequestSchema // parsed schema for request validation (nil if no schema)
	SlowRequestThreshold time.Duration  // per-route slow request threshold (0 means use default)
}

// requestKeys tracks which specific request fields a JSONata expression uses.
// By extracting only the needed keys, we avoid copying all headers/cookies on
// every request — a significant win for high-throughput BFF servers.
type requestKeys struct {
	Headers []string // specific header names
	Cookies []string // specific cookie names
	Query   bool     // needs query params
	Params  bool     // needs route params
	Path    bool     // needs request path
	Method  bool     // needs HTTP method
	Body    bool     // needs request body
}

// reRequestField matches patterns like $request().headers.Authorization,
// $request().cookies.session, $request().query.page, $request().params.id,
// $request().path, $request().method, $request().body.
var reRequestField = regexp.MustCompile(`\$request\(\)\.(headers|cookies|query|params|path|method|body)(?:\.(\w+))?`)

func scanRequestKeys(expr string) requestKeys {
	var rk requestKeys
	seenHeaders := map[string]bool{}
	seenCookies := map[string]bool{}

	for _, match := range reRequestField.FindAllStringSubmatch(expr, -1) {
		kind := match[1]
		name := match[2]

		switch kind {
		case "headers":
			if name != "" && !seenHeaders[name] {
				seenHeaders[name] = true
				rk.Headers = append(rk.Headers, name)
			}
		case "cookies":
			if name != "" && !seenCookies[name] {
				seenCookies[name] = true
				rk.Cookies = append(rk.Cookies, name)
			}
		case "query":
			rk.Query = true
		case "params":
			rk.Params = true
		case "path":
			rk.Path = true
		case "method":
			rk.Method = true
		case "body":
			rk.Body = true
		}
	}
	return rk
}

func parseConfig(path, jsonataDir string) ([]configRoute, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg struct {
		Routes []struct {
			Path    string `yaml:"path"`
			Method  string `yaml:"method"`
			Jsonata string `yaml:"jsonata"`
			Proxy   string `yaml:"proxy"`
		} `yaml:"routes"`
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("YAML parse: %w", err)
	}

	var routes []configRoute
	for _, r := range cfg.Routes {
		if r.Proxy != "" {
			// Legacy proxy route: still supports proxy field for backward compatibility
			// but this should be migrated to proxies.yaml with url field
			return nil, fmt.Errorf("route %s %s: proxy field is deprecated, use proxies.yaml with url field instead", r.Method, r.Path)
		}

		// Regular JSONata route
		if r.Jsonata == "" {
			return nil, fmt.Errorf("route %s %s: missing jsonata or proxy field", r.Method, r.Path)
		}

		baseName := strings.TrimSuffix(r.Jsonata, filepath.Ext(r.Jsonata))
		funcName := "Transform" + exportedName(baseName)

		jsonataPath := r.Jsonata
		if jsonataDir != "" {
			jsonataPath = filepath.Join(jsonataDir, r.Jsonata)
		}

		expr, err := os.ReadFile(jsonataPath)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", jsonataPath, err)
		}

		rk := scanRequestKeys(string(expr))
		routes = append(routes, configRoute{
			Method:   capitalizeFirst(r.Method),
			Path:     r.Path,
			FuncName: funcName,
			ReqKeys:  rk,
		})
	}
	return routes, nil
}

func parseProxies(path string) ([]configRoute, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg struct {
		Routes []struct {
			Path   string `yaml:"path"`
			Method string `yaml:"method"`
			URL    string `yaml:"url"`
		} `yaml:"routes"`
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("YAML parse: %w", err)
	}

	var routes []configRoute
	for _, r := range cfg.Routes {
		if r.URL == "" {
			return nil, fmt.Errorf("route %s %s: missing url field", r.Method, r.Path)
		}
		routes = append(routes, configRoute{
			Method:   capitalizeFirst(r.Method),
			Path:     r.Path,
			ProxyURL: r.URL,
		})
	}
	return routes, nil
}

func generateConfigRoutes(routes []configRoute, pkg, genPkg string) ([]byte, error) {
	var buf bytes.Buffer
	w := func(f string, args ...any) { fmt.Fprintf(&buf, f, args...) }

	hasProxy := false
	for _, r := range routes {
		if r.ProxyURL != "" {
			hasProxy = true
			break
		}
	}

	w("//go:build goexperiment.jsonv2\n\n")
	w("// Code generated by apigen from openapi.yaml and proxies.yaml. DO NOT EDIT.\n\n")
	w("package %s\n\n", pkg)
	w("import (\n")
	hasValidation := false
	for _, r := range routes {
		if r.RequestSchema != nil && r.RequestSchema.HasSchema() {
			hasValidation = true
			break
		}
	}
	if hasProxy {
		w("\t\"bytes\"\n")
		w("\t\"fmt\"\n")
		w("\t\"io\"\n")
		w("\t\"net/http\"\n")
		w("\t\"net/url\"\n")
		w("\t\"strings\"\n")
		w("\t\"time\"\n")
	}
	if hasValidation {
		if !hasProxy {
			w("\t\"bytes\"\n")
		}
		w("\t\"encoding/json/jsontext\"\n")
		w("\t\"fmt\"\n")
		w("\t\"regexp\"\n")
		w("\t\"strconv\"\n")
		if !hasProxy {
			w("\t\"strings\"\n")
		}
	}
	if !hasProxy && !hasValidation {
		w("\t\"time\"\n")
	}
	w("\t\"github.com/gofiber/fiber/v3\"\n")
	w("\t\"github.com/gcossani/ssfbff/internal/aggregator\"\n")
	w("\t\"github.com/gcossani/ssfbff/runtime\"\n")
	w("\t\"github.com/rs/zerolog\"\n")
	w("\tgenerated %q\n", genPkg)
	w(")\n\n")
	w("var routeLogger = zerolog.Nop()\n\n")
	w("func SetRouteLogger(logger zerolog.Logger) {\n")
	w("\trouteLogger = logger\n")
	w("}\n\n")

	if hasProxy {
		w("// proxyClient is used for pass-through proxy requests.\n")
		w("// It should be set by main() to use the shared HTTP client with OTel instrumentation.\n")
		w("var proxyClient *http.Client\n\n")
	}

	// Generate validation functions before route handlers
	if hasValidation {
		for _, r := range routes {
			if r.ProxyURL == "" && r.RequestSchema != nil && r.RequestSchema.HasSchema() {
				validatorCode := generateValidator(r.Method, r.Path, r.RequestSchema)
				if validatorCode != "" {
					w("%s", validatorCode)
				}
			}
		}
	}

	w("func RegisterRoutes(app *fiber.App, agg *aggregator.Aggregator, client *http.Client) {\n")
	if hasProxy {
		w("\tproxyClient = client\n\n")
	}

	for _, r := range routes {
		if r.ProxyURL != "" {
			// Proxy route: forward request to downstream server without modification
			w("\tapp.%s(%q, func(c fiber.Ctx) error {\n", r.Method, r.Path)
			w("\t\tstartTime := time.Now()\n")
			w("\t\tendpoint := %q\n", r.Path)
			w("\t\tmethod := c.Method()\n")
			w("\t\tupstreamURL := %q\n", r.ProxyURL)
			w("\t\t// Build target URL: append request path to upstream base URL\n")
			w("\t\t// If route path ends with /*, strip the prefix before the wildcard\n")
			w("\t\ttargetPath := c.Path()\n")
			w("\t\troutePath := %q\n", r.Path)
			w("\t\tif strings.HasSuffix(routePath, \"/*\") {\n")
			w("\t\t\tprefix := routePath[:len(routePath)-2] // Remove \"/*\"\n")
			w("\t\t\tif strings.HasPrefix(targetPath, prefix) {\n")
			w("\t\t\ttargetPath = targetPath[len(prefix):]\n")
			w("\t\t\tif !strings.HasPrefix(targetPath, \"/\") {\n")
			w("\t\t\t\ttargetPath = \"/\" + targetPath\n")
			w("\t\t\t}\n")
			w("\t\t}\n")
			w("\t\t}\n")
			w("\t\tif !strings.HasSuffix(upstreamURL, \"/\") && !strings.HasPrefix(targetPath, \"/\") {\n")
			w("\t\t\tupstreamURL += \"/\"\n")
			w("\t\t}\n")
			w("\t\tif strings.HasSuffix(upstreamURL, \"/\") && strings.HasPrefix(targetPath, \"/\") {\n")
			w("\t\t\ttargetPath = targetPath[1:]\n")
			w("\t\t}\n")
			w("\t\ttargetURL := upstreamURL + targetPath\n")
			w("\t\tqueries := c.Queries()\n")
			w("\t\tif len(queries) > 0 {\n")
			w("\t\t\tvals := make(url.Values, len(queries))\n")
			w("\t\t\tfor k, v := range queries {\n")
			w("\t\t\t\tvals[k] = []string{v}\n")
			w("\t\t\t}\n")
			w("\t\t\ttargetURL += \"?\" + vals.Encode()\n")
			w("\t\t}\n\n")
			w("\t\t// Create HTTP request with same method, headers, and body\n")
			w("\t\tvar bodyReader io.Reader\n")
			w("\t\tif len(c.Body()) > 0 {\n")
			w("\t\t\tbodyReader = bytes.NewReader(c.Body())\n")
			w("\t\t}\n")
			w("\t\treq, err := http.NewRequestWithContext(c.Context(), c.Method(), targetURL, bodyReader)\n")
			w("\t\tif err != nil {\n")
			w("\t\t\tduration := time.Since(startTime)\n")
			w("\t\t\trecordHTTPRequestDuration(endpoint, method, fiber.StatusInternalServerError, duration)\n")
			w("\t\t\trecordHTTPResponseSize(endpoint, method, fiber.StatusInternalServerError, 0)\n")
			w("\t\t\trouteLogger.Error().\n")
			w("\t\t\t\tStr(\"endpoint\", %q).\n", r.Path)
			w("\t\t\t\tStr(\"method\", c.Method()).\n")
			w("\t\t\t\tErr(err).\n")
			w("\t\t\t\tMsg(\"failed to create proxy request\")\n")
			w("\t\t\trecordHTTPError(%q, c.Method(), fiber.StatusInternalServerError)\n", r.Path)
			w("\t\t\tsanitizedMsg := runtime.SanitizeError(err)\n")
			w("\t\t\treturn fiber.NewError(fiber.StatusInternalServerError, sanitizedMsg)\n")
			w("\t\t}\n\n")
			w("\t\t// Copy all headers from incoming request\n")
			w("\t\tc.Request().Header.VisitAll(func(key, value []byte) {\n")
			w("\t\t\treq.Header.Set(string(key), string(value))\n")
			w("\t\t})\n\n")
			w("\t\t// Make request using shared HTTP client (with OTel instrumentation)\n")
			w("\t\tresp, err := proxyClient.Do(req)\n")
			w("\t\tif err != nil {\n")
			w("\t\t\tduration := time.Since(startTime)\n")
			w("\t\t\trecordHTTPRequestDuration(endpoint, method, fiber.StatusBadGateway, duration)\n")
			w("\t\t\trecordHTTPResponseSize(endpoint, method, fiber.StatusBadGateway, 0)\n")
			w("\t\t\trouteLogger.Error().\n")
			w("\t\t\t\tStr(\"endpoint\", %q).\n", r.Path)
			w("\t\t\t\tStr(\"method\", c.Method()).\n")
			w("\t\t\t\tStr(\"upstream_url\", targetURL).\n")
			w("\t\t\t\tErr(err).\n")
			w("\t\t\t\tMsg(\"proxy upstream request failed\")\n")
			w("\t\t\trecordHTTPError(%q, c.Method(), fiber.StatusBadGateway)\n", r.Path)
			w("\t\t\tsanitizedMsg := runtime.SanitizeError(err)\n")
			w("\t\t\treturn fiber.NewError(fiber.StatusBadGateway, sanitizedMsg)\n")
			w("\t\t}\n")
			w("\t\tdefer resp.Body.Close()\n\n")
			w("\t\t// Copy all response headers\n")
			w("\t\tfor k, v := range resp.Header {\n")
			w("\t\t\tc.Set(k, strings.Join(v, \", \"))\n")
			w("\t\t}\n\n")
			w("\t\t// Set status code and copy response body\n")
			w("\t\tc.Status(resp.StatusCode)\n")
			w("\t\tif resp.StatusCode >= 400 {\n")
			w("\t\t\trouteLogger.Error().\n")
			w("\t\t\t\tStr(\"endpoint\", %q).\n", r.Path)
			w("\t\t\t\tStr(\"method\", c.Method()).\n")
			w("\t\t\t\tInt(\"status_code\", resp.StatusCode).\n")
			w("\t\t\t\tStr(\"upstream_url\", targetURL).\n")
			w("\t\t\t\tMsg(\"proxy upstream returned error status\")\n")
			w("\t\t\trecordHTTPError(%q, c.Method(), resp.StatusCode)\n", r.Path)
			w("\t\t}\n")
			w("\t\tmaxSize := getCachedMaxResponseBodySize()\n")
			w("\t\tlimitedReader := io.LimitReader(resp.Body, int64(maxSize))\n")
			w("\t\tbody, err := io.ReadAll(limitedReader)\n")
			w("\t\tif err != nil {\n")
			w("\t\t\tduration := time.Since(startTime)\n")
			w("\t\t\trecordHTTPRequestDuration(endpoint, method, fiber.StatusBadGateway, duration)\n")
			w("\t\t\trecordHTTPResponseSize(endpoint, method, fiber.StatusBadGateway, 0)\n")
			w("\t\t\trouteLogger.Error().\n")
			w("\t\t\t\tStr(\"endpoint\", %q).\n", r.Path)
			w("\t\t\t\tStr(\"method\", c.Method()).\n")
			w("\t\t\t\tErr(err).\n")
			w("\t\t\t\tMsg(\"failed to read proxy response\")\n")
			w("\t\t\trecordHTTPError(%q, c.Method(), fiber.StatusBadGateway)\n", r.Path)
			w("\t\t\tsanitizedMsg := runtime.SanitizeError(err)\n")
			w("\t\t\treturn fiber.NewError(fiber.StatusBadGateway, sanitizedMsg)\n")
			w("\t\t}\n")
			w("\t\t// Check for truncation by trying to read one more byte\n")
			w("\t\tvar extraByte [1]byte\n")
			w("\t\tn, _ := resp.Body.Read(extraByte[:])\n")
			w("\t\tif n > 0 {\n")
			w("\t\t\t// Response was truncated - log error and return error\n")
			w("\t\t\tduration := time.Since(startTime)\n")
			w("\t\t\trecordHTTPRequestDuration(endpoint, method, fiber.StatusBadGateway, duration)\n")
			w("\t\t\trecordHTTPResponseSize(endpoint, method, fiber.StatusBadGateway, 0)\n")
			w("\t\t\trouteLogger.Error().\n")
			w("\t\t\t\tStr(\"endpoint\", %q).\n", r.Path)
			w("\t\t\t\tStr(\"method\", c.Method()).\n")
			w("\t\t\t\tInt(\"max_size\", maxSize).\n")
			w("\t\t\t\tMsg(\"proxy response body exceeds maximum size\")\n")
			w("\t\t\trecordHTTPError(%q, c.Method(), fiber.StatusBadGateway)\n", r.Path)
			w("\t\t\treturn fiber.NewError(fiber.StatusBadGateway, fmt.Sprintf(\"response body exceeds maximum size of %%d bytes\", maxSize))\n")
			w("\t\t}\n")
			w("\t\tduration := time.Since(startTime)\n")
			w("\t\tstatusCode := resp.StatusCode\n")
			w("\t\trecordHTTPRequestDuration(endpoint, method, statusCode, duration)\n")
			w("\t\trecordHTTPResponseSize(endpoint, method, statusCode, len(body))\n")
			w("\t\t// Check for slow request\n")
			w("\t\tthreshold := getCachedSlowRequestThreshold()\n")
			if r.SlowRequestThreshold > 0 {
				w("\t\tif duration > %d*time.Nanosecond {\n", r.SlowRequestThreshold.Nanoseconds())
				w("\t\t\trecordSlowRequest(endpoint, method)\n")
				w("\t\t}\n")
			} else {
				w("\t\tif duration > threshold {\n")
				w("\t\t\trecordSlowRequest(endpoint, method)\n")
				w("\t\t}\n")
			}
			w("\t\treturn c.Send(body)\n")
			w("\t})\n\n")
		} else {
			// Regular JSONata route
			execName := strings.TrimPrefix(r.FuncName, "Transform")
			w("\tapp.%s(%q, func(c fiber.Ctx) error {\n", r.Method, r.Path)
			w("\t\tstartTime := time.Now()\n")
			w("\t\tendpoint := %q\n", r.Path)
			w("\t\tmethod := c.Method()\n")
			writeRequestContextBuilder(w, "\t\t", r.ReqKeys)

			// Add validation call if schema exists
			if r.RequestSchema != nil && r.RequestSchema.HasSchema() {
				validatorFuncName := "Validate" + exportedName(r.Method) + sanitizeRouteName(r.Path) + "Request"
				w("\t\tif err := %s(reqCtx); err != nil {\n", validatorFuncName)
				w("\t\t\trouteLogger.Error().\n")
				w("\t\t\t\tStr(\"endpoint\", %q).\n", r.Path)
				w("\t\t\t\tStr(\"method\", c.Method()).\n")
				w("\t\t\t\tErr(err).\n")
				w("\t\t\t\tMsg(\"request validation failed\")\n")
				w("\t\t\trecordHTTPError(%q, c.Method(), fiber.StatusBadRequest)\n", r.Path)
				w("\t\t\treturn c.Status(400).JSON(fiber.Map{\n")
				w("\t\t\t\t\"error\": err.Error(),\n")
				w("\t\t\t\t\"status\": 400,\n")
				w("\t\t\t\t\"code\": %q,\n", "VALIDATION_ERROR")
				w("\t\t\t})\n")
				w("\t\t}\n\n")
			}

			// Initialize request-scoped cache for $fetch() calls
			w("\t\tfetchCache := &aggregator.FetchCache{}\n")
			w("\t\tctx := aggregator.WithFetchCache(c.Context(), fetchCache)\n\n")

			w("\t\tresp, err := generated.Execute%s(ctx, agg, reqCtx)\n", execName)
			w("\t\tif err != nil {\n")
			w("\t\t\tduration := time.Since(startTime)\n")
			w("\t\t\t// Check if it's an HTTPError\n")
			w("\t\t\tif httpErr, ok := err.(*runtime.HTTPError); ok {\n")
			w("\t\t\t\tresp, respErr := httpErr.ToResponse()\n")
			w("\t\t\t\tif respErr != nil {\n")
			w("\t\t\t\t\trecordHTTPRequestDuration(endpoint, method, fiber.StatusInternalServerError, duration)\n")
			w("\t\t\t\t\trecordHTTPResponseSize(endpoint, method, fiber.StatusInternalServerError, 0)\n")
			w("\t\t\t\t\trouteLogger.Error().\n")
			w("\t\t\t\t\t\tStr(\"endpoint\", %q).\n", r.Path)
			w("\t\t\t\t\t\tStr(\"method\", c.Method()).\n")
			w("\t\t\t\t\t\tErr(respErr).\n")
			w("\t\t\t\t\t\tMsg(\"failed to convert HTTPError to response\")\n")
			w("\t\t\t\t\trecordHTTPError(%q, c.Method(), fiber.StatusInternalServerError)\n", r.Path)
			w("\t\t\t\t\treturn fiber.NewError(fiber.StatusInternalServerError, \"internal error\")\n")
			w("\t\t\t\t}\n")
			w("\t\t\t\tstatusCode := resp.StatusCode\n")
			w("\t\t\t\tresponseSize := len(resp.Body)\n")
			w("\t\t\t\trecordHTTPRequestDuration(endpoint, method, statusCode, duration)\n")
			w("\t\t\t\trecordHTTPResponseSize(endpoint, method, statusCode, responseSize)\n")
			w("\t\t\t\t// Check for slow request\n")
			w("\t\t\t\tthreshold := getCachedSlowRequestThreshold()\n")
			if r.SlowRequestThreshold > 0 {
				w("\t\t\t\tif duration > %d*time.Nanosecond {\n", r.SlowRequestThreshold.Nanoseconds())
				w("\t\t\t\t\trecordSlowRequest(endpoint, method)\n")
				w("\t\t\t\t}\n")
			} else {
				w("\t\t\t\tif duration > threshold {\n")
				w("\t\t\t\t\trecordSlowRequest(endpoint, method)\n")
				w("\t\t\t\t}\n")
			}
			w("\t\t\t\tfor k, v := range resp.Headers {\n")
			w("\t\t\t\t\tc.Set(k, v)\n")
			w("\t\t\t\t}\n")
			w("\t\t\t\treturn c.Status(resp.StatusCode).Send(resp.Body)\n")
			w("\t\t\t}\n")
			w("\t\t\t// Regular error handling\n")
			w("\t\t\trecordHTTPRequestDuration(endpoint, method, fiber.StatusBadGateway, duration)\n")
			w("\t\t\trecordHTTPResponseSize(endpoint, method, fiber.StatusBadGateway, 0)\n")
			w("\t\t\trouteLogger.Error().\n")
			w("\t\t\t\tStr(\"endpoint\", %q).\n", r.Path)
			w("\t\t\t\tStr(\"method\", c.Method()).\n")
			w("\t\t\t\tErr(err).\n")
			w("\t\t\t\tMsg(\"route execution failed\")\n")
			w("\t\t\trecordHTTPError(%q, c.Method(), fiber.StatusBadGateway)\n", r.Path)
			w("\t\t\tsanitizedMsg := runtime.SanitizeError(err)\n")
			w("\t\t\treturn fiber.NewError(fiber.StatusBadGateway, sanitizedMsg)\n")
			w("\t\t}\n\n")
			w("\t\tif resp == nil {\n")
			w("\t\t\tduration := time.Since(startTime)\n")
			w("\t\t\trecordHTTPRequestDuration(endpoint, method, fiber.StatusInternalServerError, duration)\n")
			w("\t\t\trecordHTTPResponseSize(endpoint, method, fiber.StatusInternalServerError, 0)\n")
			w("\t\t\treturn fiber.NewError(500, \"empty response\")\n")
			w("\t\t}\n\n")
			w("\t\t// Set headers\n")
			w("\t\tfor k, v := range resp.Headers {\n")
			w("\t\t\tc.Set(k, v)\n")
			w("\t\t}\n\n")
			w("\t\t// Handle redirects\n")
			w("\t\tif resp.StatusCode >= 300 && resp.StatusCode < 400 {\n")
			w("\t\t\tif location := resp.Headers[\"Location\"]; location != \"\" {\n")
			w("\t\t\t\tc.Set(\"Location\", location)\n")
			w("\t\t\t\tc.Status(resp.StatusCode)\n")
			w("\t\t\t\tc.Redirect()\n")
			w("\t\t\t\treturn nil\n")
			w("\t\t\t}\n")
			w("\t\t}\n\n")
			w("\t\t// Handle 204 No Content\n")
			w("\t\tif resp.StatusCode == 204 {\n")
			w("\t\t\tduration := time.Since(startTime)\n")
			w("\t\t\trecordHTTPRequestDuration(endpoint, method, 204, duration)\n")
			w("\t\t\trecordHTTPResponseSize(endpoint, method, 204, 0)\n")
			w("\t\t\t// Check for slow request\n")
			w("\t\t\tthreshold := getCachedSlowRequestThreshold()\n")
			if r.SlowRequestThreshold > 0 {
				w("\t\t\tif duration > %d*time.Nanosecond {\n", r.SlowRequestThreshold.Nanoseconds())
				w("\t\t\t\trecordSlowRequest(endpoint, method)\n")
				w("\t\t\t}\n")
			} else {
				w("\t\t\tif duration > threshold {\n")
				w("\t\t\t\trecordSlowRequest(endpoint, method)\n")
				w("\t\t\t}\n")
			}
			w("\t\t\treturn c.Status(204).Send(nil)\n")
			w("\t\t}\n\n")
			w("\t\tduration := time.Since(startTime)\n")
			w("\t\tstatusCode := resp.StatusCode\n")
			w("\t\tresponseSize := len(resp.Body)\n")
			w("\t\trecordHTTPRequestDuration(endpoint, method, statusCode, duration)\n")
			w("\t\trecordHTTPResponseSize(endpoint, method, statusCode, responseSize)\n")
			w("\t\t// Check for slow request\n")
			w("\t\tthreshold := getCachedSlowRequestThreshold()\n")
			if r.SlowRequestThreshold > 0 {
				w("\t\tif duration > %d*time.Nanosecond {\n", r.SlowRequestThreshold.Nanoseconds())
				w("\t\t\trecordSlowRequest(endpoint, method)\n")
				w("\t\t}\n")
			} else {
				w("\t\tif duration > threshold {\n")
				w("\t\t\trecordSlowRequest(endpoint, method)\n")
				w("\t\t}\n")
			}
			w("\t\treturn c.Status(resp.StatusCode).Send(resp.Body)\n")
			w("\t})\n\n")
		}
	}

	w("}\n")
	return format.Source(buf.Bytes())
}

// writeRequestContextBuilder emits Go code that populates a runtime.RequestContext
// from the Fiber Ctx. It only extracts the specific fields that the JSONata expression
// actually references — no wasted work copying all headers/cookies on every request.
func writeRequestContextBuilder(w func(string, ...any), indent string, rk requestKeys) {
	w("%sreqCtx := runtime.RequestContext{}\n", indent)

	// Only extract specific headers that the expression references.
	if len(rk.Headers) > 0 {
		w("%sreqCtx.Headers = map[string]string{\n", indent)
		for _, h := range rk.Headers {
			w("%s\t%q: c.Get(%q),\n", indent, h, h)
		}
		w("%s}\n", indent)
	}

	// Only extract specific cookies.
	if len(rk.Cookies) > 0 {
		w("%sreqCtx.Cookies = map[string]string{\n", indent)
		for _, c := range rk.Cookies {
			w("%s\t%q: c.Cookies(%q),\n", indent, c, c)
		}
		w("%s}\n", indent)
	}

	if rk.Query {
		w("%sif queries := c.Queries(); len(queries) > 0 {\n", indent)
		w("%s\treqCtx.Query = queries\n", indent)
		w("%s}\n", indent)
	}

	if rk.Params {
		w("%sif routeParams := c.Route().Params; len(routeParams) > 0 {\n", indent)
		w("%s\treqCtx.Params = make(map[string]string, len(routeParams))\n", indent)
		w("%s\tfor _, p := range routeParams {\n", indent)
		w("%s\t\treqCtx.Params[p] = c.Params(p)\n", indent)
		w("%s\t}\n", indent)
		w("%s}\n", indent)
	}

	if rk.Path {
		w("%sreqCtx.Path = c.Path()\n", indent)
	}

	if rk.Method {
		w("%sreqCtx.Method = c.Method()\n", indent)
	}

	if rk.Body {
		w("%sreqCtx.Body = c.Body()\n", indent)
	}

	w("\n")
}

// --- Helpers ---

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(strings.ToLower(s))
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func exportedName(s string) string {
	acronyms := map[string]string{
		"id": "ID", "url": "URL", "api": "API", "http": "HTTP",
	}
	if v, ok := acronyms[strings.ToLower(s)]; ok {
		return v
	}
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if v, ok := acronyms[strings.ToLower(part)]; ok {
			b.WriteString(v)
		} else {
			runes := []rune(part)
			runes[0] = unicode.ToUpper(runes[0])
			b.WriteString(string(runes))
		}
	}
	return b.String()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "apigen: "+format+"\n", args...)
	os.Exit(1)
}

// extractComponents extracts the components section from OpenAPI doc for $ref resolution.
func extractComponents(doc map[string]interface{}) map[string]interface{} {
	components, ok := doc["components"].(map[string]interface{})
	if !ok {
		return make(map[string]interface{})
	}
	schemas, ok := components["schemas"].(map[string]interface{})
	if !ok {
		return make(map[string]interface{})
	}
	return schemas
}

// parseRequestSchema extracts request schema from an OpenAPI operation.
func parseRequestSchema(op map[string]interface{}, components map[string]interface{}) *RequestSchema {
	var schema RequestSchema

	// Parse parameters
	if paramsVal, ok := op["parameters"].([]interface{}); ok {
		for _, paramVal := range paramsVal {
			param, ok := paramVal.(map[string]interface{})
			if !ok {
				continue
			}
			parsedParam := parseParameter(param, components)
			if parsedParam == nil {
				continue
			}

			paramIn, _ := param["in"].(string)
			switch paramIn {
			case "query":
				schema.Query = append(schema.Query, *parsedParam)
			case "path":
				schema.Path = append(schema.Path, *parsedParam)
			case "header":
				schema.Header = append(schema.Header, *parsedParam)
			}
		}
	}

	// Parse requestBody
	if bodyVal, ok := op["requestBody"].(map[string]interface{}); ok {
		required, _ := bodyVal["required"].(bool)
		content, ok := bodyVal["content"].(map[string]interface{})
		if ok {
			// Look for application/json content
			if jsonContent, ok := content["application/json"].(map[string]interface{}); ok {
				if schemaVal, ok := jsonContent["schema"].(map[string]interface{}); ok {
					schemaInfo := parseSchemaInfo(schemaVal, components)
					if schemaInfo != nil {
						schema.Body = &BodySchema{
							Required: required,
							Schema:   *schemaInfo,
						}
					}
				}
			}
		}
	}

	if !schema.HasSchema() {
		return nil
	}
	return &schema
}

// parseParameter parses a single OpenAPI parameter.
func parseParameter(param map[string]interface{}, components map[string]interface{}) *Parameter {
	name, _ := param["name"].(string)
	if name == "" {
		return nil
	}

	required, _ := param["required"].(bool)
	schemaVal, ok := param["schema"].(map[string]interface{})
	if !ok {
		return nil
	}

	schemaInfo := parseSchemaInfo(schemaVal, components)
	if schemaInfo == nil {
		return nil
	}

	return &Parameter{
		Name:     name,
		Required: required,
		Schema:   *schemaInfo,
	}
}

// parseSchemaInfo parses a JSON Schema definition, resolving $ref if needed.
func parseSchemaInfo(schemaVal map[string]interface{}, components map[string]interface{}) *SchemaInfo {
	// Handle $ref
	if ref, ok := schemaVal["$ref"].(string); ok {
		return resolveRef(ref, components)
	}

	schema := SchemaInfo{
		Properties: make(map[string]SchemaInfo),
	}

	// Type
	if typeVal, ok := schemaVal["type"].(string); ok {
		schema.Type = typeVal
	}

	// Format
	if formatVal, ok := schemaVal["format"].(string); ok {
		schema.Format = formatVal
	}

	// Required fields (for objects)
	if requiredVal, ok := schemaVal["required"].([]interface{}); ok {
		for _, req := range requiredVal {
			if reqStr, ok := req.(string); ok {
				schema.Required = append(schema.Required, reqStr)
			}
		}
	}

	// Properties (for objects)
	if propsVal, ok := schemaVal["properties"].(map[string]interface{}); ok {
		for propName, propVal := range propsVal {
			if propMap, ok := propVal.(map[string]interface{}); ok {
				propSchema := parseSchemaInfo(propMap, components)
				if propSchema != nil {
					schema.Properties[propName] = *propSchema
				}
			}
		}
	}

	// Items (for arrays)
	if itemsVal, ok := schemaVal["items"].(map[string]interface{}); ok {
		itemsSchema := parseSchemaInfo(itemsVal, components)
		if itemsSchema != nil {
			schema.Items = itemsSchema
		}
	}

	// Constraints
	constraints := Constraints{}
	if minLen, ok := schemaVal["minLength"].(int); ok {
		constraints.MinLength = &minLen
	}
	if maxLen, ok := schemaVal["maxLength"].(int); ok {
		constraints.MaxLength = &maxLen
	}
	if min, ok := schemaVal["minimum"].(float64); ok {
		constraints.Minimum = &min
	} else if min, ok := schemaVal["minimum"].(int); ok {
		minFloat := float64(min)
		constraints.Minimum = &minFloat
	}
	if max, ok := schemaVal["maximum"].(float64); ok {
		constraints.Maximum = &max
	} else if max, ok := schemaVal["maximum"].(int); ok {
		maxFloat := float64(max)
		constraints.Maximum = &maxFloat
	}
	if pattern, ok := schemaVal["pattern"].(string); ok {
		constraints.Pattern = pattern
	}
	if enumVal, ok := schemaVal["enum"].([]interface{}); ok {
		constraints.Enum = enumVal
	}
	schema.Constraints = constraints

	return &schema
}

// resolveRef resolves a $ref reference to a schema.
func resolveRef(ref string, components map[string]interface{}) *SchemaInfo {
	// Handle #/components/schemas/Name format
	if !strings.HasPrefix(ref, "#/components/schemas/") {
		return nil
	}
	schemaName := strings.TrimPrefix(ref, "#/components/schemas/")
	schemaVal, ok := components[schemaName].(map[string]interface{})
	if !ok {
		return nil
	}
	schema := parseSchemaInfo(schemaVal, components)
	if schema != nil {
		schema.Ref = ref
	}
	return schema
}
