package transpiler

import (
	"bytes"
	"fmt"
	"go/format"
	"strconv"
	"strings"
)

// expressionForComment formats expr for a Go block comment so that newlines
// do not produce uncommented lines (which would break the build if expr contains e.g. $).
func expressionForComment(expr string) string {
	return strings.ReplaceAll(expr, "\n", "\n// ")
}

func writeInputStructs(buf *bytes.Buffer, w func(string, ...any), plan *QueryPlan) {
	// Write nested structs first (for array fields with children).
	for _, f := range plan.InputFields {
		if f.IsArray && len(f.Children) > 0 {
			w("type %s struct {\n", unexportedName(f.GoName)+"Item")
			for _, child := range f.Children {
				w("\t%s %s `json:\"%s\"`\n", child.GoName, child.GoType, child.JSONName)
			}
			w("}\n\n")
		}
	}

	// Write the main input struct. InputStructPrefix overrides RootField when
	// multiple services share the same endpoint name (e.g., both use "data").
	prefix := plan.InputStructPrefix
	if prefix == "" {
		prefix = unexportedName(plan.RootField)
	}
	structName := prefix + "Element"
	w("type %s struct {\n", structName)
	for _, f := range plan.InputFields {
		if f.IsArray {
			itemType := unexportedName(f.GoName) + "Item"
			w("\t%s []%s `json:\"%s\"`\n", f.GoName, itemType, f.JSONName)
		} else {
			w("\t%s %s `json:\"%s\"`\n", f.GoName, f.GoType, f.JSONName)
		}
	}
	w("}\n\n")
}

func writeOutputStruct(buf *bytes.Buffer, w func(string, ...any), plan *QueryPlan) {
	w("type %s struct {\n", plan.OutputName)
	for _, f := range plan.OutputFields {
		w("\t%s %s `json:\"%s\"`\n", f.GoName, f.GoType, f.JSONName)
	}
	w("}\n\n")
}

func writeTransformFunc(buf *bytes.Buffer, w func(string, ...any), plan *QueryPlan) {
	prefix := plan.InputStructPrefix
	if prefix == "" {
		prefix = unexportedName(plan.RootField)
	}
	elemType := prefix + "Element"
	outType := plan.OutputName
	hasSort := len(plan.SortTerms) > 0

	w("func %s(data []byte) ([]%s, error) {\n", plan.FuncName, outType)
	w("\tdec := jsontext.NewDecoder(bytes.NewReader(data))\n\n")

	// Navigate into the root object.
	w("\ttok, err := dec.ReadToken()\n")
	w("\tif err != nil {\n")
	w("\t\treturn nil, fmt.Errorf(\"reading root: %%w\", err)\n")
	w("\t}\n")
	w("\tif tok.Kind() != '{' {\n")
	w("\t\treturn nil, fmt.Errorf(\"expected JSON object at root, got %%v\", tok.Kind())\n")
	w("\t}\n\n")

	w("\tvar results []%s\n\n", outType)

	// Stream through top-level fields looking for the target array.
	w("\tfor dec.PeekKind() != '}' {\n")
	w("\t\tnameTok, err := dec.ReadToken()\n")
	w("\t\tif err != nil {\n")
	w("\t\t\treturn nil, fmt.Errorf(\"reading field name: %%w\", err)\n")
	w("\t\t}\n\n")

	w("\t\tif nameTok.String() != %q {\n", plan.RootField)
	w("\t\t\tif err := dec.SkipValue(); err != nil {\n")
	w("\t\t\t\treturn nil, fmt.Errorf(\"skipping field: %%w\", err)\n")
	w("\t\t\t}\n")
	w("\t\t\tcontinue\n")
	w("\t\t}\n\n")

	// Found the target array — consume '['.
	w("\t\tarrTok, err := dec.ReadToken()\n")
	w("\t\tif err != nil {\n")
	w("\t\t\treturn nil, fmt.Errorf(\"reading array start: %%w\", err)\n")
	w("\t\t}\n")
	w("\t\tif arrTok.Kind() != '[' {\n")
	w("\t\t\treturn nil, fmt.Errorf(\"expected array for %s, got %%v\", arrTok.Kind())\n", plan.RootField)
	w("\t\t}\n\n")

	if hasSort {
		// Sort mode: buffer all elements, sort, then filter+project.
		w("\t\tvar allElems []%s\n", elemType)
		w("\t\tfor dec.PeekKind() != ']' {\n")
		w("\t\t\tvar elem %s\n", elemType)
		w("\t\t\tif err := jsonv2.UnmarshalDecode(dec, &elem); err != nil {\n")
		w("\t\t\t\treturn nil, fmt.Errorf(\"reading element: %%w\", err)\n")
		w("\t\t\t}\n")
		w("\t\t\tallElems = append(allElems, elem)\n")
		w("\t\t}\n")
		w("\t\tif _, err := dec.ReadToken(); err != nil {\n")
		w("\t\t\treturn nil, fmt.Errorf(\"reading array end: %%w\", err)\n")
		w("\t\t}\n\n")

		// Emit sort.
		w("\t\tslices.SortFunc(allElems, func(a, b %s) int {\n", elemType)
		for _, st := range plan.SortTerms {
			if st.Descending {
				w("\t\t\tif a.%s > b.%s { return -1 }\n", st.GoName, st.GoName)
				w("\t\t\tif a.%s < b.%s { return 1 }\n", st.GoName, st.GoName)
			} else {
				w("\t\t\tif a.%s < b.%s { return -1 }\n", st.GoName, st.GoName)
				w("\t\t\tif a.%s > b.%s { return 1 }\n", st.GoName, st.GoName)
			}
		}
		w("\t\t\treturn 0\n")
		w("\t\t})\n\n")

		// Iterate sorted elements.
		if plan.HasIndexFilter {
			w("\t\tfor elemIdx, elem := range allElems {\n")
		} else {
			w("\t\tfor _, elem := range allElems {\n")
		}
	} else {
		// Streaming mode: process each element as it's decoded.
		if plan.HasIndexFilter {
			w("\t\telemIdx := 0\n")
		}
		w("\t\tfor dec.PeekKind() != ']' {\n")
		w("\t\t\tvar elem %s\n", elemType)
		w("\t\t\tif err := jsonv2.UnmarshalDecode(dec, &elem); err != nil {\n")
		w("\t\t\t\treturn nil, fmt.Errorf(\"reading element: %%w\", err)\n")
		w("\t\t\t}\n\n")
	}

	em := &exprEmitter{w: w, indent: "\t\t\t", elemVar: "elem", funcVarNames: collectFuncVarNames(plan.Bindings)}

	// Apply filters.
	if len(plan.Filters) > 0 {
		conditions := make([]string, len(plan.Filters))
		for i, f := range plan.Filters {
			conditions[i] = em.emit(f)
		}
		allConditions := strings.Join(conditions, " && ")
		w("\t\t\tpassesFilter := %s\n", allConditions)
		w("\t\t\tif !passesFilter {\n")
		if !hasSort && plan.HasIndexFilter {
			w("\t\t\t\telemIdx++\n")
		}
		w("\t\t\t\tcontinue\n")
		w("\t\t\t}\n\n")
	}

	// Emit variable bindings ($x := expr) before the output struct.
	for _, b := range plan.Bindings {
		em.emit(b)
	}
	if len(plan.Bindings) > 0 {
		w("\n")
	}

	// Pre-compute output field values (aggregates need for loops emitted first).
	outputExprs := make([]string, len(plan.OutputFields))
	for i, out := range plan.OutputFields {
		outputExprs[i] = em.emit(out.Value)
	}

	// Build the output struct.
	w("\t\t\tresults = append(results, %s{\n", outType)
	for i, out := range plan.OutputFields {
		w("\t\t\t\t%s: %s,\n", out.GoName, outputExprs[i])
	}
	w("\t\t\t})\n")
	if !hasSort && plan.HasIndexFilter {
		w("\t\t\telemIdx++\n")
	}
	w("\t\t}\n\n")

	if !hasSort {
		// Consume ']'.
		w("\t\tif _, err := dec.ReadToken(); err != nil {\n")
		w("\t\t\treturn nil, fmt.Errorf(\"reading array end: %%w\", err)\n")
		w("\t\t}\n")
	}
	w("\t}\n\n")

	w("\treturn results, nil\n")
	w("}\n")
}

// ---------------------------------------------------------------------------
// exprEmitter — walks an Expr tree and produces Go code
// ---------------------------------------------------------------------------
// It writes pre-computation lines (for loops, conditionals) directly to w
// and returns a Go expression string that can be used inline.

