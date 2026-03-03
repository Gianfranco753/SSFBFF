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
	jsonv2 "encoding/json/v2"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
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

// RequestFieldSet describes which incoming request fields a transform needs.
// The generated route handler uses this to extract only the necessary
// headers/cookies/query/params instead of copying everything on every request.
type RequestFieldSet struct {
	Headers    []string // header names needed
	Cookies    []string // cookie names needed
	Query      []string // query param names needed
	Params     []string // route param names needed
	NeedPath   bool     // needs request path
	NeedMethod bool     // needs HTTP method
	NeedBody   bool     // needs request body
}

// ProviderDep identifies an upstream service endpoint that must be fetched
// before a transform function can run. The aggregator uses this to build
// the fan-out plan. Method/Headers/Body are optional and allow $fetch()
// configs to shape the outgoing request (e.g. POST with a custom body).
type ProviderDep struct {
	Provider string            // e.g. "user_service"
	Endpoint string            // e.g. "profile"
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
// If data is nil, empty, or a JSON null literal, it returns the JSON value
// "null" — this supports graceful degradation when an optional provider fails.
//
// Example: ExtractPath(data, "user", "name") finds {"user":{"name":"Alice"}}
// and returns the raw JSON value "Alice".
func ExtractPath(data []byte, path ...string) (jsontext.Value, error) {
	if len(data) == 0 || string(bytes.TrimSpace(data)) == "null" {
		return jsontext.Value("null"), nil
	}

	dec := jsontext.NewDecoder(bytes.NewReader(data))
	fullPath := strings.Join(path, ".")

	for i, key := range path {
		// We expect an object at each level of the path.
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("reading object at path %q (segment %d: %q): %w", fullPath, i, key, err)
		}
		if tok.Kind() != '{' {
			return nil, fmt.Errorf("expected object at path %q (segment %d: %q), got %v", fullPath, i, key, tok.Kind())
		}

		found := false
		for dec.PeekKind() != '}' {
			nameTok, err := dec.ReadToken()
			if err != nil {
				return nil, fmt.Errorf("reading field name at path %q (segment %d): %w", fullPath, i, err)
			}

			if nameTok.String() != key {
				if err := dec.SkipValue(); err != nil {
					return nil, fmt.Errorf("skipping field at path %q (segment %d): %w", fullPath, i, err)
				}
				continue
			}

			found = true
			break
		}

		if !found {
			return nil, fmt.Errorf("field %q not found at path %q (segment %d)", key, fullPath, i)
		}

		// If this is the last key, read and return the value.
		if i == len(path)-1 {
			val, err := dec.ReadValue()
			if err != nil {
				return nil, fmt.Errorf("reading value for path %q (final segment %q): %w", fullPath, key, err)
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

// --- String functions ---
// These mirror JSONata's built-in string functions. They accept any type and
// coerce to string using ToString before operating.

// ToString coerces any value to a string.
//
// Type coercion rules:
//   - nil → ""
//   - string → as-is
//   - everything else → fmt.Sprintf("%v", v)
//
// Example:
//
//	ToString(42)        // "42"
//	ToString("hello")   // "hello"
//	ToString(nil)       // ""
func ToString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// Uppercase returns the upper-case version of v coerced to string.
func Uppercase(v any) string { return strings.ToUpper(ToString(v)) }

// Lowercase returns the lower-case version of v coerced to string.
func Lowercase(v any) string { return strings.ToLower(ToString(v)) }

// Trim removes leading and trailing whitespace from v coerced to string.
func Trim(v any) string { return strings.TrimSpace(ToString(v)) }

// Length returns the character count of v coerced to string.
func Length(v any) float64 { return float64(len([]rune(ToString(v)))) }

// Substring returns a portion of the string starting at index start (0-based).
// Negative start counts from the end. An optional length limits the result.
func Substring(str any, start float64, length ...float64) string {
	s := []rune(ToString(str))
	i := int(start)
	if i < 0 {
		i = len(s) + i
	}
	if i < 0 {
		i = 0
	}
	if i > len(s) {
		return ""
	}
	if len(length) > 0 {
		end := i + int(length[0])
		if end > len(s) {
			end = len(s)
		}
		if end < i {
			return ""
		}
		return string(s[i:end])
	}
	return string(s[i:])
}

// SubstringBefore returns the part of str before the first occurrence of chars.
func SubstringBefore(str, chars any) string {
	s := ToString(str)
	c := ToString(chars)
	i := strings.Index(s, c)
	if i < 0 {
		return s
	}
	return s[:i]
}

// SubstringAfter returns the part of str after the first occurrence of chars.
func SubstringAfter(str, chars any) string {
	s := ToString(str)
	c := ToString(chars)
	i := strings.Index(s, c)
	if i < 0 {
		return s
	}
	return s[i+len(c):]
}

// Contains returns true if str contains the pattern.
func Contains(str, pattern any) bool {
	return strings.Contains(ToString(str), ToString(pattern))
}

// JoinArray joins a slice of values into a single string with the given
// separator (default ",").
func JoinArray(v any, sep ...string) string {
	s := ","
	if len(sep) > 0 {
		s = sep[0]
	}
	switch arr := v.(type) {
	case []any:
		strs := make([]string, len(arr))
		for i, item := range arr {
			strs[i] = ToString(item)
		}
		return strings.Join(strs, s)
	case []string:
		return strings.Join(arr, s)
	default:
		return ToString(v)
	}
}

// Pad pads a string to the specified length with the given character (default " ").
// If the string is longer than the length, it is returned unchanged.
func Pad(str any, length float64, padChar ...any) string {
	s := ToString(str)
	targetLen := int(length)
	if len(s) >= targetLen {
		return s
	}
	pad := " "
	if len(padChar) > 0 {
		pad = ToString(padChar[0])
		if pad == "" {
			pad = " "
		}
	}
	padLen := targetLen - len(s)
	return s + strings.Repeat(pad, padLen)
}

// SplitArray splits a string into an array of strings using the given separator.
func SplitArray(str any, separator any) any {
	s := ToString(str)
	sep := ToString(separator)
	if sep == "" {
		// Empty separator splits into individual characters
		runes := []rune(s)
		result := make([]any, len(runes))
		for i, r := range runes {
			result[i] = string(r)
		}
		return result
	}
	parts := strings.Split(s, sep)
	result := make([]any, len(parts))
	for i, part := range parts {
		result[i] = part
	}
	return result
}

// --- Numeric functions ---

// ToNumber coerces a value to float64.
//
// Type coercion rules:
//   - float64, int, int64 → converted to float64
//   - string → parsed as float64 (returns 0 on parse error)
//   - bool → 1.0 if true, 0.0 if false
//   - nil → 0.0
//   - other types → 0.0
//
// Example:
//
//	ToNumber("42")      // 42.0
//	ToNumber(42)        // 42.0
//	ToNumber(true)      // 1.0
//	ToNumber(nil)       // 0.0
func ToNumber(v any) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case string:
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
		return 0
	case bool:
		if val {
			return 1
		}
		return 0
	default:
		return 0
	}
}

// Abs returns the absolute value.
func Abs(v float64) float64 { return math.Abs(v) }

// Floor returns the largest integer ≤ v.
func Floor(v float64) float64 { return math.Floor(v) }

// Ceil returns the smallest integer ≥ v.
func Ceil(v float64) float64 { return math.Ceil(v) }

// Round rounds v to the given number of decimal places (default 0).
func Round(v float64, precision ...int) float64 {
	if len(precision) == 0 || precision[0] == 0 {
		return math.Round(v)
	}
	p := math.Pow(10, float64(precision[0]))
	return math.Round(v*p) / p
}

// Power returns base raised to the power of exponent.
func Power(base, exponent float64) float64 {
	return math.Pow(base, exponent)
}

// Sqrt returns the square root of v.
func Sqrt(v float64) float64 {
	return math.Sqrt(v)
}

// globalRand is a seeded random number generator initialized at package load time.
var globalRand = rand.New(rand.NewSource(time.Now().UnixNano()))

// Random returns a random number between 0.0 and 1.0.
// The random number generator is seeded once at package initialization.
func Random() float64 {
	return globalRand.Float64()
}

// --- Boolean functions ---

// ToBoolean coerces a value to bool following JSONata truthiness rules:
// nil→false, false→false, 0→false, ""→false, everything else→true.
func ToBoolean(v any) bool {
	if v == nil {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case float64:
		return val != 0
	case string:
		return len(val) > 0
	default:
		return true
	}
}

// Truthy is an alias for ToBoolean, used in generated conditional expressions.
func Truthy(v any) bool { return ToBoolean(v) }

// Not returns the boolean negation: !$boolean(v).
func Not(v any) bool { return !Truthy(v) }

// Exists returns true when v is non-nil (i.e. the value was present in JSON).
func Exists(v any) bool { return v != nil }

// --- Array functions ---
// These operate on Go any values (typically []any from JSON deserialization).

// SortArray sorts a slice lexicographically by value.
//
// Input: expects []any (array of any type)
// Output: returns []any (sorted copy of input)
//
// If input is not []any, returns the input unchanged.
//
// Example:
//
//	SortArray([]any{3, 1, 2})           // []any{1, 2, 3}
//	SortArray([]any{"c", "a", "b"})     // []any{"a", "b", "c"}
//	SortArray("not an array")            // "not an array" (unchanged)
func SortArray(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	sorted := make([]any, len(arr))
	copy(sorted, arr)
	sort.Slice(sorted, func(i, j int) bool {
		return anyLess(sorted[i], sorted[j])
	})
	return sorted
}

// ReverseArray reverses the order of elements in a slice.
//
// Input: expects []any (array of any type)
// Output: returns []any (reversed copy of input)
//
// If input is not []any, returns the input unchanged.
//
// Example:
//
//	ReverseArray([]any{1, 2, 3})        // []any{3, 2, 1}
func ReverseArray(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	reversed := make([]any, len(arr))
	for i, val := range arr {
		reversed[len(arr)-1-i] = val
	}
	return reversed
}

// AppendArray concatenates two slices.
//
// Input: expects []any for both arguments (or converts single values to []any)
// Output: returns []any (concatenated result)
//
// If both inputs are []any, concatenates them.
// If one input is not []any, converts it to []any and concatenates.
// If neither is []any, returns []any{v1, v2}.
//
// Example:
//
//	AppendArray([]any{1, 2}, []any{3, 4})  // []any{1, 2, 3, 4}
//	AppendArray([]any{1}, 2)               // []any{1, 2}
func AppendArray(v1, v2 any) any {
	a1, ok1 := v1.([]any)
	a2, ok2 := v2.([]any)
	if !ok1 && !ok2 {
		return []any{v1, v2}
	}
	if !ok1 {
		return append([]any{v1}, a2...)
	}
	if !ok2 {
		return append(a1, v2)
	}
	result := make([]any, 0, len(a1)+len(a2))
	result = append(result, a1...)
	result = append(result, a2...)
	return result
}

// DistinctArray removes duplicate values from a slice.
//
// Input: expects []any (array of any type)
// Output: returns []any (deduplicated copy of input)
//
// If input is not []any, returns the input unchanged.
// Duplicate detection uses string representation for comparison.
//
// Example:
//
//	DistinctArray([]any{1, 1, 2, 2, 3})    // []any{1, 2, 3}
func DistinctArray(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	seen := map[string]bool{}
	result := make([]any, 0, len(arr))
	for _, item := range arr {
		key := anyString(item)
		if !seen[key] {
			seen[key] = true
			result = append(result, item)
		}
	}
	return result
}

// ShuffleArray randomly shuffles the elements of an array.
//
// Input: expects []any (array of any type)
// Output: returns []any (shuffled copy of input)
//
// If input is not []any, returns the input unchanged.
//
// Example:
//
//	ShuffleArray([]any{1, 2, 3})    // []any{3, 1, 2} (random order)
func ShuffleArray(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	shuffled := make([]any, len(arr))
	copy(shuffled, arr)
	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	return shuffled
}

// ZipArray combines multiple arrays into an array of tuples.
// Each tuple contains elements at the same index from each input array.
// The result length is the minimum length of all input arrays.
//
// Input: variable number of []any arguments
// Output: returns []any where each element is []any (a tuple)
//
// Example:
//
//	ZipArray([]any{1, 2}, []any{3, 4})    // []any{[]any{1, 3}, []any{2, 4}}
func ZipArray(arrays ...any) any {
	if len(arrays) == 0 {
		return []any{}
	}
	// Convert all inputs to []any and find minimum length
	var arrs [][]any
	minLen := -1
	for _, a := range arrays {
		arr, ok := a.([]any)
		if !ok {
			// If not []any, treat as single-element array
			arr = []any{a}
		}
		arrs = append(arrs, arr)
		if minLen == -1 || len(arr) < minLen {
			minLen = len(arr)
		}
	}
	if minLen == 0 {
		return []any{}
	}
	// Build result: array of tuples
	result := make([]any, minLen)
	for i := 0; i < minLen; i++ {
		tuple := make([]any, len(arrs))
		for j, arr := range arrs {
			tuple[j] = arr[i]
		}
		result[i] = tuple
	}
	return result
}

// --- Object functions ---

// KeysMap returns the keys of a map[string]any as []any.
//
// Input: expects map[string]any (object/map)
// Output: returns []any (sorted array of keys)
//
// If input is not map[string]any, returns nil.
//
// Example:
//
//	KeysMap(map[string]any{"a": 1, "b": 2})  // []any{"a", "b"}
func KeysMap(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]any, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].(string) < keys[j].(string)
	})
	return keys
}

