package transpiler

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/xiatechs/jsonata-go/jparse"
)

// QueryPlan is the intermediate representation extracted from a JSONata AST
// for filter+projection pipelines. It captures everything needed to generate
// Go code: which field to stream to, what to filter, and how to project the output.
// Used internally by fetchFilter mode: $fetch("provider", "endpoint")[filter].{proj}

// SortTerm describes one sort key for ^() order-by.
type SortTerm struct {
	FieldJSON  string // JSON field name (e.g., "price")
	GoName     string // Go struct field name (e.g., "Price")
	Descending bool   // true for >field (descending)
}

type QueryPlan struct {
	RootField      string        // top-level JSON field to navigate to (e.g., "orders")
	InputFields    []StructField // fields to deserialize from each array element
	Filters        []*Expr       // each entry is a predicate expression; all ANDed
	Bindings       []*Expr       // variable assignments ($x := expr) before the projection
	HasIndexFilter bool          // true if any filter is a numeric index ([0], [1], …)
	SortTerms      []SortTerm    // ^(field) order-by terms (empty = no sort)
	OutputName     string        // Go type name for output struct
	OutputFields   []OutputField // fields in the output struct
	FuncName       string        // generated Go function name

	// InputStructPrefix overrides the prefix used for the generated element
	// struct type name. When empty, RootField is used. Set by callers that
	// share a RootField across multiple services (e.g. fetchFilter plans where
	// two different services use the same endpoint name like "data").
	InputStructPrefix string
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

// Expr represents a value expression in a filter+projection pipeline.
// It is a recursive tree that models field access, literals, function calls,
// arithmetic, comparisons, boolean logic, string concatenation, conditionals,
// and variable bindings. Used internally by fetchFilter mode.
type Expr struct {
	Kind   string // "field","arrayField","literal","funcCall","binary","unary","conditional","assign","varRef","requestValue","error","response","arrayMap"
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

	// Kind="requestValue": reads from request context or child-service params.
	RequestSource string   // "header","cookie","query","param","path","method","body","serviceParam"
	RequestArg    string   // for header/cookie/query/param: key name
	RequestPath   []string // for body/serviceParam: trailing path segments

	// Kind="error": signals an HTTP error should be returned.
	StatusCode   int    // HTTP status code
	ErrorMessage string // Error message
	ErrorCode    string // Optional error code for programmatic handling

	// Kind="response": signals a full HTTP response should be returned.
	ResponseStatusCode int              // HTTP status code
	ResponseBodyExpr   *Expr            // Expression for response body
	ResponseHeaders    map[string]*Expr // Custom headers (optional)

	// Kind="lambda": user-defined function ($plot := function($x) { ... }).
	LambdaParams   []string // parameter names (without $)
	LambdaBindings []*Expr  // optional bindings inside the function body (block)
	LambdaBody     *Expr    // last expression / return value of the function

	// Kind="builtinRef": reference to a JSONata built-in function ($string, $number, …).
	// Used when the name appears as a variable (e.g. LHS of ~> chain).
	// FuncName holds the built-in name for codegen.
	// (FuncName is also used for Kind="funcCall")

	// Kind="lambdaCall": invoke a lambda with one argument. Left = lambda Expr, Right = arg Expr.
	// Used when composing lambdas in ~> chains.

	// Kind="arrayMap": array-map path (e.g. [a..b].(expr) or arr.f($)). Left = array/range expr, Right = per-element expr (rootRef $ = current element).
}

// analyzeFilterPipeline walks a parsed JSONata AST and produces a QueryPlan.
// It supports the pattern: rootField[predicate].{key: value, ...}
// This is an internal function used only by fetchFilter mode to analyze
// the filter and projection parts of $fetch("provider", "endpoint")[filter].{proj}
func analyzeFilterPipeline(root jparse.Node) (*QueryPlan, error) {
	path, ok := root.(*jparse.PathNode)
	if !ok {
		return nil, fmt.Errorf("expected a path expression at the top level, got %T", root)
	}

	if len(path.Steps) < 2 {
		return nil, fmt.Errorf("expected at least 2 path steps (source + projection), got %d", len(path.Steps))
	}

	// The first step can be a PredicateNode (items[filter]) or a SortNode
	// wrapping a PredicateNode (items[filter]^(price) or items^(price)[filter]).
	var pred *jparse.PredicateNode
	var sortTerms []SortTerm

	switch step := path.Steps[0].(type) {
	case *jparse.PredicateNode:
		// Check if the predicate wraps a SortNode: items^(price)[filter].
		if sn, ok := step.Expr.(*jparse.SortNode); ok {
			pred = &jparse.PredicateNode{Expr: sn.Expr, Filters: step.Filters}
			for _, t := range sn.Terms {
				st, err := extractSortTerm(t)
				if err != nil {
					return nil, fmt.Errorf("sort term: %w", err)
				}
				sortTerms = append(sortTerms, st)
			}
		} else {
			pred = step
		}
	case *jparse.SortNode:
		// items[filter]^(price) — sort wraps predicate.
		inner, ok := step.Expr.(*jparse.PathNode)
		if !ok {
			return nil, fmt.Errorf("expected path inside sort, got %T", step.Expr)
		}
		if len(inner.Steps) != 1 {
			return nil, fmt.Errorf("expected single step inside sort path, got %d", len(inner.Steps))
		}
		innerPred, ok := inner.Steps[0].(*jparse.PredicateNode)
		if !ok {
			return nil, fmt.Errorf("expected predicate inside sort, got %T", inner.Steps[0])
		}
		pred = innerPred
		for _, t := range step.Terms {
			st, err := extractSortTerm(t)
			if err != nil {
				return nil, fmt.Errorf("sort term: %w", err)
			}
			sortTerms = append(sortTerms, st)
		}
	default:
		return nil, fmt.Errorf("expected first step to be a predicate (array[filter]) or sort, got %T", path.Steps[0])
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
	hasIndexFilter := false
	for _, f := range pred.Filters {
		// Numeric filter [0], [1], … → index access instead of boolean predicate.
		if numNode, ok := f.(*jparse.NumberNode); ok {
			idx := int(numNode.Value)
			if idx < 0 {
				return nil, fmt.Errorf("negative array index [%d] not supported", idx)
			}
			hasIndexFilter = true
			filters = append(filters, &Expr{
				Kind:   "binary",
				Op:     "==",
				Left:   &Expr{Kind: "elemIndex", GoType: "int"},
				Right:  &Expr{Kind: "literal", LiteralValue: fmt.Sprintf("%d", idx), GoType: "int"},
				GoType: "bool",
			})
			continue
		}
		filter, err := analyzeExpr(f, fields)
		if err != nil {
			return nil, fmt.Errorf("analyzing filter: %w", err)
		}
		filters = append(filters, filter)
	}

	// Register sort fields as numeric input fields so the struct has them.
	for _, st := range sortTerms {
		fields.addField(st.FieldJSON, true)
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
		RootField:      rootField,
		InputFields:    inputFields,
		Filters:        filters,
		Bindings:       bindings,
		HasIndexFilter: hasIndexFilter,
		SortTerms:      sortTerms,
		OutputName:     exportedName(rootField) + "Result",
		OutputFields:   outputFields,
		FuncName:       funcName,
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
// analyzeBlockAsResult analyzes a node that may be a block (expr1; expr2; ...; last).
// Returns (bindings, result expr) for use inside lambda bodies. Single expressions
// return (nil, analyzed expr).
func analyzeBlockAsResult(node jparse.Node, fc *fieldCollector) ([]*Expr, *Expr, error) {
	block, ok := node.(*jparse.BlockNode)
	if !ok {
		expr, err := analyzeExpr(node, fc)
		return nil, expr, err
	}
	if len(block.Exprs) < 1 {
		return nil, nil, fmt.Errorf("empty block")
	}
	if len(block.Exprs) == 1 {
		expr, err := analyzeExpr(block.Exprs[0], fc)
		return nil, expr, err
	}
	var bindings []*Expr
	for _, bNode := range block.Exprs[:len(block.Exprs)-1] {
		b, err := analyzeExpr(bNode, fc)
		if err != nil {
			return nil, nil, err
		}
		if b.Kind != "assign" {
			return nil, nil, fmt.Errorf("expected variable binding in block, got %s", b.Kind)
		}
		bindings = append(bindings, b)
	}
	last, err := analyzeExpr(block.Exprs[len(block.Exprs)-1], fc)
	if err != nil {
		return nil, nil, err
	}
	return bindings, last, nil
}

// isArrayLikeExpr returns true if e represents an array or range (suitable as LHS of array-map path).
func isArrayLikeExpr(e *Expr) bool {
	if e == nil {
		return false
	}
	if e.Kind == "array" {
		return true
	}
	return e.Kind == "funcCall" && e.FuncName == "_range"
}

// analyzeExpr recursively walks a JSONata AST node and produces an Expr tree.
// It handles field references, literals, comparisons, boolean operators,
// arithmetic, negation, string concatenation, conditionals, and function calls.

func analyzeExpr(node jparse.Node, fc *fieldCollector) (*Expr, error) {
	switch n := node.(type) {
	case *jparse.PredicateNode:
		// PredicateNode represents expr[filter], e.g., $fetch(...)[filter]
		// When used as a function argument, we need to analyze the base expression
		// and the filters. For now, we'll analyze the base expression and note
		// that filters are present (they'll be evaluated at runtime).
		baseExpr, err := analyzeExpr(n.Expr, fc)
		if err != nil {
			return nil, fmt.Errorf("predicate base expression: %w", err)
		}
		// Analyze filters to collect any field references
		for _, filter := range n.Filters {
			if _, err := analyzeExpr(filter, fc); err != nil {
				// If filter analysis fails, we still continue but note it
				// The filter will be evaluated at runtime
			}
		}
		// Return the base expression - filters will be applied at runtime
		// This is a simplification; in a full implementation, we'd need to
		// represent the filtered expression more explicitly
		return baseExpr, nil

	case *jparse.PathNode:
		if len(n.Steps) > 0 {
			if fnCall, ok := n.Steps[0].(*jparse.FunctionCallNode); ok {
				var trailingPath []string
				for _, step := range n.Steps[1:] {
					name, ok := step.(*jparse.NameNode)
					if !ok {
						return nil, fmt.Errorf("expected name in path after function call, got %T", step)
					}
					trailingPath = append(trailingPath, name.Value)
				}
				return analyzeContextExprCall(fnCall, trailingPath)
			}
		}
		if len(n.Steps) == 1 {
			return analyzeExpr(n.Steps[0], fc)
		}
		if len(n.Steps) >= 2 {
			base, err := analyzeExpr(n.Steps[0], fc)
			if err != nil {
				return nil, err
			}
			if isArrayLikeExpr(base) {
				for i := 1; i < len(n.Steps); i++ {
					body, err := analyzeExpr(n.Steps[i], fc)
					if err != nil {
						return nil, fmt.Errorf("array-map step %d: %w", i, err)
					}
					base = &Expr{Kind: "arrayMap", Left: base, Right: body, GoType: "any"}
				}
				return base, nil
			}
		}
		// Multi-step path: if exactly 2 steps and both are names, treat as
		// a field.subfield reference (used in $sum(items.price) arguments).
		if len(n.Steps) == 2 {
			// field.* — wildcard: all values of an object field.
			if _, isWild := n.Steps[1].(*jparse.WildcardNode); isWild {
				name, err := extractName(n.Steps[0])
				if err != nil {
					return nil, fmt.Errorf("wildcard path: %w", err)
				}
				fc.addField(name, false)
				return &Expr{
					Kind:     "funcCall",
					FuncName: "_wildcard",
					FuncArgs: []*Expr{{
						Kind:      "field",
						FieldName: exportedName(name),
						FieldJSON: name,
						GoType:    "any",
					}},
					GoType: "any",
				}, nil
			}

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
		// $ bare variable = root context reference.
		if n.Name == "" {
			return &Expr{Kind: "rootRef", GoType: "any"}, nil
		}
		goType, ok := fc.varTypes[n.Name]
		if ok {
			return &Expr{Kind: "varRef", VarName: n.Name, GoType: goType}, nil
		}
		// Allow built-in function names (e.g. $string in $string ~> $substringBefore(?, '.')).
		if isBuiltinFunc(n.Name) {
			return &Expr{Kind: "builtinRef", FuncName: n.Name, GoType: "any"}, nil
		}
		return nil, fmt.Errorf("undefined variable $%s", n.Name)

	case *jparse.ArrayNode:
		// [start..end] — jparse wraps a RangeNode in a single-item ArrayNode.
		// Unwrap so we don't double-nest the result.
		if len(n.Items) == 1 {
			if _, isRange := n.Items[0].(*jparse.RangeNode); isRange {
				return analyzeExpr(n.Items[0], fc)
			}
		}
		items := make([]*Expr, len(n.Items))
		for i, item := range n.Items {
			analyzed, err := analyzeExpr(item, fc)
			if err != nil {
				return nil, fmt.Errorf("array item %d: %w", i, err)
			}
			items[i] = analyzed
		}
		return &Expr{Kind: "array", FuncArgs: items, GoType: "any"}, nil

	case *jparse.RangeNode:
		left, err := analyzeExpr(n.LHS, fc)
		if err != nil {
			return nil, fmt.Errorf("range LHS: %w", err)
		}
		right, err := analyzeExpr(n.RHS, fc)
		if err != nil {
			return nil, fmt.Errorf("range RHS: %w", err)
		}
		return &Expr{Kind: "funcCall", FuncName: "_range", FuncArgs: []*Expr{left, right}, GoType: "any"}, nil

	case *jparse.FunctionApplicationNode:
		lhs, err := analyzeExpr(n.LHS, fc)
		if err != nil {
			return nil, fmt.Errorf("chain LHS: %w", err)
		}
		// When LHS is a function (builtinRef or lambda), this is function composition:
		// $string ~> $substringBefore(?, '.') means x -> substringBefore(string(x), '.').
		ctxArg := &Expr{Kind: "varRef", VarName: "_x", GoType: "any"}
		var firstArg *Expr
		switch lhs.Kind {
		case "builtinRef":
			firstArg = &Expr{Kind: "funcCall", FuncName: lhs.FuncName, FuncArgs: []*Expr{ctxArg}, GoType: "any"}
		case "lambda":
			firstArg = invokeLambda(lhs, ctxArg)
		default:
			firstArg = lhs
		}
		var body *Expr
		switch rhs := n.RHS.(type) {
		case *jparse.VariableNode:
			switch lhs.Kind {
			case "builtinRef":
				body = &Expr{
					Kind:     "funcCall",
					FuncName: rhs.Name,
					FuncArgs: []*Expr{{Kind: "funcCall", FuncName: lhs.FuncName, FuncArgs: []*Expr{ctxArg}, GoType: "any"}},
					GoType:   inferFuncReturnType(rhs.Name),
				}
			case "lambda":
				body = &Expr{
					Kind:     "funcCall",
					FuncName: rhs.Name,
					FuncArgs: []*Expr{invokeLambda(lhs, ctxArg)},
					GoType:   inferFuncReturnType(rhs.Name),
				}
			default:
				body = &Expr{
					Kind:     "funcCall",
					FuncName: rhs.Name,
					FuncArgs: []*Expr{lhs},
					GoType:   inferFuncReturnType(rhs.Name),
				}
			}
		case *jparse.FunctionCallNode:
			fname, err := extractVariableName(rhs.Func)
			if err != nil {
				return nil, fmt.Errorf("chain function name: %w", err)
			}
			args := []*Expr{firstArg}
			for _, a := range rhs.Args {
				arg, err := analyzeExpr(a, fc)
				if err != nil {
					return nil, fmt.Errorf("chain $%s argument: %w", fname, err)
				}
				args = append(args, arg)
			}
			body = &Expr{Kind: "funcCall", FuncName: fname, FuncArgs: args, GoType: inferFuncReturnType(fname)}
		case *jparse.PartialNode:
			fname, err := extractVariableName(rhs.Func)
			if err != nil {
				return nil, fmt.Errorf("chain partial function name: %w", err)
			}
			var args []*Expr
			for _, a := range rhs.Args {
				if _, isPlaceholder := a.(*jparse.PlaceholderNode); isPlaceholder {
					args = append(args, firstArg)
				} else {
					arg, err := analyzeExpr(a, fc)
					if err != nil {
						return nil, fmt.Errorf("chain partial $%s argument: %w", fname, err)
					}
					args = append(args, arg)
				}
			}
			body = &Expr{Kind: "funcCall", FuncName: fname, FuncArgs: args, GoType: inferFuncReturnType(fname)}
		default:
			return nil, fmt.Errorf("unsupported ~> RHS type %T", n.RHS)
		}
		if body != nil && (lhs.Kind == "builtinRef" || lhs.Kind == "lambda") {
			return &Expr{
				Kind:         "lambda",
				LambdaParams: []string{"_x"},
				LambdaBody:   body,
				GoType:       body.GoType,
			}, nil
		}
		return body, nil

	case *jparse.WildcardNode:
		return nil, fmt.Errorf("wildcard (*) must appear as a path step, not standalone")

	case *jparse.LambdaNode:
		lambdaFC := &fieldCollector{numeric: map[string]bool{}, varTypes: map[string]string{}}
		for k, v := range fc.varTypes {
			lambdaFC.varTypes[k] = v
		}
		for _, p := range n.ParamNames {
			lambdaFC.varTypes[p] = "any"
		}
		bindings, body, err := analyzeBlockAsResult(n.Body, lambdaFC)
		if err != nil {
			return nil, fmt.Errorf("lambda body: %w", err)
		}
		return &Expr{
			Kind:           "lambda",
			LambdaParams:   n.ParamNames,
			LambdaBindings: bindings,
			LambdaBody:     body,
			GoType:         "any",
		}, nil

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
		// "in" membership is not a binary Go operator — map to runtime.In().
		if n.Type == jparse.ComparisonIn {
			return &Expr{
				Kind:     "funcCall",
				FuncName: "_in",
				FuncArgs: []*Expr{left, right},
				GoType:   "bool",
			}, nil
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

	case *jparse.ObjectNode:
		// Object literals in expressions (e.g., $httpResponse body) are converted
		// to a map[string]any expression that will be JSON-marshaled at runtime.
		pairs := make([]*Expr, 0, len(n.Pairs)*2)
		for _, pair := range n.Pairs {
			// Extract key as string literal
			keyName, err := extractSingleName(pair[0])
			if err != nil {
				return nil, fmt.Errorf("object key: %w", err)
			}
			keyExpr := &Expr{
				Kind:         "literal",
				LiteralValue: fmt.Sprintf("%q", keyName),
				GoType:       "string",
			}
			value, err := analyzeExpr(pair[1], fc)
			if err != nil {
				return nil, fmt.Errorf("object value: %w", err)
			}
			pairs = append(pairs, keyExpr, value)
		}
		return &Expr{
			Kind:     "object",
			FuncArgs: pairs,
			GoType:   "any",
		}, nil

	case *jparse.FunctionCallNode:
		funcName, err := extractVariableName(n.Func)
		if err != nil {
			return nil, fmt.Errorf("function name: %w", err)
		}

		if funcName == "request" || funcName == "params" {
			return analyzeContextExprCall(n, nil)
		}

		// Handle $httpError() function
		if funcName == "httpError" {
			if len(n.Args) < 2 || len(n.Args) > 3 {
				return nil, fmt.Errorf("$httpError() requires 2 or 3 arguments (statusCode, message, code?), got %d", len(n.Args))
			}
			statusCodeNode, ok := n.Args[0].(*jparse.NumberNode)
			if !ok {
				return nil, fmt.Errorf("$httpError() first argument must be a number (status code), got %T", n.Args[0])
			}
			messageNode, ok := n.Args[1].(*jparse.StringNode)
			if !ok {
				return nil, fmt.Errorf("$httpError() second argument must be a string (message), got %T", n.Args[1])
			}
			errorCode := ""
			if len(n.Args) == 3 {
				codeNode, ok := n.Args[2].(*jparse.StringNode)
				if !ok {
					return nil, fmt.Errorf("$httpError() third argument must be a string (error code), got %T", n.Args[2])
				}
				errorCode = codeNode.Value
			}
			return &Expr{
				Kind:         "error",
				StatusCode:   int(statusCodeNode.Value),
				ErrorMessage: messageNode.Value,
				ErrorCode:    errorCode,
				GoType:       "any",
			}, nil
		}

		// Handle $httpResponse() function
		if funcName == "httpResponse" {
			if len(n.Args) < 2 || len(n.Args) > 3 {
				return nil, fmt.Errorf("$httpResponse() requires 2 or 3 arguments (statusCode, body, headers?), got %d", len(n.Args))
			}
			statusCodeNode, ok := n.Args[0].(*jparse.NumberNode)
			if !ok {
				return nil, fmt.Errorf("$httpResponse() first argument must be a number (status code), got %T", n.Args[0])
			}
			bodyExpr, err := analyzeExpr(n.Args[1], fc)
			if err != nil {
				return nil, fmt.Errorf("$httpResponse() body: %w", err)
			}
			expr := &Expr{
				Kind:               "response",
				ResponseStatusCode: int(statusCodeNode.Value),
				ResponseBodyExpr:   bodyExpr,
				GoType:             "any",
			}
			if len(n.Args) == 3 {
				headersObj, ok := n.Args[2].(*jparse.ObjectNode)
				if !ok {
					return nil, fmt.Errorf("$httpResponse() third argument must be an object (headers), got %T", n.Args[2])
				}
				headers := make(map[string]*Expr)
				for _, pair := range headersObj.Pairs {
					key, err := extractSingleName(pair[0])
					if err != nil {
						return nil, fmt.Errorf("$httpResponse() header key: %w", err)
					}
					valueExpr, err := analyzeExpr(pair[1], fc)
					if err != nil {
						return nil, fmt.Errorf("$httpResponse() header %q value: %w", key, err)
					}
					headers[key] = valueExpr
				}
				expr.ResponseHeaders = headers
			}
			return expr, nil
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

// invokeLambda returns an Expr that represents calling the lambda with the given argument.
func invokeLambda(lambda *Expr, arg *Expr) *Expr {
	return &Expr{Kind: "lambdaCall", Left: lambda, Right: arg, GoType: lambda.GoType}
}

// isBuiltinFunc returns true for JSONata built-in function names so they can
// be used as variables (e.g. $string in $string ~> $substringBefore(?, '.')).
func isBuiltinFunc(name string) bool {
	switch name {
	case "string", "length", "substring", "substringBefore", "substringAfter",
		"uppercase", "lowercase", "trim", "contains", "join", "pad", "split",
		"number", "abs", "floor", "ceil", "round", "power", "sqrt", "random",
		"boolean", "not", "exists",
		"sort", "reverse", "append", "distinct", "shuffle", "zip",
		"keys", "merge", "type", "values", "spread",
		"now", "millis", "fromMillis", "toMillis",
		"sum", "count", "min", "max", "average", "reduce":
		return true
	}
	return false
}

func isNumericFunc(name string) bool {
	switch name {
	case "number", "abs", "floor", "ceil", "round", "power", "sqrt":
		return true
	}
	return false
}

func inferFuncReturnType(name string) string {
	switch name {
	case "sum", "count", "min", "max", "average",
		"number", "abs", "floor", "ceil", "round",
		"power", "sqrt", "random", "length", "millis", "toMillis":
		return "float64"
	case "string", "uppercase", "lowercase", "trim",
		"substring", "substringBefore", "substringAfter",
		"join", "pad", "type", "now", "fromMillis":
		return "string"
	case "split", "shuffle", "zip", "values", "spread":
		return "any"
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

// NeedsSlices returns true if the plan uses slices.SortFunc (for ^() order-by).
func (plan *QueryPlan) NeedsSlices() bool {
	return len(plan.SortTerms) > 0
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
	Fields       []ProviderField    // output fields in order (unused when RootExpr is set)
	Deps         []ProviderDepEntry // unique provider+endpoint pairs needed
	Services     []string           // unique service names referenced via $service()
	ServiceCalls []ServiceCall      // service invocations in evaluation order
	NeedsRequest bool               // true if any field or fetch config uses request functions

	// When set, the response body is the value of RootExpr (after evaluating RootBindings), not an object built from Fields.
	RootExpr     *Expr   // single expression whose value is the entire response body
	RootBindings []*Expr // variable bindings evaluated in order before RootExpr (assign expressions)

	// When set, the response body is an array from mapping a projection over [StartExpr..EndExpr] (range + array map pattern).
	RangeMap *RangeMapPlan
}

// RangeMapPlan is the plan for top-level [a..b].{key: value}. Response is a JSON array of objects.
type RangeMapPlan struct {
	StartExpr    *Expr         // range start (e.g. literal 0 or variable)
	EndExpr      *Expr         // range end (e.g. literal 24 or variable)
	OutputFields []OutputField // projection; expressions may use rootRef ($) for current element
}

// ServiceCall describes one $service(...) invocation. ResultKey is unique per
// call so repeated child-service invocations do not collide in the results map.
type ServiceCall struct {
	ServiceName string
	ResultKey   string
	ParamsExpr  *Expr
}

// ProviderField describes one key in the output JSON object. Kind determines
// which group of fields is populated — readers only need to look at Kind and
// the corresponding group.
type ProviderField struct {
	OutputKey string
	Kind      string // "fetch", "fetchFilter", "service", "header", "cookie", "query", "param", "path", "method", "body", "serviceParam", "static", "error", "response"

	// Kind="fetch": value comes from a pre-fetched upstream response.
	Provider    string
	Endpoint    string
	JSONPath    []string
	FetchConfig *FetchConfig // nil when $fetch() has only 2 args

	// Kind="fetchFilter": fetch from upstream then stream-filter+project the result.
	// Provider and Endpoint identify the upstream. FilterPlan holds the pipeline.
	// The generated Execute function returns a JSON array (not wrapped in an object).
	FilterPlan *QueryPlan

	// Kind="service": value comes from another generated transform pipeline.
	// JSONPath is reused for the path into the service result.
	ServiceName       string
	ServiceResultKey  string
	ServiceParamsExpr *Expr

	// Kind="header"/"cookie"/"query"/"param": value from $request().headers.X etc.
	Arg string

	// Kind="body": value extracted from $request().body via path navigation.
	BodyPath []string // empty = entire body

	// Kind="serviceParam": value extracted from $params() via path navigation.
	ParamsPath []string // empty = entire params object

	// Kind="static": a literal string value.
	StaticValue string

	// Kind="error": signals an HTTP error should be returned.
	StatusCode   int    // HTTP status code
	ErrorMessage string // Error message
	ErrorCode    string // Optional error code for programmatic handling

	// Kind="response": signals a full HTTP response should be returned.
	BodyExpr *Expr            // Expression for response body
	Headers  map[string]*Expr // Custom headers (optional)

	// Kind="expr": complex expression (conditionals, etc.) that needs evaluation
	ValueExpr *Expr // The expression to evaluate
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
// a static string, a $request() path, or a $params() path.
type ConfigValue struct {
	Kind   string   // "static", "header", "cookie", "query", "param", "path", "method", "body", "serviceParam"
	Arg    string   // for header/cookie/query/param: the key name from the path
	Path   []string // for body/serviceParam: path segments after $request().body or $params()
	Static string   // for static: the literal value
}

// ProviderDepEntry is a unique provider+endpoint pair.
type ProviderDepEntry struct {
	Provider string
	Endpoint string
}

// RequestFieldSet describes which incoming request fields a transform needs.
// This is used by the codegen to emit a targeted extraction var so the route
// handler only copies the necessary headers/cookies instead of everything.
type RequestFieldSet struct {
	Headers    []string
	Cookies    []string
	Query      []string
	Params     []string
	NeedPath   bool
	NeedMethod bool
	NeedBody   bool
}

func (rf RequestFieldSet) IsEmpty() bool {
	return len(rf.Headers) == 0 && len(rf.Cookies) == 0 &&
		len(rf.Query) == 0 && len(rf.Params) == 0 &&
		!rf.NeedPath && !rf.NeedMethod && !rf.NeedBody
}

// RequestFields returns the unique set of request context keys that this plan
// needs, grouped by kind (headers, cookies, query, params) plus booleans for
// path, method, and body. This lets the route handler extract only the needed
// values from the incoming request instead of copying all headers/cookies.
func (p *ProviderPlan) RequestFields() RequestFieldSet {
	var rf RequestFieldSet
	seen := map[string]bool{}

	addKey := func(kind, arg string) {
		key := kind + ":" + arg
		if seen[key] {
			return
		}
		seen[key] = true
		switch kind {
		case "header":
			rf.Headers = append(rf.Headers, arg)
		case "cookie":
			rf.Cookies = append(rf.Cookies, arg)
		case "query":
			rf.Query = append(rf.Query, arg)
		case "param":
			rf.Params = append(rf.Params, arg)
		case "path":
			rf.NeedPath = true
		case "method":
			rf.NeedMethod = true
		case "body":
			rf.NeedBody = true
		}
	}

	for _, f := range p.Fields {
		switch f.Kind {
		case "header", "cookie", "query", "param":
			addKey(f.Kind, f.Arg)
		case "path":
			addKey("path", "")
		case "method":
			addKey("method", "")
		case "body":
			addKey("body", "")
		}

		// Also scan fetch configs for request field references.
		if f.FetchConfig != nil {
			for _, h := range f.FetchConfig.Headers {
				if h.Value.Kind != "static" {
					addKey(h.Value.Kind, h.Value.Arg)
				}
			}
			for _, b := range f.FetchConfig.Body {
				if b.Value.Kind != "static" {
					addKey(b.Value.Kind, b.Value.Arg)
				}
			}
		}

		if f.ServiceParamsExpr != nil {
			collectRequestRefsFromExpr(f.ServiceParamsExpr, addKey)
		}
		if f.ValueExpr != nil {
			collectRequestRefsFromExpr(f.ValueExpr, addKey)
		}
	}

	for _, b := range p.RootBindings {
		collectRequestRefsFromExpr(b, addKey)
	}
	collectRequestRefsFromExpr(p.RootExpr, addKey)
	if p.RangeMap != nil {
		collectRequestRefsFromExpr(p.RangeMap.StartExpr, addKey)
		collectRequestRefsFromExpr(p.RangeMap.EndExpr, addKey)
		for _, out := range p.RangeMap.OutputFields {
			collectRequestRefsFromExpr(out.Value, addKey)
		}
	}

	return rf
}

// AnalyzeFetchCalls walks a JSONata AST that uses $fetch() and/or $request()
// calls, producing a ProviderPlan. It handles two top-level forms:
//
// 1. Object literal — each value is one of:
//   - $fetch("provider", "endpoint").path             → Kind="fetch"
//   - $fetch("provider", "endpoint", {config}).path   → Kind="fetch" + FetchConfig
//   - $request().headers.Name                         → Kind="header"
//   - $request().cookies.Name                         → Kind="cookie"
//   - $request().query.Name                           → Kind="query"
//   - $request().params.Name                          → Kind="param"
//   - $request().path                                 → Kind="path"
//   - $request().method                               → Kind="method"
//   - $request().body.field                            → Kind="body"
//
// 2. Fetch-filter pipeline:
//   - $fetch("provider", "endpoint")[filter].{proj}   → Kind="fetchFilter"
//     The Execute function returns a JSON array directly.
func AnalyzeFetchCalls(root jparse.Node, funcName string) (*ProviderPlan, error) {
	// Handle top-level $httpError() call
	if fnCall, ok := root.(*jparse.FunctionCallNode); ok {
		if v, ok := fnCall.Func.(*jparse.VariableNode); ok && v.Name == "httpError" {
			field, err := analyzeErrorFn(fnCall)
			if err != nil {
				return nil, err
			}
			return &ProviderPlan{
				FuncName: funcName,
				Fields:   []ProviderField{field},
			}, nil
		}
		// Handle top-level $httpResponse() call
		if v, ok := fnCall.Func.(*jparse.VariableNode); ok && v.Name == "httpResponse" {
			fc := &fieldCollector{numeric: map[string]bool{}, varTypes: map[string]string{}}
			field, err := analyzeResponseFn(fnCall, fc)
			if err != nil {
				return nil, err
			}
			return &ProviderPlan{
				FuncName: funcName,
				Fields:   []ProviderField{field},
			}, nil
		}
	}

	// Detect the $fetch(p,e)[filter].{projection} top-level pattern before
	// attempting to unwrap an object literal.
	if field, ok := tryAnalyzeFetchFilter(root, funcName); ok {
		plan := &ProviderPlan{
			FuncName: funcName,
			Fields:   []ProviderField{field},
			Deps: []ProviderDepEntry{{
				Provider: field.Provider,
				Endpoint: field.Endpoint,
			}},
		}
		return plan, nil
	}

	// Detect [a..b].{key: value} (range + array map) top-level pattern.
	if plan, ok := tryAnalyzeRangeMap(root, funcName); ok {
		return plan, nil
	}

	// Detect completely bare $fetch() generator expressions without any transformation.
	// Field access (e.g., $fetch(...).field) is considered transformation and is allowed.
	// Only completely bare $fetch(...) calls should use a proxy instead.
	if isBareFetchCall(root) {
		return nil, fmt.Errorf("generator expression with only $fetch() and no transformation (field access, filter, or projection) is not allowed; use a proxy route in proxies.yaml instead")
	}

	// Handle top-level conditional expressions (e.g., condition ? $httpError(...) : ...)
	if _, ok := root.(*jparse.ConditionalNode); ok {
		fc := &fieldCollector{numeric: map[string]bool{}, varTypes: map[string]string{}}
		condExpr, err := analyzeExpr(root, fc)
		if err != nil {
			return nil, fmt.Errorf("conditional expression: %w", err)
		}
		field := ProviderField{
			Kind:      "expr",
			ValueExpr: condExpr,
		}
		return &ProviderPlan{
			FuncName: funcName,
			Fields:   []ProviderField{field},
		}, nil
	}

	// Empty block is invalid.
	if block, ok := root.(*jparse.BlockNode); ok && len(block.Exprs) == 0 {
		return nil, fmt.Errorf("empty block (no expressions)")
	}

	obj := unwrapObject(root)
	if obj == nil {
		// Top-level is a block whose last expression is not an object, or a non-object root (array, primitive, path).
		return buildRootExprPlan(root, funcName)
	}

	plan := &ProviderPlan{FuncName: funcName}
	seen := map[string]bool{}
	serviceCount := map[string]int{}

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
			serviceCount[field.ServiceName]++
			field.ServiceResultKey = serviceResultKey(field.ServiceName, serviceCount[field.ServiceName])
			svcKey := "$service." + field.ServiceName
			if !seen[svcKey] {
				seen[svcKey] = true
				plan.Services = append(plan.Services, field.ServiceName)
			}
			plan.ServiceCalls = append(plan.ServiceCalls, ServiceCall{
				ServiceName: field.ServiceName,
				ResultKey:   field.ServiceResultKey,
				ParamsExpr:  field.ServiceParamsExpr,
			})
		}

		plan.Fields = append(plan.Fields, field)
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
			if v.Name == "fetch" || v.Name == "request" || v.Name == "params" || v.Name == "service" || v.Name == "httpError" || v.Name == "httpResponse" {
				return true
			}
		}
	}
	return false
}

// isBareFetchCall detects if the root is a completely bare $fetch() generator expression
// without any transformation (no field access, filter, or projection). Such expressions
// should use a proxy route instead.
//
// Field access (e.g., $fetch(...).field) is considered transformation, so it's allowed.
// Only completely bare $fetch(...) calls are blocked.
func isBareFetchCall(root jparse.Node) bool {
	// Check if root is a completely bare $fetch() function call with no path navigation
	if fnCall, ok := root.(*jparse.FunctionCallNode); ok {
		if v, ok := fnCall.Func.(*jparse.VariableNode); ok && v.Name == "fetch" {
			return true
		}
	}

	// If it's a PathNode, it means there's some navigation (field access, filter, etc.)
	// which counts as transformation, so it's not bare.
	return false
}

// unwrapObject extracts an ObjectNode from the root, handling the case where
// jparse wraps it in a PathNode or a BlockNode whose last expression is an object.
func unwrapObject(root jparse.Node) *jparse.ObjectNode {
	if obj, ok := root.(*jparse.ObjectNode); ok {
		return obj
	}
	if p, ok := root.(*jparse.PathNode); ok && len(p.Steps) == 1 {
		if obj, ok := p.Steps[0].(*jparse.ObjectNode); ok {
			return obj
		}
	}
	if block, ok := root.(*jparse.BlockNode); ok && len(block.Exprs) >= 1 {
		last := block.Exprs[len(block.Exprs)-1]
		if obj, ok := last.(*jparse.ObjectNode); ok {
			return obj
		}
	}
	return nil
}

// buildRootExprPlan builds a ProviderPlan for a top-level expression whose result is
// a single value (array, primitive, or any non-object). Root may be a BlockNode
// (bindings + last expr) or a single expression node.
func buildRootExprPlan(root jparse.Node, funcName string) (*ProviderPlan, error) {
	var bindingNodes []jparse.Node
	var lastExpr jparse.Node

	if block, ok := root.(*jparse.BlockNode); ok {
		bindingNodes = block.Exprs[:len(block.Exprs)-1]
		lastExpr = block.Exprs[len(block.Exprs)-1]
	} else {
		lastExpr = root
	}

	fc := &fieldCollector{numeric: map[string]bool{}, varTypes: map[string]string{}}
	var rootBindings []*Expr

	for _, bNode := range bindingNodes {
		binding, err := analyzeExpr(bNode, fc)
		if err != nil {
			return nil, fmt.Errorf("analyzing variable binding: %w", err)
		}
		if binding.Kind != "assign" {
			return nil, fmt.Errorf("expected variable binding (:=) in block, got %s", binding.Kind)
		}
		rootBindings = append(rootBindings, binding)
	}

	rootExpr, err := analyzeExpr(lastExpr, fc)
	if err != nil {
		return nil, fmt.Errorf("analyzing root expression: %w", err)
	}

	plan := &ProviderPlan{
		FuncName:     funcName,
		RootExpr:     rootExpr,
		RootBindings: rootBindings,
	}
	collectPlanRefsFromAST(plan, root)
	return plan, nil
}

// collectPlanRefsFromAST walks the jparse AST and populates plan.Deps, plan.Services,
// and plan.NeedsRequest for root-expr plans (so Execute and request extraction work).
func collectPlanRefsFromAST(plan *ProviderPlan, node jparse.Node) {
	seenDep := map[string]bool{}
	seenSvc := map[string]bool{}
	serviceCount := map[string]int{}

	var walk func(jparse.Node)
	walk = func(n jparse.Node) {
		if n == nil {
			return
		}
		if fnCall, ok := n.(*jparse.FunctionCallNode); ok {
			if v, ok := fnCall.Func.(*jparse.VariableNode); ok {
				switch v.Name {
				case "fetch":
					if len(fnCall.Args) >= 2 {
						if p, ok := fnCall.Args[0].(*jparse.StringNode); ok && p != nil {
							if e, ok := fnCall.Args[1].(*jparse.StringNode); ok && e != nil {
								key := p.Value + "." + e.Value
								if !seenDep[key] {
									seenDep[key] = true
									plan.Deps = append(plan.Deps, ProviderDepEntry{Provider: p.Value, Endpoint: e.Value})
								}
							}
						}
					}
				case "service":
					if len(fnCall.Args) >= 1 {
						if s, ok := fnCall.Args[0].(*jparse.StringNode); ok && s != nil {
							key := "$service." + s.Value
							if !seenSvc[key] {
								seenSvc[key] = true
								plan.Services = append(plan.Services, s.Value)
							}
							serviceCount[s.Value]++
							call := ServiceCall{
								ServiceName: s.Value,
								ResultKey:   serviceResultKey(s.Value, serviceCount[s.Value]),
							}
							if len(fnCall.Args) == 2 {
								fc := &fieldCollector{numeric: map[string]bool{}, varTypes: map[string]string{}}
								paramsExpr, err := analyzeExpr(fnCall.Args[1], fc)
								if err == nil {
									call.ParamsExpr = paramsExpr
								}
							}
							plan.ServiceCalls = append(plan.ServiceCalls, call)
						}
					}
				case "request":
					plan.NeedsRequest = true
				}
			}
		}
		switch n := n.(type) {
		case *jparse.PathNode:
			for _, step := range n.Steps {
				walk(step)
			}
		case *jparse.BlockNode:
			for _, e := range n.Exprs {
				walk(e)
			}
		case *jparse.ObjectNode:
			for _, pair := range n.Pairs {
				walk(pair[0])
				walk(pair[1])
			}
		case *jparse.FunctionCallNode:
			walk(n.Func)
			for _, arg := range n.Args {
				walk(arg)
			}
		case *jparse.ConditionalNode:
			walk(n.If)
			walk(n.Then)
			walk(n.Else)
		case *jparse.PredicateNode:
			walk(n.Expr)
			for _, f := range n.Filters {
				walk(f)
			}
		case *jparse.AssignmentNode:
			walk(n.Value)
		case *jparse.FunctionApplicationNode:
			walk(n.LHS)
			walk(n.RHS)
		case *jparse.ArrayNode:
			for _, item := range n.Items {
				walk(item)
			}
		case *jparse.LambdaNode:
			walk(n.Body)
		case *jparse.SortNode:
			walk(n.Expr)
			for _, t := range n.Terms {
				walk(t.Expr)
			}
		case *jparse.PartialNode:
			walk(n.Func)
			for _, a := range n.Args {
				walk(a)
			}
		}
	}
	walk(node)
}

// tryAnalyzeFetchFilter detects the $fetch(provider, endpoint)[filter].{proj}
// top-level pattern. If found it returns a ProviderField with Kind="fetchFilter"
// and ok=true. Otherwise it returns ok=false and the caller falls through to the
// standard object-literal path.
//
// jparse represents "$fetch(p,e)[filter].{proj}" as a PathNode with two steps:
//   - Step 0: PredicateNode{Expr: FunctionCallNode($fetch), Filters: [...]}
//   - Step 1: ObjectNode{...}
//
// To reuse the existing Analyze logic, we substitute a NameNode(endpoint) for
// the $fetch call. This tells Analyze to expect the upstream response to be a
// JSON object with the endpoint name as the root key — e.g. $fetch("svc", "data")
// expects the upstream to return {"data": [...]}.
func tryAnalyzeFetchFilter(root jparse.Node, funcName string) (ProviderField, bool) {
	path, ok := root.(*jparse.PathNode)
	if !ok || len(path.Steps) < 2 {
		return ProviderField{}, false
	}

	// First step must be a PredicateNode whose Expr is a $fetch() call.
	pred, ok := path.Steps[0].(*jparse.PredicateNode)
	if !ok {
		return ProviderField{}, false
	}
	fnCall, ok := pred.Expr.(*jparse.FunctionCallNode)
	if !ok {
		return ProviderField{}, false
	}
	fnVar, ok := fnCall.Func.(*jparse.VariableNode)
	if !ok || fnVar.Name != "fetch" {
		return ProviderField{}, false
	}
	if len(fnCall.Args) < 2 {
		return ProviderField{}, false
	}
	provArg, ok1 := fnCall.Args[0].(*jparse.StringNode)
	epArg, ok2 := fnCall.Args[1].(*jparse.StringNode)
	if !ok1 || !ok2 {
		return ProviderField{}, false
	}

	// Build a synthetic PathNode that replaces $fetch(...) with a NameNode
	// using the endpoint name. This lets the existing Analyze function run
	// cleanly: the endpoint name becomes the root JSON key the generated code
	// will navigate to in the upstream response body.
	syntheticPred := &jparse.PredicateNode{
		Expr:    &jparse.NameNode{Value: epArg.Value},
		Filters: pred.Filters,
	}
	syntheticPath := &jparse.PathNode{
		Steps: append([]jparse.Node{syntheticPred}, path.Steps[1:]...),
	}

	queryPlan, err := analyzeFilterPipeline(syntheticPath)
	if err != nil {
		return ProviderField{}, false
	}

	// Override the generated names with the outer function name so the filter
	// function and its output type are scoped to the service, not the endpoint.
	// InputStructPrefix avoids collisions when multiple services share the same
	// endpoint name (e.g. both use "data") — each gets its own element struct.
	baseName := strings.TrimPrefix(funcName, "Transform")
	queryPlan.FuncName = funcName
	queryPlan.OutputName = baseName + "Result"
	queryPlan.InputStructPrefix = unexportedName(baseName)

	return ProviderField{
		Kind:       "fetchFilter",
		Provider:   provArg.Value,
		Endpoint:   epArg.Value,
		FilterPlan: queryPlan,
	}, true
}

// tryAnalyzeRangeMap detects the top-level [a..b].{key: value} pattern (range + array map).
// Supports (bindings; [a..b].{...}). Returns a ProviderPlan with RangeMap set and optional RootBindings, or (nil, false).
func tryAnalyzeRangeMap(root jparse.Node, funcName string) (*ProviderPlan, bool) {
	var bindingNodes []jparse.Node
	var lastExpr jparse.Node

	if block, ok := root.(*jparse.BlockNode); ok && len(block.Exprs) >= 1 {
		bindingNodes = block.Exprs[:len(block.Exprs)-1]
		lastExpr = block.Exprs[len(block.Exprs)-1]
	} else {
		lastExpr = root
	}

	path, ok := lastExpr.(*jparse.PathNode)
	if !ok || len(path.Steps) != 2 {
		return nil, false
	}

	// Step 0: must be [a..b] — ArrayNode with single RangeNode item.
	step0, ok := path.Steps[0].(*jparse.ArrayNode)
	if !ok || len(step0.Items) != 1 {
		return nil, false
	}
	rangeNode, ok := step0.Items[0].(*jparse.RangeNode)
	if !ok {
		return nil, false
	}

	// Step 1: object projection — ObjectNode or BlockNode whose last expr is ObjectNode.
	var obj *jparse.ObjectNode
	switch step1 := path.Steps[1].(type) {
	case *jparse.ObjectNode:
		obj = step1
	case *jparse.BlockNode:
		if len(step1.Exprs) < 1 {
			return nil, false
		}
		last, ok := step1.Exprs[len(step1.Exprs)-1].(*jparse.ObjectNode)
		if !ok {
			return nil, false
		}
		obj = last
	default:
		return nil, false
	}

	fc := &fieldCollector{numeric: map[string]bool{}, varTypes: map[string]string{}}
	plan := &ProviderPlan{FuncName: funcName}

	// Analyze bindings first so range/projection can reference their variables.
	for _, bNode := range bindingNodes {
		binding, err := analyzeExpr(bNode, fc)
		if err != nil {
			return nil, false
		}
		if binding.Kind != "assign" {
			return nil, false
		}
		fc.varTypes[binding.VarName] = binding.GoType
		plan.RootBindings = append(plan.RootBindings, binding)
	}

	startExpr, err := analyzeExpr(rangeNode.LHS, fc)
	if err != nil {
		return nil, false
	}
	endExpr, err := analyzeExpr(rangeNode.RHS, fc)
	if err != nil {
		return nil, false
	}

	var outputFields []OutputField
	for _, pair := range obj.Pairs {
		out, err := analyzeProjection(pair, fc)
		if err != nil {
			return nil, false
		}
		outputFields = append(outputFields, out)
	}
	for i := range outputFields {
		resolveFieldTypes(outputFields[i].Value, fc)
		outputFields[i].GoType = outputFields[i].Value.GoType
	}

	plan.RangeMap = &RangeMapPlan{
		StartExpr:    startExpr,
		EndExpr:      endExpr,
		OutputFields: outputFields,
	}

	return plan, true
}

// analyzeValueNode determines the kind of a value expression and extracts its
// metadata. It handles $fetch(), request functions, string literals, and complex expressions.
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
	if ok && len(path.Steps) > 0 {
		fnCall, ok := path.Steps[0].(*jparse.FunctionCallNode)
		if ok {
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
	}

	// For complex expressions (conditionals, etc.), use analyzeExpr
	// and check if the result is an error/response
	fc := &fieldCollector{numeric: map[string]bool{}, varTypes: map[string]string{}}
	expr, err := analyzeExpr(node, fc)
	if err != nil {
		return ProviderField{}, fmt.Errorf("analyzing expression: %w", err)
	}

	// Check if the expression is an error or response
	if expr.Kind == "error" {
		return ProviderField{
			Kind:         "error",
			StatusCode:   expr.StatusCode,
			ErrorMessage: expr.ErrorMessage,
		}, nil
	}

	if expr.Kind == "response" {
		return ProviderField{
			Kind:       "response",
			StatusCode: expr.ResponseStatusCode,
			BodyExpr:   expr.ResponseBodyExpr,
			Headers:    expr.ResponseHeaders,
		}, nil
	}

	// For other expressions, store the Expr for codegen to evaluate
	return ProviderField{
		Kind:      "expr",
		ValueExpr: expr,
	}, nil
}

// analyzeFunctionCall parses a FunctionCallNode and its trailing path segments
// into a ProviderField. It handles $fetch(), $request(), $httpError(), and $httpResponse().
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

	case "params":
		if len(fnCall.Args) != 0 {
			return ProviderField{}, fmt.Errorf("$params() takes no arguments, got %d", len(fnCall.Args))
		}
		return analyzeParamsPath(trailingPath)

	case "httpError":
		if len(trailingPath) > 0 {
			return ProviderField{}, fmt.Errorf("$httpError() does not support path navigation")
		}
		return analyzeErrorFn(fnCall)

	case "httpResponse":
		if len(trailingPath) > 0 {
			return ProviderField{}, fmt.Errorf("$httpResponse() does not support path navigation")
		}
		// $httpResponse() needs a fieldCollector for analyzing expressions in body/headers
		// We'll create a temporary one here
		fc := &fieldCollector{numeric: map[string]bool{}, varTypes: map[string]string{}}
		return analyzeResponseFn(fnCall, fc)

	default:
		return ProviderField{}, fmt.Errorf("unsupported function $%s", fnVar.Name)
	}
}

// analyzeRequestPath maps $request() trailing path segments to a ProviderField.
// e.g. ["headers", "Authorization"] → Kind="header", Arg="Authorization"
//
//	["path"]                     → Kind="path"
//	["body", "user", "name"]     → Kind="body", BodyPath=["user","name"]
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

func analyzeParamsPath(path []string) (ProviderField, error) {
	return ProviderField{
		Kind:       "serviceParam",
		ParamsPath: path,
	}, nil
}

func analyzeContextExprCall(fnCall *jparse.FunctionCallNode, trailingPath []string) (*Expr, error) {
	fnVar, ok := fnCall.Func.(*jparse.VariableNode)
	if !ok {
		return nil, fmt.Errorf("expected a named function, got %T", fnCall.Func)
	}

	if len(fnCall.Args) != 0 {
		return nil, fmt.Errorf("$%s() takes no arguments, got %d", fnVar.Name, len(fnCall.Args))
	}

	switch fnVar.Name {
	case "request":
		return analyzeRequestExprPath(trailingPath)
	case "params":
		return &Expr{
			Kind:          "requestValue",
			RequestSource: "serviceParam",
			RequestPath:   trailingPath,
			GoType:        "any",
		}, nil
	default:
		return nil, fmt.Errorf("unsupported function $%s", fnVar.Name)
	}
}

func analyzeRequestExprPath(path []string) (*Expr, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("$request() requires a category (e.g. $request().headers.Name)")
	}

	category := path[0]
	switch category {
	case "headers", "cookies", "query", "params":
		if len(path) < 2 {
			return nil, fmt.Errorf("$request().%s requires a key name (e.g. $request().%s.Name)", category, category)
		}
		return &Expr{
			Kind:          "requestValue",
			RequestSource: categoryToKind[category],
			RequestArg:    path[1],
			GoType:        "any",
		}, nil
	case "path":
		return &Expr{Kind: "requestValue", RequestSource: "path", GoType: "string"}, nil
	case "method":
		return &Expr{Kind: "requestValue", RequestSource: "method", GoType: "string"}, nil
	case "body":
		return &Expr{
			Kind:          "requestValue",
			RequestSource: "body",
			RequestPath:   path[1:],
			GoType:        "any",
		}, nil
	default:
		return nil, fmt.Errorf("$request().%s is not a valid category (use headers/cookies/query/params/path/method/body)", category)
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

// analyzeServiceFn parses a $service("name", params?) call. It takes a required
// string service name, an optional params object, and optional trailing path
// segments for extracting nested values from the service result.
func analyzeServiceFn(fnCall *jparse.FunctionCallNode, trailingPath []string) (ProviderField, error) {
	if len(fnCall.Args) < 1 || len(fnCall.Args) > 2 {
		return ProviderField{}, fmt.Errorf("$service() requires 1 or 2 arguments, got %d", len(fnCall.Args))
	}

	nameArg, ok := fnCall.Args[0].(*jparse.StringNode)
	if !ok {
		return ProviderField{}, fmt.Errorf("$service() first argument must be a string literal, got %T", fnCall.Args[0])
	}

	field := ProviderField{
		Kind:        "service",
		ServiceName: nameArg.Value,
		JSONPath:    trailingPath,
	}
	if len(fnCall.Args) == 2 {
		if _, ok := fnCall.Args[1].(*jparse.ObjectNode); !ok {
			return ProviderField{}, fmt.Errorf("$service() second argument must be an object, got %T", fnCall.Args[1])
		}
		if err := validateServiceParamsNode(fnCall.Args[1]); err != nil {
			return ProviderField{}, err
		}
		fc := &fieldCollector{numeric: map[string]bool{}, varTypes: map[string]string{}}
		paramsExpr, err := analyzeExpr(fnCall.Args[1], fc)
		if err != nil {
			return ProviderField{}, fmt.Errorf("$service() params: %w", err)
		}
		field.ServiceParamsExpr = paramsExpr
	}
	return field, nil
}

func validateServiceParamsNode(node jparse.Node) error {
	if node == nil {
		return nil
	}

	if fnCall, ok := node.(*jparse.FunctionCallNode); ok {
		if fnVar, ok := fnCall.Func.(*jparse.VariableNode); ok && fnVar.Name == "fetch" {
			return fmt.Errorf("$service() params cannot contain $fetch(); fetch the value in the parent service first and pass it explicitly")
		}
	}

	switch n := node.(type) {
	case *jparse.PathNode:
		for _, step := range n.Steps {
			if err := validateServiceParamsNode(step); err != nil {
				return err
			}
		}
	case *jparse.BlockNode:
		for _, expr := range n.Exprs {
			if err := validateServiceParamsNode(expr); err != nil {
				return err
			}
		}
	case *jparse.ObjectNode:
		for _, pair := range n.Pairs {
			if err := validateServiceParamsNode(pair[0]); err != nil {
				return err
			}
			if err := validateServiceParamsNode(pair[1]); err != nil {
				return err
			}
		}
	case *jparse.FunctionCallNode:
		if err := validateServiceParamsNode(n.Func); err != nil {
			return err
		}
		for _, arg := range n.Args {
			if err := validateServiceParamsNode(arg); err != nil {
				return err
			}
		}
	case *jparse.ConditionalNode:
		if err := validateServiceParamsNode(n.If); err != nil {
			return err
		}
		if err := validateServiceParamsNode(n.Then); err != nil {
			return err
		}
		if err := validateServiceParamsNode(n.Else); err != nil {
			return err
		}
	case *jparse.PredicateNode:
		if err := validateServiceParamsNode(n.Expr); err != nil {
			return err
		}
		for _, filter := range n.Filters {
			if err := validateServiceParamsNode(filter); err != nil {
				return err
			}
		}
	case *jparse.AssignmentNode:
		return validateServiceParamsNode(n.Value)
	case *jparse.FunctionApplicationNode:
		if err := validateServiceParamsNode(n.LHS); err != nil {
			return err
		}
		if err := validateServiceParamsNode(n.RHS); err != nil {
			return err
		}
	case *jparse.ArrayNode:
		for _, item := range n.Items {
			if err := validateServiceParamsNode(item); err != nil {
				return err
			}
		}
	case *jparse.RangeNode:
		if err := validateServiceParamsNode(n.LHS); err != nil {
			return err
		}
		if err := validateServiceParamsNode(n.RHS); err != nil {
			return err
		}
	case *jparse.LambdaNode:
		return validateServiceParamsNode(n.Body)
	case *jparse.SortNode:
		if err := validateServiceParamsNode(n.Expr); err != nil {
			return err
		}
		for _, term := range n.Terms {
			if err := validateServiceParamsNode(term.Expr); err != nil {
				return err
			}
		}
	case *jparse.PartialNode:
		if err := validateServiceParamsNode(n.Func); err != nil {
			return err
		}
		for _, arg := range n.Args {
			if err := validateServiceParamsNode(arg); err != nil {
				return err
			}
		}
	}

	return nil
}

// analyzeErrorFn parses a $httpError(statusCode, message, code?) call.
// It takes 2 or 3 arguments: status code (number), message (string), and optional error code (string).
func analyzeErrorFn(fnCall *jparse.FunctionCallNode) (ProviderField, error) {
	if len(fnCall.Args) < 2 || len(fnCall.Args) > 3 {
		return ProviderField{}, fmt.Errorf("$httpError() requires 2 or 3 arguments (statusCode, message, code?), got %d", len(fnCall.Args))
	}

	statusCodeNode, ok := fnCall.Args[0].(*jparse.NumberNode)
	if !ok {
		return ProviderField{}, fmt.Errorf("$httpError() first argument must be a number (status code), got %T", fnCall.Args[0])
	}

	messageNode, ok := fnCall.Args[1].(*jparse.StringNode)
	if !ok {
		return ProviderField{}, fmt.Errorf("$httpError() second argument must be a string (message), got %T", fnCall.Args[1])
	}

	errorCode := ""
	if len(fnCall.Args) == 3 {
		codeNode, ok := fnCall.Args[2].(*jparse.StringNode)
		if !ok {
			return ProviderField{}, fmt.Errorf("$httpError() third argument must be a string (error code), got %T", fnCall.Args[2])
		}
		errorCode = codeNode.Value
	}

	return ProviderField{
		Kind:         "error",
		StatusCode:   int(statusCodeNode.Value),
		ErrorMessage: messageNode.Value,
		ErrorCode:    errorCode,
	}, nil
}

// analyzeResponseFn parses a $httpResponse(statusCode, body, headers?) call.
// Arguments:
//   - statusCode: number (HTTP status code)
//   - body: any (response body, will be JSON-marshalled)
//   - headers: object (optional, custom headers)
func analyzeResponseFn(fnCall *jparse.FunctionCallNode, fc *fieldCollector) (ProviderField, error) {
	if len(fnCall.Args) < 2 || len(fnCall.Args) > 3 {
		return ProviderField{}, fmt.Errorf("$httpResponse() requires 2 or 3 arguments (statusCode, body, headers?), got %d", len(fnCall.Args))
	}

	statusCodeNode, ok := fnCall.Args[0].(*jparse.NumberNode)
	if !ok {
		return ProviderField{}, fmt.Errorf("$httpResponse() first argument must be a number (status code), got %T", fnCall.Args[0])
	}

	// Second arg is the body - can be any expression, we'll evaluate it
	bodyExpr, err := analyzeExpr(fnCall.Args[1], fc)
	if err != nil {
		return ProviderField{}, fmt.Errorf("$httpResponse() body: %w", err)
	}

	field := ProviderField{
		Kind:       "response",
		StatusCode: int(statusCodeNode.Value),
		BodyExpr:   bodyExpr,
	}

	// Optional 3rd arg: headers object
	if len(fnCall.Args) == 3 {
		headersObj, ok := fnCall.Args[2].(*jparse.ObjectNode)
		if !ok {
			return ProviderField{}, fmt.Errorf("$httpResponse() third argument must be an object (headers), got %T", fnCall.Args[2])
		}

		headers := make(map[string]*Expr)
		for _, pair := range headersObj.Pairs {
			key, err := extractSingleName(pair[0])
			if err != nil {
				return ProviderField{}, fmt.Errorf("$httpResponse() header key: %w", err)
			}
			valueExpr, err := analyzeExpr(pair[1], fc)
			if err != nil {
				return ProviderField{}, fmt.Errorf("$httpResponse() header %q value: %w", key, err)
			}
			headers[key] = valueExpr
		}
		field.Headers = headers
	}

	return field, nil
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

	switch fnVar.Name {
	case "request":
		if len(fnCall.Args) != 0 {
			return ConfigValue{}, fmt.Errorf("$request() takes no arguments, got %d", len(fnCall.Args))
		}
		return analyzeConfigRequestPath(trailing)
	case "params":
		if len(fnCall.Args) != 0 {
			return ConfigValue{}, fmt.Errorf("$params() takes no arguments, got %d", len(fnCall.Args))
		}
		return ConfigValue{Kind: "serviceParam", Path: trailing}, nil
	default:
		return ConfigValue{}, fmt.Errorf("unsupported function $%s in config (use $request() or $params())", fnVar.Name)
	}
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

func collectRequestRefsFromExpr(expr *Expr, addKey func(kind, arg string)) {
	if expr == nil {
		return
	}

	if expr.Kind == "requestValue" {
		switch expr.RequestSource {
		case "header", "cookie", "query", "param":
			addKey(expr.RequestSource, expr.RequestArg)
		case "path":
			addKey("path", "")
		case "method":
			addKey("method", "")
		case "body":
			addKey("body", "")
		}
	}

	collectRequestRefsFromExpr(expr.Left, addKey)
	collectRequestRefsFromExpr(expr.Right, addKey)
	collectRequestRefsFromExpr(expr.Cond, addKey)
	collectRequestRefsFromExpr(expr.Then, addKey)
	collectRequestRefsFromExpr(expr.Else, addKey)
	for _, arg := range expr.FuncArgs {
		collectRequestRefsFromExpr(arg, addKey)
	}
	for _, binding := range expr.LambdaBindings {
		collectRequestRefsFromExpr(binding, addKey)
	}
	collectRequestRefsFromExpr(expr.LambdaBody, addKey)
	collectRequestRefsFromExpr(expr.ResponseBodyExpr, addKey)
	for _, headerExpr := range expr.ResponseHeaders {
		collectRequestRefsFromExpr(headerExpr, addKey)
	}
}

func serviceResultKey(serviceName string, occurrence int) string {
	if occurrence <= 1 {
		return "$service." + serviceName
	}
	return fmt.Sprintf("$service.%s#%d", serviceName, occurrence)
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

func extractVariableName(node jparse.Node) (string, error) {
	v, ok := node.(*jparse.VariableNode)
	if !ok {
		return "", fmt.Errorf("expected a variable ($name), got %T", node)
	}
	return v.Name, nil
}

func extractSortTerm(t jparse.SortTerm) (SortTerm, error) {
	// The term expr is usually a PathNode wrapping a single NameNode.
	var fieldJSON string
	if p, ok := t.Expr.(*jparse.PathNode); ok && len(p.Steps) == 1 {
		name, err := extractName(p.Steps[0])
		if err != nil {
			return SortTerm{}, err
		}
		fieldJSON = name
	} else {
		name, err := extractName(t.Expr)
		if err != nil {
			return SortTerm{}, fmt.Errorf("sort term must be a simple field name: %w", err)
		}
		fieldJSON = name
	}
	return SortTerm{
		FieldJSON:  fieldJSON,
		GoName:     exportedName(fieldJSON),
		Descending: t.Dir == jparse.SortDescending,
	}, nil
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