type exprEmitter struct {
	w                func(string, ...any)
	indent           string
	elemVar          string
	counter          int
	contextParam     string          // when set, rootRef ($) emits this (for lambda body context)
	paramPrefix      string          // when set, varRef uses it only for names in lambdaParamNames
	funcVarNames     map[string]bool // names of variables that hold lambdas (from bindings in scope)
	lambdaParamNames map[string]bool // when set, varRef uses paramPrefix only for these names (lambda params)
}

// collectFuncVarNames returns the set of variable names that are bound to lambdas
// in the given bindings. Used so codegen can emit variable-as-callee for $f(args).
func collectFuncVarNames(bindings []*Expr) map[string]bool {
	var out map[string]bool
	for _, e := range bindings {
		if e == nil || e.Kind != "assign" || e.Left == nil || e.Left.Kind != "lambda" {
			continue
		}
		if out == nil {
			out = make(map[string]bool)
		}
		out[e.VarName] = true
	}
	return out
}

// hasRootRef returns true if the expression tree contains a rootRef ($).
func hasRootRef(e *Expr) bool {
	if e == nil {
		return false
	}
	if e.Kind == "rootRef" {
		return true
	}
	if hasRootRef(e.Left) || hasRootRef(e.Right) {
		return true
	}
	if hasRootRef(e.Cond) || hasRootRef(e.Then) || hasRootRef(e.Else) {
		return true
	}
	for _, arg := range e.FuncArgs {
		if hasRootRef(arg) {
			return true
		}
	}
	return false
}

// collectVarRefNames returns the set of variable names (VarName) referenced in the expression tree.
// Used to detect which bindings are referenced before definition (for forward declaration).
func collectVarRefNames(e *Expr) map[string]bool {
	if e == nil {
		return nil
	}
	out := make(map[string]bool)
	var walk func(*Expr)
	walk = func(x *Expr) {
		if x == nil {
			return
		}
		if x.Kind == "varRef" && x.VarName != "" {
			out[x.VarName] = true
		}
		// funcCall with a name may be a variable (e.g. $cos(...)); count for forward-declaration.
		if x.Kind == "funcCall" && x.FuncName != "" {
			out[x.FuncName] = true
		}
		walk(x.Left)
		walk(x.Right)
		walk(x.Cond)
		walk(x.Then)
		walk(x.Else)
		for _, a := range x.FuncArgs {
			walk(a)
		}
		if x.Kind == "assign" && x.Left != nil && x.Left.Kind == "lambda" {
			for _, b := range x.Left.LambdaBindings {
				walk(b)
			}
			walk(x.Left.LambdaBody)
		}
	}
	walk(e)
	return out
}

// mergeFuncVarNames merges parent and fromBindings into a new map. Used when
// building the inner emitter for a lambda so it can call function vars from outer scope.
func mergeFuncVarNames(parent, fromBindings map[string]bool) map[string]bool {
	if len(parent) == 0 && len(fromBindings) == 0 {
		return nil
	}
	out := make(map[string]bool)
	for k := range parent {
		out[k] = true
	}
	for k := range fromBindings {
		out[k] = true
	}
	return out
}

func (em *exprEmitter) emit(e *Expr) string {
	if e == nil {
		return "nil"
	}
	switch e.Kind {
	case "field":
		return em.elemVar + "." + e.FieldName

	case "arrayField":
		return em.elemVar + "." + e.ArrayName

	case "literal":
		return e.LiteralValue

	case "binary":
		return em.emitBinary(e)

	case "unary":
		operand := em.emit(e.Left)
		if e.Left != nil && e.Left.GoType == "any" {
			operand = fmt.Sprintf("runtime.ToNumber(%s)", operand)
		}
		return fmt.Sprintf("(-%s)", operand)

	case "conditional":
		return em.emitConditional(e)

	case "funcCall":
		return em.emitFuncCall(e)

	case "assign":
		if e.Left != nil && e.Left.Kind == "lambda" {
			return em.emitLambdaAssign(e.VarName, e.Left)
		}
		val := em.emit(e.Left)
		varName := "jsonataVar_" + e.VarName
		em.w("%s%s := %s\n", em.indent, varName, val)
		return varName

	case "varRef":
		if em.lambdaParamNames != nil && em.lambdaParamNames[e.VarName] && em.paramPrefix != "" {
			return em.paramPrefix + e.VarName
		}
		return "jsonataVar_" + e.VarName

	case "requestValue":
		return em.emitRequestValue(e)

	case "array":
		items := make([]string, len(e.FuncArgs))
		for i, item := range e.FuncArgs {
			items[i] = em.emit(item)
		}
		return fmt.Sprintf("[]any{%s}", strings.Join(items, ", "))

	case "object":
		// Object literal: convert key-value pairs to map[string]any
		if len(e.FuncArgs)%2 != 0 {
			return "map[string]any{}"
		}
		pairs := make([]string, 0, len(e.FuncArgs)/2)
		for i := 0; i < len(e.FuncArgs); i += 2 {
			key := em.emit(e.FuncArgs[i])
			value := em.emit(e.FuncArgs[i+1])
			pairs = append(pairs, fmt.Sprintf("%s: %s", key, value))
		}
		return fmt.Sprintf("map[string]any{%s}", strings.Join(pairs, ", "))

	case "rootRef":
		if em.contextParam != "" {
			return em.contextParam
		}
		return "data"

	case "lambda":
		// Standalone lambda: assign to a temp var so it can be used as a value.
		em.counter++
		tempName := "jsonataLambda_" + strconv.Itoa(em.counter)
		return em.emitLambdaToVar(tempName, e, false)

	case "lambdaCall":
		fn := em.emit(e.Left)
		arg := em.emit(e.Right)
		return fmt.Sprintf("%s(%s)", fn, arg)

	case "elemIndex":
		return "elemIdx"

	case "error":
		// Error expression - this should be handled at a higher level
		// Return a placeholder that will trigger error handling
		em.w("%s// ERROR: $httpError() should be handled before evaluation\n", em.indent)
		return "nil"

	case "response":
		// Response expression - this should be handled at a higher level
		em.w("%s// ERROR: $httpResponse() should be handled before evaluation\n", em.indent)
		return "nil"

	case "arrayMap":
		em.counter++
		resultVar := "arrayMapResult_" + strconv.Itoa(em.counter)
		useElem := hasRootRef(e.Right)
		arrExpr := em.emit(e.Left)
		em.w("%svar %s []any\n", em.indent, resultVar)
		if useElem {
			elemVar := "arrayMapElem_" + strconv.Itoa(em.counter)
			em.w("%sfor _, %s := range %s {\n", em.indent, elemVar, arrExpr)
			savedContext := em.contextParam
			em.contextParam = elemVar
			em.w("%s\t%s = append(%s, %s)\n", em.indent, resultVar, resultVar, em.emit(e.Right))
			em.contextParam = savedContext
			em.w("%s}\n", em.indent)
		} else {
			em.w("%sfor range %s {\n", em.indent, arrExpr)
			em.w("%s\t%s = append(%s, %s)\n", em.indent, resultVar, resultVar, em.emit(e.Right))
			em.w("%s}\n", em.indent)
		}
		return resultVar

	default:
		return "nil"
	}
}

func (em *exprEmitter) emitBinary(e *Expr) string {
	left := em.emit(e.Left)
	right := em.emit(e.Right)

	if e.Op == "&" {
		return fmt.Sprintf("runtime.ToString(%s) + runtime.ToString(%s)", left, right)
	}
	// Numeric ops and comparisons require concrete types; wrap any in runtime.ToNumber.
	numericOps := map[string]bool{"+": true, "-": true, "*": true, "/": true, "%": true}
	comparisonOps := map[string]bool{"<": true, "<=": true, ">": true, ">=": true, "==": true, "!=": true}
	if numericOps[e.Op] || comparisonOps[e.Op] {
		if e.Left != nil && e.Left.GoType == "any" {
			left = fmt.Sprintf("runtime.ToNumber(%s)", left)
		}
		if e.Right != nil && e.Right.GoType == "any" {
			right = fmt.Sprintf("runtime.ToNumber(%s)", right)
		}
	}
	return fmt.Sprintf("(%s %s %s)", left, e.Op, right)
}

func (em *exprEmitter) emitRequestValue(e *Expr) string {
	switch e.RequestSource {
	case "header", "cookie", "query", "param":
		return fmt.Sprintf("req.%s[%q]", requestFuncToMap(e.RequestSource), e.RequestArg)
	case "path":
		return "req.Path"
	case "method":
		return "req.Method"
	case "body":
		return lookupJSONExpr("req.Body", e.RequestPath)
	case "serviceParam":
		return lookupValueExpr("req.ServiceParams", e.RequestPath)
	default:
		return "nil"
	}
}