// MergeArray merges an array of objects into a single object.
//
// Input: expects []any where each element is map[string]any
// Output: returns map[string]any (merged object)
//
// If input is not []any, returns the input unchanged.
// Only map[string]any elements are merged; other elements are ignored.
//
// Example:
//
//	MergeArray([]any{
//	  map[string]any{"a": 1},
//	  map[string]any{"b": 2},
//	})  // map[string]any{"a": 1, "b": 2}
func MergeArray(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	result := map[string]any{}
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			for k, val := range m {
				result[k] = val
			}
		}
	}
	return result
}

// TypeOf returns the JSONata type name of a value.
//
// Returns one of: "null", "boolean", "number", "string", "array", "object", "undefined"
//
// Example:
//
//	TypeOf(42)              // "number"
//	TypeOf("hello")         // "string"
//	TypeOf([]any{1, 2})     // "array"
//	TypeOf(map[string]any{}) // "object"
func TypeOf(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case bool:
		return "boolean"
	case float64, int, int64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "undefined"
	}
}

// ValuesMap extracts all values from an object as an array.
//
// Input: expects map[string]any (object)
// Output: returns []any (array of values)
//
// If input is not map[string]any, returns nil.
// Values are returned in sorted key order for deterministic output.
//
// Example:
//
//	ValuesMap(map[string]any{"a": 1, "b": 2})    // []any{1, 2}
func ValuesMap(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	// Collect values in sorted key order for deterministic output
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	values := make([]any, len(keys))
	for i, k := range keys {
		values[i] = m[k]
	}
	return values
}

