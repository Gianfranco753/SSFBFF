package transpiler

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/blues/jsonata-go/jparse"
)

// QueryPlan is the intermediate representation extracted from a JSONata AST.
// It captures everything needed to generate Go code: which field to stream to,
// what to filter, and how to project the output.
type QueryPlan struct {
	RootField    string        // top-level JSON field to navigate to (e.g., "orders")
	InputFields  []StructField // fields to deserialize from each array element
	Filters      []Filter      // conditions applied to each element
	OutputName   string        // Go type name for output struct
	OutputFields []OutputField // fields in the output struct
	FuncName     string        // generated Go function name
}

// StructField describes a field in the generated input struct.
type StructField struct {
	JSONName string        // field name in JSON (e.g., "order_id")
	GoName   string        // exported Go name (e.g., "OrderID")
	GoType   string        // Go type (e.g., "float64", "any")
	IsArray  bool          // true if this field is a slice of objects
	Children []StructField // nested fields (only when IsArray is true)
}

// Filter describes a comparison applied to each array element.
type Filter struct {
	FieldGoName string // Go struct field name to compare (e.g., "Price")
	Op          string // Go comparison operator (e.g., ">")
	Literal     string // Go literal value (e.g., "100")
}

// OutputField describes a field in the generated output struct.
type OutputField struct {
	JSONName string // output JSON name (e.g., "id")
	GoName   string // exported Go name (e.g., "ID")
	GoType   string // Go type (e.g., "float64", "any")

	// Exactly one of these groups is populated:
	SourceField string // direct field mapping — Go name of input field (e.g., "OrderID")

	AggregateFunc  string // aggregate function name (e.g., "sum")
	AggregateArray string // Go name of the array field (e.g., "Items")
	AggregateField string // Go name of the field inside the array element (e.g., "Price")
}

// Analyze walks a parsed JSONata AST and produces a QueryPlan.
// It supports the pattern: rootField[predicate].{key: value, ...}
func Analyze(root jparse.Node) (*QueryPlan, error) {
	path, ok := root.(*jparse.PathNode)
	if !ok {
		return nil, fmt.Errorf("expected a path expression at the top level, got %T", root)
	}

	if len(path.Steps) < 2 {
		return nil, fmt.Errorf("expected at least 2 path steps (source + projection), got %d", len(path.Steps))
	}

	pred, ok := path.Steps[0].(*jparse.PredicateNode)
	if !ok {
		return nil, fmt.Errorf("expected first step to be a predicate (array[filter]), got %T", path.Steps[0])
	}

	obj, ok := path.Steps[1].(*jparse.ObjectNode)
	if !ok {
		return nil, fmt.Errorf("expected second step to be an object projection ({...}), got %T", path.Steps[1])
	}

	rootField, err := extractName(pred.Expr)
	if err != nil {
		return nil, fmt.Errorf("extracting root field name: %w", err)
	}

	// Collect all fields referenced anywhere in the expression.
	// We track which fields are numeric (used in comparisons or aggregations).
	fields := &fieldCollector{numeric: map[string]bool{}}

	// --- Filters ---
	var filters []Filter
	for _, f := range pred.Filters {
		filter, err := analyzeFilter(f, fields)
		if err != nil {
			return nil, fmt.Errorf("analyzing filter: %w", err)
		}
		filters = append(filters, filter)
	}

	// --- Projections ---
	var outputFields []OutputField
	for _, pair := range obj.Pairs {
		out, err := analyzeProjection(pair, fields)
		if err != nil {
			return nil, fmt.Errorf("analyzing projection: %w", err)
		}
		outputFields = append(outputFields, out)
	}

	inputFields := fields.build()
	funcName := "Transform" + exportedName(rootField)

	return &QueryPlan{
		RootField:    rootField,
		InputFields:  inputFields,
		Filters:      filters,
		OutputName:   exportedName(rootField) + "Result",
		OutputFields: outputFields,
		FuncName:     funcName,
	}, nil
}

// --- Filter analysis ---

