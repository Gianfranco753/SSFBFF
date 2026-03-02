//go:build goexperiment.jsonv2

// Package runtime provides helper functions and types used by generated code.
//
// It contains two categories of helpers:
//   - Aggregation functions (SumFloat64, CountFloat64, etc.) for filter+projection transforms
//   - Provider types and ExtractPath for multi-provider transforms that pull
//     values from pre-fetched upstream JSON responses
package runtime

import (
	"bytes"
	"encoding/json/jsontext"
	"fmt"
)

// RequestContext holds data from the incoming HTTP request, populated by the
// Fiber handler before calling the generated transform functions. Generated
// code accesses specific keys — e.g. req.Headers["Authorization"] — so the
// maps only need to contain the entries that the JSONata expression references.
type RequestContext struct {
	Cookies map[string]string
	Headers map[string]string
	Query   map[string]string
	Params  map[string]string
	Path    string
	Method  string
	Body    []byte
}

// ProviderDep identifies an upstream service endpoint that must be fetched
// before a transform function can run. The aggregator uses this to build
// the fan-out plan. Method/Headers/Body are optional and allow $fetch()
// configs to shape the outgoing request (e.g. POST with a custom body).
type ProviderDep struct {
	Provider string // e.g. "user_service"
	Endpoint string // e.g. "profile"
	Method   string            // HTTP method (default "GET")
	Headers  map[string]string // custom headers for this upstream call
	Body     []byte            // request body (pre-serialized JSON)
}

// Key returns the map key used to store/retrieve fetched data for this dep.
func (d ProviderDep) Key() string {
	return d.Provider + "." + d.Endpoint
}

// ExtractPath navigates into a JSON document and returns the raw value at the
// given path. It streams through the JSON with jsontext.Decoder so it never
// allocates a map[string]any for the entire document.
//
// Example: ExtractPath(data, "user", "name") finds {"user":{"name":"Alice"}}
// and returns the raw JSON value "Alice".
func ExtractPath(data []byte, path ...string) (jsontext.Value, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data))

	for i, key := range path {
		// We expect an object at each level of the path.
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("reading object at path[%d] %q: %w", i, key, err)
		}
		if tok.Kind() != '{' {
			return nil, fmt.Errorf("expected object at path[%d] %q, got %v", i, key, tok.Kind())
		}

		found := false
		for dec.PeekKind() != '}' {
			nameTok, err := dec.ReadToken()
			if err != nil {
				return nil, fmt.Errorf("reading field name at path[%d]: %w", i, err)
			}

			if nameTok.String() != key {
				if err := dec.SkipValue(); err != nil {
					return nil, fmt.Errorf("skipping field at path[%d]: %w", i, err)
				}
				continue
			}

			found = true
			break
		}

		if !found {
			return nil, fmt.Errorf("field %q not found at path[%d]", key, i)
		}

		// If this is the last key, read and return the value.
		if i == len(path)-1 {
			val, err := dec.ReadValue()
			if err != nil {
				return nil, fmt.Errorf("reading value for %q: %w", key, err)
			}
			return val, nil
		}
		// Otherwise, the next iteration will read the '{' of the nested object.
	}

	// Empty path — return the entire document as-is.
	return jsontext.Value(data), nil
}

// SumFloat64 returns the sum of all values in the slice.
func SumFloat64(values []float64) float64 {
	var total float64
	for _, v := range values {
		total += v
	}
	return total
}

// CountFloat64 returns the number of elements in the slice.
func CountFloat64(values []float64) float64 {
	return float64(len(values))
}

// MinFloat64 returns the minimum value in the slice, or 0 if empty.
func MinFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	min := values[0]
	for _, v := range values[1:] {
		if v < min {
			min = v
		}
	}
	return min
}

// MaxFloat64 returns the maximum value in the slice, or 0 if empty.
func MaxFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	max := values[0]
	for _, v := range values[1:] {
		if v > max {
			max = v
		}
	}
	return max
}

// AverageFloat64 returns the arithmetic mean of all values, or 0 if empty.
func AverageFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return SumFloat64(values) / float64(len(values))
}