func (em *exprEmitter) emitConditional(e *Expr) string {
	em.counter++
	varName := fmt.Sprintf("cond%d", em.counter)
	cond := em.emit(e.Cond)
	thenVal := em.emit(e.Then)

	goType := e.GoType
	if goType == "" || goType == "any" {
		goType = "any"
	}

	em.w("%svar %s %s\n", em.indent, varName, goType)
	em.w("%sif runtime.Truthy(%s) {\n", em.indent, cond)
	em.w("%s\t%s = %s\n", em.indent, varName, thenVal)
	if e.Else != nil {
		em.w("%s} else {\n", em.indent)
		savedIndent := em.indent
		em.indent = em.indent + "\t"
		elseVal := em.emit(e.Else)
		em.indent = savedIndent
		em.w("%s\t%s = %s\n", em.indent, varName, elseVal)
	}
	em.w("%s}\n", em.indent)

	return varName
}

// emitLambdaToVar writes a Go function literal for a JSONata lambda and assigns it
// to targetVarName. When useAssignment is true, emits "=" instead of ":=" (for forward-declared vars).
func (em *exprEmitter) emitLambdaToVar(targetVarName string, lambda *Expr, useAssignment bool) string {
	params := make([]string, len(lambda.LambdaParams))
	for i, p := range lambda.LambdaParams {
		params[i] = "jsonataParam_" + p + " any"
	}
	paramList := strings.Join(params, ", ")
	innerIndent := em.indent + "\t"
	lambdaParams := make(map[string]bool)
	for _, p := range lambda.LambdaParams {
		lambdaParams[p] = true
	}
	inner := &exprEmitter{
		w:                em.w,
		indent:           innerIndent,
		elemVar:          em.elemVar,
		counter:          em.counter,
		paramPrefix:      "jsonataParam_",
		funcVarNames:     mergeFuncVarNames(em.funcVarNames, collectFuncVarNames(lambda.LambdaBindings)),
		lambdaParamNames: lambdaParams,
	}
	if len(lambda.LambdaParams) > 0 {
		inner.contextParam = "jsonataParam_" + lambda.LambdaParams[0]
	}
	assignOp := ":="
	if useAssignment {
		assignOp = "="
	}
	em.w("%s%s %s func(%s) any {\n", em.indent, targetVarName, assignOp, paramList)
	for _, b := range lambda.LambdaBindings {
		_ = inner.emit(b)
	}
	bodyVal := inner.emit(lambda.LambdaBody)
	em.w("%sreturn %s\n", innerIndent, bodyVal)
	em.w("%s}\n", em.indent)
	return targetVarName
}

// emitLambdaAssign writes a Go function literal for a JSONata lambda and assigns it
// to jsonataVar_<varName>. Used when the RHS of := is function(...) { ... }.
func (em *exprEmitter) emitLambdaAssign(varName string, lambda *Expr) string {
	return em.emitLambdaToVar("jsonataVar_"+varName, lambda, false)
}

func (em *exprEmitter) emitFuncCall(e *Expr) string {
	// Aggregate functions with an arrayField argument need a pre-computation loop.
	if isAggregateFunc(e.FuncName) && len(e.FuncArgs) == 1 && e.FuncArgs[0].Kind == "arrayField" {
		return em.emitAggregate(e)
	}

	// All other functions map directly to runtime.* calls.
	args := make([]string, len(e.FuncArgs))
	for i, arg := range e.FuncArgs {
		args[i] = em.emit(arg)
	}
	return em.mapFuncCall(e.FuncName, args)
}

func (em *exprEmitter) emitAggregate(e *Expr) string {
	em.counter++
	af := e.FuncArgs[0]
	varName := fmt.Sprintf("agg%d", em.counter)

	switch e.FuncName {
	case "sum":
		em.w("%svar %s float64\n", em.indent, varName)
		em.w("%sfor _, v := range %s.%s {\n", em.indent, em.elemVar, af.ArrayName)
		em.w("%s\t%s += v.%s\n", em.indent, varName, af.ChildName)
		em.w("%s}\n", em.indent)

	case "count":
		em.w("%s%s := float64(len(%s.%s))\n", em.indent, varName, em.elemVar, af.ArrayName)

	case "min", "max", "average":
		sliceVar := varName + "Slice"
		em.w("%svar %s []float64\n", em.indent, sliceVar)
		em.w("%sfor _, v := range %s.%s {\n", em.indent, em.elemVar, af.ArrayName)
		em.w("%s\t%s = append(%s, v.%s)\n", em.indent, sliceVar, sliceVar, af.ChildName)
		em.w("%s}\n", em.indent)

		runtimeFn := map[string]string{"min": "MinFloat64", "max": "MaxFloat64", "average": "AverageFloat64"}
		em.w("%s%s := runtime.%s(%s)\n", em.indent, varName, runtimeFn[e.FuncName], sliceVar)
	}

	return varName
}

func (em *exprEmitter) mapFuncCall(name string, args []string) string {
	all := strings.Join(args, ", ")
	// User-defined lambdas shadow builtins (e.g. $floor := lambda overrides builtin $floor).
	if em.funcVarNames != nil && em.funcVarNames[name] {
		return fmt.Sprintf("jsonataVar_%s(%s)", name, all)
	}
	switch name {
	// String functions
	case "string":
		return fmt.Sprintf("runtime.ToString(%s)", all)
	case "length":
		return fmt.Sprintf("runtime.Length(%s)", all)
	case "substring":
		return fmt.Sprintf("runtime.Substring(%s)", all)
	case "substringBefore":
		return fmt.Sprintf("runtime.SubstringBefore(%s)", all)
	case "substringAfter":
		return fmt.Sprintf("runtime.SubstringAfter(%s)", all)
	case "uppercase":
		return fmt.Sprintf("runtime.Uppercase(%s)", all)
	case "lowercase":
		return fmt.Sprintf("runtime.Lowercase(%s)", all)
	case "trim":
		return fmt.Sprintf("runtime.Trim(%s)", all)
	case "contains":
		return fmt.Sprintf("runtime.Contains(%s)", all)
	case "join":
		return fmt.Sprintf("runtime.JoinArray(%s)", all)
	case "pad":
		return fmt.Sprintf("runtime.Pad(%s)", all)
	case "split":
		return fmt.Sprintf("runtime.SplitArray(%s)", all)

	// Numeric functions
	case "number":
		return fmt.Sprintf("runtime.ToNumber(%s)", all)
	case "abs":
		return fmt.Sprintf("runtime.Abs(%s)", all)
	case "floor":
		return fmt.Sprintf("runtime.Floor(%s)", all)
	case "ceil":
		return fmt.Sprintf("runtime.Ceil(%s)", all)
	case "round":
		return fmt.Sprintf("runtime.Round(%s)", all)
	case "power":
		if len(args) == 2 {
			return fmt.Sprintf("runtime.Power(runtime.ToNumber(%s), runtime.ToNumber(%s))", args[0], args[1])
		}
		return fmt.Sprintf("runtime.Power(%s)", all)
	case "sqrt":
		return fmt.Sprintf("runtime.Sqrt(%s)", all)
	case "random":
		return fmt.Sprintf("runtime.Random(%s)", all)

	// Boolean functions
	case "boolean":
		return fmt.Sprintf("runtime.ToBoolean(%s)", all)
	case "not":
		return fmt.Sprintf("runtime.Not(%s)", all)
	case "exists":
		return fmt.Sprintf("runtime.Exists(%s)", all)

	// Internal functions (mapped from special syntax)
	case "_range":
		if len(args) == 2 {
			return fmt.Sprintf("runtime.Range(runtime.ToNumber(%s), runtime.ToNumber(%s))", args[0], args[1])
		}
		return fmt.Sprintf("runtime.Range(%s)", all)
	case "_in":
		return fmt.Sprintf("runtime.In(%s)", all)
	case "_wildcard":
		return fmt.Sprintf("runtime.WildcardValues(%s)", all)

	// Array/object functions
	case "sort":
		return fmt.Sprintf("runtime.SortArray(%s)", all)
	case "reverse":
		return fmt.Sprintf("runtime.ReverseArray(%s)", all)
	case "append":
		return fmt.Sprintf("runtime.AppendArray(%s)", all)
	case "distinct":
		return fmt.Sprintf("runtime.DistinctArray(%s)", all)
	case "shuffle":
		return fmt.Sprintf("runtime.ShuffleArray(%s)", all)
	case "zip":
		return fmt.Sprintf("runtime.ZipArray(%s)", all)
	case "keys":
		return fmt.Sprintf("runtime.KeysMap(%s)", all)
	case "merge":
		return fmt.Sprintf("runtime.MergeArray(%s)", all)
	case "type":
		return fmt.Sprintf("runtime.TypeOf(%s)", all)
	case "values":
		return fmt.Sprintf("runtime.ValuesMap(%s)", all)
	case "spread":
		return fmt.Sprintf("runtime.SpreadMap(%s)", all)

	// Date/Time functions
	case "now":
		return fmt.Sprintf("runtime.Now(%s)", all)
	case "millis":
		return fmt.Sprintf("runtime.Millis(%s)", all)
	case "fromMillis":
		return fmt.Sprintf("runtime.FromMillis(%s)", all)
	case "toMillis":
		return fmt.Sprintf("runtime.ToMillis(%s)", all)

	default:
		return fmt.Sprintf("nil /* unsupported: $%s(%s) */", name, all)
	}
}

