package main

import (
	"fmt"
	"strings"
)

// generateValidator generates a validation function for a route.
func generateValidator(method string, path string, schema *RequestSchema) string {
	if schema == nil || !schema.HasSchema() {
		return ""
	}

	var buf strings.Builder
	// Generate function name from method and path
	// e.g., GET /dashboard -> ValidateGetDashboardRequest
	// e.g., POST /api/v1/users -> ValidatePostApiV1UsersRequest
	routeName := sanitizeRouteName(path)
	funcName := "Validate" + exportedName(method) + routeName + "Request"

	buf.WriteString(fmt.Sprintf("func %s(reqCtx runtime.RequestContext) error {\n", funcName))

	// Generate header validation
	if len(schema.Header) > 0 {
		buf.WriteString(generateHeaderValidation(schema.Header))
	}

	// Generate query validation
	if len(schema.Query) > 0 {
		buf.WriteString(generateQueryValidation(schema.Query))
	}

	// Generate path validation
	if len(schema.Path) > 0 {
		buf.WriteString(generatePathValidation(schema.Path))
	}

	// Generate body validation
	if schema.Body != nil {
		buf.WriteString(generateBodyValidation(schema.Body))
	}

	buf.WriteString("\treturn nil\n")
	buf.WriteString("}\n\n")

	return buf.String()
}

// generateHeaderValidation generates validation code for header parameters.
func generateHeaderValidation(params []Parameter) string {
	var buf strings.Builder
	buf.WriteString("\t// Header validation\n")

	for _, param := range params {
		paramName := param.Name
		if param.Required {
			buf.WriteString(fmt.Sprintf("\tif reqCtx.Headers == nil {\n"))
			buf.WriteString(fmt.Sprintf("\t\treturn fmt.Errorf(\"required header '%s' is missing\")\n", paramName))
			buf.WriteString("\t}\n")
			buf.WriteString(fmt.Sprintf("\tval, ok := reqCtx.Headers[%q]\n", paramName))
			buf.WriteString("\tif !ok || val == \"\" {\n")
			buf.WriteString(fmt.Sprintf("\t\treturn fmt.Errorf(\"required header '%s' is missing\")\n", paramName))
			buf.WriteString("\t}\n")
		} else {
			buf.WriteString(fmt.Sprintf("\tif reqCtx.Headers != nil {\n"))
			buf.WriteString(fmt.Sprintf("\t\tif val, ok := reqCtx.Headers[%q]; ok && val != \"\" {\n", paramName))
		}

		// Add constraint validation if needed
		buf.WriteString(generateParameterConstraints("val", param.Schema, "\t\t"))

		if !param.Required {
			buf.WriteString("\t\t}\n")
			buf.WriteString("\t}\n")
		}
	}

	return buf.String()
}

// generateQueryValidation generates validation code for query parameters.
func generateQueryValidation(params []Parameter) string {
	var buf strings.Builder
	buf.WriteString("\t// Query validation\n")

	for _, param := range params {
		paramName := param.Name
		if param.Required {
			buf.WriteString(fmt.Sprintf("\tif reqCtx.Query == nil {\n"))
			buf.WriteString(fmt.Sprintf("\t\treturn fmt.Errorf(\"required query parameter '%s' is missing\")\n", paramName))
			buf.WriteString("\t}\n")
			buf.WriteString(fmt.Sprintf("\tval, ok := reqCtx.Query[%q]\n", paramName))
			buf.WriteString("\tif !ok || val == \"\" {\n")
			buf.WriteString(fmt.Sprintf("\t\treturn fmt.Errorf(\"required query parameter '%s' is missing\")\n", paramName))
			buf.WriteString("\t}\n")
		} else {
			buf.WriteString(fmt.Sprintf("\tif reqCtx.Query != nil {\n"))
			buf.WriteString(fmt.Sprintf("\t\tif val, ok := reqCtx.Query[%q]; ok && val != \"\" {\n", paramName))
		}

		// Type conversion and validation
		buf.WriteString(generateQueryParameterValidation("val", param.Schema, paramName, "\t\t"))

		if !param.Required {
			buf.WriteString("\t\t}\n")
			buf.WriteString("\t}\n")
		}
	}

	return buf.String()
}

// generatePathValidation generates validation code for path parameters.
func generatePathValidation(params []Parameter) string {
	var buf strings.Builder
	buf.WriteString("\t// Path validation\n")

	for _, param := range params {
		paramName := param.Name
		buf.WriteString(fmt.Sprintf("\tif reqCtx.Params == nil {\n"))
		buf.WriteString(fmt.Sprintf("\t\treturn fmt.Errorf(\"required path parameter '%s' is missing\")\n", paramName))
		buf.WriteString("\t}\n")
		buf.WriteString(fmt.Sprintf("\tval, ok := reqCtx.Params[%q]\n", paramName))
		buf.WriteString("\tif !ok || val == \"\" {\n")
		buf.WriteString(fmt.Sprintf("\t\treturn fmt.Errorf(\"required path parameter '%s' is missing\")\n", paramName))
		buf.WriteString("\t}\n")

		// Type conversion and validation
		buf.WriteString(generateQueryParameterValidation("val", param.Schema, paramName, "\t"))
	}

	return buf.String()
}