func analyzeFilter(node jparse.Node, fc *fieldCollector) (Filter, error) {
	cmp, ok := node.(*jparse.ComparisonOperatorNode)
	if !ok {
		return Filter{}, fmt.Errorf("unsupported filter type %T (only comparisons supported)", node)
	}

	fieldPath, err := extractPath(cmp.LHS)
	if err != nil {
		return Filter{}, fmt.Errorf("filter LHS: %w", err)
	}

	num, ok := cmp.RHS.(*jparse.NumberNode)
	if !ok {
		return Filter{}, fmt.Errorf("filter RHS must be a number literal, got %T", cmp.RHS)
	}

	jsonName := fieldPath[0]
	fc.addField(jsonName, true)

	op := comparisonOpToGo(cmp.Type)
	literal := formatFloat(num.Value)

	return Filter{
		FieldGoName: exportedName(jsonName),
		Op:          op,
		Literal:     literal,
	}, nil
}

// --- Projection analysis ---

func analyzeProjection(pair [2]jparse.Node, fc *fieldCollector) (OutputField, error) {
	keyName, err := extractSingleName(pair[0])
	if err != nil {
		return OutputField{}, fmt.Errorf("projection key: %w", err)
	}

	out := OutputField{
		JSONName: keyName,
		GoName:   exportedName(keyName),
	}

	switch val := pair[1].(type) {
	case *jparse.PathNode:
		// Direct field mapping: value comes from a field on the element.
		jsonField, err := extractSingleName(val)
		if err != nil {
			return OutputField{}, fmt.Errorf("projection value path: %w", err)
		}
		fc.addField(jsonField, false)
		out.SourceField = exportedName(jsonField)
		out.GoType = fc.typeOf(jsonField)

	case *jparse.FunctionCallNode:
		// Aggregate function, e.g. $sum(items.price)
		funcName, err := extractVariableName(val.Func)
		if err != nil {
			return OutputField{}, fmt.Errorf("function name: %w", err)
		}
		if len(val.Args) != 1 {
			return OutputField{}, fmt.Errorf("expected 1 argument for $%s, got %d", funcName, len(val.Args))
		}
		argPath, err := extractPath(val.Args[0])
		if err != nil {
			return OutputField{}, fmt.Errorf("$%s argument: %w", funcName, err)
		}
		if len(argPath) != 2 {
			return OutputField{}, fmt.Errorf("$%s argument must be a two-part path (array.field), got %v", funcName, argPath)
		}

		arrayField := argPath[0]
		innerField := argPath[1]

		fc.addArrayField(arrayField, innerField, true)

		out.AggregateFunc = funcName
		out.AggregateArray = exportedName(arrayField)
		out.AggregateField = exportedName(innerField)
		out.GoType = "float64"

	default:
		return OutputField{}, fmt.Errorf("unsupported projection value type %T", pair[1])
	}

	return out, nil
}

// --- Field collector ---
// Tracks all fields accessed from each array element and their inferred types.

type fieldCollector struct {
	fields  []collectedField
	numeric map[string]bool // jsonName -> whether it's used in numeric context
}

type collectedField struct {
	jsonName   string
	isArray    bool
	childName  string // only set when isArray is true
	childIsNum bool
}

func (fc *fieldCollector) addField(jsonName string, isNumeric bool) {
	if isNumeric {
		fc.numeric[jsonName] = true
	}
	for _, f := range fc.fields {
		if f.jsonName == jsonName && !f.isArray {
			return
		}
	}
	fc.fields = append(fc.fields, collectedField{jsonName: jsonName})
}

func (fc *fieldCollector) addArrayField(arrayName, childName string, childIsNumeric bool) {
	for i, f := range fc.fields {
		if f.jsonName == arrayName && f.isArray {
			// Already tracked; ensure child is present.
			if f.childName == childName {
				if childIsNumeric {
					fc.fields[i].childIsNum = true
				}
				return
			}
		}
	}
	fc.fields = append(fc.fields, collectedField{
		jsonName:   arrayName,
		isArray:    true,
		childName:  childName,
		childIsNum: childIsNumeric,
	})
}

func (fc *fieldCollector) typeOf(jsonName string) string {
	if fc.numeric[jsonName] {
		return "float64"
	}
	return "any"
}