// GenerateProvider produces Go source code from a ProviderPlan.
// It emits:
//   - A Deps function that builds the list of upstream calls (with optional
//     method/headers/body from the request context)
//   - A transform function that assembles the output JSON from fetched results
//     and/or request context data
//   - An Execute function that bundles the full pipeline: deps + fetch +
//     sub-services + transform (only when the plan has deps or services)
//
// For a plan with a single Kind="fetchFilter" field, it instead emits the
// streaming filter+project pipeline (same as Generate) wrapped in an Execute
// function that fetches the single upstream via the aggregator.
func GenerateProvider(plan *ProviderPlan, packageName, sourceFile, expression string) ([]byte, error) {
	// Delegate to the filter-as-provider path when the plan is a single
	// $fetch(p,e)[filter].{projection} expression.
	if len(plan.Fields) == 1 && plan.Fields[0].Kind == "fetchFilter" {
		return generateFetchFilter(plan, packageName, sourceFile, expression)
	}

	var buf bytes.Buffer
	w := func(format string, args ...any) {
		fmt.Fprintf(&buf, format, args...)
	}

	hasDeps := len(plan.Deps) > 0
	hasServices := len(plan.ServiceCalls) > 0
	needsExecute := hasDeps || hasServices
	hasRootExpr := plan.RootExpr != nil
	hasRangeMap := plan.RangeMap != nil

	w("//go:build goexperiment.jsonv2\n\n")
	w("// Code generated by jsonata-transpiler. DO NOT EDIT.\n")
	if sourceFile != "" {
		w("// Source: %s\n", sourceFile)
	}
	w("// Expression: %s\n\n", expressionForComment(expression))
	w("package %s\n\n", packageName)
	w("import (\n")
	// bytes/jsontext only for object output (Fields); root-expr uses jsonv2.Marshal only.
	needsStreamingEncoder := len(plan.Fields) > 0
	if needsStreamingEncoder {
		w("\t\"bytes\"\n")
		w("\t\"encoding/json/jsontext\"\n")
	}
	if needsExecute {
		w("\t\"context\"\n")
	}
	// Add fmt and jsonv2 only when the transform actually emits Marshal/Errorf (response body or conditional response).
	needsFmtAndJsonv2InTransform := hasDeps || hasServices || hasRootExpr || hasRangeMap || planNeedsResponseMarshal(plan)
	if needsFmtAndJsonv2InTransform {
		w("\t\"fmt\"\n")
	}
	if hasRootExpr || hasRangeMap || planNeedsResponseMarshal(plan) {
		w("\tjsonv2 \"encoding/json/v2\"\n")
	}
	if hasServices {
		w("\t\"sync\"\n")
	}
	w("\n")
	if needsExecute {
		w("\t\"github.com/gcossani/ssfbff/internal/aggregator\"\n")
	}
	w("\t\"github.com/gcossani/ssfbff/runtime\"\n")
	if hasServices {
		w("\t\"golang.org/x/sync/errgroup\"\n")
	}
	w(")\n\n")

	// --- RequestFields function (tells apigen which request keys are needed) ---
	writeRequestFieldsFunc(w, plan)

	// --- Deps function ---
	writeProviderDepsFunc(w, plan, false)

	// --- Transform function ---
	writeProviderTransformFunc(w, plan, hasDeps || hasServices)

	// --- Execute function (bundles deps + fetch + sub-services + transform) ---
	if needsExecute {
		writeExecuteFunc(w, plan)
	}

	return format.Source(buf.Bytes())
}

// planNeedsResponseMarshal returns true if the transform will emit jsonv2.Marshal or fmt.Errorf
// (so we need those imports). This includes response bodies and conditional branches that marshal the then-value.
func planNeedsResponseMarshal(plan *ProviderPlan) bool {
	for _, f := range plan.Fields {
		if f.Kind == "response" {
			return true
		}
		if f.Kind == "expr" && f.ValueExpr != nil {
			if f.ValueExpr.Kind == "response" {
				return true
			}
			if f.ValueExpr.Kind == "conditional" {
				if f.ValueExpr.Then != nil && f.ValueExpr.Then.Kind == "response" {
					return true
				}
				if f.ValueExpr.Else != nil && (f.ValueExpr.Else.Kind == "response" || f.ValueExpr.Else.Kind == "error") {
					return true
				}
			}
		}
	}
	return false
}

// generateFetchFilter produces code for a $fetch(p,e)[filter].{projection}
// plan. It emits the same streaming filter+project pipeline as Generate (for
// the inner QueryPlan), then adds an Execute wrapper that fetches the single
// upstream via the aggregator and calls the filter function. The Execute
// function returns a JSON array (marshalled from the typed slice).
func generateFetchFilter(plan *ProviderPlan, packageName, sourceFile, expression string) ([]byte, error) {
	field := plan.Fields[0]
	qp := field.FilterPlan

	var buf bytes.Buffer
	w := func(format string, args ...any) {
		fmt.Fprintf(&buf, format, args...)
	}

	// Header — needs all imports used by the filter pipeline plus aggregator.
	w("//go:build goexperiment.jsonv2\n\n")
	w("// Code generated by jsonata-transpiler. DO NOT EDIT.\n")
	if sourceFile != "" {
		w("// Source: %s\n", sourceFile)
	}
	w("// Expression: %s\n\n", expressionForComment(expression))
	w("package %s\n\n", packageName)
	w("import (\n")
	w("\t\"bytes\"\n")
	w("\t\"context\"\n")
	w("\t\"encoding/json/jsontext\"\n")
	w("\tjsonv2 \"encoding/json/v2\"\n")
	w("\t\"fmt\"\n")
	if qp.NeedsSlices() {
		w("\t\"slices\"\n")
	}
	w("\n")
	w("\t\"github.com/gcossani/ssfbff/internal/aggregator\"\n")
	w("\t\"github.com/gcossani/ssfbff/runtime\"\n")
	w(")\n\n")

	// Emit the filter pipeline structs and internal transform function.
	writeInputStructs(&buf, w, qp)
	writeOutputStruct(&buf, w, qp)
	// The filter function is unexported — it is only called from Execute.
	writeFilterFunc(w, qp)

	// Execute function: fetch via aggregator → filter → marshal to JSON.
	baseName := strings.TrimPrefix(plan.FuncName, "Transform")
	depKey := field.Provider + "." + field.Endpoint
	w("func Execute%s(ctx context.Context, agg *aggregator.Aggregator, req runtime.RequestContext) (*runtime.Response, error) {\n", baseName)
	w("\tdeps := []runtime.ProviderDep{{Provider: %q, Endpoint: %q}}\n", field.Provider, field.Endpoint)
	w("\tresults, err := agg.Fetch(ctx, deps)\n")
	w("\tif err != nil {\n")
	w("\t\treturn nil, fmt.Errorf(\"%s: fetch: %%w\", err)\n", baseName)
	w("\t}\n\n")
	w("\titems, err := %s(results[%q])\n", plan.FuncName, depKey)
	w("\tif err != nil {\n")
	w("\t\treturn nil, err\n")
	w("\t}\n\n")
	w("\tout, err := jsonv2.Marshal(items)\n")
	w("\tif err != nil {\n")
	w("\t\treturn nil, fmt.Errorf(\"%s: marshal: %%w\", err)\n", baseName)
	w("\t}\n")
	w("\t// Wrap array result in 200 OK response\n")
	w("\treturn &runtime.Response{\n")
	w("\t\tStatusCode: 200,\n")
	w("\t\tHeaders:    map[string]string{\"Content-Type\": \"application/json\"},\n")
	w("\t\tBody:       out,\n")
	w("\t}, nil\n")
	w("}\n")

	return format.Source(buf.Bytes())
}

// writeFilterFunc emits the streaming filter+project function with an unexported
// name. It is identical to writeTransformFunc but uses the plan.FuncName directly
// (which will be unexported by the caller naming convention).
func writeFilterFunc(w func(string, ...any), plan *QueryPlan) {
	writeTransformFunc(nil, w, plan)
}