// generateQueryParameterValidation generates validation for query/path parameters with type conversion.
func generateQueryParameterValidation(varName string, schema SchemaInfo, paramName string, indent string) string {
	var buf strings.Builder

	switch schema.Type {
	case "integer":
		buf.WriteString(fmt.Sprintf("%sintVal, err := strconv.Atoi(%s)\n", indent, varName))
		buf.WriteString(fmt.Sprintf("%sif err != nil {\n", indent))
		buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"query parameter '%s' must be an integer: %%w\", err)\n", indent, paramName))
		buf.WriteString(fmt.Sprintf("%s}\n", indent))
		buf.WriteString(generateParameterConstraints("intVal", schema, indent))
	case "number":
		buf.WriteString(fmt.Sprintf("%sfloatVal, err := strconv.ParseFloat(%s, 64)\n", indent, varName))
		buf.WriteString(fmt.Sprintf("%sif err != nil {\n", indent))
		buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"query parameter '%s' must be a number: %%w\", err)\n", indent, paramName))
		buf.WriteString(fmt.Sprintf("%s}\n", indent))
		buf.WriteString(generateParameterConstraints("floatVal", schema, indent))
	case "boolean":
		buf.WriteString(fmt.Sprintf("%sboolVal, err := strconv.ParseBool(%s)\n", indent, varName))
		buf.WriteString(fmt.Sprintf("%sif err != nil {\n", indent))
		buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"query parameter '%s' must be a boolean: %%w\", err)\n", indent, paramName))
		buf.WriteString(fmt.Sprintf("%s}\n", indent))
		// Boolean doesn't have constraints typically
	default: // string
		buf.WriteString(generateParameterConstraints(varName, schema, indent))
	}

	return buf.String()
}

// generateParameterConstraints generates constraint validation code.
func generateParameterConstraints(varName string, schema SchemaInfo, indent string) string {
	var buf strings.Builder
	c := schema.Constraints

	if schema.Type == "string" {
		if c.MinLength != nil {
			buf.WriteString(fmt.Sprintf("%sif len(%s) < %d {\n", indent, varName, *c.MinLength))
			buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"field must be at least %d characters\")\n", indent, *c.MinLength))
			buf.WriteString(fmt.Sprintf("%s}\n", indent))
		}
		if c.MaxLength != nil {
			buf.WriteString(fmt.Sprintf("%sif len(%s) > %d {\n", indent, varName, *c.MaxLength))
			buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"field must be at most %d characters\")\n", indent, *c.MaxLength))
			buf.WriteString(fmt.Sprintf("%s}\n", indent))
		}
		if c.Pattern != "" {
			buf.WriteString(fmt.Sprintf("%smatched, err := regexp.MatchString(%q, %s)\n", indent, c.Pattern, varName))
			buf.WriteString(fmt.Sprintf("%sif err != nil || !matched {\n", indent))
			buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"field does not match required pattern\")\n", indent))
			buf.WriteString(fmt.Sprintf("%s}\n", indent))
		}
		if len(c.Enum) > 0 {
			buf.WriteString(fmt.Sprintf("%sswitch %s {\n", indent, varName))
			for _, enumVal := range c.Enum {
				enumStr := fmt.Sprintf("%v", enumVal)
				buf.WriteString(fmt.Sprintf("%scase %q:\n", indent, enumStr))
			}
			buf.WriteString(fmt.Sprintf("%sdefault:\n", indent))
			buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"field must be one of: ", indent))
			enumStrs := make([]string, len(c.Enum))
			for i, e := range c.Enum {
				enumStrs[i] = fmt.Sprintf("%v", e)
			}
			buf.WriteString(strings.Join(enumStrs, ", "))
			buf.WriteString("\")\n")
			buf.WriteString(fmt.Sprintf("%s}\n", indent))
		}
		// Format validation
		if schema.Format == "email" {
			buf.WriteString(fmt.Sprintf("%sif !strings.Contains(%s, \"@\") {\n", indent, varName))
			buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"field must be a valid email address\")\n", indent))
			buf.WriteString(fmt.Sprintf("%s}\n", indent))
		}
	} else if schema.Type == "integer" || schema.Type == "number" {
		if c.Minimum != nil {
			comp := "float64(" + varName + ")"
			if schema.Type == "integer" {
				comp = varName
			}
			buf.WriteString(fmt.Sprintf("%sif %s < %v {\n", indent, comp, *c.Minimum))
			buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"field must be at least %v\")\n", indent, *c.Minimum))
			buf.WriteString(fmt.Sprintf("%s}\n", indent))
		}
		if c.Maximum != nil {
			comp := "float64(" + varName + ")"
			if schema.Type == "integer" {
				comp = varName
			}
			buf.WriteString(fmt.Sprintf("%sif %s > %v {\n", indent, comp, *c.Maximum))
			buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"field must be at most %v\")\n", indent, *c.Maximum))
			buf.WriteString(fmt.Sprintf("%s}\n", indent))
		}
	}

	return buf.String()
}