// SpreadMap spreads object properties into the parent context.
// In JSONata, this is context-dependent. For our implementation,
// we return the object itself, allowing codegen to handle the spreading
// in object construction contexts.
//
// Input: expects map[string]any (object)
// Output: returns map[string]any (the object itself)
//
// If input is not map[string]any, returns the input unchanged.
//
// Example:
//
//	SpreadMap(map[string]any{"a": 1, "b": 2})    // map[string]any{"a": 1, "b": 2}
func SpreadMap(v any) any {
	// Spread is context-dependent in JSONata. For now, we return the object itself.
	// The codegen can handle spreading in object construction contexts.
	return v
}

// --- Date/Time functions ---

// Now generates a UTC timestamp in ISO 8601 compatible format and returns it as a string.
//
// Example:
//
//	Now()    // "2024-01-15T10:30:00.123456789Z"
func Now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// Millis returns the number of milliseconds since the Unix Epoch (1 January, 1970 UTC) as a number.
//
// Example:
//
//	Millis()    // 1705315800123.0
func Millis() float64 {
	return float64(time.Now().UnixMilli())
}

// FromMillis converts the number representing milliseconds since the Unix Epoch
// to a formatted string representation of the timestamp.
//
// The picture and timezone parameters are accepted for API compatibility but
// only ISO 8601 format and UTC timezone are supported for performance.
//
// Example:
//
//	FromMillis(1705315800123)    // "2024-01-15T10:30:00.123Z"
func FromMillis(ms float64, picture ...string) string {
	// Picture and timezone parameters are ignored for performance (ISO 8601 only)
	t := time.UnixMilli(int64(ms)).UTC()
	return t.Format(time.RFC3339Nano)
}