// writeRequestFieldsFunc emits a function that returns which incoming request
// fields (headers, cookies, query, params, path, method, body) the transform
// needs. The generated route handler calls this at init time to build targeted
// extraction logic instead of copying all headers/cookies on every request.
func writeRequestFieldsFunc(w func(string, ...any), plan *ProviderPlan) {
	rf := plan.RequestFields()
	if rf.IsEmpty() {
		return
	}

	baseName := strings.TrimPrefix(plan.FuncName, "Transform")
	w("// %sRequestFields describes which incoming request data this\n", baseName)
	w("// transform needs. The route handler uses it to extract only the\n")
	w("// necessary headers/cookies/query/params instead of copying everything.\n")
	w("var %sRequestFields = runtime.RequestFieldSet{\n", baseName)

	if len(rf.Headers) > 0 {
		w("\tHeaders: []string{")
		for i, h := range rf.Headers {
			if i > 0 {
				w(", ")
			}
			w("%q", h)
		}
		w("},\n")
	}
	if len(rf.Cookies) > 0 {
		w("\tCookies: []string{")
		for i, c := range rf.Cookies {
			if i > 0 {
				w(", ")
			}
			w("%q", c)
		}
		w("},\n")
	}
	if len(rf.Query) > 0 {
		w("\tQuery: []string{")
		for i, q := range rf.Query {
			if i > 0 {
				w(", ")
			}
			w("%q", q)
		}
		w("},\n")
	}
	if len(rf.Params) > 0 {
		w("\tParams: []string{")
		for i, p := range rf.Params {
			if i > 0 {
				w(", ")
			}
			w("%q", p)
		}
		w("},\n")
	}
	if rf.NeedPath {
		w("\tNeedPath: true,\n")
	}
	if rf.NeedMethod {
		w("\tNeedMethod: true,\n")
	}
	if rf.NeedBody {
		w("\tNeedBody: true,\n")
	}

	w("}\n\n")
}

func writeProviderDepsFunc(w func(string, ...any), plan *ProviderPlan, _ bool) {
	if len(plan.Deps) == 0 {
		return
	}

	// Deps is always a function now (accepts RequestContext), so the aggregator
	// can receive per-request method/headers/body built from the incoming request.
	w("func %sDeps(req runtime.RequestContext) []runtime.ProviderDep {\n", plan.FuncName)
	w("\treturn []runtime.ProviderDep{\n")

	// Root-expr plans have Deps from AST walk but no Fields; emit simple deps.
	if plan.RootExpr != nil {
		for _, dep := range plan.Deps {
			w("\t\t{Provider: %q, Endpoint: %q},\n", dep.Provider, dep.Endpoint)
		}
		w("\t}\n")
		w("}\n\n")
		return
	}

	// Deduplicate: only emit one dep per unique provider+endpoint pair,
	// using the config from the first field that references it.
	seen := map[string]bool{}
	for _, field := range plan.Fields {
		if field.Kind != "fetch" {
			continue
		}
		depKey := field.Provider + "." + field.Endpoint
		if seen[depKey] {
			continue
		}
		seen[depKey] = true

		if field.FetchConfig == nil {
			w("\t\t{Provider: %q, Endpoint: %q},\n", field.Provider, field.Endpoint)
			continue
		}

		cfg := field.FetchConfig
		w("\t\t{\n")
		w("\t\t\tProvider: %q,\n", field.Provider)
		w("\t\t\tEndpoint: %q,\n", field.Endpoint)

		if cfg.Method != "" {
			w("\t\t\tMethod: %q,\n", cfg.Method)
		}

		if len(cfg.Headers) > 0 {
			w("\t\t\tHeaders: map[string]string{\n")
			for _, h := range cfg.Headers {
				w("\t\t\t\t%q: %s,\n", h.Key, configValueToGoExpr(h.Value, "req"))
			}
			w("\t\t\t},\n")
		}

		if len(cfg.Body) > 0 {
			// Build body JSON inline using jsontext.Encoder.
			w("\t\t\tBody: func() []byte {\n")
			w("\t\t\t\tvar b bytes.Buffer\n")
			w("\t\t\t\te := jsontext.NewEncoder(&b)\n")
			w("\t\t\t\te.WriteToken(jsontext.BeginObject)\n")
			for _, entry := range cfg.Body {
				w("\t\t\t\te.WriteToken(jsontext.String(%q))\n", entry.Key)
				writeConfigValueToken(w, entry.Value, "req", "\t\t\t\t")
			}
			w("\t\t\t\te.WriteToken(jsontext.EndObject)\n")
			w("\t\t\t\treturn b.Bytes()\n")
			w("\t\t\t}(),\n")
		}

		w("\t\t},\n")
	}

	w("\t}\n")
	w("}\n\n")
}

// writeRootExprTransformBody emits the transform body when the plan has a single root
// expression (and optional bindings). The response body is the marshalled value of
// that expression, not a JSON object.
// Lambdas are forward-declared so mutual recursion (e.g. $sin calling $cos and vice versa) compiles.
func writeRootExprTransformBody(w func(string, ...any), plan *ProviderPlan) {
	em := &exprEmitter{
		w:            w,
		indent:       "\t\t",
		elemVar:      "elem",
		funcVarNames: collectFuncVarNames(plan.RootBindings),
	}

	// Forward-declare only lambdas that are referenced before their definition (e.g. mutual recursion).
	needForwardDeclare := make(map[string]bool)
	for j, b := range plan.RootBindings {
		if b == nil || b.Kind != "assign" || b.Left == nil || b.Left.Kind != "lambda" {
			continue
		}
		for i := 0; i < j; i++ {
			refs := collectVarRefNames(plan.RootBindings[i])
			if refs[b.VarName] {
				needForwardDeclare[b.VarName] = true
				break
			}
		}
	}
	for _, b := range plan.RootBindings {
		if b != nil && b.Kind == "assign" && b.Left != nil && b.Left.Kind == "lambda" && needForwardDeclare[b.VarName] {
			params := make([]string, len(b.Left.LambdaParams))
			for i, p := range b.Left.LambdaParams {
				params[i] = "jsonataParam_" + p + " any"
			}
			w("\t\tvar jsonataVar_%s func(%s) any\n", b.VarName, strings.Join(params, ", "))
		}
	}
	for _, b := range plan.RootBindings {
		if b != nil && b.Kind == "assign" && b.Left != nil && b.Left.Kind == "lambda" {
			useAssign := needForwardDeclare[b.VarName]
			_ = em.emitLambdaToVar("jsonataVar_"+b.VarName, b.Left, useAssign)
		} else {
			_ = em.emit(b)
		}
	}
	// Reference lambdas so the compiler does not report "declared and not used" when they are
	// only referenced inside unsupported code (e.g. $reduce).
	for _, b := range plan.RootBindings {
		if b != nil && b.Kind == "assign" && b.Left != nil && b.Left.Kind == "lambda" {
			w("\t\t_ = jsonataVar_%s\n", b.VarName)
		}
	}

	root := plan.RootExpr
	if root.Kind == "error" {
		if root.ErrorCode != "" {
			w("\treturn nil, runtime.NewHTTPError(%d, %q, %q)\n", root.StatusCode, root.ErrorMessage, root.ErrorCode)
		} else {
			w("\treturn nil, runtime.NewHTTPError(%d, %q)\n", root.StatusCode, root.ErrorMessage)
		}
		return
	}
	if root.Kind == "response" {
		bodyVal := em.emit(root.ResponseBodyExpr)
		w("\tbodyBytes, err := jsonv2.Marshal(%s)\n", bodyVal)
		w("\tif err != nil {\n")
		w("\t\treturn nil, fmt.Errorf(\"marshal response body: %%w\", err)\n")
		w("\t}\n")
		w("\theaders := make(map[string]string)\n")
		if root.ResponseHeaders != nil {
			for headerName, headerExpr := range root.ResponseHeaders {
				headerVal := em.emit(headerExpr)
				w("\theaders[%q] = runtime.ToString(%s)\n", headerName, headerVal)
			}
		}
		w("\treturn &runtime.Response{\n")
		w("\t\tStatusCode: %d,\n", root.ResponseStatusCode)
		w("\t\tHeaders:    headers,\n")
		w("\t\tBody:       bodyBytes,\n")
		w("\t}, nil\n")
		return
	}

	rootVal := em.emit(root)
	w("\tbodyBytes, err := jsonv2.Marshal(%s)\n", rootVal)
	w("\tif err != nil {\n")
	w("\t\treturn nil, fmt.Errorf(\"marshal response body: %%w\", err)\n")
	w("\t}\n")
	w("\treturn &runtime.Response{\n")
	w("\t\tStatusCode: 200,\n")
	w("\t\tHeaders:    map[string]string{\"Content-Type\": \"application/json\"},\n")
	w("\t\tBody:       bodyBytes,\n")
	w("\t}, nil\n")
	w("}\n")
}