func (fc *fieldCollector) build() []StructField {
	var result []StructField
	for _, f := range fc.fields {
		if f.isArray {
			childType := "any"
			if f.childIsNum {
				childType = "float64"
			}
			result = append(result, StructField{
				JSONName: f.jsonName,
				GoName:   exportedName(f.jsonName),
				IsArray:  true,
				Children: []StructField{
					{
						JSONName: f.childName,
						GoName:   exportedName(f.childName),
						GoType:   childType,
					},
				},
			})
		} else {
			result = append(result, StructField{
				JSONName: f.jsonName,
				GoName:   exportedName(f.jsonName),
				GoType:   fc.typeOf(f.jsonName),
			})
		}
	}
	return result
}

// --- Fetch-mode / request-context analysis ---
//
// Supports two kinds of custom function calls in JSONata expressions:
//
// 1. $fetch("provider", "endpoint") — declares an upstream data dependency.
//    Optional 3rd arg is a config object: {"method": "POST", "headers": {...}, "body": {...}}
//
// 2. $request() — reads data from the incoming HTTP request via dot-path navigation:
//    $request().headers.Authorization, $request().cookies.session,
//    $request().query.page, $request().params.id,
//    $request().path, $request().method, $request().body.field

// ProviderPlan is the intermediate representation for expressions that use
// $fetch() calls, $service() calls, and/or request functions.
type ProviderPlan struct {
	FuncName     string             // e.g. "TransformDashboard"
	Fields       []ProviderField    // output fields in order
	Deps         []ProviderDepEntry // unique provider+endpoint pairs needed
	Services     []string           // unique service names referenced via $service()
	NeedsRequest bool               // true if any field or fetch config uses request functions
}

// ProviderField describes one key in the output JSON object. Kind determines
// which group of fields is populated — readers only need to look at Kind and
// the corresponding group.
type ProviderField struct {
	OutputKey string
	Kind      string // "fetch", "service", "header", "cookie", "query", "param", "path", "method", "body", "static"

	// Kind="fetch": value comes from a pre-fetched upstream response.
	Provider    string
	Endpoint    string
	JSONPath    []string
	FetchConfig *FetchConfig // nil when $fetch() has only 2 args

	// Kind="service": value comes from another generated transform pipeline.
	// JSONPath is reused for the path into the service result.
	ServiceName string

	// Kind="header"/"cookie"/"query"/"param": value from $request().headers.X etc.
	Arg string

	// Kind="body": value extracted from $request().body via path navigation.
	BodyPath []string // empty = entire body

	// Kind="static": a literal string value.
	StaticValue string
}

// FetchConfig describes the optional 3rd argument to $fetch() — an object
// that shapes the outgoing HTTP request to the upstream provider.
type FetchConfig struct {
	Method  string        // HTTP method (empty = default GET)
	Headers []ConfigEntry // custom headers
	Body    []ConfigEntry // JSON body fields
}

// ConfigEntry is a key-value pair inside a FetchConfig.
type ConfigEntry struct {
	Key   string
	Value ConfigValue
}

// ConfigValue is a simple expression used inside a FetchConfig. It's either
// a static string or a $request() path (e.g. $request().headers.Authorization).
type ConfigValue struct {
	Kind   string   // "static", "header", "cookie", "query", "param", "path", "method", "body"
	Arg    string   // for header/cookie/query/param: the key name from the path
	Path   []string // for body: path segments after $request().body
	Static string   // for static: the literal value
}

// ProviderDepEntry is a unique provider+endpoint pair.
type ProviderDepEntry struct {
	Provider string
	Endpoint string
}

