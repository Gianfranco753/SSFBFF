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
	Filters      []*Expr       // each entry is a predicate expression; all ANDed
	Bindings     []*Expr       // variable assignments ($x := expr) before the projection
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

// OutputField describes a field in the generated output struct.
type OutputField struct {
	JSONName string // output JSON name (e.g., "id")
	GoName   string // exported Go name (e.g., "ID")
	GoType   string // Go type (e.g., "float64", "any")
	Value    *Expr  // expression tree that computes this field's value
}

// Expr represents a value expression in the filter-mode pipeline.
// It is a recursive tree that models field access, literals, function calls,
// arithmetic, comparisons, boolean logic, string concatenation, conditionals,
// and variable bindings.
type Expr struct {
	Kind   string // "field","arrayField","literal","funcCall","binary","unary","conditional","assign","varRef"
	GoType string // inferred Go type: "float64","string","bool","any"

	// Kind="field": reference to an input struct field.
	FieldName string // Go struct field name (e.g., "Price")
	FieldJSON string // JSON name (e.g., "price")

	// Kind="arrayField": reference to a nested array+child pattern (items.price).
	ArrayName string // Go name of array field
	ArrayJSON string // JSON name
	ChildName string // Go name of child field
	ChildJSON string // JSON name

	// Kind="literal": a Go literal value.
	LiteralValue string // e.g. "100", `"active"`, "true", "nil"

	// Kind="funcCall": a function invocation ($sum, $uppercase, …).
	FuncName string
	FuncArgs []*Expr

	// Kind="binary"/"unary": operator + operands.
	Op    string // Go operator: +,-,*,/,%,==,!=,<,<=,>,>=,&&,||,& (concat)
	Left  *Expr  // binary left / unary operand
	Right *Expr  // binary right (nil for unary)

	// Kind="conditional": ternary ? : expression.
	Cond *Expr
	Then *Expr
	Else *Expr

	// Kind="assign": variable binding ($x := expr).
	// Kind="varRef": variable reference ($x).
	VarName string // variable name (without $ prefix)
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

	// The projection step is usually an ObjectNode, but may be wrapped in a
	// BlockNode when variables are defined: items[filter].($x := expr; {output}).
	var obj *jparse.ObjectNode
	var bindingNodes []jparse.Node

	switch step := path.Steps[1].(type) {
	case *jparse.ObjectNode:
		obj = step
	case *jparse.BlockNode:
		if len(step.Exprs) < 1 {
			return nil, fmt.Errorf("empty block in projection")
		}
		last, ok := step.Exprs[len(step.Exprs)-1].(*jparse.ObjectNode)
		if !ok {
			return nil, fmt.Errorf("last expression in projection block must be an object ({...}), got %T", step.Exprs[len(step.Exprs)-1])
		}
		obj = last
		bindingNodes = step.Exprs[:len(step.Exprs)-1]
	default:
		return nil, fmt.Errorf("expected second step to be an object projection ({...}), got %T", path.Steps[1])
	}

	rootField, err := extractName(pred.Expr)
	if err != nil {
		return nil, fmt.Errorf("extracting root field name: %w", err)
	}

	// Collect all fields referenced anywhere in the expression.
	// We track which fields are numeric (used in comparisons or aggregations).
	fields := &fieldCollector{numeric: map[string]bool{}, varTypes: map[string]string{}}

	// --- Filters ---
	var filters []*Expr
	for _, f := range pred.Filters {
		filter, err := analyzeExpr(f, fields)
		if err != nil {
			return nil, fmt.Errorf("analyzing filter: %w", err)
		}
		filters = append(filters, filter)
	}

	// --- Variable bindings ---
	var bindings []*Expr
	for _, bNode := range bindingNodes {
		binding, err := analyzeExpr(bNode, fields)
		if err != nil {
			return nil, fmt.Errorf("analyzing variable binding: %w", err)
		}
		if binding.Kind != "assign" {
			return nil, fmt.Errorf("expected variable binding (:=) in projection block, got %s", binding.Kind)
		}
		bindings = append(bindings, binding)
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

	// Resolve field types after all references are collected.
	for _, f := range filters {
		resolveFieldTypes(f, fields)
	}
	for _, b := range bindings {
		resolveFieldTypes(b, fields)
	}
	for i := range outputFields {
		resolveFieldTypes(outputFields[i].Value, fields)
		outputFields[i].GoType = outputFields[i].Value.GoType
	}

	inputFields := fields.build()
	funcName := "Transform" + exportedName(rootField)

	return &QueryPlan{
		RootField:    rootField,
		InputFields:  inputFields,
		Filters:      filters,
		Bindings:     bindings,
		OutputName:   exportedName(rootField) + "Result",
		OutputFields: outputFields,
		FuncName:     funcName,
	}, nil
}

// --- Projection analysis ---

func analyzeProjection(pair [2]jparse.Node, fc *fieldCollector) (OutputField, error) {
	keyName, err := extractSingleName(pair[0])
	if err != nil {
		return OutputField{}, fmt.Errorf("projection key: %w", err)
	}

	value, err := analyzeExpr(pair[1], fc)
	if err != nil {
		return OutputField{}, fmt.Errorf("projection %q: %w", keyName, err)
	}

	return OutputField{
		JSONName: keyName,
		GoName:   exportedName(keyName),
		GoType:   value.GoType,
		Value:    value,
	}, nil
}

// --- Expression analysis ---
// analyzeExpr recursively walks a JSONata AST node and produces an Expr tree.
// It handles field references, literals, comparisons, boolean operators,
// arithmetic, negation, string concatenation, conditionals, and function calls.

func analyzeExpr(node jparse.Node, fc *fieldCollector) (*Expr, error) {
	switch n := node.(type) {
	case *jparse.PathNode:
		if len(n.Steps) == 1 {
			return analyzeExpr(n.Steps[0], fc)
		}
		// Multi-step path: if exactly 2 steps and both are names, treat as
		// a field.subfield reference (used in $sum(items.price) arguments).
		if len(n.Steps) == 2 {
			name1, err1 := extractName(n.Steps[0])
			name2, err2 := extractName(n.Steps[1])
			if err1 == nil && err2 == nil {
				// This is the items.price pattern. Register as array field.
				fc.addArrayField(name1, name2, true)
				return &Expr{
					Kind:      "arrayField",
					ArrayName: exportedName(name1),
					ArrayJSON: name1,
					ChildName: exportedName(name2),
					ChildJSON: name2,
					GoType:    "float64",
				}, nil
			}
		}
		return nil, fmt.Errorf("multi-step path not supported in this context (got %d steps)", len(n.Steps))

	case *jparse.NameNode:
		jsonName := n.Value
		fc.addField(jsonName, false)
		return &Expr{
			Kind:      "field",
			FieldName: exportedName(jsonName),
			FieldJSON: jsonName,
			GoType:    "any", // resolved later
		}, nil

	case *jparse.NumberNode:
		return &Expr{
			Kind:         "literal",
			LiteralValue: formatFloat(n.Value),
			GoType:       "float64",
		}, nil

	case *jparse.StringNode:
		return &Expr{
			Kind:         "literal",
			LiteralValue: fmt.Sprintf("%q", n.Value),
			GoType:       "string",
		}, nil

	case *jparse.BooleanNode:
		return &Expr{
			Kind:         "literal",
			LiteralValue: fmt.Sprintf("%t", n.Value),
			GoType:       "bool",
		}, nil

	case *jparse.NullNode:
		return &Expr{
			Kind:         "literal",
			LiteralValue: "nil",
			GoType:       "any",
		}, nil

	case *jparse.AssignmentNode:
		if _, exists := fc.varTypes[n.Name]; exists {
			return nil, fmt.Errorf("variable $%s already defined in this block", n.Name)
		}
		value, err := analyzeExpr(n.Value, fc)
		if err != nil {
			return nil, fmt.Errorf("assignment $%s: %w", n.Name, err)
		}
		fc.varTypes[n.Name] = value.GoType
		return &Expr{Kind: "assign", VarName: n.Name, Left: value, GoType: value.GoType}, nil

	case *jparse.VariableNode:
		goType, ok := fc.varTypes[n.Name]
		if !ok {
			return nil, fmt.Errorf("undefined variable $%s", n.Name)
		}
		return &Expr{Kind: "varRef", VarName: n.Name, GoType: goType}, nil

	case *jparse.BlockNode:
		// Parenthesized expression: (expr). Unwrap the single child.
		if len(n.Exprs) == 1 {
			return analyzeExpr(n.Exprs[0], fc)
		}
		return nil, fmt.Errorf("block with %d expressions not supported (only single-expression parentheses)", len(n.Exprs))

	case *jparse.ComparisonOperatorNode:
		left, err := analyzeExpr(n.LHS, fc)
		if err != nil {
			return nil, fmt.Errorf("comparison LHS: %w", err)
		}
		right, err := analyzeExpr(n.RHS, fc)
		if err != nil {
			return nil, fmt.Errorf("comparison RHS: %w", err)
		}
		// When the RHS is numeric, mark the LHS field as numeric so the
		// generated struct uses float64 and comparisons like > compile.
		if right.GoType == "float64" {
			markNumeric(left, fc)
		}
		return &Expr{
			Kind:   "binary",
			Op:     comparisonOpToGo(n.Type),
			Left:   left,
			Right:  right,
			GoType: "bool",
		}, nil

	case *jparse.BooleanOperatorNode:
		left, err := analyzeExpr(n.LHS, fc)
		if err != nil {
			return nil, fmt.Errorf("boolean LHS: %w", err)
		}
		right, err := analyzeExpr(n.RHS, fc)
		if err != nil {
			return nil, fmt.Errorf("boolean RHS: %w", err)
		}
		op := "&&"
		if n.Type == jparse.BooleanOr {
			op = "||"
		}
		return &Expr{
			Kind:   "binary",
			Op:     op,
			Left:   left,
			Right:  right,
			GoType: "bool",
		}, nil

	case *jparse.NumericOperatorNode:
		left, err := analyzeExpr(n.LHS, fc)
		if err != nil {
			return nil, fmt.Errorf("arithmetic LHS: %w", err)
		}
		right, err := analyzeExpr(n.RHS, fc)
		if err != nil {
			return nil, fmt.Errorf("arithmetic RHS: %w", err)
		}
		markNumeric(left, fc)
		markNumeric(right, fc)
		return &Expr{
			Kind:   "binary",
			Op:     numericOpToGo(n.Type),
			Left:   left,
			Right:  right,
			GoType: "float64",
		}, nil

	case *jparse.NegationNode:
		operand, err := analyzeExpr(n.RHS, fc)
		if err != nil {
			return nil, fmt.Errorf("negation: %w", err)
		}
		markNumeric(operand, fc)
		return &Expr{
			Kind:   "unary",
			Op:     "-",
			Left:   operand,
			GoType: "float64",
		}, nil

	case *jparse.StringConcatenationNode:
		left, err := analyzeExpr(n.LHS, fc)
		if err != nil {
			return nil, fmt.Errorf("concat LHS: %w", err)
		}
		right, err := analyzeExpr(n.RHS, fc)
		if err != nil {
			return nil, fmt.Errorf("concat RHS: %w", err)
		}
		return &Expr{
			Kind:   "binary",
			Op:     "&",
			Left:   left,
			Right:  right,
			GoType: "string",
		}, nil

	case *jparse.ConditionalNode:
		cond, err := analyzeExpr(n.If, fc)
		if err != nil {
			return nil, fmt.Errorf("conditional if: %w", err)
		}
		then, err := analyzeExpr(n.Then, fc)
		if err != nil {
			return nil, fmt.Errorf("conditional then: %w", err)
		}
		var els *Expr
		if n.Else != nil {
			els, err = analyzeExpr(n.Else, fc)
			if err != nil {
				return nil, fmt.Errorf("conditional else: %w", err)
			}
		}
		goType := then.GoType
		if els != nil && els.GoType != goType {
			goType = "any"
		}
		return &Expr{
			Kind:   "conditional",
			Cond:   cond,
			Then:   then,
			Else:   els,
			GoType: goType,
		}, nil

	case *jparse.FunctionCallNode:
		funcName, err := extractVariableName(n.Func)
		if err != nil {
			return nil, fmt.Errorf("function name: %w", err)
		}

		// For aggregate functions with a 2-part path argument, build an
		// arrayField Expr so codegen can emit the right loop pattern.
		if isAggregateFunc(funcName) && len(n.Args) == 1 {
			if p, ok := n.Args[0].(*jparse.PathNode); ok && len(p.Steps) == 2 {
				name1, err1 := extractName(p.Steps[0])
				name2, err2 := extractName(p.Steps[1])
				if err1 == nil && err2 == nil {
					fc.addArrayField(name1, name2, true)
					return &Expr{
						Kind:     "funcCall",
						FuncName: funcName,
						FuncArgs: []*Expr{{
							Kind:      "arrayField",
							ArrayName: exportedName(name1),
							ArrayJSON: name1,
							ChildName: exportedName(name2),
							ChildJSON: name2,
							GoType:    "float64",
						}},
						GoType: "float64",
					}, nil
				}
			}
		}

		// Build arguments recursively.
		var args []*Expr
		for _, arg := range n.Args {
			a, err := analyzeExpr(arg, fc)
			if err != nil {
				return nil, fmt.Errorf("$%s argument: %w", funcName, err)
			}
			args = append(args, a)
		}

		// Mark operands numeric for numeric functions.
		if isNumericFunc(funcName) {
			for _, a := range args {
				markNumeric(a, fc)
			}
		}

		return &Expr{
			Kind:     "funcCall",
			FuncName: funcName,
			FuncArgs: args,
			GoType:   inferFuncReturnType(funcName),
		}, nil

	default:
		return nil, fmt.Errorf("unsupported expression type %T", node)
	}
}

// markNumeric marks a field expression as used in numeric context so the
// generated input struct declares it as float64.
func markNumeric(e *Expr, fc *fieldCollector) {
	if e.Kind == "field" {
		fc.addField(e.FieldJSON, true)
	}
}

// resolveFieldTypes walks an expression tree and updates GoType for field
// references based on the final field collector state.
func resolveFieldTypes(e *Expr, fc *fieldCollector) {
	if e == nil {
		return
	}
	switch e.Kind {
	case "field":
		e.GoType = fc.typeOf(e.FieldJSON)
	case "varRef":
		if goType, ok := fc.varTypes[e.VarName]; ok {
			e.GoType = goType
		}
	case "assign":
		resolveFieldTypes(e.Left, fc)
		e.GoType = e.Left.GoType
		fc.varTypes[e.VarName] = e.GoType
	}
	resolveFieldTypes(e.Left, fc)
	resolveFieldTypes(e.Right, fc)
	for _, a := range e.FuncArgs {
		resolveFieldTypes(a, fc)
	}
	resolveFieldTypes(e.Cond, fc)
	resolveFieldTypes(e.Then, fc)
	resolveFieldTypes(e.Else, fc)
}

func numericOpToGo(op jparse.NumericOperator) string {
	switch op {
	case jparse.NumericAdd:
		return "+"
	case jparse.NumericSubtract:
		return "-"
	case jparse.NumericMultiply:
		return "*"
	case jparse.NumericDivide:
		return "/"
	case jparse.NumericModulo:
		return "%"
	default:
		return "+"
	}
}

func isAggregateFunc(name string) bool {
	switch name {
	case "sum", "count", "min", "max", "average":
		return true
	}
	return false
}

func isNumericFunc(name string) bool {
	switch name {
	case "number", "abs", "floor", "ceil", "round":
		return true
	}
	return false
}

func inferFuncReturnType(name string) string {
	switch name {
	case "sum", "count", "min", "max", "average",
		"number", "abs", "floor", "ceil", "round",
		"length":
		return "float64"
	case "string", "uppercase", "lowercase", "trim",
		"substring", "substringBefore", "substringAfter",
		"join", "type":
		return "string"
	case "boolean", "not", "exists", "contains":
		return "bool"
	default:
		return "any"
	}
}

// NeedsRuntime returns true if the plan uses any runtime.* helpers.
func (plan *QueryPlan) NeedsRuntime() bool {
	for _, f := range plan.Filters {
		if exprNeedsRuntime(f) {
			return true
		}
	}
	for _, b := range plan.Bindings {
		if exprNeedsRuntime(b) {
			return true
		}
	}
	for _, of := range plan.OutputFields {
		if exprNeedsRuntime(of.Value) {
			return true
		}
	}
	return false
}

// NeedsMath returns true if the plan uses math.* functions.
func (plan *QueryPlan) NeedsMath() bool {
	for _, f := range plan.Filters {
		if exprNeedsMath(f) {
			return true
		}
	}
	for _, b := range plan.Bindings {
		if exprNeedsMath(b) {
			return true
		}
	}
	for _, of := range plan.OutputFields {
		if exprNeedsMath(of.Value) {
			return true
		}
	}
	return false
}

func exprNeedsRuntime(e *Expr) bool {
	if e == nil {
		return false
	}
	// Aggregation functions that use runtime.Min/Max/AverageFloat64
	if e.Kind == "funcCall" && isAggregateFunc(e.FuncName) && e.FuncName != "sum" && e.FuncName != "count" {
		return true
	}
	// String/numeric/boolean/array/object functions
	if e.Kind == "funcCall" && !isAggregateFunc(e.FuncName) {
		return true
	}
	// String concatenation uses runtime.ToString
	if e.Kind == "binary" && e.Op == "&" {
		return true
	}
	// Conditional uses runtime.Truthy
	if e.Kind == "conditional" {
		return true
	}
	return exprNeedsRuntime(e.Left) || exprNeedsRuntime(e.Right) ||
		exprNeedsRuntime(e.Cond) || exprNeedsRuntime(e.Then) || exprNeedsRuntime(e.Else) ||
		anyExprNeedsRuntime(e.FuncArgs)
}

func anyExprNeedsRuntime(args []*Expr) bool {
	for _, a := range args {
		if exprNeedsRuntime(a) {
			return true
		}
	}
	return false
}

func exprNeedsMath(e *Expr) bool {
	if e == nil {
		return false
	}
	if e.Kind == "funcCall" {
		switch e.FuncName {
		case "abs", "floor", "ceil", "round":
			return true
		}
	}
	return exprNeedsMath(e.Left) || exprNeedsMath(e.Right) ||
		exprNeedsMath(e.Cond) || exprNeedsMath(e.Then) || exprNeedsMath(e.Else) ||
		anyExprNeedsMath(e.FuncArgs)
}

func anyExprNeedsMath(args []*Expr) bool {
	for _, a := range args {
		if exprNeedsMath(a) {
			return true
		}
	}
	return false
}

// --- Field collector ---
// Tracks all fields accessed from each array element and their inferred types.

type fieldCollector struct {
	fields   []collectedField
	numeric  map[string]bool   // jsonName -> whether it's used in numeric context
	varTypes map[string]string // variable name -> Go type (for := bindings)
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