// writeRangeMapTransformBody emits the transform body for [a..b].{key: value}:
// bindings, range(start, end), loop over elements with $ as current element, marshal array.
func writeRangeMapTransformBody(w func(string, ...any), plan *ProviderPlan) {
	rm := plan.RangeMap
	em := &exprEmitter{
		w:            w,
		indent:       "\t\t",
		elemVar:      "elem",
		funcVarNames: collectFuncVarNames(plan.RootBindings),
	}

	for _, b := range plan.RootBindings {
		_ = em.emit(b)
	}

	startVal := em.emit(rm.StartExpr)
	endVal := em.emit(rm.EndExpr)
	w("\tarr := runtime.Range(%s, %s)\n", startVal, endVal)
	w("\tresults := make([]map[string]any, 0, len(arr))\n")
	w("\tfor _, elem := range arr {\n")

	emLoop := &exprEmitter{
		w:            w,
		indent:       "\t\t\t",
		elemVar:      "elem",
		contextParam: "elem",
		funcVarNames: collectFuncVarNames(plan.RootBindings),
	}
	w("\t\tm := make(map[string]any)\n")
	for _, of := range rm.OutputFields {
		val := emLoop.emit(of.Value)
		w("\t\tm[%q] = %s\n", of.JSONName, val)
	}
	w("\t\tresults = append(results, m)\n")
	w("\t}\n")

	w("\tbodyBytes, err := jsonv2.Marshal(results)\n")
	w("\tif err != nil {\n")
	w("\t\treturn nil, fmt.Errorf(\"marshal response body: %%w\", err)\n")
	w("\t}\n")
	w("\treturn &runtime.Response{\n")
	w("\t\tStatusCode: 200,\n")
	w("\t\tHeaders:    map[string]string{\"Content-Type\": \"application/json\"},\n")
	w("\t\tBody:       bodyBytes,\n")
	w("\t}, nil\n")
	w("}\n")
}