// ToMillis converts a timestamp string to the number of milliseconds since the
// Unix Epoch (1 January, 1970 UTC) as a number.
//
// The picture parameter is accepted for API compatibility but only ISO 8601
// format is supported for performance.
//
// Example:
//
//	ToMillis("2024-01-15T10:30:00.123Z")    // 1705315800123.0
func ToMillis(timestamp any, picture ...string) float64 {
	// Picture parameter is ignored for performance (ISO 8601 only)
	str := ToString(timestamp)
	t, err := time.Parse(time.RFC3339Nano, str)
	if err != nil {
		// Try RFC3339 format as fallback
		t, err = time.Parse(time.RFC3339, str)
		if err != nil {
			return 0
		}
	}
	return float64(t.UnixMilli())
}

// Range returns a slice of float64 values from start to end (inclusive),
// implementing the JSONata [start..end] range operator.
func Range(start, end float64) []float64 {
	s := int(start)
	e := int(end)
	if s > e {
		return nil
	}
	result := make([]float64, 0, e-s+1)
	for i := s; i <= e; i++ {
		result = append(result, float64(i))
	}
	return result
}

// In checks if value is contained in set, implementing the JSONata "in" operator.
// The set can be a []any, []float64, []string, or similar slice.
func In(value any, set any) bool {
	switch s := set.(type) {
	case []any:
		for _, item := range s {
			if anyEqual(value, item) {
				return true
			}
		}
	case []float64:
		v, ok := toFloat(value)
		if !ok {
			return false
		}
		for _, item := range s {
			if v == item {
				return true
			}
		}
	case []string:
		vs := ToString(value)
		for _, item := range s {
			if vs == item {
				return true
			}
		}
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// anyEqual compares two any values without fmt.Sprintf allocations.
// It handles the common JSON types (string, float64, bool, nil) directly,
// falling back to fmt.Sprintf only for complex types like maps/slices.
func anyEqual(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	default:
		return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
	}
}

// anyLess compares two any values for ordering without fmt.Sprintf allocations.
// Numbers sort numerically, strings lexicographically, everything else by type name.
func anyLess(a, b any) bool {
	switch av := a.(type) {
	case float64:
		if bv, ok := b.(float64); ok {
			return av < bv
		}
	case string:
		if bv, ok := b.(string); ok {
			return av < bv
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return !av && bv // false < true
		}
	}
	return fmt.Sprintf("%v", a) < fmt.Sprintf("%v", b)
}

// anyString returns a string key for a value, used for deduplication maps.
// It avoids fmt.Sprintf for the common JSON types.
func anyString(v any) string {
	switch val := v.(type) {
	case string:
		return "s:" + val
	case float64:
		return "n:" + strconv.FormatFloat(val, 'g', -1, 64)
	case bool:
		if val {
			return "b:true"
		}
		return "b:false"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// WildcardValues returns all values of a map (object), implementing the
// JSONata wildcard operator (obj.*).
func WildcardValues(v any) []any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	// Collect values in sorted key order for deterministic output.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]any, len(keys))
	for i, k := range keys {
		vals[i] = m[k]
	}
	return vals
}

// Response represents a complete HTTP response with status code, headers, and body.
// This allows JSONata transforms to control the full HTTP response.
type Response struct {
	StatusCode int
	Headers    map[string]string
	Body       []byte
}

// HTTPError represents an error with an associated HTTP status code.
// This is a convenience wrapper around Response for error cases.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return e.Message
}

// NewHTTPError creates a new HTTPError with the given status code and message.
func NewHTTPError(statusCode int, message string) *HTTPError {
	return &HTTPError{
		StatusCode: statusCode,
		Message:    message,
	}
}

// ToResponse converts an HTTPError to a Response.
// Returns an error if JSON marshaling fails.
func (e *HTTPError) ToResponse() (*Response, error) {
	body, err := jsonv2.Marshal(map[string]any{
		"error":  e.Message,
		"status": e.StatusCode,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal HTTPError response: %w", err)
	}
	return &Response{
		StatusCode: e.StatusCode,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       body,
	}, nil
}
