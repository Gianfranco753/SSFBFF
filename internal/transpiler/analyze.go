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

// --- Helpers ---

func extractName(node jparse.Node) (string, error) {
	switch n := node.(type) {
	case *jparse.NameNode:
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

// exportedName converts a snake_case or lowercase JSON name to an exported Go name.
// "order_id" -> "OrderID", "price" -> "Price", "id" -> "ID"
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