func writeProviderTransformFunc(w func(string, ...any), plan *ProviderPlan, hasDeps bool) {
	// The transform always accepts RequestContext — it's zero-cost when unused
	// and keeps the signature uniform for the generated handler.
	if hasDeps {
		w("func %s(results map[string][]byte, req runtime.RequestContext) (*runtime.Response, error) {\n", plan.FuncName)
	} else {
		w("func %s(req runtime.RequestContext) (*runtime.Response, error) {\n", plan.FuncName)
	}

	if plan.RootExpr != nil {
		writeRootExprTransformBody(w, plan)
		return
	}

	if plan.RangeMap != nil {
		writeRangeMapTransformBody(w, plan)
		return
	}

	w("\tvar buf bytes.Buffer\n")
	w("\tenc := jsontext.NewEncoder(&buf)\n\n")
	w("\tenc.WriteToken(jsontext.BeginObject)\n\n")

	for _, field := range plan.Fields {
		w("\tenc.WriteToken(jsontext.String(%q))\n", field.OutputKey)

		switch field.Kind {
		case "error":
			// If field is directly an error, return immediately
			w("\t// Error field - return immediately\n")
			w("\tenc.WriteToken(jsontext.EndObject)\n")
			if field.ErrorCode != "" {
				w("\treturn nil, runtime.NewHTTPError(%d, %q, %q)\n", field.StatusCode, field.ErrorMessage, field.ErrorCode)
			} else {
				w("\treturn nil, runtime.NewHTTPError(%d, %q)\n", field.StatusCode, field.ErrorMessage)
			}

		case "response":
			// If field is directly a response, evaluate body and headers, then return
			w("\t// Response field - evaluate and return\n")
			em := &exprEmitter{
				w:            w,
				indent:       "\t\t",
				elemVar:      "elem",
				funcVarNames: collectFuncVarNames(plan.RootBindings),
			}
			if field.BodyExpr != nil {
				bodyVal := em.emit(field.BodyExpr)
				w("\tbodyBytes, err := jsonv2.Marshal(%s)\n", bodyVal)
			} else {
				w("\tbodyBytes, err := jsonv2.Marshal(nil)\n")
			}
			w("\tif err != nil {\n")
			w("\t\treturn nil, fmt.Errorf(\"marshal response body: %%w\", err)\n")
			w("\t}\n")
			w("\theaders := make(map[string]string)\n")
			if field.Headers != nil {
				for headerName, headerExpr := range field.Headers {
					headerVal := em.emit(headerExpr)
					w("\theaders[%q] = runtime.ToString(%s)\n", headerName, headerVal)
				}
			}
			w("\tenc.WriteToken(jsontext.EndObject)\n")
			w("\treturn &runtime.Response{\n")
			w("\t\tStatusCode: %d,\n", field.StatusCode)
			w("\t\tHeaders:    headers,\n")
			w("\t\tBody:       bodyBytes,\n")
			w("\t}, nil\n")

		case "expr":
			// Complex expression - evaluate it and check if result is error/response
			// Use a provider-mode exprEmitter to evaluate the expression
			em := &exprEmitter{
				w:            w,
				indent:       "\t\t",
				elemVar:      "elem", // Not used in provider mode, but needed for interface
				funcVarNames: collectFuncVarNames(plan.RootBindings),
			}
			// Check if expression contains error or response
			switch field.ValueExpr.Kind {
			case "error":
				w("\t// Expression evaluates to error - return immediately\n")
				w("\tenc.WriteToken(jsontext.EndObject)\n")
				if field.ValueExpr.ErrorCode != "" {
					w("\treturn nil, runtime.NewHTTPError(%d, %q, %q)\n", field.ValueExpr.StatusCode, field.ValueExpr.ErrorMessage, field.ValueExpr.ErrorCode)
				} else {
					w("\treturn nil, runtime.NewHTTPError(%d, %q)\n", field.ValueExpr.StatusCode, field.ValueExpr.ErrorMessage)
				}
			case "response":
				w("\t// Expression evaluates to response - return immediately\n")
				// Evaluate body
				bodyVal := em.emit(field.ValueExpr.ResponseBodyExpr)
				w("\tbodyBytes, err := jsonv2.Marshal(%s)\n", bodyVal)
				w("\tif err != nil {\n")
				w("\t\treturn nil, fmt.Errorf(\"marshal response body: %%w\", err)\n")
				w("\t}\n")
				w("\theaders := make(map[string]string)\n")
				if field.ValueExpr.ResponseHeaders != nil {
					for headerName, headerExpr := range field.ValueExpr.ResponseHeaders {
						headerVal := em.emit(headerExpr)
						w("\theaders[%q] = runtime.ToString(%s)\n", headerName, headerVal)
					}
				}
				w("\tenc.WriteToken(jsontext.EndObject)\n")
				w("\treturn &runtime.Response{\n")
				w("\t\tStatusCode: %d,\n", field.ValueExpr.ResponseStatusCode)
				w("\t\tHeaders:    headers,\n")
				w("\t\tBody:       bodyBytes,\n")
				w("\t}, nil\n")
			case "conditional":
				// Handle conditional - check if branches are error/response
				w("\t// Conditional expression - evaluate and check branches\n")
				condVal := em.emit(field.ValueExpr.Cond)
				if field.ValueExpr.Then.Kind == "error" {
					w("\tif runtime.Truthy(%s) {\n", condVal)
					w("\t\tenc.WriteToken(jsontext.EndObject)\n")
					if field.ValueExpr.Then.ErrorCode != "" {
						w("\t\treturn nil, runtime.NewHTTPError(%d, %q, %q)\n", field.ValueExpr.Then.StatusCode, field.ValueExpr.Then.ErrorMessage, field.ValueExpr.Then.ErrorCode)
					} else {
						w("\t\treturn nil, runtime.NewHTTPError(%d, %q)\n", field.ValueExpr.Then.StatusCode, field.ValueExpr.Then.ErrorMessage)
					}
					w("\t}\n")
					// Evaluate else branch
					if field.ValueExpr.Else != nil {
						switch field.ValueExpr.Else.Kind {
						case "error":
							w("\tenc.WriteToken(jsontext.EndObject)\n")
							if field.ValueExpr.Else.ErrorCode != "" {
								w("\treturn nil, runtime.NewHTTPError(%d, %q, %q)\n", field.ValueExpr.Else.StatusCode, field.ValueExpr.Else.ErrorMessage, field.ValueExpr.Else.ErrorCode)
							} else {
								w("\treturn nil, runtime.NewHTTPError(%d, %q)\n", field.ValueExpr.Else.StatusCode, field.ValueExpr.Else.ErrorMessage)
							}
						case "response":
							bodyVal := em.emit(field.ValueExpr.Else.ResponseBodyExpr)
							w("\tbodyBytes, err := jsonv2.Marshal(%s)\n", bodyVal)
							w("\tif err != nil {\n")
							w("\t\treturn nil, fmt.Errorf(\"marshal response body: %%w\", err)\n")
							w("\t}\n")
							w("\theaders := make(map[string]string)\n")
							if field.ValueExpr.Else.ResponseHeaders != nil {
								for headerName, headerExpr := range field.ValueExpr.Else.ResponseHeaders {
									headerVal := em.emit(headerExpr)
									w("\theaders[%q] = runtime.ToString(%s)\n", headerName, headerVal)
								}
							}
							w("\tenc.WriteToken(jsontext.EndObject)\n")
							w("\treturn &runtime.Response{StatusCode: %d, Headers: headers, Body: bodyBytes}, nil\n", field.ValueExpr.Else.ResponseStatusCode)
						default:
							elseVal := em.emit(field.ValueExpr.Else)
							w("\tenc.WriteValue(%s)\n\n", elseVal)
						}
					}
				} else if field.ValueExpr.Else != nil && field.ValueExpr.Else.Kind == "error" {
					// cond ? normal : $httpError(...)
					w("\tif runtime.Truthy(%s) {\n", condVal)
					thenVal := em.emit(field.ValueExpr.Then)
					w("\t\tbodyBytes, err := jsonv2.Marshal(%s)\n", thenVal)
					w("\t\tif err != nil {\n")
					w("\t\t\treturn nil, fmt.Errorf(\"marshal: %%w\", err)\n")
					w("\t\t}\n")
					w("\t\tenc.WriteValue(jsontext.Value(bodyBytes))\n")
					w("\t} else {\n")
					w("\t\tenc.WriteToken(jsontext.EndObject)\n")
					if field.ValueExpr.Else.ErrorCode != "" {
						w("\t\treturn nil, runtime.NewHTTPError(%d, %q, %q)\n", field.ValueExpr.Else.StatusCode, field.ValueExpr.Else.ErrorMessage, field.ValueExpr.Else.ErrorCode)
					} else {
						w("\t\treturn nil, runtime.NewHTTPError(%d, %q)\n", field.ValueExpr.Else.StatusCode, field.ValueExpr.Else.ErrorMessage)
					}
					w("\t}\n")
				} else if field.ValueExpr.Else != nil && field.ValueExpr.Else.Kind == "response" {
					// cond ? normal : $httpResponse(...)
					w("\tif runtime.Truthy(%s) {\n", condVal)
					thenVal := em.emit(field.ValueExpr.Then)
					w("\t\tbodyBytes, err := jsonv2.Marshal(%s)\n", thenVal)
					w("\t\tif err != nil {\n")
					w("\t\t\treturn nil, fmt.Errorf(\"marshal: %%w\", err)\n")
					w("\t\t}\n")
					w("\t\tenc.WriteValue(jsontext.Value(bodyBytes))\n")
					w("\t} else {\n")
					bodyVal := em.emit(field.ValueExpr.Else.ResponseBodyExpr)
					w("\t\tbodyBytes, err := jsonv2.Marshal(%s)\n", bodyVal)
					w("\t\tif err != nil {\n")
					w("\t\t\treturn nil, fmt.Errorf(\"marshal response body: %%w\", err)\n")
					w("\t\t}\n")
					w("\t\theaders := make(map[string]string)\n")
					if field.ValueExpr.Else.ResponseHeaders != nil {
						for headerName, headerExpr := range field.ValueExpr.Else.ResponseHeaders {
							headerVal := em.emit(headerExpr)
							w("\t\theaders[%q] = runtime.ToString(%s)\n", headerName, headerVal)
						}
					}
					w("\t\tenc.WriteToken(jsontext.EndObject)\n")
					w("\t\treturn &runtime.Response{StatusCode: %d, Headers: headers, Body: bodyBytes}, nil\n", field.ValueExpr.Else.ResponseStatusCode)
					w("\t}\n")
				} else if field.ValueExpr.Then.Kind == "response" {
					w("\tif runtime.Truthy(%s) {\n", condVal)
					bodyVal := em.emit(field.ValueExpr.Then.ResponseBodyExpr)
					w("\t\tbodyBytes, err := jsonv2.Marshal(%s)\n", bodyVal)
					w("\t\tif err != nil {\n")
					w("\t\t\treturn nil, fmt.Errorf(\"marshal response body: %%w\", err)\n")
					w("\t\t}\n")
					w("\t\theaders := make(map[string]string)\n")
					if field.ValueExpr.Then.ResponseHeaders != nil {
						for headerName, headerExpr := range field.ValueExpr.Then.ResponseHeaders {
							headerVal := em.emit(headerExpr)
							w("\t\theaders[%q] = runtime.ToString(%s)\n", headerName, headerVal)
						}
					}
					w("\t\tenc.WriteToken(jsontext.EndObject)\n")
					w("\t\treturn &runtime.Response{StatusCode: %d, Headers: headers, Body: bodyBytes}, nil\n", field.ValueExpr.Then.ResponseStatusCode)
					w("\t}\n")
					if field.ValueExpr.Else != nil {
						switch field.ValueExpr.Else.Kind {
						case "error":
							w("\tenc.WriteToken(jsontext.EndObject)\n")
							if field.ValueExpr.Else.ErrorCode != "" {
								w("\treturn nil, runtime.NewHTTPError(%d, %q, %q)\n", field.ValueExpr.Else.StatusCode, field.ValueExpr.Else.ErrorMessage, field.ValueExpr.Else.ErrorCode)
							} else {
								w("\treturn nil, runtime.NewHTTPError(%d, %q)\n", field.ValueExpr.Else.StatusCode, field.ValueExpr.Else.ErrorMessage)
							}
						case "response":
							bodyVal := em.emit(field.ValueExpr.Else.ResponseBodyExpr)
							w("\tbodyBytes, err := jsonv2.Marshal(%s)\n", bodyVal)
							w("\tif err != nil {\n")
							w("\t\treturn nil, fmt.Errorf(\"marshal response body: %%w\", err)\n")
							w("\t}\n")
							w("\theaders := make(map[string]string)\n")
							if field.ValueExpr.Else.ResponseHeaders != nil {
								for headerName, headerExpr := range field.ValueExpr.Else.ResponseHeaders {
									headerVal := em.emit(headerExpr)
									w("\theaders[%q] = runtime.ToString(%s)\n", headerName, headerVal)
								}
							}
							w("\tenc.WriteToken(jsontext.EndObject)\n")
							w("\treturn &runtime.Response{StatusCode: %d, Headers: headers, Body: bodyBytes}, nil\n", field.ValueExpr.Else.ResponseStatusCode)
						default:
							elseVal := em.emit(field.ValueExpr.Else)
							w("\tenc.WriteValue(%s)\n\n", elseVal)
						}
					}
				} else {
					// Normal conditional - evaluate normally
					exprVal := em.emitConditional(field.ValueExpr)
					w("\tenc.WriteValue(%s)\n\n", exprVal)
				}
			default:
				// Normal expression - evaluate and write value
				exprVal := em.emit(field.ValueExpr)
				w("\tenc.WriteValue(%s)\n\n", exprVal)
			}
		case "fetch":
			depKey := field.Provider + "." + field.Endpoint
			if len(field.JSONPath) > 0 {
				pathArgs := ""
				for _, p := range field.JSONPath {
					pathArgs += fmt.Sprintf(", %q", p)
				}
				varName := "val" + exportedName(field.OutputKey)
				w("\t%s, err := runtime.ExtractPath(results[%q]%s)\n", varName, depKey, pathArgs)
				w("\tif err != nil {\n")
				w("\t\treturn nil, fmt.Errorf(\"extracting %s.%s.%s: %%w\", err)\n",
					field.Provider, field.Endpoint, strings.Join(field.JSONPath, "."))
				w("\t}\n")
				w("\tenc.WriteValue(%s)\n\n", varName)
			} else {
				w("\tenc.WriteValue(results[%q])\n\n", depKey)
			}

		case "header", "cookie", "query", "param":
			mapName := requestFuncToMap(field.Kind)
			w("\tenc.WriteToken(jsontext.String(req.%s[%q]))\n\n", mapName, field.Arg)

		case "path":
			w("\tenc.WriteToken(jsontext.String(req.Path))\n\n")

		case "method":
			w("\tenc.WriteToken(jsontext.String(req.Method))\n\n")

		case "body":
			if len(field.BodyPath) > 0 {
				pathArgs := ""
				for _, p := range field.BodyPath {
					pathArgs += fmt.Sprintf(", %q", p)
				}
				varName := "val" + exportedName(field.OutputKey)
				w("\t%s, err := runtime.ExtractPath(req.Body%s)\n", varName, pathArgs)
				w("\tif err != nil {\n")
				w("\t\treturn nil, fmt.Errorf(\"extracting body.%s: %%w\", err)\n",
					strings.Join(field.BodyPath, "."))
				w("\t}\n")
				w("\tenc.WriteValue(%s)\n\n", varName)
			} else {
				w("\tenc.WriteValue(req.Body)\n\n")
			}

		case "serviceParam":
			w("\tenc.WriteValue(%s)\n\n", lookupValueExpr("req.ServiceParams", field.ParamsPath))

		case "service":
			svcKey := field.ServiceResultKey
			if len(field.JSONPath) > 0 {
				pathArgs := ""
				for _, p := range field.JSONPath {
					pathArgs += fmt.Sprintf(", %q", p)
				}
				varName := "val" + exportedName(field.OutputKey)
				w("\t%s, err := runtime.ExtractPath(results[%q]%s)\n", varName, svcKey, pathArgs)
				w("\tif err != nil {\n")
				w("\t\treturn nil, fmt.Errorf(\"extracting %s.%s: %%w\", err)\n",
					field.ServiceName, strings.Join(field.JSONPath, "."))
				w("\t}\n")
				w("\tenc.WriteValue(%s)\n\n", varName)
			} else {
				w("\tenc.WriteValue(results[%q])\n\n", svcKey)
			}

		case "static":
			w("\tenc.WriteToken(jsontext.String(%q))\n\n", field.StaticValue)
		}
	}

	w("\tenc.WriteToken(jsontext.EndObject)\n")
	w("\t// Wrap normal result in 200 OK response\n")
	w("\treturn &runtime.Response{\n")
	w("\t\tStatusCode: 200,\n")
	w("\t\tHeaders:    map[string]string{\"Content-Type\": \"application/json\"},\n")
	w("\t\tBody:       buf.Bytes(),\n")
	w("\t}, nil\n")
	w("}\n")
}