// AnalyzeFetchCalls walks a JSONata AST that uses $fetch() and/or $request()
// calls, producing a ProviderPlan. It expects an ObjectNode at the root
// where each value is one of:
//   - $fetch("provider", "endpoint").path             → Kind="fetch"
//   - $fetch("provider", "endpoint", {config}).path   → Kind="fetch" + FetchConfig
//   - $request().headers.Name                         → Kind="header"
//   - $request().cookies.Name                         → Kind="cookie"
//   - $request().query.Name                           → Kind="query"
//   - $request().params.Name                          → Kind="param"
//   - $request().path                                 → Kind="path"
//   - $request().method                               → Kind="method"
//   - $request().body.field                            → Kind="body"
func AnalyzeFetchCalls(root jparse.Node, funcName string) (*ProviderPlan, error) {
	obj := unwrapObject(root)
	if obj == nil {
		return nil, fmt.Errorf("expression must be an object literal, got %T", root)
	}

	plan := &ProviderPlan{FuncName: funcName}
	seen := map[string]bool{}

	for _, pair := range obj.Pairs {
		keyName, err := extractSingleName(pair[0])
		if err != nil {
			return nil, fmt.Errorf("object key: %w", err)
		}

		field, err := analyzeValueNode(pair[1])
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", keyName, err)
		}
		field.OutputKey = keyName

		// Track whether request context is needed.
		if field.Kind != "fetch" && field.Kind != "static" {
			plan.NeedsRequest = true
		}
		if field.FetchConfig != nil && fetchConfigNeedsRequest(field.FetchConfig) {
			plan.NeedsRequest = true
		}

		plan.Fields = append(plan.Fields, field)

		// Register unique fetch deps.
		if field.Kind == "fetch" {
			depKey := field.Provider + "." + field.Endpoint
			if !seen[depKey] {
				seen[depKey] = true
				plan.Deps = append(plan.Deps, ProviderDepEntry{
					Provider: field.Provider,
					Endpoint: field.Endpoint,
				})
			}
		}

		// Register unique service deps.
		if field.Kind == "service" {
			svcKey := "$service." + field.ServiceName
			if !seen[svcKey] {
				seen[svcKey] = true
				plan.Services = append(plan.Services, field.ServiceName)
			}
		}
	}

	return plan, nil
}

// HasFetchCalls returns true if the AST contains $fetch(), $request(), or
// $service() calls. Used to auto-detect which codegen path the transpiler
// should take.
func HasFetchCalls(root jparse.Node) bool {
	switch n := root.(type) {
	case *jparse.PathNode:
		for _, step := range n.Steps {
			if HasFetchCalls(step) {
				return true
			}
		}
	case *jparse.ObjectNode:
		for _, pair := range n.Pairs {
			if HasFetchCalls(pair[0]) || HasFetchCalls(pair[1]) {
				return true
			}
		}
	case *jparse.FunctionCallNode:
		if v, ok := n.Func.(*jparse.VariableNode); ok {
			if v.Name == "fetch" || v.Name == "request" || v.Name == "service" {
				return true
			}
		}
	}
	return false
}

// unwrapObject extracts an ObjectNode from the root, handling the case where
// jparse wraps it in a PathNode.
func unwrapObject(root jparse.Node) *jparse.ObjectNode {
	if obj, ok := root.(*jparse.ObjectNode); ok {
		return obj
	}
	if p, ok := root.(*jparse.PathNode); ok && len(p.Steps) == 1 {
		if obj, ok := p.Steps[0].(*jparse.ObjectNode); ok {
			return obj
		}
	}
	return nil
}

// analyzeValueNode determines the kind of a value expression and extracts its
// metadata. It handles $fetch(), request functions, and string literals.
func analyzeValueNode(node jparse.Node) (ProviderField, error) {
	// A bare FunctionCallNode (no path after it) — e.g. $path() or $method().
	if fnCall, ok := node.(*jparse.FunctionCallNode); ok {
		return analyzeFunctionCall(fnCall, nil)
	}

	// A StringNode — static literal value.
	if str, ok := node.(*jparse.StringNode); ok {
		return ProviderField{Kind: "static", StaticValue: str.Value}, nil
	}

	// A PathNode — could be $fetch(...).field, $body().field, $header("X"), etc.
	path, ok := node.(*jparse.PathNode)
	if !ok {
		return ProviderField{}, fmt.Errorf("unsupported value expression %T", node)
	}
	if len(path.Steps) < 1 {
		return ProviderField{}, fmt.Errorf("empty path")
	}

	fnCall, ok := path.Steps[0].(*jparse.FunctionCallNode)
	if !ok {
		return ProviderField{}, fmt.Errorf("expected a function call at start of path, got %T", path.Steps[0])
	}

	// Collect trailing path segments (NameNodes after the function call).
	var trailingPath []string
	for _, step := range path.Steps[1:] {
		name, ok := step.(*jparse.NameNode)
		if !ok {
			return ProviderField{}, fmt.Errorf("expected name in path after function call, got %T", step)
		}
		trailingPath = append(trailingPath, name.Value)
	}

	return analyzeFunctionCall(fnCall, trailingPath)
}

