package main

// RequestSchema represents the complete request schema for an OpenAPI operation.
type RequestSchema struct {
	Body   *BodySchema  // requestBody schema
	Query  []Parameter  // query parameters
	Path   []Parameter  // path parameters
	Header []Parameter  // header parameters
}

// BodySchema represents a request body schema.
type BodySchema struct {
	Required bool
	Schema   SchemaInfo
}

// Parameter represents a single OpenAPI parameter (query, path, header).
type Parameter struct {
	Name     string
	Required bool
	Schema   SchemaInfo
}

// SchemaInfo represents a JSON Schema definition.
type SchemaInfo struct {
	Type       string                 // "string", "number", "integer", "boolean", "array", "object"
	Format     string                 // "email", "date", etc.
	Required   []string               // for objects: list of required field names
	Properties map[string]SchemaInfo  // for objects: field definitions
	Items      *SchemaInfo            // for arrays: item schema
	Constraints Constraints            // minLength, maxLength, minimum, maximum, pattern, enum
	Ref        string                 // for $ref: the reference path (e.g., "#/components/schemas/User")
}

// Constraints represents JSON Schema validation constraints.
type Constraints struct {
	MinLength *int
	MaxLength *int
	Minimum   *float64
	Maximum   *float64
	Pattern   string
	Enum      []interface{}
}

// HasSchema returns true if this RequestSchema has any validation rules.
func (rs *RequestSchema) HasSchema() bool {
	if rs == nil {
		return false
	}
	return rs.Body != nil || len(rs.Query) > 0 || len(rs.Path) > 0 || len(rs.Header) > 0
}