// writeExecuteFunc emits a function that bundles the full service pipeline:
// 1. Build deps and fetch all providers in parallel
// 2. Run all sub-services in parallel
// 3. Call the transform with the combined results
func writeExecuteFunc(w func(string, ...any), plan *ProviderPlan) {
	baseName := strings.TrimPrefix(plan.FuncName, "Transform")
	hasDeps := len(plan.Deps) > 0
	hasServices := len(plan.ServiceCalls) > 0

	w("func Execute%s(ctx context.Context, agg *aggregator.Aggregator, req runtime.RequestContext) (*runtime.Response, error) {\n", baseName)

	if hasDeps {
		w("\tdeps := %sDeps(req)\n", plan.FuncName)
		w("\tresults, err := agg.Fetch(ctx, deps)\n")
		w("\tif err != nil {\n")
		w("\t\treturn nil, fmt.Errorf(\"%s: fetch: %%w\", err)\n", baseName)
		w("\t}\n\n")
	} else {
		w("\tresults := make(map[string][]byte)\n\n")
	}

	if hasServices {
		w("\tg, gctx := errgroup.WithContext(ctx)\n")
		w("\tvar mu sync.Mutex\n\n")
		em := &exprEmitter{w: w, indent: "\t\t", elemVar: "elem"}
		for _, svc := range plan.ServiceCalls {
			execFn := "Execute" + exportedName(svc.ServiceName)
			svcName := svc.ServiceName
			svcKey := svc.ResultKey
			w("\tg.Go(func() error {\n")
			w("\t\tchildReq := req\n")
			w("\t\tchildReq.ServiceParams = nil\n")
			if svc.ParamsExpr != nil {
				paramValue := em.emit(svc.ParamsExpr)
				w("\t\tchildReq.ServiceParams = %s\n", paramValue)
			}
			w("\t\tr, err := %s(gctx, agg, childReq)\n", execFn)
			w("\t\tif err != nil {\n")
			w("\t\t\treturn fmt.Errorf(\"service %s: %%w\", err)\n", svcName)
			w("\t\t}\n")
			w("\t\tif r == nil {\n")
			w("\t\t\treturn fmt.Errorf(\"service %s: empty response\")\n", svcName)
			w("\t\t}\n")
			w("\t\tmu.Lock()\n")
			w("\t\tresults[%q] = r.Body\n", svcKey)
			w("\t\tmu.Unlock()\n")
			w("\t\treturn nil\n")
			w("\t})\n\n")
		}
		w("\tif err := g.Wait(); err != nil {\n")
		w("\t\treturn nil, err\n")
		w("\t}\n\n")
	}

	w("\tresp, err := %s(results, req)\n", plan.FuncName)
	w("\tif err != nil {\n")
	w("\t\treturn nil, err\n")
	w("\t}\n")
	w("\treturn resp, nil\n")
	w("}\n\n")
}

// configValueToGoExpr returns a Go expression string for a ConfigValue.
func configValueToGoExpr(cv ConfigValue, reqVar string) string {
	switch cv.Kind {
	case "static":
		return fmt.Sprintf("%q", cv.Static)
	case "header", "cookie", "query", "param":
		return fmt.Sprintf("%s.%s[%q]", reqVar, requestFuncToMap(cv.Kind), cv.Arg)
	case "path":
		return reqVar + ".Path"
	case "method":
		return reqVar + ".Method"
	case "serviceParam":
		return fmt.Sprintf("runtime.ToString(%s)", lookupValueExpr(reqVar+".ServiceParams", cv.Path))
	default:
		return `""`
	}
}

// writeConfigValueToken writes the code to emit a single config value into
// a jsontext.Encoder. Used for building $fetch() body JSON.
func writeConfigValueToken(w func(string, ...any), cv ConfigValue, reqVar, indent string) {
	switch cv.Kind {
	case "static":
		w("%se.WriteToken(jsontext.String(%q))\n", indent, cv.Static)
	case "header", "cookie", "query", "param":
		mapName := requestFuncToMap(cv.Kind)
		w("%se.WriteToken(jsontext.String(%s.%s[%q]))\n", indent, reqVar, mapName, cv.Arg)
	case "path":
		w("%se.WriteToken(jsontext.String(%s.Path))\n", indent, reqVar)
	case "method":
		w("%se.WriteToken(jsontext.String(%s.Method))\n", indent, reqVar)
	case "body":
		if len(cv.Path) > 0 {
			pathArgs := ""
			for _, p := range cv.Path {
				pathArgs += fmt.Sprintf(", %q", p)
			}
			w("%sbodyVal, _ := runtime.ExtractPath(%s.Body%s)\n", indent, reqVar, pathArgs)
			w("%se.WriteValue(bodyVal)\n", indent)
		} else {
			w("%se.WriteValue(%s.Body)\n", indent, reqVar)
		}
	case "serviceParam":
		w("%se.WriteValue(%s)\n", indent, lookupValueExpr(reqVar+".ServiceParams", cv.Path))
	}
}

func requestFuncToMap(kind string) string {
	switch kind {
	case "header":
		return "Headers"
	case "cookie":
		return "Cookies"
	case "query":
		return "Query"
	case "param":
		return "Params"
	default:
		return "Headers"
	}
}

func lookupJSONExpr(dataExpr string, path []string) string {
	if len(path) == 0 {
		return fmt.Sprintf("runtime.LookupJSON(%s)", dataExpr)
	}

	pathArgs := make([]string, len(path))
	for i, part := range path {
		pathArgs[i] = fmt.Sprintf("%q", part)
	}
	return fmt.Sprintf("runtime.LookupJSON(%s, %s)", dataExpr, strings.Join(pathArgs, ", "))
}

func lookupValueExpr(dataExpr string, path []string) string {
	if len(path) == 0 {
		return dataExpr
	}

	pathArgs := make([]string, len(path))
	for i, part := range path {
		pathArgs[i] = fmt.Sprintf("%q", part)
	}
	return fmt.Sprintf("runtime.LookupPath(%s, %s)", dataExpr, strings.Join(pathArgs, ", "))
}

// unexportedName returns a lowercase version of a Go name for local variables/types.
func unexportedName(s string) string {
	if s == "" {
		return s
	}
	// Handle all-caps names like "ID" -> "id"
	if strings.ToUpper(s) == s {
		return strings.ToLower(s)
	}
	runes := []rune(s)
	runes[0] = rune(strings.ToLower(string(runes[0]))[0])
	return string(runes)
}