// analyzeFunctionCall parses a FunctionCallNode and its trailing path segments
// into a ProviderField. It handles $fetch() and $request().
func analyzeFunctionCall(fnCall *jparse.FunctionCallNode, trailingPath []string) (ProviderField, error) {
	fnVar, ok := fnCall.Func.(*jparse.VariableNode)
	if !ok {
		return ProviderField{}, fmt.Errorf("expected a named function, got %T", fnCall.Func)
	}

	switch fnVar.Name {
	case "fetch":
		return analyzeFetchFn(fnCall, trailingPath)

	case "service":
		return analyzeServiceFn(fnCall, trailingPath)

	case "request":
		if len(fnCall.Args) != 0 {
			return ProviderField{}, fmt.Errorf("$request() takes no arguments, got %d", len(fnCall.Args))
		}
		return analyzeRequestPath(trailingPath)

	default:
		return ProviderField{}, fmt.Errorf("unsupported function $%s", fnVar.Name)
	}
}

// analyzeRequestPath maps $request() trailing path segments to a ProviderField.
// e.g. ["headers", "Authorization"] → Kind="header", Arg="Authorization"
//      ["path"]                     → Kind="path"
//      ["body", "user", "name"]     → Kind="body", BodyPath=["user","name"]
func analyzeRequestPath(path []string) (ProviderField, error) {
	if len(path) == 0 {
		return ProviderField{}, fmt.Errorf("$request() requires a category (e.g. $request().headers.Name)")
	}

	category := path[0]

	switch category {
	case "headers", "cookies", "query", "params":
		if len(path) < 2 {
			return ProviderField{}, fmt.Errorf("$request().%s requires a key name (e.g. $request().%s.Name)", category, category)
		}
		kind := categoryToKind[category]
		return ProviderField{Kind: kind, Arg: path[1]}, nil

	case "path":
		return ProviderField{Kind: "path"}, nil

	case "method":
		return ProviderField{Kind: "method"}, nil

	case "body":
		return ProviderField{Kind: "body", BodyPath: path[1:]}, nil

	default:
		return ProviderField{}, fmt.Errorf("$request().%s is not a valid category (use headers/cookies/query/params/path/method/body)", category)
	}
}

// categoryToKind maps $request() path segments to internal Kind values.
var categoryToKind = map[string]string{
	"headers": "header",
	"cookies": "cookie",
	"query":   "query",
	"params":  "param",
}

// analyzeFetchFn parses a $fetch() call with 2 or 3 arguments.
func analyzeFetchFn(fnCall *jparse.FunctionCallNode, trailingPath []string) (ProviderField, error) {
	if len(fnCall.Args) < 2 || len(fnCall.Args) > 3 {
		return ProviderField{}, fmt.Errorf("$fetch() requires 2 or 3 arguments, got %d", len(fnCall.Args))
	}

	providerArg, ok := fnCall.Args[0].(*jparse.StringNode)
	if !ok {
		return ProviderField{}, fmt.Errorf("$fetch() first argument must be a string literal, got %T", fnCall.Args[0])
	}
	endpointArg, ok := fnCall.Args[1].(*jparse.StringNode)
	if !ok {
		return ProviderField{}, fmt.Errorf("$fetch() second argument must be a string literal, got %T", fnCall.Args[1])
	}

	field := ProviderField{
		Kind:     "fetch",
		Provider: providerArg.Value,
		Endpoint: endpointArg.Value,
		JSONPath: trailingPath,
	}

	// Optional 3rd arg: request config object.
	if len(fnCall.Args) == 3 {
		cfg, err := analyzeFetchConfig(fnCall.Args[2])
		if err != nil {
			return ProviderField{}, fmt.Errorf("$fetch() config: %w", err)
		}
		field.FetchConfig = cfg
	}

	return field, nil
}