// generateBodyValidation generates validation code for request body.
func generateBodyValidation(bodySchema *BodySchema) string {
	var buf strings.Builder
	buf.WriteString("\t// Body validation\n")

	if bodySchema.Required {
		buf.WriteString("\tif reqCtx.Body == nil || len(reqCtx.Body) == 0 {\n")
		buf.WriteString("\t\treturn fmt.Errorf(\"request body is required\")\n")
		buf.WriteString("\t}\n")
	}

	buf.WriteString("\tif reqCtx.Body != nil && len(reqCtx.Body) > 0 {\n")
	buf.WriteString(generateBodySchemaValidation(bodySchema.Schema, "\t\t", ""))
	buf.WriteString("\t}\n")

	return buf.String()
}

// generateBodySchemaValidation generates validation for JSON body schema.
func generateBodySchemaValidation(schema SchemaInfo, indent string, pathPrefix string) string {
	var buf strings.Builder

	// For now, we'll do basic validation. Full streaming validation would be more complex.
	// This validates required fields and basic structure.
	if schema.Type == "object" && len(schema.Required) > 0 {
		buf.WriteString(fmt.Sprintf("%s// Validate required fields\n", indent))
		buf.WriteString(fmt.Sprintf("%sdec := jsontext.NewDecoder(bytes.NewReader(reqCtx.Body))\n", indent))
		buf.WriteString(fmt.Sprintf("%stok, err := dec.ReadToken()\n", indent))
		buf.WriteString(fmt.Sprintf("%sif err != nil || tok.Kind() != '{' {\n", indent))
		buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"request body must be a JSON object\")\n", indent))
		buf.WriteString(fmt.Sprintf("%s}\n", indent))

		buf.WriteString(fmt.Sprintf("%sfoundFields := make(map[string]bool)\n", indent))
		buf.WriteString(fmt.Sprintf("%sfor dec.PeekKind() != '}' {\n", indent))
		buf.WriteString(fmt.Sprintf("%s\tnameTok, err := dec.ReadToken()\n", indent))
		buf.WriteString(fmt.Sprintf("%s\tif err != nil {\n", indent))
		buf.WriteString(fmt.Sprintf("%s\t\treturn fmt.Errorf(\"error reading JSON: %%w\", err)\n", indent))
		buf.WriteString(fmt.Sprintf("%s\t}\n", indent))
		buf.WriteString(fmt.Sprintf("%s\tfieldName := nameTok.String()\n", indent))
		buf.WriteString(fmt.Sprintf("%s\tfoundFields[fieldName] = true\n", indent))
		buf.WriteString(fmt.Sprintf("%s\tif err := dec.SkipValue(); err != nil {\n", indent))
		buf.WriteString(fmt.Sprintf("%s\t\treturn fmt.Errorf(\"error skipping JSON value: %%w\", err)\n", indent))
		buf.WriteString(fmt.Sprintf("%s\t}\n", indent))
		buf.WriteString(fmt.Sprintf("%s}\n", indent))

		for _, reqField := range schema.Required {
			fieldPath := pathPrefix
			if fieldPath != "" {
				fieldPath += "."
			}
			fieldPath += reqField
			buf.WriteString(fmt.Sprintf("%sif !foundFields[%q] {\n", indent, reqField))
			buf.WriteString(fmt.Sprintf("%s\treturn fmt.Errorf(\"required field '%s' is missing\")\n", indent, fieldPath))
			buf.WriteString(fmt.Sprintf("%s}\n", indent))
		}
	}

	return buf.String()
}

// sanitizeRouteName converts a route path to a valid Go identifier.
// /dashboard -> Dashboard
// /api/v1/users -> ApiV1Users
func sanitizeRouteName(path string) string {
	// Remove leading/trailing slashes
	path = strings.Trim(path, "/")
	if path == "" {
		return "Root"
	}

	// Split by / and convert each part
	parts := strings.Split(path, "/")
	var result strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		// Remove special characters, convert to exported name
		cleanPart := strings.ReplaceAll(part, "-", "_")
		cleanPart = strings.ReplaceAll(cleanPart, ".", "_")
		result.WriteString(exportedName(cleanPart))
	}

	return result.String()
}