// analyzeServiceFn parses a $service("name") call. It takes one string argument
// (the service name) and optional trailing path segments for extracting nested
// values from the service result.
func analyzeServiceFn(fnCall *jparse.FunctionCallNode, trailingPath []string) (ProviderField, error) {
	if len(fnCall.Args) != 1 {
		return ProviderField{}, fmt.Errorf("$service() requires exactly 1 argument, got %d", len(fnCall.Args))
	}

	nameArg, ok := fnCall.Args[0].(*jparse.StringNode)
	if !ok {
		return ProviderField{}, fmt.Errorf("$service() argument must be a string literal, got %T", fnCall.Args[0])
	}

	return ProviderField{
		Kind:        "service",
		ServiceName: nameArg.Value,
		JSONPath:    trailingPath,
	}, nil
}

// analyzeFetchConfig parses the 3rd argument of $fetch() — an ObjectNode with
// optional "method", "headers", and "body" keys.
func analyzeFetchConfig(node jparse.Node) (*FetchConfig, error) {
	obj := unwrapObject(node)
	if obj == nil {
		return nil, fmt.Errorf("expected an object literal, got %T", node)
	}

	cfg := &FetchConfig{}

	for _, pair := range obj.Pairs {
		key, err := extractSingleName(pair[0])
		if err != nil {
			return nil, fmt.Errorf("config key: %w", err)
		}

		switch key {
		case "method":
			str, ok := pair[1].(*jparse.StringNode)
			if !ok {
				return nil, fmt.Errorf("\"method\" must be a string literal, got %T", pair[1])
			}
			cfg.Method = str.Value

		case "headers":
			entries, err := analyzeConfigObject(pair[1], "headers")
			if err != nil {
				return nil, err
			}
			cfg.Headers = entries

		case "body":
			entries, err := analyzeConfigObject(pair[1], "body")
			if err != nil {
				return nil, err
			}
			cfg.Body = entries

		default:
			return nil, fmt.Errorf("unknown config key %q (expected method/headers/body)", key)
		}
	}

	return cfg, nil
}

// analyzeConfigObject parses an object whose values are either string literals
// or $request() path expressions (e.g. $request().headers.Authorization).
func analyzeConfigObject(node jparse.Node, label string) ([]ConfigEntry, error) {
	obj := unwrapObject(node)
	if obj == nil {
		return nil, fmt.Errorf("%q must be an object literal, got %T", label, node)
	}

	var entries []ConfigEntry
	for _, pair := range obj.Pairs {
		key, err := extractSingleName(pair[0])
		if err != nil {
			return nil, fmt.Errorf("%s key: %w", label, err)
		}

		val, err := analyzeConfigValue(pair[1])
		if err != nil {
			return nil, fmt.Errorf("%s[%q]: %w", label, key, err)
		}

		entries = append(entries, ConfigEntry{Key: key, Value: val})
	}
	return entries, nil
}

// analyzeConfigValue parses a single value inside a fetch config — either a
// static string or a $request() path expression.
func analyzeConfigValue(node jparse.Node) (ConfigValue, error) {
	// Static string literal.
	if str, ok := node.(*jparse.StringNode); ok {
		return ConfigValue{Kind: "static", Static: str.Value}, nil
	}

	// Bare function call: $path(), $method(), $header("X"), etc.
	if fnCall, ok := node.(*jparse.FunctionCallNode); ok {
		return analyzeConfigFuncCall(fnCall, nil)
	}

	// Path starting with function call: $body().field
	if path, ok := node.(*jparse.PathNode); ok && len(path.Steps) >= 1 {
		if fnCall, ok := path.Steps[0].(*jparse.FunctionCallNode); ok {
			var trailing []string
			for _, step := range path.Steps[1:] {
				name, ok := step.(*jparse.NameNode)
				if !ok {
					return ConfigValue{}, fmt.Errorf("expected name in path, got %T", step)
				}
				trailing = append(trailing, name.Value)
			}
			return analyzeConfigFuncCall(fnCall, trailing)
		}
	}

	return ConfigValue{}, fmt.Errorf("expected a string literal or request function, got %T", node)
}

func analyzeConfigFuncCall(fnCall *jparse.FunctionCallNode, trailing []string) (ConfigValue, error) {
	fnVar, ok := fnCall.Func.(*jparse.VariableNode)
	if !ok {
		return ConfigValue{}, fmt.Errorf("expected a named function, got %T", fnCall.Func)
	}

	if fnVar.Name != "request" {
		return ConfigValue{}, fmt.Errorf("unsupported function $%s in config (use $request())", fnVar.Name)
	}

	if len(fnCall.Args) != 0 {
		return ConfigValue{}, fmt.Errorf("$request() takes no arguments, got %d", len(fnCall.Args))
	}

	return analyzeConfigRequestPath(trailing)
}

// analyzeConfigRequestPath maps $request() trailing path segments to a ConfigValue.
func analyzeConfigRequestPath(path []string) (ConfigValue, error) {
	if len(path) == 0 {
		return ConfigValue{}, fmt.Errorf("$request() requires a category in config context")
	}

	category := path[0]

	switch category {
	case "headers", "cookies", "query", "params":
		if len(path) < 2 {
			return ConfigValue{}, fmt.Errorf("$request().%s requires a key name", category)
		}
		kind := categoryToKind[category]
		return ConfigValue{Kind: kind, Arg: path[1]}, nil

	case "path":
		return ConfigValue{Kind: "path"}, nil

	case "method":
		return ConfigValue{Kind: "method"}, nil

	case "body":
		return ConfigValue{Kind: "body", Path: path[1:]}, nil

	default:
		return ConfigValue{}, fmt.Errorf("$request().%s is not a valid category in config", category)
	}
}

// fetchConfigNeedsRequest returns true if any value in the config references
// request data (anything other than "static").
func fetchConfigNeedsRequest(cfg *FetchConfig) bool {
	for _, e := range cfg.Headers {
		if e.Value.Kind != "static" {
			return true
		}
	}
	for _, e := range cfg.Body {
		if e.Value.Kind != "static" {
			return true
		}
	}
	return false
}

// --- Helpers ---

func extractName(node jparse.Node) (string, error) {
	switch n := node.(type) {
	case *jparse.NameNode:
		return n.Value, nil
	case *jparse.StringNode:
		return n.Value, nil
	case *jparse.PathNode:
		if len(n.Steps) == 1 {
			return extractName(n.Steps[0])
		}
	}
	return "", fmt.Errorf("expected a simple name, got %T (%s)", node, node)
}

func extractSingleName(node jparse.Node) (string, error) {
	if p, ok := node.(*jparse.PathNode); ok && len(p.Steps) == 1 {
		return extractName(p.Steps[0])
	}
	return extractName(node)
}

func extractPath(node jparse.Node) ([]string, error) {
	switch n := node.(type) {
	case *jparse.PathNode:
		var parts []string
		for _, step := range n.Steps {
			name, err := extractName(step)
			if err != nil {
				return nil, err
			}
			parts = append(parts, name)
		}
		return parts, nil
	case *jparse.NameNode:
		return []string{n.Value}, nil
	}
	return nil, fmt.Errorf("expected a path, got %T (%s)", node, node)
}

func extractVariableName(node jparse.Node) (string, error) {
	v, ok := node.(*jparse.VariableNode)
	if !ok {
		return "", fmt.Errorf("expected a variable ($name), got %T", node)
	}
	return v.Name, nil
}

func comparisonOpToGo(op jparse.ComparisonOperator) string {
	switch op {
	case jparse.ComparisonEqual:
		return "=="
	case jparse.ComparisonNotEqual:
		return "!="
	case jparse.ComparisonLess:
		return "<"
	case jparse.ComparisonLessEqual:
		return "<="
	case jparse.ComparisonGreater:
		return ">"
	case jparse.ComparisonGreaterEqual:
		return ">="
	default:
		return "=="
	}
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// ExportedName converts a snake_case or lowercase JSON name to an exported Go name.
// "order_id" -> "OrderID", "price" -> "Price", "id" -> "ID"
// It is also used by the transpiler CLI to derive function names from file names.
func ExportedName(s string) string {
	return exportedName(s)
}

func exportedName(s string) string {
	// Common abbreviations that should be all-caps.
	acronyms := map[string]string{
		"id":   "ID",
		"url":  "URL",
		"api":  "API",
		"http": "HTTP",
		"json": "JSON",
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
